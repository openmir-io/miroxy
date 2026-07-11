package cred

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	corecred "miroxy/core/cred"
)

const googleTokenURL = "https://oauth2.googleapis.com/token"
const tokenExpiryMargin = 5 * time.Minute

// OAuthSource acquires a Google OAuth access token on demand by exchanging a
// long-lived refresh token. The access token is cached until near expiry.
type OAuthSource struct {
	clientID     string
	clientSecret string
	refreshToken string
	endpoint     string

	mu          sync.Mutex
	accessToken string
	expiry      time.Time

	httpClient *http.Client
}

// NewOAuthSource returns a CredentialSource that exchanges a Google OAuth refresh
// token for access tokens using the standard token endpoint.
func NewOAuthSource(clientID, clientSecret, refreshToken string) *OAuthSource {
	return newOAuthSource(clientID, clientSecret, refreshToken, googleTokenURL)
}

// NewOAuthSourceWithEndpoint is like NewOAuthSource but uses a custom token endpoint.
// Intended for testing.
func NewOAuthSourceWithEndpoint(clientID, clientSecret, refreshToken, endpoint string) *OAuthSource {
	return newOAuthSource(clientID, clientSecret, refreshToken, endpoint)
}

func newOAuthSource(clientID, clientSecret, refreshToken, endpoint string) *OAuthSource {
	return &OAuthSource{
		clientID:     clientID,
		clientSecret: clientSecret,
		refreshToken: refreshToken,
		endpoint:     endpoint,
		httpClient:   &http.Client{Timeout: 15 * time.Second},
	}
}

// SetTokenForTest seeds an access token and expiry directly.
// Only used in unit tests.
func (s *OAuthSource) SetTokenForTest(token string, expiry time.Time) {
	s.mu.Lock()
	s.accessToken = token
	s.expiry = expiry
	s.mu.Unlock()
}

// Credential returns a valid access token as a Bearer HeaderCredential,
// refreshing automatically when within tokenExpiryMargin of expiry.
func (s *OAuthSource) Credential(ctx context.Context) (corecred.Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if time.Now().Add(tokenExpiryMargin).Before(s.expiry) {
		return &corecred.HeaderCredential{Header: "Authorization", Value: "Bearer " + s.accessToken}, nil
	}
	token, err := s.exchange(ctx)
	if err != nil {
		return nil, err
	}
	return &corecred.HeaderCredential{Header: "Authorization", Value: "Bearer " + token}, nil
}

func (s *OAuthSource) exchange(ctx context.Context) (string, error) {
	body := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {s.clientID},
		"client_secret": {s.clientSecret},
		"refresh_token": {s.refreshToken},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint,
		strings.NewReader(body.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if result.Error != "" {
		return "", fmt.Errorf("oauth %s: %s", result.Error, result.ErrorDesc)
	}
	if result.AccessToken == "" {
		return "", fmt.Errorf("empty access_token in response (status %d)", resp.StatusCode)
	}

	s.accessToken = result.AccessToken
	s.expiry = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
	return s.accessToken, nil
}

// WarnIfMultiReplicaUnsafe logs a warning when an oauth_refresh credpool looks
// like it's running in a multi-replica deployment. credstone does not yet
// support OAuth refresh (reserved, not implemented on the credstone side), so
// every oauth_refresh pool self-refreshes locally via OAuthSource today,
// regardless of whether credsource is enabled — there is no cross-replica
// coordination for the refresh_token exchange. Detection is best-effort; when
// it can't be determined this errs toward warning rather than staying silent.
func WarnIfMultiReplicaUnsafe(poolName string) {
	if !likelyMultiReplica() {
		return
	}
	slog.Warn("oauth_refresh credpool has no cross-replica coordination for its refresh_token exchange; "+
		"concurrent refreshes from multiple replicas can race (credstone does not manage OAuth refresh yet)",
		"pool", poolName)
}

func likelyMultiReplica() bool {
	v := os.Getenv("MIROXY_REPLICA_COUNT")
	if v == "" {
		return true // can't tell — err toward warning
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return true
	}
	return n > 1
}

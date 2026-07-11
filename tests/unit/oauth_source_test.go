package unit_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corecred "miroxy/core/cred"
	"miroxy/core/selector"
	"miroxy/internal/cred"
)

// --- OAuthSource tests ---

func tokenServer(t *testing.T, hits *int, accessToken string, expiresIn int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": accessToken,
			"expires_in":   expiresIn,
			"token_type":   "Bearer",
		})
	}))
}

func TestOAuthSource_ExchangesToken(t *testing.T) {
	hits := 0
	srv := tokenServer(t, &hits, "tok-abc", 3600)
	defer srv.Close()

	src := cred.NewOAuthSourceWithEndpoint("client-id", "client-secret", "refresh-tok", srv.URL)

	got, err := src.Credential(context.Background())
	if err != nil {
		t.Fatalf("expected token, got error: %v", err)
	}
	hc, ok := got.(*corecred.HeaderCredential)
	if !ok {
		t.Fatalf("expected *HeaderCredential, got %T", got)
	}
	if hc.Value != "Bearer tok-abc" {
		t.Errorf("expected Bearer tok-abc, got %q", hc.Value)
	}
	if hits != 1 {
		t.Errorf("expected 1 server hit, got %d", hits)
	}
}

func TestOAuthSource_CachesToken(t *testing.T) {
	hits := 0
	srv := tokenServer(t, &hits, "tok-cached", 3600)
	defer srv.Close()

	src := cred.NewOAuthSourceWithEndpoint("id", "secret", "rt", srv.URL)

	for i := range 3 {
		if _, err := src.Credential(context.Background()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if hits != 1 {
		t.Errorf("expected 1 exchange (cached), got %d", hits)
	}
}

func TestOAuthSource_RefreshesNearExpiry(t *testing.T) {
	hits := 0
	srv := tokenServer(t, &hits, "tok-refreshed", 3600)
	defer srv.Close()

	src := cred.NewOAuthSourceWithEndpoint("id", "secret", "rt", srv.URL)
	// Seed a token that expires in 3 minutes (inside the 5-minute margin).
	src.SetTokenForTest("old-tok", time.Now().Add(3*time.Minute))

	got, err := src.Credential(context.Background())
	if err != nil {
		t.Fatalf("expected refresh, got error: %v", err)
	}
	hc2, ok := got.(*corecred.HeaderCredential)
	if !ok {
		t.Fatalf("expected *HeaderCredential, got %T", got)
	}
	if hc2.Value != "Bearer tok-refreshed" {
		t.Errorf("expected Bearer tok-refreshed, got %q", hc2.Value)
	}
	if hits != 1 {
		t.Errorf("expected 1 exchange on refresh, got %d", hits)
	}
}

func TestOAuthSource_ErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"error":             "invalid_grant",
			"error_description": "Token has been expired or revoked.",
		})
	}))
	defer srv.Close()

	src := cred.NewOAuthSourceWithEndpoint("id", "secret", "bad-rt", srv.URL)
	_, err := src.Credential(context.Background())
	if err == nil {
		t.Fatal("expected error for invalid_grant, got nil")
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("error should mention invalid_grant, got: %v", err)
	}
}

// --- CredPool source-error tests ---

type failSource struct{ err error }

func (f *failSource) Credential(_ context.Context) (corecred.Credential, error) { return nil, f.err }

func TestCredPool_SourceError_ReturnsError(t *testing.T) {
	src := &failSource{err: errors.New("token exchange failed")}
	pool := selector.NewCredPool(selector.CredPoolConfig{
		Keys:      []selector.CredSpec{{Source: src}},
		Strategy:  "round_robin",
		Threshold: 3,
		Cooldown:  5 * time.Second,
	})

	_, err := pool.Select(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error when source fails, got nil")
	}
}

func TestCredPool_SourceError_CircuitBreaks(t *testing.T) {
	src := &failSource{err: errors.New("token exchange failed")}
	pool := selector.NewCredPool(selector.CredPoolConfig{
		Keys:      []selector.CredSpec{{Source: src}},
		Strategy:  "round_robin",
		Threshold: 1,
		Cooldown:  5 * time.Second,
	})

	_, _ = pool.Select(context.Background(), nil) // triggers circuit break at threshold=1

	_, err := pool.Select(context.Background(), nil)
	if !errors.Is(err, selector.ErrNoSelection) {
		t.Errorf("expected ErrNoSelection after circuit break, got %v", err)
	}
}

// Package cred: credstone REST client.
//
// credstone exposes its CredBroker protocol over plain HTTP/1.1 POST + JSON
// (the paths look like ConnectRPC paths but require no gRPC/protobuf library
// on this side — see proto/credbroker/v1/cred_broker.proto in the
// miroxy-rpc repo for the canonical field semantics; credstone speaks the
// same wire contract as protojson output, but this client marshals plain
// Go structs, not generated protobuf types).
package cred

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// ── Wire types ──────────────────────────────────────────────────────────────
// Field names are camelCase (credstone uses protojson convention).
// omitempty on response fields mirrors credstone's zero-value omission.

type acquireRequest struct {
	PoolID   string `json:"poolId"`
	CallerID string `json:"callerId,omitempty"`
}

type acquireResponse struct {
	LeaseID    string `json:"leaseId"`
	TTLSeconds int    `json:"ttlSeconds"`
	Kind       string `json:"kind"` // "CREDENTIAL_KIND_HEADER" | "CREDENTIAL_KIND_QUERY"
	Value      string `json:"value"`
	HeaderName string `json:"headerName,omitempty"`
	ParamName  string `json:"paramName,omitempty"`
	EntryID    string `json:"entryId,omitempty"`
}

type releaseRequest struct {
	LeaseID           string `json:"leaseId"`
	RateLimited       bool   `json:"rateLimited,omitempty"`
	ServerOverload    bool   `json:"serverOverload,omitempty"`
	CallError         bool   `json:"callError,omitempty"`
	RetryAfterSeconds int    `json:"retryAfterSeconds,omitempty"`
}

// releaseResponse is {} — no fields needed.
type releaseResponse struct{}

type poolStatusRequest struct {
	PoolID string `json:"poolId,omitempty"` // empty = all pools
}

// reportUsageRequest carries accumulated rpd/tpd deltas for one pool.
//
// credstone does not yet expose a matching endpoint — its /Release only
// accepts rateLimited/serverOverload/callError/retryAfterSeconds today, and
// rpd_limit/tpd_limit are pure config metadata with zero enforcement. This
// defines the wire shape miroxy will send once credstone's side lands; until
// then, ReportUsage calls will fail with a 404/unimplemented-style error,
// which UsageAccumulator already treats as "leave the deltas accumulated for
// next flush" — the same tolerance it has for any other transient failure.
type reportUsageRequest struct {
	PoolID            string `json:"poolId"`
	DeltaRequests     int64  `json:"deltaRequests,omitempty"`
	DeltaInputTokens  int64  `json:"deltaInputTokens,omitempty"`
	DeltaOutputTokens int64  `json:"deltaOutputTokens,omitempty"`
}

type reportUsageResponse struct{}

type poolStatusResponse struct {
	Pools []poolStatus `json:"pools"`
}

type poolStatus struct {
	PoolID      string `json:"poolId"`
	Healthy     int    `json:"healthy"`
	RateLimited int    `json:"rateLimited"`
	CoolingDown int    `json:"coolingDown"`
	InFlight    int    `json:"inFlight"`
}

// credstoneError is the ConnectRPC JSON error format credstone returns on
// non-200 responses.
type credstoneError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *credstoneError) Error() string {
	return fmt.Sprintf("credstone error %s: %s", e.Code, e.Message)
}

// ── Client ────────────────────────────────────────────────────────────────

// CredstoneClient is a plain HTTP/JSON client for credstone's CredBroker
// endpoints. No gRPC or protobuf dependency.
type CredstoneClient struct {
	baseURL    string
	authToken  string
	httpClient *http.Client
}

// NewCredstoneClient creates a client for the given credstone base URL and
// optional bearer token. baseURL should include the scheme; a trailing
// slash is trimmed.
func NewCredstoneClient(baseURL, authToken string) *CredstoneClient {
	return &CredstoneClient{
		baseURL:   strings.TrimRight(baseURL, "/"),
		authToken: authToken,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// post marshals reqBody, POSTs it to <baseURL>/credbroker.v1.CredBrokerService/<method>,
// and unmarshals the response into respBody (when non-nil).
//
// NEVER logs the auth token value. Log lines reference the method name only,
// never the full URL.
func (c *CredstoneClient) post(ctx context.Context, method string, reqBody, respBody any) error {
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("credstone %s: marshal request: %w", method, err)
	}

	url := c.baseURL + "/credbroker.v1.CredBrokerService/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("credstone %s: build request: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("credstone %s: %w", method, err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return fmt.Errorf("credstone %s: read response: %w", method, readErr)
	}

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			slog.Warn("credstone auth failed", "method", method, "hint", "check credsource.auth_token")
		}
		var ce credstoneError
		if jsonErr := json.Unmarshal(body, &ce); jsonErr == nil && ce.Code != "" {
			return &ce
		}
		return fmt.Errorf("credstone %s: HTTP %d", method, resp.StatusCode)
	}

	if respBody != nil {
		if err := json.Unmarshal(body, respBody); err != nil {
			preview := string(body)
			if len(preview) > 200 {
				preview = preview[:200]
			}
			slog.Warn("credstone response JSON parse failed", "method", method, "body_preview", preview)
			return fmt.Errorf("credstone %s: parse response: %w", method, err)
		}
	}
	return nil
}

// Acquire leases one credential from poolID.
func (c *CredstoneClient) Acquire(ctx context.Context, poolID string) (*acquireResponse, error) {
	var resp acquireResponse
	if err := c.post(ctx, "Acquire", acquireRequest{PoolID: poolID, CallerID: "miroxy"}, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Release returns a leased credential with outcome flags.
//
// miroxy has a known gap where a lease ID is not always available after
// Acquire (see internal/cred/credsource.go). An empty leaseID is handled
// gracefully here rather than as an error: log DEBUG and skip the call.
func (c *CredstoneClient) Release(ctx context.Context, req releaseRequest) error {
	if req.LeaseID == "" {
		slog.Debug("skipping credstone Release: empty leaseID")
		return nil
	}
	return c.post(ctx, "Release", req, &releaseResponse{})
}

// PoolStatus returns health metrics for poolID, or all pools when empty.
func (c *CredstoneClient) PoolStatus(ctx context.Context, poolID string) (*poolStatusResponse, error) {
	var resp poolStatusResponse
	if err := c.post(ctx, "PoolStatus", poolStatusRequest{PoolID: poolID}, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ReportUsage sends accumulated rpd/tpd usage deltas for poolID. See
// reportUsageRequest's doc comment — credstone has no receiving endpoint for
// this yet, so callers should treat errors as routine and retry the combined
// delta next interval (UsageAccumulator already does this).
func (c *CredstoneClient) ReportUsage(ctx context.Context, poolID string, deltaRequests, deltaInputTokens, deltaOutputTokens int64) error {
	req := reportUsageRequest{
		PoolID:            poolID,
		DeltaRequests:     deltaRequests,
		DeltaInputTokens:  deltaInputTokens,
		DeltaOutputTokens: deltaOutputTokens,
	}
	return c.post(ctx, "ReportUsage", req, &reportUsageResponse{})
}

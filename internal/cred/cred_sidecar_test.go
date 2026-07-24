package cred

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, handler http.HandlerFunc, authToken string) *CredstoneClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewCredstoneClient(srv.URL, authToken)
}

func decodeBody(t *testing.T, r *http.Request, v any) {
	t.Helper()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
}

// warmHealthy waits (bounded) for cs.IsHealthy() to become true, since the
// poller's first poll runs asynchronously in its own goroutine.
func warmHealthy(t *testing.T, cs *CredSource) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cs.IsHealthy() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("CredSource never became healthy")
}

// ── Acquire ─────────────────────────────────────────────────────────────────

func TestAcquire_Success(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/credbroker.v1.CredBrokerService/Acquire" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"leaseId": "lease-123",
			"ttlSeconds": 30,
			"kind": "CREDENTIAL_KIND_HEADER",
			"value": "sk-test",
			"headerName": "Authorization",
			"entryId": "entry-1"
		}`))
	}, "")

	resp, err := client.Acquire(context.Background(), "gemini-flash")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if resp.LeaseID != "lease-123" {
		t.Errorf("LeaseID = %q, want lease-123", resp.LeaseID)
	}
	if resp.HeaderName != "Authorization" {
		t.Errorf("HeaderName = %q, want Authorization", resp.HeaderName)
	}
	if resp.Value != "sk-test" {
		t.Errorf("Value = %q, want sk-test", resp.Value)
	}
}

func TestAcquire_PoolExhausted(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":"unavailable","message":"pool exhausted"}`))
	}, "")

	_, err := client.Acquire(context.Background(), "gemini-flash")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ce *credstoneError
	if !asCredstoneError(err, &ce) {
		t.Fatalf("error = %v, want it to wrap *credstoneError", err)
	}
	if ce.Code != "unavailable" || ce.Message != "pool exhausted" {
		t.Errorf("credstoneError = %+v, want code=unavailable message=pool exhausted", ce)
	}
}

func asCredstoneError(err error, target **credstoneError) bool {
	ce, ok := err.(*credstoneError)
	if !ok {
		return false
	}
	*target = ce
	return true
}

// ── CredSource credential-kind mapping (via CredSource.Credential + Apply) ──

func TestAcquire_HeaderKind(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/PoolStatus") {
			_, _ = w.Write([]byte(`{"pools":[{"poolId":"p1","healthy":1}]}`))
			return
		}
		_, _ = w.Write([]byte(`{
			"leaseId": "lease-1", "ttlSeconds": 30,
			"kind": "CREDENTIAL_KIND_HEADER", "value": "sk-test",
			"headerName": "Authorization"
		}`))
	}, "")

	cs := NewCredSource(client, "p1")
	cs.StartPoller(context.Background(), time.Hour)
	warmHealthy(t, cs)

	got, err := cs.Credential(context.Background())
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, "http://upstream", nil)
	if err := got.Apply(req); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-test" {
		t.Errorf("Authorization header = %q, want %q", got, "Bearer sk-test")
	}
}

func TestAcquire_QueryKind(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/PoolStatus") {
			_, _ = w.Write([]byte(`{"pools":[{"poolId":"p1","healthy":1}]}`))
			return
		}
		_, _ = w.Write([]byte(`{
			"leaseId": "lease-2", "ttlSeconds": 30,
			"kind": "CREDENTIAL_KIND_QUERY", "value": "AIzaTest",
			"paramName": "key"
		}`))
	}, "")

	cs := NewCredSource(client, "p1")
	cs.StartPoller(context.Background(), time.Hour)
	warmHealthy(t, cs)

	got, err := cs.Credential(context.Background())
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, "http://upstream", nil)
	if err := got.Apply(req); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := req.URL.Query().Get("key"); got != "AIzaTest" {
		t.Errorf("query param key = %q, want AIzaTest", got)
	}
}

func TestAcquire_XApiKeyHeader(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/PoolStatus") {
			_, _ = w.Write([]byte(`{"pools":[{"poolId":"p1","healthy":1}]}`))
			return
		}
		_, _ = w.Write([]byte(`{
			"leaseId": "lease-3", "ttlSeconds": 30,
			"kind": "CREDENTIAL_KIND_HEADER", "value": "raw-value",
			"headerName": "x-api-key"
		}`))
	}, "")

	cs := NewCredSource(client, "p1")
	cs.StartPoller(context.Background(), time.Hour)
	warmHealthy(t, cs)

	got, err := cs.Credential(context.Background())
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, "http://upstream", nil)
	if err := got.Apply(req); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := req.Header.Get("x-api-key"); got != "raw-value" {
		t.Errorf("x-api-key header = %q, want %q (no Bearer prefix)", got, "raw-value")
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization header should be unset, got %q", got)
	}
}

// ── Release ─────────────────────────────────────────────────────────────────

func TestRelease_EmptyLeaseID(t *testing.T) {
	called := false
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{}`))
	}, "")

	if err := client.Release(context.Background(), releaseRequest{LeaseID: ""}); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if called {
		t.Error("expected no HTTP call for empty leaseID")
	}
}

func TestRelease_RateLimited(t *testing.T) {
	var gotBody map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		decodeBody(t, r, &gotBody)
		_, _ = w.Write([]byte(`{}`))
	}, "")

	req := releaseRequest{LeaseID: "lease-1", RateLimited: true, RetryAfterSeconds: 5}
	if err := client.Release(context.Background(), req); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if gotBody["rateLimited"] != true {
		t.Errorf("rateLimited = %v, want true", gotBody["rateLimited"])
	}
	if v, ok := gotBody["callError"]; ok && v == true {
		t.Errorf("callError should be false/omitted, got %v", v)
	}
	if gotBody["retryAfterSeconds"] != float64(5) {
		t.Errorf("retryAfterSeconds = %v, want 5", gotBody["retryAfterSeconds"])
	}
}

// ── PoolStatus ────────────────────────────────────────────────────────────

func TestPoolStatus_SinglePool(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		decodeBody(t, r, &req)
		if req["poolId"] != "gemini-flash" {
			t.Errorf("poolId = %v, want gemini-flash", req["poolId"])
		}
		_, _ = w.Write([]byte(`{"pools":[{"poolId":"gemini-flash","healthy":3,"inFlight":1}]}`))
	}, "")

	resp, err := client.PoolStatus(context.Background(), "gemini-flash")
	if err != nil {
		t.Fatalf("PoolStatus: %v", err)
	}
	if len(resp.Pools) != 1 || resp.Pools[0].Healthy != 3 {
		t.Fatalf("unexpected pools: %+v", resp.Pools)
	}
}

func TestPoolStatus_AllPools(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		decodeBody(t, r, &req)
		if req["poolId"] != nil && req["poolId"] != "" {
			t.Errorf("poolId = %v, want empty/absent for all-pools query", req["poolId"])
		}
		_, _ = w.Write([]byte(`{"pools":[
			{"poolId":"gemini-flash","healthy":2},
			{"poolId":"deepseek","healthy":1,"rateLimited":1}
		]}`))
	}, "")

	resp, err := client.PoolStatus(context.Background(), "")
	if err != nil {
		t.Fatalf("PoolStatus: %v", err)
	}
	if len(resp.Pools) != 2 {
		t.Fatalf("got %d pools, want 2", len(resp.Pools))
	}
}

// ── Auth header ──────────────────────────────────────────────────────────

func TestAuthHeader(t *testing.T) {
	t.Run("token set", func(t *testing.T) {
		var gotAuth string
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"pools":[]}`))
		}, "s3cr3t")

		if _, err := client.PoolStatus(context.Background(), ""); err != nil {
			t.Fatalf("PoolStatus: %v", err)
		}
		if gotAuth != "Bearer s3cr3t" {
			t.Errorf("Authorization header = %q, want Bearer <token>", gotAuth)
		}
	})

	t.Run("token empty", func(t *testing.T) {
		var sawHeader bool
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			sawHeader = r.Header.Get("Authorization") != ""
			_, _ = w.Write([]byte(`{"pools":[]}`))
		}, "")

		if _, err := client.PoolStatus(context.Background(), ""); err != nil {
			t.Fatalf("PoolStatus: %v", err)
		}
		if sawHeader {
			t.Error("expected no Authorization header when authToken is empty")
		}
	})
}

// ── CredSource fast-fail / poller ─────────────────────────────────────────

func TestCredSource_FastFail(t *testing.T) {
	called := false
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{}`))
	}, "")

	// A freshly-constructed CredSource has never polled — lastHealthy starts
	// at 0, so Credential() must fast-fail without an HTTP round-trip.
	cs := NewCredSource(client, "p1")

	_, err := cs.Credential(context.Background())
	if err == nil {
		t.Fatal("expected error when pool is not known healthy")
	}
	if called {
		t.Error("expected no HTTP call on fast-fail path")
	}
}

func TestCredSource_Poller(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"pools":[{"poolId":"p1","healthy":3}]}`))
	}, "")

	cs := NewCredSource(client, "p1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs.StartPoller(ctx, time.Hour) // long interval — we only care about the immediate first poll

	warmHealthy(t, cs)
}

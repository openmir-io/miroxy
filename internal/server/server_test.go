package server

import (
	"context"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"miroxy/core/cred"
	"miroxy/internal/config"
	"miroxy/internal/localstate"
)

func TestParseRetryDelay_EmptyBody(t *testing.T) {
	if d := parseRetryDelay(nil); d != 0 {
		t.Errorf("nil body: want 0, got %v", d)
	}
	if d := parseRetryDelay([]byte{}); d != 0 {
		t.Errorf("zero-length body: want 0, got %v", d)
	}
}

func TestParseRetryDelay_InvalidJSON(t *testing.T) {
	if d := parseRetryDelay([]byte("not json")); d != 0 {
		t.Errorf("invalid JSON: want 0, got %v", d)
	}
}

func TestParseRetryDelay_NoErrorDetails(t *testing.T) {
	body := []byte(`{"error":{"code":429,"message":"quota exceeded"}}`)
	if d := parseRetryDelay(body); d != 0 {
		t.Errorf("no details field: want 0, got %v", d)
	}
}

func TestParseRetryDelay_DetailWithoutRetryDelay(t *testing.T) {
	body := []byte(`{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure"}]}}`)
	if d := parseRetryDelay(body); d != 0 {
		t.Errorf("detail without retryDelay: want 0, got %v", d)
	}
}

func TestParseRetryDelay_SecondsOnly(t *testing.T) {
	body := []byte(`{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"42s"}]}}`)
	d := parseRetryDelay(body)
	if d != 42*time.Second {
		t.Errorf("want 42s, got %v", d)
	}
}

func TestParseRetryDelay_MinutesAndSeconds(t *testing.T) {
	body := []byte(`{"error":{"details":[{"retryDelay":"1m30s"}]}}`)
	d := parseRetryDelay(body)
	if d != 90*time.Second {
		t.Errorf("want 90s, got %v", d)
	}
}

func TestParseRetryDelay_FractionalSeconds(t *testing.T) {
	// Gemini sometimes returns fractional durations like "156h14m36.752s".
	body := []byte(`{"error":{"details":[{"retryDelay":"1.5s"}]}}`)
	d := parseRetryDelay(body)
	if d != 1500*time.Millisecond {
		t.Errorf("want 1.5s, got %v", d)
	}
}

func TestParseRetryDelay_MalformedDuration(t *testing.T) {
	body := []byte(`{"error":{"details":[{"retryDelay":"not-a-duration"}]}}`)
	if d := parseRetryDelay(body); d != 0 {
		t.Errorf("malformed duration: want 0, got %v", d)
	}
}

func TestParseRetryDelay_ZeroDurationString(t *testing.T) {
	body := []byte(`{"error":{"details":[{"retryDelay":"0s"}]}}`)
	if d := parseRetryDelay(body); d != 0 {
		t.Errorf("zero duration string: want 0, got %v", d)
	}
}

// TestParseRetryDelay_FirstValidDetailWins verifies that the first parseable
// retryDelay is returned even when multiple detail entries are present.
func TestParseRetryDelay_FirstValidDetailWins(t *testing.T) {
	body := []byte(`{"error":{"details":[
		{"@type":"type.googleapis.com/google.rpc.QuotaFailure"},
		{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"20s"},
		{"retryDelay":"99s"}
	]}}`)
	d := parseRetryDelay(body)
	if d != 20*time.Second {
		t.Errorf("want 20s (first valid), got %v", d)
	}
}

// TestAdminEndpoints_IndependentOfCredstoneReachability builds a Server with
// credsource enabled but pointed at an address nothing listens on, and
// asserts /health, /stat, and /metrics all respond quickly and correctly —
// none of them may block on or depend on credstone being reachable.
func TestAdminEndpoints_IndependentOfCredstoneReachability(t *testing.T) {
	cfg := &config.Config{
		Admin: config.AdminConfig{Password: "test"},
		CredPools: map[string]config.CredPoolCfg{
			"pool-a": {
				Keys: []config.CredEntry{{Name: "k1", Key: "test-key"}},
			},
		},
		ModelRoutes: []config.ModelEntry{
			{ModelName: "test-model", Provider: "gemini", ProviderModel: "test-model", CredpoolRef: "pool-a"},
		},
		Metrics: config.MetricsConfig{Enabled: true},
		Sidecar: config.SidecarConfig{
			CredSource: config.CredSourceConfig{
				Enabled:      true,
				BaseURL:      "http://127.0.0.1:1", // nothing listens here — always unreachable
				SyncInterval: 300,
			},
		},
	}

	srv := New(cfg, "")
	defer srv.Close()

	deadline := 2 * time.Second
	for _, path := range []string{"/health", "/metrics"} {
		start := time.Now()
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if elapsed := time.Since(start); elapsed > deadline {
			t.Errorf("%s took %v, want under %v (should never wait on credstone)", path, elapsed, deadline)
		}
		if rec.Code != 200 {
			t.Errorf("%s: status = %d, want 200", path, rec.Code)
		}
	}

	start := time.Now()
	req := httptest.NewRequest("GET", "/stat", nil)
	rec := httptest.NewRecorder()
	srv.AdminHandler().ServeHTTP(rec, req)
	if elapsed := time.Since(start); elapsed > deadline {
		t.Errorf("/stat took %v, want under %v (should never wait on credstone)", elapsed, deadline)
	}
	if rec.Code != 200 {
		t.Errorf("/stat: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
}

// TestBuildCredSpecsFromPool_Sigv4 verifies a sigv4 credpool builds
// *cred.SigV4Credential entries with Region/Service from the pool and
// per-key material from each entry.
func TestBuildCredSpecsFromPool_Sigv4(t *testing.T) {
	kp := config.CredPoolCfg{
		AuthStyle: "sigv4",
		Region:    "us-east-1",
		Service:   "bedrock-runtime",
		Keys: []config.CredEntry{
			{Name: "prod", AccessKeyID: "AKIA-prod", SecretAccessKey: "secret-prod", SessionToken: "session-prod"},
			{Name: "backup", AccessKeyID: "AKIA-backup", SecretAccessKey: "secret-backup"},
		},
	}

	specs := buildCredSpecsFromPool(kp, "sigv4", "bedrock-claude")
	if len(specs) != 2 {
		t.Fatalf("got %d specs, want 2", len(specs))
	}

	for i, wantName := range []string{"prod", "backup"} {
		if specs[i].Name != wantName {
			t.Errorf("specs[%d].Name = %q, want %q", i, specs[i].Name, wantName)
		}
		c, err := specs[i].Source.Credential(context.Background())
		if err != nil {
			t.Fatalf("specs[%d].Source.Credential: %v", i, err)
		}
		sig, ok := c.(*cred.SigV4Credential)
		if !ok {
			t.Fatalf("specs[%d] credential type = %T, want *cred.SigV4Credential", i, c)
		}
		if sig.Region != "us-east-1" || sig.Service != "bedrock-runtime" {
			t.Errorf("specs[%d] region/service = %q/%q, want us-east-1/bedrock-runtime", i, sig.Region, sig.Service)
		}
		if sig.AccessKeyID != kp.Keys[i].AccessKeyID || sig.SecretAccessKey != kp.Keys[i].SecretAccessKey {
			t.Errorf("specs[%d] access/secret = %q/%q, want %q/%q", i, sig.AccessKeyID, sig.SecretAccessKey, kp.Keys[i].AccessKeyID, kp.Keys[i].SecretAccessKey)
		}
		if sig.SessionToken != kp.Keys[i].SessionToken {
			t.Errorf("specs[%d] SessionToken = %q, want %q", i, sig.SessionToken, kp.Keys[i].SessionToken)
		}
	}
}

// TestOpenLocalStateStore_CredsourceTakesPrecedence verifies local_state is
// ignored (returns nil, no file created) whenever sidecar.credsource is
// enabled, regardless of local_state.enabled — credstone is already the
// authoritative source of credential health in that mode.
func TestOpenLocalStateStore_CredsourceTakesPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/state.db"
	cfg := &config.Config{
		Sidecar:    config.SidecarConfig{CredSource: config.CredSourceConfig{Enabled: true}},
		LocalState: config.LocalStateConfig{Enabled: true, Path: path},
	}

	store := openLocalStateStore(cfg)
	if store != nil {
		t.Error("expected nil store when sidecar.credsource.enabled is true")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("expected no local_state file to be created when credsource takes precedence")
	}
}

// TestOpenLocalStateStore_DisabledByDefault verifies no store (and no file)
// is created when local_state.enabled is left false.
func TestOpenLocalStateStore_DisabledByDefault(t *testing.T) {
	cfg := &config.Config{}
	if store := openLocalStateStore(cfg); store != nil {
		defer store.Close()
		t.Error("expected nil store when local_state.enabled is false")
	}
}

// TestOpenLocalStateStore_StandaloneEnabled verifies a real store (and file)
// is created when local_state.enabled is true and credsource is not enabled.
func TestOpenLocalStateStore_StandaloneEnabled(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/state.db"
	cfg := &config.Config{LocalState: config.LocalStateConfig{Enabled: true, Path: path}}

	store := openLocalStateStore(cfg)
	if store == nil {
		t.Fatal("expected a non-nil store in standalone mode with local_state enabled")
	}
	defer store.Close()

	if err := store.SaveAllCredHealth("p", map[string]localstate.CredHealth{"k": {State: "healthy"}}); err != nil {
		t.Fatalf("SaveAllCredHealth: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected local_state file at %s: %v", path, err)
	}
}

package server

import (
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	ccomp "miroxy/core/compress"
	"miroxy/core/cred"
	corewarden "miroxy/core/warden"
	"miroxy/internal/config"
	"miroxy/internal/localstate"
	"miroxy/internal/stats"
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
			{ModelName: "test-model", ProviderRef: "gemini", UpstreamModel: "test-model", CredpoolRef: "pool-a"},
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

// TestBuildCredSpecsFromPool_SkipsUnusableKeys guards the fix where a config
// key with an empty/unexpanded value only warns at validation time (see
// validateKeys) instead of failing config load — the runtime pool must still
// never receive that entry, or Select() could hand out a credential with an
// empty Authorization value.
func TestBuildCredSpecsFromPool_SkipsUnusableKeys(t *testing.T) {
	kp := config.CredPoolCfg{
		Keys: []config.CredEntry{
			{Name: "real", Key: "sk-real"},
			{Name: "unset", Key: ""},
		},
	}

	specs := buildCredSpecsFromPool(kp, "bearer", "mistral-free")
	if len(specs) != 1 {
		t.Fatalf("got %d specs, want 1 (the unusable key must be skipped)", len(specs))
	}
	if specs[0].Name != "real" {
		t.Errorf("specs[0].Name = %q, want %q", specs[0].Name, "real")
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

// TestWardenStats_PersistsAcrossRestart proves the restart-recovery path:
// a flush from one warden instance is readable by a second, independent
// warden instance opened against the same local_state file afterward —
// simulating a process restart without needing an actual second process.
func TestWardenStats_PersistsAcrossRestart(t *testing.T) {
	path := t.TempDir() + "/warden-state.db"
	cfg := &config.Config{LocalState: config.LocalStateConfig{Enabled: true, Path: path}}
	wardenCfg := &config.WardenConfig{Enabled: true, Mode: "redact"}

	store1 := openLocalStateStore(cfg)
	if store1 == nil {
		t.Fatal("expected a non-nil store in standalone mode with local_state enabled")
	}

	_, stats1 := buildWardenPlugin(wardenCfg, store1)
	stats1.Record([]corewarden.Finding{
		{Category: corewarden.CategorySecret, Type: "aws_access_key_id", Verdict: corewarden.VerdictRedact},
	}, 0)
	flushWardenStats(stats1, store1)
	if err := store1.Close(); err != nil {
		t.Fatalf("store1.Close: %v", err)
	}

	// Simulate a restart: a fresh store and a fresh warden instance against
	// the same file, with no in-memory state carried over.
	store2 := openLocalStateStore(cfg)
	if store2 == nil {
		t.Fatal("expected a non-nil store on reopen")
	}
	defer store2.Close()

	_, stats2 := buildWardenPlugin(wardenCfg, store2)
	snap := stats2.Snapshot()

	if snap.RequestsInspected != 1 {
		t.Errorf("RequestsInspected = %d, want 1", snap.RequestsInspected)
	}
	if snap.SecretsFound != 1 {
		t.Errorf("SecretsFound = %d, want 1", snap.SecretsFound)
	}
	if got := snap.ByType["secret:aws_access_key_id"]; got != 1 {
		t.Errorf("ByType[secret:aws_access_key_id] = %d, want 1", got)
	}

	// A subsequent live increment on the restored instance builds on top of
	// the restored baseline rather than replacing it.
	stats2.Record([]corewarden.Finding{
		{Category: corewarden.CategoryPII, Type: "email", Verdict: corewarden.VerdictRedact},
	}, 0)
	snap2 := stats2.Snapshot()
	if snap2.RequestsInspected != 2 {
		t.Errorf("RequestsInspected after a live increment = %d, want 2", snap2.RequestsInspected)
	}
	if snap2.SecretsFound != 1 {
		t.Errorf("SecretsFound should still reflect the restored baseline: got %d, want 1", snap2.SecretsFound)
	}
	if snap2.PIIFound != 1 {
		t.Errorf("PIIFound = %d, want 1", snap2.PIIFound)
	}
}

// TestTokenStats_PersistsAcrossRestart mirrors TestWardenStats_PersistsAcrossRestart
// for token usage: a flush from one Registry must be readable by a second,
// independent Registry restored from the same local_state file, including
// the model → credpool → credential hierarchy.
func TestTokenStats_PersistsAcrossRestart(t *testing.T) {
	path := t.TempDir() + "/token-state.db"
	cfg := &config.Config{LocalState: config.LocalStateConfig{Enabled: true, Path: path}}

	store1 := openLocalStateStore(cfg)
	if store1 == nil {
		t.Fatal("expected a non-nil store in standalone mode with local_state enabled")
	}
	reg1 := &stats.Registry{}
	reg1.Record("mistral-test", "mistral-free", "mistral_bytebyteops", 1000, 200)
	flushTokenStats(reg1, store1)
	if err := store1.Close(); err != nil {
		t.Fatalf("store1.Close: %v", err)
	}

	store2 := openLocalStateStore(cfg)
	if store2 == nil {
		t.Fatal("expected a non-nil store on reopen")
	}
	defer store2.Close()

	reg2 := &stats.Registry{}
	if persisted, ok := store2.LoadTokenStats(); ok {
		reg2.Restore(tokenStatsFromPersisted(persisted))
	} else {
		t.Fatal("expected a persisted token stats snapshot")
	}

	totalIn, totalOut, totalReq, models := reg2.Snapshot()
	if totalIn != 1000 || totalOut != 200 || totalReq != 1 {
		t.Fatalf("restored totals = (%d, %d, %d), want (1000, 200, 1)", totalIn, totalOut, totalReq)
	}
	if len(models) != 1 || models[0].Name != "mistral-test" {
		t.Fatalf("restored models = %+v", models)
	}
	if len(models[0].Pools) != 1 || models[0].Pools[0].Name != "mistral-free" {
		t.Fatalf("restored pools = %+v", models[0].Pools)
	}
	if keys := models[0].Pools[0].Keys; len(keys) != 1 || keys[0].Name != "mistral_bytebyteops" || keys[0].Input != 1000 {
		t.Fatalf("restored keys = %+v", keys)
	}

	// A live increment after restore builds on the restored baseline.
	reg2.Record("mistral-test", "mistral-free", "mistral_bytebyteops", 50, 10)
	totalIn2, _, totalReq2, _ := reg2.Snapshot()
	if totalIn2 != 1050 || totalReq2 != 2 {
		t.Errorf("after live increment: totalIn=%d totalReq=%d, want 1050/2", totalIn2, totalReq2)
	}
}

// TestCompressStats_PersistsAcrossRestart mirrors the same contract for
// compression's per-model counters.
func TestCompressStats_PersistsAcrossRestart(t *testing.T) {
	path := t.TempDir() + "/compress-state.db"
	cfg := &config.Config{LocalState: config.LocalStateConfig{Enabled: true, Path: path}}
	compressCfg := &config.CompressConfig{Enabled: true, Threshold: 4000}

	store1 := openLocalStateStore(cfg)
	if store1 == nil {
		t.Fatal("expected a non-nil store in standalone mode with local_state enabled")
	}
	_, cs1 := buildCompressPlugin(compressCfg, store1)
	cs1.Record("mistral-test", &ccomp.Result{OriginalTokens: 5000, CompressedTokens: 3000}, 100)
	flushCompressStats(cs1, store1)
	if err := store1.Close(); err != nil {
		t.Fatalf("store1.Close: %v", err)
	}

	store2 := openLocalStateStore(cfg)
	if store2 == nil {
		t.Fatal("expected a non-nil store on reopen")
	}
	defer store2.Close()

	_, cs2 := buildCompressPlugin(compressCfg, store2)
	snap := cs2.Snapshot()
	if snap.Requests != 1 || snap.OriginalTokens != 5000 || snap.CompressedTokens != 3000 {
		t.Fatalf("restored snapshot = %+v", snap)
	}
	if len(snap.Models) != 1 || snap.Models[0].Name != "mistral-test" {
		t.Fatalf("restored models = %+v", snap.Models)
	}

	cs2.Record("mistral-test", &ccomp.Result{OriginalTokens: 1000, CompressedTokens: 900}, 50)
	snap2 := cs2.Snapshot()
	if snap2.Requests != 2 || snap2.OriginalTokens != 6000 {
		t.Fatalf("after live record: %+v, want Requests=2 OriginalTokens=6000", snap2)
	}
}

// TestStatsText_ShowsFullHierarchyAndCompressionNote guards the report
// rewrite: model -> credpool(provider) -> credential must all be visible,
// and a compression note must appear whenever compressStats has real data.
func TestStatsText_ShowsFullHierarchyAndCompressionNote(t *testing.T) {
	cfg := &config.Config{
		CredPools: map[string]config.CredPoolCfg{
			"mistral-free": {ProviderRef: "mistral", Keys: []config.CredEntry{{Name: "mistral_bytebyteops", Key: "test-key"}}},
		},
		ModelRoutes: []config.ModelEntry{
			{ModelName: "mistral-test", ProviderRef: "mistral", UpstreamModel: "mistral-code-agent-latest", CredpoolRef: "mistral-free"},
		},
	}
	srv := New(cfg, "")
	defer srv.Close()

	srv.tokenStats.Record("mistral-test", "mistral-free", "mistral_bytebyteops", 1000, 200)
	srv.compressStats = ccomp.NewStats()
	srv.compressStats.Record("mistral-test", &ccomp.Result{OriginalTokens: 5000, CompressedTokens: 3000}, 100)

	out := srv.StatsText()
	for _, want := range []string{
		"miroxy Performance Report",
		"mistral-test",
		"mistral-free (mistral)",
		"mistral_bytebyteops",
		"Totals above already reflect compression",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("StatsText() missing %q, got:\n%s", want, out)
		}
	}
}

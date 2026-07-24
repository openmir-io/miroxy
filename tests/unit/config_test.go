package unit_test

import (
	"os"
	"path/filepath"
	"testing"

	"miroxy/internal/config"
)

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return f.Name()
}

func TestYAMLStore_Load_Basic(t *testing.T) {
	path := writeTempConfig(t, `
server:
  port: 9090
providers:
  gemini: {}
model_routes:
  - model_name: claude-haiku
    upstream_model: gemini-2.5-flash
    credpool:
      provider_ref: gemini
      strategy: round_robin
      keys:
        - test-api-key
    timeout_seconds: 30
`)
	cfg, err := config.NewYAMLStore(path).Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.Port != 9090 {
		t.Errorf("port: got %d, want 9090", cfg.Server.Port)
	}
	if len(cfg.ModelRoutes) != 1 {
		t.Fatalf("model count: got %d, want 1", len(cfg.ModelRoutes))
	}
	if cfg.ModelRoutes[0].ModelName != "claude-haiku" {
		t.Errorf("model_name: got %q", cfg.ModelRoutes[0].ModelName)
	}
}

func TestYAMLStore_Load_EnvVarExpansion(t *testing.T) {
	t.Setenv("TEST_GEMINI_KEY", "expanded-key-value")

	path := writeTempConfig(t, `
providers:
  gemini: {}
model_routes:
  - model_name: claude-haiku
    upstream_model: gemini-2.5-flash
    credpool:
      provider_ref: gemini
      keys:
        - ${TEST_GEMINI_KEY}
`)
	cfg, err := config.NewYAMLStore(path).Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := cfg.ModelRoutes[0].CredPool.Keys[0]
	if got.Key != "expanded-key-value" {
		t.Errorf("env expansion: got %q, want %q", got.Key, "expanded-key-value")
	}
}

func TestYAMLStore_Load_UnsetEnvVarFails(t *testing.T) {
	os.Unsetenv("DEFINITELY_NOT_SET_XYZ123")

	path := writeTempConfig(t, `
model_routes:
  - model_name: claude-haiku
    upstream_model: gemini-2.5-flash
    credpool:
      keys:
        - ${DEFINITELY_NOT_SET_XYZ123}
`)
	_, err := config.NewYAMLStore(path).Load()
	if err == nil {
		t.Fatal("expected error for unset env var, got nil")
	}
}

// TestYAMLStore_Load_SomeEmptyKeysWarnButLoadSucceeds guards the policy that
// an individual empty/unset credential key only warns — the pool (and the
// whole config) still loads as long as at least one usable key exists
// anywhere. Load failure is reserved for the case in
// TestYAMLStore_Load_UnsetEnvVarFails, where that's the config's only key.
func TestYAMLStore_Load_SomeEmptyKeysWarnButLoadSucceeds(t *testing.T) {
	t.Setenv("TEST_MISTRAL_KEY_1", "real-value")
	os.Unsetenv("TEST_MISTRAL_KEY_2_UNSET")

	path := writeTempConfig(t, `
providers:
  mistral: {base_url: "https://api.mistral.ai/v1", protocol: openai, auth_style: bearer}
model_routes:
  - model_name: claude-mistral
    upstream_model: mistral-large-latest
    credpool:
      provider_ref: mistral
      keys:
        - key_ok: ${TEST_MISTRAL_KEY_1}
        - key_missing: ${TEST_MISTRAL_KEY_2_UNSET}
`)
	cfg, err := config.NewYAMLStore(path).Load()
	if err != nil {
		t.Fatalf("expected load to succeed when at least one key is usable, got: %v", err)
	}
	keys := cfg.ModelRoutes[0].CredPool.Keys
	if keys[0].Key != "real-value" {
		t.Errorf("key_ok: got %q, want %q", keys[0].Key, "real-value")
	}
	if keys[1].IsUsable("bearer") {
		t.Errorf("key_missing: IsUsable should be false for an unexpanded placeholder, got Key=%q", keys[1].Key)
	}
}

// TestYAMLStore_Load_AllKeysEmptyAcrossMultiplePoolsFails guards that the
// zero-usable-keys check accumulates across every credpool in the config
// (both the named credpools: block and inline model_routes credpools), not
// just whichever pool validateKeys happens to see first.
func TestYAMLStore_Load_AllKeysEmptyAcrossMultiplePoolsFails(t *testing.T) {
	os.Unsetenv("TEST_UNSET_A")
	os.Unsetenv("TEST_UNSET_B")

	path := writeTempConfig(t, `
providers:
  gemini: {}
  mistral: {base_url: "https://api.mistral.ai/v1", protocol: openai, auth_style: bearer}
credpools:
  gemini-pool:
    provider_ref: gemini
    keys:
      - a: ${TEST_UNSET_A}
model_routes:
  - model_name: claude-a
    upstream_model: gemini-2.5-flash
    credpool_ref: gemini-pool
  - model_name: claude-b
    upstream_model: mistral-large-latest
    credpool:
      provider_ref: mistral
      keys:
        - b: ${TEST_UNSET_B}
`)
	_, err := config.NewYAMLStore(path).Load()
	if err == nil {
		t.Fatal("expected error when every key across every pool is unusable, got nil")
	}
}

func TestCredEntry_IsUsable(t *testing.T) {
	cases := []struct {
		name      string
		entry     config.CredEntry
		authStyle string
		want      bool
	}{
		{"bearer with value", config.CredEntry{Key: "sk-abc"}, "bearer", true},
		{"bearer empty", config.CredEntry{Key: ""}, "bearer", false},
		{"bearer unexpanded placeholder", config.CredEntry{Key: "${UNSET_XYZ}"}, "bearer", false},
		{"sigv4 with both parts", config.CredEntry{AccessKeyID: "AKIA1", SecretAccessKey: "secret"}, "sigv4", true},
		{"sigv4 missing secret", config.CredEntry{AccessKeyID: "AKIA1"}, "sigv4", false},
		{"sigv4 unexpanded placeholder", config.CredEntry{AccessKeyID: "AKIA1", SecretAccessKey: "${UNSET_XYZ}"}, "sigv4", false},
	}
	for _, tc := range cases {
		if got := tc.entry.IsUsable(tc.authStyle); got != tc.want {
			t.Errorf("%s: IsUsable(%q) = %v, want %v", tc.name, tc.authStyle, got, tc.want)
		}
	}
}

func TestYAMLStore_Load_MissingFile(t *testing.T) {
	_, err := config.NewYAMLStore(filepath.Join(t.TempDir(), "nonexistent.yaml")).Load()
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestYAMLStore_Load_EmptyModelRoutes(t *testing.T) {
	path := writeTempConfig(t, `server: {port: 8080}`)
	_, err := config.NewYAMLStore(path).Load()
	if err == nil {
		t.Fatal("expected validation error for empty model_routes, got nil")
	}
}

func TestYAMLStore_Load_APIBaseMismatch(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{
			name: "gemini URL with openai protocol — mismatch",
			yaml: `
model_routes:
  - model_name: m
    upstream_model: some-model
    credpool:
      protocol: openai
      api_base: https://generativelanguage.googleapis.com/v1
      auth_style: bearer
      keys: [test-key]
`,
			wantErr: true,
		},
		{
			name: "openai URL with gemini protocol — mismatch",
			yaml: `
model_routes:
  - model_name: m
    upstream_model: some-model
    credpool:
      protocol: gemini
      api_base: https://api.openai.com/v1
      keys: [test-key]
`,
			wantErr: true,
		},
		{
			name: "deepseek URL with openai protocol — allowed (openai-compatible)",
			yaml: `
model_routes:
  - model_name: m
    upstream_model: deepseek-chat
    credpool:
      protocol: openai
      api_base: https://api.deepseek.com/v1
      auth_style: bearer
      keys: [test-key]
`,
			wantErr: false,
		},
		{
			name: "custom proxy URL — unknown host, no check",
			yaml: `
model_routes:
  - model_name: m
    upstream_model: some-model
    credpool:
      protocol: openai
      api_base: https://my-proxy.internal/v1
      auth_style: bearer
      keys: [test-key]
`,
			wantErr: false,
		},
		{
			name: "invalid api_base URL",
			yaml: `
model_routes:
  - model_name: m
    upstream_model: some-model
    credpool:
      protocol: openai
      api_base: "not a url"
      auth_style: bearer
      keys: [test-key]
`,
			wantErr: true,
		},
		{
			name: "mode: passthrough skips mismatch check",
			yaml: `
model_routes:
  - model_name: m
    mode: passthrough
    upstream_model: claude-haiku-4-5-20251001
    credpool:
      protocol: anthropic
      api_base: https://generativelanguage.googleapis.com/any/path
      auth_style: bearer
      keys: [test-key]
`,
			wantErr: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempConfig(t, tc.yaml)
			_, err := config.NewYAMLStore(path).Load()
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestConfig_LookupModel(t *testing.T) {
	cfg := &config.Config{
		ModelRoutes: []config.ModelEntry{
			{ModelName: "haiku", UpstreamModel: "gemini-flash"},
			{ModelName: "haiku-mini", UpstreamModel: "gemini-mini"},
			{ModelName: "opus", UpstreamModel: "glm"},
			{ModelName: "sonnet-5", UpstreamModel: "gemini-pro"},
			{ModelName: "gpt-5.4", UpstreamModel: "deepseek"},
		},
		Server: config.ServerConfig{DefaultModel: "haiku"},
	}

	cases := []struct {
		input  string
		wantPM string // expected UpstreamModel
		wantOK bool
	}{
		// exact match
		{"haiku", "gemini-flash", true},
		// strip claude- prefix → exact match
		{"claude-haiku", "gemini-flash", true},
		// prefix match: haiku-4-5-20251001 → haiku (longer "haiku-mini" not a prefix)
		{"claude-haiku-4-5-20251001", "gemini-flash", true},
		// prefix match: haiku-mini-* → haiku-mini wins over haiku (longer prefix)
		{"claude-haiku-mini-20250101", "gemini-mini", true},
		// prefix match: opus-4-8 → opus
		{"claude-opus-4-8", "glm", true},
		// prefix match: sonnet-5 and sonnet-5-20261001 both hit sonnet-5
		{"claude-sonnet-5", "gemini-pro", true},
		{"claude-sonnet-5-20261001", "gemini-pro", true},
		// boundary: "haikux" must NOT match "haiku"
		{"haikux", "gemini-flash", true}, // no match → fallback to default "haiku"
		// gpt prefix match (no claude- stripping needed)
		{"gpt-5.4-mini", "deepseek", true},
		{"gpt-5.4-turbo", "deepseek", true},
		// nothing matches → false (no default set in this sub-test)
	}

	for _, tc := range cases {
		e, ok := cfg.LookupModel(tc.input)
		if ok != tc.wantOK {
			t.Errorf("LookupModel(%q) ok=%v want %v", tc.input, ok, tc.wantOK)
			continue
		}
		if ok && e.UpstreamModel != tc.wantPM {
			t.Errorf("LookupModel(%q) upstreamModel=%q want %q", tc.input, e.UpstreamModel, tc.wantPM)
		}
	}

	// no match without default
	cfgNoDefault := &config.Config{
		ModelRoutes: []config.ModelEntry{
			{ModelName: "haiku", UpstreamModel: "gemini-flash"},
		},
	}
	if _, ok := cfgNoDefault.LookupModel("claude-opus-4-8"); ok {
		t.Error("LookupModel(claude-opus-4-8) with no match and no default should return ok=false")
	}
}

// TestConfig_LookupModel_NativePassthrough guards LookupModel step 4: a
// model name globally unique to one vendor (claude-*) with no model_routes
// entry only resolves through a credpool that opted in via
// native_passthrough: true, and only when the root
// native_passthrough_enable switch is also on.
func TestConfig_LookupModel_NativePassthrough(t *testing.T) {
	cfg := &config.Config{
		NativePassthroughEnable: true,
		CredPools: map[string]config.CredPoolCfg{
			"real-anthropic": {
				NativePassthrough: true,
				Protocol:          "anthropic",
				Keys:              []config.CredEntry{{Name: "k", Key: "sk-real"}},
			},
		},
	}

	entry, ok := cfg.LookupModel("claude-opus-4-8-not-configured")
	if !ok {
		t.Fatal("expected native passthrough match, got ok=false")
	}
	if entry.ProviderRef != "anthropic" || entry.UpstreamModel != "claude-opus-4-8-not-configured" {
		t.Errorf("entry = %+v, want ProviderRef=anthropic UpstreamModel=claude-opus-4-8-not-configured", entry)
	}

	cfg.NativePassthroughEnable = false
	if _, ok := cfg.LookupModel("claude-opus-4-8-not-configured"); ok {
		t.Error("expected no match when native_passthrough_enable is false")
	}

	cfg.NativePassthroughEnable = true
	cfg.CredPools["real-anthropic"] = config.CredPoolCfg{NativePassthrough: false, Protocol: "anthropic", Keys: []config.CredEntry{{Name: "k", Key: "sk-real"}}}
	if _, ok := cfg.LookupModel("claude-opus-4-8-not-configured"); ok {
		t.Error("expected no match when no credpool has native_passthrough: true")
	}
}

// --- sigv4 (AWS Bedrock-style) credpool entries ---

func TestYAMLStore_Load_Sigv4StructuredShorthand(t *testing.T) {
	t.Setenv("TEST_AWS_ACCESS_KEY", "AKIA-test")
	t.Setenv("TEST_AWS_SECRET_KEY", "secret-test")
	t.Setenv("TEST_AWS_SESSION_TOKEN", "session-test")

	path := writeTempConfig(t, `
providers:
  anthropic: {}
credpools:
  bedrock-claude:
    provider_ref: anthropic
    auth_style: sigv4
    region: us-east-1
    service: bedrock-runtime
    strategy: round_robin
    keys:
      - prod:
          access_key_id: ${TEST_AWS_ACCESS_KEY}
          secret_access_key: ${TEST_AWS_SECRET_KEY}
          session_token: ${TEST_AWS_SESSION_TOKEN}
      - backup:
          access_key_id: ${TEST_AWS_ACCESS_KEY}
          secret_access_key: ${TEST_AWS_SECRET_KEY}
model_routes:
  - model_name: claude-bedrock
    upstream_model: claude-3
    credpool_ref: bedrock-claude
`)
	cfg, err := config.NewYAMLStore(path).Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pool := cfg.CredPools["bedrock-claude"]
	if pool.AuthStyle != "sigv4" || pool.Region != "us-east-1" || pool.Service != "bedrock-runtime" {
		t.Fatalf("pool fields: %+v", pool)
	}
	if len(pool.Keys) != 2 {
		t.Fatalf("keys: got %d, want 2", len(pool.Keys))
	}
	prod := pool.Keys[0]
	if prod.Name != "prod" || prod.AccessKeyID != "AKIA-test" || prod.SecretAccessKey != "secret-test" || prod.SessionToken != "session-test" {
		t.Errorf("prod entry: %+v", prod)
	}
	if prod.Key != "" {
		t.Errorf("prod.Key should stay empty for sigv4 entries, got %q", prod.Key)
	}
	backup := pool.Keys[1]
	if backup.SessionToken != "" {
		t.Errorf("backup.SessionToken should be empty (optional, omitted), got %q", backup.SessionToken)
	}
}

func TestYAMLStore_Load_Sigv4VerboseForm(t *testing.T) {
	t.Setenv("TEST_AWS_ACCESS_KEY", "AKIA-verbose")
	t.Setenv("TEST_AWS_SECRET_KEY", "secret-verbose")

	path := writeTempConfig(t, `
providers:
  anthropic: {}
credpools:
  bedrock-claude:
    provider_ref: anthropic
    auth_style: sigv4
    region: us-east-1
    service: bedrock-runtime
    keys:
      - name: prod
        access_key_id: ${TEST_AWS_ACCESS_KEY}
        secret_access_key: ${TEST_AWS_SECRET_KEY}
model_routes:
  - model_name: claude-bedrock
    upstream_model: claude-3
    credpool_ref: bedrock-claude
`)
	cfg, err := config.NewYAMLStore(path).Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := cfg.CredPools["bedrock-claude"].Keys[0]
	if got.Name != "prod" || got.AccessKeyID != "AKIA-verbose" || got.SecretAccessKey != "secret-verbose" {
		t.Errorf("verbose sigv4 entry: %+v", got)
	}
}

func TestYAMLStore_Load_Sigv4MissingSecretFails(t *testing.T) {
	path := writeTempConfig(t, `
providers:
  anthropic: {}
credpools:
  bedrock-claude:
    provider_ref: anthropic
    auth_style: sigv4
    region: us-east-1
    keys:
      - prod:
          access_key_id: AKIA-only
model_routes:
  - model_name: claude-bedrock
    upstream_model: claude-3
    credpool_ref: bedrock-claude
`)
	if _, err := config.NewYAMLStore(path).Load(); err == nil {
		t.Fatal("expected error for sigv4 entry missing secret_access_key")
	}
}

func TestYAMLStore_Load_Sigv4InvalidPoolAuthStyleFails(t *testing.T) {
	path := writeTempConfig(t, `
providers:
  anthropic: {}
credpools:
  bedrock-claude:
    auth_style: not-a-real-style
    keys:
      - prod:
          access_key_id: AKIA-x
          secret_access_key: secret-x
`)
	if _, err := config.NewYAMLStore(path).Load(); err == nil {
		t.Fatal("expected error for invalid credpool auth_style")
	}
}

// TestYAMLStore_Load_CredpoolProviderRefIsSingleSourceOfTruth guards the
// core of the native_passthrough redesign: provider_ref lives only on the
// credpool now, so two model_routes entries sharing one credpool_ref always
// get the exact same protocol — the "conflicting provider_ref" family of
// config mistakes this replaces is now impossible by construction, not
// something that needs a separate consistency check.
func TestYAMLStore_Load_CredpoolProviderRefIsSingleSourceOfTruth(t *testing.T) {
	path := writeTempConfig(t, `
providers:
  gemini: {}
credpools:
  shared-keys:
    provider_ref: gemini
    keys:
      - main: test-key
model_routes:
  - model_name: claude-haiku
    upstream_model: gemini-2.5-flash
    credpool_ref: shared-keys
  - model_name: claude-haiku-2
    upstream_model: gemini-2.5-pro
    credpool_ref: shared-keys
`)
	cfg, err := config.NewYAMLStore(path).Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, m := range cfg.ModelRoutes {
		if m.ProviderRef != "gemini" || m.Protocol != "gemini" {
			t.Errorf("%s: provider_ref/protocol = %q/%q, want gemini/gemini (resolved from credpools.shared-keys)", m.ModelName, m.ProviderRef, m.Protocol)
		}
	}
}

func TestYAMLStore_Load_NativePassthroughRequiresNativeProtocol(t *testing.T) {
	path := writeTempConfig(t, `
providers:
  grok: {}
credpools:
  grok-native-attempt:
    provider_ref: grok
    native_passthrough: true
    keys:
      - main: test-key
model_routes:
  - model_name: m
    upstream_model: grok-beta
    credpool_ref: grok-native-attempt
`)
	if _, err := config.NewYAMLStore(path).Load(); err == nil {
		t.Fatal("expected error: native_passthrough on a non-native (grok) protocol")
	}
}

func TestYAMLStore_Load_NativePassthroughValidProtocolSucceeds(t *testing.T) {
	path := writeTempConfig(t, `
providers:
  anthropic: {}
credpools:
  real-anthropic:
    provider_ref: anthropic
    native_passthrough: true
    keys:
      - main: test-key
model_routes:
  - model_name: m
    upstream_model: claude-haiku-4-5-20251001
    credpool_ref: real-anthropic
`)
	cfg, err := config.NewYAMLStore(path).Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.CredPools["real-anthropic"].NativePassthrough {
		t.Error("expected native_passthrough to be preserved as true")
	}
}

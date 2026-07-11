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
    provider: gemini
    provider_model: gemini-2.5-flash
    credpool:
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
    provider: gemini
    provider_model: gemini-2.5-flash
    credpool:
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
    provider: gemini
    provider_model: gemini-2.5-flash
    credpool:
      keys:
        - ${DEFINITELY_NOT_SET_XYZ123}
`)
	_, err := config.NewYAMLStore(path).Load()
	if err == nil {
		t.Fatal("expected error for unset env var, got nil")
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
    protocol: openai
    provider_model: some-model
    api_base: https://generativelanguage.googleapis.com/v1
    auth_style: bearer
    credpool:
      keys: [test-key]
`,
			wantErr: true,
		},
		{
			name: "openai URL with gemini protocol — mismatch",
			yaml: `
model_routes:
  - model_name: m
    protocol: gemini
    provider_model: some-model
    api_base: https://api.openai.com/v1
    credpool:
      keys: [test-key]
`,
			wantErr: true,
		},
		{
			name: "deepseek URL with openai protocol — allowed (openai-compatible)",
			yaml: `
model_routes:
  - model_name: m
    protocol: openai
    provider_model: deepseek-chat
    api_base: https://api.deepseek.com/v1
    auth_style: bearer
    credpool:
      keys: [test-key]
`,
			wantErr: false,
		},
		{
			name: "custom proxy URL — unknown host, no check",
			yaml: `
model_routes:
  - model_name: m
    protocol: openai
    provider_model: some-model
    api_base: https://my-proxy.internal/v1
    auth_style: bearer
    credpool:
      keys: [test-key]
`,
			wantErr: false,
		},
		{
			name: "invalid api_base URL",
			yaml: `
model_routes:
  - model_name: m
    protocol: openai
    provider_model: some-model
    api_base: "not a url"
    auth_style: bearer
    credpool:
      keys: [test-key]
`,
			wantErr: true,
		},
		{
			name: "passthrough skips mismatch check",
			yaml: `
model_routes:
  - model_name: m
    protocol: anthropic
    provider_model: claude-haiku-4-5-20251001
    api_base: https://generativelanguage.googleapis.com/any/path
    auth_style: bearer
    credpool:
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
			{ModelName: "haiku", ProviderModel: "gemini-flash"},
			{ModelName: "haiku-mini", ProviderModel: "gemini-mini"},
			{ModelName: "opus", ProviderModel: "glm"},
			{ModelName: "sonnet-5", ProviderModel: "gemini-pro"},
			{ModelName: "gpt-5.4", ProviderModel: "deepseek"},
		},
		Server: config.ServerConfig{DefaultModel: "haiku"},
	}

	cases := []struct {
		input  string
		wantPM string // expected ProviderModel
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
		if ok && e.ProviderModel != tc.wantPM {
			t.Errorf("LookupModel(%q) providerModel=%q want %q", tc.input, e.ProviderModel, tc.wantPM)
		}
	}

	// no match without default
	cfgNoDefault := &config.Config{
		ModelRoutes: []config.ModelEntry{
			{ModelName: "haiku", ProviderModel: "gemini-flash"},
		},
	}
	if _, ok := cfgNoDefault.LookupModel("claude-opus-4-8"); ok {
		t.Error("LookupModel(claude-opus-4-8) with no match and no default should return ok=false")
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
    provider: anthropic
    provider_model: claude-3
    credpool_ref: bedrock-claude
    auth_style: sigv4
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
    auth_style: sigv4
    region: us-east-1
    service: bedrock-runtime
    keys:
      - name: prod
        access_key_id: ${TEST_AWS_ACCESS_KEY}
        secret_access_key: ${TEST_AWS_SECRET_KEY}
model_routes:
  - model_name: claude-bedrock
    provider: anthropic
    provider_model: claude-3
    credpool_ref: bedrock-claude
    auth_style: sigv4
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
    auth_style: sigv4
    region: us-east-1
    keys:
      - prod:
          access_key_id: AKIA-only
model_routes:
  - model_name: claude-bedrock
    provider: anthropic
    provider_model: claude-3
    credpool_ref: bedrock-claude
    auth_style: sigv4
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

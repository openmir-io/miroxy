package server

import (
	"testing"

	"miroxy/internal/config"
)

// TestNamedPoolProvider_ReadsFromCredPool guards the fix for ":miroxy model"
// printing "provider=-" for every credpool: provider_ref lives on the
// credpool itself (see CredPoolCfg.ProviderRef), not inferred from whichever
// model_routes entry happens to reference it.
func TestNamedPoolProvider_ReadsFromCredPool(t *testing.T) {
	cfg := &config.Config{
		CredPools: map[string]config.CredPoolCfg{
			"gemini-2.5":   {ProviderRef: "gemini"},
			"mistral-free": {ProviderRef: "mistral"},
		},
	}

	if got := namedPoolProvider("gemini-2.5", cfg); got != "gemini" {
		t.Fatalf("got %q, want %q", got, "gemini")
	}
	if got := namedPoolProvider("mistral-free", cfg); got != "mistral" {
		t.Fatalf("got %q, want %q", got, "mistral")
	}
}

func TestNamedPoolProvider_UnknownPool(t *testing.T) {
	cfg := &config.Config{
		CredPools: map[string]config.CredPoolCfg{
			"openai": {ProviderRef: "openai"},
		},
	}

	if got := namedPoolProvider("orphan-pool", cfg); got != "" {
		t.Fatalf("got %q, want empty string for an unconfigured credpool", got)
	}
}

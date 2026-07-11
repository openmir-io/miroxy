package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// ConfigStore is the read interface for application configuration.
type ConfigStore interface {
	Load() (*Config, error)
}

type Config struct {
	Server    ServerConfig             `yaml:"server"`
	Admin     AdminConfig              `yaml:"admin"`
	Log       LogConfig                `yaml:"log"`
	Auth      AuthConfig               `yaml:"auth"`
	Providers map[string]ProviderDef  `yaml:"providers"`
	KeyPools  map[string]KeyPoolCfg   `yaml:"keypools"`
	ModelRoutes []ModelEntry             `yaml:"model_routes"`
	Metrics   MetricsConfig            `yaml:"metrics"`
	Compress     CompressConfig     `yaml:"compress"`
	Dump         DumpConfig         `yaml:"dump"`
	Transparent  TransparentConfig  `yaml:"transparent"`
}

type LogConfig struct {
	Level string `yaml:"level"`
	File  string `yaml:"file"`
}

type ServerConfig struct {
	Port           int         `yaml:"port"`
	DefaultModel   string      `yaml:"default_model"`
	Commands       CommandsCfg `yaml:"commands"`
	// ModelDiscovery controls whether miroxy auto-discovers models from upstream
	// providers at startup and injects them into the in-memory model list.
	// "strict" (default): only models explicitly listed in model_routes are exposed.
	// "auto": additionally fetches models from any configured Anthropic keypool
	//         and injects those not already present in model_routes.
	ModelDiscovery string `yaml:"model_discovery"`
}

// AdminConfig controls the separate admin listener (stats, reload, key management).
// Defaults to 127.0.0.1:8090 — localhost only. Set addr explicitly to expose
// to a wider network (e.g. "0.0.0.0:8090" inside a container with network policy).
type AdminConfig struct {
	// Addr is the listen address for the admin HTTP server.
	// Default: "127.0.0.1:8090" (localhost only).
	// Set to "0.0.0.0:8090" to expose on all interfaces (requires Password).
	Addr string `yaml:"addr"`
	// Enabled controls whether the admin server starts at all.
	// Default: true
	Enabled *bool `yaml:"enabled"`
	// Password protects all /admin/* endpoints.
	// If empty, defaults to "!miroxy".
	// Override with --admin-pass flag or this field.
	Password string `yaml:"password"`
}

type AuthConfig struct {
	AllowedKeys []string `yaml:"allowed_keys"`
}

// ProviderDef defines an upstream provider's connection parameters.
// Built-in providers (gemini, openai, anthropic, deepseek, glm, grok) have
// defaults and do not need to appear in the providers block unless overriding.
type ProviderDef struct {
	BaseURL   string `yaml:"base_url"`
	Protocol  string `yaml:"protocol"`
	AuthStyle string `yaml:"auth_style"`
}

// ModelEntry defines one routable model slot.
//
// Two formats:
//
// Simple (single provider):
//
//	model_name + provider + provider_model + keypool_ref (or inline keypool)
//
// Routing (multiple providers):
//
//	model_name + routing.strategy + routing.targets[]
//
// client_protocol selects the FrontendConverter (how to read the client request).
// protocol selects the ProviderConverter (how to write the upstream request).
// When they match, miroxy forwards the request as-is (passthrough mode).
type ModelEntry struct {
	ModelName      string         `yaml:"model_name"`
	DisplayName    string         `yaml:"display_name,omitempty"` // human-readable label for /v1/models; defaults to ModelName
	Provider       string         `yaml:"provider"`
	ClientProtocol string         `yaml:"client_protocol"`
	Protocol       string         `yaml:"protocol"`
	ProviderModel  string         `yaml:"provider_model"`
	KeyPool        KeyPoolCfg     `yaml:"keypool"`
	KeypoolRef     string         `yaml:"keypool_ref"`
	Routing        *RoutingConfig `yaml:"routing"`
	Description    string         `yaml:"description"`
	TimeoutSeconds int            `yaml:"timeout_seconds"`
	Mode           string         `yaml:"mode"`
	APIBase        string         `yaml:"api_base"`
	AuthStyle      string         `yaml:"auth_style"`
	Invisible      bool           `yaml:"invisible"`
}

// RoutingConfig describes a multi-provider routing policy for one model slot.
type RoutingConfig struct {
	Strategy string          `yaml:"strategy"` // fallback | round_robin | least_requests
	Targets  []RoutingTarget `yaml:"targets"`
}

// RoutingTarget is one upstream provider within a routing entry.
// Protocol, APIBase, and AuthStyle are resolved from the providers block
// at config load time and not expected in YAML.
type RoutingTarget struct {
	Provider       string `yaml:"provider"`
	ProviderModel  string `yaml:"provider_model"`
	KeypoolRef     string `yaml:"keypool_ref"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
	// Resolved by resolveProviders — not user-facing YAML fields.
	Protocol  string `yaml:"-"`
	APIBase   string `yaml:"-"`
	AuthStyle string `yaml:"-"`
}

// KeyEntry is a single upstream API key with an optional display name.
type KeyEntry struct {
	Name string
	Key  string
}

func (e *KeyEntry) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		// Plain string: - ${ENV_VAR}  (anonymous key, gets key_N name in logs)
		e.Key = value.Value
		return nil
	case yaml.MappingNode:
		// Shorthand: - my_label: ${ENV_VAR}
		// One key-value pair where the YAML key is the display name.
		if len(value.Content) == 2 {
			keyName := value.Content[0].Value
			if keyName != "name" && keyName != "key" {
				e.Name = keyName
				e.Key = value.Content[1].Value
				return nil
			}
		}
		// Verbose: - name: my_label\n  key: ${ENV_VAR}
		type plain struct {
			Name string `yaml:"name"`
			Key  string `yaml:"key"`
		}
		var p plain
		if err := value.Decode(&p); err != nil {
			return err
		}
		e.Name = p.Name
		e.Key = p.Key
		return nil
	default:
		return fmt.Errorf("keys entry must be a string or a mapping (e.g. \"my_label: ${ENV_VAR}\")")
	}
}

type KeyPoolCfg struct {
	// Provider optionally tags this keypool for auto-discovery.
	// Accepted values: "anthropic", "openai".
	// When model_discovery: auto is set, miroxy calls the provider's
	// /v1/models endpoint at startup and injects discovered models.
	Provider string `yaml:"provider,omitempty"`

	Type         string `yaml:"type"`
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`

	Strategy              string `yaml:"strategy"`
	CircuitBreakThreshold int    `yaml:"circuit_break_threshold"`
	CooldownSeconds       int    `yaml:"cooldown_seconds"`
	Keys                  []KeyEntry `yaml:"keys"`
	RateLimitRPM          int    `yaml:"rate_limit_rpm"`
	RateSoftLimit         int    `yaml:"rate_soft_limit"`
	CredBroker            *CredBrokerConfig `yaml:"cred_broker"`
}

// CredBrokerConfig wires one external CredBroker pool into a miroxy keypool.
type CredBrokerConfig struct {
	URL            string `yaml:"url"`
	Token          string `yaml:"token"`
	Pool           string `yaml:"pool"`
	RefreshSeconds int    `yaml:"refresh_seconds"`
}

type MetricsConfig struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"`
}

// CompressConfig controls the builtin context-compression pipeline.
// All fields are optional; zero values use the defaults documented below.
type CompressConfig struct {
	// Enabled turns the CompressPlugin on. Default false (opt-in).
	Enabled bool `yaml:"enabled"`

	// Threshold is the minimum estimated token count for the whole message
	// list before compression runs. Default 4 000.
	Threshold int `yaml:"threshold"`

	// ToolResultBudget is the per-tool_result token cap that triggers
	// SmartCrusher on JSON/log content. Default 500.
	ToolResultBudget int `yaml:"tool_result_budget"`

	// TotalBudget is the whole-conversation token target.
	// SlidingWindow kicks in when the list exceeds this value. Default 80 000.
	TotalBudget int `yaml:"total_budget"`

	// WindowRecentKeep is the number of most-recent turns always preserved
	// by the SlidingWindow. Default 6.
	WindowRecentKeep int `yaml:"window_recent_keep"`

	// AlignDynamic stabilises UUIDs/timestamps/request-IDs in messages so
	// the provider's KV-cache prefix hash stays stable. Default false.
	AlignDynamic bool `yaml:"align_dynamic"`

	// CCRPath is the file path for the persistent CCR bbolt store.
	// Leave empty to use the in-memory store (session-scoped, no persistence).
	// Example: /data/ccr.db
	CCRPath string `yaml:"ccr_path"`
}

// LookupModel returns the ModelEntry matching name, then falls back to
// LookupModel resolves a client model name to a configured ModelEntry using
// a four-step cascade:
//
//  1. Exact match           — "claude-haiku-4-5-20251001" in model_routes
//  2. Strip-prefix exact    — "claude-haiku-4-5-20251001" → look up "haiku-4-5-20251001"
//  3. Longest prefix match  — "claude-haiku-4-5-20251001" matches config "haiku";
//     the remainder after the prefix must start with '-' or be empty, so
//     "haiku" never accidentally matches "haiku-mini" when "haiku-mini" is also
//     configured (longer prefix wins). Both the request name and config names
//     are normalised by stripping a leading "claude-" before comparison.
//  4. Default model fallback — cfg.Server.DefaultModel
func (c *Config) LookupModel(name string) (ModelEntry, bool) {
	// 1. Exact match.
	for _, m := range c.ModelRoutes {
		if m.ModelName == name {
			return m, true
		}
	}

	// 2. Strip "claude-" prefix, then exact match.
	normName := strings.TrimPrefix(name, "claude-")
	if normName != name {
		for _, m := range c.ModelRoutes {
			if m.ModelName == normName {
				return m, true
			}
		}
	}

	// 3. Longest prefix match (both sides normalised).
	// A valid prefix must be followed by '-' or end-of-string to prevent
	// "haiku" from matching "haiku-mini" when "haiku-mini" is also configured.
	bestIdx := -1
	bestLen := 0
	for i, m := range c.ModelRoutes {
		normConfig := strings.TrimPrefix(m.ModelName, "claude-")
		if !strings.HasPrefix(normName, normConfig) {
			continue
		}
		rest := normName[len(normConfig):]
		if rest != "" && !strings.HasPrefix(rest, "-") {
			continue // not a clean boundary (e.g. "haiku" must not match "haikux")
		}
		if len(normConfig) > bestLen {
			bestLen = len(normConfig)
			bestIdx = i
		}
	}
	if bestIdx >= 0 {
		return c.ModelRoutes[bestIdx], true
	}

	// 4. Provider passthrough: infer provider from model name pattern, find a
	//    keypool tagged with that provider. Returns a synthetic ModelEntry so
	//    the executor can use the passthroughSelectors built at startup.
	if provider := inferModelProvider(name); provider != "" {
		for poolName, pool := range c.KeyPools {
			if pool.Provider == provider && len(pool.Keys) > 0 {
				return ModelEntry{
					ModelName:     name,
					Provider:      provider,
					ProviderModel: name, // forward the original model name to the upstream
					KeypoolRef:    poolName,
				}, true
			}
		}
	}

	// 5. Default model fallback.
	if c.Server.DefaultModel != "" && c.Server.DefaultModel != name {
		for _, m := range c.ModelRoutes {
			if m.ModelName == c.Server.DefaultModel {
				return m, true
			}
		}
	}
	return ModelEntry{}, false
}

// inferModelProvider maps a model name to its likely provider.
// Returns "anthropic" for claude-* models, "openai" for gpt-*/o1*/o3*/o4* models.
// Returns "" if the pattern is unrecognised.
func inferModelProvider(name string) string {
	if strings.HasPrefix(name, "claude-") {
		return "anthropic"
	}
	for _, pfx := range []string{"gpt-", "o1", "o3", "o4", "chatgpt-"} {
		if strings.HasPrefix(name, pfx) {
			return "openai"
		}
	}
	return ""
}

// DumpConfig enables request/response capture for debugging (trace level).
// When enabled every request generates a trace_id and all stages are written
// to a JSONL file so requests and responses can be correlated 1-to-1.
type DumpConfig struct {
	// Enabled turns on dump capture. Default false.
	Enabled bool `yaml:"enabled"`
	// Path is the JSONL output file. Empty string = stdout.
	Path string `yaml:"path"`
	// IncludeSSE writes each upstream SSE event as a separate record.
	// Verbose but useful for debugging streaming protocol issues.
	IncludeSSE bool `yaml:"include_sse"`
	// MaxSizeMB rotates the dump file when it exceeds this size in MiB.
	// 0 = unlimited (no rotation). Default 10.
	MaxSizeMB int `yaml:"max_size_mb"`
	// MaxBackups is the number of rotated files to retain (dump.jsonl.1, .2, ...).
	// 0 = use default (2). Set to 1 to keep only the most recent backup.
	MaxBackups int `yaml:"max_backups"`
}

// TransparentConfig enables pure transparent proxy mode.
// All pipeline processing (auth, compress, translate) is bypassed.
// miroxy acts as a dumb reverse proxy: client request is forwarded as-is,
// upstream response is returned as-is. Only the target URL and auth header
// are rewritten. Useful for discovering undocumented API formats.
type TransparentConfig struct {
	// Enabled turns on transparent proxy mode. Default false.
	Enabled bool `yaml:"enabled"`
	// Upstream is the real API base URL to forward to.
	// Example: https://api.anthropic.com
	Upstream string `yaml:"upstream"`
	// APIKey is the upstream API key to inject.
	// Uses ${ENV_VAR} substitution like all other keys.
	APIKey string `yaml:"api_key"`
}

// CommandsCfg controls miroxy in-band commands (:miroxy ...).
type CommandsCfg struct {
	// Disabled turns off all :miroxy commands. Default false (commands enabled).
	Disabled  bool `yaml:"disabled"`
	// AllowDump permits :miroxy dump on|off. Default false.
	AllowDump bool `yaml:"allow_dump"`
}

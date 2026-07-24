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
	Server      ServerConfig           `yaml:"server"`
	Admin       AdminConfig            `yaml:"admin"`
	Log         LogConfig              `yaml:"log"`
	Auth        AuthConfig             `yaml:"auth"`
	Providers   map[string]ProviderDef `yaml:"providers"`
	CredPools   map[string]CredPoolCfg `yaml:"credpools"`
	ModelRoutes []ModelEntry           `yaml:"model_routes"`
	Metrics     MetricsConfig          `yaml:"metrics"`
	Warden      WardenConfig           `yaml:"warden"`
	Compress    CompressConfig         `yaml:"compress"`
	Dump        DumpConfig             `yaml:"dump"`
	Transparent TransparentConfig      `yaml:"transparent"`
	Sidecar     SidecarConfig          `yaml:"sidecar"`
	LocalState  LocalStateConfig       `yaml:"local_state"`

	// NativePassthroughEnable is the root safety switch for LookupModel step
	// 4 (see its doc comment) — a client's default model picker resolving to
	// a native vendor name (e.g. Claude Code choosing "claude-opus-4-8")
	// must not silently reach a real vendor credpool unless the operator
	// explicitly opts in here. Default false. Even when true, a request only
	// actually reaches a native pool if at least one credpool also sets
	// native_passthrough: true for the matching protocol.
	NativePassthroughEnable bool `yaml:"native_passthrough_enable"`
}

// SidecarConfig groups every optional external-service integration under one
// namespace. Each sidecar is independently enabled; miroxy runs with zero
// sidecars configured by default (fully standalone, in-memory only).
type SidecarConfig struct {
	CredSource CredSourceConfig `yaml:"credsource"`
	// Future sidecars (compressor, securitygate, ...) are added here as their
	// own <Domain>Config field once they have a real implementation — see
	// docs/design/architecture-v3.md for the pattern to follow.
}

// CredSourceConfig configures the optional credstone credential source.
// Disabled by default — miroxy uses local credpools only unless enabled.
type CredSourceConfig struct {
	Enabled      bool   `yaml:"enabled"`
	BaseURL      string `yaml:"base_url"`
	AuthToken    string `yaml:"auth_token"`
	SyncInterval int    `yaml:"sync_interval"` // seconds, default 300
}

// LocalStateConfig configures optional on-disk caching of local runtime state
// (currently: per-credential health — state/cooldown/failure counters — and
// warden's cumulative counters) via a small embedded buntdb store. Disabled
// by default — miroxy is fully in-memory and resets on restart unless this
// is turned on.
//
// Meaningless (and ignored, with a startup warning) when
// sidecar.credsource.enabled is true: credstone is already the
// authoritative, cross-restart source of credential health in that mode, so
// a local disk cache would just be a second, potentially-stale copy with no
// correctness benefit. This is a standalone-mode-only optimization.
type LocalStateConfig struct {
	Enabled bool `yaml:"enabled"`
	// Path is the buntdb file path. Defaults to "./miroxy-local-state.db" when
	// enabled and left blank. The file is a pure cache: if it's missing or
	// corrupt, miroxy deletes and recreates it rather than failing to start.
	Path string `yaml:"path"`
}

type LogConfig struct {
	Level string `yaml:"level"`
	File  string `yaml:"file"`
}

type ServerConfig struct {
	Port         int         `yaml:"port"`
	DefaultModel string      `yaml:"default_model"`
	Commands     CommandsCfg `yaml:"commands"`
	// ModelDiscovery controls whether miroxy auto-discovers models from upstream
	// providers at startup and injects them into the in-memory model list.
	// "strict" (default): only models explicitly listed in model_routes are exposed.
	// "auto": additionally fetches models from any configured Anthropic credpool
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
//	model_name + provider_ref + upstream_model + credpool_ref (or inline credpool)
//
// Routing (multiple providers):
//
//	model_name + routing.strategy + routing.targets[]
//
// The FrontendConverter that reads the client request is chosen structurally,
// by which HTTP path the request hit (see core/downstream.DownstreamAdapter) —
// not by config. protocol selects the ProviderConverter (how to write the
// upstream request). Whether a given attempt forwards raw bytes instead of
// running the IR transform is decided per request, per attempt, by comparing
// the request's actual client protocol against this value (see
// internal/server/upstream.go's dispatchFor) — mode: passthrough below
// forces raw forwarding unconditionally regardless of that comparison.
type ModelEntry struct {
	ModelName   string `yaml:"model_name"`
	DisplayName string `yaml:"display_name,omitempty"` // human-readable label for /v1/models; defaults to ModelName

	// ProviderRef/Protocol/APIBase/AuthStyle are resolved from this entry's
	// credpool (CredpoolRef, or the inline CredPool below) by
	// resolveProviders — not user-facing YAML fields. A credpool's keys
	// always belong to a single upstream provider, so that binding is
	// declared once, on the credpool (see CredPoolCfg.ProviderRef), never
	// duplicated here.
	ProviderRef string `yaml:"-"`
	Protocol    string `yaml:"-"`
	APIBase     string `yaml:"-"`
	AuthStyle   string `yaml:"-"`

	UpstreamModel  string         `yaml:"upstream_model"`
	CredPool       CredPoolCfg    `yaml:"credpool"`
	CredpoolRef    string         `yaml:"credpool_ref"`
	Routing        *RoutingConfig `yaml:"routing"`
	Description    string         `yaml:"description"`
	TimeoutSeconds int            `yaml:"timeout_seconds"`
	Mode           string         `yaml:"mode"`
	Invisible      bool           `yaml:"invisible"`
}

// RoutingConfig describes a multi-provider routing policy for one model slot.
type RoutingConfig struct {
	Strategy string          `yaml:"strategy"` // fallback | round_robin | least_requests
	Targets  []RoutingTarget `yaml:"targets"`

	// Sticky keeps a conversation on the same target across requests instead
	// of rotating every call; no effect on "fallback". See CredPoolCfg.Sticky.
	Sticky           bool `yaml:"sticky"`
	StickyTTLSeconds int  `yaml:"sticky_ttl_seconds"`
}

// RoutingTarget is one upstream provider within a routing entry.
// ProviderRef, Protocol, APIBase, and AuthStyle are resolved from
// CredpoolRef's credpool at config load time and not expected in YAML —
// see CredPoolCfg.ProviderRef.
type RoutingTarget struct {
	UpstreamModel  string `yaml:"upstream_model"`
	CredpoolRef    string `yaml:"credpool_ref"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
	// Resolved by resolveProviders from the credpool named by CredpoolRef —
	// not user-facing YAML fields.
	ProviderRef string `yaml:"-"`
	Protocol    string `yaml:"-"`
	APIBase     string `yaml:"-"`
	AuthStyle   string `yaml:"-"`
}

// CredEntry is a single upstream credential with an optional display name.
//
// Key holds the material for single-value kinds (API key, bearer token, or —
// for oauth_refresh pools — the refresh_token). AccessKeyID/SecretAccessKey/
// SessionToken hold SigV4 material instead, populated only when the owning
// pool's auth_style is "sigv4"; Key is left empty in that case.
type CredEntry struct {
	Name string
	Key  string

	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string // optional — empty for long-term IAM credentials
}

// IsUsable reports whether e has real credential material for authStyle —
// false for an empty value or an unexpanded ${ENV_VAR} placeholder.
func (e CredEntry) IsUsable(authStyle string) bool {
	if authStyle == "sigv4" {
		return e.AccessKeyID != "" && e.SecretAccessKey != "" &&
			!envVarRe.MatchString(e.AccessKeyID) && !envVarRe.MatchString(e.SecretAccessKey)
	}
	return e.Key != "" && !envVarRe.MatchString(e.Key)
}

func (e *CredEntry) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		// Plain string: - ${ENV_VAR}  (anonymous key, gets key_N name in logs)
		e.Key = value.Value
		return nil
	case yaml.MappingNode:
		// Shorthand: - my_label: ${ENV_VAR}
		// One key-value pair where the YAML key is the display name and the
		// value is a plain string.
		if len(value.Content) == 2 {
			keyName := value.Content[0].Value
			valueNode := value.Content[1]
			if keyName != "name" && keyName != "key" {
				if valueNode.Kind == yaml.ScalarNode {
					e.Name = keyName
					e.Key = valueNode.Value
					return nil
				}
				if valueNode.Kind == yaml.MappingNode {
					// Structured shorthand (sigv4): - my_label: {access_key_id: ..., ...}
					var fields sigv4Fields
					if err := valueNode.Decode(&fields); err != nil {
						return err
					}
					e.Name = keyName
					e.AccessKeyID = fields.AccessKeyID
					e.SecretAccessKey = fields.SecretAccessKey
					e.SessionToken = fields.SessionToken
					return nil
				}
			}
		}
		// Verbose: - name: my_label\n  key: ${ENV_VAR}
		// or (sigv4) - name: my_label\n  access_key_id: ...\n  secret_access_key: ...
		var p struct {
			Name        string `yaml:"name"`
			Key         string `yaml:"key"`
			sigv4Fields `yaml:",inline"`
		}
		if err := value.Decode(&p); err != nil {
			return err
		}
		e.Name = p.Name
		e.Key = p.Key
		e.AccessKeyID = p.AccessKeyID
		e.SecretAccessKey = p.SecretAccessKey
		e.SessionToken = p.SessionToken
		return nil
	default:
		return fmt.Errorf("keys entry must be a string or a mapping (e.g. \"my_label: ${ENV_VAR}\")")
	}
}

// sigv4Fields is the structured material for an "auth_style: sigv4" entry.
type sigv4Fields struct {
	AccessKeyID     string `yaml:"access_key_id"`
	SecretAccessKey string `yaml:"secret_access_key"`
	SessionToken    string `yaml:"session_token"`
}

type CredPoolCfg struct {
	// ProviderRef is the key into the top-level providers: block that
	// determines this pool's upstream wire protocol, base URL, and auth
	// style — a credpool's keys always belong to a single upstream
	// provider (never split across two protocols), so this is the one
	// place that binding is declared; model_routes only ever says WHICH
	// credpool to use, never which provider (see ModelEntry/RoutingTarget,
	// whose own ProviderRef is resolved from here). Required unless
	// Protocol and APIBase are both set directly below (a fully
	// self-contained pool that needs no shared providers: entry).
	ProviderRef string `yaml:"provider_ref,omitempty"`

	// NativePassthrough opts this pool into serving requests for a model
	// name that is globally unique to one vendor (claude-*/gpt-*/gemini-* —
	// see inferModelProvider) but matches no model_routes entry — see
	// LookupModel step 4. Only valid when this pool's resolved Protocol is
	// one of nativeVendorProtocols: its keys must genuinely be that
	// vendor's own API, since the client's model name is forwarded upstream
	// verbatim with no translation. Also gates model_discovery: auto (see
	// openai_discover.go/anthropic_discover.go) for the same reason — only
	// a pool holding real vendor keys can be safely queried against that
	// vendor's global /v1/models endpoint. The root native_passthrough_enable
	// switch must also be set for this to take effect. Default false.
	NativePassthrough bool `yaml:"native_passthrough,omitempty"`

	// Protocol/APIBase are resolved from ProviderRef (+ built-in defaults —
	// see provider_defaults.go) by resolveProviders when left blank, or may
	// be set directly here for a fully self-contained pool with no
	// providers: entry. Every model_routes entry or routing target that
	// references this pool inherits these values.
	Protocol string `yaml:"protocol,omitempty"`
	APIBase  string `yaml:"api_base,omitempty"`

	// AuthStyle declares this pool's credential kind: "bearer", "api_key",
	// "query_key", "sigv4", or "none". Resolved from ProviderRef when left
	// blank; required explicitly for "sigv4" pools, since sigv4 validation
	// happens at pool-definition time, before provider resolution runs.
	AuthStyle string `yaml:"auth_style,omitempty"`

	Type         string `yaml:"type"`
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`

	// Region/Service are shared SigV4 signing parameters for every key in a
	// sigv4 pool (e.g. AWS Bedrock's region + "bedrock-runtime"). Ignored for
	// any other auth_style.
	Region  string `yaml:"region,omitempty"`
	Service string `yaml:"service,omitempty"`

	Strategy              string      `yaml:"strategy"` // round_robin | least_requests | fallback
	CircuitBreakThreshold int         `yaml:"circuit_break_threshold"`
	CooldownSeconds       int         `yaml:"cooldown_seconds"`
	Keys                  []CredEntry `yaml:"keys"`
	RateLimitRPM          int         `yaml:"rate_limit_rpm"`
	RateSoftLimit         int         `yaml:"rate_soft_limit"`
	// RateLimitTPM caps total (input+output) tokens per credential per
	// minute; 0 = disabled (the default — most credentials have no
	// provider-side token quota worth tracking locally).
	RateLimitTPM int `yaml:"rate_limit_tpm"`

	// Sticky keeps a conversation on the same credential (cache hit rate)
	// instead of rotating every call. Orthogonal to Strategy. Default false.
	Sticky bool `yaml:"sticky"`
	// StickyTTLSeconds is how long an idle conversation's binding survives.
	// Default 1800 (30m) when Sticky is true and this is left at 0.
	StickyTTLSeconds int `yaml:"sticky_ttl_seconds"`

	// RoundRobinBatchSize: use each credential this many calls before
	// rotating — a global counter, not per-conversation. Default 1.
	RoundRobinBatchSize int `yaml:"round_robin_batch_size"`
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

// WardenConfig controls the builtin content-defense pipeline: secret/PII
// detection, prompt-injection and jailbreak phrase matching, and reversible
// tokenization. All fields are optional; zero values use the defaults
// documented below.
type WardenConfig struct {
	// Enabled turns WardenPlugin on. Default false (opt-in).
	Enabled bool `yaml:"enabled"`

	// Mode selects how Redact/Block-verdict findings are rewritten:
	// "redact" (destructive masking, the default), "tokenize" (reversible
	// vault placeholders restored in the response), or "block_only" (never
	// rewrite — a Block verdict still halts the request either way).
	Mode string `yaml:"mode"`

	// Secrets/PII/Injection/Jailbreak toggle each detector independently.
	// Default true for all four when Warden itself is enabled.
	Secrets   *bool `yaml:"secrets,omitempty"`
	PII       *bool `yaml:"pii,omitempty"`
	Injection *bool `yaml:"injection,omitempty"`
	Jailbreak *bool `yaml:"jailbreak,omitempty"`

	// FailClosed, when true, blocks a request outright if a detector
	// errors (e.g. a recovered panic) instead of passing it through
	// best-effort. Default false — a brand-new detector failing closed by
	// default risks false-positive outages more than it buys safety.
	FailClosed bool `yaml:"fail_closed"`
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

	// 4. Native vendor passthrough: the model name is globally unique to one
	//    vendor (claude-*/gpt-*/gemini-* — see inferModelProvider) but
	//    matched no model_routes entry. Only engages when
	//    native_passthrough_enable is set and at least one credpool opted
	//    in via native_passthrough: true for that vendor's protocol.
	//    Returns a synthetic ModelEntry with no CredpoolRef — actual
	//    selection (and round-robin across multiple such pools) happens via
	//    the PassthroughSelectors built in buildRoutingState, keyed by
	//    protocol and looked up through this entry's ProviderRef (see
	//    router.BuiltinRouter.Route).
	if c.NativePassthroughEnable {
		if provider := inferModelProvider(name); provider != "" && c.hasNativePassthroughPool(provider) {
			return ModelEntry{
				ModelName:     name,
				ProviderRef:   provider,
				UpstreamModel: name, // forward the original model name to the upstream
			}, true
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

// nativeVendorProtocols is the fixed set of protocols inferModelProvider can
// return — the only protocols a credpool may set native_passthrough: true
// for (see CredPoolCfg.NativePassthrough). Single source of truth for "these
// three vendors have a globally unique model-naming convention"; extending
// inferModelProvider to recognize a new vendor must add its protocol here
// too, so both stay in lockstep rather than drifting into two subtly
// different enums.
var nativeVendorProtocols = map[string]bool{
	"anthropic": true,
	"openai":    true,
	"gemini":    true,
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
	if strings.HasPrefix(name, "gemini-") {
		return "gemini"
	}
	return ""
}

// hasNativePassthroughPool reports whether any credpool opted into
// native_passthrough for protocol and has at least one usable key — see
// LookupModel step 4.
func (c *Config) hasNativePassthroughPool(protocol string) bool {
	for _, kp := range c.CredPools {
		if kp.NativePassthrough && kp.Protocol == protocol && len(kp.Keys) > 0 {
			return true
		}
	}
	return false
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
	Disabled bool `yaml:"disabled"`
	// AllowDump permits :miroxy dump on|off. Default false.
	AllowDump bool `yaml:"allow_dump"`
}

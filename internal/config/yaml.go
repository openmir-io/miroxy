package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

var envVarRe = regexp.MustCompile(`\$\{([^}]+)\}`)

// YAMLStore implements ConfigStore by reading a YAML file from disk.
type YAMLStore struct {
	path string
}

func NewYAMLStore(path string) *YAMLStore {
	return &YAMLStore{path: path}
}

func (s *YAMLStore) Load() (*Config, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", s.path, err)
	}

	expanded := expandEnvVars(raw)

	var cfg Config
	if err := yaml.Unmarshal(expanded, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", s.path, err)
	}

	if err := resolveProviders(&cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	applyConfigDefaults(&cfg)

	if err := validateConfig(&cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}

func expandEnvVars(data []byte) []byte {
	return envVarRe.ReplaceAllFunc(data, func(match []byte) []byte {
		name := string(match[2 : len(match)-1])
		if val, ok := os.LookupEnv(name); ok {
			return []byte(val)
		}
		return match
	})
}

// LoadFromBytesWithEnv parses a config from raw YAML bytes.
// ${VAR_NAME} placeholders are substituted from extraEnv first, then os.LookupEnv.
// Use this for the setup-mode import flow where secrets come from an uploaded file.
func LoadFromBytesWithEnv(data []byte, extraEnv map[string]string) (*Config, error) {
	expanded := expandEnvVarsWithMap(data, extraEnv)
	var cfg Config
	if err := yaml.Unmarshal(expanded, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := resolveProviders(&cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	if err := validateConfig(&cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return &cfg, nil
}

func expandEnvVarsWithMap(data []byte, extra map[string]string) []byte {
	return envVarRe.ReplaceAllFunc(data, func(match []byte) []byte {
		name := string(match[2 : len(match)-1])
		if extra != nil {
			if val, ok := extra[name]; ok {
				return []byte(val)
			}
		}
		if val, ok := os.LookupEnv(name); ok {
			return []byte(val)
		}
		return match
	})
}

// resolveProviders resolves each credpool's Protocol/APIBase/AuthStyle from
// its ProviderRef (via the providers: block + built-in defaults from
// provider_defaults.go), then propagates those resolved values onto every
// model_routes entry/routing target that references the pool. A credpool's
// keys always belong to a single upstream provider, so this is the one
// place that binding is decided — model_routes only ever says WHICH
// credpool to use, never which provider.
func resolveProviders(cfg *Config) error {
	// lookupProvider checks only cfg.Providers — built-in defaults are applied
	// after a successful lookup via fillProviderDefaults, not as a fallback.
	lookupProvider := func(name string) (ProviderDef, bool) {
		if cfg.Providers != nil {
			if pd, ok := cfg.Providers[name]; ok {
				fillProviderDefaults(&pd, name)
				return pd, true
			}
		}
		return ProviderDef{}, false
	}

	// resolvePool fills kp.Protocol/APIBase/AuthStyle from kp.ProviderRef.
	// Having no ProviderRef is allowed only when Protocol and APIBase are
	// both set directly (a fully self-contained pool needing no shared
	// providers: entry).
	resolvePool := func(context string, kp *CredPoolCfg) error {
		if kp.ProviderRef == "" {
			if kp.Protocol == "" && kp.APIBase == "" {
				return fmt.Errorf("%s: provider_ref is required — "+
					"add a providers.<name> entry and set provider_ref to it (fields can be empty to use built-in defaults), "+
					"or set protocol and api_base directly on this credpool", context)
			}
			return nil
		}
		pd, ok := lookupProvider(kp.ProviderRef)
		if !ok {
			return fmt.Errorf("%s: provider_ref %q is not declared in the providers block — "+
				"add a providers.%s entry (fields can be empty to use built-in defaults), "+
				"or set protocol and api_base directly on this credpool",
				context, kp.ProviderRef, kp.ProviderRef)
		}
		if kp.Protocol == "" {
			kp.Protocol = pd.Protocol
		}
		if kp.APIBase == "" {
			kp.APIBase = pd.BaseURL
		}
		if kp.AuthStyle == "" {
			kp.AuthStyle = pd.AuthStyle
		}
		return nil
	}

	for name, kp := range cfg.CredPools {
		if err := resolvePool(fmt.Sprintf("credpools.%s", name), &kp); err != nil {
			return err
		}
		cfg.CredPools[name] = kp
	}

	for i := range cfg.ModelRoutes {
		m := &cfg.ModelRoutes[i]

		if m.Routing != nil {
			// Routing entry: propagate each target's own credpool.
			for j := range m.Routing.Targets {
				t := &m.Routing.Targets[j]
				kp, ok := cfg.CredPools[t.CredpoolRef]
				if !ok {
					continue // validateRoutingEntry reports the unknown credpool_ref
				}
				t.ProviderRef = kp.ProviderRef
				t.Protocol = kp.Protocol
				t.APIBase = kp.APIBase
				t.AuthStyle = kp.AuthStyle
			}
			continue
		}

		if m.CredpoolRef != "" {
			kp, ok := cfg.CredPools[m.CredpoolRef]
			if !ok {
				continue // validateConfig reports the unknown credpool_ref
			}
			m.ProviderRef = kp.ProviderRef
			m.Protocol = kp.Protocol
			m.APIBase = kp.APIBase
			m.AuthStyle = kp.AuthStyle
			continue
		}

		// Inline credpool.
		if err := resolvePool(fmt.Sprintf("model_routes[%d] %q credpool", i, m.ModelName), &m.CredPool); err != nil {
			return err
		}
		m.ProviderRef = m.CredPool.ProviderRef
		m.Protocol = m.CredPool.Protocol
		m.APIBase = m.CredPool.APIBase
		m.AuthStyle = m.CredPool.AuthStyle
	}
	return nil
}

var validLogLevels = map[string]bool{"debug": true, "info": true, "warn": true, "error": true, "fatal": true, "": true}

func validateConfig(cfg *Config) error {
	if !validLogLevels[cfg.Log.Level] {
		return fmt.Errorf("log.level %q is invalid — must be one of: debug, info, warn, error, fatal", cfg.Log.Level)
	}
	if cfg.Sidecar.CredSource.Enabled {
		if err := validateCredSource(cfg.Sidecar.CredSource); err != nil {
			return err
		}
	}
	for i, k := range cfg.Auth.AllowedKeys {
		if envVarRe.MatchString(k) {
			return fmt.Errorf("auth.allowed_keys[%d] has unexpanded placeholder %q (env var not set)", i, k)
		}
	}
	if len(cfg.ModelRoutes) == 0 {
		return fmt.Errorf("model_routes must have at least one entry")
	}
	var totalUsableKeys int

	// Build set of model names for cross-reference validation.
	modelNames := make(map[string]bool, len(cfg.ModelRoutes))
	for _, m := range cfg.ModelRoutes {
		modelNames[m.ModelName] = true
	}

	// Warn when both "modelA" and "claude-modelA" are configured: the claude-
	// prefix is auto-added for gateway discovery, so explicit claude- entries
	// shadow the auto-prefixed version and may cause confusing routing behaviour.
	for _, m := range cfg.ModelRoutes {
		if strings.HasPrefix(m.ModelName, "claude-") {
			bare := strings.TrimPrefix(m.ModelName, "claude-")
			if modelNames[bare] {
				slog.Warn("model_routes naming conflict: both names resolve to the same gateway ID — remove one",
					"explicit", m.ModelName, "bare", bare, "gateway_id", m.ModelName)
			}
		}
	}

	// Validate default_model reference.
	if cfg.Server.DefaultModel != "" && !modelNames[cfg.Server.DefaultModel] {
		return fmt.Errorf("server.default_model %q does not match any model_name in model_routes", cfg.Server.DefaultModel)
	}

	// Build set of named credpool names.
	credpoolNames := make(map[string]bool, len(cfg.CredPools))
	for name := range cfg.CredPools {
		credpoolNames[name] = true
	}

	for i := range cfg.ModelRoutes {
		m := &cfg.ModelRoutes[i]
		if m.ModelName == "" {
			return fmt.Errorf("model_routes[%d]: model_name is required", i)
		}

		if m.Routing != nil {
			if err := validateRoutingEntry(i, m, credpoolNames); err != nil {
				return err
			}
			continue
		}

		// Simple entry: needs upstream_model.
		if m.UpstreamModel == "" {
			return fmt.Errorf("model_routes[%d] %q: upstream_model is required", i, m.ModelName)
		}

		if m.CredpoolRef != "" {
			// References a named pool — skip inline key validation.
			if !credpoolNames[m.CredpoolRef] {
				return fmt.Errorf("model_routes[%d] %q: credpool_ref %q not found in credpools block", i, m.ModelName, m.CredpoolRef)
			}
		} else {
			// Inline credpool — validate keys.
			if len(m.CredPool.Keys) == 0 {
				return fmt.Errorf("model_routes[%d] %q: credpool.keys must have at least one entry (or use credpool_ref)", i, m.ModelName)
			}
			usable, err := validateKeys(i, m.ModelName, m.CredPool.Keys, m.AuthStyle)
			if err != nil {
				return err
			}
			totalUsableKeys += usable
		}
		if !validCredPoolStrategies[m.CredPool.Strategy] {
			return fmt.Errorf("model_routes[%d] %q: credpool.strategy %q is invalid (round_robin | least_requests | fallback)", i, m.ModelName, m.CredPool.Strategy)
		}

		if err := validateAPIBase(i, m); err != nil {
			return err
		}
	}

	// Validate named credpools. A credpool may have zero local keys only when
	// the global credsource fallback is enabled to supply credentials for it.
	for name, kp := range cfg.CredPools {
		if kp.AuthStyle != "" && !validAuthStyles[kp.AuthStyle] {
			return fmt.Errorf("credpools.%s: auth_style %q is invalid — accepted: %s",
				name, kp.AuthStyle, joinKeys(validAuthStyles))
		}
		if kp.RoundRobinBatchSize < 0 {
			return fmt.Errorf("credpools.%s: round_robin_batch_size must be >= 0, got %d", name, kp.RoundRobinBatchSize)
		}
		if !validCredPoolStrategies[kp.Strategy] {
			return fmt.Errorf("credpools.%s: strategy %q is invalid (round_robin | least_requests | fallback)", name, kp.Strategy)
		}
		if len(kp.Keys) == 0 && !cfg.Sidecar.CredSource.Enabled {
			return fmt.Errorf("credpools.%s: must have at least one key (or enable credsource)", name)
		}
		usable, err := validateKeys(-1, "credpools."+name, kp.Keys, kp.AuthStyle)
		if err != nil {
			return err
		}
		totalUsableKeys += usable
		if kp.Protocol != "" && !validProtocols[kp.Protocol] {
			return fmt.Errorf("credpools.%s: protocol %q is invalid — accepted: %s",
				name, kp.Protocol, joinKeys(validProtocols))
		}
		if kp.NativePassthrough && !nativeVendorProtocols[kp.Protocol] {
			return fmt.Errorf("credpools.%s: native_passthrough requires protocol to be one of %s (resolved protocol is %q)",
				name, joinKeys(nativeVendorProtocols), kp.Protocol)
		}
		// Named pools may have no model_routes reference at all (e.g. a
		// native-passthrough-only pool — see LookupModel step 4), so this is
		// the only place their api_base/protocol consistency gets checked.
		if err := checkAPIBaseHostConsistency(fmt.Sprintf("credpools.%s", name), kp.APIBase, kp.Protocol); err != nil {
			return err
		}
	}

	// Validate protocol and auth_style on providers block and model_routes.
	// resolveProviders has already filled defaults from built-in provider definitions,
	// so any remaining unknown value was set explicitly and is a config error.
	for name, pd := range cfg.Providers {
		if pd.Protocol != "" && !validProtocols[pd.Protocol] {
			return fmt.Errorf("providers.%s: protocol %q is invalid — accepted: %s",
				name, pd.Protocol, joinKeys(validProtocols))
		}
		if pd.AuthStyle != "" && !validAuthStyles[pd.AuthStyle] {
			return fmt.Errorf("providers.%s: auth_style %q is invalid — accepted: %s",
				name, pd.AuthStyle, joinKeys(validAuthStyles))
		}
	}

	for i, m := range cfg.ModelRoutes {
		if m.Protocol != "" && !validProtocols[m.Protocol] {
			return fmt.Errorf("model_routes[%d] %q: protocol %q is invalid — accepted: %s",
				i, m.ModelName, m.Protocol, joinKeys(validProtocols))
		}
		if m.AuthStyle != "" && !validAuthStyles[m.AuthStyle] {
			return fmt.Errorf("model_routes[%d] %q: auth_style %q is invalid — accepted: %s",
				i, m.ModelName, m.AuthStyle, joinKeys(validAuthStyles))
		}
		if m.Routing != nil {
			for j, t := range m.Routing.Targets {
				if t.Protocol != "" && !validProtocols[t.Protocol] {
					return fmt.Errorf("model_routes[%d] %q: routing.targets[%d]: protocol %q is invalid — accepted: %s",
						i, m.ModelName, j, t.Protocol, joinKeys(validProtocols))
				}
				if t.AuthStyle != "" && !validAuthStyles[t.AuthStyle] {
					return fmt.Errorf("model_routes[%d] %q: routing.targets[%d]: auth_style %q is invalid — accepted: %s",
						i, m.ModelName, j, t.AuthStyle, joinKeys(validAuthStyles))
				}
			}
		}
	}

	if totalUsableKeys == 0 && !cfg.Sidecar.CredSource.Enabled {
		return fmt.Errorf("no credential key anywhere in config.yaml or the environment has a real value — miroxy cannot authenticate any request (enable sidecar.credsource, or set at least one key's env var)")
	}

	return nil
}

// applyConfigDefaults fills fields that are empty/zero with sensible defaults.
// Called after resolveProviders, before validateConfig.
func applyConfigDefaults(cfg *Config) {
	if cfg.Server.Port == 0 {
		cfg.Server.Port = DefaultProxyPort
	}
	if cfg.Admin.Addr == "" {
		cfg.Admin.Addr = DefaultAdminAddr
	}
	if cfg.Log.File == "" {
		cfg.Log.File = DefaultLogFile
	}
	if cfg.Dump.Path == "" {
		cfg.Dump.Path = DefaultDumpPath
	}
	if cfg.Sidecar.CredSource.SyncInterval == 0 {
		cfg.Sidecar.CredSource.SyncInterval = 300
	}
	if cfg.Warden.Enabled {
		if cfg.Warden.Mode == "" {
			cfg.Warden.Mode = "redact"
		}
		boolDefaultTrue(&cfg.Warden.Secrets)
		boolDefaultTrue(&cfg.Warden.PII)
		boolDefaultTrue(&cfg.Warden.Injection)
		boolDefaultTrue(&cfg.Warden.Jailbreak)
	}
	for name, kp := range cfg.CredPools {
		if kp.Sticky && kp.StickyTTLSeconds <= 0 {
			kp.StickyTTLSeconds = defaultStickyTTLSeconds
			cfg.CredPools[name] = kp
		}
	}
	for i := range cfg.ModelRoutes {
		r := cfg.ModelRoutes[i].Routing
		if r != nil && r.Sticky && r.StickyTTLSeconds <= 0 {
			r.StickyTTLSeconds = defaultStickyTTLSeconds
		}
	}
}

// defaultStickyTTLSeconds is how long an idle conversation's sticky binding
// survives when sticky is enabled but sticky_ttl_seconds is left at 0.
const defaultStickyTTLSeconds = 1800

// boolDefaultTrue sets *p to true when the config author omitted the field
// (nil) — used for WardenConfig's per-detector toggles, which default on
// once Warden itself is enabled.
func boolDefaultTrue(p **bool) {
	if *p == nil {
		v := true
		*p = &v
	}
}

var (
	validProtocols = map[string]bool{
		"gemini": true, "openai": true, "anthropic": true,
		"deepseek": true, "glm": true, "grok": true, "bedrock": true,
	}
	validAuthStyles = map[string]bool{
		"bearer": true, "api_key": true, "none": true, "query_key": true, "sigv4": true,
	}
	validCredPoolStrategies = map[string]bool{
		"round_robin": true, "least_requests": true, "fallback": true, "": true,
	}
)

// joinKeys returns the sorted keys of a bool map for error messages.
func joinKeys(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := ""
	for i, k := range keys {
		if i > 0 {
			out += ", "
		}
		out += k
	}
	return out
}

func validateRoutingEntry(idx int, m *ModelEntry, credpoolNames map[string]bool) error {
	r := m.Routing
	if len(r.Targets) == 0 {
		return fmt.Errorf("model_routes[%d] %q: routing.targets must have at least one entry", idx, m.ModelName)
	}
	validStrategies := map[string]bool{"fallback": true, "round_robin": true, "least_requests": true, "": true}
	if !validStrategies[r.Strategy] {
		return fmt.Errorf("model_routes[%d] %q: routing.strategy %q is invalid (fallback | round_robin | least_requests)", idx, m.ModelName, r.Strategy)
	}
	if r.Sticky && (r.Strategy == "fallback" || r.Strategy == "") {
		slog.Warn("routing.sticky has no effect with strategy fallback (targets are already tried in fixed order)",
			"model_name", m.ModelName)
	}
	for j, t := range r.Targets {
		if t.UpstreamModel == "" {
			return fmt.Errorf("model_routes[%d] %q: routing.targets[%d].upstream_model is required", idx, m.ModelName, j)
		}
		if t.CredpoolRef == "" {
			return fmt.Errorf("model_routes[%d] %q: routing.targets[%d].credpool_ref is required", idx, m.ModelName, j)
		}
		if !credpoolNames[t.CredpoolRef] {
			return fmt.Errorf("model_routes[%d] %q: routing.targets[%d].credpool_ref %q not found in credpools block", idx, m.ModelName, j, t.CredpoolRef)
		}
	}
	return nil
}

// validateKeys checks a credpool's key entries. authStyle selects which
// fields are required: "sigv4" needs AccessKeyID+SecretAccessKey per entry
// (SessionToken optional); everything else needs the plain Key string.
// validateKeys checks name uniqueness (a hard error) and reports, via
// usable, how many of keys actually have real credential material. An
// individual empty or unexpanded-${ENV_VAR} key only warns — the pool may
// still be viable on its other keys; validateConfig hard-errors only when
// the usable count across the entire config is zero.
func validateKeys(idx int, label string, keys []CredEntry, authStyle string) (usable int, err error) {
	seen := make(map[string]bool, len(keys))
	for j := range keys {
		k := &keys[j]
		if k.Name == "" {
			k.Name = fmt.Sprintf("key_%d", j)
		}
		if seen[k.Name] {
			return usable, fmt.Errorf("%s: duplicate key name %q — names must be unique within a pool", label, k.Name)
		}
		seen[k.Name] = true

		if k.IsUsable(authStyle) {
			usable++
			continue
		}
		slog.Warn("credential key has no usable value — skipped (check env var is set)",
			"pool", label, "key", k.Name)
	}
	return usable, nil
}

// validateCredSource checks the global credsource block. Only called when
// enabled — an absent or disabled block is always valid (default off).
func validateCredSource(cs CredSourceConfig) error {
	if cs.BaseURL == "" {
		return fmt.Errorf("credsource.base_url is required when credsource.enabled is true")
	}
	u, err := url.Parse(cs.BaseURL)
	if err != nil {
		return fmt.Errorf("credsource.base_url %q is not a valid URL: %w", cs.BaseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("credsource.base_url %q must use http or https", cs.BaseURL)
	}
	if envVarRe.MatchString(cs.AuthToken) {
		return fmt.Errorf("credsource.auth_token has unexpanded placeholder %q (env var not set)", cs.AuthToken)
	}
	if cs.AuthToken == "" {
		slog.Warn("credsource.auth_token is empty — credstone requests will be unauthenticated")
	}
	return nil
}

// validateAPIBase checks that api_base is a well-formed URL and is not a known
// canonical endpoint for a different protocol than configured.
// validateAPIBase checks that a model_routes entry's resolved api_base is
// not a known canonical endpoint for a different protocol than resolved.
// mode: passthrough skips this — a forced-passthrough target may
// deliberately point at a non-standard endpoint (e.g. AWS Bedrock's own URL
// shape).
func validateAPIBase(idx int, m *ModelEntry) error {
	if m.Mode == "passthrough" {
		return nil
	}
	return checkAPIBaseHostConsistency(fmt.Sprintf("model_routes[%d] %q", idx, m.ModelName), m.APIBase, m.Protocol)
}

// checkAPIBaseHostConsistency reports an error if apiBase's host is a known
// canonical endpoint for a protocol other than proto. Shared by
// validateAPIBase (per model_routes entry) and the named-credpool
// validation loop — a credpool may have no model_routes reference at all
// (e.g. a native-passthrough-only pool — see LookupModel step 4), so the
// credpool loop is the only place that would otherwise catch this.
func checkAPIBaseHostConsistency(context, apiBase, proto string) error {
	if apiBase == "" {
		return nil
	}
	u, err := url.Parse(apiBase)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("%s: api_base %q is not a valid URL (need scheme://host...)", context, apiBase)
	}
	if proto == "" {
		proto = "gemini"
	}

	type hostRule struct {
		hostSubstr string
		allowed    []string
	}
	rules := []hostRule{
		{"generativelanguage.googleapis.com", []string{"gemini"}},
		{"aiplatform.googleapis.com", []string{"gemini"}},
		{"api.openai.com", []string{"openai"}},
		{"api.deepseek.com", []string{"deepseek", "openai"}},
		{"api.x.ai", []string{"grok"}},
		{"open.bigmodel.cn", []string{"glm"}},
		{"api.z.ai", []string{"glm"}},
	}
	host := strings.ToLower(u.Hostname())
	for _, rule := range rules {
		if !strings.Contains(host, rule.hostSubstr) {
			continue
		}
		for _, p := range rule.allowed {
			if proto == p {
				return nil
			}
		}
		return fmt.Errorf(
			"%s: api_base %q is a %v endpoint but protocol resolves to %q — fix protocol or api_base",
			context, apiBase, rule.allowed, proto,
		)
	}
	return nil
}

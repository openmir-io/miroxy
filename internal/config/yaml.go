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

// resolveProviders fills Protocol, APIBase, and AuthStyle on model entries and
// routing targets from the providers block. Built-in defaults from
// provider_defaults.go fill any fields the operator left blank.
//
// Every provider referenced in model_routes MUST appear in the providers block —
// relying on implicit built-in defaults is not permitted. An entry with all
// fields empty is valid (defaults fill them in), but the key must be present.
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

	for i := range cfg.ModelRoutes {
		m := &cfg.ModelRoutes[i]

		if m.Routing != nil {
			// Routing entry: resolve each target.
			for j := range m.Routing.Targets {
				t := &m.Routing.Targets[j]
				if t.Provider == "" {
					continue
				}
				pd, ok := lookupProvider(t.Provider)
				if !ok {
					return fmt.Errorf("model_routes[%d] %q: routing target[%d] references unknown provider %q", i, m.ModelName, j, t.Provider)
				}
				if t.Protocol == "" {
					t.Protocol = pd.Protocol
				}
				if t.APIBase == "" {
					t.APIBase = pd.BaseURL
				}
				if t.AuthStyle == "" {
					t.AuthStyle = pd.AuthStyle
				}
			}
			continue
		}

		// Simple or inline-credpool entry.
		if m.Provider == "" {
			continue
		}
		pd, ok := lookupProvider(m.Provider)
		if !ok {
			// Provider not declared in providers block.
			// Allowed only when api_base + protocol are set directly on the entry
			// (fully self-contained route that needs no provider definition).
			if m.Protocol == "" && m.APIBase == "" {
				return fmt.Errorf("model_routes[%d] %q: provider %q is not declared in the providers block — "+
					"add a providers.%s entry (fields can be empty to use built-in defaults), "+
					"or set api_base and protocol directly on this model_routes entry",
					i, m.ModelName, m.Provider, m.Provider)
			}
			continue
		}
		if m.Protocol == "" {
			m.Protocol = pd.Protocol
		}
		if m.APIBase == "" {
			m.APIBase = pd.BaseURL
		}
		if m.AuthStyle == "" {
			m.AuthStyle = pd.AuthStyle
		}
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

		// Simple entry: needs provider_model.
		if m.ProviderModel == "" {
			return fmt.Errorf("model_routes[%d] %q: provider_model is required", i, m.ModelName)
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
			if err := validateKeys(i, m.ModelName, m.CredPool.Keys, m.AuthStyle); err != nil {
				return err
			}
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
		if len(kp.Keys) == 0 && !cfg.Sidecar.CredSource.Enabled {
			return fmt.Errorf("credpools.%s: must have at least one key (or enable credsource)", name)
		}
		if err := validateKeys(-1, "credpools."+name, kp.Keys, kp.AuthStyle); err != nil {
			return err
		}
		if kp.Provider != "" && !validProviderTags[kp.Provider] {
			return fmt.Errorf("credpools.%s: provider %q is invalid — accepted values: %s",
				name, kp.Provider, joinKeys(validProviderTags))
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
}

var (
	validProtocols = map[string]bool{
		"gemini": true, "openai": true, "anthropic": true,
		"deepseek": true, "glm": true, "grok": true,
	}
	validAuthStyles = map[string]bool{
		"bearer": true, "api_key": true, "none": true, "query_key": true, "sigv4": true,
	}
	validProviderTags = map[string]bool{
		"anthropic": true, "openai": true,
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
	for j, t := range r.Targets {
		if t.Provider == "" {
			return fmt.Errorf("model_routes[%d] %q: routing.targets[%d].provider is required", idx, m.ModelName, j)
		}
		if t.ProviderModel == "" {
			return fmt.Errorf("model_routes[%d] %q: routing.targets[%d].provider_model is required", idx, m.ModelName, j)
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
func validateKeys(idx int, label string, keys []CredEntry, authStyle string) error {
	seen := make(map[string]bool, len(keys))
	for j := range keys {
		k := &keys[j]
		if k.Name == "" {
			k.Name = fmt.Sprintf("key_%d", j)
		}
		if seen[k.Name] {
			return fmt.Errorf("%s: duplicate key name %q — names must be unique within a pool", label, k.Name)
		}
		seen[k.Name] = true

		if authStyle == "sigv4" {
			if k.AccessKeyID == "" || k.SecretAccessKey == "" {
				return fmt.Errorf("%s: keys[%d] %q: sigv4 entries require access_key_id and secret_access_key", label, j, k.Name)
			}
			for _, v := range []string{k.AccessKeyID, k.SecretAccessKey, k.SessionToken} {
				if envVarRe.MatchString(v) {
					return fmt.Errorf("%s: keys[%d] %q: unexpanded placeholder %q (env var not set)", label, j, k.Name, v)
				}
			}
			continue
		}

		if k.Key == "" {
			return fmt.Errorf("%s: keys[%d] %q: key is empty (check env var is set)", label, j, k.Name)
		}
		if envVarRe.MatchString(k.Key) {
			return fmt.Errorf("%s: keys[%d] %q: unexpanded placeholder %q (env var not set)", label, j, k.Name, k.Key)
		}
	}
	return nil
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
func validateAPIBase(idx int, m *ModelEntry) error {
	if m.APIBase == "" {
		return nil
	}
	u, err := url.Parse(m.APIBase)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("model_routes[%d] %q: api_base %q is not a valid URL (need scheme://host...)", idx, m.ModelName, m.APIBase)
	}

	proto := m.Protocol
	if proto == "" {
		proto = m.Provider
	}
	if proto == "" {
		proto = "gemini"
	}

	clientProto := m.ClientProtocol
	if clientProto == "" {
		clientProto = "anthropic"
	}
	if m.Mode == "passthrough" || clientProto == proto {
		return nil
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
			"model_routes[%d] %q: api_base %q is a %v endpoint but protocol resolves to %q — fix protocol or api_base",
			idx, m.ModelName, m.APIBase, rule.allowed, proto,
		)
	}
	return nil
}

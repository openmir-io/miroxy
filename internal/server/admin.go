package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"miroxy/internal/config"
)

// compressSnapshot returns a JSON-friendly map of compress stats, or
// {"enabled": false} when compression is disabled.
func (s *Server) compressSnapshot() map[string]any {
	if s.compressStats == nil {
		return map[string]any{"enabled": false}
	}
	snap := s.compressStats.Snapshot()
	reductionPct := snap.ReductionPct()
	return map[string]any{
		"enabled":           true,
		"requests":          snap.Requests,
		"original_tokens":   snap.OriginalTokens,
		"compressed_tokens": snap.CompressedTokens,
		"saved_tokens":      snap.SavedTokens,
		"reduction_pct":     reductionPct,
		"latency_p50_ms":    float64(snap.LatencyP50Us) / 1000,
		"latency_p95_ms":    float64(snap.LatencyP95Us) / 1000,
		"latency_max_ms":    float64(snap.LatencyMaxUs) / 1000,
	}
}

// wardenSnapshot returns a JSON-friendly map of warden stats, or
// {"enabled": false} when warden is disabled.
func (s *Server) wardenSnapshot() map[string]any {
	if s.wardenStats == nil {
		return map[string]any{"enabled": false}
	}
	snap := s.wardenStats.Snapshot()
	return map[string]any{
		"enabled":            true,
		"requests_inspected": snap.RequestsInspected,
		"secrets_found":      snap.SecretsFound,
		"pii_found":          snap.PIIFound,
		"injections_blocked": snap.InjectionsBlocked,
		"jailbreaks_blocked": snap.JailbreaksBlocked,
		"tokens_vaulted":     snap.TokensVaulted,
		"by_type":            snap.ByType,
	}
}

// adminGuard enforces password-based access on all /admin/* routes.
// Static UI files (/, *.js, *.css) are always served so the browser can load
// the login form even before authentication.
// A random session token is generated per-process; login trades the password
// for this token, which the UI stores in localStorage.
type adminGuard struct {
	password string
	token    string
}

func newAdminGuard(password string) *adminGuard {
	if password == "" {
		password = "!miroxy"
	}
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return &adminGuard{password: password, token: hex.EncodeToString(b)}
}

// checkToken returns true when the request carries a valid admin session token.
// Accepts either X-Admin-Token header or Authorization: Bearer <token>.
func (g *adminGuard) checkToken(r *http.Request) bool {
	if r.Header.Get("X-Admin-Token") == g.token {
		return true
	}
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return bearer == g.token
}

func (g *adminGuard) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/admin/") && r.URL.Path != "/admin/login" {
			if !g.checkToken(r) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (g *adminGuard) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Password != g.password {
		writeAdminJSON(w, http.StatusUnauthorized, map[string]string{"error": "wrong password"})
		return
	}
	writeAdminJSON(w, http.StatusOK, map[string]string{"token": g.token})
}

// AdminHandler returns an http.Handler for the admin API.
// Endpoints mirror the ConnectRPC path convention used by cmd/miroxy/admin.go.
//
// All endpoints accept POST with application/json (ConnectRPC JSON mode).
// Authentication is intentionally absent — bind admin to localhost only.
func (s *Server) AdminHandler() http.Handler {
	pass := s.cfg.Load().Admin.Password
	guard := newAdminGuard(pass)

	mux := http.NewServeMux()

	// Auth — no token required.
	mux.HandleFunc("POST /admin/login", guard.handleLogin)

	// ConnectRPC endpoints (used by miroxy CLI sub-commands).
	mux.HandleFunc("POST /miroxy.v1.AdminService/GetHealth", s.adminHealth)
	mux.HandleFunc("POST /miroxy.v1.AdminService/GetStatus", s.adminStatus)
	mux.HandleFunc("POST /miroxy.v1.AdminService/Reload", s.adminReload)

	// Convenience REST endpoints (curl / browser / UI).
	mux.HandleFunc("GET /health", s.adminHealth)
	mux.HandleFunc("GET /stat", s.adminStatus)
	mux.HandleFunc("GET /admin/mode", s.adminMode)
	mux.HandleFunc("GET /admin/config", s.adminConfig)
	mux.HandleFunc("POST /admin/reload", s.adminReload)
	mux.HandleFunc("POST /admin/proxy/stop", s.adminProxyStop)
	mux.HandleFunc("POST /admin/proxy/start", s.adminProxyStart)

	// Runtime config inspection — effective config with all defaults filled in.
	// Keys are masked (last 4 chars visible). Equivalent to `kubectl get -o yaml`.
	// Auth: accepts MIROXY_AUTH_TOKEN (any auth.allowed_keys value) OR admin guard token.
	pag := s.proxyAuthGuard(guard)
	mux.Handle("GET /v1/config", pag(http.HandlerFunc(s.configFull)))
	mux.Handle("GET /v1/config/providers", pag(http.HandlerFunc(s.configProviders)))
	mux.Handle("GET /v1/config/credpools", pag(http.HandlerFunc(s.configCredpools)))
	mux.Handle("GET /v1/config/routes", pag(http.HandlerFunc(s.configRoutes)))

	return guard.wrap(mux)
}

// ── handlers ─────────────────────────────────────────────────────────────────

func (s *Server) adminMode(w http.ResponseWriter, _ *http.Request) {
	mode := "running"
	if !s.IsProxyRunning() {
		mode = "paused"
	}
	writeAdminJSON(w, http.StatusOK, map[string]string{"mode": mode})
}

func (s *Server) adminProxyStop(w http.ResponseWriter, _ *http.Request) {
	if !s.IsProxyRunning() {
		writeAdminJSON(w, http.StatusBadRequest, map[string]string{"error": "proxy is not running"})
		return
	}
	s.SignalStop()
	writeAdminJSON(w, http.StatusOK, map[string]string{"status": "stopping"})
}

func (s *Server) adminProxyStart(w http.ResponseWriter, _ *http.Request) {
	if s.IsProxyRunning() {
		writeAdminJSON(w, http.StatusBadRequest, map[string]string{"error": "proxy is already running"})
		return
	}
	s.SignalStart()
	writeAdminJSON(w, http.StatusOK, map[string]string{"status": "starting"})
}

func (s *Server) adminConfig(w http.ResponseWriter, _ *http.Request) {
	writeAdminJSON(w, http.StatusOK, sanitizeConfig(s.cfg.Load()))
}

func (s *Server) adminHealth(w http.ResponseWriter, _ *http.Request) {
	writeAdminJSON(w, http.StatusOK, map[string]string{
		"status": "healthy",
	})
}

func (s *Server) adminStatus(w http.ResponseWriter, _ *http.Request) {
	cfg := s.cfg.Load()

	type modelRow struct {
		ModelName     string `json:"model_name"`
		Provider      string `json:"provider"`
		ProviderModel string `json:"provider_model"`
		Strategy      string `json:"strategy,omitempty"`
	}
	models := make([]modelRow, 0, len(cfg.ModelRoutes))
	for _, m := range cfg.ModelRoutes {
		row := modelRow{
			ModelName:     m.ModelName,
			Provider:      m.Provider,
			ProviderModel: m.ProviderModel,
		}
		if m.Routing != nil {
			row.Strategy = m.Routing.Strategy
		}
		models = append(models, row)
	}

	type poolRow struct {
		Name string `json:"name"`
		Keys int    `json:"keys"`
	}
	pools := make([]poolRow, 0, len(cfg.CredPools))
	for name, kp := range cfg.CredPools {
		pools = append(pools, poolRow{Name: name, Keys: len(kp.Keys)})
	}

	uptime := time.Since(s.startTime).Round(time.Second)

	totalIn, totalOut, totalReq, modelSnaps := s.tokenStats.Snapshot()
	type keyUsage struct {
		Name     string `json:"name"`
		Input    int64  `json:"input_tokens"`
		Output   int64  `json:"output_tokens"`
		Requests int64  `json:"requests"`
	}
	type modelUsage struct {
		Name     string     `json:"model"`
		Input    int64      `json:"input_tokens"`
		Output   int64      `json:"output_tokens"`
		Requests int64      `json:"requests"`
		Keys     []keyUsage `json:"keys,omitempty"`
	}
	usageModels := make([]modelUsage, 0, len(modelSnaps))
	for _, ms := range modelSnaps {
		mu := modelUsage{
			Name: ms.Name, Input: ms.Input, Output: ms.Output, Requests: ms.Requests,
		}
		for _, k := range ms.Keys {
			mu.Keys = append(mu.Keys, keyUsage{Name: k.Name, Input: k.Input, Output: k.Output, Requests: k.Requests})
		}
		usageModels = append(usageModels, mu)
	}

	writeAdminJSON(w, http.StatusOK, map[string]any{
		"uptime":    uptime.String(),
		"in_flight": s.inFlight.Load(),
		"models":    models,
		"credpools": pools,
		"config":    s.cfgPath,
		"usage": map[string]any{
			"total_input_tokens":  totalIn,
			"total_output_tokens": totalOut,
			"total_tokens":        totalIn + totalOut,
			"total_requests":      totalReq,
			"by_model":            usageModels,
		},
		"compress": s.compressSnapshot(),
		"warden":   s.wardenSnapshot(),
	})
}

func (s *Server) adminReload(w http.ResponseWriter, _ *http.Request) {
	result, err := s.Reload()
	if err != nil {
		writeAdminJSON(w, http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
		return
	}
	writeAdminJSON(w, http.StatusOK, map[string]string{
		"status":  "reloaded",
		"changes": result.String(),
	})
}

func writeAdminJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ── ServerRef text methods (used by CommandPlugin, zero LLM token) ────────────

// StatsText returns a human-readable status report.
func (s *Server) StatsText() string {
	cfg := s.cfg.Load()
	uptime := time.Since(s.startTime).Round(time.Second)
	sep := strings.Repeat("─", 54)
	var b strings.Builder

	fmt.Fprintf(&b, "miroxy  uptime: %s  in-flight: %d\n%s\n",
		uptime, s.inFlight.Load(), sep)

	// ── Token usage ──────────────────────────────────────────
	totalIn, totalOut, totalReq, modelSnaps := s.tokenStats.Snapshot()
	total := totalIn + totalOut
	fmt.Fprintf(&b, "\nToken usage (session)\n%s\n", sep)
	fmt.Fprintf(&b, "  Total   %s in / %s out = %s  (%d req)\n\n",
		fmtTokens(totalIn), fmtTokens(totalOut), fmtTokens(total), totalReq)

	if len(modelSnaps) > 0 {
		fmt.Fprintf(&b, "  %-22s  %10s  %10s  %10s  %6s\n",
			"model route", "input", "output", "total", "reqs")
		fmt.Fprintf(&b, "  %s\n", strings.Repeat("·", 66))
		for _, ms := range modelSnaps {
			bar := tokenBar(ms.Input+ms.Output, total, 10)
			fmt.Fprintf(&b, "  %-22s  %10s  %10s  %s%10s  %6d\n",
				ms.Name,
				fmtTokens(ms.Input), fmtTokens(ms.Output),
				bar, fmtTokens(ms.Input+ms.Output),
				ms.Requests)
			for _, k := range ms.Keys {
				fmt.Fprintf(&b, "      %-18s  %10s  %10s  %16s  %6d\n",
					"└ "+k.Name,
					fmtTokens(k.Input), fmtTokens(k.Output),
					fmtTokens(k.Input+k.Output), k.Requests)
			}
		}
	}

	// ── Compression performance ──────────────────────────────
	fmt.Fprintf(&b, "\n%s\n", sep)
	if s.compressStats != nil {
		snap := s.compressStats.Snapshot()
		fmt.Fprint(&b, snap.Format())
	} else {
		fmt.Fprintf(&b, "Compression: disabled  (set compress.enabled: true to enable)\n")
	}

	// ── Security (warden) ────────────────────────────────────
	fmt.Fprintf(&b, "\n%s\n", sep)
	if s.wardenStats != nil {
		snap := s.wardenStats.Snapshot()
		fmt.Fprint(&b, snap.Format())
	} else {
		fmt.Fprintf(&b, "Warden: disabled  (set warden.enabled: true to enable)\n")
	}

	// ── Config summary ───────────────────────────────────────
	fmt.Fprintf(&b, "\n%s\nConfig: %s\n", sep, s.cfgPath)
	for _, m := range cfg.ModelRoutes {
		if m.Routing != nil {
			fmt.Fprintf(&b, "  %-22s [%s]\n", m.ModelName, m.Routing.Strategy)
			for _, t := range m.Routing.Targets {
				fmt.Fprintf(&b, "    └─ %-12s %s\n", t.Provider, t.ProviderModel)
			}
		} else {
			fmt.Fprintf(&b, "  %-22s %s / %s\n", m.ModelName, m.Provider, m.ProviderModel)
		}
	}
	return b.String()
}

// ── /v1/config handlers ──────────────────────────────────────────────────────

// maskSecret returns the last 4 characters of s, padded with ****. Safe for
// logging and API responses — hides the bulk of a secret while keeping enough
// to identify which key was used.
func maskSecret(s string) string {
	if len(s) <= 4 {
		return "****"
	}
	return "****" + s[len(s)-4:]
}

func (s *Server) buildConfigResponse(cfg *config.Config, sections ...string) map[string]any {
	all := len(sections) == 0
	wants := make(map[string]bool, len(sections))
	for _, sec := range sections {
		wants[sec] = true
	}

	resp := map[string]any{}

	if all {
		resp["server"] = cfg.Server
		resp["admin"] = cfg.Admin
		resp["log"] = cfg.Log
		resp["auth"] = map[string]any{
			"allowed_keys": func() []string {
				out := make([]string, len(cfg.Auth.AllowedKeys))
				for i, k := range cfg.Auth.AllowedKeys {
					out[i] = maskSecret(k)
				}
				return out
			}(),
		}
		resp["compress"] = cfg.Compress
		resp["dump"] = map[string]any{
			"enabled":     cfg.Dump.Enabled,
			"path":        cfg.Dump.Path,
			"include_sse": cfg.Dump.IncludeSSE,
			"max_size_mb": cfg.Dump.MaxSizeMB,
			"max_backups": cfg.Dump.MaxBackups,
		}
		resp["metrics"] = cfg.Metrics
	}

	if all || wants["credpools"] {
		type keyEntry struct {
			Name string `json:"name"`
			Key  string `json:"key"`
		}
		type poolView struct {
			Provider              string     `json:"provider,omitempty"`
			Strategy              string     `json:"strategy,omitempty"`
			CircuitBreakThreshold int        `json:"circuit_break_threshold,omitempty"`
			CooldownSeconds       int        `json:"cooldown_seconds,omitempty"`
			RateLimitRPM          int        `json:"rate_limit_rpm,omitempty"`
			Keys                  []keyEntry `json:"keys"`
		}
		pools := map[string]poolView{}
		for name, kp := range cfg.CredPools {
			pv := poolView{
				Provider:              kp.Provider,
				Strategy:              kp.Strategy,
				CircuitBreakThreshold: kp.CircuitBreakThreshold,
				CooldownSeconds:       kp.CooldownSeconds,
				RateLimitRPM:          kp.RateLimitRPM,
			}
			for _, k := range kp.Keys {
				pv.Keys = append(pv.Keys, keyEntry{Name: k.Name, Key: maskSecret(k.Key)})
			}
			pools[name] = pv
		}
		resp["credpools"] = pools
	}

	if all || wants["providers"] {
		resp["providers"] = cfg.Providers
	}

	if all || wants["routes"] {
		type routeView struct {
			ModelName     string                `json:"model_name"`
			DisplayName   string                `json:"display_name,omitempty"`
			Provider      string                `json:"provider,omitempty"`
			ProviderModel string                `json:"provider_model,omitempty"`
			Protocol      string                `json:"protocol,omitempty"`
			APIBase       string                `json:"api_base,omitempty"`
			AuthStyle     string                `json:"auth_style,omitempty"`
			CredpoolRef   string                `json:"credpool_ref,omitempty"`
			Routing       *config.RoutingConfig `json:"routing,omitempty"`
		}
		routes := make([]routeView, len(cfg.ModelRoutes))
		for i, m := range cfg.ModelRoutes {
			routes[i] = routeView{
				ModelName: m.ModelName, DisplayName: m.DisplayName,
				Provider: m.Provider, ProviderModel: m.ProviderModel,
				Protocol: m.Protocol, APIBase: m.APIBase, AuthStyle: m.AuthStyle,
				CredpoolRef: m.CredpoolRef, Routing: m.Routing,
			}
		}
		resp["model_routes"] = routes
	}

	if all {
		resp["admin"] = cfg.Admin
		resp["metrics"] = cfg.Metrics
	}

	return resp
}

// proxyAuthGuard wraps an adminGuard so that /v1/config/* endpoints also accept
// any token from auth.allowed_keys — clients can reuse their proxy auth token
// without needing the separate admin password.
func (s *Server) proxyAuthGuard(ag *adminGuard) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 1. Try admin guard token (existing mechanism).
			if ag.checkToken(r) {
				next.ServeHTTP(w, r)
				return
			}
			// 2. Fall back to any proxy auth.allowed_keys value.
			tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			cfg := s.cfg.Load()
			for _, k := range cfg.Auth.AllowedKeys {
				if tok == k {
					next.ServeHTTP(w, r)
					return
				}
			}
			http.Error(w, `{"type":"error","error":{"type":"auth_error","message":"unauthorized"}}`,
				http.StatusUnauthorized)
		})
	}
}

// configFull returns the complete effective config (all defaults filled in, keys masked).
func (s *Server) configFull(w http.ResponseWriter, _ *http.Request) {
	writeAdminJSON(w, http.StatusOK, s.buildConfigResponse(s.cfg.Load()))
}

// configProviders returns only the providers section with resolved defaults.
func (s *Server) configProviders(w http.ResponseWriter, _ *http.Request) {
	writeAdminJSON(w, http.StatusOK, s.buildConfigResponse(s.cfg.Load(), "providers"))
}

// configCredpools returns credpool names, strategies, and masked keys.
func (s *Server) configCredpools(w http.ResponseWriter, _ *http.Request) {
	writeAdminJSON(w, http.StatusOK, s.buildConfigResponse(s.cfg.Load(), "credpools"))
}

// configRoutes returns providers and model_routes (including auto-discovered entries).
func (s *Server) configRoutes(w http.ResponseWriter, _ *http.Request) {
	writeAdminJSON(w, http.StatusOK, s.buildConfigResponse(s.cfg.Load(), "routes"))
}

func fmtTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func tokenBar(part, total int64, width int) string {
	if total == 0 || width == 0 {
		return strings.Repeat("░", width)
	}
	filled := int(float64(part) / float64(total) * float64(width))
	if filled > width {
		filled = width
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

// ModelInfoText returns a human-readable, read-only overview of the current
// default model, the model routing table, configured providers, and each
// credpool's key names (never key values). Used by CommandPlugin's
// ":miroxy model" — model switching itself is done via each client's native
// /model picker, not through this command.
func (s *Server) ModelInfoText() string {
	cfg := s.cfg.Load()
	var b strings.Builder
	sep := strings.Repeat("─", 62)

	defaultModel := cfg.Server.DefaultModel
	if defaultModel == "" {
		defaultModel = "(none set — first model_routes entry is used)"
	}
	fmt.Fprintf(&b, "Current model: %s\n%s\n", defaultModel, sep)

	fmt.Fprintf(&b, "\nModel routes\n%s\n", sep)
	if len(cfg.ModelRoutes) == 0 {
		fmt.Fprintf(&b, "  (no model_routes configured)\n")
	}
	for _, m := range cfg.ModelRoutes {
		if m.Routing != nil {
			fmt.Fprintf(&b, "  %-20s [%s]\n", m.ModelName, m.Routing.Strategy)
			for i, t := range m.Routing.Targets {
				tree := "├─"
				if i == len(m.Routing.Targets)-1 {
					tree = "└─"
				}
				fmt.Fprintf(&b, "    %s %-14s %-20s  credpool: %s\n",
					tree, t.Provider, t.ProviderModel, orDash(t.CredpoolRef))
			}
		} else {
			fmt.Fprintf(&b, "  %-20s %-14s %-20s  credpool: %s\n",
				m.ModelName, m.Provider, m.ProviderModel, orDash(m.CredpoolRef))
		}
	}

	fmt.Fprintf(&b, "\nProviders\n%s\n", sep)
	if len(cfg.Providers) == 0 {
		fmt.Fprintf(&b, "  (no providers configured — built-ins resolved implicitly)\n")
	}
	for _, name := range sortedKeys(cfg.Providers) {
		fmt.Fprintf(&b, "  %-14s %s\n", name, cfg.Providers[name].BaseURL)
	}

	fmt.Fprintf(&b, "\nCredpools\n%s\n", sep)
	if len(cfg.CredPools) == 0 {
		fmt.Fprintf(&b, "  (no named credpools)\n")
	}
	for _, name := range sortedKeys(cfg.CredPools) {
		kp := cfg.CredPools[name]
		fmt.Fprintf(&b, "  %-14s provider=%-10s strategy=%-14s keys: %s\n",
			name, orDash(kp.Provider), orDash(kp.Strategy), keyNames(kp.Keys))
	}

	return b.String()
}

// orDash returns "-" for an empty string, otherwise s unchanged.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// keyNames renders a credpool's key names (never values) as a comma-separated
// list. Anonymous keys (no yaml name) fall back to key_N, matching the
// convention used by sanitizeConfig().
func keyNames(keys []config.CredEntry) string {
	if len(keys) == 0 {
		return "(none)"
	}
	names := make([]string, len(keys))
	for i, k := range keys {
		if k.Name != "" {
			names[i] = k.Name
		} else {
			names[i] = fmt.Sprintf("key_%d", i+1)
		}
	}
	return strings.Join(names, ", ")
}

// sortedKeys returns a map's keys in sorted order for deterministic text output.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sanitizeConfig converts a Config into a UI-safe JSON structure.
// Key values are masked to last-4 chars; secrets are never returned in full.
func sanitizeConfig(cfg *config.Config) map[string]any {
	type keyView struct {
		Name   string `json:"name"`
		Masked string `json:"masked"`
	}
	type poolView struct {
		Strategy     string    `json:"strategy,omitempty"`
		Keys         []keyView `json:"keys"`
		RateLimitRPM int       `json:"rate_limit_rpm,omitempty"`
		CooldownSecs int       `json:"cooldown_seconds,omitempty"`
	}
	type targetView struct {
		Provider      string `json:"provider"`
		ProviderModel string `json:"provider_model"`
		CredpoolRef   string `json:"credpool_ref,omitempty"`
	}
	type routingView struct {
		Strategy string       `json:"strategy"`
		Targets  []targetView `json:"targets"`
	}
	type modelView struct {
		ModelName     string       `json:"model_name"`
		Provider      string       `json:"provider,omitempty"`
		ProviderModel string       `json:"provider_model,omitempty"`
		CredpoolRef   string       `json:"credpool_ref,omitempty"`
		Routing       *routingView `json:"routing,omitempty"`
	}

	pools := make(map[string]poolView, len(cfg.CredPools))
	for name, kp := range cfg.CredPools {
		keys := make([]keyView, len(kp.Keys))
		for i, k := range kp.Keys {
			masked := "••••"
			if len(k.Key) > 4 {
				masked = "••••" + k.Key[len(k.Key)-4:]
			}
			displayName := k.Name
			if displayName == "" {
				displayName = fmt.Sprintf("key_%d", i+1)
			}
			keys[i] = keyView{Name: displayName, Masked: masked}
		}
		pools[name] = poolView{
			Strategy:     kp.Strategy,
			Keys:         keys,
			RateLimitRPM: kp.RateLimitRPM,
			CooldownSecs: kp.CooldownSeconds,
		}
	}

	models := make([]modelView, len(cfg.ModelRoutes))
	for i, m := range cfg.ModelRoutes {
		mv := modelView{
			ModelName:     m.ModelName,
			Provider:      m.Provider,
			ProviderModel: m.ProviderModel,
			CredpoolRef:   m.CredpoolRef,
		}
		if m.Routing != nil {
			targets := make([]targetView, len(m.Routing.Targets))
			for j, t := range m.Routing.Targets {
				targets[j] = targetView{
					Provider:      t.Provider,
					ProviderModel: t.ProviderModel,
					CredpoolRef:   t.CredpoolRef,
				}
			}
			mv.Routing = &routingView{
				Strategy: m.Routing.Strategy,
				Targets:  targets,
			}
		}
		models[i] = mv
	}

	return map[string]any{
		"server": map[string]any{
			"port":          effectivePort(cfg),
			"default_model": cfg.Server.DefaultModel,
			"commands": map[string]any{
				"disabled":   cfg.Server.Commands.Disabled,
				"allow_dump": cfg.Server.Commands.AllowDump,
			},
		},
		"credpools":    pools,
		"model_routes": models,
		"compress": map[string]any{
			"enabled":       cfg.Compress.Enabled,
			"align_dynamic": cfg.Compress.AlignDynamic,
			"threshold":     cfg.Compress.Threshold,
			"total_budget":  cfg.Compress.TotalBudget,
		},
		"dump": map[string]any{
			"enabled": cfg.Dump.Enabled,
			"path":    cfg.Dump.Path,
		},
	}
}

// SetDump enables or disables the dump store dynamically.
func (s *Server) SetDump(enabled bool) {
	if !enabled {
		s.dumpStore = nil
	}
	// Enabling at runtime requires a path — not supported without a config reload.
	// Users should use /miroxy reload after setting dump.enabled: true in config.
}

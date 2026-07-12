package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"miroxy/internal/config"
	"miroxy/internal/types"
)

// tryInjectAnthropicModels finds the first named credpool backed by an Anthropic
// provider, fetches the live model list from api.anthropic.com, and appends any
// model not already present in cfg.ModelRoutes. Modifies cfg in memory only —
// the config file on disk is never touched. Called once at startup when
// model_discovery: auto is set.
func tryInjectAnthropicModels(cfg *config.Config) {
	poolName, key := findAnthropicPoolKey(cfg)
	if key == "" {
		slog.Debug("model_discovery: auto enabled but no Anthropic credpool found, skipping")
		return
	}

	models, err := fetchAnthropicModels(key)
	if err != nil {
		slog.Warn("model_discovery: failed to fetch Anthropic models", "error", err)
		return
	}

	configured := make(map[string]bool, len(cfg.ModelRoutes))
	for _, m := range cfg.ModelRoutes {
		configured[m.ModelName] = true
	}

	injected := 0
	for _, m := range models {
		if configured[m.ID] {
			continue
		}
		cfg.ModelRoutes = append(cfg.ModelRoutes, config.ModelEntry{
			ModelName:     m.ID,
			DisplayName:   m.DisplayName,
			ProviderRef:   "anthropic",
			UpstreamModel: m.ID,
			CredpoolRef:   poolName,
		})
		injected++
	}

	slog.Info("model_discovery: injected Anthropic models", "count", injected, "pool", poolName)
}

// findAnthropicPoolKey returns the first credpool configured for Anthropic.
// Checks in order:
//  1. Credpools with upstream_model_type: "anthropic" tag (explicit)
//  2. Model routes with provider_ref: "anthropic" referencing a named credpool (legacy)
func findAnthropicPoolKey(cfg *config.Config) (poolName, key string) {
	// 1. Explicit credpool tag.
	for name, pool := range cfg.CredPools {
		if pool.UpstreamModelType == "anthropic" && len(pool.Keys) > 0 {
			return name, pool.Keys[0].Key
		}
	}
	// 2. Infer from model_routes (backward compat).
	for _, m := range cfg.ModelRoutes {
		if m.ProviderRef != "anthropic" || m.CredpoolRef == "" {
			continue
		}
		pool, ok := cfg.CredPools[m.CredpoolRef]
		if !ok || len(pool.Keys) == 0 {
			continue
		}
		return m.CredpoolRef, pool.Keys[0].Key
	}
	return "", ""
}

// fetchAnthropicModels calls GET api.anthropic.com/v1/models and returns the list.
func fetchAnthropicModels(apiKey string) ([]types.Model, error) {
	req, err := http.NewRequest(http.MethodGet, "https://api.anthropic.com/v1/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Anthropic API returned %d: %.200s", resp.StatusCode, body)
	}

	var result types.ModelsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return result.Data, nil
}

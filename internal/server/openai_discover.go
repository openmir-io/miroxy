package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"miroxy/internal/config"
	"miroxy/internal/types"
)

// tryInjectOpenAIModels finds the first named keypool backed by an OpenAI
// provider, fetches the live model list, and appends chat-capable models not
// already present in cfg.ModelRoutes. Called once at startup alongside
// tryInjectAnthropicModels when model_discovery: auto is set.
func tryInjectOpenAIModels(cfg *config.Config) {
	poolName, key, baseURL := findOpenAIPoolKey(cfg)
	if key == "" {
		slog.Debug("model_discovery: no OpenAI keypool found, skipping")
		return
	}

	models, err := fetchOpenAIModels(key, baseURL)
	if err != nil {
		slog.Warn("model_discovery: failed to fetch OpenAI models", "error", err)
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
		display := m.DisplayName
		if display == "" {
			display = m.ID
		}
		cfg.ModelRoutes = append(cfg.ModelRoutes, config.ModelEntry{
			ModelName:     m.ID,
			DisplayName:   display,
			Provider:      "openai",
			ProviderModel: m.ID,
			KeypoolRef:    poolName,
		})
		injected++
	}

	slog.Info("model_discovery: injected OpenAI models", "count", injected, "pool", poolName)
}

// findOpenAIPoolKey returns the first keypool configured for OpenAI.
// Checks in order:
//  1. Keypools with provider: "openai" tag (explicit, no model_routes needed)
//  2. Model routes with provider: "openai" referencing a named keypool (legacy)
func findOpenAIPoolKey(cfg *config.Config) (poolName, key, baseURL string) {
	// 1. Explicit keypool tag.
	for name, pool := range cfg.KeyPools {
		if pool.Provider == "openai" && len(pool.Keys) > 0 {
			base := resolveOpenAIBaseFromPool(cfg, name)
			return name, pool.Keys[0].Key, base
		}
	}
	// 2. Infer from model_routes (backward compat).
	for _, m := range cfg.ModelRoutes {
		if m.Provider != "openai" || m.KeypoolRef == "" {
			continue
		}
		pool, ok := cfg.KeyPools[m.KeypoolRef]
		if !ok || len(pool.Keys) == 0 {
			continue
		}
		base := resolveOpenAIBase(cfg, m)
		return m.KeypoolRef, pool.Keys[0].Key, base
	}
	return "", "", ""
}

// resolveOpenAIBaseFromPool resolves the base URL for a pool identified by name
// (used when the pool has no associated model_route to read api_base from).
func resolveOpenAIBaseFromPool(cfg *config.Config, _ string) string {
	if p, ok := cfg.Providers["openai"]; ok && p.BaseURL != "" {
		return strings.TrimRight(p.BaseURL, "/")
	}
	return "https://api.openai.com/v1"
}

// resolveOpenAIBase returns the base URL for the OpenAI-compatible endpoint,
// checking the model entry's api_base, then the providers block, then the default.
func resolveOpenAIBase(cfg *config.Config, m config.ModelEntry) string {
	if m.APIBase != "" {
		return strings.TrimRight(m.APIBase, "/")
	}
	if p, ok := cfg.Providers["openai"]; ok && p.BaseURL != "" {
		return strings.TrimRight(p.BaseURL, "/")
	}
	return "https://api.openai.com/v1"
}

// chatModelPrefixes lists the ID prefixes of OpenAI chat/completion models.
// Filters out embeddings, tts, image, audio, and legacy instruct models.
var chatModelPrefixes = []string{
	"gpt-", "o1", "o3", "o4", "chatgpt-",
}

func isOpenAIChatModel(id string) bool {
	for _, pfx := range chatModelPrefixes {
		if strings.HasPrefix(id, pfx) {
			return true
		}
	}
	return false
}

// fetchOpenAIModels calls GET {baseURL}/models and returns chat-capable models.
// OpenAI returns many models (embeddings, tts, dall-e, etc.); we filter to
// those whose IDs start with known chat-model prefixes.
func fetchOpenAIModels(apiKey, baseURL string) ([]types.Model, error) {
	url := baseURL + "/models"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

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
		return nil, fmt.Errorf("OpenAI API returned %d: %.200s", resp.StatusCode, body)
	}

	// OpenAI format: {"object":"list","data":[{"id":"gpt-5.5","object":"model",...}]}
	var result types.ModelsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	var chat []types.Model
	for _, m := range result.Data {
		if isOpenAIChatModel(m.ID) {
			chat = append(chat, m)
		}
	}
	return chat, nil
}

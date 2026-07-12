package integration_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"miroxy/internal/config"
	"miroxy/internal/server"
	"miroxy/internal/types"
)

// TestRoundRobin_DynamicProtocolDispatch is the scenario from the design
// discussion: an OpenAI-protocol client (e.g. Codex) hits a single
// model_routes entry that round-robins across three differently-protocoled
// targets (gemini, anthropic, openai). Each attempt's real-vs-passthrough
// choice must be made per target, per request — not by any static config —
// comparing the client's actual protocol (openai, from the HTTP path it
// hit) against each target's protocol:
//
//   - gemini target (mismatch)    -> real IR transform to Gemini wire shape
//   - anthropic target (mismatch) -> real IR transform to Anthropic wire shape
//   - openai target (match)       -> raw byte passthrough, no transform
//
// A field with no Anthropic-canonical equivalent (reasoning_effort) proves
// the distinction: it must survive verbatim only on the openai leg.
func TestRoundRobin_DynamicProtocolDispatch(t *testing.T) {
	var mu sync.Mutex
	received := map[string]string{} // target name -> raw request body received

	record := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			received[name] = string(body)
			mu.Unlock()

			switch name {
			case "gemini":
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(types.GeminiResponse{
					Candidates: []types.GeminiCandidate{{
						Content:      types.GeminiContent{Role: "model", Parts: []types.GeminiPart{{Text: "hi from gemini"}}},
						FinishReason: "STOP",
					}},
					UsageMetadata: types.GeminiUsageMetadata{PromptTokenCount: 1, CandidatesTokenCount: 1},
				})
			case "anthropic":
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(types.MessageResponse{
					ID: "msg_1", Type: "message", Role: "assistant",
					Content:    []types.ContentBlock{{Type: "text", Text: "hi from anthropic"}},
					Model:      "claude-target",
					StopReason: "end_turn",
				})
			case "openai":
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"id":"chatcmpl-raw","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi from openai"}}]}`))
			}
		}
	}

	geminiStub := httptest.NewServer(record("gemini"))
	defer geminiStub.Close()
	anthropicStub := httptest.NewServer(record("anthropic"))
	defer anthropicStub.Close()
	openaiStub := httptest.NewServer(record("openai"))
	defer openaiStub.Close()

	cfg := &config.Config{
		Auth: config.AuthConfig{AllowedKeys: []string{testClientKey}},
		CredPools: map[string]config.CredPoolCfg{
			"pool-gemini":    {Keys: []config.CredEntry{{Name: "k", Key: "k"}}},
			"pool-anthropic": {Keys: []config.CredEntry{{Name: "k", Key: "k"}}},
			"pool-openai":    {Keys: []config.CredEntry{{Name: "k", Key: "k"}}},
		},
		ModelRoutes: []config.ModelEntry{{
			ModelName: "miroxy-code",
			Routing: &config.RoutingConfig{
				Strategy: "round_robin",
				Targets: []config.RoutingTarget{
					{Provider: "gemini", ProviderModel: "gemini-target", CredpoolRef: "pool-gemini", Protocol: "gemini", APIBase: geminiStub.URL},
					{Provider: "anthropic", ProviderModel: "claude-target", CredpoolRef: "pool-anthropic", Protocol: "anthropic", APIBase: anthropicStub.URL},
					{Provider: "openai", ProviderModel: "gpt-target", CredpoolRef: "pool-openai", Protocol: "openai", APIBase: openaiStub.URL},
				},
			},
			TimeoutSeconds: 5,
		}},
	}

	srv := server.New(cfg, "")
	defer srv.Close()
	miroxy := httptest.NewServer(srv.Handler())
	defer miroxy.Close()

	const body = `{"model":"miroxy-code","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"xhigh"}`

	// round_robin visits each of the 3 targets exactly once across 3 calls.
	for i := 0; i < 3; i++ {
		req, err := http.NewRequest(http.MethodPost, miroxy.URL+"/v1/chat/completions", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+testClientKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, resp.StatusCode)
		}
		resp.Body.Close()
	}

	mu.Lock()
	defer mu.Unlock()

	if len(received) != 3 {
		t.Fatalf("expected all 3 targets to be hit exactly once, got %d: %v", len(received), received)
	}

	// openai leg: protocol matched the client's actual protocol (openai) ->
	// raw passthrough. The original bytes, including reasoning_effort (which
	// has no Anthropic-canonical equivalent), must survive verbatim.
	if got := received["openai"]; got != body {
		t.Errorf("openai leg: raw body mismatch.\n got:  %s\n want: %s", got, body)
	}

	// gemini leg: protocol mismatched -> real IR transform. reasoning_effort
	// has no canonical representation and must NOT survive; the body must
	// look like real Gemini wire shape (contents/generationConfig), not the
	// client's original OpenAI shape.
	if got := received["gemini"]; strings.Contains(got, "reasoning_effort") {
		t.Errorf("gemini leg: reasoning_effort leaked through the IR transform: %s", got)
	}
	if got := received["gemini"]; !strings.Contains(got, "contents") {
		t.Errorf("gemini leg: body doesn't look like Gemini wire shape: %s", got)
	}

	// anthropic leg: protocol mismatched (client is openai, target is
	// anthropic, not byte-identical despite canonical also being
	// Anthropic-shaped) -> real transform via AnthropicUpstream.
	// reasoning_effort must not survive.
	if got := received["anthropic"]; strings.Contains(got, "reasoning_effort") {
		t.Errorf("anthropic leg: reasoning_effort leaked through the IR transform: %s", got)
	}
	if got := received["anthropic"]; !strings.Contains(got, `"messages"`) {
		t.Errorf("anthropic leg: body doesn't look like Anthropic wire shape: %s", got)
	}
}

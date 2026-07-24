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
					{ProviderRef: "gemini", UpstreamModel: "gemini-target", CredpoolRef: "pool-gemini", Protocol: "gemini", APIBase: geminiStub.URL},
					{ProviderRef: "anthropic", UpstreamModel: "claude-target", CredpoolRef: "pool-anthropic", Protocol: "anthropic", APIBase: anthropicStub.URL},
					{ProviderRef: "openai", UpstreamModel: "gpt-target", CredpoolRef: "pool-openai", Protocol: "openai", APIBase: openaiStub.URL},
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
	// raw passthrough. reasoning_effort (no Anthropic-canonical equivalent)
	// must survive verbatim — but "model" is not a protocol-shape concern,
	// it's miroxy's own routing alias ("miroxy-code"), which this real
	// upstream (unlike this test's permissive stub) would reject outright.
	// Passthrough rewrites it to the target's upstream_model ("gpt-target"),
	// same as every real transform adapter already does (see
	// AnthropicUpstream.build's outReq.Model = a.upstreamModel) — the one
	// field passthrough does not forward byte-for-byte. Key order shifts to
	// alphabetical because the rewrite round-trips through a map.
	const wantOpenAIBody = `{"messages":[{"role":"user","content":"hi"}],"model":"gpt-target","reasoning_effort":"xhigh"}`
	if got := received["openai"]; got != wantOpenAIBody {
		t.Errorf("openai leg: raw body mismatch.\n got:  %s\n want: %s", got, wantOpenAIBody)
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

// TestRetry_Raw_404OnFirstKey_RetriesToSecondKey is the literal reported
// bug: in raw/passthrough dispatch mode (client protocol == target
// protocol — the exact case here, an openai client hitting an openai
// target), a non-429/5xx status (404 — e.g. a provider's free-tier model
// deprecation) must still fail over to the next key, not terminate the
// retry loop on the very first attempt.
func TestRetry_Raw_404OnFirstKey_RetriesToSecondKey(t *testing.T) {
	var mu sync.Mutex
	calls := 0

	openaiStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()

		if n == 1 {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":{"message":"model unavailable for free","code":404}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-raw","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer openaiStub.Close()

	cfg := &config.Config{
		Auth: config.AuthConfig{AllowedKeys: []string{testClientKey}},
		CredPools: map[string]config.CredPoolCfg{
			"pool-openai": {Keys: []config.CredEntry{
				{Name: "bad", Key: "bad-key"},
				{Name: "good", Key: "good-key"},
			}},
		},
		ModelRoutes: []config.ModelEntry{{
			ModelName: "miroxy-code",
			Routing: &config.RoutingConfig{
				Strategy: "round_robin",
				Targets: []config.RoutingTarget{
					{ProviderRef: "openai", UpstreamModel: "gpt-target", CredpoolRef: "pool-openai", Protocol: "openai", APIBase: openaiStub.URL},
				},
			},
			TimeoutSeconds: 5,
		}},
	}

	srv := server.New(cfg, "")
	defer srv.Close()
	miroxy := httptest.NewServer(srv.Handler())
	defer miroxy.Close()

	const body = `{"model":"miroxy-code","messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequest(http.MethodPost, miroxy.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testClientKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after raw-mode failover past 404, got %d", resp.StatusCode)
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Errorf("expected exactly 2 upstream calls (1 failed + 1 succeeded), got %d", calls)
	}
}

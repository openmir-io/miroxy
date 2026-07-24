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

// TestCrossProtocol_CompressedRequestRawDispatch_NoPrivilegeLeak is the
// integration-level regression test for the tool_use/tool_result leak fixed
// 2026-07-19 (see docs/dev/DESIGNLOG.md): Compress rewrites the canonical
// (IR) request, then a raw-dispatch attempt (client protocol == upstream
// protocol) must re-encode the rewritten request into THAT protocol's own
// wire shape — never leak the other protocol's block-type vocabulary.
//
// Each of the two protocols that can actually reach raw dispatch (Anthropic,
// OpenAI — Responses never can, since no upstream ever resolves to protocol
// "openai-responses") gets its own model route whose upstream protocol
// matches the client protocol exactly, forcing DispatchRaw after Compress
// has rewritten the request.
func TestCrossProtocol_CompressedRequestRawDispatch_NoPrivilegeLeak(t *testing.T) {
	var mu sync.Mutex
	received := map[string]string{}

	record := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			received[name] = string(body)
			mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			switch name {
			case "anthropic":
				_ = json.NewEncoder(w).Encode(types.MessageResponse{
					ID: "msg_1", Type: "message", Role: "assistant",
					Content:    []types.ContentBlock{{Type: "text", Text: "ok"}},
					Model:      "claude-target",
					StopReason: "end_turn",
				})
			case "openai":
				_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
			}
		}
	}

	anthropicStub := httptest.NewServer(record("anthropic"))
	defer anthropicStub.Close()
	openaiStub := httptest.NewServer(record("openai"))
	defer openaiStub.Close()

	cfg := &config.Config{
		Auth:     config.AuthConfig{AllowedKeys: []string{testClientKey}},
		Compress: config.CompressConfig{Enabled: true, Threshold: 1}, // always rewrite, for determinism
		CredPools: map[string]config.CredPoolCfg{
			"pool-anthropic": {Keys: []config.CredEntry{{Name: "k", Key: "k"}}},
			"pool-openai":    {Keys: []config.CredEntry{{Name: "k", Key: "k"}}},
		},
		ModelRoutes: []config.ModelEntry{
			{ModelName: "route-anthropic", UpstreamModel: "claude-target", CredpoolRef: "pool-anthropic", Protocol: "anthropic", APIBase: anthropicStub.URL, TimeoutSeconds: 5},
			{ModelName: "route-openai", UpstreamModel: "gpt-target", CredpoolRef: "pool-openai", Protocol: "openai", APIBase: openaiStub.URL, TimeoutSeconds: 5},
		},
	}

	srv := server.New(cfg, "")
	defer srv.Close()
	miroxy := httptest.NewServer(srv.Handler())
	defer miroxy.Close()

	anthropicBody := `{"model":"route-anthropic","max_tokens":100,"messages":[` +
		`{"role":"user","content":"read the file"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"read","input":{"path":"x"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"file contents"}]}]}`

	openaiBody := `{"model":"route-openai","messages":[` +
		`{"role":"user","content":"read the file"},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"read","arguments":"{\"path\":\"x\"}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"file contents"}]}`

	postJSON(t, miroxy.URL+"/v1/messages", anthropicBody)
	postJSON(t, miroxy.URL+"/v1/chat/completions", openaiBody)

	mu.Lock()
	defer mu.Unlock()

	if len(received) != 2 {
		t.Fatalf("expected both upstreams to be hit, got %d: %v", len(received), received)
	}

	anthropicUp := received["anthropic"]
	if !strings.Contains(anthropicUp, `"type":"tool_use"`) {
		t.Errorf("anthropic leg: expected native tool_use block after compress rewrite, got: %s", anthropicUp)
	}
	if strings.Contains(anthropicUp, `"tool_calls"`) || strings.Contains(anthropicUp, `"role":"tool"`) {
		t.Errorf("anthropic leg: OpenAI-shaped tool_calls/role:tool leaked in: %s", anthropicUp)
	}

	openaiUp := received["openai"]
	if !strings.Contains(openaiUp, `"tool_calls"`) {
		t.Errorf("openai leg: expected native tool_calls after compress rewrite, got: %s", openaiUp)
	}
	if strings.Contains(openaiUp, `"type":"tool_use"`) || strings.Contains(openaiUp, `"type":"tool_result"`) {
		t.Errorf("openai leg: Anthropic-shaped tool_use/tool_result leaked in (the original bug): %s", openaiUp)
	}
}

func postJSON(t *testing.T, url, body string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testClientKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST %s: status = %d, body: %s", url, resp.StatusCode, b)
	}
}

package integration_test

import (
	"bufio"
	"encoding/json"
	"fmt"
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

// TestWardenPlugin_RedactsSecretAcrossPassthroughAndTransform proves the
// dual-representation fix in WardenPlugin.sanitizeRequest: a leaked secret
// must never reach the upstream regardless of whether this attempt takes
// the raw-passthrough path (ships RawRequestBody, bypassing c.Request
// entirely) or the real-transform path (ships c.Request, ignoring
// RawRequestBody). round_robin visits one target of each kind.
func TestWardenPlugin_RedactsSecretAcrossPassthroughAndTransform(t *testing.T) {
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
			case "anthropic": // client protocol matches -> passthrough
				json.NewEncoder(w).Encode(types.MessageResponse{
					ID: "msg_1", Type: "message", Role: "assistant",
					Content: []types.ContentBlock{{Type: "text", Text: "ok"}},
				})
			case "gemini": // mismatch -> real transform
				json.NewEncoder(w).Encode(types.GeminiResponse{
					Candidates: []types.GeminiCandidate{{
						Content:      types.GeminiContent{Role: "model", Parts: []types.GeminiPart{{Text: "ok"}}},
						FinishReason: "STOP",
					}},
				})
			}
		}
	}

	anthropicStub := httptest.NewServer(record("anthropic"))
	defer anthropicStub.Close()
	geminiStub := httptest.NewServer(record("gemini"))
	defer geminiStub.Close()

	cfg := &config.Config{
		Auth: config.AuthConfig{AllowedKeys: []string{testClientKey}},
		Warden: config.WardenConfig{
			Enabled: true,
			Mode:    "redact",
		},
		CredPools: map[string]config.CredPoolCfg{
			"pool-anthropic": {Keys: []config.CredEntry{{Name: "k", Key: "k"}}},
			"pool-gemini":    {Keys: []config.CredEntry{{Name: "k", Key: "k"}}},
		},
		ModelRoutes: []config.ModelEntry{{
			ModelName: "miroxy-code",
			Routing: &config.RoutingConfig{
				Strategy: "round_robin",
				Targets: []config.RoutingTarget{
					{Provider: "anthropic", ProviderModel: "claude-target", CredpoolRef: "pool-anthropic", Protocol: "anthropic", APIBase: anthropicStub.URL},
					{Provider: "gemini", ProviderModel: "gemini-target", CredpoolRef: "pool-gemini", Protocol: "gemini", APIBase: geminiStub.URL},
				},
			},
			TimeoutSeconds: 5,
		}},
	}

	srv := server.New(cfg, "")
	defer srv.Close()
	miroxy := httptest.NewServer(srv.Handler())
	defer miroxy.Close()

	const secret = "AKIAABCDEFGHIJKLMNOP"
	body := fmt.Sprintf(`{"model":"miroxy-code","max_tokens":100,"messages":[{"role":"user","content":"my key is %s please help"}]}`, secret)

	for i := 0; i < 2; i++ {
		req, err := http.NewRequest(http.MethodPost, miroxy.URL+"/v1/messages", strings.NewReader(body))
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
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("request %d: status = %d, body: %s", i, resp.StatusCode, b)
		}
		resp.Body.Close()
	}

	mu.Lock()
	defer mu.Unlock()

	if len(received) != 2 {
		t.Fatalf("expected both targets to be hit exactly once, got %d: %v", len(received), received)
	}
	if strings.Contains(received["anthropic"], secret) {
		t.Errorf("passthrough leg leaked the raw secret: %s", received["anthropic"])
	}
	if strings.Contains(received["gemini"], secret) {
		t.Errorf("transform leg leaked the raw secret: %s", received["gemini"])
	}
}

// TestWardenPlugin_TokenizeMode_ResolvesInNonStreamingPassthroughResponse
// proves the response-side half of tokenize mode for the passthrough path:
// c.Response.RawBody carries a vault token because the upstream echoed back
// exactly what the (tokenized) request contained, and WardenPlugin must
// restore it before the client sees it.
func TestWardenPlugin_TokenizeMode_ResolvesInNonStreamingPassthroughResponse(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Echo back the deterministic first-PII-of-this-type token miroxy's
		// vault would have minted for the email in the request below.
		json.NewEncoder(w).Encode(types.MessageResponse{
			ID: "msg_1", Type: "message", Role: "assistant",
			Content: []types.ContentBlock{{Type: "text", Text: "got it, will email ⟦EMAIL:001⟧ back"}},
		})
	}))
	defer stub.Close()

	cfg := &config.Config{
		Auth:   config.AuthConfig{AllowedKeys: []string{testClientKey}},
		Warden: config.WardenConfig{Enabled: true, Mode: "tokenize"},
		CredPools: map[string]config.CredPoolCfg{
			"pool": {Keys: []config.CredEntry{{Name: "k", Key: "k"}}},
		},
		ModelRoutes: []config.ModelEntry{{
			ModelName:      "miroxy-code",
			ProviderModel:  "claude-target",
			CredpoolRef:    "pool",
			Protocol:       "anthropic",
			APIBase:        stub.URL,
			TimeoutSeconds: 5,
		}},
	}

	srv := server.New(cfg, "")
	defer srv.Close()
	miroxy := httptest.NewServer(srv.Handler())
	defer miroxy.Close()

	const body = `{"model":"miroxy-code","max_tokens":100,"messages":[{"role":"user","content":"my email is jane@example.com"}]}`
	req, err := http.NewRequest(http.MethodPost, miroxy.URL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testClientKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body: %s", resp.StatusCode, respBody)
	}

	var msg types.MessageResponse
	if err := json.Unmarshal(respBody, &msg); err != nil {
		t.Fatalf("unmarshal response: %v\nbody: %s", err, respBody)
	}
	if len(msg.Content) == 0 {
		t.Fatalf("expected content in response, got none: %s", respBody)
	}
	text := msg.Content[0].Text
	if strings.Contains(text, "⟦") {
		t.Errorf("client-visible text still contains a raw vault token: %q", text)
	}
	if !strings.Contains(text, "jane@example.com") {
		t.Errorf("client-visible text = %q, want it to contain the restored email", text)
	}
}

// TestWardenPlugin_TokenizeMode_ResolvesInStreamingTransformResponse proves
// the response-side half of tokenize mode for the canonical SSEEvent path
// (ResolveEvents): the client hits an OpenAI-shaped endpoint but the target
// is Gemini-protocoled (mismatch -> real transform, streaming), and the
// stub echoes the deterministic vault token in a content delta.
func TestWardenPlugin_TokenizeMode_ResolvesInStreamingTransformResponse(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := types.GeminiResponse{
			Candidates: []types.GeminiCandidate{{
				Content:      types.GeminiContent{Role: "model", Parts: []types.GeminiPart{{Text: "sure, ⟦EMAIL:001⟧ noted"}}},
				FinishReason: "STOP",
			}},
			UsageMetadata: types.GeminiUsageMetadata{PromptTokenCount: 5, CandidatesTokenCount: 5},
		}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}))
	defer stub.Close()

	cfg := &config.Config{
		Auth:   config.AuthConfig{AllowedKeys: []string{testClientKey}},
		Warden: config.WardenConfig{Enabled: true, Mode: "tokenize"},
		CredPools: map[string]config.CredPoolCfg{
			"pool": {Keys: []config.CredEntry{{Name: "k", Key: "k"}}},
		},
		ModelRoutes: []config.ModelEntry{{
			ModelName:      "miroxy-code",
			ProviderModel:  "gemini-target",
			CredpoolRef:    "pool",
			Protocol:       "gemini",
			APIBase:        stub.URL,
			TimeoutSeconds: 5,
		}},
	}

	srv := server.New(cfg, "")
	defer srv.Close()
	miroxy := httptest.NewServer(srv.Handler())
	defer miroxy.Close()

	const body = `{"model":"miroxy-code","stream":true,"messages":[{"role":"user","content":"my email is jane@example.com"}]}`
	req, err := http.NewRequest(http.MethodPost, miroxy.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testClientKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body: %s", resp.StatusCode, b)
	}

	var full strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		full.WriteString(scanner.Text())
		full.WriteString("\n")
	}

	wire := full.String()
	if strings.Contains(wire, "⟦") {
		t.Errorf("client-visible SSE stream still contains a raw vault token:\n%s", wire)
	}
	if !strings.Contains(wire, "jane@example.com") {
		t.Errorf("client-visible SSE stream doesn't contain the restored email:\n%s", wire)
	}
}

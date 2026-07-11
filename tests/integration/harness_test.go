package integration_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"miroxy/internal/config"
	"miroxy/internal/server"
	coreup "miroxy/core/upstream"
	intup "miroxy/internal/upstream"
	"miroxy/internal/types"
)

const testClientKey = "test-client-key"

// --- Stub Gemini server helpers ---

// newStubGemini builds an httptest.Server mimicking the Gemini API.
//
// keyHandlers maps API key query-param values to custom handlers for
// non-streaming requests. Unmatched keys get defaultNonStreamResp.
// streamHandler handles :streamGenerateContent requests; if nil, uses defaultStreamResp.
func newStubGemini(t *testing.T, keyHandlers map[string]http.HandlerFunc, streamHandler http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			if streamHandler != nil {
				streamHandler(w, r)
			} else {
				defaultStreamResp(w, r)
			}
			return
		}
		if h, ok := keyHandlers[key]; ok {
			h(w, r)
			return
		}
		defaultNonStreamResp(w, "Hello from stub Gemini!")
	}))
}

// defaultNonStreamResp writes a minimal valid Gemini generateContent response.
func defaultNonStreamResp(w http.ResponseWriter, text string) {
	resp := types.GeminiResponse{
		Candidates: []types.GeminiCandidate{{
			Content:      types.GeminiContent{Role: "model", Parts: []types.GeminiPart{{Text: text}}},
			FinishReason: "STOP",
		}},
		UsageMetadata: types.GeminiUsageMetadata{PromptTokenCount: 10, CandidatesTokenCount: 5},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// defaultStreamResp writes a two-chunk Gemini SSE stream.
func defaultStreamResp(w http.ResponseWriter, r *http.Request) {
	writeGeminiSSEChunks(w, r, []string{"Hello", " world"})
}

// writeGeminiSSEChunks writes text chunks as Gemini SSE events.
// The last chunk gets FinishReason="STOP" and UsageMetadata.
func writeGeminiSSEChunks(w http.ResponseWriter, r *http.Request, chunks []string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "flusher required", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")

	for i, text := range chunks {
		isLast := i == len(chunks)-1
		candidate := types.GeminiCandidate{
			Content: types.GeminiContent{Role: "model", Parts: []types.GeminiPart{{Text: text}}},
		}
		var resp types.GeminiResponse
		if isLast {
			candidate.FinishReason = "STOP"
			resp = types.GeminiResponse{
				Candidates:    []types.GeminiCandidate{candidate},
				UsageMetadata: types.GeminiUsageMetadata{PromptTokenCount: 5, CandidatesTokenCount: 3},
			}
		} else {
			resp = types.GeminiResponse{Candidates: []types.GeminiCandidate{candidate}}
		}

		b, _ := json.Marshal(resp)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()

		select {
		case <-r.Context().Done():
			return
		default:
		}
	}
}

// gemini500Handler returns HTTP 500 for any request — simulates a bad key.
func gemini500Handler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusInternalServerError)
	w.Write([]byte(`{"error":{"code":500,"message":"internal error","status":"INTERNAL"}}`))
}

// gemini429Handler returns a Gemini-style 429 with no retryDelay.
// The CredPool applies its default tier cooldown (10 s) before the key is re-eligible.
func gemini429Handler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	w.Write([]byte(`{"error":{"code":429,"message":"quota exceeded","status":"RESOURCE_EXHAUSTED"}}`))
}

// gemini429LongCooldownHandler returns a 429 with a 60 s retryDelay.
// Use when tests need the key to stay in cooldown across multiple requests
// (i.e. the key should still be cooling when the next request arrives).
func gemini429LongCooldownHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	w.Write([]byte(`{"error":{"code":429,"message":"quota exceeded","status":"RESOURCE_EXHAUSTED","details":[{"retryDelay":"60s"}]}}`))
}

// --- Miroxy test server helpers ---

// miroxyConfig controls how newTestServer builds the miroxy server.
type miroxyConfig struct {
	keys      []string // upstream keys (passed to the CredPool)
	threshold int      // circuit-break threshold (default 1 for fast failover tests)
	stubURL   string
}

// newTestServer builds an httptest.Server running miroxy, backed by the given stubURL.
func newTestServer(t *testing.T, cfg miroxyConfig) *httptest.Server {
	t.Helper()

	if len(cfg.keys) == 0 {
		cfg.keys = []string{"stub-key"}
	}
	if cfg.threshold == 0 {
		cfg.threshold = 1
	}

	keyEntries := make([]config.KeyEntry, len(cfg.keys))
	for i, k := range cfg.keys {
		keyEntries[i] = config.KeyEntry{Key: k}
	}

	appCfg := &config.Config{
		Auth: config.AuthConfig{AllowedKeys: []string{testClientKey}},
		ModelRoutes: []config.ModelEntry{{
			ModelName:     "claude-haiku",
			Provider:      "gemini",
			ProviderModel: "gemini-2.5-flash",
			KeyPool: config.KeyPoolCfg{
				Strategy:              "round_robin",
				CircuitBreakThreshold: cfg.threshold,
				CooldownSeconds:       1,
				Keys:                  keyEntries,
			},
			TimeoutSeconds: 5,
		}},
	}

	trans := map[string]coreup.UpstreamAdapter{
		"claude-haiku": intup.NewGeminiWithBase("gemini-2.5-flash", cfg.stubURL),
	}

	srv := server.NewWithTranslators(appCfg, trans)
	return httptest.NewServer(srv.Handler())
}

// --- HTTP client helpers ---

func doPost(t *testing.T, serverURL, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, serverURL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testClientKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeJSON(t *testing.T, r io.Reader, v any) {
	t.Helper()
	if err := json.NewDecoder(r).Decode(v); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
}

// --- SSE reader ---

type sseEvent struct {
	event string
	data  []byte
}

// readSSEEvents reads all SSE events from r until EOF.
func readSSEEvents(t *testing.T, r io.Reader) []sseEvent {
	t.Helper()
	scanner := bufio.NewScanner(r)
	var events []sseEvent
	var cur string
	for scanner.Scan() {
		line := scanner.Text()
		if ev, ok := strings.CutPrefix(line, "event: "); ok {
			cur = ev
			continue
		}
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			events = append(events, sseEvent{event: cur, data: []byte(data)})
			cur = ""
		}
	}
	return events
}

// --- Request body constants ---

const (
	nonStreamBody = `{"model":"claude-haiku","messages":[{"role":"user","content":"Say hello."}],"max_tokens":100}`
	streamBody    = `{"model":"claude-haiku","messages":[{"role":"user","content":"Say hello."}],"max_tokens":100,"stream":true}`
)

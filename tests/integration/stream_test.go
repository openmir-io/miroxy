package integration_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"miroxy/internal/types"
)

// TestStreaming_HappyPath is mandatory scenario 3 (partial) from the design doc:
// mock upstream emits chunks; client receives correctly-shaped Anthropic SSE events.
func TestStreaming_HappyPath(t *testing.T) {
	stub := newStubGemini(t, nil, nil) // default stream handler sends ["Hello", " world"]
	defer stub.Close()

	miroxy := newTestServer(t, miroxyConfig{stubURL: stub.URL, keys: []string{"key-a"}})
	defer miroxy.Close()

	resp := doPost(t, miroxy.URL, streamBody)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type: got %q, want text/event-stream", ct)
	}

	events := readSSEEvents(t, resp.Body)
	eventTypes := make([]string, len(events))
	for i, e := range events {
		eventTypes[i] = e.event
	}

	// Verify the required Anthropic SSE event sequence.
	// ping is now emitted immediately after message_start (before content arrives).
	want := []string{
		"message_start",
		"ping",
		"content_block_start",
		"content_block_delta", // "Hello"
		"content_block_delta", // " world"
		"content_block_stop",
		"message_delta",
		"message_stop",
	}
	if len(eventTypes) != len(want) {
		t.Fatalf("event count: got %d (%v), want %d (%v)", len(eventTypes), eventTypes, len(want), want)
	}
	for i, got := range eventTypes {
		if got != want[i] {
			t.Errorf("event[%d]: got %q, want %q", i, got, want[i])
		}
	}
}

// TestStreaming_EventContents verifies the structural content of key SSE events.
func TestStreaming_EventContents(t *testing.T) {
	stub := newStubGemini(t, nil, nil)
	defer stub.Close()
	miroxy := newTestServer(t, miroxyConfig{stubURL: stub.URL, keys: []string{"key-a"}})
	defer miroxy.Close()

	resp := doPost(t, miroxy.URL, streamBody)
	defer resp.Body.Close()

	events := readSSEEvents(t, resp.Body)
	byType := make(map[string][]sseEvent)
	for _, e := range events {
		byType[e.event] = append(byType[e.event], e)
	}

	// message_start: has id, role, model
	{
		var d types.MessageStartData
		if err := json.Unmarshal(byType["message_start"][0].data, &d); err != nil {
			t.Fatalf("parse message_start: %v", err)
		}
		if !strings.HasPrefix(d.Message.ID, "msg_") {
			t.Errorf("message_start.id format: %q", d.Message.ID)
		}
		if d.Message.Role != "assistant" {
			t.Errorf("message_start.role: %q", d.Message.Role)
		}
		if d.Message.Model != "claude-haiku" {
			t.Errorf("message_start.model: %q", d.Message.Model)
		}
	}

	// content_block_delta: combined text == "Hello world"
	{
		var combined strings.Builder
		for _, e := range byType["content_block_delta"] {
			var d types.ContentBlockDeltaData
			if err := json.Unmarshal(e.data, &d); err != nil {
				t.Fatalf("parse content_block_delta: %v", err)
			}
			if d.Delta.Type != "text_delta" {
				t.Errorf("delta type: got %q, want text_delta", d.Delta.Type)
			}
			combined.WriteString(d.Delta.Text)
		}
		if got := combined.String(); got != "Hello world" {
			t.Errorf("combined text: got %q, want %q", got, "Hello world")
		}
	}

	// message_delta: stop_reason present
	{
		var d types.MessageDeltaData
		if err := json.Unmarshal(byType["message_delta"][0].data, &d); err != nil {
			t.Fatalf("parse message_delta: %v", err)
		}
		if d.Delta.StopReason != "end_turn" {
			t.Errorf("stop_reason: got %q, want end_turn", d.Delta.StopReason)
		}
		if d.Usage.OutputTokens <= 0 {
			t.Errorf("output_tokens: got %d, want > 0", d.Usage.OutputTokens)
		}
	}
}

// TestStreaming_ClientDisconnect_CancelsUpstream is mandatory scenario 3 (cancellation):
// client disconnects mid-stream → miroxy closes the upstream connection.
//
// Detection strategy: the stub sends SSE heartbeat comments (": ka") every 50 ms after
// the first data chunk. When miroxy closes the upstream connection, the stub's write
// fails and it signals disconnection. Using write-error detection rather than
// r.Context().Done() because HTTP/1.1 does not guarantee prompt context cancellation
// on the server side without an attempted write.
func TestStreaming_ClientDisconnect_CancelsUpstream(t *testing.T) {
	disconnected := make(chan struct{})

	blockingStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "need flusher", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")

		// Emit one data chunk so the client starts reading.
		chunk := types.GeminiResponse{
			Candidates: []types.GeminiCandidate{{
				Content: types.GeminiContent{
					Role:  "model",
					Parts: []types.GeminiPart{{Text: "partial"}},
				},
			}},
		}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()

		// Poll for disconnect every 50 ms via write error or context cancellation.
		// SSE comments (": ...") are ignored by parsers so heartbeats are safe to emit.
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case <-r.Context().Done():
				close(disconnected)
				return
			case <-ticker.C:
				if _, err := fmt.Fprintf(w, ": ka\n\n"); err != nil {
					close(disconnected)
					return
				}
				flusher.Flush()
			case <-deadline:
				return // test will catch the timeout below
			}
		}
	}))
	defer blockingStub.Close()

	miroxy := newTestServer(t, miroxyConfig{
		stubURL: blockingStub.URL,
		keys:    []string{"key-a"},
	})
	defer miroxy.Close()

	// Use a cancellable context so we can cleanly abort the HTTP request.
	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		miroxy.URL+"/v1/messages", strings.NewReader(streamBody))
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

	// Read line-by-line; cancel the request when we see the first content_block_delta.
	found := false
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), "content_block_delta") && !found {
			found = true
			cancelReq() // aborts the HTTP request, closing the TCP connection
			break
		}
	}

	if !found {
		t.Fatal("did not receive content_block_delta before cancelling")
	}

	select {
	case <-disconnected:
		// stub detected upstream connection closed — pass
	case <-time.After(3 * time.Second):
		t.Error("upstream did not detect client disconnect within 3s")
	}
}

// TestStreaming_MaxTokensFinishReason verifies MAX_TOKENS maps to "max_tokens".
func TestStreaming_MaxTokensFinishReason(t *testing.T) {
	stub := newStubGemini(t, nil, func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		resp := types.GeminiResponse{
			Candidates: []types.GeminiCandidate{{
				Content:      types.GeminiContent{Role: "model", Parts: []types.GeminiPart{{Text: "truncated"}}},
				FinishReason: "MAX_TOKENS",
			}},
			UsageMetadata: types.GeminiUsageMetadata{PromptTokenCount: 5, CandidatesTokenCount: 10},
		}
		b, _ := json.Marshal(resp)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	})
	defer stub.Close()

	miroxy := newTestServer(t, miroxyConfig{stubURL: stub.URL})
	defer miroxy.Close()

	resp := doPost(t, miroxy.URL, streamBody)
	defer resp.Body.Close()

	events := readSSEEvents(t, resp.Body)
	for _, e := range events {
		if e.event == "message_delta" {
			var d types.MessageDeltaData
			if err := json.Unmarshal(e.data, &d); err != nil {
				t.Fatal(err)
			}
			if d.Delta.StopReason != "max_tokens" {
				t.Errorf("stop_reason: got %q, want max_tokens", d.Delta.StopReason)
			}
			return
		}
	}
	t.Error("message_delta event not received")
}

// TestStreaming_ToolsTranslated verifies that streaming requests with tool definitions
// are forwarded (not rejected). The stub returns a plain text stream.
func TestStreaming_ToolsTranslated(t *testing.T) {
	stub := newStubGemini(t, nil, nil)
	defer stub.Close()
	miroxy := newTestServer(t, miroxyConfig{stubURL: stub.URL})
	defer miroxy.Close()

	body := `{"model":"claude-haiku","messages":[{"role":"user","content":"hi"}],"max_tokens":10,"stream":true,"tools":[{"name":"search","input_schema":{"type":"object","properties":{}}}]}`
	resp := doPost(t, miroxy.URL, body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for streaming tool request, got %d", resp.StatusCode)
	}
}

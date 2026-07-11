// Package downstream contains DownstreamAdapter implementations — one per
// client-facing wire protocol.
package downstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	coredown "miroxy/core/downstream"
	"miroxy/internal/types"
)

// Ensure compile-time interface satisfaction.
var _ coredown.DownstreamAdapter = (*AnthropicAdapter)(nil)

// AnthropicAdapter handles POST /v1/messages.
// Clients: Claude Code, Cursor, Windsurf, any Anthropic-SDK client.
type AnthropicAdapter struct{}

func (a *AnthropicAdapter) Protocol() string { return "anthropic" }
func (a *AnthropicAdapter) Path() string     { return "/v1/messages" }

// Decode parses the Anthropic Messages request body and normalises it.
// NormalizeSystem extracts any role="system" message into the top-level
// system field — some client skills inject it that way (OpenAI convention).
func (a *AnthropicAdapter) Decode(r *http.Request) (*types.MessageRequest, error) {
	var req types.MessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, fmt.Errorf("invalid request body: %w", err)
	}
	req.NormalizeSystem()
	if err := req.Validate(); err != nil {
		return nil, err
	}
	return &req, nil
}

func (a *AnthropicAdapter) WriteError(w http.ResponseWriter, status int, errType, msg string) {
	writeJSON(w, status, types.ErrorResponse{
		Type:  "error",
		Error: types.ErrorBody{Type: errType, Message: msg},
	})
}

func (a *AnthropicAdapter) WriteResponse(w http.ResponseWriter, resp *types.MessageResponse) {
	writeJSON(w, http.StatusOK, resp)
}

// WriteResponseAsStream emits the response as Anthropic SSE events.
func (a *AnthropicAdapter) WriteResponseAsStream(ctx context.Context, w http.ResponseWriter, resp *types.MessageResponse) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		a.WriteResponse(w, resp)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(event string, data any) {
		_ = writeSSEEvent(w, event, jsonRawOf(data))
		flusher.Flush()
	}

	// Emit minimal Anthropic SSE sequence from the response.
	send("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": resp.ID, "type": "message",
			"role": "assistant", "model": resp.Model,
			"content": []any{},
			"usage":   map[string]any{"input_tokens": 0},
		},
	})
	for i, block := range resp.Content {
		if block.Type != "text" {
			continue
		}
		send("content_block_start", map[string]any{
			"type": "content_block_start", "index": i,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		send("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": i,
			"delta": map[string]any{"type": "text_delta", "text": block.Text},
		})
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
	}
	send("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": resp.Usage.OutputTokens},
	})
	send("message_stop", map[string]any{"type": "message_stop"})
}

// WriteStream delivers Anthropic SSE events to the client.
func (a *AnthropicAdapter) WriteStream(ctx context.Context, w http.ResponseWriter, _ *types.MessageRequest, src <-chan types.SSEEvent) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming not supported by server configuration")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-src:
			if !ok {
				return nil
			}
			if err := writeSSEEvent(w, ev.Event, jsonRawOf(ev.Data)); err != nil {
				return err
			}
			flusher.Flush()
		}
	}
}

// ── shared helpers ────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeSSEEvent(w http.ResponseWriter, event string, data json.RawMessage) error {
	_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	return err
}

// jsonRawOf converts an any value that is already JSON-encoded bytes into
// json.RawMessage.  types.SSEEvent.Data is typed as any but always holds
// json.RawMessage at runtime.
func jsonRawOf(v any) json.RawMessage {
	if raw, ok := v.(json.RawMessage); ok {
		return raw
	}
	b, _ := json.Marshal(v)
	return b
}

// Package downstream contains DownstreamAdapter implementations — one per
// client-facing wire protocol.
package downstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	coredown "miroxy/core/downstream"
	"miroxy/core/ir"
	"miroxy/internal/idgen"
	"miroxy/internal/types"
	"miroxy/internal/wireformat"
)

// Ensure compile-time interface satisfaction.
var _ coredown.DownstreamAdapter = (*AnthropicAdapter)(nil)

// AnthropicAdapter handles POST /v1/messages.
// Clients: Claude Code, Cursor, Windsurf, any Anthropic-SDK client.
type AnthropicAdapter struct{}

func (a *AnthropicAdapter) Protocol() string { return "anthropic" }
func (a *AnthropicAdapter) Path() string     { return "/v1/messages" }

// Decode parses the Anthropic Messages request body, normalises it, and
// converts it to the canonical IR. NormalizeSystem extracts any
// role="system" message into the top-level system field — some client
// skills inject it that way (OpenAI convention).
func (a *AnthropicAdapter) Decode(r *http.Request) (*ir.IRRequest, string, error) {
	var req types.MessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, "", fmt.Errorf("invalid request body: %w", err)
	}
	req.NormalizeSystem()
	if err := req.Validate(); err != nil {
		return nil, "", err
	}
	irReq, err := (wireformat.AnthropicConverter{}).RequestToIR(&req)
	if err != nil {
		return nil, "", fmt.Errorf("request to IR: %w", err)
	}
	return irReq, req.Model, nil
}

// EncodeRequest is the reverse of Decode.
func (a *AnthropicAdapter) EncodeRequest(req *ir.IRRequest, model string) ([]byte, error) {
	return json.Marshal((wireformat.AnthropicConverter{}).RequestFromIR(req, model))
}

func (a *AnthropicAdapter) WriteError(w http.ResponseWriter, status int, errType, msg string) {
	writeJSON(w, status, types.ErrorResponse{
		Type:  "error",
		Error: types.ErrorBody{Type: errType, Message: msg},
	})
}

func (a *AnthropicAdapter) WriteResponse(w http.ResponseWriter, resp *ir.IRResponse, msgID, model string) {
	writeJSON(w, http.StatusOK, (wireformat.AnthropicConverter{}).ResponseFromIR(resp, msgID, model))
}

// WriteResponseAsStream emits the response as Anthropic SSE events.
func (a *AnthropicAdapter) WriteResponseAsStream(ctx context.Context, w http.ResponseWriter, resp *ir.IRResponse, msgID, model string) {
	wire := (wireformat.AnthropicConverter{}).ResponseFromIR(resp, msgID, model)

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusOK, wire)
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
			"id": wire.ID, "type": "message",
			"role": "assistant", "model": wire.Model,
			"content": []any{},
			"usage":   map[string]any{"input_tokens": 0},
		},
	})
	for i, block := range wire.Content {
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
		"usage": map[string]any{"output_tokens": wire.Usage.OutputTokens},
	})
	send("message_stop", map[string]any{"type": "message_stop"})
}

// WriteStream delivers Anthropic SSE events to the client.
func (a *AnthropicAdapter) WriteStream(ctx context.Context, w http.ResponseWriter, model string, src <-chan ir.StreamEvent) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming not supported by server configuration")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	wire := make(chan types.SSEEvent, 32)
	go func() {
		defer close(wire)
		(wireformat.AnthropicConverter{}).StreamFromIR(ctx, src, wire, idgen.NewMsgID(), model)
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-wire:
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

// Package downstream — OpenAI Responses API adapter.
// Handles POST /v1/responses (Codex CLI wire_api = "responses").
//
// Mapping:
//   instructions          → Anthropic system (top-level)
//   input[type=message]   → messages[]
//   input[type=function_call]        → assistant message, tool_use block
//   input[type=function_call_output] → user message, tool_result block
//   input[type=reasoning]            → skipped (provider-managed)
//
// SSE (Anthropic → Responses):
//   message_start          → response.created
//   content_block_start    → response.output_item.added
//   content_block_delta    → response.output_text.delta / response.function_call_arguments.delta
//   content_block_stop     → response.output_item.done
//   message_delta/stop     → response.completed
package downstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	coredown "miroxy/core/downstream"
	"miroxy/internal/idgen"
	"miroxy/internal/types"
)

var _ coredown.DownstreamAdapter = (*ResponsesAdapter)(nil)

// ResponsesAdapter handles POST /v1/responses.
// Clients: Codex CLI (wire_api = "responses").
type ResponsesAdapter struct{}

func (a *ResponsesAdapter) Protocol() string { return "openai-responses" }
func (a *ResponsesAdapter) Path() string     { return "/v1/responses" }

// ── request types ─────────────────────────────────────────────────────────────

type responsesRequest struct {
	Model           string            `json:"model"`
	Instructions    string            `json:"instructions,omitempty"`
	Input           []json.RawMessage `json:"input"`
	Stream          bool              `json:"stream,omitempty"`
	MaxOutputTokens int               `json:"max_output_tokens,omitempty"`
	Tools           []json.RawMessage `json:"tools,omitempty"`
	ToolChoice      any               `json:"tool_choice,omitempty"`
}

type inputItem struct {
	Type      string            `json:"type"`
	// message fields
	Role    string            `json:"role,omitempty"`
	Content []json.RawMessage `json:"content,omitempty"`
	// function_call fields
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	ID        string `json:"id,omitempty"`
	// function_call_output fields
	Output json.RawMessage `json:"output,omitempty"`
}

type inputContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

// ── Decode ────────────────────────────────────────────────────────────────────

func (a *ResponsesAdapter) Decode(r *http.Request) (*types.MessageRequest, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	var rreq responsesRequest
	if err := json.Unmarshal(body, &rreq); err != nil {
		return nil, fmt.Errorf("parse responses request: %w", err)
	}

	req := &types.MessageRequest{
		Model:     rreq.Model,
		Stream:    rreq.Stream,
		MaxTokens: rreq.MaxOutputTokens,
	}
	if req.MaxTokens == 0 {
		req.MaxTokens = 8192
	}

	// instructions → system
	if rreq.Instructions != "" {
		b, _ := json.Marshal(rreq.Instructions)
		req.System = b
	}

	// Tools: Responses API uses same OpenAI function schema.
	// Pass through as raw JSON for irc layer to handle.
	// (req.Tools is []types.Tool; skip for now — tool calling via chat completions
	// path is more complete; Responses tool support is additive.)

	// "developer" role is OpenAI GPT-4.5/5.5's replacement for "system".
	// Extract it here — it must never reach Anthropic's message validation.
	devSystem, msgs := splitDeveloperRole(rreq.Input)
	if devSystem != "" && req.System == nil {
		b, _ := json.Marshal(devSystem)
		req.System = b
	}
	req.Messages = msgs
	req.NormalizeSystem()
	if err := req.Validate(); err != nil {
		return nil, err
	}
	return req, nil
}

// splitDeveloperRole separates developer-role messages from regular input items.
// Returns the combined developer text (for use as system prompt) and the
// remaining input items converted to Anthropic messages.
func splitDeveloperRole(raw []json.RawMessage) (developerText string, msgs []types.Message) {
	var devParts []string
	var remaining []json.RawMessage

	for _, r := range raw {
		var item inputItem
		if err := json.Unmarshal(r, &item); err != nil {
			remaining = append(remaining, r)
			continue
		}
		if item.Type == "message" && item.Role == "developer" {
			// Collect text from developer-role content blocks.
			for _, cr := range item.Content {
				var c inputContent
				if json.Unmarshal(cr, &c) == nil && c.Text != "" {
					devParts = append(devParts, c.Text)
				}
			}
			continue
		}
		remaining = append(remaining, r)
	}

	developerText = strings.Join(devParts, "\n")
	msgs = convertInputItems(remaining)
	return developerText, msgs
}

// convertInputItems maps Responses input[] → Anthropic messages[].
func convertInputItems(raw []json.RawMessage) []types.Message {
	var msgs []types.Message

	for _, r := range raw {
		var item inputItem
		if err := json.Unmarshal(r, &item); err != nil {
			continue
		}
		switch item.Type {
		case "message":
			msgs = append(msgs, convertMessage(item))

		case "function_call":
			// Tool use → assistant message with tool_use block.
			block := types.ContentBlock{
				Type:  "tool_use",
				ID:    item.ID,
				Name:  item.Name,
				Input: json.RawMessage(item.Arguments),
			}
			if block.ID == "" {
				block.ID = item.CallID
			}
			blockJSON, _ := json.Marshal([]types.ContentBlock{block})
			msgs = appendOrMerge(msgs, "assistant", blockJSON)

		case "function_call_output":
			// Tool result → user message with tool_result block.
			block := types.ContentBlock{
				Type:      "tool_result",
				ToolUseID: item.CallID,
				Content:   item.Output,
			}
			blockJSON, _ := json.Marshal([]types.ContentBlock{block})
			msgs = appendOrMerge(msgs, "user", blockJSON)

		case "reasoning":
			// Skip — managed by the provider.
		}
	}
	return msgs
}

// convertMessage converts a Responses message item to an Anthropic message.
func convertMessage(item inputItem) types.Message {
	if len(item.Content) == 0 {
		return types.Message{Role: item.Role, Content: json.RawMessage(`""`)}
	}
	var blocks []types.ContentBlock
	for _, cr := range item.Content {
		var c inputContent
		if err := json.Unmarshal(cr, &c); err != nil {
			continue
		}
		switch c.Type {
		case "input_text", "output_text", "text":
			blocks = append(blocks, types.ContentBlock{Type: "text", Text: c.Text})
		case "input_image":
			blocks = append(blocks, types.ContentBlock{Type: "image", Content: cr})
		default:
			blocks = append(blocks, types.ContentBlock{Type: "text", Text: c.Text})
		}
	}
	b, _ := json.Marshal(blocks)
	return types.Message{Role: item.Role, Content: b}
}

// appendOrMerge appends a new message or merges content blocks into the last
// message if it has the same role (handles consecutive tool results).
func appendOrMerge(msgs []types.Message, role string, contentBlocks json.RawMessage) []types.Message {
	if len(msgs) > 0 && msgs[len(msgs)-1].Role == role {
		last := &msgs[len(msgs)-1]
		var existing, incoming []types.ContentBlock
		_ = json.Unmarshal(last.Content, &existing)
		_ = json.Unmarshal(contentBlocks, &incoming)
		merged, _ := json.Marshal(append(existing, incoming...))
		last.Content = merged
		return msgs
	}
	return append(msgs, types.Message{Role: role, Content: contentBlocks})
}

// ── WriteError ────────────────────────────────────────────────────────────────

func (a *ResponsesAdapter) WriteError(w http.ResponseWriter, status int, errType, msg string) {
	type respErr struct {
		Type  string `json:"type"`
		Code  string `json:"code"`
		Msg   string `json:"message"`
	}
	type envelope struct {
		Error respErr `json:"error"`
	}
	writeJSON(w, status, envelope{Error: respErr{Type: "error", Code: errType, Msg: msg}})
}

// ── WriteResponse (non-streaming) ─────────────────────────────────────────────

func (a *ResponsesAdapter) WriteResponse(w http.ResponseWriter, resp *types.MessageResponse) {
	var textSB strings.Builder
	for _, block := range resp.Content {
		if block.Type == "text" {
			textSB.WriteString(block.Text)
		}
	}
	out := map[string]any{
		"id":     "resp_" + resp.ID,
		"object": "response",
		"model":  resp.Model,
		"status": "completed",
		"output": []map[string]any{
			{
				"type":    "message",
				"role":    "assistant",
				"content": []map[string]any{{"type": "output_text", "text": textSB.String()}},
			},
		},
		"usage": map[string]any{
			"input_tokens":  resp.Usage.InputTokens,
			"output_tokens": resp.Usage.OutputTokens,
			"total_tokens":  resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	}
	writeJSON(w, http.StatusOK, out)
}

// WriteResponseAsStream emits a Responses API SSE sequence directly from a
// canonical response — no Anthropic SSE intermediate format involved.
func (a *ResponsesAdapter) WriteResponseAsStream(ctx context.Context, w http.ResponseWriter, resp *types.MessageResponse) {
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

	respID := "resp_" + resp.ID
	msgItemID := "msg_" + resp.ID

	send := func(event string, data any) {
		b, _ := json.Marshal(data)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}

	send("response.created", map[string]any{
		"type":     "response.created",
		"response": map[string]any{"id": respID, "object": "response", "status": "in_progress"},
	})
	send("response.output_item.added", map[string]any{
		"type":         "response.output_item.added",
		"output_index": 0,
		"item":         map[string]any{"id": msgItemID, "type": "message", "role": "assistant"},
	})

	var fullText strings.Builder
	for _, block := range resp.Content {
		if block.Type == "text" {
			fullText.WriteString(block.Text)
			send("response.output_text.delta", map[string]any{
				"type":          "response.output_text.delta",
				"item_id":       msgItemID,
				"output_index":  0,
				"content_index": 0,
				"delta":         block.Text,
			})
		}
	}
	send("response.output_item.done", map[string]any{
		"type":         "response.output_item.done",
		"output_index": 0,
		"item": map[string]any{
			"id":      msgItemID,
			"type":    "message",
			"role":    "assistant",
			"content": []map[string]any{{"type": "output_text", "text": fullText.String()}},
		},
	})
	send("response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":     respID,
			"object": "response",
			"status": "completed",
			"usage": map[string]any{
				"input_tokens":  resp.Usage.InputTokens,
				"output_tokens": resp.Usage.OutputTokens,
				"total_tokens":  resp.Usage.InputTokens + resp.Usage.OutputTokens,
			},
		},
	})
}

// ── WriteStream ───────────────────────────────────────────────────────────────

// responseBlockState tracks the current content block during SSE translation.
type responseBlockState struct {
	outputIndex int
	itemID      string
	blockType   string // "text" or "tool_use"
	toolName    string
	toolID      string
	// accumulated text for output_item.done content field
	textBuf strings.Builder
}

func (a *ResponsesAdapter) WriteStream(ctx context.Context, w http.ResponseWriter, req *types.MessageRequest, src <-chan types.SSEEvent) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming not supported")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	respID := "resp_" + idgen.NewMsgID()
	msgItemID := "msg_" + idgen.NewMsgID()

	send := func(eventType string, data any) error {
		b, _ := json.Marshal(data)
		_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, b)
		if err == nil {
			flusher.Flush()
		}
		return err
	}

	// Emit response.created once.
	_ = send("response.created", map[string]any{
		"type":     "response.created",
		"response": map[string]any{"id": respID, "object": "response", "status": "in_progress"},
	})

	var cur *responseBlockState
	outputIndex := 0
	var inputTokens, outputTokens int

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-src:
			if !ok {
				return nil
			}
			if err := a.translateEvent(ev, send, &cur, &outputIndex, msgItemID, &inputTokens, &outputTokens, respID); err != nil {
				return err
			}
		}
	}
}

func (a *ResponsesAdapter) translateEvent(
	ev types.SSEEvent,
	send func(string, any) error,
	cur **responseBlockState,
	outputIndex *int,
	msgItemID string,
	inputTokens, outputTokens *int,
	respID string,
) error {
	raw := jsonRawOf(ev.Data)

	switch ev.Event {
	case "message_start":
		// Already emitted response.created above.
		var ms struct {
			Message struct {
				Usage struct {
					InputTokens int `json:"input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		_ = json.Unmarshal(raw, &ms)
		*inputTokens = ms.Message.Usage.InputTokens

	case "content_block_start":
		var cbs struct {
			Index        int `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
		}
		_ = json.Unmarshal(raw, &cbs)

		itemID := "item_" + idgen.NewMsgID()
		*cur = &responseBlockState{
			outputIndex: *outputIndex,
			itemID:      itemID,
			blockType:   cbs.ContentBlock.Type,
			toolName:    cbs.ContentBlock.Name,
			toolID:      cbs.ContentBlock.ID,
		}

		if cbs.ContentBlock.Type == "text" {
			// First text block: emit the message item wrapper.
			return send("response.output_item.added", map[string]any{
				"type":         "response.output_item.added",
				"output_index": *outputIndex,
				"item": map[string]any{
					"id":   msgItemID,
					"type": "message",
					"role": "assistant",
				},
			})
		} else if cbs.ContentBlock.Type == "tool_use" {
			return send("response.output_item.added", map[string]any{
				"type":         "response.output_item.added",
				"output_index": *outputIndex,
				"item": map[string]any{
					"id":        cbs.ContentBlock.ID,
					"type":      "function_call",
					"name":      cbs.ContentBlock.Name,
					"arguments": "",
					"call_id":   cbs.ContentBlock.ID,
				},
			})
		}

	case "content_block_delta":
		if *cur == nil {
			return nil
		}
		var cbd struct {
			Delta struct {
				Type      string `json:"type"`
				Text      string `json:"text"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		_ = json.Unmarshal(raw, &cbd)

		if (*cur).blockType == "text" && cbd.Delta.Text != "" {
			(*cur).textBuf.WriteString(cbd.Delta.Text)
			return send("response.output_text.delta", map[string]any{
				"type":          "response.output_text.delta",
				"item_id":       msgItemID,
				"output_index":  (*cur).outputIndex,
				"content_index": 0,
				"delta":         cbd.Delta.Text,
			})
		} else if (*cur).blockType == "tool_use" && cbd.Delta.PartialJSON != "" {
			return send("response.function_call_arguments.delta", map[string]any{
				"type":         "response.function_call_arguments.delta",
				"item_id":      (*cur).toolID,
				"output_index": (*cur).outputIndex,
				"delta":        cbd.Delta.PartialJSON,
			})
		}

	case "content_block_stop":
		if *cur == nil {
			return nil
		}
		var doneItem map[string]any
		if (*cur).blockType == "text" {
			doneItem = map[string]any{
				"id":   msgItemID,
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{
					{"type": "output_text", "text": (*cur).textBuf.String()},
				},
			}
		} else {
			doneItem = map[string]any{
				"id":        (*cur).toolID,
				"type":      "function_call",
				"name":      (*cur).toolName,
				"call_id":   (*cur).toolID,
			}
		}
		err := send("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": (*cur).outputIndex,
			"item":         doneItem,
		})
		*outputIndex++
		*cur = nil
		return err

	case "message_delta":
		var md struct {
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		_ = json.Unmarshal(raw, &md)
		*outputTokens = md.Usage.OutputTokens

	case "message_stop":
		return send("response.completed", map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":     respID,
				"object": "response",
				"status": "completed",
				"usage": map[string]any{
					"input_tokens":  *inputTokens,
					"output_tokens": *outputTokens,
					"total_tokens":  *inputTokens + *outputTokens,
				},
			},
		})
	}
	return nil
}

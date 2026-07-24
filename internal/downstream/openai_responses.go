// Package downstream — OpenAI Responses API adapter.
// Handles POST /v1/responses (Codex CLI wire_api = "responses").
//
// Mapping:
//
//	instructions          → Anthropic system (top-level)
//	input[type=message]   → messages[]
//	input[type=function_call]        → assistant message, tool_use block
//	input[type=function_call_output] → user message, tool_result block
//	input[type=reasoning]            → assistant message, thinking/redacted_thinking block
//
// SSE (Anthropic → Responses):
//
//	message_start          → response.created
//	content_block_start    → response.output_item.added
//	content_block_delta    → response.output_text.delta / response.function_call_arguments.delta
//	content_block_stop     → response.output_item.done
//	message_delta/stop     → response.completed
package downstream

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	coredown "miroxy/core/downstream"
	"miroxy/core/ir"
	"miroxy/internal/idgen"
	"miroxy/internal/types"
	"miroxy/internal/wireformat"
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
	Type string `json:"type"`
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
	// reasoning fields
	Summary          []json.RawMessage `json:"summary,omitempty"`
	EncryptedContent string            `json:"encrypted_content,omitempty"`
}

type inputContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

// ── Decode ────────────────────────────────────────────────────────────────────

// Decode parses a Responses API request into the canonical IR. The
// Anthropic-shaped intermediate here is an internal implementation detail
// of this one parsing step (reusing NormalizeSystem/Validate), not a
// pipeline-wide privilege — see docs/dev/DESIGNLOG.md, 2026-07-19.
func (a *ResponsesAdapter) Decode(r *http.Request) (*ir.IRRequest, string, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read body: %w", err)
	}
	var rreq responsesRequest
	if err := json.Unmarshal(body, &rreq); err != nil {
		return nil, "", fmt.Errorf("parse responses request: %w", err)
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
		return nil, "", err
	}
	irReq, err := (wireformat.AnthropicConverter{}).RequestToIR(req)
	if err != nil {
		return nil, "", fmt.Errorf("request to IR: %w", err)
	}
	return irReq, req.Model, nil
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
			text := reasoningSummaryText(item.Summary)
			var sig string
			if item.EncryptedContent != "" {
				sig = reasoningEnvelope(item)
			}
			if text == "" && sig == "" {
				continue
			}
			var block types.ContentBlock
			if text != "" {
				block = types.ContentBlock{Type: "thinking", Thinking: text, Signature: sig}
			} else {
				block = types.ContentBlock{Type: "redacted_thinking", Data: sig}
			}
			blockJSON, _ := json.Marshal([]types.ContentBlock{block})
			msgs = appendOrMerge(msgs, "assistant", blockJSON)
		}
	}
	return msgs
}

// reasoningSummaryText joins the visible text of a reasoning item's summary
// array (parts of type "summary_text"); other summary part types are skipped.
func reasoningSummaryText(summary []json.RawMessage) string {
	var parts []string
	for _, s := range summary {
		var c inputContent
		if json.Unmarshal(s, &c) == nil && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// reasoningEnvelope base64-encodes the original reasoning item's JSON so it
// can be reconstructed verbatim if this request round-trips back out through
// EncodeRequest — the Responses API requires reasoning items to be replayed
// exactly as issued, including their opaque encrypted_content.
func reasoningEnvelope(item inputItem) string {
	b, _ := json.Marshal(item)
	return base64.StdEncoding.EncodeToString(b)
}

// reasoningFromEnvelope reverses reasoningEnvelope. ok is false when raw is
// empty or not a valid envelope — e.g. a Reasoning part that originated from
// a different provider, carrying only visible text and no opaque blob.
func reasoningFromEnvelope(raw string) (item inputItem, ok bool) {
	if raw == "" {
		return inputItem{}, false
	}
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return inputItem{}, false
	}
	if err := json.Unmarshal(b, &item); err != nil {
		return inputItem{}, false
	}
	return item, true
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

// EncodeRequest converts the canonical IR back into a Responses API request
// body — the reverse of Decode.
func (a *ResponsesAdapter) EncodeRequest(req *ir.IRRequest, model string) ([]byte, error) {
	rreq := responsesRequest{
		Model:           model,
		Instructions:    req.System,
		Input:           messagesToInputItems(req.Messages),
		Stream:          req.Stream,
		MaxOutputTokens: req.Gen.MaxTokens,
		Tools:           toolsToResponsesTools(req.Tools),
		ToolChoice:      toolChoiceToResponses(req.ToolChoice),
	}
	return json.Marshal(rreq)
}

// messagesToInputItems converts IR messages back into Responses input items
// — the reverse of convertInputItems/convertMessage. tool_use/tool_result/
// reasoning parts become separate top-level items (mirroring how Decode
// received them); text/image parts stay nested inside a message item.
func messagesToInputItems(msgs []ir.IRMessage) []json.RawMessage {
	var items []json.RawMessage
	for _, m := range msgs {
		var contents []inputContent
		flush := func() {
			if len(contents) == 0 {
				return
			}
			items = append(items, marshalItem(inputItem{Type: "message", Role: m.Role, Content: marshalContents(contents)}))
			contents = nil
		}
		for _, p := range m.Parts {
			switch {
			case p.Text != nil:
				textType := "input_text"
				if m.Role == "assistant" {
					textType = "output_text"
				}
				contents = append(contents, inputContent{Type: textType, Text: p.Text.Text})

			case p.Image != nil:
				contents = append(contents, inputContent{Type: "input_image", ImageURL: p.Image.Data})

			case p.ToolUse != nil:
				flush()
				items = append(items, marshalItem(inputItem{
					Type: "function_call", Name: p.ToolUse.Name,
					Arguments: string(p.ToolUse.InputJSON), CallID: p.ToolUse.ID, ID: p.ToolUse.ID,
				}))

			case p.ToolResult != nil:
				flush()
				items = append(items, marshalItem(inputItem{
					Type: "function_call_output", CallID: p.ToolResult.ToolUseID,
					Output: toolResultOutputJSON(p.ToolResult.Content),
				}))

			case p.Reasoning != nil:
				flush()
				items = append(items, marshalReasoningItem(p.Reasoning))
			}
		}
		flush()
	}
	return items
}

func marshalItem(item inputItem) json.RawMessage {
	b, _ := json.Marshal(item)
	return b
}

func marshalContents(contents []inputContent) []json.RawMessage {
	out := make([]json.RawMessage, len(contents))
	for i, c := range contents {
		b, _ := json.Marshal(c)
		out[i] = b
	}
	return out
}

// toolResultOutputJSON flattens a tool result's text content parts into the
// plain string Responses expects for function_call_output.output.
func toolResultOutputJSON(content []ir.IRContentPart) json.RawMessage {
	var sb strings.Builder
	for _, p := range content {
		if p.Text != nil {
			sb.WriteString(p.Text.Text)
		}
	}
	b, _ := json.Marshal(sb.String())
	return b
}

// marshalReasoningItem renders an IR reasoning part back to a Responses
// input item. When Signature decodes as a valid envelope (the reasoning
// originated from a prior Responses request/response), the original item
// is reconstructed verbatim; otherwise a fresh item is synthesized from Text.
func marshalReasoningItem(r *ir.IRReasoningPart) json.RawMessage {
	if item, ok := reasoningFromEnvelope(r.Signature); ok {
		return marshalItem(item)
	}
	item := inputItem{Type: "reasoning", ID: "rs_" + idgen.NewMsgID()}
	if r.Text != "" {
		summaryBlock, _ := json.Marshal(inputContent{Type: "summary_text", Text: r.Text})
		item.Summary = []json.RawMessage{summaryBlock}
	}
	return marshalItem(item)
}

func toolsToResponsesTools(tools []ir.IRTool) []json.RawMessage {
	if len(tools) == 0 {
		return nil
	}
	out := make([]json.RawMessage, 0, len(tools))
	for _, t := range tools {
		b, _ := json.Marshal(map[string]any{
			"type":        "function",
			"name":        t.Name,
			"description": t.Description,
			"parameters":  json.RawMessage(t.InputSchemaJSON),
		})
		out = append(out, b)
	}
	return out
}

func toolChoiceToResponses(tc *ir.IRToolChoice) any {
	if tc == nil {
		return nil
	}
	switch tc.Type {
	case "tool":
		return map[string]any{"type": "function", "name": tc.Name}
	case "any":
		return "required"
	case "none":
		return "none"
	default:
		return "auto"
	}
}

// reasoningOutputItem renders an IR reasoning block to a Responses output
// item. When Signature decodes as a valid envelope, the original item's id/
// encrypted_content/summary are reconstructed verbatim; otherwise a fresh
// item is synthesized from Text alone.
func reasoningOutputItem(r *ir.IRReasoningPart) map[string]any {
	if envelope, ok := reasoningFromEnvelope(r.Signature); ok {
		item := map[string]any{"type": "reasoning", "id": envelope.ID, "summary": envelope.Summary}
		if envelope.EncryptedContent != "" {
			item["encrypted_content"] = envelope.EncryptedContent
		}
		return item
	}
	item := map[string]any{"type": "reasoning", "id": "rs_" + idgen.NewMsgID(), "summary": []map[string]any{}}
	if r.Text != "" {
		item["summary"] = []map[string]any{{"type": "summary_text", "text": r.Text}}
	}
	return item
}

// ── WriteError ────────────────────────────────────────────────────────────────

func (a *ResponsesAdapter) WriteError(w http.ResponseWriter, status int, errType, msg string) {
	type respErr struct {
		Type string `json:"type"`
		Code string `json:"code"`
		Msg  string `json:"message"`
	}
	type envelope struct {
		Error respErr `json:"error"`
	}
	writeJSON(w, status, envelope{Error: respErr{Type: "error", Code: errType, Msg: msg}})
}

// ── WriteResponse (non-streaming) ─────────────────────────────────────────────

func (a *ResponsesAdapter) WriteResponse(w http.ResponseWriter, resp *ir.IRResponse, msgID, model string) {
	var textSB strings.Builder
	var output []map[string]any
	for _, block := range resp.Content {
		if block.Text != nil {
			textSB.WriteString(block.Text.Text)
		}
		if block.Reasoning != nil {
			output = append(output, reasoningOutputItem(block.Reasoning))
		}
	}
	output = append(output, map[string]any{
		"type":    "message",
		"role":    "assistant",
		"content": []map[string]any{{"type": "output_text", "text": textSB.String()}},
	})
	out := map[string]any{
		"id":     "resp_" + msgID,
		"object": "response",
		"model":  model,
		"status": "completed",
		"output": output,
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
func (a *ResponsesAdapter) WriteResponseAsStream(ctx context.Context, w http.ResponseWriter, resp *ir.IRResponse, msgID, model string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		a.WriteResponse(w, resp, msgID, model)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	respID := "resp_" + msgID
	msgItemID := "msg_" + msgID

	send := func(event string, data any) {
		b, _ := json.Marshal(data)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}

	send("response.created", map[string]any{
		"type":     "response.created",
		"response": map[string]any{"id": respID, "object": "response", "status": "in_progress"},
	})

	outputIndex := 0
	for _, block := range resp.Content {
		if block.Reasoning == nil {
			continue
		}
		item := reasoningOutputItem(block.Reasoning)
		send("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": outputIndex, "item": item})
		send("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": outputIndex, "item": item})
		outputIndex++
	}

	send("response.output_item.added", map[string]any{
		"type":         "response.output_item.added",
		"output_index": outputIndex,
		"item":         map[string]any{"id": msgItemID, "type": "message", "role": "assistant"},
	})

	var fullText strings.Builder
	for _, block := range resp.Content {
		if block.Text != nil {
			fullText.WriteString(block.Text.Text)
			send("response.output_text.delta", map[string]any{
				"type":          "response.output_text.delta",
				"item_id":       msgItemID,
				"output_index":  outputIndex,
				"content_index": 0,
				"delta":         block.Text.Text,
			})
		}
	}
	send("response.output_item.done", map[string]any{
		"type":         "response.output_item.done",
		"output_index": outputIndex,
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
	blockType   string // "text", "tool_use", or "reasoning"
	toolName    string
	toolID      string
	// accumulated text for output_item.done content field (also reasoning summary text)
	textBuf strings.Builder
	// accumulated signature for a reasoning block (delivered once, non-incremental)
	reasoningSig string
}

func (a *ResponsesAdapter) WriteStream(ctx context.Context, w http.ResponseWriter, model string, src <-chan ir.StreamEvent) error {
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

// translateEvent converts one neutral IR stream event directly into
// Responses-API SSE — no Anthropic-shaped intermediate (see
// docs/dev/DESIGNLOG.md, 2026-07-19).
func (a *ResponsesAdapter) translateEvent(
	ev ir.StreamEvent,
	send func(string, any) error,
	cur **responseBlockState,
	outputIndex *int,
	msgItemID string,
	inputTokens, outputTokens *int,
	respID string,
) error {
	switch ev.Kind {
	case ir.EvStreamStart:
		// response.created already emitted before the loop; nothing IR-side
		// carries usage at stream start (see EvUsage, emitted near the end).

	case ir.EvContentBlockStart:
		s := ev.ContentBlockStart
		itemID := "item_" + idgen.NewMsgID()
		*cur = &responseBlockState{outputIndex: *outputIndex, itemID: itemID, blockType: s.BlockType}
		item := map[string]any{"id": msgItemID, "type": "message", "role": "assistant"}
		if s.BlockType == "reasoning" {
			item = map[string]any{"id": itemID, "type": "reasoning", "summary": []map[string]any{}}
		}
		return send("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": *outputIndex,
			"item":         item,
		})

	case ir.EvReasoningDelta:
		if *cur == nil {
			return nil
		}
		d := ev.ReasoningDelta
		if d.Text != "" {
			(*cur).textBuf.WriteString(d.Text)
			return send("response.reasoning_summary_text.delta", map[string]any{
				"type":         "response.reasoning_summary_text.delta",
				"item_id":      (*cur).itemID,
				"output_index": (*cur).outputIndex,
				"delta":        d.Text,
			})
		}
		if d.Signature != "" {
			(*cur).reasoningSig = d.Signature
		}
		return nil

	case ir.EvToolCallStart:
		s := ev.ToolCallStart
		*cur = &responseBlockState{outputIndex: *outputIndex, blockType: "tool_use", toolName: s.Name, toolID: s.ID}
		return send("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": *outputIndex,
			"item": map[string]any{
				"id":        s.ID,
				"type":      "function_call",
				"name":      s.Name,
				"arguments": "",
				"call_id":   s.ID,
			},
		})

	case ir.EvTextDelta:
		if *cur == nil || ev.TextDelta.Text == "" {
			return nil
		}
		(*cur).textBuf.WriteString(ev.TextDelta.Text)
		return send("response.output_text.delta", map[string]any{
			"type":          "response.output_text.delta",
			"item_id":       msgItemID,
			"output_index":  (*cur).outputIndex,
			"content_index": 0,
			"delta":         ev.TextDelta.Text,
		})

	case ir.EvToolCallDelta:
		if *cur == nil || ev.ToolCallDelta.PartialJSON == "" {
			return nil
		}
		return send("response.function_call_arguments.delta", map[string]any{
			"type":         "response.function_call_arguments.delta",
			"item_id":      (*cur).toolID,
			"output_index": (*cur).outputIndex,
			"delta":        ev.ToolCallDelta.PartialJSON,
		})

	case ir.EvContentBlockEnd:
		if *cur == nil {
			return nil
		}
		var doneItem map[string]any
		switch (*cur).blockType {
		case "text":
			doneItem = map[string]any{
				"id":   msgItemID,
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{
					{"type": "output_text", "text": (*cur).textBuf.String()},
				},
			}
		case "reasoning":
			doneItem = reasoningOutputItem(&ir.IRReasoningPart{Text: (*cur).textBuf.String(), Signature: (*cur).reasoningSig})
		default:
			doneItem = map[string]any{
				"id":      (*cur).toolID,
				"type":    "function_call",
				"name":    (*cur).toolName,
				"call_id": (*cur).toolID,
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

	case ir.EvUsage:
		*inputTokens = ev.Usage.InputTokens
		*outputTokens = ev.Usage.OutputTokens

	case ir.EvStreamEnd:
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

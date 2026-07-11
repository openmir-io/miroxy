package irc

import (
	"context"
	"encoding/json"
	"fmt"

	"miroxy/core/ir"
	"miroxy/internal/types"
)

// AnthropicConverter is the v1 DownstreamConverter: it converts between the
// Anthropic Messages wire format and the neutral IR. This is the single place
// where Anthropic format ambiguities are resolved (string vs block content,
// system as string vs block array, cache_control stripping). It carries no
// provider knowledge — tool-result function-name resolution lives in the provider
// converter (gemini.go), not here.
type AnthropicConverter struct{}

var _ DownstreamConverter = AnthropicConverter{}

// RequestToIR converts an Anthropic MessageRequest into the canonical IR form.
func (AnthropicConverter) RequestToIR(req *types.MessageRequest) (*ir.IRRequest, error) {
	irReq := &ir.IRRequest{
		System: req.SystemText(),
		Stream: req.Stream,
		Gen: ir.IRGenerationConfig{
			Temperature: req.Temperature,
			TopP:        req.TopP,
			TopK:        req.TopK,
			MaxTokens:   req.MaxTokens,
			StopSeqs:    req.StopSequences,
		},
	}

	msgs := make([]ir.IRMessage, 0, len(req.Messages))
	for i, msg := range req.Messages {
		irMsg, err := convertMessage(msg)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		msgs = append(msgs, irMsg)
	}
	irReq.Messages = msgs

	if len(req.Tools) > 0 {
		tools := make([]ir.IRTool, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, ir.IRTool{
				Name:            t.Name,
				Description:     t.Description,
				InputSchemaJSON: []byte(t.InputSchema),
			})
		}
		irReq.Tools = tools
	}

	if req.ToolChoice != nil {
		irReq.ToolChoice = &ir.IRToolChoice{
			Type: req.ToolChoice.Type,
			Name: req.ToolChoice.Name,
		}
	}

	return irReq, nil
}

// convertMessage maps one Anthropic message to an IR message, resolving the
// string-vs-block content ambiguity and dropping cache_control.
func convertMessage(msg types.Message) (ir.IRMessage, error) {
	irMsg := ir.IRMessage{Role: msg.Role}

	if text, ok := msg.TextContent(); ok {
		irMsg.Parts = []ir.IRContentPart{{Text: &ir.IRTextPart{Text: text}}}
		return irMsg, nil
	}

	blocks, ok := msg.BlockContent()
	if !ok {
		return irMsg, fmt.Errorf("content is neither a string nor a content-block array")
	}

	parts := make([]ir.IRContentPart, 0, len(blocks))
	for _, b := range blocks {
		// cache_control is stripped by omission — not mapped to any IR field.
		switch b.Type {
		case "text":
			parts = append(parts, ir.IRContentPart{Text: &ir.IRTextPart{Text: b.Text}})

		case "tool_use":
			inputJSON := []byte(b.Input)
			if len(inputJSON) == 0 || string(inputJSON) == "null" {
				inputJSON = []byte("{}")
			}
			parts = append(parts, ir.IRContentPart{ToolUse: &ir.IRToolUsePart{
				ID:        b.ID,
				Name:      b.Name,
				InputJSON: inputJSON,
			}})

		case "tool_result":
			content, err := convertResultContent(b.Content)
			if err != nil {
				return irMsg, fmt.Errorf("tool_result content: %w", err)
			}
			parts = append(parts, ir.IRContentPart{ToolResult: &ir.IRToolResultPart{
				ToolUseID: b.ToolUseID,
				Content:   content,
			}})
		}
	}
	irMsg.Parts = parts
	return irMsg, nil
}

// convertResultContent converts a tool_result content field to IR parts.
// Handles three forms: empty, plain string, block-array.
func convertResultContent(raw json.RawMessage) ([]ir.IRContentPart, error) {
	if len(raw) == 0 {
		return []ir.IRContentPart{{Text: &ir.IRTextPart{Text: ""}}}, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []ir.IRContentPart{{Text: &ir.IRTextPart{Text: s}}}, nil
	}
	var blocks []types.ContentBlock
	if json.Unmarshal(raw, &blocks) == nil {
		parts := make([]ir.IRContentPart, 0, len(blocks))
		for _, b := range blocks {
			if b.Type == "text" {
				parts = append(parts, ir.IRContentPart{Text: &ir.IRTextPart{Text: b.Text}})
			}
		}
		return parts, nil
	}
	return []ir.IRContentPart{{Text: &ir.IRTextPart{Text: string(raw)}}}, nil
}

// irToAnthropicStopReason maps neutral IR stop reasons to Anthropic wire values.
// Anthropic's stop_reason set: "end_turn", "max_tokens", "stop_sequence", "tool_use".
func irToAnthropicStopReason(r ir.IRStopReason) string {
	switch r {
	case ir.IRStopReasonToolUse:
		return "tool_use"
	case ir.IRStopReasonMaxTokens:
		return "max_tokens"
	default: // stop, content_filter, error — all map to end_turn
		return "end_turn"
	}
}

// ResponseFromIR renders an IR response into the Anthropic MessageResponse wire
// format. msgID and model are supplied by the caller (the IR carries neither).
func (AnthropicConverter) ResponseFromIR(irResp *ir.IRResponse, msgID, model string) *types.MessageResponse {
	content := make([]types.ContentBlock, 0, len(irResp.Content))
	for _, block := range irResp.Content {
		if block.Text != nil {
			content = append(content, types.ContentBlock{Type: "text", Text: block.Text.Text})
		}
		if block.ToolUse != nil {
			content = append(content, types.ContentBlock{
				Type:  "tool_use",
				ID:    block.ToolUse.ID,
				Name:  block.ToolUse.Name,
				Input: json.RawMessage(block.ToolUse.InputJSON),
			})
		}
	}
	return &types.MessageResponse{
		ID:           msgID,
		Type:         "message",
		Role:         "assistant",
		Content:      content,
		Model:        model,
		StopReason:   irToAnthropicStopReason(irResp.StopReason),
		StopSequence: irResp.StopSeq,
		Usage: types.Usage{
			InputTokens:  irResp.Usage.InputTokens,
			OutputTokens: irResp.Usage.OutputTokens,
		},
	}
}

// StreamFromIR maps neutral IR stream events to the Anthropic 7-event SSE
// sequence: message_start, ping, content_block_start, content_block_delta(×N),
// content_block_stop, message_delta, message_stop. Finish + Usage are buffered
// and folded into the single message_delta. Synchronous: returns when in is
// closed (normal end or ctx cancellation). Does not close out.
func (AnthropicConverter) StreamFromIR(ctx context.Context, in <-chan ir.StreamEvent, out chan<- types.SSEEvent, msgID, model string) {
	send := func(event string, data any) bool {
		select {
		case <-ctx.Done():
			return false
		case out <- types.SSEEvent{Event: event, Data: data}:
			return true
		}
	}

	var stopReason string
	var usage ir.UsageEvent

	for ev := range in {
		switch ev.Kind {
		case ir.EvStreamStart:
			if !send("message_start", types.MessageStartData{
				Type: "message_start",
				Message: types.StartMessage{
					ID:      msgID,
					Type:    "message",
					Role:    "assistant",
					Content: []types.ContentBlock{},
					Model:   model,
					Usage:   types.Usage{InputTokens: 0, OutputTokens: 1},
				},
			}) {
				return
			}
			if !send("ping", types.PingData{Type: "ping"}) {
				return
			}

		case ir.EvContentBlockStart:
			if !send("content_block_start", types.ContentBlockStartData{
				Type:         "content_block_start",
				Index:        ev.ContentBlockStart.Index,
				ContentBlock: types.ContentBlock{Type: ev.ContentBlockStart.BlockType, Text: ""},
			}) {
				return
			}

		case ir.EvTextDelta:
			if !send("content_block_delta", types.ContentBlockDeltaData{
				Type:  "content_block_delta",
				Index: ev.TextDelta.Index,
				Delta: types.TextDelta{Type: "text_delta", Text: ev.TextDelta.Text},
			}) {
				return
			}

		case ir.EvToolCallStart:
			if !send("content_block_start", types.ContentBlockStartData{
				Type:  "content_block_start",
				Index: ev.ToolCallStart.Index,
				ContentBlock: types.ContentBlock{
					Type: "tool_use",
					ID:   ev.ToolCallStart.ID,
					Name: ev.ToolCallStart.Name,
				},
			}) {
				return
			}

		case ir.EvToolCallDelta:
			if !send("content_block_delta", types.ContentBlockDeltaData{
				Type:  "content_block_delta",
				Index: ev.ToolCallDelta.Index,
				Delta: types.TextDelta{Type: "input_json_delta", PartialJSON: ev.ToolCallDelta.PartialJSON},
			}) {
				return
			}

		case ir.EvContentBlockEnd:
			if !send("content_block_stop", types.ContentBlockStopData{
				Type:  "content_block_stop",
				Index: ev.ContentBlockEnd.Index,
			}) {
				return
			}

		case ir.EvFinish:
			stopReason = irToAnthropicStopReason(ev.Finish.StopReason)

		case ir.EvUsage:
			usage = *ev.Usage

		case ir.EvStreamEnd:
			// G-07: emit real input_tokens from the buffered usage event.
			if !send("message_delta", types.MessageDeltaData{
				Type:  "message_delta",
				Delta: types.MessageDelta{StopReason: stopReason},
				Usage: types.DeltaUsage{
					OutputTokens: usage.OutputTokens,
					InputTokens:  usage.InputTokens,
				},
			}) {
				return
			}
			send("message_stop", types.MessageStopData{Type: "message_stop"})
			return
		}
	}
}

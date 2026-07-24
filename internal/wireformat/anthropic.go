package wireformat

import (
	"cmp"
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
	if req.Metadata != nil {
		irReq.UserID = req.Metadata.UserID
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

// RequestFromIR converts the canonical IR back into an Anthropic
// MessageRequest — the reverse of RequestToIR. model is supplied by the
// caller (the IR carries no model field).
func (AnthropicConverter) RequestFromIR(irReq *ir.IRRequest, model string) *types.MessageRequest {
	req := &types.MessageRequest{
		Model:         model,
		MaxTokens:     irReq.Gen.MaxTokens,
		Stream:        irReq.Stream,
		Temperature:   irReq.Gen.Temperature,
		TopP:          irReq.Gen.TopP,
		TopK:          irReq.Gen.TopK,
		StopSequences: irReq.Gen.StopSeqs,
	}
	if irReq.System != "" {
		b, _ := json.Marshal(irReq.System)
		req.System = b
	}
	if irReq.UserID != "" {
		req.Metadata = &types.RequestMetadata{UserID: irReq.UserID}
	}

	req.Messages = make([]types.Message, 0, len(irReq.Messages))
	for _, m := range irReq.Messages {
		req.Messages = append(req.Messages, messageFromIR(m))
	}

	if len(irReq.Tools) > 0 {
		tools := make([]types.Tool, 0, len(irReq.Tools))
		for _, t := range irReq.Tools {
			tools = append(tools, types.Tool{
				Name: t.Name, Description: t.Description, InputSchema: json.RawMessage(t.InputSchemaJSON),
			})
		}
		req.Tools = tools
	}
	if irReq.ToolChoice != nil {
		req.ToolChoice = &types.ToolChoice{Type: irReq.ToolChoice.Type, Name: irReq.ToolChoice.Name}
	}
	return req
}

// messageFromIR converts one IR message to Anthropic wire shape — the
// reverse of convertMessage.
func messageFromIR(m ir.IRMessage) types.Message {
	blocks := make([]types.ContentBlock, 0, len(m.Parts))
	for _, p := range m.Parts {
		switch {
		case p.Text != nil:
			blocks = append(blocks, types.ContentBlock{Type: "text", Text: p.Text.Text})
		case p.ToolUse != nil:
			blocks = append(blocks, types.ContentBlock{
				Type: "tool_use", ID: p.ToolUse.ID, Name: p.ToolUse.Name, Input: json.RawMessage(p.ToolUse.InputJSON),
			})
		case p.ToolResult != nil:
			blocks = append(blocks, types.ContentBlock{
				Type: "tool_result", ToolUseID: p.ToolResult.ToolUseID,
				Content: resultContentFromIR(p.ToolResult.Content), IsError: p.ToolResult.IsError,
			})
		case p.Image != nil:
			blocks = append(blocks, types.ContentBlock{Type: "image", Source: imageSourceFromIR(p.Image)})
		case p.Reasoning != nil:
			blocks = append(blocks, reasoningBlockFromIR(p.Reasoning))
		}
	}
	b, _ := json.Marshal(blocks)
	return types.Message{Role: m.Role, Content: b}
}

// reasoningBlockFromIR renders an IR reasoning part back to its Anthropic
// wire shape — "thinking" when visible text survived, "redacted_thinking"
// when only the opaque signature/envelope is present.
func reasoningBlockFromIR(r *ir.IRReasoningPart) types.ContentBlock {
	if r.Text != "" {
		return types.ContentBlock{Type: "thinking", Thinking: r.Text, Signature: r.Signature}
	}
	return types.ContentBlock{Type: "redacted_thinking", Data: r.Signature}
}

// resultContentFromIR converts a tool_result's IR content parts back into an
// Anthropic content-block array — the reverse of convertResultContent.
func resultContentFromIR(parts []ir.IRContentPart) json.RawMessage {
	blocks := make([]types.ContentBlock, 0, len(parts))
	for _, p := range parts {
		switch {
		case p.Text != nil:
			blocks = append(blocks, types.ContentBlock{Type: "text", Text: p.Text.Text})
		case p.Image != nil:
			blocks = append(blocks, types.ContentBlock{Type: "image", Source: imageSourceFromIR(p.Image)})
		}
	}
	b, _ := json.Marshal(blocks)
	return b
}

// imageSourceFromIR converts an IR image part back to Anthropic's image
// source shape — the reverse of convertImageSource.
func imageSourceFromIR(img *ir.IRImagePart) *types.ImageSource {
	if img == nil {
		return nil
	}
	src := &types.ImageSource{Type: img.SourceType, MediaType: img.MediaType}
	if img.SourceType == "url" {
		src.URL = img.Data
	} else {
		src.Data = img.Data
	}
	return src
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
				IsError:   b.IsError,
			}})

		case "image":
			parts = append(parts, ir.IRContentPart{Image: convertImageSource(b.Source)})

		case "thinking":
			parts = append(parts, ir.IRContentPart{Reasoning: &ir.IRReasoningPart{Text: b.Thinking, Signature: b.Signature}})

		case "redacted_thinking":
			parts = append(parts, ir.IRContentPart{Reasoning: &ir.IRReasoningPart{Signature: b.Data}})
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
			switch b.Type {
			case "text":
				parts = append(parts, ir.IRContentPart{Text: &ir.IRTextPart{Text: b.Text}})
			case "image":
				parts = append(parts, ir.IRContentPart{Image: convertImageSource(b.Source)})
			}
		}
		return parts, nil
	}
	return []ir.IRContentPart{{Text: &ir.IRTextPart{Text: string(raw)}}}, nil
}

// convertImageSource maps an Anthropic image block's source to IR. Returns
// nil (caller appends a part with no active field) when src is nil.
func convertImageSource(src *types.ImageSource) *ir.IRImagePart {
	if src == nil {
		return nil
	}
	return &ir.IRImagePart{
		SourceType: src.Type,
		MediaType:  src.MediaType,
		Data:       cmp.Or(src.Data, src.URL),
	}
}

// AnthropicToIRStopReason maps an Anthropic wire stop_reason to the
// provider-neutral IR vocabulary — the reverse of irToAnthropicStopReason.
func AnthropicToIRStopReason(r string) ir.IRStopReason {
	switch r {
	case "tool_use":
		return ir.IRStopReasonToolUse
	case "max_tokens":
		return ir.IRStopReasonMaxTokens
	default: // end_turn, stop_sequence
		return ir.IRStopReasonStop
	}
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
		if block.Reasoning != nil {
			content = append(content, reasoningBlockFromIR(block.Reasoning))
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
			InputTokens:              irResp.Usage.InputTokens,
			CacheCreationInputTokens: irResp.Usage.CacheCreationInputTokens,
			CacheReadInputTokens:     irResp.Usage.CacheReadInputTokens,
			OutputTokens:             irResp.Usage.OutputTokens,
		},
	}
}

// ResponseToIR converts a genuine Anthropic-wire MessageResponse (from a real
// Anthropic upstream) into the canonical IR — the reverse of ResponseFromIR.
func (AnthropicConverter) ResponseToIR(resp *types.MessageResponse) *ir.IRResponse {
	content := make([]ir.IRResponseBlock, 0, len(resp.Content))
	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			content = append(content, ir.IRResponseBlock{Text: &ir.IRTextPart{Text: b.Text}})
		case "tool_use":
			inputJSON := []byte(b.Input)
			if len(inputJSON) == 0 || string(inputJSON) == "null" {
				inputJSON = []byte("{}")
			}
			content = append(content, ir.IRResponseBlock{ToolUse: &ir.IRToolUsePart{
				ID: b.ID, Name: b.Name, InputJSON: inputJSON,
			}})
		case "thinking":
			content = append(content, ir.IRResponseBlock{Reasoning: &ir.IRReasoningPart{Text: b.Thinking, Signature: b.Signature}})
		case "redacted_thinking":
			content = append(content, ir.IRResponseBlock{Reasoning: &ir.IRReasoningPart{Signature: b.Data}})
		}
	}
	return &ir.IRResponse{
		Content:    content,
		StopReason: AnthropicToIRStopReason(resp.StopReason),
		StopSeq:    resp.StopSequence,
		Usage: ir.IRUsage{
			InputTokens:              resp.Usage.InputTokens,
			OutputTokens:             resp.Usage.OutputTokens,
			CacheCreationInputTokens: resp.Usage.CacheCreationInputTokens,
			CacheReadInputTokens:     resp.Usage.CacheReadInputTokens,
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
			// "reasoning" is the neutral IR block-type name; Anthropic's own
			// wire term for it is "thinking".
			blockType := ev.ContentBlockStart.BlockType
			if blockType == "reasoning" {
				blockType = "thinking"
			}
			if !send("content_block_start", types.ContentBlockStartData{
				Type:         "content_block_start",
				Index:        ev.ContentBlockStart.Index,
				ContentBlock: types.ContentBlock{Type: blockType, Text: ""},
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

		case ir.EvReasoningDelta:
			delta := types.TextDelta{Type: "thinking_delta", Thinking: ev.ReasoningDelta.Text}
			if ev.ReasoningDelta.Signature != "" {
				delta = types.TextDelta{Type: "signature_delta", Signature: ev.ReasoningDelta.Signature}
			}
			if !send("content_block_delta", types.ContentBlockDeltaData{
				Type:  "content_block_delta",
				Index: ev.ReasoningDelta.Index,
				Delta: delta,
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
					OutputTokens:             usage.OutputTokens,
					InputTokens:              usage.InputTokens,
					CacheCreationInputTokens: usage.CacheCreationInputTokens,
					CacheReadInputTokens:     usage.CacheReadInputTokens,
				},
			}) {
				return
			}
			send("message_stop", types.MessageStopData{Type: "message_stop"})
			return
		}
	}
}

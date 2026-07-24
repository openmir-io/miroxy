// CompressPlugin integrates BuiltinCompressor into the miroxy pipeline.
// It runs before the upstream executor and converts ir.IRMessage ↔ compress.Message
// at the boundary.
package compress

import (
	"encoding/json"
	"log/slog"

	ccomp "miroxy/core/compress"
	"miroxy/core/ir"
	"miroxy/internal/pipeline"
)

const priorityCompress = pipeline.PriorityWarden + 50 // 350 — after warden, before rectifier

// CompressPlugin compresses the inbound message list to fit within the
// configured token budget before forwarding to the upstream executor.
type CompressPlugin struct {
	comp      ccomp.Compressor
	threshold int // only compress when total tokens exceed this (default 4_000)
}

// NewCompressPlugin creates a CompressPlugin wrapping the given Compressor.
// threshold is the minimum total token count that triggers compression;
// use 0 to always compress.
func NewCompressPlugin(comp ccomp.Compressor, threshold int) *CompressPlugin {
	if threshold <= 0 {
		threshold = 4_000
	}
	return &CompressPlugin{comp: comp, threshold: threshold}
}

func (p *CompressPlugin) Name() string  { return "compress" }
func (p *CompressPlugin) Priority() int { return priorityCompress }

// Execute runs compression if the message list is large enough to warrant it.
func (p *CompressPlugin) Execute(c *pipeline.LLMContext, next pipeline.Handler) error {
	msgs := toCompressMessages(c.Request.Messages)
	toks := totalTokens(msgs)

	if toks <= p.threshold {
		return next(c)
	}

	result, err := p.comp.Compress(c.RequestCtx, &ccomp.Request{
		Model:    c.ClientModel,
		System:   c.Request.System,
		Messages: msgs,
	})
	if err != nil {
		slog.Warn("compress: failed, continuing uncompressed", "error", err)
		return next(c)
	}

	c.Request.Messages = fromCompressMessages(result.Messages)
	c.RequestRewritten = true
	slog.Debug("compress: done",
		"original_tokens", result.OriginalTokens,
		"compressed_tokens", result.CompressedTokens,
		"strategies", result.Strategies,
	)
	return next(c)
}

// ── type conversion helpers ───────────────────────────────────────────────────

// toCompressMessages converts []ir.IRMessage → []ccomp.Message.
func toCompressMessages(in []ir.IRMessage) []ccomp.Message {
	out := make([]ccomp.Message, len(in))
	for i, m := range in {
		out[i] = ccomp.Message{Role: m.Role, Parts: toCompressParts(m.Parts)}
	}
	return out
}

func toCompressParts(parts []ir.IRContentPart) []ccomp.ContentPart {
	out := make([]ccomp.ContentPart, len(parts))
	for i, p := range parts {
		out[i] = partToCompress(p)
	}
	return out
}

func partToCompress(p ir.IRContentPart) ccomp.ContentPart {
	switch {
	case p.Text != nil:
		return ccomp.ContentPart{Type: "text", Text: p.Text.Text}

	case p.ToolUse != nil:
		return ccomp.ContentPart{
			Type:     "tool_use",
			ToolID:   p.ToolUse.ID,
			ToolName: p.ToolUse.Name,
			Raw:      p.ToolUse.InputJSON,
		}

	case p.ToolResult != nil:
		return ccomp.ContentPart{
			Type:      "tool_result",
			ToolUseID: p.ToolResult.ToolUseID,
			Text:      extractToolResultText(p.ToolResult.Content),
		}

	case p.Image != nil:
		raw, _ := json.Marshal(p.Image)
		return ccomp.ContentPart{Type: "image", Raw: raw}

	case p.Reasoning != nil:
		// Kept in Raw, never Text: alignMessages/crushToolResults only ever
		// touch a part's Text field, so this stays byte-for-byte untouched
		// through compression — rewriting reasoning text would desync it
		// from its Signature, which some providers verify cryptographically.
		raw, _ := json.Marshal(p.Reasoning)
		return ccomp.ContentPart{Type: "reasoning", Raw: raw}

	default:
		return ccomp.ContentPart{Type: "unknown"}
	}
}

// extractToolResultText flattens a tool_result's IR content parts down to
// their concatenated text — matches the compressor's text-only scope;
// non-text parts (e.g. images) are dropped, same as before this migration.
func extractToolResultText(parts []ir.IRContentPart) string {
	var sb []byte
	for _, p := range parts {
		if p.Text != nil {
			sb = append(sb, []byte(p.Text.Text)...)
		}
	}
	return string(sb)
}

// fromCompressMessages converts []ccomp.Message → []ir.IRMessage.
func fromCompressMessages(in []ccomp.Message) []ir.IRMessage {
	out := make([]ir.IRMessage, len(in))
	for i, m := range in {
		out[i] = ir.IRMessage{Role: m.Role, Parts: fromCompressParts(m.Parts)}
	}
	return out
}

func fromCompressParts(parts []ccomp.ContentPart) []ir.IRContentPart {
	out := make([]ir.IRContentPart, len(parts))
	for i, p := range parts {
		out[i] = partFromCompress(p)
	}
	return out
}

func partFromCompress(p ccomp.ContentPart) ir.IRContentPart {
	switch p.Type {
	case "text":
		return ir.IRContentPart{Text: &ir.IRTextPart{Text: p.Text}}

	case "tool_use":
		inputJSON := p.Raw
		if len(inputJSON) == 0 {
			inputJSON = []byte("{}")
		}
		return ir.IRContentPart{ToolUse: &ir.IRToolUsePart{ID: p.ToolID, Name: p.ToolName, InputJSON: inputJSON}}

	case "tool_result":
		return ir.IRContentPart{ToolResult: &ir.IRToolResultPart{
			ToolUseID: p.ToolUseID,
			Content:   []ir.IRContentPart{{Text: &ir.IRTextPart{Text: p.Text}}},
		}}

	case "image":
		var img ir.IRImagePart
		_ = json.Unmarshal(p.Raw, &img)
		return ir.IRContentPart{Image: &img}

	case "reasoning":
		var r ir.IRReasoningPart
		_ = json.Unmarshal(p.Raw, &r)
		return ir.IRContentPart{Reasoning: &r}

	default:
		return ir.IRContentPart{Text: &ir.IRTextPart{Text: p.Text}}
	}
}

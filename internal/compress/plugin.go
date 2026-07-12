// CompressPlugin integrates BuiltinCompressor into the miroxy pipeline.
// It runs before the upstream executor and converts types.Message ↔ compress.Message
// at the boundary.
package compress

import (
	"encoding/json"
	"log/slog"

	ccomp "miroxy/core/compress"
	"miroxy/internal/pipeline"
	"miroxy/internal/types"
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
		System:   c.Request.SystemText(),
		Messages: msgs,
	})
	if err != nil {
		slog.Warn("compress: failed, continuing uncompressed", "error", err)
		return next(c)
	}

	c.Request.Messages = fromCompressMessages(result.Messages)
	slog.Debug("compress: done",
		"original_tokens", result.OriginalTokens,
		"compressed_tokens", result.CompressedTokens,
		"strategies", result.Strategies,
	)
	return next(c)
}

// ── type conversion helpers ───────────────────────────────────────────────────

// toCompressMessages converts []types.Message → []ccomp.Message.
func toCompressMessages(in []types.Message) []ccomp.Message {
	out := make([]ccomp.Message, len(in))
	for i, m := range in {
		out[i] = ccomp.Message{
			Role:  m.Role,
			Parts: toCompressParts(m),
		}
	}
	return out
}

// toCompressParts unpacks a types.Message's content into ContentParts.
func toCompressParts(m types.Message) []ccomp.ContentPart {
	// Try structured content (array of ContentBlocks).
	if blocks, ok := m.BlockContent(); ok {
		parts := make([]ccomp.ContentPart, len(blocks))
		for i, b := range blocks {
			parts[i] = blockToPart(b)
		}
		return parts
	}
	// Plain string content.
	if text, ok := m.TextContent(); ok {
		return []ccomp.ContentPart{{Type: "text", Text: text}}
	}
	// Unknown — preserve verbatim.
	return []ccomp.ContentPart{{Type: "raw", Raw: m.Content}}
}

func blockToPart(b types.ContentBlock) ccomp.ContentPart {
	switch b.Type {
	case "text":
		return ccomp.ContentPart{Type: "text", Text: b.Text}

	case "tool_use":
		raw, _ := json.Marshal(b)
		return ccomp.ContentPart{
			Type:     "tool_use",
			ToolID:   b.ID,
			ToolName: b.Name,
			Raw:      raw,
		}

	case "tool_result":
		text := extractToolResultText(b.Content)
		return ccomp.ContentPart{
			Type:      "tool_result",
			ToolUseID: b.ToolUseID,
			Text:      text,
		}

	default:
		raw, _ := json.Marshal(b)
		return ccomp.ContentPart{Type: b.Type, Raw: raw}
	}
}

// extractToolResultText unwraps the content field of a tool_result block.
// It can be a plain string or an array of text blocks.
func extractToolResultText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	// Try plain string first.
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	// Try array of content blocks.
	var blocks []types.ContentBlock
	if json.Unmarshal(content, &blocks) != nil {
		return string(content)
	}
	var sb []byte
	for _, b := range blocks {
		if b.Type == "text" {
			sb = append(sb, []byte(b.Text)...)
		}
	}
	return string(sb)
}

// fromCompressMessages converts []ccomp.Message → []types.Message.
func fromCompressMessages(in []ccomp.Message) []types.Message {
	out := make([]types.Message, len(in))
	for i, m := range in {
		out[i] = types.Message{
			Role:    m.Role,
			Content: fromCompressParts(m.Parts),
		}
	}
	return out
}

func fromCompressParts(parts []ccomp.ContentPart) json.RawMessage {
	if len(parts) == 1 {
		p := parts[0]
		// Scalar text — serialise as a plain JSON string.
		if p.Type == "text" && len(p.Raw) == 0 {
			b, _ := json.Marshal(p.Text)
			return b
		}
	}
	// Otherwise build a block array.
	blocks := make([]json.RawMessage, 0, len(parts))
	for _, p := range parts {
		blocks = append(blocks, partToRaw(p))
	}
	out, _ := json.Marshal(blocks)
	return out
}

func partToRaw(p ccomp.ContentPart) json.RawMessage {
	if len(p.Raw) > 0 {
		return p.Raw
	}
	switch p.Type {
	case "text":
		b, _ := json.Marshal(types.ContentBlock{Type: "text", Text: p.Text})
		return b
	case "tool_result":
		textJSON, _ := json.Marshal(p.Text)
		b, _ := json.Marshal(types.ContentBlock{
			Type:      "tool_result",
			ToolUseID: p.ToolUseID,
			Content:   textJSON,
		})
		return b
	default:
		b, _ := json.Marshal(map[string]any{"type": p.Type, "text": p.Text})
		return b
	}
}

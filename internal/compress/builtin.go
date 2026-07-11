// BuiltinCompressor orchestrates the four compression modules in order:
//  1. CacheAligner  — stabilise dynamic fields for KV-cache efficiency
//  2. SmartCrusher  — compress oversized tool_result content
//  3. SlidingWindow — trim conversation history if still over budget
//  4. CCRStore      — store originals, inject retrieval markers
package compress

import (
	"context"
	"fmt"
	"time"

	ccomp "miroxy/core/compress"
)

// BuiltinConfig controls the BuiltinCompressor behaviour.
type BuiltinConfig struct {
	// ToolResultBudget is the per-tool-result token threshold.
	// Tool result parts exceeding this will be compressed by SmartCrusher.
	// Default 500.
	ToolResultBudget int

	// TotalBudget is the total token target for the whole message list.
	// SlidingWindow kicks in when the list exceeds this value.
	// Default 80_000.
	TotalBudget int

	// WindowRecentKeep is the number of recent turns always preserved.
	// Default 6.
	WindowRecentKeep int

	// AlignDynamic enables the CacheAligner pass (UUID/timestamp stabilisation).
	// Disabled by default because it mutates content; enable explicitly.
	AlignDynamic bool

	// CCR is the CCRStore used to persist originals and inject markers.
	// Use NewMemCCRStore() for session-scoped retrieval (default nil = disabled).
	CCR ccomp.CCRStore
}

func (c *BuiltinConfig) setDefaults() {
	if c.ToolResultBudget <= 0 {
		c.ToolResultBudget = 500
	}
	if c.TotalBudget <= 0 {
		c.TotalBudget = 80_000
	}
	if c.WindowRecentKeep <= 0 {
		c.WindowRecentKeep = 6
	}
}

// BuiltinCompressor implements core/compress.Compressor using deterministic
// Go algorithms.  Safe for concurrent use.
type BuiltinCompressor struct {
	cfg     BuiltinConfig
	aligner *CacheAligner
	crusher *SmartCrusher
	window  *Window
	stats   *ccomp.Stats
}

// NewBuiltinCompressor creates a compressor with the given config.
func NewBuiltinCompressor(cfg BuiltinConfig) *BuiltinCompressor {
	cfg.setDefaults()
	w := newWindow()
	w.RecentKeep = cfg.WindowRecentKeep
	return &BuiltinCompressor{
		cfg:     cfg,
		aligner: &CacheAligner{},
		crusher: newSmartCrusher(),
		window:  w,
		stats:   ccomp.NewStats(),
	}
}

// Stats returns the live statistics tracker for this compressor.
// Use Stats.Snapshot() to get a point-in-time view suitable for reporting.
func (b *BuiltinCompressor) Stats() *ccomp.Stats { return b.stats }

// Compress runs the full pipeline on req.Messages.
func (b *BuiltinCompressor) Compress(_ context.Context, req *ccomp.Request) (*ccomp.Result, error) {
	start := time.Now()
	msgs := req.Messages
	origTokens := totalTokens(msgs)

	budget := req.Budget
	if budget <= 0 {
		budget = b.cfg.TotalBudget
	}

	var strategies []string

	// ── Pass 1: CacheAligner ─────────────────────────────────────────────────
	if b.cfg.AlignDynamic {
		msgs = alignMessages(msgs, b.aligner)
		strategies = append(strategies, "align")
	}

	// ── Pass 2: SmartCrusher on tool_result parts ────────────────────────────
	crushed, crushStrategies, err := b.crushToolResults(msgs)
	if err != nil {
		return nil, fmt.Errorf("compress: crusher: %w", err)
	}
	if len(crushStrategies) > 0 {
		msgs = crushed
		strategies = append(strategies, crushStrategies...)
	}

	// ── Pass 3: SlidingWindow ────────────────────────────────────────────────
	if totalTokens(msgs) > budget {
		trimmed, dropped := b.window.Trim(msgs, budget)
		if dropped > 0 {
			msgs = trimmed
			strategies = append(strategies, fmt.Sprintf("window(-%d)", dropped))
		}
	}

	compTokens := totalTokens(msgs)
	result := &ccomp.Result{
		System:           req.System,
		Messages:         msgs,
		OriginalTokens:   origTokens,
		CompressedTokens: compTokens,
		Strategies:       strategies,
	}
	b.stats.Record(result, time.Since(start).Microseconds())
	return result, nil
}

// crushToolResults runs SmartCrusher on every tool_result part that exceeds
// the configured token threshold.  Returns the modified message slice and a
// list of strategy labels applied.
func (b *BuiltinCompressor) crushToolResults(msgs []ccomp.Message) ([]ccomp.Message, []string, error) {
	var strategies []string
	out := make([]ccomp.Message, len(msgs))
	copy(out, msgs)

	for i, m := range out {
		newParts := make([]ccomp.ContentPart, len(m.Parts))
		copy(newParts, m.Parts)
		for j, p := range newParts {
			if p.Type != "tool_result" || p.Text == "" {
				continue
			}
			if estimateTokens(p.Text) <= b.cfg.ToolResultBudget {
				continue
			}
			if !b.crusher.CanHandle([]byte(p.Text)) {
				continue
			}
			result, err := b.crusher.Crush([]byte(p.Text), b.cfg.ToolResultBudget)
			if err != nil {
				continue
			}
			compressed, err := storeAndMark(b.cfg.CCR, []byte(p.Text), result.Data, result.Omitted)
			if err != nil {
				compressed = result.Data
			}
			newParts[j].Text = string(compressed)
			strategies = append(strategies, fmt.Sprintf("crush(tool_result,%d→%d)",
				estimateTokens(p.Text), estimateTokens(string(compressed))))
		}
		out[i].Parts = newParts
	}

	return out, strategies, nil
}

// alignMessages runs CacheAligner on all text/tool_result parts.
func alignMessages(msgs []ccomp.Message, a *CacheAligner) []ccomp.Message {
	out := make([]ccomp.Message, len(msgs))
	for i, m := range msgs {
		newParts := make([]ccomp.ContentPart, len(m.Parts))
		copy(newParts, m.Parts)
		for j, p := range newParts {
			if p.Text == "" {
				continue
			}
			r := a.Align(p.Text)
			newParts[j].Text = r.Content
		}
		out[i] = m
		out[i].Parts = newParts
	}
	return out
}

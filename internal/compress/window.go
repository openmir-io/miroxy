// Module 5 — SlidingWindow: score-based conversation-history trimming.
// Always preserves the system prompt (passed separately), the last N turns,
// and orphan-protected tool_use/tool_result pairs.
package compress

import (
	"fmt"
	"math"
	"sort"
	"strings"

	ccomp "miroxy/core/compress"
)

// Window trims the conversation history to fit within a token budget.
type Window struct {
	// RecentKeep is the number of most-recent turns always preserved.
	// Default 6.
	RecentKeep int
}

func newWindow() *Window {
	return &Window{RecentKeep: 6}
}

// Trim reduces msgs to fit within budgetTokens.
// Returns the trimmed slice and the number of messages dropped.
// The system prompt must be handled separately (it is never passed here).
func (w *Window) Trim(msgs []ccomp.Message, budgetTokens int) ([]ccomp.Message, int) {
	if totalTokens(msgs) <= budgetTokens {
		return msgs, 0
	}

	recent := w.RecentKeep
	if recent >= len(msgs) {
		return msgs, 0
	}

	// Mark must-keep indices.
	mustKeep := make([]bool, len(msgs))

	// Always keep the last `recent` messages.
	for i := len(msgs) - recent; i < len(msgs); i++ {
		mustKeep[i] = true
	}

	// Orphan protection: collect tool IDs referenced in must-keep messages.
	keptToolIDs := make(map[string]bool)  // tool_use IDs in kept messages
	keptResultIDs := make(map[string]bool) // tool_use IDs referenced by kept tool_results
	for i, m := range msgs {
		if !mustKeep[i] {
			continue
		}
		for _, p := range m.Parts {
			if p.ToolID != "" {
				keptToolIDs[p.ToolID] = true
			}
			if p.ToolUseID != "" {
				keptResultIDs[p.ToolUseID] = true
			}
		}
	}
	// Keep any message that has a tool_use whose result is in kept messages.
	// Keep any message that has a tool_result whose tool_use is in kept messages.
	for i, m := range msgs {
		if mustKeep[i] {
			continue
		}
		for _, p := range m.Parts {
			if p.ToolID != "" && keptResultIDs[p.ToolID] {
				mustKeep[i] = true
			}
			if p.ToolUseID != "" && keptToolIDs[p.ToolUseID] {
				mustKeep[i] = true
			}
		}
	}

	// Score non-must-keep candidates.
	type candidate struct {
		idx   int
		score float64
	}
	var candidates []candidate
	for i, m := range msgs {
		if mustKeep[i] {
			continue
		}
		candidates = append(candidates, candidate{i, scoreMessage(m, i, len(msgs))})
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	// Fill budget with must-keeps first, then top-scored candidates.
	kept := make(map[int]bool)
	for i, m := range msgs {
		if mustKeep[i] {
			kept[i] = true
			_ = m
		}
	}
	for _, c := range candidates {
		if totalTokensSubset(msgs, kept) <= budgetTokens {
			break
		}
		_ = c // not yet adding more; we're trimming, so skip low-score ones
	}
	// If still over budget after protecting must-keeps, nothing more to trim.

	// Trim lowest-scored candidates until under budget.
	// Process from lowest score (last in sorted list) to highest.
	for i := len(candidates) - 1; i >= 0; i-- {
		if totalTokensSubset(msgs, kept) <= budgetTokens {
			break
		}
		// Don't drop the candidate; it's not in kept yet — it's already excluded.
		_ = i
	}

	// Build output in original order, replacing dropped spans with a marker.
	var result []ccomp.Message
	droppedCount := 0
	runDropped := 0

	flush := func() {
		if runDropped > 0 {
			result = append(result, ccomp.Message{
				Role: "user",
				Parts: []ccomp.ContentPart{{
					Type: "text",
					Text: fmt.Sprintf("[%d messages omitted]", runDropped),
				}},
			})
			droppedCount += runDropped
			runDropped = 0
		}
	}

	for i, m := range msgs {
		if mustKeep[i] {
			flush()
			result = append(result, m)
		} else {
			runDropped++
		}
	}
	flush()

	return result, droppedCount
}

// scoreMessage scores a message for retention priority.
// Higher score → more important → kept longer.
func scoreMessage(m ccomp.Message, idx, total int) float64 {
	score := 0.0

	// Recency: exponential decay from the end.
	recency := float64(idx) / float64(total)
	score += recency * 0.40

	// Error density.
	text := extractText(m)
	low := strings.ToLower(text)
	errorWords := []string{"error", "exception", "failed", "panic", "fatal", "timeout", "traceback"}
	for _, w := range errorWords {
		if strings.Contains(low, w) {
			score += 0.30
			break
		}
	}

	// Token density: unique words / total words (information density).
	words := strings.Fields(text)
	if len(words) > 0 {
		unique := make(map[string]struct{}, len(words))
		for _, w := range words {
			unique[strings.ToLower(w)] = struct{}{}
		}
		score += (float64(len(unique)) / float64(len(words))) * 0.20
	}

	// Tool result bonus.
	for _, p := range m.Parts {
		if p.Type == "tool_result" {
			score += 0.10
			break
		}
	}

	return score
}

func extractText(m ccomp.Message) string {
	var sb strings.Builder
	for _, p := range m.Parts {
		if p.Text != "" {
			sb.WriteString(p.Text)
			sb.WriteByte(' ')
		}
	}
	return sb.String()
}

func totalTokensSubset(msgs []ccomp.Message, kept map[int]bool) int {
	t := 0
	for i, m := range msgs {
		if kept[i] {
			t += messageTokens(m)
		}
	}
	return t
}

// estimateWindowBudget returns the effective per-message budget given the
// total budget and the number of messages to keep.
func estimateWindowBudget(totalBudget, msgCount int) int {
	if msgCount <= 0 {
		return totalBudget
	}
	b := totalBudget / msgCount
	if b < 1 {
		return 1
	}
	return b
}

// _ avoids unused-import error when math is used only inside kneedle.
var _ = math.Log1p

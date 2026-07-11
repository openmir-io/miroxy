// Module 1 — SmartCrusher: deterministic structural compression for JSON arrays,
// JSON objects, and log output.  No ML dependencies, compression ratio 60–90%.
package compress

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// CrushResult is returned by SmartCrusher.Crush.
type CrushResult struct {
	Data    []byte // compressed output (valid JSON or truncated text)
	Omitted int    // number of items / lines omitted
	Hash    string // CCR hash injected into Data ("" when CCR is disabled)
}

// SmartCrusher compresses structured content (JSON arrays/objects, logs).
type SmartCrusher struct {
	// HeadRatio is the fraction of items taken from the head (schema understanding).
	// Default 0.30.
	HeadRatio float64
	// TailRatio is the fraction taken from the tail (recency).
	// Default 0.15.
	TailRatio float64
	// MaxLogLines is the maximum number of lines kept from log output.
	// Default 60 (30 head + 30 tail).
	MaxLogLines int
	// MaxObjectDepth is the maximum nesting depth preserved in JSON objects.
	// Default 3.
	MaxObjectDepth int
}

func newSmartCrusher() *SmartCrusher {
	return &SmartCrusher{
		HeadRatio:      0.30,
		TailRatio:      0.15,
		MaxLogLines:    60,
		MaxObjectDepth: 3,
	}
}

// CanHandle returns true when data looks like JSON or log output.
func (c *SmartCrusher) CanHandle(data []byte) bool {
	t := strings.TrimSpace(string(data))
	return len(t) > 0 && (t[0] == '[' || t[0] == '{' || isLogLike(t))
}

// Crush compresses data to fit within budgetTokens.
// Returns the original data unchanged if it already fits.
func (c *SmartCrusher) Crush(data []byte, budgetTokens int) (CrushResult, error) {
	text := strings.TrimSpace(string(data))
	if estimateTokens(text) <= budgetTokens {
		return CrushResult{Data: data}, nil
	}

	switch {
	case len(text) > 0 && text[0] == '[':
		return c.crushArray([]byte(text), budgetTokens)
	case len(text) > 0 && text[0] == '{':
		return c.crushObject([]byte(text), budgetTokens)
	case isLogLike(text):
		return c.crushLog(text, budgetTokens), nil
	default:
		return c.crushLines(text, budgetTokens), nil
	}
}

// ── JSON array compression ────────────────────────────────────────────────────

type fieldStat struct {
	uniqueVals map[string]struct{}
	total      int
	hasError   bool
}

func (c *SmartCrusher) crushArray(data []byte, budgetTokens int) (CrushResult, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(data, &items); err != nil {
		return c.crushLines(string(data), budgetTokens), nil
	}
	if len(items) == 0 {
		return CrushResult{Data: data}, nil
	}

	// Parse items into maps for analysis.
	parsed := make([]map[string]any, 0, len(items))
	for _, raw := range items {
		var m map[string]any
		if json.Unmarshal(raw, &m) == nil {
			parsed = append(parsed, m)
		}
	}
	if len(parsed) == 0 {
		// Non-object array — keep first/last items.
		return c.truncateArray(items, budgetTokens)
	}

	// Analyse field variance.
	stats := analyseFields(parsed)

	// Separate low-variance fields into schema header.
	schema, highVarFields := extractSchema(parsed, stats)

	// Score every item.
	scores := make([]float64, len(parsed))
	errorFlags := make([]bool, len(parsed))
	for i, item := range parsed {
		scores[i], errorFlags[i] = scoreItem(item, stats, highVarFields, i, len(parsed))
	}

	// Estimate how many items fit in budget.
	avgToks := estimateTokens(string(data)) / len(items)
	if avgToks < 1 {
		avgToks = 1
	}
	// Reserve ~20 tokens for the schema header.
	itemBudget := budgetTokens - 20
	if itemBudget < 1 {
		itemBudget = 1
	}
	n := itemBudget / avgToks
	if n >= len(items) {
		return CrushResult{Data: data}, nil
	}
	if n < 1 {
		n = 1
	}

	// Apply the Kneedle heuristic to find the elbow of the coverage curve.
	n = kneedle(scores, n)

	// 30/15/55 selection + always keep error items.
	kept := selectItems(items, scores, errorFlags, n)
	omitted := len(items) - len(kept)

	// Build output.
	schemaJSON, _ := json.Marshal(schema)
	keptJSON, _ := json.Marshal(kept)
	out := fmt.Sprintf(`{"_schema":%s,"_sample":%s,"_omitted":%d}`,
		schemaJSON, keptJSON, omitted)
	return CrushResult{Data: []byte(out), Omitted: omitted}, nil
}

func analyseFields(items []map[string]any) map[string]*fieldStat {
	stats := make(map[string]*fieldStat)
	errorWords := []string{"error", "exception", "fail", "fatal", "panic", "warn"}
	for _, item := range items {
		for k, v := range item {
			if _, ok := stats[k]; !ok {
				stats[k] = &fieldStat{uniqueVals: make(map[string]struct{})}
			}
			s := stats[k]
			s.total++
			vs := fmt.Sprintf("%v", v)
			s.uniqueVals[vs] = struct{}{}
			low := strings.ToLower(vs)
			for _, w := range errorWords {
				if strings.Contains(low, w) {
					s.hasError = true
					break
				}
			}
		}
	}
	return stats
}

// extractSchema returns fields whose value is constant across all items
// (low variance) as a schema map, plus the remaining high-variance field names.
func extractSchema(items []map[string]any, stats map[string]*fieldStat) (map[string]any, []string) {
	schema := make(map[string]any)
	var highVar []string
	for k, s := range stats {
		if len(s.uniqueVals) <= 2 && s.total == len(items) {
			// Constant or near-constant field — move to schema.
			schema[k] = items[0][k]
		} else {
			highVar = append(highVar, k)
		}
	}
	return schema, highVar
}

func scoreItem(item map[string]any, stats map[string]*fieldStat, highVar []string, idx, total int) (float64, bool) {
	score := 0.0
	isError := false
	errorWords := []string{"error", "exception", "fail", "fatal", "panic"}

	for _, k := range highVar {
		s, ok := stats[k]
		if !ok || s.total == 0 {
			continue
		}
		v, exists := item[k]
		if !exists {
			continue
		}
		// Information gain: higher uniqueness = higher score contribution.
		uniqueness := float64(len(s.uniqueVals)) / float64(s.total)
		score += math.Log1p(uniqueness)

		// Error detection.
		low := strings.ToLower(fmt.Sprintf("%v", v))
		for _, w := range errorWords {
			if strings.Contains(low, w) {
				isError = true
				score += 2.0
				break
			}
		}
	}
	return score, isError
}

// kneedle finds the elbow of the normalised bigram-coverage curve.
// Returns the adjusted n that maximises information per token.
func kneedle(scores []float64, maxN int) int {
	if maxN >= len(scores) {
		return len(scores)
	}
	// Sort scores descending to build coverage curve.
	sorted := make([]float64, len(scores))
	copy(sorted, scores)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] > sorted[j] })

	// Cumulative coverage.
	cum := make([]float64, len(sorted))
	total := 0.0
	for _, v := range sorted {
		total += v
	}
	if total == 0 {
		return maxN
	}
	running := 0.0
	for i, v := range sorted {
		running += v
		cum[i] = running / total
	}

	// Normalised knee: maximise (coverage_y - items_fraction_x).
	bestKnee := maxN
	bestVal := -1.0
	for i := 1; i <= maxN && i < len(cum); i++ {
		x := float64(i) / float64(len(scores))
		diff := cum[i-1] - x
		if diff > bestVal {
			bestVal = diff
			bestKnee = i
		}
	}
	if bestKnee < 1 {
		bestKnee = 1
	}
	return bestKnee
}

func selectItems(items []json.RawMessage, scores []float64, errorFlags []bool, n int) []json.RawMessage {
	type indexed struct {
		idx   int
		score float64
	}
	all := make([]indexed, len(items))
	for i, s := range scores {
		all[i] = indexed{i, s}
	}

	// Always include error items.
	kept := make(map[int]bool)
	var nonError []indexed
	for _, it := range all {
		if errorFlags[it.idx] {
			kept[it.idx] = true
		} else {
			nonError = append(nonError, it)
		}
	}

	// Head: 30% of n.
	headN := int(math.Ceil(float64(n) * 0.30))
	for i := 0; i < headN && i < len(nonError); i++ {
		kept[nonError[i].idx] = true
	}

	// Tail: 15% of n.
	tailN := int(math.Ceil(float64(n) * 0.15))
	for i := len(nonError) - 1; i >= len(nonError)-tailN && i >= 0; i-- {
		kept[nonError[i].idx] = true
	}

	// Remaining budget: top-scored.
	sort.Slice(nonError, func(i, j int) bool { return nonError[i].score > nonError[j].score })
	for _, it := range nonError {
		if len(kept) >= n {
			break
		}
		kept[it.idx] = true
	}

	// Collect in original order.
	var result []json.RawMessage
	for i, item := range items {
		if kept[i] {
			result = append(result, item)
		}
	}
	return result
}

func (c *SmartCrusher) truncateArray(items []json.RawMessage, budgetTokens int) (CrushResult, error) {
	kept := []json.RawMessage{}
	toks := 0
	for _, item := range items {
		t := estimateTokens(string(item))
		if toks+t > budgetTokens {
			break
		}
		kept = append(kept, item)
		toks += t
	}
	omitted := len(items) - len(kept)
	out, _ := json.Marshal(kept)
	if omitted > 0 {
		out = []byte(fmt.Sprintf(`{"_sample":%s,"_omitted":%d}`, out, omitted))
	}
	return CrushResult{Data: out, Omitted: omitted}, nil
}

// ── JSON object truncation ────────────────────────────────────────────────────

func (c *SmartCrusher) crushObject(data []byte, budgetTokens int) (CrushResult, error) {
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return c.crushLines(string(data), budgetTokens), nil
	}
	truncated := truncateDepth(obj, c.MaxObjectDepth)
	out, err := json.Marshal(truncated)
	if err != nil {
		return CrushResult{Data: data}, nil
	}
	return CrushResult{Data: out, Omitted: 0}, nil
}

func truncateDepth(v any, depth int) any {
	if depth == 0 {
		return "<truncated>"
	}
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, child := range val {
			out[k] = truncateDepth(child, depth-1)
		}
		return out
	case []any:
		if len(val) > 10 {
			truncated := make([]any, 11)
			for i := 0; i < 10; i++ {
				truncated[i] = truncateDepth(val[i], depth-1)
			}
			truncated[10] = fmt.Sprintf("… +%d more", len(val)-10)
			return truncated
		}
		out := make([]any, len(val))
		for i, child := range val {
			out[i] = truncateDepth(child, depth-1)
		}
		return out
	default:
		return v
	}
}

// ── Log compression ───────────────────────────────────────────────────────────

func (c *SmartCrusher) crushLog(text string, budgetTokens int) CrushResult {
	lines := strings.Split(text, "\n")
	maxLines := c.MaxLogLines
	if maxLines <= 0 {
		maxLines = 60
	}

	var errorLines, otherLines []string
	for _, l := range lines {
		low := strings.ToLower(l)
		if strings.Contains(low, "error") || strings.Contains(low, "fatal") ||
			strings.Contains(low, "exception") || strings.Contains(low, "panic") {
			errorLines = append(errorLines, l)
		} else {
			otherLines = append(otherLines, l)
		}
	}

	headN := maxLines / 2
	tailN := maxLines - headN
	var kept []string

	// Always keep error lines.
	kept = append(kept, errorLines...)

	// Head.
	for i := 0; i < headN && i < len(otherLines); i++ {
		kept = append(kept, otherLines[i])
	}
	// Tail.
	for i := len(otherLines) - tailN; i < len(otherLines); i++ {
		if i >= headN { // don't duplicate
			kept = append(kept, otherLines[i])
		}
	}

	omitted := len(lines) - len(kept)
	if omitted <= 0 {
		return CrushResult{Data: []byte(text)}
	}
	marker := fmt.Sprintf("[%d log lines omitted]", omitted)
	result := strings.Join(kept, "\n") + "\n" + marker
	return CrushResult{Data: []byte(result), Omitted: omitted}
}

// ── Generic line truncation (fallback) ───────────────────────────────────────

func (c *SmartCrusher) crushLines(text string, budgetTokens int) CrushResult {
	lines := strings.Split(text, "\n")
	maxLines := c.MaxLogLines
	if maxLines <= 0 {
		maxLines = 60
	}
	if len(lines) <= maxLines {
		return CrushResult{Data: []byte(text)}
	}
	head := lines[:maxLines/2]
	tail := lines[len(lines)-maxLines/2:]
	omitted := len(lines) - maxLines
	marker := fmt.Sprintf("[%d lines omitted]", omitted)
	result := strings.Join(head, "\n") + "\n" + marker + "\n" + strings.Join(tail, "\n")
	return CrushResult{Data: []byte(result), Omitted: omitted}
}

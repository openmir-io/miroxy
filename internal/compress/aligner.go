// Package compress provides the builtin context-compression pipeline.
// Module 4 — CacheAligner: stabilise dynamic fields so provider KV-cache
// prefix hashing produces the same key across requests.
package compress

import (
	"regexp"
	"strings"
)

// AlignResult is the output of CacheAligner.Align.
type AlignResult struct {
	Content      string
	Replacements map[string]string // placeholder → original value
}

// dynamicPattern maps a regexp to a stable placeholder.
// Patterns are applied in order; an early match prevents later ones from
// running on the same span (handled by ReplaceAllStringFunc per-pattern).
var dynamicPatterns = []struct {
	re          *regexp.Regexp
	placeholder string
}{
	// UUID v4
	{
		regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}`),
		"<uuid>",
	},
	// ISO-8601 / RFC-3339 timestamps
	{
		regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})?`),
		"<timestamp>",
	},
	// Unix epoch (10 or 13 digits, not part of a longer number)
	{
		regexp.MustCompile(`\b(?:17|18|19|20)\d{8,10}\b`),
		"<epoch>",
	},
	// Common request-ID prefixes
	{
		regexp.MustCompile(`(?:req|msg|trace|span|sess)_[a-zA-Z0-9]{12,}`),
		"<request-id>",
	},
	// Bearer tokens (also scrubs credentials from logs)
	{
		regexp.MustCompile(`(?i)Bearer [a-zA-Z0-9\-._~+/]+=*`),
		"Bearer <token>",
	},
}

// CacheAligner replaces dynamic fields (UUIDs, timestamps, request IDs,
// Bearer tokens) with stable placeholders so that repeated requests produce
// the same prefix hash and hit the provider's KV cache.
type CacheAligner struct{}

// Align replaces dynamic fields in content with stable placeholders.
// The returned Replacements map records the last original value seen for each
// placeholder; pass it to the CCRStore to allow later restoration if needed.
func (a *CacheAligner) Align(content string) AlignResult {
	replacements := make(map[string]string)
	for _, dp := range dynamicPatterns {
		content = dp.re.ReplaceAllStringFunc(content, func(match string) string {
			// Only record the first original value for each placeholder key.
			if _, seen := replacements[dp.placeholder]; !seen {
				replacements[dp.placeholder] = match
			}
			return dp.placeholder
		})
	}
	return AlignResult{Content: content, Replacements: replacements}
}

// AlignMessages runs Align on every text and tool_result part of every
// message.  Returns the aligned messages and the union of all replacement maps.
func (a *CacheAligner) AlignMessages(msgs []alignMsg) ([]alignMsg, map[string]string) {
	all := make(map[string]string)
	out := make([]alignMsg, len(msgs))
	for i, m := range msgs {
		out[i] = m
		for j, p := range m.Parts {
			text := p.Text
			if text == "" {
				continue
			}
			r := a.Align(text)
			out[i].Parts[j].Text = r.Content
			for k, v := range r.Replacements {
				if _, ok := all[k]; !ok {
					all[k] = v
				}
			}
		}
	}
	return out, all
}

// alignMsg is a shim so aligner.go does not import ccomp (avoiding a
// dependency on ccomp in this low-level file). The builtin.go layer
// converts ccomp.Message ↔ alignMsg.
type alignMsg struct {
	Role  string
	Parts []alignPart
}

type alignPart struct {
	Text string
	// other fields from ccomp.ContentPart passed through unchanged
	Type      string
	ToolUseID string
	ToolID    string
	ToolName  string
	Raw       []byte
}

// isLogLike returns true when the content looks like log output:
// at least 3 lines where the majority start with a log-level keyword.
func isLogLike(s string) bool {
	lines := strings.Split(s, "\n")
	if len(lines) < 3 {
		return false
	}
	hits := 0
	for _, l := range lines {
		low := strings.ToLower(strings.TrimSpace(l))
		if strings.HasPrefix(low, "error") || strings.HasPrefix(low, "warn") ||
			strings.HasPrefix(low, "info") || strings.HasPrefix(low, "debug") ||
			strings.HasPrefix(low, "fatal") || strings.HasPrefix(low, "trace") {
			hits++
		}
	}
	return hits*3 >= len(lines)
}

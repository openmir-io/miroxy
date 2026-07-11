package compress

import ccomp "miroxy/core/compress"

// estimateTokens returns a rough token count for s using the standard
// 4-chars-per-token approximation (consistent with cl100k_base for ASCII).
func estimateTokens(s string) int {
	n := len(s) / 4
	if n < 1 {
		return 1
	}
	return n
}

// totalTokens returns the sum of estimated tokens across all message parts.
func totalTokens(msgs []ccomp.Message) int {
	t := 0
	for _, m := range msgs {
		t += messageTokens(m)
	}
	return t
}

func messageTokens(m ccomp.Message) int {
	t := 0
	for _, p := range m.Parts {
		t += partTokens(p)
	}
	return t
}

func partTokens(p ccomp.ContentPart) int {
	if len(p.Raw) > 0 {
		return estimateTokens(string(p.Raw))
	}
	return estimateTokens(p.Text)
}

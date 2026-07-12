package normalize

import (
	"encoding/base64"
	"encoding/hex"
	"regexp"
	"unicode/utf8"
)

var (
	base64Candidate = regexp.MustCompile(`[A-Za-z0-9+/]{16,}={0,2}`)
	hexCandidate    = regexp.MustCompile(`[0-9a-fA-F]{16,}`)
)

// DecodeCandidates scans s for base64- and hex-shaped substrings, decodes
// each, and returns the ones that decode to printable UTF-8 text. Detectors
// re-scan these alongside the original text so a secret wrapped in a single
// layer of base64/hex encoding still matches a plain pattern.
func DecodeCandidates(s string) []string {
	var out []string
	for _, m := range base64Candidate.FindAllString(s, -1) {
		if decoded, err := base64.StdEncoding.DecodeString(m); err == nil && isPrintableText(decoded) {
			out = append(out, string(decoded))
		}
	}
	for _, m := range hexCandidate.FindAllString(s, -1) {
		if len(m)%2 != 0 {
			m = m[:len(m)-1]
		}
		if decoded, err := hex.DecodeString(m); err == nil && isPrintableText(decoded) {
			out = append(out, string(decoded))
		}
	}
	return out
}

// isPrintableText rejects decoded output that's mostly binary noise — a
// real encoded secret/PII value decodes to printable text, an arbitrary
// byte string usually doesn't.
func isPrintableText(b []byte) bool {
	if len(b) == 0 || !utf8.Valid(b) {
		return false
	}
	printable := 0
	for _, r := range string(b) {
		if r == '\t' || r == '\n' || r == '\r' || (r >= 0x20 && r < 0x7f) {
			printable++
		}
	}
	return float64(printable)/float64(utf8.RuneCountInString(string(b))) > 0.95
}

package warden

import (
	"context"
	"encoding/base64"
	"math"
	"regexp"
	"strings"

	corewarden "miroxy/core/warden"
)

// clampScore keeps a summed confidence contribution within 0-100.
func clampScore(v int) int {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// confidenceScore combines the three signals every detector in this package
// can contribute — this project's own scheme, not a copy of any surveyed
// repo's factor breakdown:
//   - pattern:  how specific the regex/keyword match itself is. A
//     known-provider-prefix match (AKIA, ghp_, sk-ant-, ...) scores high
//     enough on its own to cross the Redact threshold with no other
//     signal — its false-positive rate is low enough that requiring a
//     nearby keyword before redacting would under-protect the common case
//     of a bare leaked key. A generic/ambiguous pattern (loose phone
//     shape, entropy-only fallback) scores low enough to need a checksum
//     or context hit to get there.
//   - checksum: set only when a structural validator (Luhn, mod-97, JWT
//     segment shape, ...) actually confirmed the match
//   - context:  a nearby keyword (e.g. "secret", "cvv") within a fixed window
func confidenceScore(pattern, checksum, context int) int {
	return clampScore(pattern + checksum + context)
}

// verdictForScore maps a 0-100 confidence score to a recommended action.
// Thresholds are this project's own ladder, not copied 1:1 from any
// surveyed repo.
func verdictForScore(score int) corewarden.Verdict {
	switch {
	case score >= 85:
		return corewarden.VerdictBlock
	case score >= 60:
		return corewarden.VerdictRedact
	case score >= 35:
		return corewarden.VerdictLog
	default:
		return corewarden.VerdictAllow
	}
}

// hasNearbyKeyword reports whether any of keywords appears (case-insensitive)
// within window bytes before start or after end in text.
func hasNearbyKeyword(text string, start, end, window int, keywords []string) bool {
	lo := start - window
	if lo < 0 {
		lo = 0
	}
	hi := end + window
	if hi > len(text) {
		hi = len(text)
	}
	near := strings.ToLower(text[lo:hi])
	for _, kw := range keywords {
		if strings.Contains(near, kw) {
			return true
		}
	}
	return false
}

var secretContextKeywords = []string{"secret", "key", "token", "credential", "api", "auth", "password"}

// secretRule is one regex-based credential pattern. checksum, when non-nil,
// gives the match a structural-validation bonus on top of the base pattern
// score.
type secretRule struct {
	typ       string
	pattern   *regexp.Regexp
	baseScore int
	checksum  func(match string) bool
}

// Known-provider-prefix rules score 60 alone (crossing the Redact
// threshold with no checksum/context needed) — a match on AKIA/ghp_/
// sk-ant-/etc. has a vanishingly low false-positive rate on its own, so
// requiring a nearby keyword before redacting would under-protect the
// common case of a bare leaked key. "sk-" alone (shared prefix scheme
// across several providers, more collision-prone) and JWT (needs its
// checksum to mean anything) stay lower and rely on their extra signal to
// cross the same threshold.
var secretRules = []secretRule{
	{typ: "aws_access_key_id", pattern: regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), baseScore: 60},
	{typ: "github_pat_classic", pattern: regexp.MustCompile(`\bghp_[A-Za-z0-9]{36}\b`), baseScore: 60},
	{typ: "github_oauth_token", pattern: regexp.MustCompile(`\bgho_[A-Za-z0-9]{36}\b`), baseScore: 60},
	{typ: "github_pat_fine_grained", pattern: regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{60,90}\b`), baseScore: 60},
	{typ: "anthropic_api_key", pattern: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9\-_]{20,}\b`), baseScore: 60},
	{typ: "openai_api_key", pattern: regexp.MustCompile(`\bsk-[A-Za-z0-9]{20,}\b`), baseScore: 45},
	{typ: "gcp_api_key", pattern: regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`), baseScore: 60},
	{typ: "slack_token", pattern: regexp.MustCompile(`\bxox[baprs]-[0-9A-Za-z-]{10,48}\b`), baseScore: 60},
	{typ: "stripe_key", pattern: regexp.MustCompile(`\b(?:sk|pk|rk)_(?:live|test)_[0-9a-zA-Z]{24,}\b`), baseScore: 60},
	{typ: "private_key_block", pattern: regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH |DSA |PGP )?PRIVATE KEY-----`), baseScore: 60},
	{
		typ:       "jwt",
		pattern:   regexp.MustCompile(`\bey[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`),
		baseScore: 35,
		checksum:  isWellFormedJWT,
	},
}

// isWellFormedJWT checks that a JWT candidate's first two dot-separated
// segments are valid base64url — a real JWT always decodes cleanly there,
// so this filters out incidental dotted strings that merely start with "ey".
func isWellFormedJWT(match string) bool {
	parts := strings.Split(match, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts[:2] {
		if _, err := base64.RawURLEncoding.DecodeString(p); err != nil {
			return false
		}
	}
	return true
}

// entropyTokenPattern finds long token-shaped runs with no recognized
// prefix, for the entropy fallback below.
var entropyTokenPattern = regexp.MustCompile(`[A-Za-z0-9+/_-]{20,}`)

// shannonEntropy returns the Shannon entropy of s in bits per character.
func shannonEntropy(s string) float64 {
	if s == "" {
		return 0
	}
	counts := make(map[rune]int)
	for _, r := range s {
		counts[r]++
	}
	n := float64(len(s))
	var entropy float64
	for _, c := range counts {
		p := float64(c) / n
		entropy -= p * math.Log2(p)
	}
	return entropy
}

const entropyThreshold = 4.3 // bits/char — same threshold LLM-Redactor's entropy detector uses, a widely-cited gitleaks default

// SecretInspector detects credential material via known-format regexes plus
// a generic high-entropy fallback for unrecognized key formats.
type SecretInspector struct{}

func NewSecretInspector() *SecretInspector { return &SecretInspector{} }

func (s *SecretInspector) Name() string { return "secrets" }

func (s *SecretInspector) Detect(_ context.Context, text string) ([]corewarden.Finding, error) {
	var findings []corewarden.Finding
	matched := make([]bool, len(text)+1)

	for _, rule := range secretRules {
		for _, loc := range rule.pattern.FindAllStringIndex(text, -1) {
			start, end := loc[0], loc[1]
			match := text[start:end]
			checksumScore := 0
			if rule.checksum != nil {
				if !rule.checksum(match) {
					continue
				}
				checksumScore = 35
			}
			contextScore := 0
			if hasNearbyKeyword(text, start, end, 40, secretContextKeywords) {
				contextScore = 20
			}
			score := confidenceScore(rule.baseScore, checksumScore, contextScore)
			findings = append(findings, corewarden.Finding{
				Category: corewarden.CategorySecret,
				Type:     rule.typ,
				Start:    start,
				End:      end,
				Value:    match,
				Score:    score,
				Verdict:  verdictForScore(score),
			})
			for i := start; i < end && i < len(matched); i++ {
				matched[i] = true
			}
		}
	}

	// Generic entropy fallback: long token-shaped substrings not already
	// claimed by a known-format rule above, with high enough entropy to be
	// unlikely to be natural-language or structured text.
	for _, loc := range entropyTokenPattern.FindAllStringIndex(text, -1) {
		start, end := loc[0], loc[1]
		if matched[start] {
			continue
		}
		match := text[start:end]
		if shannonEntropy(match) < entropyThreshold {
			continue
		}
		contextScore := 0
		if hasNearbyKeyword(text, start, end, 40, secretContextKeywords) {
			contextScore = 20
		}
		score := confidenceScore(20, 0, contextScore)
		findings = append(findings, corewarden.Finding{
			Category: corewarden.CategorySecret,
			Type:     "high_entropy_token",
			Start:    start,
			End:      end,
			Value:    match,
			Score:    score,
			Verdict:  verdictForScore(score),
		})
	}

	return findings, nil
}

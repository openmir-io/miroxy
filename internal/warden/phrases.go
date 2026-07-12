package warden

import (
	"context"
	"strings"

	corewarden "miroxy/core/warden"
	"miroxy/internal/warden/normalize"
)

// phraseRule is one denylist entry. Unlike secrets/PII, a literal phrase
// match has no separate structural checksum or context signal to layer on
// top — the match itself either is or isn't the phrase — so baseScore is
// assigned per-phrase directly, reflecting how unambiguous that specific
// wording is, rather than decomposed into pattern/checksum/context.
type phraseRule struct {
	typ       string
	phrase    string // lowercase literal, matched against normalized text
	baseScore int
}

var injectionPhraseRules = []phraseRule{
	{typ: "instruction_override", phrase: "ignore all previous instructions", baseScore: 90},
	{typ: "instruction_override", phrase: "ignore previous instructions", baseScore: 90},
	{typ: "instruction_override", phrase: "disregard all prior instructions", baseScore: 90},
	{typ: "instruction_override", phrase: "disregard the above", baseScore: 80},
	{typ: "instruction_override", phrase: "forget everything above", baseScore: 85},
	{typ: "instruction_override", phrase: "override your instructions", baseScore: 85},
	{typ: "prompt_extraction", phrase: "reveal your system prompt", baseScore: 85},
	{typ: "prompt_extraction", phrase: "print your instructions", baseScore: 75},
	{typ: "prompt_extraction", phrase: "repeat the text above", baseScore: 70},
	{typ: "role_injection", phrase: "new instructions:", baseScore: 65},
	{typ: "role_injection", phrase: "system prompt:", baseScore: 60},
}

var jailbreakPhraseRules = []phraseRule{
	{typ: "known_jailbreak_persona", phrase: "dan mode", baseScore: 80},
	{typ: "known_jailbreak_persona", phrase: "developer mode", baseScore: 60},
	{typ: "restriction_bypass", phrase: "pretend you have no restrictions", baseScore: 85},
	{typ: "restriction_bypass", phrase: "ignore your guidelines", baseScore: 85},
	{typ: "restriction_bypass", phrase: "bypass your safety", baseScore: 85},
	{typ: "restriction_bypass", phrase: "act without any restrictions", baseScore: 85},
	{typ: "restriction_bypass", phrase: "no ethical guidelines", baseScore: 80},
	{typ: "restriction_bypass", phrase: "unfiltered ai", baseScore: 65},
	{typ: "known_jailbreak_persona", phrase: "jailbreak", baseScore: 55},
}

// PhraseInspector matches a fixed denylist of phrases against
// anti-evasion-normalized text. It backs both the injection and jailbreak
// categories — same mechanism, different rule lists and Category tag.
type PhraseInspector struct {
	name     string
	category corewarden.Category
	rules    []phraseRule
}

func NewInjectionInspector() *PhraseInspector {
	return &PhraseInspector{name: "injection", category: corewarden.CategoryInjection, rules: injectionPhraseRules}
}

func NewJailbreakInspector() *PhraseInspector {
	return &PhraseInspector{name: "jailbreak", category: corewarden.CategoryJailbreak, rules: jailbreakPhraseRules}
}

func (p *PhraseInspector) Name() string { return p.name }

// Detect matches against a lowercased, anti-evasion-normalized copy of
// text. Reported Start/End offsets index into that normalized copy, not the
// original — acceptable here because phrase findings are acted on by
// blocking the request outright, never by surgically redacting a span (see
// WardenPlugin), so byte-exact original-text offsets aren't needed.
func (p *PhraseInspector) Detect(_ context.Context, text string) ([]corewarden.Finding, error) {
	canon := strings.ToLower(normalize.Canonicalize(text))

	var findings []corewarden.Finding
	for _, rule := range p.rules {
		searchFrom := 0
		for {
			i := strings.Index(canon[searchFrom:], rule.phrase)
			if i < 0 {
				break
			}
			start := searchFrom + i
			end := start + len(rule.phrase)
			score := confidenceScore(rule.baseScore, 0, 0)
			findings = append(findings, corewarden.Finding{
				Category: p.category,
				Type:     rule.typ,
				Start:    start,
				End:      end,
				Value:    canon[start:end],
				Score:    score,
				Verdict:  verdictForScore(score),
			})
			searchFrom = end
		}
	}
	return findings, nil
}

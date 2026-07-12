// Package normalize applies anti-evasion text normalization before content
// is handed to warden's detectors, so a secret or PII value split up with
// zero-width characters, cross-script lookalike letters, or combining
// accents still matches a plain-ASCII pattern.
package normalize

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// invisibleRunes are zero-width/format characters with no visible glyph,
// commonly inserted between characters of a secret to dodge a literal
// substring match. Written as bare hex code points rather than literal
// characters so the source file itself stays free of invisible bytes.
var invisibleRunes = map[rune]struct{}{
	0x200B: {}, // zero-width space
	0x200C: {}, // zero-width non-joiner
	0x200D: {}, // zero-width joiner
	0x2060: {}, // word joiner
	0xFEFF: {}, // BOM / zero-width no-break space
}

// crossScriptLookalikes maps Cyrillic/Greek letters that are visually
// indistinguishable from a Latin letter onto that Latin letter. Unlike
// fullwidth forms or the Mathematical Alphanumeric Symbols block — both of
// which norm.NFKC already folds via their Unicode compatibility
// decomposition — these are semantically distinct letters in a different
// script with no compatibility mapping to Latin, so NFKC alone won't catch
// them.
var crossScriptLookalikes = map[rune]rune{
	// Cyrillic
	'а': 'a', 'А': 'A',
	'В': 'B',
	'е': 'e', 'Е': 'E',
	'К': 'K',
	'М': 'M',
	'Н': 'H',
	'о': 'o', 'О': 'O',
	'р': 'p', 'Р': 'P',
	'с': 'c', 'С': 'C',
	'Т': 'T',
	'у': 'y', 'У': 'Y',
	'х': 'x', 'Х': 'X',
	// Greek
	'α': 'a', 'Α': 'A',
	'β': 'b', 'Β': 'B',
	'ε': 'e', 'Ε': 'E',
	'ι': 'i', 'Ι': 'I',
	'κ': 'k', 'Κ': 'K',
	'ο': 'o', 'Ο': 'O',
	'ρ': 'p', 'Ρ': 'P',
	'τ': 't', 'Τ': 'T',
	'υ': 'y', 'Υ': 'Y',
	'χ': 'x', 'Χ': 'X',
}

// StripInvisible removes zero-width/format characters from s.
func StripInvisible(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if _, invisible := invisibleRunes[r]; invisible {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// FoldLookalikes rewrites cross-script lookalike letters to their Latin
// equivalent.
func FoldLookalikes(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if folded, ok := crossScriptLookalikes[r]; ok {
			b.WriteRune(folded)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// FoldAccents strips combining diacritical marks (e.g. é -> e) via
// decompose-then-filter.
func FoldAccents(s string) string {
	decomposed := norm.NFD.String(s)
	var b strings.Builder
	b.Grow(len(decomposed))
	for _, r := range decomposed {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		b.WriteRune(r)
	}
	return norm.NFC.String(b.String())
}

// Canonicalize runs the full anti-evasion pipeline: strip invisible
// characters, apply Unicode compatibility normalization (folds fullwidth
// forms and the Mathematical Alphanumeric Symbols block for free), fold
// cross-script lookalikes, then fold accents. The result is intended for
// detector matching only — never surfaced back to the user.
func Canonicalize(s string) string {
	s = StripInvisible(s)
	s = norm.NFKC.String(s)
	s = FoldLookalikes(s)
	s = FoldAccents(s)
	return s
}

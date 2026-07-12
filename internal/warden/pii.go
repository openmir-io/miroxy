package warden

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	corewarden "miroxy/core/warden"
)

var (
	emailPattern = regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`)
	phonePattern = regexp.MustCompile(`\b(?:\+?\d{1,3}[-.\s]?)?\(?\d{3}\)?[-.\s]?\d{3}[-.\s]?\d{4}\b`)
	ipv4Pattern  = regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4][0-9]|1?[0-9]{1,2})\.){3}(?:25[0-5]|2[0-4][0-9]|1?[0-9]{1,2})\b`)
	cardPattern  = regexp.MustCompile(`\b\d(?:[ -]?\d){12,18}\b`)
	ibanPattern  = regexp.MustCompile(`\b[A-Z]{2}\d{2}[A-Z0-9]{11,30}\b`)
	ssnPattern   = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)
)

var (
	emailContextKeywords = []string{"email", "e-mail", "contact"}
	phoneContextKeywords = []string{"phone", "mobile", "call", "tel"}
	ipContextKeywords    = []string{"ip address", "server", "host"}
	cardContextKeywords  = []string{"card", "cvv", "payment", "credit"}
	ibanContextKeywords  = []string{"iban", "account", "bank"}
	ssnContextKeywords   = []string{"ssn", "social security"}
)

// onlyDigits strips everything but decimal digits from s.
func onlyDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// luhnValid checks the Luhn (mod-10) checksum used by card numbers.
func luhnValid(digits string) bool {
	if len(digits) < 12 {
		return false
	}
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

// ibanValid checks the ISO 7064 MOD 97-10 checksum used by IBANs.
func ibanValid(s string) bool {
	s = strings.ToUpper(strings.ReplaceAll(s, " ", ""))
	if len(s) < 15 || len(s) > 34 {
		return false
	}
	rearranged := s[4:] + s[:4]
	var numeric strings.Builder
	for _, r := range rearranged {
		switch {
		case r >= '0' && r <= '9':
			numeric.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			numeric.WriteString(strconv.Itoa(int(r-'A') + 10))
		default:
			return false
		}
	}
	remainder := 0
	for _, c := range numeric.String() {
		remainder = (remainder*10 + int(c-'0')) % 97
	}
	return remainder == 1
}

// maskEmail keeps the first local-part character and the whole domain,
// masking the rest of the local part: j***@example.com.
func maskEmail(s string) string {
	at := strings.IndexByte(s, '@')
	if at <= 0 {
		return maskGeneric(s)
	}
	local, domain := s[:at], s[at:]
	if len(local) <= 1 {
		return local + "***" + domain
	}
	return local[:1] + strings.Repeat("*", len(local)-1) + domain
}

// maskPAN applies the PCI-DSS convention of preserving the first 6 (issuer
// BIN) and last 4 digits of a card number, masking everything between.
func maskPAN(s string) string {
	digits := onlyDigits(s)
	if len(digits) < 10 {
		return maskGeneric(s)
	}
	return digits[:6] + strings.Repeat("*", len(digits)-10) + digits[len(digits)-4:]
}

// maskGeneric masks the middle of a value, keeping short edges for context.
func maskGeneric(s string) string {
	if len(s) <= 4 {
		return strings.Repeat("*", len(s))
	}
	return s[:2] + strings.Repeat("*", len(s)-4) + s[len(s)-2:]
}

// Mask returns a destructive placeholder for f.Value, using a PII-aware
// format when one exists (email, card) and a generic middle-mask otherwise.
func Mask(f corewarden.Finding) string {
	switch f.Type {
	case "email":
		return maskEmail(f.Value)
	case "credit_card":
		return maskPAN(f.Value)
	default:
		return maskGeneric(f.Value)
	}
}

// PIIInspector detects structured personal data: email, phone, IP address,
// credit-card numbers (Luhn-validated), IBANs (mod-97 validated), and
// US-SSN-shaped numbers.
type PIIInspector struct{}

func NewPIIInspector() *PIIInspector { return &PIIInspector{} }

func (p *PIIInspector) Name() string { return "pii" }

func (p *PIIInspector) Detect(_ context.Context, text string) ([]corewarden.Finding, error) {
	var findings []corewarden.Finding

	add := func(typ string, start, end int, base, checksum int, keywords []string) {
		contextScore := 0
		if hasNearbyKeyword(text, start, end, 40, keywords) {
			contextScore = 20
		}
		score := confidenceScore(base, checksum, contextScore)
		findings = append(findings, corewarden.Finding{
			Category: corewarden.CategoryPII,
			Type:     typ,
			Start:    start,
			End:      end,
			Value:    text[start:end],
			Score:    score,
			Verdict:  verdictForScore(score),
		})
	}

	// email/ssn score high enough alone to cross the Redact threshold — both
	// patterns have a low false-positive rate on their own. phone/ip stay
	// lower and need a nearby context keyword to redact by default: both
	// patterns are loose enough (arbitrary digit groups, version-number-
	// shaped strings) that redacting on pattern alone would over-trigger.
	for _, loc := range emailPattern.FindAllStringIndex(text, -1) {
		add("email", loc[0], loc[1], 60, 0, emailContextKeywords)
	}
	for _, loc := range phonePattern.FindAllStringIndex(text, -1) {
		add("phone", loc[0], loc[1], 40, 0, phoneContextKeywords)
	}
	for _, loc := range ipv4Pattern.FindAllStringIndex(text, -1) {
		add("ip_address", loc[0], loc[1], 30, 0, ipContextKeywords)
	}
	for _, loc := range ssnPattern.FindAllStringIndex(text, -1) {
		add("ssn", loc[0], loc[1], 60, 0, ssnContextKeywords)
	}
	for _, loc := range cardPattern.FindAllStringIndex(text, -1) {
		start, end := loc[0], loc[1]
		digits := onlyDigits(text[start:end])
		checksum := 0
		if luhnValid(digits) {
			checksum = 35
		} else {
			continue // no checksum match and no known-prefix -> too weak a signal, drop
		}
		add("credit_card", start, end, 30, checksum, cardContextKeywords)
	}
	for _, loc := range ibanPattern.FindAllStringIndex(text, -1) {
		start, end := loc[0], loc[1]
		checksum := 0
		if ibanValid(text[start:end]) {
			checksum = 35
		} else {
			continue
		}
		add("iban", start, end, 30, checksum, ibanContextKeywords)
	}

	return findings, nil
}

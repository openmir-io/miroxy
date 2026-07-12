// Package warden defines the stable interface for request/response content
// defense: secret and PII detection, prompt-injection/jailbreak phrase
// matching, and reversible tokenization. Builtin (in-process, pure Go) and
// future sidecar (external service) implementations both satisfy Warden —
// same split as core/cred.CredentialSource and core/router.Router.
package warden

import "context"

// Category classifies what kind of content a Finding matched.
type Category int

const (
	CategorySecret Category = iota
	CategoryPII
	CategoryInjection
	CategoryJailbreak
)

func (c Category) String() string {
	switch c {
	case CategorySecret:
		return "secret"
	case CategoryPII:
		return "pii"
	case CategoryInjection:
		return "injection"
	case CategoryJailbreak:
		return "jailbreak"
	default:
		return "unknown"
	}
}

// Verdict is the action an Inspector recommends for a Finding, driven by its
// confidence score.
type Verdict int

const (
	VerdictAllow Verdict = iota
	VerdictLog
	VerdictRedact
	VerdictBlock
)

func (v Verdict) String() string {
	switch v {
	case VerdictAllow:
		return "allow"
	case VerdictLog:
		return "log"
	case VerdictRedact:
		return "redact"
	case VerdictBlock:
		return "block"
	default:
		return "unknown"
	}
}

// Finding is one detected match within a scanned string.
type Finding struct {
	Category Category
	Type     string // detector-specific label, e.g. "aws_access_key", "email"
	Start    int    // byte offset into the scanned text
	End      int    // byte offset into the scanned text (exclusive)
	Value    string // the matched substring
	Score    int    // 0-100 confidence
	Verdict  Verdict
}

// Inspector detects one category/family of content within a string.
// Implementations must be safe for concurrent use.
type Inspector interface {
	Name() string
	Detect(ctx context.Context, text string) ([]Finding, error)
}

// Warden inspects text for sensitive/unsafe content and sanitizes it before
// the request continues, then reverses that sanitization on the response.
// Implementations must be safe for concurrent use.
type Warden interface {
	// Inspect runs every configured Inspector over text and returns the
	// deduplicated findings (overlapping spans keep the highest score).
	Inspect(ctx context.Context, text string) ([]Finding, error)

	// Sanitize rewrites text according to findings' verdicts (redact/
	// tokenize) and returns the rewritten text plus the findings that were
	// actually acted on (VerdictAllow/VerdictLog findings pass through
	// unchanged and are omitted). vault is consulted only in tokenize mode;
	// it is owned by the caller (scoped to one request) since Warden itself
	// is a long-lived, config-driven singleton with no per-request state —
	// same split as core/router.Router vs. the per-request RouteTarget it
	// returns. Pass nil when tokenize mode isn't in use.
	Sanitize(ctx context.Context, text string, findings []Finding, vault TokenVault) (string, []Finding)
}

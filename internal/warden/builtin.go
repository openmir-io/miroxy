// Package warden is the builtin (in-process, pure Go) implementation of
// core/warden.Warden. A future sidecar-backed Warden (e.g. an external
// Presidio-style NER service for semantic PII, or a real toxicity
// classifier) would live alongside BuiltinWarden here, exactly how
// internal/router holds BuiltinRouter behind core/router.Router — no
// shared transport abstraction, each implementation picks its own IO.
package warden

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	corewarden "miroxy/core/warden"
)

// Config is the builtin Warden's runtime policy. Swapped atomically on
// UpdateConfig so Inspect/Sanitize never block on a mutex and are safe
// under concurrent config reload.
type Config struct {
	Enabled bool
	// Mode selects how Redact/Block-verdict findings are rewritten:
	// "redact" (destructive masking, the default), "tokenize" (reversible
	// vault placeholders), or "block_only" (never rewrite — VerdictBlock
	// findings still halt the request, everything else just passes).
	Mode string
	// Secrets/PII/Injection/Jailbreak toggle each Inspector independently.
	Secrets   bool
	PII       bool
	Injection bool
	Jailbreak bool
	// FailClosed, when true, turns an Inspector error (e.g. a recovered
	// panic) into a blocked request rather than a best-effort pass —
	// aisecuritygateway's fail-closed pattern, opt-in here.
	FailClosed bool
}

// DefaultConfig returns the out-of-the-box policy: everything on, redact
// mode, fail-open (a brand-new detector failing closed by default risks
// false-positive outages more than it buys safety).
func DefaultConfig() *Config {
	return &Config{
		Enabled:   true,
		Mode:      "redact",
		Secrets:   true,
		PII:       true,
		Injection: true,
		Jailbreak: true,
	}
}

// BuiltinWarden is the in-process Warden: a fixed set of pure-Go Inspectors
// run fan-out/fan-in over the scanned text. All of today's Inspectors are
// cheap (regex/checksum/literal-phrase matching) so there is no fast/slow
// split like tamga's — every Inspector always runs, in its own goroutine,
// joined with a WaitGroup.
type BuiltinWarden struct {
	cfg    atomic.Pointer[Config]
	active atomic.Pointer[[]corewarden.Inspector]
}

var _ corewarden.Warden = (*BuiltinWarden)(nil)

// NewBuiltinWarden creates a BuiltinWarden with DefaultConfig applied.
func NewBuiltinWarden() *BuiltinWarden {
	w := &BuiltinWarden{}
	w.UpdateConfig(DefaultConfig())
	return w
}

// UpdateConfig swaps in a new policy and rebuilds the active Inspector set.
func (w *BuiltinWarden) UpdateConfig(cfg *Config) {
	w.cfg.Store(cfg)

	var active []corewarden.Inspector
	if cfg.Secrets {
		active = append(active, NewSecretInspector())
	}
	if cfg.PII {
		active = append(active, NewPIIInspector())
	}
	if cfg.Injection {
		active = append(active, NewInjectionInspector())
	}
	if cfg.Jailbreak {
		active = append(active, NewJailbreakInspector())
	}
	w.active.Store(&active)
}

// Config returns the currently active policy.
func (w *BuiltinWarden) Config() *Config { return w.cfg.Load() }

// Inspect runs every active Inspector over text concurrently, recovers any
// individual Inspector panic (so one bad detector can never take down a
// request — the same "no panic in the request path" rule the rest of
// miroxy follows), and deduplicates overlapping findings by keeping the
// highest-scored one per overlapping span.
func (w *BuiltinWarden) Inspect(ctx context.Context, text string) ([]corewarden.Finding, error) {
	inspectors := w.active.Load()
	if inspectors == nil || len(*inspectors) == 0 {
		return nil, nil
	}

	type result struct {
		findings []corewarden.Finding
		err      error
	}
	results := make([]result, len(*inspectors))

	var wg sync.WaitGroup
	for i, insp := range *inspectors {
		wg.Add(1)
		go func(i int, insp corewarden.Inspector) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					results[i].err = fmt.Errorf("warden: inspector %q panicked: %v", insp.Name(), r)
				}
			}()
			findings, err := insp.Detect(ctx, text)
			results[i].findings, results[i].err = findings, err
		}(i, insp)
	}
	wg.Wait()

	var all []corewarden.Finding
	var firstErr error
	for _, r := range results {
		if r.err != nil && firstErr == nil {
			firstErr = r.err
		}
		all = append(all, r.findings...)
	}

	return dedupeOverlapping(all), firstErr
}

// dedupeOverlapping sorts findings by start offset and, for each pair of
// overlapping spans, keeps only the higher-scored one.
func dedupeOverlapping(findings []corewarden.Finding) []corewarden.Finding {
	if len(findings) < 2 {
		return findings
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].Start < findings[j].Start })

	kept := findings[:1]
	for _, f := range findings[1:] {
		last := &kept[len(kept)-1]
		if f.Start < last.End { // overlaps the last kept finding
			if f.Score > last.Score {
				*last = f
			}
			continue
		}
		kept = append(kept, f)
	}
	return kept
}

// Replacement returns the substitution string a Redact/Block-verdict
// finding would get under the current Mode. Exposed (not just used
// internally by Sanitize) so a caller that needs to apply the identical
// substitution to a second representation of the same content — see
// WardenPlugin's raw-request-body pass, which mirrors the substitutions
// already applied to c.Request rather than re-inspecting the raw bytes
// from scratch — doesn't have to duplicate the Mode dispatch logic.
// Idempotent in tokenize mode: BuiltinVault.Tokenize caches by
// (category, type, value), so calling this again for a finding Sanitize
// already applied returns the same token rather than minting a new one.
func (w *BuiltinWarden) Replacement(f corewarden.Finding, vault corewarden.TokenVault) string {
	cfg := w.cfg.Load()
	if cfg.Mode == "tokenize" && vault != nil {
		return vault.Tokenize(f.Category, f.Type, f.Value)
	}
	return Mask(f)
}

// Sanitize rewrites findings with a Redact/Block verdict according to the
// configured Mode. Log/Allow findings are left untouched — they're purely
// observational (see stats.go). vault is only consulted in "tokenize" mode.
func (w *BuiltinWarden) Sanitize(_ context.Context, text string, findings []corewarden.Finding, vault corewarden.TokenVault) (string, []corewarden.Finding) {
	cfg := w.cfg.Load()

	var toRewrite []corewarden.Finding
	for _, f := range findings {
		if f.Verdict == corewarden.VerdictAllow || f.Verdict == corewarden.VerdictLog {
			continue
		}
		toRewrite = append(toRewrite, f)
	}
	if len(toRewrite) == 0 || cfg.Mode == "block_only" {
		return text, toRewrite
	}

	// Replace right-to-left so earlier offsets stay valid as the string
	// length changes underneath later replacements.
	sort.Slice(toRewrite, func(i, j int) bool { return toRewrite[i].Start > toRewrite[j].Start })

	out := text
	for _, f := range toRewrite {
		out = out[:f.Start] + w.Replacement(f, vault) + out[f.End:]
	}
	return out, toRewrite
}

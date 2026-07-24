package warden

import (
	"bytes"
	"context"
	"fmt"

	"miroxy/core/ir"
	corewarden "miroxy/core/warden"
	"miroxy/internal/pipeline"
)

// WardenPlugin wires BuiltinWarden into the pipeline at PriorityWarden —
// the first real occupant of that slot, matching the intended
// "security/warden ⇒ compress ⇒ route-select ⇒ ..." ordering: CompressPlugin
// already runs at PriorityWarden+50 expecting this to have run first.
type WardenPlugin struct {
	warden *BuiltinWarden
	stats  *Stats
}

// NewWardenPlugin wires w into the pipeline. stats may be nil (disabled).
func NewWardenPlugin(w *BuiltinWarden, stats *Stats) *WardenPlugin {
	return &WardenPlugin{warden: w, stats: stats}
}

func (p *WardenPlugin) Name() string  { return "warden" }
func (p *WardenPlugin) Priority() int { return pipeline.PriorityWarden }

// Execute inspects and sanitizes the inbound request, halts the chain on
// any Block verdict, otherwise forwards to next and then resolves vault
// tokens in whatever comes back — covering non-streaming, canonical SSE,
// and raw-passthrough streaming responses alike.
func (p *WardenPlugin) Execute(c *pipeline.LLMContext, next pipeline.Handler) error {
	cfg := p.warden.Config()
	if !cfg.Enabled {
		return next(c)
	}

	vault := NewBuiltinVault()
	findings, substitutions, scanErr := p.sanitizeRequest(c.RequestCtx, c, vault)

	if p.stats != nil {
		// substitutions counts every acted-on finding regardless of Mode —
		// in "redact" mode those are destructive masks, not vault tokens,
		// so only report tokensVaulted when tokenize mode actually minted
		// real vault entries.
		tokensVaulted := 0
		if cfg.Mode == "tokenize" {
			tokensVaulted = len(substitutions)
		}
		p.stats.Record(findings, tokensVaulted)
	}
	if scanErr != nil && cfg.FailClosed {
		return &pipeline.PipelineError{
			Status:  500,
			ErrType: "internal_error",
			Msg:     "content safety scan failed; request blocked (fail_closed policy)",
		}
	}
	for _, f := range findings {
		if f.Verdict == corewarden.VerdictBlock {
			return &pipeline.PipelineError{
				Status:  400,
				ErrType: "invalid_request_error",
				Msg:     fmt.Sprintf("request blocked by content safety policy (%s: %s)", f.Category, f.Type),
			}
		}
	}

	if err := next(c); err != nil {
		return err
	}

	p.resolveResponse(c, vault)
	return nil
}

// sanitizeRequest scans and rewrites every text field in c.Request, then
// mirrors the same substitutions onto c.RawRequestBody directly (a second
// byte-level pass, not a second Inspect call — see dispatchFor in
// internal/server/upstream.go: passthrough mode ships RawRequestBody
// verbatim, completely bypassing c.Request, so a retry attempt that lands
// on a passthrough-eligible target must see the same redactions or Warden
// provides zero protection for that path).
func (p *WardenPlugin) sanitizeRequest(ctx context.Context, c *pipeline.LLMContext, vault *BuiltinVault) ([]corewarden.Finding, map[string]string, error) {
	fields := collectTextFields(c.Request)

	var allFindings []corewarden.Finding
	substitutions := make(map[string]string)
	var firstErr error

	for _, field := range fields {
		findings, err := p.warden.Inspect(ctx, field.text)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		allFindings = append(allFindings, findings...)

		sanitized, acted := p.warden.Sanitize(ctx, field.text, findings, vault)
		if len(acted) == 0 {
			continue
		}
		field.set(sanitized)
		for _, f := range acted {
			substitutions[f.Value] = p.warden.Replacement(f, vault)
		}
	}

	if len(substitutions) > 0 && len(c.RawRequestBody) > 0 {
		raw := c.RawRequestBody
		for original, replacement := range substitutions {
			raw = bytes.ReplaceAll(raw, []byte(original), []byte(replacement))
		}
		c.RawRequestBody = raw
	}

	return allFindings, substitutions, firstErr
}

// resolveResponse restores vault tokens in whatever response shape
// UpstreamExecutor produced. Exactly one of the four blocks below actually
// does anything for a given request — canonical vs. raw and streaming vs.
// non-streaming are mutually exclusive per dispatchFor's per-attempt
// choice — but checking all four is cheap and needs no knowledge of which
// path this attempt took.
func (p *WardenPlugin) resolveResponse(c *pipeline.LLMContext, vault *BuiltinVault) {
	if c.Response != nil {
		for i := range c.Response.Content {
			if t := c.Response.Content[i].Text; t != nil && t.Text != "" {
				t.Text = vault.Resolve(t.Text)
			}
			if r := c.Response.Content[i].Reasoning; r != nil && r.Text != "" {
				r.Text = vault.Resolve(r.Text)
			}
		}
	}

	if body, contentType, status, ok := c.RawResponse(); ok {
		c.SetRawResponse([]byte(vault.Resolve(string(body))), contentType, status)
	}

	if src := c.StreamSrc(); src != nil {
		c.SetStream(ResolveEvents(src, vault), c.ReleaseFunc())
	}

	if body, contentType, status, ok := c.RawStream(); ok {
		c.SetRawStream(NewResolvingReader(body, vault), contentType, status, c.ReleaseFunc())
	}
}

// textField is one scannable/rewritable text location within an IRRequest:
// the system prompt, a text-type content part, or a reasoning part's visible
// text. tool_use/tool_result payloads are not scanned in v1 — a documented
// scope limit, not an oversight.
type textField struct {
	text string
	set  func(string)
}

func collectTextFields(req *ir.IRRequest) []textField {
	var fields []textField

	if req.System != "" {
		fields = append(fields, textField{
			text: req.System,
			set:  func(s string) { req.System = s },
		})
	}

	for i := range req.Messages {
		parts := req.Messages[i].Parts
		for j := range parts {
			if part := parts[j].Text; part != nil && part.Text != "" {
				fields = append(fields, textField{
					text: part.Text,
					set:  func(s string) { part.Text = s },
				})
			}
			if r := parts[j].Reasoning; r != nil && r.Text != "" {
				fields = append(fields, textField{
					text: r.Text,
					set:  func(s string) { r.Text = s },
				})
			}
		}
	}

	return fields
}

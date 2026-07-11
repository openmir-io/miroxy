package pipeline

import (
	"fmt"
	"strings"
	"sync/atomic"

	"miroxy/internal/idgen"
	"miroxy/internal/types"
)

// CommandPlugin intercepts messages starting with ":miroxy" and handles them
// locally — zero LLM tokens, instant response.
//
// Syntax:
//
//	:miroxy ?|help             — list all commands
//	:miroxy <cmd>              — execute command, return result directly
//	:miroxy <cmd> ?            — show help for that command
//	:miroxy <cmd> <text...>    — execute command, inject result, forward <text> to LLM
//	:miroxy on|off             — enable/disable commands at runtime (until restart)
//
// The plugin fires ONLY when:
//  1. The last message has role=="user"
//  2. The trimmed text starts at position 0 with ":miroxy" (case-sensitive)
//
// This means normal conversation about miroxy never triggers the plugin.
const priorityCommand = PriorityAuth + 5 // = 5

// ServerRef is the subset of Server capabilities the CommandPlugin needs.
type ServerRef interface {
	StatsText() string
	SetDump(enabled bool)
}

// CommandConfig is loaded from config at startup.
type CommandConfig struct {
	// Disabled turns off all :miroxy commands. Default false (commands enabled).
	Disabled bool
	// AllowDump permits :miroxy dump on|off. Default false.
	AllowDump bool
}

// CommandPlugin intercepts :miroxy messages before they reach the LLM.
type CommandPlugin struct {
	srv      ServerRef
	cfg      CommandConfig
	prefix   string
	disabled atomic.Bool // runtime toggle: :miroxy off / :miroxy on
}

// NewCommandPlugin creates a CommandPlugin bound to the given server.
func NewCommandPlugin(srv ServerRef, cfg CommandConfig) *CommandPlugin {
	p := &CommandPlugin{srv: srv, cfg: cfg, prefix: ":miroxy"}
	p.disabled.Store(cfg.Disabled)
	return p
}

func (p *CommandPlugin) Name() string  { return "command" }
func (p *CommandPlugin) Priority() int { return priorityCommand }

func (p *CommandPlugin) Execute(c *LLMContext, next Handler) error {
	text, ok := lastUserText(c.Request)
	if !ok {
		return next(c)
	}

	// Must start at position 0, case-sensitive.
	if !strings.HasPrefix(text, p.prefix) {
		return next(c)
	}

	rest := strings.TrimSpace(text[len(p.prefix):])

	// Runtime on/off — always handled even when disabled.
	switch rest {
	case "on":
		p.disabled.Store(false)
		p.deliver(c, ":miroxy commands enabled")
		return nil
	case "off":
		p.disabled.Store(true)
		p.deliver(c, ":miroxy commands disabled until restart (or :miroxy on to re-enable)")
		return nil
	}

	// If commands are disabled, pass through to LLM.
	if p.disabled.Load() {
		return next(c)
	}

	// Top-level help.
	if rest == "" || rest == "?" || rest == "help" {
		p.deliver(c, p.topHelp())
		return nil
	}

	parts := strings.SplitN(rest, " ", 2)
	cmd := parts[0]
	extra := ""
	if len(parts) > 1 {
		extra = strings.TrimSpace(parts[1])
	}

	// Per-command help.
	if extra == "?" || extra == "help" {
		p.deliver(c, p.cmdHelp(cmd))
		return nil
	}

	return p.dispatch(c, next, cmd, extra)
}

func (p *CommandPlugin) dispatch(c *LLMContext, next Handler, cmd, extra string) error {
	switch cmd {
	case "stats":
		return p.respond(c, next, p.srv.StatsText(), extra)

	case "health":
		return p.respond(c, next, "status: healthy\nin_flight: (see :miroxy stats)", extra)

	case "dump":
		if !p.cfg.AllowDump {
			p.deliver(c,
				"dump command is disabled.\n"+
					"To enable: set server.commands.allow_dump: true in config.yaml, then restart miroxy.\n"+
					"Note: restart required — this setting is not hot-reloadable.")
			return nil
		}
		switch extra {
		case "on":
			p.srv.SetDump(true)
			p.deliver(c, "dump enabled — all traffic written to dump.jsonl\nWarning: captures all user message content.")
		case "off":
			p.srv.SetDump(false)
			p.deliver(c, "dump disabled")
		default:
			p.deliver(c, "usage: :miroxy dump on|off\n       :miroxy dump ? for help")
		}
		return nil

	default:
		// Unknown command → error, never forward to LLM.
		p.deliver(c, fmt.Sprintf("unknown command %q\nType :miroxy ? to see available commands.", cmd))
		return nil
	}
}

// respond short-circuits when extra is empty, otherwise injects output + forwards.
func (p *CommandPlugin) respond(c *LLMContext, next Handler, output, extra string) error {
	if extra == "" {
		p.deliver(c, output)
		return nil
	}
	replaceLastUserText(c.Request, fmt.Sprintf("```\n%s\n```\n\n%s", output, extra))
	return next(c)
}

// deliver sets c.Response with the synthetic text reply.
// When the client requested stream:true, makeHandler detects c.Response != nil
// and calls a.WriteResponseAsStream() on the DownstreamAdapter — keeping the
// pipeline layer free of any protocol-specific SSE format knowledge.
func (p *CommandPlugin) deliver(c *LLMContext, text string) {
	model := c.Request.Model
	if model == "" {
		model = "miroxy"
	}
	c.Response = &types.MessageResponse{
		ID:         "msg_" + idgen.NewMsgID(),
		Type:       "message",
		Role:       "assistant",
		Model:      model,
		Content:    []types.ContentBlock{{Type: "text", Text: text}},
		StopReason: "end_turn",
	}
}

// ── help text ─────────────────────────────────────────────────────────────────

func (p *CommandPlugin) topHelp() string {
	dumpNote := "(disabled — set server.commands.allow_dump: true to enable)"
	if p.cfg.AllowDump {
		dumpNote = "write all traffic to dump.jsonl"
	}
	return fmt.Sprintf(`miroxy built-in commands (zero LLM tokens)
============================================================
  :miroxy stats              Uptime, model routing, keypool health
  Use /model in Claude Code or Codex to switch models via the native picker
  :miroxy dump on|off        %s
  :miroxy health             Quick health check
  :miroxy off                Disable all :miroxy commands (until restart or :miroxy on)
  :miroxy on                 Re-enable :miroxy commands
  :miroxy ?|help             Show this help
  :miroxy <cmd> ?|help       Show help for a specific command

Tip: append a question after any command to inject the result into the LLM.
  Example:  :miroxy stats is keypool utilization high?`, dumpNote)
}

func (p *CommandPlugin) cmdHelp(cmd string) string {
	helps := map[string]string{
		"stats": `  :miroxy stats
  :miroxy stats <question>   — get stats then ask the LLM <question>

Shows: uptime, in-flight count, model routing table, keypool health.`,

		"model": `  Model switching is available via /model in Claude Code and Codex.
  miroxy routes requests based on the model name sent by the client.

Model names come from model_routes in config. Use "show" to discover them.`,

		"dump": `  :miroxy dump on    — capture all requests/responses to dump.jsonl
  :miroxy dump off   — stop capturing

Requires server.commands.allow_dump: true in config (restart needed).`,

		"health": `  :miroxy health   — returns "healthy" when miroxy is running`,
	}
	if h, ok := helps[cmd]; ok {
		return h
	}
	return fmt.Sprintf("no help for %q — type :miroxy ? for a list of commands.", cmd)
}

// ── message helpers ───────────────────────────────────────────────────────────

// lastUserText returns the last text-type content block of the last user message.
// Claude Code injects system reminders as leading blocks; the user's actual input
// is always the final text block in the content array.
func lastUserText(req *types.MessageRequest) (string, bool) {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role != "user" {
			continue
		}
		if t, ok := m.TextContent(); ok {
			return strings.TrimSpace(t), true
		}
		if blocks, ok := m.BlockContent(); ok {
			for j := len(blocks) - 1; j >= 0; j-- {
				if blocks[j].Type == "text" {
					return strings.TrimSpace(blocks[j].Text), true
				}
			}
		}
		return "", false
	}
	return "", false
}

// replaceLastUserText updates only the last text block of the last user message,
// preserving all other blocks (system reminders, tool calls, etc.).
func replaceLastUserText(req *types.MessageRequest, text string) {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			req.Messages[i].SetLastBlockText(text)
			return
		}
	}
}

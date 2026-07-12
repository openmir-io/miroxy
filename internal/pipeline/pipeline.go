package pipeline

import (
	"fmt"
	"log/slog"
	"sort"
)

// Priority constants for standard pipeline positions.
// Lower value runs first (Auth=0 before Upstream=1000).
const (
	PriorityAuth      = 0
	PriorityObserve   = 100
	PriorityWarden    = 300
	PriorityRectifier = 400
	PriorityRouter    = 500
	PriorityTerminal  = 1000
)

// Handler is the continuation passed to Plugin.Execute.
// Calling it invokes the next plugin in the chain.
type Handler func(c *LLMContext) error

// Plugin is the core extension point. All request-path business logic runs as a Plugin.
// Implementations must be safe for concurrent use.
type Plugin interface {
	Name() string
	Priority() int
	Execute(c *LLMContext, next Handler) error
}

// Pipeline executes a sorted chain of plugins.
type Pipeline struct {
	plugins []Plugin
}

// New builds a Pipeline from plugins, sorted ascending by Priority().
func New(plugins []Plugin) *Pipeline {
	sorted := make([]Plugin, len(plugins))
	copy(sorted, plugins)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Priority() < sorted[j].Priority()
	})
	return &Pipeline{plugins: sorted}
}

// Run executes all plugins in priority order against c.
// A plugin that does not call next halts the chain at that position.
func (p *Pipeline) Run(c *LLMContext) error {
	return p.dispatch(c, 0)
}

func (p *Pipeline) dispatch(c *LLMContext, idx int) error {
	if idx >= len(p.plugins) {
		return nil
	}
	pl := p.plugins[idx]
	slog.Debug("pipeline: entering plugin", "plugin", pl.Name(), "priority", pl.Priority())
	err := pl.Execute(c, func(c *LLMContext) error {
		return p.dispatch(c, idx+1)
	})
	if err != nil {
		slog.Debug("pipeline: plugin failed", "plugin", pl.Name(), "error", err)
	}
	return err
}

// PipelineError carries an HTTP status code and Anthropic error type back to the
// delivery layer. Plugins return this to request a specific HTTP error response.
// When RawBody is set (invisible mode), the body is written verbatim and ErrType/Msg
// are ignored.
type PipelineError struct {
	Status      int
	ErrType     string
	Msg         string
	RawBody     []byte // non-nil: write verbatim, bypassing Anthropic error format
	ContentType string // MIME type for RawBody (defaults to application/json)
}

func (e *PipelineError) Error() string {
	return fmt.Sprintf("pipeline error %d (%s): %s", e.Status, e.ErrType, e.Msg)
}

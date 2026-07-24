package pipeline_test

import (
	"context"
	"errors"
	"testing"

	"miroxy/core/ir"
	"miroxy/core/router"
	"miroxy/internal/pipeline"
)

// stub plugin for testing
type stubPlugin struct {
	name     string
	priority int
	fn       func(c *pipeline.LLMContext, next pipeline.Handler) error
}

func (s *stubPlugin) Name() string  { return s.name }
func (s *stubPlugin) Priority() int { return s.priority }
func (s *stubPlugin) Execute(c *pipeline.LLMContext, next pipeline.Handler) error {
	return s.fn(c, next)
}

func newCtx() *pipeline.LLMContext {
	req := &ir.IRRequest{Gen: ir.IRGenerationConfig{MaxTokens: 100}}
	return pipeline.NewContext(context.Background(), req, "test", router.RouteTarget{})
}

// TestPipeline_OrdersByPriority verifies plugins run in ascending priority order
// regardless of the order they are passed to New().
func TestPipeline_OrdersByPriority(t *testing.T) {
	var order []string

	record := func(name string) func(c *pipeline.LLMContext, next pipeline.Handler) error {
		return func(c *pipeline.LLMContext, next pipeline.Handler) error {
			order = append(order, name)
			return next(c)
		}
	}

	// Intentionally pass plugins in reverse priority order.
	p := pipeline.New([]pipeline.Plugin{
		&stubPlugin{name: "c", priority: 300, fn: record("c")},
		&stubPlugin{name: "a", priority: 100, fn: record("a")},
		&stubPlugin{name: "b", priority: 200, fn: record("b")},
	})

	if err := p.Run(newCtx()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"a", "b", "c"}
	for i, got := range order {
		if got != want[i] {
			t.Errorf("position %d: got %q, want %q", i, got, want[i])
		}
	}
}

// TestPipeline_ShortCircuit verifies that a plugin that does not call next
// halts the chain — subsequent plugins are not executed.
func TestPipeline_ShortCircuit(t *testing.T) {
	reached := false

	p := pipeline.New([]pipeline.Plugin{
		&stubPlugin{name: "halt", priority: 10, fn: func(c *pipeline.LLMContext, _ pipeline.Handler) error {
			// deliberately does not call next
			return nil
		}},
		&stubPlugin{name: "after", priority: 20, fn: func(c *pipeline.LLMContext, next pipeline.Handler) error {
			reached = true
			return next(c)
		}},
	})

	if err := p.Run(newCtx()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reached {
		t.Error("plugin after halt should not have been called")
	}
}

// TestPipeline_ErrorPropagates verifies that an error returned by a plugin
// bubbles up from Run unchanged.
func TestPipeline_ErrorPropagates(t *testing.T) {
	sentinel := errors.New("plugin error")

	p := pipeline.New([]pipeline.Plugin{
		&stubPlugin{name: "fail", priority: 10, fn: func(c *pipeline.LLMContext, _ pipeline.Handler) error {
			return sentinel
		}},
	})

	if err := p.Run(newCtx()); !errors.Is(err, sentinel) {
		t.Errorf("want sentinel error, got %v", err)
	}
}

// TestPipeline_ZeroCopyMutation verifies that mutations made by a plugin to
// c.Values are visible to subsequent plugins without copying.
func TestPipeline_ZeroCopyMutation(t *testing.T) {
	var seenValue string

	p := pipeline.New([]pipeline.Plugin{
		&stubPlugin{name: "writer", priority: 10, fn: func(c *pipeline.LLMContext, next pipeline.Handler) error {
			c.Values["key"] = "hello"
			return next(c)
		}},
		&stubPlugin{name: "reader", priority: 20, fn: func(c *pipeline.LLMContext, next pipeline.Handler) error {
			if v, ok := c.Values["key"].(string); ok {
				seenValue = v
			}
			return next(c)
		}},
	})

	if err := p.Run(newCtx()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if seenValue != "hello" {
		t.Errorf("reader saw %q, want %q", seenValue, "hello")
	}
}

// TestPipeline_EmptyPipeline verifies Run on an empty pipeline is a no-op.
func TestPipeline_EmptyPipeline(t *testing.T) {
	p := pipeline.New(nil)
	if err := p.Run(newCtx()); err != nil {
		t.Fatalf("empty pipeline Run: %v", err)
	}
}

// TestPipelineError_IsError verifies PipelineError satisfies the error interface.
func TestPipelineError_IsError(t *testing.T) {
	pe := &pipeline.PipelineError{Status: 503, ErrType: "overloaded_error", Msg: "no keys"}
	if pe.Error() == "" {
		t.Error("PipelineError.Error() returned empty string")
	}
	var target *pipeline.PipelineError
	if !errors.As(pe, &target) {
		t.Error("errors.As should match *PipelineError")
	}
}

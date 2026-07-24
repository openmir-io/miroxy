package compress

import (
	"context"
	"testing"

	ccomp "miroxy/core/compress"
	"miroxy/core/ir"
	"miroxy/core/router"
	"miroxy/internal/pipeline"
)

type fakeCompressor struct {
	called bool
}

func (f *fakeCompressor) Compress(ctx context.Context, req *ccomp.Request) (*ccomp.Result, error) {
	f.called = true
	return &ccomp.Result{
		Messages: []ccomp.Message{
			{Role: "user", Parts: []ccomp.ContentPart{{Type: "text", Text: "summary"}}},
		},
		OriginalTokens:   9999,
		CompressedTokens: 10,
	}, nil
}

func newLLMContext(msgs []ir.IRMessage) *pipeline.LLMContext {
	req := &ir.IRRequest{Gen: ir.IRGenerationConfig{MaxTokens: 100}, Messages: msgs}
	return pipeline.NewContext(context.Background(), req, "test", router.RouteTarget{})
}

func manyUserMessages(n int, text string) []ir.IRMessage {
	out := make([]ir.IRMessage, n)
	for i := range out {
		out[i] = ir.IRMessage{Role: "user", Parts: []ir.IRContentPart{{Text: &ir.IRTextPart{Text: text}}}}
	}
	return out
}

// TestCompressPlugin_MarksRequestRewrittenWhenCompressed guards the fix for a
// bug where a passthrough-eligible attempt shipped the client's original,
// uncompressed RawRequestBody because nothing signaled that Request had been
// rewritten. RequestRewritten is what UpstreamExecutor checks before dispatch.
func TestCompressPlugin_MarksRequestRewrittenWhenCompressed(t *testing.T) {
	fc := &fakeCompressor{}
	p := NewCompressPlugin(fc, 10) // low threshold — this conversation exceeds it

	c := newLLMContext(manyUserMessages(5, "hello world, this is a decently sized message to push over threshold"))
	c.RawRequestBody = []byte(`{"model":"test"}`)

	called := false
	err := p.Execute(c, func(*pipeline.LLMContext) error { called = true; return nil })
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !called {
		t.Fatal("next was not called")
	}
	if !fc.called {
		t.Fatal("compressor was not invoked")
	}
	if !c.RequestRewritten {
		t.Fatal("expected RequestRewritten=true after compression")
	}
}

func TestCompressPlugin_LeavesRequestRewrittenFalseBelowThreshold(t *testing.T) {
	fc := &fakeCompressor{}
	p := NewCompressPlugin(fc, 1_000_000) // threshold never reached

	c := newLLMContext(manyUserMessages(1, "short"))

	if err := p.Execute(c, func(*pipeline.LLMContext) error { return nil }); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if fc.called {
		t.Fatal("compressor should not have been invoked below threshold")
	}
	if c.RequestRewritten {
		t.Fatal("expected RequestRewritten to stay false when compression did not run")
	}
}

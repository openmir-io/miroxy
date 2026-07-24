package server

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"miroxy/core/dispatch"
	"miroxy/core/ir"
	"miroxy/core/selector"
)

// probeSchedule defines the wait before each successive all-credentials probe attempt.
// The last entry repeats indefinitely.
var probeSchedule = []time.Duration{
	10 * time.Minute,
	1 * time.Hour,
	3 * time.Hour,
}

// probeCapable is satisfied by CredPool (type assertion, not on the Selector interface).
type probeCapable interface {
	TakeRateLimited(ctx context.Context) []*selector.ExecutionPlan
}

// keyProber probes all rate-limited credentials on an escalating schedule when the
// entire pool is down. One prober per model; trigger() is idempotent while running.
type keyProber struct {
	mu       sync.Mutex
	running  bool
	tierIdx  int
	cancelFn context.CancelFunc

	modelName  string
	sel        selector.Selector
	dispatcher dispatch.Dispatcher
}

func newKeyProber(modelName string, sel selector.Selector, disp dispatch.Dispatcher) *keyProber {
	return &keyProber{
		modelName:  modelName,
		sel:        sel,
		dispatcher: disp,
	}
}

// trigger starts the background probe loop if it is not already running.
// Safe to call on every 503 — subsequent calls are no-ops until the prober stops.
func (p *keyProber) trigger() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		return
	}
	p.running = true
	p.tierIdx = 0
	ctx, cancel := context.WithCancel(context.Background())
	p.cancelFn = cancel
	go p.run(ctx)
}

func (p *keyProber) run(ctx context.Context) {
	defer func() {
		p.mu.Lock()
		p.running = false
		p.mu.Unlock()
	}()

	for {
		p.mu.Lock()
		tier := p.tierIdx
		p.mu.Unlock()

		delay := probeSchedule[min(tier, len(probeSchedule)-1)]
		slog.Info("all credentials rate-limited, probe scheduled",
			"model", p.modelName, "wait", delay, "attempt", tier+1)

		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}

		if p.probeAll(ctx) {
			return
		}

		p.mu.Lock()
		if p.tierIdx < len(probeSchedule)-1 {
			p.tierIdx++
		}
		p.mu.Unlock()
	}
}

// probeAll probes every currently rate-limited credential and logs one line per credential.
// Returns true when at least one credential recovered (prober should stop).
func (p *keyProber) probeAll(ctx context.Context) bool {
	pc, ok := p.sel.(probeCapable)
	if !ok {
		return false
	}
	plans := pc.TakeRateLimited(ctx)
	if len(plans) == 0 {
		// All credentials already recovered naturally before the timer fired.
		return true
	}

	anySuccess := false
	for _, plan := range plans {
		err := p.probeKey(ctx, plan)
		if err == nil {
			slog.Info(p.modelName + " - " + plan.SelectionID + " retried: successful")
			p.sel.Release(plan, nil)
			anySuccess = true
		} else {
			slog.Warn(p.modelName + " - " + plan.SelectionID + " retried: failed (" + err.Error() + ")")
			p.sel.Release(plan, &selector.RateLimitError{})
		}
	}
	return anySuccess
}

// probeKey sends a minimal 1-token request to test whether the credential is usable.
func (p *keyProber) probeKey(ctx context.Context, plan *selector.ExecutionPlan) error {
	req := &ir.IRRequest{
		Messages: []ir.IRMessage{{Role: "user", Parts: []ir.IRContentPart{{Text: &ir.IRTextPart{Text: "hi"}}}}},
		Gen:      ir.IRGenerationConfig{MaxTokens: 1},
	}
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	upstreamReq, err := plan.Upstream.ToUpstream(reqCtx, req, plan.Credential)
	if err != nil {
		return err
	}
	resp, err := p.dispatcher.Do(reqCtx, upstreamReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 429 {
		return fmt.Errorf("still rate-limited (429)")
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("upstream %d", resp.StatusCode)
	}
	return nil
}

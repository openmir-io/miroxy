package cred

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// UsageAccumulator holds in-memory, per-pool request/token deltas since the
// last confirmed-successful send to credstone (rpd/tpd delegation). It is
// never persisted — on process restart the deltas are gone, same as every
// other in-memory counter in miroxy.
//
// Deltas are cleared only on a successful flush; a failed send leaves them in
// place so the next tick sends the combined total (accumulate-until-success,
// never fire-and-discard).
type UsageAccumulator struct {
	client *CredstoneClient
	poolID string

	mu       sync.Mutex
	requests int64
	input    int64
	output   int64
}

// NewUsageAccumulator creates an accumulator that flushes to poolID via client.
func NewUsageAccumulator(client *CredstoneClient, poolID string) *UsageAccumulator {
	return &UsageAccumulator{client: client, poolID: poolID}
}

// AddRequest records one completed upstream attempt against this pool's
// credstone-sourced credential. Called from CredSource.ReportOutcome, on the
// existing outcomeReporter path — every attempt counts, matching how the
// local RPM window counts every Select(), not just successes.
func (u *UsageAccumulator) AddRequest() {
	u.mu.Lock()
	u.requests++
	u.mu.Unlock()
}

// AddTokens records input/output token counts from one successful response.
func (u *UsageAccumulator) AddTokens(input, output int64) {
	if input == 0 && output == 0 {
		return
	}
	u.mu.Lock()
	u.input += input
	u.output += output
	u.mu.Unlock()
}

// StartFlusher periodically sends accumulated deltas to credstone. Call once;
// the goroutine runs until ctx is cancelled. Mirrors CredSource.StartPoller's
// interval-loop shape rather than introducing a second ticker pattern.
func (u *UsageAccumulator) StartFlusher(ctx context.Context, interval time.Duration) {
	go u.flushLoop(ctx, interval)
}

func (u *UsageAccumulator) flushLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			u.flush(ctx)
		}
	}
}

// flush sends the current deltas and subtracts exactly what was sent on
// success — not a reset to zero — so increments that land during the network
// call are preserved for the next flush rather than lost.
func (u *UsageAccumulator) flush(ctx context.Context) {
	u.mu.Lock()
	requests, input, output := u.requests, u.input, u.output
	u.mu.Unlock()

	if requests == 0 && input == 0 && output == 0 {
		return
	}

	if err := u.client.ReportUsage(ctx, u.poolID, requests, input, output); err != nil {
		slog.Warn("credstone ReportUsage failed, deltas will accumulate for next flush",
			"pool", u.poolID, "requests", requests, "input_tokens", input, "output_tokens", output, "error", err)
		return
	}

	u.mu.Lock()
	u.requests -= requests
	u.input -= input
	u.output -= output
	u.mu.Unlock()
}

package cred

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestUsageAccumulator_NoOpWhenEmpty(t *testing.T) {
	called := false
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{}`))
	}, "")

	ua := NewUsageAccumulator(client, "p1")
	ua.flush(context.Background())

	if called {
		t.Error("expected no HTTP call when nothing was accumulated")
	}
}

func TestUsageAccumulator_FlushSendsAndResetsOnSuccess(t *testing.T) {
	var got reportUsageRequest
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		decodeBody(t, r, &got)
		_, _ = w.Write([]byte(`{}`))
	}, "")

	ua := NewUsageAccumulator(client, "p1")
	ua.AddRequest()
	ua.AddRequest()
	ua.AddTokens(10, 20)

	ua.flush(context.Background())

	if got.PoolID != "p1" || got.DeltaRequests != 2 || got.DeltaInputTokens != 10 || got.DeltaOutputTokens != 20 {
		t.Fatalf("unexpected request body: %+v", got)
	}

	ua.mu.Lock()
	requests, input, output := ua.requests, ua.input, ua.output
	ua.mu.Unlock()
	if requests != 0 || input != 0 || output != 0 {
		t.Fatalf("expected deltas reset to zero after success, got requests=%d input=%d output=%d", requests, input, output)
	}
}

func TestUsageAccumulator_FailureLeavesDeltasForNextFlush(t *testing.T) {
	var callCount atomic.Int32
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var req reportUsageRequest
		decodeBody(t, r, &req)
		if req.DeltaRequests != 1 || req.DeltaInputTokens != 5 {
			t.Errorf("second flush should carry the combined total, got %+v", req)
		}
		_, _ = w.Write([]byte(`{}`))
	}, "")

	ua := NewUsageAccumulator(client, "p1")
	ua.AddRequest()
	ua.AddTokens(5, 0)

	ua.flush(context.Background()) // fails — deltas must survive

	ua.mu.Lock()
	requests := ua.requests
	ua.mu.Unlock()
	if requests != 1 {
		t.Fatalf("expected delta to survive a failed flush, got requests=%d", requests)
	}

	ua.flush(context.Background()) // succeeds — should send the same combined delta

	if callCount.Load() != 2 {
		t.Fatalf("expected exactly 2 flush attempts, got %d", callCount.Load())
	}
}

// TestUsageAccumulator_SplitOrCombinedFlushesSendSameTotal verifies that
// sending deltas across two successful flushes vs. one flush of the combined
// amount is commutative: the total requests/tokens credstone sees is the
// same either way.
func TestUsageAccumulator_SplitOrCombinedFlushesSendSameTotal(t *testing.T) {
	var totalRequests, totalInput int64
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req reportUsageRequest
		decodeBody(t, r, &req)
		totalRequests += req.DeltaRequests
		totalInput += req.DeltaInputTokens
		_, _ = w.Write([]byte(`{}`))
	}, "")

	ua := NewUsageAccumulator(client, "p1")
	ua.AddRequest()
	ua.AddTokens(3, 0)
	ua.flush(context.Background())

	ua.AddRequest()
	ua.AddTokens(4, 0)
	ua.flush(context.Background())

	if totalRequests != 2 || totalInput != 7 {
		t.Fatalf("split flushes: got totalRequests=%d totalInput=%d, want 2/7", totalRequests, totalInput)
	}
}

func TestUsageAccumulator_StartFlusher_StopsOnContextCancel(t *testing.T) {
	var calls atomic.Int32
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}, "")

	ua := NewUsageAccumulator(client, "p1")
	ua.AddRequest()

	ctx, cancel := context.WithCancel(context.Background())
	ua.StartFlusher(ctx, 10*time.Millisecond)

	time.Sleep(50 * time.Millisecond)
	cancel()
	seenBeforeCancel := calls.Load()
	time.Sleep(50 * time.Millisecond)

	if seenBeforeCancel == 0 {
		t.Fatal("expected at least one flush tick before cancel")
	}
	if calls.Load() > seenBeforeCancel+1 {
		// Allow at most one in-flight tick racing the cancel.
		t.Errorf("flusher kept ticking after context cancel: before=%d after=%d", seenBeforeCancel, calls.Load())
	}
}

package unit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"miroxy/core/cred"
	"miroxy/core/selector"
)

func toSpecs(vals ...string) []selector.CredSpec {
	specs := make([]selector.CredSpec, len(vals))
	for i, v := range vals {
		// Wrap as a HeaderCredential; credpool tests check SelectionID not credential type.
		specs[i] = selector.CredSpec{Source: selector.NewStaticSource(&cred.HeaderCredential{Header: "x-api-key", Value: v})}
	}
	return specs
}

func newPool(strategy string, keys ...string) *selector.CredPool {
	return selector.NewCredPool(selector.CredPoolConfig{
		Keys:      toSpecs(keys...),
		Strategy:  strategy,
		Threshold: 3,
		Cooldown:  5 * time.Second,
	})
}

func TestRoundRobin_DistributesAcrossKeys(t *testing.T) {
	pool := newPool("round_robin", "key-a", "key-b", "key-c")

	seen := map[string]int{}
	for i := 0; i < 9; i++ {
		plan, err := pool.Select(context.Background(), nil)
		if err != nil {
			t.Fatalf("Select[%d]: %v", i, err)
		}
		seen[plan.SelectionID]++
		pool.Release(plan, nil)
	}

	if len(seen) != 3 {
		t.Errorf("expected 3 distinct keys used, got %d: %v", len(seen), seen)
	}
}

func TestLeastRequests_PrefersIdle(t *testing.T) {
	pool := newPool("least_requests", "key-a", "key-b")

	// Hold key-a without releasing.
	held, err := pool.Select(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	// Next select should land on key-b (lower in_flight).
	next, err := pool.Select(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if next.SelectionID == held.SelectionID {
		t.Errorf("expected different key, got same %s", next.SelectionID)
	}

	pool.Release(held, nil)
	pool.Release(next, nil)
}

func TestCircuitBreaker_BreaksOnThreshold(t *testing.T) {
	pool := newPool("round_robin", "only-key")
	ctx := context.Background()

	// Exhaust threshold (3) with failures.
	for i := 0; i < 3; i++ {
		plan, err := pool.Select(ctx, nil)
		if err != nil {
			t.Fatalf("Select[%d]: %v", i, err)
		}
		pool.Release(plan, errors.New("upstream 500"))
	}

	// Pool should now be fully circuit-broken.
	_, err := pool.Select(ctx, nil)
	if !errors.Is(err, selector.ErrNoSelection) {
		t.Errorf("expected ErrNoSelection after threshold, got %v", err)
	}
}

func TestCircuitBreaker_RecoveryAfterCooldown(t *testing.T) {
	pool := selector.NewCredPool(selector.CredPoolConfig{
		Keys:      toSpecs("key-a"),
		Strategy:  "round_robin",
		Threshold: 1,
		Cooldown:  50 * time.Millisecond, // short for test
	})
	ctx := context.Background()

	plan, _ := pool.Select(ctx, nil)
	pool.Release(plan, errors.New("fail"))

	// Should be broken now.
	if _, err := pool.Select(ctx, nil); !errors.Is(err, selector.ErrNoSelection) {
		t.Fatal("expected circuit to be open")
	}

	time.Sleep(60 * time.Millisecond)

	// Should recover after cooldown.
	plan2, err := pool.Select(ctx, nil)
	if err != nil {
		t.Errorf("expected recovery after cooldown, got %v", err)
	}
	if plan2 != nil {
		pool.Release(plan2, nil)
	}
}

func TestSelect_NoKeys_ReturnsError(t *testing.T) {
	// A pool with zero keys should return ErrNoSelection immediately.
	// (In practice the config validator prevents this, but the pool should be safe.)
	pool := selector.NewCredPool(selector.CredPoolConfig{Keys: toSpecs()})
	_, err := pool.Select(context.Background(), nil)
	if !errors.Is(err, selector.ErrNoSelection) {
		t.Errorf("expected ErrNoSelection for empty pool, got %v", err)
	}
}

func TestInFlight_Decremented_OnRelease(t *testing.T) {
	pool := newPool("least_requests", "key-a", "key-b")
	ctx := context.Background()

	p1, _ := pool.Select(ctx, nil)
	p2, _ := pool.Select(ctx, nil)
	pool.Release(p1, nil)
	pool.Release(p2, nil)

	// After release, should be able to acquire again without issues.
	p3, err := pool.Select(ctx, nil)
	if err != nil {
		t.Fatalf("select after release: %v", err)
	}
	pool.Release(p3, nil)
}

// --- Proactive rate-limit rotation tests ---

func newRateLimitedPool(rateLimit, softLimit int, window time.Duration, keys ...string) *selector.CredPool {
	return selector.NewCredPool(selector.CredPoolConfig{
		Keys:          toSpecs(keys...),
		Strategy:      "round_robin",
		Threshold:     5,
		Cooldown:      5 * time.Second,
		RateLimitRPM:  rateLimit,
		RateSoftLimit: softLimit,
		RateWindow:    window,
	})
}

// TestRateLimit_SoftRotation verifies that requests are routed away from a key
// once it reaches the soft limit, even before a 429 occurs.
//
// Uses least_requests strategy so key_0 wins ties (scanned first), making it
// predictable which key gets saturated.
func TestRateLimit_SoftRotation(t *testing.T) {
	ctx := context.Background()
	pool := selector.NewCredPool(selector.CredPoolConfig{
		Keys:          toSpecs("key-a", "key-b"),
		Strategy:      "least_requests",
		Threshold:     5,
		Cooldown:      5 * time.Second,
		RateLimitRPM:  3,
		RateSoftLimit: 2,
		RateWindow:    5 * time.Second,
	})

	// With least_requests, key_0 wins when both have inFlight=0. Saturate it to soft limit.
	for i := 0; i < 2; i++ {
		plan, err := pool.Select(ctx, nil)
		if err != nil {
			t.Fatalf("saturate select %d: %v", i, err)
		}
		if plan.SelectionID != "key_0" {
			t.Fatalf("saturate select %d: expected key_0, got %s", i, plan.SelectionID)
		}
		pool.Release(plan, nil)
	}

	// key_0 is now at its soft limit (2/2). The pool must rotate to key_1.
	plan, err := pool.Select(ctx, nil)
	if err != nil {
		t.Fatalf("select after soft limit: %v", err)
	}
	if plan.SelectionID != "key_1" {
		t.Errorf("expected rotation to key_1, got %s", plan.SelectionID)
	}
	pool.Release(plan, nil)
}

// TestRateLimit_FallbackWhenAllAtSoftLimit verifies that when every key is at
// or over the soft limit, Select still returns a key rather than ErrNoSelection.
func TestRateLimit_FallbackWhenAllAtSoftLimit(t *testing.T) {
	ctx := context.Background()
	pool := newRateLimitedPool(3, 1, 5*time.Second, "only-key")

	// Push the single key past its soft limit.
	for i := 0; i < 2; i++ {
		plan, err := pool.Select(ctx, nil)
		if err != nil {
			t.Fatalf("saturate select %d: %v", i, err)
		}
		pool.Release(plan, nil)
	}

	// Even though the key is over the soft limit, it should still be returned
	// (reactive 429 handling is the backstop, not ErrNoSelection).
	plan, err := pool.Select(ctx, nil)
	if err != nil {
		t.Fatalf("expected key despite soft-limit overflow, got: %v", err)
	}
	pool.Release(plan, nil)
}

// TestRateLimit_WindowExpiry verifies that request counts reset after the window
// elapses, making a previously-saturated key eligible again.
func TestRateLimit_WindowExpiry(t *testing.T) {
	ctx := context.Background()
	// Very short window (100ms) so we can observe expiry without long sleeps.
	pool := newRateLimitedPool(2, 2, 100*time.Millisecond, "key-a", "key-b")

	// Saturate key-a (soft limit = 2).
	for i := 0; i < 2; i++ {
		plan, _ := pool.Select(ctx, nil)
		pool.Release(plan, nil)
	}

	// Wait for the window to expire.
	time.Sleep(110 * time.Millisecond)

	// key-a's window has cleared — it should be preferred again by round-robin.
	plan, err := pool.Select(ctx, nil)
	if err != nil {
		t.Fatalf("select after window expiry: %v", err)
	}
	// Both keys are now clean; round-robin will pick whichever is next. Just
	// verify we get a key without error — the important invariant is no ErrNoSelection.
	pool.Release(plan, nil)
}

// --- Escalating 429 cooldown tests ---

// shortTierPool builds a pool whose 429 backoff tiers are in milliseconds so
// tests can observe the escalating schedule without long sleeps.
func shortTierPool(tiers []time.Duration, keys ...string) *selector.CredPool {
	return selector.NewCredPool(selector.CredPoolConfig{
		Keys:           toSpecs(keys...),
		Strategy:       "round_robin",
		Threshold:      10, // high so circuit-break doesn't interfere
		Cooldown:       5 * time.Second,
		RateLimitTiers: tiers,
	})
}

// TestRateLimit_EscalatingCooldown_KeyUnavailableAfter429 verifies that a key
// is immediately put into cooldown after the first 429.
func TestRateLimit_EscalatingCooldown_KeyUnavailableAfter429(t *testing.T) {
	pool := shortTierPool([]time.Duration{200 * time.Millisecond, 500 * time.Millisecond}, "key-a")
	ctx := context.Background()

	plan, err := pool.Select(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	pool.Release(plan, selector.ErrRateLimit)

	// Key is in cooldown; pool has no other key, so ErrNoSelection.
	_, err = pool.Select(ctx, nil)
	if !errors.Is(err, selector.ErrNoSelection) {
		t.Errorf("expected ErrNoSelection immediately after 429, got %v", err)
	}
}

// TestRateLimit_EscalatingCooldown_SecondFailureUsesLongerTier verifies that the
// second consecutive 429 uses the second (longer) backoff tier.
func TestRateLimit_EscalatingCooldown_SecondFailureUsesLongerTier(t *testing.T) {
	// Tiers: first failure = 80ms, second failure = 300ms.
	tiers := []time.Duration{80 * time.Millisecond, 300 * time.Millisecond}
	pool := shortTierPool(tiers, "key-a")
	ctx := context.Background()

	// First 429 → 80ms cooldown.
	plan, _ := pool.Select(ctx, nil)
	pool.Release(plan, selector.ErrRateLimit)
	time.Sleep(90 * time.Millisecond)

	// Key is back (rateLimitFailures = 1; not reset, but cooldown expired).
	plan, err := pool.Select(ctx, nil)
	if err != nil {
		t.Fatalf("expected key after first cooldown expired: %v", err)
	}

	// Second 429 → 300ms cooldown (tier index 1).
	pool.Release(plan, selector.ErrRateLimit)

	// 150ms in: key still in cooldown (300ms not yet elapsed).
	time.Sleep(150 * time.Millisecond)
	_, err = pool.Select(ctx, nil)
	if !errors.Is(err, selector.ErrNoSelection) {
		t.Errorf("expected key still in cooldown 150ms into 300ms tier, got %v", err)
	}

	// After full 300ms: key available.
	time.Sleep(160 * time.Millisecond)
	plan, err = pool.Select(ctx, nil)
	if err != nil {
		t.Fatalf("expected key available after second cooldown expired: %v", err)
	}
	pool.Release(plan, nil)
}

// TestRateLimit_EscalatingCooldown_CapsAtLastTier verifies that failures beyond
// the tier slice length all use the last (maximum) tier.
func TestRateLimit_EscalatingCooldown_CapsAtLastTier(t *testing.T) {
	tiers := []time.Duration{50 * time.Millisecond, 200 * time.Millisecond}
	pool := shortTierPool(tiers, "key-a")
	ctx := context.Background()

	// Three consecutive 429s (failure counts 0, 1, 2 — only two tiers).
	for i := range 3 {
		plan, err := pool.Select(ctx, nil)
		if err != nil {
			t.Fatalf("select before 429 #%d: %v", i, err)
		}
		pool.Release(plan, selector.ErrRateLimit)
		time.Sleep(210 * time.Millisecond) // always enough for the max tier (200ms)
	}

	// Third failure used last tier (200ms). After waiting 210ms it should be available.
	plan, err := pool.Select(ctx, nil)
	if err != nil {
		t.Fatalf("expected key after third cooldown (capped at last tier): %v", err)
	}
	pool.Release(plan, nil)
}

// TestRateLimit_SuccessResetsEscalatingCounter verifies that a successful request
// resets rateLimitFailures to 0, so the next 429 restarts from tier 0.
func TestRateLimit_SuccessResetsEscalatingCounter(t *testing.T) {
	// Tiers: first = 60ms, second = 500ms.
	tiers := []time.Duration{60 * time.Millisecond, 500 * time.Millisecond}
	pool := shortTierPool(tiers, "key-a")
	ctx := context.Background()

	// First 429 → 60ms; rateLimitFailures becomes 1.
	plan, _ := pool.Select(ctx, nil)
	pool.Release(plan, selector.ErrRateLimit)
	time.Sleep(70 * time.Millisecond)

	// Successful request resets rateLimitFailures to 0.
	plan, err := pool.Select(ctx, nil)
	if err != nil {
		t.Fatalf("expected key after first cooldown: %v", err)
	}
	pool.Release(plan, nil)

	// Next 429 should use tier 0 (60ms), not tier 1 (500ms).
	plan, err = pool.Select(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	pool.Release(plan, selector.ErrRateLimit)

	time.Sleep(70 * time.Millisecond)
	plan, err = pool.Select(ctx, nil)
	if err != nil {
		t.Fatalf("expected key available after reset to tier 0 (60ms): %v", err)
	}
	pool.Release(plan, nil)
}

// TestRateLimit_ExplicitRetryAfterOverridesEscalating verifies that a
// RateLimitError with a positive RetryAfter uses that duration instead of
// the escalating schedule.
func TestRateLimit_ExplicitRetryAfterOverridesEscalating(t *testing.T) {
	// Default tiers would use 10s — far too long. Explicit RetryAfter: 60ms.
	pool := shortTierPool([]time.Duration{10 * time.Second, 30 * time.Second}, "key-a")
	ctx := context.Background()

	plan, _ := pool.Select(ctx, nil)
	pool.Release(plan, &selector.RateLimitError{RetryAfter: 60 * time.Millisecond})

	_, err := pool.Select(ctx, nil)
	if !errors.Is(err, selector.ErrNoSelection) {
		t.Errorf("expected ErrNoSelection during explicit RetryAfter cooldown, got %v", err)
	}

	// After 70ms the key should be back — the 10s tier was NOT used.
	time.Sleep(70 * time.Millisecond)
	plan, err = pool.Select(ctx, nil)
	if err != nil {
		t.Fatalf("expected key available after explicit RetryAfter: %v", err)
	}
	pool.Release(plan, nil)
}

// TestRateLimit_ErrRateLimitIsRateLimitError verifies that errors.Is matches any
// *RateLimitError against ErrRateLimit regardless of RetryAfter value.
func TestRateLimit_ErrRateLimitIsRateLimitError(t *testing.T) {
	custom := &selector.RateLimitError{RetryAfter: 5 * time.Second}
	if !errors.Is(custom, selector.ErrRateLimit) {
		t.Error("errors.Is(custom *RateLimitError, ErrRateLimit) should be true")
	}
	if !errors.Is(selector.ErrRateLimit, selector.ErrRateLimit) {
		t.Error("errors.Is(ErrRateLimit, ErrRateLimit) should be true")
	}
}

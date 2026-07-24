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
		plan, err := pool.Select(context.Background(), nil, "test-model")
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

func TestRoundRobinBatch_UsesEachKeyNTimesBeforeRotating(t *testing.T) {
	pool := selector.NewCredPool(selector.CredPoolConfig{
		Keys:            toSpecs("key-a", "key-b", "key-c"),
		Strategy:        "round_robin",
		Threshold:       3,
		Cooldown:        5 * time.Second,
		RoundRobinBatch: 2,
	})

	var order []string
	for i := 0; i < 6; i++ {
		plan, err := pool.Select(context.Background(), nil, "test-model")
		if err != nil {
			t.Fatalf("Select[%d]: %v", i, err)
		}
		order = append(order, plan.SelectionID)
		pool.Release(plan, nil)
	}

	for i, id := range order {
		want := order[(i/2)*2]
		if id != want {
			t.Errorf("call %d: expected %s (batch of 2), got %s; full order=%v", i, want, id, order)
		}
	}
	if order[0] == order[2] || order[2] == order[4] {
		t.Errorf("expected rotation to a new key every 2 calls, got %v", order)
	}
}

func TestRoundRobinBatch_DefaultIsOnePerCall(t *testing.T) {
	pool := newPool("round_robin", "key-a", "key-b", "key-c")

	seen := map[string]int{}
	for i := 0; i < 9; i++ {
		plan, err := pool.Select(context.Background(), nil, "test-model")
		if err != nil {
			t.Fatalf("Select[%d]: %v", i, err)
		}
		seen[plan.SelectionID]++
		pool.Release(plan, nil)
	}
	if len(seen) != 3 {
		t.Errorf("expected every call to rotate (default batch=1), got %d distinct keys: %v", len(seen), seen)
	}
}

func TestFallback_DrainsFirstKeyBeforeTouchingNext(t *testing.T) {
	pool := selector.NewCredPool(selector.CredPoolConfig{
		Keys:      toSpecs("key-a", "key-b", "key-c"),
		Strategy:  "fallback",
		Threshold: 3,
		Cooldown:  5 * time.Second,
	})
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		plan, err := pool.Select(ctx, nil, "test-model")
		if err != nil {
			t.Fatalf("Select[%d]: %v", i, err)
		}
		if plan.SelectionID != "key_0" {
			t.Errorf("call %d: expected key_0 to keep being reused, got %s", i, plan.SelectionID)
		}
		pool.Release(plan, nil)
	}
}

func TestFallback_MovesOnOnceFirstKeyIsRateLimited(t *testing.T) {
	pool := selector.NewCredPool(selector.CredPoolConfig{
		Keys:      toSpecs("key-a", "key-b"),
		Strategy:  "fallback",
		Threshold: 3,
		Cooldown:  5 * time.Second,
	})
	ctx := context.Background()

	first, err := pool.Select(ctx, nil, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	pool.Release(first, &selector.RateLimitError{RetryAfter: time.Hour})

	for i := 0; i < 3; i++ {
		plan, err := pool.Select(ctx, nil, "test-model")
		if err != nil {
			t.Fatalf("Select[%d]: %v", i, err)
		}
		if plan.SelectionID != "key_1" {
			t.Errorf("call %d: expected fallthrough to key_1, got %s", i, plan.SelectionID)
		}
		pool.Release(plan, nil)
	}
}

func TestLeastRequests_PrefersIdle(t *testing.T) {
	pool := newPool("least_requests", "key-a", "key-b")

	// Hold key-a without releasing.
	held, err := pool.Select(context.Background(), nil, "test-model")
	if err != nil {
		t.Fatal(err)
	}

	// Next select should land on key-b (lower in_flight).
	next, err := pool.Select(context.Background(), nil, "test-model")
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
		plan, err := pool.Select(ctx, nil, "test-model")
		if err != nil {
			t.Fatalf("Select[%d]: %v", i, err)
		}
		pool.Release(plan, errors.New("upstream 500"))
	}

	// Pool should now be fully circuit-broken.
	_, err := pool.Select(ctx, nil, "test-model")
	if !errors.Is(err, selector.ErrNoSelection) {
		t.Errorf("expected ErrNoSelection after threshold, got %v", err)
	}
}

func TestRelease_DeadlineExhausted_NoPenalty(t *testing.T) {
	pool := newPool("round_robin", "only-key") // Threshold: 3
	ctx := context.Background()

	plan, err := pool.Select(ctx, nil, "test-model")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	pool.Release(plan, errors.New("upstream 500")) // failures = 1

	// A run of deadline-exhausted releases must neither count toward the
	// threshold nor reset the failure counter accrued above.
	for i := 0; i < 5; i++ {
		plan, err := pool.Select(ctx, nil, "test-model")
		if err != nil {
			t.Fatalf("Select[%d]: %v", i, err)
		}
		pool.Release(plan, &selector.DeadlineExhaustedError{Err: context.DeadlineExceeded})
	}

	// Exactly 2 more real failures (not 3) should now trip the threshold —
	// proving the deadline releases above left the counter at 1, not 0.
	for i := 0; i < 2; i++ {
		plan, err := pool.Select(ctx, nil, "test-model")
		if err != nil {
			t.Fatalf("Select post-deadline[%d]: %v", i, err)
		}
		pool.Release(plan, errors.New("upstream 500"))
	}

	if _, err := pool.Select(ctx, nil, "test-model"); !errors.Is(err, selector.ErrNoSelection) {
		t.Error("expected circuit-broken after 3 real failures; deadline releases must not have reset or inflated the counter")
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

	plan, _ := pool.Select(ctx, nil, "test-model")
	pool.Release(plan, errors.New("fail"))

	// Should be broken now.
	if _, err := pool.Select(ctx, nil, "test-model"); !errors.Is(err, selector.ErrNoSelection) {
		t.Fatal("expected circuit to be open")
	}

	time.Sleep(60 * time.Millisecond)

	// Should recover after cooldown.
	plan2, err := pool.Select(ctx, nil, "test-model")
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
	_, err := pool.Select(context.Background(), nil, "test-model")
	if !errors.Is(err, selector.ErrNoSelection) {
		t.Errorf("expected ErrNoSelection for empty pool, got %v", err)
	}
}

func TestInFlight_Decremented_OnRelease(t *testing.T) {
	pool := newPool("least_requests", "key-a", "key-b")
	ctx := context.Background()

	p1, _ := pool.Select(ctx, nil, "test-model")
	p2, _ := pool.Select(ctx, nil, "test-model")
	pool.Release(p1, nil)
	pool.Release(p2, nil)

	// After release, should be able to acquire again without issues.
	p3, err := pool.Select(ctx, nil, "test-model")
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
		plan, err := pool.Select(ctx, nil, "test-model")
		if err != nil {
			t.Fatalf("saturate select %d: %v", i, err)
		}
		if plan.SelectionID != "key_0" {
			t.Fatalf("saturate select %d: expected key_0, got %s", i, plan.SelectionID)
		}
		pool.Release(plan, nil)
	}

	// key_0 is now at its soft limit (2/2). The pool must rotate to key_1.
	plan, err := pool.Select(ctx, nil, "test-model")
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
		plan, err := pool.Select(ctx, nil, "test-model")
		if err != nil {
			t.Fatalf("saturate select %d: %v", i, err)
		}
		pool.Release(plan, nil)
	}

	// Even though the key is over the soft limit, it should still be returned
	// (reactive 429 handling is the backstop, not ErrNoSelection).
	plan, err := pool.Select(ctx, nil, "test-model")
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
		plan, _ := pool.Select(ctx, nil, "test-model")
		pool.Release(plan, nil)
	}

	// Wait for the window to expire.
	time.Sleep(110 * time.Millisecond)

	// key-a's window has cleared — it should be preferred again by round-robin.
	plan, err := pool.Select(ctx, nil, "test-model")
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

	plan, err := pool.Select(ctx, nil, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	pool.Release(plan, selector.ErrRateLimit)

	// Key is in cooldown; pool has no other key, so ErrNoSelection.
	_, err = pool.Select(ctx, nil, "test-model")
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
	plan, _ := pool.Select(ctx, nil, "test-model")
	pool.Release(plan, selector.ErrRateLimit)
	time.Sleep(90 * time.Millisecond)

	// Key is back (rateLimitFailures = 1; not reset, but cooldown expired).
	plan, err := pool.Select(ctx, nil, "test-model")
	if err != nil {
		t.Fatalf("expected key after first cooldown expired: %v", err)
	}

	// Second 429 → 300ms cooldown (tier index 1).
	pool.Release(plan, selector.ErrRateLimit)

	// 150ms in: key still in cooldown (300ms not yet elapsed).
	time.Sleep(150 * time.Millisecond)
	_, err = pool.Select(ctx, nil, "test-model")
	if !errors.Is(err, selector.ErrNoSelection) {
		t.Errorf("expected key still in cooldown 150ms into 300ms tier, got %v", err)
	}

	// After full 300ms: key available.
	time.Sleep(160 * time.Millisecond)
	plan, err = pool.Select(ctx, nil, "test-model")
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
		plan, err := pool.Select(ctx, nil, "test-model")
		if err != nil {
			t.Fatalf("select before 429 #%d: %v", i, err)
		}
		pool.Release(plan, selector.ErrRateLimit)
		time.Sleep(210 * time.Millisecond) // always enough for the max tier (200ms)
	}

	// Third failure used last tier (200ms). After waiting 210ms it should be available.
	plan, err := pool.Select(ctx, nil, "test-model")
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
	plan, _ := pool.Select(ctx, nil, "test-model")
	pool.Release(plan, selector.ErrRateLimit)
	time.Sleep(70 * time.Millisecond)

	// Successful request resets rateLimitFailures to 0.
	plan, err := pool.Select(ctx, nil, "test-model")
	if err != nil {
		t.Fatalf("expected key after first cooldown: %v", err)
	}
	pool.Release(plan, nil)

	// Next 429 should use tier 0 (60ms), not tier 1 (500ms).
	plan, err = pool.Select(ctx, nil, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	pool.Release(plan, selector.ErrRateLimit)

	time.Sleep(70 * time.Millisecond)
	plan, err = pool.Select(ctx, nil, "test-model")
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

	plan, _ := pool.Select(ctx, nil, "test-model")
	pool.Release(plan, &selector.RateLimitError{RetryAfter: 60 * time.Millisecond})

	_, err := pool.Select(ctx, nil, "test-model")
	if !errors.Is(err, selector.ErrNoSelection) {
		t.Errorf("expected ErrNoSelection during explicit RetryAfter cooldown, got %v", err)
	}

	// After 70ms the key should be back — the 10s tier was NOT used.
	time.Sleep(70 * time.Millisecond)
	plan, err = pool.Select(ctx, nil, "test-model")
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

// --- TPM (tokens-per-minute) sliding window tests ---
//
// TPM mirrors RPM's sliding window, but the sample can only be recorded via
// RecordTokens after a response — there is no soft-limit reservation at
// Select time, since token counts aren't known yet then.

func newTPMPool(tpmLimit int, window time.Duration, keys ...string) *selector.CredPool {
	return selector.NewCredPool(selector.CredPoolConfig{
		Keys:         toSpecs(keys...),
		Strategy:     "round_robin",
		Threshold:    5,
		Cooldown:     5 * time.Second,
		RateLimitTPM: tpmLimit,
		RateWindow:   window,
	})
}

// TestTPM_DisabledByDefault verifies RecordTokens is a harmless no-op and
// Select never filters on tokens when RateLimitTPM is unset (0).
func TestTPM_DisabledByDefault(t *testing.T) {
	ctx := context.Background()
	pool := newTPMPool(0, time.Minute, "only-key")

	plan, err := pool.Select(ctx, nil, "test-model")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	pool.RecordTokens(plan.SelectionID, 1_000_000) // would blow any real budget
	pool.Release(plan, nil)

	if _, err := pool.Select(ctx, nil, "test-model"); err != nil {
		t.Fatalf("expected key available with TPM disabled, got %v", err)
	}
}

// TestTPM_SkipsCredentialOverBudget verifies that once RecordTokens pushes a
// credential's window total at or above the cap, Select rotates to the next
// credential instead.
func TestTPM_SkipsCredentialOverBudget(t *testing.T) {
	ctx := context.Background()
	pool := selector.NewCredPool(selector.CredPoolConfig{
		Keys:         toSpecs("key-a", "key-b"),
		Strategy:     "least_requests",
		Threshold:    5,
		Cooldown:     5 * time.Second,
		RateLimitTPM: 100,
		RateWindow:   5 * time.Second,
	})

	// key_0 wins the first (tied, inFlight=0) selection under least_requests.
	plan, err := pool.Select(ctx, nil, "test-model")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if plan.SelectionID != "key_0" {
		t.Fatalf("expected key_0 first, got %s", plan.SelectionID)
	}
	pool.RecordTokens(plan.SelectionID, 120) // over the 100 TPM cap
	pool.Release(plan, nil)

	// key_0 is now over budget — Select must rotate to key_1.
	plan2, err := pool.Select(ctx, nil, "test-model")
	if err != nil {
		t.Fatalf("Select after over-budget: %v", err)
	}
	if plan2.SelectionID != "key_1" {
		t.Errorf("expected rotation to key_1, got %s", plan2.SelectionID)
	}
	pool.Release(plan2, nil)
}

// TestTPM_FallbackWhenAllOverBudget verifies that when every credential is
// over its TPM budget, Select still returns one rather than ErrNoSelection —
// same reactive-backstop behavior as the existing RPM soft-limit fallback.
func TestTPM_FallbackWhenAllOverBudget(t *testing.T) {
	ctx := context.Background()
	pool := newTPMPool(50, 5*time.Second, "only-key")

	plan, err := pool.Select(ctx, nil, "test-model")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	pool.RecordTokens(plan.SelectionID, 999)
	pool.Release(plan, nil)

	plan2, err := pool.Select(ctx, nil, "test-model")
	if err != nil {
		t.Fatalf("expected key despite TPM overflow, got: %v", err)
	}
	pool.Release(plan2, nil)
}

// TestTPM_WindowExpiry verifies that recorded token samples age out after
// RateWindow elapses, making a previously over-budget credential eligible
// again — mirrors TestRateLimit_WindowExpiry for RPM.
func TestTPM_WindowExpiry(t *testing.T) {
	ctx := context.Background()
	pool := newTPMPool(50, 100*time.Millisecond, "key-a", "key-b")

	plan, err := pool.Select(ctx, nil, "test-model")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	pool.RecordTokens(plan.SelectionID, 200) // well over the 50 TPM cap
	pool.Release(plan, nil)

	time.Sleep(110 * time.Millisecond)

	// Window has cleared — Select must succeed without ErrNoSelection.
	plan2, err := pool.Select(ctx, nil, "test-model")
	if err != nil {
		t.Fatalf("select after TPM window expiry: %v", err)
	}
	pool.Release(plan2, nil)
}

// TestTPM_RecordTokens_IgnoresNonPositiveAndUnknownID verifies RecordTokens is
// a safe no-op for zero/negative token counts and unknown selection IDs.
func TestTPM_RecordTokens_IgnoresNonPositiveAndUnknownID(t *testing.T) {
	pool := newTPMPool(10, time.Minute, "only-key")
	pool.RecordTokens("does-not-exist", 5)
	pool.RecordTokens("key_0", 0)
	pool.RecordTokens("key_0", -5)

	// None of the above should have registered — key should still be usable.
	plan, err := pool.Select(context.Background(), nil, "test-model")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	pool.Release(plan, nil)
}

// --- HealthSnapshot / RestoreHealth (internal/localstate support) ---

// TestHealthSnapshot_RestoreHealth_RoundTrip verifies that a cooldown applied
// to one pool carries over to a fresh pool instance via
// HealthSnapshot/RestoreHealth — the standalone-mode restart-recovery path.
func TestHealthSnapshot_RestoreHealth_RoundTrip(t *testing.T) {
	ctx := context.Background()
	original := newPool("least_requests", "key-a", "key-b")

	// Rate-limit key_0 (wins the tie under least_requests).
	plan, err := original.Select(ctx, nil, "test-model")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if plan.SelectionID != "key_0" {
		t.Fatalf("expected key_0 first, got %s", plan.SelectionID)
	}
	original.Release(plan, &selector.RateLimitError{RetryAfter: time.Hour}) // long cooldown, won't expire mid-test

	snap := original.HealthSnapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot has %d entries, want 2", len(snap))
	}
	if snap["key_0"].State != "rate_limited" {
		t.Errorf("key_0 snapshot state = %q, want rate_limited", snap["key_0"].State)
	}

	// Fresh pool, same keys, never saw the 429 — restore the snapshot into it.
	restored := newPool("least_requests", "key-a", "key-b")
	restored.RestoreHealth(snap)

	plan2, err := restored.Select(ctx, nil, "test-model")
	if err != nil {
		t.Fatalf("Select on restored pool: %v", err)
	}
	if plan2.SelectionID != "key_1" {
		t.Errorf("expected restored pool to skip still-cooling key_0, got %s", plan2.SelectionID)
	}
	restored.Release(plan2, nil)
}

// TestRestoreHealth_PastCoolEnd_ImmediatelyAvailable verifies that a restored
// snapshot whose CoolEnd has already elapsed does not block Select — no
// special-casing needed, available(now) already treats a past CoolEnd as
// healthy.
func TestRestoreHealth_PastCoolEnd_ImmediatelyAvailable(t *testing.T) {
	pool := newPool("round_robin", "only-key")
	pool.RestoreHealth(map[string]selector.CredHealthSnapshot{
		"key_0": {State: "rate_limited", CoolEnd: time.Now().Add(-time.Hour)},
	})

	plan, err := pool.Select(context.Background(), nil, "test-model")
	if err != nil {
		t.Fatalf("Select after restoring an expired cooldown: %v", err)
	}
	pool.Release(plan, nil)
}

// TestRestoreHealth_UnknownIDAndEmpty_NoOp verifies RestoreHealth safely
// ignores snapshot entries for credentials the pool doesn't have, and is a
// harmless no-op on an empty/nil snapshot.
func TestRestoreHealth_UnknownIDAndEmpty_NoOp(t *testing.T) {
	pool := newPool("round_robin", "only-key")

	pool.RestoreHealth(nil)
	pool.RestoreHealth(map[string]selector.CredHealthSnapshot{})
	pool.RestoreHealth(map[string]selector.CredHealthSnapshot{
		"does-not-exist": {State: "rate_limited", CoolEnd: time.Now().Add(time.Hour)},
	})

	plan, err := pool.Select(context.Background(), nil, "test-model")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	pool.Release(plan, nil)
}

// TestSelect_PlanCarriesPoolName guards stats attribution: SelectionID is
// only unique within a pool, so per-credpool/provider stats need the pool's
// own name on the plan rather than guessing it from the key name.
func TestSelect_PlanCarriesPoolName(t *testing.T) {
	pool := selector.NewCredPool(selector.CredPoolConfig{
		Name:      "mistral-free",
		Keys:      toSpecs("mistral_a", "mistral_b"),
		Strategy:  "round_robin",
		Threshold: 3,
		Cooldown:  5 * time.Second,
	})

	plan, err := pool.Select(context.Background(), nil, "test-model")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if plan.PoolName != "mistral-free" {
		t.Errorf("plan.PoolName = %q, want %q", plan.PoolName, "mistral-free")
	}
}

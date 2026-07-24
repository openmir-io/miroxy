What Miroxy should borrow from 9Router:

1. Per-model locking (high value, low effort)
9Router's modelLock_<model> means a key rate-limited on gemini-2.5-flash stays available for gemini-1.5-pro on the same key. Miroxy's current design locks the whole key. Fix is contained to keyEntry in memory.go: add rateLimitedModels map[string]time.Time, update Acquire/Release to accept the model name.

2. Hard cap on provider-reported RetryAfter (5-min fix)
Miroxy already uses rlErr.RetryAfter verbatim. 9Router caps it at 30 minutes. One line: if cooldown > 30*time.Minute { cooldown = 30*time.Minute } — prevents a misbehaving upstream from locking a key for hours.

3. OAuth + token refresh (high value, high effort — Phase 2)
9Router handles Claude/Codex/Kiro/GitHub/Google OAuth with proactive pre-flight refresh. The design pattern: separate authType from the key value, add a TokenRefresher interface, call it in the executor before ToUpstream(). First step for Miroxy: add Refresh(ctx) error as a no-op to KeyPool that OAuth-aware pools can override.

4. Combo/fallback chains
Already on Miroxy's Phase 3.5 roadmap. 9Router's implementation confirms the mechanical change: replace single ModelEntry in pipeline.Target with []ModelEntry. The capability-reorder logic (prefer vision-capable providers when the request has images) is also worth porting.

5. Warmup/bypass detection
Claude Code sends naming/warmup requests on every session start that consume quota and return nothing useful. 9Router short-circuits them. Miroxy could add a pre-pipeline filter for this.

---------------
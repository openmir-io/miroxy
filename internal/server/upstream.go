package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"miroxy/core/selector"
	coreup "miroxy/core/upstream"
	intcred "miroxy/internal/cred"
	"miroxy/internal/idgen"
	"miroxy/internal/irc"
	"miroxy/internal/pipeline"
	"miroxy/internal/stats"
	"miroxy/internal/types"
)

// --- UpstreamExecutor (pipeline plugin) ---

// keyAttempt records the outcome of one upstream call for error reporting.
type keyAttempt struct {
	keyID  string
	status int
	msg    string
	body   []byte // raw upstream response body (for invisible pass-through)
}

// allKeysFailed builds the error returned when every retry attempt ended in failure.
func allKeysFailed(modelName, upstreamModel string, attempts []keyAttempt, invisible bool) *pipeline.PipelineError {
	if invisible && len(attempts) > 0 {
		last := attempts[len(attempts)-1]
		return &pipeline.PipelineError{
			Status:  last.status,
			RawBody: last.body,
		}
	}
	parts := make([]string, len(attempts))
	for i, a := range attempts {
		parts[i] = fmt.Sprintf("%s: %d %s", a.keyID, a.status, a.msg)
	}
	msg := fmt.Sprintf("%s <=> %s - %s", modelName, upstreamModel, strings.Join(parts, "; "))
	return &pipeline.PipelineError{
		Status:  http.StatusServiceUnavailable,
		ErrType: "overloaded_error",
		Msg:     msg,
	}
}

// maxRetries caps the retry loop. ErrNoSelection terminates early when all keys are exhausted.
const maxRetries = 10

// dispatchFor decides, for one retry attempt, whether to dispatch through the
// target's real IR-transform adapter or its raw-bytes passthrough adapter —
// comparing the request's actual client protocol (which DownstreamAdapter
// decoded it, carried on c.ClientProtocol) against this target's static
// Protocol. Passthrough only fires when PassthroughUpstream was actually
// built for this target; ctx carries the original request bytes so
// PassthroughAdapter can forward them verbatim instead of re-marshaling req.
func dispatchFor(ctx context.Context, plan *selector.ExecutionPlan, clientProtocol string, rawBody []byte) (context.Context, coreup.UpstreamAdapter) {
	rawEligible := plan.ForcePassthrough || (clientProtocol != "" && clientProtocol == plan.Protocol)
	if rawEligible && plan.PassthroughUpstream != nil {
		return coreup.WithRawBody(ctx, rawBody), plan.PassthroughUpstream
	}
	return ctx, plan.Upstream
}

// UpstreamExecutor is the terminal pipeline plugin. It owns the retry loops for
// both streaming and non-streaming upstream calls, using c.Target.Dispatcher for
// physical transport (HTTPDispatcher by default; future: SDKDispatcher for AWS Bedrock).
type UpstreamExecutor struct {
	probers map[string]*keyProber
	stats   *stats.Registry
	// usageAcc holds one entry per credstone-backed named pool, keyed by
	// plan.SelectionID (see routingState.usageAcc). nil/empty when credsource
	// is disabled — every lookup below is a no-op map read in that case.
	usageAcc map[string]*intcred.UsageAccumulator
}

func newUpstreamExecutor(probers map[string]*keyProber, reg *stats.Registry, usageAcc map[string]*intcred.UsageAccumulator) *UpstreamExecutor {
	return &UpstreamExecutor{probers: probers, stats: reg, usageAcc: usageAcc}
}

func (e *UpstreamExecutor) Name() string  { return "upstream" }
func (e *UpstreamExecutor) Priority() int { return pipeline.PriorityTerminal }

func (e *UpstreamExecutor) Execute(c *pipeline.LLMContext, _ pipeline.Handler) error {
	if c.Request.Stream {
		return e.executeStream(c)
	}
	return e.executeNonStream(c)
}

// --- Non-streaming retry loop ---

func (e *UpstreamExecutor) executeNonStream(c *pipeline.LLMContext) error {
	ctx, cancel := context.WithTimeout(c.RequestCtx, c.Target.Timeout)
	defer cancel()

	sel := c.Target.Selector
	req := c.Request
	if req.MaxTokens <= 0 {
		req.MaxTokens = 1024
	}
	model := c.Target.Model
	invisible := c.Target.Invisible

	var attempts []keyAttempt

	for attempt := range maxRetries {
		slog.Debug("upstream attempt", "attempt", attempt+1, "max", maxRetries, "model", model.Name)

		plan, err := sel.Select(ctx, req)
		if errors.Is(err, selector.ErrNoSelection) {
			slog.Debug("upstream: no healthy credential available",
				"attempt", attempt+1, "model", model.Name, "past_attempts", len(attempts))
			if p := e.probers[model.Name]; p != nil {
				p.trigger()
			}
			return allKeysFailed(model.Name, model.UpstreamModel, attempts, invisible)
		}
		slog.Debug("upstream key selected", "attempt", attempt+1, "key_id", plan.SelectionID, "model", model.Name)

		attemptCtx, dispatch := dispatchFor(ctx, plan, c.ClientProtocol, c.RawRequestBody)

		upstreamReq, err := dispatch.ToUpstream(attemptCtx, req, plan.Credential)
		if err != nil {
			sel.Release(plan, nil)
			return &pipeline.PipelineError{Status: http.StatusBadRequest, ErrType: "invalid_request_error", Msg: err.Error()}
		}

		resp, err := c.Target.Dispatcher.Do(attemptCtx, upstreamReq)
		if err != nil {
			sel.Release(plan, err)
			attempts = append(attempts, keyAttempt{keyID: plan.SelectionID, status: http.StatusBadGateway, msg: err.Error()})
			slog.Warn("upstream request failed, retrying", "attempt", attempt+1, "error", err)
			continue
		}

		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			trimmed := string(bytes.TrimSpace(body))
			attempts = append(attempts, keyAttempt{keyID: plan.SelectionID, status: resp.StatusCode, msg: trimmed, body: body})
			if resp.StatusCode == 429 {
				sel.Release(plan, &selector.RateLimitError{RetryAfter: parseRetryDelay(body)})
				slog.Warn("upstream rate limited (429), retrying with next key", "attempt", attempt+1, "model", model.Name)
			} else {
				sel.Release(plan, &selector.ServerOverloadError{})
				slog.Warn("upstream 5xx, parking key and retrying with next",
					"attempt", attempt+1, "status", resp.StatusCode, "key_id", plan.SelectionID, "model", model.Name)
			}
			continue
		}

		anthropicResp, err := dispatch.FromUpstream(resp)
		if err != nil {
			var upstreamErr *irc.UpstreamError
			if errors.As(err, &upstreamErr) {
				switch {
				case upstreamErr.HTTPStatus == 429:
					attempts = append(attempts, keyAttempt{keyID: plan.SelectionID, status: 429, msg: upstreamErr.Message})
					sel.Release(plan, &selector.RateLimitError{})
					slog.Warn("upstream body 429 (relay pattern), rate-limit cooldown applied", "attempt", attempt+1, "model", model.Name)
				case upstreamErr.HTTPStatus >= 500:
					attempts = append(attempts, keyAttempt{keyID: plan.SelectionID, status: upstreamErr.HTTPStatus, msg: upstreamErr.Message})
					sel.Release(plan, &selector.ServerOverloadError{})
					slog.Warn("upstream body 5xx (relay pattern), parking key and retrying with next",
						"attempt", attempt+1, "status", upstreamErr.HTTPStatus, "key_id", plan.SelectionID, "model", model.Name)
				default:
					sel.Release(plan, nil)
					return &pipeline.PipelineError{Status: http.StatusBadRequest, ErrType: "api_error", Msg: upstreamErr.Message}
				}
				continue
			}
			attempts = append(attempts, keyAttempt{keyID: plan.SelectionID, status: http.StatusBadGateway, msg: err.Error()})
			sel.Release(plan, err)
			slog.Warn("upstream response error, retrying",
				"attempt", attempt+1, "key_id", plan.SelectionID, "model", model.Name, "error", err)
			continue
		}

		sel.Release(plan, nil)
		anthropicResp.Model = req.Model
		slog.Debug("non-stream response",
			"model", req.Model,
			"stop_reason", anthropicResp.StopReason,
			"input_tokens", anthropicResp.Usage.InputTokens,
			"output_tokens", anthropicResp.Usage.OutputTokens,
		)
		if e.stats != nil {
			e.stats.Record(req.Model, plan.SelectionID,
				int64(anthropicResp.Usage.InputTokens),
				int64(anthropicResp.Usage.OutputTokens))
		}
		if ua := e.usageAcc[plan.SelectionID]; ua != nil {
			ua.AddTokens(int64(anthropicResp.Usage.InputTokens), int64(anthropicResp.Usage.OutputTokens))
		}
		if tr, ok := sel.(tokenRecorder); ok {
			tr.RecordTokens(plan.SelectionID, int64(anthropicResp.Usage.InputTokens)+int64(anthropicResp.Usage.OutputTokens))
		}
		c.Response = anthropicResp
		return nil
	}

	return allKeysFailed(model.Name, model.UpstreamModel, attempts, invisible)
}

// tokenRecorder is satisfied by CredPool (type assertion, not part of the
// Selector interface — mirrors probeCapable/outcomeReporter). Token counts
// are only known after the upstream response, well after Select/Release have
// already run, so this is fed from the executor rather than threaded through
// the retry loop's existing calls.
type tokenRecorder interface {
	RecordTokens(selectionID string, tokens int64)
}

// --- Streaming retry loop ---

func (e *UpstreamExecutor) executeStream(c *pipeline.LLMContext) error {
	ctx, cancel := context.WithTimeout(c.RequestCtx, c.Target.Timeout)

	sel := c.Target.Selector
	req := c.Request
	if req.MaxTokens <= 0 {
		req.MaxTokens = 1024
	}
	model := c.Target.Model
	invisible := c.Target.Invisible

	var (
		plan     *selector.ExecutionPlan
		dispatch coreup.UpstreamAdapter
		resp     *http.Response
		attempts []keyAttempt
	)

	for attempt := range maxRetries {
		slog.Debug("stream upstream attempt", "attempt", attempt+1, "max", maxRetries, "model", model.Name)

		p, err := sel.Select(ctx, req)
		if errors.Is(err, selector.ErrNoSelection) {
			slog.Debug("stream: no healthy credential available",
				"attempt", attempt+1, "model", model.Name, "past_attempts", len(attempts))
			if pr := e.probers[model.Name]; pr != nil {
				pr.trigger()
			}
			cancel()
			return allKeysFailed(model.Name, model.UpstreamModel, attempts, invisible)
		}
		slog.Debug("stream key selected", "attempt", attempt+1, "key_id", p.SelectionID, "model", model.Name)

		attemptCtx, d := dispatchFor(ctx, p, c.ClientProtocol, c.RawRequestBody)

		upstreamReq, err := d.ToUpstreamStream(attemptCtx, req, p.Credential)
		if err != nil {
			sel.Release(p, nil)
			cancel()
			return &pipeline.PipelineError{Status: http.StatusBadRequest, ErrType: "invalid_request_error", Msg: err.Error()}
		}

		upstreamResp, err := c.Target.Dispatcher.Do(attemptCtx, upstreamReq)
		if err != nil {
			sel.Release(p, err)
			attempts = append(attempts, keyAttempt{keyID: p.SelectionID, status: http.StatusBadGateway, msg: err.Error()})
			slog.Warn("stream upstream request failed, retrying", "attempt", attempt+1, "error", err)
			continue
		}

		if upstreamResp.StatusCode == 429 {
			body, _ := io.ReadAll(io.LimitReader(upstreamResp.Body, 4096))
			upstreamResp.Body.Close()
			trimmed := string(bytes.TrimSpace(body))
			attempts = append(attempts, keyAttempt{keyID: p.SelectionID, status: 429, msg: trimmed, body: body})
			sel.Release(p, &selector.RateLimitError{RetryAfter: parseRetryDelay(body)})
			slog.Warn("stream upstream rate-limited (429), retrying with next key", "attempt", attempt+1, "model", model.Name)
			continue
		}
		if upstreamResp.StatusCode >= 500 {
			body, _ := io.ReadAll(io.LimitReader(upstreamResp.Body, 4096))
			upstreamResp.Body.Close()
			trimmed := string(bytes.TrimSpace(body))
			attempts = append(attempts, keyAttempt{keyID: p.SelectionID, status: upstreamResp.StatusCode, msg: trimmed, body: body})
			sel.Release(p, &selector.ServerOverloadError{})
			slog.Warn("stream upstream 5xx, parking key and retrying with next",
				"attempt", attempt+1, "status", upstreamResp.StatusCode, "key_id", p.SelectionID, "model", model.Name)
			continue
		}
		if upstreamResp.StatusCode >= 400 {
			body, _ := io.ReadAll(io.LimitReader(upstreamResp.Body, 4096))
			upstreamResp.Body.Close()
			slog.Debug("stream upstream 4xx (non-retryable)",
				"attempt", attempt+1, "status", upstreamResp.StatusCode, "key_id", p.SelectionID, "model", model.Name)
			sel.Release(p, nil)
			cancel()
			return &pipeline.PipelineError{
				Status:  http.StatusBadRequest,
				ErrType: "api_error",
				Msg:     fmt.Sprintf("upstream error %d: %s", upstreamResp.StatusCode, bytes.TrimSpace(body)),
			}
		}

		plan = p
		dispatch = d
		resp = upstreamResp
		break
	}

	if plan == nil {
		cancel()
		return allKeysFailed(model.Name, model.UpstreamModel, attempts, invisible)
	}

	msgID := idgen.NewMsgID()
	slog.Debug("stream starting", "model", req.Model, "key_id", plan.SelectionID, "msg_id", msgID)

	// Raw passthrough attempt: relay upstream stream bytes verbatim instead of
	// decoding into the canonical SSEEvent channel — see dispatchFor.
	if plan.PassthroughUpstream != nil && dispatch == plan.PassthroughUpstream {
		c.SetRawStream(resp.Body, resp.Header.Get("Content-Type"), resp.StatusCode, func(streamErr error) {
			sel.Release(plan, streamErr)
			cancel()
		})
		return nil
	}

	events, _ := dispatch.StreamFromUpstream(ctx, resp, msgID, req.Model)
	if e.stats != nil {
		var tr tokenRecorder
		if r, ok := sel.(tokenRecorder); ok {
			tr = r
		}
		events = trackUsageStream(events, e.stats, req.Model, plan.SelectionID, e.usageAcc[plan.SelectionID], tr)
	}

	c.SetStream(events, func(streamErr error) {
		sel.Release(plan, streamErr)
		cancel()
	})
	return nil
}

// trackUsageStream wraps a SSE event channel, accumulates token usage from
// message_start (input) and message_delta (output) events, and records the
// totals to reg (and, when non-nil, ua/tr) when the channel closes.
func trackUsageStream(src <-chan types.SSEEvent, reg *stats.Registry, model, keyID string, ua *intcred.UsageAccumulator, tr tokenRecorder) <-chan types.SSEEvent {
	out := make(chan types.SSEEvent, 64)
	go func() {
		defer close(out)
		var totalIn, totalOut int64
		for ev := range src {
			switch ev.Event {
			case "message_start":
				if ms, ok := ev.Data.(types.MessageStartData); ok {
					totalIn += int64(ms.Message.Usage.InputTokens)
				}
			case "message_delta":
				if md, ok := ev.Data.(types.MessageDeltaData); ok {
					totalOut += int64(md.Usage.OutputTokens)
				}
			}
			out <- ev
		}
		if totalIn > 0 || totalOut > 0 {
			reg.Record(model, keyID, totalIn, totalOut)
			if ua != nil {
				ua.AddTokens(totalIn, totalOut)
			}
			if tr != nil {
				tr.RecordTokens(keyID, totalIn+totalOut)
			}
		}
	}()
	return out
}

// --- parseRetryDelay ---
// Extracts the retry wait duration from a Gemini 429 response body.
// Gemini encodes it as a duration string in RetryInfo: {"error":{"details":[{"retryDelay":"42s"}]}}.
func parseRetryDelay(body []byte) time.Duration {
	var payload struct {
		Error struct {
			Details []json.RawMessage `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return 0
	}
	var detail struct {
		RetryDelay string `json:"retryDelay"`
	}
	for _, raw := range payload.Error.Details {
		if json.Unmarshal(raw, &detail) != nil || detail.RetryDelay == "" {
			continue
		}
		d, err := time.ParseDuration(detail.RetryDelay)
		if err != nil || d <= 0 {
			continue
		}
		slog.Debug("parsed retryDelay from 429 body", "retryDelay", detail.RetryDelay, "duration", d)
		return d
	}
	return 0
}

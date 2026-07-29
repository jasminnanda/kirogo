package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jasminnanda/kirogo/internal/kiro"
	"github.com/jasminnanda/kirogo/internal/util"
)

// errFirstTokenTimeout reports that the backend produced no content within the
// first-token budget.
var errFirstTokenTimeout = errors.New("no first token within the timeout")

// eventOrError is one item from the upstream event stream.
type eventOrError struct {
	event *kiro.Event
	err   error
}

// session is one open upstream response being decoded.
type session struct {
	resp   *http.Response
	events <-chan eventOrError
	cancel context.CancelFunc
}

// close releases the upstream connection and stops the decoding goroutine.
func (s *session) close() {
	if s == nil {
		return
	}
	if s.cancel != nil {
		s.cancel()
	}
	if s.resp != nil && s.resp.Body != nil {
		s.resp.Body.Close()
	}
}

// startSession issues the upstream request and begins decoding it.
//
// The returned session owns the response body. A non-200 response is returned as
// an *kiro.APIError, with the body already consumed.
func (s *Server) startSession(ctx context.Context, req *kiro.Request) (*session, error) {
	streamCtx, cancel := context.WithCancel(ctx)

	resp, err := s.kiro.GenerateAssistantResponse(streamCtx, req)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		apiErr := kiro.ReadErrorResponse(resp)
		resp.Body.Close()
		cancel()
		return nil, apiErr
	}

	events := make(chan eventOrError, 64)
	reader := kiro.NewReader(resp.Body)

	go func() {
		defer close(events)
		for {
			msg, readErr := reader.Next()
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) {
					select {
					case events <- eventOrError{err: readErr}:
					case <-streamCtx.Done():
					}
				}
				return
			}
			event, parseErr := kiro.ParseEvent(msg)
			if parseErr != nil {
				select {
				case events <- eventOrError{err: parseErr}:
				case <-streamCtx.Done():
				}
				return
			}
			select {
			case events <- eventOrError{event: event}:
			case <-streamCtx.Done():
				return
			}
		}
	}()

	return &session{resp: resp, events: events, cancel: cancel}, nil
}

// producesOutput reports whether an event counts as the first token.
//
// Only content and reasoning count. Bookkeeping events such as
// messageMetadataEvent arrive promptly even when the model is stuck, so treating
// them as a first token would defeat the timeout.
func producesOutput(e *kiro.Event) bool {
	switch e.Kind {
	case kiro.EventAssistantResponse:
		return e.AssistantResponse != nil && e.AssistantResponse.Content != ""
	case kiro.EventReasoningContent:
		return e.Reasoning != nil && (e.Reasoning.Text != "" || len(e.Reasoning.RedactedContent) > 0)
	case kiro.EventToolUse:
		// A tool call is real output, so a tool-only response is not a stall.
		return true
	}
	return false
}

// awaitFirstToken waits for the first output-bearing event.
//
// Every event seen while waiting is buffered and returned, so nothing is lost.
// An exception or a clean end of stream also ends the wait, because neither is a
// stall the caller should retry through.
func awaitFirstToken(ctx context.Context, sess *session, timeout time.Duration) ([]*kiro.Event, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var buffered []*kiro.Event
	for {
		select {
		case <-ctx.Done():
			return buffered, ctx.Err()

		case <-timer.C:
			return buffered, errFirstTokenTimeout

		case item, ok := <-sess.events:
			if !ok {
				// The stream ended before producing anything. That is an empty
				// response, not a stall, so it is handed back for the caller to
				// report rather than retried here.
				return buffered, io.EOF
			}
			if item.err != nil {
				return buffered, item.err
			}
			buffered = append(buffered, item.event)
			if item.event.Kind == kiro.EventException {
				return buffered, nil
			}
			if producesOutput(item.event) {
				return buffered, nil
			}
		}
	}
}

// openStream sends the request and returns a session that has already produced
// its first output, applying the first-token retry policy.
//
// A retry is only ever attempted before any byte has reached the client, so a
// partially delivered response is never restarted. It also handles the single
// THINKING_SIGNATURE_INVALID recovery: the backend refused a reasoning signature,
// so the request is retried once with all reasoning stripped from history.
func (s *Server) openStream(ctx context.Context, req *kiro.Request) (*session, []*kiro.Event, error) {
	strippedReasoning := false

	for {
		sess, buffered, err := s.openStreamOnce(ctx, req)
		if err == nil {
			return sess, buffered, nil
		}

		var apiErr *kiro.APIError
		if errors.As(err, &apiErr) && apiErr.IsThinkingSignatureInvalid() && !strippedReasoning {
			if req.StripReasoning() {
				strippedReasoning = true
				slog.Warn("the backend rejected a reasoning signature, retrying once with reasoning stripped from history")
				continue
			}
			slog.Debug("reasoning signature was rejected but there is no reasoning to strip")
		}
		return nil, nil, err
	}
}

// openStreamOnce runs the first-token retry loop for a single request shape.
func (s *Server) openStreamOnce(ctx context.Context, req *kiro.Request) (*session, []*kiro.Event, error) {
	attempts := s.cfg.FirstTokenMaxRetries
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}

		sess, err := s.startSession(ctx, req)
		if err != nil {
			// A modelled API error is final: retrying an identical request would
			// get the identical rejection.
			var apiErr *kiro.APIError
			if errors.As(err, &apiErr) {
				return nil, nil, err
			}
			lastErr = err
			slog.Warn("could not open the upstream stream",
				"attempt", attempt, "of", attempts, "error", err.Error())
			continue
		}

		buffered, waitErr := awaitFirstToken(ctx, sess, s.cfg.FirstTokenTimeout)
		switch {
		case waitErr == nil:
			return sess, buffered, nil

		case errors.Is(waitErr, errFirstTokenTimeout):
			sess.close()
			lastErr = waitErr
			slog.Warn("the model produced nothing within the first-token budget, retrying",
				"timeout", s.cfg.FirstTokenTimeout, "attempt", attempt, "of", attempts)
			continue

		case errors.Is(waitErr, io.EOF):
			// An empty response. Retrying is worth one shot, since it is usually
			// transient, but the buffered events are handed back if this was the
			// last attempt so the caller can answer with an empty completion.
			if attempt < attempts {
				sess.close()
				lastErr = waitErr
				slog.Warn("the upstream stream ended with no output, retrying",
					"attempt", attempt, "of", attempts)
				continue
			}
			return sess, buffered, nil

		case errors.Is(waitErr, context.Canceled), errors.Is(waitErr, context.DeadlineExceeded):
			sess.close()
			return nil, nil, waitErr

		default:
			sess.close()
			return nil, nil, waitErr
		}
	}

	if lastErr == nil {
		lastErr = errFirstTokenTimeout
	}
	return nil, nil, fmt.Errorf("the model produced no output after %d attempts, each waiting %s: %w",
		attempts, s.cfg.FirstTokenTimeout, lastErr)
}

// accumulatedTool is a tool call being assembled from fragments.
type accumulatedTool struct {
	id      string
	name    string
	input   strings.Builder
	stopped bool
}

// toolAccumulator reassembles tool calls whose input arrives in pieces.
type toolAccumulator struct {
	order []string
	byID  map[string]*accumulatedTool
}

// newToolAccumulator returns an empty accumulator.
func newToolAccumulator() *toolAccumulator {
	return &toolAccumulator{byID: map[string]*accumulatedTool{}}
}

// add folds one toolUseEvent into the accumulator.
func (a *toolAccumulator) add(e *kiro.ToolUseEvent) {
	if e == nil {
		return
	}
	// A fragment with no id cannot be correlated, so it extends the most recent
	// call rather than being dropped.
	id := e.ToolUseID
	if id == "" {
		if len(a.order) == 0 {
			return
		}
		id = a.order[len(a.order)-1]
	}

	tool, ok := a.byID[id]
	if !ok {
		tool = &accumulatedTool{id: id}
		a.byID[id] = tool
		a.order = append(a.order, id)
	}
	if e.Name != "" {
		tool.name = e.Name
	}
	tool.input.WriteString(e.Input)
	if e.Stop {
		tool.stopped = true
	}
}

// empty reports whether no tool calls were seen.
func (a *toolAccumulator) empty() bool { return len(a.order) == 0 }

// FinishedToolCall is a fully assembled tool call.
type FinishedToolCall struct {
	// ID is the tool use identifier.
	ID string
	// Name is the tool name.
	Name string
	// Arguments is the compacted JSON arguments object.
	Arguments string
}

// finish assembles, validates and deduplicates the accumulated tool calls.
//
// Arguments that parse are re-marshalled compactly. Empty arguments become an
// empty object. Arguments that do not parse also become an empty object, with an
// error logged: sending a truncated JSON fragment guarantees a failure downstream,
// while an empty object at least lets the conversation continue.
func (a *toolAccumulator) finish() []FinishedToolCall {
	out := make([]FinishedToolCall, 0, len(a.order))

	for _, id := range a.order {
		tool := a.byID[id]
		raw := strings.TrimSpace(tool.input.String())

		arguments := "{}"
		switch {
		case raw == "":
			// Nothing to parse; an empty object is the correct representation.
		default:
			var parsed any
			if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
				slog.Error("a tool call arrived with arguments that are not valid JSON; sending an empty object instead",
					"tool", tool.name, "tool_use_id", tool.id,
					"bytes", len(raw), "error", err.Error())
			} else if compact, err := json.Marshal(parsed); err == nil {
				arguments = string(compact)
			}
		}
		if !tool.stopped {
			slog.Debug("a tool call never reported stop; using what arrived",
				"tool", tool.name, "tool_use_id", tool.id)
		}

		out = append(out, FinishedToolCall{ID: tool.id, Name: tool.name, Arguments: arguments})
	}

	return dedupeToolCalls(out)
}

// dedupeToolCalls removes duplicates, first by id and then by name and arguments.
//
// The backend occasionally repeats a call. Passing duplicates through makes a
// client execute the same side effect twice, which is worse than dropping one.
func dedupeToolCalls(calls []FinishedToolCall) []FinishedToolCall {
	// By id, keeping whichever copy carries the richer arguments.
	byID := map[string]int{}
	var stage []FinishedToolCall
	for _, c := range calls {
		if c.ID == "" {
			stage = append(stage, c)
			continue
		}
		if idx, seen := byID[c.ID]; seen {
			if len(c.Arguments) > len(stage[idx].Arguments) {
				stage[idx] = c
			}
			slog.Debug("dropped a duplicate tool call with a repeated id", "tool_use_id", c.ID)
			continue
		}
		byID[c.ID] = len(stage)
		stage = append(stage, c)
	}

	// Then by name and arguments, which catches the same call issued under two ids.
	seenSignature := map[string]bool{}
	out := make([]FinishedToolCall, 0, len(stage))
	for _, c := range stage {
		signature := c.Name + "\x00" + c.Arguments
		if seenSignature[signature] {
			slog.Debug("dropped a duplicate tool call with identical name and arguments",
				"tool", c.Name, "tool_use_id", c.ID)
			continue
		}
		seenSignature[signature] = true
		out = append(out, c)
	}
	return out
}

// outputBudget enforces a client's max_tokens ceiling on generated output.
//
// The Kiro backend has no max-tokens parameter, so a ceiling cannot be handed
// upstream. Dropping it is not an option either: max_tokens is a required field
// of the Anthropic API, clients size their context against it, and ignoring an
// explicit client instruction is the gateway overruling a decision that is not
// its to make. So kirogo honours it locally, cutting the output at the ceiling
// and reporting the length stop reason the client is waiting for.
//
// The budget covers text and reasoning, which is what the backend bills as
// output. Tool call arguments are deliberately exempt: they are machine-directed
// JSON, and half of one is a fragment no client can parse, which is a worse
// outcome than a reply running slightly over length.
type outputBudget struct {
	// limit is the ceiling in tokens. Zero or negative means no ceiling.
	limit int
	// contentRunes and reasoningRunes are what has been admitted so far. They are
	// counted apart because the usage report totals them apart.
	contentRunes   int
	reasoningRunes int
	// exhausted records that the ceiling has been reached.
	exhausted bool
}

// unlimited reports whether no ceiling applies.
func (b *outputBudget) unlimited() bool { return b.limit <= 0 }

// tokens returns the output tokens admitted so far.
//
// This mirrors estimateCompletionTokens exactly, so the point where a response is
// cut and the number reported for it cannot drift apart.
func (b *outputBudget) tokens() int {
	return util.ApplyClaudeCorrection(util.EstimateTokensForRuneCount(b.contentRunes)) +
		util.ApplyClaudeCorrection(util.EstimateTokensForRuneCount(b.reasoningRunes))
}

// take admits as much of chunk as the ceiling allows and returns the admitted
// part, marking the budget exhausted once the ceiling is met.
//
// Runes are admitted one at a time so the cut lands on the ceiling rather than
// somewhere past it, and on a rune boundary so multi-byte text is never split
// into invalid UTF-8.
func (b *outputBudget) take(chunk string, reasoning bool) string {
	if b.unlimited() || chunk == "" {
		return chunk
	}
	if b.exhausted {
		return ""
	}

	counter := &b.contentRunes
	if reasoning {
		counter = &b.reasoningRunes
	}

	admitted := 0
	for _, r := range chunk {
		*counter++
		if b.tokens() > b.limit {
			*counter--
			b.exhausted = true
			break
		}
		admitted += utf8.RuneLen(r)
	}
	return chunk[:admitted]
}

// accepted reports what apply took from an event once the budget was applied.
//
// The streaming paths emit exactly this rather than the raw event, which is what
// keeps the bytes a client receives and the tokens kirogo reports in agreement.
type accepted struct {
	// Content is the text admitted from the event.
	Content string
	// Reasoning is the reasoning text admitted from the event.
	Reasoning string
	// LimitReached reports that the client's ceiling has now been met, so the
	// caller should stop emitting.
	LimitReached bool
}

// collected is everything gathered from one upstream response.
type collected struct {
	content   strings.Builder
	reasoning strings.Builder
	// signature is the reasoning signature, needed to echo reasoning back.
	signature string
	// redacted is an opaque reasoning blob.
	redacted []byte

	tools *toolAccumulator

	// budget caps generated output at the client's max_tokens.
	budget outputBudget
	// mayCallTools records that the request offered tools, so a tool call could
	// still arrive after the budget is spent.
	mayCallTools bool

	usage      *kiro.TokenUsage
	stopReason string
	modelID    string

	credits    float64
	creditUnit string

	contextPercentage    float64
	hasContextPercentage bool

	// exception holds a modelled error delivered mid-stream.
	exception *kiro.ExceptionEvent
}

// newCollected returns an empty collector holding the client's output ceiling.
//
// outputLimit of zero or less means no ceiling, which is the case for an OpenAI
// request that omits max_tokens. mayCallTools should be true when the request
// offered any tool, because that decides whether the stream can be released as
// soon as the ceiling is met.
func newCollected(outputLimit int, mayCallTools bool) *collected {
	return &collected{
		tools:        newToolAccumulator(),
		budget:       outputBudget{limit: outputLimit},
		mayCallTools: mayCallTools,
	}
}

// apply folds one event into the collector and reports what was admitted.
func (c *collected) apply(e *kiro.Event) accepted {
	var out accepted

	switch e.Kind {
	case kiro.EventAssistantResponse:
		out.Content = c.budget.take(e.AssistantResponse.Content, false)
		c.content.WriteString(out.Content)
		if e.AssistantResponse.ModelID != "" {
			c.modelID = e.AssistantResponse.ModelID
		}

	case kiro.EventReasoningContent:
		out.Reasoning = c.budget.take(e.Reasoning.Text, true)
		c.reasoning.WriteString(out.Reasoning)
		// The signature and the redacted blob are bookkeeping, not generated text,
		// so they are recorded whatever the budget says. Withholding a signature
		// would cost a client the ability to replay the reasoning it did receive.
		if e.Reasoning.Signature != "" {
			c.signature = e.Reasoning.Signature
		}
		if len(e.Reasoning.RedactedContent) > 0 {
			c.redacted = e.Reasoning.RedactedContent
		}

	case kiro.EventToolUse:
		c.tools.add(e.ToolUse)

	case kiro.EventMetadata:
		if e.Metadata.TokenUsage != nil {
			c.usage = e.Metadata.TokenUsage
			if e.Metadata.TokenUsage.ContextUsagePercentage > 0 {
				c.contextPercentage = e.Metadata.TokenUsage.ContextUsagePercentage
				c.hasContextPercentage = true
			}
		}
		if e.Metadata.StopReason != "" {
			c.stopReason = e.Metadata.StopReason
		}

	case kiro.EventMetering:
		c.credits = e.Metering.Usage
		c.creditUnit = e.Metering.Unit

	case kiro.EventContextUsage:
		c.contextPercentage = e.ContextUsage.ContextUsagePercentage
		c.hasContextPercentage = true

	case kiro.EventException:
		c.exception = e.Exception
	}

	out.LimitReached = c.budget.exhausted
	return out
}

// stopReadingEarly reports whether the upstream stream can be abandoned now that
// the client's ceiling is met.
//
// Nothing that follows can reach the client, so releasing the connection saves
// waiting out a response nobody will read. The exception is a request that offered
// tools: a tool call may still be on its way, and tool arguments are exempt from
// the budget precisely so they arrive whole, so such a stream is drained in full.
// Checking whether a call has already started would not be enough, because the
// first fragment may not have arrived yet.
func (c *collected) stopReadingEarly() bool {
	return c.budget.exhausted && !c.mayCallTools
}

// logOutputLimit records that the client's own ceiling ended the response, not
// anything upstream.
func logOutputLimit(c *collected, released bool) {
	slog.Info("the response reached the client's max_tokens ceiling and was cut there",
		"max_tokens", c.budget.limit,
		"output_tokens", c.budget.tokens(),
		"upstream_released", released)
}

// truncated reports whether the response looks cut short by an upstream limit.
//
// The signal is a stream that produced content but no accounting at all: no usage
// in a metadata event and no context usage event. A complete response always
// carries one of them. Tool calls are excluded because a tool-only response
// legitimately ends without them.
func (c *collected) truncated() bool {
	if c.budget.exhausted {
		// kirogo stopped this response itself to honour the client's ceiling. The
		// missing accounting is a consequence of releasing the stream early, not a
		// signal from the backend, and reporting it as one would be a lie.
		return false
	}
	if c.content.Len() == 0 {
		return false
	}
	if !c.tools.empty() {
		return false
	}
	if c.usage != nil {
		return false
	}
	return !c.hasContextPercentage
}

// UsageReport is the token accounting for one response.
type UsageReport struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int

	CacheReadInputTokens  int
	CacheWriteInputTokens int

	ContextUsagePercentage float64
	HasContextUsage        bool

	CreditsUsed float64
	CreditUnit  string

	// Estimated marks a report that did not come from upstream counts.
	Estimated bool
}

// usageReport builds the accounting for a response.
//
// Exact counts from the backend are used whenever present. Without them, the
// context usage percentage against the model's context window gives a good total,
// and only if that is missing too does the estimator get involved.
func (c *collected) usageReport(promptEstimate, maxInputTokens int) UsageReport {
	completion := estimateCompletionTokens(c.content.String(), c.reasoning.String())

	report := UsageReport{
		CreditsUsed:            c.credits,
		CreditUnit:             c.creditUnit,
		ContextUsagePercentage: c.contextPercentage,
		HasContextUsage:        c.hasContextPercentage,
	}

	if c.usage != nil {
		report.PromptTokens = c.usage.PromptTokens()
		report.CompletionTokens = c.usage.OutputTokens
		report.TotalTokens = c.usage.Total()
		report.CacheReadInputTokens = c.usage.CacheReadInputTokens
		report.CacheWriteInputTokens = c.usage.CacheWriteInputTokens
		if report.CompletionTokens == 0 && completion > 0 {
			// Usage arrived but reported no output tokens, which happens on some
			// paths; the visible text is a better answer than zero.
			report.CompletionTokens = completion
			report.TotalTokens = report.PromptTokens + completion
		}
		return report
	}

	report.Estimated = true
	report.CompletionTokens = completion

	if c.hasContextPercentage && maxInputTokens > 0 && c.contextPercentage > 0 {
		total := int(c.contextPercentage / 100 * float64(maxInputTokens))
		prompt := total - completion
		if prompt < 0 {
			prompt = 0
		}
		report.PromptTokens = prompt
		report.TotalTokens = total
		return report
	}

	report.PromptTokens = promptEstimate
	report.TotalTokens = promptEstimate + completion
	return report
}

// estimateCompletionTokens estimates the output tokens from the visible text.
// Reasoning counts towards output, because the backend bills it that way.
func estimateCompletionTokens(content, reasoning string) int {
	return estimateOutputTokens(content) + estimateOutputTokens(reasoning)
}

// finishReasonFor maps a response onto an OpenAI finish reason.
func finishReasonFor(c *collected, toolCalls []FinishedToolCall, truncated bool) string {
	if truncated {
		return "length"
	}
	if len(toolCalls) > 0 {
		// Tool calls are ranked above the output ceiling on purpose. They are
		// exempt from the budget and so arrive complete, and an agent told
		// "length" would stop instead of running the call it was just handed.
		return "tool_calls"
	}
	if c.budget.exhausted {
		return "length"
	}
	switch strings.ToLower(c.stopReason) {
	case "max_tokens", "length":
		return "length"
	case "tool_use", "tool_calls":
		return "tool_calls"
	case "content_filtered", "content_filter":
		return "content_filter"
	default:
		return "stop"
	}
}

// anthropicStopReasonFor maps a response onto an Anthropic stop reason.
func anthropicStopReasonFor(c *collected, toolCalls []FinishedToolCall, truncated bool) string {
	if truncated {
		return "max_tokens"
	}
	if len(toolCalls) > 0 {
		// Ranked above the ceiling for the same reason as the OpenAI mapping: the
		// calls are complete, and the client should run them.
		return "tool_use"
	}
	if c.budget.exhausted {
		return "max_tokens"
	}
	switch strings.ToLower(c.stopReason) {
	case "max_tokens", "length":
		return "max_tokens"
	case "tool_use", "tool_calls":
		return "tool_use"
	case "stop_sequence":
		return "stop_sequence"
	default:
		return "end_turn"
	}
}

// logTruncation records a truncated response as an upstream limit.
func logTruncation(c *collected) {
	slog.Error("the upstream response was cut short: it delivered content but no token accounting, "+
		"which is how the Kiro backend signals an output limit. The client is being told the response hit a length limit.",
		"content_bytes", c.content.Len(), "reasoning_bytes", c.reasoning.Len())
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.

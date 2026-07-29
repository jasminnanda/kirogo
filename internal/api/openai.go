package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"kirogo/internal/catalog"
	"kirogo/internal/kiro"
	"kirogo/internal/translate"
	"kirogo/internal/util"
)

// maxRequestBodyBytes caps an inbound request body. It is generous enough for a
// large conversation while refusing an unbounded upload.
const maxRequestBodyBytes = 64 << 20

// preparedRequest is an inbound request translated and ready to send upstream.
type preparedRequest struct {
	// kiro is the upstream request.
	kiro *kiro.Request
	// ir is the intermediate form, kept for token estimation.
	ir *translate.Request
	// resolution records how the model name was resolved.
	resolution catalog.Resolution
	// promptEstimate is the fallback input token count.
	promptEstimate int
	// maxInputTokens is the resolved model's context window, zero when unknown.
	maxInputTokens int
}

// prepare resolves the model, applies the structural rules and builds the
// upstream request.
func (s *Server) prepare(ctx context.Context, ir *translate.Request) (*preparedRequest, error) {
	s.catalog.EnsureFresh(ctx)

	requestEffort := ir.EffortLevel
	operatorDefault := s.cfg.EffortLevel
	if ir.DisableReasoning {
		// An explicit request for no reasoning overrides both the operator default
		// and the model default.
		requestEffort = ""
		operatorDefault = ""
	}

	resolution := s.catalog.Resolve(ir.Model, requestEffort, operatorDefault)

	fields := resolution.AdditionalModelRequestFields()
	if ir.DisableReasoning {
		fields = nil
	}

	req, err := translate.Build(translate.BuildInput{
		Messages:                     ir.Messages,
		SystemPrompt:                 ir.SystemPrompt,
		Tools:                        ir.Tools,
		ModelID:                      resolution.ModelID,
		ConversationID:               util.UUID4(),
		ProfileARN:                   s.profileARN(),
		AdditionalModelRequestFields: fields,
		AgentMode:                    s.cfg.AgentMode,
		ToolDescriptionMaxLength:     s.cfg.ToolDescriptionMaxLength,
		MaxPayloadBytes:              s.cfg.MaxPayloadBytes,
		SystemPromptAsField:          s.cfg.SystemPromptAsField,
	})
	if err != nil {
		return nil, err
	}

	prepared := &preparedRequest{
		kiro:           req,
		ir:             ir,
		resolution:     resolution,
		promptEstimate: estimatePromptTokens(ir),
	}
	if resolution.Model != nil {
		prepared.maxInputTokens = resolution.Model.MaxInputTokens
	}

	slog.Debug("prepared a Kiro request",
		"requested_model", ir.Model,
		"resolved_model", resolution.ModelID,
		"resolution", string(resolution.Source),
		"effort", orNone(resolution.EffortLevel),
		"tools", len(ir.Tools),
		"messages", len(ir.Messages),
		"stream", ir.Stream)

	return prepared, nil
}

// profileARN returns the ARN to send, if any.
func (s *Server) profileARN() string {
	if s.auth == nil {
		return ""
	}
	return s.auth.ProfileARN()
}

// orNone renders an empty string as "(none)" for logging.
func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// handleChatCompletions serves POST /v1/chat/completions.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, flavorOpenAI, http.StatusMethodNotAllowed, "Use POST for /v1/chat/completions.")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes))
	if err != nil {
		writeError(w, flavorOpenAI, http.StatusBadRequest, "Could not read the request body: "+err.Error())
		return
	}

	var req translate.OpenAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, flavorOpenAI, http.StatusBadRequest,
			"The request body is not valid JSON: "+err.Error())
		return
	}
	if req.Model == "" {
		writeError(w, flavorOpenAI, http.StatusBadRequest,
			"No model was given. Set \"model\" to one of the ids from GET /v1/models.")
		return
	}

	ir, err := translate.FromOpenAI(&req)
	if err != nil {
		writeError(w, flavorOpenAI, http.StatusBadRequest, err.Error())
		return
	}

	prepared, err := s.prepare(r.Context(), ir)
	if err != nil {
		writeBuildError(w, flavorOpenAI, err)
		return
	}

	if ir.Stream {
		s.streamOpenAI(w, r, prepared)
		return
	}
	s.completeOpenAI(w, r, prepared)
}

// writeBuildError converts a translation failure into an HTTP response.
func writeBuildError(w http.ResponseWriter, flavor apiFlavor, err error) {
	var be *translate.BuildError
	if errors.As(err, &be) {
		status := http.StatusBadRequest
		if be.TooLarge {
			status = http.StatusRequestEntityTooLarge
		}
		writeError(w, flavor, status, be.Message)
		return
	}
	slog.Error("could not build the upstream request", "error", err.Error())
	writeError(w, flavor, http.StatusInternalServerError,
		"kirogo could not build the upstream request: "+err.Error())
}

// writeUpstreamError converts an upstream failure into an HTTP response.
func writeUpstreamError(w http.ResponseWriter, flavor apiFlavor, err error) {
	var apiErr *kiro.APIError
	if errors.As(err, &apiErr) {
		slog.Error("the Kiro backend rejected the request", "error", apiErr.Error())
		writeError(w, flavor, apiErr.ClientStatus(), apiErr.UserMessage())
		return
	}
	if errors.Is(err, errFirstTokenTimeout) {
		slog.Error("the model never started responding", "error", err.Error())
		writeError(w, flavor, http.StatusGatewayTimeout, err.Error())
		return
	}
	if errors.Is(err, context.Canceled) {
		// The client hung up. There is nobody left to answer.
		slog.Debug("the client disconnected before the response started")
		return
	}
	slog.Error("the upstream request failed", "error", err.Error())
	writeError(w, flavor, http.StatusBadGateway, err.Error())
}

// openAIUsage is the usage block on a completion.
type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`

	// The remaining fields are additive. Strict clients ignore them; clients that
	// look get the cache accounting and credit cost without another request.
	CacheReadInputTokens   int      `json:"cache_read_input_tokens,omitempty"`
	CacheWriteInputTokens  int      `json:"cache_write_input_tokens,omitempty"`
	ContextUsagePercentage *float64 `json:"context_usage_percentage,omitempty"`
	CreditsUsed            *float64 `json:"credits_used,omitempty"`
	CreditUnit             string   `json:"credit_unit,omitempty"`
	Estimated              bool     `json:"estimated,omitempty"`
}

// buildOpenAIUsage converts a usage report into the response block.
func buildOpenAIUsage(r UsageReport) *openAIUsage {
	out := &openAIUsage{
		PromptTokens:          r.PromptTokens,
		CompletionTokens:      r.CompletionTokens,
		TotalTokens:           r.TotalTokens,
		CacheReadInputTokens:  r.CacheReadInputTokens,
		CacheWriteInputTokens: r.CacheWriteInputTokens,
		CreditUnit:            r.CreditUnit,
		Estimated:             r.Estimated,
	}
	if r.HasContextUsage {
		pct := r.ContextUsagePercentage
		out.ContextUsagePercentage = &pct
	}
	if r.CreditsUsed > 0 {
		credits := r.CreditsUsed
		out.CreditsUsed = &credits
	}
	return out
}

// openAIToolCall is a tool call in a non-streaming response.
type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIToolFunction `json:"function"`
}

// openAIToolFunction is the callable part of a tool call.
type openAIToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// openAIMessage is the assembled assistant message.
type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`

	// ReasoningContent carries native reasoning back to the client.
	ReasoningContent string `json:"reasoning_content,omitempty"`
	// ReasoningSignature must be echoed back on the next turn for the backend to
	// accept the reasoning. OpenAI has no field for it, so kirogo adds one.
	ReasoningSignature string `json:"reasoning_signature,omitempty"`
	// ReasoningRedactedContent is the base64 opaque reasoning blob.
	ReasoningRedactedContent string `json:"reasoning_redacted_content,omitempty"`

	ToolCalls []openAIToolCall `json:"tool_calls,omitempty"`
}

// openAIChoice is one completion choice.
type openAIChoice struct {
	Index        int           `json:"index"`
	Message      openAIMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

// openAICompletion is the non-streaming response.
type openAICompletion struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
	Usage   *openAIUsage   `json:"usage,omitempty"`
}

// completeOpenAI serves a non-streaming completion.
//
// It consumes the same upstream stream as the streaming path and assembles the
// result, so the two paths cannot drift apart.
func (s *Server) completeOpenAI(w http.ResponseWriter, r *http.Request, prepared *preparedRequest) {
	sess, buffered, err := s.openStream(r.Context(), prepared.kiro)
	if err != nil {
		writeUpstreamError(w, flavorOpenAI, err)
		return
	}
	defer sess.close()

	c := newCollected(prepared.ir.MaxTokens, len(prepared.ir.Tools) > 0)
	for _, e := range buffered {
		c.apply(e)
	}

	released := c.stopReadingEarly()
	if !released {
		for item := range sess.events {
			if item.err != nil {
				// Content already gathered is worth returning; a mid-stream failure
				// with nothing to show is an error.
				if c.content.Len() == 0 && c.tools.empty() && c.reasoning.Len() == 0 {
					writeUpstreamError(w, flavorOpenAI, item.err)
					return
				}
				slog.Error("the upstream stream failed partway through; returning what arrived",
					"error", item.err.Error())
				break
			}
			c.apply(item.event)
			if c.stopReadingEarly() {
				released = true
				break
			}
		}
	}
	if c.budget.exhausted {
		logOutputLimit(c, released)
	}

	if c.exception != nil {
		writeUpstreamError(w, flavorOpenAI, apiErrorFromException(c.exception))
		return
	}

	toolCalls := c.tools.finish()
	truncated := c.truncated()
	if truncated {
		logTruncation(c)
	}

	message := openAIMessage{Role: "assistant", Content: c.content.String()}
	if reasoning := c.reasoning.String(); reasoning != "" {
		message.ReasoningContent = reasoning
		message.ReasoningSignature = c.signature
	}
	if len(c.redacted) > 0 {
		message.ReasoningRedactedContent = encodeBase64(c.redacted)
	}
	for _, tc := range toolCalls {
		message.ToolCalls = append(message.ToolCalls, openAIToolCall{
			ID:       tc.ID,
			Type:     "function",
			Function: openAIToolFunction{Name: tc.Name, Arguments: tc.Arguments},
		})
	}

	usage := c.usageReport(prepared.promptEstimate, prepared.maxInputTokens)

	writeJSON(w, http.StatusOK, openAICompletion{
		ID:      util.CompletionID(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   prepared.ir.Model,
		Choices: []openAIChoice{{
			Index:        0,
			Message:      message,
			FinishReason: finishReasonFor(c, toolCalls, truncated),
		}},
		Usage: buildOpenAIUsage(usage),
	})
}

// apiErrorFromException converts a mid-stream exception into an APIError so it
// shares the error mapping used for HTTP failures.
func apiErrorFromException(e *kiro.ExceptionEvent) *kiro.APIError {
	status := http.StatusBadGateway
	switch {
	case e.RetryAfterMilliseconds > 0:
		status = http.StatusTooManyRequests
	case e.Type == "internalServerException":
		status = http.StatusInternalServerError
	}
	return &kiro.APIError{
		StatusCode:             status,
		Message:                e.Message,
		Reason:                 e.Reason,
		RetryAfterMilliseconds: e.RetryAfterMilliseconds,
		ExceptionType:          e.Type,
	}
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.

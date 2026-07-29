package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/jasminnanda/kirogo/internal/translate"
	"github.com/jasminnanda/kirogo/internal/util"
)

// anthropicContentBlock is one block in an assembled message.
type anthropicContentBlock struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`

	// redacted_thinking
	Data string `json:"data,omitempty"`

	// tool_use
	ID    string `json:"id,omitempty"`
	Name  string `json:"name,omitempty"`
	Input any    `json:"input,omitempty"`
}

// anthropicUsage is the usage block on a message.
type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`

	// Anthropic's own cache field names, so a client that understands them works
	// without change.
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`

	// The remaining fields are kirogo additions.
	ContextUsagePercentage *float64 `json:"context_usage_percentage,omitempty"`
	CreditsUsed            *float64 `json:"credits_used,omitempty"`
	CreditUnit             string   `json:"credit_unit,omitempty"`
	Estimated              bool     `json:"estimated,omitempty"`
}

// buildAnthropicUsage converts a usage report into the Anthropic shape.
func buildAnthropicUsage(r UsageReport) anthropicUsage {
	out := anthropicUsage{
		InputTokens:              r.PromptTokens,
		OutputTokens:             r.CompletionTokens,
		CacheReadInputTokens:     r.CacheReadInputTokens,
		CacheCreationInputTokens: r.CacheWriteInputTokens,
		CreditUnit:               r.CreditUnit,
		Estimated:                r.Estimated,
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

// anthropicMessage is the assembled non-streaming response.
type anthropicMessage struct {
	ID           string                  `json:"id"`
	Type         string                  `json:"type"`
	Role         string                  `json:"role"`
	Model        string                  `json:"model"`
	Content      []anthropicContentBlock `json:"content"`
	StopReason   *string                 `json:"stop_reason"`
	StopSequence *string                 `json:"stop_sequence"`
	Usage        anthropicUsage          `json:"usage"`
}

// handleMessages serves POST /v1/messages.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, flavorAnthropic, http.StatusMethodNotAllowed, "Use POST for /v1/messages.")
		return
	}

	ir, ok := s.decodeAnthropic(w, r)
	if !ok {
		return
	}

	prepared, err := s.prepare(r.Context(), ir)
	if err != nil {
		writeBuildError(w, flavorAnthropic, err)
		return
	}

	if ir.Stream {
		s.streamAnthropic(w, r, prepared)
		return
	}
	s.completeAnthropic(w, r, prepared)
}

// decodeAnthropic reads and validates an Anthropic request body.
func (s *Server) decodeAnthropic(w http.ResponseWriter, r *http.Request) (*translate.Request, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes))
	if err != nil {
		writeError(w, flavorAnthropic, http.StatusBadRequest, "Could not read the request body: "+err.Error())
		return nil, false
	}

	var req translate.AnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, flavorAnthropic, http.StatusBadRequest,
			"The request body is not valid JSON: "+err.Error())
		return nil, false
	}
	if req.Model == "" {
		writeError(w, flavorAnthropic, http.StatusBadRequest,
			"No model was given. Set \"model\" to one of the ids from GET /v1/models.")
		return nil, false
	}

	ir, err := translate.FromAnthropic(&req)
	if err != nil {
		writeError(w, flavorAnthropic, http.StatusBadRequest, err.Error())
		return nil, false
	}
	return ir, true
}

// completeAnthropic serves a non-streaming message.
func (s *Server) completeAnthropic(w http.ResponseWriter, r *http.Request, prepared *preparedRequest) {
	sess, buffered, err := s.openStream(r.Context(), prepared.kiro)
	if err != nil {
		writeUpstreamError(w, flavorAnthropic, err)
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
				if c.content.Len() == 0 && c.tools.empty() && c.reasoning.Len() == 0 {
					writeUpstreamError(w, flavorAnthropic, item.err)
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
		writeUpstreamError(w, flavorAnthropic, apiErrorFromException(c.exception))
		return
	}

	toolCalls := c.tools.finish()
	truncated := c.truncated()
	if truncated {
		logTruncation(c)
	}

	// Blocks appear in the order the model produced them: reasoning, then text,
	// then tool calls.
	var blocks []anthropicContentBlock
	if reasoning := c.reasoning.String(); reasoning != "" {
		blocks = append(blocks, anthropicContentBlock{
			Type:      "thinking",
			Thinking:  reasoning,
			Signature: c.signature,
		})
	}
	if len(c.redacted) > 0 {
		blocks = append(blocks, anthropicContentBlock{
			Type: "redacted_thinking",
			Data: encodeBase64(c.redacted),
		})
	}
	if content := c.content.String(); content != "" {
		blocks = append(blocks, anthropicContentBlock{Type: "text", Text: content})
	}
	for _, tc := range toolCalls {
		blocks = append(blocks, anthropicContentBlock{
			Type:  "tool_use",
			ID:    toolUseID(tc.ID),
			Name:  tc.Name,
			Input: rawJSONOrEmptyObject(tc.Arguments),
		})
	}
	if len(blocks) == 0 {
		// The content array must never be null: clients index into it.
		blocks = []anthropicContentBlock{}
	}

	stopReason := anthropicStopReasonFor(c, toolCalls, truncated)
	usage := c.usageReport(prepared.promptEstimate, prepared.maxInputTokens)

	writeJSON(w, http.StatusOK, anthropicMessage{
		ID:         util.MessageID(),
		Type:       "message",
		Role:       "assistant",
		Model:      prepared.ir.Model,
		Content:    blocks,
		StopReason: &stopReason,
		Usage:      buildAnthropicUsage(usage),
	})
}

// toolUseID returns a usable tool use id, synthesising one when the backend
// omitted it, because a client needs an id to answer the call.
func toolUseID(id string) string {
	if id != "" {
		return id
	}
	return util.ToolUseID()
}

// rawJSONOrEmptyObject parses tool arguments for embedding as a JSON object.
func rawJSONOrEmptyObject(arguments string) any {
	if arguments == "" {
		return map[string]any{}
	}
	var parsed any
	if err := json.Unmarshal([]byte(arguments), &parsed); err != nil || parsed == nil {
		return map[string]any{}
	}
	return parsed
}

// countTokensResponse is the /v1/messages/count_tokens body.
type countTokensResponse struct {
	InputTokens int `json:"input_tokens"`
}

// handleCountTokens serves POST /v1/messages/count_tokens.
//
// The count is an estimate produced locally, with no upstream call. Claude Code
// calls this before every request, so it has to be fast and it has to exist;
// omitting it breaks the client outright.
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, flavorAnthropic, http.StatusMethodNotAllowed,
			"Use POST for /v1/messages/count_tokens.")
		return
	}

	ir, ok := s.decodeAnthropic(w, r)
	if !ok {
		return
	}

	writeJSON(w, http.StatusOK, countTokensResponse{InputTokens: estimatePromptTokens(ir)})
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.

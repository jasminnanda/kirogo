package util

import (
	"encoding/json"
	"unicode/utf8"
)

// Token estimation constants.
//
// This is an estimator, not a tokenizer. The Kiro backend reports exact counts in
// its metadata event, so these numbers only serve the pre-flight count_tokens
// endpoint and the fallback path when a response carries no usage.
//
// Exact cl100k_base parity is not reachable in pure Go: that pre-tokenizer uses a
// negative lookahead, \s+(?!\S), which Go's RE2 engine does not support. Shipping
// an embedded BPE vocabulary to chase parity would add megabytes to the binary for
// an endpoint whose answer is advisory.
const (
	// charsPerToken is the average characters per token for mixed text.
	charsPerToken = 4
	// claudeCorrection accounts for Claude tokenising roughly 15 percent higher
	// than the GPT-4 vocabulary this ratio is derived from.
	claudeCorrection = 1.15
	// perMessageOverhead covers the role and the delimiters around a message.
	perMessageOverhead = 4
	// imageTokens is a flat charge per image, since real cost depends on
	// dimensions the estimator does not see.
	imageTokens = 100
	// perToolOverhead covers the wrapper around a tool declaration.
	perToolOverhead = 4
	// finalOverhead covers the reply priming tokens.
	finalOverhead = 3
)

// EstimateTextTokens estimates the tokens in a piece of text.
//
// Rune count is used rather than byte length so multi-byte scripts are not
// over-counted several times over.
func EstimateTextTokens(text string) int {
	return EstimateTokensForRuneCount(utf8.RuneCountInString(text))
}

// EstimateTokensForRuneCount estimates the tokens in text of a known length.
//
// It exists so a streaming output budget can be tracked as runes arrive, without
// re-measuring the whole accumulated response on every chunk. Sharing the
// arithmetic with EstimateTextTokens is the point: a budget that counted
// differently from the usage report would cut a response at one number and then
// report another.
func EstimateTokensForRuneCount(runes int) int {
	if runes <= 0 {
		return 0
	}
	return runes/charsPerToken + 1
}

// ApplyClaudeCorrection scales a raw estimate for Claude's vocabulary.
func ApplyClaudeCorrection(tokens int) int {
	return int(float64(tokens) * claudeCorrection)
}

// TokenEstimateInput describes a request to estimate.
type TokenEstimateInput struct {
	// System is the system prompt text.
	System string
	// Messages are the conversation turns.
	Messages []TokenEstimateMessage
	// Tools are the tool declarations.
	Tools []TokenEstimateTool
}

// TokenEstimateMessage is one turn to estimate.
type TokenEstimateMessage struct {
	// Role is the turn's role.
	Role string
	// Text is the turn's text content.
	Text string
	// Images is how many images the turn carries.
	Images int
	// ToolCalls are the serialised tool calls on the turn.
	ToolCalls []TokenEstimateToolCall
	// ToolResults are the serialised tool results on the turn.
	ToolResults []TokenEstimateToolResult
}

// TokenEstimateToolCall is a tool call to estimate.
type TokenEstimateToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// TokenEstimateToolResult is a tool result to estimate.
type TokenEstimateToolResult struct {
	ToolUseID string
	Content   string
	Images    int
}

// TokenEstimateTool is a tool declaration to estimate.
type TokenEstimateTool struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// EstimateTokens returns an approximate input token count for a request.
//
// The result is intentionally on the generous side: under-reporting would let a
// client believe a request fits when it does not.
func EstimateTokens(in TokenEstimateInput) int {
	total := 0

	if in.System != "" {
		total += EstimateTextTokens(in.System)
	}

	for _, m := range in.Messages {
		total += perMessageOverhead
		total += EstimateTextTokens(m.Role)
		total += EstimateTextTokens(m.Text)
		total += m.Images * imageTokens

		for _, tc := range m.ToolCalls {
			total += perMessageOverhead
			total += EstimateTextTokens(tc.ID)
			total += EstimateTextTokens(tc.Name)
			total += EstimateTextTokens(tc.Arguments)
		}
		for _, tr := range m.ToolResults {
			total += perMessageOverhead
			total += EstimateTextTokens(tr.ToolUseID)
			total += EstimateTextTokens(tr.Content)
			total += tr.Images * imageTokens
		}
	}

	for _, t := range in.Tools {
		total += perToolOverhead
		total += EstimateTextTokens(t.Name)
		total += EstimateTextTokens(t.Description)
		if len(t.InputSchema) > 0 {
			if encoded, err := json.Marshal(t.InputSchema); err == nil {
				total += EstimateTextTokens(string(encoded))
			}
		}
	}

	total += finalOverhead
	return ApplyClaudeCorrection(total)
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.

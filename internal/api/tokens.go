package api

import (
	"github.com/jasminnanda/kirogo/internal/translate"
	"github.com/jasminnanda/kirogo/internal/util"
)

// estimateOutputTokens estimates the tokens in generated text.
func estimateOutputTokens(text string) int {
	if text == "" {
		return 0
	}
	return util.ApplyClaudeCorrection(util.EstimateTextTokens(text))
}

// estimatePromptTokens estimates the input tokens for a translated request.
//
// It exists for the fallback path, when a response carries no upstream counts, and
// for the count_tokens endpoint.
func estimatePromptTokens(ir *translate.Request) int {
	in := util.TokenEstimateInput{System: ir.SystemPrompt}

	for _, m := range ir.Messages {
		msg := util.TokenEstimateMessage{
			Role:   m.Role,
			Text:   m.Content,
			Images: len(m.Images),
		}
		for _, tc := range m.ToolCalls {
			msg.ToolCalls = append(msg.ToolCalls, util.TokenEstimateToolCall{
				ID:        tc.ID,
				Name:      tc.Name,
				Arguments: tc.Arguments,
			})
		}
		for _, tr := range m.ToolResults {
			msg.ToolResults = append(msg.ToolResults, util.TokenEstimateToolResult{
				ToolUseID: tr.ToolUseID,
				Content:   tr.Content,
				Images:    len(tr.Images),
			})
		}
		in.Messages = append(in.Messages, msg)
	}

	for _, t := range ir.Tools {
		in.Tools = append(in.Tools, util.TokenEstimateTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}

	return util.EstimateTokens(in)
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.

package translate

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// AnthropicRequest is the /v1/messages request body.
type AnthropicRequest struct {
	Model     string             `json:"model"`
	Messages  []AnthropicMessage `json:"messages"`
	MaxTokens int                `json:"max_tokens"`
	// System is a string or an array of content blocks.
	System any  `json:"system"`
	Stream bool `json:"stream"`

	Temperature   *float64           `json:"temperature"`
	TopP          *float64           `json:"top_p"`
	TopK          *int               `json:"top_k"`
	StopSequences []string           `json:"stop_sequences"`
	Tools         []AnthropicTool    `json:"tools"`
	ToolChoice    any                `json:"tool_choice"`
	Thinking      *AnthropicThinking `json:"thinking"`
	Metadata      map[string]any     `json:"metadata"`
}

// AnthropicThinking is the extended thinking configuration.
type AnthropicThinking struct {
	// Type is "enabled" or "disabled".
	Type string `json:"type"`
	// BudgetTokens is the thinking budget, mapped onto an effort tier.
	BudgetTokens int `json:"budget_tokens"`
}

// AnthropicMessage is one message in the request.
type AnthropicMessage struct {
	Role string `json:"role"`
	// Content is a string or an array of content blocks.
	Content any `json:"content"`
}

// AnthropicTool is a tool declaration.
type AnthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
	// Type marks server-side tools, which kirogo does not implement.
	Type string `json:"type"`
}

// anthropicBlock is one content block.
type anthropicBlock struct {
	Type string `json:"type"`

	// text
	Text string `json:"text"`

	// image
	Source *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	} `json:"source"`

	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	// tool_result
	ToolUseID string `json:"tool_use_id"`
	IsError   bool   `json:"is_error"`
	// Content of a tool_result is a string or an array of blocks.
	Content any `json:"content"`

	// thinking and redacted_thinking
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
	Data      string `json:"data"`
}

// FromAnthropic converts an Anthropic request into the intermediate
// representation.
func FromAnthropic(req *AnthropicRequest) (*Request, error) {
	out := &Request{
		Model:     req.Model,
		Stream:    req.Stream,
		MaxTokens: req.MaxTokens,
	}

	out.SystemPrompt = anthropicSystem(req.System)

	for _, t := range req.Tools {
		if t.Name == "" {
			// A server-side tool with no name is one kirogo cannot serve.
			if t.Type != "" {
				slog.Warn("ignoring a server-side tool that kirogo does not implement", "type", t.Type)
			}
			continue
		}
		out.Tools = append(out.Tools, Tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}

	if req.Thinking != nil {
		out.EffortLevel, out.DisableReasoning = effortFromThinking(req.Thinking)
	}

	for i := range req.Messages {
		m := &req.Messages[i]
		role := strings.ToLower(m.Role)
		text, images, toolUses, toolResults, reasoning := anthropicContent(m.Content)

		msg := Message{Content: text, Images: images}
		switch role {
		case "assistant":
			msg.Role = RoleAssistant
			msg.ToolCalls = toolUses
			msg.Reasoning = reasoning
			// Tool results on an assistant turn are not valid Anthropic input, but
			// some clients send them. Keeping them lets the invariants inline them
			// rather than losing the content.
			msg.ToolResults = toolResults
		default:
			msg.Role = RoleUser
			msg.ToolResults = toolResults
			// Likewise, tool calls on a user turn are kept so nothing is lost.
			msg.ToolCalls = toolUses
		}
		out.Messages = append(out.Messages, msg)
	}

	if len(out.Messages) == 0 && out.SystemPrompt == "" {
		return nil, fmt.Errorf("the request has no messages. Send at least one message in the messages array")
	}
	return out, nil
}

// anthropicSystem flattens the system field, which may be a string or blocks.
func anthropicSystem(system any) string {
	switch v := system.(type) {
	case nil:
		return ""
	case string:
		return v
	}

	raw, err := json.Marshal(system)
	if err != nil {
		return ""
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		return ""
	}

	var out string
	for _, b := range blocks {
		if b.Text != "" {
			out = appendSystemText(out, b.Text)
		}
	}
	return out
}

// anthropicContent decomposes a message content field into its parts.
func anthropicContent(content any) (text string, images []Image, toolUses []ToolCall, toolResults []ToolResult, reasoning *Reasoning) {
	switch v := content.(type) {
	case nil:
		return "", nil, nil, nil, nil
	case string:
		return v, nil, nil, nil, nil
	}

	raw, err := json.Marshal(content)
	if err != nil {
		return fmt.Sprint(content), nil, nil, nil, nil
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s, nil, nil, nil, nil
		}
		return string(raw), nil, nil, nil, nil
	}

	var textBuilder strings.Builder
	for _, b := range blocks {
		switch b.Type {
		case "text", "":
			textBuilder.WriteString(b.Text)

		case "image":
			if b.Source == nil {
				continue
			}
			if b.Source.Type == "url" || b.Source.URL != "" {
				slog.Warn("skipping an image given as a URL: the Kiro backend cannot fetch remote images. Send it inline as base64 instead.")
				continue
			}
			if b.Source.Data != "" {
				mediaType := b.Source.MediaType
				if mediaType == "" {
					mediaType = "image/png"
				}
				images = append(images, Image{MediaType: mediaType, Data: b.Source.Data})
			}

		case "tool_use":
			args := "{}"
			if len(b.Input) > 0 {
				args = string(b.Input)
			}
			toolUses = append(toolUses, ToolCall{ID: b.ID, Name: b.Name, Arguments: args})

		case "tool_result":
			resultText, resultImages := anthropicToolResultContent(b.Content)
			toolResults = append(toolResults, ToolResult{
				ToolUseID: b.ToolUseID,
				Content:   resultText,
				IsError:   b.IsError,
				Images:    resultImages,
			})

		case "thinking":
			if reasoning == nil {
				reasoning = &Reasoning{}
			}
			reasoning.Text += b.Thinking
			if b.Signature != "" {
				reasoning.Signature = b.Signature
			}

		case "redacted_thinking":
			if reasoning == nil {
				reasoning = &Reasoning{}
			}
			if b.Data != "" {
				if blob, err := base64.StdEncoding.DecodeString(b.Data); err == nil {
					reasoning.RedactedContent = blob
				} else {
					slog.Debug("ignoring redacted_thinking data that is not valid base64", "error", err)
				}
			}

		default:
			// Keep any text an unrecognised block carries.
			if b.Text != "" {
				textBuilder.WriteString(b.Text)
			}
		}
	}

	return textBuilder.String(), images, toolUses, toolResults, reasoning
}

// anthropicToolResultContent flattens a tool_result content field.
func anthropicToolResultContent(content any) (string, []Image) {
	switch v := content.(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	}

	raw, err := json.Marshal(content)
	if err != nil {
		return fmt.Sprint(content), nil
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s, nil
		}
		return string(raw), nil
	}

	var text strings.Builder
	var images []Image
	for _, b := range blocks {
		switch b.Type {
		case "text", "":
			text.WriteString(b.Text)
		case "image":
			if b.Source == nil || b.Source.Data == "" {
				continue
			}
			mediaType := b.Source.MediaType
			if mediaType == "" {
				mediaType = "image/png"
			}
			// MCP tools commonly return screenshots here.
			images = append(images, Image{MediaType: mediaType, Data: b.Source.Data})
		default:
			if b.Text != "" {
				text.WriteString(b.Text)
			}
		}
	}
	return text.String(), images
}

// Thinking budget tiers. Anthropic expresses extended thinking as a token budget,
// while Kiro expresses it as a named effort level, so the budget is bucketed.
const (
	thinkingBudgetLow    = 1024
	thinkingBudgetMedium = 4096
	thinkingBudgetHigh   = 8192
	thinkingBudgetXHigh  = 16384
)

// effortFromThinking maps a thinking configuration onto an effort level.
func effortFromThinking(t *AnthropicThinking) (level string, disabled bool) {
	if t == nil {
		return "", false
	}
	if strings.EqualFold(t.Type, "disabled") {
		return "", true
	}
	switch {
	case t.BudgetTokens <= 0:
		// Thinking enabled with no budget: let the model's own default apply.
		return "", false
	case t.BudgetTokens <= thinkingBudgetLow:
		return "low", false
	case t.BudgetTokens <= thinkingBudgetMedium:
		return "medium", false
	case t.BudgetTokens <= thinkingBudgetHigh:
		return "high", false
	case t.BudgetTokens <= thinkingBudgetXHigh:
		return "xhigh", false
	default:
		return "max", false
	}
}

// decodeBase64 decodes standard base64, tolerating the URL-safe alphabet and
// missing padding that some clients produce.
func decodeBase64(s string) ([]byte, error) {
	if blob, err := base64.StdEncoding.DecodeString(s); err == nil {
		return blob, nil
	}
	if blob, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return blob, nil
	}
	if blob, err := base64.URLEncoding.DecodeString(s); err == nil {
		return blob, nil
	}
	return base64.RawURLEncoding.DecodeString(s)
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.

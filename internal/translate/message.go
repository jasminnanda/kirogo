// Package translate converts OpenAI and Anthropic requests into the Kiro wire
// format, applying the structural rules the Kiro backend enforces.
package translate

import (
	"encoding/json"
	"strings"
)

// Roles used in the intermediate representation. Anything else is normalised to
// RoleUser before a request is built, because Kiro history only has two sides.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleSystem    = "system"
)

// Image is an inline image in base64 form.
type Image struct {
	// MediaType is the full media type, for example "image/png".
	MediaType string
	// Data is the base64 payload, without a data URL prefix.
	Data string
}

// Format returns the bare subtype the Kiro backend expects, for example "png".
func (i Image) Format() string {
	if idx := strings.LastIndex(i.MediaType, "/"); idx >= 0 && idx+1 < len(i.MediaType) {
		return i.MediaType[idx+1:]
	}
	if i.MediaType == "" {
		return "png"
	}
	return i.MediaType
}

// ToolCall is a tool invocation the assistant made.
type ToolCall struct {
	// ID is the tool use identifier the matching result must reference.
	ID string
	// Name is the tool name.
	Name string
	// Arguments is the raw JSON arguments object, as a string.
	Arguments string
}

// ArgumentsObject parses Arguments into a JSON object.
//
// Kiro expects toolUses.input to be an object, not a string. Invalid or empty
// arguments become an empty object, because sending a malformed value is a
// guaranteed rejection while an empty object is merely wrong.
func (t ToolCall) ArgumentsObject() any {
	trimmed := strings.TrimSpace(t.Arguments)
	if trimmed == "" {
		return map[string]any{}
	}
	var parsed any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return map[string]any{}
	}
	if parsed == nil {
		return map[string]any{}
	}
	return parsed
}

// ToolResult is the outcome of a tool call.
type ToolResult struct {
	// ToolUseID references the tool call this answers.
	ToolUseID string
	// Content is the textual result.
	Content string
	// IsError marks a failed tool call.
	IsError bool
	// Images are images returned by the tool, which MCP screenshot tools do.
	Images []Image
}

// Status returns the Kiro tool result status.
func (t ToolResult) Status() string {
	if t.IsError {
		return "error"
	}
	return "success"
}

// Reasoning is native reasoning produced by the model.
type Reasoning struct {
	// Text is the reasoning text.
	Text string
	// Signature is the backend's signature over Text. Reasoning without one must
	// never be sent back.
	Signature string
	// RedactedContent is an opaque reasoning blob.
	RedactedContent []byte
	// ModelID records which model produced the reasoning. When it is set and
	// differs from the model being called, the reasoning is dropped, because the
	// signature is only valid for its own model.
	ModelID string
}

// Signed reports whether this reasoning can be sent back to the backend.
func (r *Reasoning) Signed() bool {
	if r == nil {
		return false
	}
	return len(r.RedactedContent) > 0 || (r.Text != "" && r.Signature != "")
}

// Message is one conversation turn in the intermediate representation.
type Message struct {
	// Role is user, assistant or system.
	Role string
	// Content is the text of the turn.
	Content string
	// ToolCalls are the tool invocations on an assistant turn.
	ToolCalls []ToolCall
	// ToolResults are the tool outcomes on a user turn.
	ToolResults []ToolResult
	// Images are images attached to the turn.
	Images []Image
	// Reasoning is native reasoning on an assistant turn.
	Reasoning *Reasoning
}

// HasToolContent reports whether the message carries any tool data.
func (m *Message) HasToolContent() bool {
	return len(m.ToolCalls) > 0 || len(m.ToolResults) > 0
}

// IsEmpty reports whether the message carries nothing at all.
func (m *Message) IsEmpty() bool {
	return m.Content == "" && !m.HasToolContent() && len(m.Images) == 0 && m.Reasoning == nil
}

// Tool is a tool declaration.
type Tool struct {
	// Name must be 64 characters or fewer.
	Name string
	// Description is shown to the model. An empty one is replaced with a
	// placeholder, because the backend rejects a blank description.
	Description string
	// InputSchema is the JSON Schema for the arguments.
	InputSchema map[string]any
}

// Request is the API-agnostic form of an inbound chat request.
type Request struct {
	// Model is the model name exactly as the client sent it.
	Model string
	// Messages are the conversation turns, system messages already extracted.
	Messages []Message
	// SystemPrompt is the combined system text.
	SystemPrompt string
	// Tools are the declared tools.
	Tools []Tool
	// Stream requests a streamed response.
	Stream bool
	// EffortLevel is the reasoning effort the client asked for, possibly empty.
	EffortLevel string
	// DisableReasoning records an explicit request for no reasoning at all.
	DisableReasoning bool
	// MaxTokens is the client's output ceiling. The backend has no equivalent
	// field, so it cannot be forwarded; kirogo enforces it locally instead, cutting
	// generated output at the ceiling and reporting a length stop reason. Zero
	// means no ceiling, which is the case for an OpenAI request that omits it.
	MaxTokens int
}

// appendSystemText joins system fragments with a blank line between them.
func appendSystemText(existing, addition string) string {
	addition = strings.TrimSpace(addition)
	if addition == "" {
		return existing
	}
	if existing == "" {
		return addition
	}
	return existing + "\n\n" + addition
}

// parseDataURL splits a data URL into its media type and base64 payload.
//
// It reports false for anything that is not a base64 data URL, including the
// http(s) URLs some clients send, which the Kiro backend cannot fetch.
func parseDataURL(url string) (mediaType, data string, ok bool) {
	if !strings.HasPrefix(url, "data:") {
		return "", "", false
	}
	comma := strings.Index(url, ",")
	if comma < 0 {
		return "", "", false
	}
	header := url[len("data:"):comma]
	payload := url[comma+1:]
	if payload == "" {
		return "", "", false
	}
	// The header is "image/png;base64" or just "image/png".
	mediaType = header
	if semi := strings.Index(header, ";"); semi >= 0 {
		mediaType = header[:semi]
	}
	if mediaType == "" {
		mediaType = "image/png"
	}
	return mediaType, payload, true
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.

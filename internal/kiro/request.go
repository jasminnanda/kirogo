package kiro

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// Placeholder is the filler Kiro requires wherever a content string would
// otherwise be empty, because the backend rejects empty content outright.
//
// The wording matters a little: this text reaches the model as part of the
// conversation, so it needs to read as an absent turn rather than as an
// instruction. Any non-empty string satisfies the backend.
const Placeholder = "(no content)"

// Origin identifies the caller to the backend. AI_EDITOR is what Kiro IDE sends.
const Origin = "AI_EDITOR"

// ChatTriggerType marks a user-initiated turn.
const ChatTriggerType = "MANUAL"

// Tool result statuses accepted by the backend.
const (
	ToolResultSuccess = "success"
	ToolResultError   = "error"
)

// Request is the GenerateAssistantResponse request body.
//
// The schema declares five members. Four are body fields; agentMode binds to the
// x-amzn-kiro-agent-mode header, so it is excluded from JSON and carried
// separately.
type Request struct {
	ConversationState ConversationState `json:"conversationState"`
	ProfileARN        string            `json:"profileArn,omitempty"`
	// SystemPrompt is a real top-level field. It must not be folded into the
	// first user message.
	SystemPrompt string `json:"systemPrompt,omitempty"`
	// AdditionalModelRequestFields is an arbitrary JSON document. Reasoning
	// effort lives here, at the top level of the request.
	AdditionalModelRequestFields map[string]any `json:"additionalModelRequestFields,omitempty"`

	// AgentMode is sent as a header, never in the body.
	AgentMode string `json:"-"`
}

// ConversationState holds the conversation itself.
type ConversationState struct {
	ChatTriggerType string         `json:"chatTriggerType"`
	ConversationID  string         `json:"conversationId"`
	CurrentMessage  CurrentMessage `json:"currentMessage"`
	// History alternates userInputMessage and assistantResponseMessage entries.
	// It is omitted when empty.
	History []HistoryEntry `json:"history,omitempty"`
}

// CurrentMessage wraps the turn being sent now.
type CurrentMessage struct {
	UserInputMessage *UserInputMessage `json:"userInputMessage,omitempty"`
}

// HistoryEntry is one past turn. Exactly one field is set.
type HistoryEntry struct {
	UserInputMessage         *UserInputMessage         `json:"userInputMessage,omitempty"`
	AssistantResponseMessage *AssistantResponseMessage `json:"assistantResponseMessage,omitempty"`
}

// UserInputMessage is a user turn.
//
// Images sit directly on the message, not inside userInputMessageContext.
type UserInputMessage struct {
	Content                 string                   `json:"content"`
	ModelID                 string                   `json:"modelId,omitempty"`
	Origin                  string                   `json:"origin,omitempty"`
	Images                  []ImageBlock             `json:"images,omitempty"`
	UserInputMessageContext *UserInputMessageContext `json:"userInputMessageContext,omitempty"`
}

// ImageBlock is an inline image.
type ImageBlock struct {
	// Format is the bare subtype, for example "png", not "image/png".
	Format string      `json:"format"`
	Source ImageSource `json:"source"`
}

// ImageSource carries base64 image bytes.
type ImageSource struct {
	Bytes string `json:"bytes"`
}

// UserInputMessageContext carries tool declarations and tool results.
type UserInputMessageContext struct {
	Tools       []Tool       `json:"tools,omitempty"`
	ToolResults []ToolResult `json:"toolResults,omitempty"`
}

// IsEmpty reports whether the context would serialise to an empty object, in
// which case it must be omitted entirely.
func (c *UserInputMessageContext) IsEmpty() bool {
	return c == nil || (len(c.Tools) == 0 && len(c.ToolResults) == 0)
}

// Tool wraps a tool specification.
type Tool struct {
	ToolSpecification ToolSpecification `json:"toolSpecification"`
}

// ToolSpecification declares a callable tool.
type ToolSpecification struct {
	// Name must be 64 characters or fewer.
	Name string `json:"name"`
	// Description must be non-empty.
	Description string          `json:"description"`
	InputSchema ToolInputSchema `json:"inputSchema"`
}

// ToolInputSchema wraps the JSON Schema for a tool's arguments.
type ToolInputSchema struct {
	JSON map[string]any `json:"json"`
}

// ToolResult reports the outcome of a tool call.
type ToolResult struct {
	Content []ToolResultContent `json:"content"`
	// Status is "success" or "error".
	Status string `json:"status"`
	// ToolUseID must match a toolUses entry on the preceding assistant turn.
	ToolUseID string `json:"toolUseId"`
}

// ToolResultContent is one block of tool output.
type ToolResultContent struct {
	Text string `json:"text"`
}

// AssistantResponseMessage is an assistant turn in history.
type AssistantResponseMessage struct {
	Content  string    `json:"content"`
	ToolUses []ToolUse `json:"toolUses,omitempty"`
	// ReasoningContent echoes native reasoning back so the backend can verify
	// its signature. Omitting a signed block is safe; sending an unsigned one is
	// not.
	ReasoningContent *ReasoningContent `json:"reasoningContent,omitempty"`
}

// ToolUse is a tool call the assistant made.
type ToolUse struct {
	Name string `json:"name"`
	// Input is the parsed arguments object, not a JSON string.
	Input     any    `json:"input"`
	ToolUseID string `json:"toolUseId"`
}

// ReasoningContent is the reasoning union. Exactly one member is set:
// reasoningText for signed reasoning, redactedContent for an opaque blob.
type ReasoningContent struct {
	ReasoningText *ReasoningText `json:"reasoningText,omitempty"`
	// RedactedContent is base64-encoded, because the schema types it as a blob.
	RedactedContent string `json:"redactedContent,omitempty"`
}

// ReasoningText is signed reasoning. Both fields are required by the backend.
type ReasoningText struct {
	Text      string `json:"text"`
	Signature string `json:"signature"`
}

// NewReasoningText builds a signed reasoning block. It returns nil when either
// half is missing, because unsigned reasoning must never be sent.
func NewReasoningText(text, signature string) *ReasoningContent {
	if text == "" || signature == "" {
		return nil
	}
	return &ReasoningContent{ReasoningText: &ReasoningText{Text: text, Signature: signature}}
}

// NewRedactedReasoning builds a redacted reasoning block from raw blob bytes.
func NewRedactedReasoning(blob []byte) *ReasoningContent {
	if len(blob) == 0 {
		return nil
	}
	return &ReasoningContent{RedactedContent: base64.StdEncoding.EncodeToString(blob)}
}

// StripReasoning removes every reasoningContent block from history.
//
// This is the recovery step for a THINKING_SIGNATURE_INVALID rejection: the
// backend refused a signature, so the request is retried once without any
// reasoning. It reports whether anything was removed, so the caller can skip a
// pointless retry.
func (r *Request) StripReasoning() bool {
	stripped := false
	for i := range r.ConversationState.History {
		if a := r.ConversationState.History[i].AssistantResponseMessage; a != nil && a.ReasoningContent != nil {
			a.ReasoningContent = nil
			stripped = true
		}
	}
	return stripped
}

// EffortFields builds the additionalModelRequestFields document for a reasoning
// effort level.
//
// schemaPath is the key discovered from the model's
// additionalModelRequestFieldsSchema, either "output_config" or "reasoning".
// A blank path or level yields nil, so no field is sent.
func EffortFields(schemaPath, level string) map[string]any {
	if schemaPath == "" || level == "" {
		return nil
	}
	return map[string]any{
		schemaPath: map[string]any{"effort": level},
	}
}

// Marshal serialises the request as compact JSON, the exact bytes sent upstream.
func (r *Request) Marshal() ([]byte, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("could not encode the Kiro request: %w", err)
	}
	return data, nil
}

// SizeBytes returns the serialised size of the request, used by the payload
// guard. A request that cannot be encoded reports zero along with the error.
func (r *Request) SizeBytes() (int, error) {
	data, err := r.Marshal()
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

// ListModelsResponse is the /ListAvailableModels response.
type ListModelsResponse struct {
	Models       []ModelSpec   `json:"models"`
	DefaultModel *DefaultModel `json:"defaultModel"`
	NextToken    string        `json:"nextToken"`
}

// DefaultModel names the backend's default.
type DefaultModel struct {
	ModelID string `json:"modelId"`
}

// ModelSpec is one entry from the model catalog. Field names come from the
// service schema.
type ModelSpec struct {
	ModelID             string       `json:"modelId"`
	ModelName           string       `json:"modelName"`
	Description         string       `json:"description"`
	ModelProvider       string       `json:"modelProvider"`
	RateMultiplier      float64      `json:"rateMultiplier"`
	RateUnit            string       `json:"rateUnit"`
	TokenLimits         *TokenLimits `json:"tokenLimits"`
	SupportedInputTypes []string     `json:"supportedInputTypes"`
	// AdditionalModelRequestFieldsSchema describes per-model request fields; the
	// reasoning effort enum is discovered inside it.
	AdditionalModelRequestFieldsSchema map[string]any `json:"additionalModelRequestFieldsSchema"`
}

// TokenLimits reports the model's context and output ceilings.
type TokenLimits struct {
	MaxInputTokens  int `json:"maxInputTokens"`
	MaxOutputTokens int `json:"maxOutputTokens"`
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.

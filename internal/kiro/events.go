package kiro

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

// EventKind classifies a decoded stream event.
type EventKind int

// Event kinds. Only the six kinds kirogo acts on get their own value; every
// other modelled member of the response union decodes to EventIgnored so that
// new server-side events cannot break a stream.
const (
	// EventIgnored is a modelled event kirogo does not use.
	EventIgnored EventKind = iota
	// EventAssistantResponse carries a chunk of assistant text.
	EventAssistantResponse
	// EventReasoningContent carries a chunk of native reasoning.
	EventReasoningContent
	// EventToolUse carries a tool call, possibly as a fragment.
	EventToolUse
	// EventMetadata carries exact token usage and the stop reason.
	EventMetadata
	// EventMetering carries credit consumption.
	EventMetering
	// EventContextUsage carries the percentage of the context window used.
	EventContextUsage
	// EventException carries a modelled error, ending the stream.
	EventException
	// EventUnknown is an event type absent from the union kirogo knows about.
	EventUnknown
)

// String names the kind for diagnostics.
func (k EventKind) String() string {
	switch k {
	case EventAssistantResponse:
		return "assistantResponse"
	case EventReasoningContent:
		return "reasoningContent"
	case EventToolUse:
		return "toolUse"
	case EventMetadata:
		return "metadata"
	case EventMetering:
		return "metering"
	case EventContextUsage:
		return "contextUsage"
	case EventException:
		return "exception"
	case EventUnknown:
		return "unknown"
	default:
		return "ignored"
	}
}

// AssistantResponseEvent is a chunk of assistant text.
type AssistantResponseEvent struct {
	Content string `json:"content"`
	ModelID string `json:"modelId"`
}

// ReasoningContentEvent is a chunk of native reasoning.
//
// Signature must be captured and echoed back on the assistant turn in history,
// or the backend rejects the next request with THINKING_SIGNATURE_INVALID.
// RedactedContent decodes from base64 automatically because it is a blob.
type ReasoningContentEvent struct {
	Text            string `json:"text"`
	RedactedContent []byte `json:"redactedContent"`
	Signature       string `json:"signature"`
}

// ToolUseEvent is a tool call. Input arrives as string fragments that must be
// concatenated per ToolUseID until Stop is true.
type ToolUseEvent struct {
	ToolUseID string `json:"toolUseId"`
	Name      string `json:"name"`
	Input     string `json:"input"`
	Stop      bool   `json:"stop"`
}

// TokenUsage is the exact accounting the backend reports. These numbers are
// authoritative; kirogo never estimates when they are present.
type TokenUsage struct {
	UncachedInputTokens    int     `json:"uncachedInputTokens"`
	OutputTokens           int     `json:"outputTokens"`
	TotalTokens            int     `json:"totalTokens"`
	CacheReadInputTokens   int     `json:"cacheReadInputTokens"`
	CacheWriteInputTokens  int     `json:"cacheWriteInputTokens"`
	ContextUsagePercentage float64 `json:"contextUsagePercentage"`
	NormalizedTokenUsage   float64 `json:"normalizedTokenUsage"`
}

// PromptTokens returns the total input cost: fresh input plus both cache legs.
func (u TokenUsage) PromptTokens() int {
	return u.UncachedInputTokens + u.CacheReadInputTokens + u.CacheWriteInputTokens
}

// Total returns the reported total, falling back to the component sum when the
// server omits it.
func (u TokenUsage) Total() int {
	if u.TotalTokens > 0 {
		return u.TotalTokens
	}
	return u.PromptTokens() + u.OutputTokens
}

// MetadataEvent carries usage and the stop reason.
//
// The bundled service schema declares only tokenUsage, but the live service also
// sends stopReason and stopDetails, so both are decoded when present.
type MetadataEvent struct {
	TokenUsage  *TokenUsage    `json:"tokenUsage"`
	StopReason  string         `json:"stopReason"`
	StopDetails map[string]any `json:"stopDetails"`
}

// MeteringEvent reports credit consumption for the request.
type MeteringEvent struct {
	Usage      float64 `json:"usage"`
	Unit       string  `json:"unit"`
	UnitPlural string  `json:"unitPlural"`
}

// ContextUsageEvent reports how much of the context window is in use.
type ContextUsageEvent struct {
	ContextUsagePercentage float64 `json:"contextUsagePercentage"`
}

// ExceptionEvent is a modelled error delivered inside the stream.
type ExceptionEvent struct {
	// Type is the :exception-type header, for example ThrottlingException.
	Type string `json:"-"`
	// Message is the human-readable message.
	Message string `json:"message"`
	// Reason is the machine-readable reason code, often empty.
	Reason string `json:"reason"`
	// RetryAfterMilliseconds accompanies throttling errors.
	RetryAfterMilliseconds int `json:"retryAfterMilliseconds"`
}

// Error renders the exception, so it can be returned as an error value.
func (e *ExceptionEvent) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("%s: %s (reason: %s)", e.Type, e.Message, e.Reason)
	}
	if e.Message != "" {
		return fmt.Sprintf("%s: %s", e.Type, e.Message)
	}
	return e.Type
}

// Event is a decoded stream event. Only the pointer matching Kind is non-nil.
type Event struct {
	Kind EventKind
	// Type is the raw :event-type or :exception-type header.
	Type string

	AssistantResponse *AssistantResponseEvent
	Reasoning         *ReasoningContentEvent
	ToolUse           *ToolUseEvent
	Metadata          *MetadataEvent
	Metering          *MeteringEvent
	ContextUsage      *ContextUsageEvent
	Exception         *ExceptionEvent
}

// ignoredEventTypes are the modelled union members kirogo decodes and discards.
// Keeping the list explicit is what lets a genuinely new event type be logged as
// unknown instead of disappearing silently.
var ignoredEventTypes = map[string]bool{
	"messageMetadataEvent":       true,
	"dryRunSucceedEvent":         true,
	"codeReferenceEvent":         true,
	"supplementaryWebLinksEvent": true,
	"followupPromptEvent":        true,
	"codeEvent":                  true,
	"intentsEvent":               true,
	"interactionComponentsEvent": true,
	"toolResultEvent":            true,
	"citationEvent":              true,
	"documentCitationEvent":      true,
	"invalidStateEvent":          true,
}

// exceptionEventTypes are union members that are errors rather than content.
var exceptionEventTypes = map[string]bool{
	"internalServerException": true,
}

// ParseEvent turns a decoded frame into a typed event.
//
// A payload that fails to parse is reported as an error, because silently
// dropping content would corrupt the response. An unrecognised event type is
// not an error: it becomes EventUnknown and is logged at DEBUG so new
// server-side events surface without breaking anything.
func ParseEvent(msg *Message) (*Event, error) {
	messageType := msg.MessageType()

	if messageType == "exception" || msg.ExceptionType() != "" {
		return parseException(msg.ExceptionType(), msg.Payload)
	}

	eventType := msg.EventType()
	if eventType == "" {
		// Some frames, notably the initial-response frame, carry no event type.
		slog.Debug("event stream frame has no :event-type header", "message_type", messageType)
		return &Event{Kind: EventIgnored, Type: ""}, nil
	}

	if exceptionEventTypes[eventType] {
		return parseException(eventType, msg.Payload)
	}

	switch eventType {
	case "assistantResponseEvent":
		var e AssistantResponseEvent
		if err := unmarshalEvent(eventType, msg.Payload, &e); err != nil {
			return nil, err
		}
		return &Event{Kind: EventAssistantResponse, Type: eventType, AssistantResponse: &e}, nil

	case "reasoningContentEvent":
		var e ReasoningContentEvent
		if err := unmarshalEvent(eventType, msg.Payload, &e); err != nil {
			return nil, err
		}
		return &Event{Kind: EventReasoningContent, Type: eventType, Reasoning: &e}, nil

	case "toolUseEvent":
		var e ToolUseEvent
		if err := unmarshalEvent(eventType, msg.Payload, &e); err != nil {
			return nil, err
		}
		return &Event{Kind: EventToolUse, Type: eventType, ToolUse: &e}, nil

	case "metadataEvent":
		var e MetadataEvent
		if err := unmarshalEvent(eventType, msg.Payload, &e); err != nil {
			return nil, err
		}
		return &Event{Kind: EventMetadata, Type: eventType, Metadata: &e}, nil

	case "meteringEvent":
		var e MeteringEvent
		if err := unmarshalEvent(eventType, msg.Payload, &e); err != nil {
			return nil, err
		}
		return &Event{Kind: EventMetering, Type: eventType, Metering: &e}, nil

	case "contextUsageEvent":
		var e ContextUsageEvent
		if err := unmarshalEvent(eventType, msg.Payload, &e); err != nil {
			return nil, err
		}
		return &Event{Kind: EventContextUsage, Type: eventType, ContextUsage: &e}, nil
	}

	if ignoredEventTypes[eventType] {
		slog.Debug("ignoring modelled event kirogo does not use", "event_type", eventType)
		return &Event{Kind: EventIgnored, Type: eventType}, nil
	}

	slog.Debug("unknown event type in the Kiro response stream; it was skipped",
		"event_type", eventType, "payload_bytes", len(msg.Payload))
	return &Event{Kind: EventUnknown, Type: eventType}, nil
}

// unmarshalEvent decodes an event payload, treating an empty payload as an empty
// event rather than an error: the server sends bare frames for some events.
func unmarshalEvent(eventType string, payload []byte, into any) error {
	if len(payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(payload, into); err != nil {
		return fmt.Errorf("event stream: payload of %s is not valid JSON: %w", eventType, err)
	}
	return nil
}

// parseException decodes an exception frame.
func parseException(exceptionType string, payload []byte) (*Event, error) {
	e := &ExceptionEvent{Type: exceptionType}
	if e.Type == "" {
		e.Type = "UnknownException"
	}
	if len(payload) > 0 {
		// A malformed exception body still has to surface as an error, so a
		// decode failure falls back to reporting the raw type.
		if err := json.Unmarshal(payload, e); err != nil {
			slog.Debug("exception payload is not valid JSON", "exception_type", e.Type, "error", err)
		}
	}
	return &Event{Kind: EventException, Type: e.Type, Exception: e}, nil
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.

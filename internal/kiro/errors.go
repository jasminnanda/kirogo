package kiro

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Known reason codes returned by the Kiro backend, verified against the Kiro IDE
// bundle.
const (
	ReasonContentLengthExceeded    = "CONTENT_LENGTH_EXCEEDS_THRESHOLD"
	ReasonMonthlyRequestCount      = "MONTHLY_REQUEST_COUNT"
	ReasonInvalidModelID           = "INVALID_MODEL_ID"
	ReasonThinkingSignatureInvalid = "THINKING_SIGNATURE_INVALID"
)

// improperlyFormedRequest is the backend's catch-all validation message. It is
// deliberately uninformative and can mean almost any schema violation.
const improperlyFormedRequest = "Improperly formed request."

// APIError is a non-streaming failure from the Kiro backend.
type APIError struct {
	// StatusCode is the HTTP status.
	StatusCode int
	// Message is the backend's message field.
	Message string
	// Reason is the backend's reason field, often empty.
	Reason string
	// RetryAfterMilliseconds accompanies throttling errors.
	RetryAfterMilliseconds int
	// RequestID is the x-amzn-requestid response header. AWS asks for it in bug
	// reports, so it is always captured.
	RequestID string
	// ExceptionType is the modelled exception name when the backend supplies one.
	ExceptionType string
}

// kiroErrorBody is the backend's error envelope.
type kiroErrorBody struct {
	Message                string `json:"message"`
	Reason                 string `json:"reason"`
	RetryAfterMilliseconds int    `json:"retryAfterMilliseconds"`
	// Some paths use __type or code instead of a header.
	Type string `json:"__type"`
}

// ParseAPIError builds an APIError from an HTTP failure.
//
// The body is parsed for the modelled envelope; anything unparseable is still
// reported, using the status code alone.
func ParseAPIError(status int, body []byte, header http.Header) *APIError {
	e := &APIError{StatusCode: status}
	if header != nil {
		e.RequestID = header.Get("x-amzn-requestid")
		e.ExceptionType = exceptionTypeFromHeader(header)
	}

	var parsed kiroErrorBody
	if err := json.Unmarshal(body, &parsed); err == nil {
		e.Message = strings.TrimSpace(parsed.Message)
		e.Reason = strings.TrimSpace(parsed.Reason)
		e.RetryAfterMilliseconds = parsed.RetryAfterMilliseconds
		if e.ExceptionType == "" && parsed.Type != "" {
			e.ExceptionType = shortExceptionName(parsed.Type)
		}
	}
	// "null" appears literally in some responses where a reason is absent.
	if strings.EqualFold(e.Reason, "null") {
		e.Reason = ""
	}
	return e
}

// exceptionTypeFromHeader reads the exception name AWS puts in a response header.
func exceptionTypeFromHeader(h http.Header) string {
	for _, key := range []string{"x-amzn-errortype", "x-amzn-exception-type"} {
		if v := h.Get(key); v != "" {
			return shortExceptionName(v)
		}
	}
	return ""
}

// shortExceptionName trims the AWS decorations from an exception identifier, for
// example "ValidationException:http://internal" becomes "ValidationException".
func shortExceptionName(s string) string {
	if i := strings.IndexAny(s, ":#"); i > 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// Error renders the technical form, for logs.
func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "kiro api: HTTP %d", e.StatusCode)
	if e.ExceptionType != "" {
		b.WriteString(" " + e.ExceptionType)
	}
	if e.Message != "" {
		b.WriteString(": " + e.Message)
	}
	if e.Reason != "" {
		b.WriteString(" (reason: " + e.Reason + ")")
	}
	if e.RequestID != "" {
		b.WriteString(" [request id " + e.RequestID + "]")
	}
	return b.String()
}

// IsThinkingSignatureInvalid reports whether the backend rejected a reasoning
// signature, which is recoverable by retrying once without reasoning.
func (e *APIError) IsThinkingSignatureInvalid() bool {
	return e.Reason == ReasonThinkingSignatureInvalid
}

// IsRetryable reports whether the same request could succeed on a retry.
func (e *APIError) IsRetryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

// ClientStatus maps a backend failure to the status kirogo returns to its client.
func (e *APIError) ClientStatus() int {
	switch e.Reason {
	case ReasonContentLengthExceeded:
		// The request was too large for the model, which is the client's problem
		// to solve by sending less.
		return http.StatusRequestEntityTooLarge
	case ReasonMonthlyRequestCount:
		return http.StatusTooManyRequests
	case ReasonInvalidModelID:
		return http.StatusBadRequest
	}

	switch e.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		// Upstream authorisation is kirogo's problem, not the client's
		// credential problem, so it is reported as a gateway failure.
		return http.StatusBadGateway
	case http.StatusPaymentRequired:
		return http.StatusPaymentRequired
	case http.StatusTooManyRequests:
		return http.StatusTooManyRequests
	case http.StatusBadRequest:
		return http.StatusBadRequest
	}
	if e.StatusCode >= 500 {
		return http.StatusBadGateway
	}
	return e.StatusCode
}

// UserMessage returns an explanation aimed at the person using the client,
// including what to do next.
func (e *APIError) UserMessage() string {
	switch e.Reason {
	case ReasonContentLengthExceeded:
		return "Context limit reached. The conversation exceeds this model's capacity. " +
			"Start a new session, shorten the conversation, or switch to a model with a larger context window."

	case ReasonMonthlyRequestCount:
		return "Monthly request limit exceeded. Your Kiro plan has no requests left for this billing period."

	case ReasonInvalidModelID:
		return "Invalid model, or your plan does not include it. " +
			"Run kirogo -dump-models to see the models your account can actually use."

	case ReasonThinkingSignatureInvalid:
		return "The backend rejected a reasoning signature. kirogo retries once without reasoning; " +
			"if you are seeing this message, that retry also failed. Starting a new session clears the stale reasoning history."
	}

	// The catch-all validation error. Being honest about its vagueness is more
	// useful than inventing a cause.
	if e.Message == improperlyFormedRequest && e.Reason == "" {
		msg := "The Kiro backend rejected this request as malformed but gave no reason. " +
			"That single error covers every kind of schema violation, so it needs narrowing down by hand. " +
			"Restart kirogo with LOG_LEVEL=DEBUG to capture the exact payload that was sent, then look for " +
			"an over-long tool name, a malformed tool schema, or an unusually large conversation."
		if e.RequestID != "" {
			msg += " Request id: " + e.RequestID + "."
		}
		return msg
	}

	switch e.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "The Kiro backend refused kirogo's credentials (HTTP " + fmt.Sprint(e.StatusCode) + "). " +
			"Sign in to Kiro IDE again so a fresh token is written, then restart kirogo." + e.suffix()

	case http.StatusPaymentRequired:
		return "The Kiro backend reports a billing problem (HTTP 402). Check your Kiro subscription." + e.suffix()

	case http.StatusTooManyRequests:
		msg := "The Kiro backend is rate limiting requests (HTTP 429)."
		if e.RetryAfterMilliseconds > 0 {
			msg += fmt.Sprintf(" It asked to wait %dms.", e.RetryAfterMilliseconds)
		}
		return msg + " kirogo already retried with backoff; wait a little and try again." + e.suffix()
	}

	if e.StatusCode >= 500 {
		return fmt.Sprintf("The Kiro backend returned HTTP %d. This is an upstream fault, not a problem with your request; retry shortly.", e.StatusCode) + e.suffix()
	}

	if e.Message != "" {
		out := e.Message
		if e.Reason != "" {
			out += " (reason: " + e.Reason + ")"
		}
		return out + e.suffix()
	}
	return fmt.Sprintf("The Kiro backend returned HTTP %d with no explanation.", e.StatusCode) + e.suffix()
}

// suffix appends the request id when there is one, since AWS asks for it.
func (e *APIError) suffix() string {
	if e.RequestID == "" {
		return ""
	}
	return " Request id: " + e.RequestID + "."
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.

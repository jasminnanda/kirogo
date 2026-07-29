package kiro

import (
	"net/http"
	"strings"
	"testing"
)

func TestParseAPIErrorEnvelope(t *testing.T) {
	header := http.Header{
		"X-Amzn-Requestid": []string{"req-abc"},
		"X-Amzn-Errortype": []string{"ValidationException:http://internal.amazon.com/coral/"},
	}
	body := []byte(`{"message":"Input is too long.","reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD","retryAfterMilliseconds":250}`)

	e := ParseAPIError(http.StatusBadRequest, body, header)
	if e.StatusCode != http.StatusBadRequest {
		t.Errorf("StatusCode = %d", e.StatusCode)
	}
	if e.Message != "Input is too long." {
		t.Errorf("Message = %q", e.Message)
	}
	if e.Reason != ReasonContentLengthExceeded {
		t.Errorf("Reason = %q", e.Reason)
	}
	if e.RetryAfterMilliseconds != 250 {
		t.Errorf("RetryAfterMilliseconds = %d", e.RetryAfterMilliseconds)
	}
	if e.RequestID != "req-abc" {
		t.Errorf("RequestID = %q", e.RequestID)
	}
	if e.ExceptionType != "ValidationException" {
		t.Errorf("ExceptionType = %q, want the AWS decorations trimmed", e.ExceptionType)
	}
}

func TestParseAPIErrorTreatsLiteralNullReasonAsAbsent(t *testing.T) {
	for _, raw := range []string{`{"message":"Improperly formed request.","reason":"null"}`,
		`{"message":"Improperly formed request.","reason":"NULL"}`,
		`{"message":"Improperly formed request.","reason":null}`,
		`{"message":"Improperly formed request."}`} {
		e := ParseAPIError(http.StatusBadRequest, []byte(raw), nil)
		if e.Reason != "" {
			t.Errorf("for body %s, Reason = %q, want empty", raw, e.Reason)
		}
	}
}

func TestParseAPIErrorWithUnparseableBody(t *testing.T) {
	e := ParseAPIError(http.StatusBadGateway, []byte("<html>502 Bad Gateway</html>"), nil)
	if e.StatusCode != http.StatusBadGateway {
		t.Errorf("StatusCode = %d", e.StatusCode)
	}
	if e.Message != "" {
		t.Errorf("Message = %q, want empty for an unparseable body", e.Message)
	}
	if !strings.Contains(e.UserMessage(), "502") {
		t.Errorf("UserMessage() = %q, should still name the status", e.UserMessage())
	}
}

func TestParseAPIErrorWithEmptyBody(t *testing.T) {
	e := ParseAPIError(http.StatusInternalServerError, nil, nil)
	if e.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d", e.StatusCode)
	}
	if e.UserMessage() == "" {
		t.Error("UserMessage() should never be empty")
	}
}

func TestParseAPIErrorUsesTypeFieldWhenNoHeader(t *testing.T) {
	e := ParseAPIError(http.StatusBadRequest, []byte(`{"__type":"ThrottlingException:x","message":"slow"}`), http.Header{})
	if e.ExceptionType != "ThrottlingException" {
		t.Errorf("ExceptionType = %q", e.ExceptionType)
	}
}

func TestAPIErrorErrorString(t *testing.T) {
	e := &APIError{
		StatusCode:    429,
		ExceptionType: "ThrottlingException",
		Message:       "Too many requests",
		Reason:        "THROTTLED",
		RequestID:     "rid",
	}
	got := e.Error()
	for _, want := range []string{"HTTP 429", "ThrottlingException", "Too many requests", "reason: THROTTLED", "request id rid"} {
		if !strings.Contains(got, want) {
			t.Errorf("Error() = %q, should contain %q", got, want)
		}
	}
}

func TestAPIErrorErrorStringMinimal(t *testing.T) {
	e := &APIError{StatusCode: 500}
	if got := e.Error(); got != "kiro api: HTTP 500" {
		t.Errorf("Error() = %q", got)
	}
}

func TestIsThinkingSignatureInvalid(t *testing.T) {
	yes := &APIError{Reason: ReasonThinkingSignatureInvalid}
	if !yes.IsThinkingSignatureInvalid() {
		t.Error("should recognise THINKING_SIGNATURE_INVALID")
	}
	for _, reason := range []string{"", "CONTENT_LENGTH_EXCEEDS_THRESHOLD", "thinking_signature_invalid"} {
		e := &APIError{Reason: reason}
		if e.IsThinkingSignatureInvalid() {
			t.Errorf("reason %q should not match", reason)
		}
	}
}

func TestIsRetryable(t *testing.T) {
	cases := map[int]bool{
		200: false, 400: false, 401: false, 402: false, 403: false, 404: false,
		413: false, 429: true, 500: true, 502: true, 503: true, 504: true, 599: true,
	}
	for status, want := range cases {
		e := &APIError{StatusCode: status}
		if got := e.IsRetryable(); got != want {
			t.Errorf("IsRetryable() for %d = %v, want %v", status, got, want)
		}
	}
}

func TestClientStatusMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        APIError
		wantStatus int
	}{
		{"context limit becomes 413", APIError{StatusCode: 400, Reason: ReasonContentLengthExceeded}, http.StatusRequestEntityTooLarge},
		{"monthly quota becomes 429", APIError{StatusCode: 400, Reason: ReasonMonthlyRequestCount}, http.StatusTooManyRequests},
		{"invalid model stays 400", APIError{StatusCode: 400, Reason: ReasonInvalidModelID}, http.StatusBadRequest},
		{"upstream 401 becomes 502", APIError{StatusCode: 401}, http.StatusBadGateway},
		{"upstream 403 becomes 502", APIError{StatusCode: 403}, http.StatusBadGateway},
		{"402 passes through", APIError{StatusCode: 402}, http.StatusPaymentRequired},
		{"429 passes through", APIError{StatusCode: 429}, http.StatusTooManyRequests},
		{"400 passes through", APIError{StatusCode: 400}, http.StatusBadRequest},
		{"500 becomes 502", APIError{StatusCode: 500}, http.StatusBadGateway},
		{"503 becomes 502", APIError{StatusCode: 503}, http.StatusBadGateway},
		{"404 passes through", APIError{StatusCode: 404}, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.ClientStatus(); got != tc.wantStatus {
				t.Errorf("ClientStatus() = %d, want %d", got, tc.wantStatus)
			}
		})
	}
}

func TestUserMessageForKnownReasons(t *testing.T) {
	cases := []struct {
		reason   string
		mustHave []string
	}{
		{ReasonContentLengthExceeded, []string{"Context limit reached", "new session", "larger context window"}},
		{ReasonMonthlyRequestCount, []string{"Monthly request limit exceeded"}},
		{ReasonInvalidModelID, []string{"Invalid model", "plan does not include it", "-dump-models"}},
		{ReasonThinkingSignatureInvalid, []string{"reasoning signature", "retries once", "new session"}},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			e := &APIError{StatusCode: 400, Message: "raw upstream text", Reason: tc.reason}
			got := e.UserMessage()
			for _, want := range tc.mustHave {
				if !strings.Contains(got, want) {
					t.Errorf("UserMessage() = %q, should contain %q", got, want)
				}
			}
		})
	}
}

func TestUserMessageForImproperlyFormedRequest(t *testing.T) {
	e := &APIError{StatusCode: 400, Message: improperlyFormedRequest, RequestID: "rid-77"}
	got := e.UserMessage()

	// It must be honest about the vagueness and tell the user how to investigate.
	for _, want := range []string{
		"rejected this request as malformed",
		"gave no reason",
		"LOG_LEVEL=DEBUG",
		"tool name",
		"rid-77",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("UserMessage() = %q, should contain %q", got, want)
		}
	}
}

func TestUserMessageForImproperlyFormedRequestWithAReasonUsesTheReason(t *testing.T) {
	e := &APIError{StatusCode: 400, Message: improperlyFormedRequest, Reason: ReasonInvalidModelID}
	got := e.UserMessage()
	if strings.Contains(got, "gave no reason") {
		t.Errorf("a request with a reason should not use the vague message, got %q", got)
	}
	if !strings.Contains(got, "Invalid model") {
		t.Errorf("UserMessage() = %q, want the reason-specific text", got)
	}
}

func TestUserMessageForStatusCodes(t *testing.T) {
	cases := []struct {
		name     string
		err      APIError
		mustHave []string
	}{
		{"401", APIError{StatusCode: 401}, []string{"refused kirogo's credentials", "Sign in to Kiro IDE"}},
		{"403", APIError{StatusCode: 403}, []string{"refused kirogo's credentials", "403"}},
		{"402", APIError{StatusCode: 402}, []string{"billing problem", "subscription"}},
		{"429 plain", APIError{StatusCode: 429}, []string{"rate limiting", "already retried"}},
		{"429 with retry-after", APIError{StatusCode: 429, RetryAfterMilliseconds: 1500}, []string{"1500ms"}},
		{"500", APIError{StatusCode: 500}, []string{"HTTP 500", "upstream fault"}},
		{"503", APIError{StatusCode: 503}, []string{"HTTP 503", "retry shortly"}},
		{"message passthrough", APIError{StatusCode: 409, Message: "conflict happened"}, []string{"conflict happened"}},
		{"message with reason", APIError{StatusCode: 409, Message: "boom", Reason: "WEIRD"}, []string{"boom", "reason: WEIRD"}},
		{"no message", APIError{StatusCode: 418}, []string{"HTTP 418", "no explanation"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.err.UserMessage()
			for _, want := range tc.mustHave {
				if !strings.Contains(got, want) {
					t.Errorf("UserMessage() = %q, should contain %q", got, want)
				}
			}
		})
	}
}

func TestUserMessageAppendsRequestID(t *testing.T) {
	with := &APIError{StatusCode: 500, RequestID: "rid-42"}
	if !strings.Contains(with.UserMessage(), "Request id: rid-42.") {
		t.Errorf("UserMessage() = %q, want the request id appended", with.UserMessage())
	}
	without := &APIError{StatusCode: 500}
	if strings.Contains(without.UserMessage(), "Request id") {
		t.Errorf("UserMessage() = %q, should omit an empty request id", without.UserMessage())
	}
}

func TestUserMessageIsNeverEmpty(t *testing.T) {
	statuses := []int{200, 400, 401, 402, 403, 404, 409, 413, 418, 429, 500, 502, 503, 504}
	reasons := []string{"", ReasonContentLengthExceeded, ReasonMonthlyRequestCount, ReasonInvalidModelID,
		ReasonThinkingSignatureInvalid, "SOME_UNKNOWN_REASON"}
	for _, status := range statuses {
		for _, reason := range reasons {
			e := &APIError{StatusCode: status, Reason: reason}
			if e.UserMessage() == "" {
				t.Errorf("UserMessage() is empty for status %d reason %q", status, reason)
			}
		}
	}
}

func TestShortExceptionName(t *testing.T) {
	cases := map[string]string{
		"ValidationException": "ValidationException",
		"ValidationException:http://internal.amazon.com/coral/": "ValidationException",
		"com.amazon#ThrottlingException":                        "com.amazon",
		"  Padded  ":                                            "Padded",
		"":                                                      "",
	}
	for in, want := range cases {
		if got := shortExceptionName(in); got != want {
			t.Errorf("shortExceptionName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExceptionTypeFromHeader(t *testing.T) {
	cases := []struct {
		name   string
		header http.Header
		want   string
	}{
		{"errortype", http.Header{"X-Amzn-Errortype": []string{"ThrottlingException"}}, "ThrottlingException"},
		{"exception-type", http.Header{"X-Amzn-Exception-Type": []string{"ValidationException"}}, "ValidationException"},
		{"none", http.Header{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exceptionTypeFromHeader(tc.header); got != tc.want {
				t.Errorf("exceptionTypeFromHeader() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClassifyTransportError(t *testing.T) {
	// The categories are asserted through transportError, which is what callers
	// actually see, plus the nil case.
	if got := classifyTransportError(nil); got != "" {
		t.Errorf("classifyTransportError(nil) = %q, want empty", got)
	}
}

func TestTransportErrorAdviceMentionsTheHost(t *testing.T) {
	err := transportError("https://runtime.us-east-1.kiro.dev/generateAssistantResponse", errTestNetwork{})
	msg := err.Error()
	if !strings.Contains(msg, "runtime.us-east-1.kiro.dev") {
		t.Errorf("error = %q, should name the host", msg)
	}
	if !strings.Contains(msg, "HTTPS_PROXY") && !strings.Contains(msg, "could not complete") {
		t.Errorf("error = %q, should give actionable advice", msg)
	}
}

func TestEndpointHost(t *testing.T) {
	cases := map[string]string{
		"https://runtime.us-east-1.kiro.dev/x": "runtime.us-east-1.kiro.dev",
		"http://127.0.0.1:8000/y":              "127.0.0.1:8000",
		"not a url":                            "not a url",
	}
	for in, want := range cases {
		if got := endpointHost(in); got != want {
			t.Errorf("endpointHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// errTestNetwork is a plain error used to exercise the default advice branch.
type errTestNetwork struct{}

func (errTestNetwork) Error() string { return "some unexpected failure" }

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.

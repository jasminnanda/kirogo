package kiro

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// parseFrame builds an event frame and parses it into a typed event.
func parseFrame(t *testing.T, eventType, payload string) *Event {
	t.Helper()
	frame := eventFrame(t, eventType, payload)
	msgs := decodeAll(t, frame)
	if len(msgs) != 1 {
		t.Fatalf("decoded %d messages, want 1", len(msgs))
	}
	ev, err := ParseEvent(msgs[0])
	if err != nil {
		t.Fatalf("ParseEvent(%s): %v", eventType, err)
	}
	return ev
}

func TestParseAssistantResponseEvent(t *testing.T) {
	ev := parseFrame(t, "assistantResponseEvent", `{"content":"Hello","modelId":"claude-opus-5"}`)
	if ev.Kind != EventAssistantResponse {
		t.Fatalf("Kind = %v, want EventAssistantResponse", ev.Kind)
	}
	if ev.AssistantResponse.Content != "Hello" {
		t.Errorf("Content = %q", ev.AssistantResponse.Content)
	}
	if ev.AssistantResponse.ModelID != "claude-opus-5" {
		t.Errorf("ModelID = %q", ev.AssistantResponse.ModelID)
	}
}

func TestParseReasoningContentEvent(t *testing.T) {
	blob := base64.StdEncoding.EncodeToString([]byte{0x01, 0x02, 0xff})
	ev := parseFrame(t, "reasoningContentEvent",
		`{"text":"thinking hard","signature":"sig-abc","redactedContent":"`+blob+`"}`)

	if ev.Kind != EventReasoningContent {
		t.Fatalf("Kind = %v", ev.Kind)
	}
	r := ev.Reasoning
	if r.Text != "thinking hard" {
		t.Errorf("Text = %q", r.Text)
	}
	if r.Signature != "sig-abc" {
		t.Errorf("Signature = %q", r.Signature)
	}
	if !bytes.Equal(r.RedactedContent, []byte{0x01, 0x02, 0xff}) {
		t.Errorf("RedactedContent = %v, want the base64 blob decoded", r.RedactedContent)
	}
}

func TestParseReasoningWithoutSignature(t *testing.T) {
	ev := parseFrame(t, "reasoningContentEvent", `{"text":"unsigned thought"}`)
	if ev.Reasoning.Signature != "" {
		t.Errorf("Signature = %q, want empty", ev.Reasoning.Signature)
	}
	if ev.Reasoning.Text != "unsigned thought" {
		t.Errorf("Text = %q", ev.Reasoning.Text)
	}
}

func TestParseToolUseEvent(t *testing.T) {
	ev := parseFrame(t, "toolUseEvent",
		`{"toolUseId":"tu-1","name":"get_weather","input":"{\"city\":\"Berlin\"}","stop":true}`)
	if ev.Kind != EventToolUse {
		t.Fatalf("Kind = %v", ev.Kind)
	}
	tu := ev.ToolUse
	if tu.ToolUseID != "tu-1" || tu.Name != "get_weather" {
		t.Errorf("tool identity = %+v", tu)
	}
	if tu.Input != `{"city":"Berlin"}` {
		t.Errorf("Input = %q", tu.Input)
	}
	if !tu.Stop {
		t.Error("Stop should be true")
	}
}

func TestParseToolUseFragmentHasStopFalse(t *testing.T) {
	ev := parseFrame(t, "toolUseEvent", `{"toolUseId":"tu-1","name":"f","input":"{\"a\":"}`)
	if ev.ToolUse.Stop {
		t.Error("a fragment must have Stop false")
	}
}

func TestParseMetadataEvent(t *testing.T) {
	ev := parseFrame(t, "metadataEvent", `{
	  "tokenUsage": {
	    "uncachedInputTokens": 100,
	    "outputTokens": 42,
	    "totalTokens": 700,
	    "cacheReadInputTokens": 500,
	    "cacheWriteInputTokens": 58,
	    "contextUsagePercentage": 12.5,
	    "normalizedTokenUsage": 123
	  },
	  "stopReason": "end_turn",
	  "stopDetails": {"foo":"bar"}
	}`)

	if ev.Kind != EventMetadata {
		t.Fatalf("Kind = %v", ev.Kind)
	}
	u := ev.Metadata.TokenUsage
	if u == nil {
		t.Fatal("TokenUsage is nil")
	}
	if got := u.PromptTokens(); got != 658 {
		t.Errorf("PromptTokens() = %d, want 658 (100 + 500 + 58)", got)
	}
	if got := u.Total(); got != 700 {
		t.Errorf("Total() = %d, want the reported 700", got)
	}
	if u.OutputTokens != 42 {
		t.Errorf("OutputTokens = %d", u.OutputTokens)
	}
	if u.ContextUsagePercentage != 12.5 {
		t.Errorf("ContextUsagePercentage = %v", u.ContextUsagePercentage)
	}
	if ev.Metadata.StopReason != "end_turn" {
		t.Errorf("StopReason = %q", ev.Metadata.StopReason)
	}
	if ev.Metadata.StopDetails["foo"] != "bar" {
		t.Errorf("StopDetails = %v", ev.Metadata.StopDetails)
	}
}

func TestTokenUsageTotalFallsBackToTheSum(t *testing.T) {
	u := TokenUsage{UncachedInputTokens: 10, CacheReadInputTokens: 5, CacheWriteInputTokens: 2, OutputTokens: 3}
	if got := u.Total(); got != 20 {
		t.Errorf("Total() = %d, want 20 when totalTokens is absent", got)
	}
	empty := TokenUsage{}
	if got := empty.Total(); got != 0 {
		t.Errorf("Total() on an empty usage = %d, want 0", got)
	}
}

func TestParseMetadataEventWithoutUsage(t *testing.T) {
	ev := parseFrame(t, "metadataEvent", `{"stopReason":"max_tokens"}`)
	if ev.Metadata.TokenUsage != nil {
		t.Errorf("TokenUsage = %+v, want nil", ev.Metadata.TokenUsage)
	}
	if ev.Metadata.StopReason != "max_tokens" {
		t.Errorf("StopReason = %q", ev.Metadata.StopReason)
	}
}

func TestParseMeteringEvent(t *testing.T) {
	ev := parseFrame(t, "meteringEvent", `{"usage":2.2,"unit":"credit","unitPlural":"credits"}`)
	if ev.Kind != EventMetering {
		t.Fatalf("Kind = %v", ev.Kind)
	}
	if ev.Metering.Usage != 2.2 || ev.Metering.Unit != "credit" || ev.Metering.UnitPlural != "credits" {
		t.Errorf("Metering = %+v", ev.Metering)
	}
}

func TestParseContextUsageEvent(t *testing.T) {
	ev := parseFrame(t, "contextUsageEvent", `{"contextUsagePercentage":2.86}`)
	if ev.Kind != EventContextUsage {
		t.Fatalf("Kind = %v", ev.Kind)
	}
	if ev.ContextUsage.ContextUsagePercentage != 2.86 {
		t.Errorf("ContextUsagePercentage = %v", ev.ContextUsage.ContextUsagePercentage)
	}
}

func TestModelledButUnusedEventsAreDiscarded(t *testing.T) {
	ignored := []string{
		"messageMetadataEvent", "dryRunSucceedEvent", "codeReferenceEvent",
		"supplementaryWebLinksEvent", "followupPromptEvent", "codeEvent",
		"intentsEvent", "interactionComponentsEvent", "toolResultEvent",
		"citationEvent", "documentCitationEvent", "invalidStateEvent",
	}
	for _, eventType := range ignored {
		t.Run(eventType, func(t *testing.T) {
			ev := parseFrame(t, eventType, `{"anything":true,"nested":{"deep":[1,2,3]}}`)
			if ev.Kind != EventIgnored {
				t.Errorf("Kind = %v, want EventIgnored", ev.Kind)
			}
			if ev.Type != eventType {
				t.Errorf("Type = %q, want %q", ev.Type, eventType)
			}
		})
	}
}

func TestUnknownEventTypeIsLoggedNotFatal(t *testing.T) {
	buf := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previous)

	ev := parseFrame(t, "brandNewEventNobodyHasSeen", `{"surprise":true}`)
	if ev.Kind != EventUnknown {
		t.Errorf("Kind = %v, want EventUnknown", ev.Kind)
	}
	if ev.Type != "brandNewEventNobodyHasSeen" {
		t.Errorf("Type = %q", ev.Type)
	}
	logged := buf.String()
	if !strings.Contains(logged, "unknown event type") {
		t.Errorf("expected a DEBUG line about the unknown event, got:\n%s", logged)
	}
	if !strings.Contains(logged, "brandNewEventNobodyHasSeen") {
		t.Errorf("the log should name the event type, got:\n%s", logged)
	}
}

func TestExceptionMessageType(t *testing.T) {
	frame, err := EncodeMessage([]Header{
		StringHeader(":message-type", "exception"),
		StringHeader(":exception-type", "ThrottlingException"),
		StringHeader(":content-type", "application/json"),
	}, []byte(`{"message":"Too many requests","reason":"THROTTLED","retryAfterMilliseconds":1500}`))
	if err != nil {
		t.Fatal(err)
	}

	msgs := decodeAll(t, frame)
	ev, parseErr := ParseEvent(msgs[0])
	if parseErr != nil {
		t.Fatalf("ParseEvent: %v", parseErr)
	}
	if ev.Kind != EventException {
		t.Fatalf("Kind = %v, want EventException", ev.Kind)
	}
	e := ev.Exception
	if e.Type != "ThrottlingException" {
		t.Errorf("Type = %q", e.Type)
	}
	if e.Message != "Too many requests" {
		t.Errorf("Message = %q", e.Message)
	}
	if e.Reason != "THROTTLED" {
		t.Errorf("Reason = %q", e.Reason)
	}
	if e.RetryAfterMilliseconds != 1500 {
		t.Errorf("RetryAfterMilliseconds = %d", e.RetryAfterMilliseconds)
	}
	if !strings.Contains(e.Error(), "ThrottlingException") || !strings.Contains(e.Error(), "THROTTLED") {
		t.Errorf("Error() = %q, should include the type and reason", e.Error())
	}
}

func TestInternalServerExceptionArrivesAsAnEvent(t *testing.T) {
	ev := parseFrame(t, "internalServerException", `{"message":"internal failure"}`)
	if ev.Kind != EventException {
		t.Fatalf("Kind = %v, want EventException", ev.Kind)
	}
	if ev.Exception.Type != "internalServerException" {
		t.Errorf("Type = %q", ev.Exception.Type)
	}
	if ev.Exception.Message != "internal failure" {
		t.Errorf("Message = %q", ev.Exception.Message)
	}
}

func TestExceptionWithoutTypeHeader(t *testing.T) {
	frame, err := EncodeMessage([]Header{
		StringHeader(":message-type", "exception"),
	}, []byte(`{"message":"mystery"}`))
	if err != nil {
		t.Fatal(err)
	}
	msgs := decodeAll(t, frame)
	ev, parseErr := ParseEvent(msgs[0])
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	if ev.Exception.Type != "UnknownException" {
		t.Errorf("Type = %q, want UnknownException as a placeholder", ev.Exception.Type)
	}
}

func TestExceptionWithMalformedBodyStillSurfaces(t *testing.T) {
	frame, err := EncodeMessage([]Header{
		StringHeader(":message-type", "exception"),
		StringHeader(":exception-type", "ValidationException"),
	}, []byte(`<not json>`))
	if err != nil {
		t.Fatal(err)
	}
	msgs := decodeAll(t, frame)
	ev, parseErr := ParseEvent(msgs[0])
	if parseErr != nil {
		t.Fatalf("a malformed exception body must not be swallowed as a parse error: %v", parseErr)
	}
	if ev.Kind != EventException {
		t.Fatalf("Kind = %v, want EventException", ev.Kind)
	}
	if ev.Exception.Type != "ValidationException" {
		t.Errorf("Type = %q", ev.Exception.Type)
	}
	if !strings.Contains(ev.Exception.Error(), "ValidationException") {
		t.Errorf("Error() = %q", ev.Exception.Error())
	}
}

func TestExceptionErrorRendering(t *testing.T) {
	cases := []struct {
		name string
		e    ExceptionEvent
		want string
	}{
		{"type message reason", ExceptionEvent{Type: "T", Message: "m", Reason: "R"}, "T: m (reason: R)"},
		{"type and message", ExceptionEvent{Type: "T", Message: "m"}, "T: m"},
		{"type only", ExceptionEvent{Type: "T"}, "T"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.e.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
	// It must satisfy the error interface.
	var err error = &ExceptionEvent{Type: "T", Message: "m"}
	if err.Error() == "" {
		t.Error("ExceptionEvent should be usable as an error")
	}
}

func TestMalformedEventPayloadIsAnError(t *testing.T) {
	consumed := []string{
		"assistantResponseEvent", "reasoningContentEvent", "toolUseEvent",
		"metadataEvent", "meteringEvent", "contextUsageEvent",
	}
	for _, eventType := range consumed {
		t.Run(eventType, func(t *testing.T) {
			frame := eventFrame(t, eventType, `{"content": `)
			msgs := decodeAll(t, frame)
			_, err := ParseEvent(msgs[0])
			if err == nil {
				t.Fatal("a truncated payload for a consumed event must be an error, not silent data loss")
			}
			if !strings.Contains(err.Error(), eventType) {
				t.Errorf("error %q should name the event type", err)
			}
		})
	}
}

func TestWrongTypeInPayloadIsAnError(t *testing.T) {
	frame := eventFrame(t, "assistantResponseEvent", `{"content": 12345}`)
	msgs := decodeAll(t, frame)
	if _, err := ParseEvent(msgs[0]); err == nil {
		t.Error("a numeric content field should fail to decode into a string")
	}
}

func TestEmptyPayloadIsTreatedAsAnEmptyEvent(t *testing.T) {
	frame := eventFrame(t, "assistantResponseEvent", ``)
	msgs := decodeAll(t, frame)
	ev, err := ParseEvent(msgs[0])
	if err != nil {
		t.Fatalf("an empty payload should not be an error: %v", err)
	}
	if ev.Kind != EventAssistantResponse {
		t.Errorf("Kind = %v", ev.Kind)
	}
	if ev.AssistantResponse.Content != "" {
		t.Errorf("Content = %q, want empty", ev.AssistantResponse.Content)
	}
}

func TestFrameWithNoEventTypeIsIgnored(t *testing.T) {
	frame, err := EncodeMessage([]Header{StringHeader(":message-type", "event")}, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	msgs := decodeAll(t, frame)
	ev, parseErr := ParseEvent(msgs[0])
	if parseErr != nil {
		t.Fatalf("ParseEvent: %v", parseErr)
	}
	if ev.Kind != EventIgnored {
		t.Errorf("Kind = %v, want EventIgnored for a frame with no :event-type", ev.Kind)
	}
}

func TestEventKindString(t *testing.T) {
	cases := map[EventKind]string{
		EventAssistantResponse: "assistantResponse",
		EventReasoningContent:  "reasoningContent",
		EventToolUse:           "toolUse",
		EventMetadata:          "metadata",
		EventMetering:          "metering",
		EventContextUsage:      "contextUsage",
		EventException:         "exception",
		EventUnknown:           "unknown",
		EventIgnored:           "ignored",
	}
	for kind, want := range cases {
		if got := kind.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int(kind), got, want)
		}
	}
}

func TestCapturedStreamDecodesToTheExpectedTypedSequence(t *testing.T) {
	// A full, realistic response: reasoning, then text, then a tool call in
	// fragments, then the trailing accounting events.
	fixtures := []struct{ eventType, payload string }{
		{"messageMetadataEvent", `{"conversationId":"conv-1"}`},
		{"reasoningContentEvent", `{"text":"The user wants the weather."}`},
		{"reasoningContentEvent", `{"text":" I should call the tool.","signature":"sig-final"}`},
		{"assistantResponseEvent", `{"content":"Let me check."}`},
		{"toolUseEvent", `{"toolUseId":"tu-9","name":"get_weather","input":"{\"city"}`},
		{"toolUseEvent", `{"toolUseId":"tu-9","name":"get_weather","input":"\":\"Berlin\"}"}`},
		{"toolUseEvent", `{"toolUseId":"tu-9","name":"get_weather","input":"","stop":true}`},
		{"contextUsageEvent", `{"contextUsagePercentage":3.14}`},
		{"meteringEvent", `{"usage":2.2,"unit":"credit","unitPlural":"credits"}`},
		{"metadataEvent", `{"tokenUsage":{"uncachedInputTokens":120,"outputTokens":31,"totalTokens":151},"stopReason":"tool_use"}`},
	}
	var stream []byte
	for _, f := range fixtures {
		stream = append(stream, eventFrame(t, f.eventType, f.payload)...)
	}

	wantKinds := []EventKind{
		EventIgnored,
		EventReasoningContent, EventReasoningContent,
		EventAssistantResponse,
		EventToolUse, EventToolUse, EventToolUse,
		EventContextUsage, EventMetering, EventMetadata,
	}

	// Decode via the Reader, the same path streaming uses.
	r := NewReader(bytes.NewReader(stream))
	var kinds []EventKind
	var text, reasoning, toolInput, signature string
	var usage *TokenUsage
	var credits float64
	var stopReason string

	for {
		msg, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Reader.Next: %v", err)
		}
		ev, err := ParseEvent(msg)
		if err != nil {
			t.Fatalf("ParseEvent: %v", err)
		}
		kinds = append(kinds, ev.Kind)

		switch ev.Kind {
		case EventAssistantResponse:
			text += ev.AssistantResponse.Content
		case EventReasoningContent:
			reasoning += ev.Reasoning.Text
			if ev.Reasoning.Signature != "" {
				signature = ev.Reasoning.Signature
			}
		case EventToolUse:
			toolInput += ev.ToolUse.Input
		case EventMetadata:
			usage = ev.Metadata.TokenUsage
			stopReason = ev.Metadata.StopReason
		case EventMetering:
			credits = ev.Metering.Usage
		}
	}

	if len(kinds) != len(wantKinds) {
		t.Fatalf("decoded %d events, want %d", len(kinds), len(wantKinds))
	}
	for i := range kinds {
		if kinds[i] != wantKinds[i] {
			t.Errorf("event %d kind = %v, want %v", i, kinds[i], wantKinds[i])
		}
	}
	if text != "Let me check." {
		t.Errorf("assembled text = %q", text)
	}
	if reasoning != "The user wants the weather. I should call the tool." {
		t.Errorf("assembled reasoning = %q", reasoning)
	}
	if signature != "sig-final" {
		t.Errorf("signature = %q, want the last non-empty value", signature)
	}
	if toolInput != `{"city":"Berlin"}` {
		t.Errorf("assembled tool input = %q", toolInput)
	}
	if usage == nil || usage.Total() != 151 || usage.OutputTokens != 31 {
		t.Errorf("usage = %+v", usage)
	}
	if credits != 2.2 {
		t.Errorf("credits = %v", credits)
	}
	if stopReason != "tool_use" {
		t.Errorf("stopReason = %q", stopReason)
	}
}

func TestByteWiseFeedingProducesIdenticalTypedEvents(t *testing.T) {
	fixtures := []struct{ eventType, payload string }{
		{"reasoningContentEvent", `{"text":"a","signature":"s"}`},
		{"assistantResponseEvent", `{"content":"b"}`},
		{"toolUseEvent", `{"toolUseId":"t","name":"n","input":"{}","stop":true}`},
		{"metadataEvent", `{"tokenUsage":{"outputTokens":1,"totalTokens":2},"stopReason":"end_turn"}`},
	}
	var stream []byte
	for _, f := range fixtures {
		stream = append(stream, eventFrame(t, f.eventType, f.payload)...)
	}

	collect := func(msgs []*Message) []string {
		var out []string
		for _, m := range msgs {
			ev, err := ParseEvent(m)
			if err != nil {
				t.Fatalf("ParseEvent: %v", err)
			}
			out = append(out, ev.Kind.String()+"|"+ev.Type+"|"+string(m.Payload))
		}
		return out
	}

	whole := collect(decodeAll(t, stream))
	drip := collect(decodeByteAtATime(t, stream))

	if len(whole) != len(fixtures) {
		t.Fatalf("whole-buffer produced %d events, want %d", len(whole), len(fixtures))
	}
	for i := range whole {
		if whole[i] != drip[i] {
			t.Errorf("event %d differs:\n whole: %s\n  drip: %s", i, whole[i], drip[i])
		}
	}
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.

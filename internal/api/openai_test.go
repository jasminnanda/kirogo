package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kirogo/internal/kiro"
	"kirogo/internal/translate"
)

// postChatJSON sends a non-streaming request and parses the completion.
func postChatJSON(t *testing.T, s *Server, body string) (int, openAICompletion, string) {
	t.Helper()
	rec := postChat(t, s, body)
	var parsed openAICompletion
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
			t.Fatalf("response is not a completion: %v\n%s", err, rec.Body.String())
		}
	}
	return rec.Code, parsed, rec.Body.String()
}

func TestNonStreamingCompletionShape(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"reasoningContentEvent", `{"text":"Thinking.","signature":"sig-1"}`},
		{"assistantResponseEvent", `{"content":"Hello, "}`},
		{"assistantResponseEvent", `{"content":"world!"}`},
		{"meteringEvent", `{"usage":1.5,"unit":"credit","unitPlural":"credits"}`},
		{"metadataEvent", `{"tokenUsage":{"uncachedInputTokens":10,"outputTokens":5,"totalTokens":15},"stopReason":"end_turn"}`},
	}})
	s := newHarness(t, up, testServerOptions{})

	status, completion, raw := postChatJSON(t, s, simpleChatBody(false))
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}

	if completion.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", completion.Object)
	}
	if !strings.HasPrefix(completion.ID, "chatcmpl-") {
		t.Errorf("id = %q", completion.ID)
	}
	if completion.Created == 0 {
		t.Error("created is missing")
	}
	if completion.Model != "claude-opus-5" {
		t.Errorf("model = %q, want the model as requested", completion.Model)
	}
	if len(completion.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(completion.Choices))
	}

	choice := completion.Choices[0]
	if choice.Index != 0 {
		t.Errorf("index = %d", choice.Index)
	}
	if choice.FinishReason != "stop" {
		t.Errorf("finish_reason = %q", choice.FinishReason)
	}
	if choice.Message.Role != "assistant" {
		t.Errorf("role = %q", choice.Message.Role)
	}
	if choice.Message.Content != "Hello, world!" {
		t.Errorf("content = %q, want the deltas assembled", choice.Message.Content)
	}
	if choice.Message.ReasoningContent != "Thinking." {
		t.Errorf("reasoning_content = %q", choice.Message.ReasoningContent)
	}
	if choice.Message.ReasoningSignature != "sig-1" {
		t.Errorf("reasoning_signature = %q, want it exposed for the next turn", choice.Message.ReasoningSignature)
	}

	if completion.Usage == nil {
		t.Fatal("usage is missing")
	}
	if completion.Usage.PromptTokens != 10 || completion.Usage.CompletionTokens != 5 || completion.Usage.TotalTokens != 15 {
		t.Errorf("usage = %+v", completion.Usage)
	}
	if completion.Usage.CreditsUsed == nil || *completion.Usage.CreditsUsed != 1.5 {
		t.Errorf("credits_used = %v", completion.Usage.CreditsUsed)
	}
}

func TestNonStreamingToolCallsHaveNoIndex(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"toolUseEvent", `{"toolUseId":"tu-1","name":"get_weather","input":"{\"city\":\"Berlin\"}","stop":true}`},
		{"metadataEvent", `{"tokenUsage":{"uncachedInputTokens":1,"outputTokens":1,"totalTokens":2},"stopReason":"tool_use"}`},
	}})
	s := newHarness(t, up, testServerOptions{})

	status, completion, raw := postChatJSON(t, s, simpleChatBody(false))
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, raw)
	}

	calls := completion.Choices[0].Message.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %d, want 1", len(calls))
	}
	if calls[0].ID != "tu-1" || calls[0].Type != "function" {
		t.Errorf("tool call = %+v", calls[0])
	}
	if calls[0].Function.Arguments != `{"city":"Berlin"}` {
		t.Errorf("arguments = %q", calls[0].Function.Arguments)
	}
	if completion.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q", completion.Choices[0].FinishReason)
	}
	// The non-streaming shape has no index field, unlike the streaming deltas.
	if strings.Contains(raw, `"index"`) && strings.Contains(raw, `"tool_calls"`) {
		var probe struct {
			Choices []struct {
				Message struct {
					ToolCalls []map[string]any `json:"tool_calls"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(raw), &probe); err == nil {
			for _, c := range probe.Choices[0].Message.ToolCalls {
				if _, present := c["index"]; present {
					t.Errorf("a non-streaming tool call must not carry an index: %v", c)
				}
			}
		}
	}
}

func TestNonStreamingRedactedReasoning(t *testing.T) {
	blob := base64.StdEncoding.EncodeToString([]byte{7, 8, 9})
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"reasoningContentEvent", `{"redactedContent":"` + blob + `"}`},
		{"assistantResponseEvent", `{"content":"answer"}`},
		{"metadataEvent", `{"tokenUsage":{"outputTokens":1,"totalTokens":2},"stopReason":"end_turn"}`},
	}})
	s := newHarness(t, up, testServerOptions{})

	_, completion, _ := postChatJSON(t, s, simpleChatBody(false))
	if got := completion.Choices[0].Message.ReasoningRedactedContent; got != blob {
		t.Errorf("reasoning_redacted_content = %q, want %q", got, blob)
	}
}

func TestNonStreamingOmitsEmptyReasoningFields(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"assistantResponseEvent", `{"content":"plain"}`},
		{"metadataEvent", `{"tokenUsage":{"outputTokens":1,"totalTokens":2},"stopReason":"end_turn"}`},
	}})
	s := newHarness(t, up, testServerOptions{})

	_, _, raw := postChatJSON(t, s, simpleChatBody(false))
	for _, absent := range []string{"reasoning_content", "reasoning_signature",
		"reasoning_redacted_content", "tool_calls"} {
		if strings.Contains(raw, `"`+absent+`"`) {
			t.Errorf("%q should be omitted when empty: %s", absent, raw)
		}
	}
}

func TestNonStreamingTruncationSetsLength(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"assistantResponseEvent", `{"content":"cut short"}`},
	}})
	s := newHarness(t, up, testServerOptions{})

	_, completion, _ := postChatJSON(t, s, simpleChatBody(false))
	if completion.Choices[0].FinishReason != "length" {
		t.Errorf("finish_reason = %q, want length", completion.Choices[0].FinishReason)
	}
}

func TestNonStreamingUpstreamErrorIsMapped(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{
		Status:    http.StatusBadRequest,
		ErrorBody: `{"message":"Input is too long.","reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`,
	})
	s := newHarness(t, up, testServerOptions{})

	rec := postChat(t, s, simpleChatBody(false))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", rec.Code, rec.Body.String())
	}
	var body openAIError
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.Error.Message, "Context limit reached") {
		t.Errorf("message = %q", body.Error.Message)
	}
}

func TestNonStreamingAndStreamingAgreeOnContent(t *testing.T) {
	events := []scriptedEvent{
		{"reasoningContentEvent", `{"text":"reason","signature":"s"}`},
		{"assistantResponseEvent", `{"content":"part one "}`},
		{"assistantResponseEvent", `{"content":"part two"}`},
		{"toolUseEvent", `{"toolUseId":"t1","name":"tool","input":"{\"a\":1}","stop":true}`},
		{"metadataEvent", `{"tokenUsage":{"uncachedInputTokens":7,"outputTokens":3,"totalTokens":10},"stopReason":"tool_use"}`},
	}

	upStream := newFakeUpstream(t, upstreamScript{Events: events})
	streamed := newHarness(t, upStream, testServerOptions{})
	var streamContent, streamReasoning string
	var streamCalls []openAIStreamToolCall
	var streamUsage *openAIUsage
	var streamFinish string
	for _, c := range chunkDeltas(t, postChat(t, streamed, simpleChatBody(true)).Body.String()) {
		streamContent += c.Choices[0].Delta.Content
		streamReasoning += c.Choices[0].Delta.ReasoningContent
		streamCalls = append(streamCalls, c.Choices[0].Delta.ToolCalls...)
		if c.Usage != nil {
			streamUsage = c.Usage
		}
		if c.Choices[0].FinishReason != nil {
			streamFinish = *c.Choices[0].FinishReason
		}
	}

	upWhole := newFakeUpstream(t, upstreamScript{Events: events})
	whole := newHarness(t, upWhole, testServerOptions{})
	_, completion, _ := postChatJSON(t, whole, simpleChatBody(false))

	if streamContent != completion.Choices[0].Message.Content {
		t.Errorf("content differs: streamed %q, assembled %q", streamContent, completion.Choices[0].Message.Content)
	}
	if streamReasoning != completion.Choices[0].Message.ReasoningContent {
		t.Errorf("reasoning differs: streamed %q, assembled %q",
			streamReasoning, completion.Choices[0].Message.ReasoningContent)
	}
	if len(streamCalls) != len(completion.Choices[0].Message.ToolCalls) {
		t.Errorf("tool call counts differ: %d streamed, %d assembled",
			len(streamCalls), len(completion.Choices[0].Message.ToolCalls))
	}
	if streamFinish != completion.Choices[0].FinishReason {
		t.Errorf("finish reasons differ: %q streamed, %q assembled", streamFinish, completion.Choices[0].FinishReason)
	}
	if streamUsage == nil || completion.Usage == nil {
		t.Fatal("both paths must report usage")
	}
	if streamUsage.TotalTokens != completion.Usage.TotalTokens {
		t.Errorf("usage differs: %d streamed, %d assembled", streamUsage.TotalTokens, completion.Usage.TotalTokens)
	}
}

// ---------- reasoning signature round-trip and strip retry ----------

func TestReasoningSignatureSurvivesTwoTurns(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"assistantResponseEvent", `{"content":"second turn"}`},
		{"metadataEvent", `{"tokenUsage":{"outputTokens":1,"totalTokens":2},"stopReason":"end_turn"}`},
	}})
	s := newHarness(t, up, testServerOptions{})

	// The client echoes back the reasoning and signature kirogo emitted on turn one.
	rec := postChat(t, s, `{"model":"claude-opus-5","messages":[
	  {"role":"user","content":"first question"},
	  {"role":"assistant","content":"first answer",
	   "reasoning_content":"my earlier thinking","reasoning_signature":"sig-from-turn-1"},
	  {"role":"user","content":"second question"}
	]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	requests := up.Requests()
	if len(requests) != 1 {
		t.Fatalf("upstream requests = %d, want 1", len(requests))
	}
	body := string(requests[0])

	// The reasoning must be echoed upstream in the union wrapper the schema wants.
	want := `"reasoningContent":{"reasoningText":{"text":"my earlier thinking","signature":"sig-from-turn-1"}}`
	if !strings.Contains(body, want) {
		t.Errorf("the upstream payload does not carry the reasoning signature.\nwant substring: %s\ngot: %s", want, body)
	}
}

func TestUnsignedReasoningIsDroppedFromHistory(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"assistantResponseEvent", `{"content":"ok"}`},
		{"metadataEvent", `{"tokenUsage":{"outputTokens":1,"totalTokens":2},"stopReason":"end_turn"}`},
	}})
	s := newHarness(t, up, testServerOptions{})

	rec := postChat(t, s, `{"model":"claude-opus-5","messages":[
	  {"role":"user","content":"q"},
	  {"role":"assistant","content":"a","reasoning_content":"thinking with no signature"},
	  {"role":"user","content":"next"}
	]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	body := string(up.Requests()[0])
	if strings.Contains(body, "reasoningContent") {
		t.Errorf("unsigned reasoning must not be sent upstream: %s", body)
	}
	if strings.Contains(body, "thinking with no signature") {
		t.Errorf("the unsigned reasoning text leaked into the payload: %s", body)
	}
	// The rest of the turn must survive.
	if !strings.Contains(body, `"content":"a"`) {
		t.Errorf("the assistant turn itself was lost: %s", body)
	}
}

func TestThinkingSignatureInvalidRetriesOnceWithReasoningStripped(t *testing.T) {
	up := newFakeUpstream(t,
		// First attempt: the backend refuses the signature.
		upstreamScript{
			Status:    http.StatusBadRequest,
			ErrorBody: `{"message":"bad signature","reason":"THINKING_SIGNATURE_INVALID"}`,
		},
		// Second attempt succeeds.
		upstreamScript{Events: []scriptedEvent{
			{"assistantResponseEvent", `{"content":"recovered"}`},
			{"metadataEvent", `{"tokenUsage":{"outputTokens":1,"totalTokens":2},"stopReason":"end_turn"}`},
		}},
	)
	s := newHarness(t, up, testServerOptions{})

	rec := postChat(t, s, `{"model":"claude-opus-5","stream":true,"messages":[
	  {"role":"user","content":"q"},
	  {"role":"assistant","content":"a","reasoning_content":"stale","reasoning_signature":"stale-sig"},
	  {"role":"user","content":"next"}
	]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want the retry to succeed: %s", rec.Code, rec.Body.String())
	}

	var content string
	for _, c := range chunkDeltas(t, rec.Body.String()) {
		content += c.Choices[0].Delta.Content
	}
	if content != "recovered" {
		t.Errorf("content = %q, want the retry's output", content)
	}

	requests := up.Requests()
	if len(requests) != 2 {
		t.Fatalf("upstream requests = %d, want exactly 2", len(requests))
	}
	if !strings.Contains(string(requests[0]), "reasoningContent") {
		t.Error("the first attempt should have carried the reasoning")
	}
	if strings.Contains(string(requests[1]), "reasoningContent") {
		t.Errorf("the retry must strip all reasoning: %s", requests[1])
	}
	if strings.Contains(string(requests[1]), "stale-sig") {
		t.Error("the rejected signature leaked into the retry")
	}
}

func TestThinkingSignatureInvalidDoesNotLoop(t *testing.T) {
	// The backend rejects the signature every time. Exactly one retry is allowed.
	up := newFakeUpstream(t, upstreamScript{
		Status:    http.StatusBadRequest,
		ErrorBody: `{"message":"bad signature","reason":"THINKING_SIGNATURE_INVALID"}`,
	})
	s := newHarness(t, up, testServerOptions{})

	rec := postChat(t, s, `{"model":"claude-opus-5","messages":[
	  {"role":"user","content":"q"},
	  {"role":"assistant","content":"a","reasoning_content":"r","reasoning_signature":"s"},
	  {"role":"user","content":"next"}
	]}`)
	if rec.Code == http.StatusOK {
		t.Fatal("a second rejection must propagate, not succeed")
	}
	if n := up.RequestCount(); n != 2 {
		t.Errorf("upstream requests = %d, want 2: the original plus one strip retry", n)
	}
	var body openAIError
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.Error.Message, "reasoning signature") {
		t.Errorf("message = %q, should explain the signature problem", body.Error.Message)
	}
}

func TestThinkingSignatureInvalidWithNoReasoningToStrip(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{
		Status:    http.StatusBadRequest,
		ErrorBody: `{"message":"bad signature","reason":"THINKING_SIGNATURE_INVALID"}`,
	})
	s := newHarness(t, up, testServerOptions{})

	rec := postChat(t, s, simpleChatBody(false))
	if rec.Code == http.StatusOK {
		t.Fatal("expected the error to propagate")
	}
	// With nothing to strip there is no point retrying.
	if n := up.RequestCount(); n != 1 {
		t.Errorf("upstream requests = %d, want 1: there was no reasoning to strip", n)
	}
}

// ---------- request handling ----------

func TestChatCompletionsRejectsBadInput(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"assistantResponseEvent", `{"content":"x"}`},
	}})
	s := newHarness(t, up, testServerOptions{})

	cases := []struct {
		name     string
		body     string
		wantText string
	}{
		{"not json", `{not json`, "not valid JSON"},
		{"no model", `{"messages":[{"role":"user","content":"hi"}]}`, "No model was given"},
		{"no messages", `{"model":"claude-opus-5","messages":[]}`, "no messages"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postChat(t, s, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			var body openAIError
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(body.Error.Message, tc.wantText) {
				t.Errorf("message = %q, want it to mention %q", body.Error.Message, tc.wantText)
			}
		})
	}
}

func TestChatCompletionsRequiresAuth(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{{"assistantResponseEvent", `{"content":"x"}`}}})
	s := newHarness(t, up, testServerOptions{})

	req := newRequest(http.MethodPost, "/v1/chat/completions", simpleChatBody(false))
	rec := recordRequest(s, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 with no API key", rec.Code)
	}
	if up.RequestCount() != 0 {
		t.Error("an unauthenticated request must not reach the backend")
	}
}

func TestChatCompletionsRejectsWrongMethod(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{})
	s := newHarness(t, up, testServerOptions{})

	req := newRequest(http.MethodGet, "/v1/chat/completions", "")
	req.Header.Set("Authorization", "Bearer test-key")
	rec := recordRequest(s, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestPayloadTooLargeIsRejectedBeforeSending(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{{"assistantResponseEvent", `{"content":"x"}`}}})
	s := newHarness(t, up, testServerOptions{MaxPayloadBytes: 2000})

	big := strings.Repeat("x", 5000)
	rec := postChat(t, s, `{"model":"claude-opus-5","messages":[{"role":"user","content":"`+big+`"}]}`)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", rec.Code, rec.Body.String())
	}
	if up.RequestCount() != 0 {
		t.Error("an oversized payload must not be sent upstream")
	}
	var body openAIError
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.Error.Message, "too large") {
		t.Errorf("message = %q", body.Error.Message)
	}
}

func TestLongToolNameIsRejectedBeforeSending(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{{"assistantResponseEvent", `{"content":"x"}`}}})
	s := newHarness(t, up, testServerOptions{})

	long := strings.Repeat("n", 70)
	rec := postChat(t, s, `{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}],
	  "tools":[{"type":"function","function":{"name":"`+long+`","description":"d","parameters":{}}}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if up.RequestCount() != 0 {
		t.Error("the request must not reach the backend")
	}
	if !strings.Contains(rec.Body.String(), "64 characters") {
		t.Errorf("the error should state the limit: %s", rec.Body.String())
	}
}

func TestEffortReachesTheUpstreamPayload(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		want       string
		wantAbsent bool
	}{
		{
			name: "pinned by the model name",
			body: `{"model":"claude-opus-5:max","messages":[{"role":"user","content":"hi"}]}`,
			want: `"additionalModelRequestFields":{"reasoning":{"effort":"max"}}`,
		},
		{
			name: "from reasoning_effort",
			body: `{"model":"claude-opus-5","reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`,
			want: `"additionalModelRequestFields":{"reasoning":{"effort":"low"}}`,
		},
		{
			name: "model default when unspecified",
			body: `{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`,
			want: `"additionalModelRequestFields":{"reasoning":{"effort":"high"}}`,
		},
		{
			name:       "reasoning_effort none sends no effort at all",
			body:       `{"model":"claude-opus-5","reasoning_effort":"none","messages":[{"role":"user","content":"hi"}]}`,
			wantAbsent: true,
		},
		{
			name: "unsupported level clamps to the model default",
			body: `{"model":"claude-opus-5","reasoning_effort":"turbo","messages":[{"role":"user","content":"hi"}]}`,
			want: `"additionalModelRequestFields":{"reasoning":{"effort":"high"}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
				{"assistantResponseEvent", `{"content":"x"}`},
				{"metadataEvent", `{"tokenUsage":{"outputTokens":1,"totalTokens":2},"stopReason":"end_turn"}`},
			}})
			s := newHarness(t, up, testServerOptions{})

			rec := postChat(t, s, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			body := string(up.Requests()[0])
			if tc.wantAbsent {
				if strings.Contains(body, "additionalModelRequestFields") {
					t.Errorf("no effort should be sent: %s", body)
				}
				return
			}
			if !strings.Contains(body, tc.want) {
				t.Errorf("payload does not contain %s\ngot: %s", tc.want, body)
			}
		})
	}
}

func TestOperatorDefaultEffortIsApplied(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"assistantResponseEvent", `{"content":"x"}`},
		{"metadataEvent", `{"tokenUsage":{"outputTokens":1,"totalTokens":2},"stopReason":"end_turn"}`},
	}})
	s := newHarness(t, up, testServerOptions{EffortLevel: "low"})

	rec := postChat(t, s, simpleChatBody(false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(string(up.Requests()[0]), `{"reasoning":{"effort":"low"}}`) {
		t.Errorf("KIRO_EFFORT_LEVEL was not applied: %s", up.Requests()[0])
	}
}

func TestSystemPromptIsFoldedIntoTheConversation(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"assistantResponseEvent", `{"content":"x"}`},
		{"metadataEvent", `{"tokenUsage":{"outputTokens":1,"totalTokens":2},"stopReason":"end_turn"}`},
	}})
	s := newHarness(t, up, testServerOptions{})

	rec := postChat(t, s, `{"model":"claude-opus-5","messages":[
	  {"role":"system","content":"SYSTEM_MARKER"},
	  {"role":"user","content":"hello"}
	]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	// The deployed backend rejects a top-level systemPrompt field with
	// REQUEST_BODY_INVALID, so the prompt is folded into the first user turn.
	body := string(up.Requests()[0])
	if strings.Contains(body, `"systemPrompt"`) {
		t.Errorf("the backend rejects a top-level systemPrompt field: %s", body)
	}
	if !strings.Contains(body, `SYSTEM_MARKER\n\nhello`) {
		t.Errorf("the system prompt should prefix the user turn: %s", body)
	}
}

func TestProfileARNAndAgentModeAreSent(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"assistantResponseEvent", `{"content":"x"}`},
		{"metadataEvent", `{"tokenUsage":{"outputTokens":1,"totalTokens":2},"stopReason":"end_turn"}`},
	}})
	s := newHarness(t, up, testServerOptions{})

	if rec := postChat(t, s, simpleChatBody(false)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := string(up.Requests()[0])
	if !strings.Contains(body, `"profileArn":"arn:aws:codewhisperer:us-east-1:1:profile/A"`) {
		t.Errorf("profileArn was not sent: %s", body)
	}
	if strings.Contains(body, "agentMode") {
		t.Errorf("agentMode belongs in a header, not the body: %s", body)
	}
}

// ---------- unit-level checks ----------

func TestToolAccumulatorFragmentWithoutID(t *testing.T) {
	a := newToolAccumulator()
	a.add(&kiro.ToolUseEvent{ToolUseID: "t1", Name: "tool", Input: `{"a":`})
	// A continuation with no id extends the most recent call.
	a.add(&kiro.ToolUseEvent{Input: `1}`, Stop: true})

	calls := a.finish()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if calls[0].Arguments != `{"a":1}` {
		t.Errorf("arguments = %q", calls[0].Arguments)
	}
}

func TestToolAccumulatorIgnoresLeadingIDlessFragment(t *testing.T) {
	a := newToolAccumulator()
	// Nothing to attach to, so it is discarded rather than creating a phantom call.
	a.add(&kiro.ToolUseEvent{Input: "orphan"})
	if !a.empty() {
		t.Errorf("accumulator should be empty, got %+v", a.finish())
	}
}

func TestToolAccumulatorNilEvent(t *testing.T) {
	a := newToolAccumulator()
	a.add(nil)
	if !a.empty() {
		t.Error("a nil event must be ignored")
	}
}

func TestToolAccumulatorEmptyInputBecomesEmptyObject(t *testing.T) {
	a := newToolAccumulator()
	a.add(&kiro.ToolUseEvent{ToolUseID: "t", Name: "n", Input: "", Stop: true})
	calls := a.finish()
	if len(calls) != 1 || calls[0].Arguments != "{}" {
		t.Errorf("calls = %+v, want one call with empty-object arguments", calls)
	}
}

func TestDedupeToolCallsKeepsCallsWithNoID(t *testing.T) {
	in := []FinishedToolCall{
		{ID: "", Name: "a", Arguments: "{}"},
		{ID: "", Name: "b", Arguments: "{}"},
	}
	out := dedupeToolCalls(in)
	if len(out) != 2 {
		t.Errorf("calls = %d, want both kept: %+v", len(out), out)
	}
}

func TestUsageReportPrefersUpstreamCounts(t *testing.T) {
	c := newCollected(0, false)
	c.content.WriteString("some output")
	c.usage = &kiro.TokenUsage{
		UncachedInputTokens:   10,
		CacheReadInputTokens:  20,
		CacheWriteInputTokens: 5,
		OutputTokens:          7,
		TotalTokens:           42,
	}
	report := c.usageReport(999999, 1000000)
	if report.PromptTokens != 35 {
		t.Errorf("PromptTokens = %d, want 35", report.PromptTokens)
	}
	if report.TotalTokens != 42 {
		t.Errorf("TotalTokens = %d, want the reported 42", report.TotalTokens)
	}
	if report.Estimated {
		t.Error("upstream counts must not be marked estimated")
	}
}

func TestUsageReportFillsZeroOutputTokens(t *testing.T) {
	c := newCollected(0, false)
	c.content.WriteString("this is some generated output text")
	c.usage = &kiro.TokenUsage{UncachedInputTokens: 10, OutputTokens: 0}
	report := c.usageReport(0, 0)
	if report.CompletionTokens == 0 {
		t.Error("a zero output count with visible text should fall back to the estimate")
	}
	if report.TotalTokens != report.PromptTokens+report.CompletionTokens {
		t.Errorf("total should be consistent: %+v", report)
	}
}

func TestTruncatedDetection(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*collected)
		want  bool
	}{
		{"content only", func(c *collected) { c.content.WriteString("x") }, true},
		{"no content at all", func(c *collected) {}, false},
		{"content with usage", func(c *collected) {
			c.content.WriteString("x")
			c.usage = &kiro.TokenUsage{OutputTokens: 1}
		}, false},
		{"content with context usage", func(c *collected) {
			c.content.WriteString("x")
			c.hasContextPercentage = true
		}, false},
		{"tool calls only", func(c *collected) {
			c.tools.add(&kiro.ToolUseEvent{ToolUseID: "t", Name: "n", Stop: true})
		}, false},
		{"content plus tool calls", func(c *collected) {
			c.content.WriteString("x")
			c.tools.add(&kiro.ToolUseEvent{ToolUseID: "t", Name: "n", Stop: true})
		}, false},
		{"reasoning only", func(c *collected) { c.reasoning.WriteString("x") }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCollected(0, false)
			tc.setup(c)
			if got := c.truncated(); got != tc.want {
				t.Errorf("truncated() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestApiErrorFromException(t *testing.T) {
	cases := []struct {
		name       string
		event      kiro.ExceptionEvent
		wantStatus int
	}{
		{"throttling", kiro.ExceptionEvent{Type: "ThrottlingException", RetryAfterMilliseconds: 100}, http.StatusTooManyRequests},
		{"internal", kiro.ExceptionEvent{Type: "internalServerException"}, http.StatusInternalServerError},
		{"other", kiro.ExceptionEvent{Type: "ValidationException"}, http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := apiErrorFromException(&tc.event)
			if got.StatusCode != tc.wantStatus {
				t.Errorf("StatusCode = %d, want %d", got.StatusCode, tc.wantStatus)
			}
			if got.UserMessage() == "" {
				t.Error("UserMessage should never be empty")
			}
		})
	}
}

func TestProducesOutput(t *testing.T) {
	cases := []struct {
		name  string
		event *kiro.Event
		want  bool
	}{
		{"content", &kiro.Event{Kind: kiro.EventAssistantResponse,
			AssistantResponse: &kiro.AssistantResponseEvent{Content: "x"}}, true},
		{"empty content", &kiro.Event{Kind: kiro.EventAssistantResponse,
			AssistantResponse: &kiro.AssistantResponseEvent{}}, false},
		{"reasoning text", &kiro.Event{Kind: kiro.EventReasoningContent,
			Reasoning: &kiro.ReasoningContentEvent{Text: "x"}}, true},
		{"reasoning blob", &kiro.Event{Kind: kiro.EventReasoningContent,
			Reasoning: &kiro.ReasoningContentEvent{RedactedContent: []byte{1}}}, true},
		{"empty reasoning", &kiro.Event{Kind: kiro.EventReasoningContent,
			Reasoning: &kiro.ReasoningContentEvent{}}, false},
		{"tool use", &kiro.Event{Kind: kiro.EventToolUse, ToolUse: &kiro.ToolUseEvent{}}, true},
		{"metadata is not output", &kiro.Event{Kind: kiro.EventMetadata,
			Metadata: &kiro.MetadataEvent{}}, false},
		{"ignored is not output", &kiro.Event{Kind: kiro.EventIgnored}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := producesOutput(tc.event); got != tc.want {
				t.Errorf("producesOutput() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEstimatePromptTokensGrowsWithInput(t *testing.T) {
	small := estimatePromptTokens(translateRequestFixture("hi"))
	large := estimatePromptTokens(translateRequestFixture(strings.Repeat("word ", 500)))
	if small <= 0 {
		t.Errorf("small estimate = %d, want positive", small)
	}
	if large <= small {
		t.Errorf("large estimate %d should exceed small estimate %d", large, small)
	}
}

func TestStreamReadTimeoutIsHonouredByTheClient(t *testing.T) {
	// A sanity check that the harness plumbing sets a usable timeout, so the
	// timeout-based tests above are meaningful rather than accidentally instant.
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"assistantResponseEvent", `{"content":"x"}`},
		{"metadataEvent", `{"tokenUsage":{"outputTokens":1,"totalTokens":2},"stopReason":"end_turn"}`},
	}})
	s := newHarness(t, up, testServerOptions{FirstTokenTimeout: 2 * time.Second})
	start := time.Now()
	if rec := postChat(t, s, simpleChatBody(true)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("a prompt response took %v, which suggests the timeout is being waited out", elapsed)
	}
}

// newRequest builds a request for the harness.
func newRequest(method, path, body string) *http.Request {
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	return req
}

// recordRequest runs a request through the server and returns the recorder.
func recordRequest(s *Server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// translateRequestFixture builds a minimal IR request for estimator tests.
func translateRequestFixture(text string) *translate.Request {
	return &translate.Request{
		Model:    "claude-opus-5",
		Messages: []translate.Message{{Role: translate.RoleUser, Content: text}},
	}
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.

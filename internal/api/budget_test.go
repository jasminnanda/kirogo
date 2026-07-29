package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"kirogo/internal/kiro"
)

// ---------- outputBudget unit tests ----------

func TestOutputBudgetWithoutALimitAdmitsEverything(t *testing.T) {
	for _, limit := range []int{0, -1, -9999} {
		t.Run(fmt.Sprintf("limit_%d", limit), func(t *testing.T) {
			b := &outputBudget{limit: limit}
			long := strings.Repeat("word ", 5000)
			if got := b.take(long, false); got != long {
				t.Errorf("admitted %d runes of %d; a non-positive limit must mean no ceiling",
					utf8.RuneCountInString(got), utf8.RuneCountInString(long))
			}
			if b.exhausted {
				t.Error("a budget with no ceiling must never report itself exhausted")
			}
		})
	}
}

func TestOutputBudgetCutsExactlyAtTheCeiling(t *testing.T) {
	// The estimator is runes/4+1 scaled by 1.15, so for a ceiling of 5 tokens the
	// last admissible length is 19 runes: 19/4+1 = 5, and 5*1.15 truncates to 5,
	// while 20 runes gives 6 tokens and overshoots.
	cases := []struct {
		limit     int
		wantRunes int
	}{
		{1, 3},
		{2, 7},
		{3, 11},
		{4, 15},
		{5, 19},
		{6, 23},
		// 347/4+1 = 87 raw tokens, and 87*1.15 truncates to exactly 100. At 348 the
		// integer division ticks over to 88 and the scaled value passes the ceiling.
		{100, 347},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("limit_%d", tc.limit), func(t *testing.T) {
			b := &outputBudget{limit: tc.limit}
			got := b.take(strings.Repeat("a", 10000), false)
			if n := utf8.RuneCountInString(got); n != tc.wantRunes {
				t.Errorf("admitted %d runes, want %d", n, tc.wantRunes)
			}
			if b.tokens() > tc.limit {
				t.Errorf("tokens() = %d, which exceeds the ceiling of %d", b.tokens(), tc.limit)
			}
			if !b.exhausted {
				t.Error("a budget that stopped short of the input must report itself exhausted")
			}
			// One more rune must not fit, which is what makes the cut exact rather
			// than merely under the ceiling.
			probe := &outputBudget{limit: tc.limit}
			probe.take(strings.Repeat("a", tc.wantRunes), false)
			if probe.exhausted {
				t.Errorf("%d runes should fit within %d tokens without exhausting the budget",
					tc.wantRunes, tc.limit)
			}
		})
	}
}

func TestOutputBudgetNeverSplitsAMultiByteRune(t *testing.T) {
	// Japanese and emoji are 3 and 4 bytes each. A budget slicing by byte offset
	// rather than rune boundary would emit invalid UTF-8 here.
	for _, text := range []string{
		strings.Repeat("日本語のテキスト", 100),
		strings.Repeat("🙂🚀🎉", 100),
		strings.Repeat("aé日🚀", 100),
	} {
		for _, limit := range []int{1, 2, 3, 7, 15, 40} {
			b := &outputBudget{limit: limit}
			got := b.take(text, false)
			if !utf8.ValidString(got) {
				t.Fatalf("limit %d produced invalid UTF-8 from %q", limit, text[:12])
			}
			if !strings.HasPrefix(text, got) {
				t.Fatalf("limit %d produced text that is not a prefix of the input", limit)
			}
			if b.tokens() > limit {
				t.Fatalf("limit %d overshot: tokens() = %d", limit, b.tokens())
			}
		}
	}
}

func TestOutputBudgetCountsContentAndReasoningTowardsTheSameCeiling(t *testing.T) {
	b := &outputBudget{limit: 6}
	// Reasoning first, consuming part of the ceiling.
	reasoning := b.take(strings.Repeat("r", 8), true)
	if utf8.RuneCountInString(reasoning) != 8 {
		t.Fatalf("reasoning admitted %d runes, want all 8", utf8.RuneCountInString(reasoning))
	}
	// Content must now be limited by what reasoning already spent, not get a fresh
	// allowance of its own.
	content := b.take(strings.Repeat("c", 100), false)
	if b.tokens() > 6 {
		t.Errorf("combined tokens = %d, which exceeds the shared ceiling of 6", b.tokens())
	}
	if utf8.RuneCountInString(content) >= 23 {
		t.Errorf("content admitted %d runes; reasoning should have eaten part of the ceiling",
			utf8.RuneCountInString(content))
	}
}

func TestOutputBudgetStaysExhaustedOnceSpent(t *testing.T) {
	b := &outputBudget{limit: 2}
	b.take(strings.Repeat("a", 500), false)
	if !b.exhausted {
		t.Fatal("the budget should be exhausted")
	}
	for i := 0; i < 5; i++ {
		if got := b.take("more text", false); got != "" {
			t.Fatalf("call %d admitted %q after exhaustion; nothing may follow", i, got)
		}
		if got := b.take("more reasoning", true); got != "" {
			t.Fatalf("call %d admitted reasoning %q after exhaustion", i, got)
		}
	}
}

func TestOutputBudgetHandlesEmptyAndTinyInput(t *testing.T) {
	b := &outputBudget{limit: 10}
	if got := b.take("", false); got != "" {
		t.Errorf("take(\"\") = %q, want empty", got)
	}
	if b.exhausted {
		t.Error("an empty chunk must not exhaust the budget")
	}
	if got := b.take("a", false); got != "a" {
		t.Errorf("take(\"a\") = %q, want the single rune admitted", got)
	}
}

func TestOutputBudgetOfOneStillAdmitsSomething(t *testing.T) {
	// A pathologically small ceiling must not produce an empty response with no
	// explanation; the client asked for one token and should get one token.
	b := &outputBudget{limit: 1}
	got := b.take("hello world, this is a long reply", false)
	if got == "" {
		t.Fatal("a ceiling of 1 token admitted nothing at all")
	}
	if b.tokens() != 1 {
		t.Errorf("tokens() = %d, want exactly 1", b.tokens())
	}
}

func TestBudgetAndUsageReportAgree(t *testing.T) {
	// The invariant that matters: whatever the budget admits, the number reported
	// for it must not exceed what the client asked for. A budget that counted
	// differently from the usage report would cut at one number and report another.
	for _, limit := range []int{1, 2, 5, 13, 64, 500} {
		for _, split := range []struct{ reasoning, content int }{
			{0, 4000}, {4000, 0}, {200, 4000}, {4000, 200},
		} {
			c := newCollected(limit, false)
			c.apply(&kiro.Event{
				Kind:      kiro.EventReasoningContent,
				Reasoning: &kiro.ReasoningContentEvent{Text: strings.Repeat("r", split.reasoning)},
			})
			c.apply(&kiro.Event{
				Kind:              kiro.EventAssistantResponse,
				AssistantResponse: &kiro.AssistantResponseEvent{Content: strings.Repeat("c", split.content)},
			})
			report := c.usageReport(100, 0)
			if report.CompletionTokens > limit {
				t.Errorf("limit %d with split %+v reported %d completion tokens",
					limit, split, report.CompletionTokens)
			}
		}
	}
}

func TestBudgetExhaustionIsNotReportedAsUpstreamTruncation(t *testing.T) {
	// truncated() means the backend cut the response. When kirogo cut it to honour
	// the client's own ceiling, saying so would blame the wrong party and would log
	// an error about a fault that did not happen.
	c := newCollected(1, false)
	c.apply(&kiro.Event{
		Kind:              kiro.EventAssistantResponse,
		AssistantResponse: &kiro.AssistantResponseEvent{Content: strings.Repeat("a", 400)},
	})
	if !c.budget.exhausted {
		t.Fatal("the budget should be exhausted")
	}
	if c.truncated() {
		t.Error("a response cut by kirogo's own budget must not be reported as upstream truncation")
	}
	if got := finishReasonFor(c, nil, c.truncated()); got != "length" {
		t.Errorf("finish reason = %q, want length", got)
	}
	if got := anthropicStopReasonFor(c, nil, c.truncated()); got != "max_tokens" {
		t.Errorf("stop reason = %q, want max_tokens", got)
	}
}

func TestToolCallsOutrankTheOutputCeilingInStopReasons(t *testing.T) {
	// Tool calls are exempt from the budget, so they arrive complete. Telling a
	// client "length" would make an agent stop instead of running the call.
	c := newCollected(1, true)
	c.apply(&kiro.Event{
		Kind:              kiro.EventAssistantResponse,
		AssistantResponse: &kiro.AssistantResponseEvent{Content: strings.Repeat("a", 400)},
	})
	calls := []FinishedToolCall{{ID: "t1", Name: "get_weather", Arguments: `{"city":"Paris"}`}}
	if got := finishReasonFor(c, calls, false); got != "tool_calls" {
		t.Errorf("finish reason = %q, want tool_calls to outrank the ceiling", got)
	}
	if got := anthropicStopReasonFor(c, calls, false); got != "tool_use" {
		t.Errorf("stop reason = %q, want tool_use to outrank the ceiling", got)
	}
}

func TestStopReadingEarlyWaitsForToolsButNotForProse(t *testing.T) {
	cases := []struct {
		name         string
		limit        int
		mayCallTools bool
		content      string
		want         bool
	}{
		{"budget intact, nothing to release", 100, false, "short", false},
		{"budget spent with no tools offered", 1, false, strings.Repeat("a", 400), true},
		{"budget spent but tools were offered", 1, true, strings.Repeat("a", 400), false},
		{"no ceiling at all", 0, false, strings.Repeat("a", 400), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCollected(tc.limit, tc.mayCallTools)
			c.apply(&kiro.Event{
				Kind:              kiro.EventAssistantResponse,
				AssistantResponse: &kiro.AssistantResponseEvent{Content: tc.content},
			})
			if got := c.stopReadingEarly(); got != tc.want {
				t.Errorf("stopReadingEarly() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestApplyReportsWhatItAdmitted(t *testing.T) {
	c := newCollected(2, false)
	acc := c.apply(&kiro.Event{
		Kind:              kiro.EventAssistantResponse,
		AssistantResponse: &kiro.AssistantResponseEvent{Content: strings.Repeat("a", 100)},
	})
	if acc.Content == "" {
		t.Fatal("apply admitted nothing")
	}
	if utf8.RuneCountInString(acc.Content) != 7 {
		t.Errorf("admitted %d runes, want 7 for a 2 token ceiling", utf8.RuneCountInString(acc.Content))
	}
	if !acc.LimitReached {
		t.Error("apply should report the ceiling was reached")
	}
	// What apply admitted and what the collector stored must be identical,
	// otherwise the stream and the usage report disagree.
	if acc.Content != c.content.String() {
		t.Errorf("apply returned %q but stored %q", acc.Content, c.content.String())
	}
}

// ---------- end to end, all four paths ----------

// longContentScript emits far more text than any small ceiling allows, then the
// accounting events a complete response carries.
func longContentScript() upstreamScript {
	return upstreamScript{Events: []scriptedEvent{
		{"assistantResponseEvent", `{"content":"` + strings.Repeat("word ", 200) + `"}`},
		{"metadataEvent", `{"tokenUsage":{"uncachedInputTokens":10,"outputTokens":250,"totalTokens":260}}`},
	}}
}

func TestOpenAINonStreamingHonoursMaxTokens(t *testing.T) {
	s := newHarness(t, newFakeUpstream(t, longContentScript()), testServerOptions{})
	status, completion, raw := postChatJSON(t, s,
		`{"model":"claude-opus-5","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)

	if status != 200 {
		t.Fatalf("status = %d\n%s", status, raw)
	}
	got := completion.Choices[0].Message.Content
	if n := utf8.RuneCountInString(got); n != 19 {
		t.Errorf("content is %d runes, want 19 for a 5 token ceiling: %q", n, got)
	}
	if completion.Choices[0].FinishReason != "length" {
		t.Errorf("finish_reason = %q, want length", completion.Choices[0].FinishReason)
	}
	if completion.Usage.CompletionTokens > 5 {
		t.Errorf("completion_tokens = %d, which exceeds the requested 5", completion.Usage.CompletionTokens)
	}
}

func TestOpenAIStreamingHonoursMaxTokens(t *testing.T) {
	s := newHarness(t, newFakeUpstream(t, longContentScript()), testServerOptions{})
	rec := postChat(t, s,
		`{"model":"claude-opus-5","stream":true,"max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)

	var text strings.Builder
	var finish string
	var usage *openAIUsage
	for _, chunk := range chunkDeltas(t, rec.Body.String()) {
		for _, ch := range chunk.Choices {
			text.WriteString(ch.Delta.Content)
			if ch.FinishReason != nil {
				finish = *ch.FinishReason
			}
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
	}

	if n := utf8.RuneCountInString(text.String()); n != 19 {
		t.Errorf("streamed %d runes, want 19: %q", n, text.String())
	}
	if finish != "length" {
		t.Errorf("finish_reason = %q, want length", finish)
	}
	if usage == nil {
		t.Fatal("no usage in the final chunk")
	}
	if usage.CompletionTokens > 5 {
		t.Errorf("completion_tokens = %d, which exceeds the requested 5", usage.CompletionTokens)
	}
}

func TestAnthropicNonStreamingHonoursMaxTokens(t *testing.T) {
	s := newHarness(t, newFakeUpstream(t, longContentScript()), testServerOptions{})
	rec := postMessages(t, s, "/v1/messages",
		`{"model":"claude-opus-5","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != 200 {
		t.Fatalf("status = %d\n%s", rec.Code, rec.Body.String())
	}
	var msg anthropicMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body.String())
	}
	var text string
	for _, b := range msg.Content {
		if b.Type == "text" {
			text = b.Text
		}
	}
	if n := utf8.RuneCountInString(text); n != 19 {
		t.Errorf("text is %d runes, want 19: %q", n, text)
	}
	if msg.StopReason == nil || *msg.StopReason != "max_tokens" {
		t.Errorf("stop_reason = %v, want max_tokens", msg.StopReason)
	}
	if msg.Usage.OutputTokens > 5 {
		t.Errorf("output_tokens = %d, which exceeds the requested 5", msg.Usage.OutputTokens)
	}
}

func TestAnthropicStreamingHonoursMaxTokens(t *testing.T) {
	s := newHarness(t, newFakeUpstream(t, longContentScript()), testServerOptions{})
	rec := postMessages(t, s, "/v1/messages",
		`{"model":"claude-opus-5","stream":true,"max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)

	var text strings.Builder
	var stop any
	var output float64
	for _, f := range anthropicFrames(t, rec.Body.String()) {
		switch f.Name {
		case evtContentBlockDelta:
			d, _ := f.Payload["delta"].(map[string]any)
			if d["type"] == "text_delta" {
				s, _ := d["text"].(string)
				text.WriteString(s)
			}
		case evtMessageDelta:
			d, _ := f.Payload["delta"].(map[string]any)
			stop = d["stop_reason"]
			if u, ok := f.Payload["usage"].(map[string]any); ok {
				output, _ = u["output_tokens"].(float64)
			}
		}
	}

	if n := utf8.RuneCountInString(text.String()); n != 19 {
		t.Errorf("streamed %d runes, want 19: %q", n, text.String())
	}
	if stop != "max_tokens" {
		t.Errorf("stop_reason = %v, want max_tokens", stop)
	}
	if int(output) > 5 {
		t.Errorf("output_tokens = %d, which exceeds the requested 5", int(output))
	}
}

// ---------- tool calls are exempt ----------

// toolAfterLongContentScript floods the ceiling with prose and only then emits a
// tool call, which is the ordering that would lose the call if the stream were
// released the moment the budget ran out.
func toolAfterLongContentScript() upstreamScript {
	return upstreamScript{Events: []scriptedEvent{
		{"assistantResponseEvent", `{"content":"` + strings.Repeat("word ", 200) + `"}`},
		{"toolUseEvent", `{"toolUseId":"t1","name":"get_weather","input":"{\"city\":"}`},
		{"toolUseEvent", `{"toolUseId":"t1","input":"\"Paris\"}","stop":true}`},
		{"metadataEvent", `{"tokenUsage":{"uncachedInputTokens":10,"outputTokens":250}}`},
	}}
}

const toolRequestOpenAI = `{"model":"claude-opus-5","max_tokens":2,
  "messages":[{"role":"user","content":"weather?"}],
  "tools":[{"type":"function","function":{"name":"get_weather","description":"w",
    "parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`

func TestMaxTokensNeverTruncatesAToolCallOpenAI(t *testing.T) {
	s := newHarness(t, newFakeUpstream(t, toolAfterLongContentScript()), testServerOptions{})
	status, completion, raw := postChatJSON(t, s, toolRequestOpenAI)
	if status != 200 {
		t.Fatalf("status = %d\n%s", status, raw)
	}

	calls := completion.Choices[0].Message.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want 1; a low ceiling must not drop them\n%s", len(calls), raw)
	}
	if calls[0].Function.Arguments != `{"city":"Paris"}` {
		t.Errorf("arguments = %q, want the fully assembled object", calls[0].Function.Arguments)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &parsed); err != nil {
		t.Errorf("arguments are not valid JSON: %v", err)
	}
	if completion.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", completion.Choices[0].FinishReason)
	}
	// The prose is still capped even though the tool call was let through whole.
	if n := utf8.RuneCountInString(completion.Choices[0].Message.Content); n != 7 {
		t.Errorf("content is %d runes, want 7 for a 2 token ceiling", n)
	}
}

func TestMaxTokensNeverTruncatesAToolCallAnthropic(t *testing.T) {
	s := newHarness(t, newFakeUpstream(t, toolAfterLongContentScript()), testServerOptions{})
	rec := postMessages(t, s, "/v1/messages", `{"model":"claude-opus-5","max_tokens":2,
	  "messages":[{"role":"user","content":"weather?"}],
	  "tools":[{"name":"get_weather","description":"w",
	    "input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]}`)

	var msg anthropicMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body.String())
	}
	var tools []anthropicContentBlock
	for _, b := range msg.Content {
		if b.Type == "tool_use" {
			tools = append(tools, b)
		}
	}
	if len(tools) != 1 {
		t.Fatalf("got %d tool_use blocks, want 1\n%s", len(tools), rec.Body.String())
	}
	input, ok := tools[0].Input.(map[string]any)
	if !ok || input["city"] != "Paris" {
		t.Errorf("input = %#v, want the fully assembled {city: Paris}", tools[0].Input)
	}
	if msg.StopReason == nil || *msg.StopReason != "tool_use" {
		t.Errorf("stop_reason = %v, want tool_use", msg.StopReason)
	}
}

func TestMaxTokensNeverTruncatesAToolCallStreaming(t *testing.T) {
	s := newHarness(t, newFakeUpstream(t, toolAfterLongContentScript()), testServerOptions{})
	rec := postMessages(t, s, "/v1/messages", `{"model":"claude-opus-5","max_tokens":2,"stream":true,
	  "messages":[{"role":"user","content":"weather?"}],
	  "tools":[{"name":"get_weather","description":"w",
	    "input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]}`)

	var partial strings.Builder
	var stop any
	for _, f := range anthropicFrames(t, rec.Body.String()) {
		if f.Name == evtContentBlockDelta {
			d, _ := f.Payload["delta"].(map[string]any)
			if d["type"] == "input_json_delta" {
				s, _ := d["partial_json"].(string)
				partial.WriteString(s)
			}
		}
		if f.Name == evtMessageDelta {
			d, _ := f.Payload["delta"].(map[string]any)
			stop = d["stop_reason"]
		}
	}
	if partial.String() != `{"city":"Paris"}` {
		t.Errorf("input_json_delta = %q, want the whole object", partial.String())
	}
	if stop != "tool_use" {
		t.Errorf("stop_reason = %v, want tool_use", stop)
	}
}

// ---------- no ceiling means no change ----------

func TestOpenAIWithoutMaxTokensIsUnlimited(t *testing.T) {
	// OpenAI treats max_tokens as optional, so omitting it must leave the response
	// untouched rather than defaulting to some cap of kirogo's invention.
	s := newHarness(t, newFakeUpstream(t, longContentScript()), testServerOptions{})
	status, completion, raw := postChatJSON(t, s,
		`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`)
	if status != 200 {
		t.Fatalf("status = %d\n%s", status, raw)
	}
	want := strings.Repeat("word ", 200)
	if completion.Choices[0].Message.Content != want {
		t.Errorf("content was altered: got %d runes, want %d",
			utf8.RuneCountInString(completion.Choices[0].Message.Content), utf8.RuneCountInString(want))
	}
	if completion.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", completion.Choices[0].FinishReason)
	}
}

func TestAGenerousMaxTokensChangesNothing(t *testing.T) {
	s := newHarness(t, newFakeUpstream(t, longContentScript()), testServerOptions{})
	rec := postMessages(t, s, "/v1/messages",
		`{"model":"claude-opus-5","max_tokens":64000,"messages":[{"role":"user","content":"hi"}]}`)
	var msg anthropicMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := strings.Repeat("word ", 200)
	var text string
	for _, b := range msg.Content {
		if b.Type == "text" {
			text = b.Text
		}
	}
	if text != want {
		t.Errorf("a ceiling far above the response still altered it: %d runes vs %d",
			utf8.RuneCountInString(text), utf8.RuneCountInString(want))
	}
	if msg.StopReason == nil || *msg.StopReason != "end_turn" {
		t.Errorf("stop_reason = %v, want end_turn", msg.StopReason)
	}
}

// ---------- reasoning shares the ceiling ----------

func TestReasoningCountsTowardsMaxTokens(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"reasoningContentEvent", `{"text":"` + strings.Repeat("t", 40) + `","signature":"sig-abc"}`},
		{"assistantResponseEvent", `{"content":"` + strings.Repeat("c", 400) + `"}`},
		{"metadataEvent", `{"tokenUsage":{"uncachedInputTokens":10,"outputTokens":100}}`},
	}})
	s := newHarness(t, up, testServerOptions{})
	rec := postMessages(t, s, "/v1/messages",
		`{"model":"claude-opus-5","max_tokens":6,"messages":[{"role":"user","content":"hi"}]}`)

	var msg anthropicMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body.String())
	}

	var thinking, text string
	for _, b := range msg.Content {
		switch b.Type {
		case "thinking":
			thinking = b.Thinking
		case "text":
			text = b.Text
		}
	}
	if thinking == "" {
		t.Fatal("no thinking block survived")
	}
	// Reasoning arrived first, so it spends the ceiling before any text is seen.
	// It must itself be cut, and with the whole ceiling gone there is nothing left
	// for the text that follows. That is the same rule Anthropic applies, and it is
	// why a client enabling extended thinking has to set max_tokens above its
	// thinking budget.
	if n := utf8.RuneCountInString(thinking); n != 23 {
		t.Errorf("thinking is %d runes, want it cut to 23 by the 6 token ceiling", n)
	}
	if text != "" {
		t.Errorf("text = %q, want none: reasoning had already spent the ceiling", text)
	}
	if got := estimateCompletionTokens(text, thinking); got > 6 {
		t.Errorf("reasoning and text together estimate %d tokens, over the ceiling of 6", got)
	}
	if msg.Usage.OutputTokens > 6 {
		t.Errorf("output_tokens = %d, which exceeds the requested 6", msg.Usage.OutputTokens)
	}
	if msg.StopReason == nil || *msg.StopReason != "max_tokens" {
		t.Errorf("stop_reason = %v, want max_tokens", msg.StopReason)
	}
	// The signature is bookkeeping, not generated text, so it survives the cut.
	for _, b := range msg.Content {
		if b.Type == "thinking" && b.Signature != "sig-abc" {
			t.Errorf("signature = %q, want it preserved through truncation", b.Signature)
		}
	}
}

// ---------- the stream is released when nothing more can be shown ----------

func TestTheUpstreamStreamIsReleasedOnceTheCeilingIsMet(t *testing.T) {
	// With no tools offered there is nothing further worth reading, so kirogo stops
	// instead of waiting out a long generation the client will never see. The proof
	// is that a script which stalls between events still answers promptly.
	up := newFakeUpstream(t, upstreamScript{
		Events: []scriptedEvent{
			{"assistantResponseEvent", `{"content":"` + strings.Repeat("a", 400) + `"}`},
			{"assistantResponseEvent", `{"content":"never reaches the client"}`},
			{"metadataEvent", `{"tokenUsage":{"outputTokens":999}}`},
		},
		DelayBetween: 2 * time.Second,
	})
	s := newHarness(t, up, testServerOptions{})

	done := make(chan string, 1)
	go func() {
		_, completion, _ := postChatJSON(t, s,
			`{"model":"claude-opus-5","max_tokens":2,"messages":[{"role":"user","content":"hi"}]}`)
		done <- completion.Choices[0].Message.Content
	}()

	select {
	case content := <-done:
		if strings.Contains(content, "never") {
			t.Error("text past the ceiling reached the client")
		}
		if utf8.RuneCountInString(content) != 7 {
			t.Errorf("content is %d runes, want 7", utf8.RuneCountInString(content))
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("the response waited for the rest of the upstream stream instead of releasing it")
	}
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.

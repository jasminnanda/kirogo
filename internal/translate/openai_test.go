package translate

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// parseOpenAI decodes a request body fixture.
func parseOpenAI(t *testing.T, body string) *OpenAIRequest {
	t.Helper()
	var req OpenAIRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}
	return &req
}

// irFromOpenAI parses and converts a fixture.
func irFromOpenAI(t *testing.T, body string) *Request {
	t.Helper()
	ir, err := FromOpenAI(parseOpenAI(t, body))
	if err != nil {
		t.Fatalf("FromOpenAI: %v", err)
	}
	return ir
}

func TestOpenAISystemAndDeveloperFoldIntoTheSystemPrompt(t *testing.T) {
	ir := irFromOpenAI(t, `{
	  "model": "claude-opus-5",
	  "messages": [
	    {"role": "system", "content": "You are terse."},
	    {"role": "developer", "content": "Prefer Go."},
	    {"role": "user", "content": "hello"},
	    {"role": "system", "content": "Also be kind."}
	  ]
	}`)

	want := "You are terse.\n\nPrefer Go.\n\nAlso be kind."
	if ir.SystemPrompt != want {
		t.Errorf("SystemPrompt = %q, want %q", ir.SystemPrompt, want)
	}
	if len(ir.Messages) != 1 || ir.Messages[0].Role != RoleUser {
		t.Errorf("messages = %+v, want just the user turn", ir.Messages)
	}
}

func TestOpenAIToolMessagesBecomeToolResultsOnTheNextUserTurn(t *testing.T) {
	ir := irFromOpenAI(t, `{
	  "model": "m",
	  "messages": [
	    {"role": "user", "content": "weather?"},
	    {"role": "assistant", "tool_calls": [
	      {"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"Berlin\"}"}},
	      {"id": "call_2", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"Paris\"}"}}
	    ]},
	    {"role": "tool", "tool_call_id": "call_1", "content": "18C"},
	    {"role": "tool", "tool_call_id": "call_2", "content": "21C"},
	    {"role": "user", "content": "thanks"}
	  ]
	}`)

	if len(ir.Messages) != 3 {
		t.Fatalf("messages = %d, want 3: user, assistant, user-with-results", len(ir.Messages))
	}
	assistant := ir.Messages[1]
	if len(assistant.ToolCalls) != 2 {
		t.Fatalf("tool calls = %d, want 2", len(assistant.ToolCalls))
	}
	if assistant.ToolCalls[0].Name != "get_weather" || assistant.ToolCalls[0].ID != "call_1" {
		t.Errorf("tool call 0 = %+v", assistant.ToolCalls[0])
	}

	last := ir.Messages[2]
	if last.Role != RoleUser {
		t.Errorf("last role = %q", last.Role)
	}
	if len(last.ToolResults) != 2 {
		t.Fatalf("tool results = %d, want both attached to the following user turn", len(last.ToolResults))
	}
	if last.ToolResults[0].ToolUseID != "call_1" || last.ToolResults[0].Content != "18C" {
		t.Errorf("result 0 = %+v", last.ToolResults[0])
	}
	if last.Content != "thanks" {
		t.Errorf("content = %q, want the user's own text preserved", last.Content)
	}
}

func TestOpenAITrailingToolMessagesGetASynthesisedUserTurn(t *testing.T) {
	ir := irFromOpenAI(t, `{
	  "model": "m",
	  "messages": [
	    {"role": "user", "content": "go"},
	    {"role": "assistant", "tool_calls": [{"id": "c1", "type": "function", "function": {"name": "t", "arguments": "{}"}}]},
	    {"role": "tool", "tool_call_id": "c1", "content": "done"}
	  ]
	}`)

	last := ir.Messages[len(ir.Messages)-1]
	if last.Role != RoleUser {
		t.Errorf("last role = %q, want a synthesised user turn", last.Role)
	}
	if len(last.ToolResults) != 1 || last.ToolResults[0].Content != "done" {
		t.Errorf("tool results = %+v", last.ToolResults)
	}
}

func TestOpenAIToolResultsFlushBeforeAnAssistantTurn(t *testing.T) {
	// Tool results followed directly by an assistant turn must not be swallowed.
	ir := irFromOpenAI(t, `{
	  "model": "m",
	  "messages": [
	    {"role": "user", "content": "a"},
	    {"role": "assistant", "tool_calls": [{"id": "c1", "type": "function", "function": {"name": "t", "arguments": "{}"}}]},
	    {"role": "tool", "tool_call_id": "c1", "content": "result"},
	    {"role": "assistant", "content": "final answer"}
	  ]
	}`)

	var sawResult bool
	for _, m := range ir.Messages {
		for _, tr := range m.ToolResults {
			if tr.Content == "result" {
				sawResult = true
			}
		}
	}
	if !sawResult {
		t.Errorf("the tool result was lost: %+v", ir.Messages)
	}
	if ir.Messages[len(ir.Messages)-1].Content != "final answer" {
		t.Errorf("last message = %+v", ir.Messages[len(ir.Messages)-1])
	}
}

func TestOpenAIBothToolDeclarationShapes(t *testing.T) {
	ir := irFromOpenAI(t, `{
	  "model": "m",
	  "messages": [{"role": "user", "content": "hi"}],
	  "tools": [
	    {"type": "function", "function": {"name": "spec_shape", "description": "d1",
	      "parameters": {"type": "object", "properties": {"a": {"type": "string"}}}}},
	    {"name": "cursor_flat", "description": "d2",
	      "input_schema": {"type": "object", "properties": {"b": {"type": "number"}}}},
	    {"name": "flat_with_parameters", "description": "d3",
	      "parameters": {"type": "object"}},
	    {"type": "function"}
	  ]
	}`)

	if len(ir.Tools) != 3 {
		t.Fatalf("tools = %d, want 3 with the nameless one skipped: %+v", len(ir.Tools), ir.Tools)
	}
	byName := map[string]Tool{}
	for _, tool := range ir.Tools {
		byName[tool.Name] = tool
	}

	spec, ok := byName["spec_shape"]
	if !ok {
		t.Fatal("the spec-compliant shape was not recognised")
	}
	if spec.Description != "d1" || spec.InputSchema["type"] != "object" {
		t.Errorf("spec_shape = %+v", spec)
	}

	flat, ok := byName["cursor_flat"]
	if !ok {
		t.Fatal("Cursor's flat shape was not recognised")
	}
	if flat.Description != "d2" || flat.InputSchema["type"] != "object" {
		t.Errorf("cursor_flat = %+v", flat)
	}

	if byName["flat_with_parameters"].InputSchema == nil {
		t.Error("a flat tool using parameters instead of input_schema should still work")
	}
}

func TestOpenAIImagesFromDataURLs(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("pngbytes"))
	ir := irFromOpenAI(t, `{
	  "model": "m",
	  "messages": [{"role": "user", "content": [
	    {"type": "text", "text": "what is this? "},
	    {"type": "image_url", "image_url": {"url": "data:image/png;base64,`+png+`"}},
	    {"type": "text", "text": "be brief"}
	  ]}]
	}`)

	m := ir.Messages[0]
	if m.Content != "what is this? be brief" {
		t.Errorf("content = %q, want the text blocks concatenated", m.Content)
	}
	if len(m.Images) != 1 {
		t.Fatalf("images = %d, want 1", len(m.Images))
	}
	if m.Images[0].MediaType != "image/png" {
		t.Errorf("media type = %q", m.Images[0].MediaType)
	}
	if m.Images[0].Data != png {
		t.Errorf("data = %q, want the base64 payload with the prefix stripped", m.Images[0].Data)
	}
	if m.Images[0].Format() != "png" {
		t.Errorf("Format() = %q, want png", m.Images[0].Format())
	}
}

func TestOpenAIHTTPImageURLsAreSkipped(t *testing.T) {
	ir := irFromOpenAI(t, `{
	  "model": "m",
	  "messages": [{"role": "user", "content": [
	    {"type": "text", "text": "look"},
	    {"type": "image_url", "image_url": {"url": "https://example.test/cat.png"}},
	    {"type": "image_url", "image_url": {"url": "http://example.test/dog.png"}}
	  ]}]
	}`)

	if len(ir.Messages[0].Images) != 0 {
		t.Errorf("images = %+v, want none: the backend cannot fetch remote images", ir.Messages[0].Images)
	}
	if ir.Messages[0].Content != "look" {
		t.Errorf("the text should survive, got %q", ir.Messages[0].Content)
	}
}

func TestOpenAIImagesFromToolMessages(t *testing.T) {
	// MCP tools return screenshots inside a tool message.
	shot := base64.StdEncoding.EncodeToString([]byte("screenshot"))
	ir := irFromOpenAI(t, `{
	  "model": "m",
	  "messages": [
	    {"role": "user", "content": "screenshot the page"},
	    {"role": "assistant", "tool_calls": [{"id": "s1", "type": "function", "function": {"name": "shot", "arguments": "{}"}}]},
	    {"role": "tool", "tool_call_id": "s1", "content": [
	      {"type": "text", "text": "captured"},
	      {"type": "image_url", "image_url": {"url": "data:image/png;base64,`+shot+`"}}
	    ]}
	  ]
	}`)

	last := ir.Messages[len(ir.Messages)-1]
	if len(last.ToolResults) != 1 {
		t.Fatalf("tool results = %+v", last.ToolResults)
	}
	tr := last.ToolResults[0]
	if tr.Content != "captured" {
		t.Errorf("result text = %q", tr.Content)
	}
	if len(tr.Images) != 1 || tr.Images[0].Data != shot {
		t.Errorf("the screenshot returned by the tool was lost: %+v", tr.Images)
	}
}

func TestOpenAIAnthropicStyleImageBlockInsideAnOpenAIRequest(t *testing.T) {
	ir := irFromOpenAI(t, `{
	  "model": "m",
	  "messages": [{"role": "user", "content": [
	    {"type": "image", "source": {"type": "base64", "media_type": "image/jpeg", "data": "SkFQRw=="}}
	  ]}]
	}`)
	if len(ir.Messages[0].Images) != 1 {
		t.Fatalf("images = %+v", ir.Messages[0].Images)
	}
	if ir.Messages[0].Images[0].MediaType != "image/jpeg" {
		t.Errorf("media type = %q", ir.Messages[0].Images[0].MediaType)
	}
}

func TestOpenAIPlainStringContent(t *testing.T) {
	ir := irFromOpenAI(t, `{"model":"m","messages":[{"role":"user","content":"just text"}]}`)
	if ir.Messages[0].Content != "just text" {
		t.Errorf("content = %q", ir.Messages[0].Content)
	}
}

func TestOpenAINullAndMissingContent(t *testing.T) {
	ir := irFromOpenAI(t, `{"model":"m","messages":[
	  {"role":"user","content":null},
	  {"role":"assistant"},
	  {"role":"user","content":""}
	]}`)
	for i, m := range ir.Messages {
		if m.Content != "" {
			t.Errorf("message %d content = %q, want empty", i, m.Content)
		}
	}
}

func TestOpenAIReasoningRoundTrip(t *testing.T) {
	blob := base64.StdEncoding.EncodeToString([]byte{1, 2, 3})
	ir := irFromOpenAI(t, `{
	  "model": "m",
	  "messages": [
	    {"role": "user", "content": "q"},
	    {"role": "assistant", "content": "a",
	     "reasoning_content": "I thought about it",
	     "reasoning_signature": "sig-xyz"},
	    {"role": "user", "content": "again"},
	    {"role": "assistant", "content": "b", "reasoning_redacted_content": "`+blob+`"}
	  ]
	}`)

	first := ir.Messages[1]
	if first.Reasoning == nil {
		t.Fatal("reasoning was not recovered from the assistant message")
	}
	if first.Reasoning.Text != "I thought about it" || first.Reasoning.Signature != "sig-xyz" {
		t.Errorf("reasoning = %+v", first.Reasoning)
	}
	if !first.Reasoning.Signed() {
		t.Error("signed reasoning should report Signed() true")
	}

	second := ir.Messages[3]
	if second.Reasoning == nil || len(second.Reasoning.RedactedContent) != 3 {
		t.Errorf("redacted reasoning = %+v", second.Reasoning)
	}
}

func TestOpenAIReasoningWithoutSignatureIsCarriedButUnsigned(t *testing.T) {
	ir := irFromOpenAI(t, `{
	  "model": "m",
	  "messages": [{"role": "assistant", "content": "a", "reasoning_content": "unsigned"}]
	}`)
	r := ir.Messages[0].Reasoning
	if r == nil {
		t.Fatal("reasoning should be present in the IR")
	}
	if r.Signed() {
		t.Error("reasoning with no signature must not report Signed() true")
	}
}

func TestOpenAIInvalidBase64ReasoningIsIgnored(t *testing.T) {
	ir := irFromOpenAI(t, `{
	  "model": "m",
	  "messages": [{"role": "assistant", "content": "a", "reasoning_redacted_content": "!!!not base64!!!"}]
	}`)
	r := ir.Messages[0].Reasoning
	if r == nil {
		t.Fatal("reasoning should still exist")
	}
	if len(r.RedactedContent) != 0 {
		t.Errorf("invalid base64 should be ignored, got %v", r.RedactedContent)
	}
}

func TestOpenAIReasoningEffortMapping(t *testing.T) {
	cases := []struct {
		in           string
		wantLevel    string
		wantDisabled bool
	}{
		{"", "", false},
		{"none", "", true},
		{"off", "", true},
		{"disabled", "", true},
		{"NONE", "", true},
		{"minimal", "low", false},
		{"low", "low", false},
		{"medium", "medium", false},
		{"high", "high", false},
		{"xhigh", "xhigh", false},
		{"max", "max", false},
		{"XHIGH", "xhigh", false},
		{"  high  ", "high", false},
		{"nonsense", "nonsense", false},
	}
	for _, tc := range cases {
		t.Run("effort="+tc.in, func(t *testing.T) {
			level, disabled := normalizeReasoningEffort(tc.in)
			if level != tc.wantLevel || disabled != tc.wantDisabled {
				t.Errorf("normalizeReasoningEffort(%q) = (%q, %v), want (%q, %v)",
					tc.in, level, disabled, tc.wantLevel, tc.wantDisabled)
			}
		})
	}
}

func TestOpenAIReasoningEffortReachesTheIR(t *testing.T) {
	ir := irFromOpenAI(t, `{"model":"m","messages":[{"role":"user","content":"q"}],"reasoning_effort":"max"}`)
	if ir.EffortLevel != "max" {
		t.Errorf("EffortLevel = %q", ir.EffortLevel)
	}
	if ir.DisableReasoning {
		t.Error("DisableReasoning should be false")
	}

	off := irFromOpenAI(t, `{"model":"m","messages":[{"role":"user","content":"q"}],"reasoning_effort":"none"}`)
	if !off.DisableReasoning {
		t.Error("reasoning_effort none should disable reasoning")
	}
}

func TestOpenAIUnknownFieldsAreIgnored(t *testing.T) {
	// Cursor, Cline and friends send plenty of fields with no Kiro equivalent.
	ir := irFromOpenAI(t, `{
	  "model": "m",
	  "messages": [{"role": "user", "content": "hi"}],
	  "temperature": 0.7, "top_p": 0.9, "n": 1, "seed": 42,
	  "presence_penalty": 0, "frequency_penalty": 0,
	  "stop": ["\n\n"], "user": "someone", "parallel_tool_calls": true,
	  "tool_choice": "auto", "stream_options": {"include_usage": true},
	  "logit_bias": {"1": 1}, "response_format": {"type": "json_object"},
	  "some_field_invented_next_year": {"nested": [1,2,3]}
	}`)
	if len(ir.Messages) != 1 {
		t.Errorf("messages = %+v", ir.Messages)
	}
}

func TestOpenAIMaxTokensPreference(t *testing.T) {
	withBoth := irFromOpenAI(t, `{"model":"m","messages":[{"role":"user","content":"q"}],
	  "max_tokens":100,"max_completion_tokens":200}`)
	if withBoth.MaxTokens != 200 {
		t.Errorf("MaxTokens = %d, want max_completion_tokens to win", withBoth.MaxTokens)
	}

	legacyOnly := irFromOpenAI(t, `{"model":"m","messages":[{"role":"user","content":"q"}],"max_tokens":100}`)
	if legacyOnly.MaxTokens != 100 {
		t.Errorf("MaxTokens = %d, want 100", legacyOnly.MaxTokens)
	}
}

func TestOpenAIStreamFlag(t *testing.T) {
	on := irFromOpenAI(t, `{"model":"m","messages":[{"role":"user","content":"q"}],"stream":true}`)
	if !on.Stream {
		t.Error("Stream should be true")
	}
	off := irFromOpenAI(t, `{"model":"m","messages":[{"role":"user","content":"q"}]}`)
	if off.Stream {
		t.Error("Stream should default to false")
	}
}

func TestOpenAINoMessagesIsAnError(t *testing.T) {
	_, err := FromOpenAI(&OpenAIRequest{Model: "m"})
	if err == nil {
		t.Fatal("a request with no messages must be rejected")
	}
	if !strings.Contains(err.Error(), "no messages") {
		t.Errorf("error = %q", err)
	}
}

func TestOpenAISystemOnlyRequestIsAccepted(t *testing.T) {
	// A system prompt with no user turn is odd but recoverable: the invariants
	// synthesise the missing user turn.
	ir, err := FromOpenAI(parseOpenAI(t, `{"model":"m","messages":[{"role":"system","content":"only system"}]}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ir.SystemPrompt != "only system" {
		t.Errorf("SystemPrompt = %q", ir.SystemPrompt)
	}
}

func TestOpenAIFunctionRoleIsTreatedAsTool(t *testing.T) {
	ir := irFromOpenAI(t, `{
	  "model": "m",
	  "messages": [
	    {"role": "user", "content": "q"},
	    {"role": "assistant", "tool_calls": [{"id": "f1", "type": "function", "function": {"name": "t", "arguments": "{}"}}]},
	    {"role": "function", "tool_call_id": "f1", "content": "legacy result"}
	  ]
	}`)
	last := ir.Messages[len(ir.Messages)-1]
	if len(last.ToolResults) != 1 || last.ToolResults[0].Content != "legacy result" {
		t.Errorf("the legacy function role should behave like tool, got %+v", last.ToolResults)
	}
}

func TestToolCallArgumentsObject(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string
	}{
		{"valid object", `{"a":1}`, `{"a":1}`},
		{"empty string", "", `{}`},
		{"whitespace", "   ", `{}`},
		{"invalid json", `{not json`, `{}`},
		{"json null", `null`, `{}`},
		{"empty object", `{}`, `{}`},
		{"nested", `{"a":{"b":[1,2]}}`, `{"a":{"b":[1,2]}}`},
		{"array is preserved", `[1,2]`, `[1,2]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(ToolCall{Arguments: tc.args}.ArgumentsObject())
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("ArgumentsObject() for %q = %s, want %s", tc.args, got, tc.want)
			}
		})
	}
}

func TestParseDataURL(t *testing.T) {
	cases := []struct {
		name          string
		url           string
		wantMediaType string
		wantData      string
		wantOK        bool
	}{
		{"png base64", "data:image/png;base64,AAA", "image/png", "AAA", true},
		{"jpeg base64", "data:image/jpeg;base64,BBB", "image/jpeg", "BBB", true},
		{"no base64 marker", "data:image/gif,CCC", "image/gif", "CCC", true},
		{"no media type", "data:;base64,DDD", "image/png", "DDD", true},
		{"extra parameters", "data:image/webp;charset=utf-8;base64,EEE", "image/webp", "EEE", true},
		{"not a data url", "https://example.test/x.png", "", "", false},
		{"no comma", "data:image/png;base64", "", "", false},
		{"empty payload", "data:image/png;base64,", "", "", false},
		{"empty string", "", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mediaType, data, ok := parseDataURL(tc.url)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if mediaType != tc.wantMediaType || data != tc.wantData {
				t.Errorf("= (%q, %q), want (%q, %q)", mediaType, data, tc.wantMediaType, tc.wantData)
			}
		})
	}
}

func TestImageFormat(t *testing.T) {
	cases := map[string]string{
		"image/png":  "png",
		"image/jpeg": "jpeg",
		"image/webp": "webp",
		"image/gif":  "gif",
		"png":        "png",
		"":           "png",
	}
	for mediaType, want := range cases {
		if got := (Image{MediaType: mediaType}).Format(); got != want {
			t.Errorf("Format() for %q = %q, want %q", mediaType, got, want)
		}
	}
}

func TestOpenAIEndToEndGoldenPayload(t *testing.T) {
	// A multi-turn, tool-calling, image-bearing conversation, converted all the
	// way to the Kiro wire format.
	img := base64.StdEncoding.EncodeToString([]byte("IMG"))
	ir := irFromOpenAI(t, `{
	  "model": "claude-opus-5",
	  "stream": true,
	  "reasoning_effort": "xhigh",
	  "messages": [
	    {"role": "system", "content": "You are terse."},
	    {"role": "user", "content": [
	      {"type": "text", "text": "What is in this image?"},
	      {"type": "image_url", "image_url": {"url": "data:image/png;base64,`+img+`"}}
	    ]},
	    {"role": "assistant", "content": "A cat. Let me confirm.",
	     "reasoning_content": "It looks feline.", "reasoning_signature": "sig-1",
	     "tool_calls": [{"id": "call_1", "type": "function",
	       "function": {"name": "classify", "arguments": "{\"kind\":\"animal\"}"}}]},
	    {"role": "tool", "tool_call_id": "call_1", "content": "cat, 98% confidence"},
	    {"role": "user", "content": "Thanks."}
	  ],
	  "tools": [{"type": "function", "function": {
	    "name": "classify", "description": "Classify an image.",
	    "parameters": {"type": "object", "required": ["kind"], "additionalProperties": false,
	      "properties": {"kind": {"type": "string"}}}}}]
	}`)

	req, err := Build(BuildInput{
		Messages:                     ir.Messages,
		SystemPrompt:                 ir.SystemPrompt,
		Tools:                        ir.Tools,
		ModelID:                      "claude-opus-5",
		ConversationID:               "conv-openai-golden",
		ProfileARN:                   "arn:aws:codewhisperer:us-east-1:1:profile/A",
		AdditionalModelRequestFields: map[string]any{"reasoning": map[string]any{"effort": "xhigh"}},
		AgentMode:                    "vibe",
		ToolDescriptionMaxLength:     10000,
		MaxPayloadBytes:              600000,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	got, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	// The tool-result turn and the following "Thanks." turn merge into the single
	// current turn, so the request ends with one user turn carrying both the tool
	// results and the user's text. That keeps history strictly alternating without
	// inserting a placeholder.
	want := `{"conversationState":{"chatTriggerType":"MANUAL","conversationId":"conv-openai-golden","currentMessage":{"userInputMessage":{"content":"Thanks.","modelId":"claude-opus-5","origin":"AI_EDITOR","userInputMessageContext":{"tools":[{"toolSpecification":{"name":"classify","description":"Classify an image.","inputSchema":{"json":{"properties":{"kind":{"type":"string"}},"required":["kind"],"type":"object"}}}}],"toolResults":[{"content":[{"text":"cat, 98% confidence"}],"status":"success","toolUseId":"call_1"}]}}},"history":[{"userInputMessage":{"content":"You are terse.\n\nWhat is in this image?","modelId":"claude-opus-5","origin":"AI_EDITOR","images":[{"format":"png","source":{"bytes":"SU1H"}}]}},{"assistantResponseMessage":{"content":"A cat. Let me confirm.","toolUses":[{"name":"classify","input":{"kind":"animal"},"toolUseId":"call_1"}],"reasoningContent":{"reasoningText":{"text":"It looks feline.","signature":"sig-1"}}}}]},"profileArn":"arn:aws:codewhisperer:us-east-1:1:profile/A","additionalModelRequestFields":{"reasoning":{"effort":"xhigh"}}}`

	if string(got) != want {
		t.Errorf("OpenAI golden payload mismatch.\n got: %s\nwant: %s", got, want)
	}
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.

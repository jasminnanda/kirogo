package translate

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// irFromAnthropic parses and converts an Anthropic fixture.
func irFromAnthropic(t *testing.T, body string) *Request {
	t.Helper()
	var req AnthropicRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}
	ir, err := FromAnthropic(&req)
	if err != nil {
		t.Fatalf("FromAnthropic: %v", err)
	}
	return ir
}

func TestAnthropicSystemAsString(t *testing.T) {
	ir := irFromAnthropic(t, `{
	  "model": "claude-opus-5",
	  "max_tokens": 1024,
	  "system": "You are terse.",
	  "messages": [{"role": "user", "content": "hi"}]
	}`)
	if ir.SystemPrompt != "You are terse." {
		t.Errorf("SystemPrompt = %q", ir.SystemPrompt)
	}
	if ir.MaxTokens != 1024 {
		t.Errorf("MaxTokens = %d", ir.MaxTokens)
	}
}

func TestAnthropicSystemAsBlockList(t *testing.T) {
	ir := irFromAnthropic(t, `{
	  "model": "m",
	  "max_tokens": 100,
	  "system": [
	    {"type": "text", "text": "First instruction."},
	    {"type": "text", "text": "Second instruction.", "cache_control": {"type": "ephemeral"}}
	  ],
	  "messages": [{"role": "user", "content": "hi"}]
	}`)
	want := "First instruction.\n\nSecond instruction."
	if ir.SystemPrompt != want {
		t.Errorf("SystemPrompt = %q, want %q", ir.SystemPrompt, want)
	}
}

func TestAnthropicSystemAbsentOrNull(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","max_tokens":1,"system":null,"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","max_tokens":1,"system":[],"messages":[{"role":"user","content":"hi"}]}`,
	} {
		ir := irFromAnthropic(t, body)
		if ir.SystemPrompt != "" {
			t.Errorf("SystemPrompt = %q, want empty for %s", ir.SystemPrompt, body)
		}
	}
}

func TestAnthropicContentBlocks(t *testing.T) {
	img := base64.StdEncoding.EncodeToString([]byte("PNG"))
	ir := irFromAnthropic(t, `{
	  "model": "m",
	  "max_tokens": 100,
	  "messages": [
	    {"role": "user", "content": [
	      {"type": "text", "text": "What is this? "},
	      {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "`+img+`"}},
	      {"type": "text", "text": "Be brief."}
	    ]},
	    {"role": "assistant", "content": [
	      {"type": "thinking", "thinking": "Considering the image.", "signature": "sig-a"},
	      {"type": "text", "text": "A cat."},
	      {"type": "tool_use", "id": "tu-1", "name": "classify", "input": {"kind": "animal"}}
	    ]},
	    {"role": "user", "content": [
	      {"type": "tool_result", "tool_use_id": "tu-1", "content": "cat"}
	    ]}
	  ]
	}`)

	if len(ir.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(ir.Messages))
	}

	first := ir.Messages[0]
	if first.Content != "What is this? Be brief." {
		t.Errorf("text blocks = %q", first.Content)
	}
	if len(first.Images) != 1 || first.Images[0].Data != img {
		t.Errorf("images = %+v", first.Images)
	}

	assistant := ir.Messages[1]
	if assistant.Content != "A cat." {
		t.Errorf("assistant text = %q", assistant.Content)
	}
	if assistant.Reasoning == nil {
		t.Fatal("thinking block was not converted to reasoning")
	}
	if assistant.Reasoning.Text != "Considering the image." || assistant.Reasoning.Signature != "sig-a" {
		t.Errorf("reasoning = %+v", assistant.Reasoning)
	}
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "tu-1" {
		t.Fatalf("tool calls = %+v", assistant.ToolCalls)
	}
	// The decoder compacts the raw value, which is semantically identical.
	if assistant.ToolCalls[0].Arguments != `{"kind":"animal"}` {
		t.Errorf("arguments = %q, want the tool input JSON", assistant.ToolCalls[0].Arguments)
	}

	result := ir.Messages[2]
	if len(result.ToolResults) != 1 || result.ToolResults[0].Content != "cat" {
		t.Errorf("tool results = %+v", result.ToolResults)
	}
}

func TestAnthropicMultipleThinkingBlocksConcatenate(t *testing.T) {
	ir := irFromAnthropic(t, `{
	  "model": "m", "max_tokens": 1,
	  "messages": [{"role": "assistant", "content": [
	    {"type": "thinking", "thinking": "First. "},
	    {"type": "thinking", "thinking": "Second.", "signature": "final-sig"}
	  ]}]
	}`)
	r := ir.Messages[0].Reasoning
	if r == nil {
		t.Fatal("no reasoning")
	}
	if r.Text != "First. Second." {
		t.Errorf("Text = %q, want the blocks concatenated", r.Text)
	}
	if r.Signature != "final-sig" {
		t.Errorf("Signature = %q, want the last non-empty one", r.Signature)
	}
}

func TestAnthropicRedactedThinking(t *testing.T) {
	blob := base64.StdEncoding.EncodeToString([]byte{9, 8, 7})
	ir := irFromAnthropic(t, `{
	  "model": "m", "max_tokens": 1,
	  "messages": [{"role": "assistant", "content": [
	    {"type": "redacted_thinking", "data": "`+blob+`"}
	  ]}]
	}`)
	r := ir.Messages[0].Reasoning
	if r == nil || len(r.RedactedContent) != 3 {
		t.Fatalf("redacted reasoning = %+v", r)
	}
	if !r.Signed() {
		t.Error("a redacted blob counts as sendable reasoning")
	}
}

func TestAnthropicInvalidRedactedThinkingIsIgnored(t *testing.T) {
	ir := irFromAnthropic(t, `{
	  "model": "m", "max_tokens": 1,
	  "messages": [{"role": "assistant", "content": [
	    {"type": "redacted_thinking", "data": "***not base64***"}
	  ]}]
	}`)
	r := ir.Messages[0].Reasoning
	if r != nil && len(r.RedactedContent) != 0 {
		t.Errorf("invalid base64 should be ignored, got %v", r.RedactedContent)
	}
}

func TestAnthropicToolResultWithBlockContent(t *testing.T) {
	shot := base64.StdEncoding.EncodeToString([]byte("SHOT"))
	ir := irFromAnthropic(t, `{
	  "model": "m", "max_tokens": 1,
	  "messages": [{"role": "user", "content": [
	    {"type": "tool_result", "tool_use_id": "tu-1", "content": [
	      {"type": "text", "text": "captured "},
	      {"type": "text", "text": "the page"},
	      {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "`+shot+`"}}
	    ]}
	  ]}]
	}`)

	tr := ir.Messages[0].ToolResults[0]
	if tr.Content != "captured the page" {
		t.Errorf("content = %q", tr.Content)
	}
	if len(tr.Images) != 1 || tr.Images[0].Data != shot {
		t.Errorf("an image returned by an MCP tool was lost: %+v", tr.Images)
	}
}

func TestAnthropicToolResultErrorFlag(t *testing.T) {
	ir := irFromAnthropic(t, `{
	  "model": "m", "max_tokens": 1,
	  "messages": [{"role": "user", "content": [
	    {"type": "tool_result", "tool_use_id": "t1", "content": "boom", "is_error": true},
	    {"type": "tool_result", "tool_use_id": "t2", "content": "fine"}
	  ]}]
	}`)
	results := ir.Messages[0].ToolResults
	if len(results) != 2 {
		t.Fatalf("results = %d", len(results))
	}
	if !results[0].IsError || results[0].Status() != "error" {
		t.Errorf("result 0 = %+v, status %q", results[0], results[0].Status())
	}
	if results[1].IsError || results[1].Status() != "success" {
		t.Errorf("result 1 = %+v, status %q", results[1], results[1].Status())
	}
}

func TestAnthropicToolsUseInputSchema(t *testing.T) {
	ir := irFromAnthropic(t, `{
	  "model": "m", "max_tokens": 1,
	  "messages": [{"role": "user", "content": "hi"}],
	  "tools": [
	    {"name": "get_weather", "description": "Look it up.",
	     "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}}},
	    {"type": "web_search_20250305", "name": ""}
	  ]
	}`)

	if len(ir.Tools) != 1 {
		t.Fatalf("tools = %d, want the nameless server-side tool skipped: %+v", len(ir.Tools), ir.Tools)
	}
	tool := ir.Tools[0]
	if tool.Name != "get_weather" || tool.Description != "Look it up." {
		t.Errorf("tool = %+v", tool)
	}
	if tool.InputSchema["type"] != "object" {
		t.Errorf("input schema = %+v", tool.InputSchema)
	}
}

func TestAnthropicHTTPImagesAreSkipped(t *testing.T) {
	ir := irFromAnthropic(t, `{
	  "model": "m", "max_tokens": 1,
	  "messages": [{"role": "user", "content": [
	    {"type": "text", "text": "look"},
	    {"type": "image", "source": {"type": "url", "url": "https://example.test/x.png"}}
	  ]}]
	}`)
	if len(ir.Messages[0].Images) != 0 {
		t.Errorf("images = %+v, want none", ir.Messages[0].Images)
	}
	if ir.Messages[0].Content != "look" {
		t.Errorf("content = %q", ir.Messages[0].Content)
	}
}

func TestAnthropicPlainStringContent(t *testing.T) {
	ir := irFromAnthropic(t, `{"model":"m","max_tokens":1,
	  "messages":[{"role":"user","content":"plain"},{"role":"assistant","content":"reply"}]}`)
	if ir.Messages[0].Content != "plain" || ir.Messages[1].Content != "reply" {
		t.Errorf("messages = %+v", ir.Messages)
	}
	if ir.Messages[1].Role != RoleAssistant {
		t.Errorf("role = %q", ir.Messages[1].Role)
	}
}

func TestAnthropicThinkingBudgetToEffortTier(t *testing.T) {
	cases := []struct {
		name         string
		thinking     *AnthropicThinking
		wantLevel    string
		wantDisabled bool
	}{
		{"nil", nil, "", false},
		{"disabled", &AnthropicThinking{Type: "disabled"}, "", true},
		{"disabled mixed case", &AnthropicThinking{Type: "Disabled"}, "", true},
		{"disabled ignores budget", &AnthropicThinking{Type: "disabled", BudgetTokens: 100000}, "", true},
		{"enabled with no budget", &AnthropicThinking{Type: "enabled"}, "", false},
		{"negative budget", &AnthropicThinking{Type: "enabled", BudgetTokens: -5}, "", false},
		{"1 token", &AnthropicThinking{Type: "enabled", BudgetTokens: 1}, "low", false},
		{"1024 exactly", &AnthropicThinking{Type: "enabled", BudgetTokens: 1024}, "low", false},
		{"1025", &AnthropicThinking{Type: "enabled", BudgetTokens: 1025}, "medium", false},
		{"4096 exactly", &AnthropicThinking{Type: "enabled", BudgetTokens: 4096}, "medium", false},
		{"4097", &AnthropicThinking{Type: "enabled", BudgetTokens: 4097}, "high", false},
		{"8192 exactly", &AnthropicThinking{Type: "enabled", BudgetTokens: 8192}, "high", false},
		{"8193", &AnthropicThinking{Type: "enabled", BudgetTokens: 8193}, "xhigh", false},
		{"16384 exactly", &AnthropicThinking{Type: "enabled", BudgetTokens: 16384}, "xhigh", false},
		{"16385", &AnthropicThinking{Type: "enabled", BudgetTokens: 16385}, "max", false},
		{"very large", &AnthropicThinking{Type: "enabled", BudgetTokens: 200000}, "max", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			level, disabled := effortFromThinking(tc.thinking)
			if level != tc.wantLevel || disabled != tc.wantDisabled {
				t.Errorf("effortFromThinking(%+v) = (%q, %v), want (%q, %v)",
					tc.thinking, level, disabled, tc.wantLevel, tc.wantDisabled)
			}
		})
	}
}

func TestAnthropicThinkingReachesTheIR(t *testing.T) {
	ir := irFromAnthropic(t, `{"model":"m","max_tokens":1,
	  "messages":[{"role":"user","content":"q"}],
	  "thinking":{"type":"enabled","budget_tokens":10000}}`)
	if ir.EffortLevel != "xhigh" {
		t.Errorf("EffortLevel = %q, want xhigh for a 10000 token budget", ir.EffortLevel)
	}

	off := irFromAnthropic(t, `{"model":"m","max_tokens":1,
	  "messages":[{"role":"user","content":"q"}],
	  "thinking":{"type":"disabled"}}`)
	if !off.DisableReasoning {
		t.Error("thinking type disabled should disable reasoning")
	}
}

func TestAnthropicUnknownFieldsAreIgnored(t *testing.T) {
	ir := irFromAnthropic(t, `{
	  "model": "m", "max_tokens": 100,
	  "messages": [{"role": "user", "content": "hi"}],
	  "temperature": 0.5, "top_p": 0.9, "top_k": 40,
	  "stop_sequences": ["END"], "tool_choice": {"type": "auto"},
	  "metadata": {"user_id": "abc"},
	  "anthropic_beta": ["computer-use-2024"],
	  "invented_next_year": {"a": 1}
	}`)
	if len(ir.Messages) != 1 {
		t.Errorf("messages = %+v", ir.Messages)
	}
}

func TestAnthropicNoMessagesIsAnError(t *testing.T) {
	_, err := FromAnthropic(&AnthropicRequest{Model: "m", MaxTokens: 10})
	if err == nil {
		t.Fatal("a request with no messages must be rejected")
	}
	if !strings.Contains(err.Error(), "no messages") {
		t.Errorf("error = %q", err)
	}
}

func TestAnthropicToolUseWithNoInput(t *testing.T) {
	ir := irFromAnthropic(t, `{
	  "model": "m", "max_tokens": 1,
	  "messages": [{"role": "assistant", "content": [
	    {"type": "tool_use", "id": "t1", "name": "noargs"}
	  ]}]
	}`)
	tc := ir.Messages[0].ToolCalls[0]
	if tc.Arguments != "{}" {
		t.Errorf("Arguments = %q, want an empty object", tc.Arguments)
	}
	obj, err := json.Marshal(tc.ArgumentsObject())
	if err != nil {
		t.Fatal(err)
	}
	if string(obj) != "{}" {
		t.Errorf("ArgumentsObject() = %s", obj)
	}
}

func TestAnthropicUnknownBlockTypeKeepsItsText(t *testing.T) {
	ir := irFromAnthropic(t, `{
	  "model": "m", "max_tokens": 1,
	  "messages": [{"role": "user", "content": [
	    {"type": "text", "text": "known "},
	    {"type": "invented_block", "text": "also kept"}
	  ]}]
	}`)
	if ir.Messages[0].Content != "known also kept" {
		t.Errorf("content = %q, want text from the unknown block kept too", ir.Messages[0].Content)
	}
}

func TestAnthropicRolesOtherThanAssistantBecomeUser(t *testing.T) {
	ir := irFromAnthropic(t, `{
	  "model": "m", "max_tokens": 1,
	  "messages": [{"role": "human", "content": "legacy role"}]
	}`)
	if ir.Messages[0].Role != RoleUser {
		t.Errorf("role = %q, want user", ir.Messages[0].Role)
	}
}

func TestAnthropicEndToEndGoldenPayload(t *testing.T) {
	img := base64.StdEncoding.EncodeToString([]byte("IMG"))
	ir := irFromAnthropic(t, `{
	  "model": "claude-opus-5",
	  "max_tokens": 2048,
	  "stream": true,
	  "system": [{"type": "text", "text": "You are terse."}],
	  "thinking": {"type": "enabled", "budget_tokens": 10000},
	  "tools": [{"name": "classify", "description": "Classify an image.",
	    "input_schema": {"type": "object", "required": ["kind"], "additionalProperties": false,
	      "properties": {"kind": {"type": "string"}}}}],
	  "messages": [
	    {"role": "user", "content": [
	      {"type": "text", "text": "What is in this image?"},
	      {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "`+img+`"}}
	    ]},
	    {"role": "assistant", "content": [
	      {"type": "thinking", "thinking": "It looks feline.", "signature": "sig-1"},
	      {"type": "text", "text": "A cat. Let me confirm."},
	      {"type": "tool_use", "id": "toolu_1", "name": "classify", "input": {"kind": "animal"}}
	    ]},
	    {"role": "user", "content": [
	      {"type": "tool_result", "tool_use_id": "toolu_1", "content": "cat, 98% confidence"}
	    ]}
	  ]
	}`)

	if ir.EffortLevel != "xhigh" {
		t.Fatalf("EffortLevel = %q, want xhigh from the 10000 token budget", ir.EffortLevel)
	}

	req, err := Build(BuildInput{
		Messages:                     ir.Messages,
		SystemPrompt:                 ir.SystemPrompt,
		Tools:                        ir.Tools,
		ModelID:                      "claude-opus-5",
		ConversationID:               "conv-anthropic-golden",
		ProfileARN:                   "arn:aws:codewhisperer:us-east-1:1:profile/A",
		AdditionalModelRequestFields: map[string]any{"reasoning": map[string]any{"effort": ir.EffortLevel}},
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
	want := `{"conversationState":{"chatTriggerType":"MANUAL","conversationId":"conv-anthropic-golden","currentMessage":{"userInputMessage":{"content":"(no content)","modelId":"claude-opus-5","origin":"AI_EDITOR","userInputMessageContext":{"tools":[{"toolSpecification":{"name":"classify","description":"Classify an image.","inputSchema":{"json":{"properties":{"kind":{"type":"string"}},"required":["kind"],"type":"object"}}}}],"toolResults":[{"content":[{"text":"cat, 98% confidence"}],"status":"success","toolUseId":"toolu_1"}]}}},"history":[{"userInputMessage":{"content":"You are terse.\n\nWhat is in this image?","modelId":"claude-opus-5","origin":"AI_EDITOR","images":[{"format":"png","source":{"bytes":"SU1H"}}]}},{"assistantResponseMessage":{"content":"A cat. Let me confirm.","toolUses":[{"name":"classify","input":{"kind":"animal"},"toolUseId":"toolu_1"}],"reasoningContent":{"reasoningText":{"text":"It looks feline.","signature":"sig-1"}}}}]},"profileArn":"arn:aws:codewhisperer:us-east-1:1:profile/A","additionalModelRequestFields":{"reasoning":{"effort":"xhigh"}}}`

	if string(got) != want {
		t.Errorf("Anthropic golden payload mismatch.\n got: %s\nwant: %s", got, want)
	}
}

func TestBothAPIsProduceTheSameKiroPayloadForTheSameConversation(t *testing.T) {
	// The same conversation expressed in each API's shape must translate to the
	// same Kiro request, which is what keeps the two surfaces consistent.
	img := base64.StdEncoding.EncodeToString([]byte("IMG"))

	openAI := irFromOpenAI(t, `{
	  "model": "claude-opus-5",
	  "messages": [
	    {"role": "system", "content": "Be terse."},
	    {"role": "user", "content": [
	      {"type": "text", "text": "Describe it."},
	      {"type": "image_url", "image_url": {"url": "data:image/png;base64,`+img+`"}}
	    ]},
	    {"role": "assistant", "content": "Checking.",
	     "reasoning_content": "hmm", "reasoning_signature": "s1",
	     "tool_calls": [{"id": "x1", "type": "function", "function": {"name": "look", "arguments": "{\"a\":1}"}}]},
	    {"role": "tool", "tool_call_id": "x1", "content": "found it"}
	  ],
	  "tools": [{"type": "function", "function": {"name": "look", "description": "Look.",
	    "parameters": {"type": "object", "properties": {"a": {"type": "number"}}}}}]
	}`)

	anthropic := irFromAnthropic(t, `{
	  "model": "claude-opus-5",
	  "max_tokens": 100,
	  "system": "Be terse.",
	  "messages": [
	    {"role": "user", "content": [
	      {"type": "text", "text": "Describe it."},
	      {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "`+img+`"}}
	    ]},
	    {"role": "assistant", "content": [
	      {"type": "thinking", "thinking": "hmm", "signature": "s1"},
	      {"type": "text", "text": "Checking."},
	      {"type": "tool_use", "id": "x1", "name": "look", "input": {"a": 1}}
	    ]},
	    {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "x1", "content": "found it"}]}
	  ],
	  "tools": [{"name": "look", "description": "Look.",
	    "input_schema": {"type": "object", "properties": {"a": {"type": "number"}}}}]
	}`)

	build := func(ir *Request) string {
		req, err := Build(BuildInput{
			Messages:                 ir.Messages,
			SystemPrompt:             ir.SystemPrompt,
			Tools:                    ir.Tools,
			ModelID:                  "claude-opus-5",
			ConversationID:           "same",
			ToolDescriptionMaxLength: 10000,
			MaxPayloadBytes:          600000,
		})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		data, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	fromOpenAI := build(openAI)
	fromAnthropic := build(anthropic)
	if fromOpenAI != fromAnthropic {
		t.Errorf("the two API surfaces disagree for the same conversation:\n OpenAI:    %s\n Anthropic: %s",
			fromOpenAI, fromAnthropic)
	}
}

func TestDecodeBase64Variants(t *testing.T) {
	payload := []byte{0xfb, 0xff, 0x01, 0x02}
	encodings := map[string]string{
		"standard":     base64.StdEncoding.EncodeToString(payload),
		"raw standard": base64.RawStdEncoding.EncodeToString(payload),
		"url safe":     base64.URLEncoding.EncodeToString(payload),
		"raw url safe": base64.RawURLEncoding.EncodeToString(payload),
	}
	for name, encoded := range encodings {
		t.Run(name, func(t *testing.T) {
			got, err := decodeBase64(encoded)
			if err != nil {
				t.Fatalf("decodeBase64(%q): %v", encoded, err)
			}
			if string(got) != string(payload) {
				t.Errorf("decoded %v, want %v", got, payload)
			}
		})
	}
	if _, err := decodeBase64("not valid base64 at all!!!"); err == nil {
		t.Error("invalid input should fail")
	}
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.

package kiro

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// fullyPopulatedRequest builds a request that exercises every field kirogo emits.
func fullyPopulatedRequest() *Request {
	return &Request{
		ConversationState: ConversationState{
			ChatTriggerType: ChatTriggerType,
			ConversationID:  "conv-0001",
			History: []HistoryEntry{
				{UserInputMessage: &UserInputMessage{
					Content: "What is the weather in Berlin?",
					ModelID: "claude-opus-5",
					Origin:  Origin,
					Images: []ImageBlock{
						{Format: "png", Source: ImageSource{Bytes: "aW1hZ2U="}},
					},
				}},
				{AssistantResponseMessage: &AssistantResponseMessage{
					Content: "Let me check.",
					ToolUses: []ToolUse{{
						Name:      "get_weather",
						Input:     map[string]any{"city": "Berlin"},
						ToolUseID: "tu-1",
					}},
					ReasoningContent: NewReasoningText("I should call the tool.", "sig-abc"),
				}},
				{UserInputMessage: &UserInputMessage{
					Content: Placeholder,
					ModelID: "claude-opus-5",
					Origin:  Origin,
					UserInputMessageContext: &UserInputMessageContext{
						ToolResults: []ToolResult{{
							Content:   []ToolResultContent{{Text: "18C and sunny"}},
							Status:    ToolResultSuccess,
							ToolUseID: "tu-1",
						}},
					},
				}},
			},
			CurrentMessage: CurrentMessage{
				UserInputMessage: &UserInputMessage{
					Content: "Thanks. And Paris?",
					ModelID: "claude-opus-5",
					Origin:  Origin,
					UserInputMessageContext: &UserInputMessageContext{
						Tools: []Tool{{ToolSpecification: ToolSpecification{
							Name:        "get_weather",
							Description: "Look up the weather for a city.",
							InputSchema: ToolInputSchema{JSON: map[string]any{
								"type":       "object",
								"properties": map[string]any{"city": map[string]any{"type": "string"}},
								"required":   []any{"city"},
							}},
						}}},
					},
				},
			},
		},
		ProfileARN:                   "arn:aws:codewhisperer:us-east-1:123456789012:profile/ABCDEF",
		SystemPrompt:                 "You are a concise assistant.",
		AdditionalModelRequestFields: EffortFields("reasoning", "xhigh"),
		AgentMode:                    "vibe",
	}
}

const goldenRequestJSON = `{"conversationState":{"chatTriggerType":"MANUAL","conversationId":"conv-0001","currentMessage":{"userInputMessage":{"content":"Thanks. And Paris?","modelId":"claude-opus-5","origin":"AI_EDITOR","userInputMessageContext":{"tools":[{"toolSpecification":{"name":"get_weather","description":"Look up the weather for a city.","inputSchema":{"json":{"properties":{"city":{"type":"string"}},"required":["city"],"type":"object"}}}}]}}},"history":[{"userInputMessage":{"content":"What is the weather in Berlin?","modelId":"claude-opus-5","origin":"AI_EDITOR","images":[{"format":"png","source":{"bytes":"aW1hZ2U="}}]}},{"assistantResponseMessage":{"content":"Let me check.","toolUses":[{"name":"get_weather","input":{"city":"Berlin"},"toolUseId":"tu-1"}],"reasoningContent":{"reasoningText":{"text":"I should call the tool.","signature":"sig-abc"}}}},{"userInputMessage":{"content":"(no content)","modelId":"claude-opus-5","origin":"AI_EDITOR","userInputMessageContext":{"toolResults":[{"content":[{"text":"18C and sunny"}],"status":"success","toolUseId":"tu-1"}]}}}]},"profileArn":"arn:aws:codewhisperer:us-east-1:123456789012:profile/ABCDEF","systemPrompt":"You are a concise assistant.","additionalModelRequestFields":{"reasoning":{"effort":"xhigh"}}}`

func TestGoldenRequestJSON(t *testing.T) {
	got, err := fullyPopulatedRequest().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != goldenRequestJSON {
		t.Errorf("request JSON does not match the golden value.\n got: %s\nwant: %s", got, goldenRequestJSON)
	}
}

func TestSystemPromptAndEffortAreTopLevel(t *testing.T) {
	data, err := fullyPopulatedRequest().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"conversationState", "profileArn", "systemPrompt", "additionalModelRequestFields"} {
		if _, ok := top[key]; !ok {
			t.Errorf("%q must be a top-level field", key)
		}
	}
	// agentMode binds to a header, so it must not be in the body.
	if _, ok := top["agentMode"]; ok {
		t.Error("agentMode must not appear in the request body: it is a header")
	}

	var effort map[string]map[string]string
	if err := json.Unmarshal(top["additionalModelRequestFields"], &effort); err != nil {
		t.Fatal(err)
	}
	if effort["reasoning"]["effort"] != "xhigh" {
		t.Errorf("additionalModelRequestFields = %v, want {reasoning:{effort:xhigh}}", effort)
	}

	// The system prompt must not have been folded into any message content.
	if strings.Contains(string(data), `"content":"You are a concise assistant.`) {
		t.Error("the system prompt was folded into a message instead of staying top-level")
	}
}

func TestImagesSitOnUserInputMessageNotInContext(t *testing.T) {
	data, err := fullyPopulatedRequest().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		ConversationState struct {
			History []struct {
				UserInputMessage *struct {
					Images                  []ImageBlock `json:"images"`
					UserInputMessageContext *struct {
						Images []ImageBlock `json:"images"`
					} `json:"userInputMessageContext"`
				} `json:"userInputMessage"`
			} `json:"history"`
		} `json:"conversationState"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}

	first := parsed.ConversationState.History[0].UserInputMessage
	if first == nil {
		t.Fatal("first history entry is not a user message")
	}
	if len(first.Images) != 1 {
		t.Fatalf("images = %v, want one image on userInputMessage", first.Images)
	}
	if first.Images[0].Format != "png" {
		t.Errorf("image format = %q, want the bare subtype png", first.Images[0].Format)
	}
	if first.UserInputMessageContext != nil && len(first.UserInputMessageContext.Images) > 0 {
		t.Error("images must not be nested inside userInputMessageContext")
	}
}

func TestOmitEmptyBehaviour(t *testing.T) {
	minimal := &Request{
		ConversationState: ConversationState{
			ChatTriggerType: ChatTriggerType,
			ConversationID:  "c",
			CurrentMessage: CurrentMessage{
				UserInputMessage: &UserInputMessage{Content: Placeholder},
			},
		},
	}
	data, err := minimal.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	want := `{"conversationState":{"chatTriggerType":"MANUAL","conversationId":"c","currentMessage":{"userInputMessage":{"content":"(no content)"}}}}`
	if got != want {
		t.Errorf("minimal request =\n %s\nwant\n %s", got, want)
	}

	for _, absent := range []string{"history", "profileArn", "systemPrompt", "additionalModelRequestFields",
		"userInputMessageContext", "images", "modelId", "origin"} {
		if strings.Contains(got, `"`+absent+`"`) {
			t.Errorf("%q should be omitted when empty, got %s", absent, got)
		}
	}
}

func TestContentIsNeverOmittedEvenWhenEmpty(t *testing.T) {
	// content has no omitempty on purpose: the field must always be present, and
	// callers are responsible for filling in the placeholder.
	msg := &UserInputMessage{}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"content":""}` {
		t.Errorf("marshalled = %s, want content always present", data)
	}
}

func TestUserInputMessageContextIsEmpty(t *testing.T) {
	cases := []struct {
		name string
		ctx  *UserInputMessageContext
		want bool
	}{
		{"nil", nil, true},
		{"zero value", &UserInputMessageContext{}, true},
		{"has tools", &UserInputMessageContext{Tools: []Tool{{}}}, false},
		{"has tool results", &UserInputMessageContext{ToolResults: []ToolResult{{}}}, false},
		{"empty slices", &UserInputMessageContext{Tools: []Tool{}, ToolResults: []ToolResult{}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ctx.IsEmpty(); got != tc.want {
				t.Errorf("IsEmpty() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNewReasoningText(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		signature string
		wantNil   bool
	}{
		{"both present", "thought", "sig", false},
		{"no signature", "thought", "", true},
		{"no text", "", "sig", true},
		{"neither", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NewReasoningText(tc.text, tc.signature)
			if tc.wantNil {
				if got != nil {
					t.Errorf("NewReasoningText(%q, %q) = %+v, want nil: unsigned reasoning must never be sent",
						tc.text, tc.signature, got)
				}
				return
			}
			if got == nil || got.ReasoningText == nil {
				t.Fatalf("NewReasoningText(%q, %q) = nil, want a signed block", tc.text, tc.signature)
			}
			if got.ReasoningText.Text != tc.text || got.ReasoningText.Signature != tc.signature {
				t.Errorf("block = %+v", got.ReasoningText)
			}
		})
	}
}

func TestReasoningContentUsesTheUnionWrapper(t *testing.T) {
	rc := NewReasoningText("thought", "sig")
	data, err := json.Marshal(rc)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"reasoningText":{"text":"thought","signature":"sig"}}`
	if string(data) != want {
		t.Errorf("reasoningContent = %s, want the reasoningText union wrapper %s", data, want)
	}
}

func TestNewRedactedReasoning(t *testing.T) {
	if got := NewRedactedReasoning(nil); got != nil {
		t.Errorf("NewRedactedReasoning(nil) = %+v, want nil", got)
	}
	blob := []byte{0x00, 0x01, 0xfe, 0xff}
	rc := NewRedactedReasoning(blob)
	if rc == nil {
		t.Fatal("want a redacted block")
	}
	if rc.ReasoningText != nil {
		t.Error("a redacted block must not also set reasoningText")
	}
	if want := base64.StdEncoding.EncodeToString(blob); rc.RedactedContent != want {
		t.Errorf("RedactedContent = %q, want %q", rc.RedactedContent, want)
	}
	data, err := json.Marshal(rc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "reasoningText") {
		t.Errorf("marshalled = %s, should contain only redactedContent", data)
	}
}

func TestStripReasoning(t *testing.T) {
	req := fullyPopulatedRequest()
	if !req.StripReasoning() {
		t.Fatal("StripReasoning() = false, want true when reasoning is present")
	}
	data, err := req.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "reasoningContent") {
		t.Errorf("reasoningContent survived the strip: %s", data)
	}
	// Everything else must be untouched.
	if !strings.Contains(string(data), `"toolUses"`) {
		t.Error("StripReasoning removed more than it should have")
	}
	// Stripping again is a no-op and reports so, which lets the caller skip a
	// pointless retry.
	if req.StripReasoning() {
		t.Error("a second StripReasoning() should report that nothing changed")
	}
}

func TestStripReasoningOnRequestWithoutReasoning(t *testing.T) {
	req := &Request{ConversationState: ConversationState{
		History: []HistoryEntry{
			{UserInputMessage: &UserInputMessage{Content: "hi"}},
			{AssistantResponseMessage: &AssistantResponseMessage{Content: "hello"}},
		},
	}}
	if req.StripReasoning() {
		t.Error("StripReasoning() = true, want false when there is no reasoning")
	}
}

func TestEffortFields(t *testing.T) {
	cases := []struct {
		name       string
		schemaPath string
		level      string
		want       string
	}{
		{"reasoning path", "reasoning", "xhigh", `{"reasoning":{"effort":"xhigh"}}`},
		{"output_config path", "output_config", "max", `{"output_config":{"effort":"max"}}`},
		{"no path", "", "high", ""},
		{"no level", "reasoning", "", ""},
		{"neither", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EffortFields(tc.schemaPath, tc.level)
			if tc.want == "" {
				if got != nil {
					t.Errorf("EffortFields(%q, %q) = %v, want nil", tc.schemaPath, tc.level, got)
				}
				return
			}
			data, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != tc.want {
				t.Errorf("EffortFields(%q, %q) = %s, want %s", tc.schemaPath, tc.level, data, tc.want)
			}
		})
	}
}

func TestSizeBytesMatchesMarshalledLength(t *testing.T) {
	req := fullyPopulatedRequest()
	data, err := req.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	size, err := req.SizeBytes()
	if err != nil {
		t.Fatal(err)
	}
	if size != len(data) {
		t.Errorf("SizeBytes() = %d, want %d", size, len(data))
	}
}

func TestMarshalReportsUnencodableInput(t *testing.T) {
	req := &Request{ConversationState: ConversationState{
		CurrentMessage: CurrentMessage{UserInputMessage: &UserInputMessage{Content: "x"}},
	}}
	// A channel cannot be encoded as JSON.
	req.AdditionalModelRequestFields = map[string]any{"bad": make(chan int)}
	if _, err := req.Marshal(); err == nil {
		t.Error("Marshal should report an unencodable field")
	}
	if _, err := req.SizeBytes(); err == nil {
		t.Error("SizeBytes should propagate the encode failure")
	}
}

func TestToolUseInputIsAnObjectNotAString(t *testing.T) {
	tu := ToolUse{Name: "f", Input: map[string]any{"a": 1}, ToolUseID: "id"}
	data, err := json.Marshal(tu)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"input":{"a":1}`) {
		t.Errorf("marshalled = %s, want input as a JSON object", data)
	}
}

func TestToolResultStatuses(t *testing.T) {
	if ToolResultSuccess != "success" || ToolResultError != "error" {
		t.Errorf("tool result statuses = %q/%q, want success/error", ToolResultSuccess, ToolResultError)
	}
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.

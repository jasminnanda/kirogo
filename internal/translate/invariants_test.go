package translate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jasminnanda/kirogo/internal/kiro"
)

// buildInput returns a BuildInput with sane defaults for a test.
func buildInput(messages []Message) BuildInput {
	return BuildInput{
		Messages:                 messages,
		ModelID:                  "claude-opus-5",
		ConversationID:           "conv-1",
		ToolDescriptionMaxLength: 10000,
		MaxPayloadBytes:          600000,
	}
}

// mustBuild builds a request, failing the test on error.
func mustBuild(t *testing.T, in BuildInput) *kiro.Request {
	t.Helper()
	req, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return req
}

// historyRoles renders the history as a compact role string, using "u" and "a".
func historyRoles(req *kiro.Request) string {
	var b strings.Builder
	for _, e := range req.ConversationState.History {
		if e.UserInputMessage != nil {
			b.WriteString("u")
		}
		if e.AssistantResponseMessage != nil {
			b.WriteString("a")
		}
	}
	return b.String()
}

func TestRule1ContentIsNeverEmpty(t *testing.T) {
	req := mustBuild(t, buildInput([]Message{
		{Role: RoleUser, Content: ""},
		{Role: RoleAssistant, Content: "   "},
		{Role: RoleUser, Content: "\n\t "},
	}))

	current := req.ConversationState.CurrentMessage.UserInputMessage
	if current.Content != kiro.Placeholder {
		t.Errorf("current content = %q, want the placeholder", current.Content)
	}
	for i, e := range req.ConversationState.History {
		var content string
		if e.UserInputMessage != nil {
			content = e.UserInputMessage.Content
		} else {
			content = e.AssistantResponseMessage.Content
		}
		if content == "" {
			t.Errorf("history entry %d has empty content", i)
		}
		if strings.TrimSpace(content) == "" {
			t.Errorf("history entry %d content is only whitespace: %q", i, content)
		}
	}
}

func TestRule2AlternatingRoles(t *testing.T) {
	cases := []struct {
		name        string
		messages    []Message
		wantHistory string
	}{
		{
			name: "three consecutive user turns",
			messages: []Message{
				{Role: RoleUser, Content: "first"},
				{Role: RoleUser, Content: "second"},
				{Role: RoleUser, Content: "third"},
			},
			// Merging runs before alternation, so three user turns become one.
			wantHistory: "",
		},
		{
			name: "user assistant user",
			messages: []Message{
				{Role: RoleUser, Content: "a"},
				{Role: RoleAssistant, Content: "b"},
				{Role: RoleUser, Content: "c"},
			},
			wantHistory: "ua",
		},
		{
			name: "trailing consecutive user turns merge rather than gaining a placeholder",
			messages: []Message{
				{Role: RoleUser, Content: "a"},
				{Role: RoleAssistant, Content: "b"},
				{Role: RoleUser, Content: "c"},
				{Role: RoleUser, Content: "d"},
			},
			wantHistory: "ua",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := mustBuild(t, buildInput(tc.messages))
			if got := historyRoles(req); got != tc.wantHistory {
				t.Errorf("history roles = %q, want %q", got, tc.wantHistory)
			}
			assertAlternating(t, req)
		})
	}
}

// assertAlternating checks the history never has two turns from the same side.
func assertAlternating(t *testing.T, req *kiro.Request) {
	t.Helper()
	roles := historyRoles(req)
	for i := 1; i < len(roles); i++ {
		if roles[i] == roles[i-1] {
			t.Errorf("history has consecutive %c turns: %q", roles[i], roles)
			return
		}
	}
	// History must end with an assistant turn, because the current turn is a user
	// turn and the two together have to alternate.
	if len(roles) > 0 && roles[len(roles)-1] != 'a' {
		t.Errorf("history ends with %c, but the current turn is a user turn: %q", roles[len(roles)-1], roles)
	}
}

func TestRule2InsertsPlaceholderWhenMergingCannotFix(t *testing.T) {
	// Merging handles turns that already share a role. Placeholders exist for
	// adjacency that only appears once unknown roles become user turns, which
	// happens after merging has already run.
	req := mustBuild(t, buildInput([]Message{
		{Role: RoleUser, Content: "start"},
		{Role: "developer", Content: "context"},
		{Role: "reviewer", Content: "more context"},
		{Role: RoleUser, Content: "question"},
	}))
	assertAlternating(t, req)

	var placeholders int
	for _, e := range req.ConversationState.History {
		if e.AssistantResponseMessage != nil && e.AssistantResponseMessage.Content == kiro.Placeholder {
			placeholders++
		}
	}
	if placeholders == 0 {
		t.Error("expected placeholder assistant turns between the normalised user turns")
	}
	// Nothing may be lost to the insertion.
	body := marshalString(t, req)
	for _, want := range []string{"start", "context", "more context", "question"} {
		if !strings.Contains(body, want) {
			t.Errorf("content %q was lost", want)
		}
	}
}

func TestRule3FirstHistoryEntryIsAUserTurn(t *testing.T) {
	req := mustBuild(t, buildInput([]Message{
		{Role: RoleAssistant, Content: "I spoke first"},
		{Role: RoleUser, Content: "now me"},
	}))

	if len(req.ConversationState.History) == 0 {
		t.Fatal("expected history")
	}
	first := req.ConversationState.History[0]
	if first.UserInputMessage == nil {
		t.Fatal("the first history entry must be a user turn")
	}
	if first.UserInputMessage.Content != kiro.Placeholder {
		t.Errorf("synthetic first turn content = %q, want the placeholder", first.UserInputMessage.Content)
	}
}

func TestRule3IsNotAppliedWhenTheFirstTurnIsAlreadyUser(t *testing.T) {
	req := mustBuild(t, buildInput([]Message{
		{Role: RoleUser, Content: "hello"},
		{Role: RoleAssistant, Content: "hi"},
		{Role: RoleUser, Content: "again"},
	}))
	if got := len(req.ConversationState.History); got != 2 {
		t.Errorf("history has %d entries, want 2 with nothing prepended", got)
	}
	if req.ConversationState.History[0].UserInputMessage.Content != "hello" {
		t.Error("an unnecessary placeholder was prepended")
	}
}

func TestRule4UnknownRolesBecomeUser(t *testing.T) {
	req := mustBuild(t, buildInput([]Message{
		{Role: "developer", Content: "context one"},
		{Role: "moderator", Content: "context two"},
		{Role: RoleUser, Content: "question"},
	}))

	// Every history entry must be one of the two Kiro roles.
	for i, e := range req.ConversationState.History {
		if e.UserInputMessage == nil && e.AssistantResponseMessage == nil {
			t.Errorf("history entry %d is neither a user nor an assistant turn", i)
		}
	}
	assertAlternating(t, req)

	// The content must survive the normalisation.
	all := marshalString(t, req)
	for _, want := range []string{"context one", "context two", "question"} {
		if !strings.Contains(all, want) {
			t.Errorf("content %q was lost", want)
		}
	}
}

func TestRule4RunsBeforeAlternation(t *testing.T) {
	// Two turns with different unknown roles do not merge, but both become user
	// turns, so the alternation fix has to see them as adjacent.
	req := mustBuild(t, buildInput([]Message{
		{Role: "developer", Content: "one"},
		{Role: "reviewer", Content: "two"},
		{Role: "auditor", Content: "three"},
	}))
	assertAlternating(t, req)
}

func TestRule5MergesAdjacentSameRoleTurns(t *testing.T) {
	req := mustBuild(t, BuildInput{
		ModelID: "m",
		Tools:   []Tool{{Name: "t", Description: "d"}},
		Messages: []Message{
			{Role: RoleUser, Content: "line one"},
			{Role: RoleUser, Content: "line two"},
			{Role: RoleAssistant, Content: "reply one", ToolCalls: []ToolCall{{ID: "a", Name: "t", Arguments: `{"x":1}`}}},
			{Role: RoleAssistant, Content: "reply two", ToolCalls: []ToolCall{{ID: "b", Name: "t", Arguments: `{"y":2}`}}},
			{Role: RoleUser, Content: "final"},
		},
	})

	first := req.ConversationState.History[0].UserInputMessage
	if first.Content != "line one\nline two" {
		t.Errorf("merged user content = %q, want the two lines joined with a newline", first.Content)
	}

	assistant := req.ConversationState.History[1].AssistantResponseMessage
	if assistant == nil {
		t.Fatal("expected an assistant history entry")
	}
	if assistant.Content != "reply one\nreply two" {
		t.Errorf("merged assistant content = %q", assistant.Content)
	}
	if len(assistant.ToolUses) != 2 {
		t.Errorf("merged tool calls = %d, want both concatenated", len(assistant.ToolUses))
	}
}

func TestRule5MergeConcatenatesToolResultsAndImages(t *testing.T) {
	req := mustBuild(t, BuildInput{
		ModelID: "m",
		Tools:   []Tool{{Name: "t", Description: "d"}},
		Messages: []Message{
			{Role: RoleUser, Content: "start"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{
				{ID: "1", Name: "t", Arguments: "{}"},
				{ID: "2", Name: "t", Arguments: "{}"},
			}},
			{Role: RoleUser, ToolResults: []ToolResult{{ToolUseID: "1", Content: "r1"}},
				Images: []Image{{MediaType: "image/png", Data: "AAA"}}},
			{Role: RoleUser, ToolResults: []ToolResult{{ToolUseID: "2", Content: "r2"}},
				Images: []Image{{MediaType: "image/jpeg", Data: "BBB"}}},
		},
	})

	current := req.ConversationState.CurrentMessage.UserInputMessage
	if current.UserInputMessageContext == nil || len(current.UserInputMessageContext.ToolResults) != 2 {
		t.Fatalf("tool results were not merged: %+v", current.UserInputMessageContext)
	}
	if len(current.Images) != 2 {
		t.Errorf("images = %d, want both merged", len(current.Images))
	}
}

func TestRule5MergeKeepsContentWhenOneSideIsEmpty(t *testing.T) {
	req := mustBuild(t, buildInput([]Message{
		{Role: RoleUser, Content: ""},
		{Role: RoleUser, Content: "actual text"},
	}))
	current := req.ConversationState.CurrentMessage.UserInputMessage
	if current.Content != "actual text" {
		t.Errorf("content = %q, want no leading blank line", current.Content)
	}
}

func TestRule6ToolContentBecomesTextWhenNoToolsAreDeclared(t *testing.T) {
	in := buildInput([]Message{
		{Role: RoleUser, Content: "check the weather"},
		{Role: RoleAssistant, Content: "calling the tool", ToolCalls: []ToolCall{
			{ID: "call_1", Name: "get_weather", Arguments: `{"city":"Berlin"}`},
		}},
		{Role: RoleUser, ToolResults: []ToolResult{
			{ToolUseID: "call_1", Content: "18C and sunny"},
		}},
		{Role: RoleUser, Content: "thanks"},
	})
	in.Tools = nil

	req := mustBuild(t, in)
	body := marshalString(t, req)

	// No structural tool fields may survive.
	for _, forbidden := range []string{"toolUses", "toolResults", "tools"} {
		if strings.Contains(body, `"`+forbidden+`"`) {
			t.Errorf("%q must not appear when no tools are declared: %s", forbidden, body)
		}
	}
	// The information must survive as readable text. The arguments are checked in
	// their JSON-escaped form, because this assertion runs on the marshalled body.
	for _, want := range []string{
		"[Tool: get_weather (call_1)]",
		`{\"city\":\"Berlin\"}`,
		"[Tool Result (call_1)]",
		"18C and sunny",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in the converted text, got: %s", want, body)
		}
	}
}

func TestRule6PreservesImagesFromToolResults(t *testing.T) {
	in := buildInput([]Message{
		{Role: RoleUser, Content: "screenshot please"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "s1", Name: "screenshot", Arguments: "{}"}}},
		{Role: RoleUser, ToolResults: []ToolResult{{
			ToolUseID: "s1",
			Content:   "here it is",
			Images:    []Image{{MediaType: "image/png", Data: "SCREENSHOT"}},
		}}},
	})
	in.Tools = nil

	req := mustBuild(t, in)
	body := marshalString(t, req)
	if !strings.Contains(body, "SCREENSHOT") {
		t.Errorf("a screenshot returned by an MCP tool was lost: %s", body)
	}
	if !strings.Contains(body, `"images"`) {
		t.Error("the image should still be attached to the message")
	}
}

func TestRule6EmptyToolContentStillYieldsNonEmptyContent(t *testing.T) {
	in := buildInput([]Message{
		{Role: RoleUser, Content: "x"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "", Name: "", Arguments: ""}}},
	})
	in.Tools = nil
	req := mustBuild(t, in)

	for _, e := range req.ConversationState.History {
		if e.AssistantResponseMessage != nil && e.AssistantResponseMessage.Content == "" {
			t.Error("an assistant turn ended up with empty content")
		}
	}
}

func TestRule7OrphanedToolResultsAreInlined(t *testing.T) {
	req := mustBuild(t, BuildInput{
		ModelID: "m",
		Tools:   []Tool{{Name: "get_weather", Description: "d"}},
		Messages: []Message{
			// A truncated conversation: the assistant turn that made the call is gone.
			{Role: RoleUser, Content: "question"},
			{Role: RoleAssistant, Content: "some text but no tool calls"},
			{Role: RoleUser, ToolResults: []ToolResult{{ToolUseID: "missing-id", Content: "orphan result"}}},
		},
	})

	current := req.ConversationState.CurrentMessage.UserInputMessage
	if current.UserInputMessageContext != nil && len(current.UserInputMessageContext.ToolResults) > 0 {
		t.Error("an orphaned tool result must not be sent as a structural toolResult")
	}
	if !strings.Contains(current.Content, "orphan result") {
		t.Errorf("the orphaned result should be inlined as text, got %q", current.Content)
	}
	if !strings.Contains(current.Content, "[Tool Result (missing-id)]") {
		t.Errorf("expected a labelled inline result, got %q", current.Content)
	}
}

func TestRule7KeepsMatchedToolResults(t *testing.T) {
	req := mustBuild(t, BuildInput{
		ModelID: "m",
		Tools:   []Tool{{Name: "t", Description: "d"}},
		Messages: []Message{
			{Role: RoleUser, Content: "q"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "good", Name: "t", Arguments: "{}"}}},
			{Role: RoleUser, ToolResults: []ToolResult{{ToolUseID: "good", Content: "matched"}}},
		},
	})

	ctx := req.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if ctx == nil || len(ctx.ToolResults) != 1 {
		t.Fatalf("a matched tool result should be sent structurally, got %+v", ctx)
	}
	if ctx.ToolResults[0].ToolUseID != "good" {
		t.Errorf("toolUseId = %q", ctx.ToolResults[0].ToolUseID)
	}
	if ctx.ToolResults[0].Status != "success" {
		t.Errorf("status = %q, want success", ctx.ToolResults[0].Status)
	}
}

func TestRule7MixedMatchedAndOrphanedResults(t *testing.T) {
	req := mustBuild(t, BuildInput{
		ModelID: "m",
		Tools:   []Tool{{Name: "t", Description: "d"}},
		Messages: []Message{
			{Role: RoleUser, Content: "q"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "known", Name: "t", Arguments: "{}"}}},
			{Role: RoleUser, ToolResults: []ToolResult{
				{ToolUseID: "known", Content: "kept"},
				{ToolUseID: "unknown", Content: "inlined"},
			}},
		},
	})

	current := req.ConversationState.CurrentMessage.UserInputMessage
	ctx := current.UserInputMessageContext
	if ctx == nil || len(ctx.ToolResults) != 1 || ctx.ToolResults[0].ToolUseID != "known" {
		t.Errorf("only the matched result should stay structural, got %+v", ctx)
	}
	if !strings.Contains(current.Content, "inlined") {
		t.Errorf("the orphan should be inlined, got %q", current.Content)
	}
	if strings.Contains(current.Content, "kept") {
		t.Error("the matched result must not be duplicated into the text")
	}
}

func TestRule7ErrorStatusIsPreserved(t *testing.T) {
	req := mustBuild(t, BuildInput{
		ModelID: "m",
		Tools:   []Tool{{Name: "t", Description: "d"}},
		Messages: []Message{
			{Role: RoleUser, Content: "q"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "e", Name: "t", Arguments: "{}"}}},
			{Role: RoleUser, ToolResults: []ToolResult{{ToolUseID: "e", Content: "it failed", IsError: true}}},
		},
	})
	ctx := req.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if ctx.ToolResults[0].Status != "error" {
		t.Errorf("status = %q, want error", ctx.ToolResults[0].Status)
	}
}

func TestRule7EmptyToolResultContentGetsAPlaceholder(t *testing.T) {
	req := mustBuild(t, BuildInput{
		ModelID: "m",
		Tools:   []Tool{{Name: "t", Description: "d"}},
		Messages: []Message{
			{Role: RoleUser, Content: "q"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "x", Name: "t", Arguments: "{}"}}},
			{Role: RoleUser, ToolResults: []ToolResult{{ToolUseID: "x", Content: "   "}}},
		},
	})
	ctx := req.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if got := ctx.ToolResults[0].Content[0].Text; got != "(empty result)" {
		t.Errorf("empty tool result text = %q, want a placeholder", got)
	}
}

func TestRule8SchemaSanitizationInTheBuiltRequest(t *testing.T) {
	req := mustBuild(t, BuildInput{
		ModelID:  "m",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Tools: []Tool{{
			Name:        "t",
			Description: "d",
			InputSchema: map[string]any{
				"type":                 "object",
				"required":             []any{},
				"additionalProperties": false,
				"properties": map[string]any{
					"nested": map[string]any{
						"type":                 "object",
						"additionalProperties": true,
						"required":             []any{},
					},
				},
			},
		}},
	})

	body := marshalString(t, req)
	if strings.Contains(body, "additionalProperties") {
		t.Errorf("additionalProperties survived sanitisation: %s", body)
	}
	if strings.Contains(body, `"required":[]`) {
		t.Errorf("an empty required array survived sanitisation: %s", body)
	}
}

func TestRule9EmptyToolDescriptionGetsAPlaceholder(t *testing.T) {
	req := mustBuild(t, BuildInput{
		ModelID:  "m",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Tools: []Tool{
			{Name: "no_description"},
			{Name: "blank_description", Description: "   "},
			{Name: "has_description", Description: "real text"},
		},
	})

	specs := req.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools
	byName := map[string]string{}
	for _, s := range specs {
		byName[s.ToolSpecification.Name] = s.ToolSpecification.Description
	}
	if byName["no_description"] != "Tool: no_description" {
		t.Errorf("no_description got %q, want a placeholder", byName["no_description"])
	}
	if byName["blank_description"] != "Tool: blank_description" {
		t.Errorf("blank_description got %q, want a placeholder", byName["blank_description"])
	}
	if byName["has_description"] != "real text" {
		t.Errorf("has_description got %q, want it untouched", byName["has_description"])
	}
	for name, desc := range byName {
		if desc == "" {
			t.Errorf("tool %s ended up with an empty description", name)
		}
	}
}

func TestRule10LongToolNameIsRejected(t *testing.T) {
	long := strings.Repeat("a", 65)
	alsoLong := strings.Repeat("b", 100)

	_, err := Build(BuildInput{
		ModelID:  "m",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Tools: []Tool{
			{Name: "fine", Description: "d"},
			{Name: long, Description: "d"},
			{Name: alsoLong, Description: "d"},
		},
	})
	if err == nil {
		t.Fatal("a tool name over the limit must be rejected")
	}
	var be *BuildError
	if !asBuildError(err, &be) {
		t.Fatalf("error is %T, want *BuildError", err)
	}
	if be.TooLarge {
		t.Error("a name violation is not a size violation")
	}
	msg := err.Error()
	// It must name the offenders and their lengths.
	if !strings.Contains(msg, long) || !strings.Contains(msg, "65 characters") {
		t.Errorf("error should name the first offender and its length: %s", msg)
	}
	if !strings.Contains(msg, alsoLong) || !strings.Contains(msg, "100 characters") {
		t.Errorf("error should name every offender: %s", msg)
	}
	if strings.Contains(msg, "\"fine\"") {
		t.Error("a compliant tool must not be listed as an offender")
	}
	if !strings.Contains(msg, "64") {
		t.Error("the error should state the limit")
	}
}

func TestRule10AcceptsExactlyTheLimit(t *testing.T) {
	name := strings.Repeat("a", 64)
	if _, err := Build(BuildInput{
		ModelID:  "m",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Tools:    []Tool{{Name: name, Description: "d"}},
	}); err != nil {
		t.Errorf("a 64-character name is within the limit: %v", err)
	}
}

func TestRule11LongDescriptionMovesToTheSystemPrompt(t *testing.T) {
	longDoc := strings.TrimSpace(strings.Repeat("documentation. ", 200)) // well over the limit below
	req := mustBuild(t, BuildInput{
		ModelID:                  "m",
		Messages:                 []Message{{Role: RoleUser, Content: "hi"}},
		SystemPrompt:             "You are helpful.",
		ToolDescriptionMaxLength: 100,
		Tools: []Tool{
			{Name: "big_tool", Description: longDoc},
			{Name: "small_tool", Description: "short"},
		},
	})

	specs := req.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools
	byName := map[string]string{}
	for _, s := range specs {
		byName[s.ToolSpecification.Name] = s.ToolSpecification.Description
	}

	wantPointer := "[Full documentation in system prompt under '## Tool: big_tool']"
	if byName["big_tool"] != wantPointer {
		t.Errorf("big_tool description = %q, want %q", byName["big_tool"], wantPointer)
	}
	if byName["small_tool"] != "short" {
		t.Errorf("small_tool description = %q, want it untouched", byName["small_tool"])
	}

	effective := effectiveSystemPrompt(req)
	if !strings.Contains(effective, "## Tool: big_tool") {
		t.Error("the system prompt should carry the relocated documentation under its heading")
	}
	if !strings.Contains(effective, longDoc) {
		t.Error("the full documentation text should be in the system prompt")
	}
	if !strings.HasPrefix(effective, "You are helpful.") {
		t.Errorf("the original system prompt should come first, got %q", truncate(effective, 80))
	}
	if strings.Contains(effective, "## Tool: small_tool") {
		t.Error("a short description must not be relocated")
	}
}

func TestRule11CanBeDisabled(t *testing.T) {
	longDoc := strings.Repeat("x", 20000)
	req := mustBuild(t, BuildInput{
		ModelID:                  "m",
		Messages:                 []Message{{Role: RoleUser, Content: "hi"}},
		ToolDescriptionMaxLength: 0, // disabled
		Tools:                    []Tool{{Name: "t", Description: longDoc}},
	})
	specs := req.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools
	if specs[0].ToolSpecification.Description != longDoc {
		t.Error("with the limit disabled, the description should be sent inline")
	}
	if got := effectiveSystemPrompt(req); strings.Contains(got, "## Tool:") {
		t.Errorf("nothing should be relocated, got system prompt %q", truncate(got, 60))
	}
}

func TestRule11WithNoExistingSystemPrompt(t *testing.T) {
	req := mustBuild(t, BuildInput{
		ModelID:                  "m",
		Messages:                 []Message{{Role: RoleUser, Content: "hi"}},
		ToolDescriptionMaxLength: 10,
		Tools:                    []Tool{{Name: "t", Description: strings.Repeat("y", 50)}},
	})
	effective := effectiveSystemPrompt(req)
	if !strings.Contains(effective, "## Tool: t") {
		t.Errorf("system prompt = %q, want the relocated docs", truncate(effective, 80))
	}
	if strings.HasPrefix(effective, "\n") {
		t.Error("the system prompt should not start with a blank line")
	}
}

func TestRule12TrailingAssistantTurnMovesToHistory(t *testing.T) {
	req := mustBuild(t, buildInput([]Message{
		{Role: RoleUser, Content: "question"},
		{Role: RoleAssistant, Content: "partial answer"},
	}))

	current := req.ConversationState.CurrentMessage.UserInputMessage
	if current.Content != kiro.Placeholder {
		t.Errorf("current content = %q, want the placeholder", current.Content)
	}

	last := req.ConversationState.History[len(req.ConversationState.History)-1]
	if last.AssistantResponseMessage == nil {
		t.Fatal("the trailing assistant turn should be the last history entry")
	}
	if last.AssistantResponseMessage.Content != "partial answer" {
		t.Errorf("moved content = %q", last.AssistantResponseMessage.Content)
	}
}

func TestRule12PreservesToolCallsAndReasoningOnTheMovedTurn(t *testing.T) {
	req := mustBuild(t, BuildInput{
		ModelID: "claude-opus-5",
		Tools:   []Tool{{Name: "t", Description: "d"}},
		Messages: []Message{
			{Role: RoleUser, Content: "q"},
			{Role: RoleAssistant, Content: "thinking out loud",
				ToolCalls: []ToolCall{{ID: "tc", Name: "t", Arguments: `{"a":1}`}},
				Reasoning: &Reasoning{Text: "reasoned", Signature: "sig"}},
		},
	})

	last := req.ConversationState.History[len(req.ConversationState.History)-1].AssistantResponseMessage
	if last == nil {
		t.Fatal("expected the assistant turn in history")
	}
	if len(last.ToolUses) != 1 || last.ToolUses[0].ToolUseID != "tc" {
		t.Errorf("tool calls were lost when the turn moved: %+v", last.ToolUses)
	}
	if last.ReasoningContent == nil || last.ReasoningContent.ReasoningText == nil {
		t.Fatal("reasoning was lost when the turn moved")
	}
	if last.ReasoningContent.ReasoningText.Signature != "sig" {
		t.Errorf("signature = %q", last.ReasoningContent.ReasoningText.Signature)
	}
}

func TestRule12SingleAssistantMessage(t *testing.T) {
	req := mustBuild(t, buildInput([]Message{
		{Role: RoleAssistant, Content: "only me"},
	}))
	// Rule 3 prepends a user turn, then rule 12 moves the assistant turn into
	// history and leaves a placeholder current turn.
	if got := historyRoles(req); got != "ua" {
		t.Errorf("history roles = %q, want ua", got)
	}
	if req.ConversationState.CurrentMessage.UserInputMessage.Content != kiro.Placeholder {
		t.Error("the current turn should be a placeholder")
	}
	assertAlternating(t, req)
}

func TestNoMessagesIsAnError(t *testing.T) {
	_, err := Build(BuildInput{ModelID: "m"})
	if err == nil {
		t.Fatal("a request with no messages must be rejected")
	}
	if !strings.Contains(err.Error(), "no messages") {
		t.Errorf("error = %q", err)
	}
}

func TestPayloadSizeGuard(t *testing.T) {
	big := strings.Repeat("x", 5000)
	messages := []Message{{Role: RoleUser, Content: big}}

	t.Run("under the limit passes", func(t *testing.T) {
		in := buildInput(messages)
		in.MaxPayloadBytes = 100000
		if _, err := Build(in); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("over the limit is rejected", func(t *testing.T) {
		in := buildInput(messages)
		in.MaxPayloadBytes = 1000
		_, err := Build(in)
		if err == nil {
			t.Fatal("an oversized payload must be rejected")
		}
		var be *BuildError
		if !asBuildError(err, &be) {
			t.Fatalf("error is %T, want *BuildError", err)
		}
		if !be.TooLarge {
			t.Error("TooLarge should be set so the caller can map it to 413")
		}
		msg := err.Error()
		for _, want := range []string{"too large", "1000 byte limit", "new session", "fewer tools"} {
			if !strings.Contains(msg, want) {
				t.Errorf("error should contain %q: %s", want, msg)
			}
		}
	})

	t.Run("zero disables the check", func(t *testing.T) {
		in := buildInput(messages)
		in.MaxPayloadBytes = 0
		if _, err := Build(in); err != nil {
			t.Errorf("unexpected error with the check disabled: %v", err)
		}
	})

	t.Run("the error reports the tool count", func(t *testing.T) {
		in := buildInput(messages)
		in.MaxPayloadBytes = 1000
		in.Tools = []Tool{{Name: "a", Description: "d"}, {Name: "b", Description: "d"}}
		_, err := Build(in)
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "carries 2") {
			t.Errorf("error should report the tool count: %s", err)
		}
	})
}

func TestBuildDoesNotMutateItsInput(t *testing.T) {
	original := []Message{
		{Role: "developer", Content: "one",
			ToolCalls:   []ToolCall{{ID: "a", Name: "t", Arguments: "{}"}},
			ToolResults: []ToolResult{{ToolUseID: "a", Content: "r"}},
			Images:      []Image{{MediaType: "image/png", Data: "D"}}},
		{Role: RoleUser, Content: "two"},
	}
	snapshot := marshalString(t, original)

	if _, err := Build(BuildInput{ModelID: "m", Messages: original}); err != nil {
		t.Fatal(err)
	}
	if after := marshalString(t, original); after != snapshot {
		t.Errorf("Build mutated the caller's messages:\nbefore %s\nafter  %s", snapshot, after)
	}
}

func TestSystemPromptIsFoldedIntoTheFirstTurnByDefault(t *testing.T) {
	// The service schema declares a top-level systemPrompt field, but the deployed
	// backend answers 400 REQUEST_BODY_INVALID for any request carrying it, and
	// silently ignores a copy nested in conversationState. Folding it into the
	// first user turn is the only placement the live service honours.
	req := mustBuild(t, BuildInput{
		ModelID:      "m",
		SystemPrompt: "SYSTEM_MARKER_TEXT",
		Messages: []Message{
			{Role: RoleUser, Content: "first"},
			{Role: RoleAssistant, Content: "reply"},
			{Role: RoleUser, Content: "second"},
		},
	})

	if req.SystemPrompt != "" {
		t.Errorf("SystemPrompt = %q, want it empty: the backend rejects that field", req.SystemPrompt)
	}
	first := req.ConversationState.History[0].UserInputMessage
	if first == nil {
		t.Fatal("the first history entry should be a user turn")
	}
	if first.Content != "SYSTEM_MARKER_TEXT\n\nfirst" {
		t.Errorf("first turn content = %q, want the prompt prefixed", first.Content)
	}
	if strings.Count(marshalString(t, req), "SYSTEM_MARKER_TEXT") != 1 {
		t.Errorf("the system prompt appears more than once: %s", marshalString(t, req))
	}
}

func TestSystemPromptAsFieldWhenExplicitlyRequested(t *testing.T) {
	req := mustBuild(t, BuildInput{
		ModelID:             "m",
		SystemPrompt:        "SYSTEM_MARKER_TEXT",
		SystemPromptAsField: true,
		Messages: []Message{
			{Role: RoleUser, Content: "first"},
			{Role: RoleAssistant, Content: "reply"},
			{Role: RoleUser, Content: "second"},
		},
	})

	if req.SystemPrompt != "SYSTEM_MARKER_TEXT" {
		t.Errorf("SystemPrompt = %q, want the top-level field populated", req.SystemPrompt)
	}
	for i, e := range req.ConversationState.History {
		if e.UserInputMessage != nil && strings.Contains(e.UserInputMessage.Content, "SYSTEM_MARKER_TEXT") {
			t.Errorf("history entry %d also has the prompt folded in", i)
		}
	}
	if strings.Contains(req.ConversationState.CurrentMessage.UserInputMessage.Content, "SYSTEM_MARKER_TEXT") {
		t.Error("the current message also has the prompt folded in")
	}
}

func TestSystemPromptReplacesAPlaceholderFirstTurn(t *testing.T) {
	// A conversation opening with an assistant turn gains a synthetic placeholder
	// user turn. The prompt should replace that placeholder rather than sit above
	// the words "(no content)".
	req := mustBuild(t, BuildInput{
		ModelID:      "m",
		SystemPrompt: "INSTRUCTIONS",
		Messages: []Message{
			{Role: RoleAssistant, Content: "I spoke first"},
			{Role: RoleUser, Content: "now me"},
		},
	})
	first := req.ConversationState.History[0].UserInputMessage
	if first.Content != "INSTRUCTIONS" {
		t.Errorf("first turn content = %q, want just the instructions", first.Content)
	}
	if strings.Contains(marshalString(t, req), kiro.Placeholder+`\n\n`) {
		t.Error("the prompt was stacked on top of a placeholder")
	}
}

func TestNoSystemPromptLeavesMessagesUntouched(t *testing.T) {
	req := mustBuild(t, buildInput([]Message{
		{Role: RoleUser, Content: "only my words"},
	}))
	if req.SystemPrompt != "" {
		t.Errorf("SystemPrompt = %q, want empty", req.SystemPrompt)
	}
	if got := req.ConversationState.CurrentMessage.UserInputMessage.Content; got != "only my words" {
		t.Errorf("content = %q, want it unchanged", got)
	}
}

// effectiveSystemPrompt returns wherever the system prompt ended up: the
// top-level field when that mode is on, otherwise the first turn's content.
func effectiveSystemPrompt(req *kiro.Request) string {
	if req.SystemPrompt != "" {
		return req.SystemPrompt
	}
	if len(req.ConversationState.History) > 0 {
		if first := req.ConversationState.History[0].UserInputMessage; first != nil {
			return first.Content
		}
	}
	if current := req.ConversationState.CurrentMessage.UserInputMessage; current != nil {
		return current.Content
	}
	return ""
}

func TestToolsAreDeclaredOnlyOnTheCurrentTurn(t *testing.T) {
	req := mustBuild(t, BuildInput{
		ModelID: "m",
		Tools:   []Tool{{Name: "t", Description: "d"}},
		Messages: []Message{
			{Role: RoleUser, Content: "a"},
			{Role: RoleAssistant, Content: "b"},
			{Role: RoleUser, Content: "c"},
		},
	})

	if req.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext == nil {
		t.Fatal("tools should be declared on the current turn")
	}
	for i, e := range req.ConversationState.History {
		if e.UserInputMessage != nil && e.UserInputMessage.UserInputMessageContext != nil &&
			len(e.UserInputMessage.UserInputMessageContext.Tools) > 0 {
			t.Errorf("history entry %d also declares tools", i)
		}
	}
}

func TestEmptyContextObjectIsOmitted(t *testing.T) {
	req := mustBuild(t, buildInput([]Message{{Role: RoleUser, Content: "hi"}}))
	if req.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext != nil {
		t.Error("an empty userInputMessageContext must be omitted entirely")
	}
	if strings.Contains(marshalString(t, req), "userInputMessageContext") {
		t.Error("userInputMessageContext appeared in the JSON despite being empty")
	}
}

func TestModelIDAndOriginOnEveryUserTurn(t *testing.T) {
	req := mustBuild(t, buildInput([]Message{
		{Role: RoleUser, Content: "a"},
		{Role: RoleAssistant, Content: "b"},
		{Role: RoleUser, Content: "c"},
	}))

	check := func(m *kiro.UserInputMessage, where string) {
		if m.ModelID != "claude-opus-5" {
			t.Errorf("%s modelId = %q", where, m.ModelID)
		}
		if m.Origin != kiro.Origin {
			t.Errorf("%s origin = %q, want %q", where, m.Origin, kiro.Origin)
		}
	}
	check(req.ConversationState.CurrentMessage.UserInputMessage, "current")
	for i, e := range req.ConversationState.History {
		if e.UserInputMessage != nil {
			check(e.UserInputMessage, "history["+string(rune('0'+i))+"]")
		}
	}
}

func TestImagesAreAttachedToTheMessageNotTheContext(t *testing.T) {
	req := mustBuild(t, buildInput([]Message{{
		Role:    RoleUser,
		Content: "look",
		Images: []Image{
			{MediaType: "image/png", Data: "PNGDATA"},
			{MediaType: "image/jpeg", Data: "JPGDATA"},
			{MediaType: "image/webp", Data: ""}, // skipped: no data
		},
	}}))

	current := req.ConversationState.CurrentMessage.UserInputMessage
	if len(current.Images) != 2 {
		t.Fatalf("images = %d, want 2 with the empty one skipped", len(current.Images))
	}
	if current.Images[0].Format != "png" || current.Images[1].Format != "jpeg" {
		t.Errorf("formats = %q, %q; want the bare subtypes", current.Images[0].Format, current.Images[1].Format)
	}
	if current.Images[0].Source.Bytes != "PNGDATA" {
		t.Errorf("image bytes = %q", current.Images[0].Source.Bytes)
	}
	if current.UserInputMessageContext != nil {
		t.Error("images must not create a userInputMessageContext")
	}
}

func TestReasoningRoundTripAndDropRules(t *testing.T) {
	cases := []struct {
		name        string
		reasoning   *Reasoning
		modelID     string
		wantPresent bool
		wantKind    string // "text" or "redacted"
	}{
		{"signed text is kept", &Reasoning{Text: "t", Signature: "s"}, "m", true, "text"},
		{"unsigned text is dropped", &Reasoning{Text: "t"}, "m", false, ""},
		{"signature with no text is dropped", &Reasoning{Signature: "s"}, "m", false, ""},
		{"redacted blob is kept", &Reasoning{RedactedContent: []byte{1, 2, 3}}, "m", true, "redacted"},
		{"redacted wins over unsigned text", &Reasoning{Text: "t", RedactedContent: []byte{1}}, "m", true, "redacted"},
		{"nil reasoning", nil, "m", false, ""},
		{"model mismatch is dropped", &Reasoning{Text: "t", Signature: "s", ModelID: "other"}, "m", false, ""},
		{"model match is kept", &Reasoning{Text: "t", Signature: "s", ModelID: "m"}, "m", true, "text"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := mustBuild(t, BuildInput{
				ModelID: tc.modelID,
				Messages: []Message{
					{Role: RoleUser, Content: "q"},
					{Role: RoleAssistant, Content: "a", Reasoning: tc.reasoning},
					{Role: RoleUser, Content: "next"},
				},
			})

			var found *kiro.ReasoningContent
			for _, e := range req.ConversationState.History {
				if e.AssistantResponseMessage != nil && e.AssistantResponseMessage.ReasoningContent != nil {
					found = e.AssistantResponseMessage.ReasoningContent
				}
			}

			if !tc.wantPresent {
				if found != nil {
					t.Errorf("reasoning should have been dropped, got %+v", found)
				}
				return
			}
			if found == nil {
				t.Fatal("reasoning should have been kept")
			}
			switch tc.wantKind {
			case "text":
				if found.ReasoningText == nil {
					t.Error("want a reasoningText block")
				}
				if found.RedactedContent != "" {
					t.Error("a signed block must not also carry redactedContent")
				}
			case "redacted":
				if found.RedactedContent == "" {
					t.Error("want a redactedContent block")
				}
				if found.ReasoningText != nil {
					t.Error("a redacted block must not also carry reasoningText")
				}
			}
		})
	}
}

func TestReasoningUsesTheUnionWrapperInJSON(t *testing.T) {
	req := mustBuild(t, BuildInput{
		ModelID: "m",
		Messages: []Message{
			{Role: RoleUser, Content: "q"},
			{Role: RoleAssistant, Content: "a", Reasoning: &Reasoning{Text: "thought", Signature: "sig"}},
			{Role: RoleUser, Content: "next"},
		},
	})
	body := marshalString(t, req)
	if !strings.Contains(body, `"reasoningContent":{"reasoningText":{"text":"thought","signature":"sig"}}`) {
		t.Errorf("reasoning is not in the union wrapper shape: %s", body)
	}
}

func TestCombinedInvariantsOnARealisticConversation(t *testing.T) {
	// Every rule at once: an assistant-first history, unknown roles, consecutive
	// turns, orphaned and matched tool results, images, reasoning, a long tool
	// description and an empty schema.
	longDoc := strings.Repeat("detail ", 500)
	req := mustBuild(t, BuildInput{
		ModelID:                  "claude-opus-5",
		ConversationID:           "conv-combined",
		ProfileARN:               "arn:aws:codewhisperer:us-east-1:1:profile/A",
		SystemPrompt:             "Be brief.",
		ToolDescriptionMaxLength: 200,
		MaxPayloadBytes:          600000,
		Tools: []Tool{
			{Name: "get_weather", Description: longDoc, InputSchema: map[string]any{
				"type": "object", "required": []any{}, "additionalProperties": false,
				"properties": map[string]any{"city": map[string]any{"type": "string"}},
			}},
			{Name: "no_desc"},
		},
		Messages: []Message{
			{Role: RoleAssistant, Content: "I start, which is not allowed"},
			{Role: "developer", Content: "extra context"},
			{Role: RoleUser, Content: "weather in Berlin?", Images: []Image{{MediaType: "image/png", Data: "IMG"}}},
			{Role: RoleUser, Content: "and Paris?"},
			{Role: RoleAssistant, Content: "checking",
				ToolCalls: []ToolCall{{ID: "call_1", Name: "get_weather", Arguments: `{"city":"Berlin"}`}},
				Reasoning: &Reasoning{Text: "call the tool", Signature: "sig-1"}},
			{Role: RoleUser, ToolResults: []ToolResult{
				{ToolUseID: "call_1", Content: "18C"},
				{ToolUseID: "call_missing", Content: "orphan"},
			}},
			{Role: RoleUser, Content: "thanks"},
		},
	})

	assertAlternating(t, req)

	// Structure.
	if req.ConversationState.ChatTriggerType != "MANUAL" {
		t.Errorf("chatTriggerType = %q", req.ConversationState.ChatTriggerType)
	}
	if req.ConversationState.ConversationID != "conv-combined" {
		t.Errorf("conversationId = %q", req.ConversationState.ConversationID)
	}
	if req.ProfileARN == "" {
		t.Error("profileArn should be set")
	}
	if req.ConversationState.History[0].UserInputMessage == nil {
		t.Error("history must start with a user turn")
	}

	body := marshalString(t, req)

	// Nothing was silently lost.
	for _, want := range []string{
		"extra context", "weather in Berlin?", "and Paris?", "checking",
		"18C", "orphan", "thanks", "IMG", "sig-1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("content %q was lost", want)
		}
	}
	// Sanitisation held.
	if strings.Contains(body, "additionalProperties") || strings.Contains(body, `"required":[]`) {
		t.Error("schema sanitisation did not hold")
	}
	// The long description was relocated.
	effective := effectiveSystemPrompt(req)
	if !strings.Contains(effective, "## Tool: get_weather") {
		t.Error("the long tool description was not relocated")
	}
	if !strings.HasPrefix(effective, "Be brief.") {
		t.Error("the original system prompt should come first")
	}
	// The description-less tool got a placeholder.
	if !strings.Contains(body, "Tool: no_desc") {
		t.Error("the description-less tool did not get a placeholder")
	}
	// The matched result stayed structural, the orphan was inlined.
	if !strings.Contains(body, `"toolUseId":"call_1"`) {
		t.Error("the matched tool result should be structural")
	}
	if strings.Contains(body, `"toolUseId":"call_missing"`) {
		t.Error("the orphaned tool result must not be sent structurally")
	}
}

// marshalString renders any value as compact JSON for assertions.
func marshalString(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(data)
}

// asBuildError unwraps a *BuildError.
func asBuildError(err error, target **BuildError) bool {
	be, ok := err.(*BuildError)
	if ok {
		*target = be
	}
	return ok
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.

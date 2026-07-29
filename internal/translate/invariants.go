package translate

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/jasminnanda/kirogo/internal/kiro"
)

// MaxToolNameLength is the backend's hard limit on a tool name.
const MaxToolNameLength = 64

// toolDocumentationHeading introduces relocated tool documentation in the system
// prompt. The reference in the tool spec points at this exact heading, so the
// model can find the full text.
const toolDocumentationHeading = "## Tool: "

// BuildInput is everything needed to construct a Kiro request.
type BuildInput struct {
	// Messages are the conversation turns, system messages already extracted.
	Messages []Message
	// SystemPrompt is the combined system text. It becomes the top-level
	// systemPrompt field, not part of any message.
	SystemPrompt string
	// Tools are the declared tools, possibly empty.
	Tools []Tool
	// ModelID is the resolved model id to send.
	ModelID string
	// ConversationID identifies the conversation upstream.
	ConversationID string
	// ProfileARN is the CodeWhisperer profile ARN, omitted when empty.
	ProfileARN string
	// AdditionalModelRequestFields carries the reasoning effort document.
	AdditionalModelRequestFields map[string]any
	// AgentMode populates the agent mode header.
	AgentMode string
	// ToolDescriptionMaxLength is the length above which a tool description moves
	// into the system prompt. Zero disables the relocation.
	ToolDescriptionMaxLength int
	// MaxPayloadBytes rejects an oversized request. Zero disables the check.
	MaxPayloadBytes int
	// SystemPromptAsField sends the system prompt in the top-level systemPrompt
	// field instead of folding it into the first user turn.
	//
	// The service schema declares that field, but the deployed backend currently
	// answers 400 REQUEST_BODY_INVALID for any request carrying it, and silently
	// ignores a copy nested inside conversationState. Folding the prompt into the
	// first user turn is the only placement the live service actually honours, so
	// that is the default. This switch exists for the day the deployment catches
	// up with its own schema.
	SystemPromptAsField bool
}

// BuildError is a request that cannot be sent, with an explanation for the user.
type BuildError struct {
	// Message is the user-facing explanation.
	Message string
	// TooLarge marks a payload size failure, which maps to a different status.
	TooLarge bool
}

// Error renders the message.
func (e *BuildError) Error() string { return e.Message }

// Build converts the intermediate representation into a Kiro request.
//
// The transformations below are not cosmetic. The Kiro backend answers a single
// uninformative "Improperly formed request." for every structural violation, so
// each rule here is the difference between a working request and an error with no
// diagnostic value. They are applied in a fixed order because several depend on
// the output of an earlier one.
func Build(in BuildInput) (*kiro.Request, error) {
	// Rule 10: a tool name over the limit is rejected outright, naming the
	// offenders. There is nothing kirogo can safely rename.
	if err := validateToolNames(in.Tools); err != nil {
		return nil, err
	}

	// Rule 11: an over-long tool description moves into the system prompt, with a
	// pointer left behind. Claude Code and Cline ship very long tool docs, and the
	// backend rejects them inline.
	tools, relocatedDocs := relocateLongToolDescriptions(in.Tools, in.ToolDescriptionMaxLength)
	systemPrompt := in.SystemPrompt
	if relocatedDocs != "" {
		systemPrompt = appendSystemText(systemPrompt, relocatedDocs)
	}

	messages := cloneMessages(in.Messages)

	// Rule 5: adjacent same-role turns merge. The backend does not accept two
	// consecutive turns from one side.
	//
	// This runs before the tool rules below, unlike the reference gateway, which
	// merges afterwards. With parallel tool calls answered across two consecutive
	// user turns, checking orphans first sees only the first turn as adjacent to
	// the assistant and wrongly inlines the second turn's results. Merging first
	// puts every result on one turn, immediately after the assistant that made
	// the calls, so they are all recognised.
	messages = mergeAdjacent(messages)

	if len(tools) == 0 {
		// Rule 6: with no tools declared, tool content is converted to readable
		// text rather than dropped. Cline and Roo send tool history without
		// redeclaring the tools, and the backend rejects toolResults that have no
		// matching tools.
		messages = toolContentToText(messages)
	} else {
		// Rule 7: a toolResult needs a preceding assistant turn whose toolUses
		// contain its id. Orphans are inlined as text, because the original tool
		// name and arguments are unknowable and the backend validates them.
		messages = inlineOrphanedToolResults(messages)
	}

	// Rule 3: history has to start with a user turn.
	messages = ensureFirstIsUser(messages)

	// Rule 4: any other role becomes user. This must precede the alternation fix
	// so that turns which only become adjacent after normalisation are seen.
	messages = normalizeRoles(messages)

	// Rule 2: user and assistant must alternate, with a placeholder inserted
	// between consecutive user turns.
	messages = ensureAlternating(messages)

	if len(messages) == 0 {
		return nil, &BuildError{Message: "This request contains no messages to send. Include at least one message."}
	}

	// Place the system prompt. Folding it into the first turn is the default
	// because the deployed backend rejects the dedicated field; see
	// SystemPromptAsField. The rules above guarantee the first turn is a user
	// turn, so this placement is always valid.
	topLevelSystemPrompt := ""
	if systemPrompt != "" {
		if in.SystemPromptAsField {
			topLevelSystemPrompt = systemPrompt
		} else {
			messages[0].Content = joinSystemPromptInto(systemPrompt, messages[0].Content)
		}
	}

	// Split history from the turn being sent now.
	history := messages[:len(messages)-1]
	current := messages[len(messages)-1]

	entries := make([]kiro.HistoryEntry, 0, len(history)+1)
	for i := range history {
		entries = append(entries, historyEntry(&history[i], in.ModelID))
	}

	// Rule 12: a trailing assistant turn moves into history and the current turn
	// becomes a placeholder user turn, because a request has to end with the user.
	if current.Role == RoleAssistant {
		entries = append(entries, historyEntry(&current, in.ModelID))
		current = Message{Role: RoleUser, Content: kiro.Placeholder}
	}

	userMessage := userInputMessage(&current, in.ModelID)

	// Tools are declared on the current turn only.
	if len(tools) > 0 {
		if userMessage.UserInputMessageContext == nil {
			userMessage.UserInputMessageContext = &kiro.UserInputMessageContext{}
		}
		userMessage.UserInputMessageContext.Tools = buildToolSpecs(tools)
	}
	if userMessage.UserInputMessageContext.IsEmpty() {
		userMessage.UserInputMessageContext = nil
	}

	req := &kiro.Request{
		ConversationState: kiro.ConversationState{
			ChatTriggerType: kiro.ChatTriggerType,
			ConversationID:  in.ConversationID,
			CurrentMessage:  kiro.CurrentMessage{UserInputMessage: userMessage},
			History:         entries,
		},
		ProfileARN:                   in.ProfileARN,
		SystemPrompt:                 topLevelSystemPrompt,
		AdditionalModelRequestFields: in.AdditionalModelRequestFields,
		AgentMode:                    in.AgentMode,
	}

	// Section 7.4: refuse an oversized payload rather than silently deleting
	// history. Which messages matter is the client's decision, and quietly
	// dropping them produces baffling model behaviour.
	if in.MaxPayloadBytes > 0 {
		size, err := req.SizeBytes()
		if err != nil {
			return nil, err
		}
		if size > in.MaxPayloadBytes {
			return nil, &BuildError{
				TooLarge: true,
				Message: fmt.Sprintf(
					"This conversation is too large to send: %d bytes against a %d byte limit. "+
						"kirogo does not trim history automatically, because choosing what to drop is the client's call. "+
						"Start a new session, remove older messages, or declare fewer tools. "+
						"Tool declarations are often the bulk of it: this request carries %d.",
					size, in.MaxPayloadBytes, len(tools)),
			}
		}
	}

	return req, nil
}

// cloneMessages copies the slice and its nested slices so the transformations
// never mutate the caller's data.
func cloneMessages(in []Message) []Message {
	out := make([]Message, len(in))
	for i, m := range in {
		out[i] = m
		if len(m.ToolCalls) > 0 {
			out[i].ToolCalls = append([]ToolCall(nil), m.ToolCalls...)
		}
		if len(m.ToolResults) > 0 {
			out[i].ToolResults = append([]ToolResult(nil), m.ToolResults...)
		}
		if len(m.Images) > 0 {
			out[i].Images = append([]Image(nil), m.Images...)
		}
	}
	return out
}

// validateToolNames enforces rule 10.
func validateToolNames(tools []Tool) error {
	var offenders []string
	for _, t := range tools {
		if len(t.Name) > MaxToolNameLength {
			offenders = append(offenders, fmt.Sprintf("  - %q is %d characters", t.Name, len(t.Name)))
		}
	}
	if len(offenders) == 0 {
		return nil
	}
	return &BuildError{Message: fmt.Sprintf(
		"The Kiro backend limits tool names to %d characters, and these are longer:\n%s\n\n"+
			"Rename them in the client that declared them. kirogo cannot shorten them itself, "+
			"because the model calls tools by the exact name it was given.",
		MaxToolNameLength, strings.Join(offenders, "\n"))}
}

// relocateLongToolDescriptions implements rule 11.
//
// The tool spec keeps a pointer to the system prompt section holding the full
// text, so the model knows where to look rather than losing the documentation.
func relocateLongToolDescriptions(tools []Tool, maxLength int) ([]Tool, string) {
	if len(tools) == 0 || maxLength <= 0 {
		return tools, ""
	}

	out := make([]Tool, 0, len(tools))
	var sections []string
	for _, t := range tools {
		if len(t.Description) <= maxLength {
			out = append(out, t)
			continue
		}
		slog.Debug("tool description moved into the system prompt",
			"tool", t.Name, "length", len(t.Description), "limit", maxLength)
		sections = append(sections, toolDocumentationHeading+t.Name+"\n\n"+t.Description)
		relocated := t
		relocated.Description = "[Full documentation in system prompt under '" +
			toolDocumentationHeading + t.Name + "']"
		out = append(out, relocated)
	}

	if len(sections) == 0 {
		return out, ""
	}
	doc := "---\n# Tool Documentation\n" +
		"These tools have documentation too long for their declaration.\n\n" +
		strings.Join(sections, "\n\n---\n\n")
	return out, doc
}

// buildToolSpecs converts tools to the Kiro shape, applying rules 8 and 9.
func buildToolSpecs(tools []Tool) []kiro.Tool {
	out := make([]kiro.Tool, 0, len(tools))
	for _, t := range tools {
		description := strings.TrimSpace(t.Description)
		if description == "" {
			// Rule 9: the backend rejects a blank description.
			description = "Tool: " + t.Name
			slog.Debug("tool has no description, using a placeholder", "tool", t.Name)
		}
		out = append(out, kiro.Tool{ToolSpecification: kiro.ToolSpecification{
			Name:        t.Name,
			Description: description,
			// Rule 8: strip the schema constructs the backend rejects.
			InputSchema: kiro.ToolInputSchema{JSON: SanitizeJSONSchema(t.InputSchema)},
		}})
	}
	return out
}

// toolCallToText renders a tool call for rule 6.
func toolCallToText(tc ToolCall) string {
	args := strings.TrimSpace(tc.Arguments)
	if args == "" {
		args = "{}"
	}
	if tc.ID != "" {
		return "[Tool: " + tc.Name + " (" + tc.ID + ")]\n" + args
	}
	return "[Tool: " + tc.Name + "]\n" + args
}

// toolResultToText renders a tool result for rules 6 and 7.
func toolResultToText(tr ToolResult) string {
	content := tr.Content
	if strings.TrimSpace(content) == "" {
		content = "(empty result)"
	}
	label := "[Tool Result"
	if tr.ToolUseID != "" {
		label += " (" + tr.ToolUseID + ")"
	}
	if tr.IsError {
		label += " [error]"
	}
	label += "]"
	return label + "\n" + content
}

// toolContentToText implements rule 6.
//
// Images attached to a tool result are preserved on the message, because MCP
// tools return screenshots and losing them would silently degrade the request.
func toolContentToText(messages []Message) []Message {
	out := make([]Message, 0, len(messages))
	converted := 0

	for _, m := range messages {
		if !m.HasToolContent() {
			out = append(out, m)
			continue
		}

		var parts []string
		if m.Content != "" {
			parts = append(parts, m.Content)
		}
		for _, tc := range m.ToolCalls {
			parts = append(parts, toolCallToText(tc))
			converted++
		}
		for _, tr := range m.ToolResults {
			parts = append(parts, toolResultToText(tr))
			converted++
			m.Images = append(m.Images, tr.Images...)
		}

		m.Content = strings.Join(parts, "\n\n")
		if m.Content == "" {
			m.Content = kiro.Placeholder
		}
		m.ToolCalls = nil
		m.ToolResults = nil
		out = append(out, m)
	}

	if converted > 0 {
		slog.Debug("converted tool content to text because no tools were declared",
			"items", converted)
	}
	return out
}

// inlineOrphanedToolResults implements rule 7.
func inlineOrphanedToolResults(messages []Message) []Message {
	out := make([]Message, 0, len(messages))

	for _, m := range messages {
		if len(m.ToolResults) == 0 {
			out = append(out, m)
			continue
		}

		// Collect the ids the previous assistant turn actually called.
		valid := map[string]bool{}
		if len(out) > 0 {
			prev := out[len(out)-1]
			if prev.Role == RoleAssistant {
				for _, tc := range prev.ToolCalls {
					if tc.ID != "" {
						valid[tc.ID] = true
					}
				}
			}
		}

		var kept []ToolResult
		var inlined []string
		for _, tr := range m.ToolResults {
			if tr.ToolUseID != "" && valid[tr.ToolUseID] {
				kept = append(kept, tr)
				continue
			}
			inlined = append(inlined, toolResultToText(tr))
			m.Images = append(m.Images, tr.Images...)
		}

		if len(inlined) > 0 {
			slog.Debug("inlined tool results with no matching tool call",
				"count", len(inlined))
			extra := strings.Join(inlined, "\n\n")
			if m.Content == "" {
				m.Content = extra
			} else {
				m.Content = m.Content + "\n\n" + extra
			}
		}
		m.ToolResults = kept
		out = append(out, m)
	}
	return out
}

// mergeAdjacent implements rule 5.
func mergeAdjacent(messages []Message) []Message {
	if len(messages) == 0 {
		return messages
	}
	out := make([]Message, 0, len(messages))
	merged := 0

	for _, m := range messages {
		if len(out) == 0 || out[len(out)-1].Role != m.Role {
			out = append(out, m)
			continue
		}
		last := &out[len(out)-1]
		switch {
		case last.Content == "":
			last.Content = m.Content
		case m.Content != "":
			last.Content = last.Content + "\n" + m.Content
		}
		last.ToolCalls = append(last.ToolCalls, m.ToolCalls...)
		last.ToolResults = append(last.ToolResults, m.ToolResults...)
		last.Images = append(last.Images, m.Images...)
		// Keep the later reasoning: it belongs to the most recent turn.
		if m.Reasoning != nil {
			last.Reasoning = m.Reasoning
		}
		merged++
	}

	if merged > 0 {
		slog.Debug("merged adjacent same-role messages", "merged", merged)
	}
	return out
}

// ensureFirstIsUser implements rule 3.
func ensureFirstIsUser(messages []Message) []Message {
	if len(messages) == 0 || messages[0].Role == RoleUser {
		return messages
	}
	slog.Debug("prepended a placeholder user turn: history must start with the user",
		"first_role", messages[0].Role)
	return append([]Message{{Role: RoleUser, Content: kiro.Placeholder}}, messages...)
}

// normalizeRoles implements rule 4.
func normalizeRoles(messages []Message) []Message {
	converted := 0
	for i := range messages {
		if messages[i].Role != RoleUser && messages[i].Role != RoleAssistant {
			slog.Debug("normalised an unsupported role to user", "role", messages[i].Role)
			messages[i].Role = RoleUser
			converted++
		}
	}
	if converted > 0 {
		slog.Debug("normalised roles to user", "count", converted)
	}
	return messages
}

// ensureAlternating implements rule 2.
func ensureAlternating(messages []Message) []Message {
	if len(messages) < 2 {
		return messages
	}
	out := make([]Message, 0, len(messages)*2)
	out = append(out, messages[0])
	inserted := 0

	for _, m := range messages[1:] {
		if m.Role == RoleUser && out[len(out)-1].Role == RoleUser {
			out = append(out, Message{Role: RoleAssistant, Content: kiro.Placeholder})
			inserted++
		}
		out = append(out, m)
	}

	if inserted > 0 {
		slog.Debug("inserted placeholder assistant turns to keep roles alternating",
			"inserted", inserted)
	}
	return out
}

// historyEntry converts one IR message into a history entry.
func historyEntry(m *Message, modelID string) kiro.HistoryEntry {
	if m.Role == RoleAssistant {
		return kiro.HistoryEntry{AssistantResponseMessage: assistantMessage(m, modelID)}
	}
	return kiro.HistoryEntry{UserInputMessage: userInputMessage(m, modelID)}
}

// assistantMessage converts an assistant turn.
func assistantMessage(m *Message, modelID string) *kiro.AssistantResponseMessage {
	out := &kiro.AssistantResponseMessage{Content: nonEmptyContent(m.Content)}

	for _, tc := range m.ToolCalls {
		out.ToolUses = append(out.ToolUses, kiro.ToolUse{
			Name:      tc.Name,
			Input:     tc.ArgumentsObject(),
			ToolUseID: tc.ID,
		})
	}

	out.ReasoningContent = reasoningContent(m.Reasoning, modelID)
	return out
}

// reasoningContent converts reasoning for a history turn.
//
// Unsigned reasoning is dropped: the backend validates the signature and answers
// THINKING_SIGNATURE_INVALID for anything it cannot verify. Reasoning produced by
// a different model is dropped for the same reason, since a signature is only
// valid for the model that issued it.
func reasoningContent(r *Reasoning, modelID string) *kiro.ReasoningContent {
	if r == nil {
		return nil
	}
	if r.ModelID != "" && modelID != "" && r.ModelID != modelID {
		slog.Debug("reasoning dropped: it was produced by a different model",
			"reasoning_model", r.ModelID, "request_model", modelID)
		return nil
	}
	if len(r.RedactedContent) > 0 {
		return kiro.NewRedactedReasoning(r.RedactedContent)
	}
	if r.Text != "" && r.Signature == "" {
		slog.Debug("reasoning dropped: no signature to verify it with")
		return nil
	}
	return kiro.NewReasoningText(r.Text, r.Signature)
}

// userInputMessage converts a user turn.
func userInputMessage(m *Message, modelID string) *kiro.UserInputMessage {
	out := &kiro.UserInputMessage{
		// Rule 1: content is never empty.
		Content: nonEmptyContent(m.Content),
		ModelID: modelID,
		Origin:  kiro.Origin,
	}

	// Images go on the message itself, not inside the context object.
	for _, img := range m.Images {
		if img.Data == "" {
			continue
		}
		out.Images = append(out.Images, kiro.ImageBlock{
			Format: img.Format(),
			Source: kiro.ImageSource{Bytes: img.Data},
		})
	}

	if len(m.ToolResults) > 0 {
		ctx := &kiro.UserInputMessageContext{}
		for _, tr := range m.ToolResults {
			content := tr.Content
			if strings.TrimSpace(content) == "" {
				content = "(empty result)"
			}
			ctx.ToolResults = append(ctx.ToolResults, kiro.ToolResult{
				Content:   []kiro.ToolResultContent{{Text: content}},
				Status:    tr.Status(),
				ToolUseID: tr.ToolUseID,
			})
			// A tool that returned an image contributes it to the turn.
			for _, img := range tr.Images {
				if img.Data == "" {
					continue
				}
				out.Images = append(out.Images, kiro.ImageBlock{
					Format: img.Format(),
					Source: kiro.ImageSource{Bytes: img.Data},
				})
			}
		}
		out.UserInputMessageContext = ctx
	}

	return out
}

// nonEmptyContent implements rule 1.
func nonEmptyContent(s string) string {
	if strings.TrimSpace(s) == "" {
		return kiro.Placeholder
	}
	return s
}

// joinSystemPromptInto prefixes a turn's content with the system prompt.
//
// A placeholder is replaced rather than kept, so a synthesised turn does not end
// up reading "(no content)" after the real instructions.
func joinSystemPromptInto(systemPrompt, content string) string {
	if strings.TrimSpace(content) == "" || content == kiro.Placeholder {
		return systemPrompt
	}
	return systemPrompt + "\n\n" + content
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.

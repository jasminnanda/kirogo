package translate

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// OpenAIRequest is the /v1/chat/completions request body.
//
// Unknown fields are accepted and ignored: IDEs send a great deal that has no
// Kiro equivalent, and rejecting a request over an unrecognised field would break
// clients for no benefit.
type OpenAIRequest struct {
	Model    string          `json:"model"`
	Messages []OpenAIMessage `json:"messages"`
	Stream   bool            `json:"stream"`

	Temperature      *float64             `json:"temperature"`
	TopP             *float64             `json:"top_p"`
	N                *int                 `json:"n"`
	MaxTokens        *int                 `json:"max_tokens"`
	MaxCompletionTok *int                 `json:"max_completion_tokens"`
	Stop             any                  `json:"stop"`
	PresencePenalty  *float64             `json:"presence_penalty"`
	FrequencyPenalty *float64             `json:"frequency_penalty"`
	ReasoningEffort  string               `json:"reasoning_effort"`
	Tools            []OpenAITool         `json:"tools"`
	ToolChoice       any                  `json:"tool_choice"`
	StreamOptions    *OpenAIStreamOptions `json:"stream_options"`
	User             string               `json:"user"`
	Seed             *int                 `json:"seed"`
	ParallelToolCall *bool                `json:"parallel_tool_calls"`
}

// OpenAIStreamOptions controls streaming extras.
type OpenAIStreamOptions struct {
	// IncludeUsage asks for a usage block on the final chunk. kirogo always sends
	// usage, so this only records the client's preference.
	IncludeUsage bool `json:"include_usage"`
}

// OpenAIMessage is one message in the request.
type OpenAIMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
	Name    string `json:"name"`

	ToolCalls  []OpenAIToolCall `json:"tool_calls"`
	ToolCallID string           `json:"tool_call_id"`

	// ReasoningContent carries reasoning back on an assistant turn. It is the
	// field kirogo emits, so a client that echoes the whole message round-trips.
	ReasoningContent string `json:"reasoning_content"`
	// ReasoningSignature is kirogo's own addition. The backend requires a
	// signature to accept reasoning, and OpenAI has no field for one, so a client
	// that wants reasoning to survive a turn must echo this back.
	ReasoningSignature string `json:"reasoning_signature"`
	// ReasoningRedacted is the base64 opaque reasoning blob, echoed back the
	// same way.
	ReasoningRedacted string `json:"reasoning_redacted_content"`
}

// OpenAIToolCall is a tool call on an assistant message.
type OpenAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Index    *int               `json:"index"`
	Function OpenAIToolFunction `json:"function"`
}

// OpenAIToolFunction is the callable part of a tool call.
type OpenAIToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// OpenAITool is a tool declaration.
//
// Both shapes clients use are accepted: the spec-compliant
// {"type":"function","function":{...}} and Cursor's flat {name, description,
// input_schema}.
type OpenAITool struct {
	Type     string `json:"type"`
	Function *struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`

	// Flat form, as sent by Cursor.
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
	Parameters  map[string]any `json:"parameters"`
}

// Normalise returns the tool in a single shape, reporting whether it is usable.
func (t OpenAITool) Normalise() (Tool, bool) {
	if t.Function != nil && t.Function.Name != "" {
		return Tool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: t.Function.Parameters,
		}, true
	}
	if t.Name != "" {
		schema := t.InputSchema
		if schema == nil {
			schema = t.Parameters
		}
		return Tool{Name: t.Name, Description: t.Description, InputSchema: schema}, true
	}
	return Tool{}, false
}

// openAIContentPart is one block of structured message content.
type openAIContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL *struct {
		URL    string `json:"url"`
		Detail string `json:"detail"`
	} `json:"image_url"`
	// Some clients use the Anthropic-style source object inside an OpenAI request.
	Source *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	} `json:"source"`
}

// FromOpenAI converts an OpenAI request into the intermediate representation.
func FromOpenAI(req *OpenAIRequest) (*Request, error) {
	out := &Request{
		Model:  req.Model,
		Stream: req.Stream,
	}
	if req.MaxTokens != nil {
		out.MaxTokens = *req.MaxTokens
	}
	if req.MaxCompletionTok != nil {
		out.MaxTokens = *req.MaxCompletionTok
	}
	out.EffortLevel, out.DisableReasoning = normalizeReasoningEffort(req.ReasoningEffort)

	for _, t := range req.Tools {
		tool, ok := t.Normalise()
		if !ok {
			slog.Debug("skipping a tool declaration with no name")
			continue
		}
		out.Tools = append(out.Tools, tool)
	}

	// Tool results attach to the next user turn, so they are buffered until one
	// exists. If the conversation ends on tool results, a user turn is synthesised
	// to carry them.
	var pendingResults []ToolResult

	flushPending := func() {
		if len(pendingResults) == 0 {
			return
		}
		out.Messages = append(out.Messages, Message{
			Role:        RoleUser,
			ToolResults: pendingResults,
		})
		pendingResults = nil
	}

	for i := range req.Messages {
		m := &req.Messages[i]
		switch strings.ToLower(m.Role) {
		case "system", "developer":
			// Both fold into the system prompt.
			text, _ := openAIContent(m.Content)
			out.SystemPrompt = appendSystemText(out.SystemPrompt, text)

		case "tool", "function":
			text, images := openAIContent(m.Content)
			pendingResults = append(pendingResults, ToolResult{
				ToolUseID: m.ToolCallID,
				Content:   text,
				Images:    images,
			})

		case "assistant":
			flushPending()
			text, images := openAIContent(m.Content)
			msg := Message{Role: RoleAssistant, Content: text, Images: images}
			for _, tc := range m.ToolCalls {
				msg.ToolCalls = append(msg.ToolCalls, ToolCall{
					ID:        tc.ID,
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				})
			}
			msg.Reasoning = openAIReasoning(m)
			out.Messages = append(out.Messages, msg)

		default:
			// user, and anything unrecognised, which the invariants normalise.
			text, images := openAIContent(m.Content)
			msg := Message{Role: RoleUser, Content: text, Images: images}
			if len(pendingResults) > 0 {
				msg.ToolResults = pendingResults
				pendingResults = nil
			}
			out.Messages = append(out.Messages, msg)
		}
	}
	flushPending()

	if len(out.Messages) == 0 && out.SystemPrompt == "" {
		return nil, fmt.Errorf("the request has no messages. Send at least one message in the messages array")
	}
	return out, nil
}

// openAIReasoning rebuilds reasoning from an assistant message.
func openAIReasoning(m *OpenAIMessage) *Reasoning {
	if m.ReasoningContent == "" && m.ReasoningRedacted == "" {
		return nil
	}
	r := &Reasoning{Text: m.ReasoningContent, Signature: m.ReasoningSignature}
	if m.ReasoningRedacted != "" {
		if blob, err := decodeBase64(m.ReasoningRedacted); err == nil {
			r.RedactedContent = blob
		} else {
			slog.Debug("ignoring reasoning_redacted_content that is not valid base64", "error", err)
		}
	}
	return r
}

// openAIContent extracts text and images from a content field.
//
// The field may be a plain string or an array of typed blocks. Images arrive as
// data URLs; an http(s) URL is skipped with a log line, because the Kiro backend
// cannot fetch it and silently sending nothing would be worse than saying so.
func openAIContent(content any) (string, []Image) {
	switch v := content.(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	}

	raw, err := json.Marshal(content)
	if err != nil {
		return fmt.Sprint(content), nil
	}

	var parts []openAIContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		// Not an array of blocks: fall back to a plain string if possible.
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s, nil
		}
		return string(raw), nil
	}

	var text strings.Builder
	var images []Image
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text", "":
			text.WriteString(p.Text)
		case "image_url":
			if p.ImageURL == nil {
				continue
			}
			if img, ok := imageFromURL(p.ImageURL.URL); ok {
				images = append(images, img)
			}
		case "image":
			if p.Source == nil {
				continue
			}
			if p.Source.Type == "url" || p.Source.URL != "" {
				slog.Warn("skipping an image given as a URL: the Kiro backend cannot fetch remote images. Send it inline as a base64 data URL instead.")
				continue
			}
			if p.Source.Data != "" {
				mediaType := p.Source.MediaType
				if mediaType == "" {
					mediaType = "image/png"
				}
				images = append(images, Image{MediaType: mediaType, Data: p.Source.Data})
			}
		default:
			// An unrecognised block with text is still worth keeping.
			if p.Text != "" {
				text.WriteString(p.Text)
			}
		}
	}
	return text.String(), images
}

// imageFromURL turns an image_url value into an Image.
func imageFromURL(url string) (Image, bool) {
	if url == "" {
		return Image{}, false
	}
	if mediaType, data, ok := parseDataURL(url); ok {
		return Image{MediaType: mediaType, Data: data}, true
	}
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		slog.Warn("skipping an image given as a URL: the Kiro backend cannot fetch remote images. Send it inline as a base64 data URL instead.",
			"url_prefix", truncate(url, 60))
		return Image{}, false
	}
	slog.Debug("skipping an image_url that is neither a data URL nor http(s)")
	return Image{}, false
}

// normalizeReasoningEffort maps the OpenAI reasoning_effort values onto Kiro's.
//
// "none" disables reasoning outright, "minimal" is the closest thing to "low",
// and anything else is passed through for the catalog to clamp.
func normalizeReasoningEffort(value string) (level string, disabled bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return "", false
	case "none", "off", "disabled":
		return "", true
	case "minimal":
		return "low", false
	default:
		return strings.ToLower(strings.TrimSpace(value)), false
	}
}

// truncate shortens a string for logging.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.

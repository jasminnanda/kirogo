package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kirogo/internal/kiro"
)

// postMessages sends an Anthropic request with the x-api-key header.
func postMessages(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("x-api-key", "test-key")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// simpleMessagesBody is a minimal Anthropic request.
func simpleMessagesBody(stream bool) string {
	if stream {
		return `{"model":"claude-opus-5","max_tokens":1024,"stream":true,
		  "messages":[{"role":"user","content":"hello"}]}`
	}
	return `{"model":"claude-opus-5","max_tokens":1024,
	  "messages":[{"role":"user","content":"hello"}]}`
}

// anthropicFrames parses an SSE body into (eventName, decoded payload) pairs.
func anthropicFrames(t *testing.T, body string) []struct {
	Name    string
	Payload map[string]any
} {
	t.Helper()
	var out []struct {
		Name    string
		Payload map[string]any
	}
	for _, f := range parseSSE(t, body) {
		var payload map[string]any
		if err := json.Unmarshal([]byte(f.Data), &payload); err != nil {
			t.Fatalf("frame %q has invalid JSON: %v\n%s", f.Event, err, f.Data)
		}
		out = append(out, struct {
			Name    string
			Payload map[string]any
		}{Name: f.Event, Payload: payload})
	}
	return out
}

// frameNames lists the event names in order.
func frameNames(frames []struct {
	Name    string
	Payload map[string]any
}) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		out = append(out, f.Name)
	}
	return out
}

func TestMessagesStreamExactEventSequence(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"reasoningContentEvent", `{"text":"Let me think."}`},
		{"reasoningContentEvent", `{"text":" Done.","signature":"sig-1"}`},
		{"assistantResponseEvent", `{"content":"Hello, "}`},
		{"assistantResponseEvent", `{"content":"world!"}`},
		{"metadataEvent", `{"tokenUsage":{"uncachedInputTokens":10,"outputTokens":5,"totalTokens":15},"stopReason":"end_turn"}`},
	}})
	s := newHarness(t, up, testServerOptions{})

	rec := postMessages(t, s, "/v1/messages", simpleMessagesBody(true))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q", got)
	}

	frames := anthropicFrames(t, rec.Body.String())
	names := frameNames(frames)

	want := []string{
		"message_start",
		"content_block_start", // thinking
		"content_block_delta", // thinking_delta
		"content_block_delta", // thinking_delta
		"content_block_delta", // signature_delta
		"content_block_stop",  // thinking closes
		"content_block_start", // text
		"content_block_delta", // text_delta
		"content_block_delta", // text_delta
		"content_block_stop",  // text closes
		"message_delta",
		"message_stop",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("event sequence =\n  %v\nwant\n  %v", names, want)
	}

	// Every frame must carry a type matching its event name.
	for i, f := range frames {
		if f.Payload["type"] != f.Name {
			t.Errorf("frame %d: event %q but payload type %v", i, f.Name, f.Payload["type"])
		}
	}

	start := frames[0].Payload["message"].(map[string]any)
	if start["type"] != "message" || start["role"] != "assistant" {
		t.Errorf("message_start message = %v", start)
	}
	id, _ := start["id"].(string)
	if !strings.HasPrefix(id, "msg_") {
		t.Errorf("message id = %q, want a msg_ prefix", id)
	}
	if len(id) != len("msg_")+24 {
		t.Errorf("message id = %q, want 24 hex characters after the prefix", id)
	}
	if start["model"] != "claude-opus-5" {
		t.Errorf("model = %v", start["model"])
	}
	if content, ok := start["content"].([]any); !ok || len(content) != 0 {
		t.Errorf("message_start content = %v, want an empty array", start["content"])
	}
	if start["stop_reason"] != nil {
		t.Errorf("message_start stop_reason = %v, want null", start["stop_reason"])
	}
	usage := start["usage"].(map[string]any)
	if usage["output_tokens"].(float64) != 0 {
		t.Errorf("message_start output_tokens = %v, want 0", usage["output_tokens"])
	}
	if usage["input_tokens"].(float64) <= 0 {
		t.Errorf("message_start input_tokens = %v, want the estimate", usage["input_tokens"])
	}

	// Block indices increment across the whole message.
	if got := frames[1].Payload["index"].(float64); got != 0 {
		t.Errorf("thinking block index = %v, want 0", got)
	}
	if got := frames[6].Payload["index"].(float64); got != 1 {
		t.Errorf("text block index = %v, want 1", got)
	}

	// The thinking block declares its type and starts empty.
	thinkingBlock := frames[1].Payload["content_block"].(map[string]any)
	if thinkingBlock["type"] != "thinking" || thinkingBlock["thinking"] != "" {
		t.Errorf("thinking content_block = %v", thinkingBlock)
	}
	textBlock := frames[6].Payload["content_block"].(map[string]any)
	if textBlock["type"] != "text" || textBlock["text"] != "" {
		t.Errorf("text content_block = %v", textBlock)
	}

	// Delta types.
	var thinking, text, signature string
	for _, f := range frames {
		if f.Name != "content_block_delta" {
			continue
		}
		delta := f.Payload["delta"].(map[string]any)
		switch delta["type"] {
		case "thinking_delta":
			thinking += delta["thinking"].(string)
		case "text_delta":
			text += delta["text"].(string)
		case "signature_delta":
			signature = delta["signature"].(string)
		default:
			t.Errorf("unexpected delta type %v", delta["type"])
		}
	}
	if thinking != "Let me think. Done." {
		t.Errorf("thinking = %q", thinking)
	}
	if text != "Hello, world!" {
		t.Errorf("text = %q", text)
	}
	if signature != "sig-1" {
		t.Errorf("signature = %q", signature)
	}

	// message_delta carries the stop reason and final usage.
	md := frames[len(frames)-2]
	delta := md.Payload["delta"].(map[string]any)
	if delta["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v, want end_turn", delta["stop_reason"])
	}
	if _, present := delta["stop_sequence"]; !present {
		t.Error("message_delta should include stop_sequence, even as null")
	}
	finalUsage := md.Payload["usage"].(map[string]any)
	if finalUsage["input_tokens"].(float64) != 10 || finalUsage["output_tokens"].(float64) != 5 {
		t.Errorf("final usage = %v, want the exact upstream counts", finalUsage)
	}
}

func TestMessagesStreamToolUseBlock(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"assistantResponseEvent", `{"content":"Checking."}`},
		{"toolUseEvent", `{"toolUseId":"toolu_abc","name":"get_weather","input":"{\"city\":"}`},
		{"toolUseEvent", `{"toolUseId":"toolu_abc","name":"get_weather","input":"\"Berlin\"}","stop":true}`},
		{"metadataEvent", `{"tokenUsage":{"uncachedInputTokens":5,"outputTokens":2,"totalTokens":7},"stopReason":"tool_use"}`},
	}})
	s := newHarness(t, up, testServerOptions{})

	rec := postMessages(t, s, "/v1/messages", `{"model":"claude-opus-5","max_tokens":1024,"stream":true,
	  "messages":[{"role":"user","content":"weather?"}],
	  "tools":[{"name":"get_weather","description":"d","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	frames := anthropicFrames(t, rec.Body.String())

	// Find the tool_use block.
	var toolStart, toolDelta, toolStop int = -1, -1, -1
	for i, f := range frames {
		switch f.Name {
		case "content_block_start":
			if block, ok := f.Payload["content_block"].(map[string]any); ok && block["type"] == "tool_use" {
				toolStart = i
			}
		case "content_block_delta":
			if delta, ok := f.Payload["delta"].(map[string]any); ok && delta["type"] == "input_json_delta" {
				toolDelta = i
			}
		case "content_block_stop":
			if toolStart >= 0 && toolStop < 0 && i > toolStart {
				toolStop = i
			}
		}
	}
	if toolStart < 0 || toolDelta < 0 || toolStop < 0 {
		t.Fatalf("tool_use block is incomplete: start=%d delta=%d stop=%d\n%v",
			toolStart, toolDelta, toolStop, frameNames(frames))
	}
	if !(toolStart < toolDelta && toolDelta < toolStop) {
		t.Errorf("tool_use frames are out of order: start=%d delta=%d stop=%d", toolStart, toolDelta, toolStop)
	}

	block := frames[toolStart].Payload["content_block"].(map[string]any)
	if block["id"] != "toolu_abc" || block["name"] != "get_weather" {
		t.Errorf("tool_use block = %v", block)
	}
	if input, ok := block["input"].(map[string]any); !ok || len(input) != 0 {
		t.Errorf("tool_use input = %v, want an empty object at block start", block["input"])
	}

	delta := frames[toolDelta].Payload["delta"].(map[string]any)
	if delta["partial_json"] != `{"city":"Berlin"}` {
		t.Errorf("partial_json = %v, want the reassembled arguments", delta["partial_json"])
	}

	// The text block must have index 0 and the tool block index 1.
	if got := frames[toolStart].Payload["index"].(float64); got != 1 {
		t.Errorf("tool_use index = %v, want 1 (after the text block)", got)
	}

	md := frames[len(frames)-2]
	if md.Payload["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v, want tool_use", md.Payload["delta"])
	}
}

func TestMessagesStreamMultipleToolBlocksGetDistinctIndices(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"toolUseEvent", `{"toolUseId":"t1","name":"a","input":"{\"x\":1}","stop":true}`},
		{"toolUseEvent", `{"toolUseId":"t2","name":"b","input":"{\"y\":2}","stop":true}`},
		{"metadataEvent", `{"tokenUsage":{"outputTokens":1,"totalTokens":2},"stopReason":"tool_use"}`},
	}})
	s := newHarness(t, up, testServerOptions{})

	frames := anthropicFrames(t, postMessages(t, s, "/v1/messages", simpleMessagesBody(true)).Body.String())

	var indices []float64
	for _, f := range frames {
		if f.Name != "content_block_start" {
			continue
		}
		if block, ok := f.Payload["content_block"].(map[string]any); ok && block["type"] == "tool_use" {
			indices = append(indices, f.Payload["index"].(float64))
		}
	}
	if len(indices) != 2 {
		t.Fatalf("tool_use blocks = %d, want 2", len(indices))
	}
	if indices[0] == indices[1] {
		t.Errorf("both tool blocks share index %v", indices[0])
	}
	if indices[1] != indices[0]+1 {
		t.Errorf("indices = %v, want consecutive", indices)
	}
}

func TestMessagesStreamEveryOpenedBlockIsClosed(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"reasoningContentEvent", `{"text":"think","signature":"s"}`},
		{"assistantResponseEvent", `{"content":"text"}`},
		{"toolUseEvent", `{"toolUseId":"t","name":"n","input":"{}","stop":true}`},
		{"metadataEvent", `{"tokenUsage":{"outputTokens":1,"totalTokens":2},"stopReason":"tool_use"}`},
	}})
	s := newHarness(t, up, testServerOptions{})

	frames := anthropicFrames(t, postMessages(t, s, "/v1/messages", simpleMessagesBody(true)).Body.String())

	starts, stops := 0, 0
	openIndices := map[float64]bool{}
	for _, f := range frames {
		switch f.Name {
		case "content_block_start":
			starts++
			idx := f.Payload["index"].(float64)
			if openIndices[idx] {
				t.Errorf("index %v was opened twice", idx)
			}
			openIndices[idx] = true
		case "content_block_stop":
			stops++
			idx := f.Payload["index"].(float64)
			if !openIndices[idx] {
				t.Errorf("index %v was closed without being opened", idx)
			}
			delete(openIndices, idx)
		}
	}
	if starts != stops {
		t.Errorf("%d blocks opened but %d closed", starts, stops)
	}
	if len(openIndices) != 0 {
		t.Errorf("blocks left open: %v", openIndices)
	}
	if starts != 3 {
		t.Errorf("blocks = %d, want thinking, text and tool_use", starts)
	}
}

func TestMessagesStreamRedactedThinking(t *testing.T) {
	blob := base64.StdEncoding.EncodeToString([]byte{4, 5, 6})
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"reasoningContentEvent", `{"redactedContent":"` + blob + `"}`},
		{"assistantResponseEvent", `{"content":"answer"}`},
		{"metadataEvent", `{"tokenUsage":{"outputTokens":1,"totalTokens":2},"stopReason":"end_turn"}`},
	}})
	s := newHarness(t, up, testServerOptions{})

	frames := anthropicFrames(t, postMessages(t, s, "/v1/messages", simpleMessagesBody(true)).Body.String())

	var found bool
	for _, f := range frames {
		if f.Name != "content_block_start" {
			continue
		}
		block, ok := f.Payload["content_block"].(map[string]any)
		if !ok || block["type"] != "redacted_thinking" {
			continue
		}
		found = true
		if block["data"] != blob {
			t.Errorf("redacted data = %v, want %q", block["data"], blob)
		}
	}
	if !found {
		t.Errorf("no redacted_thinking block: %v", frameNames(frames))
	}
}

func TestMessagesStreamStopReasonMapping(t *testing.T) {
	cases := map[string]string{
		"end_turn":      "end_turn",
		"stop_sequence": "stop_sequence",
		"max_tokens":    "max_tokens",
		"tool_use":      "tool_use",
		"anything_else": "end_turn",
	}
	for upstream, want := range cases {
		t.Run("stopReason="+upstream, func(t *testing.T) {
			up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
				{"assistantResponseEvent", `{"content":"text"}`},
				{"metadataEvent", `{"tokenUsage":{"uncachedInputTokens":1,"outputTokens":1,"totalTokens":2},"stopReason":"` + upstream + `"}`},
			}})
			s := newHarness(t, up, testServerOptions{})
			frames := anthropicFrames(t, postMessages(t, s, "/v1/messages", simpleMessagesBody(true)).Body.String())

			md := frames[len(frames)-2]
			if md.Name != "message_delta" {
				t.Fatalf("second-last frame is %q, want message_delta", md.Name)
			}
			if got := md.Payload["delta"].(map[string]any)["stop_reason"]; got != want {
				t.Errorf("stop_reason = %v, want %q", got, want)
			}
		})
	}
}

func TestMessagesStreamTruncationBecomesMaxTokens(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"assistantResponseEvent", `{"content":"cut short"}`},
	}})
	s := newHarness(t, up, testServerOptions{})
	frames := anthropicFrames(t, postMessages(t, s, "/v1/messages", simpleMessagesBody(true)).Body.String())

	md := frames[len(frames)-2]
	if got := md.Payload["delta"].(map[string]any)["stop_reason"]; got != "max_tokens" {
		t.Errorf("stop_reason = %v, want max_tokens for a truncated response", got)
	}
}

func TestMessagesStreamMidStreamErrorEvent(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"assistantResponseEvent", `{"content":"starting"}`},
		{"ThrottlingException", `{"message":"Too many requests","retryAfterMilliseconds":500}`},
	}})
	s := newHarness(t, up, testServerOptions{})

	rec := postMessages(t, s, "/v1/messages", simpleMessagesBody(true))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: the stream had begun", rec.Code)
	}
	frames := anthropicFrames(t, rec.Body.String())

	last := frames[len(frames)-1]
	if last.Name != "error" {
		t.Fatalf("last frame is %q, want error: %v", last.Name, frameNames(frames))
	}
	errBody := last.Payload["error"].(map[string]any)
	if errBody["type"] == nil || errBody["message"] == nil {
		t.Errorf("error frame = %v, want a type and message", errBody)
	}
	// Any open block must be closed before the error.
	starts, stops := 0, 0
	for _, f := range frames {
		switch f.Name {
		case "content_block_start":
			starts++
		case "content_block_stop":
			stops++
		}
	}
	if starts != stops {
		t.Errorf("%d blocks opened, %d closed: an open block was abandoned", starts, stops)
	}
}

// ---------- non-streaming ----------

func TestMessagesNonStreamingShape(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"reasoningContentEvent", `{"text":"Thinking.","signature":"sig-1"}`},
		{"assistantResponseEvent", `{"content":"Hello!"}`},
		{"toolUseEvent", `{"toolUseId":"toolu_1","name":"tool","input":"{\"a\":1}","stop":true}`},
		{"meteringEvent", `{"usage":1.25,"unit":"credit","unitPlural":"credits"}`},
		{"metadataEvent", `{"tokenUsage":{"uncachedInputTokens":10,"outputTokens":4,"totalTokens":14,
		  "cacheReadInputTokens":3,"cacheWriteInputTokens":2}}`},
	}})
	s := newHarness(t, up, testServerOptions{})

	rec := postMessages(t, s, "/v1/messages", simpleMessagesBody(false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	var msg anthropicMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("response is not a message: %v\n%s", err, rec.Body.String())
	}

	if msg.Type != "message" || msg.Role != "assistant" {
		t.Errorf("type/role = %q/%q", msg.Type, msg.Role)
	}
	if !strings.HasPrefix(msg.ID, "msg_") {
		t.Errorf("id = %q", msg.ID)
	}
	if msg.Model != "claude-opus-5" {
		t.Errorf("model = %q", msg.Model)
	}
	if msg.StopReason == nil || *msg.StopReason != "tool_use" {
		t.Errorf("stop_reason = %v, want tool_use", msg.StopReason)
	}

	if len(msg.Content) != 3 {
		t.Fatalf("content blocks = %d, want thinking, text and tool_use: %+v", len(msg.Content), msg.Content)
	}
	if msg.Content[0].Type != "thinking" || msg.Content[0].Thinking != "Thinking." {
		t.Errorf("block 0 = %+v", msg.Content[0])
	}
	if msg.Content[0].Signature != "sig-1" {
		t.Errorf("thinking signature = %q, want it exposed for the next turn", msg.Content[0].Signature)
	}
	if msg.Content[1].Type != "text" || msg.Content[1].Text != "Hello!" {
		t.Errorf("block 1 = %+v", msg.Content[1])
	}
	if msg.Content[2].Type != "tool_use" || msg.Content[2].ID != "toolu_1" || msg.Content[2].Name != "tool" {
		t.Errorf("block 2 = %+v", msg.Content[2])
	}
	input, ok := msg.Content[2].Input.(map[string]any)
	if !ok || input["a"] != float64(1) {
		t.Errorf("tool_use input = %v, want a parsed object", msg.Content[2].Input)
	}

	if msg.Usage.InputTokens != 15 {
		t.Errorf("input_tokens = %d, want 15 (10 uncached + 3 read + 2 write)", msg.Usage.InputTokens)
	}
	if msg.Usage.OutputTokens != 4 {
		t.Errorf("output_tokens = %d", msg.Usage.OutputTokens)
	}
	if msg.Usage.CacheReadInputTokens != 3 || msg.Usage.CacheCreationInputTokens != 2 {
		t.Errorf("cache usage = %d / %d", msg.Usage.CacheReadInputTokens, msg.Usage.CacheCreationInputTokens)
	}
	if msg.Usage.CreditsUsed == nil || *msg.Usage.CreditsUsed != 1.25 {
		t.Errorf("credits_used = %v", msg.Usage.CreditsUsed)
	}
}

func TestMessagesNonStreamingEmptyContentIsAnArray(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"metadataEvent", `{"tokenUsage":{"uncachedInputTokens":1,"outputTokens":0,"totalTokens":1},"stopReason":"end_turn"}`},
	}})
	s := newHarness(t, up, testServerOptions{FirstTokenMaxRetries: 1})

	rec := postMessages(t, s, "/v1/messages", simpleMessagesBody(false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	// Clients index into content, so it must never be null.
	if !strings.Contains(rec.Body.String(), `"content":[]`) {
		t.Errorf("content should be an empty array, got %s", rec.Body.String())
	}
}

func TestMessagesNonStreamingAndStreamingAgree(t *testing.T) {
	events := []scriptedEvent{
		{"reasoningContentEvent", `{"text":"reason","signature":"s"}`},
		{"assistantResponseEvent", `{"content":"answer"}`},
		{"toolUseEvent", `{"toolUseId":"t1","name":"tool","input":"{\"a\":1}","stop":true}`},
		{"metadataEvent", `{"tokenUsage":{"uncachedInputTokens":7,"outputTokens":3,"totalTokens":10},"stopReason":"tool_use"}`},
	}

	upStream := newFakeUpstream(t, upstreamScript{Events: events})
	frames := anthropicFrames(t, postMessages(t, newHarness(t, upStream, testServerOptions{}),
		"/v1/messages", simpleMessagesBody(true)).Body.String())

	var streamThinking, streamText, streamToolJSON, streamStop string
	var streamUsage map[string]any
	for _, f := range frames {
		switch f.Name {
		case "content_block_delta":
			delta := f.Payload["delta"].(map[string]any)
			switch delta["type"] {
			case "thinking_delta":
				streamThinking += delta["thinking"].(string)
			case "text_delta":
				streamText += delta["text"].(string)
			case "input_json_delta":
				streamToolJSON += delta["partial_json"].(string)
			}
		case "message_delta":
			streamStop = f.Payload["delta"].(map[string]any)["stop_reason"].(string)
			streamUsage = f.Payload["usage"].(map[string]any)
		}
	}

	upWhole := newFakeUpstream(t, upstreamScript{Events: events})
	rec := postMessages(t, newHarness(t, upWhole, testServerOptions{}), "/v1/messages", simpleMessagesBody(false))
	var msg anthropicMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatal(err)
	}

	var wholeThinking, wholeText, wholeToolJSON string
	for _, b := range msg.Content {
		switch b.Type {
		case "thinking":
			wholeThinking = b.Thinking
		case "text":
			wholeText = b.Text
		case "tool_use":
			encoded, err := json.Marshal(b.Input)
			if err != nil {
				t.Fatal(err)
			}
			wholeToolJSON = string(encoded)
		}
	}

	if streamThinking != wholeThinking {
		t.Errorf("thinking differs: %q streamed, %q assembled", streamThinking, wholeThinking)
	}
	if streamText != wholeText {
		t.Errorf("text differs: %q streamed, %q assembled", streamText, wholeText)
	}
	if streamToolJSON != wholeToolJSON {
		t.Errorf("tool input differs: %q streamed, %q assembled", streamToolJSON, wholeToolJSON)
	}
	if msg.StopReason == nil || streamStop != *msg.StopReason {
		t.Errorf("stop reason differs: %q streamed, %v assembled", streamStop, msg.StopReason)
	}
	if int(streamUsage["input_tokens"].(float64)) != msg.Usage.InputTokens {
		t.Errorf("input tokens differ: %v streamed, %d assembled",
			streamUsage["input_tokens"], msg.Usage.InputTokens)
	}
}

// ---------- auth and validation ----------

func TestMessagesRejectsWrongAPIKey(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{{"assistantResponseEvent", `{"content":"x"}`}}})
	s := newHarness(t, up, testServerOptions{})

	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(simpleMessagesBody(false)))
			req.Header.Set("x-api-key", "wrong-key")
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			var body anthropicError
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("not JSON: %v", err)
			}
			if body.Type != "error" || body.Error.Type != "authentication_error" {
				t.Errorf("envelope = %+v, want the Anthropic error shape", body)
			}
		})
	}
	if up.RequestCount() != 0 {
		t.Error("an unauthenticated request must not reach the backend")
	}
}

func TestMessagesMissingKeyMentionsXAPIKey(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{})
	s := newHarness(t, up, testServerOptions{})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(simpleMessagesBody(false)))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	var body anthropicError
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.Error.Message, "x-api-key") {
		t.Errorf("message = %q, should name the header Anthropic clients use", body.Error.Message)
	}
}

func TestMessagesRejectsBadInput(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{{"assistantResponseEvent", `{"content":"x"}`}}})
	s := newHarness(t, up, testServerOptions{})

	cases := []struct {
		name     string
		body     string
		wantText string
	}{
		{"not json", `{nope`, "not valid JSON"},
		{"no model", `{"max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`, "No model was given"},
		{"no messages", `{"model":"claude-opus-5","max_tokens":1,"messages":[]}`, "no messages"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postMessages(t, s, "/v1/messages", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			var body anthropicError
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(body.Error.Message, tc.wantText) {
				t.Errorf("message = %q, want it to mention %q", body.Error.Message, tc.wantText)
			}
			if body.Error.Type != "invalid_request_error" {
				t.Errorf("error type = %q", body.Error.Type)
			}
		})
	}
}

func TestMessagesRejectsWrongMethod(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{})
	s := newHarness(t, up, testServerOptions{})

	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("x-api-key", "test-key")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d, want 405", path, rec.Code)
		}
	}
}

func TestMessagesUpstreamErrorUsesAnthropicEnvelope(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{
		Status:    http.StatusBadRequest,
		ErrorBody: `{"message":"Input is too long.","reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`,
	})
	s := newHarness(t, up, testServerOptions{})

	rec := postMessages(t, s, "/v1/messages", simpleMessagesBody(false))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", rec.Code, rec.Body.String())
	}
	var body anthropicError
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Type != "request_too_large" {
		t.Errorf("error type = %q, want request_too_large", body.Error.Type)
	}
	if !strings.Contains(body.Error.Message, "Context limit reached") {
		t.Errorf("message = %q", body.Error.Message)
	}
}

// ---------- system prompt, thinking budget, tools ----------

func TestMessagesSystemAsStringAndBlocks(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "string",
			body: `{"model":"claude-opus-5","max_tokens":1,"system":"BE_TERSE",
			  "messages":[{"role":"user","content":"hi"}]}`,
			want: `BE_TERSE\n\nhi`,
		},
		{
			name: "block list",
			body: `{"model":"claude-opus-5","max_tokens":1,
			  "system":[{"type":"text","text":"FIRST"},{"type":"text","text":"SECOND"}],
			  "messages":[{"role":"user","content":"hi"}]}`,
			want: `FIRST\n\nSECOND\n\nhi`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
				{"assistantResponseEvent", `{"content":"x"}`},
				{"metadataEvent", `{"tokenUsage":{"outputTokens":1,"totalTokens":2},"stopReason":"end_turn"}`},
			}})
			s := newHarness(t, up, testServerOptions{})

			if rec := postMessages(t, s, "/v1/messages", tc.body); rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			body := string(up.Requests()[0])
			if !strings.Contains(body, tc.want) {
				t.Errorf("payload does not contain %s\ngot: %s", tc.want, body)
			}
		})
	}
}

func TestMessagesThinkingBudgetSelectsEffort(t *testing.T) {
	cases := []struct {
		budget int
		want   string
	}{
		{500, `{"output_config":{"effort":"low"}}`},
		{2000, `{"output_config":{"effort":"medium"}}`},
		{6000, `{"output_config":{"effort":"high"}}`},
		{12000, `{"output_config":{"effort":"xhigh"}}`},
		{50000, `{"output_config":{"effort":"max"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
				{"assistantResponseEvent", `{"content":"x"}`},
				{"metadataEvent", `{"tokenUsage":{"outputTokens":1,"totalTokens":2},"stopReason":"end_turn"}`},
			}})
			// The harness model advertises effort under output_config.
			s := newHarness(t, up, testServerOptions{ModelSpecs: []kiro.ModelSpec{specWithOutputConfig()}})

			body := `{"model":"claude-opus-5","max_tokens":1024,
			  "thinking":{"type":"enabled","budget_tokens":` + itoa(tc.budget) + `},
			  "messages":[{"role":"user","content":"hi"}]}`
			if rec := postMessages(t, s, "/v1/messages", body); rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			sent := string(up.Requests()[0])
			if !strings.Contains(sent, tc.want) {
				t.Errorf("payload does not contain %s\ngot: %s", tc.want, sent)
			}
		})
	}
}

func TestMessagesThinkingDisabledSendsNoEffort(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"assistantResponseEvent", `{"content":"x"}`},
		{"metadataEvent", `{"tokenUsage":{"outputTokens":1,"totalTokens":2},"stopReason":"end_turn"}`},
	}})
	s := newHarness(t, up, testServerOptions{})

	body := `{"model":"claude-opus-5","max_tokens":1024,"thinking":{"type":"disabled"},
	  "messages":[{"role":"user","content":"hi"}]}`
	if rec := postMessages(t, s, "/v1/messages", body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(string(up.Requests()[0]), "additionalModelRequestFields") {
		t.Errorf("thinking disabled must send no effort: %s", up.Requests()[0])
	}
}

// ---------- count_tokens ----------

func TestCountTokensShape(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{})
	s := newHarness(t, up, testServerOptions{})

	rec := postMessages(t, s, "/v1/messages/count_tokens", `{"model":"claude-opus-5","max_tokens":1024,
	  "messages":[{"role":"user","content":"Count the tokens in this sentence please."}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	var body countTokensResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, rec.Body.String())
	}
	if body.InputTokens <= 0 {
		t.Errorf("input_tokens = %d, want a positive estimate", body.InputTokens)
	}

	// The response must carry exactly the one field Anthropic specifies.
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 1 {
		t.Errorf("body has %d fields (%v), want only input_tokens", len(raw), raw)
	}
	if _, ok := raw["input_tokens"]; !ok {
		t.Errorf("body = %v, want an input_tokens field", raw)
	}

	// It must not touch the backend.
	if up.RequestCount() != 0 {
		t.Errorf("count_tokens made %d upstream calls, want none", up.RequestCount())
	}
}

func TestCountTokensGrowsWithInput(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{})
	s := newHarness(t, up, testServerOptions{})

	count := func(body string) int {
		rec := postMessages(t, s, "/v1/messages/count_tokens", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		var parsed countTokensResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
			t.Fatal(err)
		}
		return parsed.InputTokens
	}

	small := count(`{"model":"claude-opus-5","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
	large := count(`{"model":"claude-opus-5","max_tokens":1,"messages":[{"role":"user","content":"` +
		strings.Repeat("word ", 500) + `"}]}`)
	withSystem := count(`{"model":"claude-opus-5","max_tokens":1,"system":"` +
		strings.Repeat("instruction ", 200) + `","messages":[{"role":"user","content":"hi"}]}`)
	withTools := count(`{"model":"claude-opus-5","max_tokens":1,
	  "messages":[{"role":"user","content":"hi"}],
	  "tools":[{"name":"t","description":"` + strings.Repeat("doc ", 200) +
		`","input_schema":{"type":"object","properties":{"a":{"type":"string"}}}}]}`)

	if large <= small {
		t.Errorf("more text should count higher: %d vs %d", large, small)
	}
	if withSystem <= small {
		t.Errorf("a system prompt should count: %d vs %d", withSystem, small)
	}
	if withTools <= small {
		t.Errorf("tool declarations should count: %d vs %d", withTools, small)
	}
}

func TestCountTokensCountsImages(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{})
	s := newHarness(t, up, testServerOptions{})

	textOnly := postMessages(t, s, "/v1/messages/count_tokens",
		`{"model":"claude-opus-5","max_tokens":1,"messages":[{"role":"user","content":[{"type":"text","text":"look"}]}]}`)
	withImage := postMessages(t, s, "/v1/messages/count_tokens",
		`{"model":"claude-opus-5","max_tokens":1,"messages":[{"role":"user","content":[
		  {"type":"text","text":"look"},
		  {"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]}]}`)

	var a, b countTokensResponse
	if err := json.Unmarshal(textOnly.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(withImage.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if b.InputTokens <= a.InputTokens {
		t.Errorf("an image should add tokens: %d with, %d without", b.InputTokens, a.InputTokens)
	}
}

func TestCountTokensRejectsBadInput(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{})
	s := newHarness(t, up, testServerOptions{})

	rec := postMessages(t, s, "/v1/messages/count_tokens", `{"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 with no model", rec.Code)
	}
}

// ---------- helpers ----------

// specWithOutputConfig returns a model spec whose effort lives under
// output_config, matching the live claude-opus-5.
func specWithOutputConfig() kiro.ModelSpec {
	return kiro.ModelSpec{
		ModelID:     "claude-opus-5",
		ModelName:   "Claude Opus 5",
		TokenLimits: &kiro.TokenLimits{MaxInputTokens: 1000000, MaxOutputTokens: 128000},
		AdditionalModelRequestFieldsSchema: map[string]any{"properties": map[string]any{
			"output_config": map[string]any{"properties": map[string]any{
				"effort": map[string]any{
					"enum":    []any{"low", "medium", "high", "xhigh", "max"},
					"default": "high",
				},
			}},
		}},
	}
}

// itoa converts an int to a decimal string.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	negative := n < 0
	if negative {
		n = -n
	}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.

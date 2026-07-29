package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kirogo/internal/catalog"
	"kirogo/internal/config"
	"kirogo/internal/kiro"
)

// ---------- test harness ----------

// stubAuth satisfies both the Kiro TokenProvider and the api ProfileProvider.
type stubAuth struct {
	host       string
	profileARN string
	refreshes  atomic.Int64
}

func (a *stubAuth) Token(context.Context) (string, error) { return "test-token", nil }
func (a *stubAuth) ForceRefresh(context.Context) (string, error) {
	a.refreshes.Add(1)
	return "test-token", nil
}
func (a *stubAuth) ProfileARN() string       { return a.profileARN }
func (a *stubAuth) RuntimeHost() string      { return a.host }
func (a *stubAuth) ControlPlaneHost() string { return a.host }
func (a *stubAuth) Fingerprint() string      { return "fingerprint" }
func (a *stubAuth) KiroVersion() string      { return "0.7.45" }
func (a *stubAuth) TokenTypeHeader() string  { return "" }

// upstreamScript describes one scripted upstream response.
type upstreamScript struct {
	// Status is the HTTP status. Defaults to 200.
	Status int
	// ErrorBody is the JSON body for a non-200 response.
	ErrorBody string
	// Events are event frames to write, in order.
	Events []scriptedEvent
	// DelayBeforeFirst stalls before the first event, to exercise the first-token
	// timeout.
	DelayBeforeFirst time.Duration
	// DelayBetween stalls between events.
	DelayBetween time.Duration
	// TruncateAfter cuts the connection after this many events, if positive.
	TruncateAfter int
}

// scriptedEvent is one event frame to emit.
type scriptedEvent struct {
	Type    string
	Payload string
}

// fakeUpstream is a Kiro backend that replays scripted responses.
type fakeUpstream struct {
	*httptest.Server
	mu       sync.Mutex
	scripts  []upstreamScript
	requests [][]byte
	// catalogBody is served for GET /ListAvailableModels.
	catalogBody string
}

// newFakeUpstream starts a backend replaying the given scripts. The last script
// repeats once the list is exhausted.
func newFakeUpstream(t *testing.T, scripts ...upstreamScript) *fakeUpstream {
	t.Helper()
	u := &fakeUpstream{scripts: scripts}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			body := u.catalogBody
			if body == "" {
				body = `{"models":[{"modelId":"claude-opus-5"}]}`
			}
			_, _ = w.Write([]byte(body))
			return
		}

		body := make([]byte, 0)
		if r.Body != nil {
			buf := new(strings.Builder)
			_, _ = fmt.Fprint(buf)
			b := make([]byte, 1<<20)
			n, _ := r.Body.Read(b)
			for n > 0 {
				body = append(body, b[:n]...)
				n, _ = r.Body.Read(b)
			}
		}

		u.mu.Lock()
		idx := len(u.requests)
		u.requests = append(u.requests, body)
		var script upstreamScript
		if len(u.scripts) > 0 {
			if idx < len(u.scripts) {
				script = u.scripts[idx]
			} else {
				script = u.scripts[len(u.scripts)-1]
			}
		}
		u.mu.Unlock()

		if script.Status != 0 && script.Status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(script.Status)
			_, _ = w.Write([]byte(script.ErrorBody))
			return
		}

		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		if script.DelayBeforeFirst > 0 {
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(script.DelayBeforeFirst)
		}

		for i, e := range script.Events {
			if script.TruncateAfter > 0 && i >= script.TruncateAfter {
				return
			}
			frame, err := kiro.EncodeMessage([]kiro.Header{
				kiro.StringHeader(":message-type", messageTypeFor(e.Type)),
				eventTypeHeader(e.Type),
				kiro.StringHeader(":content-type", "application/json"),
			}, []byte(e.Payload))
			if err != nil {
				t.Errorf("EncodeMessage: %v", err)
				return
			}
			if _, err := w.Write(frame); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			if script.DelayBetween > 0 {
				time.Sleep(script.DelayBetween)
			}
		}
	}))
	t.Cleanup(u.Close)
	return u
}

// messageTypeFor returns the :message-type for a scripted event.
func messageTypeFor(eventType string) string {
	if strings.HasSuffix(eventType, "Exception") && eventType != "internalServerException" {
		return "exception"
	}
	return "event"
}

// eventTypeHeader returns the type header for a scripted event.
func eventTypeHeader(eventType string) kiro.Header {
	if messageTypeFor(eventType) == "exception" {
		return kiro.StringHeader(":exception-type", eventType)
	}
	return kiro.StringHeader(":event-type", eventType)
}

func (u *fakeUpstream) Requests() [][]byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([][]byte, len(u.requests))
	copy(out, u.requests)
	return out
}

func (u *fakeUpstream) RequestCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.requests)
}

// testServerOptions tunes the harness server.
type testServerOptions struct {
	FirstTokenTimeout    time.Duration
	FirstTokenMaxRetries int
	EffortLevel          string
	MaxPayloadBytes      int
	ModelSpecs           []kiro.ModelSpec
}

// newHarness builds a Server wired to a fake upstream.
func newHarness(t *testing.T, up *fakeUpstream, opts testServerOptions) *Server {
	t.Helper()

	auth := &stubAuth{host: up.URL, profileARN: "arn:aws:codewhisperer:us-east-1:1:profile/A"}
	client := kiro.NewClient(kiro.Options{
		Auth:                auth,
		AgentMode:           "vibe",
		HTTPClient:          up.Client(),
		StreamClientFactory: func() *http.Client { return up.Client() },
		Sleep:               func(time.Duration) {},
	})

	specs := opts.ModelSpecs
	if specs == nil {
		specs = []kiro.ModelSpec{{
			ModelID:     "claude-opus-5",
			ModelName:   "Claude Opus 5",
			TokenLimits: &kiro.TokenLimits{MaxInputTokens: 1000000, MaxOutputTokens: 64000},
			AdditionalModelRequestFieldsSchema: map[string]any{"properties": map[string]any{
				"reasoning": map[string]any{"properties": map[string]any{
					"effort": map[string]any{"enum": []any{"low", "medium", "high", "xhigh", "max"}, "default": "high"},
				}},
			}},
		}}
	}
	cat := catalog.New(catalog.Options{Fetcher: staticCatalog{specs: specs}, TTL: time.Hour})
	if err := cat.Refresh(context.Background()); err != nil {
		t.Fatalf("catalog refresh: %v", err)
	}

	cfg := &config.Config{
		ProxyAPIKey:              "test-key",
		ExposeEffortVariants:     true,
		AgentMode:                "vibe",
		FirstTokenTimeout:        opts.FirstTokenTimeout,
		FirstTokenMaxRetries:     opts.FirstTokenMaxRetries,
		ToolDescriptionMaxLength: 10000,
		MaxPayloadBytes:          opts.MaxPayloadBytes,
		EffortLevel:              opts.EffortLevel,
	}
	if cfg.FirstTokenTimeout == 0 {
		cfg.FirstTokenTimeout = 5 * time.Second
	}
	if cfg.FirstTokenMaxRetries == 0 {
		cfg.FirstTokenMaxRetries = 3
	}
	if cfg.MaxPayloadBytes == 0 {
		cfg.MaxPayloadBytes = 600000
	}

	return NewServer(Deps{Config: cfg, Catalog: cat, Kiro: client, Auth: auth})
}

// staticCatalog serves a fixed model list.
type staticCatalog struct{ specs []kiro.ModelSpec }

func (s staticCatalog) ListAvailableModels(context.Context, string) (*kiro.ListModelsResponse, error) {
	return &kiro.ListModelsResponse{Models: s.specs}, nil
}

// postChat sends a chat completion request through the server.
func postChat(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// sseFrame is one parsed server-sent event.
type sseFrame struct {
	Event string
	Data  string
}

// parseSSE splits an SSE body into frames.
func parseSSE(t *testing.T, body string) []sseFrame {
	t.Helper()
	var frames []sseFrame
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimRight(block, "\n")
		if strings.TrimSpace(block) == "" {
			continue
		}
		var frame sseFrame
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				frame.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				frame.Data = strings.TrimPrefix(line, "data: ")
			}
		}
		frames = append(frames, frame)
	}
	return frames
}

// chunkDeltas parses the streaming chunks out of an SSE body, ignoring [DONE].
func chunkDeltas(t *testing.T, body string) []openAIStreamChunk {
	t.Helper()
	var out []openAIStreamChunk
	for _, frame := range parseSSE(t, body) {
		if frame.Data == "[DONE]" {
			continue
		}
		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(frame.Data), &chunk); err != nil {
			// Could be an inline error frame; skip it here and let the test that
			// cares assert on it directly.
			continue
		}
		out = append(out, chunk)
	}
	return out
}

// simpleChatBody is a minimal streaming request.
func simpleChatBody(stream bool) string {
	return fmt.Sprintf(`{"model":"claude-opus-5","stream":%t,"messages":[{"role":"user","content":"hello"}]}`, stream)
}

// ---------- streaming tests ----------

func TestStreamFullFrameSequence(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"messageMetadataEvent", `{"conversationId":"c1"}`},
		{"reasoningContentEvent", `{"text":"Let me think."}`},
		{"reasoningContentEvent", `{"text":" Done.","signature":"sig-1"}`},
		{"assistantResponseEvent", `{"content":"Hello, "}`},
		{"assistantResponseEvent", `{"content":"world!"}`},
		{"contextUsageEvent", `{"contextUsagePercentage":1.5}`},
		{"meteringEvent", `{"usage":2.2,"unit":"credit","unitPlural":"credits"}`},
		{"metadataEvent", `{"tokenUsage":{"uncachedInputTokens":10,"outputTokens":5,"totalTokens":15},"stopReason":"end_turn"}`},
	}})
	s := newHarness(t, up, testServerOptions{})

	rec := postChat(t, s, simpleChatBody(true))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q", got)
	}
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no so proxies do not buffer", got)
	}

	body := rec.Body.String()
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("stream must end with the DONE terminator, got tail %q", tail(body, 40))
	}

	chunks := chunkDeltas(t, body)
	if len(chunks) < 4 {
		t.Fatalf("chunks = %d, want at least four: %s", len(chunks), body)
	}

	// The first chunk must announce the role.
	if chunks[0].Choices[0].Delta.Role != "assistant" {
		t.Errorf("first chunk delta role = %q, want assistant", chunks[0].Choices[0].Delta.Role)
	}
	for i, c := range chunks[1:] {
		if c.Choices[0].Delta.Role != "" {
			t.Errorf("chunk %d repeats the role, which clients treat as a new message", i+1)
		}
	}

	// Every chunk shares the same id, object and model.
	for i, c := range chunks {
		if c.ID != chunks[0].ID {
			t.Errorf("chunk %d id = %q, want %q", i, c.ID, chunks[0].ID)
		}
		if !strings.HasPrefix(c.ID, "chatcmpl-") {
			t.Errorf("chunk id = %q, want a chatcmpl- prefix", c.ID)
		}
		if c.Object != "chat.completion.chunk" {
			t.Errorf("chunk %d object = %q", i, c.Object)
		}
		if c.Model != "claude-opus-5" {
			t.Errorf("chunk %d model = %q, want the model as requested", i, c.Model)
		}
		if c.Created == 0 {
			t.Errorf("chunk %d has no created timestamp", i)
		}
	}

	// Reasoning comes before content, in upstream order.
	var order []string
	var reasoning, content, signature string
	for _, c := range chunks {
		d := c.Choices[0].Delta
		if d.ReasoningContent != "" {
			order = append(order, "reasoning")
			reasoning += d.ReasoningContent
		}
		if d.Content != "" {
			order = append(order, "content")
			content += d.Content
		}
		if d.ReasoningSignature != "" {
			signature = d.ReasoningSignature
		}
	}
	if reasoning != "Let me think. Done." {
		t.Errorf("assembled reasoning = %q", reasoning)
	}
	if content != "Hello, world!" {
		t.Errorf("assembled content = %q", content)
	}
	if signature != "sig-1" {
		t.Errorf("reasoning signature = %q, want it streamed so a client can echo it back", signature)
	}
	firstContent := indexOf(order, "content")
	lastReasoning := lastIndexOf(order, "reasoning")
	if firstContent >= 0 && lastReasoning >= 0 && lastReasoning > firstContent {
		t.Errorf("reasoning arrived after content: %v", order)
	}

	// The final chunk carries the finish reason and usage.
	final := chunks[len(chunks)-1]
	if final.Choices[0].FinishReason == nil || *final.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %v, want stop", final.Choices[0].FinishReason)
	}
	if final.Usage == nil {
		t.Fatal("the final chunk must carry usage")
	}
	if final.Usage.PromptTokens != 10 || final.Usage.CompletionTokens != 5 || final.Usage.TotalTokens != 15 {
		t.Errorf("usage = %+v, want the exact upstream counts", final.Usage)
	}
	if final.Choices[0].Delta.Content != "" {
		t.Error("the final chunk delta should be empty")
	}
}

func TestStreamFinishReasonIsNullOnContentChunks(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"assistantResponseEvent", `{"content":"hi"}`},
		{"metadataEvent", `{"tokenUsage":{"outputTokens":1,"totalTokens":2},"stopReason":"end_turn"}`},
	}})
	s := newHarness(t, up, testServerOptions{})
	body := postChat(t, s, simpleChatBody(true)).Body.String()

	frames := parseSSE(t, body)
	for _, f := range frames {
		if f.Data == "[DONE]" {
			continue
		}
		var probe struct {
			Choices []struct {
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(f.Data), &probe); err != nil {
			continue
		}
		if len(probe.Choices) == 0 {
			continue
		}
		// finish_reason must be present as null on non-final chunks, which is what
		// the OpenAI shape specifies.
		if !strings.Contains(f.Data, `"finish_reason"`) {
			t.Errorf("chunk is missing the finish_reason key: %s", f.Data)
		}
	}
}

func TestStreamToolCallsEmittedAsOneChunkAtTheEnd(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"assistantResponseEvent", `{"content":"Looking that up."}`},
		{"toolUseEvent", `{"toolUseId":"tu-1","name":"get_weather","input":"{\"city\":"}`},
		{"toolUseEvent", `{"toolUseId":"tu-1","name":"get_weather","input":"\"Berlin\"}"}`},
		{"toolUseEvent", `{"toolUseId":"tu-1","name":"get_weather","input":"","stop":true}`},
		{"metadataEvent", `{"tokenUsage":{"uncachedInputTokens":5,"outputTokens":3,"totalTokens":8},"stopReason":"tool_use"}`},
	}})
	s := newHarness(t, up, testServerOptions{})

	body := postChat(t, s, `{"model":"claude-opus-5","stream":true,
	  "messages":[{"role":"user","content":"weather?"}],
	  "tools":[{"type":"function","function":{"name":"get_weather","description":"d",
	    "parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`).Body.String()

	chunks := chunkDeltas(t, body)

	var toolChunks int
	var calls []openAIStreamToolCall
	for _, c := range chunks {
		if len(c.Choices[0].Delta.ToolCalls) > 0 {
			toolChunks++
			calls = append(calls, c.Choices[0].Delta.ToolCalls...)
		}
	}
	if toolChunks != 1 {
		t.Errorf("tool calls arrived in %d chunks, want exactly 1 at the end", toolChunks)
	}
	if len(calls) != 1 {
		t.Fatalf("tool calls = %d, want 1 reassembled from three fragments", len(calls))
	}
	call := calls[0]
	if call.ID != "tu-1" || call.Function.Name != "get_weather" {
		t.Errorf("tool call = %+v", call)
	}
	if call.Function.Arguments != `{"city":"Berlin"}` {
		t.Errorf("arguments = %q, want the fragments reassembled and compacted", call.Function.Arguments)
	}
	if call.Type != "function" {
		t.Errorf("type = %q, want function", call.Type)
	}
	if call.Index != 0 {
		t.Errorf("index = %d, want 0", call.Index)
	}

	final := chunks[len(chunks)-1]
	if final.Choices[0].FinishReason == nil || *final.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls", final.Choices[0].FinishReason)
	}
}

func TestStreamToolCallWithInvalidJSONBecomesEmptyObject(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"toolUseEvent", `{"toolUseId":"tu-1","name":"t","input":"{\"broken\":","stop":true}`},
		{"metadataEvent", `{"tokenUsage":{"outputTokens":1,"totalTokens":2},"stopReason":"tool_use"}`},
	}})
	s := newHarness(t, up, testServerOptions{})
	body := postChat(t, s, simpleChatBody(true)).Body.String()

	for _, c := range chunkDeltas(t, body) {
		for _, call := range c.Choices[0].Delta.ToolCalls {
			if call.Function.Arguments != "{}" {
				t.Errorf("arguments = %q, want an empty object for unparseable JSON", call.Function.Arguments)
			}
		}
	}
}

func TestStreamToolCallDeduplication(t *testing.T) {
	cases := []struct {
		name      string
		events    []scriptedEvent
		wantCalls int
		wantArgs  string
	}{
		{
			name: "duplicate id keeps the richer arguments",
			events: []scriptedEvent{
				{"toolUseEvent", `{"toolUseId":"dup","name":"t","input":"{}","stop":true}`},
				{"toolUseEvent", `{"toolUseId":"dup","name":"t","input":"","stop":true}`},
			},
			wantCalls: 1,
			wantArgs:  "{}",
		},
		{
			name: "identical name and arguments under two ids collapse",
			events: []scriptedEvent{
				{"toolUseEvent", `{"toolUseId":"a","name":"t","input":"{\"x\":1}","stop":true}`},
				{"toolUseEvent", `{"toolUseId":"b","name":"t","input":"{\"x\":1}","stop":true}`},
			},
			wantCalls: 1,
			wantArgs:  `{"x":1}`,
		},
		{
			name: "different arguments are kept",
			events: []scriptedEvent{
				{"toolUseEvent", `{"toolUseId":"a","name":"t","input":"{\"x\":1}","stop":true}`},
				{"toolUseEvent", `{"toolUseId":"b","name":"t","input":"{\"x\":2}","stop":true}`},
			},
			wantCalls: 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := append(tc.events, scriptedEvent{"metadataEvent",
				`{"tokenUsage":{"outputTokens":1,"totalTokens":2},"stopReason":"tool_use"}`})
			up := newFakeUpstream(t, upstreamScript{Events: events})
			s := newHarness(t, up, testServerOptions{})
			body := postChat(t, s, simpleChatBody(true)).Body.String()

			var calls []openAIStreamToolCall
			for _, c := range chunkDeltas(t, body) {
				calls = append(calls, c.Choices[0].Delta.ToolCalls...)
			}
			if len(calls) != tc.wantCalls {
				t.Fatalf("tool calls = %d, want %d: %+v", len(calls), tc.wantCalls, calls)
			}
			if tc.wantArgs != "" && calls[0].Function.Arguments != tc.wantArgs {
				t.Errorf("arguments = %q, want %q", calls[0].Function.Arguments, tc.wantArgs)
			}
			// Indices must be contiguous from zero.
			for i, c := range calls {
				if c.Index != i {
					t.Errorf("call %d has index %d", i, c.Index)
				}
			}
		})
	}
}

func TestStreamFirstTokenTimeoutRetriesThenFails(t *testing.T) {
	// Every attempt stalls past the budget.
	up := newFakeUpstream(t, upstreamScript{
		DelayBeforeFirst: 2 * time.Second,
		Events:           []scriptedEvent{{"assistantResponseEvent", `{"content":"too late"}`}},
	})
	s := newHarness(t, up, testServerOptions{
		FirstTokenTimeout:    80 * time.Millisecond,
		FirstTokenMaxRetries: 3,
	})

	rec := postChat(t, s, simpleChatBody(true))
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504: %s", rec.Code, rec.Body.String())
	}
	if n := up.RequestCount(); n != 3 {
		t.Errorf("upstream attempts = %d, want exactly the configured 3", n)
	}
	// The failure must be a clean HTTP error, not an SSE stream.
	if strings.Contains(rec.Body.String(), "data:") {
		t.Errorf("a pre-stream failure must not be delivered as SSE: %s", rec.Body.String())
	}
	var body openAIError
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not JSON: %v", err)
	}
	if !strings.Contains(body.Error.Message, "no output") {
		t.Errorf("message = %q, should explain the model never started", body.Error.Message)
	}
}

func TestStreamFirstTokenTimeoutRecoversOnRetry(t *testing.T) {
	up := newFakeUpstream(t,
		upstreamScript{DelayBeforeFirst: time.Second,
			Events: []scriptedEvent{{"assistantResponseEvent", `{"content":"slow"}`}}},
		upstreamScript{Events: []scriptedEvent{
			{"assistantResponseEvent", `{"content":"fast"}`},
			{"metadataEvent", `{"tokenUsage":{"outputTokens":1,"totalTokens":2},"stopReason":"end_turn"}`},
		}},
	)
	s := newHarness(t, up, testServerOptions{
		FirstTokenTimeout:    80 * time.Millisecond,
		FirstTokenMaxRetries: 3,
	})

	rec := postChat(t, s, simpleChatBody(true))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var content string
	for _, c := range chunkDeltas(t, rec.Body.String()) {
		content += c.Choices[0].Delta.Content
	}
	if content != "fast" {
		t.Errorf("content = %q, want the retry's output", content)
	}
	if n := up.RequestCount(); n != 2 {
		t.Errorf("upstream attempts = %d, want 2", n)
	}
}

func TestStreamIsNotRestartedAfterTheFirstByte(t *testing.T) {
	// The first event arrives promptly, then the upstream stalls and the
	// connection is cut. No retry may happen, because bytes already reached the
	// client.
	up := newFakeUpstream(t, upstreamScript{
		Events: []scriptedEvent{
			{"assistantResponseEvent", `{"content":"partial"}`},
			{"assistantResponseEvent", `{"content":" more"}`},
		},
		TruncateAfter: 1,
	})
	s := newHarness(t, up, testServerOptions{
		FirstTokenTimeout:    80 * time.Millisecond,
		FirstTokenMaxRetries: 3,
	})

	rec := postChat(t, s, simpleChatBody(true))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if n := up.RequestCount(); n != 1 {
		t.Errorf("upstream attempts = %d, want 1: a partially delivered stream must never restart", n)
	}
	var content string
	for _, c := range chunkDeltas(t, rec.Body.String()) {
		content += c.Choices[0].Delta.Content
	}
	if content != "partial" {
		t.Errorf("content = %q, want what actually arrived", content)
	}
}

func TestStreamUpstreamErrorBeforeFirstByteIsACleanHTTPError(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantStatus int
		wantText   string
	}{
		{"context limit", http.StatusBadRequest,
			`{"message":"Input is too long.","reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`,
			http.StatusRequestEntityTooLarge, "Context limit reached"},
		{"invalid model", http.StatusBadRequest,
			`{"message":"bad model","reason":"INVALID_MODEL_ID"}`,
			http.StatusBadRequest, "Invalid model"},
		{"monthly quota", http.StatusBadRequest,
			`{"message":"quota","reason":"MONTHLY_REQUEST_COUNT"}`,
			http.StatusTooManyRequests, "Monthly request limit"},
		{"improperly formed", http.StatusBadRequest,
			`{"message":"Improperly formed request."}`,
			http.StatusBadRequest, "LOG_LEVEL=DEBUG"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newFakeUpstream(t, upstreamScript{Status: tc.status, ErrorBody: tc.body})
			s := newHarness(t, up, testServerOptions{})

			rec := postChat(t, s, simpleChatBody(true))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "data:") {
				t.Error("a pre-stream failure must not be delivered as SSE")
			}
			var body openAIError
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("not JSON: %v", err)
			}
			if !strings.Contains(body.Error.Message, tc.wantText) {
				t.Errorf("message = %q, want it to contain %q", body.Error.Message, tc.wantText)
			}
		})
	}
}

func TestStreamMidStreamExceptionIsDeliveredInline(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"assistantResponseEvent", `{"content":"starting"}`},
		{"ThrottlingException", `{"message":"Too many requests","retryAfterMilliseconds":500}`},
	}})
	s := newHarness(t, up, testServerOptions{})

	rec := postChat(t, s, simpleChatBody(true))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: the stream had already begun", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"error"`) {
		t.Errorf("a mid-stream error must be delivered inside the stream: %s", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("the stream should still terminate cleanly, tail = %q", tail(body, 40))
	}
}

func TestStreamTruncationDetection(t *testing.T) {
	t.Run("content with no accounting is treated as truncated", func(t *testing.T) {
		up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
			{"assistantResponseEvent", `{"content":"this response was cut"}`},
		}})
		s := newHarness(t, up, testServerOptions{})
		body := postChat(t, s, simpleChatBody(true)).Body.String()

		chunks := chunkDeltas(t, body)
		final := chunks[len(chunks)-1]
		if final.Choices[0].FinishReason == nil || *final.Choices[0].FinishReason != "length" {
			t.Errorf("finish_reason = %v, want length so the client can react",
				final.Choices[0].FinishReason)
		}
	})

	t.Run("usage present means not truncated", func(t *testing.T) {
		up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
			{"assistantResponseEvent", `{"content":"complete"}`},
			{"metadataEvent", `{"tokenUsage":{"outputTokens":1,"totalTokens":2},"stopReason":"end_turn"}`},
		}})
		s := newHarness(t, up, testServerOptions{})
		chunks := chunkDeltas(t, postChat(t, s, simpleChatBody(true)).Body.String())
		final := chunks[len(chunks)-1]
		if *final.Choices[0].FinishReason != "stop" {
			t.Errorf("finish_reason = %q, want stop", *final.Choices[0].FinishReason)
		}
	})

	t.Run("context usage alone means not truncated", func(t *testing.T) {
		up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
			{"assistantResponseEvent", `{"content":"complete"}`},
			{"contextUsageEvent", `{"contextUsagePercentage":5}`},
		}})
		s := newHarness(t, up, testServerOptions{})
		chunks := chunkDeltas(t, postChat(t, s, simpleChatBody(true)).Body.String())
		final := chunks[len(chunks)-1]
		if *final.Choices[0].FinishReason != "stop" {
			t.Errorf("finish_reason = %q, want stop", *final.Choices[0].FinishReason)
		}
	})

	t.Run("tool calls without accounting are not truncated", func(t *testing.T) {
		up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
			{"assistantResponseEvent", `{"content":"calling"}`},
			{"toolUseEvent", `{"toolUseId":"t","name":"n","input":"{}","stop":true}`},
		}})
		s := newHarness(t, up, testServerOptions{})
		chunks := chunkDeltas(t, postChat(t, s, simpleChatBody(true)).Body.String())
		final := chunks[len(chunks)-1]
		if *final.Choices[0].FinishReason != "tool_calls" {
			t.Errorf("finish_reason = %q, want tool_calls", *final.Choices[0].FinishReason)
		}
	})
}

func TestStreamStopReasonMapping(t *testing.T) {
	cases := map[string]string{
		"end_turn":         "stop",
		"stop_sequence":    "stop",
		"max_tokens":       "length",
		"length":           "length",
		"tool_use":         "tool_calls",
		"content_filtered": "content_filter",
		"something_new":    "stop",
		"":                 "stop",
	}
	for upstream, want := range cases {
		t.Run("stopReason="+upstream, func(t *testing.T) {
			payload := fmt.Sprintf(
				`{"tokenUsage":{"uncachedInputTokens":1,"outputTokens":1,"totalTokens":2},"stopReason":%q}`, upstream)
			up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
				{"assistantResponseEvent", `{"content":"text"}`},
				{"metadataEvent", payload},
			}})
			s := newHarness(t, up, testServerOptions{})
			chunks := chunkDeltas(t, postChat(t, s, simpleChatBody(true)).Body.String())
			final := chunks[len(chunks)-1]
			if final.Choices[0].FinishReason == nil {
				t.Fatal("no finish reason")
			}
			if *final.Choices[0].FinishReason != want {
				t.Errorf("finish_reason = %q, want %q", *final.Choices[0].FinishReason, want)
			}
		})
	}
}

func TestStreamUsageIncludesCacheAndCredits(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"assistantResponseEvent", `{"content":"hi"}`},
		{"meteringEvent", `{"usage":2.2,"unit":"credit","unitPlural":"credits"}`},
		{"metadataEvent", `{"tokenUsage":{"uncachedInputTokens":100,"outputTokens":42,"totalTokens":700,
		  "cacheReadInputTokens":500,"cacheWriteInputTokens":58,"contextUsagePercentage":12.5}}`},
	}})
	s := newHarness(t, up, testServerOptions{})
	chunks := chunkDeltas(t, postChat(t, s, simpleChatBody(true)).Body.String())

	usage := chunks[len(chunks)-1].Usage
	if usage == nil {
		t.Fatal("no usage")
	}
	if usage.PromptTokens != 658 {
		t.Errorf("prompt_tokens = %d, want 658 (100 uncached + 500 read + 58 write)", usage.PromptTokens)
	}
	if usage.CompletionTokens != 42 {
		t.Errorf("completion_tokens = %d, want 42", usage.CompletionTokens)
	}
	if usage.TotalTokens != 700 {
		t.Errorf("total_tokens = %d, want the reported 700", usage.TotalTokens)
	}
	if usage.CacheReadInputTokens != 500 || usage.CacheWriteInputTokens != 58 {
		t.Errorf("cache tokens = %d / %d", usage.CacheReadInputTokens, usage.CacheWriteInputTokens)
	}
	if usage.ContextUsagePercentage == nil || *usage.ContextUsagePercentage != 12.5 {
		t.Errorf("context_usage_percentage = %v", usage.ContextUsagePercentage)
	}
	if usage.CreditsUsed == nil || *usage.CreditsUsed != 2.2 {
		t.Errorf("credits_used = %v, want 2.2", usage.CreditsUsed)
	}
	if usage.CreditUnit != "credit" {
		t.Errorf("credit_unit = %q", usage.CreditUnit)
	}
	if usage.Estimated {
		t.Error("usage from upstream counts must not be marked estimated")
	}
}

func TestStreamUsageFallbackFromContextPercentage(t *testing.T) {
	// No metadata event, but a context usage percentage against a 1M window.
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"assistantResponseEvent", `{"content":"some output text"}`},
		{"contextUsageEvent", `{"contextUsagePercentage":10}`},
	}})
	s := newHarness(t, up, testServerOptions{})
	chunks := chunkDeltas(t, postChat(t, s, simpleChatBody(true)).Body.String())

	usage := chunks[len(chunks)-1].Usage
	if usage == nil {
		t.Fatal("no usage")
	}
	if !usage.Estimated {
		t.Error("a derived usage must be marked estimated")
	}
	// 10 percent of 1,000,000 is 100,000.
	if usage.TotalTokens != 100000 {
		t.Errorf("total_tokens = %d, want 100000 derived from the context percentage", usage.TotalTokens)
	}
	if usage.PromptTokens != usage.TotalTokens-usage.CompletionTokens {
		t.Errorf("prompt tokens should be the remainder: %+v", usage)
	}
}

func TestStreamUsageFallbackToEstimator(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"assistantResponseEvent", `{"content":"output"}`},
	}})
	s := newHarness(t, up, testServerOptions{})
	chunks := chunkDeltas(t, postChat(t, s, simpleChatBody(true)).Body.String())

	usage := chunks[len(chunks)-1].Usage
	if usage == nil {
		t.Fatal("no usage")
	}
	if !usage.Estimated {
		t.Error("a fully estimated usage must say so")
	}
	if usage.PromptTokens <= 0 {
		t.Errorf("prompt_tokens = %d, want a positive estimate", usage.PromptTokens)
	}
	if usage.TotalTokens != usage.PromptTokens+usage.CompletionTokens {
		t.Errorf("total should be the sum: %+v", usage)
	}
}

func TestStreamClientDisconnectReleasesTheUpstreamConnection(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{
		Events: []scriptedEvent{
			{"assistantResponseEvent", `{"content":"one"}`},
			{"assistantResponseEvent", `{"content":"two"}`},
			{"assistantResponseEvent", `{"content":"three"}`},
		},
		DelayBetween: 60 * time.Millisecond,
	})
	s := newHarness(t, up, testServerOptions{})

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(simpleChatBody(true))).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer test-key")

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Handler().ServeHTTP(rec, req)
	}()

	time.Sleep(80 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// The handler must return promptly rather than draining the whole stream.
	case <-time.After(3 * time.Second):
		t.Fatal("the handler did not return after the client disconnected")
	}
}

func TestStreamReasoningOnlyResponse(t *testing.T) {
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"reasoningContentEvent", `{"text":"thinking","signature":"s"}`},
		{"metadataEvent", `{"tokenUsage":{"outputTokens":1,"totalTokens":2},"stopReason":"end_turn"}`},
	}})
	s := newHarness(t, up, testServerOptions{})
	rec := postChat(t, s, simpleChatBody(true))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var reasoning string
	for _, c := range chunkDeltas(t, rec.Body.String()) {
		reasoning += c.Choices[0].Delta.ReasoningContent
	}
	if reasoning != "thinking" {
		t.Errorf("reasoning = %q", reasoning)
	}
}

func TestStreamRedactedReasoningIsBase64(t *testing.T) {
	blob := base64.StdEncoding.EncodeToString([]byte{1, 2, 3})
	up := newFakeUpstream(t, upstreamScript{Events: []scriptedEvent{
		{"reasoningContentEvent", `{"redactedContent":"` + blob + `"}`},
		{"assistantResponseEvent", `{"content":"answer"}`},
		{"metadataEvent", `{"tokenUsage":{"outputTokens":1,"totalTokens":2},"stopReason":"end_turn"}`},
	}})
	s := newHarness(t, up, testServerOptions{})

	var got string
	for _, c := range chunkDeltas(t, postChat(t, s, simpleChatBody(true)).Body.String()) {
		if c.Choices[0].Delta.ReasoningRedactedContent != "" {
			got = c.Choices[0].Delta.ReasoningRedactedContent
		}
	}
	if got != blob {
		t.Errorf("redacted reasoning = %q, want the base64 blob %q", got, blob)
	}
}

// ---------- helpers ----------

// tail returns the last n characters of s.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// indexOf returns the first index of v, or -1.
func indexOf(list []string, v string) int {
	for i, s := range list {
		if s == v {
			return i
		}
	}
	return -1
}

// lastIndexOf returns the last index of v, or -1.
func lastIndexOf(list []string, v string) int {
	for i := len(list) - 1; i >= 0; i-- {
		if list[i] == v {
			return i
		}
	}
	return -1
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.

package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"kirogo/internal/kiro"
	"kirogo/internal/util"
)

// openAIStreamDelta is the incremental payload in a streaming chunk.
type openAIStreamDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`

	ReasoningContent         string `json:"reasoning_content,omitempty"`
	ReasoningSignature       string `json:"reasoning_signature,omitempty"`
	ReasoningRedactedContent string `json:"reasoning_redacted_content,omitempty"`

	ToolCalls []openAIStreamToolCall `json:"tool_calls,omitempty"`
}

// openAIStreamToolCall is a tool call inside a streaming delta. Unlike the
// non-streaming shape it carries an index.
type openAIStreamToolCall struct {
	Index    int                `json:"index"`
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIToolFunction `json:"function"`
}

// openAIStreamChoice is one choice in a streaming chunk.
type openAIStreamChoice struct {
	Index        int               `json:"index"`
	Delta        openAIStreamDelta `json:"delta"`
	FinishReason *string           `json:"finish_reason"`
}

// openAIStreamChunk is one server-sent event payload.
type openAIStreamChunk struct {
	ID      string               `json:"id"`
	Object  string               `json:"object"`
	Created int64                `json:"created"`
	Model   string               `json:"model"`
	Choices []openAIStreamChoice `json:"choices"`
	Usage   *openAIUsage         `json:"usage,omitempty"`
}

// sseWriter emits server-sent events, flushing after every frame.
//
// Flushing per frame is what makes a response feel streamed: without it the
// framework buffers, and a client sees nothing until the response ends.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	// failed records a write error so later writes stop trying.
	failed bool
}

// newSSEWriter sets the streaming headers and returns a writer.
func newSSEWriter(w http.ResponseWriter) (*sseWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("this HTTP server cannot stream responses")
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Without this, an nginx or similar in front of kirogo buffers the stream.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return &sseWriter{w: w, flusher: flusher}, nil
}

// writeData emits one "data:" frame containing compact JSON.
func (s *sseWriter) writeData(payload any) {
	if s.failed {
		return
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		slog.Error("could not encode a streaming chunk", "error", err.Error())
		return
	}
	s.writeRaw("data: " + string(encoded) + "\n\n")
}

// writeEvent emits a named event frame, which the Anthropic surface needs.
func (s *sseWriter) writeEvent(name string, payload any) {
	if s.failed {
		return
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		slog.Error("could not encode a streaming event", "event", name, "error", err.Error())
		return
	}
	s.writeRaw("event: " + name + "\ndata: " + string(encoded) + "\n\n")
}

// writeRaw writes a prepared frame and flushes it.
func (s *sseWriter) writeRaw(frame string) {
	if s.failed {
		return
	}
	if _, err := s.w.Write([]byte(frame)); err != nil {
		// A write failure means the client is gone. Recording it stops a flood of
		// identical errors for the rest of the stream.
		s.failed = true
		slog.Debug("stopped writing: the client connection is gone", "error", err.Error())
		return
	}
	s.flusher.Flush()
}

// done emits the OpenAI terminator.
func (s *sseWriter) done() {
	s.writeRaw("data: [DONE]\n\n")
}

// streamOpenAI serves a streaming completion.
func (s *Server) streamOpenAI(w http.ResponseWriter, r *http.Request, prepared *preparedRequest) {
	// The upstream request runs first: a failure before any byte is written can
	// still be reported as a normal HTTP error, which is far more useful to a
	// client than an error smuggled inside a 200 stream.
	sess, buffered, err := s.openStream(r.Context(), prepared.kiro)
	if err != nil {
		writeUpstreamError(w, flavorOpenAI, err)
		return
	}
	defer sess.close()

	sse, err := newSSEWriter(w)
	if err != nil {
		writeError(w, flavorOpenAI, http.StatusInternalServerError, err.Error())
		return
	}

	completionID := util.CompletionID()
	created := time.Now().Unix()
	roleSent := false

	chunk := func(delta openAIStreamDelta) {
		if !roleSent {
			delta.Role = "assistant"
			roleSent = true
		}
		sse.writeData(openAIStreamChunk{
			ID:      completionID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   prepared.ir.Model,
			Choices: []openAIStreamChoice{{Index: 0, Delta: delta}},
		})
	}

	c := newCollected(prepared.ir.MaxTokens, len(prepared.ir.Tools) > 0)
	signatureSent := false

	// emit turns one event into client-visible frames, returning what the output
	// budget admitted so the caller knows when to stop.
	emit := func(item eventOrError) accepted {
		acc := c.apply(item.event)
		e := item.event

		switch e.Kind {
		case kiro.EventReasoningContent:
			// Reasoning is emitted before content, preserving upstream order.
			// Only the admitted part goes out, so the frames a client receives and
			// the tokens reported for them stay in step.
			if acc.Reasoning != "" {
				chunk(openAIStreamDelta{ReasoningContent: acc.Reasoning})
			}
			if e.Reasoning.Signature != "" && !signatureSent {
				// The signature is sent as soon as it arrives so a client that
				// stores deltas can echo it back on the next turn.
				chunk(openAIStreamDelta{ReasoningSignature: e.Reasoning.Signature})
				signatureSent = true
			}
			if len(e.Reasoning.RedactedContent) > 0 {
				chunk(openAIStreamDelta{
					ReasoningRedactedContent: base64.StdEncoding.EncodeToString(e.Reasoning.RedactedContent),
				})
			}

		case kiro.EventAssistantResponse:
			if acc.Content != "" {
				chunk(openAIStreamDelta{Content: acc.Content})
			}
		}
		return acc
	}

	for _, e := range buffered {
		emit(eventOrError{event: e})
	}

	streamFailed := false
	released := c.stopReadingEarly()
	if !released {
		for item := range sess.events {
			if item.err != nil {
				slog.Error("the upstream stream failed partway through", "error", item.err.Error())
				streamFailed = true
				break
			}
			emit(item)
			if r.Context().Err() != nil {
				// IDEs cancel constantly. A disconnect is ordinary, not a fault.
				slog.Debug("the client disconnected mid-stream, closing the upstream connection")
				return
			}
			if c.stopReadingEarly() {
				released = true
				break
			}
		}
	}
	if c.budget.exhausted {
		logOutputLimit(c, released)
	}

	if c.exception != nil {
		// The stream already returned 200, so the error has to travel inside it.
		apiErr := apiErrorFromException(c.exception)
		slog.Error("the Kiro backend reported an error mid-stream", "error", apiErr.Error())
		sse.writeData(map[string]any{
			"error": map[string]any{
				"message": apiErr.UserMessage(),
				"type":    errorTypeFor(flavorOpenAI, apiErr.ClientStatus()),
			},
		})
		sse.done()
		return
	}

	toolCalls := c.tools.finish()

	// Tool calls go out as a single chunk at the end of the stream, matching the
	// reference gateway. Emitting partial argument fragments invites clients to
	// parse incomplete JSON.
	if len(toolCalls) > 0 {
		delta := openAIStreamDelta{}
		for i, tc := range toolCalls {
			delta.ToolCalls = append(delta.ToolCalls, openAIStreamToolCall{
				Index:    i,
				ID:       tc.ID,
				Type:     "function",
				Function: openAIToolFunction{Name: tc.Name, Arguments: tc.Arguments},
			})
		}
		chunk(delta)
	}

	truncated := c.truncated()
	if truncated {
		logTruncation(c)
	}

	finishReason := finishReasonFor(c, toolCalls, truncated)
	if streamFailed && c.content.Len() > 0 {
		// The connection broke with content already delivered, which is the same
		// outcome for the client as a truncated response.
		finishReason = "length"
	}

	usage := c.usageReport(prepared.promptEstimate, prepared.maxInputTokens)
	sse.writeData(openAIStreamChunk{
		ID:      completionID,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   prepared.ir.Model,
		Choices: []openAIStreamChoice{{
			Index:        0,
			Delta:        openAIStreamDelta{},
			FinishReason: &finishReason,
		}},
		Usage: buildOpenAIUsage(usage),
	})
	sse.done()
}

// encodeBase64 encodes a blob for a response field.
func encodeBase64(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.

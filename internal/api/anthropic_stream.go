package api

import (
	"encoding/base64"
	"log/slog"
	"net/http"

	"kirogo/internal/kiro"
	"kirogo/internal/util"
)

// Anthropic stream event names.
const (
	evtMessageStart      = "message_start"
	evtContentBlockStart = "content_block_start"
	evtContentBlockDelta = "content_block_delta"
	evtContentBlockStop  = "content_block_stop"
	evtMessageDelta      = "message_delta"
	evtMessageStop       = "message_stop"
	evtPing              = "ping"
	evtError             = "error"
)

// blockKind identifies which sort of content block is currently open.
type blockKind int

const (
	blockNone blockKind = iota
	blockThinking
	blockText
	blockToolUse
)

// anthropicBlockWriter emits content blocks with correctly incrementing indices.
//
// Anthropic numbers every block in one sequence, whatever its type, and every
// opened block must be closed before the next one opens. Getting the indices
// wrong makes a client attribute deltas to the wrong block.
type anthropicBlockWriter struct {
	sse   *sseWriter
	index int
	open  blockKind
	// pendingSignature holds a reasoning signature until the thinking block is
	// about to close, since a signature_delta only makes sense inside it.
	pendingSignature string
	// signatureSent records that the signature has already gone out.
	signatureSent bool
}

// ensure opens a block of the given kind, closing any other kind first.
func (b *anthropicBlockWriter) ensure(kind blockKind, block map[string]any) {
	if b.open == kind {
		return
	}
	b.closeCurrent()
	b.sse.writeEvent(evtContentBlockStart, map[string]any{
		"type":          evtContentBlockStart,
		"index":         b.index,
		"content_block": block,
	})
	b.open = kind
}

// closeCurrent closes the open block, if any, and advances the index.
func (b *anthropicBlockWriter) closeCurrent() {
	if b.open == blockNone {
		return
	}
	// A signature belongs inside the thinking block it signs, so it is flushed
	// immediately before that block closes.
	if b.open == blockThinking && b.pendingSignature != "" && !b.signatureSent {
		b.sse.writeEvent(evtContentBlockDelta, map[string]any{
			"type":  evtContentBlockDelta,
			"index": b.index,
			"delta": map[string]any{"type": "signature_delta", "signature": b.pendingSignature},
		})
		b.signatureSent = true
	}
	b.sse.writeEvent(evtContentBlockStop, map[string]any{
		"type":  evtContentBlockStop,
		"index": b.index,
	})
	b.index++
	b.open = blockNone
}

// delta emits a delta for the open block.
func (b *anthropicBlockWriter) delta(payload map[string]any) {
	b.sse.writeEvent(evtContentBlockDelta, map[string]any{
		"type":  evtContentBlockDelta,
		"index": b.index,
		"delta": payload,
	})
}

// thinking emits a thinking delta, opening the block if needed.
func (b *anthropicBlockWriter) thinking(text string) {
	b.ensure(blockThinking, map[string]any{"type": "thinking", "thinking": ""})
	b.delta(map[string]any{"type": "thinking_delta", "thinking": text})
}

// signature records a reasoning signature to emit before the block closes.
func (b *anthropicBlockWriter) signature(sig string) {
	if sig == "" || b.signatureSent {
		return
	}
	b.pendingSignature = sig
	// If the thinking block is still open, sending it now is both valid and more
	// useful, because a client can store it as soon as it arrives.
	if b.open == blockThinking {
		b.delta(map[string]any{"type": "signature_delta", "signature": sig})
		b.signatureSent = true
	}
}

// redactedThinking emits a redacted thinking block as a self-contained unit.
func (b *anthropicBlockWriter) redactedThinking(blob []byte) {
	b.closeCurrent()
	b.sse.writeEvent(evtContentBlockStart, map[string]any{
		"type":  evtContentBlockStart,
		"index": b.index,
		"content_block": map[string]any{
			"type": "redacted_thinking",
			"data": base64.StdEncoding.EncodeToString(blob),
		},
	})
	b.open = blockThinking
	b.closeCurrent()
}

// text emits a text delta, opening the block if needed.
func (b *anthropicBlockWriter) text(chunk string) {
	b.ensure(blockText, map[string]any{"type": "text", "text": ""})
	b.delta(map[string]any{"type": "text_delta", "text": chunk})
}

// toolUse emits a complete tool_use block.
//
// The arguments are already fully assembled, so they go out as one
// input_json_delta rather than as fragments a client would have to buffer.
func (b *anthropicBlockWriter) toolUse(call FinishedToolCall) {
	b.closeCurrent()
	b.sse.writeEvent(evtContentBlockStart, map[string]any{
		"type":  evtContentBlockStart,
		"index": b.index,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    toolUseID(call.ID),
			"name":  call.Name,
			"input": map[string]any{},
		},
	})
	b.open = blockToolUse
	b.delta(map[string]any{"type": "input_json_delta", "partial_json": call.Arguments})
	b.closeCurrent()
}

// streamAnthropic serves a streaming message.
func (s *Server) streamAnthropic(w http.ResponseWriter, r *http.Request, prepared *preparedRequest) {
	sess, buffered, err := s.openStream(r.Context(), prepared.kiro)
	if err != nil {
		writeUpstreamError(w, flavorAnthropic, err)
		return
	}
	defer sess.close()

	sse, err := newSSEWriter(w)
	if err != nil {
		writeError(w, flavorAnthropic, http.StatusInternalServerError, err.Error())
		return
	}

	messageID := util.MessageID()
	c := newCollected(prepared.ir.MaxTokens, len(prepared.ir.Tools) > 0)
	blocks := &anthropicBlockWriter{sse: sse}

	// message_start carries the input token count. Without upstream usage yet, the
	// estimate is the only number available, and clients display it immediately.
	sse.writeEvent(evtMessageStart, map[string]any{
		"type": evtMessageStart,
		"message": map[string]any{
			"id":            messageID,
			"type":          "message",
			"role":          "assistant",
			"model":         prepared.ir.Model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  prepared.promptEstimate,
				"output_tokens": 0,
			},
		},
	})

	// Only the part the output budget admits is emitted, so the blocks a client
	// receives match the token count reported at the end of the stream.
	emit := func(e *kiro.Event) {
		acc := c.apply(e)
		switch e.Kind {
		case kiro.EventReasoningContent:
			if acc.Reasoning != "" {
				blocks.thinking(acc.Reasoning)
			}
			if e.Reasoning.Signature != "" {
				blocks.signature(e.Reasoning.Signature)
			}
			if len(e.Reasoning.RedactedContent) > 0 {
				blocks.redactedThinking(e.Reasoning.RedactedContent)
			}
		case kiro.EventAssistantResponse:
			if acc.Content != "" {
				blocks.text(acc.Content)
			}
		}
	}

	for _, e := range buffered {
		emit(e)
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
			emit(item.event)
			if r.Context().Err() != nil {
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
		// The response is already a 200 stream, so the error travels inside it as
		// an error event, which is the shape Anthropic clients expect.
		apiErr := apiErrorFromException(c.exception)
		slog.Error("the Kiro backend reported an error mid-stream", "error", apiErr.Error())
		blocks.closeCurrent()
		sse.writeEvent(evtError, map[string]any{
			"type": evtError,
			"error": map[string]any{
				"type":    errorTypeFor(flavorAnthropic, apiErr.ClientStatus()),
				"message": apiErr.UserMessage(),
			},
		})
		return
	}

	toolCalls := c.tools.finish()
	for _, call := range toolCalls {
		blocks.toolUse(call)
	}
	blocks.closeCurrent()

	truncated := c.truncated()
	if truncated {
		logTruncation(c)
	}
	stopReason := anthropicStopReasonFor(c, toolCalls, truncated)
	if streamFailed && c.content.Len() > 0 {
		stopReason = "max_tokens"
	}

	usage := c.usageReport(prepared.promptEstimate, prepared.maxInputTokens)
	sse.writeEvent(evtMessageDelta, map[string]any{
		"type": evtMessageDelta,
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": buildAnthropicUsage(usage),
	})
	sse.writeEvent(evtMessageStop, map[string]any{"type": evtMessageStop})
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.

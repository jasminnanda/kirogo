package kiro

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

// eventFrame builds a well-formed event frame for a given event type and payload.
func eventFrame(t *testing.T, eventType, payload string) []byte {
	t.Helper()
	frame, err := EncodeMessage([]Header{
		StringHeader(":message-type", "event"),
		StringHeader(":event-type", eventType),
		StringHeader(":content-type", "application/json"),
	}, []byte(payload))
	if err != nil {
		t.Fatalf("EncodeMessage: %v", err)
	}
	return frame
}

// decodeAll feeds the whole buffer at once and returns every message.
func decodeAll(t *testing.T, data []byte) []*Message {
	t.Helper()
	d := NewDecoder()
	if _, err := d.Write(data); err != nil {
		t.Fatalf("Write: %v", err)
	}
	var out []*Message
	for {
		msg, err := d.Next()
		if errors.Is(err, ErrIncomplete) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, msg)
	}
	return out
}

// decodeByteAtATime feeds one byte per call, which is the adversarial case for an
// incremental decoder.
func decodeByteAtATime(t *testing.T, data []byte) []*Message {
	t.Helper()
	d := NewDecoder()
	var out []*Message
	for i := 0; i < len(data); i++ {
		if _, err := d.Write(data[i : i+1]); err != nil {
			t.Fatalf("Write: %v", err)
		}
		for {
			msg, err := d.Next()
			if errors.Is(err, ErrIncomplete) {
				break
			}
			if err != nil {
				t.Fatalf("Next at byte %d: %v", i, err)
			}
			out = append(out, msg)
		}
	}
	return out
}

func TestFrameLayoutIsExact(t *testing.T) {
	payload := `{"content":"hi"}`
	frame := eventFrame(t, "assistantResponseEvent", payload)

	totalLength := binary.BigEndian.Uint32(frame[0:4])
	headersLength := binary.BigEndian.Uint32(frame[4:8])
	preludeCRC := binary.BigEndian.Uint32(frame[8:12])

	if int(totalLength) != len(frame) {
		t.Errorf("total_length = %d, want %d (the whole frame)", totalLength, len(frame))
	}
	if want := crc32.ChecksumIEEE(frame[0:8]); preludeCRC != want {
		t.Errorf("prelude_crc = %#08x, want the CRC32 of the first 8 bytes (%#08x)", preludeCRC, want)
	}
	messageCRC := binary.BigEndian.Uint32(frame[len(frame)-4:])
	if want := crc32.ChecksumIEEE(frame[:len(frame)-4]); messageCRC != want {
		t.Errorf("message_crc = %#08x, want the CRC32 of everything up to the payload end (%#08x)", messageCRC, want)
	}
	if got := int(totalLength) - int(headersLength) - 16; got != len(payload) {
		t.Errorf("derived payload length = %d, want %d", got, len(payload))
	}
}

func TestDecodeSingleEvent(t *testing.T) {
	frame := eventFrame(t, "assistantResponseEvent", `{"content":"Hello","modelId":"claude-opus-5"}`)
	msgs := decodeAll(t, frame)

	if len(msgs) != 1 {
		t.Fatalf("decoded %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	if m.MessageType() != "event" {
		t.Errorf("MessageType() = %q, want event", m.MessageType())
	}
	if m.EventType() != "assistantResponseEvent" {
		t.Errorf("EventType() = %q", m.EventType())
	}
	if string(m.Payload) != `{"content":"Hello","modelId":"claude-opus-5"}` {
		t.Errorf("Payload = %s", m.Payload)
	}
	if _, ok := m.Header(":content-type"); !ok {
		t.Error(":content-type header was lost")
	}
	if _, ok := m.Header(":absent"); ok {
		t.Error("Header reported a missing header as present")
	}
}

func TestByteAtATimeMatchesWholeBuffer(t *testing.T) {
	// A realistic sequence covering every consumed event type.
	var stream []byte
	fixtures := []struct{ eventType, payload string }{
		{"messageMetadataEvent", `{"conversationId":"abc"}`},
		{"reasoningContentEvent", `{"text":"Let me think","signature":"sig-1"}`},
		{"reasoningContentEvent", `{"text":" harder","signature":"sig-2"}`},
		{"assistantResponseEvent", `{"content":"Hello, "}`},
		{"assistantResponseEvent", `{"content":"world"}`},
		{"toolUseEvent", `{"toolUseId":"tu-1","name":"get_weather","input":"{\"city\":"}`},
		{"toolUseEvent", `{"toolUseId":"tu-1","name":"get_weather","input":"\"Berlin\"}","stop":true}`},
		{"contextUsageEvent", `{"contextUsagePercentage":2.86}`},
		{"meteringEvent", `{"usage":2.2,"unit":"credit","unitPlural":"credits"}`},
		{"metadataEvent", `{"tokenUsage":{"uncachedInputTokens":10,"outputTokens":5,"totalTokens":15},"stopReason":"end_turn"}`},
	}
	for _, f := range fixtures {
		stream = append(stream, eventFrame(t, f.eventType, f.payload)...)
	}

	whole := decodeAll(t, stream)
	drip := decodeByteAtATime(t, stream)

	if len(whole) != len(fixtures) {
		t.Fatalf("whole-buffer decode produced %d messages, want %d", len(whole), len(fixtures))
	}
	if len(drip) != len(whole) {
		t.Fatalf("byte-at-a-time produced %d messages, whole-buffer produced %d", len(drip), len(whole))
	}
	for i := range whole {
		if !reflect.DeepEqual(whole[i].Headers, drip[i].Headers) {
			t.Errorf("message %d headers differ:\n whole: %v\n  drip: %v", i, whole[i].Headers, drip[i].Headers)
		}
		if !bytes.Equal(whole[i].Payload, drip[i].Payload) {
			t.Errorf("message %d payload differs:\n whole: %s\n  drip: %s", i, whole[i].Payload, drip[i].Payload)
		}
		if got := string(whole[i].Payload); got != fixtures[i].payload {
			t.Errorf("message %d payload = %s, want %s", i, got, fixtures[i].payload)
		}
	}
}

func TestChunkBoundaryInsideAStringDoesNotCorrupt(t *testing.T) {
	// A payload whose JSON contains bytes that look like a frame prelude. A
	// scanner that hunted for JSON keys instead of honouring the framing would
	// corrupt exactly here.
	payload := `{"content":"total_length \u0000\u0000\u0001\u0002 :event-type assistantResponseEvent"}`
	stream := append(eventFrame(t, "assistantResponseEvent", payload),
		eventFrame(t, "assistantResponseEvent", `{"content":"after"}`)...)

	for _, chunkSize := range []int{1, 2, 3, 5, 7, 11, 13, 16, 17, 32, 64, 128} {
		t.Run(fmt.Sprintf("chunk-%d", chunkSize), func(t *testing.T) {
			d := NewDecoder()
			var payloads []string
			for off := 0; off < len(stream); off += chunkSize {
				end := off + chunkSize
				if end > len(stream) {
					end = len(stream)
				}
				if _, err := d.Write(stream[off:end]); err != nil {
					t.Fatal(err)
				}
				for {
					msg, err := d.Next()
					if errors.Is(err, ErrIncomplete) {
						break
					}
					if err != nil {
						t.Fatalf("Next: %v", err)
					}
					payloads = append(payloads, string(msg.Payload))
				}
			}
			if len(payloads) != 2 {
				t.Fatalf("decoded %d payloads, want 2", len(payloads))
			}
			if payloads[0] != payload {
				t.Errorf("payload 0 = %s, want %s", payloads[0], payload)
			}
			if payloads[1] != `{"content":"after"}` {
				t.Errorf("payload 1 = %s", payloads[1])
			}
		})
	}
}

func TestAllHeaderValueTypesRoundTrip(t *testing.T) {
	var uuid [16]byte
	for i := range uuid {
		uuid[i] = byte(i + 1)
	}
	stamp := time.UnixMilli(1_700_000_000_123).UTC()

	headers := []Header{
		{Name: "h-true", Value: HeaderValue{Type: HeaderBoolTrue, Bool: true}},
		{Name: "h-false", Value: HeaderValue{Type: HeaderBoolFalse}},
		{Name: "h-byte", Value: HeaderValue{Type: HeaderByte, Int: -5}},
		{Name: "h-int16", Value: HeaderValue{Type: HeaderInt16, Int: -30000}},
		{Name: "h-int32", Value: HeaderValue{Type: HeaderInt32, Int: -2000000000}},
		{Name: "h-int64", Value: HeaderValue{Type: HeaderInt64, Int: -9000000000000000000}},
		{Name: "h-bytes", Value: HeaderValue{Type: HeaderBytes, Bytes: []byte{0x00, 0xff, 0x10}}},
		{Name: "h-string", Value: HeaderValue{Type: HeaderString, Str: "hello"}},
		{Name: "h-timestamp", Value: HeaderValue{Type: HeaderTimestamp, Timestamp: stamp}},
		{Name: "h-uuid", Value: HeaderValue{Type: HeaderUUID, UUID: uuid}},
	}

	frame, err := EncodeMessage(headers, []byte(`{}`))
	if err != nil {
		t.Fatalf("EncodeMessage: %v", err)
	}
	msgs := decodeAll(t, frame)
	if len(msgs) != 1 {
		t.Fatalf("decoded %d messages, want 1", len(msgs))
	}
	got := msgs[0].Headers

	checks := []struct {
		name string
		want HeaderValue
	}{
		{"h-true", HeaderValue{Type: HeaderBoolTrue, Bool: true}},
		{"h-false", HeaderValue{Type: HeaderBoolFalse, Bool: false}},
		{"h-byte", HeaderValue{Type: HeaderByte, Int: -5}},
		{"h-int16", HeaderValue{Type: HeaderInt16, Int: -30000}},
		{"h-int32", HeaderValue{Type: HeaderInt32, Int: -2000000000}},
		{"h-int64", HeaderValue{Type: HeaderInt64, Int: -9000000000000000000}},
		{"h-bytes", HeaderValue{Type: HeaderBytes, Bytes: []byte{0x00, 0xff, 0x10}}},
		{"h-string", HeaderValue{Type: HeaderString, Str: "hello"}},
	}
	for _, c := range checks {
		g, ok := got[c.name]
		if !ok {
			t.Errorf("header %s missing", c.name)
			continue
		}
		if g.Type != c.want.Type {
			t.Errorf("%s type = %v, want %v", c.name, g.Type, c.want.Type)
		}
		switch c.want.Type {
		case HeaderBoolTrue, HeaderBoolFalse:
			if g.Bool != c.want.Bool {
				t.Errorf("%s bool = %v, want %v", c.name, g.Bool, c.want.Bool)
			}
		case HeaderBytes:
			if !bytes.Equal(g.Bytes, c.want.Bytes) {
				t.Errorf("%s bytes = %v, want %v", c.name, g.Bytes, c.want.Bytes)
			}
		case HeaderString:
			if g.Str != c.want.Str {
				t.Errorf("%s string = %q, want %q", c.name, g.Str, c.want.Str)
			}
		default:
			if g.Int != c.want.Int {
				t.Errorf("%s int = %d, want %d", c.name, g.Int, c.want.Int)
			}
		}
	}

	if ts := got["h-timestamp"]; !ts.Timestamp.Equal(stamp) {
		t.Errorf("timestamp = %v, want %v", ts.Timestamp, stamp)
	}
	if u := got["h-uuid"]; u.UUID != uuid {
		t.Errorf("uuid = %x, want %x", u.UUID, uuid)
	}
}

func TestHeaderValueString(t *testing.T) {
	var uuid [16]byte
	for i := range uuid {
		uuid[i] = byte(i)
	}
	cases := []struct {
		value HeaderValue
		want  string
	}{
		{HeaderValue{Type: HeaderBoolTrue}, "true"},
		{HeaderValue{Type: HeaderBoolFalse}, "false"},
		{HeaderValue{Type: HeaderByte, Int: 7}, "7"},
		{HeaderValue{Type: HeaderInt16, Int: -1}, "-1"},
		{HeaderValue{Type: HeaderInt32, Int: 1 << 20}, "1048576"},
		{HeaderValue{Type: HeaderInt64, Int: -1 << 40}, "-1099511627776"},
		{HeaderValue{Type: HeaderBytes, Bytes: []byte("raw")}, "raw"},
		{HeaderValue{Type: HeaderString, Str: "text"}, "text"},
		{HeaderValue{Type: HeaderTimestamp, Timestamp: time.UnixMilli(0).UTC()}, "1970-01-01T00:00:00Z"},
		{HeaderValue{Type: HeaderUUID, UUID: uuid}, "00010203-0405-0607-0809-0a0b0c0d0e0f"},
		{HeaderValue{Type: HeaderType(200)}, ""},
	}
	for _, c := range cases {
		if got := c.value.String(); got != c.want {
			t.Errorf("HeaderValue{%v}.String() = %q, want %q", c.value.Type, got, c.want)
		}
	}
}

func TestHeaderTypeString(t *testing.T) {
	if got := HeaderString.String(); got != "string" {
		t.Errorf("HeaderString.String() = %q", got)
	}
	if got := HeaderType(99).String(); !strings.Contains(got, "99") {
		t.Errorf("unknown header type should render its number, got %q", got)
	}
}

func TestCorruptPreludeCRC(t *testing.T) {
	frame := eventFrame(t, "assistantResponseEvent", `{"content":"x"}`)
	frame[10] ^= 0xff // damage the prelude CRC

	d := NewDecoder()
	if _, err := d.Write(frame); err != nil {
		t.Fatal(err)
	}
	_, err := d.Next()
	if err == nil || errors.Is(err, ErrIncomplete) {
		t.Fatalf("expected a checksum error, got %v", err)
	}
	if !strings.Contains(err.Error(), "prelude checksum mismatch") {
		t.Errorf("error = %q, want a prelude checksum complaint", err)
	}
	// A desynchronised stream must stay failed.
	if _, again := d.Next(); again == nil || !strings.Contains(again.Error(), "prelude checksum mismatch") {
		t.Errorf("second Next() = %v, want the same permanent error", again)
	}
}

func TestCorruptMessageCRC(t *testing.T) {
	frame := eventFrame(t, "assistantResponseEvent", `{"content":"x"}`)
	frame[len(frame)-1] ^= 0xff

	d := NewDecoder()
	if _, err := d.Write(frame); err != nil {
		t.Fatal(err)
	}
	_, err := d.Next()
	if err == nil || !strings.Contains(err.Error(), "message checksum mismatch") {
		t.Fatalf("error = %v, want a message checksum complaint", err)
	}
}

func TestCorruptPayloadIsCaughtByTheMessageCRC(t *testing.T) {
	frame := eventFrame(t, "assistantResponseEvent", `{"content":"original"}`)
	// Flip a byte inside the payload without touching either checksum.
	idx := bytes.Index(frame, []byte("original"))
	if idx < 0 {
		t.Fatal("could not locate the payload in the frame")
	}
	frame[idx] = 'X'

	d := NewDecoder()
	if _, err := d.Write(frame); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Next(); err == nil || !strings.Contains(err.Error(), "message checksum mismatch") {
		t.Fatalf("a mutated payload must fail the message CRC, got %v", err)
	}
}

func TestTruncatedPreludeWaitsForMoreBytes(t *testing.T) {
	frame := eventFrame(t, "assistantResponseEvent", `{"content":"x"}`)
	for n := 0; n < preludeWithCRC; n++ {
		d := NewDecoder()
		if _, err := d.Write(frame[:n]); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Next(); !errors.Is(err, ErrIncomplete) {
			t.Errorf("with %d of %d prelude bytes, Next() = %v, want ErrIncomplete", n, preludeWithCRC, err)
		}
	}
}

func TestTruncatedBodyWaitsForMoreBytes(t *testing.T) {
	frame := eventFrame(t, "assistantResponseEvent", `{"content":"hello"}`)
	for n := preludeWithCRC; n < len(frame); n++ {
		d := NewDecoder()
		if _, err := d.Write(frame[:n]); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Next(); !errors.Is(err, ErrIncomplete) {
			t.Errorf("with %d of %d frame bytes, Next() = %v, want ErrIncomplete", n, len(frame), err)
		}
	}
}

// makePrelude builds a valid prelude for arbitrary length fields, so length
// validation can be tested independently of the checksum.
func makePrelude(totalLength, headersLength uint32) []byte {
	out := make([]byte, 0, preludeWithCRC)
	out = binary.BigEndian.AppendUint32(out, totalLength)
	out = binary.BigEndian.AppendUint32(out, headersLength)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(out[0:8]))
}

func TestAbsurdLengthsAreRejected(t *testing.T) {
	cases := []struct {
		name          string
		totalLength   uint32
		headersLength uint32
		wantSubstring string
	}{
		{"zero total length", 0, 0, "below the"},
		{"total length below minimum", 15, 0, "below the"},
		{"gigantic total length", 0xFFFFFFFF, 0, "above the"},
		{"one byte over the limit", uint32(DefaultMaxMessageSize) + 1, 0, "above the"},
		{"headers larger than the frame", 100, 200, "does not fit"},
		{"headers exactly overflow", 100, 85, "does not fit"},
		{"headers at max uint32", 64, 0xFFFFFFFF, "does not fit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewDecoder()
			if _, err := d.Write(makePrelude(tc.totalLength, tc.headersLength)); err != nil {
				t.Fatal(err)
			}
			_, err := d.Next()
			if err == nil || errors.Is(err, ErrIncomplete) {
				t.Fatalf("Next() = %v, want a validation error", err)
			}
			if !strings.Contains(err.Error(), tc.wantSubstring) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantSubstring)
			}
		})
	}
}

func TestMinimalFrameWithNoHeadersOrPayload(t *testing.T) {
	frame, err := EncodeMessage(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(frame) != minMessageLength {
		t.Fatalf("empty frame is %d bytes, want %d", len(frame), minMessageLength)
	}
	msgs := decodeAll(t, frame)
	if len(msgs) != 1 {
		t.Fatalf("decoded %d messages, want 1", len(msgs))
	}
	if len(msgs[0].Headers) != 0 || len(msgs[0].Payload) != 0 {
		t.Errorf("expected an empty message, got %+v", msgs[0])
	}
}

func TestMalformedHeaderSection(t *testing.T) {
	// Build frames whose header section is deliberately broken. EncodeMessage
	// cannot produce these, so the bytes are assembled by hand.
	cases := []struct {
		name          string
		headerBytes   []byte
		wantSubstring string
	}{
		{"name runs past the end", []byte{0x10, 'a', 'b'}, "runs past the end"},
		{"no value type", []byte{0x01, 'a'}, "no value type"},
		{"truncated int32", []byte{0x01, 'a', byte(HeaderInt32), 0x00, 0x01}, "truncated"},
		{"truncated int16", []byte{0x01, 'a', byte(HeaderInt16), 0x00}, "truncated"},
		{"truncated int64", []byte{0x01, 'a', byte(HeaderInt64), 0x00}, "truncated"},
		{"truncated byte", []byte{0x01, 'a', byte(HeaderByte)}, "truncated"},
		{"truncated timestamp", []byte{0x01, 'a', byte(HeaderTimestamp), 0x00}, "truncated"},
		{"truncated uuid", []byte{0x01, 'a', byte(HeaderUUID), 0x00}, "truncated"},
		{"missing string length", []byte{0x01, 'a', byte(HeaderString), 0x00}, "truncated"},
		{"string length overruns", []byte{0x01, 'a', byte(HeaderString), 0x00, 0x09, 'x'}, "value bytes but only"},
		{"unsupported value type", []byte{0x01, 'a', 0x63}, "unsupported value type"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte(`{}`)
			total := frameOverhead + len(tc.headerBytes) + len(payload)
			frame := make([]byte, 0, total)
			frame = binary.BigEndian.AppendUint32(frame, uint32(total))
			frame = binary.BigEndian.AppendUint32(frame, uint32(len(tc.headerBytes)))
			frame = binary.BigEndian.AppendUint32(frame, crc32.ChecksumIEEE(frame[0:8]))
			frame = append(frame, tc.headerBytes...)
			frame = append(frame, payload...)
			frame = binary.BigEndian.AppendUint32(frame, crc32.ChecksumIEEE(frame))

			d := NewDecoder()
			if _, err := d.Write(frame); err != nil {
				t.Fatal(err)
			}
			_, err := d.Next()
			if err == nil || errors.Is(err, ErrIncomplete) {
				t.Fatalf("Next() = %v, want a header parse error", err)
			}
			if !strings.Contains(err.Error(), tc.wantSubstring) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantSubstring)
			}
		})
	}
}

func TestDuplicateHeaderKeepsTheLastValue(t *testing.T) {
	frame, err := EncodeMessage([]Header{
		StringHeader("dup", "first"),
		StringHeader("dup", "second"),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	msgs := decodeAll(t, frame)
	if got, _ := msgs[0].Header("dup"); got != "second" {
		t.Errorf("duplicate header resolved to %q, want the last value", got)
	}
}

func TestDecoderBufferedAndReuse(t *testing.T) {
	first := eventFrame(t, "assistantResponseEvent", `{"content":"a"}`)
	second := eventFrame(t, "assistantResponseEvent", `{"content":"b"}`)

	d := NewDecoder()
	if _, err := d.Write(first); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write(second[:5]); err != nil {
		t.Fatal(err)
	}

	if _, err := d.Next(); err != nil {
		t.Fatal(err)
	}
	if got := d.Buffered(); got != 5 {
		t.Errorf("Buffered() = %d, want 5 after consuming the first frame", got)
	}
	if _, err := d.Next(); !errors.Is(err, ErrIncomplete) {
		t.Errorf("Next() = %v, want ErrIncomplete", err)
	}
	if _, err := d.Write(second[5:]); err != nil {
		t.Fatal(err)
	}
	msg, err := d.Next()
	if err != nil {
		t.Fatal(err)
	}
	if string(msg.Payload) != `{"content":"b"}` {
		t.Errorf("payload = %s", msg.Payload)
	}
	if got := d.Buffered(); got != 0 {
		t.Errorf("Buffered() = %d, want 0 once the stream is drained", got)
	}
}

func TestDecoderBufferDoesNotGrowUnbounded(t *testing.T) {
	frame := eventFrame(t, "assistantResponseEvent", `{"content":"x"}`)
	d := NewDecoder()
	for i := 0; i < 2000; i++ {
		if _, err := d.Write(frame); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Next(); err != nil {
			t.Fatal(err)
		}
		if d.Buffered() != 0 {
			t.Fatalf("iteration %d left %d bytes buffered", i, d.Buffered())
		}
	}
	if cap(d.buf) > 64*len(frame) {
		t.Errorf("buffer capacity grew to %d for a %d-byte frame", cap(d.buf), len(frame))
	}
}

func TestDecoderSizeLimitIsConfigurable(t *testing.T) {
	frame := eventFrame(t, "assistantResponseEvent", `{"content":"hello there"}`)

	d := NewDecoderWithLimit(len(frame) - 1)
	if _, err := d.Write(frame); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Next(); err == nil || !strings.Contains(err.Error(), "above the") {
		t.Errorf("Next() = %v, want the frame to exceed the configured limit", err)
	}

	ok := NewDecoderWithLimit(len(frame))
	if _, err := ok.Write(frame); err != nil {
		t.Fatal(err)
	}
	if _, err := ok.Next(); err != nil {
		t.Errorf("a frame exactly at the limit should decode, got %v", err)
	}
}

func TestDecoderLimitHasAFloor(t *testing.T) {
	d := NewDecoderWithLimit(1)
	if d.maxMessageSize != minMessageLength {
		t.Errorf("maxMessageSize = %d, want it clamped up to %d", d.maxMessageSize, minMessageLength)
	}
}

func TestReaderStreamsMessages(t *testing.T) {
	var stream []byte
	for i := 0; i < 3; i++ {
		stream = append(stream, eventFrame(t, "assistantResponseEvent",
			fmt.Sprintf(`{"content":"chunk-%d"}`, i))...)
	}

	r := NewReader(bytes.NewReader(stream))
	var got []string
	for {
		msg, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		got = append(got, string(msg.Payload))
	}
	want := []string{`{"content":"chunk-0"}`, `{"content":"chunk-1"}`, `{"content":"chunk-2"}`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("payloads = %v, want %v", got, want)
	}
}

// oneByteReader yields a single byte per Read, simulating a dribbling network.
type oneByteReader struct {
	data []byte
	pos  int
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = r.data[r.pos]
	r.pos++
	return 1, nil
}

func TestReaderHandlesOneBytePerRead(t *testing.T) {
	stream := append(eventFrame(t, "assistantResponseEvent", `{"content":"a"}`),
		eventFrame(t, "metadataEvent", `{"stopReason":"end_turn"}`)...)

	r := NewReader(&oneByteReader{data: stream})
	var types []string
	for {
		msg, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		types = append(types, msg.EventType())
	}
	want := []string{"assistantResponseEvent", "metadataEvent"}
	if !reflect.DeepEqual(types, want) {
		t.Errorf("event types = %v, want %v", types, want)
	}
}

func TestReaderReportsTruncatedStream(t *testing.T) {
	frame := eventFrame(t, "assistantResponseEvent", `{"content":"never finished"}`)
	r := NewReader(bytes.NewReader(frame[:len(frame)-3]))

	_, err := r.Next()
	if err == nil {
		t.Fatal("expected an error for a truncated stream")
	}
	if errors.Is(err, io.EOF) {
		t.Fatal("a truncated frame must not look like a clean end of stream")
	}
	if !strings.Contains(err.Error(), "unfinished message") {
		t.Errorf("error = %q, want it to describe the unfinished message", err)
	}
}

// errorReader fails after delivering its data.
type errorReader struct {
	data []byte
	pos  int
	err  error
}

func (r *errorReader) Read(p []byte) (int, error) {
	if r.pos < len(r.data) {
		n := copy(p, r.data[r.pos:])
		r.pos += n
		return n, nil
	}
	return 0, r.err
}

func TestReaderPropagatesTransportErrors(t *testing.T) {
	want := errors.New("connection reset by peer")
	frame := eventFrame(t, "assistantResponseEvent", `{"content":"a"}`)
	r := NewReader(&errorReader{data: frame, err: want})

	if _, err := r.Next(); err != nil {
		t.Fatalf("the buffered frame should decode first: %v", err)
	}
	_, err := r.Next()
	if !errors.Is(err, want) {
		t.Errorf("error = %v, want the transport error %v", err, want)
	}
}

func TestReaderOnEmptyStream(t *testing.T) {
	r := NewReader(bytes.NewReader(nil))
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("Next() on an empty stream = %v, want io.EOF", err)
	}
}

func TestEncodeMessageRejectsOversizedHeaders(t *testing.T) {
	long := strings.Repeat("n", 256)
	if _, err := EncodeMessage([]Header{StringHeader(long, "v")}, nil); err == nil {
		t.Error("a header name longer than 255 bytes should be rejected")
	}
	if _, err := EncodeMessage([]Header{StringHeader("n", strings.Repeat("v", 65536))}, nil); err == nil {
		t.Error("a header value longer than 65535 bytes should be rejected")
	}
	if _, err := EncodeMessage([]Header{{Name: "n", Value: HeaderValue{Type: HeaderType(200)}}}, nil); err == nil {
		t.Error("an unsupported header value type should be rejected")
	}
	big := HeaderValue{Type: HeaderBytes, Bytes: make([]byte, 65536)}
	if _, err := EncodeMessage([]Header{{Name: "n", Value: big}}, nil); err == nil {
		t.Error("a byte value longer than 65535 bytes should be rejected")
	}
}

func TestLargePayloadRoundTrip(t *testing.T) {
	payload := `{"content":"` + strings.Repeat("x", 1<<20) + `"}`
	frame := eventFrame(t, "assistantResponseEvent", payload)

	d := NewDecoder()
	// Feed in 4 KiB chunks, as a real connection would.
	for off := 0; off < len(frame); off += 4096 {
		end := off + 4096
		if end > len(frame) {
			end = len(frame)
		}
		if _, err := d.Write(frame[off:end]); err != nil {
			t.Fatal(err)
		}
	}
	msg, err := d.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if string(msg.Payload) != payload {
		t.Errorf("payload of %d bytes did not round-trip", len(payload))
	}
}

func TestPayloadIsCopiedNotAliased(t *testing.T) {
	first := eventFrame(t, "assistantResponseEvent", `{"content":"first-payload"}`)
	second := eventFrame(t, "assistantResponseEvent", `{"content":"second-payload"}`)

	d := NewDecoder()
	if _, err := d.Write(append(first, second...)); err != nil {
		t.Fatal(err)
	}
	m1, err := d.Next()
	if err != nil {
		t.Fatal(err)
	}
	saved := string(m1.Payload)
	// Decoding the next frame shifts the internal buffer; an aliased payload
	// would be corrupted by that move.
	if _, err := d.Next(); err != nil {
		t.Fatal(err)
	}
	if string(m1.Payload) != saved {
		t.Errorf("first payload changed to %s after decoding the second frame", m1.Payload)
	}
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.

// Package kiro speaks the Kiro (Amazon Q Developer / CodeWhisperer) wire
// protocol: request construction, the vnd.amazon.eventstream response framing
// and the modelled error shapes.
package kiro

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"time"
)

// Frame layout constants for vnd.amazon.eventstream.
//
//	[ total_length u32 | headers_length u32 | prelude_crc u32 ]
//	[ headers ... headers_length bytes                        ]
//	[ payload ... total_length - headers_length - 16 bytes    ]
//	[ message_crc u32                                         ]
//
// All integers are big-endian.
const (
	preludeSize      = 8  // total_length + headers_length
	preludeWithCRC   = 12 // prelude plus its CRC
	messageCRCSize   = 4
	frameOverhead    = preludeWithCRC + messageCRCSize // 16
	minMessageLength = frameOverhead
)

// DefaultMaxMessageSize caps a single frame. AWS event stream messages are
// limited to 16 MiB; the extra headroom tolerates a larger server-side limit
// while still refusing an absurd length field.
const DefaultMaxMessageSize = 32 << 20

// ErrIncomplete reports that the buffer does not yet hold a whole message.
// Callers should read more bytes and try again. It is a normal condition on a
// streaming connection, not a failure.
var ErrIncomplete = errors.New("event stream: incomplete message")

// HeaderType enumerates the value encodings a header may use.
type HeaderType uint8

// Header value types defined by vnd.amazon.eventstream.
const (
	HeaderBoolTrue  HeaderType = 0 // no value bytes
	HeaderBoolFalse HeaderType = 1 // no value bytes
	HeaderByte      HeaderType = 2
	HeaderInt16     HeaderType = 3
	HeaderInt32     HeaderType = 4
	HeaderInt64     HeaderType = 5
	HeaderBytes     HeaderType = 6 // u16 length prefix
	HeaderString    HeaderType = 7 // u16 length prefix
	HeaderTimestamp HeaderType = 8 // int64 milliseconds since the epoch
	HeaderUUID      HeaderType = 9 // 16 raw bytes
)

// String names the header type for diagnostics.
func (t HeaderType) String() string {
	switch t {
	case HeaderBoolTrue:
		return "bool-true"
	case HeaderBoolFalse:
		return "bool-false"
	case HeaderByte:
		return "byte"
	case HeaderInt16:
		return "int16"
	case HeaderInt32:
		return "int32"
	case HeaderInt64:
		return "int64"
	case HeaderBytes:
		return "bytes"
	case HeaderString:
		return "string"
	case HeaderTimestamp:
		return "timestamp"
	case HeaderUUID:
		return "uuid"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(t))
	}
}

// HeaderValue is one decoded header value. Only the field matching Type is
// meaningful.
type HeaderValue struct {
	Type      HeaderType
	Bool      bool
	Int       int64
	Str       string
	Bytes     []byte
	Timestamp time.Time
	UUID      [16]byte
}

// String renders the value as text. Header names kirogo cares about
// (:message-type, :event-type, :exception-type) are always strings, so this is
// the accessor the event layer uses.
func (v HeaderValue) String() string {
	switch v.Type {
	case HeaderBoolTrue:
		return "true"
	case HeaderBoolFalse:
		return "false"
	case HeaderByte, HeaderInt16, HeaderInt32, HeaderInt64:
		return fmt.Sprintf("%d", v.Int)
	case HeaderBytes:
		return string(v.Bytes)
	case HeaderString:
		return v.Str
	case HeaderTimestamp:
		return v.Timestamp.UTC().Format(time.RFC3339Nano)
	case HeaderUUID:
		return fmt.Sprintf("%x-%x-%x-%x-%x", v.UUID[0:4], v.UUID[4:6], v.UUID[6:8], v.UUID[8:10], v.UUID[10:16])
	default:
		return ""
	}
}

// Message is one decoded event stream frame.
type Message struct {
	// Headers is keyed by header name. Duplicate names keep the last value,
	// matching how the AWS SDKs interpret them.
	Headers map[string]HeaderValue
	// Payload is the frame body, JSON for event and exception messages. It is a
	// copy, so it stays valid after the decoder buffer is reused.
	Payload []byte
}

// Header returns a header's textual value and whether it was present.
func (m *Message) Header(name string) (string, bool) {
	v, ok := m.Headers[name]
	if !ok {
		return "", false
	}
	return v.String(), true
}

// MessageType returns the :message-type header, normally "event" or "exception".
func (m *Message) MessageType() string {
	s, _ := m.Header(":message-type")
	return s
}

// EventType returns the :event-type header.
func (m *Message) EventType() string {
	s, _ := m.Header(":event-type")
	return s
}

// ExceptionType returns the :exception-type header.
func (m *Message) ExceptionType() string {
	s, _ := m.Header(":exception-type")
	return s
}

// Decoder turns a byte stream into event stream messages.
//
// It is incremental by design: bytes arrive in whatever chunks the network
// produces, and a message is emitted only once every byte of it is buffered.
// One chunk is never assumed to be one message.
//
// A Decoder is not safe for concurrent use.
type Decoder struct {
	buf []byte
	// maxMessageSize rejects an implausible total_length before allocating.
	maxMessageSize int
	// failed records a permanent framing failure. Once the stream is
	// desynchronised there is no safe way to resynchronise, so every later call
	// returns the same error.
	failed error
}

// NewDecoder returns a Decoder with the default size limit.
func NewDecoder() *Decoder {
	return &Decoder{maxMessageSize: DefaultMaxMessageSize}
}

// NewDecoderWithLimit returns a Decoder that rejects frames larger than max.
func NewDecoderWithLimit(max int) *Decoder {
	if max < minMessageLength {
		max = minMessageLength
	}
	return &Decoder{maxMessageSize: max}
}

// Write appends bytes to the decoder's buffer. It never fails, so it satisfies
// io.Writer for use with io.Copy and friends.
func (d *Decoder) Write(p []byte) (int, error) {
	d.buf = append(d.buf, p...)
	return len(p), nil
}

// Buffered reports how many bytes are held but not yet decoded. A non-zero
// value at end of stream means the connection was cut mid-message.
func (d *Decoder) Buffered() int {
	return len(d.buf)
}

// Next decodes the next complete message.
//
// It returns ErrIncomplete when more bytes are needed. Any other error means the
// stream is corrupt and cannot be resynchronised; the same error is returned to
// every later call.
func (d *Decoder) Next() (*Message, error) {
	if d.failed != nil {
		return nil, d.failed
	}
	if len(d.buf) < preludeWithCRC {
		return nil, ErrIncomplete
	}

	totalLength := binary.BigEndian.Uint32(d.buf[0:4])
	headersLength := binary.BigEndian.Uint32(d.buf[4:8])
	preludeCRC := binary.BigEndian.Uint32(d.buf[8:12])

	// Validate the prelude before trusting either length. A corrupt length would
	// otherwise make the decoder wait forever or allocate wildly.
	if got := crc32.ChecksumIEEE(d.buf[0:preludeSize]); got != preludeCRC {
		return nil, d.fail(fmt.Errorf("event stream: prelude checksum mismatch (computed %#08x, header says %#08x). The connection is corrupt", got, preludeCRC))
	}

	if totalLength < minMessageLength {
		return nil, d.fail(fmt.Errorf("event stream: frame claims to be %d bytes, which is below the %d-byte minimum", totalLength, minMessageLength))
	}
	if totalLength > uint32(d.maxMessageSize) {
		return nil, d.fail(fmt.Errorf("event stream: frame claims to be %d bytes, above the %d-byte limit", totalLength, d.maxMessageSize))
	}
	if uint64(headersLength)+uint64(frameOverhead) > uint64(totalLength) {
		return nil, d.fail(fmt.Errorf("event stream: header section of %d bytes does not fit in a %d-byte frame", headersLength, totalLength))
	}

	if uint32(len(d.buf)) < totalLength {
		return nil, ErrIncomplete
	}

	frame := d.buf[:totalLength]

	// The message CRC covers everything from byte 0 to the end of the payload.
	wantCRC := binary.BigEndian.Uint32(frame[totalLength-messageCRCSize:])
	if got := crc32.ChecksumIEEE(frame[:totalLength-messageCRCSize]); got != wantCRC {
		return nil, d.fail(fmt.Errorf("event stream: message checksum mismatch (computed %#08x, frame says %#08x). The connection is corrupt", got, wantCRC))
	}

	headerBytes := frame[preludeWithCRC : preludeWithCRC+headersLength]
	headers, err := parseHeaders(headerBytes)
	if err != nil {
		return nil, d.fail(err)
	}

	payloadStart := preludeWithCRC + int(headersLength)
	payloadEnd := int(totalLength) - messageCRCSize
	payload := make([]byte, payloadEnd-payloadStart)
	copy(payload, frame[payloadStart:payloadEnd])

	// Advance past the consumed frame. Reslicing and copying down keeps the
	// buffer from growing without bound over a long stream.
	remaining := len(d.buf) - int(totalLength)
	copy(d.buf, d.buf[totalLength:])
	d.buf = d.buf[:remaining]

	return &Message{Headers: headers, Payload: payload}, nil
}

// fail records a permanent decode failure and returns it.
func (d *Decoder) fail(err error) error {
	d.failed = err
	return err
}

// parseHeaders decodes the header section.
func parseHeaders(b []byte) (map[string]HeaderValue, error) {
	headers := make(map[string]HeaderValue)
	pos := 0

	for pos < len(b) {
		nameLen := int(b[pos])
		pos++
		if pos+nameLen > len(b) {
			return nil, fmt.Errorf("event stream: header name of %d bytes runs past the end of the header section", nameLen)
		}
		name := string(b[pos : pos+nameLen])
		pos += nameLen

		if pos >= len(b) {
			return nil, fmt.Errorf("event stream: header %q has no value type", name)
		}
		valueType := HeaderType(b[pos])
		pos++

		value := HeaderValue{Type: valueType}
		switch valueType {
		case HeaderBoolTrue:
			value.Bool = true
		case HeaderBoolFalse:
			value.Bool = false
		case HeaderByte:
			if pos+1 > len(b) {
				return nil, headerTruncated(name, "byte")
			}
			value.Int = int64(int8(b[pos]))
			pos++
		case HeaderInt16:
			if pos+2 > len(b) {
				return nil, headerTruncated(name, "int16")
			}
			value.Int = int64(int16(binary.BigEndian.Uint16(b[pos:])))
			pos += 2
		case HeaderInt32:
			if pos+4 > len(b) {
				return nil, headerTruncated(name, "int32")
			}
			value.Int = int64(int32(binary.BigEndian.Uint32(b[pos:])))
			pos += 4
		case HeaderInt64:
			if pos+8 > len(b) {
				return nil, headerTruncated(name, "int64")
			}
			value.Int = int64(binary.BigEndian.Uint64(b[pos:]))
			pos += 8
		case HeaderBytes, HeaderString:
			if pos+2 > len(b) {
				return nil, headerTruncated(name, valueType.String())
			}
			length := int(binary.BigEndian.Uint16(b[pos:]))
			pos += 2
			if pos+length > len(b) {
				return nil, fmt.Errorf("event stream: header %q declares %d value bytes but only %d remain", name, length, len(b)-pos)
			}
			if valueType == HeaderString {
				value.Str = string(b[pos : pos+length])
			} else {
				value.Bytes = make([]byte, length)
				copy(value.Bytes, b[pos:pos+length])
			}
			pos += length
		case HeaderTimestamp:
			if pos+8 > len(b) {
				return nil, headerTruncated(name, "timestamp")
			}
			millis := int64(binary.BigEndian.Uint64(b[pos:]))
			value.Int = millis
			value.Timestamp = time.UnixMilli(millis).UTC()
			pos += 8
		case HeaderUUID:
			if pos+16 > len(b) {
				return nil, headerTruncated(name, "uuid")
			}
			copy(value.UUID[:], b[pos:pos+16])
			pos += 16
		default:
			// An unknown value type makes the rest of the header section
			// unparseable, because its length is unknown.
			return nil, fmt.Errorf("event stream: header %q uses unsupported value type %d", name, uint8(valueType))
		}

		headers[name] = value
	}

	return headers, nil
}

// headerTruncated builds the error for a header whose value is cut short.
func headerTruncated(name, kind string) error {
	return fmt.Errorf("event stream: header %q is truncated: not enough bytes for a %s value", name, kind)
}

// Reader decodes messages from an io.Reader, refilling the decoder as needed.
type Reader struct {
	src   io.Reader
	dec   *Decoder
	chunk []byte
	eof   bool
}

// DefaultReadChunkSize is the read granularity used by Reader.
const DefaultReadChunkSize = 32 << 10

// NewReader wraps src. Close is the caller's responsibility.
func NewReader(src io.Reader) *Reader {
	return &Reader{
		src:   src,
		dec:   NewDecoder(),
		chunk: make([]byte, DefaultReadChunkSize),
	}
}

// Next returns the next message, or io.EOF once the stream ends cleanly.
//
// A stream that ends with a partial frame buffered is reported as an error
// rather than a clean EOF, because that means the response was cut short.
func (r *Reader) Next() (*Message, error) {
	for {
		msg, err := r.dec.Next()
		if err == nil {
			return msg, nil
		}
		if !errors.Is(err, ErrIncomplete) {
			return nil, err
		}
		if r.eof {
			if r.dec.Buffered() > 0 {
				return nil, fmt.Errorf("event stream: connection ended with %d bytes of an unfinished message", r.dec.Buffered())
			}
			return nil, io.EOF
		}

		n, readErr := r.src.Read(r.chunk)
		if n > 0 {
			if _, err := r.dec.Write(r.chunk[:n]); err != nil {
				return nil, err
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				r.eof = true
				continue
			}
			return nil, readErr
		}
	}
}

// EncodeMessage builds a frame for the given headers and payload.
//
// Production code only decodes, but the encoder keeps the test fixtures honest:
// they are generated from the same field definitions the decoder reads.
func EncodeMessage(headers []Header, payload []byte) ([]byte, error) {
	headerBytes, err := encodeHeaders(headers)
	if err != nil {
		return nil, err
	}

	total := frameOverhead + len(headerBytes) + len(payload)
	if total > math.MaxUint32 {
		return nil, fmt.Errorf("event stream: message of %d bytes is too large to encode", total)
	}

	out := make([]byte, 0, total)
	out = binary.BigEndian.AppendUint32(out, uint32(total))
	out = binary.BigEndian.AppendUint32(out, uint32(len(headerBytes)))
	out = binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(out[0:preludeSize]))
	out = append(out, headerBytes...)
	out = append(out, payload...)
	out = binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(out))
	return out, nil
}

// Header is a name/value pair for EncodeMessage.
type Header struct {
	Name  string
	Value HeaderValue
}

// StringHeader builds a string-typed header.
func StringHeader(name, value string) Header {
	return Header{Name: name, Value: HeaderValue{Type: HeaderString, Str: value}}
}

// encodeHeaders serialises headers into the wire format.
func encodeHeaders(headers []Header) ([]byte, error) {
	var out []byte
	for _, h := range headers {
		if len(h.Name) > math.MaxUint8 {
			return nil, fmt.Errorf("event stream: header name %q is longer than 255 bytes", h.Name)
		}
		out = append(out, byte(len(h.Name)))
		out = append(out, h.Name...)
		out = append(out, byte(h.Value.Type))

		switch h.Value.Type {
		case HeaderBoolTrue, HeaderBoolFalse:
			// No value bytes.
		case HeaderByte:
			out = append(out, byte(int8(h.Value.Int)))
		case HeaderInt16:
			out = binary.BigEndian.AppendUint16(out, uint16(int16(h.Value.Int)))
		case HeaderInt32:
			out = binary.BigEndian.AppendUint32(out, uint32(int32(h.Value.Int)))
		case HeaderInt64:
			out = binary.BigEndian.AppendUint64(out, uint64(h.Value.Int))
		case HeaderBytes:
			if len(h.Value.Bytes) > math.MaxUint16 {
				return nil, fmt.Errorf("event stream: header %q value is longer than 65535 bytes", h.Name)
			}
			out = binary.BigEndian.AppendUint16(out, uint16(len(h.Value.Bytes)))
			out = append(out, h.Value.Bytes...)
		case HeaderString:
			if len(h.Value.Str) > math.MaxUint16 {
				return nil, fmt.Errorf("event stream: header %q value is longer than 65535 bytes", h.Name)
			}
			out = binary.BigEndian.AppendUint16(out, uint16(len(h.Value.Str)))
			out = append(out, h.Value.Str...)
		case HeaderTimestamp:
			out = binary.BigEndian.AppendUint64(out, uint64(h.Value.Timestamp.UnixMilli()))
		case HeaderUUID:
			out = append(out, h.Value.UUID[:]...)
		default:
			return nil, fmt.Errorf("event stream: cannot encode header %q with value type %d", h.Name, uint8(h.Value.Type))
		}
	}
	return out, nil
}

// Copyright (c) 2026 Jasmin (https://github.com/jasminnanda)
// Licensed under the MIT License. See LICENSE in the project root.

// Package wire implements the jsingo framing protocol.
//
// The protocol is deliberately small: length-prefixed frames over a stream,
// each tagged with a type and a call id. Multiplexing, cancellation and
// logging all ride on the same connection.
//
//	┌────────────┬─────────┬───────────┬─────────────┐
//	│ len uint32 │ type u8 │ id uint64 │ payload ... │
//	└────────────┴─────────┴───────────┴─────────────┘
//	 big endian    len counts type+id+payload, not itself
//
// This package knows nothing about processes, sockets or JavaScript. It is
// pure byte manipulation so it can be fuzzed and tested in isolation.
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// HeaderSize is the number of bytes preceding a frame payload: the 4 byte
// length prefix, the 1 byte type and the 8 byte call id.
const HeaderSize = 4 + 1 + 8

// prefixSize is the length prefix itself, which is not counted by the prefix.
const prefixSize = 4

// DefaultMaxFrameSize caps a single frame's length field. It bounds the
// allocation a peer can induce; documents are commonly multi-megabyte, so the
// default is generous but finite.
const DefaultMaxFrameSize = 64 << 20 // 64 MiB

// Type identifies what a frame carries.
type Type uint8

const (
	// TypeCall invokes an exported JS function. Payload: [nameLen uint16][name][body].
	TypeCall Type = 1
	// TypeReply carries a successful result. Payload: the encoded return value.
	TypeReply Type = 2
	// TypeError carries a handler or protocol failure.
	// Payload: [code uint16][msgLen uint16][msg][details].
	TypeError Type = 3
	// TypeCancel aborts an in-flight call. Payload: empty.
	TypeCancel Type = 4
	// TypeLog carries one structured log record. Payload: an NDJSON record.
	// Log frames use id 0.
	TypeLog Type = 5
	// TypePing probes liveness. Payload: empty.
	TypePing Type = 6
	// TypePong answers a ping, echoing its id.
	TypePong Type = 7
)

// String implements fmt.Stringer.
func (t Type) String() string {
	switch t {
	case TypeCall:
		return "CALL"
	case TypeReply:
		return "REPLY"
	case TypeError:
		return "ERROR"
	case TypeCancel:
		return "CANCEL"
	case TypeLog:
		return "LOG"
	case TypePing:
		return "PING"
	case TypePong:
		return "PONG"
	default:
		return fmt.Sprintf("Type(%d)", uint8(t))
	}
}

// Valid reports whether t is a known frame type. Decoders reject unknown
// types rather than skipping them: a peer sending one is out of sync, and
// continuing would desynchronise the stream.
func (t Type) Valid() bool {
	return t >= TypeCall && t <= TypePong
}

// Protocol errors. All decode failures are wrapped so callers can branch with
// errors.Is without matching on strings.
var (
	// ErrFrameTooLarge means a length prefix exceeded the configured maximum.
	ErrFrameTooLarge = errors.New("wire: frame exceeds maximum size")
	// ErrFrameTooSmall means a length prefix could not cover the header.
	ErrFrameTooSmall = errors.New("wire: frame shorter than header")
	// ErrUnknownType means the type byte is not a defined Type.
	ErrUnknownType = errors.New("wire: unknown frame type")
	// ErrMalformedPayload means a payload did not match its type's layout.
	ErrMalformedPayload = errors.New("wire: malformed payload")
)

// Frame is a single decoded protocol message.
//
// Payload aliases the buffer it was decoded from. A Frame obtained from
// [Reader.ReadFrame] is only valid until the next read on that Reader; use
// [Frame.Clone] to retain it.
type Frame struct {
	Type    Type
	ID      uint64
	Payload []byte
}

// Clone returns a deep copy safe to retain past the next read.
func (f Frame) Clone() Frame {
	if f.Payload == nil {
		return f
	}
	p := make([]byte, len(f.Payload))
	copy(p, f.Payload)
	f.Payload = p
	return f
}

// Size is the total encoded length of the frame, including the length prefix.
func (f Frame) Size() int {
	return prefixSize + 1 + 8 + len(f.Payload)
}

// AppendTo appends the encoded frame to dst and returns the extended slice.
// It reports ErrFrameTooLarge if the frame would overflow the length prefix.
func (f Frame) AppendTo(dst []byte, max uint32) ([]byte, error) {
	if max == 0 {
		max = DefaultMaxFrameSize
	}
	// Compute in uint64 so the check itself cannot overflow.
	body := uint64(1) + 8 + uint64(len(f.Payload))
	if body > uint64(max) || body > math.MaxUint32 {
		return dst, fmt.Errorf("%w: %d bytes, max %d", ErrFrameTooLarge, body, max)
	}

	dst = binary.BigEndian.AppendUint32(dst, uint32(body))
	dst = append(dst, byte(f.Type))
	dst = binary.BigEndian.AppendUint64(dst, f.ID)
	dst = append(dst, f.Payload...)
	return dst, nil
}

// DecodeFrame decodes one frame from the front of buf, which must contain the
// length prefix. It returns the frame and the number of bytes consumed.
//
// The returned Payload aliases buf.
func DecodeFrame(buf []byte, max uint32) (Frame, int, error) {
	if max == 0 {
		max = DefaultMaxFrameSize
	}
	if len(buf) < prefixSize {
		return Frame{}, 0, fmt.Errorf("%w: need %d bytes for length prefix, have %d",
			ErrFrameTooSmall, prefixSize, len(buf))
	}

	body := binary.BigEndian.Uint32(buf[:prefixSize])
	if err := checkBodyLen(body, max); err != nil {
		return Frame{}, 0, err
	}

	total := prefixSize + int(body)
	if len(buf) < total {
		return Frame{}, 0, fmt.Errorf("%w: need %d bytes, have %d",
			ErrFrameTooSmall, total, len(buf))
	}

	f, err := decodeBody(buf[prefixSize:total])
	if err != nil {
		return Frame{}, 0, err
	}
	return f, total, nil
}

// checkBodyLen validates a length prefix against the header minimum and max.
func checkBodyLen(body, max uint32) error {
	if body < 1+8 {
		return fmt.Errorf("%w: length %d cannot cover type and id", ErrFrameTooSmall, body)
	}
	if body > max {
		return fmt.Errorf("%w: %d bytes, max %d", ErrFrameTooLarge, body, max)
	}
	return nil
}

// decodeBody decodes the type, id and payload from a frame body, which is
// everything after the length prefix and is exactly body bytes long.
func decodeBody(b []byte) (Frame, error) {
	if len(b) < 1+8 {
		return Frame{}, fmt.Errorf("%w: body %d bytes", ErrFrameTooSmall, len(b))
	}
	t := Type(b[0])
	if !t.Valid() {
		return Frame{}, fmt.Errorf("%w: %d", ErrUnknownType, b[0])
	}
	f := Frame{
		Type: t,
		ID:   binary.BigEndian.Uint64(b[1:9]),
	}
	if len(b) > 9 {
		f.Payload = b[9:]
	}
	return f, nil
}

// --- payload codecs -------------------------------------------------------
//
// Each frame type that carries structure gets an explicit encoder and a
// bounds-checked decoder here, rather than callers indexing into Payload.

// maxMethodLen bounds a method name so a corrupt length cannot force a large
// string allocation. JS identifiers are far shorter than this.
const maxMethodLen = 1024

// AppendCallPayload encodes a CALL payload: [nameLen uint16][name][body].
func AppendCallPayload(dst []byte, method string, body []byte) ([]byte, error) {
	if method == "" {
		return dst, fmt.Errorf("%w: empty method name", ErrMalformedPayload)
	}
	if len(method) > maxMethodLen {
		return dst, fmt.Errorf("%w: method name %d bytes, max %d",
			ErrMalformedPayload, len(method), maxMethodLen)
	}
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(method)))
	dst = append(dst, method...)
	dst = append(dst, body...)
	return dst, nil
}

// DecodeCallPayload decodes a CALL payload. The returned body aliases p.
func DecodeCallPayload(p []byte) (method string, body []byte, err error) {
	if len(p) < 2 {
		return "", nil, fmt.Errorf("%w: call payload %d bytes, need at least 2",
			ErrMalformedPayload, len(p))
	}
	n := int(binary.BigEndian.Uint16(p[:2]))
	if n == 0 {
		return "", nil, fmt.Errorf("%w: empty method name", ErrMalformedPayload)
	}
	if len(p) < 2+n {
		return "", nil, fmt.Errorf("%w: method length %d exceeds payload (%d bytes)",
			ErrMalformedPayload, n, len(p))
	}
	return string(p[2 : 2+n]), p[2+n:], nil
}

// ErrorCode classifies a failure. Values mirror the subset of gRPC codes that
// carry meaning here, so a future gRPC transport maps across without
// translation loss.
type ErrorCode uint16

const (
	CodeUnknown         ErrorCode = 2
	CodeInvalidArgument ErrorCode = 3
	CodeNotFound        ErrorCode = 5
	CodeCanceled        ErrorCode = 1
	CodeInternal        ErrorCode = 13
	CodeUnavailable     ErrorCode = 14
	CodeUnimplemented   ErrorCode = 12
	CodeDeadlineExceed  ErrorCode = 4
)

// String implements fmt.Stringer.
func (c ErrorCode) String() string {
	switch c {
	case CodeCanceled:
		return "Canceled"
	case CodeUnknown:
		return "Unknown"
	case CodeInvalidArgument:
		return "InvalidArgument"
	case CodeDeadlineExceed:
		return "DeadlineExceeded"
	case CodeNotFound:
		return "NotFound"
	case CodeUnimplemented:
		return "Unimplemented"
	case CodeInternal:
		return "Internal"
	case CodeUnavailable:
		return "Unavailable"
	default:
		return fmt.Sprintf("ErrorCode(%d)", uint16(c))
	}
}

// maxErrMsgLen bounds an error message from a peer.
const maxErrMsgLen = 8192

// ErrorPayload is a decoded ERROR frame payload.
type ErrorPayload struct {
	Code    ErrorCode
	Message string
	// Details is opaque, typically a JS stack trace. It may be empty.
	Details []byte
}

// AppendErrorPayload encodes an ERROR payload:
// [code uint16][msgLen uint16][msg][details].
func AppendErrorPayload(dst []byte, e ErrorPayload) ([]byte, error) {
	if len(e.Message) > maxErrMsgLen {
		e.Message = e.Message[:maxErrMsgLen]
	}
	dst = binary.BigEndian.AppendUint16(dst, uint16(e.Code))
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(e.Message)))
	dst = append(dst, e.Message...)
	dst = append(dst, e.Details...)
	return dst, nil
}

// DecodeErrorPayload decodes an ERROR payload. Details aliases p.
func DecodeErrorPayload(p []byte) (ErrorPayload, error) {
	if len(p) < 4 {
		return ErrorPayload{}, fmt.Errorf("%w: error payload %d bytes, need at least 4",
			ErrMalformedPayload, len(p))
	}
	e := ErrorPayload{Code: ErrorCode(binary.BigEndian.Uint16(p[:2]))}
	n := int(binary.BigEndian.Uint16(p[2:4]))
	if len(p) < 4+n {
		return ErrorPayload{}, fmt.Errorf("%w: message length %d exceeds payload (%d bytes)",
			ErrMalformedPayload, n, len(p))
	}
	e.Message = string(p[4 : 4+n])
	if len(p) > 4+n {
		e.Details = p[4+n:]
	}
	return e, nil
}

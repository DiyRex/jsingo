package wire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		f    Frame
	}{
		{"call with payload", Frame{Type: TypeCall, ID: 1, Payload: []byte("hello")}},
		{"reply empty payload", Frame{Type: TypeReply, ID: 2}},
		{"cancel no payload", Frame{Type: TypeCancel, ID: 3}},
		{"max id", Frame{Type: TypePong, ID: ^uint64(0), Payload: []byte{0}}},
		{"log id zero", Frame{Type: TypeLog, ID: 0, Payload: []byte(`{"lvl":"info"}`)}},
		{"binary payload", Frame{Type: TypeReply, ID: 9, Payload: []byte{0x00, 0xff, 0x7f, 0x80}}},
		{"large payload", Frame{Type: TypeReply, ID: 10, Payload: bytes.Repeat([]byte("x"), 1<<16)}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			enc, err := tc.f.AppendTo(nil, 0)
			if err != nil {
				t.Fatalf("AppendTo: %v", err)
			}
			if len(enc) != tc.f.Size() {
				t.Errorf("Size() = %d, encoded %d bytes", tc.f.Size(), len(enc))
			}

			got, n, err := DecodeFrame(enc, 0)
			if err != nil {
				t.Fatalf("DecodeFrame: %v", err)
			}
			if n != len(enc) {
				t.Errorf("consumed %d bytes, want %d", n, len(enc))
			}
			if got.Type != tc.f.Type || got.ID != tc.f.ID {
				t.Errorf("got type=%v id=%d, want type=%v id=%d",
					got.Type, got.ID, tc.f.Type, tc.f.ID)
			}
			if !bytes.Equal(got.Payload, tc.f.Payload) {
				t.Errorf("payload mismatch: got %d bytes, want %d",
					len(got.Payload), len(tc.f.Payload))
			}
		})
	}
}

func TestFrameAppendToPreservesPrefix(t *testing.T) {
	t.Parallel()

	prefix := []byte("keep me")
	out, err := Frame{Type: TypePing, ID: 1}.AppendTo(prefix, 0)
	if err != nil {
		t.Fatalf("AppendTo: %v", err)
	}
	if !bytes.HasPrefix(out, prefix) {
		t.Fatalf("AppendTo clobbered the destination slice")
	}
}

func TestDecodeFrameRejects(t *testing.T) {
	t.Parallel()

	// A well-formed frame we can corrupt in specific ways.
	valid, err := Frame{Type: TypeCall, ID: 1, Payload: []byte("body")}.AppendTo(nil, 0)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	withLen := func(n uint32, rest ...byte) []byte {
		b := binary.BigEndian.AppendUint32(nil, n)
		return append(b, rest...)
	}

	tests := []struct {
		name string
		in   []byte
		max  uint32
		want error
	}{
		{"empty", nil, 0, ErrFrameTooSmall},
		{"partial prefix", []byte{0, 0}, 0, ErrFrameTooSmall},
		{"length zero", withLen(0), 0, ErrFrameTooSmall},
		{"length below header", withLen(8), 0, ErrFrameTooSmall},
		{"truncated body", valid[:len(valid)-1], 0, ErrFrameTooSmall},
		{"length exceeds max", withLen(5000), 100, ErrFrameTooLarge},
		{"unknown type zero", withLen(9, 0, 0, 0, 0, 0, 0, 0, 0, 0), 0, ErrUnknownType},
		{"unknown type high", withLen(9, 99, 0, 0, 0, 0, 0, 0, 0, 0), 0, ErrUnknownType},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := DecodeFrame(tc.in, tc.max)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestAppendToRejectsOversize(t *testing.T) {
	t.Parallel()

	f := Frame{Type: TypeReply, ID: 1, Payload: bytes.Repeat([]byte("x"), 200)}
	if _, err := f.AppendTo(nil, 100); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("got %v, want ErrFrameTooLarge", err)
	}
}

func TestFrameCloneDetaches(t *testing.T) {
	t.Parallel()

	orig := Frame{Type: TypeReply, ID: 1, Payload: []byte("abc")}
	clone := orig.Clone()
	orig.Payload[0] = 'z'

	if clone.Payload[0] != 'a' {
		t.Fatal("Clone aliases the original payload")
	}
	if nilClone := (Frame{Type: TypePing}).Clone(); nilClone.Payload != nil {
		t.Fatal("Clone of a nil payload should stay nil")
	}
}

func TestTypeValidAndString(t *testing.T) {
	t.Parallel()

	for _, tt := range []Type{TypeCall, TypeReply, TypeError, TypeCancel, TypeLog, TypePing, TypePong} {
		if !tt.Valid() {
			t.Errorf("%v should be valid", tt)
		}
		if strings.HasPrefix(tt.String(), "Type(") {
			t.Errorf("%d has no String case", uint8(tt))
		}
	}
	for _, tt := range []Type{0, 8, 255} {
		if tt.Valid() {
			t.Errorf("Type(%d) should be invalid", uint8(tt))
		}
	}
}

func TestCallPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, method string
		body         []byte
	}{
		{"typical", "parseArticle", []byte(`{"html":"<p>hi</p>"}`)},
		{"empty body", "ping", nil},
		{"unicode method", "parseArtículo", []byte("{}")},
		{"body with nulls", "f", []byte{0, 1, 0, 2}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p, err := AppendCallPayload(nil, tc.method, tc.body)
			if err != nil {
				t.Fatalf("AppendCallPayload: %v", err)
			}
			method, body, err := DecodeCallPayload(p)
			if err != nil {
				t.Fatalf("DecodeCallPayload: %v", err)
			}
			if method != tc.method {
				t.Errorf("method = %q, want %q", method, tc.method)
			}
			if !bytes.Equal(body, tc.body) {
				t.Errorf("body = %q, want %q", body, tc.body)
			}
		})
	}
}

func TestCallPayloadRejects(t *testing.T) {
	t.Parallel()

	if _, err := AppendCallPayload(nil, "", nil); !errors.Is(err, ErrMalformedPayload) {
		t.Errorf("empty method: got %v, want ErrMalformedPayload", err)
	}
	long := strings.Repeat("a", maxMethodLen+1)
	if _, err := AppendCallPayload(nil, long, nil); !errors.Is(err, ErrMalformedPayload) {
		t.Errorf("long method: got %v, want ErrMalformedPayload", err)
	}

	decodes := []struct {
		name string
		in   []byte
	}{
		{"empty", nil},
		{"one byte", []byte{0}},
		{"zero length name", []byte{0, 0}},
		{"name longer than payload", []byte{0, 200, 'a', 'b'}},
	}
	for _, tc := range decodes {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := DecodeCallPayload(tc.in); !errors.Is(err, ErrMalformedPayload) {
				t.Fatalf("got %v, want ErrMalformedPayload", err)
			}
		})
	}
}

func TestErrorPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		e    ErrorPayload
	}{
		{"full", ErrorPayload{CodeNotFound, "no article", []byte("stack\n at x")}},
		{"no details", ErrorPayload{CodeInternal, "boom", nil}},
		{"empty message", ErrorPayload{CodeUnknown, "", nil}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p, err := AppendErrorPayload(nil, tc.e)
			if err != nil {
				t.Fatalf("AppendErrorPayload: %v", err)
			}
			got, err := DecodeErrorPayload(p)
			if err != nil {
				t.Fatalf("DecodeErrorPayload: %v", err)
			}
			if got.Code != tc.e.Code || got.Message != tc.e.Message {
				t.Errorf("got %+v, want %+v", got, tc.e)
			}
			if !bytes.Equal(got.Details, tc.e.Details) {
				t.Errorf("details = %q, want %q", got.Details, tc.e.Details)
			}
		})
	}
}

func TestErrorPayloadTruncatesLongMessage(t *testing.T) {
	t.Parallel()

	e := ErrorPayload{Code: CodeInternal, Message: strings.Repeat("m", maxErrMsgLen*2)}
	p, err := AppendErrorPayload(nil, e)
	if err != nil {
		t.Fatalf("AppendErrorPayload: %v", err)
	}
	got, err := DecodeErrorPayload(p)
	if err != nil {
		t.Fatalf("DecodeErrorPayload: %v", err)
	}
	if len(got.Message) != maxErrMsgLen {
		t.Fatalf("message length = %d, want %d", len(got.Message), maxErrMsgLen)
	}
}

func TestErrorPayloadRejects(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		in   []byte
	}{
		{"empty", nil},
		{"short", []byte{0, 5, 0}},
		{"message longer than payload", []byte{0, 5, 0, 200, 'a'}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := DecodeErrorPayload(tc.in); !errors.Is(err, ErrMalformedPayload) {
				t.Fatalf("got %v, want ErrMalformedPayload", err)
			}
		})
	}
}

func TestErrorCodeString(t *testing.T) {
	t.Parallel()

	if got := CodeNotFound.String(); got != "NotFound" {
		t.Errorf("CodeNotFound = %q", got)
	}
	if got := ErrorCode(999).String(); !strings.HasPrefix(got, "ErrorCode(") {
		t.Errorf("unknown code = %q", got)
	}
}

// --- Reader / Writer ------------------------------------------------------

func TestReaderWriterStream(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := NewWriter(&buf, 0)

	want := []Frame{
		{Type: TypeCall, ID: 1, Payload: []byte("first")},
		{Type: TypeCancel, ID: 1},
		{Type: TypeReply, ID: 2, Payload: bytes.Repeat([]byte("y"), 5000)},
		{Type: TypeLog, ID: 0, Payload: []byte(`{"lvl":"warn"}`)},
	}
	for _, f := range want {
		if err := w.WriteFrame(f); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
	}

	r := NewReader(&buf, 0)
	for i, expect := range want {
		got, err := r.ReadFrame()
		if err != nil {
			t.Fatalf("frame %d: ReadFrame: %v", i, err)
		}
		if got.Type != expect.Type || got.ID != expect.ID {
			t.Errorf("frame %d: got type=%v id=%d, want type=%v id=%d",
				i, got.Type, got.ID, expect.Type, expect.ID)
		}
		if !bytes.Equal(got.Payload, expect.Payload) {
			t.Errorf("frame %d: payload mismatch", i)
		}
	}
	if _, err := r.ReadFrame(); !errors.Is(err, io.EOF) {
		t.Fatalf("at stream end: got %v, want io.EOF", err)
	}
}

// A stream cut mid-frame must be distinguishable from an orderly close: the
// supervisor treats the former as a crash and the latter as a shutdown.
func TestReaderTruncatedStreamIsUnexpectedEOF(t *testing.T) {
	t.Parallel()

	enc, err := Frame{Type: TypeReply, ID: 1, Payload: []byte("truncated")}.AppendTo(nil, 0)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	for _, cut := range []int{prefixSize, prefixSize + 3, len(enc) - 1} {
		r := NewReader(bytes.NewReader(enc[:cut]), 0)
		if _, err := r.ReadFrame(); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("cut at %d: got %v, want io.ErrUnexpectedEOF", cut, err)
		}
	}
}

func TestReaderRejectsOversizeFrame(t *testing.T) {
	t.Parallel()

	// Claim a huge frame without sending it: the Reader must reject on the
	// length prefix alone and never attempt the allocation.
	hostile := binary.BigEndian.AppendUint32(nil, 1<<30)
	r := NewReader(bytes.NewReader(hostile), 4096)
	if _, err := r.ReadFrame(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("got %v, want ErrFrameTooLarge", err)
	}
}

// ReadFrame documents that payloads alias a reused buffer. Pin the contract so
// a future optimisation cannot silently change it.
func TestReaderPayloadAliasingContract(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := NewWriter(&buf, 0)
	for _, p := range []string{"aaaa", "bbbb"} {
		if err := w.WriteFrame(Frame{Type: TypeReply, ID: 1, Payload: []byte(p)}); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
	}

	r := NewReader(&buf, 0)
	first, err := r.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	retained := first.Clone()

	if _, err := r.ReadFrame(); err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if string(retained.Payload) != "aaaa" {
		t.Fatalf("cloned payload was overwritten: %q", retained.Payload)
	}
}

type errWriter struct{ err error }

func (e errWriter) Write([]byte) (int, error) { return 0, e.err }

func TestWriterStickyError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("socket closed")
	// bufio only surfaces the underlying error on flush, which WriteFrame
	// always performs.
	w := NewWriter(errWriter{sentinel}, 0)

	if err := w.WriteFrame(Frame{Type: TypePing, ID: 1}); !errors.Is(err, sentinel) {
		t.Fatalf("first write: got %v, want %v", err, sentinel)
	}
	if err := w.WriteFrame(Frame{Type: TypePing, ID: 2}); !errors.Is(err, sentinel) {
		t.Fatalf("second write should return the sticky error, got %v", err)
	}
	if !errors.Is(w.Err(), sentinel) {
		t.Fatalf("Err() = %v, want %v", w.Err(), sentinel)
	}
}

// An oversize frame is the caller's mistake, not a transport failure, so it
// must not poison the connection.
func TestWriterEncodeErrorDoesNotPoison(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := NewWriter(&buf, 64)

	big := Frame{Type: TypeReply, ID: 1, Payload: bytes.Repeat([]byte("x"), 100)}
	if err := w.WriteFrame(big); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("got %v, want ErrFrameTooLarge", err)
	}
	if w.Err() != nil {
		t.Fatalf("encode failure poisoned the writer: %v", w.Err())
	}
	if err := w.WriteFrame(Frame{Type: TypePing, ID: 2}); err != nil {
		t.Fatalf("writer unusable after encode error: %v", err)
	}
}

func TestWriterConcurrentFramesDoNotInterleave(t *testing.T) {
	t.Parallel()

	const writers, each = 16, 50

	var buf lockedBuffer
	w := NewWriter(&buf, 0)

	done := make(chan struct{})
	for i := range writers {
		go func(id int) {
			defer func() { done <- struct{}{} }()
			payload := bytes.Repeat([]byte{byte(id)}, 64)
			for range each {
				if err := w.WriteFrame(Frame{Type: TypeCall, ID: uint64(id), Payload: payload}); err != nil {
					t.Errorf("WriteFrame: %v", err)
					return
				}
			}
		}(i)
	}
	for range writers {
		<-done
	}

	// Every frame must decode cleanly and its payload must be uniform: any
	// interleaving would corrupt the framing or mix payload bytes.
	r := NewReader(bytes.NewReader(buf.Bytes()), 0)
	count := 0
	for {
		f, err := r.ReadFrame()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("frame %d: %v", count, err)
		}
		want := bytes.Repeat([]byte{byte(f.ID)}, 64)
		if !bytes.Equal(f.Payload, want) {
			t.Fatalf("frame %d (id %d): interleaved payload", count, f.ID)
		}
		count++
	}
	if count != writers*each {
		t.Fatalf("read %d frames, want %d", count, writers*each)
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Bytes()
}

func BenchmarkFrameRoundTrip(b *testing.B) {
	f := Frame{Type: TypeCall, ID: 42, Payload: bytes.Repeat([]byte("x"), 512)}
	buf := make([]byte, 0, f.Size())

	b.ReportAllocs()
	for b.Loop() {
		var err error
		buf, err = f.AppendTo(buf[:0], 0)
		if err != nil {
			b.Fatal(err)
		}
		if _, _, err := DecodeFrame(buf, 0); err != nil {
			b.Fatal(err)
		}
	}
}

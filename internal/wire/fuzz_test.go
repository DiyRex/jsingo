package wire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// The decoders sit on the trust boundary: every byte they see comes from a
// separate process running third-party npm code. These fuzzers assert the only
// two properties that matter there — no panic, and no allocation a peer can
// steer — rather than checking specific outputs.

func FuzzDecodeFrame(f *testing.F) {
	seed := func(fr Frame) []byte {
		b, err := fr.AppendTo(nil, 0)
		if err != nil {
			f.Fatalf("seed: %v", err)
		}
		return b
	}

	f.Add(seed(Frame{Type: TypeCall, ID: 1, Payload: []byte("hello")}))
	f.Add(seed(Frame{Type: TypeReply, ID: ^uint64(0)}))
	f.Add(seed(Frame{Type: TypeError, ID: 7, Payload: []byte{0, 5, 0, 2, 'h', 'i'}}))
	f.Add(seed(Frame{Type: TypeLog, ID: 0, Payload: []byte(`{"lvl":"info"}`)}))
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0})
	f.Add([]byte{0, 0, 0, 0})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 1})             // huge length, no body
	f.Add([]byte{0, 0, 0, 9, 0, 0, 0, 0, 0, 0, 0, 0, 0}) // type 0

	const max = 1 << 16

	f.Fuzz(func(t *testing.T, data []byte) {
		fr, n, err := DecodeFrame(data, max)
		if err != nil {
			if n != 0 {
				t.Fatalf("consumed %d bytes on error", n)
			}
			return
		}

		if n <= 0 || n > len(data) {
			t.Fatalf("consumed %d bytes from %d", n, len(data))
		}
		if !fr.Type.Valid() {
			t.Fatalf("decoded invalid type %d", uint8(fr.Type))
		}
		if len(fr.Payload) > max {
			t.Fatalf("payload %d exceeds max %d", len(fr.Payload), max)
		}

		// Re-encoding a decoded frame must reproduce the exact input bytes.
		// This is what guarantees a peer cannot smuggle two readings of the
		// same wire bytes past us.
		out, err := fr.AppendTo(nil, max)
		if err != nil {
			t.Fatalf("re-encode failed: %v", err)
		}
		if !bytes.Equal(out, data[:n]) {
			t.Fatalf("round trip differs:\n got %x\nwant %x", out, data[:n])
		}
	})
}

// The Reader has its own framing state (buffer reuse, io.ReadFull boundaries)
// that DecodeFrame does not exercise, so it is fuzzed separately over
// multi-frame streams.
func FuzzReaderStream(f *testing.F) {
	var buf bytes.Buffer
	w := NewWriter(&buf, 0)
	for _, fr := range []Frame{
		{Type: TypeCall, ID: 1, Payload: []byte("a")},
		{Type: TypeReply, ID: 1, Payload: []byte("b")},
		{Type: TypeCancel, ID: 2},
	} {
		if err := w.WriteFrame(fr); err != nil {
			f.Fatalf("seed: %v", err)
		}
	}
	f.Add(buf.Bytes())
	f.Add([]byte{0, 0, 0, 9, 1, 0, 0, 0, 0, 0, 0, 0, 1})
	f.Add([]byte{0, 0, 0, 0})

	const max = 1 << 16

	f.Fuzz(func(t *testing.T, data []byte) {
		r := NewReader(bytes.NewReader(data), max)
		// Bound the loop: a valid stream of empty frames yields one frame per
		// HeaderSize bytes, so anything beyond that means the Reader failed to
		// advance and would spin forever in production.
		limit := len(data)/HeaderSize + 2
		for i := 0; ; i++ {
			if i > limit {
				t.Fatalf("reader did not terminate after %d frames from %d bytes", i, len(data))
			}
			fr, err := r.ReadFrame()
			if err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
					errors.Is(err, ErrFrameTooLarge) || errors.Is(err, ErrFrameTooSmall) ||
					errors.Is(err, ErrUnknownType) {
					return
				}
				t.Fatalf("unclassified reader error: %v", err)
			}
			if !fr.Type.Valid() {
				t.Fatalf("reader produced invalid type %d", uint8(fr.Type))
			}
		}
	})
}

func FuzzDecodeCall(f *testing.F) {
	add := func(method string, body []byte) {
		p, err := AppendCallPayload(nil, method, body)
		if err != nil {
			f.Fatalf("seed: %v", err)
		}
		f.Add(p)
	}
	add("parseArticle", []byte(`{"html":"x"}`))
	add("f", nil)
	f.Add([]byte{})
	f.Add([]byte{0, 0})
	f.Add([]byte{0xff, 0xff, 'a'}) // length far beyond the payload

	f.Fuzz(func(t *testing.T, data []byte) {
		method, body, err := DecodeCallPayload(data)
		if err != nil {
			return
		}
		if method == "" {
			t.Fatal("decoded an empty method name")
		}
		if len(method)+len(body)+2 != len(data) {
			t.Fatalf("method %d + body %d + 2 != input %d",
				len(method), len(body), len(data))
		}
	})
}

func FuzzDecodeError(f *testing.F) {
	p, err := AppendErrorPayload(nil, ErrorPayload{CodeNotFound, "missing", []byte("stack")})
	if err != nil {
		f.Fatalf("seed: %v", err)
	}
	f.Add(p)
	f.Add([]byte{})
	f.Add([]byte{0, 5, 0xff, 0xff})

	f.Fuzz(func(t *testing.T, data []byte) {
		e, err := DecodeErrorPayload(data)
		if err != nil {
			return
		}
		if len(e.Message)+len(e.Details)+4 != len(data) {
			t.Fatalf("message %d + details %d + 4 != input %d",
				len(e.Message), len(e.Details), len(data))
		}
	})
}

// A peer must never be able to make the Reader allocate more than max, no
// matter what the length prefix claims.
func FuzzReaderAllocationBound(f *testing.F) {
	f.Add(uint32(1 << 30))
	f.Add(uint32(0))
	f.Add(uint32(9))
	f.Add(^uint32(0))

	const max = 4096

	f.Fuzz(func(t *testing.T, claimed uint32) {
		// A length prefix with no body behind it.
		data := binary.BigEndian.AppendUint32(nil, claimed)
		r := NewReader(bytes.NewReader(data), max)

		_, err := r.ReadFrame()
		if err == nil {
			t.Fatalf("claimed %d bytes with an empty body but decode succeeded", claimed)
		}
		if claimed > max && !errors.Is(err, ErrFrameTooLarge) {
			t.Fatalf("claimed %d > max %d: got %v, want ErrFrameTooLarge", claimed, max, err)
		}
		if cap(r.buf) > max {
			t.Fatalf("allocated %d bytes for a claim of %d, max %d", cap(r.buf), claimed, max)
		}
	})
}

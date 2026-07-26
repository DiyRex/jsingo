package wire

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

// defaultBufSize is the read/write buffer size. Large enough that small frames
// batch into one syscall, small enough to stay off the large-object heap.
const defaultBufSize = 64 << 10

// Reader decodes frames from a stream.
//
// A Reader is not safe for concurrent use: the protocol assigns exactly one
// goroutine to the read loop, which fans out to waiters via the mux.
type Reader struct {
	br  *bufio.Reader
	max uint32
	// buf is reused across reads. See ReadFrame's aliasing contract.
	buf []byte
}

// NewReader returns a Reader over r. A max of 0 selects DefaultMaxFrameSize.
func NewReader(r io.Reader, max uint32) *Reader {
	if max == 0 {
		max = DefaultMaxFrameSize
	}
	return &Reader{br: bufio.NewReaderSize(r, defaultBufSize), max: max}
}

// ReadFrame reads the next frame.
//
// The returned Frame's Payload aliases an internal buffer that the next
// ReadFrame call overwrites. Callers that retain a frame past the next read
// must use [Frame.Clone]. This keeps the steady state allocation-free; the
// single read loop is the only caller, and it clones exactly once when handing
// a payload to a waiting goroutine.
//
// At a clean frame boundary ReadFrame returns io.EOF. A stream that ends
// mid-frame returns io.ErrUnexpectedEOF, which distinguishes an orderly
// sidecar shutdown from a crash.
func (r *Reader) ReadFrame() (Frame, error) {
	var prefix [prefixSize]byte
	if _, err := io.ReadFull(r.br, prefix[:]); err != nil {
		// ReadFull maps a zero-byte read to EOF and a partial read to
		// ErrUnexpectedEOF, which is exactly the distinction we want.
		return Frame{}, err
	}

	body := binary.BigEndian.Uint32(prefix[:])
	if err := checkBodyLen(body, r.max); err != nil {
		return Frame{}, err
	}

	if cap(r.buf) < int(body) {
		r.buf = make([]byte, body)
	}
	r.buf = r.buf[:body]

	if _, err := io.ReadFull(r.br, r.buf); err != nil {
		if errors.Is(err, io.EOF) {
			// The prefix promised bytes that never arrived: always unexpected.
			return Frame{}, io.ErrUnexpectedEOF
		}
		return Frame{}, err
	}
	return decodeBody(r.buf)
}

// Writer encodes frames to a stream.
//
// Unlike Reader, Writer is safe for concurrent use: many goroutines issue
// calls and cancellations against one connection. Frames are written whole
// under the lock so they never interleave.
type Writer struct {
	mu  sync.Mutex
	bw  *bufio.Writer
	max uint32
	// scratch is the encode buffer, reused under mu.
	scratch []byte
	err     error
}

// NewWriter returns a Writer over w. A max of 0 selects DefaultMaxFrameSize.
func NewWriter(w io.Writer, max uint32) *Writer {
	if max == 0 {
		max = DefaultMaxFrameSize
	}
	return &Writer{bw: bufio.NewWriterSize(w, defaultBufSize), max: max}
}

// WriteFrame encodes and flushes one frame.
//
// Every frame is flushed rather than batched. Buffering would deadlock
// request/response traffic: a caller blocks on a reply that is still sitting
// in the local write buffer. Throughput comes from multiplexing concurrent
// calls, not from coalescing writes.
func (w *Writer) WriteFrame(f Frame) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.err != nil {
		return w.err
	}

	w.scratch = w.scratch[:0]
	buf, err := f.AppendTo(w.scratch, w.max)
	if err != nil {
		// Encoding failures are the caller's fault, not the connection's:
		// do not poison the Writer.
		return err
	}
	w.scratch = buf

	if _, err := w.bw.Write(buf); err != nil {
		w.err = fmt.Errorf("wire: write %s frame: %w", f.Type, err)
		return w.err
	}
	if err := w.bw.Flush(); err != nil {
		w.err = fmt.Errorf("wire: flush %s frame: %w", f.Type, err)
		return w.err
	}
	return nil
}

// Err returns the sticky write error, if the connection has failed.
func (w *Writer) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

package wire

import (
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// fakePeer is a Go implementation of the sidecar side of the protocol. It
// exists so the mux can be tested exhaustively - including cancellation and
// crash paths - on a machine with no JavaScript runtime installed.
type fakePeer struct {
	t  *testing.T
	r  *Reader
	w  *Writer
	rw io.Closer

	// handler answers a call. Returning a nil frame means "send nothing",
	// which models a handler that hangs or a reply lost to a cancellation.
	handler func(p *fakePeer, id uint64, method string, body []byte) *Frame

	mu        sync.Mutex
	cancelled map[uint64]bool
	calls     int

	stopped chan struct{}
}

// newPipePeer wires a Mux to a fakePeer over an in-memory socketpair and
// registers cleanup. It returns the mux and the peer.
func newPipePeer(t *testing.T, h func(p *fakePeer, id uint64, method string, body []byte) *Frame) (*Mux, *fakePeer) {
	t.Helper()

	// net.Pipe is synchronous and unbuffered, which would deadlock a protocol
	// that writes before reading. A real socketpair matches production.
	c1, c2 := socketPair(t)

	m := NewMux(NewReader(c1, 0), NewWriter(c1, 0), nil)
	p := &fakePeer{
		t:         t,
		r:         NewReader(c2, 0),
		w:         NewWriter(c2, 0),
		rw:        c2,
		handler:   h,
		cancelled: make(map[uint64]bool),
		stopped:   make(chan struct{}),
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- m.Serve() }()
	go p.serve()

	t.Cleanup(func() {
		_ = m.Close()
		_ = c1.Close()
		p.stop()
		select {
		case <-serveErr:
		case <-time.After(2 * time.Second):
			t.Error("Mux.Serve did not return within 2s")
		}
	})
	return m, p
}

func socketPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	type res struct {
		c   net.Conn
		err error
	}
	accepted := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		accepted <- res{c, err}
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	got := <-accepted
	if got.err != nil {
		t.Fatalf("accept: %v", got.err)
	}
	return client, got.c
}

func (p *fakePeer) serve() {
	defer close(p.stopped)
	for {
		f, err := p.r.ReadFrame()
		if err != nil {
			return // connection closed; the test is finished with us
		}

		switch f.Type {
		case TypeCall:
			method, body, err := DecodeCallPayload(f.Payload)
			if err != nil {
				p.send(Frame{Type: TypeError, ID: f.ID, Payload: mustErrPayload(p.t, ErrorPayload{
					Code: CodeInvalidArgument, Message: err.Error(),
				})})
				continue
			}

			p.mu.Lock()
			p.calls++
			p.mu.Unlock()

			// Copy the body: it aliases the reader's buffer, and handlers may
			// answer from another goroutine.
			bodyCopy := append([]byte(nil), body...)
			if reply := p.handler(p, f.ID, method, bodyCopy); reply != nil {
				p.send(*reply)
			}

		case TypeCancel:
			p.mu.Lock()
			p.cancelled[f.ID] = true
			p.mu.Unlock()

		case TypePing:
			p.send(Frame{Type: TypePong, ID: f.ID})

		default:
			p.t.Errorf("fakePeer got unexpected %s frame", f.Type)
		}
	}
}

func (p *fakePeer) send(f Frame) {
	if err := p.w.WriteFrame(f); err != nil && !isClosedConn(err) {
		p.t.Errorf("fakePeer write %s: %v", f.Type, err)
	}
}

// crash closes the connection without a graceful shutdown, modelling a
// sidecar that segfaults or is killed with SIGKILL.
func (p *fakePeer) crash() { _ = p.rw.Close() }

func (p *fakePeer) stop() {
	_ = p.rw.Close()
	<-p.stopped
}

func (p *fakePeer) wasCancelled(id uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cancelled[id]
}

func (p *fakePeer) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// waitCancelled polls until id is cancelled or the deadline passes. The CANCEL
// frame travels asynchronously, so tests cannot assert on it synchronously.
func (p *fakePeer) waitCancelled(id uint64, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if p.wasCancelled(id) {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

func isClosedConn(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrClosedPipe)
}

func mustErrPayload(t *testing.T, e ErrorPayload) []byte {
	t.Helper()
	p, err := AppendErrorPayload(nil, e)
	if err != nil {
		t.Fatalf("AppendErrorPayload: %v", err)
	}
	return p
}

// echo is the default handler: it returns the request body unchanged.
func echo(_ *fakePeer, id uint64, _ string, body []byte) *Frame {
	return &Frame{Type: TypeReply, ID: id, Payload: body}
}

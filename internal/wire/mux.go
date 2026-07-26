package wire

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// Mux multiplexes concurrent calls over one connection.
//
// One goroutine runs [Mux.Serve], reading frames and routing replies to the
// goroutine waiting on each call id. Any number of goroutines may call
// [Mux.Call] concurrently.
//
// Lifecycle: Serve runs until the connection fails or [Mux.Close] is called.
// Either way every in-flight and subsequent call fails with the shutdown
// cause, so no caller can block forever on a dead sidecar.
type Mux struct {
	w   *Writer
	r   *Reader
	log func(Frame)

	// nextID is odd-incremented from 1. Zero is reserved for LOG frames, so a
	// zero id in a reply is always a peer bug rather than a real call.
	nextID atomic.Uint64

	mu      sync.Mutex
	pending map[uint64]chan<- result
	closed  bool
	cause   error

	// done closes exactly once, when the mux stops. Serve's return value is
	// the authoritative error; cause carries it to blocked callers.
	done     chan struct{}
	doneOnce sync.Once
}

type result struct {
	frame Frame // cloned; safe to retain
	err   error
}

// Mux errors.
var (
	// ErrClosed means the mux was shut down before or during a call.
	ErrClosed = errors.New("wire: mux closed")
	// ErrProtocol means the peer violated the protocol.
	ErrProtocol = errors.New("wire: protocol violation")
)

// CallError is a structured failure reported by the peer via an ERROR frame.
type CallError struct {
	Code    ErrorCode
	Message string
	Details []byte
}

func (e *CallError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("jsingo: %s", e.Code)
	}
	return fmt.Sprintf("jsingo: %s: %s", e.Code, e.Message)
}

// Is lets callers match on code alone: errors.Is(err, &CallError{Code: ...}).
func (e *CallError) Is(target error) bool {
	t, ok := target.(*CallError)
	return ok && t.Code == e.Code && t.Message == ""
}

// NewMux returns a Mux over r and w. Log receives LOG frames from the peer; a
// nil log drops them. Log is called on the read goroutine, so it must not
// block.
func NewMux(r *Reader, w *Writer, log func(Frame)) *Mux {
	return &Mux{
		w:       w,
		r:       r,
		log:     log,
		pending: make(map[uint64]chan<- result),
		done:    make(chan struct{}),
	}
}

// Serve reads frames until the connection ends or Close is called, routing
// each to its waiting caller. It returns the error that stopped it, or nil
// after a clean Close or a peer EOF at a frame boundary.
//
// Exactly one goroutine may run Serve.
func (m *Mux) Serve() error {
	err := m.readLoop()
	m.shutdown(err)
	return err
}

func (m *Mux) readLoop() error {
	for {
		f, err := m.r.ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil // orderly close at a frame boundary
			}
			return err
		}

		switch f.Type {
		case TypeReply, TypeError:
			// Clone before handing off: the payload aliases the Reader's
			// buffer, which the next iteration overwrites.
			m.deliver(f.ID, result{frame: f.Clone()})

		case TypeLog:
			if m.log != nil {
				m.log(f)
			}

		case TypePing:
			// Answer immediately, echoing the id. A write failure here means
			// the connection is gone; let the next read surface it.
			_ = m.w.WriteFrame(Frame{Type: TypePong, ID: f.ID})

		case TypePong:
			m.deliver(f.ID, result{frame: f.Clone()})

		case TypeCall, TypeCancel:
			// Go is the client; the peer must never originate these.
			return fmt.Errorf("%w: peer sent %s", ErrProtocol, f.Type)

		default:
			// Unreachable: ReadFrame rejects unknown types.
			return fmt.Errorf("%w: unhandled %s", ErrProtocol, f.Type)
		}
	}
}

// deliver routes a result to the caller waiting on id, if any.
//
// A missing id is not an error: the caller may have already given up after a
// cancellation, and the peer's reply lost the race. Dropping it is correct.
func (m *Mux) deliver(id uint64, res result) {
	m.mu.Lock()
	ch, ok := m.pending[id]
	if ok {
		delete(m.pending, id)
	}
	m.mu.Unlock()

	if ok {
		// Buffered by construction, so this never blocks the read loop.
		ch <- res
	}
}

// Call invokes method on the peer and waits for a reply.
//
// If ctx ends first, Call sends a CANCEL frame and returns ctx.Err(). The
// peer's handler observes an AbortSignal; without this a cancelled Go context
// would leave the sidecar burning CPU on a result nobody wants.
//
// The returned payload is owned by the caller.
func (m *Mux) Call(ctx context.Context, method string, body []byte) ([]byte, error) {
	// Fail fast on an already-cancelled context rather than allocating an id
	// and immediately cancelling it.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	id := m.nextID.Add(1)
	ch := make(chan result, 1)

	if err := m.register(id, ch); err != nil {
		return nil, err
	}

	payload, err := AppendCallPayload(nil, method, body)
	if err != nil {
		m.unregister(id)
		return nil, err
	}

	if err := m.w.WriteFrame(Frame{Type: TypeCall, ID: id, Payload: payload}); err != nil {
		m.unregister(id)
		return nil, err
	}

	select {
	case res := <-ch:
		if res.err != nil {
			return nil, res.err
		}
		return decodeResult(res.frame)

	case <-ctx.Done():
		m.unregister(id)
		// Best effort: if the connection is already gone the peer has died
		// anyway, which achieves the same thing.
		_ = m.w.WriteFrame(Frame{Type: TypeCancel, ID: id})
		return nil, ctx.Err()

	case <-m.done:
		m.unregister(id)
		return nil, m.Err()
	}
}

// decodeResult converts a REPLY or ERROR frame into a payload or a CallError.
func decodeResult(f Frame) ([]byte, error) {
	switch f.Type {
	case TypeReply:
		return f.Payload, nil
	case TypeError:
		e, err := DecodeErrorPayload(f.Payload)
		if err != nil {
			return nil, err
		}
		return nil, &CallError{Code: e.Code, Message: e.Message, Details: e.Details}
	default:
		return nil, fmt.Errorf("%w: %s frame answered a call", ErrProtocol, f.Type)
	}
}

// Ping round-trips a PING and waits for the matching PONG.
func (m *Mux) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	id := m.nextID.Add(1)
	ch := make(chan result, 1)

	if err := m.register(id, ch); err != nil {
		return err
	}
	if err := m.w.WriteFrame(Frame{Type: TypePing, ID: id}); err != nil {
		m.unregister(id)
		return err
	}

	select {
	case res := <-ch:
		if res.err != nil {
			return res.err
		}
		if res.frame.Type != TypePong {
			return fmt.Errorf("%w: %s answered a ping", ErrProtocol, res.frame.Type)
		}
		return nil
	case <-ctx.Done():
		m.unregister(id)
		return ctx.Err()
	case <-m.done:
		m.unregister(id)
		return m.Err()
	}
}

// register adds a waiter, refusing if the mux has already shut down.
//
// Checking closed under the same lock that shutdown uses is what prevents a
// call registering after the pending map has been drained, which would block
// until its context expired.
func (m *Mux) register(id uint64, ch chan<- result) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return m.causeLocked()
	}
	m.pending[id] = ch
	return nil
}

func (m *Mux) unregister(id uint64) {
	m.mu.Lock()
	delete(m.pending, id)
	m.mu.Unlock()
}

// shutdown marks the mux closed and fails every waiter exactly once.
func (m *Mux) shutdown(cause error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	m.cause = cause

	// Take the waiters under the lock, notify outside it: a waiter's channel
	// is buffered, but holding the lock across the sends would serialise
	// teardown behind them for no reason.
	waiters := make([]chan<- result, 0, len(m.pending))
	for id, ch := range m.pending {
		waiters = append(waiters, ch)
		delete(m.pending, id)
	}
	err := m.causeLocked()
	m.mu.Unlock()

	for _, ch := range waiters {
		ch <- result{err: err}
	}
	m.doneOnce.Do(func() { close(m.done) })
}

// Close shuts the mux down. In-flight calls fail with ErrClosed. Close is
// idempotent and safe to call concurrently with Serve.
func (m *Mux) Close() error {
	m.shutdown(nil)
	return nil
}

// Done is closed when the mux stops, for callers that want to select on it.
func (m *Mux) Done() <-chan struct{} { return m.done }

// Err returns why the mux stopped, or nil while it is running.
func (m *Mux) Err() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed {
		return nil
	}
	return m.causeLocked()
}

// causeLocked renders the shutdown cause. A nil cause means a deliberate
// Close or a clean peer EOF, both of which present as ErrClosed so callers
// have one sentinel to match.
func (m *Mux) causeLocked() error {
	if m.cause == nil {
		return ErrClosed
	}
	return fmt.Errorf("%w: %w", ErrClosed, m.cause)
}

// InFlight reports how many calls are awaiting a reply. Intended for Stats
// and tests.
func (m *Mux) InFlight() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pending)
}

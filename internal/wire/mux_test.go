package wire

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMuxCallEcho(t *testing.T) {
	t.Parallel()

	m, _ := newPipePeer(t, echo)

	got, err := m.Call(t.Context(), "echo", []byte("hello"))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("got %q, want %q", got, "hello")
	}
	if n := m.InFlight(); n != 0 {
		t.Fatalf("InFlight = %d after completion, want 0", n)
	}
}

func TestMuxCallPassesMethodAndBody(t *testing.T) {
	t.Parallel()

	var gotMethod string
	var gotBody []byte
	m, _ := newPipePeer(t, func(_ *fakePeer, id uint64, method string, body []byte) *Frame {
		gotMethod, gotBody = method, body
		return &Frame{Type: TypeReply, ID: id, Payload: []byte("ok")}
	})

	if _, err := m.Call(t.Context(), "parseArticle", []byte(`{"html":"x"}`)); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if gotMethod != "parseArticle" {
		t.Errorf("method = %q", gotMethod)
	}
	if string(gotBody) != `{"html":"x"}` {
		t.Errorf("body = %q", gotBody)
	}
}

func TestMuxCallError(t *testing.T) {
	t.Parallel()

	m, _ := newPipePeer(t, func(p *fakePeer, id uint64, _ string, _ []byte) *Frame {
		return &Frame{Type: TypeError, ID: id, Payload: mustErrPayload(p.t, ErrorPayload{
			Code:    CodeNotFound,
			Message: "no article content",
			Details: []byte("Error\n    at parse"),
		})}
	})

	_, err := m.Call(t.Context(), "parseArticle", nil)
	if err == nil {
		t.Fatal("want an error")
	}

	var ce *CallError
	if !errors.As(err, &ce) {
		t.Fatalf("got %T (%v), want *CallError", err, err)
	}
	if ce.Code != CodeNotFound || ce.Message != "no article content" {
		t.Errorf("got %+v", ce)
	}
	if !strings.Contains(string(ce.Details), "at parse") {
		t.Errorf("details lost: %q", ce.Details)
	}
	// Callers should be able to match on code alone.
	if !errors.Is(err, &CallError{Code: CodeNotFound}) {
		t.Error("errors.Is by code failed")
	}
	if errors.Is(err, &CallError{Code: CodeInternal}) {
		t.Error("errors.Is matched the wrong code")
	}
}

// Replies arriving out of order must reach the right callers. This is the
// property the whole mux exists to provide.
func TestMuxConcurrentCallsRouteCorrectly(t *testing.T) {
	t.Parallel()

	// Answer from a goroutine after a jittered delay so replies deliberately
	// race and interleave.
	m, _ := newPipePeer(t, func(p *fakePeer, id uint64, _ string, body []byte) *Frame {
		go func() {
			time.Sleep(time.Duration(id%7) * time.Millisecond)
			p.send(Frame{Type: TypeReply, ID: id, Payload: body})
		}()
		return nil
	})

	const n = 100
	var wg sync.WaitGroup
	errs := make(chan error, n)

	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			want := fmt.Sprintf("payload-%d", i)
			got, err := m.Call(t.Context(), "echo", []byte(want))
			if err != nil {
				errs <- fmt.Errorf("call %d: %w", i, err)
				return
			}
			if string(got) != want {
				errs <- fmt.Errorf("call %d: got %q, want %q", i, got, want)
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}
	if n := m.InFlight(); n != 0 {
		t.Errorf("InFlight = %d, want 0", n)
	}
}

func TestMuxCancelSendsCancelFrame(t *testing.T) {
	t.Parallel()

	// A handler that never replies, modelling long-running JS work.
	m, peer := newPipePeer(t, func(*fakePeer, uint64, string, []byte) *Frame { return nil })

	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() {
		_, err := m.Call(ctx, "slow", nil)
		errc <- err
	}()

	// Wait for the call to reach the peer before cancelling, otherwise we may
	// be testing the fail-fast path instead.
	waitFor(t, time.Second, func() bool { return peer.callCount() == 1 })
	cancel()

	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not return after cancellation")
	}

	// The id is 1: it is the first call on this mux.
	if !peer.waitCancelled(1, time.Second) {
		t.Fatal("peer never received a CANCEL frame")
	}
	if n := m.InFlight(); n != 0 {
		t.Fatalf("InFlight = %d after cancellation, want 0", n)
	}
}

func TestMuxCallRespectsDeadline(t *testing.T) {
	t.Parallel()

	m, _ := newPipePeer(t, func(*fakePeer, uint64, string, []byte) *Frame { return nil })

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	_, err := m.Call(ctx, "slow", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want context.DeadlineExceeded", err)
	}
}

func TestMuxCallFailsFastOnCancelledContext(t *testing.T) {
	t.Parallel()

	m, peer := newPipePeer(t, echo)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := m.Call(ctx, "echo", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	// An already-dead context must not reach the peer at all.
	time.Sleep(20 * time.Millisecond)
	if n := peer.callCount(); n != 0 {
		t.Fatalf("peer saw %d calls, want 0", n)
	}
}

// A late reply for a call that already gave up must be dropped silently, not
// mistaken for another call's result.
func TestMuxLateReplyAfterCancelIsDropped(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	m, peer := newPipePeer(t, func(p *fakePeer, id uint64, _ string, _ []byte) *Frame {
		if id == 1 {
			go func() {
				<-release
				p.send(Frame{Type: TypeReply, ID: 1, Payload: []byte("too late")})
			}()
			return nil
		}
		return &Frame{Type: TypeReply, ID: id, Payload: []byte("second")}
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := m.Call(ctx, "slow", nil); !errors.Is(err, context.Canceled) {
			t.Errorf("first call: got %v, want context.Canceled", err)
		}
	}()

	waitFor(t, time.Second, func() bool { return peer.callCount() == 1 })
	cancel()
	<-done

	// Release the abandoned reply, then confirm a fresh call is unaffected.
	close(release)
	time.Sleep(20 * time.Millisecond)

	got, err := m.Call(t.Context(), "echo", nil)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if string(got) != "second" {
		t.Fatalf("second call got %q - a stale reply leaked across calls", got)
	}
}

func TestMuxPing(t *testing.T) {
	t.Parallel()

	m, _ := newPipePeer(t, echo)
	if err := m.Ping(t.Context()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// A sidecar crash must fail every waiting caller promptly rather than leaving
// them blocked until their contexts expire.
func TestMuxPeerCrashFailsInFlightCalls(t *testing.T) {
	t.Parallel()

	m, peer := newPipePeer(t, func(*fakePeer, uint64, string, []byte) *Frame { return nil })

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := m.Call(t.Context(), "slow", nil)
			errs <- err
		}()
	}

	waitFor(t, 2*time.Second, func() bool { return peer.callCount() == n })
	peer.crash()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("callers still blocked 3s after the peer crashed")
	}
	close(errs)

	for err := range errs {
		if !errors.Is(err, ErrClosed) {
			t.Errorf("got %v, want ErrClosed", err)
		}
	}
	if n := m.InFlight(); n != 0 {
		t.Errorf("InFlight = %d after crash, want 0", n)
	}
}

func TestMuxCallAfterCloseFails(t *testing.T) {
	t.Parallel()

	m, _ := newPipePeer(t, echo)
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := m.Call(t.Context(), "echo", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v, want ErrClosed", err)
	}
	if err := m.Ping(t.Context()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Ping: got %v, want ErrClosed", err)
	}
}

func TestMuxCloseIsIdempotentAndConcurrencySafe(t *testing.T) {
	t.Parallel()

	m, _ := newPipePeer(t, echo)

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		}()
	}
	wg.Wait()

	select {
	case <-m.Done():
	case <-time.After(time.Second):
		t.Fatal("Done() not closed after Close")
	}
	if !errors.Is(m.Err(), ErrClosed) {
		t.Fatalf("Err() = %v, want ErrClosed", m.Err())
	}
}

// Go is the client. A peer that sends CALL or CANCEL is out of sync, and
// continuing would let it drive our side of the protocol.
func TestMuxRejectsClientFramesFromPeer(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		typ  Type
	}{
		{"call", TypeCall},
		{"cancel", TypeCancel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c1, c2 := socketPair(t)
			t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })

			m := NewMux(NewReader(c1, 0), NewWriter(c1, 0), nil)
			serveErr := make(chan error, 1)
			go func() { serveErr <- m.Serve() }()

			w := NewWriter(c2, 0)
			payload, err := AppendCallPayload(nil, "sneaky", nil)
			if err != nil {
				t.Fatalf("setup: %v", err)
			}
			if err := w.WriteFrame(Frame{Type: tc.typ, ID: 1, Payload: payload}); err != nil {
				t.Fatalf("write: %v", err)
			}

			select {
			case err := <-serveErr:
				if !errors.Is(err, ErrProtocol) {
					t.Fatalf("got %v, want ErrProtocol", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Serve did not reject the frame")
			}
		})
	}
}

func TestMuxRoutesLogFrames(t *testing.T) {
	t.Parallel()

	c1, c2 := socketPair(t)
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })

	logs := make(chan []byte, 4)
	m := NewMux(NewReader(c1, 0), NewWriter(c1, 0), func(f Frame) {
		logs <- append([]byte(nil), f.Payload...)
	})
	go func() { _ = m.Serve() }()
	t.Cleanup(func() { _ = m.Close() })

	w := NewWriter(c2, 0)
	want := []byte(`{"lvl":"warn","msg":"npm package deprecated"}`)
	if err := w.WriteFrame(Frame{Type: TypeLog, ID: 0, Payload: want}); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case got := <-logs:
		if !bytes.Equal(got, want) {
			t.Fatalf("got %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("log frame not delivered")
	}
}

// The peer answering with a frame type that cannot conclude a call must
// surface as a protocol error to that caller, not hang it.
func TestMuxUnexpectedReplyTypeIsProtocolError(t *testing.T) {
	t.Parallel()

	m, _ := newPipePeer(t, func(_ *fakePeer, id uint64, _ string, _ []byte) *Frame {
		return &Frame{Type: TypePong, ID: id}
	})

	_, err := m.Call(t.Context(), "echo", nil)
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("got %v, want ErrProtocol", err)
	}
}

func TestMuxCallRejectsBadMethod(t *testing.T) {
	t.Parallel()

	m, peer := newPipePeer(t, echo)

	if _, err := m.Call(t.Context(), "", nil); !errors.Is(err, ErrMalformedPayload) {
		t.Fatalf("got %v, want ErrMalformedPayload", err)
	}
	if n := m.InFlight(); n != 0 {
		t.Fatalf("InFlight = %d, want 0 - the id leaked", n)
	}
	time.Sleep(20 * time.Millisecond)
	if n := peer.callCount(); n != 0 {
		t.Fatalf("peer saw %d calls, want 0", n)
	}
}

// Ids must never be reused while a call is outstanding, or replies would be
// delivered to the wrong caller.
func TestMuxIDsAreUnique(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	seen := make(map[uint64]bool)

	m, _ := newPipePeer(t, func(p *fakePeer, id uint64, _ string, body []byte) *Frame {
		mu.Lock()
		if seen[id] {
			p.t.Errorf("id %d reused", id)
		}
		seen[id] = true
		mu.Unlock()
		return &Frame{Type: TypeReply, ID: id, Payload: body}
	})

	const n = 200
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.Call(t.Context(), "echo", nil); err != nil {
				t.Errorf("Call: %v", err)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != n {
		t.Fatalf("saw %d distinct ids, want %d", len(seen), n)
	}
	if seen[0] {
		t.Error("id 0 was used for a call; it is reserved for LOG frames")
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within %v", d)
}

func BenchmarkMuxCallSerial(b *testing.B) {
	m, _ := newPipePeer(&testing.T{}, echo)
	body := bytes.Repeat([]byte("x"), 512)
	ctx := context.Background()

	b.ReportAllocs()
	for b.Loop() {
		if _, err := m.Call(ctx, "echo", body); err != nil {
			b.Fatal(err)
		}
	}
}

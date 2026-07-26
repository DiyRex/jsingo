package wire

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRateLimiterAdmitsBurstThenThrottles(t *testing.T) {
	t.Parallel()

	l := newRateLimiter(10, 5)
	for i := range 5 {
		if !l.allow() {
			t.Fatalf("burst event %d refused", i)
		}
	}
	if l.allow() {
		t.Fatal("the sixth event should exceed the burst")
	}
	if l.Dropped() != 1 {
		t.Fatalf("Dropped() = %d, want 1", l.Dropped())
	}
}

func TestRateLimiterRefills(t *testing.T) {
	t.Parallel()

	l := newRateLimiter(1000, 1)
	if !l.allow() {
		t.Fatal("first event refused")
	}
	if l.allow() {
		t.Fatal("second event should be throttled immediately")
	}
	time.Sleep(20 * time.Millisecond) // ~20 tokens at 1000/s
	if !l.allow() {
		t.Fatal("tokens did not refill")
	}
}

// A nil limiter means "no limit", so the zero-configuration path stays open.
func TestNilRateLimiterAllowsEverything(t *testing.T) {
	t.Parallel()

	var l *rateLimiter
	for range 1000 {
		if !l.allow() {
			t.Fatal("a nil limiter must not throttle")
		}
	}
	if l.Dropped() != 0 {
		t.Fatal("a nil limiter cannot drop")
	}
	if newRateLimiter(0, 10) != nil {
		t.Fatal("a rate of zero should disable limiting")
	}
}

// A flooding peer must not be able to grow the parent without bound. Excess
// frames are dropped and counted rather than queued.
func TestMuxRateLimitsLogFrames(t *testing.T) {
	t.Parallel()

	c1, c2 := socketPair(t)
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })

	delivered := make(chan struct{}, 4096)
	m := NewMux(NewReader(c1, 0), NewWriter(c1, 0), MuxOptions{
		Log:              func(Frame) { delivered <- struct{}{} },
		LogRatePerSecond: 10,
		LogBurst:         5,
	})
	go func() { _ = m.Serve() }()
	t.Cleanup(func() { _ = m.Close() })

	const flood = 500
	w := NewWriter(c2, 0)
	for range flood {
		if err := w.WriteFrame(Frame{Type: TypeLog, ID: 0, Payload: []byte(`{"lvl":"info"}`)}); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	// Give the reader time to consume the flood.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && m.DroppedLogs() == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	if got := len(delivered); got > 50 {
		t.Errorf("delivered %d of %d log frames; the limiter is not engaging", got, flood)
	}
	if m.DroppedLogs() == 0 {
		t.Error("no drops recorded; a gap in the log must be visible as a gap")
	}
	t.Logf("delivered %d, dropped %d of %d", len(delivered), m.DroppedLogs(), flood)
}

// An oversized reply must fail its own call, not the connection: it is a
// handler returning too much, not a broken peer.
func TestMuxRejectsOversizeReply(t *testing.T) {
	t.Parallel()

	c1, c2 := socketPair(t)
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })

	m := NewMux(NewReader(c1, 0), NewWriter(c1, 0), MuxOptions{MaxReplyBytes: 64})
	serveErr := make(chan error, 1)
	go func() { serveErr <- m.Serve() }()
	t.Cleanup(func() { _ = m.Close() })

	// A peer that answers every call with an oversized reply.
	go func() {
		r, w := NewReader(c2, 0), NewWriter(c2, 0)
		for {
			f, err := r.ReadFrame()
			if err != nil {
				return
			}
			if f.Type != TypeCall {
				continue
			}
			_, body, _ := DecodeCallPayload(f.Payload)
			size := 1024
			if string(body) == `"small"` {
				size = 8
			}
			_ = w.WriteFrame(Frame{Type: TypeReply, ID: f.ID, Payload: make([]byte, size)})
		}
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if _, err := m.Call(ctx, "big", nil); !errors.Is(err, ErrReplyTooLarge) {
		t.Fatalf("got %v, want ErrReplyTooLarge", err)
	}

	// The connection must still be usable afterwards.
	got, err := m.Call(ctx, "small", []byte(`"small"`))
	if err != nil {
		t.Fatalf("a later call failed, so the oversize reply killed the connection: %v", err)
	}
	if len(got) != 8 {
		t.Fatalf("got %d bytes, want 8", len(got))
	}

	select {
	case err := <-serveErr:
		t.Fatalf("Serve returned %v; an oversize reply must not end the session", err)
	default:
	}
}

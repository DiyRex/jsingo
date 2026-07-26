package wire

import (
	"sync"
	"sync/atomic"
	"time"
)

// rateLimiter is a token bucket.
//
// It exists for LOG frames. A compromised or merely buggy sidecar can emit
// them faster than the parent can format and write them, turning a logging
// path into unbounded memory growth in the Go process. Dropping is the correct
// response: diagnostics are best-effort, and a log line must never be able to
// take down the program reading it.
type rateLimiter struct {
	mu     sync.Mutex
	tokens float64
	burst  float64
	rate   float64 // tokens per second
	last   time.Time

	dropped atomic.Int64
}

// newRateLimiter returns a limiter admitting rate events per second with the
// given burst. A rate of zero disables limiting.
func newRateLimiter(rate, burst int) *rateLimiter {
	if rate <= 0 {
		return nil
	}
	if burst <= 0 {
		burst = rate
	}
	return &rateLimiter{
		tokens: float64(burst),
		burst:  float64(burst),
		rate:   float64(rate),
		last:   time.Now(),
	}
}

// allow reports whether an event may proceed, consuming a token if so.
func (l *rateLimiter) allow() bool {
	if l == nil {
		return true
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	if elapsed := now.Sub(l.last); elapsed > 0 {
		l.tokens += elapsed.Seconds() * l.rate
		if l.tokens > l.burst {
			l.tokens = l.burst
		}
		l.last = now
	}

	if l.tokens < 1 {
		l.dropped.Add(1)
		return false
	}
	l.tokens--
	return true
}

// Dropped returns how many events have been refused.
//
// Reported rather than silently discarded: a caller seeing gaps in the log
// needs to know they are gaps, not silence.
func (l *rateLimiter) Dropped() int64 {
	if l == nil {
		return 0
	}
	return l.dropped.Load()
}

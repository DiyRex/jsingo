//go:build unix

package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DiyRex/jsingo/internal/transport"
)

// These tests fork real processes. The behaviour under test - descriptor
// inheritance, EOF on crash, signal escalation, orphan collection - has no
// meaning in-process, so a mock would only test the mock.

// shellSidecar writes a shell script acting as a sidecar and returns a Spawner
// for it. The script talks to the protocol descriptor as fd 3.
func shellSidecar(t *testing.T, script string) Spawner {
	t.Helper()

	path := filepath.Join(t.TempDir(), "sidecar.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	return func(ctx context.Context, childFD int) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, "/bin/sh", path, fmt.Sprint(childFD)), nil
	}
}

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", bytes.TrimRight(p, "\n"))
	return len(p), nil
}

// The core contract: the child inherits a working socket on fd 3, and bytes
// it writes there arrive on the parent's endpoint.
func TestSupervisorChildInheritsTransport(t *testing.T) {
	t.Parallel()

	got := make(chan string, 1)
	sup, err := New(Config{
		// Echo a greeting to fd 3 then block so the session stays open.
		Spawn:  shellSidecar(t, `echo "hello from pid $$" >&3; sleep 30`),
		Logger: testLogger(t),
		Connect: func(ctx context.Context, conn io.ReadWriteCloser) error {
			buf := make([]byte, 128)
			n, err := conn.Read(buf)
			if err != nil {
				return err
			}
			got <- strings.TrimSpace(string(buf[:n]))
			<-ctx.Done()
			return ctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- sup.Run(ctx) }()

	select {
	case msg := <-got:
		if !strings.HasPrefix(msg, "hello from pid ") {
			t.Fatalf("got %q", msg)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no data arrived on the inherited descriptor")
	}

	sup.Stop()
	if err := <-runDone; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// If the parent keeps its copy of the child's endpoint open, a crash never
// surfaces as EOF and the session hangs. This asserts CloseChild happens.
func TestSupervisorCrashSurfacesAsEOF(t *testing.T) {
	t.Parallel()

	eof := make(chan error, 1)
	var once sync.Once

	sup, err := New(Config{
		Spawn:   shellSidecar(t, `echo ready >&3; exit 7`),
		Logger:  testLogger(t),
		Backoff: Backoff{Min: time.Millisecond, Max: 5 * time.Millisecond},
		Connect: func(_ context.Context, conn io.ReadWriteCloser) error {
			// Drain until the peer goes away.
			_, err := io.Copy(io.Discard, conn)
			if err == nil {
				err = io.EOF
			}
			once.Do(func() { eof <- err })
			return err
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	go func() { _ = sup.Run(ctx) }()
	defer sup.Stop()

	select {
	case err := <-eof:
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("got %v, want EOF or a clean read end", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("child exited but the parent never saw EOF - CloseChild did not run")
	}
}

func TestSupervisorRestartsAfterCrash(t *testing.T) {
	t.Parallel()

	var sessions atomic.Int64
	const want = 3
	reached := make(chan struct{})
	var once sync.Once

	sup, err := New(Config{
		Spawn:   shellSidecar(t, `echo x >&3; exit 1`),
		Logger:  testLogger(t),
		Backoff: Backoff{Min: time.Millisecond, Max: 5 * time.Millisecond},
		// Keep the budget above the number of restarts we assert on.
		MaxRestarts: 20,
		Connect: func(_ context.Context, conn io.ReadWriteCloser) error {
			_, _ = io.Copy(io.Discard, conn)
			if sessions.Add(1) == want {
				once.Do(func() { close(reached) })
			}
			return io.EOF
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	go func() { _ = sup.Run(ctx) }()
	defer sup.Stop()

	select {
	case <-reached:
	case <-time.After(20 * time.Second):
		t.Fatalf("only %d sessions after 20s, want %d", sessions.Load(), want)
	}
	if got := sup.Restarts(); got < want-1 {
		t.Fatalf("Restarts() = %d, want at least %d", got, want-1)
	}
}

// A sidecar that fails immediately and repeatedly must be abandoned rather
// than respawned forever.
func TestSupervisorCrashLoopGivesUp(t *testing.T) {
	t.Parallel()

	sup, err := New(Config{
		Spawn:         shellSidecar(t, `exit 1`),
		Logger:        testLogger(t),
		Backoff:       Backoff{Min: time.Millisecond, Max: 2 * time.Millisecond},
		MaxRestarts:   3,
		RestartWindow: 10 * time.Second,
		Connect: func(_ context.Context, conn io.ReadWriteCloser) error {
			_, _ = io.Copy(io.Discard, conn)
			return io.EOF
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	select {
	case err := <-done:
		if !errors.Is(err, ErrCrashLoop) {
			t.Fatalf("got %v, want ErrCrashLoop", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("supervisor never gave up")
	}
}

// stderr is the only diagnostic channel for a sidecar that dies before it can
// speak the protocol, so it must reach both the log and the error.
func TestSupervisorCapturesStderrForDiagnosis(t *testing.T) {
	t.Parallel()

	sup, err := New(Config{
		Spawn:         shellSidecar(t, `echo "SyntaxError: unexpected token" >&2; exit 1`),
		Logger:        testLogger(t),
		Backoff:       Backoff{Min: time.Millisecond, Max: 2 * time.Millisecond},
		MaxRestarts:   1,
		RestartWindow: 10 * time.Second,
		Connect: func(_ context.Context, conn io.ReadWriteCloser) error {
			_, _ = io.Copy(io.Discard, conn)
			return io.EOF
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	runErr := make(chan error, 1)
	go func() { runErr <- sup.Run(ctx) }()

	select {
	case err := <-runErr:
		if !errors.Is(err, ErrCrashLoop) {
			t.Fatalf("got %v, want ErrCrashLoop", err)
		}
		if !strings.Contains(err.Error(), "SyntaxError") {
			t.Fatalf("stderr missing from the error, so the operator sees no cause: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("supervisor did not finish")
	}
}

// A sidecar wedged in a synchronous npm call cannot service SIGTERM. Without
// escalation the supervisor would block on Wait forever.
func TestSupervisorEscalatesToSIGKILL(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	var once sync.Once

	sup, err := New(Config{
		// Ignore SIGTERM, then sleep well past the grace period.
		Spawn:         shellSidecar(t, `trap '' TERM; echo up >&3; sleep 60`),
		Logger:        testLogger(t),
		ShutdownGrace: 300 * time.Millisecond,
		Connect: func(ctx context.Context, conn io.ReadWriteCloser) error {
			buf := make([]byte, 16)
			if _, err := conn.Read(buf); err != nil {
				return err
			}
			once.Do(func() { close(started) })
			<-ctx.Done()
			return ctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	<-started
	stopAt := time.Now()
	sup.Stop()

	select {
	case <-done:
		elapsed := time.Since(stopAt)
		if elapsed > 10*time.Second {
			t.Fatalf("shutdown took %v; SIGKILL escalation did not happen", elapsed)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("supervisor never shut down a signal-ignoring child")
	}
}

// A JS runtime that spawns a worker leaves it running if only the direct child
// is signalled. Killing the process group collects the whole tree.
func TestSupervisorKillsGrandchildren(t *testing.T) {
	t.Parallel()

	marker := filepath.Join(t.TempDir(), "grandchild-alive")

	// The grandchild outlives its parent and keeps touching a file. If the
	// group kill works the file stops changing.
	script := fmt.Sprintf(`
(while true; do touch %q; sleep 0.05; done) &
echo up >&3
sleep 60
`, marker)

	started := make(chan struct{})
	var once sync.Once

	sup, err := New(Config{
		Spawn:         shellSidecar(t, script),
		Logger:        testLogger(t),
		ShutdownGrace: 500 * time.Millisecond,
		Connect: func(ctx context.Context, conn io.ReadWriteCloser) error {
			buf := make([]byte, 16)
			if _, err := conn.Read(buf); err != nil {
				return err
			}
			once.Do(func() { close(started) })
			<-ctx.Done()
			return ctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	<-started
	// Let the grandchild establish itself.
	time.Sleep(200 * time.Millisecond)
	sup.Stop()
	<-done

	// Give any survivor time to touch the marker again.
	time.Sleep(300 * time.Millisecond)
	before := modTime(t, marker)
	time.Sleep(400 * time.Millisecond)
	after := modTime(t, marker)

	if !before.Equal(after) {
		t.Fatalf("grandchild survived the group kill: marker still changing (%v -> %v)", before, after)
	}
}

func modTime(t *testing.T, path string) time.Time {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		// Never created: the grandchild is certainly not running.
		return time.Time{}
	}
	return fi.ModTime()
}

func TestSupervisorStopIsIdempotent(t *testing.T) {
	t.Parallel()

	sup, err := New(Config{
		Spawn:  shellSidecar(t, `sleep 30`),
		Logger: testLogger(t),
		Connect: func(ctx context.Context, _ io.ReadWriteCloser) error {
			<-ctx.Done()
			return ctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() { defer wg.Done(); sup.Stop() }()
	}
	wg.Wait()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after Stop: got %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
}

// Cancelling the caller's context is a failure signal, unlike Stop.
func TestSupervisorContextCancelReturnsContextError(t *testing.T) {
	t.Parallel()

	sup, err := New(Config{
		Spawn:  shellSidecar(t, `sleep 30`),
		Logger: testLogger(t),
		Connect: func(ctx context.Context, _ io.ReadWriteCloser) error {
			<-ctx.Done()
			return ctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

func TestSupervisorSpawnFailureIsReported(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("no runtime installed")
	sup, err := New(Config{
		Spawn: func(context.Context, int) (*exec.Cmd, error) {
			return nil, sentinel
		},
		Logger:        testLogger(t),
		Backoff:       Backoff{Min: time.Millisecond, Max: 2 * time.Millisecond},
		MaxRestarts:   2,
		RestartWindow: 10 * time.Second,
		Connect: func(context.Context, io.ReadWriteCloser) error {
			t.Error("Connect ran despite a spawn failure")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	runErr := sup.Run(ctx)
	if !errors.Is(runErr, ErrCrashLoop) || !strings.Contains(runErr.Error(), sentinel.Error()) {
		t.Fatalf("got %v, want ErrCrashLoop wrapping %v", runErr, sentinel)
	}
}

// Descriptors must not accumulate across restarts.
func TestSupervisorDoesNotLeakDescriptors(t *testing.T) {
	t.Parallel()

	var sessions atomic.Int64
	const rounds = 12
	reached := make(chan struct{})
	var once sync.Once

	sup, err := New(Config{
		Spawn:         shellSidecar(t, `echo x >&3; exit 1`),
		Logger:        slog.New(slog.DiscardHandler),
		Backoff:       Backoff{Min: time.Millisecond, Max: 2 * time.Millisecond},
		MaxRestarts:   100,
		RestartWindow: time.Minute,
		Connect: func(_ context.Context, conn io.ReadWriteCloser) error {
			_, _ = io.Copy(io.Discard, conn)
			if sessions.Add(1) == rounds {
				once.Do(func() { close(reached) })
			}
			return io.EOF
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	before := openDescriptors(t)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	go func() { _ = sup.Run(ctx) }()

	select {
	case <-reached:
	case <-time.After(30 * time.Second):
		t.Fatalf("only %d sessions, want %d", sessions.Load(), rounds)
	}
	sup.Stop()
	time.Sleep(300 * time.Millisecond)

	after := openDescriptors(t)
	// A small drift is normal (runtime poller, log buffers). One leaked
	// descriptor per restart would show as ~rounds.
	if after-before > rounds/2 {
		t.Fatalf("descriptors grew from %d to %d over %d restarts", before, after, rounds)
	}
}

func openDescriptors(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(fmt.Sprintf("/dev/fd/%s", ""))
	if err != nil {
		entries, err = os.ReadDir("/dev/fd")
		if err != nil {
			t.Skipf("cannot enumerate descriptors: %v", err)
		}
	}
	return len(entries)
}

func TestNewValidatesConfig(t *testing.T) {
	t.Parallel()

	valid := Config{
		Spawn:   func(context.Context, int) (*exec.Cmd, error) { return nil, nil },
		Connect: func(context.Context, io.ReadWriteCloser) error { return nil },
		Logger:  slog.New(slog.DiscardHandler),
	}

	tests := []struct {
		name  string
		mutil func(*Config)
	}{
		{"no spawn", func(c *Config) { c.Spawn = nil }},
		{"no connect", func(c *Config) { c.Connect = nil }},
		{"no logger", func(c *Config) { c.Logger = nil }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := valid
			tc.mutil(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatal("want an error")
			}
		})
	}

	sup, err := New(valid)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if sup.cfg.MaxRestarts != DefaultMaxRestarts ||
		sup.cfg.RestartWindow != DefaultRestartWindow ||
		sup.cfg.ShutdownGrace != DefaultShutdownGrace {
		t.Fatalf("zero values not defaulted: %+v", sup.cfg)
	}
}

func TestTransportPairCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	p, err := transport.NewPair()
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestTransportPairIsConnected(t *testing.T) {
	t.Parallel()

	p, err := transport.NewPair()
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	defer func() { _ = p.Close() }()

	want := "round trip"
	go func() { _, _ = p.Child.WriteString(want) }()

	buf := make([]byte, len(want))
	if _, err := io.ReadFull(p.Local, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != want {
		t.Fatalf("got %q, want %q", buf, want)
	}
}

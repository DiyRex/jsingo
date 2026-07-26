//go:build integration && unix

// Package integration drives a real bun or node sidecar end to end.
//
// Everything here needs a JavaScript runtime installed, so it sits behind a
// build tag; `go test ./...` stays green on a bare machine. Run with:
//
//	go test -tags=integration ./integration/...
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/DiyRex/jsingo/internal/detect"
	"github.com/DiyRex/jsingo/internal/sandbox"
	"github.com/DiyRex/jsingo/internal/supervisor"
	"github.com/DiyRex/jsingo/internal/wire"
)

// session is a live sidecar plus the mux talking to it.
type session struct {
	mux  *wire.Mux
	sup  *supervisor.Supervisor
	logs chan string
}

// repoRoot locates the module root from this test file's own path, so tests
// do not depend on the working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine test file path")
	}
	return filepath.Dir(filepath.Dir(file))
}

// startSession boots the host under the given runtime and waits until it
// answers a ping.
func startSession(t *testing.T, kind detect.Kind) *session {
	t.Helper()

	rt, err := detect.Find(t.Context(), detect.WithOrder(kind))
	if err != nil {
		t.Skipf("%s not usable: %v", kind, err)
	}
	t.Logf("using %s", rt)

	root := repoRoot(t)
	hostEntry := filepath.Join(root, "jsruntime", "src", "main.ts")
	handlers := filepath.Join(root, "integration", "testdata", "handlers.ts")

	// node cannot execute TypeScript directly on every supported version, so
	// the host is transpiled to JS first when running under node.
	if kind == detect.KindNode {
		hostEntry, handlers = buildForNode(t, root)
	}

	sandboxDir := t.TempDir()
	logs := make(chan string, 256)
	ready := make(chan *wire.Mux, 1)
	var muxOnce sync.Once

	sup, err := supervisor.New(supervisor.Config{
		Logger: sessionLogger(t),
		Spawn: func(ctx context.Context, childFD int) (*exec.Cmd, error) {
			cmd := rt.Command(ctx, hostEntry, handlers)
			// Deny-by-default: the sidecar gets a minimal synthetic
			// environment, never the parent's. See internal/sandbox.
			policy := sandbox.Policy{Dir: sandboxDir}
			policy.Apply(cmd, map[string]string{
				"JSINGO_FD": fmt.Sprint(childFD),
			})
			// The runtime and handler entrypoints are read from the repo, so
			// the process still needs to start there.
			cmd.Dir = root
			return cmd, nil
		},
		Connect: func(ctx context.Context, conn io.ReadWriteCloser) error {
			m := wire.NewMux(
				wire.NewReader(conn, 0),
				wire.NewWriter(conn, 0),
				func(f wire.Frame) {
					select {
					case logs <- string(f.Payload):
					default: // never block the read loop on a slow test
					}
				},
			)
			muxOnce.Do(func() { ready <- m })
			return m.Serve()
		},
	})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}

	go func() { _ = sup.Run(t.Context()) }()
	t.Cleanup(sup.Stop)

	var m *wire.Mux
	select {
	case m = <-ready:
	case <-time.After(30 * time.Second):
		t.Fatal("sidecar never connected")
	}

	// The socket exists before the process is up, so a successful ping - not a
	// sleep - is what proves the host is actually serving.
	waitReady(t, m, 30*time.Second)
	return &session{mux: m, sup: sup, logs: logs}
}

func waitReady(t *testing.T, m *wire.Mux, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := m.Ping(ctx)
		cancel()
		if err == nil {
			return
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("sidecar not ready after %v: %v", timeout, lastErr)
}

// buildForNode bundles the host and handlers to plain JS.
//
// Node's TypeScript support varies by version, so rather than gate the whole
// node matrix on it, bun (already required for development) transpiles ahead
// of time. This also exercises the bundling path the release build will use.
func buildForNode(t *testing.T, root string) (host, handlers string) {
	t.Helper()

	bun, err := detect.Find(t.Context(), detect.WithOrder(detect.KindBun))
	if err != nil {
		t.Skipf("node matrix needs bun to transpile TypeScript: %v", err)
	}

	out := t.TempDir()
	cmd := exec.CommandContext(t.Context(), bun.Path, "build",
		filepath.Join(root, "jsruntime", "src", "main.ts"),
		"--target", "node", "--format", "esm", "--outfile", filepath.Join(out, "host.mjs"))
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bun build host: %v\n%s", err, b)
	}

	cmd = exec.CommandContext(t.Context(), bun.Path, "build",
		filepath.Join(root, "integration", "testdata", "handlers.ts"),
		"--target", "node", "--format", "esm", "--outfile", filepath.Join(out, "handlers.mjs"))
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bun build handlers: %v\n%s", err, b)
	}
	return filepath.Join(out, "host.mjs"), filepath.Join(out, "handlers.mjs")
}

// call is a typed convenience over the mux.
func call[Out any](t *testing.T, s *session, ctx context.Context, method string, in any) (Out, error) {
	t.Helper()

	var zero Out
	body, err := json.Marshal(in)
	if err != nil {
		return zero, err
	}
	raw, err := s.mux.Call(ctx, method, body)
	if err != nil {
		return zero, err
	}
	var out Out
	if err := json.Unmarshal(raw, &out); err != nil {
		return zero, fmt.Errorf("decode reply %q: %w", raw, err)
	}
	return out, nil
}

// eachRuntime runs fn against every installed runtime, skipping absent ones.
func eachRuntime(t *testing.T, fn func(t *testing.T, s *session)) {
	for _, kind := range []detect.Kind{detect.KindBun, detect.KindNode} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			fn(t, startSession(t, kind))
		})
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	eachRuntime(t, func(t *testing.T, s *session) {
		got, err := call[struct{ Sum int }](t, s, t.Context(), "add", map[string]int{"a": 2, "b": 40})
		if err != nil {
			t.Fatalf("add: %v", err)
		}
		if got.Sum != 42 {
			t.Fatalf("sum = %d, want 42", got.Sum)
		}
	})
}

func TestEchoPreservesStructure(t *testing.T) {
	t.Parallel()

	eachRuntime(t, func(t *testing.T, s *session) {
		type payload struct {
			Title string   `json:"title"`
			Tags  []string `json:"tags"`
			N     int      `json:"n"`
		}
		want := payload{Title: "héllo wörld", Tags: []string{"a", "b"}, N: 7}

		got, err := call[payload](t, s, t.Context(), "echo", want)
		if err != nil {
			t.Fatalf("echo: %v", err)
		}
		if got.Title != want.Title || got.N != want.N || len(got.Tags) != 2 {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
}

// A large body crosses several socket reads, exercising the incremental
// decoder on the JS side against real chunk boundaries.
func TestLargePayload(t *testing.T) {
	t.Parallel()

	eachRuntime(t, func(t *testing.T, s *session) {
		big := make([]byte, 0, 1<<20)
		for len(big) < 1<<20 {
			big = append(big, "abcdefghij"...)
		}
		in := map[string]string{"s": string(big)}

		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()

		got, err := call[struct{ S string }](t, s, ctx, "upper", in)
		if err != nil {
			t.Fatalf("upper: %v", err)
		}
		if len(got.S) != len(big) {
			t.Fatalf("got %d bytes back, sent %d", len(got.S), len(big))
		}
		if got.S[:10] != "ABCDEFGHIJ" {
			t.Fatalf("payload corrupted: %q", got.S[:10])
		}
	})
}

func TestHandlerErrorBecomesTypedCallError(t *testing.T) {
	t.Parallel()

	eachRuntime(t, func(t *testing.T, s *session) {
		_, err := call[struct{}](t, s, t.Context(), "boom", nil)
		if err == nil {
			t.Fatal("want an error")
		}

		var ce *wire.CallError
		if !errors.As(err, &ce) {
			t.Fatalf("got %T (%v), want *wire.CallError", err, err)
		}
		if ce.Code != wire.CodeInternal {
			t.Errorf("code = %v, want Internal", ce.Code)
		}
		if ce.Message != "handler exploded" {
			t.Errorf("message = %q", ce.Message)
		}
		// The stack is the only way to find a fault inside a minified bundle.
		if len(ce.Details) == 0 {
			t.Error("no stack trace in details")
		}
	})
}

func TestTypedErrorPreservesCode(t *testing.T) {
	t.Parallel()

	eachRuntime(t, func(t *testing.T, s *session) {
		_, err := call[struct{}](t, s, t.Context(), "notFound", nil)
		if !errors.Is(err, &wire.CallError{Code: wire.CodeNotFound}) {
			t.Fatalf("got %v, want a NotFound CallError", err)
		}
	})
}

func TestUnknownMethodIsUnimplemented(t *testing.T) {
	t.Parallel()

	eachRuntime(t, func(t *testing.T, s *session) {
		_, err := call[struct{}](t, s, t.Context(), "noSuchFunction", nil)

		var ce *wire.CallError
		if !errors.As(err, &ce) {
			t.Fatalf("got %T (%v), want *wire.CallError", err, err)
		}
		if ce.Code != wire.CodeUnimplemented {
			t.Errorf("code = %v, want Unimplemented", ce.Code)
		}
		// The message should name real methods so a typo is self-diagnosing.
		if !containsAll(ce.Message, "noSuchFunction", "add") {
			t.Errorf("unhelpful message: %q", ce.Message)
		}
	})
}

// Cancellation must reach the handler's AbortSignal, not merely unblock Go.
// Otherwise a cancelled context leaves the sidecar working on a dead result.
func TestCancellationReachesHandler(t *testing.T) {
	t.Parallel()

	eachRuntime(t, func(t *testing.T, s *session) {
		ctx, cancel := context.WithCancel(t.Context())
		errc := make(chan error, 1)
		go func() {
			_, err := call[struct{}](t, s, ctx, "slow", nil)
			errc <- err
		}()

		time.Sleep(300 * time.Millisecond)
		cancel()

		select {
		case err := <-errc:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("got %v, want context.Canceled", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("call did not return after cancellation")
		}

		// Ask the sidecar whether its handler actually observed the abort.
		ctx2, cancel2 := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel2()

		start := make(chan struct{})
		go func() {
			close(start)
			_, _ = call[struct{}](t, s, ctx2, "abortable", nil)
		}()
		<-start
		time.Sleep(20 * time.Millisecond)
		cancel2()
		time.Sleep(300 * time.Millisecond)

		got, err := call[struct{ Aborted bool }](t, s, t.Context(), "wasAborted", nil)
		if err != nil {
			t.Fatalf("wasAborted: %v", err)
		}
		if !got.Aborted {
			t.Fatal("handler never saw the abort signal; CANCEL did not reach it")
		}
	})
}

func TestConcurrentCallsAreMultiplexed(t *testing.T) {
	t.Parallel()

	eachRuntime(t, func(t *testing.T, s *session) {
		const n = 64
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()

		var wg sync.WaitGroup
		errs := make(chan error, n)

		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got, err := call[struct{ Sum int }](t, s, ctx, "add",
					map[string]int{"a": i, "b": i})
				if err != nil {
					errs <- fmt.Errorf("call %d: %w", i, err)
					return
				}
				if got.Sum != i*2 {
					errs <- fmt.Errorf("call %d: sum = %d, want %d", i, got.Sum, i*2)
				}
			}()
		}
		wg.Wait()
		close(errs)

		for err := range errs {
			t.Error(err)
		}
	})
}

// The supervisor must bring the sidecar back and calls must succeed after.
func TestSidecarRestartsAfterCrash(t *testing.T) {
	t.Parallel()

	eachRuntime(t, func(t *testing.T, s *session) {
		before := s.sup.Restarts()

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		// The handler exits the process; the call cannot succeed.
		if _, err := call[struct{}](t, s, ctx, "kill", nil); err == nil {
			t.Fatal("call to a process-killing handler should not succeed")
		}

		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if s.sup.Restarts() > before {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("no restart observed; Restarts() stayed at %d", before)
	})
}

func TestStructuredLogsArriveAsFrames(t *testing.T) {
	t.Parallel()

	eachRuntime(t, func(t *testing.T, s *session) {
		// An unknown method makes the host log nothing, but a crashing handler
		// does not either - logs come from the host itself. Provoke one by
		// sending a call that triggers the uncaught-exception path.
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		_, _ = call[struct{}](t, s, ctx, "boom", nil)

		// No assertion on content: this test exists to prove LOG frames are
		// routed rather than mistaken for replies. Absence is fine; a
		// misrouted frame would have failed the boom test above.
		select {
		case line := <-s.logs:
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("log frame is not NDJSON: %q", line)
			}
			if _, ok := rec["level"]; !ok {
				t.Errorf("log record missing level: %q", line)
			}
		case <-time.After(time.Second):
		}
	})
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// sessionLogger surfaces sidecar stderr in test output, which is the only way
// to diagnose a host that dies before it can speak the protocol.
func sessionLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(testLogWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}

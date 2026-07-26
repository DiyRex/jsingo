//go:build integration && unix

package integration

import (
	"context"
	"embed"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DiyRex/jsingo"
)

// This is the API a consumer actually writes: an embedded filesystem, a
// module, and typed bindings. If this file is awkward, the API is wrong.

//go:embed js/demo.ts js/package.json
var demoFS embed.FS

var demo = jsingo.Module(demoFS, "js/demo.ts")

type greetReq struct {
	Name string `json:"name"`
}
type greetResp struct {
	Message string `json:"message"`
}

type sumReq struct {
	Values []int `json:"values"`
}
type sumResp struct {
	Total int `json:"total"`
}

func newRuntime(t *testing.T, opts ...jsingo.Option) *jsingo.Runtime {
	t.Helper()

	base := []jsingo.Option{
		jsingo.WithModule(demo),
		jsingo.WithLogger(slog.New(slog.NewTextHandler(&testLogWriter{t: t}, nil))),
		jsingo.WithCacheDir(t.TempDir()),
		jsingo.WithStartupTimeout(60 * time.Second),
	}

	rt, err := jsingo.New(t.Context(), append(base, opts...)...)
	if err != nil {
		t.Fatalf("jsingo.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := rt.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return rt
}

func TestAPIBindRoundTrip(t *testing.T) {
	t.Parallel()

	rt := newRuntime(t)
	greet := jsingo.Bind[greetReq, greetResp](rt, demo, "greet")

	got, err := greet(t.Context(), greetReq{Name: "world"})
	if err != nil {
		t.Fatalf("greet: %v", err)
	}
	if got.Message != "hello, world" {
		t.Fatalf("message = %q", got.Message)
	}
}

func TestAPIBindHandlesSlices(t *testing.T) {
	t.Parallel()

	rt := newRuntime(t)
	sum := jsingo.Bind[sumReq, sumResp](rt, demo, "sum")

	got, err := sum(t.Context(), sumReq{Values: []int{1, 2, 3, 4, 5}})
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if got.Total != 15 {
		t.Fatalf("total = %d, want 15", got.Total)
	}
}

func TestAPIHandlerErrorCarriesStack(t *testing.T) {
	t.Parallel()

	rt := newRuntime(t)
	fail := jsingo.Bind[struct{}, struct{}](rt, demo, "fail")

	_, err := fail(t.Context(), struct{}{})
	if err == nil {
		t.Fatal("want an error")
	}

	var he *jsingo.HandlerError
	if !errors.As(err, &he) {
		t.Fatalf("got %T (%v), want *jsingo.HandlerError", err, err)
	}
	if he.Code != jsingo.CodeInternal {
		t.Errorf("code = %v, want Internal", he.Code)
	}
	if he.Message != "deliberate failure" {
		t.Errorf("message = %q", he.Message)
	}
	if he.Method != "fail" {
		t.Errorf("method = %q, want the binding to name itself", he.Method)
	}
	// The stack is how a developer locates a fault inside a minified bundle.
	if he.Stack == "" {
		t.Error("no JavaScript stack trace")
	}
	// Matching on code alone is the documented pattern.
	if !errors.Is(err, &jsingo.HandlerError{Code: jsingo.CodeInternal}) {
		t.Error("errors.Is by code failed")
	}
}

func TestAPIUnknownMethodIsUnimplemented(t *testing.T) {
	t.Parallel()

	rt := newRuntime(t)
	missing := jsingo.Bind[struct{}, struct{}](rt, demo, "noSuchExport")

	_, err := missing(t.Context(), struct{}{})

	var he *jsingo.HandlerError
	if !errors.As(err, &he) {
		t.Fatalf("got %T (%v)", err, err)
	}
	if he.Code != jsingo.CodeUnimplemented {
		t.Errorf("code = %v, want Unimplemented", he.Code)
	}
	// The message should name real exports so a typo diagnoses itself.
	if !strings.Contains(he.Message, "greet") {
		t.Errorf("message does not list available methods: %q", he.Message)
	}
}

func TestAPICancellationPropagates(t *testing.T) {
	t.Parallel()

	rt := newRuntime(t)
	slow := jsingo.Bind[map[string]int, struct{}](rt, demo, "slowEcho")

	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() {
		_, err := slow(ctx, map[string]int{"ms": 60_000})
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
}

func TestAPITimeoutOption(t *testing.T) {
	t.Parallel()

	rt := newRuntime(t)
	slow := jsingo.Bind[map[string]int, struct{}](rt, demo, "slowEcho",
		jsingo.Timeout(200*time.Millisecond))

	start := time.Now()
	_, err := slow(t.Context(), map[string]int{"ms": 60_000})
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("timeout took %v to fire", elapsed)
	}
}

func TestAPIConcurrentCalls(t *testing.T) {
	t.Parallel()

	rt := newRuntime(t)
	sum := jsingo.Bind[sumReq, sumResp](rt, demo, "sum")

	const n = 100
	var wg sync.WaitGroup
	errs := make(chan error, n)

	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := sum(t.Context(), sumReq{Values: []int{i, i, i}})
			if err != nil {
				errs <- err
				return
			}
			if got.Total != i*3 {
				errs <- errors.New("wrong total")
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}

	stats := rt.Stats()
	if stats.Calls < n {
		t.Errorf("Stats().Calls = %d, want at least %d", stats.Calls, n)
	}
	if stats.InFlight != 0 {
		t.Errorf("Stats().InFlight = %d after completion, want 0", stats.InFlight)
	}
}

func TestAPIStatsAndPing(t *testing.T) {
	t.Parallel()

	rt := newRuntime(t)

	if err := rt.Ping(t.Context()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := rt.Err(); err != nil {
		t.Fatalf("Err() = %v on a healthy runtime", err)
	}

	s := rt.Stats()
	if !s.Connected {
		t.Error("Stats().Connected = false while serving")
	}
	if s.Runtime == "" {
		t.Error("Stats().Runtime is empty")
	}
	if s.Uptime <= 0 {
		t.Error("Stats().Uptime should be positive")
	}
	t.Logf("stats: %+v", s)
}

func TestAPICloseIsIdempotentAndBlocksCalls(t *testing.T) {
	t.Parallel()

	rt, err := jsingo.New(t.Context(),
		jsingo.WithModule(demo),
		jsingo.WithCacheDir(t.TempDir()),
		jsingo.WithStartupTimeout(60*time.Second),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	greet := jsingo.Bind[greetReq, greetResp](rt, demo, "greet")
	if _, err := greet(t.Context(), greetReq{Name: "x"}); err != nil {
		t.Fatalf("call before close: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for range 3 {
		if err := rt.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	if _, err := greet(t.Context(), greetReq{Name: "x"}); !errors.Is(err, jsingo.ErrClosed) {
		t.Fatalf("call after close: got %v, want ErrClosed", err)
	}
	if err := rt.Ping(t.Context()); !errors.Is(err, jsingo.ErrClosed) {
		t.Fatalf("Ping after close: got %v, want ErrClosed", err)
	}
}

// A credential-shaped AllowEnv entry must fail construction, not leak.
func TestAPIRejectsSensitiveEnvAllowlist(t *testing.T) {
	t.Parallel()

	_, err := jsingo.New(t.Context(),
		jsingo.WithModule(demo),
		jsingo.WithCacheDir(t.TempDir()),
		jsingo.WithAllowedEnv("AWS_SECRET_ACCESS_KEY"),
	)
	if err == nil {
		t.Fatal("New should refuse to forward a credential-shaped variable")
	}
	if !strings.Contains(err.Error(), "AWS_SECRET_ACCESS_KEY") {
		t.Errorf("error should name the offending variable: %v", err)
	}
}

func TestAPIRejectsMissingEntry(t *testing.T) {
	t.Parallel()

	bad := jsingo.Module(demoFS, "js/does-not-exist.ts")
	_, err := jsingo.New(t.Context(),
		jsingo.WithModule(bad),
		jsingo.WithCacheDir(t.TempDir()),
	)
	if err == nil {
		t.Fatal("want an error for a missing entry")
	}
	// The usual cause is an embed pattern that misses the file, so the message
	// must say so rather than only reporting ENOENT.
	if !strings.Contains(err.Error(), "go:embed") {
		t.Errorf("error should mention the embed pattern: %v", err)
	}
}

func TestAPIRuntimeSelection(t *testing.T) {
	t.Parallel()

	for _, kind := range []jsingo.RuntimeKind{jsingo.Bun, jsingo.Node} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()

			rt := newRuntime(t, jsingo.WithRuntime(kind))
			if got := rt.Stats().Runtime; !strings.HasPrefix(got, string(kind)) {
				t.Fatalf("Stats().Runtime = %q, want the %s runtime", got, kind)
			}

			greet := jsingo.Bind[greetReq, greetResp](rt, demo, "greet")
			if _, err := greet(t.Context(), greetReq{Name: string(kind)}); err != nil {
				t.Fatalf("greet on %s: %v", kind, err)
			}
		})
	}
}

// The module cache is content-addressed, so a second Runtime over the same
// module reuses the extracted tree rather than rewriting it.
func TestAPISharedCacheDir(t *testing.T) {
	t.Parallel()

	cache := t.TempDir()
	for i := range 2 {
		rt, err := jsingo.New(t.Context(),
			jsingo.WithModule(demo),
			jsingo.WithCacheDir(cache),
			jsingo.WithStartupTimeout(60*time.Second),
		)
		if err != nil {
			t.Fatalf("New #%d: %v", i, err)
		}
		greet := jsingo.Bind[greetReq, greetResp](rt, demo, "greet")
		if _, err := greet(t.Context(), greetReq{Name: "x"}); err != nil {
			t.Fatalf("greet #%d: %v", i, err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err = rt.Close(ctx)
		cancel()
		if err != nil {
			t.Fatalf("Close #%d: %v", i, err)
		}
	}
}

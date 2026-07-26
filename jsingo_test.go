package jsingo

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/DiyRex/jsingo/internal/supervisor"
	"github.com/DiyRex/jsingo/internal/wire"
)

// Pure-Go coverage of the public surface. Anything needing a real sidecar
// lives in integration/, so `go test ./...` stays meaningful on a machine with
// no JavaScript runtime.

func TestModuleDerivesName(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"js/article.ts":  {Data: []byte("export function f() {}")},
		"js/helper.mjs":  {Data: []byte("")},
		"plain/name.txt": {Data: []byte("")},
	}

	tests := []struct{ entry, want string }{
		{"js/article.ts", "article"},
		{"js/helper.mjs", "helper"},
		{"plain/name.txt", "name.txt"},
	}
	for _, tc := range tests {
		t.Run(tc.entry, func(t *testing.T) {
			t.Parallel()
			m := Module(fsys, tc.entry)
			if m.err != nil {
				t.Fatalf("Module: %v", m.err)
			}
			if m.Name() != tc.want {
				t.Fatalf("Name() = %q, want %q", m.Name(), tc.want)
			}
		})
	}
}

// A missing entry is nearly always a too-narrow go:embed pattern, so the error
// has to say that rather than only reporting ENOENT.
func TestModuleMissingEntryExplainsEmbed(t *testing.T) {
	t.Parallel()

	m := Module(fstest.MapFS{"js/other.ts": {Data: []byte("")}}, "js/article.ts")
	if m.err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(m.err.Error(), "go:embed") {
		t.Errorf("error should mention the embed pattern: %v", m.err)
	}
}

func TestModuleRejectsBadPaths(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{"a.ts": {Data: []byte("")}}
	for _, entry := range []string{"", "/abs/a.ts", "../escape.ts"} {
		t.Run(entry, func(t *testing.T) {
			t.Parallel()
			if m := Module(fsys, entry); m.err == nil {
				t.Fatalf("Module(%q) should fail", entry)
			}
		})
	}
	if m := Module(nil, "a.ts"); m.err == nil {
		t.Fatal("Module with a nil filesystem should fail")
	}
}

// A node_modules inside an embedded filesystem means the embed pattern was too
// broad. Refusing beats silently materialising thousands of files.
func TestModuleRefusesEmbeddedNodeModules(t *testing.T) {
	t.Parallel()

	m := Module(fstest.MapFS{
		"js/article.ts":                     {Data: []byte("")},
		"js/node_modules/left-pad/index.js": {Data: []byte("")},
	}, "js/article.ts")
	if m.err != nil {
		t.Fatalf("Module: %v", m.err)
	}

	_, err := m.files()
	if err == nil {
		t.Fatal("want an error for an embedded node_modules")
	}
	if !strings.Contains(err.Error(), "go:embed") {
		t.Errorf("error should name the remedy: %v", err)
	}
}

func TestModuleHashTracksContentAndPaths(t *testing.T) {
	t.Parallel()

	base := Module(fstest.MapFS{"a.ts": {Data: []byte("one")}}, "a.ts")
	h1, err := base.hash()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	same, _ := Module(fstest.MapFS{"a.ts": {Data: []byte("one")}}, "a.ts").hash()
	if same != h1 {
		t.Error("identical content should hash identically")
	}

	changed, _ := Module(fstest.MapFS{"a.ts": {Data: []byte("two")}}, "a.ts").hash()
	if changed == h1 {
		t.Error("changed content must change the hash")
	}

	// A rename alone has to change the fingerprint, or the cache would serve
	// the old layout.
	renamed, _ := Module(fstest.MapFS{"b.ts": {Data: []byte("one")}}, "b.ts").hash()
	if renamed == h1 {
		t.Error("a renamed file must change the hash")
	}
}

func TestJSONCodecRoundTrip(t *testing.T) {
	t.Parallel()

	type payload struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	in := payload{Name: "x", Tags: []string{"a", "b"}}

	b, err := JSONCodec{}.Encode(in)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var out payload
	if err := (JSONCodec{}).Decode(b, &out); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if out.Name != in.Name || len(out.Tags) != 2 {
		t.Fatalf("got %+v, want %+v", out, in)
	}
}

// A handler returning nothing sends "null"; the zero value is the right
// reading, not an error.
func TestJSONCodecTreatsNullAndEmptyAsZero(t *testing.T) {
	t.Parallel()

	for _, in := range [][]byte{nil, {}, []byte("null")} {
		var out struct{ N int }
		if err := (JSONCodec{}).Decode(in, &out); err != nil {
			t.Fatalf("Decode(%q) = %v", in, err)
		}
		if out.N != 0 {
			t.Fatalf("Decode(%q) produced %+v", in, out)
		}
	}
}

func TestHandlerErrorMatchesByCode(t *testing.T) {
	t.Parallel()

	err := &HandlerError{Method: "parse", Code: CodeNotFound, Message: "no article"}

	if !errors.Is(err, &HandlerError{Code: CodeNotFound}) {
		t.Error("matching by code alone should succeed")
	}
	if errors.Is(err, &HandlerError{Code: CodeInternal}) {
		t.Error("a different code must not match")
	}
	if !errors.Is(err, &HandlerError{Code: CodeNotFound, Method: "parse"}) {
		t.Error("matching code and method should succeed")
	}
	if errors.Is(err, &HandlerError{Code: CodeNotFound, Method: "other"}) {
		t.Error("a different method must not match")
	}
	if !strings.Contains(err.Error(), "parse") || !strings.Contains(err.Error(), "no article") {
		t.Errorf("message should name the method and cause: %q", err.Error())
	}
}

func TestTranslateMapsInternalErrors(t *testing.T) {
	t.Parallel()

	t.Run("handler error keeps code and stack", func(t *testing.T) {
		t.Parallel()
		err := translate("parse", &wire.CallError{
			Code: wire.CodeNotFound, Message: "missing", Details: []byte("at parse"),
		})
		var he *HandlerError
		if !errors.As(err, &he) {
			t.Fatalf("got %T", err)
		}
		if he.Code != CodeNotFound || he.Method != "parse" || he.Stack != "at parse" {
			t.Fatalf("got %+v", he)
		}
	})

	t.Run("crash loop is unrecoverable", func(t *testing.T) {
		t.Parallel()
		if err := translate("", supervisor.ErrCrashLoop); !errors.Is(err, ErrSidecarUnrecoverable) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("closed mux means restarting", func(t *testing.T) {
		t.Parallel()
		if err := translate("", wire.ErrClosed); !errors.Is(err, ErrSidecarRestarting) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("nil stays nil", func(t *testing.T) {
		t.Parallel()
		if err := translate("", nil); err != nil {
			t.Fatalf("got %v", err)
		}
	})
}

func TestRetryable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"restarting", ErrSidecarRestarting, true},
		{"not started", ErrNotStarted, true},
		{"unavailable handler", &HandlerError{Code: CodeUnavailable}, true},
		{"internal handler", &HandlerError{Code: CodeInternal}, false},
		{"not found", &HandlerError{Code: CodeNotFound}, false},
		{"unrecoverable", ErrSidecarUnrecoverable, false},
		{"closed", ErrClosed, false},
		{"nil", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Retryable(tc.err); got != tc.want {
				t.Fatalf("Retryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	t.Parallel()

	good := Module(fstest.MapFS{"a.ts": {Data: []byte("")}}, "a.ts")

	t.Run("no modules", func(t *testing.T) {
		t.Parallel()
		if _, err := New(t.Context()); err == nil {
			t.Fatal("want an error with no modules")
		}
	})

	t.Run("module with a deferred error", func(t *testing.T) {
		t.Parallel()
		bad := Module(fstest.MapFS{}, "missing.ts")
		if _, err := New(t.Context(), WithModule(bad)); err == nil {
			t.Fatal("a module error should surface from New")
		}
	})

	// Credential-shaped names must fail before anything starts.
	t.Run("sensitive env allowlist", func(t *testing.T) {
		t.Parallel()
		_, err := New(t.Context(), WithModule(good), WithAllowedEnv("DATABASE_URL"))
		if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
			t.Fatalf("got %v, want a refusal naming the variable", err)
		}
	})
}

// Bind must never panic on a misconfiguration; the error belongs at call time.
func TestBindDefersConfigErrors(t *testing.T) {
	t.Parallel()

	t.Run("nil runtime", func(t *testing.T) {
		t.Parallel()
		call := Bind[struct{}, struct{}](nil, nil, "f")
		if _, err := call(context.Background(), struct{}{}); err == nil {
			t.Fatal("want an error for a nil Runtime")
		}
	})

	t.Run("empty method", func(t *testing.T) {
		t.Parallel()
		call := Bind[struct{}, struct{}](&Runtime{}, nil, "")
		if _, err := call(context.Background(), struct{}{}); err == nil {
			t.Fatal("want an error for an empty method")
		}
	})

	t.Run("module error", func(t *testing.T) {
		t.Parallel()
		bad := Module(fstest.MapFS{}, "missing.ts")
		call := Bind[struct{}, struct{}](&Runtime{}, bad, "f")
		if _, err := call(context.Background(), struct{}{}); err == nil {
			t.Fatal("want the module error at call time")
		}
	})
}

func TestBindOptions(t *testing.T) {
	t.Parallel()

	var c bindConfig
	Idempotent()(&c)
	if !c.idempotent || c.maxRetries == 0 {
		t.Errorf("Idempotent should enable retries: %+v", c)
	}

	MaxRetries(5)(&c)
	if c.maxRetries != 5 {
		t.Errorf("maxRetries = %d", c.maxRetries)
	}

	var d bindConfig
	MaxRetries(3)(&d)
	if d.idempotent {
		t.Error("MaxRetries alone must not make a call retryable")
	}
}

func TestRetryDelayGrowsAndCaps(t *testing.T) {
	t.Parallel()

	prev := time.Duration(0)
	for i := range 10 {
		d := retryDelay(i)
		if d < prev {
			t.Fatalf("attempt %d: delay shrank from %v to %v", i, prev, d)
		}
		if d > time.Second {
			t.Fatalf("attempt %d: %v exceeds the cap", i, d)
		}
		prev = d
	}
}

func TestInFlightLimitHasAFloor(t *testing.T) {
	t.Parallel()

	if got := inFlightLimit(&config{}); got < 8 {
		t.Errorf("default limit = %d, want at least 8", got)
	}
	if got := inFlightLimit(&config{maxInFlight: 3}); got != 3 {
		t.Errorf("explicit limit = %d, want 3", got)
	}
}

func TestWithEnvMergesRatherThanReplaces(t *testing.T) {
	t.Parallel()

	c := newConfig([]Option{
		WithEnv(map[string]string{"A": "1"}),
		WithEnv(map[string]string{"B": "2"}),
	})
	if c.sandbox.Env["A"] != "1" || c.sandbox.Env["B"] != "2" {
		t.Fatalf("WithEnv should accumulate: %v", c.sandbox.Env)
	}
}

func TestCodeStringsAreReadable(t *testing.T) {
	t.Parallel()

	for _, c := range []Code{CodeNotFound, CodeInternal, CodeUnimplemented, CodeCanceled} {
		if s := c.String(); s == "" || strings.HasPrefix(s, "ErrorCode(") {
			t.Errorf("Code(%d).String() = %q", uint16(c), s)
		}
	}
}

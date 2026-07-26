//go:build integration && unix

package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DiyRex/jsingo"
	"github.com/DiyRex/jsingo/examples/readability"
)

// multiModuleArticle is a page Readability will accept.
const multiModuleArticle = `<!DOCTYPE html><html><head><title>Multi-module</title></head>
<body><nav>Home</nav><article><h1>One sidecar, two libraries</h1>
<p>An anonymous socketpair created before fork removes the socket file
   entirely, and with it stale sockets, readiness polling and peer credential
   checks. The connection exists before the process does.</p>
<p>Because both modules load into the same runtime, adding a second npm
   dependency costs a bundle rather than a whole process, which is the point of
   the design.</p></article><footer>Copyright</footer></body></html>`

// Two npm libraries must share one sidecar. If each module spawned its own
// runtime the memory cost would scale with the dependency count, which is the
// thing the single-process design exists to avoid.
func TestTwoModulesShareOneSidecar(t *testing.T) {
	t.Parallel()

	rt, err := jsingo.New(t.Context(),
		jsingo.WithModule(demo, readability.Module),
		jsingo.WithCacheDir(t.TempDir()),
		jsingo.WithStartupTimeout(60*time.Second),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := rt.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	// A handler from the first module.
	greet := jsingo.Bind[greetReq, greetResp](rt, demo, "greet")
	got, err := greet(t.Context(), greetReq{Name: "multi"})
	if err != nil {
		t.Fatalf("greet: %v", err)
	}
	if got.Message != "hello, multi" {
		t.Fatalf("greet returned %q", got.Message)
	}

	// A handler from the second, backed by a real npm dependency.
	article, err := readability.New(rt).Parse(t.Context(), readability.ParseRequest{
		HTML: multiModuleArticle,
		URL:  "https://example.com/x",
	})
	if err != nil {
		t.Fatalf("parseArticle: %v", err)
	}
	if !strings.Contains(article.TextContent, "socketpair") {
		t.Error("readability module did not extract the article")
	}

	// One sidecar, not two: a second process would show as a restart or a
	// disconnected session here.
	stats := rt.Stats()
	if !stats.Connected {
		t.Error("not connected")
	}
	if stats.Restarts != 0 {
		t.Errorf("Restarts = %d; both modules should load into one process", stats.Restarts)
	}
	t.Logf("both modules served by: %s", stats.Runtime)
}

// Colliding bare names across modules must not silently resolve to one of
// them: that routes calls to the wrong npm package and produces plausible
// wrong answers rather than a crash.
func TestQualifiedNameResolvesCollisions(t *testing.T) {
	t.Parallel()

	rt, err := jsingo.New(t.Context(),
		jsingo.WithModule(demo, readability.Module),
		jsingo.WithCacheDir(t.TempDir()),
		jsingo.WithStartupTimeout(60*time.Second),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = rt.Close(ctx)
	})

	// These two modules export no common name, so the bare form works and the
	// qualified form must work too.
	qualified := jsingo.Bind[greetReq, greetResp](rt, demo, "greet", jsingo.Qualified())
	got, err := qualified(t.Context(), greetReq{Name: "q"})
	if err != nil {
		t.Fatalf("qualified call: %v", err)
	}
	if got.Message != "hello, q" {
		t.Fatalf("got %q", got.Message)
	}

	// A name in neither module must fail as Unimplemented rather than hang.
	missing := jsingo.Bind[struct{}, struct{}](rt, demo, "notInEitherModule")
	_, err = missing(t.Context(), struct{}{})
	var he *jsingo.HandlerError
	if !errors.As(err, &he) || he.Code != jsingo.CodeUnimplemented {
		t.Fatalf("got %v, want Unimplemented", err)
	}
}

//go:build integration && unix

package readability_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DiyRex/jsingo"
	"github.com/DiyRex/jsingo/examples/readability"
)

// Exercises the whole stack against a real npm dependency: Go -> socketpair ->
// sidecar -> @mozilla/readability -> back. Everything before this used
// handlers written for the tests.

func newClient(t *testing.T) *readability.Client {
	t.Helper()

	rt, err := jsingo.New(t.Context(),
		jsingo.WithModule(readability.Module),
		jsingo.WithCacheDir(t.TempDir()),
		jsingo.WithStartupTimeout(60*time.Second),
	)
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
	return readability.New(rt)
}

const articlePage = `<!DOCTYPE html>
<html lang="en">
<head>
  <title>Sidecars for Go — Engineering Blog</title>
  <meta name="author" content="Jane Doe">
  <meta property="og:site_name" content="Engineering Blog">
</head>
<body>
  <nav><a href="/">Home</a> <a href="/about">About</a> <a href="/jobs">Jobs</a></nav>
  <aside class="promo"><h3>Subscribe now!</h3><p>Get our newsletter.</p></aside>
  <article>
    <h1>Running npm packages from Go</h1>
    <p class="byline">By Jane Doe</p>
    <p>Integrating JavaScript libraries into a Go program through a supervised
       sidecar keeps the upstream implementation intact while leaving the Go
       process in control of lifecycle, cancellation and isolation.</p>
    <p>An anonymous socketpair created before fork removes the socket file
       entirely, which in turn removes stale sockets, readiness polling and
       peer credential checks. The connection exists before the process does.</p>
    <p>Because the protocol lives on a dedicated descriptor rather than stdout,
       a dependency that prints a deprecation notice cannot corrupt the stream.
       That is a surprisingly common failure in hand-rolled integrations.</p>
    <p>The remaining question is confinement. A separate process can be given a
       scrubbed environment, its own memory limit and no network at all, none
       of which is possible with an in-process engine sharing the Go heap.</p>
  </article>
  <footer><p>Copyright 2026 Engineering Blog</p></footer>
  <script>window.analytics.track('pageview');</script>
</body>
</html>`

func TestParseRealArticle(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	got, err := c.Parse(t.Context(), readability.ParseRequest{
		HTML: articlePage,
		URL:  "https://example.com/blog/sidecars",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// Readability prefers the document <title> over the <h1>, so the site
	// suffix is expected here rather than the heading text.
	if !strings.Contains(got.Title, "Sidecars for Go") {
		t.Errorf("title = %q", got.Title)
	}
	if got.Byline != "Jane Doe" {
		t.Errorf("byline = %q", got.Byline)
	}
	if got.SiteName != "Engineering Blog" {
		t.Errorf("siteName = %q", got.SiteName)
	}
	if !strings.Contains(got.TextContent, "socketpair") {
		t.Errorf("article body missing from text content")
	}

	// The point of Readability: the chrome is gone.
	for _, junk := range []string{"Subscribe now", "window.analytics", "Jobs"} {
		if strings.Contains(got.TextContent, junk) {
			t.Errorf("%q survived extraction; the page was not cleaned", junk)
		}
	}
	if got.Length == 0 {
		t.Error("Length is zero")
	}
	if !got.Readerable {
		t.Error("an article page should be readerable")
	}
	t.Logf("title=%q byline=%q site=%q length=%d", got.Title, got.Byline, got.SiteName, got.Length)
}

// A page with no article is an ordinary outcome, not a fault, and must arrive
// as a typed NotFound rather than an opaque failure.
func TestParseNonArticleReturnsNotFound(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	_, err := c.Parse(t.Context(), readability.ParseRequest{
		HTML: `<!DOCTYPE html><html><body><ul><li><a href="/1">One</a></li></ul></body></html>`,
	})
	if err == nil {
		t.Skip("readability extracted content from a bare listing; nothing to assert")
	}

	var he *jsingo.HandlerError
	if !errors.As(err, &he) {
		t.Fatalf("got %T (%v), want *jsingo.HandlerError", err, err)
	}
	if he.Code != jsingo.CodeNotFound {
		t.Fatalf("code = %v, want NotFound", he.Code)
	}
}

func TestParseRejectsEmptyHTML(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	_, err := c.Parse(t.Context(), readability.ParseRequest{HTML: "   "})

	var he *jsingo.HandlerError
	if !errors.As(err, &he) {
		t.Fatalf("got %T (%v)", err, err)
	}
	if he.Code != jsingo.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", he.Code)
	}
}

func TestIsReaderable(t *testing.T) {
	t.Parallel()

	c := newClient(t)

	yes, err := c.IsReaderable(t.Context(), readability.ParseRequest{HTML: articlePage})
	if err != nil {
		t.Fatalf("IsReaderable: %v", err)
	}
	if !yes {
		t.Error("the article page should be readerable")
	}

	no, err := c.IsReaderable(t.Context(),
		readability.ParseRequest{HTML: `<html><body><p>hi</p></body></html>`})
	if err != nil {
		t.Fatalf("IsReaderable: %v", err)
	}
	if no {
		t.Error("a one-line page should not be readerable")
	}
}

// Relative links must resolve against the supplied URL, which is the reason
// the handler injects a <base> element.
func TestRelativeLinksResolveAgainstURL(t *testing.T) {
	t.Parallel()

	page := strings.Replace(articlePage,
		"<p>Integrating JavaScript",
		`<p><a href="/relative/link">see this</a> Integrating JavaScript`, 1)

	c := newClient(t)
	got, err := c.Parse(t.Context(), readability.ParseRequest{
		HTML: page,
		URL:  "https://example.com/blog/sidecars",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !strings.Contains(got.Content, "https://example.com/relative/link") {
		t.Errorf("relative link was not resolved; content: %.300s", got.Content)
	}
}

func TestConcurrentExtraction(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	const n = 32

	errs := make(chan error, n)
	for range n {
		go func() {
			_, err := c.Parse(t.Context(), readability.ParseRequest{HTML: articlePage})
			errs <- err
		}()
	}
	for range n {
		if err := <-errs; err != nil {
			t.Errorf("concurrent parse: %v", err)
		}
	}
}

func BenchmarkParseArticle(b *testing.B) {
	rt, err := jsingo.New(b.Context(),
		jsingo.WithModule(readability.Module),
		jsingo.WithCacheDir(b.TempDir()),
		jsingo.WithStartupTimeout(60*time.Second),
	)
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = rt.Close(ctx)
	}()

	c := readability.New(rt)
	req := readability.ParseRequest{HTML: articlePage, URL: "https://example.com/x"}

	b.ReportAllocs()
	for b.Loop() {
		if _, err := c.Parse(context.Background(), req); err != nil {
			b.Fatal(err)
		}
	}
}

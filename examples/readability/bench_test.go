//go:build integration && unix

package readability_test

import (
	"context"
	"testing"
	"time"

	"github.com/DiyRex/jsingo"
	"github.com/DiyRex/jsingo/examples/readability"
)

func benchOn(b *testing.B, kind jsingo.RuntimeKind) *jsingo.Runtime {
	b.Helper()
	rt, err := jsingo.New(b.Context(),
		jsingo.WithModule(readability.Module),
		jsingo.WithCacheDir(b.TempDir()),
		jsingo.WithRuntime(kind),
		jsingo.WithStartupTimeout(60*time.Second),
	)
	if err != nil {
		b.Skipf("%s unavailable: %v", kind, err)
	}
	b.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = rt.Close(ctx)
	})
	return rt
}

func BenchmarkRTOverhead(b *testing.B) {
	for _, kind := range []jsingo.RuntimeKind{jsingo.Bun, jsingo.Node} {
		b.Run(string(kind), func(b *testing.B) {
			rt := benchOn(b, kind)
			noop := jsingo.Bind[struct{}, struct{ OK bool }](rt, readability.Module, "noop")
			b.ReportAllocs()
			for b.Loop() {
				if _, err := noop(context.Background(), struct{}{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkRTFull(b *testing.B) {
	for _, kind := range []jsingo.RuntimeKind{jsingo.Bun, jsingo.Node} {
		b.Run(string(kind), func(b *testing.B) {
			rt := benchOn(b, kind)
			c := readability.New(rt)
			req := readability.ParseRequest{HTML: articlePage, URL: "https://example.com/x"}
			b.ReportAllocs()
			for b.Loop() {
				if _, err := c.Parse(context.Background(), req); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// The reply's `content` field carries the cleaned HTML and is usually the
// largest thing crossing the boundary. Callers that only want the text pay for
// it anyway unless they ask for the narrower shape.
func BenchmarkReplyShape(b *testing.B) {
	rt := benchOn(b, jsingo.Bun)
	req := readability.ParseRequest{HTML: articlePage, URL: "https://example.com/x"}

	b.Run("with-html", func(b *testing.B) {
		c := readability.New(rt)
		b.ReportAllocs()
		for b.Loop() {
			if _, err := c.Parse(context.Background(), req); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("text-only", func(b *testing.B) {
		parse := jsingo.Bind[readability.ParseRequest, struct {
			Title       string `json:"title"`
			TextContent string `json:"textContent"`
			Length      int    `json:"length"`
		}](rt, readability.Module, "parseText")
		b.ReportAllocs()
		for b.Loop() {
			if _, err := parse(context.Background(), req); err != nil {
				b.Fatal(err)
			}
		}
	})
}

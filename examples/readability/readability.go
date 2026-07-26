// Package readability extracts the main article from a web page using
// Mozilla's Readability, the engine behind Firefox's Reader View.
//
// It is the reference jsingo module: a real npm dependency with no adequate Go
// equivalent, wrapped as typed Go functions.
//
// # Why a sidecar rather than a Go port
//
// Readability accumulates years of fixes for the markup real sites emit. The
// Go ports are community efforts that lag upstream. Running the upstream
// library keeps that work rather than reimplementing it.
package readability

import (
	"context"
	"embed"

	"github.com/DiyRex/jsingo"
)

// The bundle is committed build output with the npm dependencies inlined, so
// consumers need no JavaScript toolchain and no node_modules at runtime.
//
// The patterns are explicit and deliberately do not name a directory. A
// directory embed recurses and skips only "." and "_" prefixes, so it would
// pull in js/node_modules - 5.8 MB here - silently, with no error.
//
//go:generate bun build js/article.ts --target=node --format=esm --outfile=js/article.bundle.js
//go:embed js/article.bundle.js
var jsFS embed.FS

// Module is the JavaScript half of this package. Pass it to [jsingo.New].
var Module = jsingo.Module(jsFS, "js/article.bundle.js")

// ParseRequest is a page to extract.
type ParseRequest struct {
	// HTML is the raw page source.
	HTML string `json:"html"`
	// URL is the page's absolute address. Supplying it lets relative links and
	// image sources in the extracted content resolve correctly.
	URL string `json:"url,omitempty"`
}

// Article is the extracted content.
type Article struct {
	Title    string `json:"title"`
	Byline   string `json:"byline"`
	Excerpt  string `json:"excerpt"`
	SiteName string `json:"siteName"`
	Lang     string `json:"lang"`
	// Content is the cleaned article as HTML.
	Content string `json:"content"`
	// TextContent is the article as plain text, which is what an LLM or a
	// search index usually wants.
	TextContent string `json:"textContent"`
	// Length is the character count of TextContent.
	Length int `json:"length"`
	// Readerable reports whether the page looked like an article before
	// extraction was attempted.
	Readerable bool `json:"readerable"`
}

// Readerable is the cheap pre-check result.
type Readerable struct {
	Readerable bool `json:"readerable"`
}

// Client is a typed handle on the extractor.
type Client struct {
	parse     jsingo.Call[ParseRequest, Article]
	readerize jsingo.Call[ParseRequest, Readerable]
}

// New binds the extractor to a running Runtime.
//
// Both calls are marked idempotent: extraction is a pure function of its
// input, so a retry after a sidecar restart is safe and invisible.
func New(rt *jsingo.Runtime) *Client {
	return &Client{
		parse:     jsingo.Bind[ParseRequest, Article](rt, Module, "parseArticle", jsingo.Idempotent()),
		readerize: jsingo.Bind[ParseRequest, Readerable](rt, Module, "isReaderable", jsingo.Idempotent()),
	}
}

// Parse extracts the main article.
//
// A page with no extractable article returns a [jsingo.HandlerError] with
// [jsingo.CodeNotFound], which is an ordinary outcome for a listing or a
// landing page rather than a fault.
func (c *Client) Parse(ctx context.Context, req ParseRequest) (Article, error) {
	return c.parse(ctx, req)
}

// IsReaderable reports whether a page looks like an article, without paying
// for extraction. Useful for filtering a crawl.
func (c *Client) IsReaderable(ctx context.Context, req ParseRequest) (bool, error) {
	out, err := c.readerize(ctx, req)
	return out.Readerable, err
}

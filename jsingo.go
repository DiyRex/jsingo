// Package jsingo calls npm packages from Go via a supervised JavaScript
// runtime sidecar.
//
// A [Runtime] owns one long-lived bun or node process and a framed protocol
// over a Unix domain socket. Contracts are ordinary Go types: there is no
// protobuf, no IDL, and no code for the caller to generate.
//
//	//go:embed js/*.ts js/package.json
//	var jsFS embed.FS
//
//	var mod = jsingo.Module(jsFS, "js/article.ts")
//
//	rt, err := jsingo.New(ctx, jsingo.WithModule(mod))
//	defer rt.Close(ctx)
//
//	parse := jsingo.Bind[ParseReq, ParseResp](rt, mod, "parseArticle")
//	resp, err := parse(ctx, ParseReq{HTML: raw})
//
// Bind returns a plain function. Every exported function in a module's
// entrypoint is callable by name.
//
// # Embedding JavaScript
//
// Always embed with explicit patterns (js/*.ts) rather than a bare directory
// (js). A directory embed recurses and skips only entries prefixed with "."
// or "_", so a node_modules directory that appears alongside the source would
// be pulled into the binary silently. See docs/ARCHITECTURE.md.
package jsingo

// Version is the module version, set at build time via -ldflags.
var Version = "dev"

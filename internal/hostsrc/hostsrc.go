// Package hostsrc embeds the built JavaScript host.
//
// The TypeScript sources live in jsruntime/ where they have an LSP, types and
// their own test suite. dist/host.js is their build output, committed
// deliberately for two reasons: go:embed cannot reach outside a package
// directory, and `go get` must work on a machine with no JavaScript toolchain
// at all.
//
// Regenerate with `make host`. CI rebuilds it and fails if the committed copy
// differs, so the two cannot drift.
package hostsrc

import (
	_ "embed"
)

// Bundle is the host, ready to execute under bun, node or deno.
//
//go:embed dist/host.js
var Bundle []byte

// Name is the filename to write Bundle as. The extension matters: both
// runtimes select the module parser from it.
const Name = "jsingo-host.mjs"

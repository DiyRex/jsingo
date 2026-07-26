# jsingo

Call npm packages from Go, through a supervised JavaScript sidecar.

Some libraries only exist in the npm ecosystem, or exist there in a form years ahead of any Go port.
jsingo runs the upstream library in a separate process and gives you typed Go functions — no schema
language, no code generator, no JavaScript in string literals.

```go
//go:embed js/article.bundle.js
var jsFS embed.FS

var article = jsingo.Module(jsFS, "js/article.bundle.js")

type ParseReq  struct{ HTML string `json:"html"` }
type ParseResp struct{ Title, TextContent string }

func main() {
    rt, err := jsingo.New(ctx, jsingo.WithModule(article))
    defer rt.Close(ctx)

    parse := jsingo.Bind[ParseReq, ParseResp](rt, article, "parseArticle")
    resp, err := parse(ctx, ParseReq{HTML: raw})
}
```

The JavaScript side is an ordinary export:

```ts
import { Readability } from "@mozilla/readability";
import { parseHTML } from "linkedom";

export function parseArticle(req: { html: string }) {
  const { document } = parseHTML(req.html);
  const article = new Readability(document).parse();
  if (!article) throw new NotFound("no article content");
  return { title: article.title, textContent: article.textContent };
}
```

Every exported function is callable by name. No registration table, no `.proto`, no generated
packages.

---

## Why not an embedded engine

goja and v8go run JavaScript inside the Go process, which sounds simpler until you try to run a real
npm package: most depend on Node built-ins, and you end up maintaining polyfills instead of using
the library.

The separate process also turns out to be the security story rather than a compromise:

| | Embedded (goja, v8go) | jsingo sidecar |
|---|---|---|
| Shares the Go heap | yes | no |
| Sees your environment (secrets) | yes | **no — scrubbed by default** |
| Own memory limit | no | yes (cgroup) |
| Own network policy | no | yes |
| Killable when wedged | no | yes |
| Blast radius of an RCE | the whole process | one restartable process |

With an in-process engine, a sandbox escape *is* access to your connection pools and decrypted
secrets. There is no boundary to enforce.

---

## What you get

- **Typed calls.** `Bind[In, Out]` returns a plain `func(context.Context, In) (Out, error)`.
- **Real cancellation.** A cancelled Go context aborts the JavaScript handler through an
  `AbortSignal`, rather than just abandoning the result.
- **Crash recovery.** Exponential backoff with a sliding-window crash-loop budget.
- **No orphans.** `PR_SET_PDEATHSIG` on Linux; a heartbeat watchdog elsewhere, because macOS and the
  BSDs have no equivalent.
- **Deny-by-default isolation.** The sidecar inherits no environment unless you name variables, and
  credential-shaped names are refused outright.
- **One process for many modules.** Two npm libraries do not mean two runtimes.
- **Self-contained binaries.** Bundles are embedded; no `npm install`, no `node_modules` at runtime.

---

## Performance

Full article extraction with `@mozilla/readability`, steady state, Apple silicon:

| Stage | Cost |
|---|---|
| Transport + JSON round trip | 40–52 µs |
| linkedom DOM construction | ~146 µs |
| Readability extraction | ~383 µs |
| **Total** | **~330–510 µs** |

About 90% is the JavaScript work; jsingo is the other 10%. Two consequences:

- **Minify your bundle.** Worth 45% here — the runtime parses less at startup and the JIT settles
  sooner. `jsingo build` does it by default.
- **JavaScript is single-threaded.** Concurrent calls are multiplexed over one connection, but a
  CPU-bound handler serialises them. Measured, parallel calls are *slower* than serial. Scale
  sidecars, not callers.

Runtime choice depends on workload shape: bun's engine is ~19% faster on CPU-heavy work, while
node's transport is ~23% faster (`net.Socket` is event-loop native; bun cannot use it and falls back
to fs streams). Chatty small calls favour node, heavy calls favour bun.

Re-derive any of this: `go test -tags=integration -bench=. ./examples/readability/`.

---

## Installation

```sh
go get github.com/DiyRex/jsingo
```

Needs [bun](https://bun.sh) 1.0+, node 18+, or deno 1.30+ at runtime. `jsingo doctor` reports what
it finds and what that implies.

---

## Layout

Colocate JavaScript with the Go package that calls it — `go:embed` cannot reach outside its own
package directory, and the two change together anyway.

```
internal/article/
├── article.go            //go:embed js/article.bundle.js
└── js/
    ├── article.ts        source
    ├── article.bundle.js built by `jsingo build`, committed
    ├── package.json      { "jsingo": { "entry": "article.ts" } }
    └── bun.lock          committed
```

> **Always embed explicit patterns, never a bare directory.** `//go:embed js` recurses and skips
> only `.`- and `_`-prefixed entries, so a `node_modules` beside your sources lands in every binary
> silently — no error, no warning, tens of megabytes. `jsingo doctor` fails the build on it.

---

## Commands

```sh
jsingo build          # bundle every module in the tree (--ignore-scripts, minified)
jsingo build -check   # fail if any committed bundle is stale — for CI
jsingo doctor         # runtimes, orphan protection, embed hazards, lockfiles
```

---

## Security

The sidecar runs third-party npm code. Treat it as hostile.

The highest-value control is `--ignore-scripts` at install time: npm lifecycle hooks execute
arbitrary code on the build machine, outside every runtime sandbox, and are the most exploited
vector in the ecosystem. `jsingo build` always passes it.

At runtime the environment is scrubbed by default, `$HOME` points at an empty directory rather than
one holding `~/.aws/credentials`, and credential-shaped variable names are refused rather than
forwarded. Memory, CPU and network confinement belong to the container; `deploy/` carries manifests
with a read-only rootfs, dropped capabilities and default-deny egress.

Full threat model in `docs/SECURITY.md`. The isolation claims are covered by adversarial tests that
attempt the attacks rather than asserting the configuration.

---

## Status

Working and tested: protocol, supervision, isolation, public API and the readability example, on bun
and node across Linux and macOS.

Not yet done: Windows named pipes, the two-container topology (needs a UDS transport — see
`deploy/k8s-two-container.yaml`), streaming calls, and a protobuf codec (the `Codec` seam exists).

## License

MIT

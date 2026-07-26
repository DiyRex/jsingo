# jsingo

**Call npm packages from Go, through a supervised JavaScript sidecar.**

Some libraries only exist in the npm ecosystem, and some exist there in a form years ahead of any Go
port. jsingo runs the upstream library in a separate, supervised process and hands you typed Go
functions — no schema language, no code generator, no JavaScript in string literals.

```go
parse := jsingo.Bind[ParseReq, ParseResp](rt, article, "parseArticle")
resp, err := parse(ctx, ParseReq{HTML: raw})
```

---

## Contents

- [Why](#why) · [Quick start](#quick-start) · [How it works](#how-it-works)
- **Recipes:** [article extraction](#1-article-extraction-mozillareadability) ·
  [HTML sanitising](#2-html-sanitising-dompurify) ·
  [Markdown](#3-markdown-with-plugins-remark--rehype) ·
  [syntax highlighting](#4-syntax-highlighting-shiki) ·
  [maths](#5-maths-rendering-katex) ·
  [JSON Schema](#6-json-schema-validation-ajv) ·
  [HTML → Markdown](#7-html--markdown-turndown) ·
  [many modules](#8-several-libraries-one-sidecar)
- **Patterns:** [errors](#errors) · [cancellation](#cancellation-and-timeouts) ·
  [retries](#retries) · [concurrency](#concurrency-and-batching) · [testing](#testing)
- [Performance](#performance) · [Security](#security) · [Deployment](#deployment) ·
  [When not to use this](#when-not-to-use-jsingo) · [API](#api-reference)

---

## Why

The usual options both disappoint:

**Port it to Go.** You inherit maintenance of a moving target. Mozilla's Readability accrues fixes
for real-world markup continuously; the Go ports lag.

**Embed a JS engine** (goja, v8go). Most npm packages depend on Node built-ins, so you end up
maintaining polyfills instead of using the library. And the engine shares your process:

| | Embedded engine | jsingo sidecar |
|---|---|---|
| Shares the Go heap | yes | no |
| Sees your environment (secrets) | yes | **no — scrubbed by default** |
| Own memory limit | no | yes (cgroup) |
| Own network policy | no | yes |
| Killable when wedged | no | yes |
| Blast radius of an RCE | the whole process | one restartable process |

With an in-process engine, a sandbox escape *is* access to your connection pools and decrypted
secrets. There is no boundary to enforce. The separate process is the security story, not a
compromise.

---

## Quick start

```sh
go get github.com/DiyRex/jsingo
```

**1. Write the JavaScript.** An ordinary module — every exported function becomes callable.

```ts
// internal/text/js/text.ts
import { marked } from "marked";

export function toHTML(req: { markdown: string }): { html: string } {
  return { html: marked.parse(req.markdown, { async: false }) as string };
}
```

```jsonc
// internal/text/js/package.json
{
  "name": "@app/text",
  "private": true,
  "type": "module",
  "jsingo": { "entry": "text.ts" },
  "dependencies": { "marked": "14.1.4" }
}
```

**2. Bundle it.** Resolves dependencies and inlines them into one committed file.

```sh
go run github.com/DiyRex/jsingo/cmd/jsingo build
```

**3. Call it from Go.**

```go
package text

import (
    "context"
    "embed"

    "github.com/DiyRex/jsingo"
)

//go:embed js/text.bundle.js
var jsFS embed.FS

var Module = jsingo.Module(jsFS, "js/text.bundle.js")

type MarkdownReq struct{ Markdown string `json:"markdown"` }
type HTMLResp   struct{ HTML     string `json:"html"` }

func main() {
    rt, err := jsingo.New(ctx, jsingo.WithModule(Module))
    if err != nil { return err }
    defer rt.Close(ctx)

    toHTML := jsingo.Bind[MarkdownReq, HTMLResp](rt, Module, "toHTML")

    out, err := toHTML(ctx, MarkdownReq{Markdown: "# Hello"})
    // out.HTML == "<h1>Hello</h1>\n"
}
```

There is no `npm install` at runtime, no `node_modules` in your image, and no generated code to
review. The Go structs *are* the contract.

> **Embed explicit patterns, never a bare directory.** `//go:embed js` recurses and skips only
> `.`- and `_`-prefixed entries, so a `node_modules` beside your sources lands in every binary
> silently — no error, tens of megabytes. `jsingo doctor` fails the build on it.

---

## How it works

```
┌──────────────── your Go process ─────────────────┐
│  parse := jsingo.Bind[Req, Resp](rt, mod, "fn")  │
│         │                                         │
│    ┌────▼─────────────────────────────────┐       │
│    │ Runtime — supervise, multiplex, retry │      │
│    └────┬─────────────────────────────────┘       │
└─────────┼─────────────────────────────────────────┘
          │  anonymous socketpair (fd 3), no socket file
┌─────────▼─────────────────────────────────────────┐
│  bun / node — your bundle + @jsingo/host          │
│  dispatch · AbortSignal per call · heartbeat      │
└───────────────────────────────────────────────────┘
```

The descriptor is created **before** the fork, so the connection exists before the process does —
there is no socket path to collide, no stale file to sweep, and no startup race to poll for. The
protocol lives on fd 3 rather than stdout, so a dependency printing a deprecation notice cannot
corrupt the stream.

---

# Recipes

The first is implemented and tested in this repository under [`examples/readability`](examples/readability).
The rest are verified patterns — each package below was run to confirm the shape works — but you
adapt them to your own module.

## 1. Article extraction (`@mozilla/readability`)

The engine behind Firefox's Reader View. Strips navigation, ads and boilerplate from a page. Ideal
for RAG pipelines, bookmarking and search indexing.

```ts
import { Readability, isProbablyReaderable } from "@mozilla/readability";
import { parseHTML } from "linkedom";

export function parseArticle(req: { html: string; url?: string }) {
  const { document } = parseHTML(req.html);

  // Readability resolves relative hrefs against the document base. Without
  // this, extracted links from a real page go nowhere.
  if (req.url) {
    const base = document.createElement("base");
    base.setAttribute("href", req.url);
    document.head?.appendChild(base);
  }

  const readerable = isProbablyReaderable(document as never);
  const article = new Readability(document as never).parse();
  if (!article) throw Object.assign(new Error("no article content"), { code: 5 }); // NotFound

  return {
    title: article.title,
    textContent: article.textContent, // what an LLM or index wants
    content: article.content,         // cleaned HTML
    readerable,
  };
}
```

```go
type Article struct {
    Title       string `json:"title"`
    TextContent string `json:"textContent"`
    Content     string `json:"content"`
    Readerable  bool   `json:"readerable"`
}

parse := jsingo.Bind[ParseReq, Article](rt, Module, "parseArticle", jsingo.Idempotent())

art, err := parse(ctx, ParseReq{HTML: page, URL: pageURL})
if errors.Is(err, &jsingo.HandlerError{Code: jsingo.CodeNotFound}) {
    // A listing or landing page. An ordinary outcome, not a fault — do not
    // log this at error level or page anyone for it.
    return nil
}
```

**Note** `linkedom` rather than `jsdom`: a fraction of the weight, and Readability needs parsing and
traversal, not layout or script execution. Not executing scripts is also the point — the input is
untrusted HTML.

## 2. HTML sanitising (DOMPurify)

For user-generated HTML. Go's `bluemonday` is good; DOMPurify is the browser-side standard with the
longest record against mXSS and parser-differential bypasses.

> ### ⚠️ This one has a dangerous failure mode
>
> **DOMPurify silently becomes a no-op if its DOM is inadequate.** Verified here:
>
> | DOM | `isSupported` | `sanitize("<img src=x onerror=alert(1)><script>evil()</script>")` |
> |---|---|---|
> | `linkedom` | `undefined` | `<img src=x onerror=alert(1)><script>evil()</script>` — **unchanged** |
> | `jsdom` | `true` | `<img src="x">` — correctly stripped |
>
> It does not throw. It returns your input. Use **jsdom**, and assert `isSupported` at module load
> so a dependency bump cannot turn your sanitiser off quietly.

```ts
import createDOMPurify from "dompurify";
import { JSDOM } from "jsdom";

const purify = createDOMPurify(new JSDOM("").window as never);

// Fail at startup, not silently at request time. Without this guard an
// inadequate DOM turns sanitize() into an identity function.
if (!purify.isSupported) {
  throw new Error("DOMPurify is not supported in this environment; refusing to run");
}

export function sanitize(req: { html: string; allowImages?: boolean }): { html: string } {
  return {
    html: purify.sanitize(req.html, {
      ALLOWED_TAGS: ["b", "i", "em", "strong", "a", "p", "ul", "ol", "li", "code", "pre",
        ...(req.allowImages ? ["img"] : [])],
      ALLOWED_ATTR: ["href", "title", ...(req.allowImages ? ["src", "alt"] : [])],
      // Blocks javascript: and data: URLs.
      ALLOWED_URI_REGEXP: /^(?:https?|mailto):/i,
    }),
  };
}

/** Round-trips a known payload. Call from a health check to prove it is live. */
export function selfTest(): { ok: boolean } {
  return { ok: !purify.sanitize("<script>x()</script>").includes("script") };
}
```

```go
sanitize := jsingo.Bind[SanitizeReq, HTMLResp](rt, Module, "sanitize", jsingo.Idempotent())
clean, err := sanitize(ctx, SanitizeReq{HTML: userInput})
```

## 3. Markdown with plugins (remark / rehype)

`goldmark` is excellent, but the unified ecosystem has hundreds of plugins — GFM, footnotes,
directives, frontmatter, autolinked headings, MDX — that have no Go equivalent.

```ts
import { unified } from "unified";
import remarkParse from "remark-parse";
import remarkGfm from "remark-gfm";
import remarkRehype from "remark-rehype";
import rehypeSlug from "rehype-slug";
import rehypeAutolinkHeadings from "rehype-autolink-headings";
import rehypeStringify from "rehype-stringify";

// Build the pipeline once at module load, not per call. Plugin resolution is
// the expensive part and it is identical for every request.
const pipeline = unified()
  .use(remarkParse)
  .use(remarkGfm)
  .use(remarkRehype, { allowDangerousHtml: false })
  .use(rehypeSlug)
  .use(rehypeAutolinkHeadings, { behavior: "wrap" })
  .use(rehypeStringify);

export async function render(req: { markdown: string }): Promise<{ html: string }> {
  const file = await pipeline.process(req.markdown);
  return { html: String(file) };
}
```

Handlers may be `async`; jsingo awaits them.

## 4. Syntax highlighting (Shiki)

Shiki uses the actual VS Code TextMate grammars and themes, so output matches your editor exactly.
Go's `chroma` uses its own lexers and diverges on edge cases.

```ts
import { createHighlighter, type Highlighter } from "shiki";

// Loading grammars costs hundreds of milliseconds. Do it once, lazily, and
// share the promise so concurrent first calls do not each trigger a load.
let ready: Promise<Highlighter> | undefined;
function highlighter(): Promise<Highlighter> {
  ready ??= createHighlighter({
    themes: ["github-dark", "github-light"],
    langs: ["go", "typescript", "python", "sql", "bash", "json"],
  });
  return ready;
}

export async function highlight(
  req: { code: string; lang: string; theme?: string },
): Promise<{ html: string }> {
  const hl = await highlighter();

  // An unknown language must not fall back silently to plaintext without the
  // caller knowing; report it so they can fix the input.
  if (!hl.getLoadedLanguages().includes(req.lang)) {
    throw Object.assign(new Error(`language not loaded: ${req.lang}`), { code: 3 }); // InvalidArgument
  }
  return { html: hl.codeToHtml(req.code, { lang: req.lang, theme: req.theme ?? "github-dark" }) };
}

/** Warms the grammar cache so the first real request is not the slow one. */
export async function warmup(): Promise<{ ok: boolean }> {
  await highlighter();
  return { ok: true };
}
```

```go
// Call warmup right after New so the first user request does not pay for it.
warm := jsingo.Bind[struct{}, struct{ OK bool }](rt, Module, "warmup")
if _, err := warm(ctx, struct{}{}); err != nil {
    log.Warn("highlighter warmup failed", "error", err)
}
```

## 5. Maths rendering (KaTeX)

LaTeX → HTML/MathML, server-side, no browser. There is no comparable Go library.

```ts
import katex from "katex";

export function renderMath(req: { tex: string; display?: boolean }): { html: string } {
  try {
    return {
      html: katex.renderToString(req.tex, {
        displayMode: req.display ?? false,
        throwOnError: true,
        strict: "warn",
        // Bounds macro expansion: a crafted input can otherwise expand
        // exponentially and wedge the sidecar.
        maxExpand: 1000,
      }),
    };
  } catch (e) {
    // A LaTeX syntax error is the caller's input problem, not a server fault.
    throw Object.assign(new Error((e as Error).message), { code: 3 }); // InvalidArgument
  }
}
```

## 6. JSON Schema validation (Ajv)

Ajv is the reference implementation for draft 2020-12, including `$dynamicRef` and full annotation
output that Go validators tend to partially support.

```ts
import { Ajv, type ValidateFunction } from "ajv";
import addFormats from "ajv-formats";

const ajv = addFormats(new Ajv({ allErrors: true, strict: true }));

// Compilation is the expensive step. Cache by schema identity so a hot path
// validating the same schema does not recompile per request.
const cache = new Map<string, ValidateFunction>();

export function validate(req: { schema: unknown; data: unknown }): {
  valid: boolean;
  errors: string[];
} {
  const key = JSON.stringify(req.schema);
  let fn = cache.get(key);
  if (!fn) {
    try {
      fn = ajv.compile(req.schema as object);
    } catch (e) {
      throw Object.assign(new Error(`invalid schema: ${(e as Error).message}`), { code: 3 });
    }
    // Bound the cache: an attacker submitting unique schemas would otherwise
    // grow it without limit.
    if (cache.size > 256) cache.clear();
    cache.set(key, fn);
  }

  const valid = fn(req.data) as boolean;
  return {
    valid,
    errors: (fn.errors ?? []).map((e) => `${e.instancePath || "/"} ${e.message}`),
  };
}
```

## 7. HTML → Markdown (Turndown)

For archiving scraped pages or feeding an LLM a compact representation.

```ts
import TurndownService from "turndown";
import { gfm } from "turndown-plugin-gfm";

const service = new TurndownService({
  headingStyle: "atx",
  codeBlockStyle: "fenced",
  bulletListMarker: "-",
}).use(gfm);

// Scripts and styles have no markdown equivalent and would otherwise be
// emitted as their raw text content.
service.remove(["script", "style", "noscript"]);

export function toMarkdown(req: { html: string }): { markdown: string } {
  return { markdown: service.turndown(req.html) };
}
```

Pairs naturally with recipe 1: extract the article, then convert it.

```go
art, err := parse(ctx, ParseReq{HTML: page, URL: u})
if err != nil { return err }

md, err := toMarkdown(ctx, HTMLReq{HTML: art.Content})
```

## 8. Several libraries, one sidecar

Two npm packages do **not** mean two processes. Modules load into one runtime and share it.

```go
rt, err := jsingo.New(ctx,
    jsingo.WithModule(readability.Module, sanitize.Module, markdown.Module),
    jsingo.WithLogger(logger),
)
defer rt.Close(ctx)
```

Every exported function is callable by bare name. When two modules export the *same* name the bare
form is ambiguous and is **removed rather than silently resolved** — shadowing would route calls to
the wrong library and produce plausible wrong answers instead of a crash. Address those with
`jsingo.Qualified()`:

```go
// Calls sanitize:render rather than markdown:render.
render := jsingo.Bind[Req, Resp](rt, sanitize.Module, "render", jsingo.Qualified())
```

---

# Patterns

## Errors

A JavaScript throw arrives as `*jsingo.HandlerError` with a code, a message and the JS stack.

```go
art, err := parse(ctx, req)

var he *jsingo.HandlerError
switch {
case errors.Is(err, &jsingo.HandlerError{Code: jsingo.CodeNotFound}):
    return nil, nil                       // expected: no article on this page

case errors.As(err, &he):
    // The stack names internal paths and dependency versions. Log it; never
    // return it to a client.
    log.Error("handler failed", "code", he.Code, "msg", he.Message, "stack", he.Stack)
    return nil, fmt.Errorf("extraction failed")

case errors.Is(err, jsingo.ErrSidecarUnrecoverable):
    return nil, err                       // terminal: fail the liveness probe

case jsingo.Retryable(err):
    return nil, fmt.Errorf("temporarily unavailable: %w", err)
}
```

Set the code from JavaScript by putting a numeric `code` on the thrown error:

| JS `code` | Go | Suggested HTTP |
|---|---|---|
| `3` | `CodeInvalidArgument` | 400 |
| `5` | `CodeNotFound` | 404 / 422 |
| `12` | `CodeUnimplemented` | 501 |
| `14` | `CodeUnavailable` | 503 |
| *(anything else)* | `CodeInternal` | 500 |

The check is structural, not `instanceof` — a handler module that resolves its own copy of the
package still gets its codes through. (`instanceof` across duplicate module instances is the npm
dual-package hazard, and it silently downgrades every typed error to `Internal`.)

## Cancellation and timeouts

Cancelling a Go context aborts the JavaScript handler, rather than merely abandoning the result.

```ts
export function slow(req: Req, signal: AbortSignal): Promise<Resp> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => resolve(work(req)), 5000);
    signal.addEventListener("abort", () => {
      clearTimeout(timer);            // stop the work, not just the reply
      reject(new DOMException("aborted", "AbortError"));
    });
  });
}
```

Without honouring the signal, a cancelled request leaves the sidecar burning CPU on a result nobody
will read — the most common defect in hand-rolled sidecars.

```go
// Per-call default, used only when the caller's context has no deadline.
parse := jsingo.Bind[Req, Resp](rt, mod, "parse", jsingo.Timeout(5*time.Second))
```

## Retries

Opt-in **per binding**, never global — because whether a repeat is safe is a fact about your
operation, not about the transport.

```go
// Safe: a pure transform. A restart mid-call is invisible.
parse := jsingo.Bind[Req, Resp](rt, mod, "parse", jsingo.Idempotent())

// Not safe: leave retries off. Repeating this after a restart charges twice.
charge := jsingo.Bind[Req, Resp](rt, mod, "chargeCard")
```

## Concurrency and batching

Calls from many goroutines are multiplexed over one connection. But **JavaScript is
single-threaded**: a CPU-bound handler serialises them. Measured here, parallel calls are *slower*
than serial.

```go
// Wrong instinct for CPU-bound work: this does not add throughput.
for _, page := range pages {
    go parse(ctx, page)
}
```

Two things that do work:

**Batch inside one call** — amortises the round trip and lets the handler reuse setup.

```ts
export function parseMany(req: { pages: Page[] }): { results: Result[] } {
  return { results: req.pages.map(parseOne) };
}
```

**Scale sidecars, not callers** — several `Runtime`s, or replicas in production.

```go
pool := make([]*jsingo.Runtime, runtime.NumCPU())
for i := range pool {
    pool[i], _ = jsingo.New(ctx, jsingo.WithModule(mod))
}
```

`WithMaxInFlight` bounds queueing; the default is derived from `GOMAXPROCS`.

## Testing

Handlers are ordinary functions — test them in JavaScript, with no transport involved:

```ts
import { expect, test } from "bun:test";
import { parseArticle } from "../src/article.ts";

test("strips navigation", () => {
  const out = parseArticle({ html: pageWithNav });
  expect(out.textContent).not.toContain("Home");
});
```

Test the Go side against a real sidecar behind a build tag, so `go test ./...` stays green on a
machine with no JavaScript runtime:

```go
//go:build integration
```

```sh
go test ./...                      # no runtime needed
go test -tags=integration ./...    # spawns bun/node
```

> Run integration tests with **`-count=2`** in CI. Several real defects here appeared only on a
> second run — goroutines logging after their test completed, and a per-test bundler invocation that
> deadlocked under contention.

---

## Performance

Full article extraction with `@mozilla/readability`, steady state, Apple silicon:

| Stage | Cost |
|---|---|
| Transport + JSON round trip | 40–52 µs |
| linkedom DOM construction | ~146 µs |
| Readability extraction | ~383 µs |
| **Total** | **~330–510 µs** |

About **90% is the JavaScript work**; jsingo is the other 10%. So:

- **Minify your bundle.** Worth 45% here — less to parse at startup, and the JIT settles sooner.
  `jsingo build` does it by default.
- **Warm up expensive handlers** at boot (see the Shiki recipe) rather than paying on first request.
- **Return less.** Omitting a large field you do not read cut 29% of allocations in the readability
  example.
- **Do not reach for goroutines** to speed up CPU-bound handlers. Scale sidecars.

**Runtime choice depends on workload shape:** bun's engine is ~19% faster on CPU-heavy work, while
node's transport is ~23% faster (`net.Socket` is event-loop native; bun cannot use it and falls back
to fs streams). Chatty small calls favour node; heavy calls favour bun.

Re-derive any of this: `go test -tags=integration -bench=. ./examples/readability/`.

---

## Security

**Treat the sidecar as hostile.** Not because your dependencies are malicious today, but because you
cannot audit the transitive closure of an npm install.

The highest-value control is at **build** time: `--ignore-scripts`. npm lifecycle hooks execute
arbitrary code on the build machine, outside every runtime sandbox, and are the most exploited
vector in the ecosystem. `jsingo build` always passes it.

At **runtime**, the environment is deny-by-default:

```go
rt, err := jsingo.New(ctx,
    jsingo.WithModule(mod),
    // Everything not named here is dropped. Credential-shaped names
    // (AWS_*, *TOKEN*, DATABASE_*) are refused outright.
    jsingo.WithAllowedEnv("LOG_LEVEL", "APP_REGION"),
    // Pass a narrowly-scoped value deliberately if a library truly needs one.
    jsingo.WithEnv(map[string]string{"API_KEY": readOnlyKey}),
)
```

`$HOME` and `$TMPDIR` point at an empty 0700 directory, so a dependency walking `$HOME` for
`~/.aws/credentials` or `~/.ssh/id_rsa` finds nothing.

Memory, CPU and network confinement belong to the container — a flag passed to a JS runtime is
*cooperative* and code inside it may undo it; a cgroup is not. [`deploy/`](deploy/) carries manifests
with a read-only rootfs, dropped capabilities and default-deny egress that blocks the cloud metadata
endpoint.

Full threat model in [`docs/SECURITY.md`](docs/SECURITY.md). The isolation claims are covered by
adversarial tests that *attempt* the attacks — dump the environment, read credential paths, dial
`169.254.169.254` — rather than asserting the configuration.

---

## Deployment

One image, one binary, no `node_modules` in production. The bundle is embedded, so the Go call site
and the JavaScript handler are versioned together and roll back together.

Three probes with three different jobs — collapsing them is the usual mistake:

```go
// readyz: transient. Sheds traffic during a respawn (a few hundred ms).
if err := rt.Ping(ctx); err != nil { http.Error(w, "not ready", 503); return }

// healthz: terminal only. Restarting the pod over a routine respawn turns a
// recoverable blip into an outage.
if errors.Is(rt.Err(), jsingo.ErrSidecarUnrecoverable) { http.Error(w, "", 500); return }
```

`terminationGracePeriodSeconds` must exceed `WithShutdownGrace`, or the kubelet tears the container
down mid-escalation. See [`examples/server`](examples/server) for a complete service and
[`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md) for the rest.

---

## When *not* to use jsingo

Being honest about the boundaries:

| Situation | Do this instead |
|---|---|
| **Go already has a good library** | Use it. `goldmark`, `bluemonday`, `chroma` are strong. Reach for jsingo when the npm version is genuinely better or has no counterpart. |
| **The package needs native bindings** | Won't work. `sharp`, `canvas`, `better-sqlite3` and anything else with a `.node` binary cannot be bundled into a single file. |
| **You need sub-100 µs latency** | The round trip alone is ~40–50 µs before any work. Fine for millisecond operations, wrong for a tight inner loop. |
| **The call is on a hot CPU-bound path** | One sidecar is one thread. Measure before committing. |
| **You only need one trivial function** | Porting fifty lines beats operating a second process. |

jsingo earns its keep when the library is substantial, actively maintained upstream, and painful to
reimplement — Readability, Shiki, the unified ecosystem, DOMPurify, KaTeX.

---

## API reference

**Setup**

| | |
|---|---|
| `Module(fsys, entry)` | Declare a JS module from an embedded filesystem |
| `New(ctx, opts...)` | Start a supervised sidecar; returns once it answers |
| `Bind[In, Out](rt, mod, method, opts...)` | Typed `func(ctx, In) (Out, error)` |
| `rt.Close(ctx)` / `Ping` / `Err` / `Stats` | Lifecycle and health |

**Options** — `WithModule`, `WithRuntime`, `WithRuntimePath`, `WithLogger`, `WithCodec`,
`WithAllowedEnv`, `WithEnv`, `WithSandbox`, `WithMaxHeapMB`, `WithStartupTimeout`,
`WithRestartPolicy`, `WithBackoff`, `WithShutdownGrace`, `WithHeartbeat`, `WithMaxFrameSize`,
`WithMaxReplyBytes`, `WithMaxInFlight`, `WithCacheDir`

**Bind options** — `Idempotent`, `MaxRetries`, `Timeout`, `Qualified`

**Errors** — `HandlerError` (`Code`, `Message`, `Stack`), `Retryable(err)`, and sentinels
`ErrClosed`, `ErrSidecarRestarting`, `ErrSidecarUnrecoverable`, `ErrNoRuntime`

**CLI**

```sh
jsingo build          # bundle every module (--ignore-scripts, minified)
jsingo build -check   # fail if a committed bundle is stale — for CI
jsingo doctor         # runtimes, orphan protection, embed hazards, lockfiles
```

---

## Requirements

Go 1.24+, and one of [bun](https://bun.sh) 1.0+ / node 18+ / deno 1.30+ at runtime.
`jsingo doctor` reports what it finds and what that implies. Linux and macOS; Windows is not
supported yet.

## Status

**Working and tested:** protocol, supervision, isolation, the public API, the CLI, multi-module
sidecars, and the readability example — on bun and node, across Linux and macOS.

**Not yet:** Windows named pipes, the two-container topology (needs a UDS transport — see
[`deploy/k8s-two-container.yaml`](deploy/k8s-two-container.yaml)), streaming calls, and a protobuf
codec (the `Codec` seam exists).

## License

MIT — see [LICENSE](LICENSE).

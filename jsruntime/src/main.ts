/**
 * Default entrypoint: `bun run main.ts <entry.ts>` or with $JSINGO_ENTRY set.
 *
 * The bundled build replaces this with a generated entrypoint that imports its
 * modules statically, so nothing is resolved at runtime. This dynamic form is
 * for development and for the Go integration tests.
 */

import process from "node:process";
import { serve } from "./host.ts";

// Every argument is a module entrypoint. Several modules share one sidecar:
// two npm libraries do not mean two runtimes.
const entries = process.argv.slice(2).filter(Boolean);
if (entries.length === 0) {
  const fromEnv = process.env["JSINGO_ENTRY"];
  if (fromEnv) entries.push(fromEnv);
}
if (entries.length === 0) {
  process.stderr.write("jsingo host: no entry modules (argv or $JSINGO_ENTRY)\n");
  process.exit(2);
}

const modules: Record<string, Record<string, unknown>> = {};
for (const entry of entries) {
  // Strip the directory and every JS/TS extension, including the ".bundle"
  // infix that `jsingo build` produces, so "article.bundle.js" registers as
  // "article" and matches the module name Go derives.
  const base = entry.replace(/^.*\//, "").replace(/\.[cm]?[jt]sx?$/, "");
  const name = base.replace(/\.bundle$/, "");
  modules[name] = (await import(entry)) as Record<string, unknown>;
}

await serve({ modules });

/**
 * Default entrypoint: `bun run main.ts <entry.ts>` or with $JSINGO_ENTRY set.
 *
 * The bundled build replaces this with a generated entrypoint that imports its
 * modules statically, so nothing is resolved at runtime. This dynamic form is
 * for development and for the Go integration tests.
 */

import process from "node:process";
import { serve } from "./host.ts";

const entry = process.argv[2] ?? process.env["JSINGO_ENTRY"];
if (!entry) {
  process.stderr.write("jsingo host: no entry module (argv[2] or $JSINGO_ENTRY)\n");
  process.exit(2);
}

const moduleName = entry.replace(/^.*\//, "").replace(/\.[cm]?[jt]s$/, "");
const exports = (await import(entry)) as Record<string, unknown>;

await serve({ modules: { [moduleName]: exports } });

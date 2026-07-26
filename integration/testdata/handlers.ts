/** Handlers exercised by the Go integration tests. */

export function echo(req: unknown): unknown {
  return req;
}

export function add(req: { a: number; b: number }): { sum: number } {
  return { sum: req.a + req.b };
}

/** Verifies the sidecar can use a real runtime API rather than only arithmetic. */
export function upper(req: { s: string }): { s: string } {
  return { s: req.s.toLocaleUpperCase() };
}

export function boom(): never {
  throw new Error("handler exploded");
}

export async function notFound(): Promise<never> {
  const { NotFound } = await import("../../jsruntime/src/errors.ts");
  throw new NotFound("no article content");
}

/** Blocks until aborted, so Go can assert cancellation reaches the handler. */
export function slow(_req: unknown, signal: AbortSignal): Promise<never> {
  return new Promise((_resolve, reject) => {
    if (signal.aborted) {
      reject(new DOMException("aborted", "AbortError"));
      return;
    }
    signal.addEventListener("abort", () => {
      reject(new DOMException("aborted", "AbortError"));
    });
  });
}

/** Reports whether the abort signal fired, proving CANCEL reached the handler. */
let lastAborted = false;
export function abortable(_req: unknown, signal: AbortSignal): Promise<{ ok: boolean }> {
  return new Promise((resolve) => {
    signal.addEventListener("abort", () => {
      lastAborted = true;
    });
    setTimeout(() => resolve({ ok: true }), 50);
  });
}
export function wasAborted(): { aborted: boolean } {
  return { aborted: lastAborted };
}

export function kill(): never {
  process.exit(3);
}

// --- adversarial handlers -------------------------------------------------
//
// These stand in for what a compromised npm dependency does first: read the
// environment for credentials, walk the filesystem for key material, and open
// an outbound connection to exfiltrate. The Go tests assert the sandbox holds.

/** Everything the sidecar can see in its environment. */
export function dumpEnv(): { keys: string[]; values: Record<string, string> } {
  const values: Record<string, string> = {};
  for (const [k, v] of Object.entries(process.env)) {
    if (typeof v === "string") values[k] = v;
  }
  return { keys: Object.keys(values).sort(), values };
}

/** Attempts to read an arbitrary path, as a credential stealer would. */
export async function readPath(req: { path: string }): Promise<{ ok: boolean; err?: string }> {
  try {
    const fs = await import("node:fs/promises");
    await fs.readFile(req.path, "utf8");
    return { ok: true };
  } catch (e) {
    return { ok: false, err: (e as Error).message };
  }
}

/** Attempts an outbound TCP connection, as an exfiltration payload would. */
export async function connectOut(req: {
  host: string;
  port: number;
}): Promise<{ ok: boolean; err?: string }> {
  const net = await import("node:net");
  return new Promise((resolve) => {
    const sock = net.connect({ host: req.host, port: req.port });
    const done = (ok: boolean, err?: string) => {
      sock.destroy();
      resolve(err === undefined ? { ok } : { ok, err });
    };
    sock.setTimeout(2000, () => done(false, "timeout"));
    sock.on("connect", () => done(true));
    sock.on("error", (e: Error) => done(false, e.message));
  });
}

/** Reports whether eval is reachable, which most obfuscated payloads need. */
export function canEval(): { ok: boolean; err?: string } {
  try {
    // eslint-disable-next-line no-eval
    const f = new Function("return 1 + 1");
    return { ok: f() === 2 };
  } catch (e) {
    return { ok: false, err: (e as Error).message };
  }
}

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

/** Handlers for the public-API integration test. */

export function greet(req: { name: string }): { message: string } {
  return { message: `hello, ${req.name}` };
}

export function sum(req: { values: number[] }): { total: number } {
  return { total: req.values.reduce((a, b) => a + b, 0) };
}

export function fail(): never {
  throw new Error("deliberate failure");
}

export function slowEcho(req: { ms: number }, signal: AbortSignal): Promise<{ done: boolean }> {
  return new Promise((resolve, reject) => {
    const t = setTimeout(() => resolve({ done: true }), req.ms);
    signal.addEventListener("abort", () => {
      clearTimeout(t);
      reject(new DOMException("aborted", "AbortError"));
    });
  });
}

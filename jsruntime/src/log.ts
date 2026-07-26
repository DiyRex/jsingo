/**
 * Structured logging from the sidecar.
 *
 * Records travel as LOG frames on the protocol socket, not stderr, so they
 * arrive interleaved with calls, survive a noisy npm dependency writing to
 * stderr, and reach Go's slog with their fields intact. stderr remains the
 * fallback for failures before the socket is usable - a syntax error in the
 * bundle, a missing native module - which the supervisor captures separately.
 */

export type Level = "debug" | "info" | "warn" | "error";

export interface LogSink {
  debug(msg: string, fields?: Record<string, unknown>): void;
  info(msg: string, fields?: Record<string, unknown>): void;
  warn(msg: string, fields?: Record<string, unknown>): void;
  error(msg: string, fields?: Record<string, unknown>): void;
}

/** Emits one NDJSON record per call to the supplied writer. */
export class Logger implements LogSink {
  constructor(private readonly write: (record: string) => void) {}

  debug(msg: string, fields?: Record<string, unknown>): void {
    this.#emit("debug", msg, fields);
  }
  info(msg: string, fields?: Record<string, unknown>): void {
    this.#emit("info", msg, fields);
  }
  warn(msg: string, fields?: Record<string, unknown>): void {
    this.#emit("warn", msg, fields);
  }
  error(msg: string, fields?: Record<string, unknown>): void {
    this.#emit("error", msg, fields);
  }

  #emit(level: Level, msg: string, fields?: Record<string, unknown>): void {
    let record: string;
    try {
      record = JSON.stringify({ level, msg, ...fields });
    } catch {
      // A field containing a cycle or a BigInt must not take down the logger,
      // which is often being called from an error path already.
      record = JSON.stringify({ level, msg, fieldsError: "not serialisable" });
    }
    try {
      this.write(record);
    } catch {
      // The socket is gone. There is nowhere left to report that.
    }
  }
}

/** A sink that discards everything, for tests and standalone runs. */
export const noopLogger: LogSink = {
  debug() {},
  info() {},
  warn() {},
  error() {},
};

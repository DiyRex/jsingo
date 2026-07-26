/**
 * Errors a handler can throw to control the code Go receives.
 *
 * A plain thrown Error becomes Internal, which is the right default: an
 * unexpected failure should not be mistaken for a meaningful outcome. Throwing
 * one of these instead lets Go branch with errors.Is on a specific code.
 */

import { ErrorCode } from "./frame.ts";

export class JsingoError extends Error {
  readonly code: number;

  constructor(code: number, message: string) {
    super(message);
    this.name = new.target.name;
    this.code = code;
  }
}

/** The requested thing does not exist. Go sees wire.CodeNotFound. */
export class NotFound extends JsingoError {
  constructor(message = "not found") {
    super(ErrorCode.NotFound, message);
  }
}

/** The request was malformed. Go sees wire.CodeInvalidArgument. */
export class InvalidArgument extends JsingoError {
  constructor(message = "invalid argument") {
    super(ErrorCode.InvalidArgument, message);
  }
}

/** The operation is not supported. Go sees wire.CodeUnimplemented. */
export class Unimplemented extends JsingoError {
  constructor(message = "unimplemented") {
    super(ErrorCode.Unimplemented, message);
  }
}

/** A dependency is temporarily unavailable; the caller may retry. */
export class Unavailable extends JsingoError {
  constructor(message = "unavailable") {
    super(ErrorCode.Unavailable, message);
  }
}

const KNOWN_CODES: ReadonlySet<number> = new Set(Object.values(ErrorCode));

/**
 * Maps a thrown value to a wire error code.
 *
 * The check is structural, not `instanceof`. A handler module frequently
 * resolves its own copy of this package - the npm dual-package hazard - and
 * then `err instanceof JsingoError` is false even though the error is one,
 * silently downgrading every typed error to Internal. Matching on the shape
 * survives duplicate module instances, bundling and realm boundaries.
 *
 * Only codes we define are accepted, so a handler cannot smuggle an arbitrary
 * number onto the wire by setting `.code` to something unrelated - `code` is
 * a common property name on Node system errors (ENOENT, ECONNREFUSED), which
 * would otherwise be misread as a wire code.
 */
export function codeOf(err: unknown): number {
  if (err instanceof JsingoError) return err.code;

  // AbortController rejects with a DOMException named AbortError.
  if (err instanceof Error && err.name === "AbortError") return ErrorCode.Canceled;

  if (typeof err === "object" && err !== null && "code" in err) {
    const code = (err as { code: unknown }).code;
    if (typeof code === "number" && KNOWN_CODES.has(code)) return code;
  }
  return ErrorCode.Internal;
}

/** Extracts a human-readable message from any thrown value. */
export function messageOf(err: unknown): string {
  if (err instanceof Error) return err.message || err.name;
  if (typeof err === "string") return err;
  try {
    return JSON.stringify(err) ?? String(err);
  } catch {
    return String(err);
  }
}

/**
 * Extracts a stack trace for the ERROR frame's details.
 *
 * This is the single most useful thing a Go developer can get when a handler
 * inside a minified npm bundle fails, so it is always sent rather than being
 * hidden behind a debug flag.
 */
export function detailsOf(err: unknown): Uint8Array {
  const stack = err instanceof Error && err.stack ? err.stack : "";
  return stack ? new TextEncoder().encode(stack) : new Uint8Array(0);
}

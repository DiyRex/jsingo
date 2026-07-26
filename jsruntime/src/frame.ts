/**
 * Framing protocol, mirroring internal/wire/frame.go byte for byte.
 *
 *   ┌────────────┬─────────┬───────────┬─────────────┐
 *   │ len uint32 │ type u8 │ id uint64 │ payload ... │
 *   └────────────┴─────────┴───────────┴─────────────┘
 *    big endian    len counts type+id+payload, not itself
 *
 * Any change here needs the same change in frame.go. The Go fuzzers assert
 * decode/re-encode is byte-identical, so a divergence surfaces as a protocol
 * error rather than silent corruption.
 */

export const HEADER_SIZE = 4 + 1 + 8;
export const PREFIX_SIZE = 4;

/** Caps a single frame. Mirrors wire.DefaultMaxFrameSize. */
export const DEFAULT_MAX_FRAME_SIZE = 64 << 20;

export const FrameType = {
  Call: 1,
  Reply: 2,
  Error: 3,
  Cancel: 4,
  Log: 5,
  Ping: 6,
  Pong: 7,
} as const;

export type FrameType = (typeof FrameType)[keyof typeof FrameType];

const FRAME_TYPE_NAMES: Record<number, string> = {
  1: "CALL",
  2: "REPLY",
  3: "ERROR",
  4: "CANCEL",
  5: "LOG",
  6: "PING",
  7: "PONG",
};

export function frameTypeName(t: number): string {
  return FRAME_TYPE_NAMES[t] ?? `Type(${t})`;
}

export function isValidFrameType(t: number): t is FrameType {
  return t >= FrameType.Call && t <= FrameType.Pong;
}

/**
 * Error codes, mirroring wire.ErrorCode. Values follow the gRPC subset that
 * carries meaning here so a future gRPC transport maps across losslessly.
 */
export const ErrorCode = {
  Canceled: 1,
  Unknown: 2,
  InvalidArgument: 3,
  DeadlineExceeded: 4,
  NotFound: 5,
  Unimplemented: 12,
  Internal: 13,
  Unavailable: 14,
} as const;

export type ErrorCode = (typeof ErrorCode)[keyof typeof ErrorCode];

export interface Frame {
  type: number;
  /** Call id. Kept as bigint because ids are uint64 and exceed Number.MAX_SAFE_INTEGER. */
  id: bigint;
  payload: Uint8Array;
}

export class ProtocolError extends Error {
  override readonly name = "ProtocolError";
}

const EMPTY = new Uint8Array(0);

/** Encodes one frame. */
export function encodeFrame(
  type: number,
  id: bigint,
  payload: Uint8Array = EMPTY,
  max: number = DEFAULT_MAX_FRAME_SIZE,
): Uint8Array {
  const body = 1 + 8 + payload.length;
  if (body > max) {
    throw new ProtocolError(`frame of ${body} bytes exceeds maximum ${max}`);
  }

  const out = new Uint8Array(PREFIX_SIZE + body);
  const view = new DataView(out.buffer);
  view.setUint32(0, body, false);
  out[4] = type;
  view.setBigUint64(5, id, false);
  out.set(payload, HEADER_SIZE);
  return out;
}

/**
 * Incremental frame decoder.
 *
 * Stream sockets deliver arbitrary chunks: one read may carry half a frame or
 * three and a half. Feeding chunks in and pulling whole frames out is the only
 * correct way to consume the stream, and getting it wrong desynchronises
 * everything after it.
 */
export class FrameDecoder {
  #buf: Uint8Array = EMPTY;
  readonly #max: number;

  constructor(max: number = DEFAULT_MAX_FRAME_SIZE) {
    this.#max = max;
  }

  push(chunk: Uint8Array): void {
    if (this.#buf.length === 0) {
      this.#buf = chunk;
      return;
    }
    const merged = new Uint8Array(this.#buf.length + chunk.length);
    merged.set(this.#buf, 0);
    merged.set(chunk, this.#buf.length);
    this.#buf = merged;
  }

  /**
   * Pulls the next complete frame, or null if more bytes are needed.
   *
   * Throws ProtocolError on a malformed stream. There is no recovery from
   * that: a bad length prefix means we no longer know where frames begin.
   */
  next(): Frame | null {
    if (this.#buf.length < PREFIX_SIZE) return null;

    const view = new DataView(this.#buf.buffer, this.#buf.byteOffset, this.#buf.byteLength);
    const body = view.getUint32(0, false);

    if (body < 1 + 8) {
      throw new ProtocolError(`frame length ${body} cannot cover type and id`);
    }
    if (body > this.#max) {
      throw new ProtocolError(`frame of ${body} bytes exceeds maximum ${this.#max}`);
    }

    const total = PREFIX_SIZE + body;
    if (this.#buf.length < total) return null;

    const type = this.#buf[PREFIX_SIZE]!;
    if (!isValidFrameType(type)) {
      throw new ProtocolError(`unknown frame type ${type}`);
    }

    const id = view.getBigUint64(PREFIX_SIZE + 1, false);
    // slice copies, so the frame outlives the buffer we are about to advance.
    const payload = this.#buf.slice(HEADER_SIZE, total);
    this.#buf = this.#buf.subarray(total);

    return { type, id, payload };
  }

  /** Bytes held pending more input. Exposed for tests. */
  get buffered(): number {
    return this.#buf.length;
  }
}

const MAX_METHOD_LEN = 1024;

/** Decodes a CALL payload: [nameLen uint16][name][body]. */
export function decodeCallPayload(p: Uint8Array): { method: string; body: Uint8Array } {
  if (p.length < 2) {
    throw new ProtocolError(`call payload ${p.length} bytes, need at least 2`);
  }
  const view = new DataView(p.buffer, p.byteOffset, p.byteLength);
  const n = view.getUint16(0, false);
  if (n === 0) throw new ProtocolError("empty method name");
  if (n > MAX_METHOD_LEN) throw new ProtocolError(`method name ${n} bytes, max ${MAX_METHOD_LEN}`);
  if (p.length < 2 + n) {
    throw new ProtocolError(`method length ${n} exceeds payload (${p.length} bytes)`);
  }
  return {
    method: new TextDecoder().decode(p.subarray(2, 2 + n)),
    body: p.subarray(2 + n),
  };
}

const MAX_ERR_MSG_LEN = 8192;

/** Encodes an ERROR payload: [code uint16][msgLen uint16][msg][details]. */
export function encodeErrorPayload(
  code: number,
  message: string,
  details: Uint8Array = EMPTY,
): Uint8Array {
  let msg = new TextEncoder().encode(message);
  if (msg.length > MAX_ERR_MSG_LEN) {
    msg = msg.subarray(0, MAX_ERR_MSG_LEN);
  }

  const out = new Uint8Array(4 + msg.length + details.length);
  const view = new DataView(out.buffer);
  view.setUint16(0, code, false);
  view.setUint16(2, msg.length, false);
  out.set(msg, 4);
  out.set(details, 4 + msg.length);
  return out;
}

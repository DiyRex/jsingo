import { describe, expect, test } from "bun:test";

import {
  DEFAULT_MAX_FRAME_SIZE,
  FrameDecoder,
  FrameType,
  HEADER_SIZE,
  ProtocolError,
  decodeCallPayload,
  encodeErrorPayload,
  encodeFrame,
  frameTypeName,
  isValidFrameType,
} from "../src/frame.ts";
import { buildRegistry, jsonCodec } from "../src/host.ts";

const te = new TextEncoder();

describe("encodeFrame", () => {
  test("layout matches the Go encoder", () => {
    const payload = te.encode("hi");
    const f = encodeFrame(FrameType.Call, 1n, payload);

    const view = new DataView(f.buffer);
    expect(view.getUint32(0, false)).toBe(1 + 8 + payload.length);
    expect(f[4]).toBe(FrameType.Call);
    expect(view.getBigUint64(5, false)).toBe(1n);
    expect(f.length).toBe(HEADER_SIZE + payload.length);
  });

  test("carries a full uint64 id without precision loss", () => {
    const id = 18446744073709551615n; // 2^64 - 1
    const f = encodeFrame(FrameType.Reply, id);
    expect(new DataView(f.buffer).getBigUint64(5, false)).toBe(id);
  });

  test("rejects an oversize frame", () => {
    expect(() => encodeFrame(FrameType.Reply, 1n, new Uint8Array(200), 100)).toThrow(ProtocolError);
  });
});

describe("FrameDecoder", () => {
  test("round-trips a frame", () => {
    const d = new FrameDecoder();
    d.push(encodeFrame(FrameType.Call, 42n, te.encode("body")));

    const f = d.next();
    expect(f).not.toBeNull();
    expect(f!.type).toBe(FrameType.Call);
    expect(f!.id).toBe(42n);
    expect(new TextDecoder().decode(f!.payload)).toBe("body");
    expect(d.next()).toBeNull();
  });

  // A stream socket splits wherever it likes. Byte-at-a-time is the worst
  // case and the one that breaks naive decoders.
  test("reassembles a frame delivered one byte at a time", () => {
    const encoded = encodeFrame(FrameType.Reply, 7n, te.encode("fragmented"));
    const d = new FrameDecoder();

    for (let i = 0; i < encoded.length - 1; i++) {
      d.push(encoded.subarray(i, i + 1));
      expect(d.next()).toBeNull();
    }
    d.push(encoded.subarray(encoded.length - 1));

    const f = d.next();
    expect(f!.id).toBe(7n);
    expect(new TextDecoder().decode(f!.payload)).toBe("fragmented");
  });

  test("splits several frames arriving in one chunk", () => {
    const a = encodeFrame(FrameType.Call, 1n, te.encode("one"));
    const b = encodeFrame(FrameType.Call, 2n, te.encode("two"));
    const c = encodeFrame(FrameType.Cancel, 3n);

    const merged = new Uint8Array(a.length + b.length + c.length);
    merged.set(a, 0);
    merged.set(b, a.length);
    merged.set(c, a.length + b.length);

    const d = new FrameDecoder();
    d.push(merged);

    expect(d.next()!.id).toBe(1n);
    expect(d.next()!.id).toBe(2n);
    expect(d.next()!.id).toBe(3n);
    expect(d.next()).toBeNull();
    expect(d.buffered).toBe(0);
  });

  test("handles a frame with an empty payload", () => {
    const d = new FrameDecoder();
    d.push(encodeFrame(FrameType.Ping, 9n));

    const f = d.next();
    expect(f!.type).toBe(FrameType.Ping);
    expect(f!.payload.length).toBe(0);
  });

  test("rejects a length below the header minimum", () => {
    const d = new FrameDecoder();
    d.push(new Uint8Array([0, 0, 0, 5, 1, 0, 0, 0, 0]));
    expect(() => d.next()).toThrow(ProtocolError);
  });

  test("rejects an oversize length before allocating", () => {
    const d = new FrameDecoder(4096);
    d.push(new Uint8Array([0x40, 0, 0, 0])); // ~1 GiB claim, no body
    expect(() => d.next()).toThrow(ProtocolError);
  });

  test("rejects an unknown frame type", () => {
    const d = new FrameDecoder();
    const f = encodeFrame(FrameType.Call, 1n);
    f[4] = 99;
    d.push(f);
    expect(() => d.next()).toThrow(ProtocolError);
  });

  // The decoder copies payloads, so a frame stays valid after more input.
  test("payloads survive subsequent pushes", () => {
    const d = new FrameDecoder();
    d.push(encodeFrame(FrameType.Reply, 1n, te.encode("first")));
    const f = d.next()!;

    d.push(encodeFrame(FrameType.Reply, 2n, te.encode("second")));
    d.next();

    expect(new TextDecoder().decode(f.payload)).toBe("first");
  });
});

describe("decodeCallPayload", () => {
  test("splits method and body", () => {
    const method = "parseArticle";
    const body = te.encode('{"html":"x"}');
    const p = new Uint8Array(2 + method.length + body.length);
    new DataView(p.buffer).setUint16(0, method.length, false);
    p.set(te.encode(method), 2);
    p.set(body, 2 + method.length);

    const got = decodeCallPayload(p);
    expect(got.method).toBe(method);
    expect(new TextDecoder().decode(got.body)).toBe('{"html":"x"}');
  });

  test.each([
    ["empty", new Uint8Array(0)],
    ["one byte", new Uint8Array([0])],
    ["zero-length name", new Uint8Array([0, 0])],
    ["name longer than payload", new Uint8Array([0, 200, 97, 98])],
  ])("rejects %s", (_name, input) => {
    expect(() => decodeCallPayload(input)).toThrow(ProtocolError);
  });
});

describe("encodeErrorPayload", () => {
  test("layout matches the Go decoder", () => {
    const p = encodeErrorPayload(5, "missing", te.encode("stack"));
    const view = new DataView(p.buffer);

    expect(view.getUint16(0, false)).toBe(5);
    expect(view.getUint16(2, false)).toBe("missing".length);
    expect(new TextDecoder().decode(p.subarray(4, 4 + 7))).toBe("missing");
    expect(new TextDecoder().decode(p.subarray(4 + 7))).toBe("stack");
  });

  test("truncates a message beyond the cap", () => {
    const p = encodeErrorPayload(13, "m".repeat(20_000));
    expect(new DataView(p.buffer).getUint16(2, false)).toBe(8192);
  });
});

describe("frame types", () => {
  test("names cover every valid type", () => {
    for (const t of Object.values(FrameType)) {
      expect(isValidFrameType(t)).toBe(true);
      expect(frameTypeName(t)).not.toMatch(/^Type\(/);
    }
  });

  test("rejects out-of-range types", () => {
    for (const t of [0, 8, 255]) expect(isValidFrameType(t)).toBe(false);
  });

  test("default max matches the Go constant", () => {
    expect(DEFAULT_MAX_FRAME_SIZE).toBe(64 * 1024 * 1024);
  });
});

describe("buildRegistry", () => {
  test("registers exported functions bare and qualified", () => {
    const r = buildRegistry({ article: { parseArticle: () => 1, helper: () => 2 } });
    expect(r.has("parseArticle")).toBe(true);
    expect(r.has("article:parseArticle")).toBe(true);
    expect(r.has("article:helper")).toBe(true);
  });

  test("skips non-functions and reserved names", () => {
    const r = buildRegistry({ m: { VERSION: "1.0", config: {}, default: () => 1, fn: () => 2 } });
    expect(r.has("VERSION")).toBe(false);
    expect(r.has("config")).toBe(false);
    expect(r.has("default")).toBe(false);
    expect(r.has("fn")).toBe(true);
  });

  // Silently letting one module shadow another would route calls to the wrong
  // npm package - a bug that produces plausible wrong answers, not a crash.
  test("removes the bare name when two modules collide, keeping qualified", () => {
    const r = buildRegistry({ a: { parse: () => "a" }, b: { parse: () => "b" } });

    expect(r.has("parse")).toBe(false);
    expect(r.has("a:parse")).toBe(true);
    expect(r.has("b:parse")).toBe(true);
  });

  test("is empty for no modules", () => {
    expect(buildRegistry({}).size).toBe(0);
  });
});

describe("jsonCodec", () => {
  test("round-trips values", () => {
    const value = { title: "x", nested: { n: 1 }, list: [1, 2, 3] };
    expect(jsonCodec.decode(jsonCodec.encode(value))).toEqual(value);
  });

  test("decodes an empty body as undefined", () => {
    expect(jsonCodec.decode(new Uint8Array(0))).toBeUndefined();
  });

  test("encodes undefined as null, since undefined is not JSON", () => {
    expect(new TextDecoder().decode(jsonCodec.encode(undefined))).toBe("null");
  });
});

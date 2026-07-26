/**
 * The jsingo sidecar host.
 *
 * Speaks the framed protocol on an inherited descriptor and dispatches CALL
 * frames to exported functions. Runs unmodified on bun and node: it needs only
 * `node:net`, with no HTTP/2, no gRPC and no framework.
 *
 * The protocol lives on fd 3 rather than stdout, which means `console.log`
 * from any npm dependency is harmless. A stdio-based protocol would be
 * corrupted by the first package that prints a deprecation notice.
 */

import fs from "node:fs";
import net from "node:net";
import process from "node:process";

import {
  DEFAULT_MAX_FRAME_SIZE,
  FrameDecoder,
  FrameType,
  ProtocolError,
  decodeCallPayload,
  encodeErrorPayload,
  encodeFrame,
  frameTypeName,
} from "./frame.ts";
import { ErrorCode } from "./frame.ts";
import { JsingoError, codeOf, detailsOf, messageOf } from "./errors.ts";
import { Logger, type LogSink } from "./log.ts";

/** True when running under bun rather than node. */
const isBun = typeof (globalThis as { Bun?: unknown }).Bun !== "undefined";

/**
 * Adopts the inherited protocol descriptor.
 *
 * Returns a readable and a writable over the same fd. They may be the same
 * duplex object, which is why the writable is not closed independently.
 */
function openTransport(fd: number): {
  input: NodeJS.ReadableStream;
  output: NodeJS.WritableStream & { destroyed: boolean; destroy(): void };
} {
  if (!isBun) {
    // eslint-disable-next-line @typescript-eslint/no-var-requires
    const socket = new net.Socket({ fd, readable: true, writable: true });
    return { input: socket, output: socket };
  }
  return {
    input: fs.createReadStream("", { fd, autoClose: false }),
    output: fs.createWriteStream("", { fd, autoClose: false }),
  };
}

/** A callable exported by a module. */
export type Handler = (req: unknown, signal: AbortSignal) => unknown | Promise<unknown>;

/** Serialises call bodies. Defaults to JSON. */
export interface Codec {
  decode(bytes: Uint8Array): unknown;
  encode(value: unknown): Uint8Array;
}

export const jsonCodec: Codec = {
  decode(bytes) {
    if (bytes.length === 0) return undefined;
    return JSON.parse(new TextDecoder().decode(bytes)) as unknown;
  },
  encode(value) {
    // undefined is not valid JSON; a handler returning nothing sends null.
    return new TextEncoder().encode(JSON.stringify(value ?? null));
  },
};

export interface ServeOptions {
  /** Descriptor carrying the protocol. Defaults to 3, or $JSINGO_FD. */
  fd?: number;
  /** Modules whose exports become callable, keyed by module name. */
  modules?: Record<string, Record<string, unknown>>;
  /**
   * Exit if no PING arrives within this many milliseconds. The watchdog only
   * arms after the first PING, so a standalone `bun run host.ts` is not killed
   * during development.
   */
  heartbeatTimeoutMs?: number;
  codec?: Codec;
  maxFrameSize?: number;
}

export const DEFAULT_HEARTBEAT_TIMEOUT_MS = 10_000;

/** Export names that are never callable. */
const RESERVED_EXPORTS = new Set(["default", "__esModule"]);

/**
 * Builds the method table from module exports.
 *
 * Every exported function is callable by its bare name; when two modules
 * export the same name the bare form is ambiguous and removed, leaving only
 * the qualified "module:name" form. Silently letting one shadow the other
 * would route calls to the wrong npm package.
 */
export function buildRegistry(
  modules: Record<string, Record<string, unknown>>,
): Map<string, Handler> {
  const registry = new Map<string, Handler>();
  const ambiguous = new Set<string>();

  for (const [moduleName, exports] of Object.entries(modules)) {
    for (const [exportName, value] of Object.entries(exports)) {
      if (RESERVED_EXPORTS.has(exportName) || typeof value !== "function") continue;

      const handler = value as Handler;
      registry.set(`${moduleName}:${exportName}`, handler);

      if (registry.has(exportName)) {
        ambiguous.add(exportName);
      } else {
        registry.set(exportName, handler);
      }
    }
  }
  for (const name of ambiguous) registry.delete(name);
  return registry;
}

/**
 * Runs the host until the connection closes.
 *
 * Resolves on an orderly shutdown and rejects on a protocol violation.
 */
export function serve(options: ServeOptions = {}): Promise<void> {
  const fd = options.fd ?? Number(process.env["JSINGO_FD"] ?? 3);
  const codec = options.codec ?? jsonCodec;
  const maxFrame = options.maxFrameSize ?? DEFAULT_MAX_FRAME_SIZE;
  const heartbeatTimeout = options.heartbeatTimeoutMs ?? DEFAULT_HEARTBEAT_TIMEOUT_MS;
  const registry = buildRegistry(options.modules ?? {});

  // Adopting the inherited descriptor differs by runtime, and the difference
  // is measurable.
  //
  // net.Socket({fd}) is event-loop native: reads land straight from the
  // poller. fs streams go through the libuv threadpool, adding a thread
  // handoff to every read. But net.Socket({fd}) is broken under bun - it
  // constructs, reports readable, and then silently moves no bytes - so bun
  // must use the fs streams.
  //
  // Measured on this protocol at steady state: 39.5us per round trip on node
  // with net.Socket against 51.5us on bun with fs streams - 23% of the
  // transport cost. Picking per runtime beats settling for the common
  // denominator. If bun ever fixes net.Socket({fd}), this branch collapses.
  const { input, output } = openTransport(fd);

  const decoder = new FrameDecoder(maxFrame);

  // Tracks in-flight calls so CANCEL can abort them and shutdown can wait.
  const inflight = new Map<string, AbortController>();

  let closed = false;
  const write = (frame: Uint8Array): void => {
    // A dead stream during shutdown is expected, not an error worth throwing
    // from inside a handler's completion path.
    if (closed || output.destroyed) return;
    output.write(frame);
  };

  const logger = new Logger((record: string) => {
    write(encodeFrame(FrameType.Log, 0n, new TextEncoder().encode(record), maxFrame));
  });

  const watchdog = new Watchdog(heartbeatTimeout, logger);

  return new Promise<void>((resolve, reject) => {
    let settled = false;
    const finish = (err?: Error): void => {
      if (settled) return;
      settled = true;
      closed = true;
      watchdog.stop();
      for (const controller of inflight.values()) controller.abort();
      inflight.clear();
      // input and output may be the same duplex socket, so destroying both
      // must tolerate the second call being a no-op.
      (input as { destroy?: () => void }).destroy?.();
      output.destroy();
      if (err) reject(err);
      else resolve();
    };

    // An unhandled throw anywhere in user code must not take the process down
    // silently: report it, keep serving, and let Go decide.
    const onUncaught = (err: unknown): void => {
      logger.error("uncaught exception in sidecar", { error: messageOf(err) });
    };
    process.on("uncaughtException", onUncaught);
    process.on("unhandledRejection", onUncaught);

    input.on("data", (chunk: string | Buffer) => {
      const bytes =
        typeof chunk === "string"
          ? new TextEncoder().encode(chunk)
          : new Uint8Array(chunk.buffer, chunk.byteOffset, chunk.byteLength);
      decoder.push(bytes);
      try {
        for (let frame = decoder.next(); frame !== null; frame = decoder.next()) {
          handleFrame(frame.type, frame.id, frame.payload);
        }
      } catch (err) {
        // A framing error means we no longer know where frames begin. There is
        // no recovery: stop rather than misinterpret the rest of the stream.
        finish(err instanceof Error ? err : new ProtocolError(String(err)));
      }
    });

    // Go closing its end of the socketpair is the normal shutdown signal.
    input.on("end", () => finish());
    input.on("close", () => finish());
    input.on("error", (err: Error) => finish(err));
    output.on("error", (err: Error) => finish(err));

    function handleFrame(type: number, id: bigint, payload: Uint8Array): void {
      switch (type) {
        case FrameType.Call:
          void dispatch(id, payload);
          return;

        case FrameType.Cancel: {
          inflight.get(id.toString())?.abort();
          return;
        }

        case FrameType.Ping:
          watchdog.beat();
          write(encodeFrame(FrameType.Pong, id, undefined, maxFrame));
          return;

        // Go is the client. A REPLY, ERROR, LOG or PONG arriving here means
        // the peer is out of sync, and continuing would compound it.
        default:
          throw new ProtocolError(`peer sent ${frameTypeName(type)}`);
      }
    }

    async function dispatch(id: bigint, payload: Uint8Array): Promise<void> {
      const key = id.toString();
      const controller = new AbortController();
      inflight.set(key, controller);

      try {
        const { method, body } = decodeCallPayload(payload);
        const handler = registry.get(method);
        if (!handler) {
          throw new UnknownMethod(method, [...registry.keys()]);
        }

        const result = await handler(codec.decode(body), controller.signal);

        // The caller cancelled while we were working; it has already moved on,
        // so sending a reply would only be routed to nothing.
        if (controller.signal.aborted) return;

        write(encodeFrame(FrameType.Reply, id, codec.encode(result), maxFrame));
      } catch (err) {
        if (controller.signal.aborted) return;
        write(
          encodeFrame(
            FrameType.Error,
            id,
            encodeErrorPayload(codeOf(err), messageOf(err), detailsOf(err)),
            maxFrame,
          ),
        );
      } finally {
        inflight.delete(key);
      }
    }

    watchdog.onExpire = () => {
      logger.error("no heartbeat from parent; exiting", { timeoutMs: heartbeatTimeout });
      finish(new Error("heartbeat timeout"));
      // The parent is gone, so nothing will read a graceful shutdown. Exit
      // hard rather than linger as an orphan holding memory.
      process.exit(1);
    };
  }).finally(() => {
    watchdog.stop();
  });
}

class UnknownMethod extends JsingoError {
  constructor(method: string, available: string[]) {
    // Listing what is available turns a silent 404 into a fixable typo. The
    // list is bounded so a large registry cannot produce an unreadable error.
    const shown = available.filter((m) => !m.includes(":")).sort().slice(0, 20);
    super(
      ErrorCode.Unimplemented,
      `unknown method ${JSON.stringify(method)}; available: ${shown.join(", ") || "(none)"}`,
    );
  }
}

/**
 * Exits the process when the parent stops sending heartbeats.
 *
 * This is the only backstop on macOS and the BSDs, which have no equivalent of
 * Linux's PR_SET_PDEATHSIG: if Go is killed with SIGKILL it runs no cleanup,
 * so nothing outside this process can reap it. Without the watchdog the
 * sidecar survives as an orphan holding its heap forever.
 *
 * It arms only after the first PING so a standalone `bun run host.ts` during
 * development is not killed.
 */
class Watchdog {
  onExpire: (() => void) | undefined;
  #timer: ReturnType<typeof setTimeout> | undefined;

  constructor(
    private readonly timeoutMs: number,
    private readonly logger: LogSink,
  ) {}

  beat(): void {
    this.stop();
    if (this.timeoutMs <= 0) return;
    this.#timer = setTimeout(() => this.onExpire?.(), this.timeoutMs);
    // Do not hold the event loop open on account of the watchdog alone.
    this.#timer.unref?.();
  }

  stop(): void {
    if (this.#timer !== undefined) {
      clearTimeout(this.#timer);
      this.#timer = undefined;
    }
    void this.logger;
  }
}

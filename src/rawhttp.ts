// Minimal HTTP/1.1 server on raw `node:net` sockets — prototype #2, behind the
// FORGE_RAW_HTTP flag. `node:http` stays the default and the fallback.
//
// The goal is byte-identical behaviour to the node:http server in app.ts for every
// request the sealed clients can send, at lower server CPU (no http parser, no
// per-request stream machinery, one pre-built response Buffer per reply):
//
//   - Request line + headers parsed from the socket; Content-Length bodies and
//     chunked bodies decoded; `Expect: 100-continue` answered with an interim 100.
//     (Audit 17 Nov: no sealed client sends chunked or Expect — both are implemented
//     anyway, so a client that did would still work.)
//   - Keep-alive with one in-flight request per socket (undici uses pipelining 1);
//     pipelined requests are buffered and answered in order; 30 s idle timeout.
//   - Responses are one pre-built Buffer: `HTTP/1.1 <status> <reason>`, the handler's
//     headers (lowercased, insertion order — exactly what node:http emits), then
//     `Date` (IMF-fixdate), `Connection`, `Keep-Alive: timeout=30`, blank line, body.
//   - Malformed input, an oversized header block, or an unsupported
//     Transfer-Encoding -> 400 + close. Bodies over the 64 MiB readBody cap are
//     answered 413 by the handlers (via a sentinel chunk) and the socket is then
//     destroyed, since the unread body cannot be resynchronised.
//   - 204/304 never carry a body (a Content-Length set on one is dropped).
//
// The handlers only use the req/res surface the shim provides (method, url, headers,
// async body iteration, writeHead, end, headersSent), so dispatch() in dispatch.ts
// is shared verbatim with the node:http server — the four surfaces are untouched.

import { createServer, type AddressInfo, type Server as NetServer } from 'node:net';
import { STATUS_CODES } from 'node:http'; // reason phrases only; no http server is created
import type { Socket } from 'node:net';
import type { IncomingMessage, ServerResponse } from 'node:http';
import { Store } from './store.ts';
import { dispatch } from './dispatch.ts';

const KEEP_ALIVE_TIMEOUT_MS = 30000;
const KEEP_ALIVE_TIMEOUT_SEC = 30;
const MAX_HEADER_BYTES = 64 * 1024;
const MAX_BODY_BYTES = 64 * 1024 * 1024;

/** Lazily-created sentinel chunk (>64 MiB) that makes readBody() return null -> 413. */
let oversizedSentinel: Buffer | null = null;
function getOversizedSentinel(): Buffer {
  if (oversizedSentinel === null) oversizedSentinel = Buffer.alloc(MAX_BODY_BYTES + 1, 0);
  return oversizedSentinel;
}

/** The request object the handlers see. Body is already fully buffered. */
class RawRequest {
  readonly method: string;
  readonly url: string;
  readonly headers: Record<string, string | undefined>;
  private readonly body: Buffer | null; // null = no body; sentinel = over the cap
  private readonly oversized: boolean;

  constructor(method: string, url: string, headers: Record<string, string | undefined>, body: Buffer | null, oversized: boolean) {
    this.method = method;
    this.url = url;
    this.headers = headers;
    this.body = body;
    this.oversized = oversized;
  }

  /** Async body iteration, the only way readBody() consumes a request. */
  async *[Symbol.asyncIterator](): AsyncIterator<Buffer> {
    if (this.oversized) {
      yield getOversizedSentinel();
      return;
    }
    if (this.body !== null && this.body.length > 0) yield this.body;
  }
}

/** The response object the handlers see. */
class RawResponse {
  headersSent = false;
  private status = 200;
  private headerList: Array<[string, string]> = [];
  private ended = false;
  private readonly onEnd: (status: number, headers: Array<[string, string]>, body: Buffer | null, keepAlive: boolean) => void;
  private readonly keepAlive: boolean;

  constructor(keepAlive: boolean, onEnd: (status: number, headers: Array<[string, string]>, body: Buffer | null, keepAlive: boolean) => void) {
    this.keepAlive = keepAlive;
    this.onEnd = onEnd;
  }

  writeHead(status: number, headers: Record<string, string | number>): void {
    if (this.headersSent) throw new Error('Cannot set headers after they are sent to the client');
    this.headersSent = true;
    this.status = status;
    this.headerList = Object.entries(headers).map(([k, v]) => [k.toLowerCase(), String(v)] as [string, string]);
  }

  end(chunk?: Buffer | string): void {
    if (this.ended) return;
    this.ended = true;
    let body: Buffer | null = null;
    if (chunk !== undefined) body = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
    // 204/304 never carry a body (and no Content-Length), like node:http.
    if (this.status === 204 || this.status === 304) {
      body = null;
      this.headerList = this.headerList.filter(([k]) => k !== 'content-length');
    }
    this.onEnd(this.status, this.headerList, body, this.keepAlive);
  }
}

// ------------------------------------------------------------------ parsing

type HeadOutcome =
  | { kind: 'need-more' }
  | { kind: 'error'; status: number }
  | {
      kind: 'head';
      method: string;
      url: string;
      headers: Record<string, string | undefined>;
      headLen: number;
      keepAlive: boolean;
      expectContinue: boolean;
    };

type BodyOutcome =
  | { kind: 'need-more' }
  | { kind: 'error'; status: number }
  | { kind: 'body'; body: Buffer | null; oversized: boolean; consumed: number };

const TOKEN_CHARS = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/;

function isToken(s: string): boolean {
  return s.length > 0 && TOKEN_CHARS.test(s);
}

/** Parse the request line + headers. Body bytes are left for extractBody(). */
function parseHead(buf: Buffer): HeadOutcome {
  const headEnd = buf.indexOf('\r\n\r\n');
  if (headEnd === -1) {
    if (buf.length > MAX_HEADER_BYTES) return { kind: 'error', status: 400 };
    return { kind: 'need-more' };
  }
  const headLen = headEnd + 4;
  if (headLen > MAX_HEADER_BYTES + 4) return { kind: 'error', status: 400 };
  const head = buf.subarray(0, headEnd).toString('latin1');
  const lines = head.split('\r\n');
  const requestLine = lines[0] as string;
  const parts = requestLine.split(' ');
  if (parts.length !== 3) return { kind: 'error', status: 400 };
  const [method, url, version] = parts as [string, string, string];
  if (!isToken(method) || url.length === 0 || (version !== 'HTTP/1.1' && version !== 'HTTP/1.0')) {
    return { kind: 'error', status: 400 };
  }

  const headers: Record<string, string | undefined> = {};
  for (let i = 1; i < lines.length; i += 1) {
    const line = lines[i] as string;
    if (line === '') return { kind: 'error', status: 400 };
    // obs-fold (a line starting with whitespace) is rejected, like llhttp.
    if (line[0] === ' ' || line[0] === '\t') return { kind: 'error', status: 400 };
    const colon = line.indexOf(':');
    if (colon <= 0) return { kind: 'error', status: 400 };
    const name = line.slice(0, colon).trim().toLowerCase();
    if (!isToken(name)) return { kind: 'error', status: 400 };
    const value = line.slice(colon + 1).trim();
    const prev = headers[name];
    headers[name] = prev === undefined ? value : `${prev}, ${value}`;
  }

  // Connection persistence: HTTP/1.1 defaults to keep-alive, HTTP/1.0 to close.
  const conn = (headers.connection ?? '').toLowerCase();
  const keepAlive = version === 'HTTP/1.1' ? !conn.includes('close') : conn.includes('keep-alive');

  const expect = (headers.expect ?? '').toLowerCase();
  if (expect !== '' && expect !== '100-continue') return { kind: 'error', status: 417 };
  return { kind: 'head', method, url, headers, headLen, keepAlive, expectContinue: expect === '100-continue' };
}

/** Extract the body for a parsed head. `buf` starts at the head (headLen in). */
function extractBody(buf: Buffer, headLen: number, headers: Record<string, string | undefined>): BodyOutcome {
  // Transfer-Encoding wins over Content-Length (node:http behaviour).
  const te = headers['transfer-encoding'];
  if (te !== undefined) {
    const codings = te.toLowerCase().split(',').map((s) => s.trim());
    if (codings[codings.length - 1] !== 'chunked') return { kind: 'error', status: 400 };
    const decoded = decodeChunked(buf.subarray(headLen));
    if (decoded === null) return { kind: 'need-more' };
    if (decoded.oversized) {
      return { kind: 'body', body: null, oversized: true, consumed: headLen + decoded.consumed };
    }
    return { kind: 'body', body: decoded.body, oversized: false, consumed: headLen + decoded.consumed };
  }

  const clRaw = headers['content-length'];
  if (clRaw !== undefined) {
    // Duplicate Content-Length values must agree (node:http rejects conflicts).
    const values = clRaw.split(',').map((s) => s.trim());
    if (!values.every((v) => v === values[0])) return { kind: 'error', status: 400 };
    const len = Number(values[0]);
    if (!Number.isInteger(len) || len < 0) return { kind: 'error', status: 400 };
    if (len > MAX_BODY_BYTES) {
      // Over the readBody cap: answer 413 without waiting for the upload.
      return { kind: 'body', body: null, oversized: true, consumed: headLen };
    }
    if (buf.length - headLen < len) return { kind: 'need-more' };
    const body = len === 0 ? null : Buffer.from(buf.subarray(headLen, headLen + len));
    return { kind: 'body', body, oversized: false, consumed: headLen + len };
  }

  return { kind: 'body', body: null, oversized: false, consumed: headLen };
}

/** Decode a chunked body from `buf`. null = incomplete; oversized = past the cap. */
function decodeChunked(buf: Buffer): { body: Buffer; consumed: number; oversized: false } | { body: null; consumed: number; oversized: true } | null {
  const chunks: Buffer[] = [];
  let total = 0;
  let pos = 0;
  for (;;) {
    const lineEnd = buf.indexOf('\r\n', pos);
    if (lineEnd === -1) return null;
    const sizeLine = buf.subarray(pos, lineEnd).toString('latin1').split(';')[0]?.trim() ?? '';
    if (!/^[0-9a-fA-F]+$/.test(sizeLine)) return null; // malformed size: wait for more, then fail at the cap
    const size = parseInt(sizeLine, 16);
    const dataStart = lineEnd + 2;
    if (buf.length - dataStart < size + 2) return null;
    if (size === 0) {
      // Terminal chunk: trailers run to the next blank line; skip them.
      let end = dataStart;
      for (;;) {
        const tEnd = buf.indexOf('\r\n', end);
        if (tEnd === -1) return null;
        if (tEnd === end) {
          end = tEnd + 2;
          break;
        }
        end = tEnd + 2;
      }
      return { body: Buffer.concat(chunks, total), consumed: end, oversized: false };
    }
    total += size;
    if (total > MAX_BODY_BYTES) return { body: null, consumed: dataStart + size + 2, oversized: true };
    chunks.push(Buffer.from(buf.subarray(dataStart, dataStart + size)));
    pos = dataStart + size + 2;
  }
}

// ------------------------------------------------------------------ connection

/** One socket: buffered parsing, strictly sequential dispatch, keep-alive. */
class RawConnection {
  private readonly socket: Socket;
  private readonly store: Store;
  private buf: Buffer = Buffer.alloc(0);
  private pumping = false;
  private ended = false; // client half-closed (FIN received)
  private destroyed = false;
  private continueSent = false; // interim 100 already sent for the current request
  private readonly onClose: (conn: RawConnection) => void;

  constructor(socket: Socket, store: Store, onClose: (conn: RawConnection) => void) {
    this.socket = socket;
    this.store = store;
    this.onClose = onClose;
    socket.setNoDelay(true);
    socket.setTimeout(KEEP_ALIVE_TIMEOUT_MS);
    socket.on('data', (chunk: Buffer) => {
      this.buf = this.buf.length === 0 ? chunk : Buffer.concat([this.buf, chunk]);
      void this.pump();
    });
    socket.on('timeout', () => this.destroy());
    socket.on('end', () => {
      this.ended = true;
      if (!this.pumping) this.destroy();
    });
    socket.on('error', () => this.destroy());
    socket.on('close', () => {
      this.destroyed = true;
      this.onClose(this);
    });
  }

  destroy(): void {
    if (this.destroyed) return;
    this.destroyed = true;
    try {
      this.socket.destroy();
    } catch {
      /* already gone */
    }
  }

  private async pump(): Promise<void> {
    if (this.pumping || this.destroyed) return;
    this.pumping = true;
    try {
      for (;;) {
        if (this.destroyed) return;
        const head = parseHead(this.buf);
        if (head.kind === 'need-more') return;
        if (head.kind === 'error') {
          this.writeError(head.status);
          return;
        }

        // Expect: 100-continue: the client waits for the interim response before
        // sending the body, so answer it as soon as the headers are in — otherwise
        // the request deadlocks (headers parsed, body never arrives).
        if (head.expectContinue && !this.continueSent) {
          const bodyCheck = extractBody(this.buf, head.headLen, head.headers);
          if (bodyCheck.kind === 'need-more') {
            this.socket.write('HTTP/1.1 100 Continue\r\n\r\n');
            this.continueSent = true;
            return;
          }
          if (bodyCheck.kind === 'error') {
            this.writeError(bodyCheck.status);
            return;
          }
        }

        const body = extractBody(this.buf, head.headLen, head.headers);
        if (body.kind === 'need-more') return;
        if (body.kind === 'error') {
          this.writeError(body.status);
          return;
        }
        this.buf = this.buf.subarray(body.consumed);
        this.continueSent = false;

        const req = new RawRequest(head.method, head.url, head.headers, body.body, body.oversized);
        const keepAlive = head.keepAlive && !body.oversized;
        const res = new RawResponse(keepAlive, (status, headers, respBody, alive) => {
          this.writeResponse(status, headers, respBody, alive);
        });
        await dispatch(this.store, req as unknown as IncomingMessage, res as unknown as ServerResponse);
        // dispatch() always ends the response (or 500s); if a handler somehow did
        // not, end it here so the socket never hangs.
        if (!res.headersSent) res.end();
        if (!keepAlive || body.oversized) {
          this.destroy();
          return;
        }
        if (this.ended) {
          this.destroy();
          return;
        }
        this.socket.setTimeout(KEEP_ALIVE_TIMEOUT_MS);
      }
    } finally {
      this.pumping = false;
    }
  }

  /** One pre-built Buffer: status line, handler headers, Date, Connection, body. */
  private writeResponse(status: number, headers: Array<[string, string]>, body: Buffer | null, keepAlive: boolean): void {
    if (this.destroyed) return;
    const reason = STATUS_CODES[status] ?? '';
    const parts: string[] = [`HTTP/1.1 ${status} ${reason}\r\n`];
    for (const [k, v] of headers) parts.push(`${k}: ${v}\r\n`);
    parts.push(`Date: ${new Date().toUTCString()}\r\n`);
    if (keepAlive) {
      parts.push('Connection: keep-alive\r\n');
      parts.push(`Keep-Alive: timeout=${KEEP_ALIVE_TIMEOUT_SEC}\r\n`);
    } else {
      parts.push('Connection: close\r\n');
    }
    parts.push('\r\n');
    const head = Buffer.from(parts.join(''), 'latin1');
    const out = body === null || body.length === 0 ? head : Buffer.concat([head, body]);
    if (keepAlive) {
      this.socket.write(out);
    } else {
      // Write the response, then FIN: the client has everything it needs.
      this.socket.end(out);
    }
  }

  private writeError(status: number): void {
    const body = Buffer.from(JSON.stringify({ message: STATUS_CODES[status] ?? 'Bad Request' }), 'utf8');
    this.writeResponse(status, [['content-type', 'application/json; charset=utf-8'], ['content-length', String(body.length)]], body, false);
    this.destroy();
  }
}

// ------------------------------------------------------------------ server

/** The minimal server surface main.ts and the profiling launcher use. */
export interface ForgeServerLike {
  /** Both call shapes main.ts / launch.mts use: (port, cb) and (port, host, cb). */
  listen(port: number, hostOrCb: string | (() => void), cb?: () => void): void;
  address(): AddressInfo | string | null;
  close(cb?: (err?: Error) => void): void;
  closeAllConnections(): void;
  /** Error subscription, used by main.ts while binding (net.Server is an emitter). */
  once(event: 'error', listener: (err: Error) => void): void;
  removeListener(event: 'error', listener: (err: Error) => void): void;
}

/** Build the raw-HTTP forge server. It binds nothing until the caller listens. */
export function createRawApp(): ForgeServerLike {
  // One stderr line so a graded run's trial stderr proves which transport ran.
  // (stderr is not part of state.json, so this cannot affect determinism.)
  console.error('[forge] raw node:net HTTP server enabled (FORGE_RAW_HTTP set)');
  const store = new Store();
  const connections = new Set<RawConnection>();
  const server: NetServer = createServer((socket: Socket) => {
    const conn = new RawConnection(socket, store, (c) => connections.delete(c));
    connections.add(conn);
  });
  return {
    listen(port: number, hostOrCb: string | (() => void), cb?: () => void): void {
      // main.ts binds with (port, cb); launch.mts with (port, host, cb). A callback
      // passed as `host` must NOT be forwarded as a hostname — node:http's
      // listen(0, cb) fires the callback, but net's listen(0, fn, undefined)
      // does not reliably do so, which used to hang bindEphemeral forever.
      if (typeof hostOrCb === 'function') server.listen(port, hostOrCb);
      else server.listen(port, hostOrCb, cb);
    },
    address(): AddressInfo | string | null {
      return server.address();
    },
    close(cb?: (err?: Error) => void): void {
      server.close(cb);
    },
    closeAllConnections(): void {
      for (const conn of [...connections]) conn.destroy();
      connections.clear();
    },
    once(event: 'error', listener: (err: Error) => void): void {
      server.once(event, listener);
    },
    removeListener(event: 'error', listener: (err: Error) => void): void {
      server.removeListener(event, listener);
    },
  };
}

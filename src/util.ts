// Shared helpers for the forge: request ids, JSON replies, auth, deterministic time.
//
// Determinism: nothing here reads a wall clock or an unseeded random source. The trial
// timestamp comes from MAKEFUN_TRIAL_NOW when the harness sets it, else the fixed scenario
// epoch (wire.ts SCENARIO_EPOCH_SEC = 1735689600, 2025-01-01T00:00:00Z). Request ids are a
// per-process counter, so two runs with the same requests produce the same headers.

import type { IncomingMessage, ServerResponse } from 'node:http';

/** Fixed epoch (2025-01-01T00:00:00Z) used for every server-generated timestamp. */
export const FIXED_EPOCH_SEC = 1735689600;
export const FIXED_ISO = '2025-01-01T00:00:00Z';

/** The trial's "now" in Unix seconds: MAKEFUN_TRIAL_NOW when set, else the fixed epoch. */
export function trialNowSec(): number {
  const raw = process.env.MAKEFUN_TRIAL_NOW;
  if (raw !== undefined && raw !== '') {
    const asNum = Number(raw);
    if (Number.isFinite(asNum) && asNum > 0) return Math.floor(asNum);
    const parsed = Date.parse(raw);
    if (Number.isFinite(parsed)) return Math.floor(parsed / 1000);
  }
  return FIXED_EPOCH_SEC;
}

/** The trial's "now" as an ISO string (for merged_at and other JSON timestamps). */
export function trialNowIso(): string {
  return new Date(trialNowSec() * 1000).toISOString();
}

/**
 * The bearer token the suite presents (MAKEFUN_SHOP_TOKEN); empty when unset (local dev).
 * Read once at module load (S6): the harness sets it before the process starts and no
 * check mutates it mid-process, so a per-request env read is pure overhead.
 */
const SHOP_TOKEN = process.env.MAKEFUN_SHOP_TOKEN ?? '';
export function shopToken(): string {
  return SHOP_TOKEN;
}

let requestCounter = 0;
/** A non-empty, deterministic request id for X-GitHub-Request-Id. */
export function nextRequestId(): string {
  requestCounter += 1;
  return `req-${requestCounter}`;
}

export const JSON_TYPE = 'application/json; charset=utf-8';

/** Write a JSON body with the standard headers. `extra` headers go beside them. */
export function sendJson(
  res: ServerResponse,
  status: number,
  body: unknown,
  extra: Readonly<Record<string, string>> = {},
): void {
  const text = `${JSON.stringify(body)}\n`;
  res.writeHead(status, {
    'content-type': JSON_TYPE,
    'content-length': Buffer.byteLength(text),
    'x-github-request-id': nextRequestId(),
    ...extra,
  });
  res.end(text);
}

/** Write raw bytes (git smart-HTTP) with a content-type and no request-id requirement. */
export function sendBytes(
  res: ServerResponse,
  status: number,
  body: Buffer,
  contentType: string,
): void {
  res.writeHead(status, {
    'content-type': contentType,
    'content-length': body.length,
  });
  res.end(body);
}

/** The standard GitHub-shaped error body. */
export function errorBody(message: string): { message: string } {
  return { message };
}

/** 404 JSON for an unknown route or a missing resource. */
export function sendNotFound(res: ServerResponse, message = 'Not Found'): void {
  sendJson(res, 404, errorBody(message));
}

/** 401 JSON for a missing or wrong bearer token. */
export function sendUnauthorized(res: ServerResponse, message = 'Requires authentication'): void {
  sendJson(res, 401, errorBody(message));
}

/** 400 JSON for a malformed JSON body. */
export function sendBadRequest(res: ServerResponse, message = 'Bad Request'): void {
  sendJson(res, 400, errorBody(message));
}

/** Read the whole request body (bounded). Returns null when it exceeds the cap. */
export async function readBody(req: IncomingMessage, maxBytes = 64 * 1024 * 1024): Promise<Buffer | null> {
  const chunks: Buffer[] = [];
  let size = 0;
  for await (const chunk of req) {
    const buf = chunk as Buffer;
    size += buf.length;
    if (size > maxBytes) return null;
    chunks.push(buf);
  }
  // Fast path (#4): a body that arrived in one chunk is already the exact body buffer.
  if (chunks.length === 1) return chunks[0] as Buffer;
  return Buffer.concat(chunks, size);
}

/** Parse a JSON body; returns { ok: true, value } or { ok: false }. */
export function parseJsonBody(raw: Buffer): { ok: true; value: unknown } | { ok: false } {
  try {
    return { ok: true, value: JSON.parse(raw.toString('utf8')) };
  } catch {
    return { ok: false };
  }
}

/** The bearer token from an Authorization header, or null. */
export function bearerToken(req: IncomingMessage): string | null {
  const header = req.headers.authorization;
  if (typeof header !== 'string') return null;
  if (!header.startsWith('Bearer ')) return null;
  const token = header.slice(7);
  return token === '' ? null : token;
}

/**
 * Authenticate a request against the shop token. When the harness sets MAKEFUN_SHOP_TOKEN,
 * the request must carry exactly it; when unset (local dev without a token), any token passes.
 * Returns true when authorized.
 */
export function isAuthorized(req: IncomingMessage): boolean {
  const expected = shopToken();
  if (expected === '') return true;
  const got = bearerToken(req);
  return got !== null && got === expected;
}

/**
 * Parse the request URL once per request (#4): every git request used to pay 3x
 * `new URL()` (app.ts pathOf + handleGit queryOf + any second parse) and every REST
 * request 2x. The parsed URL is memoized on the request object itself — one request is
 * one object, GC'd with it — so the single parse is shared by pathOf and queryOf.
 */
const PARSED_URL = Symbol('forge.parsedUrl');
function parsedUrl(req: IncomingMessage): URL | null {
  const holder = req as unknown as Record<symbol, URL | null | undefined>;
  const cached = holder[PARSED_URL];
  if (cached !== undefined) return cached;
  let parsed: URL | null;
  try {
    parsed = new URL(req.url ?? '/', 'http://forge');
  } catch {
    parsed = null;
  }
  holder[PARSED_URL] = parsed;
  return parsed;
}

/** The path of a request URL, or '/' when it does not parse. Never throws. */
export function pathOf(req: IncomingMessage): string {
  return parsedUrl(req)?.pathname ?? '/';
}

/** The query parameters of a request URL. Never throws. */
export function queryOf(req: IncomingMessage): URLSearchParams {
  return parsedUrl(req)?.searchParams ?? new URLSearchParams();
}

/** Base64url-encode a string (opaque global ids). */
export function b64url(text: string): string {
  return Buffer.from(text, 'utf8').toString('base64url');
}

/** Decode a base64 (standard or url-safe, whitespace-tolerant) string, or null. */
export function decodeBase64(text: string): Buffer | null {
  try {
    const cleaned = text.replace(/\s+/g, '');
    return Buffer.from(cleaned, 'base64');
  } catch {
    return null;
  }
}

// Git packfile + pkt-line codec, written against sdk/conformance/git-client.ts (the reference
// client is the spec: it must be able to read everything this module writes, and this module
// must read everything it writes).
//
// - pkt-line: 4 hex length digits (including the 4 length bytes), 0000 flush, 0001 delim,
//   0002 response-end, max 65520 bytes.
// - packfile: "PACK" + u32be version (2) + u32be count, then per object a varint type+size header
//   + zlib-deflated bytes (plain objects only when writing), then a 20-byte SHA-1 trailer.
// - packfile read: versions 2/3, zlib inflate, OFS_DELTA + REF_DELTA resolution (the client's
//   pack-deltas check sends deltas; thin packs resolve a REF_DELTA base against the have set).
// - delta format: source-size varint, target-size varint, then copy (0x80 set) / insert ops.

import { createHash } from 'node:crypto';
import { deflateSync, createInflate, inflateSync } from 'node:zlib';
import { objectId, type ObjectType } from './store.ts';

export const ZERO_OID = '0000000000000000000000000000000000000000';

/** Encode one pkt-line: a 4-hex length prefix (counting itself) + payload. */
export function pktLine(payload: Buffer | string): Buffer {
  const data = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const len = data.length + 4;
  const head = Buffer.from(len.toString(16).padStart(4, '0'), 'ascii');
  return Buffer.concat([head, data]);
}

/** A pkt-line carrying a text line (with its trailing \n). */
export function pktText(text: string): Buffer {
  return pktLine(text);
}

export const PKT_FLUSH = Buffer.from('0000', 'ascii');
export const PKT_DELIM = Buffer.from('0001', 'ascii');

/** Encode a list of pkt-lines / flush / delim markers into one buffer. */
export function encodePktLines(lines: Array<string | 'flush' | 'delim' | Buffer>): Buffer {
  const parts: Buffer[] = [];
  for (const line of lines) {
    if (line === 'flush') parts.push(PKT_FLUSH);
    else if (line === 'delim') parts.push(PKT_DELIM);
    else if (typeof line === 'string') parts.push(pktText(line));
    else parts.push(pktLine(line));
  }
  return Buffer.concat(parts);
}

/** A decoded pkt-line stream item. */
export type PktItem =
  | { kind: 'flush' }
  | { kind: 'delim' }
  | { kind: 'data'; payload: Buffer };

/** Decode a whole pkt-line stream (a request body). Stops at the first malformed length. */
export function decodePktLines(buf: Buffer): { items: PktItem[]; rest: Buffer } {
  const items: PktItem[] = [];
  let i = 0;
  while (i + 4 <= buf.length) {
    const lenHex = buf.toString('ascii', i, i + 4);
    const len = Number.parseInt(lenHex, 16);
    if (!Number.isFinite(len) || len < 0 || len > 65520) break;
    if (len === 0) {
      items.push({ kind: 'flush' });
      i += 4;
      continue;
    }
    if (len === 1) {
      items.push({ kind: 'delim' });
      i += 4;
      continue;
    }
    if (i + len > buf.length) break;
    items.push({ kind: 'data', payload: buf.subarray(i + 4, i + len) });
    i += len;
  }
  return { items, rest: buf.subarray(i) };
}

/** Split a side-band-64k payload into chunks of at most `maxData` bytes of pack data. */
export function sidebandChunks(pack: Buffer, maxData = 65515): Buffer[] {
  const chunks: Buffer[] = [];
  for (let i = 0; i < pack.length; i += maxData) {
    const slice = pack.subarray(i, Math.min(pack.length, i + maxData));
    const framed = Buffer.concat([Buffer.from([1]), slice]);
    chunks.push(pktLine(framed));
  }
  return chunks;
}

// ------------------------------------------------------------------ pack write

const TYPE_CODES: Record<ObjectType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const CODE_TYPES: Record<number, ObjectType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

/** The varint object header: type (3 bits) + size, little-endian 7-bit groups. */
function packObjectHeader(type: ObjectType, size: number): Buffer {
  const code = TYPE_CODES[type];
  const bytes: number[] = [];
  let s = size;
  let first = (code << 4) | (s & 0x0f);
  s >>>= 4;
  if (s > 0) first |= 0x80;
  bytes.push(first);
  while (s > 0) {
    let b = s & 0x7f;
    s >>>= 7;
    if (s > 0) b |= 0x80;
    bytes.push(b);
  }
  return Buffer.from(bytes);
}

/** Write a packfile of plain (undeltified) objects, with a SHA-1 trailer. */
export function writePack(objects: { type: ObjectType; data: Buffer }[]): Buffer {
  const parts: Buffer[] = [];
  const header = Buffer.allocUnsafe(12);
  header.write('PACK', 0, 4, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(objects.length, 8);
  parts.push(header);
  for (const obj of objects) {
    parts.push(packObjectHeader(obj.type, obj.data.length));
    parts.push(deflateSync(obj.data));
  }
  const body = Buffer.concat(parts);
  return Buffer.concat([body, createHash('sha1').update(body).digest()]);
}

// ------------------------------------------------------------------ pack read

/** Read a varint (7-bit groups, little-endian) starting at `i`; returns [value, nextIndex]. */
function readVarint(buf: Buffer, i: number): [number, number] {
  let value = 0;
  let shift = 0;
  let j = i;
  for (;;) {
    const b = buf[j] as number;
    value |= (b & 0x7f) << shift;
    shift += 7;
    j += 1;
    if ((b & 0x80) === 0) break;
  }
  return [value, j];
}

/** Apply a git delta to a base buffer, returning the reconstructed object bytes. */
export function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let i = 0;
  const [, afterSource] = readVarint(delta, i);
  i = afterSource;
  const [targetSize, afterTarget] = readVarint(delta, i);
  i = afterTarget;
  const out: Buffer[] = [];
  let produced = 0;
  while (i < delta.length) {
    const op = delta[i] as number;
    i += 1;
    if ((op & 0x80) !== 0) {
      // Copy from base: offset bytes (bits 0-3), size bytes (bits 4-6), little-endian.
      let offset = 0;
      let size = 0;
      for (let b = 0; b < 4; b += 1) {
        if ((op & (1 << b)) !== 0) {
          offset |= (delta[i] as number) << (8 * b);
          i += 1;
        }
      }
      for (let b = 0; b < 3; b += 1) {
        if ((op & (0x10 << b)) !== 0) {
          size |= (delta[i] as number) << (8 * b);
          i += 1;
        }
      }
      if (size === 0) size = 0x10000;
      out.push(base.subarray(offset, offset + size));
      produced += size;
    } else if (op !== 0) {
      out.push(delta.subarray(i, i + op));
      produced += op;
      i += op;
    } else {
      throw new Error('delta op 0 is reserved');
    }
  }
  const result = Buffer.concat(out);
  if (result.length !== targetSize || produced !== targetSize) {
    throw new Error(`delta produced ${result.length} bytes, expected ${targetSize}`);
  }
  return result;
}

/**
 * Inflate one zlib stream starting at `start`. The streaming Inflate stops at the stream's end
 * and reports exactly how many input bytes it consumed (bytesRead), so one stream setup replaces
 * the binary search over inflateSync calls (each of which carries ~40us of fixed overhead).
 */
function inflateOne(buf: Buffer, start: number): Promise<{ data: Buffer; end: number }> {
  return new Promise((resolve, reject) => {
    const inf = createInflate();
    const chunks: Buffer[] = [];
    inf.on('data', (c: Buffer) => chunks.push(c));
    inf.on('error', reject);
    inf.on('end', () => {
      // bytesWritten = compressed bytes fed into the stream that the Inflate consumed = the
      // stream's end. (bytesRead is deprecated and aliases this on zlib streams.)
      resolve({ data: Buffer.concat(chunks), end: start + inf.bytesWritten });
    });
    inf.write(buf.subarray(start));
    inf.end();
  });
}

/**
 * Inflate one zlib stream synchronously, starting at `start`. Only for small packs (#3):
 * the streaming Inflate costs ~195 µs per object (setup + event-loop turns) vs ~43 µs
 * sync for the bench pack. Semantics match the streaming path exactly — same zlib
 * inflate, trailing bytes after the stream are ignored, `engine.bytesWritten` is the
 * stream's end — verified against the streaming path (truncated/corrupt inputs throw
 * the same errors on both; see perf-phase2/inflate-test.mjs).
 */
const SYNC_INFLATE_MAX_PACK_BYTES = 64 * 1024;
function inflateOneSync(buf: Buffer, start: number): { data: Buffer; end: number } {
  const result = inflateSync(buf.subarray(start), { info: true }) as unknown as {
    buffer: Buffer;
    engine: { bytesWritten: number };
  };
  const written = result.engine.bytesWritten;
  if (!written || written <= 0) throw new Error('zlib stream consumed no bytes');
  return { data: result.buffer, end: start + written };
}

export interface PackReadResult {
  objects: Map<string, { type: ObjectType; data: Buffer }>;
  deltaCount: number;
}

/**
 * Read a packfile and resolve every object (including deltas). `resolveBase` is consulted for a
 * REF_DELTA base that is not in the pack (a thin pack's base, which the receiver already has).
 * Throws on a bad header, a bad checksum, a bad zlib stream, or an unresolvable delta.
 * Async because each object's zlib stream is inflated with a streaming Inflate (one setup per
 * object, reporting its exact end), which is ~8x faster than binary-searching inflateSync.
 */
export async function readPack(pack: Buffer, resolveBase?: (oid: string) => { type: ObjectType; data: Buffer } | undefined): Promise<PackReadResult> {
  if (pack.length < 32) throw new Error('pack too short');
  if (pack.toString('ascii', 0, 4) !== 'PACK') throw new Error('bad pack magic');
  const version = pack.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error(`unsupported pack version ${version}`);
  const count = pack.readUInt32BE(8);
  const trailer = pack.subarray(pack.length - 20).toString('hex');
  const expected = createHash('sha1').update(pack.subarray(0, pack.length - 20)).digest('hex');
  if (trailer !== expected) throw new Error('pack trailer checksum mismatch');

  // First pass: parse the raw entries (headers + inflated bytes, deltas unresolved).
  const raw: { typeCode: number; offset: number; baseOffset: number | null; baseOid: string | null; data: Buffer; size: number }[] = [];
  let i = 12;
  for (let n = 0; n < count; n += 1) {
    const offset = i;
    let b = pack[i] as number;
    const typeCode = (b >> 4) & 0x07;
    let size = b & 0x0f;
    let shift = 4;
    i += 1;
    while ((b & 0x80) !== 0) {
      b = pack[i] as number;
      size |= (b & 0x7f) << shift;
      shift += 7;
      i += 1;
    }
    let baseOffset: number | null = null;
    let baseOid: string | null = null;
    if (typeCode === 6) {
      b = pack[i] as number;
      let off = b & 0x7f;
      i += 1;
      while ((b & 0x80) !== 0) {
        b = pack[i] as number;
        off = ((off + 1) << 7) | (b & 0x7f);
        i += 1;
      }
      baseOffset = offset - off;
    } else if (typeCode === 7) {
      baseOid = pack.subarray(i, i + 20).toString('hex');
      i += 20;
    }
    // Small packs take the sync path (bench pack: 586 B); large packs keep streaming
    // (the 600-object history push stays on the streaming path by design).
    const { data, end } =
      pack.length < SYNC_INFLATE_MAX_PACK_BYTES ? inflateOneSync(pack, i) : await inflateOne(pack, i);
    if (data.length !== size) throw new Error(`pack object size mismatch: header ${size}, inflated ${data.length}`);
    raw.push({ typeCode, offset, baseOffset, baseOid, data, size });
    i = end;
  }

  // Second pass: resolve deltas (a delta's base may itself be a delta; resolve recursively).
  const byOffset = new Map<number, { type: ObjectType; data: Buffer }>();
  const byOid = new Map<string, { type: ObjectType; data: Buffer }>();
  let deltaCount = 0;
  const storeResolved = (entry: (typeof raw)[number], resolved: { type: ObjectType; data: Buffer }): void => {
    byOffset.set(entry.offset, resolved);
    byOid.set(objectId(resolved.type, resolved.data), resolved);
  };
  // Plain objects first (bases usually precede their deltas, but this pass makes it order-free).
  for (const entry of raw) {
    if (entry.typeCode === 6 || entry.typeCode === 7) continue;
    const type = CODE_TYPES[entry.typeCode];
    if (type === undefined) throw new Error(`unknown pack object type ${entry.typeCode}`);
    storeResolved(entry, { type, data: entry.data });
  }
  // O(1) OFS-delta base lookup (S3): the old `raw.find()` per delta is linear in the
  // pack size; the bench has no deltas, but delta-pack checks do.
  const rawByOffset = new Map<number, (typeof raw)[number]>();
  for (const entry of raw) rawByOffset.set(entry.offset, entry);
  const resolve = (entry: (typeof raw)[number]): { type: ObjectType; data: Buffer } => {
    const cached = byOffset.get(entry.offset);
    if (cached !== undefined) return cached;
    if (entry.typeCode !== 6 && entry.typeCode !== 7) {
      throw new Error('plain object was not resolved in the first pass');
    }
    deltaCount += 1;
    let base: { type: ObjectType; data: Buffer } | undefined;
    if (entry.typeCode === 6) {
      const baseEntry = rawByOffset.get(entry.baseOffset as number);
      if (baseEntry !== undefined) base = resolve(baseEntry);
    } else {
      base = byOid.get(entry.baseOid as string);
      if (base === undefined && resolveBase !== undefined) base = resolveBase(entry.baseOid as string);
    }
    if (base === undefined) throw new Error('delta base not found');
    const resolved = { type: base.type, data: applyDelta(base.data, entry.data) };
    storeResolved(entry, resolved);
    return resolved;
  };
  for (const entry of raw) resolve(entry);
  const objects = new Map<string, { type: ObjectType; data: Buffer }>();
  for (const [oid, obj] of byOid) objects.set(oid, obj);
  return { objects, deltaCount };
}

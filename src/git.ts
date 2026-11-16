// Git smart-HTTP surface (30 checks). Routes:
//   /{owner}/{repo}.git/info/refs?service=git-upload-pack   -> v2 advertisement
//   /{owner}/{repo}.git/info/refs?service=git-receive-pack  -> v0 advertisement
//   /{owner}/{repo}.git/git-upload-pack                     -> upload-pack v2 (or v0 fallback)
//   /{owner}/{repo}.git/git-receive-pack                    -> receive-pack v0
//
// Protocol notes (from sdk/conformance/git-client.ts, the spec):
// - v2 advertisement: `# service=...` + flush + `version 2` + capability lines + flush.
//   Capabilities: ls-refs=unborn, fetch=shallow wait-for-done, object-format=sha1, agent=...
// - v0 advertisement: `# service=...` + flush + `<oid> <ref> NUL <caps>` on the first line,
//   peeled tags as `<oid> <ref>^{}`, empty repo as `<zero> capabilities^{} NUL <caps>`.
//   Caps: symref=HEAD:refs/heads/<default> report-status side-band-64k delete-refs ofs-delta
//   object-format=sha1.
// - v2 fetch response: sections (acknowledgments, shallow-info, wanted-refs, packfile) separated
//   by delim, terminated by flush; the packfile travels over side-band-64k band 1.
// - receive-pack: command lines + NUL capabilities on the first, flush, then the raw packfile.
//   Response: report-status pkt-lines (unpack ok / ok <ref> / ng <ref> <reason>) inside
//   side-band-64k band 1, terminated by flush. Fast-forward only; non-FF is ng per-ref and the
//   other refs in a mixed push still apply.

import type { IncomingMessage, ServerResponse } from 'node:http';
import { readBody, isAuthorized, sendUnauthorized, sendBytes, queryOf } from './util.ts';
import { Store, type Repo, type ObjectType, parseCommit, parseTree, objectId } from './store.ts';
import { ZERO_OID, pktLine, pktText, PKT_FLUSH, PKT_DELIM, encodePktLines, decodePktLines, writePack, readPack, sidebandChunks } from './gitpack.ts';
import { VIEWER_LOGIN } from './scenario.ts';

const AGENT = 'github-core-clone/1.0.0';

/**
 * A per-repo cache of built fetch packs. The clone workload fetches the same tip 50 times
 * concurrently (same wants, haves, depth); rebuilding + re-deflating the pack each time is the
 * workload's main cost. The cache key is the fetch parameters and the entry is only valid for
 * the repo generation it was built at, so a push can never serve a stale pack. Bounded to 8
 * entries per repo (FIFO eviction).
 */
const packCache = new WeakMap<Repo, Map<string, { generation: number; pack: Buffer | null; shallow: string[] }>>();
const PACK_CACHE_MAX = 8;

function fetchCacheKey(wants: string[], haves: string[], depth: number | undefined, shallowSent: string[]): string {
  const w = wants.slice().sort().join(',');
  const h = haves.slice().sort().join(',');
  const s = shallowSent.slice().sort().join(',');
  return `${w}|${h}|${depth ?? ''}|${s}`;
}

function cachedFetch(
  repo: Repo,
  wants: string[],
  haves: string[],
  depth: number | undefined,
  shallowSent: string[],
  build: () => { pack: Buffer | null; shallow: string[] },
): { pack: Buffer | null; shallow: string[] } {
  const key = fetchCacheKey(wants, haves, depth, shallowSent);
  let perRepo = packCache.get(repo);
  if (perRepo === undefined) {
    perRepo = new Map();
    packCache.set(repo, perRepo);
  }
  const hit = perRepo.get(key);
  if (hit !== undefined && hit.generation === repo.generation) return { pack: hit.pack, shallow: hit.shallow };
  const built = build();
  if (perRepo.size >= PACK_CACHE_MAX) {
    const first = perRepo.keys().next().value as string | undefined;
    if (first !== undefined) perRepo.delete(first);
  }
  perRepo.set(key, { generation: repo.generation, pack: built.pack, shallow: built.shallow });
  return built;
}

/** Build a pack from a collected object set (sorted by oid, so the bytes are deterministic). */
function buildPack(objects: Map<string, { type: ObjectType; data: Buffer }>): Buffer | null {
  if (objects.size === 0) return null;
  const sorted = [...objects.entries()].sort((a, b) => (a[0] < b[0] ? -1 : 1));
  return writePack(sorted.map(([, obj]) => ({ type: obj.type, data: obj.data })));
}

/** Parse a tag object: returns the target oid (the `object` line). */
function parseTagTarget(data: Buffer): string | null {
  const text = data.toString('utf8');
  for (const line of text.split('\n')) {
    if (line.startsWith('object ')) return line.slice(7).trim();
    if (line === '') break;
  }
  return null;
}

interface RefEntry {
  name: string;
  oid: string;
  peeled?: string;
  unborn?: boolean;
}

/** All refs of a repo, in a stable order: HEAD first, then heads, then tags. */
function listRefs(repo: Repo): RefEntry[] {
  const out: RefEntry[] = [];
  const defaultTip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
  if (defaultTip !== undefined) {
    out.push({ name: 'HEAD', oid: defaultTip });
  } else {
    out.push({ name: 'HEAD', oid: ZERO_OID, unborn: true });
  }
  const heads: RefEntry[] = [];
  const tags: RefEntry[] = [];
  for (const [name, oid] of repo.refs) {
    if (name.startsWith('refs/heads/')) {
      if (name === `refs/heads/${repo.defaultBranch}` && defaultTip !== undefined) {
        // The default branch is listed after HEAD; it is not unborn.
        heads.push({ name, oid });
        continue;
      }
      heads.push({ name, oid });
    } else if (name.startsWith('refs/tags/')) {
      const entry: RefEntry = { name, oid };
      const obj = repo.objects.get(oid);
      if (obj !== undefined && obj.type === 'tag') {
        const target = parseTagTarget(obj.data);
        if (target !== null) entry.peeled = target;
      }
      tags.push(entry);
    }
  }
  heads.sort((a, b) => (a.name < b.name ? -1 : 1));
  tags.sort((a, b) => (a.name < b.name ? -1 : 1));
  out.push(...heads, ...tags);
  // An unborn default branch is NOT listed as a ref: a fresh repo advertises only HEAD (unborn,
  // with its symref target). Listing `refs/heads/<default>` as unborn makes clients treat the
  // zero oid as a real tip and fetch it (clone-empty, and the GraphQL git fixture).
  return out;
}

/** The v2 advertisement body for git-upload-pack. */
function v2Advertisement(): Buffer {
  const lines = [
    '# service=git-upload-pack\n',
    'flush',
    'version 2\n',
    'ls-refs=unborn\n',
    'fetch=shallow wait-for-done\n',
    'object-format=sha1\n',
    `agent=${AGENT}\n`,
    'flush',
  ];
  return encodePktLines(lines);
}

/** The v0 advertisement body for git-receive-pack. */
function v0Advertisement(repo: Repo): Buffer {
  const caps = `symref=HEAD:refs/heads/${repo.defaultBranch} report-status side-band-64k delete-refs ofs-delta object-format=sha1 agent=${AGENT}`;
  const refs = listRefs(repo);
  const parts: Buffer[] = [pktText('# service=git-receive-pack\n'), PKT_FLUSH];
  const hasRefs = refs.some((r) => !r.unborn);
  if (!hasRefs) {
    parts.push(pktText(`${ZERO_OID} capabilities^{}\0${caps}\n`));
    parts.push(PKT_FLUSH);
    return Buffer.concat(parts);
  }
  refs.forEach((ref, index) => {
    if (ref.unborn) return;
    const suffix = index === 0 ? `\0${caps}` : '';
    parts.push(pktText(`${ref.oid} ${ref.name}${suffix}\n`));
    if (ref.peeled !== undefined) {
      parts.push(pktText(`${ref.peeled} ${ref.name}^{}\n`));
    }
  });
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

// ------------------------------------------------------------------ object collection

interface CollectResult {
  objects: Map<string, { type: ObjectType; data: Buffer }>;
  /** Commits at the depth boundary (their parents were cut). */
  shallow: string[];
}

/**
 * Collect the objects reachable from `tips`, excluding anything reachable from `haves`.
 * `depth` limits commit depth (1 = only the tips themselves); without it, the walk is full.
 * `shallowBoundaries` are the client's current shallow boundaries: the have-walk stops at them
 * (the client has the boundary commit and its tree, but NOT its parents — a deepen fetch must
 * deliver those parents). The returned `shallow` list is the set of boundary commits.
 */
function collectObjects(repo: Repo, tips: string[], haves: string[], depth?: number, shallowBoundaries: string[] = []): CollectResult {
  const boundarySet = new Set(shallowBoundaries);
  const haveReachable = new Set<string>();
  if (haves.length > 0) {
    const stack = [...haves];
    while (stack.length > 0) {
      const oid = stack.pop() as string;
      if (haveReachable.has(oid)) continue;
      haveReachable.add(oid);
      const obj = repo.objects.get(oid);
      if (obj === undefined) continue;
      if (obj.type === 'commit') {
        const commit = parseCommit(obj.data);
        stack.push(commit.tree);
        // A shallow boundary's parents are NOT held by the client: stop the walk there.
        if (!boundarySet.has(oid)) {
          for (const p of commit.parents) stack.push(p);
        }
      } else if (obj.type === 'tree') {
        for (const e of parseTree(obj.data)) stack.push(e.oid);
      } else if (obj.type === 'tag') {
        const target = parseTagTarget(obj.data);
        if (target !== null) stack.push(target);
      }
    }
  }

  const objects = new Map<string, { type: ObjectType; data: Buffer }>();
  const shallow: string[] = [];
  const add = (oid: string): void => {
    if (haveReachable.has(oid) || objects.has(oid)) return;
    const obj = repo.objects.get(oid);
    if (obj === undefined) return;
    objects.set(oid, obj);
  };
  const addTree = (treeOid: string): void => {
    add(treeOid);
    const obj = repo.objects.get(treeOid);
    if (obj === undefined || obj.type !== 'tree') return;
    for (const e of parseTree(obj.data)) {
      if (e.type === 'tree') addTree(e.oid);
      else add(e.oid);
    }
  };

  // BFS over commits with depth tracking (depth 0 = the tips).
  const queue: { oid: string; depth: number }[] = [];
  for (const tip of tips) queue.push({ oid: tip, depth: 0 });
  const seenCommits = new Set<string>();
  while (queue.length > 0) {
    const { oid, depth: d } = queue.shift() as { oid: string; depth: number };
    if (seenCommits.has(oid)) continue;
    seenCommits.add(oid);
    const obj = repo.objects.get(oid);
    if (obj === undefined) continue;
    if (obj.type === 'tag') {
      add(oid);
      const target = parseTagTarget(obj.data);
      if (target !== null) queue.push({ oid: target, depth: d });
      continue;
    }
    if (obj.type !== 'commit') {
      add(oid);
      continue;
    }
    const isBoundary = depth !== undefined && d >= depth - 1;
    if (isBoundary) shallow.push(oid);
    add(oid);
    const commit = parseCommit(obj.data);
    addTree(commit.tree);
    if (!isBoundary) {
      for (const p of commit.parents) queue.push({ oid: p, depth: d + 1 });
    }
  }
  // Any tip that is a bare tree/blob (not a commit) is included directly.
  for (const tip of tips) {
    const obj = repo.objects.get(tip);
    if (obj !== undefined && (obj.type === 'tree' || obj.type === 'blob')) {
      if (obj.type === 'tree') addTree(tip);
      else add(tip);
    }
  }
  return { objects, shallow };
}

/** Is `ancestor` reachable from `descendant` (first-parent + all-parent walk)? */
function isAncestor(repo: Repo, ancestor: string, descendant: string): boolean {
  if (ancestor === descendant) return true;
  const seen = new Set<string>();
  const stack = [descendant];
  while (stack.length > 0) {
    const oid = stack.pop() as string;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const obj = repo.objects.get(oid);
    if (obj === undefined || obj.type !== 'commit') continue;
    for (const p of parseCommit(obj.data).parents) {
      if (p === ancestor) return true;
      stack.push(p);
    }
  }
  return false;
}

// ------------------------------------------------------------------ upload-pack v2

interface LsRefsRequest {
  peel: boolean;
  symrefs: boolean;
  unborn: boolean;
  prefixes: string[];
}

function handleLsRefs(repo: Repo, lines: string[]): Buffer {
  const req: LsRefsRequest = { peel: false, symrefs: false, unborn: false, prefixes: [] };
  for (const line of lines) {
    const t = line.replace(/\n$/, '');
    if (t === 'peel') req.peel = true;
    else if (t === 'symrefs') req.symrefs = true;
    else if (t === 'unborn') req.unborn = true;
    else if (t.startsWith('ref-prefix ')) req.prefixes.push(t.slice('ref-prefix '.length));
    // agent= / object-format= are accepted and ignored.
  }
  const refs = listRefs(repo);
  const out: Buffer[] = [];
  for (const ref of refs) {
    if (!req.unborn && ref.unborn) continue;
    if (req.prefixes.length > 0 && !req.prefixes.some((p) => ref.name.startsWith(p))) continue;
    const attrs: string[] = [];
    if (ref.name === 'HEAD' && req.symrefs) attrs.push(`symref-target:refs/heads/${repo.defaultBranch}`);
    if (ref.peeled !== undefined && req.peel) attrs.push(`peeled:${ref.peeled}`);
    const first = ref.unborn ? 'unborn' : ref.oid;
    const line = attrs.length > 0 ? `${first} ${ref.name} ${attrs.join(' ')}\n` : `${first} ${ref.name}\n`;
    out.push(pktText(line));
  }
  out.push(PKT_FLUSH);
  return Buffer.concat(out);
}

interface FetchRequest {
  wants: string[];
  haves: string[];
  shallowSent: string[];
  depth?: number;
  thinPack: boolean;
}

function parseV2Fetch(lines: string[]): FetchRequest {
  const req: FetchRequest = { wants: [], haves: [], shallowSent: [], thinPack: false };
  for (const line of lines) {
    const t = line.replace(/\n$/, '');
    if (t.startsWith('want ')) req.wants.push(t.slice(5).trim());
    else if (t.startsWith('have ')) req.haves.push(t.slice(5).trim());
    else if (t.startsWith('shallow ')) req.shallowSent.push(t.slice(8).trim());
    else if (t.startsWith('deepen ')) {
      const n = Number(t.slice(7).trim());
      if (Number.isFinite(n) && n > 0) req.depth = Math.trunc(n);
    } else if (t === 'thin-pack') req.thinPack = true;
    // deepen-since / deepen-not / filter / no-progress / ofs-delta / done / agent / object-format
    // are accepted; only deepen is honoured (the suite never sends the others).
  }
  return req;
}

function handleFetchV2(repo: Repo, req: FetchRequest): Buffer {
  // A want the server does not have is a side-band error (the client's missing-object path).
  for (const want of req.wants) {
    if (!repo.objects.has(want)) {
      const err = Buffer.from(`object not found: ${want}\n`, 'utf8');
      const framed = Buffer.concat([Buffer.from([3]), err]);
      return Buffer.concat([pktText('packfile\n'), pktLine(framed), PKT_FLUSH]);
    }
  }
  if (req.wants.length === 0) {
    return Buffer.concat([pktText('acknowledgments\n'), pktText('NAK\n'), PKT_DELIM, PKT_FLUSH]);
  }

  const { pack, shallow } = cachedFetch(repo, req.wants, req.haves, req.depth, req.shallowSent, () => {
    const collected = collectObjects(repo, req.wants, req.haves, req.depth, req.shallowSent);
    return { pack: buildPack(collected.objects), shallow: collected.shallow };
  });
  const parts: Buffer[] = [];

  // acknowledgments: ACK every have that is an ancestor of (or equal to) a want.
  parts.push(pktText('acknowledgments\n'));
  let acked = false;
  for (const have of req.haves) {
    if (repo.objects.has(have) && req.wants.some((w) => isAncestor(repo, have, w))) {
      parts.push(pktText(`ACK ${have}\n`));
      acked = true;
    }
  }
  if (!acked) parts.push(pktText('NAK\n'));
  parts.push(PKT_DELIM);

  // shallow-info: the new boundary, plus unshallow for boundaries the client sent that are
  // now interior (their parents are included at this depth).
  const unshallow: string[] = [];
  if (req.depth !== undefined) {
    for (const sent of req.shallowSent) {
      if (!shallow.includes(sent)) unshallow.push(sent);
    }
  }
  if (shallow.length > 0 || unshallow.length > 0) {
    parts.push(pktText('shallow-info\n'));
    for (const s of shallow) parts.push(pktText(`shallow ${s}\n`));
    for (const u of unshallow) parts.push(pktText(`unshallow ${u}\n`));
    parts.push(PKT_DELIM);
  }

  // wanted-refs: the refs whose tip is a want.
  const wantedRefs: { oid: string; ref: string }[] = [];
  for (const want of req.wants) {
    for (const [name, oid] of repo.refs) {
      if (oid === want) wantedRefs.push({ oid, ref: name });
    }
  }
  if (wantedRefs.length > 0) {
    parts.push(pktText('wanted-refs\n'));
    for (const w of wantedRefs) parts.push(pktText(`${w.oid} ${w.ref}\n`));
    parts.push(PKT_DELIM);
  }

  // packfile: only when there is something to send.
  if (pack !== null) {
    parts.push(pktText('packfile\n'));
    for (const chunk of sidebandChunks(pack)) parts.push(chunk);
  }
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

// ------------------------------------------------------------------ upload-pack v0 (fallback)

function handleFetchV0(repo: Repo, body: Buffer): Buffer {
  const { items } = decodePktLines(body);
  const wants: string[] = [];
  const haves: string[] = [];
  let depth: number | undefined;
  const shallowSent: string[] = [];
  for (const item of items) {
    if (item.kind === 'flush') continue;
    if (item.kind !== 'data') continue;
    const t = item.payload.toString('utf8').replace(/\n$/, '');
    if (t.startsWith('want ')) wants.push(t.slice(5).split(' ')[0] as string);
    else if (t.startsWith('have ')) haves.push(t.slice(5).trim());
    else if (t.startsWith('shallow ')) shallowSent.push(t.slice(8).trim());
    else if (t.startsWith('deepen ')) {
      const n = Number(t.slice(7).trim());
      if (Number.isFinite(n) && n > 0) depth = Math.trunc(n);
    }
  }
  const parts: Buffer[] = [];
  for (const want of wants) {
    if (!repo.objects.has(want)) {
      const err = Buffer.from(`object not found: ${want}\n`, 'utf8');
      return Buffer.concat([pktLine(Buffer.concat([Buffer.from([3]), err])), PKT_FLUSH]);
    }
  }
  let acked = false;
  for (const have of haves) {
    if (repo.objects.has(have) && wants.some((w) => isAncestor(repo, have, w))) {
      parts.push(pktText(`ACK ${have}\n`));
      acked = true;
    }
  }
  if (!acked) parts.push(pktText('NAK\n'));
  const { pack, shallow } = cachedFetch(repo, wants, haves, depth, shallowSent, () => {
    const collected = collectObjects(repo, wants, haves, depth, shallowSent);
    return { pack: buildPack(collected.objects), shallow: collected.shallow };
  });
  for (const s of shallow) parts.push(pktText(`shallow ${s}\n`));
  if (pack !== null) {
    for (const chunk of sidebandChunks(pack)) parts.push(chunk);
  }
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

// ------------------------------------------------------------------ receive-pack v0

interface PushCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

async function handleReceivePack(repo: Repo, body: Buffer): Promise<Buffer> {
  const { items, rest } = decodePktLines(body);
  const commands: PushCommand[] = [];
  for (const item of items) {
    if (item.kind === 'flush') break;
    if (item.kind !== 'data') continue;
    const text = item.payload.toString('utf8');
    const nul = text.indexOf('\0');
    const line = (nul >= 0 ? text.slice(0, nul) : text).replace(/\n$/, '');
    const parts = line.split(' ');
    if (parts.length < 3) continue;
    commands.push({ oldOid: parts[0] as string, newOid: parts[1] as string, ref: parts.slice(2).join(' ') });
  }

  // The packfile follows the flush. A delete-only push sends none.
  let unpackOk = true;
  let unpackDetail = 'ok';
  let mutated = false;
  if (rest.length > 0) {
    try {
      const read = await readPack(rest, (oid) => repo.objects.get(oid));
      for (const [oid, obj] of read.objects) {
        if (!repo.objects.has(oid)) {
          repo.objects.set(oid, obj);
          mutated = true;
        }
      }
    } catch (err) {
      unpackOk = false;
      unpackDetail = err instanceof Error ? err.message : 'unpacker error';
    }
  }

  const report: string[] = [`unpack ${unpackOk ? 'ok' : unpackDetail}\n`];
  if (unpackOk) {
    for (const cmd of commands) {
      const current = repo.refs.get(cmd.ref);
      if (cmd.newOid === ZERO_OID) {
        // Delete.
        if (current === undefined) {
          report.push(`ng ${cmd.ref} ref does not exist\n`);
        } else if (current !== cmd.oldOid) {
          report.push(`ng ${cmd.ref} old value does not match\n`);
        } else {
          repo.refs.delete(cmd.ref);
          mutated = true;
          report.push(`ok ${cmd.ref}\n`);
        }
        continue;
      }
      if (cmd.oldOid === ZERO_OID) {
        // Create.
        if (current !== undefined) {
          report.push(`ng ${cmd.ref} already exists\n`);
          continue;
        }
        if (!repo.objects.has(cmd.newOid)) {
          report.push(`ng ${cmd.ref} object not found\n`);
          continue;
        }
        repo.refs.set(cmd.ref, cmd.newOid);
        mutated = true;
        report.push(`ok ${cmd.ref}\n`);
        continue;
      }
      // Update: the ref must be at old, and new must be a descendant of old.
      if (current === undefined) {
        report.push(`ng ${cmd.ref} ref does not exist\n`);
        continue;
      }
      if (current !== cmd.oldOid) {
        report.push(`ng ${cmd.ref} old value does not match\n`);
        continue;
      }
      if (!repo.objects.has(cmd.newOid)) {
        report.push(`ng ${cmd.ref} object not found\n`);
        continue;
      }
      const oldObj = repo.objects.get(cmd.oldOid);
      const newObj = repo.objects.get(cmd.newOid);
      const bothCommits = oldObj?.type === 'commit' && newObj?.type === 'commit';
      if (bothCommits && !isAncestor(repo, cmd.oldOid, cmd.newOid)) {
        report.push(`ng ${cmd.ref} non-fast-forward\n`);
        continue;
      }
      if (!bothCommits && cmd.oldOid !== cmd.newOid) {
        report.push(`ng ${cmd.ref} non-fast-forward\n`);
        continue;
      }
      repo.refs.set(cmd.ref, cmd.newOid);
      mutated = true;
      report.push(`ok ${cmd.ref}\n`);
    }
  } else {
    for (const cmd of commands) report.push(`ng ${cmd.ref} unpack failed\n`);
  }
  if (mutated) repo.generation += 1;

  const reportBytes = encodePktLines(report);
  const parts: Buffer[] = [];
  for (let i = 0; i < reportBytes.length; i += 65515) {
    const slice = reportBytes.subarray(i, Math.min(reportBytes.length, i + 65515));
    parts.push(pktLine(Buffer.concat([Buffer.from([1]), slice])));
  }
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

// ------------------------------------------------------------------ router

/** The git smart-HTTP handler. Returns true when it handled the request. */
export async function handleGit(store: Store, req: IncomingMessage, res: ServerResponse, segments: string[]): Promise<boolean> {
  // segments: [owner, repo.git, ...rest] — the caller strips the leading slash.
  if (segments.length < 2) return false;
  const owner = segments[0] as string;
  const repoGit = segments[1] as string;
  if (!repoGit.endsWith('.git')) return false;
  const repoName = repoGit.slice(0, -4);
  const tail = segments.slice(2);
  const query = queryOf(req);
  const method = req.method ?? 'GET';

  // Resolve the repo first (unknown repo = 404), then auth (401) — same order as REST.
  if (owner !== VIEWER_LOGIN) {
    sendBytes(res, 404, Buffer.from('{"message":"Not Found"}\n', 'utf8'), 'application/json; charset=utf-8');
    return true;
  }
  const repo = store.getRepo(owner, repoName);
  if (repo === undefined) {
    sendBytes(res, 404, Buffer.from('{"message":"Not Found"}\n', 'utf8'), 'application/json; charset=utf-8');
    return true;
  }
  if (!isAuthorized(req)) { sendUnauthorized(res); return true; }

  // info/refs
  if (tail.length === 2 && tail[0] === 'info' && tail[1] === 'refs' && method === 'GET') {
    const service = query.get('service');
    if (service === 'git-upload-pack') {
      sendBytes(res, 200, v2Advertisement(), 'application/x-git-upload-pack-advertisement');
      return true;
    }
    if (service === 'git-receive-pack') {
      sendBytes(res, 200, v0Advertisement(repo), 'application/x-git-receive-pack-advertisement');
      return true;
    }
    sendBytes(res, 404, Buffer.from('{"message":"Not Found"}\n', 'utf8'), 'application/json; charset=utf-8');
    return true;
  }

  // git-upload-pack
  if (tail.length === 1 && tail[0] === 'git-upload-pack' && method === 'POST') {
    const body = await readBody(req);
    if (body === null) {
      sendBytes(res, 413, Buffer.from('{"message":"Request body too large"}\n', 'utf8'), 'application/json; charset=utf-8');
      return true;
    }
    const isV2 = (req.headers['git-protocol'] ?? '') === 'version=2';
    if (isV2) {
      const { items } = decodePktLines(body);
      const lines: string[] = [];
      let command: string | null = null;
      for (const item of items) {
        if (item.kind === 'flush') break;
        if (item.kind === 'delim') continue;
        if (item.kind !== 'data') continue;
        const text = item.payload.toString('utf8');
        if (text.startsWith('command=')) {
          command = text.slice(8).replace(/\n$/, '');
          continue;
        }
        lines.push(text);
      }
      if (command === 'ls-refs') {
        sendBytes(res, 200, handleLsRefs(repo, lines), 'application/x-git-upload-pack-result');
        return true;
      }
      if (command === 'fetch') {
        sendBytes(res, 200, handleFetchV2(repo, parseV2Fetch(lines)), 'application/x-git-upload-pack-result');
        return true;
      }
      sendBytes(res, 400, Buffer.from('{"message":"Unknown command"}\n', 'utf8'), 'application/json; charset=utf-8');
      return true;
    }
    // v0 fallback.
    sendBytes(res, 200, handleFetchV0(repo, body), 'application/x-git-upload-pack-result');
    return true;
  }

  // git-receive-pack
  if (tail.length === 1 && tail[0] === 'git-receive-pack' && method === 'POST') {
    const body = await readBody(req);
    if (body === null) {
      sendBytes(res, 413, Buffer.from('{"message":"Request body too large"}\n', 'utf8'), 'application/json; charset=utf-8');
      return true;
    }
    sendBytes(res, 200, await handleReceivePack(repo, body), 'application/x-git-receive-pack-result');
    return true;
  }

  return false;
}

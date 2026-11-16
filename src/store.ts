// The forge's single in-memory store. Every surface (git smart HTTP, REST v3, GraphQL, MCP)
// reads and writes this one store, so a repo, issue, pull request, comment or merge created
// on one surface is immediately visible on the others with the same numbers and ids.
//
// Determinism: object ids are SHA-1s of content; database ids are a creation-order counter;
// global ids are base64url of a typed key (opaque, stable across reads, never the plain
// database id or number); server-generated timestamps use the trial-now helper. No clock,
// no Math.random anywhere in the store.

import { createHash } from 'node:crypto';
import { b64url, trialNowIso, trialNowSec } from './util.ts';
import { DEFAULT_BRANCH, VIEWER_LOGIN } from './scenario.ts';

export type ObjectType = 'commit' | 'tree' | 'blob' | 'tag';

export interface GitObject {
  type: ObjectType;
  data: Buffer;
}

export interface Label {
  name: string;
  color: string;
}

export interface Comment {
  id: number;
  body: string;
}

export type IssueState = 'open' | 'closed';

export interface Issue {
  number: number;
  title: string;
  body: string;
  state: IssueState;
  labels: string[];
  comments: Comment[];
  nextCommentId: number;
}

export interface PullFile {
  filename: string;
  status: string;
  additions: number;
  deletions: number;
  changes: number;
}

export type PullState = 'open' | 'closed';

export interface Pull {
  number: number;
  title: string;
  body: string;
  state: PullState;
  headRef: string;
  baseRef: string;
  headSha: string;
  baseSha: string;
  merged: boolean;
  mergedAt: string | null;
  mergeSha: string | null;
  files: PullFile[];
}

export interface Repo {
  databaseId: number;
  name: string;
  owner: string;
  description: string;
  private: boolean;
  defaultBranch: string;
  /** ref name (refs/heads/...) -> commit oid. */
  refs: Map<string, string>;
  /** git objects, keyed by lowercase hex oid. */
  objects: Map<string, GitObject>;
  labels: Map<string, Label>;
  issues: Map<number, Issue>;
  pulls: Map<number, Pull>;
  nextIssue: number;
  nextPull: number;
  /** Creation order, for stable list ordering. */
  seq: number;
  /** Bumped on every object/ref mutation; the git pack cache keys on it. */
  generation: number;
}

export interface TreeEntry {
  mode: string;
  name: string;
  oid: string;
  type: ObjectType;
}

const EMPTY_TREE_OID = '4b825dc642cb6eb9a060e54bf8d69288fbee4904';

/** The SHA-1 of a git object: `<type> <size>\0` + bytes. */
export function objectId(type: ObjectType, data: Buffer): string {
  const header = Buffer.from(`${type} ${data.length}\0`, 'ascii');
  return createHash('sha1').update(header).update(data).digest('hex');
}

/** Store an object, returning its oid (idempotent). A new object bumps the repo generation. */
export function putObject(repo: Repo, type: ObjectType, data: Buffer): string {
  const oid = objectId(type, data);
  if (!repo.objects.has(oid)) {
    repo.objects.set(oid, { type, data });
    repo.generation += 1;
  }
  return oid;
}

/** Encode a tree object from its entries (sorted by git's byte order). */
export function encodeTree(entries: TreeEntry[]): Buffer {
  const sorted = entries.slice().sort((a, b) => {
    const ka = a.name + (a.type === 'tree' ? '/' : '');
    const kb = b.name + (b.type === 'tree' ? '/' : '');
    return Buffer.compare(Buffer.from(ka, 'utf8'), Buffer.from(kb, 'utf8'));
  });
  const parts: Buffer[] = [];
  for (const e of sorted) {
    const modeName = Buffer.from(`${e.mode} ${e.name}\0`, 'utf8');
    const oidBytes = Buffer.from(e.oid, 'hex');
    parts.push(Buffer.concat([modeName, oidBytes]));
  }
  return Buffer.concat(parts);
}

/** Parse a tree object into its entries. */
export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let i = 0;
  while (i < data.length) {
    const space = data.indexOf(0x20, i);
    const nul = data.indexOf(0x00, space);
    const mode = data.toString('ascii', i, space);
    const name = data.toString('utf8', space + 1, nul);
    const oid = data.subarray(nul + 1, nul + 21).toString('hex');
    const type: ObjectType = mode === '40000' ? 'tree' : 'blob';
    entries.push({ mode, name, oid, type });
    i = nul + 21;
  }
  return entries;
}

export interface CommitData {
  tree: string;
  parents: string[];
  author: string;
  committer: string;
  message: string;
}

/** Encode a commit object. */
export function encodeCommit(c: CommitData): Buffer {
  let out = `tree ${c.tree}\n`;
  for (const p of c.parents) out += `parent ${p}\n`;
  out += `author ${c.author}\ncommitter ${c.committer}\n\n${c.message}`;
  return Buffer.from(out, 'utf8');
}

/** Parse a commit object. */
export function parseCommit(data: Buffer): CommitData {
  const text = data.toString('utf8');
  const headerEnd = text.indexOf('\n\n');
  const header = headerEnd >= 0 ? text.slice(0, headerEnd) : text;
  const message = headerEnd >= 0 ? text.slice(headerEnd + 2) : '';
  const parents: string[] = [];
  let tree = '';
  let author = '';
  let committer = '';
  for (const line of header.split('\n')) {
    if (line.startsWith('tree ')) tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) parents.push(line.slice(7).trim());
    else if (line.startsWith('author ')) author = line.slice(7);
    else if (line.startsWith('committer ')) committer = line.slice(10);
  }
  return { tree, parents, author, committer, message };
}

/** The fixed server identity used for squash-merge commits. */
function serverIdentity(): string {
  const ts = trialNowSec();
  return `forge <forge@example.invalid> ${ts} +0000`;
}

/** Split a path into segments, ignoring empty pieces. */
export function splitPath(path: string): string[] {
  return path.split('/').filter((s) => s !== '' && s !== '.');
}

/**
 * Read a tree's entries recursively into a flat map: path -> { oid, mode }.
 * Only blobs are returned (trees are traversed, not listed).
 */
export function flattenTree(repo: Repo, treeOid: string, prefix = '', out = new Map<string, { oid: string; mode: string }>()): Map<string, { oid: string; mode: string }> {
  const obj = repo.objects.get(treeOid);
  if (obj === undefined || obj.type !== 'tree') return out;
  for (const e of parseTree(obj.data)) {
    const path = prefix === '' ? e.name : `${prefix}/${e.name}`;
    if (e.type === 'tree') flattenTree(repo, e.oid, path, out);
    else out.set(path, { oid: e.oid, mode: e.mode });
  }
  return out;
}

/**
 * Set one path in a tree, creating intermediate trees, and return the new tree oid.
 * `value` is null to delete the path. The tree is written bottom-up; unchanged subtrees are
 * reused, so the operation is O(depth + siblings).
 */
export function setTreePath(repo: Repo, treeOid: string, segments: string[], value: { oid: string; mode: string } | null): string {
  const obj = repo.objects.get(treeOid);
  const entries: TreeEntry[] = obj !== undefined && obj.type === 'tree' ? parseTree(obj.data) : [];
  const [head, ...rest] = segments as [string, ...string[]];
  const idx = entries.findIndex((e) => e.name === head);
  if (rest.length === 0) {
    if (value === null) {
      if (idx >= 0) entries.splice(idx, 1);
    } else if (idx >= 0) {
      entries[idx] = { mode: value.mode, name: head, oid: value.oid, type: 'blob' };
    } else {
      entries.push({ mode: value.mode, name: head, oid: value.oid, type: 'blob' });
    }
  } else {
    const childOid = idx >= 0 && entries[idx]?.type === 'tree' ? (entries[idx] as TreeEntry).oid : EMPTY_TREE_OID;
    const newChild = setTreePath(repo, childOid, rest, value);
    const childObj = repo.objects.get(newChild);
    const isEmpty = childObj !== undefined && childObj.type === 'tree' && childObj.data.length === 0;
    if (isEmpty) {
      if (idx >= 0) entries.splice(idx, 1);
    } else if (idx >= 0) {
      entries[idx] = { mode: '40000', name: head, oid: newChild, type: 'tree' };
    } else {
      entries.push({ mode: '40000', name: head, oid: newChild, type: 'tree' });
    }
  }
  const data = encodeTree(entries);
  return putObject(repo, 'tree', data);
}

/** Read a blob's bytes at a path in a tree, or null when the path is absent. */
export function readTreePath(repo: Repo, treeOid: string, segments: string[]): { oid: string; mode: string } | null {
  const flat = flattenTree(repo, treeOid);
  const key = segments.join('/');
  return flat.get(key) ?? null;
}

/** List the direct children of a directory path in a tree (files and subdirs). */
export function listTreeDir(repo: Repo, treeOid: string, segments: string[]): { name: string; path: string; oid: string; type: 'file' | 'dir' }[] | null {
  let current = treeOid;
  for (const seg of segments) {
    const obj = repo.objects.get(current);
    if (obj === undefined || obj.type !== 'tree') return null;
    const entry = parseTree(obj.data).find((e) => e.name === seg);
    if (entry === undefined || entry.type !== 'tree') return null;
    current = entry.oid;
  }
  const obj = repo.objects.get(current);
  if (obj === undefined || obj.type !== 'tree') return null;
  const prefix = segments.join('/');
  return parseTree(obj.data).map((e) => ({
    name: e.name,
    path: prefix === '' ? e.name : `${prefix}/${e.name}`,
    oid: e.oid,
    type: e.type === 'tree' ? ('dir' as const) : ('file' as const),
  }));
}

/** Count lines in a buffer (a trailing newline does not add an empty line). */
export function lineCount(data: Buffer): number {
  if (data.length === 0) return 0;
  const text = data.toString('utf8');
  const lines = text.split('\n');
  if (lines[lines.length - 1] === '') lines.pop();
  return lines.length;
}

/** A cheap line-diff: additions = lines in new not in old, deletions = lines in old not in new. */
export function lineDiff(oldData: Buffer | null, newData: Buffer | null): { additions: number; deletions: number } {
  const oldLines = oldData === null ? [] : oldData.toString('utf8').split('\n').filter((l) => l !== '');
  const newLines = newData === null ? [] : newData.toString('utf8').split('\n').filter((l) => l !== '');
  const oldSet = new Set(oldLines);
  const newSet = new Set(newLines);
  let additions = 0;
  for (const l of newLines) if (!oldSet.has(l)) additions += 1;
  let deletions = 0;
  for (const l of oldLines) if (!newSet.has(l)) deletions += 1;
  return { additions, deletions };
}

/** Diff two trees (base -> head) into pull-file rows. */
export function diffTrees(repo: Repo, baseTreeOid: string, headTreeOid: string): PullFile[] {
  const base = flattenTree(repo, baseTreeOid);
  const head = flattenTree(repo, headTreeOid);
  const files: PullFile[] = [];
  const paths = new Set<string>([...base.keys(), ...head.keys()]);
  for (const path of paths) {
    const b = base.get(path);
    const h = head.get(path);
    if (b !== undefined && h !== undefined && b.oid === h.oid) continue;
    if (b === undefined && h !== undefined) {
      const data = repo.objects.get(h.oid)?.data ?? null;
      const { additions } = lineDiff(null, data);
      files.push({ filename: path, status: 'added', additions, deletions: 0, changes: additions });
    } else if (b !== undefined && h === undefined) {
      const data = repo.objects.get(b.oid)?.data ?? null;
      const { deletions } = lineDiff(data, null);
      files.push({ filename: path, status: 'removed', additions: 0, deletions, changes: deletions });
    } else if (b !== undefined && h !== undefined) {
      const oldData = repo.objects.get(b.oid)?.data ?? null;
      const newData = repo.objects.get(h.oid)?.data ?? null;
      const { additions, deletions } = lineDiff(oldData, newData);
      files.push({ filename: path, status: 'modified', additions, deletions, changes: additions + deletions });
    }
  }
  files.sort((a, b) => (a.filename < b.filename ? -1 : a.filename > b.filename ? 1 : 0));
  return files;
}

/** Walk a commit's first-parent history (newest first), bounded to keep it cheap. */
export function commitHistory(repo: Repo, tipOid: string, limit = 1000): { oid: string; message: string }[] {
  const out: { oid: string; message: string }[] = [];
  const seen = new Set<string>();
  let current: string | undefined = tipOid;
  while (current !== undefined && out.length < limit && !seen.has(current)) {
    seen.add(current);
    const obj = repo.objects.get(current);
    if (obj === undefined || obj.type !== 'commit') break;
    const commit = parseCommit(obj.data);
    out.push({ oid: current, message: commit.message });
    current = commit.parents[0];
  }
  return out;
}

/** Opaque, stable global id for a repository. */
export function repoGlobalId(repo: Repo): string {
  return b64url(`Repository:${repo.databaseId}`);
}

/** Opaque, stable global id for an issue. */
export function issueGlobalId(repo: Repo, issue: Issue): string {
  return b64url(`Issue:${repo.databaseId}:${issue.number}`);
}

/** Opaque, stable global id for a pull request. */
export function pullGlobalId(repo: Repo, pull: Pull): string {
  return b64url(`PullRequest:${repo.databaseId}:${pull.number}`);
}

/** Opaque, stable global id for a comment. */
export function commentGlobalId(repo: Repo, issue: Issue, comment: Comment): string {
  return b64url(`IssueComment:${repo.databaseId}:${issue.number}:${comment.id}`);
}

/** The store itself: one map of repos plus the counters that make ids deterministic. */
export class Store {
  /** key = `${owner}/${name}` (lowercased for lookup; names keep their case in the record). */
  private repos = new Map<string, Repo>();
  private nextDatabaseId = 1;
  private nextSeq = 1;

  repoKey(owner: string, name: string): string {
    return `${owner.toLowerCase()}/${name.toLowerCase()}`;
  }

  getRepo(owner: string, name: string): Repo | undefined {
    return this.repos.get(this.repoKey(owner, name));
  }

  hasRepo(owner: string, name: string): boolean {
    return this.repos.has(this.repoKey(owner, name));
  }

  /** Create a repo owned by the viewer. Returns null when a repo of that name already exists. */
  createRepo(name: string, description: string, isPrivate: boolean): Repo | null {
    if (this.hasRepo(VIEWER_LOGIN, name)) return null;
    const repo: Repo = {
      databaseId: this.nextDatabaseId,
      name,
      owner: VIEWER_LOGIN,
      description,
      private: isPrivate,
      defaultBranch: DEFAULT_BRANCH,
      refs: new Map(),
      objects: new Map(),
      labels: new Map(),
      issues: new Map(),
      pulls: new Map(),
      nextIssue: 1,
      nextPull: 1,
      seq: this.nextSeq,
      generation: 0,
    };
    this.nextDatabaseId += 1;
    this.nextSeq += 1;
    // The empty tree object exists in every repo from the start.
    putObject(repo, 'tree', Buffer.alloc(0));
    this.repos.set(this.repoKey(VIEWER_LOGIN, name), repo);
    return repo;
  }

  deleteRepo(owner: string, name: string): boolean {
    return this.repos.delete(this.repoKey(owner, name));
  }

  /** All repos owned by `owner`, in creation order. */
  listRepos(owner: string): Repo[] {
    const out: Repo[] = [];
    for (const repo of this.repos.values()) {
      if (repo.owner === owner) out.push(repo);
    }
    out.sort((a, b) => a.seq - b.seq);
    return out;
  }

  /** The tip commit oid of a branch, or null when the branch is unborn/absent. */
  branchTip(repo: Repo, branch: string): string | null {
    return repo.refs.get(`refs/heads/${branch}`) ?? null;
  }

  /** Set a branch to a commit oid. Bumps the repo generation. */
  setBranch(repo: Repo, branch: string, oid: string): void {
    repo.refs.set(`refs/heads/${branch}`, oid);
    repo.generation += 1;
  }

  /** Delete a branch. Bumps the repo generation. */
  deleteBranch(repo: Repo, branch: string): void {
    repo.refs.delete(`refs/heads/${branch}`);
    repo.generation += 1;
  }

  /** Create a commit on a branch with a new tree, returning the commit oid. */
  commitTree(repo: Repo, branch: string, treeOid: string, message: string): string {
    const parent = this.branchTip(repo, branch);
    const identity = serverIdentity();
    const commit = encodeCommit({
      tree: treeOid,
      parents: parent === null ? [] : [parent],
      author: identity,
      committer: identity,
      message,
    });
    const oid = putObject(repo, 'commit', commit);
    this.setBranch(repo, branch, oid);
    return oid;
  }

  /** Read a file's blob at a branch tip, or null when absent/unborn. */
  readFile(repo: Repo, branch: string, path: string): { oid: string; mode: string; data: Buffer } | null {
    const tip = this.branchTip(repo, branch);
    if (tip === null) return null;
    const tipObj = repo.objects.get(tip);
    if (tipObj === undefined || tipObj.type !== 'commit') return null;
    const tree = parseCommit(tipObj.data).tree;
    const entry = readTreePath(repo, tree, splitPath(path));
    if (entry === null) return null;
    const blob = repo.objects.get(entry.oid);
    if (blob === undefined || blob.type !== 'blob') return null;
    return { oid: entry.oid, mode: entry.mode, data: blob.data };
  }

  /**
   * Write a file on a branch (create or update), creating the branch from the empty tree when
   * it is unborn. Returns the new commit oid and the blob oid.
   */
  writeFile(repo: Repo, branch: string, path: string, data: Buffer, message: string): { commitSha: string; blobSha: string } {
    const blobSha = putObject(repo, 'blob', data);
    const tip = this.branchTip(repo, branch);
    let treeOid = EMPTY_TREE_OID;
    if (tip !== null) {
      const tipObj = repo.objects.get(tip);
      if (tipObj !== undefined && tipObj.type === 'commit') treeOid = parseCommit(tipObj.data).tree;
    }
    const newTree = setTreePath(repo, treeOid, splitPath(path), { oid: blobSha, mode: '100644' });
    const commitSha = this.commitTree(repo, branch, newTree, message);
    return { commitSha, blobSha };
  }

  /** Delete a file on a branch. Returns the new commit oid, or null when the file is absent. */
  deleteFile(repo: Repo, branch: string, path: string, message: string): string | null {
    const tip = this.branchTip(repo, branch);
    if (tip === null) return null;
    const tipObj = repo.objects.get(tip);
    if (tipObj === undefined || tipObj.type !== 'commit') return null;
    const tree = parseCommit(tipObj.data).tree;
    if (readTreePath(repo, tree, splitPath(path)) === null) return null;
    const newTree = setTreePath(repo, tree, splitPath(path), null);
    return this.commitTree(repo, branch, newTree, message);
  }

  createIssue(repo: Repo, title: string, body: string, labels: string[]): Issue {
    const issue: Issue = {
      number: repo.nextIssue,
      title,
      body,
      state: 'open',
      labels: labels.slice(),
      comments: [],
      nextCommentId: 1,
    };
    repo.nextIssue += 1;
    repo.issues.set(issue.number, issue);
    return issue;
  }

  createPull(repo: Repo, title: string, body: string, headRef: string, baseRef: string, headSha: string, baseSha: string): Pull {
    const headObj = repo.objects.get(headSha);
    const baseObj = repo.objects.get(baseSha);
    const headTree = headObj !== undefined && headObj.type === 'commit' ? parseCommit(headObj.data).tree : EMPTY_TREE_OID;
    const baseTree = baseObj !== undefined && baseObj.type === 'commit' ? parseCommit(baseObj.data).tree : EMPTY_TREE_OID;
    const pull: Pull = {
      number: repo.nextPull,
      title,
      body,
      state: 'open',
      headRef,
      baseRef,
      headSha,
      baseSha,
      merged: false,
      mergedAt: null,
      mergeSha: null,
      files: diffTrees(repo, baseTree, headTree),
    };
    repo.nextPull += 1;
    repo.pulls.set(pull.number, pull);
    return pull;
  }

  /**
   * Squash-merge a pull request onto its base branch. Returns the merge commit oid, or null
   * when the merge conflicts (a file changed on both sides with different bytes).
   */
  mergePull(repo: Repo, pull: Pull): string | null {
    const baseTip = this.branchTip(repo, pull.baseRef);
    if (baseTip === null) return null;
    const baseTipObj = repo.objects.get(baseTip);
    if (baseTipObj === undefined || baseTipObj.type !== 'commit') return null;
    const baseTipTree = parseCommit(baseTipObj.data).tree;
    const baseObj = repo.objects.get(pull.baseSha);
    const headObj = repo.objects.get(pull.headSha);
    if (baseObj === undefined || headObj === undefined) return null;
    const baseTree = parseCommit(baseObj.data).tree;
    const headTree = parseCommit(headObj.data).tree;
    const baseFlat = flattenTree(repo, baseTree);
    const headFlat = flattenTree(repo, headTree);
    const tipFlat = flattenTree(repo, baseTipTree);
    // Conflict: a path changed head-vs-base AND changed tip-vs-base with different bytes.
    for (const [path, h] of headFlat) {
      const b = baseFlat.get(path);
      if (b !== undefined && b.oid === h.oid) continue;
      const t = tipFlat.get(path);
      if (t !== undefined) {
        const baseOid = b?.oid;
        if (t.oid !== baseOid && t.oid !== h.oid) return null;
      }
    }
    for (const [path, b] of baseFlat) {
      if (headFlat.has(path)) continue;
      const t = tipFlat.get(path);
      if (t !== undefined && t.oid !== b.oid) return null;
    }
    // Three-way tree merge: head's changes win, tip's other changes are kept.
    let mergedTree = baseTipTree;
    for (const [path, h] of headFlat) {
      const b = baseFlat.get(path);
      if (b !== undefined && b.oid === h.oid) continue;
      mergedTree = setTreePath(repo, mergedTree, splitPath(path), { oid: h.oid, mode: h.mode });
    }
    for (const [path] of baseFlat) {
      if (headFlat.has(path)) continue;
      mergedTree = setTreePath(repo, mergedTree, splitPath(path), null);
    }
    const identity = serverIdentity();
    const message = `${pull.title} (#${pull.number})`;
    const commit = encodeCommit({
      tree: mergedTree,
      parents: [baseTip],
      author: identity,
      committer: identity,
      message,
    });
    const mergeSha = putObject(repo, 'commit', commit);
    this.setBranch(repo, pull.baseRef, mergeSha);
    pull.state = 'closed';
    pull.merged = true;
    pull.mergedAt = trialNowIso();
    pull.mergeSha = mergeSha;
    return mergeSha;
  }
}

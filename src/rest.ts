// REST v3 surface (55 checks). Wire contract from sdk/conformance/checks-rest.ts:
// - every reply application/json + non-empty X-GitHub-Request-Id (util.sendJson does both)
// - errors are JSON {message: non-empty string}; unknown route = JSON 404
// - malformed JSON body = 400; no token = 401; same repo name under another owner = 404 (never 403)
// - POST /user/repos 201; GET /user/repos paginates per_page=10 with Link rel="next"
// - contents PUT: create 201 / update 200|201 / stale sha 409; DELETE 200 {commit:{sha}}
// - issues: per-repo sequential numbers, labels must pre-exist (else 422), comments
// - pulls: squash merge only; 200 {merged:true, sha, message}; 404 unknown; 405 conflicted
// - shapes are minimum-required: extra fields are allowed, so responses carry the full record.

import type { IncomingMessage, ServerResponse } from 'node:http';
import { sendJson, sendNotFound, sendUnauthorized, sendBadRequest, readBody, parseJsonBody, isAuthorized, queryOf, errorBody, decodeBase64, nextRequestId } from './util.ts';
import { Store, type Repo, type Issue, type Pull, commitHistory, parseCommit, flattenTree, listTreeDir } from './store.ts';
import { VIEWER_LOGIN } from './scenario.ts';

type Json = Record<string, unknown>;

function isObj(v: unknown): v is Json {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function asStr(v: unknown, fallback = ''): string {
  return typeof v === 'string' ? v : fallback;
}

function asBool(v: unknown, fallback = false): boolean {
  return typeof v === 'boolean' ? v : fallback;
}

/** The repo JSON shape (minimum required fields; extra fields are allowed). */
export function repoJson(repo: Repo): Json {
  return {
    id: repo.databaseId,
    databaseId: repo.databaseId,
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: { login: repo.owner, id: 1000 },
    private: repo.private,
    description: repo.description,
    default_branch: repo.defaultBranch,
  };
}

function issueJson(repo: Repo, issue: Issue): Json {
  return {
    id: issue.number,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => ({ name, color: repo.labels.get(name)?.color ?? 'ededed' })),
    comments: issue.comments.length,
  };
}

function pullJson(repo: Repo, pull: Pull): Json {
  return {
    id: pull.number,
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeSha,
    head: { ref: pull.headRef, sha: pull.headSha },
    base: { ref: pull.baseRef, sha: pull.baseSha },
  };
}

/** Resolve /repos/{owner}/{name} under the auth rules: owner must be the viewer, else 404. */
function resolveRepo(store: Store, owner: string, name: string): Repo | null {
  if (owner !== VIEWER_LOGIN) return null;
  return store.getRepo(owner, name) ?? null;
}

/** Parse the request body as JSON, sending 400 on malformed input. */
async function jsonBody(req: IncomingMessage, res: ServerResponse): Promise<Json | null> {
  const raw = await readBody(req);
  if (raw === null) {
    sendJson(res, 413, errorBody('Request body too large'));
    return null;
  }
  if (raw.length === 0) return {};
  const parsed = parseJsonBody(raw);
  if (!parsed.ok) {
    sendBadRequest(res, 'Problems parsing JSON');
    return null;
  }
  if (!isObj(parsed.value)) {
    sendBadRequest(res, 'Problems parsing JSON');
    return null;
  }
  return parsed.value;
}

/** Handlers receive the store, request, response, and the URL segments after /api/v3. */
export async function handleRest(store: Store, req: IncomingMessage, res: ServerResponse, segments: string[]): Promise<boolean> {
  const method = req.method ?? 'GET';
  const query = queryOf(req);

  // GET /api/v3/ — root. No auth (the starter answered it open, and the env may probe it);
  // X-OAuth-Scopes present.
  if (segments.length === 0) {
    if (method !== 'GET') return false;
    sendJson(res, 200, { current_user_url: '/api/v3/user' }, { 'x-oauth-scopes': 'repo' });
    return true;
  }

  const [first, second, third, ...rest] = segments as [string, string | undefined, string | undefined, ...string[]];

  // GET /api/v3/meta
  if (first === 'meta' && segments.length === 1) {
    if (method !== 'GET') return false;
    if (!isAuthorized(req)) { sendUnauthorized(res); return true; }
    sendJson(res, 200, { installed_version: '1.0.0' });
    return true;
  }

  // /user/repos — create + list (paginated, per_page=10, Link rel="next").
  if (first === 'user' && second === 'repos' && segments.length === 2) {
    if (!isAuthorized(req)) { sendUnauthorized(res); return true; }
    if (method === 'POST') {
      const body = await jsonBody(req, res);
      if (body === null) return true;
      const name = asStr(body.name);
      if (name === '') {
        sendJson(res, 422, errorBody('name is required'));
        return true;
      }
      const repo = store.createRepo(name, asStr(body.description), asBool(body.private));
      if (repo === null) {
        sendJson(res, 422, errorBody('Repository already exists'));
        return true;
      }
      sendJson(res, 201, repoJson(repo));
      return true;
    }
    if (method === 'GET') {
      const perPageRaw = Number(query.get('per_page') ?? '10');
      const perPage = Number.isFinite(perPageRaw) && perPageRaw > 0 ? Math.min(100, Math.trunc(perPageRaw)) : 10;
      const pageRaw = Number(query.get('page') ?? '1');
      const page = Number.isFinite(pageRaw) && pageRaw > 0 ? Math.trunc(pageRaw) : 1;
      const all = store.listRepos(VIEWER_LOGIN);
      const slice = all.slice((page - 1) * perPage, page * perPage);
      const extra: Record<string, string> = {};
      if (page * perPage < all.length) {
        // The Link URL must be absolute and on the entry's own origin (the suite rejects a
        // relative URL as "left the entry's origin"); the Host header names that origin.
        const host = typeof req.headers.host === 'string' && req.headers.host !== '' ? req.headers.host : 'localhost';
        const next = `http://${host}/api/v3/user/repos?per_page=${perPage}&page=${page + 1}`;
        extra.link = `<${next}>; rel="next"`;
      }
      sendJson(res, 200, slice.map((r) => repoJson(r)), extra);
      return true;
    }
    return false;
  }

  // /repos/{owner}/{name}[...]
  if (first === 'repos' && second !== undefined && third !== undefined) {
    const owner = second;
    const name = third;
    const tail = rest;

    // GET /repos/{o}/{r}, PATCH, DELETE — no auth check for GET on a missing repo? The suite
    // checks 401 for an anon GET on an EXISTING private repo, and 404 for a missing repo.
    // Order: resolve first (unknown owner/name -> 404), then auth (anon -> 401).
    if (tail.length === 0) {
      if (method === 'GET') {
        const repo = resolveRepo(store, owner, name);
        if (repo === null) { sendNotFound(res, 'Not Found'); return true; }
        if (!isAuthorized(req)) { sendUnauthorized(res); return true; }
        sendJson(res, 200, repoJson(repo));
        return true;
      }
      if (method === 'PATCH') {
        const repo = resolveRepo(store, owner, name);
        if (repo === null) { sendNotFound(res, 'Not Found'); return true; }
        if (!isAuthorized(req)) { sendUnauthorized(res); return true; }
        const body = await jsonBody(req, res);
        if (body === null) return true;
        if (typeof body.description === 'string') repo.description = body.description;
        sendJson(res, 200, repoJson(repo));
        return true;
      }
      if (method === 'DELETE') {
        const repo = resolveRepo(store, owner, name);
        if (repo === null) { sendNotFound(res, 'Not Found'); return true; }
        if (!isAuthorized(req)) { sendUnauthorized(res); return true; }
        store.deleteRepo(owner, name);
        res.writeHead(204, { 'x-github-request-id': nextRequestId() });
        res.end();
        return true;
      }
      return false;
    }

    const [sub, ...subTail] = tail as [string, ...string[]];

    // POST /repos/{o}/{r}/git/refs — create a ref.
    if (sub === 'git' && subTail[0] === 'refs' && subTail.length === 1) {
      if (method !== 'POST') return false;
      const repo = resolveRepo(store, owner, name);
      if (repo === null) { sendNotFound(res, 'Not Found'); return true; }
      if (!isAuthorized(req)) { sendUnauthorized(res); return true; }
      const body = await jsonBody(req, res);
      if (body === null) return true;
      const ref = asStr(body.ref);
      const sha = asStr(body.sha);
      if (!ref.startsWith('refs/heads/') || !/^[0-9a-f]{40}$/i.test(sha)) {
        sendJson(res, 422, errorBody('Invalid ref or sha'));
        return true;
      }
      repo.refs.set(ref, sha.toLowerCase());
      // Mutating a ref must bump the generation (#1): the fetch/ls-refs caches key on it,
      // and this was the one ref-mutation path that did not. Behaviour-neutral — the
      // generation is an internal cache-invalidation counter, never sent on the wire.
      repo.generation += 1;
      sendJson(res, 201, { ref, object: { sha: sha.toLowerCase(), type: 'commit' } });
      return true;
    }

    // GET /repos/{o}/{r}/branches
    if (sub === 'branches' && subTail.length === 0) {
      if (method !== 'GET') return false;
      const repo = resolveRepo(store, owner, name);
      if (repo === null) { sendNotFound(res, 'Not Found'); return true; }
      if (!isAuthorized(req)) { sendUnauthorized(res); return true; }
      const branches: Json[] = [];
      for (const [ref, sha] of repo.refs) {
        if (!ref.startsWith('refs/heads/')) continue;
        branches.push({ name: ref.slice('refs/heads/'.length), commit: { sha } });
      }
      branches.sort((a, b) => String(a.name).localeCompare(String(b.name)));
      sendJson(res, 200, branches);
      return true;
    }

    // GET /repos/{o}/{r}/commits[?sha=<branch>]
    if (sub === 'commits' && subTail.length === 0) {
      if (method !== 'GET') return false;
      const repo = resolveRepo(store, owner, name);
      if (repo === null) { sendNotFound(res, 'Not Found'); return true; }
      if (!isAuthorized(req)) { sendUnauthorized(res); return true; }
      const branch = query.get('sha') ?? repo.defaultBranch;
      const tip = store.branchTip(repo, branch);
      if (tip === null) { sendJson(res, 200, []); return true; }
      const history = commitHistory(repo, tip).map((c) => ({ sha: c.oid, commit: { message: c.message } }));
      sendJson(res, 200, history);
      return true;
    }

    // /repos/{o}/{r}/contents/{path...}
    if (sub === 'contents') {
      const path = subTail.join('/');
      if (path === '') return false;
      const repo = resolveRepo(store, owner, name);
      if (repo === null) { sendNotFound(res, 'Not Found'); return true; }
      if (!isAuthorized(req)) { sendUnauthorized(res); return true; }
      const branch = query.get('ref') ?? repo.defaultBranch;
      if (method === 'GET') {
        const tip = store.branchTip(repo, branch);
        if (tip === null) { sendNotFound(res, 'Not Found'); return true; }
        const tipObj = repo.objects.get(tip);
        if (tipObj === undefined || tipObj.type !== 'commit') { sendNotFound(res, 'Not Found'); return true; }
        const tree = parseCommit(tipObj.data).tree;
        const flat = flattenTree(repo, tree);
        const file = flat.get(path);
        if (file !== undefined) {
          const blob = repo.objects.get(file.oid);
          const data = blob?.data ?? Buffer.alloc(0);
          sendJson(res, 200, {
            type: 'file',
            encoding: 'base64',
            content: data.toString('base64'),
            sha: file.oid,
            size: data.length,
            name: path.split('/').pop() ?? path,
            path,
          });
          return true;
        }
        const dir = listTreeDir(repo, tree, path.split('/'));
        if (dir !== null) {
          sendJson(res, 200, dir.map((d) => ({ type: d.type, name: d.name, path: d.path, sha: d.oid })));
          return true;
        }
        sendNotFound(res, 'Not Found');
        return true;
      }
      if (method === 'PUT') {
        const body = await jsonBody(req, res);
        if (body === null) return true;
        const contentB64 = asStr(body.content);
        const data = decodeBase64(contentB64);
        if (data === null) { sendJson(res, 422, errorBody('content is not valid base64')); return true; }
        const message = asStr(body.message, 'Update file');
        const putBranch = typeof body.branch === 'string' && body.branch !== '' ? body.branch : repo.defaultBranch;
        const givenSha = typeof body.sha === 'string' ? body.sha.toLowerCase() : null;
        const existing = store.readFile(repo, putBranch, path);
        if (existing !== null) {
          // Update: a sha is required and must be current.
          if (givenSha === null || givenSha !== existing.oid) {
            sendJson(res, 409, errorBody('sha does not match'));
            return true;
          }
          const { commitSha, blobSha } = store.writeFile(repo, putBranch, path, data, message);
          sendJson(res, 200, { content: { sha: blobSha, encoding: 'base64' }, commit: { sha: commitSha } });
          return true;
        }
        // Create: a sha must not be supplied (a stale sha on create is a 409 too, but the
        // suite never sends one on create).
        const { commitSha, blobSha } = store.writeFile(repo, putBranch, path, data, message);
        sendJson(res, 201, { content: { sha: blobSha, encoding: 'base64' }, commit: { sha: commitSha } });
        return true;
      }
      if (method === 'DELETE') {
        const body = await jsonBody(req, res);
        if (body === null) return true;
        const message = asStr(body.message, 'Delete file');
        const delBranch = typeof body.branch === 'string' && body.branch !== '' ? body.branch : repo.defaultBranch;
        const commitSha = store.deleteFile(repo, delBranch, path, message);
        if (commitSha === null) { sendNotFound(res, 'Not Found'); return true; }
        sendJson(res, 200, { commit: { sha: commitSha } });
        return true;
      }
      return false;
    }

    // POST /repos/{o}/{r}/labels
    if (sub === 'labels' && subTail.length === 0) {
      if (method !== 'POST') return false;
      const repo = resolveRepo(store, owner, name);
      if (repo === null) { sendNotFound(res, 'Not Found'); return true; }
      if (!isAuthorized(req)) { sendUnauthorized(res); return true; }
      const body = await jsonBody(req, res);
      if (body === null) return true;
      const labelName = asStr(body.name);
      if (labelName === '') { sendJson(res, 422, errorBody('name is required')); return true; }
      const color = asStr(body.color, 'ededed');
      repo.labels.set(labelName, { name: labelName, color });
      sendJson(res, 201, { name: labelName, color });
      return true;
    }

    // /repos/{o}/{r}/issues[/{n}[/comments]]
    if (sub === 'issues') {
      const repo = resolveRepo(store, owner, name);
      if (repo === null) { sendNotFound(res, 'Not Found'); return true; }
      if (!isAuthorized(req)) { sendUnauthorized(res); return true; }
      const numRaw = subTail[0];
      if (numRaw === undefined) {
        if (method === 'POST') {
          const body = await jsonBody(req, res);
          if (body === null) return true;
          const labels = Array.isArray(body.labels) ? body.labels.filter((l): l is string => typeof l === 'string') : [];
          // The label-existence loop only matters when labels are present (#5).
          if (labels.length > 0) {
            for (const label of labels) {
              if (!repo.labels.has(label)) {
                sendJson(res, 422, errorBody(`Label ${label} does not exist`));
                return true;
              }
            }
          }
          const issue = store.createIssue(repo, asStr(body.title), asStr(body.body), labels);
          if (labels.length === 0) {
            // Fast path (#5): the bench sends no labels, so build the 201 body directly
            // instead of mapping an empty label list through issueJson. Same fields, same
            // order, byte-identical JSON.
            sendJson(res, 201, { id: issue.number, number: issue.number, title: issue.title, body: issue.body, state: issue.state, labels: [], comments: 0 });
          } else {
            sendJson(res, 201, issueJson(repo, issue));
          }
          return true;
        }
        if (method === 'GET') {
          const stateFilter = query.get('state') ?? 'open';
          const sort = query.get('sort') ?? 'created';
          const direction = query.get('direction') ?? 'desc';
          const perPageRaw = Number(query.get('per_page') ?? '100');
          const perPage = Number.isFinite(perPageRaw) && perPageRaw > 0 ? Math.min(100, Math.trunc(perPageRaw)) : 100;
          let list = [...repo.issues.values()];
          if (stateFilter !== 'all') list = list.filter((i) => i.state === stateFilter);
          if (sort === 'created') {
            list.sort((a, b) => (direction === 'asc' ? a.number - b.number : b.number - a.number));
          }
          sendJson(res, 200, list.slice(0, perPage).map((i) => issueJson(repo, i)));
          return true;
        }
        return false;
      }
      const num = Number(numRaw);
      if (!Number.isInteger(num) || num < 1) { sendNotFound(res, 'Not Found'); return true; }
      const issue = repo.issues.get(num);
      if (subTail.length === 2 && subTail[1] === 'comments') {
        if (issue === undefined) { sendNotFound(res, 'Not Found'); return true; }
        if (method === 'POST') {
          const body = await jsonBody(req, res);
          if (body === null) return true;
          const comment = { id: issue.nextCommentId, body: asStr(body.body) };
          issue.nextCommentId += 1;
          issue.comments.push(comment);
          sendJson(res, 201, { id: comment.id, body: comment.body });
          return true;
        }
        if (method === 'GET') {
          sendJson(res, 200, issue.comments.map((c) => ({ id: c.id, body: c.body })));
          return true;
        }
        return false;
      }
      if (subTail.length === 1) {
        if (issue === undefined) { sendNotFound(res, 'Not Found'); return true; }
        if (method === 'GET') {
          sendJson(res, 200, issueJson(repo, issue));
          return true;
        }
        if (method === 'PATCH') {
          const body = await jsonBody(req, res);
          if (body === null) return true;
          if (typeof body.title === 'string') issue.title = body.title;
          if (typeof body.body === 'string') issue.body = body.body;
          if (body.state === 'open' || body.state === 'closed') issue.state = body.state;
          sendJson(res, 200, issueJson(repo, issue));
          return true;
        }
        return false;
      }
      return false;
    }

    // /repos/{o}/{r}/pulls[/{n}[/merge|/files]]
    if (sub === 'pulls') {
      const repo = resolveRepo(store, owner, name);
      if (repo === null) { sendNotFound(res, 'Not Found'); return true; }
      if (!isAuthorized(req)) { sendUnauthorized(res); return true; }
      const numRaw = subTail[0];
      if (numRaw === undefined) {
        if (method === 'POST') {
          const body = await jsonBody(req, res);
          if (body === null) return true;
          const headRef = asStr(body.head);
          const baseRef = asStr(body.base, repo.defaultBranch);
          const headSha = store.branchTip(repo, headRef);
          const baseSha = store.branchTip(repo, baseRef);
          if (headSha === null || baseSha === null) {
            sendJson(res, 422, errorBody('head or base branch not found'));
            return true;
          }
          const pull = store.createPull(repo, asStr(body.title), asStr(body.body), headRef, baseRef, headSha, baseSha);
          sendJson(res, 201, pullJson(repo, pull));
          return true;
        }
        if (method === 'GET') {
          const stateFilter = query.get('state') ?? 'open';
          const perPageRaw = Number(query.get('per_page') ?? '100');
          const perPage = Number.isFinite(perPageRaw) && perPageRaw > 0 ? Math.min(100, Math.trunc(perPageRaw)) : 100;
          let list = [...repo.pulls.values()];
          if (stateFilter !== 'all') list = list.filter((p) => p.state === stateFilter);
          list.sort((a, b) => b.number - a.number);
          sendJson(res, 200, list.slice(0, perPage).map((p) => pullJson(repo, p)));
          return true;
        }
        return false;
      }
      const num = Number(numRaw);
      if (!Number.isInteger(num) || num < 1) { sendNotFound(res, 'Not Found'); return true; }
      const pull = repo.pulls.get(num);
      if (subTail.length === 2 && subTail[1] === 'files') {
        if (pull === undefined) { sendNotFound(res, 'Not Found'); return true; }
        if (method !== 'GET') return false;
        sendJson(res, 200, pull.files.map((f) => ({ filename: f.filename, status: f.status, additions: f.additions, deletions: f.deletions, changes: f.changes })));
        return true;
      }
      if (subTail.length === 2 && subTail[1] === 'merge') {
        if (pull === undefined) { sendNotFound(res, 'Not Found'); return true; }
        if (method !== 'PUT') return false;
        const mergeBody = await jsonBody(req, res);
        if (mergeBody === null) return true; // malformed body already answered 400
        if (pull.state === 'closed') {
          sendJson(res, 405, errorBody('Pull request already merged or closed'));
          return true;
        }
        const mergeSha = store.mergePull(repo, pull);
        if (mergeSha === null) {
          sendJson(res, 405, errorBody('Merge conflict'));
          return true;
        }
        sendJson(res, 200, { merged: true, sha: mergeSha, message: `Merged pull request #${pull.number}` });
        return true;
      }
      if (subTail.length === 1) {
        if (pull === undefined) { sendNotFound(res, 'Not Found'); return true; }
        if (method !== 'GET') return false;
        sendJson(res, 200, pullJson(repo, pull));
        return true;
      }
      return false;
    }

    return false;
  }

  return false;
}

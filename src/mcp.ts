// MCP surface (15 checks) at POST /mcp: JSON-RPC 2.0 over Streamable HTTP, answered as plain
// application/json (the client accepts JSON or SSE). Two eras are supported:
// - modern 2026-07-28: `server/discover` answers {supportedVersions: [...], capabilities,
//   serverInfo} — the client negotiates 2026-07-28 when it is listed.
// - handshake <=2025-11-25: `initialize` (any of 2025-11-25, 2025-06-18, 2025-03-26, 2024-11-05;
//   the requested revision is echoed) + `notifications/initialized` (202, no body).
//
// Tools (six, the github-mcp-server set): create_repo, get_repo, create_issue, list_issues,
// create_pull_request, merge_pull_request. Tool results carry BOTH a text block (JSON) and
// structuredContent, so the suite's result reader finds the object either way. Unknown tool =
// JSON-RPC -32601; missing argument = a tool result with isError:true (never HTTP 500).
// tools/list is a constant, so two calls serialize byte-identically. No auth = HTTP 401.

import type { IncomingMessage, ServerResponse } from 'node:http';
import { sendJson, readBody, parseJsonBody, isAuthorized, errorBody } from './util.ts';
import { Store, type Repo, type Issue, type Pull } from './store.ts';
import { repoJson } from './rest.ts';
import { VIEWER_LOGIN } from './scenario.ts';

const SERVER_NAME = 'github-core-clone';
const SERVER_VERSION = '1.0.0';

const SUPPORTED_VERSIONS = ['2026-07-28', '2025-11-25'];
const LEGACY_VERSIONS = ['2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];

interface ToolDef {
  name: string;
  description: string;
  inputSchema: { type: string; properties: Record<string, { type: string; description: string }>; required: string[] };
}

// The tool list is a module constant: two tools/list calls serialize byte-identically.
const TOOLS: ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a new repository owned by the authenticated user.',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string', description: 'The repository name.' },
        description: { type: 'string', description: 'A short description of the repository.' },
        private: { type: 'boolean', description: 'Whether the repository is private.' },
      },
      required: ['name'],
    },
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        title: { type: 'string', description: 'The issue title.' },
        body: { type: 'string', description: 'The issue body.' },
      },
      required: ['owner', 'repo', 'title'],
    },
  },
  {
    name: 'list_issues',
    description: 'List issues in a repository, optionally filtered by state.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        state: { type: 'string', description: 'Issue state filter: open, closed, or all.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_pull_request',
    description: 'Create a pull request from a head branch into a base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        title: { type: 'string', description: 'The pull request title.' },
        body: { type: 'string', description: 'The pull request body.' },
        head: { type: 'string', description: 'The head branch name.' },
        base: { type: 'string', description: 'The base branch name.' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request with a squash merge.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        pull_number: { type: 'number', description: 'The pull request number.' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  },
];

function isObj(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function argStr(args: Record<string, unknown>, name: string): string | null {
  const v = args[name];
  return typeof v === 'string' ? v : null;
}

function argNum(args: Record<string, unknown>, name: string): number | null {
  const v = args[name];
  return typeof v === 'number' && Number.isFinite(v) ? v : null;
}

function toolResult(data: unknown): Record<string, unknown> {
  const text = typeof data === 'string' ? data : JSON.stringify(data);
  return { content: [{ type: 'text', text }], structuredContent: data };
}

function toolError(message: string): Record<string, unknown> {
  return { content: [{ type: 'text', text: message }], isError: true };
}

function issueJson(repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    id: issue.number,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => ({ name })),
  };
}

function pullJson(pull: Pull): Record<string, unknown> {
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

function resolveRepo(store: Store, owner: string | null, name: string | null): Repo | null {
  if (owner === null || name === null) return null;
  if (owner !== VIEWER_LOGIN) return null;
  return store.getRepo(owner, name) ?? null;
}

/** Execute one tool call. Returns {result} or {error} (a tool-level error, isError:true). */
function callTool(store: Store, name: string, args: Record<string, unknown>): { result: Record<string, unknown> } | { error: string } {
  const normalized = name.toLowerCase().replace(/[^a-z0-9_]/g, '');
  const tool = TOOLS.find((t) => normalized === t.name || normalized.endsWith(t.name));
  if (tool === undefined) return { error: `Unknown tool: ${name}` };

  switch (tool.name) {
    case 'create_repo': {
      const repoName = argStr(args, 'name');
      if (repoName === null || repoName === '') return { error: 'Missing required argument: name' };
      const description = argStr(args, 'description') ?? '';
      const isPrivate = typeof args.private === 'boolean' ? args.private : false;
      const repo = store.createRepo(repoName, description, isPrivate);
      if (repo === null) return { error: `Repository ${repoName} already exists` };
      return { result: toolResult(repoJson(repo)) };
    }
    case 'get_repo': {
      const repo = resolveRepo(store, argStr(args, 'owner'), argStr(args, 'repo'));
      if (repo === null) return { error: 'Repository not found' };
      return { result: toolResult(repoJson(repo)) };
    }
    case 'create_issue': {
      const repo = resolveRepo(store, argStr(args, 'owner'), argStr(args, 'repo'));
      if (repo === null) return { error: 'Repository not found' };
      const title = argStr(args, 'title');
      if (title === null) return { error: 'Missing required argument: title' };
      const issue = store.createIssue(repo, title, argStr(args, 'body') ?? '', []);
      return { result: toolResult(issueJson(repo, issue)) };
    }
    case 'list_issues': {
      const repo = resolveRepo(store, argStr(args, 'owner'), argStr(args, 'repo'));
      if (repo === null) return { error: 'Repository not found' };
      const state = argStr(args, 'state') ?? 'open';
      let list = [...repo.issues.values()];
      if (state !== 'all') list = list.filter((i) => i.state === state);
      list.sort((a, b) => b.number - a.number);
      return { result: toolResult(list.map((i) => issueJson(repo, i))) };
    }
    case 'create_pull_request': {
      const repo = resolveRepo(store, argStr(args, 'owner'), argStr(args, 'repo'));
      if (repo === null) return { error: 'Repository not found' };
      const title = argStr(args, 'title');
      const head = argStr(args, 'head');
      const base = argStr(args, 'base');
      if (title === null) return { error: 'Missing required argument: title' };
      if (head === null) return { error: 'Missing required argument: head' };
      if (base === null) return { error: 'Missing required argument: base' };
      const headSha = store.branchTip(repo, head);
      const baseSha = store.branchTip(repo, base);
      if (headSha === null || baseSha === null) return { error: 'Head or base branch not found' };
      const pull = store.createPull(repo, title, argStr(args, 'body') ?? '', head, base, headSha, baseSha);
      return { result: toolResult(pullJson(pull)) };
    }
    case 'merge_pull_request': {
      const repo = resolveRepo(store, argStr(args, 'owner'), argStr(args, 'repo'));
      if (repo === null) return { error: 'Repository not found' };
      const pullNumber = argNum(args, 'pull_number');
      if (pullNumber === null) return { error: 'Missing required argument: pull_number' };
      const pull = repo.pulls.get(Math.trunc(pullNumber));
      if (pull === undefined) return { error: `Pull request #${pullNumber} not found` };
      if (pull.state === 'closed') return { error: 'Pull request already merged or closed' };
      const mergeSha = store.mergePull(repo, pull);
      if (mergeSha === null) return { error: 'Merge conflict' };
      return { result: toolResult({ merged: true, sha: mergeSha, message: `Merged pull request #${pull.number}`, ...pullJson(pull) }) };
    }
    default:
      return { error: `Unknown tool: ${name}` };
  }
}

interface RpcError {
  code: number;
  message: string;
}

/** Handle one JSON-RPC request object. Returns the response object, or null for a notification. */
function handleRpc(store: Store, req: unknown): Record<string, unknown> | null {
  if (!isObj(req) || req.jsonrpc !== '2.0' || typeof req.method !== 'string') {
    return { jsonrpc: '2.0', id: isObj(req) && req.id !== undefined ? req.id : null, error: { code: -32600, message: 'Invalid Request' } };
  }
  const id = req.id;
  const isNotification = id === undefined;
  const method = req.method as string;
  const params = isObj(req.params) ? req.params : {};

  const respond = (result: unknown): Record<string, unknown> | null => {
    if (isNotification) return null;
    return { jsonrpc: '2.0', id: id ?? null, result };
  };
  const respondError = (error: RpcError): Record<string, unknown> | null => {
    if (isNotification) return null;
    return { jsonrpc: '2.0', id: id ?? null, error };
  };

  if (method.startsWith('notifications/')) {
    return null; // notifications are acknowledged with HTTP 202 and no body (handled by caller)
  }

  if (method === 'server/discover') {
    return respond({
      supportedVersions: SUPPORTED_VERSIONS,
      capabilities: { tools: {} },
      serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
      _meta: { 'io.modelcontextprotocol/serverInfo': { name: SERVER_NAME, version: SERVER_VERSION } },
    });
  }

  if (method === 'initialize') {
    const requested = typeof params.protocolVersion === 'string' ? params.protocolVersion : null;
    const negotiated = requested !== null && (SUPPORTED_VERSIONS.includes(requested) || LEGACY_VERSIONS.includes(requested))
      ? requested
      : '2025-11-25';
    return respond({
      protocolVersion: negotiated,
      capabilities: { tools: {} },
      serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
    });
  }

  if (method === 'tools/list') {
    return respond({ tools: TOOLS });
  }

  if (method === 'tools/call') {
    const toolName = typeof params.name === 'string' ? params.name : null;
    const args = isObj(params.arguments) ? params.arguments : {};
    if (toolName === null || toolName === '') {
      return respondError({ code: -32602, message: 'Missing required argument: name' });
    }
    const outcome = callTool(store, toolName, args);
    if ('error' in outcome) {
      // An unknown tool is a JSON-RPC error; a known tool's failure is a tool result.
      if (outcome.error.startsWith('Unknown tool:')) {
        return respondError({ code: -32601, message: outcome.error });
      }
      return respond(toolError(outcome.error));
    }
    return respond(outcome.result);
  }

  return respondError({ code: -32601, message: `Method not found: ${method}` });
}

/** The HTTP handler for POST /mcp. Returns true when it handled the request. */
export async function handleMcp(store: Store, req: IncomingMessage, res: ServerResponse): Promise<boolean> {
  if (req.method !== 'POST') return false;
  if (!isAuthorized(req)) {
    sendJson(res, 401, errorBody('Requires authentication'));
    return true;
  }
  const raw = await readBody(req);
  if (raw === null) {
    sendJson(res, 413, errorBody('Request body too large'));
    return true;
  }
  const parsed = parseJsonBody(raw);
  if (!parsed.ok) {
    // Non-JSON body: HTTP 400 + JSON-RPC parse error -32700.
    sendJson(res, 400, { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } });
    return true;
  }
  const body = parsed.value;
  // A JSON-RPC batch (array) is answered with an array of responses.
  if (Array.isArray(body)) {
    const responses: Record<string, unknown>[] = [];
    for (const item of body) {
      const r = handleRpc(store, item);
      if (r !== null) responses.push(r);
    }
    if (responses.length === 0) {
      res.writeHead(202, { 'content-length': '0' });
      res.end();
      return true;
    }
    sendJson(res, 200, responses);
    return true;
  }
  const response = handleRpc(store, body);
  if (response === null) {
    // A notification: HTTP 202, no body.
    res.writeHead(202, { 'content-length': '0' });
    res.end();
    return true;
  }
  sendJson(res, 200, response);
  return true;
}

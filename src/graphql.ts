// GraphQL surface (20 checks) at POST /api/graphql. A small hand-rolled parser + executor for
// the documents the suite sends (sdk/conformance/checks-graphql.ts): queries and mutations with
// variables, inline input objects, inline enum lists, aliases, inline fragments, batched arrays,
// and __type introspection. No external library — the entry is stdlib-only.
//
// Wire rules: unauthenticated = exactly 401; every other outcome is HTTP 200 with {data, errors}
// (an unknown field is a 200 with a non-empty errors array whose message names the field);
// an unknown repository is data.repository = null with NO errors; a JSON-array body returns a
// JSON-array response in the same order. Global ids are opaque (base64url of a typed key) and
// stable across reads; databaseId is a positive int and never equals the global id.

import type { IncomingMessage, ServerResponse } from 'node:http';
import { sendJson, sendUnauthorized, readBody, parseJsonBody, isAuthorized, errorBody, b64url } from './util.ts';
import { Store, type Repo, type Issue, type Pull, repoGlobalId, issueGlobalId, pullGlobalId, commentGlobalId } from './store.ts';
import { VIEWER_LOGIN } from './scenario.ts';

// ------------------------------------------------------------------ tokenizer

type TokenKind = 'name' | 'string' | 'int' | 'punct' | 'eof';
interface Token { kind: TokenKind; value: string; }

function tokenize(src: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  const n = src.length;
  while (i < n) {
    const c = src[i] as string;
    if (c === ' ' || c === '\t' || c === '\n' || c === '\r' || c === ',') { i += 1; continue; }
    if (c === '#') { while (i < n && src[i] !== '\n') i += 1; continue; }
    if (c === '"') {
      // String (the suite uses plain "..." strings; block strings are not needed).
      let j = i + 1;
      let out = '';
      while (j < n && src[j] !== '"') {
        if (src[j] === '\\' && j + 1 < n) {
          const esc = src[j + 1] as string;
          if (esc === 'n') out += '\n';
          else if (esc === 't') out += '\t';
          else if (esc === 'r') out += '\r';
          else out += esc;
          j += 2;
        } else {
          out += src[j] as string;
          j += 1;
        }
      }
      tokens.push({ kind: 'string', value: out });
      i = j + 1;
      continue;
    }
    if (c === '-' || (c >= '0' && c <= '9')) {
      let j = i;
      if (src[j] === '-') j += 1;
      while (j < n && src[j] !== undefined && /[0-9.]/.test(src[j] as string)) j += 1;
      tokens.push({ kind: 'int', value: src.slice(i, j) });
      i = j;
      continue;
    }
    if (/[A-Za-z_]/.test(c)) {
      let j = i;
      while (j < n && src[j] !== undefined && /[A-Za-z0-9_]/.test(src[j] as string)) j += 1;
      tokens.push({ kind: 'name', value: src.slice(i, j) });
      i = j;
      continue;
    }
    if (c === '.' && src[i + 1] === '.' && src[i + 2] === '.') {
      tokens.push({ kind: 'punct', value: '...' });
      i += 3;
      continue;
    }
    if ('{}()[]:!=$@'.includes(c)) {
      tokens.push({ kind: 'punct', value: c });
      i += 1;
      continue;
    }
    // Unknown character: skip it rather than failing the whole document.
    i += 1;
  }
  tokens.push({ kind: 'eof', value: '' });
  return tokens;
}

// ------------------------------------------------------------------ AST

type ValueNode =
  | { kind: 'variable'; name: string }
  | { kind: 'int'; value: number }
  | { kind: 'string'; value: string }
  | { kind: 'boolean'; value: boolean }
  | { kind: 'null' }
  | { kind: 'enum'; value: string }
  | { kind: 'list'; values: ValueNode[] }
  | { kind: 'object'; fields: { name: string; value: ValueNode }[] };

interface FieldNode {
  kind: 'field';
  name: string;
  alias: string | null;
  args: { name: string; value: ValueNode }[];
  selection: SelectionNode[];
}
interface FragmentSpreadNode { kind: 'spread'; name: string; }
interface InlineFragmentNode { kind: 'inline'; typeCondition: string | null; selection: SelectionNode[]; }
type SelectionNode = FieldNode | FragmentSpreadNode | InlineFragmentNode;

interface OperationNode {
  kind: 'operation';
  operation: 'query' | 'mutation';
  name: string | null;
  variables: { name: string; type: string }[];
  selection: SelectionNode[];
}
interface FragmentNode {
  kind: 'fragment';
  name: string;
  typeCondition: string;
  selection: SelectionNode[];
}
type DefinitionNode = OperationNode | FragmentNode;

class Parser {
  private tokens: Token[];
  private pos = 0;

  constructor(src: string) {
    this.tokens = tokenize(src);
  }

  private peek(): Token { return this.tokens[this.pos] as Token; }
  private next(): Token { const t = this.tokens[this.pos] as Token; this.pos += 1; return t; }
  private expect(value: string): Token {
    const t = this.next();
    if (t.value !== value && t.kind !== 'eof') throw new Error(`expected ${value}, got ${t.value}`);
    return t;
  }
  private expectName(): string {
    const t = this.next();
    return t.value;
  }

  parseDocument(): DefinitionNode[] {
    const defs: DefinitionNode[] = [];
    while (this.peek().kind !== 'eof') {
      defs.push(this.parseDefinition());
    }
    return defs;
  }

  private parseDefinition(): DefinitionNode {
    const t = this.peek();
    if (t.kind === 'punct' && t.value === '{') {
      return { kind: 'operation', operation: 'query', name: null, variables: [], selection: this.parseSelectionSet() };
    }
    if (t.kind === 'name' && t.value === 'fragment') {
      this.next();
      const name = this.expectName();
      this.expect('on');
      const typeCondition = this.expectName();
      return { kind: 'fragment', name, typeCondition, selection: this.parseSelectionSet() };
    }
    const op = this.next().value as 'query' | 'mutation';
    let name: string | null = null;
    if (this.peek().kind === 'name') name = this.next().value;
    const variables: { name: string; type: string }[] = [];
    if (this.peek().value === '(') {
      this.next();
      while (this.peek().value !== ')') {
        this.expect('$');
        const varName = this.expectName();
        this.expect(':');
        const type = this.parseType();
        if (this.peek().value === '=') { this.next(); this.parseValue(); }
        variables.push({ name: varName, type });
      }
      this.expect(')');
    }
    return { kind: 'operation', operation: op, name, variables, selection: this.parseSelectionSet() };
  }

  private parseType(): string {
    let type: string;
    if (this.peek().value === '[') {
      this.next();
      type = `[${this.parseType()}]`;
      this.expect(']');
    } else {
      type = this.expectName();
    }
    if (this.peek().value === '!') { this.next(); type += '!'; }
    return type;
  }

  private parseSelectionSet(): SelectionNode[] {
    this.expect('{');
    const selections: SelectionNode[] = [];
    while (this.peek().value !== '}') {
      if (this.peek().kind === 'eof') break;
      selections.push(this.parseSelection());
    }
    this.expect('}');
    return selections;
  }

  private parseSelection(): SelectionNode {
    if (this.peek().value === '...') {
      this.next();
      if (this.peek().value === 'on') {
        this.next();
        const typeCondition = this.expectName();
        return { kind: 'inline', typeCondition, selection: this.parseSelectionSet() };
      }
      if (this.peek().kind === 'name') {
        return { kind: 'spread', name: this.next().value };
      }
      return { kind: 'inline', typeCondition: null, selection: this.parseSelectionSet() };
    }
    const first = this.expectName();
    let alias: string | null = null;
    let name = first;
    if (this.peek().value === ':') {
      this.next();
      alias = first;
      name = this.expectName();
    }
    const args: { name: string; value: ValueNode }[] = [];
    if (this.peek().value === '(') {
      this.next();
      while (this.peek().value !== ')') {
        const argName = this.expectName();
        this.expect(':');
        args.push({ name: argName, value: this.parseValue() });
      }
      this.expect(')');
    }
    let selection: SelectionNode[] = [];
    if (this.peek().value === '{') selection = this.parseSelectionSet();
    return { kind: 'field', name, alias, args, selection };
  }

  private parseValue(): ValueNode {
    const t = this.next();
    if (t.kind === 'punct' && t.value === '$') return { kind: 'variable', name: this.expectName() };
    if (t.kind === 'punct' && t.value === '[') {
      const values: ValueNode[] = [];
      while (this.peek().value !== ']') values.push(this.parseValue());
      this.expect(']');
      return { kind: 'list', values };
    }
    if (t.kind === 'punct' && t.value === '{') {
      const fields: { name: string; value: ValueNode }[] = [];
      while (this.peek().value !== '}') {
        const name = this.expectName();
        this.expect(':');
        fields.push({ name, value: this.parseValue() });
      }
      this.expect('}');
      return { kind: 'object', fields };
    }
    if (t.kind === 'string') return { kind: 'string', value: t.value };
    if (t.kind === 'int') return { kind: 'int', value: Number(t.value) };
    if (t.kind === 'name') {
      if (t.value === 'true') return { kind: 'boolean', value: true };
      if (t.value === 'false') return { kind: 'boolean', value: false };
      if (t.value === 'null') return { kind: 'null' };
      return { kind: 'enum', value: t.value };
    }
    throw new Error(`unexpected token ${t.value}`);
  }
}

/** Resolve a value node against the operation's variables. */
function resolveValue(node: ValueNode, variables: Record<string, unknown>): unknown {
  switch (node.kind) {
    case 'variable': return variables[node.name] ?? null;
    case 'int': return node.value;
    case 'string': return node.value;
    case 'boolean': return node.value;
    case 'null': return null;
    case 'enum': return node.value;
    case 'list': return node.values.map((v) => resolveValue(v, variables));
    case 'object': {
      const out: Record<string, unknown> = {};
      for (const f of node.fields) out[f.name] = resolveValue(f.value, variables);
      return out;
    }
  }
}

function argValue(field: FieldNode, name: string, variables: Record<string, unknown>): unknown {
  const arg = field.args.find((a) => a.name === name);
  if (arg === undefined) return undefined;
  return resolveValue(arg.value, variables);
}

// ------------------------------------------------------------------ schema (introspection)

const SCHEMA_FIELDS: Record<string, string[]> = {
  Issue: ['id', 'number', 'title', 'state', 'body'],
  PullRequest: ['id', 'number', 'headRefName', 'baseRefName', 'mergeable', 'state', 'merged', 'files'],
  Repository: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest'],
  User: ['login', 'id'],
  IssueConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  PullRequestConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  IssueComment: ['id', 'body'],
  IssueCommentEdge: ['node', 'cursor'],
  PullRequestChangedFile: ['path'],
  PageInfo: ['hasNextPage', 'hasPreviousPage', 'startCursor', 'endCursor'],
  Ref: ['name'],
  Query: ['viewer', 'repository', 'node', '__type'],
  Mutation: ['createIssue', 'closeIssue', 'createPullRequest', 'mergePullRequest', 'addComment'],
};

const SCHEMA_ENUMS: Record<string, string[]> = {
  SearchType: ['ISSUE', 'REPOSITORY', 'USER', 'PULL_REQUEST', 'DISCUSSION'],
  IssueState: ['OPEN', 'CLOSED'],
  MergeableState: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'],
  PullRequestState: ['OPEN', 'CLOSED', 'MERGED'],
  PullRequestMergeMethod: ['MERGE', 'SQUASH', 'REBASE'],
  RepositoryPermission: ['ADMIN', 'MAINTAIN', 'WRITE', 'TRIAGE', 'READ'],
  IssueOrderField: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'],
  OrderDirection: ['ASC', 'DESC'],
};

// ------------------------------------------------------------------ executor

type Ctx = {
  store: Store;
  variables: Record<string, unknown>;
  errors: { message: string }[];
  fragments: Map<string, FragmentNode>;
};

type GqlValue =
  | { __type: 'repo'; repo: Repo }
  | { __type: 'issue'; repo: Repo; issue: Issue }
  | { __type: 'pull'; repo: Repo; pull: Pull }
  | { __type: 'user'; login: string }
  | { __type: 'comment'; repo: Repo; issue: Issue; id: number; body: string }
  | { __type: 'issues'; repo: Repo; issues: Issue[]; pageInfo: { hasNextPage: boolean; endCursor: string | null } }
  | { __type: 'pulls'; repo: Repo; pulls: Pull[] }
  | { __type: 'files'; files: { path: string }[] }
  | { __type: 'ref'; name: string }
  | { __type: 'payload'; fields: Record<string, unknown> }
  | { __type: 'commentEdge'; node: { __type: 'comment'; repo: Repo; issue: Issue; id: number; body: string } }
  | { __type: 'pageInfo'; hasNextPage: boolean; endCursor: string | null }
  | { __type: 'type'; name: string };

function cursorForIssue(issue: Issue): string {
  return b64url(`cursor:${issue.number}`);
}

function issueFromCursor(cursor: string | null): number | null {
  if (cursor === null || cursor === '') return null;
  try {
    const decoded = Buffer.from(cursor, 'base64url').toString('utf8');
    const m = /^cursor:(\d+)$/.exec(decoded);
    if (m === null) return null;
    return Number(m[1]);
  } catch {
    return null;
  }
}

/** The mergeable value for a pull: MERGEABLE when it is open (the fixture PRs are never in
 * conflict at read time; a real conflict is reported by the merge itself, which returns 405). */

function resolveField(ctx: Ctx, parent: GqlValue, field: FieldNode): unknown {
  const key = field.alias ?? field.name;
  void key;
  const name = field.name;
  const args = (n: string): unknown => argValue(field, n, ctx.variables);

  switch (parent.__type) {
    case 'repo': {
      const repo = parent.repo;
      switch (name) {
        case 'id': return repoGlobalId(repo);
        case 'databaseId': return repo.databaseId;
        case 'name': return repo.name;
        case 'nameWithOwner': return `${repo.owner}/${repo.name}`;
        case 'owner': return { __type: 'user', login: repo.owner } as GqlValue;
        case 'defaultBranchRef': return { __type: 'ref', name: repo.defaultBranch } as GqlValue;
        case 'viewerPermission': return 'ADMIN';
        case 'hasIssuesEnabled': return true;
        case 'issue': {
          const number = args('number') as number;
          const issue = repo.issues.get(number);
          if (issue === undefined) return null;
          return { __type: 'issue', repo, issue } as GqlValue;
        }
        case 'issues': {
          const first = (args('first') as number | undefined) ?? 10;
          const states = args('states') as string[] | undefined;
          const orderBy = args('orderBy') as { field?: string; direction?: string } | undefined;
          const after = args('after') as string | null | undefined;
          let list = [...repo.issues.values()];
          if (states !== undefined) list = list.filter((i) => states.includes(i.state.toUpperCase()));
          const direction = orderBy?.direction ?? 'ASC';
          // CREATED_AT ordering is creation order = number order.
          list.sort((a, b) => (direction === 'DESC' ? b.number - a.number : a.number - b.number));
          const afterNum = issueFromCursor(after ?? null);
          if (afterNum !== null) list = list.filter((i) => i.number > afterNum);
          const page = list.slice(0, first);
          const hasNextPage = list.length > first;
          const endCursor = page.length > 0 ? cursorForIssue(page[page.length - 1] as Issue) : null;
          return { __type: 'issues', repo, issues: page, pageInfo: { hasNextPage, endCursor } } as GqlValue;
        }
        case 'pullRequests': {
          const first = (args('first') as number | undefined) ?? 10;
          const list = [...repo.pulls.values()].sort((a, b) => b.number - a.number).slice(0, first);
          return { __type: 'pulls', repo, pulls: list } as GqlValue;
        }
        case 'pullRequest': {
          const number = args('number') as number;
          const pull = repo.pulls.get(number);
          if (pull === undefined) return null;
          return { __type: 'pull', repo, pull } as GqlValue;
        }
        default:
          ctx.errors.push({ message: `Cannot query field "${name}" on type "Repository".` });
          return undefined;
      }
    }
    case 'issue': {
      const issue = parent.issue;
      switch (name) {
        case 'id': return issueGlobalId(parent.repo, issue);
        case 'number': return issue.number;
        case 'title': return issue.title;
        case 'state': return issue.state.toUpperCase();
        case 'body': return issue.body;
        default:
          ctx.errors.push({ message: `Cannot query field "${name}" on type "Issue".` });
          return undefined;
      }
    }
    case 'pull': {
      const pull = parent.pull;
      switch (name) {
        case 'id': return pullGlobalId(parent.repo, pull);
        case 'number': return pull.number;
        case 'state': return pull.state.toUpperCase();
        case 'merged': return pull.merged;
        case 'headRefName': return pull.headRef;
        case 'baseRefName': return pull.baseRef;
        case 'mergeable': return pull.merged ? 'UNKNOWN' : 'MERGEABLE';
        case 'files': {
          return { __type: 'files', files: pull.files.map((f) => ({ path: f.filename })) } as GqlValue;
        }
        default:
          ctx.errors.push({ message: `Cannot query field "${name}" on type "PullRequest".` });
          return undefined;
      }
    }
    case 'issues': {
      switch (name) {
        case 'nodes': return parent.issues.map((i) => ({ __type: 'issue', repo: parent.repo, issue: i }) as GqlValue);
        case 'pageInfo': return { __type: 'pageInfo', hasNextPage: parent.pageInfo.hasNextPage, endCursor: parent.pageInfo.endCursor } as GqlValue;
        case 'totalCount': return parent.issues.length;
        default:
          ctx.errors.push({ message: `Cannot query field "${name}" on type "IssueConnection".` });
          return undefined;
      }
    }
    case 'pulls': {
      switch (name) {
        case 'nodes': return parent.pulls.map((p) => ({ __type: 'pull', repo: parent.repo, pull: p }) as GqlValue);
        case 'pageInfo': return { __type: 'pageInfo', hasNextPage: false, endCursor: null } as GqlValue;
        case 'totalCount': return parent.pulls.length;
        default:
          ctx.errors.push({ message: `Cannot query field "${name}" on type "PullRequestConnection".` });
          return undefined;
      }
    }
    case 'files': {
      switch (name) {
        case 'nodes': return parent.files.map((f) => ({ __type: 'payload', fields: { path: f.path } }) as GqlValue);
        case 'pageInfo': return { __type: 'pageInfo', hasNextPage: false, endCursor: null } as GqlValue;
        default:
          ctx.errors.push({ message: `Cannot query field "${name}" on type "PullRequestChangedFileConnection".` });
          return undefined;
      }
    }
    case 'user': {
      if (name === 'login') return parent.login;
      if (name === 'id') return b64url(`User:${parent.login}`);
      ctx.errors.push({ message: `Cannot query field "${name}" on type "User".` });
      return undefined;
    }
    case 'ref': {
      if (name === 'name') return parent.name;
      ctx.errors.push({ message: `Cannot query field "${name}" on type "Ref".` });
      return undefined;
    }
    case 'pageInfo': {
      if (name === 'hasNextPage') return parent.hasNextPage;
      if (name === 'endCursor') return parent.endCursor;
      if (name === 'hasPreviousPage') return false;
      if (name === 'startCursor') return null;
      ctx.errors.push({ message: `Cannot query field "${name}" on type "PageInfo".` });
      return undefined;
    }
    case 'comment': {
      if (name === 'id') return commentGlobalId(parent.repo, parent.issue, { id: parent.id, body: parent.body });
      if (name === 'body') return parent.body;
      ctx.errors.push({ message: `Cannot query field "${name}" on type "IssueComment".` });
      return undefined;
    }
    case 'commentEdge': {
      if (name === 'node') return parent.node;
      if (name === 'cursor') return b64url(`comment:${parent.node.id}`);
      ctx.errors.push({ message: `Cannot query field "${name}" on type "IssueCommentEdge".` });
      return undefined;
    }
    case 'type': {
      if (name === 'fields') {
        const fields = SCHEMA_FIELDS[parent.name];
        if (fields === undefined) return null;
        return fields.map((f) => ({ __type: 'payload', fields: { name: f } }) as GqlValue);
      }
      if (name === 'enumValues') {
        const values = SCHEMA_ENUMS[parent.name];
        if (values === undefined) return null;
        return values.map((v) => ({ __type: 'payload', fields: { name: v } }) as GqlValue);
      }
      if (name === 'name') return parent.name;
      ctx.errors.push({ message: `Cannot query field "${name}" on type "__Type".` });
      return undefined;
    }
    case 'payload': {
      const value = parent.fields[name];
      if (value === undefined) {
        ctx.errors.push({ message: `Cannot query field "${name}" on type "Payload".` });
        return undefined;
      }
      return value;
    }
    default:
      ctx.errors.push({ message: `Cannot query field "${name}".` });
      return undefined;
  }
}

/** Execute a selection set against a parent value, returning a JSON-ready object. */
function executeSelection(ctx: Ctx, parent: GqlValue, selection: SelectionNode[]): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const sel of selection) {
    if (sel.kind === 'spread') {
      const frag = ctx.fragments.get(sel.name);
      if (frag !== undefined) Object.assign(out, executeSelection(ctx, parent, frag.selection));
      continue;
    }
    if (sel.kind === 'inline') {
      // Type conditions are trusted for the suite's documents (node(id) on Repository).
      Object.assign(out, executeSelection(ctx, parent, sel.selection));
      continue;
    }
    const key = sel.alias ?? sel.name;
    const value = resolveField(ctx, parent, sel);
    if (value === undefined) continue;
    if (value === null) { out[key] = null; continue; }
    if (Array.isArray(value)) {
      out[key] = value.map((item) => {
        if (item !== null && typeof item === 'object' && '__type' in (item as Record<string, unknown>)) {
          return executeSelection(ctx, item as GqlValue, sel.selection);
        }
        return item;
      });
      continue;
    }
    if (typeof value === 'object' && '__type' in (value as Record<string, unknown>)) {
      out[key] = executeSelection(ctx, value as GqlValue, sel.selection);
      continue;
    }
    out[key] = value;
  }
  return out;
}

/** Find a repo/issue/pull by an opaque global id. Returns the matching GqlValue or null. */
function resolveNode(ctx: Ctx, id: string): GqlValue | null {
  let decoded: string;
  try {
    decoded = Buffer.from(id, 'base64url').toString('utf8');
  } catch {
    return null;
  }
  const parts = decoded.split(':');
  const kind = parts[0];
  if (kind === 'Repository') {
    const dbId = Number(parts[1]);
    for (const repo of ctx.store.listRepos(VIEWER_LOGIN)) {
      if (repo.databaseId === dbId) return { __type: 'repo', repo };
    }
    return null;
  }
  if (kind === 'Issue') {
    const dbId = Number(parts[1]);
    const num = Number(parts[2]);
    for (const repo of ctx.store.listRepos(VIEWER_LOGIN)) {
      if (repo.databaseId !== dbId) continue;
      const issue = repo.issues.get(num);
      if (issue !== undefined) return { __type: 'issue', repo, issue };
    }
    return null;
  }
  if (kind === 'PullRequest') {
    const dbId = Number(parts[1]);
    const num = Number(parts[2]);
    for (const repo of ctx.store.listRepos(VIEWER_LOGIN)) {
      if (repo.databaseId !== dbId) continue;
      const pull = repo.pulls.get(num);
      if (pull !== undefined) return { __type: 'pull', repo, pull };
    }
    return null;
  }
  return null;
}

/** Execute one operation (query or mutation) and return its {data, errors} envelope. */
function executeOperation(ctx: Ctx, op: OperationNode): { data: Record<string, unknown> | null; errors: { message: string }[] } {
  const data: Record<string, unknown> = {};
  for (const sel of op.selection) {
    if (sel.kind !== 'field') continue;
    const key = sel.alias ?? sel.name;
    const name = sel.name;
    const args = (n: string): unknown => argValue(sel, n, ctx.variables);

    if (name === 'viewer') {
      const viewer: GqlValue = { __type: 'user', login: VIEWER_LOGIN };
      data[key] = executeSelection(ctx, viewer, sel.selection);
      continue;
    }
    if (name === 'repository') {
      const owner = args('owner') as string;
      const repoName = args('name') as string;
      const repo = owner === VIEWER_LOGIN ? ctx.store.getRepo(owner, repoName) : undefined;
      if (repo === undefined) {
        data[key] = null;
        continue;
      }
      data[key] = executeSelection(ctx, { __type: 'repo', repo }, sel.selection);
      continue;
    }
    if (name === 'node') {
      const id = args('id') as string;
      const node = resolveNode(ctx, id);
      if (node === null) {
        data[key] = null;
        continue;
      }
      data[key] = executeSelection(ctx, node, sel.selection);
      continue;
    }
    if (name === '__type') {
      const typeName = args('name') as string;
      if (SCHEMA_FIELDS[typeName] === undefined && SCHEMA_ENUMS[typeName] === undefined) {
        data[key] = null;
        continue;
      }
      data[key] = executeSelection(ctx, { __type: 'type', name: typeName }, sel.selection);
      continue;
    }
    if (op.operation === 'mutation') {
      const input = (args('input') ?? {}) as Record<string, unknown>;
      if (name === 'createIssue') {
        const repoId = String(input.repositoryId ?? '');
        const node = resolveNode(ctx, repoId);
        if (node === null || node.__type !== 'repo') {
          ctx.errors.push({ message: 'Could not resolve to a Repository with the id provided.' });
          data[key] = null;
          continue;
        }
        const issue = ctx.store.createIssue(node.repo, String(input.title ?? ''), String(input.body ?? ''), []);
        data[key] = executeSelection(ctx, { __type: 'payload', fields: { issue: { __type: 'issue', repo: node.repo, issue } as GqlValue } }, sel.selection);
        continue;
      }
      if (name === 'closeIssue') {
        const node = resolveNode(ctx, String(input.issueId ?? ''));
        if (node === null || node.__type !== 'issue') {
          ctx.errors.push({ message: 'Could not resolve to an Issue with the id provided.' });
          data[key] = null;
          continue;
        }
        node.issue.state = 'closed';
        data[key] = executeSelection(ctx, { __type: 'payload', fields: { issue: node } }, sel.selection);
        continue;
      }
      if (name === 'createPullRequest') {
        const node = resolveNode(ctx, String(input.repositoryId ?? ''));
        if (node === null || node.__type !== 'repo') {
          ctx.errors.push({ message: 'Could not resolve to a Repository with the id provided.' });
          data[key] = null;
          continue;
        }
        const repo = node.repo;
        const headRef = String(input.headRefName ?? '');
        const baseRef = String(input.baseRefName ?? repo.defaultBranch);
        const headSha = repo.refs.get(`refs/heads/${headRef}`);
        const baseSha = repo.refs.get(`refs/heads/${baseRef}`);
        if (headSha === undefined || baseSha === undefined) {
          ctx.errors.push({ message: 'Head or base ref not found.' });
          data[key] = null;
          continue;
        }
        const pull = ctx.store.createPull(repo, String(input.title ?? ''), String(input.body ?? ''), headRef, baseRef, headSha, baseSha);
        data[key] = executeSelection(ctx, { __type: 'payload', fields: { pullRequest: { __type: 'pull', repo, pull } as GqlValue } }, sel.selection);
        continue;
      }
      if (name === 'mergePullRequest') {
        const node = resolveNode(ctx, String(input.pullRequestId ?? ''));
        if (node === null || node.__type !== 'pull') {
          ctx.errors.push({ message: 'Could not resolve to a PullRequest with the id provided.' });
          data[key] = null;
          continue;
        }
        const mergeSha = ctx.store.mergePull(node.repo, node.pull);
        if (mergeSha === null) {
          ctx.errors.push({ message: 'Merge conflict.' });
          data[key] = executeSelection(ctx, { __type: 'payload', fields: { pullRequest: node } }, sel.selection);
          continue;
        }
        data[key] = executeSelection(ctx, { __type: 'payload', fields: { pullRequest: node } }, sel.selection);
        continue;
      }
      if (name === 'addComment') {
        const node = resolveNode(ctx, String(input.subjectId ?? ''));
        if (node === null || node.__type !== 'issue') {
          ctx.errors.push({ message: 'Could not resolve to an Issue with the id provided.' });
          data[key] = null;
          continue;
        }
        const comment = { id: node.issue.nextCommentId, body: String(input.body ?? '') };
        node.issue.nextCommentId += 1;
        node.issue.comments.push(comment);
        const commentValue: GqlValue = { __type: 'comment', repo: node.repo, issue: node.issue, id: comment.id, body: comment.body };
        const edge: GqlValue = { __type: 'commentEdge', node: commentValue };
        data[key] = executeSelection(ctx, { __type: 'payload', fields: { commentEdge: edge } }, sel.selection);
        continue;
      }
    }
    ctx.errors.push({ message: `Cannot query field "${name}" on type "${op.operation === 'mutation' ? 'Mutation' : 'Query'}".` });
    data[key] = null;
  }
  return { data, errors: ctx.errors };
}

/** Run one GraphQL request body (single op or batched array) and return the response body. */
export function runGraphQL(store: Store, body: unknown): unknown {
  const runOne = (item: unknown): unknown => {
    if (item === null || typeof item !== 'object' || Array.isArray(item)) {
      return { data: null, errors: [{ message: 'Invalid GraphQL request.' }] };
    }
    const req = item as { query?: unknown; variables?: unknown; operationName?: unknown };
    if (typeof req.query !== 'string') {
      return { data: null, errors: [{ message: 'Must provide query string.' }] };
    }
    const variables = (req.variables !== null && typeof req.variables === 'object' && !Array.isArray(req.variables))
      ? (req.variables as Record<string, unknown>)
      : {};
    let defs: DefinitionNode[];
    try {
      defs = new Parser(req.query).parseDocument();
    } catch (err) {
      return { data: null, errors: [{ message: `Syntax Error: ${err instanceof Error ? err.message : 'parse failed'}` }] };
    }
    const fragments = new Map<string, FragmentNode>();
    let op: OperationNode | null = null;
    for (const def of defs) {
      if (def.kind === 'fragment') fragments.set(def.name, def);
      else if (op === null) {
        if (typeof req.operationName === 'string' && req.operationName !== '' && def.name !== req.operationName) continue;
        op = def;
      }
    }
    if (op === null) {
      return { data: null, errors: [{ message: 'Must provide an operation.' }] };
    }
    const ctx: Ctx = { store, variables, errors: [], fragments };
    const result = executeOperation(ctx, op);
    const envelope: Record<string, unknown> = { data: result.data };
    if (result.errors.length > 0) envelope.errors = result.errors;
    return envelope;
  };

  if (Array.isArray(body)) {
    if (body.length === 0) return { data: null, errors: [{ message: 'Must provide at least one operation.' }] };
    return body.map((item) => runOne(item));
  }
  return runOne(body);
}

/** The HTTP handler for POST /api/graphql. Returns true when it handled the request. */
export async function handleGraphQL(store: Store, req: IncomingMessage, res: ServerResponse): Promise<boolean> {
  if (req.method !== 'POST') return false;
  if (!isAuthorized(req)) { sendUnauthorized(res); return true; }
  const raw = await readBody(req);
  if (raw === null) {
    sendJson(res, 413, errorBody('Request body too large'));
    return true;
  }
  const parsed = parseJsonBody(raw);
  if (!parsed.ok) {
    sendJson(res, 400, errorBody('Problems parsing JSON'));
    return true;
  }
  const result = runGraphQL(store, parsed.value);
  sendJson(res, 200, result);
  return true;
}

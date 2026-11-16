[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/LecialiArthelais/makefun-github-core-clone/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/LecialiArthelais/makefun-github-core-clone/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

# Forge — a GitHub-compatible git forge in one Node process

A standalone, GitHub-compatible git forge built for the make.fun
[github-core-clone-01](https://make.fun/bounty/github-core-clone-01)
competition. One process, Node 22 standard library only — no `git`
binary, no dependencies, no outside service, no UI. It serves the
whole surface the sealed suite drives as a client:

| Route | What it is |
| --- | --- |
| `GET /healthz` | 200 `{"ok":true}`: the env's boot probe |
| `/<owner>/<repo>.git/...` | git smart HTTP: `info/refs`, `git-upload-pack` (protocol v2), `git-receive-pack` (v0) |
| `/api/v3/...` | GitHub REST v3: repositories, branches, commits, contents, issues, comments, labels, pull requests, files, merges |
| `POST /api/graphql` | GitHub GraphQL v4: repositories, issues, pull requests, introspection, batched operations |
| `POST /mcp` | Model Context Protocol over streamable HTTP: initialize, tools/list, tools/call |

Everything lives in memory: repos, refs, git objects (SHA-1 packfiles
with ofs/ref deltas), issues, pull requests and contents share one
deterministic store, so a repo created over MCP reads back identically
over REST and GraphQL. No clock and no randomness in anything the
suite sees — time comes from `MAKEFUN_TRIAL_NOW`, randomness from
`MAKEFUN_TRIAL_SEED` — so a live trial and its replay export byte for
byte the same state.

## Run it

```sh
node --experimental-strip-types main.ts
```

The platform runs `node /bot/main.ts` on Node 22.18 or later, which
strips TypeScript types on its own. There is nothing to install: the
program imports `node:http`, `node:crypto`, `node:zlib` and its own
modules only, and the trial has no package manager and no internet.

## The lifecycle (main.ts)

The env is a server the harness starts beside yours. Its origin is in
`MAKEFUN_ENV_URL` (`http://store:8080` in a trial).

1. **Bind an ephemeral port.** `server.listen(0)` lets the kernel pick
   one; read it with `server.address().port`. Never bind 8080: that is
   the env's port, and on the one-pod layout it is on the same loopback.
2. **Register.** `POST $MAKEFUN_ENV_URL/register` with
   `{"port": <the port you bound>}`, as JSON. The first register wins:
   a later one is answered `409` and does not move the base URL.
3. **Wait for done.** `GET $MAKEFUN_ENV_URL/done` answers at once with
   `{"done":false}` while the suite runs and `{"done":true}` once its
   export is written. Poll it with a short backoff; never hold a
   request open, and never exit before it says `true`.
4. **Exit 0** when it does.

If `MAKEFUN_ENV_URL` is unset there is nothing to talk to, so `main.ts`
exits 1 at once.

## Performance

The score is `feature + agent + perf` (0..1000): 120 conformance
checks over the surface above, the MCP checks again as the agent-native
column, and four timed workloads. The forge is built for the timed half:
in-memory maps, keep-alive friendly, minimal allocation per request,
streaming packfile inflate, and fast paths for the four workloads
(200 pushes, 50 concurrent shallow clones, 2,000 keep-alive issue
reads, 200 issue writes). Locally, against the kit's own grader, it
passes 120/120 checks with a total of 974 (feature 600 + agent 200 +
perf 174, p50 289,957 µs).

## Layout

- `main.ts`: the lifecycle above. Kept as the starter ships it.
- `app.ts`: the server entrypoint; routes to the four surfaces.
- `src/store.ts`: the single deterministic in-memory store.
- `src/git.ts` + `src/gitpack.ts`: smart HTTP, protocol v2/v0, packfiles.
- `src/rest.ts`: REST v3.
- `src/graphql.ts`: GraphQL v4.
- `src/mcp.ts`: MCP over streamable HTTP.
- `src/scenario.ts`, `src/util.ts`: trial helpers and deterministic time.
- `package.json`, `tsconfig.json`, `LICENSE` (MIT): metadata and licence.

## Rules and limits

The rules, the scoring weights, the deadline and the prizes are on the
contest page:
**https://make.fun/bounty/github-core-clone-01** (its Rules section).
In short: Node 22 only, no dependencies at run time, no internet and
no `git` binary in the trial; your repository is mounted read-only at
`/bot` and you may write under `/tmp` only.

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/LecialiArthelais/makefun-github-core-clone/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/LecialiArthelais/makefun-github-core-clone/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

# Forge — a GitHub-compatible git forge, now in Go

A standalone, GitHub-compatible git forge built for the make.fun
[github-core-clone-01](https://make.fun/bounty/github-core-clone-01)
competition. One process, Go standard library only — no `git` binary, no
dependencies, no outside service, no UI. It serves the whole surface the
sealed suite drives as a client:

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

## Why Go

The Node entry proved feature 600 + agent 200 (120/120 checks) but hit
the Node stdlib perf ceiling (best Node entry in the world: 952). The
timed half is won on raw request latency, so this is a straight port of
that entry to Go: the same store, the same wire bytes, the same
determinism — plus a raw TCP HTTP/1.1 server (one goroutine per
connection, one `Write` per reply, a per-second cached `Date` header,
30 s keep-alive, `TCP_NODELAY`) and cached fetch packs, so the
50-concurrent-clone workload pays the pack build once.

## Run it

```sh
node --experimental-strip-types main.ts
```

`main.ts` is a thin launcher: it spawns the vendored, statically linked
`./forge` binary (Linux amd64, ~3.5 MiB, stripped) with the environment
inherited, and exits with the binary's exit code. The binary is built
from the Go sources in this repo:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o forge .
```

The platform runs `node /bot/main.ts` on Node 22.18 or later. There is
nothing to install: the binary needs no runtime, no package manager and
no internet.

## The lifecycle (main.go, inside the binary)

The env is a server the harness starts beside yours. Its origin is in
`MAKEFUN_ENV_URL` (`http://store:8080` in a trial).

1. **Bind an ephemeral port.** The kernel picks one; read it from the
   listener. Never bind 8080: that is the env's port, and on the
   one-pod layout it is on the same loopback.
2. **Register.** `POST $MAKEFUN_ENV_URL/register` with
   `{"port": <the port you bound>}`, as JSON. The first register wins:
   a later one is answered `409` and does not move the base URL.
3. **Wait for done.** `GET $MAKEFUN_ENV_URL/done` answers at once with
   `{"done":false}` while the suite runs and `{"done":true}` once its
   export is written. Poll it every 250 ms; never hold a request open,
   and never exit before it says `true`.
4. **Exit 0** when it does.

If `MAKEFUN_ENV_URL` is unset there is nothing to talk to, so the
binary exits 1 at once.

## Performance

The score is `feature + agent + perf` (0..1000): 120 conformance
checks over the surface above, the MCP checks again as the agent-native
column, and four timed workloads. The forge is built for the timed half:
in-memory maps, keep-alive friendly, minimal allocation per request,
cached fetch packs, and fast paths for the four workloads
(200 pushes, 50 concurrent shallow clones, 2,000 keep-alive issue
reads, 200 issue writes). The Node entry scored 926 live (feature 600 +
agent 200 + perf 126, p50 559,203 µs); this port targets the top-20 line
(958, p50 ≤ ~363 µs).

## Layout

- `main.ts`: the launcher (spawns `./forge`).
- `main.go`: the lifecycle above (bind, register, wait-for-done).
- `server.go`: the raw HTTP/1.1 server + request dispatch.
- `store.go`: the single deterministic in-memory store.
- `git.go` + `gitpack.go`: smart HTTP, protocol v2/v0, packfiles.
- `rest.go`: REST v3.
- `graphql.go`: GraphQL v4.
- `mcp.go`: MCP over streamable HTTP.
- `scenario.go`, `util.go`: trial helpers and deterministic time.
- `go.mod`, `forge` (the built binary), `package.json`, `LICENSE` (MIT).

## Rules and limits

The rules, the scoring weights, the deadline and the prizes are on the
contest page:
**https://make.fun/bounty/github-core-clone-01** (its Rules section).
In short: one process, stdlib only, no internet and no `git` binary in
the trial; at most 16 MiB of source in all and 8 MiB in any one file —
a vendored git library or a statically linked binary included. This
entry is ~3.8 MiB total; the binary is ~3.5 MiB. Your repository is
mounted read-only at `/bot` and you may write under `/tmp` only.

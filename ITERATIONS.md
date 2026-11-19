# Optimizer Go Perf (HTTP/server) — Iterations

Worktree: `~/workspace/makefun/github-core-clone-01/go-port-http` (branch `go-port-http`,
from `go-port` commit `e37d515`). Scope: `server.go`, `store.go` hot paths,
`rest.go`/`graphql.go`/`mcp.go`, `main.go`. TIDAK sentuh `gitpack.go` dan
pack-generation di `git.go` (area spesialis pack). TIDAK submit, TIDAK pakai kuota.

## Baseline jujur (mengikat — dari parent + GRADE-PROTOCOL §6)

- Grade-local median **954** (600+200+154), p50 **378.677 µs**, kit saat ini
  (harness@53808750…/scorer@543971df…). Run: 954/955/937. **BUKAN 966/323µs**
  (klaim port, kit lama, tidak tereproduksi).
- Median per-workload lintas-run: push 589,9 ms/43 · clone 187,6/31 ·
  read 1.418,1/37 · write 163,0/39. Run median (run1): push 630,2/42 ·
  clone 187,6/31 · read 1.067,1/42 · write 163,0/39.
- Fungsional: 120/120, state.json `a0e98d49…`, nondeterministic 0.
- Aturan noise: selisih median < ~10 poin BUKAN bukti. Kemenangan harus jelas
  di atas noise; A/B hanya pada kondisi beban sama.
- Lever analis (top5-levers-2026-11-19.md): tugasku = **L4+L5 paket "read path"**
  (memoized JSON + zero-alloc parsing/fast dispatcher). L1+L2 = spesialis pack
  (gitpack.go, di luar scope). L3 (RWMutex) DITOLAK untukku — bukti profil:
  dispatch cum 3,7µs/req, total lock-hold clone ≈ 250×~4µs ≈ 1 ms dari wall
  187 ms (~0,5%) — gain ≈ 0. L6/L7 gain ≈ 0 (cache/depth-1). L8 (epoll)
  DITOLAK analis — effort/risiko tidak sebanding; Go netpoller sudah epoll.
- Target realistis: top-20 bila R ≤ ~1,05 + stack +4; top-5 praktis <5%.
  Tetap maksimalkan: submit Go ~939–955 tetap +13–29 di atas Node 926.

## Probe lokal (Go client, server-only) — /tmp/forge-probe/probe

Mengukur round-trip server (bukan undici). Mesin tenang (load ~0,9), 19 Nov ~21:34 WIB,
binary baseline e37d515 (flag exact `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w"`):

| bench | run1 | run2 | run3 | median-of-medians |
|---|---|---|---|---|
| healthz2000 | 342,4 ms | 322,6 ms | 326,1 ms | ~326 ms (163µs/op) |
| read2000 | 368,8 ms | 350,4 ms | 322,5 ms | ~350 ms (175µs/op) |
| write200 | 27,3 ms | 27,4 ms | 27,0 ms | ~27 ms (136µs/op) |
| pushAdv200 | 28,9 ms | 25,8 ms | 29,5 ms | ~29 ms (144µs/op) |
| clone50conc | 8,4 ms | 8,8 ms | 21,8 ms | ~9 ms (sangat noisy) |

Di bawah beban (load ~4,6): healthz ~370µs/op, read ~360µs/op — probe SANGAT
sensitif beban; A/B harus back-to-back dalam kondisi sama.

## Profil CPU server (pprof, soak 164.106 req / 30 dtk, /tmp/forge-probe/cpu.prof)

Server CPU ~46µs/req. Dominan: syscall read/write+epoll (~60%), scheduler/
netpoll/futex (~15-18%), `writeResponse` cum 37% (mayoritas Write syscall),
`dispatch` cum 7,9% (~3,7µs/req), `handleRest` 5,8%, JSON marshal ~2%.
Handler+parse+marshal ≈ 6-8µs dari 46µs — diet parse/marshal saja memberi
paling banyak ~10-15% CPU server ≈ beberapa % wall. Tidak ada satu lever
HTTP yang memberi +10 poin; kemenangan (bila ada) datang dari stack.

## Iterasi

### Iter 0 — Baseline (e37d515, belum ada perubahan)
- Probe: lihat tabel di atas. Grade-local: 954 median (verifikasi benchmarker,
  grade-local-verify/verify-run1/2/3). Grade-local-http-0 saya sendiri: 905
  (TERKONTAMINASI beban load 4,6 — jangan dipakai).
- Status: baseline tercatat. Belum commit apa pun di go-port-http.

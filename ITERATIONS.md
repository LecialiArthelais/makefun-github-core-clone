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

### Iter 1 — L5 parse/dispatch/write diet (commit 3adfeb8)
- Parse head byte-level (tanpa `string(head)`, tanpa `Split`); header
  name/value lewati ToLower/TrimSpace bila sudah kanonis (fallback eksak);
  query lazy (QueryMap); writeResponse append-only + status-line table +
  buffer pre-sized; dateHeader lock-free (atomic.Value); repoKey tanpa
  alokasi ToLower bila sudah lowercase.
- Bench (net.Pipe, median 3×50k, kondisi tenang): HandleConn GET issue
  16,1µs → 12,4µs; POST issue 20,0µs → 18,0µs; WriteResponse 3,6µs → 2,4µs;
  dispatch GET issue 1,45µs → 1,35µs. Probe (TCP): read −3%, write −6%,
  pushAdv −4%, healthz dalam noise.
- Status: COMMITTED 3adfeb8. Tidak ada perubahan perilaku (byte respons sama).

### Iter 2 — L4 memoized issue JSON (commit a044829)
- GET single issue + GET list issues memakai body JSON memoized per issue,
  kunci (issue.ver, repo.labelsVer); ver naik di PATCH/comment/close/
  addComment (REST + GraphQL), labelsVer naik di label-create. POST create
  mengisi cache; PATCH mengisi ulang sesudah bump. Pola sama dengan cache git.
- Bench DispatchGetIssue 2,7µs → 1,4µs (cache-hot). Probe (TCP, back-to-back):
  read2000 369 ms → 339 ms (−8%, ketiga ronde terpisah tanpa overlap);
  write tidak berubah (issue baru tidak reuse cache — sesuai harapan).
- Status: COMMITTED a044829. Semua situs mutasi issue/label diaudit
  (rest.go PATCH/comment/label, graphql.go closeIssue/addComment).

### Iter 3 — Memoized v0 adv + refs fingerprint + sorted ref names (commit 42a181f)
- Ketiganya fungsi murni dari refs (+ DefaultBranch tetap); cache per
  Generation — sinyal invalidasi yang sama dengan lsCache/respCache/
  packCache (Generation naik di setiap mutasi refs: SetBranch/DeleteBranch,
  REST ref-create, receive-pack).
- Bench DispatchPushAdv 8,0µs → 5,9µs (−26%, −2,1µs/adv; kontrol
  UploadAdv tidak berubah sesuai harapan). Per ronde push ~0,4 ms dari ~590 ms.
- Status: COMMITTED 42a181f.

### Ditolak / tidak dikerjakan (dengan bukti)
- L3 RWMutex: total lock-hold clone ≈ 250×~4µs ≈ 1 ms dari wall 187 ms
  (~0,5%) — gain ≈ 0, risiko race/determinisme tinggi.
- L6 (compressed bytes per objek): pack hanya dibangun 1× per tip (49/50
  clone cache-hit) — gain ≈ 0.
- L7 (commit graph index): clone depth-1 tidak me-walk graf — gain ≈ 0.
- L8 epoll single-threaded: DITOLAK analis (effort/risiko >> gain; Go
  netpoller sudah epoll; tidak ada entri epoll di top-5).
- SIMD HTTP parsing: abu-abu aturan + assembly, 1 judul saja.

## Grade-local A/B: baseline e37d515 vs kandidat Iter 1+2+3 (42a181f)
- 3 pasang interleaved (grade.sh), kit sama, 19 Nov ~21:46–21:48 WIB,
  load 2,7–3,1. Direktori: grade-local-verify/http-base-{1,2,3} /
  http-opt-{1,2,3}. Semua run: 120/120 (600+200), nondeterministic 0,
  state sha256 a0e98d49… (identik).
- BASE: skor 971 / 965 / 968 → median 968; p50 301.481 / 328.776 / 308.512 µs
  → median 308.512 µs. Per-workload median: push 431,9 ms/49 · clone
  149,0 ms/36 · read 991,8 ms/43 · write 135,3 ms/42.
- OPT: skor 971 / 968 / 953 → median 968; p50 300.401 / 312.958 / 390.173 µs
  → median 312.958 µs. Per-workload median: push 481,5 ms/47 · clone
  192,3 ms/31 · read 970,1 ms/44 · write 131,4 ms/43.
- Verdict JUJUR: median skor IDENTIK 968 vs 968 — TIDAK ADA gain terbukti
  (aturan noise: selisih < ~10 poin bukan bukti). Opt menang read +1 /
  write +1, kalah push −2 / clone −5 di median — semuanya dalam noise.
  Run opt-3 (953) terdegradasi di SEMUA workload sekaligus (push 481,
  clone 228, read 1226, write 172) = spike beban sesaat, bukan regresi
  kode (opt-1/opt-2 = 971/968). Baseline sendiri kini 968 (bukan 954)
  karena mesin lebih tenang — konfirmasi skor antar-run bergeser ±15 poin
  hanya karena beban. CPU server memang turun terukur (bench/probe), tapi
  grade-local didominasi klien undici + scheduling, jadi ~5 µs/req yang
  dihemat ≈ 1% wall dan tenggelam dalam noise.
- Stack dipertahankan: tidak ada bukti harm, semua pagar lolos, CPU
  terukur lebih rendah (bench/probe), output byte-identik. Bukan klaim
  kemenangan skor — laporkan apa adanya.

## Iter 4 — DITOLAK (diukur, bukan ditebak)
- RWMutex (prioritas parent pasca-pack): diukur presisi — bench net.Pipe
  (mesin tenang, median 3×50k): DispatchPushAdv ~0,5µs,
  DispatchLsRefsCached ~2,0µs, DispatchFetchCached ~3,0µs.
  Bound serialisasi clone = 50 × (3×0,5 + 2,0 + 3,0)µs ≈ 325µs dari wall
  ~149–192 ms ≈ 0,2%. Gain ≈ 0, dan risiko race NYATA dengan cache
  Iter 2 (GET issue menulis field cache di bawah RLock). TIDAK dikerjakan.
- GOMAXPROCS=1: probe awal tampak 2–3× lebih cepat — TERNYATA CONFOUND:
  env var ikut ke proses probe (klien) JUGA. Dengan klien dipatok G=4
  dan hanya server yang divariasi: healthz 175/174/200µs (G4/G2/G1),
  read 184/162/184, write 137/175/150, pushAdv 125/178/143, clone
  171/168/206 — semua dalam noise, G1 malah merugikan clone. TIDAK
  dikerjakan (tidak ada runtime.GOMAXPROCS di main.go).
- Micro-opt tersisa (parse body POST issue, hand-marshal POST, ganti
  indexCRLFCRLF dengan bytes.Index, refresh deadline 1/detik): masing-
  masing ~0,1–0,15% wall — di bawah noise, tidak dikerjakan.
- Kesimpulan lapisan HTTP: sudah di lantai praktis (1 read + 1 write
  syscall + ~5µs handler per request); sisa wall adalah klien + scheduling
  kernel yang tidak bisa dipangkas dari sisi server.

## Grade-local A/B final: 8 pasang (pairs 1–6 normal, 7–8 reverse order)
- Tambahan 3 pasang (4–6) + 2 pasang reverse-order (7–8, opt dulu baru
  base — untuk menguji hipotesis "run kedua selalu kalah karena drift
  beban"). Total 8 run per sisi, kit sama, 19 Nov ~21:46–21:55 WIB.
- BASE (8): 971/965/968/966/967/953/957/908 → median 965,5, mean 956,9,
  range 908–971. p50 median 327,6µs. Pts median: push 49 · clone 32 ·
  read 40 · write 42.
- OPT (8): 971/968/953/959/951/959/953/968 → median 959,0, mean 960,2,
  range 951–971. p50 median 355,5µs. Pts median: push 47 · clone 31 ·
  read 41 · write 41,5.
- Verdict FINAL: SERI secara statistik. Median −6,5 poin (dalam band
  noise ±10), mean +3,3 poin untuk opt, worst-case opt lebih baik
  (951 vs 908 — base-8 kolaps saat load 3,59). Distribusi tumpang
  tindih penuh; 8 run tidak cukup untuk memisahkan selisih ~5 poin
  dari noise run-to-run σ ~15–20 poin (read satu run saja bervariasi
  850–2079 ms antar-ronde). Reverse-order TIDAK menunjukkan efek urutan
  sistematis (pair 7: opt-dulu 953 vs base-kedua 957).
- Hipotesis regresi parser DIBANTAH: bench HandleConn bentuk-undici
  (9 header, median 3×50k) — GET issue baseline 30,2µs vs iter3 24,3µs;
  POST issue 47,6µs vs 42,6µs. Iter 3 lebih cepat juga di bentuk real.
- Keputusan: stack Iter 1–3 DIPERTAHANKAN (sama seperti L1 pack:
  CPU terukur lebih rendah, semua pagar lolos, tidak ada bukti harm;
  skor grade-local netral). Bukan klaim kemenangan — netral.

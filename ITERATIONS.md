# ITERATIONS — go-port-pack (spesialis git/pack)

Worktree: `~/workspace/makefun/github-core-clone-01/go-port-pack`, branch `go-port-pack`,
dari `go-port` commit `e37d515`. Scope: `gitpack.go`, `git.go` (pack/zlib/commit
graph/cached packs/upload-pack/receive-pack). `server.go` TIDAK disentuh (area
optimizer HTTP). `store.go`/`util.go` hanya bila perlu (sejauh ini: tidak).

## Baseline jujur (kit SAAT INI — mengikat)

Median 3 run verifikasi independen (`grade-local-verify/verify-run1/2/3`,
harness@53808750…/scorer@543971df…): **954 (perf 154, p50 378.677 µs)**,
run 954/955/937. Split run median (run1): push 630,2 ms/42 · clone 187,6 ms/31 ·
read 1.067,1 ms/42 · write 163,0 ms/39. 120/120, state `a0e98d49…`,
nondeterministic 0. Klaim port 966/p50 323 µs berasal dari kit lama dan TIDAK
tereproduksi — jangan dipakai sebagai pembanding.

Aturan penilaian (GRADE-PROTOCOL §6): median ≥3 run kit yang sama; selisih
median < ~10 poin BUKAN bukti. Noise antar-run ±18 poin (load mesin).

Perintah ukur: `cd ~/workspace/makefun/github-core-clone-01/grade-local-verify
&& ./grade.sh <worktree> <label>` (Node 22, driver process).
Rebuild binary (wajib flag exact, Go 1.25.1 di
`~/workspace/tools/go/go/bin/go`):
`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o forge .`
(baseline sha256 `62fae2cb9435ca63d61b884a5888f528367888c4a70c2351ff6e3b1679dbf406`).
Cek gofmt: `gofmt -l .` (bukan `go fmt -l`).

## Temuan pengukuran (sebelum iterasi)

- Mikro-benchmark (`/tmp/forge-bench`, Go 1.25.1; angka absolut noisy di VM
  shared, rasio dalam-run yang dipakai): fetch cache-hit 0,8 µs; dispatch
  fetch 10,9 µs; `writePack` 3 objek 2,08 ms; `readPack` 3 objek ~202 µs;
  `handleReceivePack` ~260 µs.
- Load-test klien Go cepat (forge standalone + fake-env): rantai clone 4
  request median ~0,56–0,94 ms (vs kit ~3,6 ms/clone) → **clone didominasi
  klien undici, bukan server**. 50 clone konkuren 6–26 ms di server vs
  160–190 ms di kit. Per-request server+transport ~135–210 µs, handler
  git.go sendiri ~1–11 µs. **Lever server-side untuk clone ≈ 1% — praktis nol.**
- Push kit 2,95 ms/push; klien Go 0,33–0,51 ms/push (adv + receive-pack).
  Handler server ~260 µs ≈ 9% dari waktu kit per push.
- Isolasi zlib (dalam-run): inflate fresh `zlib.NewReader` 16,4 µs/objek →
  pooled 2,35 µs (**7×**); deflate fresh `zlib.NewWriter` 446 µs/objek →
  pooled 22,6 µs (**20×**). Bufio saja TIDAK membantu (18,1 µs) — kemenangan
  murni dari pooling (alokasi window 32 KiB + tabel Huffman per reader/writer),
  bukan buffering. Sumber flate Go 1.25 diperiksa: semua input hanya lewat
  `f.r` (ByteReader yang diberi); tidak ada read-ahead internal → rumus
  `consumed = cr.n − br.Buffered()` eksak, tapi bufio tidak dipakai karena
  tidak memberi apa-apa.

## Iterasi 1 — L1 pooled zlib (reader + writer) — `gitpack.go`

- Perubahan: `sync.Pool` untuk `*zlib.Reader` (seed: stream zlib kosong yang
  valid, dibuat stdlib sendiri) + `*zlib.Writer`; `Reset` di tiap pakai.
  `readPackObject`: `Reset(cr, nil)` pada countingReader langsung — rumus
  `consumed = cr.n` dan `ReadByte()` TIDAK berubah (perbaikan
  pack-truncated tetap utuh). `writePackObject`: `Reset(out)`. Tanpa bufio.
  Tanpa perubahan level kompresi, urutan objek, atau byte output apa pun.
- Bukti kebenaran (scratch `/tmp/forge-bench`, tes vs implementasi baru):
  `TestPooledInflateExact` PASS (consumed identik), `TestPooledWriterIdentical`
  PASS (byte-identik incl. reuse ke-2), `TestPooledReaderNoBufioExact` PASS.
  `readPack` 3 objek ~202 µs → ~10–26 µs; `readPack` 600 objek 15,1 ms →
  1,57 ms. `zlib.Reset` diperiksa di sumber Go: re-init penuh (`*z = reader{…}`,
  `flate.Resetter.Reset`), aman di-pool bahkan setelah error.
- Perkiraan efek skor (jujur): inflate pooling hemat ~40 µs/push dari
  handler ~260 µs → push round −~1,5% → **~+0,3 pt push**; clone ~0 (cache
  hit); read/write 0. **Di bawah noise grade-local (±10 pt)** — tidak akan
  terbukti dari satu median-of-3; nilainya adalah pengurangan CPU riil yang
  menumpuk dengan paket HTTP/REST agen lain. Writer pooling mempercepat
  fetch-miss/conformance (untimed).
- `gofmt -l` bersih, `go vet` PASS, binary 3.657.912 byte (≤8 MiB ✓).
- Status: COMMITTED (`6ec859e`), grade-local ×3 + A/B ×3 SELESAI (lihat bawah).
  Binary reproducible byte-identik (rebuild ke /tmp + `cmp` PASS).

## Hasil grade-local iterasi 1 (kit yang sama, grade.sh)

Kandidat ×3 (`grade-local-verify/pack-pool-run1/2/3`):
- run1: 935 (perf 135, p50 498.926 µs) — push 444,7/49 · clone 263,4/25 ·
  read 2437,4/27 · write 217,0/34
- run2: 958 (perf 158, p50 360.057 µs) — push 426,8/49 · clone 214,9/29 ·
  read 1101,6/42 · write 166,3/38
- run3: 914 (perf 114, p50 675.431 µs) — push 855,1/37 · clone 472,2/15 ·
  read 2382,2/28 · write 216,4/34
- Median kandidat (batch ini): **935**. Semua run: 120/120, state `a0e98d49…`,
  nondeterministic 0.

A/B interleave vs baseline `go-port-worktree` e37d515 (pasangan berurutan,
jendela beban sama; `ab-base-N` vs `ab-pack-N`):
- Pair 1 (load ~1,35): base 957 (358.573 µs) vs pack 965 (329.681 µs) → +8
- Pair 2 (load melonjak 4,29 di tengah): base 948 (416.452 µs) vs pack 934
  (502.001 µs) → −14 (kandidat jalan saat spike — ter-confound)
- Pair 3 (load 4,47): base 946 (421.651 µs) vs pack 961 (345.427 µs) → +15
- Median A/B: base **948** vs pack **961** → +13. TAPI split per-workload
  A/B: push base 453,0/48 vs pack 537,9/45 (−3); clone 186,3/31 vs 193,9/31
  (0); read 1289,6/39 vs 1160,0/41 (+2); write 185,6/36 vs 137,0/42 (+6).
  **+13 itu berasal dari read/write — workload yang pooling TIDAK BISA
  sentuh — sementara push (satu-satunya yang bisa) −3.** Itu sidik jari
  noise/drift kondisi mesin, bukan efek kode.
- Semua 6 run kandidat vs 6 run baseline (verify + A/B): median total
  kandidat 946,5 vs baseline 951 — dalam noise (< ~10 pt bukan bukti).
  Push median 6-run: kandidat 491,5 ms vs baseline 526,5 ms (lemah positif,
  dalam noise). Clone median 6-run: kandidat ~229 ms vs baseline ~187 ms —
  clone tidak tersentuh pooling (cache-hit, client-bound); selisih ini noise.
- **Verdict: pooling TIDAK TERBUKTI menambah skor grade-local (efeknya di
  bawah noise, sesuai prediksi ~+0,3 pt), dan TIDAK TERBUKTI merugikan
  (120/120 + state identik + nondet 0 di semua 6 run; tidak ada workload yang
  memburuk secara konsisten di luar noise).** Dipertahankan karena:
  pengurangan CPU pack-path terukur kuat di benchmark terisolasi (7× inflate,
  20× deflate; readPack 600 objek 15,1 ms → 1,57 ms), output byte-identik,
  risiko nol yang terdemonstrasi — dan karena beban alokasi/GC yang hilang
  membantu di host produksi yang ketat memorinya, yang tidak bisa diukur di
  sini. Bila parent butuh klaim skor, klaim yang jujur adalah "netral,
  tidak terbukti ±", bukan "+13".

## Iterasi berikutnya (kandidat, belum dikerjakan)

- L2-buffered inflate: DITOLAK berdasarkan pengukuran — bufio saja 18,1 µs vs
  16,4 µs byte-by-byte; tidak ada gain. Pooling sudah mengambil seluruh
  kemenangan inflate.
- L6 verbatim compressed bytes / L7 commit graph: gain ≈ 0 untuk workload
  yang diukur (clone depth-1, 1 tip, cache-hit) — jangan kerjakan (lihat
  top5-levers-2026-11-19.md §3).
- Sisa biaya push/clone di luar scope ini: parse/write HTTP + mutex store
  (`server.go`, optimizer HTTP) dan marshal JSON (`rest.go`, paket read-path).

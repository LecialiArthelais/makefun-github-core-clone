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
- Status: COMMITTED (lihat git log), grade-local ×3 MENUNGGU (load mesin
  ~3–4 saat implementasi; jalankan saat load memungkinkan).

## Iterasi berikutnya (kandidat, belum dikerjakan)

- L2-buffered inflate: DITOLAK berdasarkan pengukuran — bufio saja 18,1 µs vs
  16,4 µs byte-by-byte; tidak ada gain. Pooling sudah mengambil seluruh
  kemenangan inflate.
- L6 verbatim compressed bytes / L7 commit graph: gain ≈ 0 untuk workload
  yang diukur (clone depth-1, 1 tip, cache-hit) — jangan kerjakan (lihat
  top5-levers-2026-11-19.md §3).
- Sisa biaya push/clone di luar scope ini: parse/write HTTP + mutex store
  (`server.go`, optimizer HTTP) dan marshal JSON (`rest.go`, paket read-path).

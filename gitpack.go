// Git pack wire format: pkt-line framing and pack files (plain objects, no
// delta compression on write; OFS/REF delta resolution on read). Delta
// decompression is zlib (Go's compress/zlib, stdlib). Inflates happen
// synchronously for objects <= 64 KiB and stream otherwise — same split as the
// Node original, so a pathological decompression bomb cannot hold the store
// lock for long on one large object.

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

func pktLine(data []byte) []byte {
	total := len(data) + 4
	out := make([]byte, 4, total)
	out[0] = byte("0123456789abcdef"[(total>>12)&0xf])
	out[1] = byte("0123456789abcdef"[(total>>8)&0xf])
	out[2] = byte("0123456789abcdef"[(total>>4)&0xf])
	out[3] = byte("0123456789abcdef"[total&0xf])
	return append(out, data...)
}

func pktFlush() []byte { return []byte("0000") }

func pktEnd() []byte { return []byte("0001") }

// decodePktLines mirrors the Node decodePktLines exactly: it decodes a pkt-line
// stream into items (flush/delim/data) and returns the unconsumed remainder.
// It stops at the first malformed length. Lengths are parsed like JS
// parseInt(hex, 16): leading hex digits are consumed; no hex digits means
// invalid. len 0 = flush, len 1 = delim; a len of 2-3 yields a data item with
// an empty payload (subarray clamps), matching the Node behaviour.
type PktItem struct {
	Kind    string // "flush", "delim", "data"
	Payload []byte
}

func decodePktLines(buf []byte) ([]PktItem, []byte) {
	var items []PktItem
	i := 0
	for i+4 <= len(buf) {
		// Parse the 4-hex-digit length, JS parseInt style.
		val := 0
		digits := 0
		for j := 0; j < 4; j++ {
			c := buf[i+j]
			var d int
			switch {
			case c >= '0' && c <= '9':
				d = int(c - '0')
			case c >= 'a' && c <= 'f':
				d = int(c-'a') + 10
			case c >= 'A' && c <= 'F':
				d = int(c-'A') + 10
			default:
				d = -1
			}
			if d < 0 {
				break
			}
			val = val*16 + d
			digits++
		}
		if digits == 0 || val < 0 || val > 65520 {
			break
		}
		if val == 0 {
			items = append(items, PktItem{Kind: "flush"})
			i += 4
			continue
		}
		if val == 1 {
			items = append(items, PktItem{Kind: "delim"})
			i += 4
			continue
		}
		if i+val > len(buf) {
			break
		}
		var payload []byte
		if val > 4 {
			payload = buf[i+4 : i+val]
		} else {
			payload = []byte{}
		}
		items = append(items, PktItem{Kind: "data", Payload: payload})
		i += val
	}
	return items, buf[i:]
}

// packetLines is a convenience wrapper returning only the data payloads.
func packetLines(buf []byte) [][]byte {
	items, _ := decodePktLines(buf)
	var out [][]byte
	for _, it := range items {
		if it.Kind == "data" {
			out = append(out, it.Payload)
		}
	}
	return out
}

// sidebandChunks splits a payload into side-band-64k chunks of at most 65515
// bytes of data (each framed as pkt-line [1] + chunk).
func sidebandChunks(pack []byte) [][]byte {
	var chunks [][]byte
	for i := 0; i < len(pack); i += 65515 {
		end := i + 65515
		if end > len(pack) {
			end = len(pack)
		}
		framed := make([]byte, 0, end-i+1)
		framed = append(framed, 1)
		framed = append(framed, pack[i:end]...)
		chunks = append(chunks, pktLine(framed))
	}
	return chunks
}

const (
	packObjCommit = 1
	packObjTree   = 2
	packObjBlob   = 3
	packObjTag    = 4
	packObjOfs    = 6
	packObjRef    = 7
)

// writePackObject writes one object header + zlib payload into a pack.
func writePackObject(out *bytes.Buffer, typ int, data []byte) {
	size := len(data)
	// First byte: (size & 0x0f) | (type << 4) | (0x80 if more).
	first := (size & 0x0f) | (typ << 4)
	size >>= 4
	if size > 0 {
		first |= 0x80
	}
	out.WriteByte(byte(first))
	for size > 0 {
		b := size & 0x7f
		size >>= 7
		if size > 0 {
			b |= 0x80
		}
		out.WriteByte(byte(b))
	}
	zw := zlib.NewWriter(out)
	zw.Write(data)
	zw.Close()
}

// writePack serializes objects as a pack: header, objects sorted by oid, then
// the SHA-1 trailer of everything before it.
func writePack(objects map[string]GitObject) []byte {
	oids := make([]string, 0, len(objects))
	for oid := range objects {
		oids = append(oids, oid)
	}
	sort.Strings(oids)
	var out bytes.Buffer
	out.WriteString("PACK")
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], 2)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(oids)))
	out.Write(hdr[:])
	for _, oid := range oids {
		obj := objects[oid]
		typ := 3
		switch obj.Type {
		case ObjCommit:
			typ = packObjCommit
		case ObjTree:
			typ = packObjTree
		case ObjBlob:
			typ = packObjBlob
		case ObjTag:
			typ = packObjTag
		}
		writePackObject(&out, typ, obj.Data)
	}
	// Trailer: SHA-1 of the pack so far.
	sum := sha1.Sum(out.Bytes())
	out.Write(sum[:])
	return out.Bytes()
}

// readPackObject reads one object at a cursor: type, decompressed bytes, and
// the base reference for deltas (ofs offset or ref oid, empty otherwise).
// Returns (type, data, baseRef, nextPos, err); err is "" on success. The
// inflated size must match the header size, mirroring the Node readPack.
func readPackObject(data []byte, pos int) (int, []byte, string, int, string) {
	if pos >= len(data) {
		return 0, nil, "", pos, "pack truncated"
	}
	first := data[pos]
	pos++
	typ := int((first >> 4) & 0x07)
	size := int(first & 0x0f)
	shift := uint(4)
	for first&0x80 != 0 {
		if pos >= len(data) {
			return 0, nil, "", pos, "pack truncated"
		}
		b := data[pos]
		pos++
		size |= int(b&0x7f) << shift
		shift += 7
		first = b
	}
	baseRef := ""
	if typ == packObjOfs {
		if pos >= len(data) {
			return 0, nil, "", pos, "pack truncated"
		}
		b := data[pos]
		pos++
		offset := int(b & 0x7f)
		for b&0x80 != 0 {
			if pos >= len(data) {
				return 0, nil, "", pos, "pack truncated"
			}
			b = data[pos]
			pos++
			offset = ((offset + 1) << 7) | int(b&0x7f)
		}
		baseRef = "ofs:" + strconv.Itoa(offset)
	} else if typ == packObjRef {
		if pos+20 > len(data) {
			return 0, nil, "", pos, "pack truncated"
		}
		baseRef = "ref:" + hex.EncodeToString(data[pos:pos+20])
		pos += 20
	}
	// Decompress the zlib payload with a counting reader: one inflate gives
	// both the payload and the exact number of compressed bytes consumed.
	compressed := data[pos:]
	cr := &countingReader{r: bytes.NewReader(compressed)}
	zr, err := zlib.NewReader(cr)
	if err != nil {
		return 0, nil, "", pos, "pack truncated"
	}
	var out bytes.Buffer
	out.Grow(size)
	if _, err := io.Copy(&out, zr); err != nil {
		zr.Close()
		return 0, nil, "", pos, "pack truncated"
	}
	zr.Close()
	consumed := int(cr.n)
	if consumed < 0 {
		return 0, nil, "", pos, "zlib stream consumed no bytes"
	}
	if out.Len() != size {
		return 0, nil, "", pos, fmt.Sprintf("pack object size mismatch: header %d, inflated %d", size, out.Len())
	}
	return typ, out.Bytes(), baseRef, pos + consumed, ""
}

// countingReader counts the bytes read from its underlying reader. The zlib
// reader may buffer ahead, but after Close the count is the exact end of the
// zlib stream (adler32 included).

type countingReader struct {
	r *bytes.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// ReadByte makes countingReader an io.ByteReader, so Go's flate/zlib readers
// consume the stream byte-by-byte with NO read-ahead. Without it, flate wraps
// the reader in a bufio.Reader that over-reads past the zlib stream's end, and
// the byte count (hence the next object's offset) is wrong.
func (c *countingReader) ReadByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.n++
	}
	return b, err
}

// applyDelta applies a git delta to a base object, returning the result.
func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() (int, bool) {
		result := 0
		shift := uint(0)
		for pos < len(delta) {
			b := delta[pos]
			pos++
			result |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				return result, true
			}
		}
		return 0, false
	}
	srcSize, ok := readVarint()
	if !ok || srcSize != len(base) {
		return nil, fmt.Errorf("delta base size mismatch")
	}
	dstSize, ok := readVarint()
	if !ok {
		return nil, fmt.Errorf("delta target size missing")
	}
	out := make([]byte, 0, dstSize)
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			// Copy from base: offset/size are little-endian bit fields.
			offset, size := 0, 0
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, fmt.Errorf("delta truncated")
					}
					offset |= int(delta[pos]) << uint(8*i)
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, fmt.Errorf("delta truncated")
					}
					size |= int(delta[pos]) << uint(8*i)
					pos++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if offset+size > len(base) {
				return nil, fmt.Errorf("delta copy out of range")
			}
			out = append(out, base[offset:offset+size]...)
		} else if op != 0 {
			if pos+int(op) > len(delta) {
				return nil, fmt.Errorf("delta truncated")
			}
			out = append(out, delta[pos:pos+int(op)]...)
			pos += int(op)
		} else {
			return nil, fmt.Errorf("delta op 0 is reserved")
		}
	}
	if len(out) != dstSize {
		return nil, fmt.Errorf("delta produced %d bytes, expected %d", len(out), dstSize)
	}
	return out, nil
}

// readPack parses a pack into objects (delta-resolved), returning them keyed
// by oid. resolveBase is consulted for a REF_DELTA base that is not in the pack
// (a thin pack's base, which the receiver already has). Returns false on any
// malformed input: too short, bad magic, unsupported version, trailer checksum
// mismatch, bad zlib stream, size mismatch, unknown type code, or an
// unresolvable delta.
func readPack(data []byte, resolveBase func(oid string) (GitObject, bool)) (map[string]GitObject, string) {
	if len(data) < 32 {
		return nil, "pack too short"
	}
	if string(data[0:4]) != "PACK" {
		return nil, "bad pack magic"
	}
	version := binary.BigEndian.Uint32(data[4:8])
	if version != 2 && version != 3 {
		return nil, fmt.Sprintf("unsupported pack version %d", version)
	}
	count := int(binary.BigEndian.Uint32(data[8:12]))
	// Verify the trailer: SHA-1 of everything before the last 20 bytes.
	sum := sha1.Sum(data[:len(data)-20])
	if !bytes.Equal(sum[:], data[len(data)-20:]) {
		return nil, "pack trailer checksum mismatch"
	}
	pos := 12
	type rawObj struct {
		typ     int
		payload []byte
		base    string
		offset  int
	}
	raws := make([]rawObj, 0, count)
	for i := 0; i < count; i++ {
		start := pos
		typ, payload, base, next, perr := readPackObject(data, pos)
		if perr != "" {
			return nil, perr
		}
		raws = append(raws, rawObj{typ: typ, payload: payload, base: base, offset: start})
		pos = next
	}
	// Resolve: plain objects first, then deltas (iterate until all resolve).
	// resolvedOID[i] is the oid of the object produced by raws[i].
	result := map[string]GitObject{}
	byOffset := map[int]int{} // pack offset -> index in raws
	for i, r := range raws {
		byOffset[r.offset] = i
	}
	resolved := make([]bool, len(raws))
	resolvedOID := make([]string, len(raws))
	typeName := func(t int) (ObjectType, bool) {
		switch t {
		case packObjCommit:
			return ObjCommit, true
		case packObjTree:
			return ObjTree, true
		case packObjBlob:
			return ObjBlob, true
		case packObjTag:
			return ObjTag, true
		}
		return "", false
	}
	for i, r := range raws {
		if r.typ == packObjOfs || r.typ == packObjRef {
			continue
		}
		t, ok := typeName(r.typ)
		if !ok {
			return nil, fmt.Sprintf("unknown pack object type %d", r.typ)
		}
		oid := objectID(t, r.payload)
		result[oid] = GitObject{Type: t, Data: r.payload}
		resolved[i] = true
		resolvedOID[i] = oid
	}
	// Deltas: resolve iteratively (a delta may depend on another delta).
	for pass := 0; pass < len(raws)+2; pass++ {
		progress := false
		for i, r := range raws {
			if resolved[i] || (r.typ != packObjOfs && r.typ != packObjRef) {
				continue
			}
			var baseObj *GitObject
			if strings.HasPrefix(r.base, "ofs:") {
				off, _ := strconv.Atoi(strings.TrimPrefix(r.base, "ofs:"))
				baseOffset := r.offset - off
				if j, ok := byOffset[baseOffset]; ok && resolved[j] {
					o := result[resolvedOID[j]]
					baseObj = &o
				}
			} else if strings.HasPrefix(r.base, "ref:") {
				baseOID := strings.TrimPrefix(r.base, "ref:")
				if o, ok := result[baseOID]; ok {
					baseObj = &o
				} else if resolveBase != nil {
					if o, ok := resolveBase(baseOID); ok {
						baseObj = &o
					}
				}
			}
			if baseObj == nil {
				continue
			}
			applied, err := applyDelta(baseObj.Data, r.payload)
			if err != nil {
				return nil, err.Error()
			}
			oid := objectID(baseObj.Type, applied)
			result[oid] = GitObject{Type: baseObj.Type, Data: applied}
			resolved[i] = true
			resolvedOID[i] = oid
			progress = true
		}
		if !progress {
			break
		}
	}
	for _, r := range resolved {
		if !r {
			return nil, "delta base not found"
		}
	}
	return result, ""
}

// Git smart-HTTP surface (30 checks). Routes:
//   /{owner}/{repo}.git/info/refs?service=git-upload-pack   -> v2 advertisement
//   /{owner}/{repo}.git/info/refs?service=git-receive-pack  -> v0 advertisement
//   /{owner}/{repo}.git/git-upload-pack                     -> upload-pack v2 (or v0 fallback)
//   /{owner}/{repo}.git/git-receive-pack                    -> receive-pack v0
//
// Protocol notes (from sdk/conformance/git-client.ts, the spec):
// - v2 advertisement: `# service=...` + flush + `version 2` + capability lines + flush.
//   Capabilities: ls-refs=unborn, fetch=shallow wait-for-done, object-format=sha1, agent=...
// - v0 advertisement: `# service=...` + flush + `<oid> <ref> NUL <caps>` on the first line,
//   peeled tags as `<oid> <ref>^{}`, empty repo as `<zero> capabilities^{} NUL <caps>`.
//   Caps: symref=HEAD:refs/heads/<default> report-status side-band-64k delete-refs ofs-delta
//   object-format=sha1.
// - v2 fetch response: sections (acknowledgments, shallow-info, wanted-refs, packfile)
//   separated by delim, terminated by flush; the packfile travels over side-band-64k band 1.
// - receive-pack: command lines + NUL capabilities on the first, flush, then the raw
//   packfile. Response: report-status pkt-lines (unpack ok / ok <ref> / ng <ref> <reason>)
//   inside side-band-64k band 1, terminated by flush. Fast-forward only; non-FF is ng
//   per-ref and the other refs in a mixed push still apply.

package main

import (
	"sort"
	"strconv"
	"strings"
)

const agentName = "github-core-clone/1.0.0"

const zeroOID = "0000000000000000000000000000000000000000"

// encodePktLines frames a list of text lines (each ending with \n) as pkt-lines.
func encodePktLines(lines []string) []byte {
	var out []byte
	for _, l := range lines {
		out = append(out, pktLine([]byte(l))...)
	}
	return out
}

// concatBytes concatenates byte slices into one.
func concatBytes(parts [][]byte) []byte {
	total := 0
	for _, p := range parts {
		total += len(p)
	}
	out := make([]byte, 0, total)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// queryGet returns the first query value for a key, or "".
func queryGet(q map[string][]string, key string) string {
	if v, ok := q[key]; ok && len(v) > 0 {
		return v[0]
	}
	return ""
}

// ------------------------------------------------------------------ caches

// A per-repo cache of built fetch packs. The clone workload fetches the same tip
// 50 times concurrently (same wants, haves, depth); rebuilding + re-deflating the
// pack each time is the workload's main cost. The cache key is the fetch
// parameters and the entry is only valid for the repo generation it was built at,
// so a push can never serve a stale pack. Bounded to 8 entries per repo.
const packCacheMax = 8

func fetchCacheKey(wants, haves []string, depth int, shallowSent []string) string {
	w := append([]string{}, wants...)
	sort.Strings(w)
	h := append([]string{}, haves...)
	sort.Strings(h)
	s := append([]string{}, shallowSent...)
	sort.Strings(s)
	depthStr := ""
	if depth != 0 {
		depthStr = itoa(depth)
	}
	return strings.Join(w, ",") + "|" + strings.Join(h, ",") + "|" + depthStr + "|" + strings.Join(s, ",")
}

func cachedFetch(repo *Repo, req fetchRequest, build func() ([]byte, []string)) ([]byte, []string) {
	key := fetchCacheKey(req.wants, req.haves, req.depth, req.shallowSent)
	if hit, ok := repo.packCache[key]; ok && hit.Generation == repo.Generation {
		return hit.Pack, hit.Shallow
	}
	pack, shallow := build()
	if len(repo.packCache) >= packCacheMax {
		for k := range repo.packCache {
			delete(repo.packCache, k)
			break
		}
	}
	repo.packCache[key] = &fetchCacheEntry{Generation: repo.Generation, Pack: pack, Shallow: shallow}
	return pack, shallow
}

// refsFingerprint is a fingerprint of a repo's refs: `defaultBranch` + every
// `name=oid` pair, sorted. It covers everything listRefs()/wanted-refs depend
// on — a tag's peeled target is a pure function of the tag object's content,
// which its oid already fixes. Used as a cache key so a ref mutation can never
// serve a stale cached advertisement/ls-refs/fetch response. Memoized per
// Generation (which bumps on every refs mutation).
func refsFingerprint(repo *Repo) string {
	if repo.fpGen == repo.Generation {
		return repo.fp
	}
	names := make([]string, 0, len(repo.Refs))
	for name := range repo.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := []string{repo.DefaultBranch}
	for _, name := range names {
		parts = append(parts, name+"="+repo.Refs[name])
	}
	repo.fp = strings.Join(parts, "\n")
	repo.fpGen = repo.Generation
	return repo.fp
}

// sortedRefNames returns the repo's ref names, sorted. Memoized per
// Generation; callers only read the returned slice, never mutate it.
func sortedRefNames(repo *Repo) []string {
	if repo.sortedNamesGen == repo.Generation {
		return repo.sortedNames
	}
	names := make([]string, 0, len(repo.Refs))
	for name := range repo.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	repo.sortedNames = names
	repo.sortedNamesGen = repo.Generation
	return names
}

// parseTagTarget parses a tag object: returns the target oid (the `object` line).
func parseTagTarget(data []byte) string {
	text := string(data)
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "object ") {
			return strings.TrimSpace(line[7:])
		}
		if line == "" {
			break
		}
	}
	return ""
}

type refEntry struct {
	name      string
	oid       string
	peeled    string
	hasPeeled bool
	unborn    bool
}

// listRefs returns all refs of a repo, in a stable order: HEAD first, then
// heads, then tags.
func listRefs(repo *Repo) []refEntry {
	var out []refEntry
	defaultTip, hasDefault := repo.Refs["refs/heads/"+repo.DefaultBranch]
	if hasDefault {
		out = append(out, refEntry{name: "HEAD", oid: defaultTip})
	} else {
		out = append(out, refEntry{name: "HEAD", oid: zeroOID, unborn: true})
	}
	var heads, tags []refEntry
	for _, name := range sortedRefNames(repo) {
		oid := repo.Refs[name]
		if strings.HasPrefix(name, "refs/heads/") {
			heads = append(heads, refEntry{name: name, oid: oid})
		} else if strings.HasPrefix(name, "refs/tags/") {
			e := refEntry{name: name, oid: oid}
			if obj, ok := repo.Objects[oid]; ok && obj.Type == ObjTag {
				if target := parseTagTarget(obj.Data); target != "" {
					e.peeled = target
					e.hasPeeled = true
				}
			}
			tags = append(tags, e)
		}
	}
	out = append(out, heads...)
	out = append(out, tags...)
	// An unborn default branch is NOT listed as a ref: a fresh repo advertises
	// only HEAD (unborn, with its symref target). Listing `refs/heads/<default>`
	// as unborn makes clients treat the zero oid as a real tip and fetch it.
	return out
}

// v2Advertisement is the v2 advertisement body for git-upload-pack. The bytes
// are constant (fixed capability list, fixed agent string), so they are built
// once at startup instead of re-encoded per info/refs request.
var v2Advertisement = func() []byte {
	var out []byte
	out = append(out, pktLine([]byte("# service=git-upload-pack\n"))...)
	out = append(out, pktFlush()...)
	out = append(out, pktLine([]byte("version 2\n"))...)
	out = append(out, pktLine([]byte("ls-refs=unborn\n"))...)
	out = append(out, pktLine([]byte("fetch=shallow wait-for-done\n"))...)
	out = append(out, pktLine([]byte("object-format=sha1\n"))...)
	out = append(out, pktLine([]byte("agent="+agentName+"\n"))...)
	out = append(out, pktFlush()...)
	return out
}()

// v0Advertisement is the v0 advertisement body for git-receive-pack. Only the
// tip oid varies per repo state. Memoized per Generation: the bytes are a pure
// function of the refs (+ DefaultBranch, fixed at creation).
func v0Advertisement(repo *Repo) []byte {
	if repo.v0AdvGen == repo.Generation {
		return repo.v0Adv
	}
	adv := buildV0Advertisement(repo)
	repo.v0Adv = adv
	repo.v0AdvGen = repo.Generation
	return adv
}

func buildV0Advertisement(repo *Repo) []byte {
	caps := "symref=HEAD:refs/heads/" + repo.DefaultBranch + " report-status side-band-64k delete-refs ofs-delta object-format=sha1 agent=" + agentName
	refs := listRefs(repo)
	var sb strings.Builder
	sb.Write(pktLine([]byte("# service=git-receive-pack\n")))
	sb.WriteString("0000")
	hasRefs := false
	for _, r := range refs {
		if !r.unborn {
			hasRefs = true
			break
		}
	}
	if !hasRefs {
		sb.Write(pktLine([]byte(zeroOID + " capabilities^{}\x00" + caps + "\n")))
		sb.WriteString("0000")
		return []byte(sb.String())
	}
	for i, ref := range refs {
		if ref.unborn {
			continue
		}
		suffix := ""
		if i == 0 {
			suffix = "\x00" + caps
		}
		sb.Write(pktLine([]byte(ref.oid + " " + ref.name + suffix + "\n")))
		if ref.hasPeeled {
			sb.Write(pktLine([]byte(ref.peeled + " " + ref.name + "^{}\n")))
		}
	}
	sb.WriteString("0000")
	return []byte(sb.String())
}

// ------------------------------------------------------------------ object collection

type collectResult struct {
	objects map[string]GitObject
	// Commits at the depth boundary (their parents were cut).
	shallow []string
}

// collectObjects collects the objects reachable from `tips`, excluding anything
// reachable from `haves`. `depth` limits commit depth (1 = only the tips
// themselves); 0 means no limit. `shallowBoundaries` are the client's current
// shallow boundaries: the have-walk stops at them (the client has the boundary
// commit and its tree, but NOT its parents — a deepen fetch must deliver those
// parents). The returned `shallow` list is the set of boundary commits.
func collectObjects(repo *Repo, tips, haves []string, depth int, shallowBoundaries []string) collectResult {
	boundarySet := map[string]bool{}
	for _, s := range shallowBoundaries {
		boundarySet[s] = true
	}
	haveReachable := map[string]bool{}
	if len(haves) > 0 {
		stack := append([]string{}, haves...)
		for len(stack) > 0 {
			oid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if haveReachable[oid] {
				continue
			}
			haveReachable[oid] = true
			obj, ok := repo.Objects[oid]
			if !ok {
				continue
			}
			if obj.Type == ObjCommit {
				commit := parseCommit(obj.Data)
				stack = append(stack, commit.Tree)
				// A shallow boundary's parents are NOT held by the client: stop there.
				if !boundarySet[oid] {
					stack = append(stack, commit.Parents...)
				}
			} else if obj.Type == ObjTree {
				for _, e := range parseTree(obj.Data) {
					stack = append(stack, e.OID)
				}
			} else if obj.Type == ObjTag {
				if target := parseTagTarget(obj.Data); target != "" {
					stack = append(stack, target)
				}
			}
		}
	}

	objects := map[string]GitObject{}
	var shallow []string
	add := func(oid string) {
		if haveReachable[oid] {
			return
		}
		if _, ok := objects[oid]; ok {
			return
		}
		if obj, ok := repo.Objects[oid]; ok {
			objects[oid] = obj
		}
	}
	var addTree func(treeOID string)
	addTree = func(treeOID string) {
		add(treeOID)
		obj, ok := repo.Objects[treeOID]
		if !ok || obj.Type != ObjTree {
			return
		}
		for _, e := range parseTree(obj.Data) {
			if e.Type == ObjTree {
				addTree(e.OID)
			} else {
				add(e.OID)
			}
		}
	}

	// BFS over commits with depth tracking (depth 0 = the tips). Index pointer,
	// not a shift: same FIFO order, no O(n) re-indexing per pop.
	type queueItem struct {
		oid   string
		depth int
	}
	queue := make([]queueItem, 0, len(tips))
	for _, tip := range tips {
		queue = append(queue, queueItem{oid: tip, depth: 0})
	}
	seenCommits := map[string]bool{}
	qi := 0
	for qi < len(queue) {
		item := queue[qi]
		qi++
		if seenCommits[item.oid] {
			continue
		}
		seenCommits[item.oid] = true
		obj, ok := repo.Objects[item.oid]
		if !ok {
			continue
		}
		if obj.Type == ObjTag {
			add(item.oid)
			if target := parseTagTarget(obj.Data); target != "" {
				queue = append(queue, queueItem{oid: target, depth: item.depth})
			}
			continue
		}
		if obj.Type != ObjCommit {
			add(item.oid)
			continue
		}
		isBoundary := depth != 0 && item.depth >= depth-1
		if isBoundary {
			shallow = append(shallow, item.oid)
		}
		add(item.oid)
		commit := parseCommit(obj.Data)
		addTree(commit.Tree)
		if !isBoundary {
			for _, p := range commit.Parents {
				queue = append(queue, queueItem{oid: p, depth: item.depth + 1})
			}
		}
	}
	// Any tip that is a bare tree/blob (not a commit) is included directly.
	for _, tip := range tips {
		if obj, ok := repo.Objects[tip]; ok {
			if obj.Type == ObjTree {
				addTree(tip)
			} else if obj.Type == ObjBlob {
				add(tip)
			}
		}
	}
	return collectResult{objects: objects, shallow: shallow}
}

// isAncestor reports whether `ancestor` is reachable from `descendant`.
func isAncestor(repo *Repo, ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := map[string]bool{}
	stack := []string{descendant}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj, ok := repo.Objects[oid]
		if !ok || obj.Type != ObjCommit {
			continue
		}
		for _, p := range parseCommit(obj.Data).Parents {
			if p == ancestor {
				return true
			}
			stack = append(stack, p)
		}
	}
	return false
}

// buildPack builds a pack from a collected object set (sorted by oid, so the
// bytes are deterministic). Nil when there is nothing to send.
func buildPack(objects map[string]GitObject) []byte {
	if len(objects) == 0 {
		return nil
	}
	return writePack(objects)
}

// ------------------------------------------------------------------ upload-pack v2

func handleLsRefs(repo *Repo, lines []string) []byte {
	peel, symrefs, unborn := false, false, false
	var prefixes []string
	for _, line := range lines {
		t := strings.TrimSuffix(line, "\n")
		switch {
		case t == "peel":
			peel = true
		case t == "symrefs":
			symrefs = true
		case t == "unborn":
			unborn = true
		case strings.HasPrefix(t, "ref-prefix "):
			prefixes = append(prefixes, t[len("ref-prefix "):])
		}
		// agent= / object-format= are accepted and ignored.
	}
	flagStr := ""
	for _, b := range []bool{peel, symrefs, unborn} {
		if b {
			flagStr += "1"
		} else {
			flagStr += "0"
		}
	}
	key := refsFingerprint(repo) + "|" + flagStr + "|" + strings.Join(prefixes, ",")
	if hit, ok := repo.lsCache[key]; ok {
		return hit
	}
	refs := listRefs(repo)
	var parts [][]byte
	for _, ref := range refs {
		if !unborn && ref.unborn {
			continue
		}
		if len(prefixes) > 0 {
			match := false
			for _, p := range prefixes {
				if strings.HasPrefix(ref.name, p) {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		var attrs []string
		if ref.name == "HEAD" && symrefs {
			attrs = append(attrs, "symref-target:refs/heads/"+repo.DefaultBranch)
		}
		if ref.hasPeeled && peel {
			attrs = append(attrs, "peeled:"+ref.peeled)
		}
		first := ref.oid
		if ref.unborn {
			first = "unborn"
		}
		line := first + " " + ref.name
		if len(attrs) > 0 {
			line += " " + strings.Join(attrs, " ")
		}
		line += "\n"
		parts = append(parts, pktLine([]byte(line)))
	}
	parts = append(parts, pktFlush())
	body := concatBytes(parts)
	if len(repo.lsCache) >= 16 {
		for k := range repo.lsCache {
			delete(repo.lsCache, k)
			break
		}
	}
	repo.lsCache[key] = body
	return body
}

type fetchRequest struct {
	wants       []string
	haves       []string
	shallowSent []string
	depth       int // 0 = unset
	thinPack    bool
}

func parseV2Fetch(lines []string) fetchRequest {
	var req fetchRequest
	for _, line := range lines {
		t := strings.TrimSuffix(line, "\n")
		switch {
		case strings.HasPrefix(t, "want "):
			req.wants = append(req.wants, strings.TrimSpace(t[5:]))
		case strings.HasPrefix(t, "have "):
			req.haves = append(req.haves, strings.TrimSpace(t[5:]))
		case strings.HasPrefix(t, "shallow "):
			req.shallowSent = append(req.shallowSent, strings.TrimSpace(t[8:]))
		case strings.HasPrefix(t, "deepen "):
			if n, err := strconv.ParseFloat(strings.TrimSpace(t[7:]), 64); err == nil && n > 0 {
				req.depth = int(n)
			}
		case t == "thin-pack":
			req.thinPack = true
		}
		// deepen-since / deepen-not / filter / no-progress / ofs-delta / done / agent /
		// object-format are accepted; only deepen is honoured.
	}
	return req
}

func handleFetchV2(repo *Repo, req fetchRequest) []byte {
	// A want the server does not have is a side-band error (the client's
	// missing-object path).
	for _, want := range req.wants {
		if _, ok := repo.Objects[want]; !ok {
			errText := "object not found: " + want + "\n"
			framed := append([]byte{3}, []byte(errText)...)
			var out []byte
			out = append(out, pktLine([]byte("packfile\n"))...)
			out = append(out, pktLine(framed)...)
			out = append(out, pktFlush()...)
			return out
		}
	}
	if len(req.wants) == 0 {
		var out []byte
		out = append(out, pktLine([]byte("acknowledgments\n"))...)
		out = append(out, pktLine([]byte("NAK\n"))...)
		out = append(out, pktEnd()...)
		out = append(out, pktFlush()...)
		return out
	}

	// The full response is a pure function of the fetch parameters + the refs,
	// so cache the fully-framed bytes. Error/empty paths above bypass the cache
	// on purpose: an unknown want may exist after a later push.
	key := fetchCacheKey(req.wants, req.haves, req.depth, req.shallowSent) + "|" + refsFingerprint(repo)
	if hit, ok := repo.respCache[key]; ok && hit.Generation == repo.Generation {
		return hit.Body
	}
	body := buildFetchV2Response(repo, req)
	if len(repo.respCache) >= 8 {
		for k := range repo.respCache {
			delete(repo.respCache, k)
			break
		}
	}
	repo.respCache[key] = &respCacheEntry{Generation: repo.Generation, Body: body}
	return body
}

// buildFetchV2Response builds the v2 fetch response body (acknowledgments +
// shallow-info + wanted-refs + packfile).
func buildFetchV2Response(repo *Repo, req fetchRequest) []byte {
	pack, shallow := cachedFetch(repo, req, func() ([]byte, []string) {
		collected := collectObjects(repo, req.wants, req.haves, req.depth, req.shallowSent)
		return buildPack(collected.objects), collected.shallow
	})
	var parts [][]byte

	// acknowledgments: ACK every have that is an ancestor of (or equal to) a want.
	parts = append(parts, pktLine([]byte("acknowledgments\n")))
	acked := false
	for _, have := range req.haves {
		if _, ok := repo.Objects[have]; ok {
			for _, w := range req.wants {
				if isAncestor(repo, have, w) {
					parts = append(parts, pktLine([]byte("ACK "+have+"\n")))
					acked = true
					break
				}
			}
		}
	}
	if !acked {
		parts = append(parts, pktLine([]byte("NAK\n")))
	}
	parts = append(parts, pktEnd())

	// shallow-info: the new boundary, plus unshallow for boundaries the client
	// sent that are now interior (their parents are included at this depth).
	var unshallow []string
	if req.depth != 0 {
		for _, sent := range req.shallowSent {
			found := false
			for _, s := range shallow {
				if s == sent {
					found = true
					break
				}
			}
			if !found {
				unshallow = append(unshallow, sent)
			}
		}
	}
	if len(shallow) > 0 || len(unshallow) > 0 {
		parts = append(parts, pktLine([]byte("shallow-info\n")))
		for _, s := range shallow {
			parts = append(parts, pktLine([]byte("shallow "+s+"\n")))
		}
		for _, u := range unshallow {
			parts = append(parts, pktLine([]byte("unshallow "+u+"\n")))
		}
		parts = append(parts, pktEnd())
	}

	// wanted-refs: the refs whose tip is a want.
	type wantedRef struct{ oid, ref string }
	var wanted []wantedRef
	names := sortedRefNames(repo)
	for _, want := range req.wants {
		for _, name := range names {
			if repo.Refs[name] == want {
				wanted = append(wanted, wantedRef{oid: want, ref: name})
			}
		}
	}
	if len(wanted) > 0 {
		parts = append(parts, pktLine([]byte("wanted-refs\n")))
		for _, w := range wanted {
			parts = append(parts, pktLine([]byte(w.oid+" "+w.ref+"\n")))
		}
		parts = append(parts, pktEnd())
	}

	// packfile: only when there is something to send.
	if pack != nil {
		parts = append(parts, pktLine([]byte("packfile\n")))
		parts = append(parts, sidebandChunks(pack)...)
	}
	parts = append(parts, pktFlush())
	return concatBytes(parts)
}

// ------------------------------------------------------------------ upload-pack v0 (fallback)

func handleFetchV0(repo *Repo, body []byte) []byte {
	items, _ := decodePktLines(body)
	var wants, haves, shallowSent []string
	depth := 0
	for _, item := range items {
		if item.Kind == "flush" {
			continue
		}
		if item.Kind != "data" {
			continue
		}
		t := strings.TrimSuffix(string(item.Payload), "\n")
		switch {
		case strings.HasPrefix(t, "want "):
			wants = append(wants, strings.Split(t[5:], " ")[0])
		case strings.HasPrefix(t, "have "):
			haves = append(haves, strings.TrimSpace(t[5:]))
		case strings.HasPrefix(t, "shallow "):
			shallowSent = append(shallowSent, strings.TrimSpace(t[8:]))
		case strings.HasPrefix(t, "deepen "):
			if n, err := strconv.ParseFloat(strings.TrimSpace(t[7:]), 64); err == nil && n > 0 {
				depth = int(n)
			}
		}
	}
	var parts [][]byte
	for _, want := range wants {
		if _, ok := repo.Objects[want]; !ok {
			errText := "object not found: " + want + "\n"
			framed := append([]byte{3}, []byte(errText)...)
			var out []byte
			out = append(out, pktLine(framed)...)
			out = append(out, pktFlush()...)
			return out
		}
	}
	acked := false
	for _, have := range haves {
		if _, ok := repo.Objects[have]; ok {
			for _, w := range wants {
				if isAncestor(repo, have, w) {
					parts = append(parts, pktLine([]byte("ACK "+have+"\n")))
					acked = true
					break
				}
			}
		}
	}
	if !acked {
		parts = append(parts, pktLine([]byte("NAK\n")))
	}
	req := fetchRequest{wants: wants, haves: haves, shallowSent: shallowSent, depth: depth}
	pack, shallow := cachedFetch(repo, req, func() ([]byte, []string) {
		collected := collectObjects(repo, wants, haves, depth, shallowSent)
		return buildPack(collected.objects), collected.shallow
	})
	for _, s := range shallow {
		parts = append(parts, pktLine([]byte("shallow "+s+"\n")))
	}
	if pack != nil {
		parts = append(parts, sidebandChunks(pack)...)
	}
	parts = append(parts, pktFlush())
	return concatBytes(parts)
}

// ------------------------------------------------------------------ receive-pack v0

type pushCommand struct {
	oldOid string
	newOid string
	ref    string
}

func handleReceivePack(repo *Repo, body []byte) []byte {
	items, rest := decodePktLines(body)
	var commands []pushCommand
	for _, item := range items {
		if item.Kind == "flush" {
			break
		}
		if item.Kind != "data" {
			continue
		}
		text := string(item.Payload)
		if nul := strings.IndexByte(text, 0); nul >= 0 {
			text = text[:nul]
		}
		line := strings.TrimSuffix(text, "\n")
		parts := strings.Split(line, " ")
		if len(parts) < 3 {
			continue
		}
		commands = append(commands, pushCommand{oldOid: parts[0], newOid: parts[1], ref: strings.Join(parts[2:], " ")})
	}

	// The packfile follows the flush. A delete-only push sends none.
	unpackOk := true
	unpackDetail := "ok"
	mutated := false
	if len(rest) > 0 {
		objects, perr := readPack(rest, func(oid string) (GitObject, bool) {
			obj, ok := repo.Objects[oid]
			return obj, ok
		})
		if perr != "" {
			unpackOk = false
			unpackDetail = perr
		} else {
			for oid, obj := range objects {
				if _, ok := repo.Objects[oid]; !ok {
					repo.Objects[oid] = obj
					mutated = true
				}
			}
		}
	}

	unpackLine := "unpack ok\n"
	if !unpackOk {
		unpackLine = "unpack " + unpackDetail + "\n"
	}
	report := []string{unpackLine}
	if unpackOk {
		for _, cmd := range commands {
			current, hasCurrent := repo.Refs[cmd.ref]
			if cmd.newOid == zeroOID {
				// Delete.
				if !hasCurrent {
					report = append(report, "ng "+cmd.ref+" ref does not exist\n")
				} else if current != cmd.oldOid {
					report = append(report, "ng "+cmd.ref+" old value does not match\n")
				} else {
					delete(repo.Refs, cmd.ref)
					mutated = true
					report = append(report, "ok "+cmd.ref+"\n")
				}
				continue
			}
			if cmd.oldOid == zeroOID {
				// Create.
				if hasCurrent {
					report = append(report, "ng "+cmd.ref+" already exists\n")
					continue
				}
				if _, ok := repo.Objects[cmd.newOid]; !ok {
					report = append(report, "ng "+cmd.ref+" object not found\n")
					continue
				}
				repo.Refs[cmd.ref] = cmd.newOid
				mutated = true
				report = append(report, "ok "+cmd.ref+"\n")
				continue
			}
			// Update: the ref must be at old, and new must be a descendant of old.
			if !hasCurrent {
				report = append(report, "ng "+cmd.ref+" ref does not exist\n")
				continue
			}
			if current != cmd.oldOid {
				report = append(report, "ng "+cmd.ref+" old value does not match\n")
				continue
			}
			if _, ok := repo.Objects[cmd.newOid]; !ok {
				report = append(report, "ng "+cmd.ref+" object not found\n")
				continue
			}
			oldObj, oldOk := repo.Objects[cmd.oldOid]
			newObj, newOk := repo.Objects[cmd.newOid]
			bothCommits := oldOk && newOk && oldObj.Type == ObjCommit && newObj.Type == ObjCommit
			if bothCommits && !isAncestor(repo, cmd.oldOid, cmd.newOid) {
				report = append(report, "ng "+cmd.ref+" non-fast-forward\n")
				continue
			}
			if !bothCommits && cmd.oldOid != cmd.newOid {
				report = append(report, "ng "+cmd.ref+" non-fast-forward\n")
				continue
			}
			repo.Refs[cmd.ref] = cmd.newOid
			mutated = true
			report = append(report, "ok "+cmd.ref+"\n")
		}
	} else {
		for _, cmd := range commands {
			report = append(report, "ng "+cmd.ref+" unpack failed\n")
		}
	}
	if mutated {
		repo.Generation++
	}

	// Frame the report-status body into side-band chunks.
	reportBytes := encodePktLines(report)
	var out []byte
	out = append(out, concatBytes(sidebandChunks(reportBytes))...)
	out = append(out, pktFlush()...)
	return out
}

// ------------------------------------------------------------------ router

// handleGit is the git smart-HTTP handler. Returns (response, handled).
func handleGit(store *Store, req *Request, segments []string) (Response, bool) {
	// segments: [owner, repo.git, ...rest] — the caller strips the leading slash.
	if len(segments) < 2 {
		return Response{}, false
	}
	owner := segments[0]
	repoGit := segments[1]
	if !strings.HasSuffix(repoGit, ".git") {
		return Response{}, false
	}
	repoName := repoGit[:len(repoGit)-4]
	tail := segments[2:]

	// Resolve the repo first (unknown repo = 404), then auth (401) — same order as REST.
	if owner != ViewerLogin {
		return jsonBytesResponse(404, notFoundJSON), true
	}
	repo := store.GetRepo(owner, repoName)
	if repo == nil {
		return jsonBytesResponse(404, notFoundJSON), true
	}
	if !isAuthorized(req.Headers) {
		return jsonResponse(401, map[string]string{"message": "Requires authentication"}), true
	}

	// info/refs
	if len(tail) == 2 && tail[0] == "info" && tail[1] == "refs" && req.Method == "GET" {
		service := queryGet(req.QueryMap(), "service")
		if service == "git-upload-pack" {
			return bytesResponse(200, v2Advertisement, "application/x-git-upload-pack-advertisement"), true
		}
		if service == "git-receive-pack" {
			return bytesResponse(200, v0Advertisement(repo), "application/x-git-receive-pack-advertisement"), true
		}
		return jsonBytesResponse(404, notFoundJSON), true
	}

	// git-upload-pack
	if len(tail) == 1 && tail[0] == "git-upload-pack" && req.Method == "POST" {
		if req.Oversized {
			return jsonBytesResponse(413, []byte("{\"message\":\"Request body too large\"}\n")), true
		}
		if req.Headers["git-protocol"] == "version=2" {
			items, _ := decodePktLines(req.Body)
			var lines []string
			command := ""
			for _, item := range items {
				if item.Kind == "flush" {
					break
				}
				if item.Kind == "delim" {
					continue
				}
				if item.Kind != "data" {
					continue
				}
				text := string(item.Payload)
				if strings.HasPrefix(text, "command=") {
					command = strings.TrimSuffix(text[8:], "\n")
					continue
				}
				lines = append(lines, text)
			}
			switch command {
			case "ls-refs":
				return bytesResponse(200, handleLsRefs(repo, lines), "application/x-git-upload-pack-result"), true
			case "fetch":
				return bytesResponse(200, handleFetchV2(repo, parseV2Fetch(lines)), "application/x-git-upload-pack-result"), true
			default:
				return jsonBytesResponse(400, []byte("{\"message\":\"Unknown command\"}\n")), true
			}
		}
		// v0 fallback.
		return bytesResponse(200, handleFetchV0(repo, req.Body), "application/x-git-upload-pack-result"), true
	}

	// git-receive-pack
	if len(tail) == 1 && tail[0] == "git-receive-pack" && req.Method == "POST" {
		if req.Oversized {
			return jsonBytesResponse(413, []byte("{\"message\":\"Request body too large\"}\n")), true
		}
		return bytesResponse(200, handleReceivePack(repo, req.Body), "application/x-git-receive-pack-result"), true
	}

	return Response{}, false
}

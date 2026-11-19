// The forge's single in-memory store. Every surface (git smart HTTP, REST v3,
// GraphQL, MCP) reads and writes this one store, so a repo, issue, pull request,
// comment or merge created on one surface is immediately visible on the others
// with the same numbers and ids.
//
// Determinism: object ids are SHA-1s of content; database ids are a creation-order
// counter; global ids are base64url of a typed key (opaque, stable across reads,
// never the plain database id or number); server-generated timestamps use the
// trial-now helper. No clock, no unseeded random anywhere in the store.
//
// Concurrency: the store carries one mutex. The HTTP layer holds it for the whole
// dispatch of one request (the Node original is single-threaded; one mutex per
// request reproduces that ordering exactly, and the timed workloads are
// sequential except clone, whose fetches are cache hits after the first).

package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
)

type ObjectType string

const (
	ObjCommit ObjectType = "commit"
	ObjTree   ObjectType = "tree"
	ObjBlob   ObjectType = "blob"
	ObjTag    ObjectType = "tag"
)

type GitObject struct {
	Type ObjectType
	Data []byte
}

type Label struct {
	Name  string
	Color string
}

type Comment struct {
	ID   int
	Body string
}

type IssueState string

const (
	IssueOpen   IssueState = "open"
	IssueClosed IssueState = "closed"
)

type Issue struct {
	Number        int
	Title         string
	Body          string
	State         IssueState
	Labels        []string
	Comments      []Comment
	NextCommentID int
}

type PullFile struct {
	Filename  string
	Status    string
	Additions int
	Deletions int
	Changes   int
}

type PullState string

const (
	PullOpen   PullState = "open"
	PullClosed PullState = "closed"
)

type Pull struct {
	Number   int
	Title    string
	Body     string
	State    PullState
	HeadRef  string
	BaseRef  string
	HeadSha  string
	BaseSha  string
	Merged   bool
	MergedAt *string
	MergeSha *string
	Files    []PullFile
}

type Repo struct {
	DatabaseID    int
	Name          string
	Owner         string
	Description   string
	Private       bool
	DefaultBranch string
	// Ref name (refs/heads/...) -> commit oid.
	Refs map[string]string
	// Git objects, keyed by lowercase hex oid.
	Objects   map[string]GitObject
	Labels    map[string]Label
	Issues    map[int]*Issue
	Pulls     map[int]*Pull
	NextIssue int
	NextPull  int
	// Creation order, for stable list ordering.
	Seq int
	// Bumped on every object/ref mutation; the git pack cache keys on it.
	Generation int
	// Per-repo fetch/ls-refs caches (guarded by the store mutex).
	packCache map[string]*fetchCacheEntry
	lsCache   map[string][]byte
	respCache map[string]*respCacheEntry
}

type fetchCacheEntry struct {
	Generation int
	Pack       []byte // nil = no pack
	Shallow    []string
}

type respCacheEntry struct {
	Generation int
	Body       []byte
}

type TreeEntry struct {
	Mode string
	Name string
	OID  string
	Type ObjectType
}

const EmptyTreeOID = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// objectID is the SHA-1 of a git object: `<type> <size>\0` + bytes.
func objectID(typ ObjectType, data []byte) string {
	h := sha1.New()
	h.Write([]byte(string(typ) + " " + itoa(len(data)) + "\x00"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// putObject stores an object, returning its oid (idempotent). A new object
// bumps the repo generation.
func putObject(repo *Repo, typ ObjectType, data []byte) string {
	oid := objectID(typ, data)
	if _, ok := repo.Objects[oid]; !ok {
		repo.Objects[oid] = GitObject{Type: typ, Data: data}
		repo.Generation++
	}
	return oid
}

// encodeTree encodes a tree object from its entries (sorted by git's byte order).
func encodeTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		ki := sorted[i].Name
		if sorted[i].Type == ObjTree {
			ki += "/"
		}
		kj := sorted[j].Name
		if sorted[j].Type == ObjTree {
			kj += "/"
		}
		return bytes.Compare([]byte(ki), []byte(kj)) < 0
	})
	var out []byte
	for _, e := range sorted {
		out = append(out, []byte(e.Mode+" "+e.Name+"\x00")...)
		raw, _ := hex.DecodeString(e.OID)
		out = append(out, raw...)
	}
	return out
}

// parseTree parses a tree object into its entries.
func parseTree(data []byte) []TreeEntry {
	var entries []TreeEntry
	i := 0
	for i < len(data) {
		space := bytes.IndexByte(data[i:], ' ')
		if space < 0 {
			break
		}
		space += i
		nul := bytes.IndexByte(data[space:], 0)
		if nul < 0 {
			break
		}
		nul += space
		mode := string(data[i:space])
		name := string(data[space+1 : nul])
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		typ := ObjBlob
		if mode == "40000" {
			typ = ObjTree
		}
		entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: oid, Type: typ})
		i = nul + 21
	}
	return entries
}

type CommitData struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

// encodeCommit encodes a commit object.
func encodeCommit(c CommitData) []byte {
	var sb strings.Builder
	sb.WriteString("tree " + c.Tree + "\n")
	for _, p := range c.Parents {
		sb.WriteString("parent " + p + "\n")
	}
	sb.WriteString("author " + c.Author + "\ncommitter " + c.Committer + "\n\n" + c.Message)
	return []byte(sb.String())
}

// parseCommit parses a commit object.
func parseCommit(data []byte) CommitData {
	text := string(data)
	headerEnd := strings.Index(text, "\n\n")
	header := text
	message := ""
	if headerEnd >= 0 {
		header = text[:headerEnd]
		message = text[headerEnd+2:]
	}
	var c CommitData
	for _, line := range strings.Split(header, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			c.Tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			c.Parents = append(c.Parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			c.Author = line[7:]
		case strings.HasPrefix(line, "committer "):
			c.Committer = line[10:]
		}
	}
	c.Message = message
	return c
}

// serverIdentity is the fixed server identity used for squash-merge commits.
func serverIdentity() string {
	return "forge <forge@example.invalid> " + itoa(int(trialNow)) + " +0000"
}

// splitPath splits a path into segments, ignoring empty pieces.
func splitPath(path string) []string {
	parts := strings.Split(path, "/")
	out := parts[:0]
	for _, s := range parts {
		if s != "" && s != "." {
			out = append(out, s)
		}
	}
	return out
}

// flattenTree reads a tree's entries recursively into a flat map:
// path -> {oid, mode}. Only blobs are returned.
func flattenTree(repo *Repo, treeOID string, prefix string, out map[string]struct {
	OID  string
	Mode string
}) map[string]struct {
	OID  string
	Mode string
} {
	if out == nil {
		out = map[string]struct {
			OID  string
			Mode string
		}{}
	}
	obj, ok := repo.Objects[treeOID]
	if !ok || obj.Type != ObjTree {
		return out
	}
	for _, e := range parseTree(obj.Data) {
		path := e.Name
		if prefix != "" {
			path = prefix + "/" + e.Name
		}
		if e.Type == ObjTree {
			flattenTree(repo, e.OID, path, out)
		} else {
			out[path] = struct {
				OID  string
				Mode string
			}{OID: e.OID, Mode: e.Mode}
		}
	}
	return out
}

// setTreePath sets one path in a tree, creating intermediate trees, and returns
// the new tree oid. value is nil to delete the path.
func setTreePath(repo *Repo, treeOID string, segments []string, value *struct {
	OID  string
	Mode string
}) string {
	var entries []TreeEntry
	if obj, ok := repo.Objects[treeOID]; ok && obj.Type == ObjTree {
		entries = parseTree(obj.Data)
	}
	head := segments[0]
	rest := segments[1:]
	idx := -1
	for i, e := range entries {
		if e.Name == head {
			idx = i
			break
		}
	}
	if len(rest) == 0 {
		if value == nil {
			if idx >= 0 {
				entries = append(entries[:idx], entries[idx+1:]...)
			}
		} else if idx >= 0 {
			entries[idx] = TreeEntry{Mode: value.Mode, Name: head, OID: value.OID, Type: ObjBlob}
		} else {
			entries = append(entries, TreeEntry{Mode: value.Mode, Name: head, OID: value.OID, Type: ObjBlob})
		}
	} else {
		childOID := EmptyTreeOID
		if idx >= 0 && entries[idx].Type == ObjTree {
			childOID = entries[idx].OID
		}
		newChild := setTreePath(repo, childOID, rest, value)
		childObj, ok := repo.Objects[newChild]
		isEmpty := ok && childObj.Type == ObjTree && len(childObj.Data) == 0
		if isEmpty {
			if idx >= 0 {
				entries = append(entries[:idx], entries[idx+1:]...)
			}
		} else if idx >= 0 {
			entries[idx] = TreeEntry{Mode: "40000", Name: head, OID: newChild, Type: ObjTree}
		} else {
			entries = append(entries, TreeEntry{Mode: "40000", Name: head, OID: newChild, Type: ObjTree})
		}
	}
	data := encodeTree(entries)
	return putObject(repo, ObjTree, data)
}

// readTreePath reads a blob's bytes at a path in a tree, or nil when absent.
func readTreePath(repo *Repo, treeOID string, segments []string) *struct {
	OID  string
	Mode string
} {
	flat := flattenTree(repo, treeOID, "", nil)
	if e, ok := flat[strings.Join(segments, "/")]; ok {
		v := e
		return &v
	}
	return nil
}

// listTreeDir lists the direct children of a directory path in a tree.
func listTreeDir(repo *Repo, treeOID string, segments []string) []struct {
	Name string
	Path string
	OID  string
	Type string
} {
	current := treeOID
	for _, seg := range segments {
		obj, ok := repo.Objects[current]
		if !ok || obj.Type != ObjTree {
			return nil
		}
		found := false
		for _, e := range parseTree(obj.Data) {
			if e.Name == seg {
				if e.Type != ObjTree {
					return nil
				}
				current = e.OID
				found = true
				break
			}
		}
		if !found {
			return nil
		}
	}
	obj, ok := repo.Objects[current]
	if !ok || obj.Type != ObjTree {
		return nil
	}
	prefix := strings.Join(segments, "/")
	var out []struct {
		Name string
		Path string
		OID  string
		Type string
	}
	for _, e := range parseTree(obj.Data) {
		typ := "file"
		if e.Type == ObjTree {
			typ = "dir"
		}
		path := e.Name
		if prefix != "" {
			path = prefix + "/" + e.Name
		}
		out = append(out, struct {
			Name string
			Path string
			OID  string
			Type string
		}{Name: e.Name, Path: path, OID: e.OID, Type: typ})
	}
	return out
}

// lineCount counts lines in a buffer (a trailing newline adds no empty line).
func lineCount(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	lines := strings.Split(string(data), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return len(lines)
}

// lineDiff is a cheap line-diff: additions = lines in new not in old,
// deletions = lines in old not in new.
func lineDiff(oldData, newData []byte) (additions, deletions int) {
	var oldLines, newLines []string
	if oldData != nil {
		for _, l := range strings.Split(string(oldData), "\n") {
			if l != "" {
				oldLines = append(oldLines, l)
			}
		}
	}
	if newData != nil {
		for _, l := range strings.Split(string(newData), "\n") {
			if l != "" {
				newLines = append(newLines, l)
			}
		}
	}
	oldSet := map[string]bool{}
	for _, l := range oldLines {
		oldSet[l] = true
	}
	newSet := map[string]bool{}
	for _, l := range newLines {
		newSet[l] = true
	}
	for _, l := range newLines {
		if !oldSet[l] {
			additions++
		}
	}
	for _, l := range oldLines {
		if !newSet[l] {
			deletions++
		}
	}
	return additions, deletions
}

// diffTrees diffs two trees (base -> head) into pull-file rows.
func diffTrees(repo *Repo, baseTreeOID, headTreeOID string) []PullFile {
	base := flattenTree(repo, baseTreeOID, "", nil)
	head := flattenTree(repo, headTreeOID, "", nil)
	paths := map[string]bool{}
	for p := range base {
		paths[p] = true
	}
	for p := range head {
		paths[p] = true
	}
	var files []PullFile
	for path := range paths {
		b, bOk := base[path]
		h, hOk := head[path]
		if bOk && hOk && b.OID == h.OID {
			continue
		}
		if !bOk && hOk {
			var data []byte
			if o, ok := repo.Objects[h.OID]; ok {
				data = o.Data
			}
			add, _ := lineDiff(nil, data)
			files = append(files, PullFile{Filename: path, Status: "added", Additions: add, Deletions: 0, Changes: add})
		} else if bOk && !hOk {
			var data []byte
			if o, ok := repo.Objects[b.OID]; ok {
				data = o.Data
			}
			_, del := lineDiff(data, nil)
			files = append(files, PullFile{Filename: path, Status: "removed", Additions: 0, Deletions: del, Changes: del})
		} else {
			var oldData, newData []byte
			if o, ok := repo.Objects[b.OID]; ok {
				oldData = o.Data
			}
			if o, ok := repo.Objects[h.OID]; ok {
				newData = o.Data
			}
			add, del := lineDiff(oldData, newData)
			files = append(files, PullFile{Filename: path, Status: "modified", Additions: add, Deletions: del, Changes: add + del})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Filename < files[j].Filename })
	return files
}

// commitHistory walks a commit's first-parent history (newest first), bounded.
func commitHistory(repo *Repo, tipOID string, limit int) []struct {
	OID     string
	Message string
} {
	var out []struct {
		OID     string
		Message string
	}
	seen := map[string]bool{}
	current := tipOID
	for current != "" && len(out) < limit && !seen[current] {
		seen[current] = true
		obj, ok := repo.Objects[current]
		if !ok || obj.Type != ObjCommit {
			break
		}
		commit := parseCommit(obj.Data)
		out = append(out, struct {
			OID     string
			Message string
		}{OID: current, Message: commit.Message})
		if len(commit.Parents) == 0 {
			break
		}
		current = commit.Parents[0]
	}
	return out
}

// repoGlobalID is the opaque, stable global id for a repository.
func repoGlobalID(repo *Repo) string { return b64url("Repository:" + itoa(repo.DatabaseID)) }

// issueGlobalID is the opaque, stable global id for an issue.
func issueGlobalID(repo *Repo, issue *Issue) string {
	return b64url("Issue:" + itoa(repo.DatabaseID) + ":" + itoa(issue.Number))
}

// pullGlobalID is the opaque, stable global id for a pull request.
func pullGlobalID(repo *Repo, pull *Pull) string {
	return b64url("PullRequest:" + itoa(repo.DatabaseID) + ":" + itoa(pull.Number))
}

// commentGlobalID is the opaque, stable global id for a comment.
func commentGlobalID(repo *Repo, issue *Issue, id int) string {
	return b64url("IssueComment:" + itoa(repo.DatabaseID) + ":" + itoa(issue.Number) + ":" + itoa(id))
}

// Store is the forge's single in-memory store.
type Store struct {
	mu             sync.Mutex
	repos          map[string]*Repo
	nextDatabaseID int
	nextSeq        int
}

func NewStore() *Store {
	return &Store{repos: map[string]*Repo{}, nextDatabaseID: 1, nextSeq: 1}
}

// Lock acquires the store mutex for one request's dispatch.
func (s *Store) Lock()   { s.mu.Lock() }
func (s *Store) Unlock() { s.mu.Unlock() }

func (s *Store) repoKey(owner, name string) string {
	return strings.ToLower(owner) + "/" + strings.ToLower(name)
}

func (s *Store) GetRepo(owner, name string) *Repo {
	return s.repos[s.repoKey(owner, name)]
}

func (s *Store) HasRepo(owner, name string) bool {
	_, ok := s.repos[s.repoKey(owner, name)]
	return ok
}

// CreateRepo creates a repo owned by the viewer. Returns nil when a repo of
// that name already exists.
func (s *Store) CreateRepo(name, description string, isPrivate bool) *Repo {
	if s.HasRepo(ViewerLogin, name) {
		return nil
	}
	repo := &Repo{
		DatabaseID:    s.nextDatabaseID,
		Name:          name,
		Owner:         ViewerLogin,
		Description:   description,
		Private:       isPrivate,
		DefaultBranch: DefaultBranch,
		Refs:          map[string]string{},
		Objects:       map[string]GitObject{},
		Labels:        map[string]Label{},
		Issues:        map[int]*Issue{},
		Pulls:         map[int]*Pull{},
		NextIssue:     1,
		NextPull:      1,
		Seq:           s.nextSeq,
		packCache:     map[string]*fetchCacheEntry{},
		lsCache:       map[string][]byte{},
		respCache:     map[string]*respCacheEntry{},
	}
	s.nextDatabaseID++
	s.nextSeq++
	// The empty tree object exists in every repo from the start.
	putObject(repo, ObjTree, []byte{})
	s.repos[s.repoKey(ViewerLogin, name)] = repo
	return repo
}

func (s *Store) DeleteRepo(owner, name string) bool {
	key := s.repoKey(owner, name)
	if _, ok := s.repos[key]; !ok {
		return false
	}
	delete(s.repos, key)
	return true
}

// ListRepos returns all repos owned by owner, in creation order.
func (s *Store) ListRepos(owner string) []*Repo {
	var out []*Repo
	for _, repo := range s.repos {
		if repo.Owner == owner {
			out = append(out, repo)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// BranchTip returns the tip commit oid of a branch, or "" when unborn/absent.
func (s *Store) BranchTip(repo *Repo, branch string) string {
	return repo.Refs["refs/heads/"+branch]
}

// SetBranch sets a branch to a commit oid. Bumps the repo generation.
func (s *Store) SetBranch(repo *Repo, branch, oid string) {
	repo.Refs["refs/heads/"+branch] = oid
	repo.Generation++
}

// DeleteBranch deletes a branch. Bumps the repo generation.
func (s *Store) DeleteBranch(repo *Repo, branch string) {
	delete(repo.Refs, "refs/heads/"+branch)
	repo.Generation++
}

// CommitTree creates a commit on a branch with a new tree, returning the oid.
func (s *Store) CommitTree(repo *Repo, branch, treeOID, message string) string {
	parent := s.BranchTip(repo, branch)
	identity := serverIdentity()
	var parents []string
	if parent != "" {
		parents = []string{parent}
	}
	commit := encodeCommit(CommitData{Tree: treeOID, Parents: parents, Author: identity, Committer: identity, Message: message})
	oid := putObject(repo, ObjCommit, commit)
	s.SetBranch(repo, branch, oid)
	return oid
}

// ReadFile reads a file's blob at a branch tip, or nil when absent/unborn.
func (s *Store) ReadFile(repo *Repo, branch, path string) *struct {
	OID  string
	Mode string
	Data []byte
} {
	tip := s.BranchTip(repo, branch)
	if tip == "" {
		return nil
	}
	tipObj, ok := repo.Objects[tip]
	if !ok || tipObj.Type != ObjCommit {
		return nil
	}
	tree := parseCommit(tipObj.Data).Tree
	entry := readTreePath(repo, tree, splitPath(path))
	if entry == nil {
		return nil
	}
	blob, ok := repo.Objects[entry.OID]
	if !ok || blob.Type != ObjBlob {
		return nil
	}
	return &struct {
		OID  string
		Mode string
		Data []byte
	}{OID: entry.OID, Mode: entry.Mode, Data: blob.Data}
}

// WriteFile writes a file on a branch (create or update), creating the branch
// from the empty tree when it is unborn.
func (s *Store) WriteFile(repo *Repo, branch, path string, data []byte, message string) (commitSha, blobSha string) {
	blobSha = putObject(repo, ObjBlob, data)
	tip := s.BranchTip(repo, branch)
	treeOID := EmptyTreeOID
	if tip != "" {
		if tipObj, ok := repo.Objects[tip]; ok && tipObj.Type == ObjCommit {
			treeOID = parseCommit(tipObj.Data).Tree
		}
	}
	newTree := setTreePath(repo, treeOID, splitPath(path), &struct {
		OID  string
		Mode string
	}{OID: blobSha, Mode: "100644"})
	commitSha = s.CommitTree(repo, branch, newTree, message)
	return commitSha, blobSha
}

// DeleteFile deletes a file on a branch. Returns "" when the file is absent.
func (s *Store) DeleteFile(repo *Repo, branch, path, message string) string {
	tip := s.BranchTip(repo, branch)
	if tip == "" {
		return ""
	}
	tipObj, ok := repo.Objects[tip]
	if !ok || tipObj.Type != ObjCommit {
		return ""
	}
	tree := parseCommit(tipObj.Data).Tree
	if readTreePath(repo, tree, splitPath(path)) == nil {
		return ""
	}
	newTree := setTreePath(repo, tree, splitPath(path), nil)
	return s.CommitTree(repo, branch, newTree, message)
}

func (s *Store) CreateIssue(repo *Repo, title, body string, labels []string) *Issue {
	issue := &Issue{
		Number:        repo.NextIssue,
		Title:         title,
		Body:          body,
		State:         IssueOpen,
		Labels:        append([]string{}, labels...),
		Comments:      []Comment{},
		NextCommentID: 1,
	}
	repo.NextIssue++
	repo.Issues[issue.Number] = issue
	return issue
}

func (s *Store) CreatePull(repo *Repo, title, body, headRef, baseRef, headSha, baseSha string) *Pull {
	var headTree, baseTree string
	if o, ok := repo.Objects[headSha]; ok && o.Type == ObjCommit {
		headTree = parseCommit(o.Data).Tree
	} else {
		headTree = EmptyTreeOID
	}
	if o, ok := repo.Objects[baseSha]; ok && o.Type == ObjCommit {
		baseTree = parseCommit(o.Data).Tree
	} else {
		baseTree = EmptyTreeOID
	}
	pull := &Pull{
		Number:  repo.NextPull,
		Title:   title,
		Body:    body,
		State:   PullOpen,
		HeadRef: headRef,
		BaseRef: baseRef,
		HeadSha: headSha,
		BaseSha: baseSha,
		Files:   diffTrees(repo, baseTree, headTree),
	}
	repo.NextPull++
	repo.Pulls[pull.Number] = pull
	return pull
}

// MergePull squash-merges a pull request onto its base branch. Returns the
// merge commit oid, or "" when the merge conflicts.
func (s *Store) MergePull(repo *Repo, pull *Pull) string {
	baseTip := s.BranchTip(repo, pull.BaseRef)
	if baseTip == "" {
		return ""
	}
	baseTipObj, ok := repo.Objects[baseTip]
	if !ok || baseTipObj.Type != ObjCommit {
		return ""
	}
	baseTipTree := parseCommit(baseTipObj.Data).Tree
	baseObj, ok1 := repo.Objects[pull.BaseSha]
	headObj, ok2 := repo.Objects[pull.HeadSha]
	if !ok1 || !ok2 {
		return ""
	}
	baseTree := parseCommit(baseObj.Data).Tree
	headTree := parseCommit(headObj.Data).Tree
	baseFlat := flattenTree(repo, baseTree, "", nil)
	headFlat := flattenTree(repo, headTree, "", nil)
	tipFlat := flattenTree(repo, baseTipTree, "", nil)
	// Conflict: a path changed head-vs-base AND changed tip-vs-base with
	// different bytes.
	for path, h := range headFlat {
		if b, ok := baseFlat[path]; ok && b.OID == h.OID {
			continue
		}
		if t, ok := tipFlat[path]; ok {
			baseOID := ""
			if b, ok := baseFlat[path]; ok {
				baseOID = b.OID
			}
			if t.OID != baseOID && t.OID != h.OID {
				return ""
			}
		}
	}
	for path, b := range baseFlat {
		if _, ok := headFlat[path]; ok {
			continue
		}
		if t, ok := tipFlat[path]; ok && t.OID != b.OID {
			return ""
		}
	}
	// Three-way tree merge: head's changes win, tip's other changes are kept.
	mergedTree := baseTipTree
	for path, h := range headFlat {
		if b, ok := baseFlat[path]; ok && b.OID == h.OID {
			continue
		}
		mergedTree = setTreePath(repo, mergedTree, splitPath(path), &struct {
			OID  string
			Mode string
		}{OID: h.OID, Mode: h.Mode})
	}
	for path := range baseFlat {
		if _, ok := headFlat[path]; ok {
			continue
		}
		mergedTree = setTreePath(repo, mergedTree, splitPath(path), nil)
	}
	identity := serverIdentity()
	message := pull.Title + " (#" + itoa(pull.Number) + ")"
	commit := encodeCommit(CommitData{Tree: mergedTree, Parents: []string{baseTip}, Author: identity, Committer: identity, Message: message})
	mergeSha := putObject(repo, ObjCommit, commit)
	s.SetBranch(repo, pull.BaseRef, mergeSha)
	pull.State = PullClosed
	pull.Merged = true
	iso := trialNowISO()
	pull.MergedAt = &iso
	pull.MergeSha = &mergeSha
	return mergeSha
}

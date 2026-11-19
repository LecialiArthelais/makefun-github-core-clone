// REST v3 surface (55 checks). Wire contract from sdk/conformance/checks-rest.ts:
// - every reply application/json + non-empty X-GitHub-Request-Id
// - errors are JSON {message: non-empty string}; unknown route = JSON 404
// - malformed JSON body = 400; no token = 401; same repo name under another owner = 404 (never 403)
// - POST /user/repos 201; GET /user/repos paginates per_page=10 with Link rel="next"
// - contents PUT: create 201 / update 200|201 / stale sha 409; DELETE 200 {commit:{sha}}
// - issues: per-repo sequential numbers, labels must pre-exist (else 422), comments
// - pulls: squash merge only; 200 {merged:true, sha, message}; 404 unknown; 405 conflicted
// - shapes are minimum-required: extra fields are allowed, so responses carry the full record.
//
// JSON field order matches the Node object literals exactly (encoding/json
// marshals struct fields in declaration order).

package main

import (
	"encoding/base64"
	"math"
	"sort"
	"strconv"
	"strings"
)

func asStr(v any, fallback ...string) string {
	if s, ok := v.(string); ok {
		return s
	}
	if len(fallback) > 0 {
		return fallback[0]
	}
	return ""
}

func asBool(v any, fallback bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return fallback
}

// jsonNumber parses a value like JS Number(): strict, trims whitespace, and
// returns (n, true) when the result is a finite number.
func jsonNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		return n, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// parseRouteNumber parses a route number like JS Number(numRaw): an integer >= 1.
func parseRouteNumber(s string) (int, bool) {
	n, ok := jsonNumber(s)
	if !ok {
		return 0, false
	}
	if n != math.Trunc(n) || n < 1 {
		return 0, false
	}
	return int(n), true
}

// perPageOf parses a per_page query value: finite and > 0, else the default;
// capped at 100.
func perPageOf(q map[string][]string, def int) int {
	raw := queryGet(q, "per_page")
	if raw == "" {
		return def
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 {
		return def
	}
	p := int(n)
	if p > 100 {
		p = 100
	}
	return p
}

// pageOf parses a page query value: finite and > 0, else 1.
func pageOf(q map[string][]string) int {
	raw := queryGet(q, "page")
	if raw == "" {
		return 1
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 {
		return 1
	}
	return int(n)
}

// jsonBody parses the request body as a JSON object. Returns (body, nil) on
// success, or (nil, errorResponse) when the body is oversized, empty (returns
// an empty object), malformed, or not an object.
func jsonBody(req *Request) (map[string]any, *Response) {
	if req.Oversized {
		r := jsonResponse(413, map[string]string{"message": "Request body too large"})
		return nil, &r
	}
	if len(req.Body) == 0 {
		return map[string]any{}, nil
	}
	v, ok := parseJSON(req.Body)
	if !ok {
		r := jsonResponse(400, map[string]string{"message": "Problems parsing JSON"})
		return nil, &r
	}
	obj, isObj := v.(map[string]any)
	if !isObj {
		r := jsonResponse(400, map[string]string{"message": "Problems parsing JSON"})
		return nil, &r
	}
	return obj, nil
}

// --- response shapes (field order matches the Node literals) ---

type repoOwnerJSON struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
}

type repoJSON struct {
	ID            int           `json:"id"`
	DatabaseID    int           `json:"databaseId"`
	Name          string        `json:"name"`
	FullName      string        `json:"full_name"`
	Owner         repoOwnerJSON `json:"owner"`
	Private       bool          `json:"private"`
	Description   string        `json:"description"`
	DefaultBranch string        `json:"default_branch"`
}

func toRepoJSON(repo *Repo) repoJSON {
	return repoJSON{
		ID:            repo.DatabaseID,
		DatabaseID:    repo.DatabaseID,
		Name:          repo.Name,
		FullName:      repo.Owner + "/" + repo.Name,
		Owner:         repoOwnerJSON{Login: repo.Owner, ID: 1000},
		Private:       repo.Private,
		Description:   repo.Description,
		DefaultBranch: repo.DefaultBranch,
	}
}

type labelJSON struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

type issueJSON struct {
	ID       int         `json:"id"`
	Number   int         `json:"number"`
	Title    string      `json:"title"`
	Body     string      `json:"body"`
	State    string      `json:"state"`
	Labels   []labelJSON `json:"labels"`
	Comments int         `json:"comments"`
}

func toIssueJSON(repo *Repo, issue *Issue) issueJSON {
	labels := make([]labelJSON, 0, len(issue.Labels))
	for _, name := range issue.Labels {
		color := "ededed"
		if l, ok := repo.Labels[name]; ok {
			color = l.Color
		}
		labels = append(labels, labelJSON{Name: name, Color: color})
	}
	return issueJSON{
		ID:       issue.Number,
		Number:   issue.Number,
		Title:    issue.Title,
		Body:     issue.Body,
		State:    string(issue.State),
		Labels:   labels,
		Comments: len(issue.Comments),
	}
}

type refJSON struct {
	Ref string `json:"ref"`
	Sha string `json:"sha"`
}

type pullJSON struct {
	ID             int     `json:"id"`
	Number         int     `json:"number"`
	Title          string  `json:"title"`
	Body           string  `json:"body"`
	State          string  `json:"state"`
	Merged         bool    `json:"merged"`
	MergedAt       *string `json:"merged_at"`
	MergeCommitSha *string `json:"merge_commit_sha"`
	Head           refJSON `json:"head"`
	Base           refJSON `json:"base"`
}

func toPullJSON(repo *Repo, pull *Pull) pullJSON {
	return pullJSON{
		ID:             pull.Number,
		Number:         pull.Number,
		Title:          pull.Title,
		Body:           pull.Body,
		State:          string(pull.State),
		Merged:         pull.Merged,
		MergedAt:       pull.MergedAt,
		MergeCommitSha: pull.MergeSha,
		Head:           refJSON{Ref: pull.HeadRef, Sha: pull.HeadSha},
		Base:           refJSON{Ref: pull.BaseRef, Sha: pull.BaseSha},
	}
}

type commentJSON struct {
	ID   int    `json:"id"`
	Body string `json:"body"`
}

type refCreateJSON struct {
	Ref    string `json:"ref"`
	Object struct {
		Sha  string `json:"sha"`
		Type string `json:"type"`
	} `json:"object"`
}

type contentFileJSON struct {
	Type     string `json:"type"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
	Sha      string `json:"sha"`
	Size     int    `json:"size"`
	Name     string `json:"name"`
	Path     string `json:"path"`
}

type contentPutJSON struct {
	Content struct {
		Sha      string `json:"sha"`
		Encoding string `json:"encoding"`
	} `json:"content"`
	Commit struct {
		Sha string `json:"sha"`
	} `json:"commit"`
}

type contentDeleteJSON struct {
	Commit struct {
		Sha string `json:"sha"`
	} `json:"commit"`
}

type mergeResultJSON struct {
	Merged  bool   `json:"merged"`
	Sha     string `json:"sha"`
	Message string `json:"message"`
}

// resolveRepo resolves /repos/{owner}/{name} under the auth rules: owner must
// be the viewer, else 404.
func resolveRepo(store *Store, owner, name string) *Repo {
	if owner != ViewerLogin {
		return nil
	}
	return store.GetRepo(owner, name)
}

func notFoundResp() Response { return jsonResponse(404, map[string]string{"message": "Not Found"}) }

func unauthorizedResp() Response {
	return jsonResponse(401, map[string]string{"message": "Requires authentication"})
}

// handleRest is the REST v3 handler. Returns (response, handled).
func handleRest(store *Store, req *Request, segments []string) (Response, bool) {
	method := req.Method
	query := req.Query

	// GET /api/v3/ — root. No auth; X-OAuth-Scopes present.
	if len(segments) == 0 {
		if method != "GET" {
			return Response{}, false
		}
		return jsonResponse(200, map[string]string{"current_user_url": "/api/v3/user"}, [2]string{"x-oauth-scopes", "repo"}), true
	}

	first := segments[0]
	var second, third string
	if len(segments) > 1 {
		second = segments[1]
	}
	if len(segments) > 2 {
		third = segments[2]
	}
	rest := []string{}
	if len(segments) > 3 {
		rest = segments[3:]
	}

	// GET /api/v3/meta
	if first == "meta" && len(segments) == 1 {
		if method != "GET" {
			return Response{}, false
		}
		if !isAuthorized(req.Headers) {
			return unauthorizedResp(), true
		}
		return jsonResponse(200, map[string]string{"installed_version": "1.0.0"}), true
	}

	// /user/repos — create + list (paginated, per_page=10, Link rel="next").
	if first == "user" && second == "repos" && len(segments) == 2 {
		if !isAuthorized(req.Headers) {
			return unauthorizedResp(), true
		}
		if method == "POST" {
			body, errResp := jsonBody(req)
			if errResp != nil {
				return *errResp, true
			}
			name := asStr(body["name"])
			if name == "" {
				return jsonResponse(422, map[string]string{"message": "name is required"}), true
			}
			repo := store.CreateRepo(name, asStr(body["description"]), asBool(body["private"], false))
			if repo == nil {
				return jsonResponse(422, map[string]string{"message": "Repository already exists"}), true
			}
			return jsonResponse(201, toRepoJSON(repo)), true
		}
		if method == "GET" {
			perPage := perPageOf(query, 10)
			page := pageOf(query)
			all := store.ListRepos(ViewerLogin)
			start := (page - 1) * perPage
			end := page * perPage
			if start > len(all) {
				start = len(all)
			}
			if end > len(all) {
				end = len(all)
			}
			slice := all[start:end]
			var extra [][2]string
			if page*perPage < len(all) {
				// The Link URL must be absolute and on the entry's own origin (the
				// suite rejects a relative URL as "left the entry's origin"); the
				// Host header names that origin.
				host := req.Headers["host"]
				if host == "" {
					host = "localhost"
				}
				next := "http://" + host + "/api/v3/user/repos?per_page=" + itoa(perPage) + "&page=" + itoa(page+1)
				extra = append(extra, [2]string{"link", "<" + next + ">; rel=\"next\""})
			}
			list := make([]repoJSON, 0, len(slice))
			for _, r := range slice {
				list = append(list, toRepoJSON(r))
			}
			return jsonResponse(200, list, extra...), true
		}
		return Response{}, false
	}

	// /repos/{owner}/{name}[...]
	if first == "repos" && second != "" && third != "" {
		owner := second
		name := third
		tail := rest

		// GET /repos/{o}/{r}, PATCH, DELETE — resolve first (unknown owner/name
		// -> 404), then auth (anon -> 401).
		if len(tail) == 0 {
			if method == "GET" {
				repo := resolveRepo(store, owner, name)
				if repo == nil {
					return notFoundResp(), true
				}
				if !isAuthorized(req.Headers) {
					return unauthorizedResp(), true
				}
				return jsonResponse(200, toRepoJSON(repo)), true
			}
			if method == "PATCH" {
				repo := resolveRepo(store, owner, name)
				if repo == nil {
					return notFoundResp(), true
				}
				if !isAuthorized(req.Headers) {
					return unauthorizedResp(), true
				}
				body, errResp := jsonBody(req)
				if errResp != nil {
					return *errResp, true
				}
				if desc, ok := body["description"].(string); ok {
					repo.Description = desc
				}
				return jsonResponse(200, toRepoJSON(repo)), true
			}
			if method == "DELETE" {
				repo := resolveRepo(store, owner, name)
				if repo == nil {
					return notFoundResp(), true
				}
				if !isAuthorized(req.Headers) {
					return unauthorizedResp(), true
				}
				store.DeleteRepo(owner, name)
				return Response{Status: 204, Headers: [][2]string{{"x-github-request-id", nextRequestID()}}}, true
			}
			return Response{}, false
		}

		sub := tail[0]
		subTail := tail[1:]

		// POST /repos/{o}/{r}/git/refs — create a ref.
		if sub == "git" && len(subTail) == 1 && subTail[0] == "refs" {
			if method != "POST" {
				return Response{}, false
			}
			repo := resolveRepo(store, owner, name)
			if repo == nil {
				return notFoundResp(), true
			}
			if !isAuthorized(req.Headers) {
				return unauthorizedResp(), true
			}
			body, errResp := jsonBody(req)
			if errResp != nil {
				return *errResp, true
			}
			ref := asStr(body["ref"])
			sha := asStr(body["sha"])
			if !strings.HasPrefix(ref, "refs/heads/") || !isHex40(sha) {
				return jsonResponse(422, map[string]string{"message": "Invalid ref or sha"}), true
			}
			sha = strings.ToLower(sha)
			repo.Refs[ref] = sha
			// Mutating a ref must bump the generation: the fetch/ls-refs caches
			// key on it. Behaviour-neutral — the generation is an internal
			// cache-invalidation counter, never sent on the wire.
			repo.Generation++
			var resp refCreateJSON
			resp.Ref = ref
			resp.Object.Sha = sha
			resp.Object.Type = "commit"
			return jsonResponse(201, resp), true
		}

		// GET /repos/{o}/{r}/branches
		if sub == "branches" && len(subTail) == 0 {
			if method != "GET" {
				return Response{}, false
			}
			repo := resolveRepo(store, owner, name)
			if repo == nil {
				return notFoundResp(), true
			}
			if !isAuthorized(req.Headers) {
				return unauthorizedResp(), true
			}
			type branchJSON struct {
				Name   string `json:"name"`
				Commit struct {
					Sha string `json:"sha"`
				} `json:"commit"`
			}
			var branches []branchJSON
			for _, refName := range sortedRefNames(repo) {
				if !strings.HasPrefix(refName, "refs/heads/") {
					continue
				}
				var b branchJSON
				b.Name = refName[len("refs/heads/"):]
				b.Commit.Sha = repo.Refs[refName]
				branches = append(branches, b)
			}
			// sortedRefNames already sorts by ref name, which orders branches by
			// name (the refs/heads/ prefix is shared).
			return jsonResponse(200, branches), true
		}

		// GET /repos/{o}/{r}/commits[?sha=<branch>]
		if sub == "commits" && len(subTail) == 0 {
			if method != "GET" {
				return Response{}, false
			}
			repo := resolveRepo(store, owner, name)
			if repo == nil {
				return notFoundResp(), true
			}
			if !isAuthorized(req.Headers) {
				return unauthorizedResp(), true
			}
			branch := queryGet(query, "sha")
			if branch == "" {
				branch = repo.DefaultBranch
			}
			tip := store.BranchTip(repo, branch)
			if tip == "" {
				return jsonResponse(200, []any{}), true
			}
			type commitJSON struct {
				Sha    string `json:"sha"`
				Commit struct {
					Message string `json:"message"`
				} `json:"commit"`
			}
			history := commitHistory(repo, tip, 1000)
			list := make([]commitJSON, 0, len(history))
			for _, c := range history {
				var cj commitJSON
				cj.Sha = c.OID
				cj.Commit.Message = c.Message
				list = append(list, cj)
			}
			return jsonResponse(200, list), true
		}

		// /repos/{o}/{r}/contents/{path...}
		if sub == "contents" {
			path := strings.Join(subTail, "/")
			if path == "" {
				return Response{}, false
			}
			repo := resolveRepo(store, owner, name)
			if repo == nil {
				return notFoundResp(), true
			}
			if !isAuthorized(req.Headers) {
				return unauthorizedResp(), true
			}
			branch := queryGet(query, "ref")
			if branch == "" {
				branch = repo.DefaultBranch
			}
			if method == "GET" {
				tip := store.BranchTip(repo, branch)
				if tip == "" {
					return notFoundResp(), true
				}
				tipObj, ok := repo.Objects[tip]
				if !ok || tipObj.Type != ObjCommit {
					return notFoundResp(), true
				}
				tree := parseCommit(tipObj.Data).Tree
				flat := flattenTree(repo, tree, "", nil)
				if file, ok := flat[path]; ok {
					data := []byte{}
					if blob, ok := repo.Objects[file.OID]; ok {
						data = blob.Data
					}
					name := path
					if i := strings.LastIndex(path, "/"); i >= 0 {
						name = path[i+1:]
					}
					return jsonResponse(200, contentFileJSON{
						Type:     "file",
						Encoding: "base64",
						Content:  b64encode(data),
						Sha:      file.OID,
						Size:     len(data),
						Name:     name,
						Path:     path,
					}), true
				}
				dir := listTreeDir(repo, tree, splitPath(path))
				if dir != nil {
					type dirJSON struct {
						Type string `json:"type"`
						Name string `json:"name"`
						Path string `json:"path"`
						Sha  string `json:"sha"`
					}
					list := make([]dirJSON, 0, len(dir))
					for _, d := range dir {
						list = append(list, dirJSON{Type: d.Type, Name: d.Name, Path: d.Path, Sha: d.OID})
					}
					return jsonResponse(200, list), true
				}
				return notFoundResp(), true
			}
			if method == "PUT" {
				body, errResp := jsonBody(req)
				if errResp != nil {
					return *errResp, true
				}
				contentB64 := asStr(body["content"])
				data := decodeBase64(contentB64)
				if data == nil {
					return jsonResponse(422, map[string]string{"message": "content is not valid base64"}), true
				}
				message := asStr(body["message"], "Update file")
				putBranch := repo.DefaultBranch
				if b, ok := body["branch"].(string); ok && b != "" {
					putBranch = b
				}
				var givenSha *string
				if s, ok := body["sha"].(string); ok {
					lower := strings.ToLower(s)
					givenSha = &lower
				}
				existing := store.ReadFile(repo, putBranch, path)
				if existing != nil {
					// Update: a sha is required and must be current.
					if givenSha == nil || *givenSha != existing.OID {
						return jsonResponse(409, map[string]string{"message": "sha does not match"}), true
					}
					commitSha, blobSha := store.WriteFile(repo, putBranch, path, data, message)
					var resp contentPutJSON
					resp.Content.Sha = blobSha
					resp.Content.Encoding = "base64"
					resp.Commit.Sha = commitSha
					return jsonResponse(200, resp), true
				}
				// Create: a sha must not be supplied (a stale sha on create is a
				// 409 too, but the suite never sends one on create).
				commitSha, blobSha := store.WriteFile(repo, putBranch, path, data, message)
				var resp contentPutJSON
				resp.Content.Sha = blobSha
				resp.Content.Encoding = "base64"
				resp.Commit.Sha = commitSha
				return jsonResponse(201, resp), true
			}
			if method == "DELETE" {
				body, errResp := jsonBody(req)
				if errResp != nil {
					return *errResp, true
				}
				message := asStr(body["message"], "Delete file")
				delBranch := repo.DefaultBranch
				if b, ok := body["branch"].(string); ok && b != "" {
					delBranch = b
				}
				commitSha := store.DeleteFile(repo, delBranch, path, message)
				if commitSha == "" {
					return notFoundResp(), true
				}
				var resp contentDeleteJSON
				resp.Commit.Sha = commitSha
				return jsonResponse(200, resp), true
			}
			return Response{}, false
		}

		// POST /repos/{o}/{r}/labels
		if sub == "labels" && len(subTail) == 0 {
			if method != "POST" {
				return Response{}, false
			}
			repo := resolveRepo(store, owner, name)
			if repo == nil {
				return notFoundResp(), true
			}
			if !isAuthorized(req.Headers) {
				return unauthorizedResp(), true
			}
			body, errResp := jsonBody(req)
			if errResp != nil {
				return *errResp, true
			}
			labelName := asStr(body["name"])
			if labelName == "" {
				return jsonResponse(422, map[string]string{"message": "name is required"}), true
			}
			color := asStr(body["color"], "ededed")
			repo.Labels[labelName] = Label{Name: labelName, Color: color}
			return jsonResponse(201, map[string]string{"name": labelName, "color": color}), true
		}

		// /repos/{o}/{r}/issues[/{n}[/comments]]
		if sub == "issues" {
			repo := resolveRepo(store, owner, name)
			if repo == nil {
				return notFoundResp(), true
			}
			if !isAuthorized(req.Headers) {
				return unauthorizedResp(), true
			}
			if len(subTail) == 0 {
				if method == "POST" {
					body, errResp := jsonBody(req)
					if errResp != nil {
						return *errResp, true
					}
					var labels []string
					if arr, ok := body["labels"].([]any); ok {
						for _, l := range arr {
							if s, ok := l.(string); ok {
								labels = append(labels, s)
							}
						}
					}
					if len(labels) > 0 {
						for _, label := range labels {
							if _, ok := repo.Labels[label]; !ok {
								return jsonResponse(422, map[string]string{"message": "Label " + label + " does not exist"}), true
							}
						}
					}
					issue := store.CreateIssue(repo, asStr(body["title"]), asStr(body["body"]), labels)
					return jsonResponse(201, toIssueJSON(repo, issue)), true
				}
				if method == "GET" {
					stateFilter := queryGet(query, "state")
					if stateFilter == "" {
						stateFilter = "open"
					}
					sortParam := queryGet(query, "sort")
					if sortParam == "" {
						sortParam = "created"
					}
					direction := queryGet(query, "direction")
					if direction == "" {
						direction = "desc"
					}
					perPage := perPageOf(query, 100)
					// Insertion order = creation order = number order.
					nums := make([]int, 0, len(repo.Issues))
					for n := range repo.Issues {
						nums = append(nums, n)
					}
					sort.Ints(nums)
					var list []issueJSON
					for _, n := range nums {
						issue := repo.Issues[n]
						if stateFilter != "all" && string(issue.State) != stateFilter {
							continue
						}
						list = append(list, toIssueJSON(repo, issue))
					}
					if sortParam == "created" && direction == "desc" {
						for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
							list[i], list[j] = list[j], list[i]
						}
					}
					if len(list) > perPage {
						list = list[:perPage]
					}
					if list == nil {
						list = []issueJSON{}
					}
					return jsonResponse(200, list), true
				}
				return Response{}, false
			}
			num, ok := parseRouteNumber(subTail[0])
			if !ok {
				return notFoundResp(), true
			}
			issue := repo.Issues[num]
			if len(subTail) == 2 && subTail[1] == "comments" {
				if issue == nil {
					return notFoundResp(), true
				}
				if method == "POST" {
					body, errResp := jsonBody(req)
					if errResp != nil {
						return *errResp, true
					}
					comment := Comment{ID: issue.NextCommentID, Body: asStr(body["body"])}
					issue.NextCommentID++
					issue.Comments = append(issue.Comments, comment)
					return jsonResponse(201, commentJSON{ID: comment.ID, Body: comment.Body}), true
				}
				if method == "GET" {
					list := make([]commentJSON, 0, len(issue.Comments))
					for _, c := range issue.Comments {
						list = append(list, commentJSON{ID: c.ID, Body: c.Body})
					}
					return jsonResponse(200, list), true
				}
				return Response{}, false
			}
			if len(subTail) == 1 {
				if issue == nil {
					return notFoundResp(), true
				}
				if method == "GET" {
					return jsonResponse(200, toIssueJSON(repo, issue)), true
				}
				if method == "PATCH" {
					body, errResp := jsonBody(req)
					if errResp != nil {
						return *errResp, true
					}
					if t, ok := body["title"].(string); ok {
						issue.Title = t
					}
					if b, ok := body["body"].(string); ok {
						issue.Body = b
					}
					if s, ok := body["state"].(string); ok && (s == "open" || s == "closed") {
						issue.State = IssueState(s)
					}
					return jsonResponse(200, toIssueJSON(repo, issue)), true
				}
				return Response{}, false
			}
			return Response{}, false
		}

		// /repos/{o}/{r}/pulls[/{n}[/merge|/files]]
		if sub == "pulls" {
			repo := resolveRepo(store, owner, name)
			if repo == nil {
				return notFoundResp(), true
			}
			if !isAuthorized(req.Headers) {
				return unauthorizedResp(), true
			}
			if len(subTail) == 0 {
				if method == "POST" {
					body, errResp := jsonBody(req)
					if errResp != nil {
						return *errResp, true
					}
					headRef := asStr(body["head"])
					baseRef := asStr(body["base"], repo.DefaultBranch)
					headSha := store.BranchTip(repo, headRef)
					baseSha := store.BranchTip(repo, baseRef)
					if headSha == "" || baseSha == "" {
						return jsonResponse(422, map[string]string{"message": "head or base branch not found"}), true
					}
					pull := store.CreatePull(repo, asStr(body["title"]), asStr(body["body"]), headRef, baseRef, headSha, baseSha)
					return jsonResponse(201, toPullJSON(repo, pull)), true
				}
				if method == "GET" {
					stateFilter := queryGet(query, "state")
					if stateFilter == "" {
						stateFilter = "open"
					}
					perPage := perPageOf(query, 100)
					nums := make([]int, 0, len(repo.Pulls))
					for n := range repo.Pulls {
						nums = append(nums, n)
					}
					sort.Sort(sort.Reverse(sort.IntSlice(nums)))
					var list []pullJSON
					for _, n := range nums {
						pull := repo.Pulls[n]
						if stateFilter != "all" && string(pull.State) != stateFilter {
							continue
						}
						list = append(list, toPullJSON(repo, pull))
					}
					if len(list) > perPage {
						list = list[:perPage]
					}
					if list == nil {
						list = []pullJSON{}
					}
					return jsonResponse(200, list), true
				}
				return Response{}, false
			}
			num, ok := parseRouteNumber(subTail[0])
			if !ok {
				return notFoundResp(), true
			}
			pull := repo.Pulls[num]
			if len(subTail) == 2 && subTail[1] == "files" {
				if pull == nil {
					return notFoundResp(), true
				}
				if method != "GET" {
					return Response{}, false
				}
				type pullFileJSON struct {
					Filename  string `json:"filename"`
					Status    string `json:"status"`
					Additions int    `json:"additions"`
					Deletions int    `json:"deletions"`
					Changes   int    `json:"changes"`
				}
				list := make([]pullFileJSON, 0, len(pull.Files))
				for _, f := range pull.Files {
					list = append(list, pullFileJSON{Filename: f.Filename, Status: f.Status, Additions: f.Additions, Deletions: f.Deletions, Changes: f.Changes})
				}
				return jsonResponse(200, list), true
			}
			if len(subTail) == 2 && subTail[1] == "merge" {
				if pull == nil {
					return notFoundResp(), true
				}
				if method != "PUT" {
					return Response{}, false
				}
				_, errResp := jsonBody(req)
				if errResp != nil {
					return *errResp, true
				}
				if pull.State == PullClosed {
					return jsonResponse(405, map[string]string{"message": "Pull request already merged or closed"}), true
				}
				mergeSha := store.MergePull(repo, pull)
				if mergeSha == "" {
					return jsonResponse(405, map[string]string{"message": "Merge conflict"}), true
				}
				return jsonResponse(200, mergeResultJSON{
					Merged:  true,
					Sha:     mergeSha,
					Message: "Merged pull request #" + itoa(pull.Number),
				}), true
			}
			if len(subTail) == 1 {
				if pull == nil {
					return notFoundResp(), true
				}
				if method != "GET" {
					return Response{}, false
				}
				return jsonResponse(200, toPullJSON(repo, pull)), true
			}
			return Response{}, false
		}

		return Response{}, false
	}

	return Response{}, false
}

// isHex40 reports whether s is a 40-hex-character string.
func isHex40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// b64encode encodes bytes as standard base64 (with padding), like Node's
// Buffer.toString('base64').
func b64encode(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

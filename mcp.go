// MCP surface (15 checks) at POST /mcp: JSON-RPC 2.0 over Streamable HTTP,
// answered as plain application/json (the client accepts JSON or SSE). Two eras
// are supported:
// - modern 2026-07-28: `server/discover` answers {supportedVersions: [...],
//   capabilities, serverInfo} — the client negotiates 2026-07-28 when it is listed.
// - handshake <=2025-11-25: `initialize` (any of 2025-11-25, 2025-06-18,
//   2025-03-26, 2024-11-05; the requested revision is echoed) +
//   `notifications/initialized` (202, no body).
//
// Tools (six, the github-mcp-server set): create_repo, get_repo, create_issue,
// list_issues, create_pull_request, merge_pull_request. Tool results carry BOTH
// a text block (JSON) and structuredContent, so the suite's result reader finds
// the object either way. Unknown tool = JSON-RPC -32601; missing argument = a
// tool result with isError:true (never HTTP 500). tools/list is a constant, so
// two calls serialize byte-identically. No auth = HTTP 401.

package main

import (
	"math"
	"sort"
	"strconv"
	"strings"
)

const mcpServerName = "github-core-clone"
const mcpServerVersion = "1.0.0"

var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25"}
var mcpLegacyVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type mcpProperty struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

type mcpInputSchema struct {
	Type       string      `json:"type"`
	Properties *orderedMap `json:"properties"`
	Required   []string    `json:"required"`
}

type mcpToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema mcpInputSchema `json:"inputSchema"`
}

func mcpProps(pairs ...[3]string) *orderedMap {
	// pairs are (name, type, description) in order.
	m := newOrderedMap()
	for _, p := range pairs {
		m.set(p[0], mcpProperty{Type: p[1], Description: p[2]})
	}
	return m
}

// The tool list is a constant: two tools/list calls serialize byte-identically.
var mcpTools = []mcpToolDef{
	{
		Name:        "create_repo",
		Description: "Create a new repository owned by the authenticated user.",
		InputSchema: mcpInputSchema{
			Type: "object",
			Properties: mcpProps(
				[3]string{"name", "string", "The repository name."},
				[3]string{"description", "string", "A short description of the repository."},
				[3]string{"private", "boolean", "Whether the repository is private."},
			),
			Required: []string{"name"},
		},
	},
	{
		Name:        "get_repo",
		Description: "Get a repository by owner and name.",
		InputSchema: mcpInputSchema{
			Type: "object",
			Properties: mcpProps(
				[3]string{"owner", "string", "The repository owner login."},
				[3]string{"repo", "string", "The repository name."},
			),
			Required: []string{"owner", "repo"},
		},
	},
	{
		Name:        "create_issue",
		Description: "Create an issue in a repository.",
		InputSchema: mcpInputSchema{
			Type: "object",
			Properties: mcpProps(
				[3]string{"owner", "string", "The repository owner login."},
				[3]string{"repo", "string", "The repository name."},
				[3]string{"title", "string", "The issue title."},
				[3]string{"body", "string", "The issue body."},
			),
			Required: []string{"owner", "repo", "title"},
		},
	},
	{
		Name:        "list_issues",
		Description: "List issues in a repository, optionally filtered by state.",
		InputSchema: mcpInputSchema{
			Type: "object",
			Properties: mcpProps(
				[3]string{"owner", "string", "The repository owner login."},
				[3]string{"repo", "string", "The repository name."},
				[3]string{"state", "string", "Issue state filter: open, closed, or all."},
			),
			Required: []string{"owner", "repo"},
		},
	},
	{
		Name:        "create_pull_request",
		Description: "Create a pull request from a head branch into a base branch.",
		InputSchema: mcpInputSchema{
			Type: "object",
			Properties: mcpProps(
				[3]string{"owner", "string", "The repository owner login."},
				[3]string{"repo", "string", "The repository name."},
				[3]string{"title", "string", "The pull request title."},
				[3]string{"body", "string", "The pull request body."},
				[3]string{"head", "string", "The head branch name."},
				[3]string{"base", "string", "The base branch name."},
			),
			Required: []string{"owner", "repo", "title", "head", "base"},
		},
	},
	{
		Name:        "merge_pull_request",
		Description: "Merge a pull request with a squash merge.",
		InputSchema: mcpInputSchema{
			Type: "object",
			Properties: mcpProps(
				[3]string{"owner", "string", "The repository owner login."},
				[3]string{"repo", "string", "The repository name."},
				[3]string{"pull_number", "number", "The pull request number."},
			),
			Required: []string{"owner", "repo", "pull_number"},
		},
	},
}

func mcpArgStr(args map[string]any, name string) (string, bool) {
	v, ok := args[name]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func mcpArgNum(args map[string]any, name string) (float64, bool) {
	v, ok := args[name]
	if !ok {
		return 0, false
	}
	n, ok := v.(float64)
	if !ok || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, false
	}
	return n, true
}

type mcpContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpToolResult struct {
	Content           []mcpContentBlock `json:"content"`
	StructuredContent any               `json:"structuredContent"`
}

type mcpToolError struct {
	Content []mcpContentBlock `json:"content"`
	IsError bool              `json:"isError"`
}

func mcpToolResultOf(data any) mcpToolResult {
	text := ""
	if s, ok := data.(string); ok {
		text = s
	} else {
		text = string(marshalJSON(data))
	}
	return mcpToolResult{
		Content:           []mcpContentBlock{{Type: "text", Text: text}},
		StructuredContent: data,
	}
}

func mcpToolErrorOf(message string) mcpToolError {
	return mcpToolError{
		Content: []mcpContentBlock{{Type: "text", Text: message}},
		IsError: true,
	}
}

type mcpIssueLabelJSON struct {
	Name string `json:"name"`
}

type mcpIssueJSON struct {
	ID     int                 `json:"id"`
	Number int                 `json:"number"`
	Title  string              `json:"title"`
	Body   string              `json:"body"`
	State  string              `json:"state"`
	Labels []mcpIssueLabelJSON `json:"labels"`
}

func toMCPIssueJSON(repo *Repo, issue *Issue) mcpIssueJSON {
	labels := make([]mcpIssueLabelJSON, 0, len(issue.Labels))
	for _, name := range issue.Labels {
		labels = append(labels, mcpIssueLabelJSON{Name: name})
	}
	return mcpIssueJSON{
		ID:     issue.Number,
		Number: issue.Number,
		Title:  issue.Title,
		Body:   issue.Body,
		State:  string(issue.State),
		Labels: labels,
	}
}

type mcpMergeResultJSON struct {
	Merged         bool    `json:"merged"`
	Sha            string  `json:"sha"`
	Message        string  `json:"message"`
	ID             int     `json:"id"`
	Number         int     `json:"number"`
	Title          string  `json:"title"`
	Body           string  `json:"body"`
	State          string  `json:"state"`
	MergedAt       *string `json:"merged_at"`
	MergeCommitSha *string `json:"merge_commit_sha"`
	Head           refJSON `json:"head"`
	Base           refJSON `json:"base"`
}

func mcpResolveRepo(store *Store, owner, repoName string, ownerOK, repoOK bool) *Repo {
	if !ownerOK || !repoOK {
		return nil
	}
	if owner != ViewerLogin {
		return nil
	}
	return store.GetRepo(owner, repoName)
}

// callTool executes one tool call. Returns (result, "") on success, or
// ("", errorMessage) on a tool-level error.
func mcpCallTool(store *Store, name string, args map[string]any) (any, string) {
	normalized := strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			return r
		}
		return -1
	}, name)
	var tool *mcpToolDef
	for i := range mcpTools {
		if normalized == mcpTools[i].Name || strings.HasSuffix(normalized, mcpTools[i].Name) {
			tool = &mcpTools[i]
			break
		}
	}
	if tool == nil {
		return nil, "Unknown tool: " + name
	}

	switch tool.Name {
	case "create_repo":
		repoName, ok := mcpArgStr(args, "name")
		if !ok || repoName == "" {
			return nil, "Missing required argument: name"
		}
		description, _ := mcpArgStr(args, "description")
		isPrivate := false
		if b, ok := args["private"].(bool); ok {
			isPrivate = b
		}
		repo := store.CreateRepo(repoName, description, isPrivate)
		if repo == nil {
			return nil, "Repository " + repoName + " already exists"
		}
		return mcpToolResultOf(toRepoJSON(repo)), ""
	case "get_repo":
		owner, ownerOK := mcpArgStr(args, "owner")
		repoName, repoOK := mcpArgStr(args, "repo")
		repo := mcpResolveRepo(store, owner, repoName, ownerOK, repoOK)
		if repo == nil {
			return nil, "Repository not found"
		}
		return mcpToolResultOf(toRepoJSON(repo)), ""
	case "create_issue":
		owner, ownerOK := mcpArgStr(args, "owner")
		repoName, repoOK := mcpArgStr(args, "repo")
		repo := mcpResolveRepo(store, owner, repoName, ownerOK, repoOK)
		if repo == nil {
			return nil, "Repository not found"
		}
		title, ok := mcpArgStr(args, "title")
		if !ok {
			return nil, "Missing required argument: title"
		}
		body, _ := mcpArgStr(args, "body")
		issue := store.CreateIssue(repo, title, body, []string{})
		return mcpToolResultOf(toMCPIssueJSON(repo, issue)), ""
	case "list_issues":
		owner, ownerOK := mcpArgStr(args, "owner")
		repoName, repoOK := mcpArgStr(args, "repo")
		repo := mcpResolveRepo(store, owner, repoName, ownerOK, repoOK)
		if repo == nil {
			return nil, "Repository not found"
		}
		state, _ := mcpArgStr(args, "state")
		if state == "" {
			state = "open"
		}
		nums := make([]int, 0, len(repo.Issues))
		for n := range repo.Issues {
			nums = append(nums, n)
		}
		sort.Sort(sort.Reverse(sort.IntSlice(nums)))
		list := make([]mcpIssueJSON, 0, len(nums))
		for _, n := range nums {
			issue := repo.Issues[n]
			if state != "all" && string(issue.State) != state {
				continue
			}
			list = append(list, toMCPIssueJSON(repo, issue))
		}
		return mcpToolResultOf(list), ""
	case "create_pull_request":
		owner, ownerOK := mcpArgStr(args, "owner")
		repoName, repoOK := mcpArgStr(args, "repo")
		repo := mcpResolveRepo(store, owner, repoName, ownerOK, repoOK)
		if repo == nil {
			return nil, "Repository not found"
		}
		title, titleOK := mcpArgStr(args, "title")
		head, headOK := mcpArgStr(args, "head")
		base, baseOK := mcpArgStr(args, "base")
		if !titleOK {
			return nil, "Missing required argument: title"
		}
		if !headOK {
			return nil, "Missing required argument: head"
		}
		if !baseOK {
			return nil, "Missing required argument: base"
		}
		body, _ := mcpArgStr(args, "body")
		headSha := store.BranchTip(repo, head)
		baseSha := store.BranchTip(repo, base)
		if headSha == "" || baseSha == "" {
			return nil, "Head or base branch not found"
		}
		pull := store.CreatePull(repo, title, body, head, base, headSha, baseSha)
		return mcpToolResultOf(toPullJSON(repo, pull)), ""
	case "merge_pull_request":
		owner, ownerOK := mcpArgStr(args, "owner")
		repoName, repoOK := mcpArgStr(args, "repo")
		repo := mcpResolveRepo(store, owner, repoName, ownerOK, repoOK)
		if repo == nil {
			return nil, "Repository not found"
		}
		pullNumber, ok := mcpArgNum(args, "pull_number")
		if !ok {
			return nil, "Missing required argument: pull_number"
		}
		pull := repo.Pulls[int(pullNumber)]
		if pull == nil {
			return nil, "Pull request #" + formatJSNumber(pullNumber) + " not found"
		}
		if pull.State == PullClosed {
			return nil, "Pull request already merged or closed"
		}
		mergeSha := store.MergePull(repo, pull)
		if mergeSha == "" {
			return nil, "Merge conflict"
		}
		return mcpToolResultOf(mcpMergeResultJSON{
			Merged:         true,
			Sha:            mergeSha,
			Message:        "Merged pull request #" + itoa(pull.Number),
			ID:             pull.Number,
			Number:         pull.Number,
			Title:          pull.Title,
			Body:           pull.Body,
			State:          string(pull.State),
			MergedAt:       pull.MergedAt,
			MergeCommitSha: pull.MergeSha,
			Head:           refJSON{Ref: pull.HeadRef, Sha: pull.HeadSha},
			Base:           refJSON{Ref: pull.BaseRef, Sha: pull.BaseSha},
		}), ""
	default:
		return nil, "Unknown tool: " + name
	}
}

// formatJSNumber formats a float like JS String(number): integers without a
// decimal point, others with the shortest round-trip decimal.
func formatJSNumber(n float64) string {
	return strconv.FormatFloat(n, 'g', -1, 64)
}

type mcpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpResponse struct {
	JSONRPC string       `json:"jsonrpc"`
	ID      any          `json:"id"`
	Result  any          `json:"result,omitempty"`
	Error   *mcpRPCError `json:"error,omitempty"`
}

// handleRPC handles one JSON-RPC request object. Returns the response object,
// or nil for a notification.
func mcpHandleRPC(store *Store, req any) *mcpResponse {
	obj, isObj := req.(map[string]any)
	if !isObj {
		return &mcpResponse{JSONRPC: "2.0", ID: nil, Error: &mcpRPCError{Code: -32600, Message: "Invalid Request"}}
	}
	methodV, hasMethod := obj["method"]
	method, isStr := methodV.(string)
	if obj["jsonrpc"] != "2.0" || !hasMethod || !isStr {
		id := any(nil)
		if v, ok := obj["id"]; ok {
			id = v
		}
		return &mcpResponse{JSONRPC: "2.0", ID: id, Error: &mcpRPCError{Code: -32600, Message: "Invalid Request"}}
	}
	idV, hasID := obj["id"]
	isNotification := !hasID
	id := any(nil)
	if hasID {
		id = idV
	}
	params := map[string]any{}
	if p, ok := obj["params"]; ok {
		if m, isMap := p.(map[string]any); isMap {
			params = m
		}
	}

	respond := func(result any) *mcpResponse {
		if isNotification {
			return nil
		}
		return &mcpResponse{JSONRPC: "2.0", ID: id, Result: result}
	}
	respondError := func(code int, message string) *mcpResponse {
		if isNotification {
			return nil
		}
		return &mcpResponse{JSONRPC: "2.0", ID: id, Error: &mcpRPCError{Code: code, Message: message}}
	}

	if strings.HasPrefix(method, "notifications/") {
		return nil // notifications are acknowledged with HTTP 202 and no body
	}

	if method == "server/discover" {
		meta := newOrderedMap()
		meta.set("io.modelcontextprotocol/serverInfo", map[string]string{"name": mcpServerName, "version": mcpServerVersion})
		result := newOrderedMap()
		result.set("supportedVersions", mcpSupportedVersions)
		result.set("capabilities", map[string]any{"tools": map[string]any{}})
		result.set("serverInfo", map[string]string{"name": mcpServerName, "version": mcpServerVersion})
		result.set("_meta", meta)
		return respond(result)
	}

	if method == "initialize" {
		requested := ""
		if v, ok := params["protocolVersion"]; ok {
			if s, isStr := v.(string); isStr {
				requested = s
			}
		}
		negotiated := "2025-11-25"
		for _, v := range append(append([]string{}, mcpSupportedVersions...), mcpLegacyVersions...) {
			if requested != "" && requested == v {
				negotiated = requested
				break
			}
		}
		result := newOrderedMap()
		result.set("protocolVersion", negotiated)
		result.set("capabilities", map[string]any{"tools": map[string]any{}})
		result.set("serverInfo", map[string]string{"name": mcpServerName, "version": mcpServerVersion})
		return respond(result)
	}

	if method == "tools/list" {
		return respond(map[string]any{"tools": mcpTools})
	}

	if method == "tools/call" {
		toolName := ""
		if v, ok := params["name"]; ok {
			if s, isStr := v.(string); isStr {
				toolName = s
			}
		}
		args := map[string]any{}
		if v, ok := params["arguments"]; ok {
			if m, isMap := v.(map[string]any); isMap {
				args = m
			}
		}
		if toolName == "" {
			return respondError(-32602, "Missing required argument: name")
		}
		result, errMsg := mcpCallTool(store, toolName, args)
		if errMsg != "" {
			// An unknown tool is a JSON-RPC error; a known tool's failure is a
			// tool result.
			if strings.HasPrefix(errMsg, "Unknown tool:") {
				return respondError(-32601, errMsg)
			}
			return respond(mcpToolErrorOf(errMsg))
		}
		return respond(result)
	}

	return respondError(-32601, "Method not found: "+method)
}

// handleMCP is the HTTP handler for POST /mcp. Returns (response, handled).
func handleMCP(store *Store, req *Request) (Response, bool) {
	if req.Method != "POST" {
		return Response{}, false
	}
	if !isAuthorized(req.Headers) {
		return jsonResponse(401, map[string]string{"message": "Requires authentication"}), true
	}
	if req.Oversized {
		return jsonResponse(413, map[string]string{"message": "Request body too large"}), true
	}
	parsed, ok := parseJSON(req.Body)
	if !ok {
		// Non-JSON body: HTTP 400 + JSON-RPC parse error -32700.
		return jsonResponse(400, mcpResponse{
			JSONRPC: "2.0",
			ID:      nil,
			Error:   &mcpRPCError{Code: -32700, Message: "Parse error"},
		}), true
	}
	if arr, isArr := parsed.([]any); isArr {
		var responses []*mcpResponse
		for _, item := range arr {
			if r := mcpHandleRPC(store, item); r != nil {
				responses = append(responses, r)
			}
		}
		if len(responses) == 0 {
			return Response{Status: 202, Headers: [][2]string{{"content-length", "0"}}}, true
		}
		return jsonResponse(200, responses), true
	}
	response := mcpHandleRPC(store, parsed)
	if response == nil {
		// A notification: HTTP 202, no body.
		return Response{Status: 202, Headers: [][2]string{{"content-length", "0"}}}, true
	}
	return jsonResponse(200, response), true
}

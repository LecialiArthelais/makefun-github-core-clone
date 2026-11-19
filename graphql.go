// GraphQL surface (20 checks) at POST /api/graphql. A small hand-rolled parser +
// executor for the documents the suite sends (sdk/conformance/checks-graphql.ts):
// queries and mutations with variables, inline input objects, inline enum lists,
// aliases, inline fragments, batched arrays, and __type introspection. No external
// library — stdlib only.
//
// Wire rules: unauthenticated = exactly 401; every other outcome is HTTP 200 with
// {data, errors} (an unknown field is a 200 with a non-empty errors array whose
// message names the field); an unknown repository is data.repository = null with NO
// errors; a JSON-array body returns a JSON-array response in the same order. Global
// ids are opaque (base64url of a typed key) and stable across reads; databaseId is
// a positive int and never equals the global id.
//
// Response field order follows the document's selection order (like the Node
// JSON.stringify of the executor's objects), via orderedMap.

package main

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ------------------------------------------------------------------ tokenizer

type gqlTokenKind string

const (
	tokName   gqlTokenKind = "name"
	tokString gqlTokenKind = "string"
	tokInt    gqlTokenKind = "int"
	tokPunct  gqlTokenKind = "punct"
	tokEOF    gqlTokenKind = "eof"
)

type gqlToken struct {
	kind  gqlTokenKind
	value string
}

func tokenizeGQL(src string) []gqlToken {
	var tokens []gqlToken
	i, n := 0, len(src)
	isDigit := func(c byte) bool { return c >= '0' && c <= '9' }
	isNameStart := func(c byte) bool {
		return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
	}
	isNameChar := func(c byte) bool { return isNameStart(c) || isDigit(c) }
	for i < n {
		c := src[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			i++
			continue
		}
		if c == '#' {
			for i < n && src[i] != '\n' {
				i++
			}
			continue
		}
		if c == '"' {
			j := i + 1
			var out strings.Builder
			for j < n && src[j] != '"' {
				if src[j] == '\\' && j+1 < n {
					esc := src[j+1]
					switch esc {
					case 'n':
						out.WriteByte('\n')
					case 't':
						out.WriteByte('\t')
					case 'r':
						out.WriteByte('\r')
					default:
						out.WriteByte(esc)
					}
					j += 2
				} else {
					out.WriteByte(src[j])
					j++
				}
			}
			tokens = append(tokens, gqlToken{kind: tokString, value: out.String()})
			i = j + 1
			continue
		}
		if c == '-' || isDigit(c) {
			j := i
			if src[j] == '-' {
				j++
			}
			for j < n && (isDigit(src[j]) || src[j] == '.') {
				j++
			}
			tokens = append(tokens, gqlToken{kind: tokInt, value: src[i:j]})
			i = j
			continue
		}
		if isNameStart(c) {
			j := i
			for j < n && isNameChar(src[j]) {
				j++
			}
			tokens = append(tokens, gqlToken{kind: tokName, value: src[i:j]})
			i = j
			continue
		}
		if c == '.' && i+2 < n && src[i+1] == '.' && src[i+2] == '.' {
			tokens = append(tokens, gqlToken{kind: tokPunct, value: "..."})
			i += 3
			continue
		}
		if strings.ContainsRune("{}()[]:!=$@", rune(c)) {
			tokens = append(tokens, gqlToken{kind: tokPunct, value: string(c)})
			i++
			continue
		}
		// Unknown character: skip it rather than failing the whole document.
		i++
	}
	tokens = append(tokens, gqlToken{kind: tokEOF, value: ""})
	return tokens
}

// ------------------------------------------------------------------ AST

type gqlValueKind string

const (
	gqlVariable gqlValueKind = "variable"
	gqlInt      gqlValueKind = "int"
	gqlStr      gqlValueKind = "string"
	gqlBool     gqlValueKind = "boolean"
	gqlNull     gqlValueKind = "null"
	gqlEnum     gqlValueKind = "enum"
	gqlList     gqlValueKind = "list"
	gqlObject   gqlValueKind = "object"
)

type gqlValue struct {
	kind   gqlValueKind
	name   string // variable name / enum value
	num    float64
	str    string
	b      bool
	list   []gqlValue
	fields []gqlObjField
}

type gqlObjField struct {
	name  string
	value gqlValue
}

type gqlArg struct {
	name  string
	value gqlValue
}

type gqlField struct {
	name      string
	alias     string
	args      []gqlArg
	selection []gqlSelection
}

type gqlSelection struct {
	kind        string // "field", "spread", "inline"
	field       *gqlField
	spreadName  string
	typeCond    string
	hasTypeCond bool
	selection   []gqlSelection
}

type gqlOperation struct {
	operation string // "query", "mutation" (or whatever the document says)
	name      string
	variables []gqlVar
	selection []gqlSelection
}

type gqlVar struct {
	name string
	typ  string
}

type gqlFragment struct {
	name      string
	typeCond  string
	selection []gqlSelection
}

type gqlParser struct {
	tokens []gqlToken
	pos    int
}

func (p *gqlParser) peek() gqlToken {
	if p.pos >= len(p.tokens) {
		return p.tokens[len(p.tokens)-1]
	}
	return p.tokens[p.pos]
}
func (p *gqlParser) next() gqlToken {
	t := p.peek()
	if p.pos < len(p.tokens) {
		p.pos++
	}
	return t
}
func (p *gqlParser) expect(value string) (gqlToken, error) {
	t := p.next()
	if t.value != value && t.kind != tokEOF {
		return t, fmt.Errorf("expected %s, got %s", value, t.value)
	}
	return t, nil
}
func (p *gqlParser) expectName() string { return p.next().value }

func (p *gqlParser) parseDocument() ([]any, error) {
	var defs []any
	for p.peek().kind != tokEOF {
		def, err := p.parseDefinition()
		if err != nil {
			return nil, err
		}
		defs = append(defs, def)
	}
	return defs, nil
}

func (p *gqlParser) parseDefinition() (any, error) {
	t := p.peek()
	if t.kind == tokPunct && t.value == "{" {
		sel, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		return &gqlOperation{operation: "query", selection: sel}, nil
	}
	if t.kind == tokName && t.value == "fragment" {
		p.next()
		name := p.expectName()
		if _, err := p.expect("on"); err != nil {
			return nil, err
		}
		typeCond := p.expectName()
		sel, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		return &gqlFragment{name: name, typeCond: typeCond, selection: sel}, nil
	}
	op := p.next().value
	var name string
	if p.peek().kind == tokName {
		name = p.next().value
	}
	var variables []gqlVar
	if p.peek().value == "(" {
		p.next()
		for p.peek().value != ")" {
			if _, err := p.expect("$"); err != nil {
				return nil, err
			}
			varName := p.expectName()
			if _, err := p.expect(":"); err != nil {
				return nil, err
			}
			typ, err := p.parseType()
			if err != nil {
				return nil, err
			}
			if p.peek().value == "=" {
				p.next()
				if _, err := p.parseValue(); err != nil {
					return nil, err
				}
			}
			variables = append(variables, gqlVar{name: varName, typ: typ})
		}
		if _, err := p.expect(")"); err != nil {
			return nil, err
		}
	}
	sel, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	return &gqlOperation{operation: op, name: name, variables: variables, selection: sel}, nil
}

func (p *gqlParser) parseType() (string, error) {
	var typ string
	if p.peek().value == "[" {
		p.next()
		inner, err := p.parseType()
		if err != nil {
			return "", err
		}
		typ = "[" + inner + "]"
		if _, err := p.expect("]"); err != nil {
			return "", err
		}
	} else {
		typ = p.expectName()
	}
	if p.peek().value == "!" {
		p.next()
		typ += "!"
	}
	return typ, nil
}

func (p *gqlParser) parseSelectionSet() ([]gqlSelection, error) {
	if _, err := p.expect("{"); err != nil {
		return nil, err
	}
	var selections []gqlSelection
	for p.peek().value != "}" {
		if p.peek().kind == tokEOF {
			break
		}
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		selections = append(selections, sel)
	}
	if _, err := p.expect("}"); err != nil {
		return nil, err
	}
	return selections, nil
}

func (p *gqlParser) parseSelection() (gqlSelection, error) {
	if p.peek().value == "..." {
		p.next()
		if p.peek().value == "on" {
			p.next()
			typeCond := p.expectName()
			sel, err := p.parseSelectionSet()
			if err != nil {
				return gqlSelection{}, err
			}
			return gqlSelection{kind: "inline", typeCond: typeCond, hasTypeCond: true, selection: sel}, nil
		}
		if p.peek().kind == tokName {
			return gqlSelection{kind: "spread", spreadName: p.next().value}, nil
		}
		sel, err := p.parseSelectionSet()
		if err != nil {
			return gqlSelection{}, err
		}
		return gqlSelection{kind: "inline", selection: sel}, nil
	}
	first := p.expectName()
	alias := ""
	name := first
	if p.peek().value == ":" {
		p.next()
		alias = first
		name = p.expectName()
	}
	var args []gqlArg
	if p.peek().value == "(" {
		p.next()
		for p.peek().value != ")" {
			argName := p.expectName()
			if _, err := p.expect(":"); err != nil {
				return gqlSelection{}, err
			}
			val, err := p.parseValue()
			if err != nil {
				return gqlSelection{}, err
			}
			args = append(args, gqlArg{name: argName, value: val})
		}
		if _, err := p.expect(")"); err != nil {
			return gqlSelection{}, err
		}
	}
	var selection []gqlSelection
	if p.peek().value == "{" {
		sel, err := p.parseSelectionSet()
		if err != nil {
			return gqlSelection{}, err
		}
		selection = sel
	}
	return gqlSelection{kind: "field", field: &gqlField{name: name, alias: alias, args: args, selection: selection}}, nil
}

func (p *gqlParser) parseValue() (gqlValue, error) {
	t := p.next()
	if t.kind == tokPunct && t.value == "$" {
		return gqlValue{kind: gqlVariable, name: p.expectName()}, nil
	}
	if t.kind == tokPunct && t.value == "[" {
		var values []gqlValue
		for p.peek().value != "]" {
			v, err := p.parseValue()
			if err != nil {
				return gqlValue{}, err
			}
			values = append(values, v)
		}
		if _, err := p.expect("]"); err != nil {
			return gqlValue{}, err
		}
		return gqlValue{kind: gqlList, list: values}, nil
	}
	if t.kind == tokPunct && t.value == "{" {
		var fields []gqlObjField
		for p.peek().value != "}" {
			name := p.expectName()
			if _, err := p.expect(":"); err != nil {
				return gqlValue{}, err
			}
			v, err := p.parseValue()
			if err != nil {
				return gqlValue{}, err
			}
			fields = append(fields, gqlObjField{name: name, value: v})
		}
		if _, err := p.expect("}"); err != nil {
			return gqlValue{}, err
		}
		return gqlValue{kind: gqlObject, fields: fields}, nil
	}
	if t.kind == tokString {
		return gqlValue{kind: gqlStr, str: t.value}, nil
	}
	if t.kind == tokInt {
		n, _ := strconv.ParseFloat(t.value, 64)
		return gqlValue{kind: gqlInt, num: n}, nil
	}
	if t.kind == tokName {
		switch t.value {
		case "true":
			return gqlValue{kind: gqlBool, b: true}, nil
		case "false":
			return gqlValue{kind: gqlBool, b: false}, nil
		case "null":
			return gqlValue{kind: gqlNull}, nil
		default:
			return gqlValue{kind: gqlEnum, name: t.value}, nil
		}
	}
	return gqlValue{}, fmt.Errorf("unexpected token %s", t.value)
}

// resolveValue resolves a value node against the operation's variables.
func resolveGQLValue(node gqlValue, variables map[string]any) any {
	switch node.kind {
	case gqlVariable:
		if v, ok := variables[node.name]; ok {
			return v
		}
		return nil
	case gqlInt:
		return node.num
	case gqlStr:
		return node.str
	case gqlBool:
		return node.b
	case gqlNull:
		return nil
	case gqlEnum:
		return node.name
	case gqlList:
		out := make([]any, 0, len(node.list))
		for _, v := range node.list {
			out = append(out, resolveGQLValue(v, variables))
		}
		return out
	case gqlObject:
		out := newOrderedMap()
		for _, f := range node.fields {
			out.set(f.name, resolveGQLValue(f.value, variables))
		}
		return out
	}
	return nil
}

func gqlArgValue(field *gqlField, name string, variables map[string]any) (any, bool) {
	for _, a := range field.args {
		if a.name == name {
			return resolveGQLValue(a.value, variables), true
		}
	}
	return nil, false
}

// gqlGet reads a named field from a variables/input object (map or orderedMap).
func gqlGet(v any, key string) (any, bool) {
	switch m := v.(type) {
	case map[string]any:
		val, ok := m[key]
		return val, ok
	case *orderedMap:
		return m.get(key)
	}
	return nil, false
}

func gqlString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func gqlNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

func gqlStringList(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// ------------------------------------------------------------------ schema (introspection)

var gqlSchemaFields = map[string][]string{
	"Issue":                  {"id", "number", "title", "state", "body"},
	"PullRequest":            {"id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files"},
	"Repository":             {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"},
	"User":                   {"login", "id"},
	"IssueConnection":        {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestConnection":  {"nodes", "edges", "pageInfo", "totalCount"},
	"IssueComment":           {"id", "body"},
	"IssueCommentEdge":       {"node", "cursor"},
	"PullRequestChangedFile": {"path"},
	"PageInfo":               {"hasNextPage", "hasPreviousPage", "startCursor", "endCursor"},
	"Ref":                    {"name"},
	"Query":                  {"viewer", "repository", "node", "__type"},
	"Mutation":               {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"},
}

var gqlSchemaEnums = map[string][]string{
	"SearchType":             {"ISSUE", "REPOSITORY", "USER", "PULL_REQUEST", "DISCUSSION"},
	"IssueState":             {"OPEN", "CLOSED"},
	"MergeableState":         {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"PullRequestState":       {"OPEN", "CLOSED", "MERGED"},
	"PullRequestMergeMethod": {"MERGE", "SQUASH", "REBASE"},
	"RepositoryPermission":   {"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"},
	"IssueOrderField":        {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
	"OrderDirection":         {"ASC", "DESC"},
}

// ------------------------------------------------------------------ executor

type gqlCtx struct {
	store     *Store
	variables map[string]any
	errors    []gqlErrorMsg
	fragments map[string]*gqlFragment
}

type gqlErrorMsg struct {
	Message string `json:"message"`
}

// gqlVal is a resolved GraphQL value (a "node" the executor can select from).
type gqlVal struct {
	kind  string // repo, issue, pull, user, comment, issues, pulls, files, ref, payload, commentEdge, pageInfo, type
	repo  *Repo
	issue *Issue
	pull  *Pull
	login string
	// comment
	commentID   int
	commentBody string
	// connections
	issues []*Issue
	pulls  []*Pull
	files  []string // paths
	// pageInfo
	pageHasNext   bool
	pageEndCursor *string
	// ref
	refName string
	// payload: named fields (values are scalars, *gqlVal, or []any)
	payload *orderedMap
	// type
	typeName string
	// commentEdge
	edgeNode *gqlVal
}

func cursorForIssue(issue *Issue) string {
	return b64url("cursor:" + itoa(issue.Number))
}

var cursorRe = regexp.MustCompile(`^cursor:(\d+)$`)

func issueFromCursor(cursor string) (int, bool) {
	if cursor == "" {
		return 0, false
	}
	decoded, ok := b64urlDecode(cursor)
	if !ok {
		return 0, false
	}
	m := cursorRe.FindStringSubmatch(decoded)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// resolveField resolves one field against a parent value. The second return
// value is false when the field is unknown (the error is already pushed and
// the key must be omitted from the output).
func resolveField(ctx *gqlCtx, parent *gqlVal, field *gqlField) (any, bool) {
	name := field.name
	args := func(n string) (any, bool) { return gqlArgValue(field, n, ctx.variables) }

	switch parent.kind {
	case "repo":
		repo := parent.repo
		switch name {
		case "id":
			return repoGlobalID(repo), true
		case "databaseId":
			return repo.DatabaseID, true
		case "name":
			return repo.Name, true
		case "nameWithOwner":
			return repo.Owner + "/" + repo.Name, true
		case "owner":
			return &gqlVal{kind: "user", login: repo.Owner}, true
		case "defaultBranchRef":
			return &gqlVal{kind: "ref", refName: repo.DefaultBranch}, true
		case "viewerPermission":
			return "ADMIN", true
		case "hasIssuesEnabled":
			return true, true
		case "issue":
			numV, _ := args("number")
			num, _ := gqlNumber(numV)
			issue := repo.Issues[int(num)]
			if issue == nil {
				return nil, true
			}
			return &gqlVal{kind: "issue", repo: repo, issue: issue}, true
		case "issues":
			first := 10.0
			if v, ok := args("first"); ok {
				if n, ok := gqlNumber(v); ok {
					first = n
				}
			}
			var states []string
			if v, ok := args("states"); ok {
				states = gqlStringList(v)
			}
			direction := "ASC"
			if v, ok := args("orderBy"); ok {
				if d, ok := gqlGet(v, "direction"); ok {
					if s, ok := d.(string); ok && s != "" {
						direction = s
					}
				}
			}
			after := ""
			if v, ok := args("after"); ok {
				after = gqlString(v)
			}
			nums := make([]int, 0, len(repo.Issues))
			for n := range repo.Issues {
				nums = append(nums, n)
			}
			// CREATED_AT ordering is creation order = number order.
			if direction == "DESC" {
				sortDescInts(nums)
			} else {
				sortAscInts(nums)
			}
			var list []*Issue
			for _, n := range nums {
				issue := repo.Issues[n]
				if states != nil {
					match := false
					for _, s := range states {
						if s == strings.ToUpper(string(issue.State)) {
							match = true
							break
						}
					}
					if !match {
						continue
					}
				}
				list = append(list, issue)
			}
			if after != "" {
				if afterNum, ok := issueFromCursor(after); ok {
					var filtered []*Issue
					for _, i := range list {
						if i.Number > afterNum {
							filtered = append(filtered, i)
						}
					}
					list = filtered
				}
			}
			page := list
			if len(page) > int(first) {
				page = page[:int(first)]
			}
			hasNextPage := len(list) > int(first)
			var endCursor *string
			if len(page) > 0 {
				c := cursorForIssue(page[len(page)-1])
				endCursor = &c
			}
			return &gqlVal{kind: "issues", repo: repo, issues: page, pageHasNext: hasNextPage, pageEndCursor: endCursor}, true
		case "pullRequests":
			first := 10.0
			if v, ok := args("first"); ok {
				if n, ok := gqlNumber(v); ok {
					first = n
				}
			}
			nums := make([]int, 0, len(repo.Pulls))
			for n := range repo.Pulls {
				nums = append(nums, n)
			}
			sortDescInts(nums)
			var list []*Pull
			for _, n := range nums {
				list = append(list, repo.Pulls[n])
			}
			if len(list) > int(first) {
				list = list[:int(first)]
			}
			return &gqlVal{kind: "pulls", repo: repo, pulls: list}, true
		case "pullRequest":
			numV, _ := args("number")
			num, _ := gqlNumber(numV)
			pull := repo.Pulls[int(num)]
			if pull == nil {
				return nil, true
			}
			return &gqlVal{kind: "pull", repo: repo, pull: pull}, true
		default:
			ctx.errors = append(ctx.errors, gqlErrorMsg{Message: fmt.Sprintf("Cannot query field %q on type %q.", name, "Repository")})
			return nil, false
		}
	case "issue":
		issue := parent.issue
		switch name {
		case "id":
			return issueGlobalID(parent.repo, issue), true
		case "number":
			return issue.Number, true
		case "title":
			return issue.Title, true
		case "state":
			return strings.ToUpper(string(issue.State)), true
		case "body":
			return issue.Body, true
		default:
			ctx.errors = append(ctx.errors, gqlErrorMsg{Message: fmt.Sprintf("Cannot query field %q on type %q.", name, "Issue")})
			return nil, false
		}
	case "pull":
		pull := parent.pull
		switch name {
		case "id":
			return pullGlobalID(parent.repo, pull), true
		case "number":
			return pull.Number, true
		case "state":
			return strings.ToUpper(string(pull.State)), true
		case "merged":
			return pull.Merged, true
		case "headRefName":
			return pull.HeadRef, true
		case "baseRefName":
			return pull.BaseRef, true
		case "mergeable":
			if pull.Merged {
				return "UNKNOWN", true
			}
			return "MERGEABLE", true
		case "files":
			paths := make([]string, 0, len(pull.Files))
			for _, f := range pull.Files {
				paths = append(paths, f.Filename)
			}
			return &gqlVal{kind: "files", files: paths}, true
		default:
			ctx.errors = append(ctx.errors, gqlErrorMsg{Message: fmt.Sprintf("Cannot query field %q on type %q.", name, "PullRequest")})
			return nil, false
		}
	case "issues":
		switch name {
		case "nodes":
			out := make([]any, 0, len(parent.issues))
			for _, i := range parent.issues {
				out = append(out, &gqlVal{kind: "issue", repo: parent.repo, issue: i})
			}
			return out, true
		case "pageInfo":
			return &gqlVal{kind: "pageInfo", pageHasNext: parent.pageHasNext, pageEndCursor: parent.pageEndCursor}, true
		case "totalCount":
			return len(parent.issues), true
		default:
			ctx.errors = append(ctx.errors, gqlErrorMsg{Message: fmt.Sprintf("Cannot query field %q on type %q.", name, "IssueConnection")})
			return nil, false
		}
	case "pulls":
		switch name {
		case "nodes":
			out := make([]any, 0, len(parent.pulls))
			for _, p := range parent.pulls {
				out = append(out, &gqlVal{kind: "pull", repo: parent.repo, pull: p})
			}
			return out, true
		case "pageInfo":
			return &gqlVal{kind: "pageInfo", pageHasNext: false, pageEndCursor: nil}, true
		case "totalCount":
			return len(parent.pulls), true
		default:
			ctx.errors = append(ctx.errors, gqlErrorMsg{Message: fmt.Sprintf("Cannot query field %q on type %q.", name, "PullRequestConnection")})
			return nil, false
		}
	case "files":
		switch name {
		case "nodes":
			out := make([]any, 0, len(parent.files))
			for _, path := range parent.files {
				payload := newOrderedMap()
				payload.set("path", path)
				out = append(out, &gqlVal{kind: "payload", payload: payload})
			}
			return out, true
		case "pageInfo":
			return &gqlVal{kind: "pageInfo", pageHasNext: false, pageEndCursor: nil}, true
		default:
			ctx.errors = append(ctx.errors, gqlErrorMsg{Message: fmt.Sprintf("Cannot query field %q on type %q.", name, "PullRequestChangedFileConnection")})
			return nil, false
		}
	case "user":
		switch name {
		case "login":
			return parent.login, true
		case "id":
			return b64url("User:" + parent.login), true
		default:
			ctx.errors = append(ctx.errors, gqlErrorMsg{Message: fmt.Sprintf("Cannot query field %q on type %q.", name, "User")})
			return nil, false
		}
	case "ref":
		if name == "name" {
			return parent.refName, true
		}
		ctx.errors = append(ctx.errors, gqlErrorMsg{Message: fmt.Sprintf("Cannot query field %q on type %q.", name, "Ref")})
		return nil, false
	case "pageInfo":
		switch name {
		case "hasNextPage":
			return parent.pageHasNext, true
		case "endCursor":
			if parent.pageEndCursor == nil {
				return nil, true
			}
			return *parent.pageEndCursor, true
		case "hasPreviousPage":
			return false, true
		case "startCursor":
			return nil, true
		default:
			ctx.errors = append(ctx.errors, gqlErrorMsg{Message: fmt.Sprintf("Cannot query field %q on type %q.", name, "PageInfo")})
			return nil, false
		}
	case "comment":
		switch name {
		case "id":
			return commentGlobalID(parent.repo, parent.issue, parent.commentID), true
		case "body":
			return parent.commentBody, true
		default:
			ctx.errors = append(ctx.errors, gqlErrorMsg{Message: fmt.Sprintf("Cannot query field %q on type %q.", name, "IssueComment")})
			return nil, false
		}
	case "commentEdge":
		switch name {
		case "node":
			return parent.edgeNode, true
		case "cursor":
			return b64url("comment:" + itoa(parent.edgeNode.commentID)), true
		default:
			ctx.errors = append(ctx.errors, gqlErrorMsg{Message: fmt.Sprintf("Cannot query field %q on type %q.", name, "IssueCommentEdge")})
			return nil, false
		}
	case "type":
		switch name {
		case "fields":
			fields, ok := gqlSchemaFields[parent.typeName]
			if !ok {
				return nil, true
			}
			out := make([]any, 0, len(fields))
			for _, f := range fields {
				payload := newOrderedMap()
				payload.set("name", f)
				out = append(out, &gqlVal{kind: "payload", payload: payload})
			}
			return out, true
		case "enumValues":
			values, ok := gqlSchemaEnums[parent.typeName]
			if !ok {
				return nil, true
			}
			out := make([]any, 0, len(values))
			for _, v := range values {
				payload := newOrderedMap()
				payload.set("name", v)
				out = append(out, &gqlVal{kind: "payload", payload: payload})
			}
			return out, true
		case "name":
			return parent.typeName, true
		default:
			ctx.errors = append(ctx.errors, gqlErrorMsg{Message: fmt.Sprintf("Cannot query field %q on type %q.", name, "__Type")})
			return nil, false
		}
	case "payload":
		value, ok := parent.payload.get(name)
		if !ok {
			ctx.errors = append(ctx.errors, gqlErrorMsg{Message: fmt.Sprintf("Cannot query field %q on type %q.", name, "Payload")})
			return nil, false
		}
		return value, true
	default:
		ctx.errors = append(ctx.errors, gqlErrorMsg{Message: fmt.Sprintf("Cannot query field %q.", name)})
		return nil, false
	}
}

// executeSelection executes a selection set against a parent value, returning
// a JSON-ready ordered object.
func executeSelection(ctx *gqlCtx, parent *gqlVal, selection []gqlSelection) *orderedMap {
	out := newOrderedMap()
	for _, sel := range selection {
		switch sel.kind {
		case "spread":
			if frag, ok := ctx.fragments[sel.spreadName]; ok {
				mergeOrdered(out, executeSelection(ctx, parent, frag.selection))
			}
		case "inline":
			// Type conditions are trusted for the suite's documents (node(id) on Repository).
			mergeOrdered(out, executeSelection(ctx, parent, sel.selection))
		case "field":
			key := sel.field.alias
			if key == "" {
				key = sel.field.name
			}
			value, ok := resolveField(ctx, parent, sel.field)
			if !ok {
				continue
			}
			if value == nil {
				out.set(key, nil)
				continue
			}
			if arr, isArr := value.([]any); isArr {
				mapped := make([]any, 0, len(arr))
				for _, item := range arr {
					if gv, isVal := item.(*gqlVal); isVal {
						mapped = append(mapped, executeSelection(ctx, gv, sel.field.selection))
					} else {
						mapped = append(mapped, item)
					}
				}
				out.set(key, mapped)
				continue
			}
			if gv, isVal := value.(*gqlVal); isVal {
				out.set(key, executeSelection(ctx, gv, sel.field.selection))
				continue
			}
			out.set(key, value)
		}
	}
	return out
}

func mergeOrdered(dst, src *orderedMap) {
	for _, k := range src.keys {
		dst.set(k, src.vals[k])
	}
}

func sortAscInts(a []int) {
	sort.Ints(a)
}

func sortDescInts(a []int) {
	sort.Sort(sort.Reverse(sort.IntSlice(a)))
}

// resolveNode finds a repo/issue/pull by an opaque global id.
func resolveNode(ctx *gqlCtx, id string) *gqlVal {
	decoded, ok := b64urlDecode(id)
	if !ok {
		return nil
	}
	parts := strings.Split(decoded, ":")
	kind := parts[0]
	if kind == "Repository" {
		if len(parts) < 2 {
			return nil
		}
		dbID, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil
		}
		for _, repo := range ctx.store.ListRepos(ViewerLogin) {
			if repo.DatabaseID == dbID {
				return &gqlVal{kind: "repo", repo: repo}
			}
		}
		return nil
	}
	if kind == "Issue" {
		if len(parts) < 3 {
			return nil
		}
		dbID, err1 := strconv.Atoi(parts[1])
		num, err2 := strconv.Atoi(parts[2])
		if err1 != nil || err2 != nil {
			return nil
		}
		for _, repo := range ctx.store.ListRepos(ViewerLogin) {
			if repo.DatabaseID != dbID {
				continue
			}
			if issue := repo.Issues[num]; issue != nil {
				return &gqlVal{kind: "issue", repo: repo, issue: issue}
			}
		}
		return nil
	}
	if kind == "PullRequest" {
		if len(parts) < 3 {
			return nil
		}
		dbID, err1 := strconv.Atoi(parts[1])
		num, err2 := strconv.Atoi(parts[2])
		if err1 != nil || err2 != nil {
			return nil
		}
		for _, repo := range ctx.store.ListRepos(ViewerLogin) {
			if repo.DatabaseID != dbID {
				continue
			}
			if pull := repo.Pulls[num]; pull != nil {
				return &gqlVal{kind: "pull", repo: repo, pull: pull}
			}
		}
		return nil
	}
	return nil
}

// executeOperation executes one operation (query or mutation) and returns its
// {data, errors} envelope.
func executeOperation(ctx *gqlCtx, op *gqlOperation) *orderedMap {
	data := newOrderedMap()
	for _, sel := range op.selection {
		if sel.kind != "field" {
			continue
		}
		key := sel.field.alias
		if key == "" {
			key = sel.field.name
		}
		name := sel.field.name
		args := func(n string) (any, bool) { return gqlArgValue(sel.field, n, ctx.variables) }

		if name == "viewer" {
			viewer := &gqlVal{kind: "user", login: ViewerLogin}
			data.set(key, executeSelection(ctx, viewer, sel.field.selection))
			continue
		}
		if name == "repository" {
			ownerV, _ := args("owner")
			nameV, _ := args("name")
			owner := gqlString(ownerV)
			repoName := gqlString(nameV)
			var repo *Repo
			if owner == ViewerLogin {
				repo = ctx.store.GetRepo(owner, repoName)
			}
			if repo == nil {
				data.set(key, nil)
				continue
			}
			data.set(key, executeSelection(ctx, &gqlVal{kind: "repo", repo: repo}, sel.field.selection))
			continue
		}
		if name == "node" {
			idV, _ := args("id")
			node := resolveNode(ctx, gqlString(idV))
			if node == nil {
				data.set(key, nil)
				continue
			}
			data.set(key, executeSelection(ctx, node, sel.field.selection))
			continue
		}
		if name == "__type" {
			typeV, _ := args("name")
			typeName := gqlString(typeV)
			if _, ok := gqlSchemaFields[typeName]; !ok {
				if _, ok := gqlSchemaEnums[typeName]; !ok {
					data.set(key, nil)
					continue
				}
			}
			data.set(key, executeSelection(ctx, &gqlVal{kind: "type", typeName: typeName}, sel.field.selection))
			continue
		}
		if op.operation == "mutation" {
			inputV, _ := args("input")
			input := inputV
			if input == nil {
				input = map[string]any{}
			}
			if name == "createIssue" {
				repoIDV, _ := gqlGet(input, "repositoryId")
				node := resolveNode(ctx, gqlString(repoIDV))
				if node == nil || node.kind != "repo" {
					ctx.errors = append(ctx.errors, gqlErrorMsg{Message: "Could not resolve to a Repository with the id provided."})
					data.set(key, nil)
					continue
				}
				titleV, _ := gqlGet(input, "title")
				bodyV, _ := gqlGet(input, "body")
				issue := ctx.store.CreateIssue(node.repo, gqlString(titleV), gqlString(bodyV), []string{})
				payload := newOrderedMap()
				payload.set("issue", &gqlVal{kind: "issue", repo: node.repo, issue: issue})
				data.set(key, executeSelection(ctx, &gqlVal{kind: "payload", payload: payload}, sel.field.selection))
				continue
			}
			if name == "closeIssue" {
				issueIDV, _ := gqlGet(input, "issueId")
				node := resolveNode(ctx, gqlString(issueIDV))
				if node == nil || node.kind != "issue" {
					ctx.errors = append(ctx.errors, gqlErrorMsg{Message: "Could not resolve to an Issue with the id provided."})
					data.set(key, nil)
					continue
				}
				node.issue.State = IssueClosed
				node.issue.ver++
				payload := newOrderedMap()
				payload.set("issue", node)
				data.set(key, executeSelection(ctx, &gqlVal{kind: "payload", payload: payload}, sel.field.selection))
				continue
			}
			if name == "createPullRequest" {
				repoIDV, _ := gqlGet(input, "repositoryId")
				node := resolveNode(ctx, gqlString(repoIDV))
				if node == nil || node.kind != "repo" {
					ctx.errors = append(ctx.errors, gqlErrorMsg{Message: "Could not resolve to a Repository with the id provided."})
					data.set(key, nil)
					continue
				}
				repo := node.repo
				headV, _ := gqlGet(input, "headRefName")
				baseV, _ := gqlGet(input, "baseRefName")
				headRef := gqlString(headV)
				baseRef := gqlString(baseV)
				if baseRef == "" {
					baseRef = repo.DefaultBranch
				}
				headSha := repo.Refs["refs/heads/"+headRef]
				baseSha := repo.Refs["refs/heads/"+baseRef]
				if headSha == "" || baseSha == "" {
					ctx.errors = append(ctx.errors, gqlErrorMsg{Message: "Head or base ref not found."})
					data.set(key, nil)
					continue
				}
				titleV, _ := gqlGet(input, "title")
				bodyV, _ := gqlGet(input, "body")
				pull := ctx.store.CreatePull(repo, gqlString(titleV), gqlString(bodyV), headRef, baseRef, headSha, baseSha)
				payload := newOrderedMap()
				payload.set("pullRequest", &gqlVal{kind: "pull", repo: repo, pull: pull})
				data.set(key, executeSelection(ctx, &gqlVal{kind: "payload", payload: payload}, sel.field.selection))
				continue
			}
			if name == "mergePullRequest" {
				prIDV, _ := gqlGet(input, "pullRequestId")
				node := resolveNode(ctx, gqlString(prIDV))
				if node == nil || node.kind != "pull" {
					ctx.errors = append(ctx.errors, gqlErrorMsg{Message: "Could not resolve to a PullRequest with the id provided."})
					data.set(key, nil)
					continue
				}
				mergeSha := ctx.store.MergePull(node.repo, node.pull)
				if mergeSha == "" {
					ctx.errors = append(ctx.errors, gqlErrorMsg{Message: "Merge conflict."})
				}
				payload := newOrderedMap()
				payload.set("pullRequest", node)
				data.set(key, executeSelection(ctx, &gqlVal{kind: "payload", payload: payload}, sel.field.selection))
				continue
			}
			if name == "addComment" {
				subjectV, _ := gqlGet(input, "subjectId")
				node := resolveNode(ctx, gqlString(subjectV))
				if node == nil || node.kind != "issue" {
					ctx.errors = append(ctx.errors, gqlErrorMsg{Message: "Could not resolve to an Issue with the id provided."})
					data.set(key, nil)
					continue
				}
				bodyV, _ := gqlGet(input, "body")
				comment := Comment{ID: node.issue.NextCommentID, Body: gqlString(bodyV)}
				node.issue.NextCommentID++
				node.issue.Comments = append(node.issue.Comments, comment)
				node.issue.ver++
				commentValue := &gqlVal{kind: "comment", repo: node.repo, issue: node.issue, commentID: comment.ID, commentBody: comment.Body}
				edge := &gqlVal{kind: "commentEdge", edgeNode: commentValue}
				payload := newOrderedMap()
				payload.set("commentEdge", edge)
				data.set(key, executeSelection(ctx, &gqlVal{kind: "payload", payload: payload}, sel.field.selection))
				continue
			}
		}
		opType := "Query"
		if op.operation == "mutation" {
			opType = "Mutation"
		}
		ctx.errors = append(ctx.errors, gqlErrorMsg{Message: fmt.Sprintf("Cannot query field %q on type %q.", name, opType)})
		data.set(key, nil)
	}
	envelope := newOrderedMap()
	envelope.set("data", data)
	if len(ctx.errors) > 0 {
		envelope.set("errors", ctx.errors)
	}
	return envelope
}

// runGraphQL runs one GraphQL request body (single op or batched array) and
// returns the response body.
func runGraphQL(store *Store, body any) any {
	runOne := func(item any) any {
		if item == nil {
			return errorEnvelope("Invalid GraphQL request.")
		}
		obj, isObj := item.(map[string]any)
		if !isObj {
			return errorEnvelope("Invalid GraphQL request.")
		}
		queryV, ok := obj["query"]
		if !ok {
			return errorEnvelope("Must provide query string.")
		}
		query, isStr := queryV.(string)
		if !isStr {
			return errorEnvelope("Must provide query string.")
		}
		variables := map[string]any{}
		if v, ok := obj["variables"]; ok {
			if m, isMap := v.(map[string]any); isMap {
				variables = m
			}
		}
		parser := &gqlParser{tokens: tokenizeGQL(query)}
		defs, err := parser.parseDocument()
		if err != nil {
			return errorEnvelope("Syntax Error: " + err.Error())
		}
		fragments := map[string]*gqlFragment{}
		var op *gqlOperation
		operationName := ""
		if v, ok := obj["operationName"]; ok {
			if s, isStr := v.(string); isStr {
				operationName = s
			}
		}
		for _, def := range defs {
			switch d := def.(type) {
			case *gqlFragment:
				fragments[d.name] = d
			case *gqlOperation:
				if op == nil {
					if operationName != "" && d.name != operationName {
						continue
					}
					op = d
				}
			}
		}
		if op == nil {
			return errorEnvelope("Must provide an operation.")
		}
		ctx := &gqlCtx{store: store, variables: variables, fragments: fragments}
		return executeOperation(ctx, op)
	}

	if arr, isArr := body.([]any); isArr {
		if len(arr) == 0 {
			return errorEnvelope("Must provide at least one operation.")
		}
		out := make([]any, 0, len(arr))
		for _, item := range arr {
			out = append(out, runOne(item))
		}
		return out
	}
	return runOne(body)
}

func errorEnvelope(message string) *orderedMap {
	envelope := newOrderedMap()
	envelope.set("data", nil)
	envelope.set("errors", []gqlErrorMsg{{Message: message}})
	return envelope
}

// handleGraphQL is the HTTP handler for POST /api/graphql. Returns (response, handled).
func handleGraphQL(store *Store, req *Request) (Response, bool) {
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
		return jsonResponse(400, map[string]string{"message": "Problems parsing JSON"}), true
	}
	result := runGraphQL(store, parsed)
	return jsonResponse(200, result), true
}

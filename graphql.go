package main

// GraphQL v4: a parser and executor for the documents gh and github-mcp-server send.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

type token struct {
	kind  string // name, int, float, string, punct, spread
	value string
}

func lex(text string) []token {
	var out []token
	at := 0
	for at < len(text) {
		c := text[at]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			at++
		case c == '#':
			for at < len(text) && text[at] != '\n' {
				at++
			}
		case strings.HasPrefix(text[at:], "..."):
			out = append(out, token{kind: "spread", value: "..."})
			at += 3
		case strings.IndexByte("{}()[]:$!=@|", c) >= 0:
			out = append(out, token{kind: "punct", value: string(c)})
			at++
		case c == '"':
			at++
			var sb strings.Builder
			for at < len(text) && text[at] != '"' {
				if text[at] == '\\' && at+1 < len(text) {
					sb.WriteByte(text[at+1])
					at += 2
					continue
				}
				sb.WriteByte(text[at])
				at++
			}
			at++
			out = append(out, token{kind: "string", value: sb.String()})
		case (c >= '0' && c <= '9') || (c == '-' && at+1 < len(text) && text[at+1] >= '0' && text[at+1] <= '9'):
			start := at
			at++
			for at < len(text) && (text[at] >= '0' && text[at] <= '9' || text[at] == '.' || text[at] == 'e' || text[at] == 'E' || text[at] == '+' || text[at] == '-') {
				at++
			}
			val := text[start:at]
			kind := "int"
			if strings.ContainsAny(val, ".eE") {
				kind = "float"
			}
			out = append(out, token{kind: kind, value: val})
		case c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			start := at
			for at < len(text) && (text[at] == '_' || text[at] >= 'a' && text[at] <= 'z' || text[at] >= 'A' && text[at] <= 'Z' || text[at] >= '0' && text[at] <= '9') {
				at++
			}
			out = append(out, token{kind: "name", value: text[start:at]})
		default:
			at++
		}
	}
	return out
}

type field struct {
	alias        string
	name         string
	args         map[string]any
	selections   []*field
	typeCondition string
}

type operation struct {
	kind       string
	name       string
	variables  map[string]any
	selections []*field
}

type parser struct {
	tokens []token
	at     int
}

func (p *parser) peek() *token {
	if p.at < len(p.tokens) {
		return &p.tokens[p.at]
	}
	return nil
}

func (p *parser) next() *token {
	t := p.peek()
	if t != nil {
		p.at++
	}
	return t
}

func (p *parser) expectPunct(v string) error {
	t := p.next()
	if t == nil || t.value != v {
		return fmt.Errorf("expected %s", v)
	}
	return nil
}

func (p *parser) expectName() (string, error) {
	t := p.next()
	if t == nil || t.kind != "name" {
		return "", fmt.Errorf("expected a name")
	}
	return t.value, nil
}

func parseDocument(tokens []token) ([]*operation, error) {
	p := &parser{tokens: tokens}
	var ops []*operation
	for p.at < len(p.tokens) {
		op, err := p.parseOperation()
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	return ops, nil
}

func (p *parser) parseOperation() (*operation, error) {
	op := &operation{kind: "query", variables: map[string]any{}}
	t := p.peek()
	if t != nil && t.kind == "name" && (t.value == "query" || t.value == "mutation") {
		op.kind = t.value
		p.next()
		if n := p.peek(); n != nil && n.kind == "name" {
			op.name = n.value
			p.next()
		}
	}
	if p.peek() != nil && p.peek().value == "(" {
		p.next()
		for p.peek() != nil && p.peek().value != ")" {
			if err := p.expectPunct("$"); err != nil {
				return nil, err
			}
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			depth := 0
			for p.peek() != nil {
				tk := p.peek()
				if tk.value == "[" {
					depth++
				} else if tk.value == "]" {
					depth--
				} else if (tk.value == "=" || tk.value == ")") && depth == 0 {
					break
				}
				p.next()
			}
			var def any
			if p.peek() != nil && p.peek().value == "=" {
				p.next()
				def, _ = p.parseValue()
			}
			op.variables[name] = def
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
	}
	sel, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	op.selections = sel
	return op, nil
}

func (p *parser) parseSelectionSet() ([]*field, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var out []*field
	for p.peek() != nil && p.peek().value != "}" {
		f, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	if err := p.expectPunct("}"); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *parser) parseSelection() (*field, error) {
	if p.peek() != nil && p.peek().kind == "spread" {
		p.next()
		if p.peek() != nil && p.peek().value == "on" {
			p.next()
			cond, err := p.expectName()
			if err != nil {
				return nil, err
			}
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			return &field{name: "__inline", args: map[string]any{}, selections: sel, typeCondition: cond}, nil
		}
		if p.peek() != nil && p.peek().kind == "name" {
			p.next()
		}
		return &field{name: "__skip", args: map[string]any{}}, nil
	}
	name, err := p.expectName()
	if err != nil {
		return nil, err
	}
	f := &field{name: name, args: map[string]any{}}
	if p.peek() != nil && p.peek().value == ":" {
		p.next()
		f.alias = name
		f.name, err = p.expectName()
		if err != nil {
			return nil, err
		}
	}
	if p.peek() != nil && p.peek().value == "(" {
		p.next()
		for p.peek() != nil && p.peek().value != ")" {
			argName, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			value, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			f.args[argName] = value
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
	}
	if p.peek() != nil && p.peek().value == "{" {
		f.selections, err = p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
	}
	return f, nil
}

func (p *parser) parseValue() (any, error) {
	t := p.next()
	if t == nil {
		return nil, nil
	}
	switch {
	case t.value == "$":
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		return map[string]any{"__var": name}, nil
	case t.value == "[":
		var list []any
		for p.peek() != nil && p.peek().value != "]" {
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
		p.expectPunct("]")
		return list, nil
	case t.value == "{":
		obj := map[string]any{}
		for p.peek() != nil && p.peek().value != "}" {
			key, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			obj[key] = v
		}
		p.expectPunct("}")
		return obj, nil
	case t.kind == "int":
		v, _ := strconv.Atoi(t.value)
		return v, nil
	case t.kind == "float":
		v, _ := strconv.ParseFloat(t.value, 64)
		return v, nil
	case t.kind == "string":
		return t.value, nil
	case t.value == "null":
		return nil, nil
	case t.value == "true":
		return true, nil
	case t.value == "false":
		return false, nil
	}
	return t.value, nil
}

func bind(value any, vars map[string]any) any {
	switch v := value.(type) {
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = bind(e, vars)
		}
		return out
	case map[string]any:
		if name, ok := v["__var"].(string); ok {
			if got, exists := vars[name]; exists {
				return got
			}
			return nil
		}
		out := make(map[string]any, len(v))
		for k, e := range v {
			out[k] = bind(e, vars)
		}
		return out
	}
	return value
}

// ---------------------------------------------------------------- entities

type entity struct {
	kind  string
	value any
}

func globalID(kind string, id int64) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%d", kind, id)))
}

func parseGlobalID(id string) (string, int64, bool) {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return "", 0, false
	}
	text := string(raw)
	idx := strings.LastIndexByte(text, ':')
	if idx < 0 {
		return "", 0, false
	}
	n, err := strconv.ParseInt(text[idx+1:], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return text[:idx], n, true
}

type issueRef struct {
	repo  *Repo
	issue *Issue
}

type pullRef struct {
	repo *Repo
	pull *Pull
}

var schema = map[string]struct {
	fields  []string
	enums   []string
	hasEnum bool
}{
	"Issue":                 {fields: []string{"id", "number", "title", "state", "body", "createdAt", "updatedAt", "closed", "author", "labels", "comments"}},
	"PullRequest":           {fields: []string{"id", "number", "state", "merged", "headRefName", "baseRefName", "mergeable", "files", "title", "body", "createdAt", "mergeCommit"}},
	"Repository":            {fields: []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"}},
	"User":                  {fields: []string{"login", "id", "name"}},
	"SearchType":            {enums: []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION"}, hasEnum: true},
	"IssueState":            {enums: []string{"OPEN", "CLOSED"}, hasEnum: true},
	"PullRequestState":      {enums: []string{"OPEN", "CLOSED", "MERGED"}, hasEnum: true},
	"MergeableState":        {enums: []string{"MERGEABLE", "CONFLICTING", "UNKNOWN"}, hasEnum: true},
	"PullRequestMergeMethod": {enums: []string{"MERGE", "SQUASH", "REBASE"}, hasEnum: true},
	"RepositoryPermission":  {enums: []string{"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"}, hasEnum: true},
	"IssueOrderField":       {enums: []string{"CREATED_AT", "UPDATED_AT", "COMMENTS"}, hasEnum: true},
	"OrderDirection":        {enums: []string{"ASC", "DESC"}, hasEnum: true},
}

type gqlError struct{ message string }

func (e gqlError) Error() string { return e.message }

func findIssueByID(store *Store, id int64) (*Repo, *Issue, bool) {
	for _, repo := range store.Repos {
		for _, issue := range repo.Issues {
			if issue.ID == id {
				return repo, issue, true
			}
		}
	}
	return nil, nil, false
}

func findPullByID(store *Store, id int64) (*Repo, *Pull, bool) {
	for _, repo := range store.Repos {
		for _, pull := range repo.Pulls {
			if pull.ID == id {
				return repo, pull, true
			}
		}
	}
	return nil, nil, false
}

func findRepoByID(store *Store, id int64) (*Repo, bool) {
	for _, repo := range store.Repos {
		if repo.ID == id {
			return repo, true
		}
	}
	return nil, false
}

func resolveRoot(store *Store, name string, args map[string]any) (any, error) {
	switch name {
	case "viewer":
		login := store.Login()
		return &entity{kind: "User", value: map[string]any{"login": login, "id": int64(1)}}, nil
	case "repository":
		owner := asString(args["owner"])
		repoName := asString(args["name"])
		store.LearnOwner(owner)
		repo := store.Find(owner, repoName)
		if repo == nil {
			return nil, nil
		}
		return &entity{kind: "Repository", value: repo}, nil
	case "__type":
		typeName := asString(args["name"])
		entry, ok := schema[typeName]
		if !ok {
			return nil, nil
		}
		return &entity{kind: "__Type", value: map[string]any{"name": typeName, "fields": entry.fields, "enums": entry.enums, "hasEnum": entry.hasEnum}}, nil
	case "node":
		kind, id, ok := parseGlobalID(asString(args["id"]))
		if !ok {
			return nil, nil
		}
		switch kind {
		case "Repository":
			if repo, found := findRepoByID(store, id); found {
				return &entity{kind: "Repository", value: repo}, nil
			}
		case "Issue":
			if repo, issue, found := findIssueByID(store, id); found {
				return &entity{kind: "Issue", value: issueRef{repo, issue}}, nil
			}
		case "PullRequest":
			if repo, pull, found := findPullByID(store, id); found {
				return &entity{kind: "PullRequest", value: pullRef{repo, pull}}, nil
			}
		}
		return nil, nil
	}
	return nil, gqlError{"Cannot query field \"" + name + "\" on type \"Query\""}
}

func resolveMutation(store *Store, name string, args map[string]any) (any, error) {
	input, _ := args["input"].(map[string]any)
	if input == nil {
		input = map[string]any{}
	}
	switch name {
	case "createIssue":
		_, id, _ := parseGlobalID(asString(input["repositoryId"]))
		repo, ok := findRepoByID(store, id)
		if !ok {
			return nil, gqlError{"Could not resolve to a Repository with that id"}
		}
		now := store.Clock.Tick()
		issue := &Issue{ID: store.NewIssueID(), Number: repo.NextIssue, Title: asString(input["title"]), Body: asString(input["body"]), State: "open", CreatedAt: now, UpdatedAt: now}
		repo.NextIssue++
		repo.Issues = append(repo.Issues, issue)
		return map[string]any{"issue": &entity{kind: "Issue", value: issueRef{repo, issue}}}, nil
	case "closeIssue":
		_, id, _ := parseGlobalID(asString(input["issueId"]))
		repo, issue, ok := findIssueByID(store, id)
		if !ok {
			return nil, gqlError{"Could not resolve to an Issue with that id"}
		}
		issue.State = "closed"
		closed := store.Clock.Tick()
		issue.ClosedAt = &closed
		issue.UpdatedAt = closed
		delete(repo.IssueJSON, issue.ID)
		return map[string]any{"issue": &entity{kind: "Issue", value: issueRef{repo, issue}}}, nil
	case "createPullRequest":
		_, id, _ := parseGlobalID(asString(input["repositoryId"]))
		repo, ok := findRepoByID(store, id)
		if !ok {
			return nil, gqlError{"Could not resolve to a Repository with that id"}
		}
		headRef := asString(input["headRefName"])
		baseRef := asString(input["baseRefName"])
		if baseRef == "" {
			baseRef = repo.DefaultBranch
		}
		headTip := repo.Refs["refs/heads/"+headRef]
		baseTip := repo.Refs["refs/heads/"+baseRef]
		if headTip == "" || baseTip == "" {
			return nil, gqlError{"Could not resolve to a branch with that name"}
		}
		now := store.Clock.Tick()
		pull := &Pull{ID: store.NewPullID(), Number: repo.NextPull, Title: asString(input["title"]), Body: asString(input["body"]), State: "open", Head: Side{Ref: headRef, SHA: headTip}, Base: Side{Ref: baseRef, SHA: baseTip}, CreatedAt: now, UpdatedAt: now, Mergeable: true}
		repo.NextPull++
		repo.Pulls = append(repo.Pulls, pull)
		return map[string]any{"pullRequest": &entity{kind: "PullRequest", value: pullRef{repo, pull}}}, nil
	case "mergePullRequest":
		_, id, _ := parseGlobalID(asString(input["pullRequestId"]))
		repo, pull, ok := findPullByID(store, id)
		if !ok {
			return nil, gqlError{"Could not resolve to a PullRequest with that id"}
		}
		if !pull.Merged && pullMergeable(repo, pull) {
			baseTip := repo.Refs["refs/heads/"+pull.Base.Ref]
			headObj := repo.ReadObject(repo.Refs["refs/heads/"+pull.Head.Ref])
			if baseTip != "" && headObj != nil {
				tree := ParseCommit(headObj.Data).Tree
				now := store.Clock.Tick()
				message := fmt.Sprintf("Merge pull request #%d from %s\n\n%s", pull.Number, pull.Head.Ref, pull.Title)
				sha := repo.PutObject("commit", BuildCommit(tree, []string{baseTip}, message, repo.Owner, repo.Owner+"@example.invalid", now))
				repo.Refs["refs/heads/"+pull.Base.Ref] = sha
				repo.clearCaches()
				pull.Merged = true
				pull.State = "closed"
				pull.MergedAt = &now
				pull.ClosedAt = &now
				pull.MergeCommitSHA = &sha
				pull.UpdatedAt = now
			}
		}
		return map[string]any{"pullRequest": &entity{kind: "PullRequest", value: pullRef{repo, pull}}}, nil
	case "addComment":
		_, id, _ := parseGlobalID(asString(input["subjectId"]))
		repo, issue, ok := findIssueByID(store, id)
		if !ok {
			return nil, gqlError{"Could not resolve to an Issue with that id"}
		}
		now := store.Clock.Tick()
		comment := &Comment{ID: store.NewCommentID(), Body: asString(input["body"]), CreatedAt: now, UpdatedAt: now}
		issue.Comments = append(issue.Comments, comment)
		issue.UpdatedAt = now
		delete(repo.IssueJSON, issue.ID)
		return map[string]any{"commentEdge": map[string]any{"node": map[string]any{"id": globalID("IssueComment", comment.ID), "body": comment.Body}}}, nil
	}
	return nil, gqlError{"Cannot query field \"" + name + "\" on type \"Mutation\""}
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case float64:
		return int(n), true
	}
	return 0, false
}

func issueCursor(i int) string { return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(i))) }

func cursorIndex(cursor string) int {
	raw, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return 0
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 {
		return 0
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0
	}
	return n
}

func resolveField(parent *entity, name string, args map[string]any) (any, error) {
	switch parent.kind {
	case "__Type":
		info := parent.value.(map[string]any)
		switch name {
		case "name":
			return info["name"], nil
		case "fields":
			fields, _ := info["fields"].([]string)
			out := make([]*entity, 0, len(fields))
			for _, f := range fields {
				out = append(out, &entity{kind: "__Field", value: f})
			}
			return out, nil
		case "enumValues":
			enums, _ := info["enums"].([]string)
			out := make([]*entity, 0, len(enums))
			for _, e := range enums {
				out = append(out, &entity{kind: "__EnumValue", value: e})
			}
			return out, nil
		case "kind":
			if info["hasEnum"] == true {
				return "ENUM", nil
			}
			return "OBJECT", nil
		}
		return nil, gqlError{"Cannot query field \"" + name + "\" on type \"__Type\""}
	case "__Field", "__EnumValue":
		if name == "name" {
			return parent.value, nil
		}
		if name == "type" {
			return &entity{kind: "__TypeRef", value: map[string]any{"name": "String", "kind": "SCALAR"}}, nil
		}
		if name == "isDeprecated" {
			return false, nil
		}
		return nil, gqlError{"Cannot query field \"" + name + "\" on type \"" + parent.kind + "\""}
	case "__TypeRef":
		ref := parent.value.(map[string]any)
		if name == "name" || name == "kind" {
			return ref[name], nil
		}
		return nil, nil
	case "User":
		user := parent.value.(map[string]any)
		switch name {
		case "login":
			return user["login"], nil
		case "id":
			return globalID("User", 1), nil
		case "name":
			return user["login"], nil
		}
		return nil, gqlError{"Cannot query field \"" + name + "\" on type \"User\""}
	case "Repository":
		repo := parent.value.(*Repo)
		switch name {
		case "id":
			return globalID("Repository", repo.ID), nil
		case "databaseId":
			return repo.ID, nil
		case "name":
			return repo.Name, nil
		case "nameWithOwner":
			return repo.Owner + "/" + repo.Name, nil
		case "owner":
			return &entity{kind: "User", value: map[string]any{"login": repo.Owner}}, nil
		case "defaultBranchRef":
			if _, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; !ok {
				return nil, nil
			}
			return &entity{kind: "Ref", value: repo.DefaultBranch}, nil
		case "viewerPermission":
			return "ADMIN", nil
		case "hasIssuesEnabled":
			return true, nil
		case "issue":
			number, _ := asInt(args["number"])
			for _, issue := range repo.Issues {
				if issue.Number == number {
					return &entity{kind: "Issue", value: issueRef{repo, issue}}, nil
				}
			}
			return nil, nil
		case "issues":
			states, hasStates := args["states"].([]any)
			orderBy, _ := args["orderBy"].(map[string]any)
			direction := "DESC"
			orderField := "CREATED_AT"
			if orderBy != nil {
				if v := asString(orderBy["direction"]); v != "" {
					direction = strings.ToUpper(v)
				}
				if v := asString(orderBy["field"]); v != "" {
					orderField = strings.ToUpper(v)
				}
			}
			list := make([]*Issue, 0, len(repo.Issues))
			for _, issue := range repo.Issues {
				if hasStates && len(states) > 0 {
					matched := false
					for _, s := range states {
						if strings.ToLower(asString(s)) == issue.State {
							matched = true
							break
						}
					}
					if !matched {
						continue
					}
				}
				list = append(list, issue)
			}
			sort.SliceStable(list, func(a, b int) bool {
				ka, kb := list[a].CreatedAt, list[b].CreatedAt
				if orderField == "UPDATED_AT" {
					ka, kb = list[a].UpdatedAt, list[b].UpdatedAt
				}
				if ka == kb {
					if direction == "ASC" {
						return list[a].Number < list[b].Number
					}
					return list[a].Number > list[b].Number
				}
				if direction == "ASC" {
					return ka < kb
				}
				return ka > kb
			})
			first := len(list)
			if v, ok := asInt(args["first"]); ok {
				first = v
			}
			after := 0
			if cursor, ok := args["after"].(string); ok {
				after = cursorIndex(cursor)
			}
			if after > len(list) {
				after = len(list)
			}
			end := after + first
			if end > len(list) {
				end = len(list)
			}
			page := list[after:end]
			nodes := make([]*entity, 0, len(page))
			for _, issue := range page {
				nodes = append(nodes, &entity{kind: "Issue", value: issueRef{repo, issue}})
			}
			return &entity{kind: "IssueConnection", value: map[string]any{
				"nodes":      nodes,
				"totalCount": len(list),
				"pageInfo":   map[string]any{"hasNextPage": end < len(list), "endCursor": issueCursor(end), "hasPreviousPage": after > 0, "startCursor": issueCursor(after)},
			}}, nil
		case "pullRequests":
			list := make([]*Pull, len(repo.Pulls))
			copy(list, repo.Pulls)
			sort.SliceStable(list, func(a, b int) bool { return list[a].Number > list[b].Number })
			first := len(list)
			if v, ok := asInt(args["first"]); ok {
				first = v
			}
			if first > len(list) {
				first = len(list)
			}
			page := list[:first]
			nodes := make([]*entity, 0, len(page))
			for _, pull := range page {
				nodes = append(nodes, &entity{kind: "PullRequest", value: pullRef{repo, pull}})
			}
			return &entity{kind: "PullRequestConnection", value: map[string]any{
				"nodes":      nodes,
				"totalCount": len(list),
				"pageInfo":   map[string]any{"hasNextPage": first < len(list), "endCursor": issueCursor(first), "hasPreviousPage": false},
			}}, nil
		case "pullRequest":
			number, _ := asInt(args["number"])
			for _, pull := range repo.Pulls {
				if pull.Number == number {
					return &entity{kind: "PullRequest", value: pullRef{repo, pull}}, nil
				}
			}
			return nil, nil
		}
		return nil, gqlError{"Cannot query field \"" + name + "\" on type \"Repository\""}
	case "Ref":
		if name == "name" {
			return parent.value, nil
		}
		if name == "target" {
			return nil, nil
		}
		return nil, gqlError{"Cannot query field \"" + name + "\" on type \"Ref\""}
	case "Issue":
		ref := parent.value.(issueRef)
		issue := ref.issue
		switch name {
		case "id":
			return globalID("Issue", issue.ID), nil
		case "number":
			return issue.Number, nil
		case "title":
			return issue.Title, nil
		case "state":
			if issue.State == "open" {
				return "OPEN", nil
			}
			return "CLOSED", nil
		case "body":
			return issue.Body, nil
		case "createdAt":
			return issue.CreatedAt, nil
		case "updatedAt":
			return issue.UpdatedAt, nil
		case "closed":
			return issue.State == "closed", nil
		}
		return nil, gqlError{"Cannot query field \"" + name + "\" on type \"Issue\""}
	case "PullRequest":
		ref := parent.value.(pullRef)
		pull := ref.pull
		switch name {
		case "id":
			return globalID("PullRequest", pull.ID), nil
		case "number":
			return pull.Number, nil
		case "title":
			return pull.Title, nil
		case "body":
			return pull.Body, nil
		case "state":
			if pull.Merged {
				return "MERGED", nil
			}
			if pull.State == "open" {
				return "OPEN", nil
			}
			return "CLOSED", nil
		case "merged":
			return pull.Merged, nil
		case "headRefName":
			return pull.Head.Ref, nil
		case "baseRefName":
			return pull.Base.Ref, nil
		case "mergeable":
			if pullMergeable(ref.repo, pull) {
				return "MERGEABLE", nil
			}
			return "CONFLICTING", nil
		case "files":
			files := diffFiles(ref.repo, pull.Base.Ref, pull.Head.Ref)
			first := len(files)
			if v, ok := asInt(args["first"]); ok {
				first = v
			}
			if first > len(files) {
				first = len(files)
			}
			page := files[:first]
			nodes := make([]*entity, 0, len(page))
			for _, f := range page {
				nodes = append(nodes, &entity{kind: "PullRequestChangedFile", value: f.Filename})
			}
			return &entity{kind: "PullRequestChangedFileConnection", value: map[string]any{
				"nodes":      nodes,
				"totalCount": len(files),
				"pageInfo":   map[string]any{"hasNextPage": first < len(files), "endCursor": issueCursor(first), "hasPreviousPage": false},
			}}, nil
		}
		return nil, gqlError{"Cannot query field \"" + name + "\" on type \"PullRequest\""}
	case "PullRequestChangedFile":
		if name == "path" {
			return parent.value, nil
		}
		return nil, gqlError{"Cannot query field \"" + name + "\" on type \"PullRequestChangedFile\""}
	}

	if strings.HasSuffix(parent.kind, "Connection") {
		conn := parent.value.(map[string]any)
		switch name {
		case "nodes":
			return conn["nodes"], nil
		case "totalCount":
			return conn["totalCount"], nil
		case "pageInfo":
			return &entity{kind: "PageInfo", value: conn["pageInfo"]}, nil
		case "edges":
			nodes, _ := conn["nodes"].([]*entity)
			edges := make([]*entity, 0, len(nodes))
			for i, n := range nodes {
				edges = append(edges, &entity{kind: "Edge", value: map[string]any{"node": n, "cursor": issueCursor(i)}})
			}
			return edges, nil
		}
		return nil, gqlError{"Cannot query field \"" + name + "\" on type \"" + parent.kind + "\""}
	}
	if parent.kind == "Edge" {
		edge := parent.value.(map[string]any)
		if name == "node" {
			return edge["node"], nil
		}
		if name == "cursor" {
			return edge["cursor"], nil
		}
		return nil, gqlError{"Cannot query field \"" + name + "\" on type \"Edge\""}
	}
	if parent.kind == "PageInfo" {
		info := parent.value.(map[string]any)
		if v, ok := info[name]; ok {
			return v, nil
		}
		return nil, gqlError{"Cannot query field \"" + name + "\" on type \"PageInfo\""}
	}
	if parent.kind == "MutationPayload" {
		payload := parent.value.(map[string]any)
		if v, ok := payload[name]; ok {
			return v, nil
		}
		return nil, gqlError{"Cannot query field \"" + name + "\" on type \"MutationPayload\""}
	}
	if parent.kind == "CommentEdge" {
		edge := parent.value.(map[string]any)
		if name == "node" {
			return &entity{kind: "Comment", value: edge["node"]}, nil
		}
		return nil, gqlError{"Cannot query field \"" + name + "\" on type \"CommentEdge\""}
	}
	if parent.kind == "Comment" {
		comment := parent.value.(map[string]any)
		if v, ok := comment[name]; ok {
			return v, nil
		}
		return nil, gqlError{"Cannot query field \"" + name + "\" on type \"Comment\""}
	}
	return nil, gqlError{"Cannot query field \"" + name + "\" on type \"" + parent.kind + "\""}
}

type gqlErrItem struct {
	Message string `json:"message"`
	Path    []string `json:"path,omitempty"`
}

func executeSelections(parent *entity, selections []*field, path []string, errs *[]gqlErrItem, vars map[string]any) map[string]any {
	out := make(map[string]any, len(selections))
	for _, sel := range selections {
		key := sel.name
		if sel.alias != "" {
			key = sel.alias
		}
		if sel.name == "__skip" {
			continue
		}
		if sel.name == "__inline" {
			for k, v := range executeSelections(parent, sel.selections, path, errs, vars) {
				out[k] = v
			}
			continue
		}
		if sel.name == "__typename" {
			out[key] = parent.kind
			continue
		}
		boundArgs, _ := bind(sel.args, vars).(map[string]any)
		if boundArgs == nil {
			boundArgs = map[string]any{}
		}
		value, err := resolveField(parent, sel.name, boundArgs)
		if err != nil {
			*errs = append(*errs, gqlErrItem{Message: err.Error(), Path: append(append([]string{}, path...), key)})
			continue
		}
		out[key] = project(value, sel.selections, append(append([]string{}, path...), key), errs, vars)
	}
	return out
}

func project(value any, selections []*field, path []string, errs *[]gqlErrItem, vars map[string]any) any {
	if value == nil {
		return nil
	}
	switch v := value.(type) {
	case []*entity:
		out := make([]any, 0, len(v))
		for _, e := range v {
			out = append(out, project(e, selections, path, errs, vars))
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for _, e := range v {
			out = append(out, project(e, selections, path, errs, vars))
		}
		return out
	case []string:
		out := make([]any, 0, len(v))
		for _, e := range v {
			out = append(out, e)
		}
		return out
	case *entity:
		if len(selections) == 0 {
			return nil
		}
		return executeSelections(v, selections, path, errs, vars)
	case map[string]any:
		if len(selections) == 0 {
			return v
		}
		out := make(map[string]any, len(selections))
		for _, sel := range selections {
			key := sel.name
			if sel.alias != "" {
				key = sel.alias
			}
			child, ok := v[sel.name]
			if !ok {
				*errs = append(*errs, gqlErrItem{Message: "Cannot query field \"" + sel.name + "\"", Path: append(append([]string{}, path...), key)})
				continue
			}
			out[key] = project(child, sel.selections, append(append([]string{}, path...), key), errs, vars)
		}
		return out
	}
	return value
}

func runOperation(store *Store, op *operation, vars map[string]any) map[string]any {
	bound := make(map[string]any, len(op.variables)+len(vars))
	for k, v := range op.variables {
		bound[k] = v
	}
	for k, v := range vars {
		bound[k] = v
	}
	errs := []gqlErrItem{}
	data := make(map[string]any, len(op.selections))
	for _, sel := range op.selections {
		key := sel.name
		if sel.alias != "" {
			key = sel.alias
		}
		boundArgs, _ := bind(sel.args, bound).(map[string]any)
		if boundArgs == nil {
			boundArgs = map[string]any{}
		}
		if op.kind == "mutation" {
			payload, err := resolveMutation(store, sel.name, boundArgs)
			if err != nil {
				errs = append(errs, gqlErrItem{Message: err.Error(), Path: []string{key}})
				data[key] = nil
				continue
			}
			data[key] = project(&entity{kind: "MutationPayload", value: payload}, sel.selections, []string{key}, &errs, bound)
			continue
		}
		value, err := resolveRoot(store, sel.name, boundArgs)
		if err != nil {
			errs = append(errs, gqlErrItem{Message: err.Error(), Path: []string{key}})
			data[key] = nil
			continue
		}
		data[key] = project(value, sel.selections, []string{key}, &errs, bound)
	}
	if len(errs) > 0 {
		return map[string]any{"data": data, "errors": errs}
	}
	return map[string]any{"data": data}
}

func handleGraphQL(w http.ResponseWriter, r *http.Request, store *Store, body []byte) {
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		sendJSON(w, 400, map[string]any{"errors": []any{map[string]any{"message": "Problems parsing JSON"}}})
		return
	}
	runOne := func(request any) map[string]any {
		record, ok := request.(map[string]any)
		if !ok {
			return map[string]any{"data": nil, "errors": []any{map[string]any{"message": "Invalid request"}}}
		}
		query, ok := record["query"].(string)
		if !ok {
			return map[string]any{"data": nil, "errors": []any{map[string]any{"message": "No query"}}}
		}
		vars, _ := record["variables"].(map[string]any)
		if vars == nil {
			vars = map[string]any{}
		}
		ops, err := parseDocument(lex(query))
		if err != nil || len(ops) == 0 {
			message := "the document could not be parsed"
			if err != nil {
				message = err.Error()
			}
			return map[string]any{"data": nil, "errors": []any{map[string]any{"message": message}}}
		}
		return runOperation(store, ops[0], vars)
	}
	if list, ok := parsed.([]any); ok {
		out := make([]any, 0, len(list))
		for _, entry := range list {
			out = append(out, runOne(entry))
		}
		sendJSON(w, 200, out)
		return
	}
	sendJSON(w, 200, runOne(parsed))
}

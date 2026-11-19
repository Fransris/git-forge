package main

// MCP over Streamable HTTP: JSON-RPC 2.0, both the 2026-07-28 revision (server/discover) and the
// handshake era (initialize, notifications/initialized, Mcp-Session-Id).

import (
	"encoding/json"
	"net/http"
	"strings"
)

var supportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26"}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func strProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

var mcpTools = []mcpTool{
	{
		Name:        "create_repo",
		Description: "Create a repository on this forge. Returns the repository object REST reads at /api/v3/repos/{owner}/{repo}.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"name": strProp("Repository name"), "description": strProp("Description"), "private": map[string]any{"type": "boolean", "description": "Whether the repository is private"}},
			"required":   []string{"name"},
		},
	},
	{
		Name:        "get_repo",
		Description: "Read one repository by owner and name.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"owner": strProp("Repository owner login"), "repo": strProp("Repository name"), "full_name": strProp("owner/repo")},
			"required":   []string{"owner", "repo"},
		},
	},
	{
		Name:        "create_issue",
		Description: "Open an issue in a repository. Returns the issue object REST reads at /api/v3/repos/{owner}/{repo}/issues/{number}.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"owner": strProp("Repository owner"), "repo": strProp("Repository name"), "title": strProp("Issue title"), "body": strProp("Issue body"), "full_name": strProp("owner/repo")},
			"required":   []string{"owner", "repo", "title"},
		},
	},
	{
		Name:        "list_issues",
		Description: "List a repository's issues, open by default, newest first.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"owner": strProp("Repository owner"), "repo": strProp("Repository name"), "state": strProp("open, closed or all"), "full_name": strProp("owner/repo")},
			"required":   []string{"owner", "repo"},
		},
	},
	{
		Name:        "create_pull_request",
		Description: "Open a pull request from a head branch that exists in the repository.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"owner": strProp("Repository owner"), "repo": strProp("Repository name"), "title": strProp("Pull request title"), "head": strProp("Head branch"), "base": strProp("Base branch"), "body": strProp("Body"), "full_name": strProp("owner/repo")},
			"required":   []string{"owner", "repo", "title", "head", "base"},
		},
	},
	{
		Name:        "merge_pull_request",
		Description: "Squash-merge an open pull request into its base branch.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"owner": strProp("Repository owner"), "repo": strProp("Repository name"), "pull_number": map[string]any{"type": "number", "description": "Pull request number"}, "full_name": strProp("owner/repo")},
			"required":   []string{"owner", "repo", "pull_number"},
		},
	},
	{
		Name:        "create_comment",
		Description: "Comment on an issue and return the comment object.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"owner": strProp("Repository owner"), "repo": strProp("Repository name"), "issue_number": map[string]any{"type": "number", "description": "Issue number"}, "body": strProp("Comment body"), "full_name": strProp("owner/repo")},
			"required":   []string{"owner", "repo", "issue_number", "body"},
		},
	},
}

func textResult(payload any) map[string]any {
	data, _ := json.Marshal(payload)
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(data)}},
		"structuredContent": payload,
		"isError":           false,
	}
}

func errorResult(message string) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": message}}, "isError": true}
}

func mcpRepoOf(store *Store, args map[string]any) *Repo {
	full := asString(args["full_name"])
	owner := asString(args["owner"])
	repoName := asString(args["repo"])
	if owner == "" || repoName == "" {
		parts := strings.SplitN(full, "/", 2)
		if owner == "" && len(parts) > 0 {
			owner = parts[0]
		}
		if repoName == "" && len(parts) > 1 {
			repoName = parts[1]
		}
	}
	if owner == "" || repoName == "" {
		return nil
	}
	store.LearnOwner(owner)
	return store.Find(owner, repoName)
}

func issuePayload(repo *Repo, issue *Issue) map[string]any {
	labels := make([]string, 0, len(issue.Labels))
	for _, l := range issue.Labels {
		labels = append(labels, l.Name)
	}
	return map[string]any{
		"id": issue.ID, "number": issue.Number, "title": issue.Title, "body": issue.Body,
		"state": issue.State, "labels": labels, "created_at": issue.CreatedAt,
		"updated_at": issue.UpdatedAt, "repository": repo.Owner + "/" + repo.Name,
	}
}

func pullPayload(repo *Repo, pull *Pull) map[string]any {
	var mergeSHA any
	if pull.MergeCommitSHA != nil {
		mergeSHA = *pull.MergeCommitSHA
	}
	return map[string]any{
		"id": pull.ID, "number": pull.Number, "title": pull.Title, "body": pull.Body,
		"state": pull.State, "merged": pull.Merged, "merge_commit_sha": mergeSHA,
		"head": pull.Head, "base": pull.Base, "repository": repo.Owner + "/" + repo.Name,
	}
}

func mcpMerge(store *Store, repo *Repo, pull *Pull) (string, bool) {
	if pull.Merged {
		return "already merged", false
	}
	if !pullMergeable(repo, pull) {
		return "the pull request is not mergeable", false
	}
	baseTip := repo.Refs["refs/heads/"+pull.Base.Ref]
	headObj := repo.ReadObject(repo.Refs["refs/heads/"+pull.Head.Ref])
	if baseTip == "" || headObj == nil {
		return "the branch is gone", false
	}
	tree := ParseCommit(headObj.Data).Tree
	now := store.Clock.Tick()
	message := "Merge pull request #" + itoa(pull.Number) + " from " + pull.Head.Ref + "\n\n" + pull.Title
	sha := repo.PutObject("commit", BuildCommit(tree, []string{baseTip}, message, repo.Owner, repo.Owner+"@example.invalid", now))
	repo.Refs["refs/heads/"+pull.Base.Ref] = sha
	repo.clearCaches()
	pull.Merged = true
	pull.State = "closed"
	pull.MergedAt = &now
	pull.ClosedAt = &now
	pull.MergeCommitSHA = &sha
	pull.UpdatedAt = now
	return sha, true
}

func callTool(store *Store, name string, args map[string]any) map[string]any {
	bare := name
	if idx := strings.LastIndex(bare, "__"); idx >= 0 {
		bare = bare[idx+2:]
	}
	switch strings.ToLower(bare) {
	case "create_repo":
		repoName := asString(args["name"])
		if repoName == "" {
			return errorResult("name is required")
		}
		repo, created := store.CreateRepo(repoName, asString(args["description"]), args["private"] == true)
		if !created {
			return errorResult("a repository with that name already exists")
		}
		return textResult(map[string]any{"id": repo.ID, "name": repo.Name, "owner": repo.Owner, "full_name": repo.Owner + "/" + repo.Name, "private": repo.Private, "default_branch": repo.DefaultBranch, "description": repo.Description})
	case "get_repo":
		repo := mcpRepoOf(store, args)
		if repo == nil {
			return errorResult("repository not found")
		}
		open := 0
		for _, i := range repo.Issues {
			if i.State == "open" {
				open++
			}
		}
		return textResult(map[string]any{"id": repo.ID, "name": repo.Name, "owner": repo.Owner, "full_name": repo.Owner + "/" + repo.Name, "private": repo.Private, "default_branch": repo.DefaultBranch, "description": repo.Description, "open_issues": open})
	case "create_issue":
		repo := mcpRepoOf(store, args)
		if repo == nil {
			return errorResult("repository not found")
		}
		title := asString(args["title"])
		if title == "" {
			return errorResult("title is required")
		}
		now := store.Clock.Tick()
		issue := &Issue{ID: store.NewIssueID(), Number: repo.NextIssue, Title: title, Body: asString(args["body"]), State: "open", CreatedAt: now, UpdatedAt: now}
		repo.NextIssue++
		repo.Issues = append(repo.Issues, issue)
		return textResult(issuePayload(repo, issue))
	case "list_issues":
		repo := mcpRepoOf(store, args)
		if repo == nil {
			return errorResult("repository not found")
		}
		state := asString(args["state"])
		if state == "" {
			state = "open"
		}
		var list []*Issue
		for _, i := range repo.Issues {
			if state == "all" || i.State == state {
				list = append(list, i)
			}
		}
		for a := 1; a < len(list); a++ {
			for b := a; b > 0 && list[b].Number > list[b-1].Number; b-- {
				list[b], list[b-1] = list[b-1], list[b]
			}
		}
		issues := make([]map[string]any, 0, len(list))
		for _, i := range list {
			issues = append(issues, issuePayload(repo, i))
		}
		return textResult(map[string]any{"total": len(issues), "issues": issues})
	case "create_pull_request":
		repo := mcpRepoOf(store, args)
		if repo == nil {
			return errorResult("repository not found")
		}
		headRef := asString(args["head"])
		baseRef := asString(args["base"])
		if baseRef == "" {
			baseRef = repo.DefaultBranch
		}
		headTip := repo.Refs["refs/heads/"+headRef]
		baseTip := repo.Refs["refs/heads/"+baseRef]
		if headTip == "" || baseTip == "" {
			return errorResult("the head or base branch does not exist")
		}
		now := store.Clock.Tick()
		pull := &Pull{ID: store.NewPullID(), Number: repo.NextPull, Title: asString(args["title"]), Body: asString(args["body"]), State: "open", Head: Side{Ref: headRef, SHA: headTip}, Base: Side{Ref: baseRef, SHA: baseTip}, CreatedAt: now, UpdatedAt: now, Mergeable: true}
		repo.NextPull++
		repo.Pulls = append(repo.Pulls, pull)
		return textResult(pullPayload(repo, pull))
	case "merge_pull_request":
		repo := mcpRepoOf(store, args)
		if repo == nil {
			return errorResult("repository not found")
		}
		number, _ := asInt(args["pull_number"])
		var pull *Pull
		for _, p := range repo.Pulls {
			if p.Number == number {
				pull = p
				break
			}
		}
		if pull == nil {
			return errorResult("pull request not found")
		}
		sha, ok := mcpMerge(store, repo, pull)
		if !ok {
			return errorResult(sha)
		}
		payload := pullPayload(repo, pull)
		payload["merged"] = true
		payload["sha"] = sha
		payload["message"] = "Pull Request successfully merged"
		return textResult(payload)
	case "create_comment":
		repo := mcpRepoOf(store, args)
		if repo == nil {
			return errorResult("repository not found")
		}
		number, _ := asInt(args["issue_number"])
		var issue *Issue
		for _, i := range repo.Issues {
			if i.Number == number {
				issue = i
				break
			}
		}
		if issue == nil {
			return errorResult("issue not found")
		}
		body := asString(args["body"])
		if body == "" {
			return errorResult("body is required")
		}
		now := store.Clock.Tick()
		comment := &Comment{ID: store.NewCommentID(), Body: body, CreatedAt: now, UpdatedAt: now}
		issue.Comments = append(issue.Comments, comment)
		issue.UpdatedAt = now
		delete(repo.IssueJSON, issue.ID)
		return textResult(map[string]any{"id": comment.ID, "body": comment.Body, "issue_number": issue.Number, "created_at": now})
	}
	return errorResult("unknown tool: " + name)
}

func rpcResult(id any, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func rpcError(id any, code int, message string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}
}

func handleMCPMessage(store *Store, message map[string]any) map[string]any {
	id := message["id"]
	method, _ := message["method"].(string)
	params, _ := message["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	if strings.HasPrefix(method, "notifications/") {
		return nil
	}
	switch method {
	case "server/discover":
		return rpcResult(id, map[string]any{
			"resultType":        "discover",
			"supportedVersions": supportedVersions,
			"capabilities":      map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":        map[string]any{"name": "makefun-git-forge", "version": "1.0.0"},
			"instructions":      "A GitHub-compatible forge: create repositories, issues and pull requests, and merge them.",
		})
	case "initialize":
		offered := asString(params["protocolVersion"])
		chosen := supportedVersions[len(supportedVersions)-1]
		for _, v := range supportedVersions {
			if v == offered {
				chosen = offered
				break
			}
		}
		return rpcResult(id, map[string]any{
			"protocolVersion": chosen,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "makefun-git-forge", "version": "1.0.0"},
		})
	case "ping":
		return rpcResult(id, map[string]any{})
	case "tools/list":
		return rpcResult(id, map[string]any{"tools": mcpTools})
	case "tools/call":
		name := asString(params["name"])
		args, _ := params["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		if name == "" {
			return rpcError(id, -32602, "Invalid params: name is required")
		}
		var known *mcpTool
		for i := range mcpTools {
			if mcpTools[i].Name == name || strings.HasSuffix(strings.ToLower(name), "__"+mcpTools[i].Name) {
				known = &mcpTools[i]
				break
			}
		}
		if known == nil {
			return rpcError(id, -32601, "Method not found: "+name)
		}
		required, _ := known.InputSchema["required"].([]string)
		var missing []string
		for _, key := range required {
			if v, ok := args[key]; !ok || v == nil || v == "" {
				missing = append(missing, key)
			}
		}
		if len(missing) > 0 {
			return rpcResult(id, errorResult("missing required argument: "+strings.Join(missing, ", ")))
		}
		return rpcResult(id, callTool(store, name, args))
	}
	if id == nil {
		return nil
	}
	return rpcError(id, -32601, "Method not found: "+method)
}

func handleMCP(w http.ResponseWriter, r *http.Request, store *Store, body []byte) {
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		sendJSON(w, 400, map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "Parse error"}})
		return
	}
	if list, ok := parsed.([]any); ok {
		answers := make([]any, 0, len(list))
		for _, entry := range list {
			message, ok := entry.(map[string]any)
			if !ok {
				answers = append(answers, rpcError(nil, -32600, "Invalid Request"))
				continue
			}
			if answer := handleMCPMessage(store, message); answer != nil {
				answers = append(answers, answer)
			}
		}
		if len(answers) == 0 {
			w.WriteHeader(202)
			return
		}
		sendJSON(w, 200, answers)
		return
	}
	message, ok := parsed.(map[string]any)
	if !ok {
		sendJSON(w, 400, map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32600, "message": "Invalid Request"}})
		return
	}
	answer := handleMCPMessage(store, message)
	if answer == nil {
		w.WriteHeader(202)
		return
	}
	if message["method"] == "initialize" {
		w.Header().Set("Mcp-Session-Id", "forge-session")
	}
	sendJSON(w, 200, answer)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

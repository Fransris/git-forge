package main

// GitHub REST v3 over the store.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

var requestCounter int64

func sendJSON(w http.ResponseWriter, status int, body any) {
	requestCounter++
	data, err := json.Marshal(body)
	if err != nil {
		data = []byte(`{"message":"encoding error"}`)
	}
	data = append(data, '\n')
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	h.Set("X-GitHub-Request-Id", "FORGE-"+strconv.FormatInt(requestCounter, 10))
	w.WriteHeader(status)
	w.Write(data)
}

func sendJSONBytes(w http.ResponseWriter, status int, data []byte) {
	requestCounter++
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	h.Set("X-GitHub-Request-Id", "FORGE-"+strconv.FormatInt(requestCounter, 10))
	w.WriteHeader(status)
	w.Write(data)
}

func notFound(w http.ResponseWriter) { sendJSON(w, 404, map[string]any{"message": "Not Found"}) }

func failWith(w http.ResponseWriter, status int, message string) {
	sendJSON(w, status, map[string]any{"message": message})
}

func userShape(login string) map[string]any {
	return map[string]any{
		"login": login, "id": 1, "node_id": "U_" + login, "avatar_url": "", "gravatar_id": "",
		"url": "/api/v3/users/" + login, "html_url": "", "type": "User", "site_admin": false,
	}
}

func labelShape(l *Label) map[string]any {
	return map[string]any{"id": l.ID, "node_id": fmt.Sprintf("LA_%d", l.ID), "name": l.Name, "color": l.Color, "default": false, "description": l.Description}
}

func issueShape(repo *Repo, issue *Issue) map[string]any {
	labels := make([]map[string]any, 0, len(issue.Labels))
	for _, l := range issue.Labels {
		labels = append(labels, labelShape(l))
	}
	var closed any
	if issue.ClosedAt != nil {
		closed = *issue.ClosedAt
	}
	return map[string]any{
		"id": issue.ID, "node_id": fmt.Sprintf("I_%d", issue.ID), "number": issue.Number,
		"title": issue.Title, "body": issue.Body, "state": issue.State, "locked": false,
		"user": userShape(repo.Owner), "labels": labels, "assignee": nil, "assignees": []any{},
		"milestone": nil, "comments": len(issue.Comments), "created_at": issue.CreatedAt,
		"updated_at": issue.UpdatedAt, "closed_at": closed, "author_association": "OWNER",
		"html_url": "", "url": fmt.Sprintf("/api/v3/repos/%s/%s/issues/%d", repo.Owner, repo.Name, issue.Number),
		"repository_url": fmt.Sprintf("/api/v3/repos/%s/%s", repo.Owner, repo.Name),
	}
}

func repoShape(repo *Repo) map[string]any {
	open := 0
	for _, i := range repo.Issues {
		if i.State == "open" {
			open++
		}
	}
	visibility := "public"
	if repo.Private {
		visibility = "private"
	}
	return map[string]any{
		"id": repo.ID, "node_id": fmt.Sprintf("R_%d", repo.ID), "name": repo.Name,
		"full_name": repo.Owner + "/" + repo.Name, "private": repo.Private, "owner": userShape(repo.Owner),
		"description": repo.Description, "fork": false, "default_branch": repo.DefaultBranch,
		"created_at": repo.CreatedAt, "updated_at": repo.UpdatedAt, "pushed_at": repo.PushedAt,
		"clone_url": fmt.Sprintf("/%s/%s.git", repo.Owner, repo.Name), "html_url": "",
		"url": fmt.Sprintf("/api/v3/repos/%s/%s", repo.Owner, repo.Name),
		"has_issues": true, "open_issues_count": open, "visibility": visibility,
		"permissions": map[string]any{"admin": true, "push": true, "pull": true},
	}
}

func sideShape(repo *Repo, side Side) map[string]any {
	return map[string]any{"label": repo.Owner + ":" + side.Ref, "ref": side.Ref, "sha": side.SHA, "user": userShape(repo.Owner), "repo": repoShape(repo)}
}

func pullShape(repo *Repo, pull *Pull) map[string]any {
	additions, deletions := 0, 0
	for _, f := range pull.Files {
		additions += f.Additions
		deletions += f.Deletions
	}
	mergeableState := "dirty"
	if pull.Mergeable {
		mergeableState = "clean"
	}
	var mergedAt, closedAt, mergeSHA any
	if pull.MergedAt != nil {
		mergedAt = *pull.MergedAt
	}
	if pull.ClosedAt != nil {
		closedAt = *pull.ClosedAt
	}
	if pull.MergeCommitSHA != nil {
		mergeSHA = *pull.MergeCommitSHA
	}
	return map[string]any{
		"id": pull.ID, "node_id": fmt.Sprintf("PR_%d", pull.ID), "number": pull.Number,
		"title": pull.Title, "body": pull.Body, "state": pull.State, "locked": false,
		"user": userShape(repo.Owner), "head": sideShape(repo, pull.Head), "base": sideShape(repo, pull.Base),
		"merged": pull.Merged, "merged_at": mergedAt, "merge_commit_sha": mergeSHA,
		"mergeable": pull.Mergeable, "mergeable_state": mergeableState, "draft": false,
		"created_at": pull.CreatedAt, "updated_at": pull.UpdatedAt, "closed_at": closedAt,
		"commits": 1, "additions": additions, "deletions": deletions, "changed_files": len(pull.Files),
		"html_url": "", "url": fmt.Sprintf("/api/v3/repos/%s/%s/pulls/%d", repo.Owner, repo.Name, pull.Number),
	}
}

// paginate returns the page of items and writes the Link header.
func paginate[T any](w http.ResponseWriter, r *http.Request, items []T, path string) []T {
	perPage := 30
	page := 1
	q := r.URL.Query()
	if v, err := strconv.Atoi(q.Get("per_page")); err == nil && v > 0 {
		perPage = v
		if perPage > 100 {
			perPage = 100
		}
	}
	if v, err := strconv.Atoi(q.Get("page")); err == nil && v > 0 {
		page = v
	}
	start := (page - 1) * perPage
	if start > len(items) {
		start = len(items)
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	var links []string
	origin := "http://" + r.Host
	makeURL := func(target int) string {
		params := q
		params.Set("per_page", strconv.Itoa(perPage))
		params.Set("page", strconv.Itoa(target))
		return origin + "/api/v3" + path + "?" + params.Encode()
	}
	if end < len(items) {
		links = append(links, "<"+makeURL(page+1)+`>; rel="next"`)
	}
	if page > 1 {
		links = append(links, "<"+makeURL(page-1)+`>; rel="prev"`)
	}
	links = append(links, "<"+makeURL(1)+`>; rel="first"`)
	w.Header().Set("Link", strings.Join(links, ", "))
	return items[start:end]
}

func readJSONBody(r *http.Request) (map[string]any, bool) {
	var body map[string]any
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&body); err != nil {
		return nil, false
	}
	if body == nil {
		body = map[string]any{}
	}
	return body, true
}

func str(body map[string]any, key string) string {
	if v, ok := body[key].(string); ok {
		return v
	}
	return ""
}

func num(body map[string]any, key string) (int, bool) {
	switch v := body[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	}
	return 0, false
}

func handleREST(w http.ResponseWriter, r *http.Request, store *Store, path string) bool {
	segments := splitPath(path)
	method := r.Method

	if path == "" || path == "/" {
		sendJSON(w, 200, map[string]any{
			"current_user_url": "/api/v3/user", "repository_url": "/api/v3/repos/{owner}/{repo}",
			"user_repositories_url": "/api/v3/user/repos", "rate_limit_url": "/api/v3/rate_limit",
			"graphql_url": "/api/graphql",
		})
		w.Header().Set("X-OAuth-Scopes", "repo, read:org")
		return true
	}
	if path == "/meta" {
		sendJSON(w, 200, map[string]any{"installed_version": "3.0.0-forge", "verifiable_password_authentication": true, "hooks": []any{}, "git": []any{}, "ssh_key_fingerprints": map[string]any{}, "ssh_keys": []any{}, "web": []any{}, "api": []any{}, "packages": []any{}})
		return true
	}
	if path == "/rate_limit" {
		sendJSON(w, 200, map[string]any{"resources": map[string]any{}, "rate": map[string]any{"limit": 5000, "remaining": 4999, "reset": 0, "used": 1}})
		return true
	}
	if path == "/user" {
		login := store.Login()
		shape := userShape(login)
		shape["name"] = login
		shape["email"] = login + "@example.invalid"
		shape["public_repos"] = len(store.ReposOf(login))
		sendJSON(w, 200, shape)
		return true
	}
	if path == "/user/repos" {
		if method == "GET" {
			repos := store.ReposOf(store.Login())
			page := paginate(w, r, repos, "/user/repos")
			out := make([]map[string]any, 0, len(page))
			for _, repo := range page {
				out = append(out, repoShape(repo))
			}
			sendJSON(w, 200, out)
			return true
		}
		if method == "POST" {
			body, ok := readJSONBody(r)
			if !ok {
				failWith(w, 400, "Problems parsing JSON")
				return true
			}
			name := str(body, "name")
			if name == "" {
				sendJSON(w, 422, map[string]any{"message": "Repository creation failed.", "errors": []any{map[string]any{"resource": "Repository", "field": "name", "code": "missing_field"}}})
				return true
			}
			repo, created := store.CreateRepo(name, str(body, "description"), body["private"] == true)
			if !created {
				sendJSON(w, 422, map[string]any{"message": "Repository creation failed.", "errors": []any{map[string]any{"resource": "Repository", "field": "name", "code": "custom", "message": "name already exists on this account"}}})
				return true
			}
			sendJSON(w, 201, repoShape(repo))
			return true
		}
	}
	if len(segments) == 2 && segments[0] == "users" && method == "GET" {
		shape := userShape(segments[1])
		shape["name"] = segments[1]
		shape["public_repos"] = 0
		sendJSON(w, 200, shape)
		return true
	}
	if len(segments) < 3 || segments[0] != "repos" {
		return false
	}
	owner, name := segments[1], segments[2]
	store.LearnOwner(owner)
	repo := store.Find(owner, name)
	if repo == nil {
		notFound(w)
		return true
	}
	rest := segments[3:]

	if len(rest) == 0 {
		switch method {
		case "GET":
			sendJSON(w, 200, repoShape(repo))
			return true
		case "PATCH":
			body, ok := readJSONBody(r)
			if !ok {
				failWith(w, 400, "Problems parsing JSON")
				return true
			}
			if v, ok := body["description"].(string); ok {
				repo.Description = v
			}
			if v, ok := body["name"].(string); ok && v != "" && v != repo.Name {
				store.mu.Lock()
				delete(store.Repos, repoKey(repo.Owner, repo.Name))
				repo.Name = v
				store.Repos[repoKey(repo.Owner, repo.Name)] = repo
				store.mu.Unlock()
			}
			if v, ok := body["private"].(bool); ok {
				repo.Private = v
			}
			if v, ok := body["default_branch"].(string); ok {
				repo.DefaultBranch = v
			}
			repo.UpdatedAt = store.Clock.Tick()
			sendJSON(w, 200, repoShape(repo))
			return true
		case "DELETE":
			store.DeleteRepo(repo.Owner, repo.Name)
			requestCounter++
			w.Header().Set("X-GitHub-Request-Id", "FORGE-"+strconv.FormatInt(requestCounter, 10))
			w.WriteHeader(204)
			return true
		}
	}

	if len(rest) == 1 && rest[0] == "branches" && method == "GET" {
		names := refList(repo)
		branches := make([]map[string]any, 0, len(names))
		for _, ref := range names {
			if strings.HasPrefix(ref, "refs/heads/") {
				branches = append(branches, map[string]any{"name": ref[11:], "commit": map[string]any{"sha": repo.Refs[ref], "url": fmt.Sprintf("/api/v3/repos/%s/%s/commits/%s", repo.Owner, repo.Name, repo.Refs[ref])}, "protected": false})
			}
		}
		page := paginate(w, r, branches, "/repos/"+repo.Owner+"/"+repo.Name+"/branches")
		sendJSON(w, 200, page)
		return true
	}

	if len(rest) == 2 && rest[0] == "git" && rest[1] == "refs" && method == "POST" {
		body, ok := readJSONBody(r)
		if !ok {
			failWith(w, 400, "Problems parsing JSON")
			return true
		}
		ref := str(body, "ref")
		sha := str(body, "sha")
		if ref == "" || sha == "" || repo.ReadObject(sha) == nil {
			sendJSON(w, 422, map[string]any{"message": "Reference update failed", "errors": []any{map[string]any{"resource": "GitRef", "code": "custom"}}})
			return true
		}
		full := ref
		if !strings.HasPrefix(ref, "refs/") {
			full = "refs/heads/" + ref
		}
		repo.Refs[full] = sha
		repo.clearCaches()
		sendJSON(w, 201, map[string]any{"ref": full, "node_id": "REF_" + full, "object": map[string]any{"sha": sha, "type": "commit", "url": ""}})
		return true
	}

	if len(rest) == 1 && rest[0] == "commits" && method == "GET" {
		branch := r.URL.Query().Get("sha")
		if branch == "" {
			branch = r.URL.Query().Get("branch")
		}
		if branch == "" {
			branch = repo.DefaultBranch
		}
		tip := ""
		if strings.HasPrefix(branch, "refs/") {
			tip = repo.Refs[branch]
		} else {
			tip = repo.Refs["refs/heads/"+branch]
		}
		var commits []map[string]any
		if tip != "" {
			seen := map[string]struct{}{}
			queue := []string{tip}
			for len(queue) > 0 {
				id := queue[0]
				queue = queue[1:]
				if _, ok := seen[id]; ok {
					continue
				}
				seen[id] = struct{}{}
				obj := repo.ReadObject(id)
				if obj == nil || obj.Type != "commit" {
					continue
				}
				info := ParseCommit(obj.Data)
				parents := make([]map[string]any, 0, len(info.Parents))
				for _, p := range info.Parents {
					parents = append(parents, map[string]any{"sha": p})
				}
				commits = append(commits, map[string]any{
					"sha": id, "node_id": "C_" + id,
					"commit": map[string]any{
						"message":   strings.TrimRight(info.Message, "\n"),
						"author":    map[string]any{"name": info.AuthorName, "email": info.AuthorEmail, "date": info.Time},
						"committer": map[string]any{"name": info.AuthorName, "email": info.AuthorEmail, "date": info.Time},
						"tree":      map[string]any{"sha": info.Tree},
					},
					"parents": parents,
					"url":     fmt.Sprintf("/api/v3/repos/%s/%s/commits/%s", repo.Owner, repo.Name, id),
				})
				queue = append(queue, info.Parents...)
			}
		}
		page := paginate(w, r, commits, "/repos/"+repo.Owner+"/"+repo.Name+"/commits")
		sendJSON(w, 200, page)
		return true
	}

	if len(rest) >= 1 && rest[0] == "contents" {
		return handleContents(w, r, store, repo, strings.Join(rest[1:], "/"))
	}

	if rest[0] == "labels" {
		if method == "GET" && len(rest) == 1 {
			out := make([]map[string]any, 0, len(repo.Labels))
			for _, l := range repo.Labels {
				out = append(out, labelShape(l))
			}
			sendJSON(w, 200, out)
			return true
		}
		if method == "POST" && len(rest) == 1 {
			body, ok := readJSONBody(r)
			if !ok {
				failWith(w, 400, "Problems parsing JSON")
				return true
			}
			name := str(body, "name")
			if name == "" {
				sendJSON(w, 422, map[string]any{"message": "Validation Failed"})
				return true
			}
			for _, l := range repo.Labels {
				if l.Name == name {
					sendJSON(w, 201, labelShape(l))
					return true
				}
			}
			color := str(body, "color")
			if color == "" {
				color = "ededed"
			}
			label := &Label{ID: store.NewLabelID(), Name: name, Color: strings.TrimPrefix(color, "#"), Description: str(body, "description")}
			repo.Labels = append(repo.Labels, label)
			sendJSON(w, 201, labelShape(label))
			return true
		}
	}

	if rest[0] == "issues" {
		return handleIssues(w, r, store, repo, rest[1:])
	}
	if rest[0] == "pulls" {
		return handlePulls(w, r, store, repo, rest[1:])
	}
	return false
}

func handleContents(w http.ResponseWriter, r *http.Request, store *Store, repo *Repo, filePath string) bool {
	clean := strings.Trim(filePath, "/")
	refParam := r.URL.Query().Get("ref")
	branchForRead := repo.DefaultBranch
	if refParam != "" {
		branchForRead = strings.TrimPrefix(refParam, "refs/heads/")
	}
	tip := repo.Refs["refs/heads/"+branchForRead]
	files := map[string]TreeEntry{}
	if tip != "" {
		if obj := repo.ReadObject(tip); obj != nil && obj.Type == "commit" {
			repo.FlattenTree(ParseCommit(obj.Data).Tree, "", files)
		}
	}

	if r.Method == "GET" {
		if clean == "" {
			entries := make([]map[string]any, 0, len(files))
			dirs := map[string]struct{}{}
			for path, e := range files {
				if idx := strings.IndexByte(path, '/'); idx >= 0 {
					dirs[path[:idx]] = struct{}{}
					continue
				}
				size := 0
				if obj := repo.ReadObject(e.SHA); obj != nil {
					size = len(obj.Data)
				}
				entries = append(entries, map[string]any{"type": "file", "name": path, "path": path, "sha": e.SHA, "size": size, "encoding": "base64", "content": ""})
			}
			for d := range dirs {
				entries = append(entries, map[string]any{"type": "dir", "name": d, "path": d, "sha": "", "size": 0, "encoding": "base64", "content": ""})
			}
			sort.Slice(entries, func(i, j int) bool { return entries[i]["name"].(string) < entries[j]["name"].(string) })
			sendJSON(w, 200, entries)
			return true
		}
		if entry, ok := files[clean]; ok {
			if obj := repo.ReadObject(entry.SHA); obj != nil {
				sendJSON(w, 200, map[string]any{
					"type": "file", "encoding": "base64", "size": len(obj.Data),
					"name": clean[strings.LastIndexByte(clean, '/')+1:], "path": clean,
					"content": base64.StdEncoding.EncodeToString(obj.Data), "sha": entry.SHA,
					"url": fmt.Sprintf("/api/v3/repos/%s/%s/contents/%s", repo.Owner, repo.Name, clean),
				})
				return true
			}
		}
		prefix := clean + "/"
		children := map[string]map[string]any{}
		for path, e := range files {
			if !strings.HasPrefix(path, prefix) {
				continue
			}
			tail := path[len(prefix):]
			if idx := strings.IndexByte(tail, '/'); idx >= 0 {
				children[tail[:idx]] = map[string]any{"type": "dir", "name": tail[:idx], "path": clean + "/" + tail[:idx], "sha": "", "size": 0, "encoding": "base64", "content": ""}
				continue
			}
			size := 0
			if obj := repo.ReadObject(e.SHA); obj != nil {
				size = len(obj.Data)
			}
			children[tail] = map[string]any{"type": "file", "name": tail, "path": path, "sha": e.SHA, "size": size, "encoding": "base64", "content": ""}
		}
		if len(children) > 0 {
			out := make([]map[string]any, 0, len(children))
			for _, v := range children {
				out = append(out, v)
			}
			sort.Slice(out, func(i, j int) bool { return out[i]["name"].(string) < out[j]["name"].(string) })
			sendJSON(w, 200, out)
			return true
		}
		notFound(w)
		return true
	}

	body, ok := readJSONBody(r)
	if !ok {
		failWith(w, 400, "Problems parsing JSON")
		return true
	}
	message := str(body, "message")

	if r.Method == "PUT" {
		if clean == "" {
			sendJSON(w, 422, map[string]any{"message": "Invalid path"})
			return true
		}
		data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(str(body, "content"), "\n", ""))
		if err != nil {
			data = []byte{}
		}
		writeBranch := repo.DefaultBranch
		if v := str(body, "branch"); v != "" {
			writeBranch = strings.TrimPrefix(v, "refs/heads/")
		}
		writeTip := repo.Refs["refs/heads/"+writeBranch]
		branchFiles := map[string]TreeEntry{}
		if writeTip != "" {
			if obj := repo.ReadObject(writeTip); obj != nil && obj.Type == "commit" {
				repo.FlattenTree(ParseCommit(obj.Data).Tree, "", branchFiles)
			}
		}
		existing, hasExisting := branchFiles[clean]
		supplied := str(body, "sha")
		if supplied != "" && hasExisting && existing.SHA != supplied {
			sendJSON(w, 409, map[string]any{"message": clean + " does not match " + supplied})
			return true
		}
		if supplied != "" && !hasExisting {
			sendJSON(w, 409, map[string]any{"message": clean + " does not exist at " + supplied})
			return true
		}
		blob := repo.PutObject("blob", data)
		branchFiles[clean] = TreeEntry{Mode: "100644", Name: clean, SHA: blob}
		if message == "" {
			message = "Update " + clean
		}
		var parents []string
		if writeTip != "" {
			parents = []string{writeTip}
		}
		commitSHA := commitOnBranch(store, repo, writeBranch, branchFiles, message, parents)
		name := clean[strings.LastIndexByte(clean, '/')+1:]
		status := 200
		if !hasExisting {
			status = 201
		}
		sendJSON(w, status, map[string]any{
			"content": map[string]any{"name": name, "path": clean, "sha": blob, "size": len(data), "encoding": "base64", "content": base64.StdEncoding.EncodeToString(data), "url": fmt.Sprintf("/api/v3/repos/%s/%s/contents/%s", repo.Owner, repo.Name, clean)},
			"commit":  map[string]any{"sha": commitSHA, "node_id": "C_" + commitSHA, "message": message, "url": fmt.Sprintf("/api/v3/repos/%s/%s/commits/%s", repo.Owner, repo.Name, commitSHA)},
		})
		return true
	}

	if r.Method == "DELETE" {
		writeBranch := repo.DefaultBranch
		if v := str(body, "branch"); v != "" {
			writeBranch = strings.TrimPrefix(v, "refs/heads/")
		}
		writeTip := repo.Refs["refs/heads/"+writeBranch]
		deleteFiles := map[string]TreeEntry{}
		if writeTip != "" {
			if obj := repo.ReadObject(writeTip); obj != nil && obj.Type == "commit" {
				repo.FlattenTree(ParseCommit(obj.Data).Tree, "", deleteFiles)
			}
		}
		existing, ok := deleteFiles[clean]
		if !ok {
			notFound(w)
			return true
		}
		if supplied := str(body, "sha"); supplied != "" && supplied != existing.SHA {
			sendJSON(w, 409, map[string]any{"message": clean + " does not match " + supplied})
			return true
		}
		delete(deleteFiles, clean)
		if message == "" {
			message = "Delete " + clean
		}
		var parents []string
		if writeTip != "" {
			parents = []string{writeTip}
		}
		commitSHA := commitOnBranch(store, repo, writeBranch, deleteFiles, message, parents)
		sendJSON(w, 200, map[string]any{
			"content": nil,
			"commit":  map[string]any{"sha": commitSHA, "node_id": "C_" + commitSHA, "message": message, "url": fmt.Sprintf("/api/v3/repos/%s/%s/commits/%s", repo.Owner, repo.Name, commitSHA)},
		})
		return true
	}
	return false
}

func commitOnBranch(store *Store, repo *Repo, branch string, files map[string]TreeEntry, message string, parents []string) string {
	tree := repo.WriteTreeFromPaths(files)
	when := store.Clock.Tick()
	commit := repo.PutObject("commit", BuildCommit(tree, parents, message, repo.Owner, repo.Owner+"@example.invalid", when))
	repo.Refs["refs/heads/"+branch] = commit
	repo.clearCaches()
	repo.PushedAt = when
	return commit
}

func handleIssues(w http.ResponseWriter, r *http.Request, store *Store, repo *Repo, rest []string) bool {
	method := r.Method
	if len(rest) == 0 {
		if method == "GET" {
			q := r.URL.Query()
			state := q.Get("state")
			if state == "" {
				state = "open"
			}
			direction := q.Get("direction")
			if direction == "" {
				direction = "desc"
			}
			sortBy := q.Get("sort")
			if sortBy == "" {
				sortBy = "created"
			}
			list := make([]*Issue, 0, len(repo.Issues))
			for _, i := range repo.Issues {
				if state == "all" || i.State == state {
					list = append(list, i)
				}
			}
			sort.SliceStable(list, func(a, b int) bool {
				ka, kb := list[a].CreatedAt, list[b].CreatedAt
				if sortBy == "updated" {
					ka, kb = list[a].UpdatedAt, list[b].UpdatedAt
				}
				if ka == kb {
					if direction == "asc" {
						return list[a].Number < list[b].Number
					}
					return list[a].Number > list[b].Number
				}
				if direction == "asc" {
					return ka < kb
				}
				return ka > kb
			})
			page := paginate(w, r, list, "/repos/"+repo.Owner+"/"+repo.Name+"/issues")
			out := make([]json.RawMessage, 0, len(page))
			for _, issue := range page {
				out = append(out, cachedIssueJSON(repo, issue))
			}
			writeJSONArray(w, out)
			return true
		}
		if method == "POST" {
			body, ok := readJSONBody(r)
			if !ok {
				failWith(w, 400, "Problems parsing JSON")
				return true
			}
			title := str(body, "title")
			if title == "" {
				sendJSON(w, 422, map[string]any{"message": "Validation Failed", "errors": []any{map[string]any{"resource": "Issue", "field": "title", "code": "missing_field"}}})
				return true
			}
			var labels []*Label
			if raw, ok := body["labels"].([]any); ok {
				for _, entry := range raw {
					name, ok := entry.(string)
					if !ok {
						continue
					}
					var found *Label
					for _, l := range repo.Labels {
						if l.Name == name {
							found = l
							break
						}
					}
					if found == nil {
						sendJSON(w, 422, map[string]any{"message": "Validation Failed", "errors": []any{map[string]any{"resource": "Issue", "field": "labels", "code": "invalid", "message": "Label does not exist: " + name}}})
						return true
					}
					labels = append(labels, found)
				}
			}
			now := store.Clock.Tick()
			issue := &Issue{ID: store.NewIssueID(), Number: repo.NextIssue, Title: title, Body: str(body, "body"), State: "open", Labels: labels, CreatedAt: now, UpdatedAt: now}
			repo.NextIssue++
			repo.Issues = append(repo.Issues, issue)
			sendJSON(w, 201, issueShape(repo, issue))
			return true
		}
	}

	number, err := strconv.Atoi(rest[0])
	if err != nil {
		return false
	}
	var issue *Issue
	for _, i := range repo.Issues {
		if i.Number == number {
			issue = i
			break
		}
	}
	if issue == nil {
		notFound(w)
		return true
	}

	if len(rest) == 1 {
		switch method {
		case "GET":
			sendJSONBytes(w, 200, cachedIssueJSON(repo, issue))
			return true
		case "PATCH":
			body, ok := readJSONBody(r)
			if !ok {
				failWith(w, 400, "Problems parsing JSON")
				return true
			}
			if v, ok := body["title"].(string); ok {
				issue.Title = v
			}
			if v, ok := body["body"].(string); ok {
				issue.Body = v
			}
			if v, ok := body["state"].(string); ok && (v == "open" || v == "closed") {
				issue.State = v
				if v == "closed" {
					closed := store.Clock.Tick()
					issue.ClosedAt = &closed
				} else {
					issue.ClosedAt = nil
				}
			}
			issue.UpdatedAt = store.Clock.Tick()
			delete(repo.IssueJSON, issue.ID)
			sendJSON(w, 200, issueShape(repo, issue))
			return true
		}
	}

	if len(rest) == 2 && rest[1] == "comments" {
		if method == "GET" {
			out := make([]map[string]any, 0, len(issue.Comments))
			for _, c := range issue.Comments {
				out = append(out, map[string]any{"id": c.ID, "node_id": fmt.Sprintf("IC_%d", c.ID), "body": c.Body, "user": userShape(repo.Owner), "created_at": c.CreatedAt, "updated_at": c.UpdatedAt, "url": ""})
			}
			sendJSON(w, 200, out)
			return true
		}
		if method == "POST" {
			body, ok := readJSONBody(r)
			if !ok {
				failWith(w, 400, "Problems parsing JSON")
				return true
			}
			text := str(body, "body")
			if text == "" {
				sendJSON(w, 422, map[string]any{"message": "Validation Failed"})
				return true
			}
			now := store.Clock.Tick()
			comment := &Comment{ID: store.NewCommentID(), Body: text, CreatedAt: now, UpdatedAt: now}
			issue.Comments = append(issue.Comments, comment)
			issue.UpdatedAt = now
			delete(repo.IssueJSON, issue.ID)
			sendJSON(w, 201, map[string]any{"id": comment.ID, "node_id": fmt.Sprintf("IC_%d", comment.ID), "body": comment.Body, "user": userShape(repo.Owner), "created_at": now, "updated_at": now, "url": ""})
			return true
		}
	}
	return false
}

// cachedIssueJSON serializes an issue once per revision: the read workload asks for the same issue
// two thousand times.
func cachedIssueJSON(repo *Repo, issue *Issue) []byte {
	if cached, ok := repo.IssueJSON[issue.ID]; ok && cached.stamp == issue.UpdatedAt {
		return cached.text
	}
	data, err := json.Marshal(issueShape(repo, issue))
	if err != nil {
		data = []byte(`{"message":"encoding error"}`)
	}
	data = append(data, '\n')
	repo.IssueJSON[issue.ID] = cachedJSON{stamp: issue.UpdatedAt, text: data}
	return data
}

func writeJSONArray(w http.ResponseWriter, items []json.RawMessage) {
	var buf []byte
	buf = append(buf, '[')
	for i, item := range items {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, strings.TrimRight(string(item), "\n")...)
	}
	buf = append(buf, ']', '\n')
	sendJSONBytes(w, 200, buf)
}

func diffFiles(repo *Repo, baseRef, headRef string) []PullFile {
	read := func(ref string) map[string]TreeEntry {
		out := map[string]TreeEntry{}
		tip := repo.Refs["refs/heads/"+ref]
		if tip == "" {
			return out
		}
		if obj := repo.ReadObject(tip); obj != nil && obj.Type == "commit" {
			repo.FlattenTree(ParseCommit(obj.Data).Tree, "", out)
		}
		return out
	}
	before := read(baseRef)
	after := read(headRef)
	paths := make([]string, 0, len(before)+len(after))
	for p := range before {
		paths = append(paths, p)
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	files := make([]PullFile, 0, len(paths))
	for _, path := range paths {
		b, hasB := before[path]
		a, hasA := after[path]
		if hasA && hasB && a.SHA == b.SHA {
			continue
		}
		beforeData, afterData := "", ""
		if hasB {
			if obj := repo.ReadObject(b.SHA); obj != nil {
				beforeData = string(obj.Data)
			}
		}
		if hasA {
			if obj := repo.ReadObject(a.SHA); obj != nil {
				afterData = string(obj.Data)
			}
		}
		beforeLines := []string{}
		if beforeData != "" {
			beforeLines = strings.Split(beforeData, "\n")
		}
		afterLines := []string{}
		if afterData != "" {
			afterLines = strings.Split(afterData, "\n")
		}
		additions, deletions := 0, 0
		maxLen := len(beforeLines)
		if len(afterLines) > maxLen {
			maxLen = len(afterLines)
		}
		for i := 0; i < maxLen; i++ {
			var bl, al string
			hasBL := i < len(beforeLines)
			hasAL := i < len(afterLines)
			if hasBL {
				bl = beforeLines[i]
			}
			if hasAL {
				al = afterLines[i]
			}
			if hasBL && hasAL && bl == al {
				continue
			}
			if hasBL {
				deletions++
			}
			if hasAL {
				additions++
			}
		}
		status := "modified"
		if !hasB {
			status = "added"
		} else if !hasA {
			status = "removed"
		}
		sha := ""
		if hasA {
			sha = a.SHA
		} else if hasB {
			sha = b.SHA
		}
		files = append(files, PullFile{Filename: path, Status: status, Additions: additions, Deletions: deletions, Changes: additions + deletions, SHA: sha})
	}
	return files
}

func pullMergeable(repo *Repo, pull *Pull) bool {
	baseTip := repo.Refs["refs/heads/"+pull.Base.Ref]
	headTip := repo.Refs["refs/heads/"+pull.Head.Ref]
	if baseTip == "" || headTip == "" {
		return false
	}
	common, ok := repo.MergeBase(baseTip, headTip)
	if !ok {
		return false
	}
	changed := func(from, to string) map[string]struct{} {
		out := map[string]struct{}{}
		fromObj := repo.ReadObject(from)
		toObj := repo.ReadObject(to)
		if fromObj == nil || toObj == nil {
			return out
		}
		before := map[string]TreeEntry{}
		after := map[string]TreeEntry{}
		repo.FlattenTree(ParseCommit(fromObj.Data).Tree, "", before)
		repo.FlattenTree(ParseCommit(toObj.Data).Tree, "", after)
		for p, v := range after {
			if b, ok := before[p]; !ok || b.SHA != v.SHA {
				out[p] = struct{}{}
			}
		}
		for p := range before {
			if _, ok := after[p]; !ok {
				out[p] = struct{}{}
			}
		}
		return out
	}
	onBase := changed(common, baseTip)
	onHead := changed(common, headTip)
	for p := range onHead {
		if _, both := onBase[p]; both {
			return false
		}
	}
	return true
}

func refreshPull(repo *Repo, pull *Pull) {
	baseTip := repo.Refs["refs/heads/"+pull.Base.Ref]
	headTip := repo.Refs["refs/heads/"+pull.Head.Ref]
	if baseTip != "" {
		pull.Base.SHA = baseTip
	}
	if headTip != "" {
		pull.Head.SHA = headTip
	}
	if baseTip != "" && headTip != "" {
		pull.Files = diffFiles(repo, pull.Base.Ref, pull.Head.Ref)
		pull.Mergeable = pullMergeable(repo, pull)
	}
}

func handlePulls(w http.ResponseWriter, r *http.Request, store *Store, repo *Repo, rest []string) bool {
	method := r.Method
	if len(rest) == 0 {
		if method == "GET" {
			state := r.URL.Query().Get("state")
			if state == "" {
				state = "open"
			}
			list := make([]*Pull, 0, len(repo.Pulls))
			for _, p := range repo.Pulls {
				if state == "all" || p.State == state {
					list = append(list, p)
				}
			}
			sort.SliceStable(list, func(a, b int) bool { return list[a].Number > list[b].Number })
			for _, p := range list {
				refreshPull(repo, p)
			}
			page := paginate(w, r, list, "/repos/"+repo.Owner+"/"+repo.Name+"/pulls")
			out := make([]map[string]any, 0, len(page))
			for _, p := range page {
				out = append(out, pullShape(repo, p))
			}
			sendJSON(w, 200, out)
			return true
		}
		if method == "POST" {
			body, ok := readJSONBody(r)
			if !ok {
				failWith(w, 400, "Problems parsing JSON")
				return true
			}
			headRef := str(body, "head")
			baseRef := str(body, "base")
			if baseRef == "" {
				baseRef = repo.DefaultBranch
			}
			headTip := repo.Refs["refs/heads/"+headRef]
			baseTip := repo.Refs["refs/heads/"+baseRef]
			if headTip == "" || baseTip == "" {
				sendJSON(w, 422, map[string]any{"message": "Validation Failed", "errors": []any{map[string]any{"resource": "PullRequest", "code": "invalid", "field": "head"}}})
				return true
			}
			now := store.Clock.Tick()
			pull := &Pull{ID: store.NewPullID(), Number: repo.NextPull, Title: str(body, "title"), Body: str(body, "body"), State: "open", Head: Side{Ref: headRef, SHA: headTip}, Base: Side{Ref: baseRef, SHA: baseTip}, CreatedAt: now, UpdatedAt: now, Mergeable: true}
			repo.NextPull++
			repo.Pulls = append(repo.Pulls, pull)
			refreshPull(repo, pull)
			sendJSON(w, 201, pullShape(repo, pull))
			return true
		}
	}

	number, err := strconv.Atoi(rest[0])
	if err != nil {
		return false
	}
	var pull *Pull
	for _, p := range repo.Pulls {
		if p.Number == number {
			pull = p
			break
		}
	}
	if pull == nil {
		notFound(w)
		return true
	}
	refreshPull(repo, pull)

	if len(rest) == 1 && method == "GET" {
		sendJSON(w, 200, pullShape(repo, pull))
		return true
	}

	if len(rest) == 2 && rest[1] == "files" && method == "GET" {
		out := make([]map[string]any, 0, len(pull.Files))
		for _, f := range pull.Files {
			out = append(out, map[string]any{"filename": f.Filename, "status": f.Status, "additions": f.Additions, "deletions": f.Deletions, "changes": f.Changes, "sha": f.SHA, "blob_url": ""})
		}
		sendJSON(w, 200, out)
		return true
	}

	if len(rest) == 2 && rest[1] == "merge" && method == "PUT" {
		if pull.Merged || !pull.Mergeable {
			failWith(w, 405, "Pull Request is not mergeable")
			return true
		}
		headTip := repo.Refs["refs/heads/"+pull.Head.Ref]
		baseTip := repo.Refs["refs/heads/"+pull.Base.Ref]
		headObj := repo.ReadObject(headTip)
		if headTip == "" || baseTip == "" || headObj == nil {
			failWith(w, 405, "Pull Request is not mergeable")
			return true
		}
		tree := ParseCommit(headObj.Data).Tree
		now := store.Clock.Tick()
		message := fmt.Sprintf("Merge pull request #%d from %s\n\n%s", pull.Number, pull.Head.Ref, pull.Title)
		mergeSHA := repo.PutObject("commit", BuildCommit(tree, []string{baseTip}, message, repo.Owner, repo.Owner+"@example.invalid", now))
		repo.Refs["refs/heads/"+pull.Base.Ref] = mergeSHA
		repo.clearCaches()
		pull.State = "closed"
		pull.Merged = true
		pull.MergedAt = &now
		pull.ClosedAt = &now
		pull.MergeCommitSHA = &mergeSHA
		pull.UpdatedAt = now
		repo.PushedAt = now
		sendJSON(w, 200, map[string]any{"sha": mergeSHA, "merged": true, "message": "Pull Request successfully merged"})
		return true
	}
	return false
}

func splitPath(path string) []string {
	parts := strings.Split(path, "/")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

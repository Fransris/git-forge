package main

// The forge's one process: bind an ephemeral port, register it with the env, serve until the env
// says the trial is over, then exit 0.

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

var store = NewStore()
var shopToken = os.Getenv("MAKEFUN_SHOP_TOKEN")

func authorised(r *http.Request) bool {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") && !strings.HasPrefix(header, "bearer ") {
		return false
	}
	presented := strings.TrimSpace(header[7:])
	if presented == "" {
		return false
	}
	if shopToken == "" {
		return true
	}
	return presented == shopToken
}

func handler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	method := r.Method

	if method == "GET" && path == "/healthz" {
		sendJSON(w, 200, map[string]any{"ok": true})
		return
	}

	body := []byte{}
	if method != "GET" && method != "HEAD" {
		body, _ = io.ReadAll(r.Body)
		r.Body.Close()
	}

	// git smart HTTP: /{owner}/{repo}.git/...
	if strings.HasSuffix(path, "/info/refs") || strings.HasSuffix(path, "/git-upload-pack") || strings.HasSuffix(path, "/git-receive-pack") {
		if owner, repoName, rest, ok := parseGitPath(path); ok {
			store.LearnOwner(owner)
			repo := store.Find(owner, repoName)
			if repo == nil {
				sendJSON(w, 404, map[string]any{"message": "Not Found"})
				return
			}
			switch {
			case method == "GET" && rest == "/info/refs":
				handleInfoRefs(w, r, repo, r.URL.Query().Get("service"))
				return
			case method == "POST" && rest == "/git-upload-pack":
				handleUploadPack(w, r, repo, body)
				return
			case method == "POST" && rest == "/git-receive-pack":
				handleReceivePack(w, r, repo, body)
				return
			}
			sendJSON(w, 404, map[string]any{"message": "Not Found"})
			return
		}
	}

	if path == "/mcp" || strings.HasPrefix(path, "/mcp/") {
		if !authorised(r) {
			sendJSON(w, 401, map[string]any{"message": "Requires authentication"})
			return
		}
		if method != "POST" {
			sendJSON(w, 405, map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32600, "message": "Use POST for JSON-RPC"}})
			return
		}
		handleMCP(w, r, store, body)
		return
	}

	if path == "/api/graphql" || path == "/api/graphql/" {
		if !authorised(r) {
			sendJSON(w, 401, map[string]any{"message": "Requires authentication"})
			return
		}
		if method != "POST" {
			sendJSON(w, 405, map[string]any{"message": "Use POST"})
			return
		}
		handleGraphQL(w, r, store, body)
		return
	}

	if path == "/api/v3" || strings.HasPrefix(path, "/api/v3/") {
		if !authorised(r) {
			sendJSON(w, 401, map[string]any{"message": "Requires authentication"})
			return
		}
		sub := ""
		if path != "/api/v3" {
			sub = path[len("/api/v3"):]
		}
		if handleREST(w, r, store, sub) {
			return
		}
		sendJSON(w, 404, map[string]any{"message": "Not Found"})
		return
	}

	sendJSON(w, 404, map[string]any{"message": "Not Found"})
}

// parseGitPath splits /{owner}/{repo}.git/rest.
func parseGitPath(path string) (owner, repo, rest string, ok bool) {
	trimmed := strings.TrimPrefix(path, "/")
	first := strings.IndexByte(trimmed, '/')
	if first < 0 {
		return "", "", "", false
	}
	owner = trimmed[:first]
	remainder := trimmed[first+1:]
	dot := strings.Index(remainder, ".git")
	if dot < 0 {
		return "", "", "", false
	}
	repo = remainder[:dot]
	rest = remainder[dot+4:]
	return owner, repo, rest, true
}

func register(envURL string, port int) {
	payload, _ := json.Marshal(map[string]int{"port": port})
	client := &http.Client{Timeout: 30 * time.Second}
	for attempt := 0; ; attempt++ {
		res, err := client.Post(envURL+"/register", "application/json", bytes.NewReader(payload))
		if err == nil {
			res.Body.Close()
			if res.StatusCode == 200 || res.StatusCode == 409 {
				return
			}
			if res.StatusCode == 400 || res.StatusCode == 405 {
				os.Exit(1)
			}
		}
		wait := 250 * time.Millisecond
		for i := 0; i < attempt && i < 8; i++ {
			wait *= 2
		}
		if wait > 2*time.Second {
			wait = 2 * time.Second
		}
		time.Sleep(wait)
	}
}

func waitDone(envURL string) {
	client := &http.Client{Timeout: 30 * time.Second}
	for {
		res, err := client.Get(envURL + "/done")
		if err == nil {
			data, _ := io.ReadAll(res.Body)
			res.Body.Close()
			var answer struct {
				Done bool `json:"done"`
			}
			if json.Unmarshal(data, &answer) == nil && answer.Done {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func main() {
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		os.Stderr.WriteString("[forge] MAKEFUN_ENV_URL is not set\n")
		os.Exit(1)
	}

	// Bind every interface: the suite dials the address the registration request came from, which in the
	// sealed pod is not loopback, so a 127.0.0.1-only listener is unreachable and scores 0.
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		os.Stderr.WriteString("[forge] cannot bind: " + err.Error() + "\n")
		os.Exit(1)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	server := &http.Server{
		Handler:           http.HandlerFunc(handler),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	server.SetKeepAlivesEnabled(true)

	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			os.Stderr.WriteString("[forge] serve: " + err.Error() + "\n")
		}
	}()

	os.Stderr.WriteString("[forge] listening on port " + strconv.Itoa(port) + "\n")
	register(envURL, port)
	os.Stderr.WriteString("[forge] registered; waiting for the env to say done\n")
	waitDone(envURL)
	os.Stderr.WriteString("[forge] done\n")
	server.Close()
	os.Exit(0)
}

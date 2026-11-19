package main

// The forge's store: users, repositories, issues, pull requests, labels and the git object graph.
// Everything is deterministic — ids from counters, times from a fixed epoch plus a tick.

import (
	"sync"
	"time"
)

const epochMillis int64 = 1767225600000 // 2026-01-01T00:00:00Z

type Clock struct {
	mu    sync.Mutex
	ticks int64
}

func (c *Clock) Tick() string {
	c.mu.Lock()
	c.ticks++
	n := c.ticks
	c.mu.Unlock()
	t := time.UnixMilli(epochMillis + n*1000).UTC()
	return t.Format("2006-01-02T15:04:05Z")
}

func (c *Clock) Now() string {
	c.mu.Lock()
	n := c.ticks
	c.mu.Unlock()
	return time.UnixMilli(epochMillis + n*1000).UTC().Format("2006-01-02T15:04:05Z")
}

type Label struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

type Comment struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type Issue struct {
	ID        int64     `json:"id"`
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	Labels    []*Label  `json:"-"`
	CreatedAt string    `json:"created_at"`
	UpdatedAt string    `json:"updated_at"`
	ClosedAt  *string   `json:"closed_at"`
	Comments  []*Comment `json:"-"`
}

type PullFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Changes   int    `json:"changes"`
	SHA       string `json:"sha"`
	BlobURL   string `json:"blob_url"`
}

type Side struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

type Pull struct {
	ID             int64      `json:"id"`
	Number         int        `json:"number"`
	Title          string     `json:"title"`
	Body           string     `json:"body"`
	State          string     `json:"state"`
	Head           Side       `json:"head"`
	Base           Side       `json:"base"`
	CreatedAt      string     `json:"created_at"`
	UpdatedAt      string     `json:"updated_at"`
	ClosedAt       *string    `json:"closed_at"`
	Merged         bool       `json:"merged"`
	MergedAt       *string    `json:"merged_at"`
	MergeCommitSHA *string    `json:"merge_commit_sha"`
	Mergeable      bool       `json:"mergeable"`
	Files          []PullFile `json:"-"`
}

type GitObject struct {
	Type string
	Data []byte
}

type Repo struct {
	ID            int64
	Name          string
	Owner         string
	Description   string
	Private       bool
	DefaultBranch string
	CreatedAt     string
	UpdatedAt     string
	PushedAt      string
	Issues        []*Issue
	Pulls         []*Pull
	Labels        []*Label
	NextIssue     int
	NextPull      int
	Objects       map[string]*GitObject
	Refs          map[string]string
	PackCache     map[string][]byte
	BodyCache     map[string][]byte
	IssueJSON     map[int64]cachedJSON
}

type cachedJSON struct {
	stamp string
	text  []byte
}

type Store struct {
	mu         sync.RWMutex
	Clock      Clock
	Repos      map[string]*Repo
	Viewer     string
	nextRepoID int64
	nextIssue  int64
	nextComent int64
	nextPull   int64
	nextLabel  int64
}

func NewStore() *Store {
	return &Store{
		Repos:      make(map[string]*Repo),
		nextRepoID: 1000,
		nextIssue:  100000,
		nextComent: 200000,
		nextPull:   300000,
		nextLabel:  400000,
	}
}

func repoKey(owner, name string) string { return lower(owner) + "/" + lower(name) }

// LearnOwner adopts the first owner seen in a request path as this forge's own account, re-owning
// the repositories created before the login was known.
func (s *Store) LearnOwner(owner string) {
	if owner == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Viewer != "" {
		return
	}
	s.Viewer = owner
	rekeyed := make(map[string]*Repo, len(s.Repos))
	for _, repo := range s.Repos {
		repo.Owner = owner
		rekeyed[repoKey(owner, repo.Name)] = repo
	}
	s.Repos = rekeyed
}

func (s *Store) Login() string {
	s.mu.RLock()
	v := s.Viewer
	s.mu.RUnlock()
	if v == "" {
		return "viewer"
	}
	return v
}

func (s *Store) Find(owner, name string) *Repo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	repo, ok := s.Repos[repoKey(owner, name)]
	if !ok {
		return nil
	}
	if lower(owner) != lower(repo.Owner) {
		return nil
	}
	return repo
}

func (s *Store) ReposOf(owner string) []*Repo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Repo, 0, len(s.Repos))
	for _, repo := range s.Repos {
		if lower(repo.Owner) == lower(owner) {
			out = append(out, repo)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].ID > out[j-1].ID; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func (s *Store) CreateRepo(name, description string, private bool) (*Repo, bool) {
	owner := s.Login()
	s.mu.Lock()
	defer s.mu.Unlock()
	key := repoKey(owner, name)
	if _, exists := s.Repos[key]; exists {
		return nil, false
	}
	now := s.Clock.Tick()
	s.nextRepoID++
	repo := &Repo{
		ID:            s.nextRepoID,
		Name:          name,
		Owner:         owner,
		Description:   description,
		Private:       private,
		DefaultBranch: "main",
		CreatedAt:     now,
		UpdatedAt:     now,
		PushedAt:      now,
		NextIssue:     1,
		NextPull:      1,
		Objects:       make(map[string]*GitObject),
		Refs:          make(map[string]string),
		PackCache:     make(map[string][]byte),
		BodyCache:     make(map[string][]byte),
		IssueJSON:     make(map[int64]cachedJSON),
	}
	s.Repos[key] = repo
	return repo, true
}

func (s *Store) DeleteRepo(owner, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := repoKey(owner, name)
	if _, ok := s.Repos[key]; !ok {
		return false
	}
	delete(s.Repos, key)
	return true
}

func (s *Store) NewIssueID() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextIssue++
	return s.nextIssue
}

func (s *Store) NewCommentID() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextComent++
	return s.nextComent
}

func (s *Store) NewPullID() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextPull++
	return s.nextPull
}

func (s *Store) NewLabelID() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextLabel++
	return s.nextLabel
}

func (r *Repo) clearCaches() {
	if len(r.PackCache) > 0 {
		r.PackCache = make(map[string][]byte)
	}
	if len(r.BodyCache) > 0 {
		r.BodyCache = make(map[string][]byte)
	}
	r.IssueJSON = make(map[int64]cachedJSON)
}

func lower(s string) string {
	b := []byte(s)
	for i := 0; i < len(b); i++ {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

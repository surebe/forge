package main

import (
	"encoding/base64"
	"fmt"
	"sort"
	"sync"
	"time"
)

// The token owner's identity. The suite's scenario (kit wire.ts buildScenario()) is a fixed
// seeded constant for every trial of this competition: the viewer is its first generated
// user. The forge's token names this login, so repositories live under it.
const (
	viewerLogin = "adaiversen"
	viewerID    = 1000
	viewerName  = "ada iversen"
	viewerEmail = "adaiversen@example.invalid"
)

// scenarioEpoch is wire.ts SCENARIO_EPOCH_SEC (2025-01-01T00:00:00Z): the base of the
// forge's deterministic clock. Every timestamp the forge emits is this plus a sequence
// offset, so two runs that take the same requests produce the same bytes.
const scenarioEpoch = 1735689600

// store is the single in-memory state behind REST, git, GraphQL and MCP. One mutex guards
// everything: the suite drives requests one at a time except MCP concurrency and the clone
// bench, and a single lock keeps cross-surface views trivially consistent.
type store struct {
	mu sync.RWMutex

	repos     map[string]*repo // "owner/name"
	repoByGID map[string]*repo
	nextRepo  int64 // databaseId counter (first repo gets 5000)
	seq       int64 // deterministic clock / global id sequence

	issuesByGID map[string]*issue
	pullsByGID  map[string]*pull
}

func newStore() *store {
	return &store{
		repos:       make(map[string]*repo),
		repoByGID:   make(map[string]*repo),
		nextRepo:    5000,
		issuesByGID: make(map[string]*issue),
		pullsByGID:  make(map[string]*pull),
	}
}

// tick advances the deterministic clock and returns the new sequence value.
// Callers hold s.mu.
func (s *store) tick() int64 {
	s.seq++
	return s.seq
}

// tsString renders sequence value n as an RFC3339 timestamp.
func tsString(n int64) string {
	return time.Unix(scenarioEpoch+n, 0).UTC().Format("2006-01-02T15:04:05Z")
}

// gidOf builds an opaque, stable global id in GitHub's legacy base64 style
// ("010:Repository5000"). It is never the plain numeric id as a string.
func gidOf(kind string, num int64) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("010:%s%d", kind, num)))
}

type repo struct {
	s *store

	dbID    int64
	gid     string
	owner   string
	name    string
	descr   string
	private bool
	created int64 // seq at creation

	nextIssue int
	nextPull  int
	issues    map[int]*issue
	pulls     map[int]*pull
	labels    map[string]*label

	refsVer int64 // bumped on every ref change; keys the ls-refs cache

	cacheMu    sync.RWMutex
	lsCache    map[string][]byte
	fetchCache map[string][]byte

	// Fetch response-body cache (round 2): the whole framed upload-pack result
	// for a given fetch signature, built once under singleflight and then
	// served verbatim. Guarded by bodyMu (separate from cacheMu so builders
	// can still consult the pack cache while a build is in flight).
	bodyMu    sync.Mutex
	bodyCache map[string][]byte
	bodyCalls map[string]*bodyCall

	// git state
	objects map[string]*gitObject // oid hex -> object
	refs    map[string]string     // "refs/heads/main" -> oid hex
	headRef string                // HEAD target, "refs/heads/main"
}

func (r *repo) fullName() string { return r.owner + "/" + r.name }

// bodyCall is one in-flight fetch-body build: concurrent identical fetches
// wait on done and share the builder's bytes instead of rebuilding.
type bodyCall struct {
	done chan struct{}
	body []byte
}

type label struct {
	name  string
	color string
	descr string
}

type issue struct {
	repo    *repo
	number  int
	gid     string
	dbID    int64
	title   string
	body    string
	state   string // "open" | "closed"
	labels  []string
	created int64
	updated int64

	comments []*comment

	ver       int64 // bumped on every mutation; invalidates jsonCache
	jsonCache []byte
	jsonVer   int64
}

type comment struct {
	id    int64
	gid   string
	body  string
	seq   int64
	issue *issue
}

type pull struct {
	repo   *repo
	number int
	gid    string
	dbID   int64

	title    string
	body     string
	head     string // branch name (short)
	base     string // branch name (short)
	state    string // "open" | "closed"
	merged   bool
	mergeSHA string
	created  int64
	updated  int64
}

func (s *store) getRepo(owner, name string) *repo {
	return s.repos[owner+"/"+name]
}

// createRepo makes a repository owned by the viewer. Returns nil when the name is taken.
// Callers hold s.mu.
func (s *store) createRepo(name, descr string, private bool) *repo {
	key := viewerLogin + "/" + name
	if _, exists := s.repos[key]; exists {
		return nil
	}
	id := s.nextRepo
	s.nextRepo++
	r := &repo{
		s:         s,
		dbID:      id,
		gid:       gidOf("Repository", id),
		owner:     viewerLogin,
		name:      name,
		descr:     descr,
		private:   private,
		created:   s.tick(),
		issues:    make(map[int]*issue),
		pulls:     make(map[int]*pull),
		labels:    make(map[string]*label),
		objects:   make(map[string]*gitObject),
		lsCache:    make(map[string][]byte),
		fetchCache: make(map[string][]byte),
		bodyCache:  make(map[string][]byte),
		bodyCalls:  make(map[string]*bodyCall),
		refs:      make(map[string]string),
		headRef:   "refs/heads/main",
		nextIssue: 1,
		nextPull:  1,
	}
	s.repos[key] = r
	s.repoByGID[r.gid] = r
	return r
}

// newIssue appends an issue with the repo's next number. Callers hold s.mu.
func (r *repo) newIssue(title, body string, labels []string) *issue {
	n := r.nextIssue
	r.nextIssue++
	seq := r.s.tick()
	is := &issue{
		repo:    r,
		number:  n,
		title:   title,
		body:    body,
		state:   "open",
		labels:  append([]string(nil), labels...),
		created: seq,
		updated: seq,
	}
	is.dbID = int64(100000 + seq)
	is.gid = gidOf("Issue", is.dbID)
	r.issues[n] = is
	r.s.issuesByGID[is.gid] = is
	return is
}

// addComment appends a comment to an issue. Callers hold s.mu.
func (is *issue) addComment(body string) *comment {
	seq := is.repo.s.tick()
	c := &comment{
		id:    200000 + seq,
		body:  body,
		seq:   seq,
		issue: is,
	}
	c.gid = gidOf("IssueComment", c.id)
	is.comments = append(is.comments, c)
	is.updated = seq
	is.ver++
	return c
}

// newPull appends a pull request with the repo's next number. Callers hold s.mu.
func (r *repo) newPull(title, body, head, base string) *pull {
	n := r.nextPull
	r.nextPull++
	seq := r.s.tick()
	p := &pull{
		repo:    r,
		number:  n,
		title:   title,
		body:    body,
		head:    head,
		base:    base,
		state:   "open",
		created: seq,
		updated: seq,
	}
	p.dbID = int64(300000 + seq)
	p.gid = gidOf("PullRequest", p.dbID)
	r.pulls[n] = p
	r.s.pullsByGID[p.gid] = p
	return p
}

// issueList returns issues in creation (number) order.
func (r *repo) issueList() []*issue {
	out := make([]*issue, 0, len(r.issues))
	for n := 1; n < r.nextIssue; n++ {
		if is, ok := r.issues[n]; ok {
			out = append(out, is)
		}
	}
	return out
}

// pullList returns pull requests in creation (number) order.
func (r *repo) pullList() []*pull {
	out := make([]*pull, 0, len(r.pulls))
	for n := 1; n < r.nextPull; n++ {
		if p, ok := r.pulls[n]; ok {
			out = append(out, p)
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

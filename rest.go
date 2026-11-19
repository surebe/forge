package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// REST v3 surface. Contract highlights (specs/rest-mcp.md):
//   - every reply is JSON with a non-empty X-GitHub-Request-Id (writeJSON does this);
//   - errors are {"message": <non-empty string>} with exact statuses:
//     401 no token, 404 missing/foreign repo, 409 stale contents sha,
//     422 unknown label / duplicate repo, 405 conflicted merge, 400 malformed JSON;
//   - contents PUT/DELETE operate on the real git graph (blob-sha optimistic locking).

func handleREST(s *store, w http.ResponseWriter, r *http.Request) {
	if !authed(r) {
		writeError(w, r, 401, "Bad credentials")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v3")
	if path == "" {
		path = "/"
	}
	// Root and meta are not repo-scoped.
	if path == "/" && r.Method == http.MethodGet {
		w.Header().Set("X-OAuth-Scopes", "repo, read:org")
		writeJSON(w, r, 200, map[string]any{
			"current_user_url": "/api/v3/user",
			"user_url":         "/api/v3/users/" + viewerLogin,
			"rate_limit_url":   "/api/v3/rate_limit",
			"repository_url":   "/api/v3/repos/" + viewerLogin + "/{repo}",
		})
		return
	}
	if path == "/meta" && r.Method == http.MethodGet {
		writeJSON(w, r, 200, map[string]any{
			"installed_version":   "1.0.0",
			"verifiable_password_authentication": false,
			"ssh_key_fingerprints": map[string]any{},
			"hooks":                 []any{},
			"web":                   []any{},
			"api":                   []any{},
			"git":                   []any{},
		})
		return
	}
	if path == "/user" && r.Method == http.MethodGet {
		writeJSON(w, r, 200, userJSON())
		return
	}
	if path == "/user/repos" {
		switch r.Method {
		case http.MethodPost:
			restCreateRepo(s, w, r)
		case http.MethodGet:
			restListUserRepos(s, w, r)
		default:
			writeError(w, r, 404, "Not Found")
		}
		return
	}
	if strings.HasPrefix(path, "/repos/") {
		restRepoRoutes(s, w, r, strings.TrimPrefix(path, "/repos/"))
		return
	}
	writeError(w, r, 404, "Not Found")
}

var sharedUserJSON = buildUserJSON()

func userJSON() map[string]any { return sharedUserJSON }

func buildUserJSON() map[string]any {
	return map[string]any{
		"login":     viewerLogin,
		"id":        viewerID,
		"node_id":   gidOf("User", viewerID),
		"name":      viewerName,
		"email":     viewerEmail,
		"type":      "User",
		"site_admin": false,
		"url":       "/api/v3/users/" + viewerLogin,
		"html_url":  "https://make.fun/" + viewerLogin,
	}
}

// jsonBody parses a request body as a JSON object. On malformed JSON it writes the
// 400 the protocol checks require and returns ok=false. An empty body is an empty object.
func jsonBody(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	data, err := readBody(r)
	if err != nil {
		writeError(w, r, 400, "Invalid request body")
		return nil, false
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]any{}, true
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		writeError(w, r, 400, "Body is not valid JSON")
		return nil, false
	}
	obj, ok := v.(map[string]any)
	if !ok {
		writeError(w, r, 400, "Body must be a JSON object")
		return nil, false
	}
	return obj, true
}

// ------------------------------------------------------------ repositories

func repoJSON(rp *repo) map[string]any {
	return map[string]any{
		"id":             int(rp.dbID),
		"node_id":        rp.gid,
		"name":           rp.name,
		"full_name":      rp.fullName(),
		"private":        rp.private,
		"owner":          userJSON(),
		"description":    rp.descr,
		"default_branch": "main",
		"created_at":     tsString(rp.created),
		"updated_at":     tsString(rp.created),
		"pushed_at":      tsString(rp.created),
		"html_url":       "https://make.fun/" + rp.fullName(),
		"url":            "/api/v3/repos/" + rp.fullName(),
		"clone_url":      "/" + rp.fullName() + ".git",
		"fork":           false,
		"archived":       false,
		"visibility":     map[bool]string{true: "private", false: "public"}[rp.private],
	}
}

func restCreateRepo(s *store, w http.ResponseWriter, r *http.Request) {
	body, ok := jsonBody(w, r)
	if !ok {
		return
	}
	name, _ := body["name"].(string)
	if name == "" {
		writeError(w, r, 422, "Validation Failed: name is required")
		return
	}
	descr, _ := body["description"].(string)
	private, _ := body["private"].(bool)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getRepo(viewerLogin, name) != nil {
		writeError(w, r, 422, "Repository already exists")
		return
	}
	rp := s.createRepo(name, descr, private)
	writeJSON(w, r, 201, repoJSON(rp))
}

func restListUserRepos(s *store, w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	perPage := atoiOr(q.Get("per_page"), 30)
	if perPage <= 0 {
		perPage = 30
	}
	page := atoiOr(q.Get("page"), 1)
	if page <= 0 {
		page = 1
	}
	s.mu.Lock()
	var all []*repo
	for _, rp := range s.repos {
		if rp.owner == viewerLogin {
			all = append(all, rp)
		}
	}
	s.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i].dbID < all[j].dbID })
	start := (page - 1) * perPage
	if start > len(all) {
		start = len(all)
	}
	end := start + perPage
	if end > len(all) {
		end = len(all)
	}
	items := make([]any, 0, end-start)
	for _, rp := range all[start:end] {
		items = append(items, repoJSON(rp))
	}
	if end < len(all) {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		next := fmt.Sprintf("%s://%s/api/v3/user/repos?per_page=%d&page=%d", scheme, r.Host, perPage, page+1)
		last := fmt.Sprintf("%s://%s/api/v3/user/repos?per_page=%d&page=%d", scheme, r.Host, perPage, (len(all)+perPage-1)/perPage)
		w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next", <%s>; rel="last"`, next, last))
	}
	writeJSON(w, r, 200, items)
}

// restRepoRoutes dispatches /repos/{owner}/{repo}/...
func restRepoRoutes(s *store, w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 {
		writeError(w, r, 404, "Not Found")
		return
	}
	owner, name := parts[0], parts[1]
	tail := ""
	if len(parts) == 3 {
		tail = parts[2]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rp := s.getRepo(owner, name)
	if rp == nil || owner != viewerLogin {
		writeError(w, r, 404, "Not Found")
		return
	}
	switch {
	case tail == "" && r.Method == http.MethodGet:
		writeJSON(w, r, 200, repoJSON(rp))
	case tail == "" && r.Method == http.MethodPatch:
		body, ok := jsonBody(w, r)
		if !ok {
			return
		}
		if v, present := body["description"]; present {
			if str, isStr := v.(string); isStr {
				rp.descr = str
			} else if v == nil {
				rp.descr = ""
			}
		}
		if v, present := body["private"]; present {
			if b, isBool := v.(bool); isBool {
				rp.private = b
			}
		}
		if v, present := body["name"]; present {
			if str, isStr := v.(string); isStr && str != "" && str != rp.name {
				delete(s.repos, rp.fullName())
				rp.name = str
				s.repos[rp.fullName()] = rp
			}
		}
		writeJSON(w, r, 200, repoJSON(rp))
	case tail == "" && r.Method == http.MethodDelete:
		deleteRepo(s, rp)
		w.Header().Set("X-GitHub-Request-Id", requestID())
		w.WriteHeader(204)
	case tail == "labels" && r.Method == http.MethodPost:
		restCreateLabel(rp, w, r)
	case tail == "labels" && r.Method == http.MethodGet:
		var items []any
		for _, name := range sortedKeys(rp.labels) {
			items = append(items, labelJSON(rp.labels[name]))
		}
		writeJSON(w, r, 200, items)
	case tail == "issues" && r.Method == http.MethodPost:
		restCreateIssue(rp, w, r)
	case tail == "issues" && r.Method == http.MethodGet:
		restListIssues(rp, w, r)
	case strings.HasPrefix(tail, "issues/"):
		restIssueRoutes(rp, w, r, strings.TrimPrefix(tail, "issues/"))
	case tail == "pulls" && r.Method == http.MethodPost:
		restCreatePull(rp, w, r)
	case tail == "pulls" && r.Method == http.MethodGet:
		restListPulls(rp, w, r)
	case strings.HasPrefix(tail, "pulls/"):
		restPullRoutes(rp, w, r, strings.TrimPrefix(tail, "pulls/"))
	case tail == "branches" && r.Method == http.MethodGet:
		restListBranches(rp, w, r)
	case strings.HasPrefix(tail, "branches/"):
		restGetBranch(rp, w, r, strings.TrimPrefix(tail, "branches/"))
	case tail == "commits" && r.Method == http.MethodGet:
		restListCommits(rp, w, r)
	case tail == "git/refs" && r.Method == http.MethodPost:
		restCreateRef(rp, w, r)
	case strings.HasPrefix(tail, "git/refs/"):
		restGetRef(rp, w, r, "refs/"+strings.TrimPrefix(tail, "git/refs/"))
	case strings.HasPrefix(tail, "contents/") || tail == "contents":
		restContents(rp, w, r, strings.TrimPrefix(tail, "contents"))
	default:
		writeError(w, r, 404, "Not Found")
	}
}

func deleteRepo(s *store, rp *repo) {
	delete(s.repos, rp.fullName())
	delete(s.repoByGID, rp.gid)
	for _, is := range rp.issues {
		delete(s.issuesByGID, is.gid)
	}
	for _, p := range rp.pulls {
		delete(s.pullsByGID, p.gid)
	}
}

func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// ------------------------------------------------------------ labels & issues

func labelJSON(l *label) map[string]any {
	return map[string]any{
		"id":      1,
		"node_id": gidOf("Label", 1),
		"name":    l.name,
		"color":   l.color,
		"description": nil,
		"default": false,
		"url":     "",
	}
}

func restCreateLabel(rp *repo, w http.ResponseWriter, r *http.Request) {
	body, ok := jsonBody(w, r)
	if !ok {
		return
	}
	name, _ := body["name"].(string)
	if name == "" {
		writeError(w, r, 422, "Validation Failed: name is required")
		return
	}
	if _, exists := rp.labels[name]; exists {
		writeError(w, r, 422, "Validation Failed: label already exists")
		return
	}
	color, _ := body["color"].(string)
	if color == "" {
		color = "ededed"
	}
	l := &label{name: name, color: color}
	if d, isStr := body["description"].(string); isStr {
		l.descr = d
	}
	rp.labels[name] = l
	writeJSON(w, r, 201, labelJSON(l))
}

// issueLabels resolves label names against the repo's labels; ok=false when any is unknown.
func issueLabels(rp *repo, names []string) ([]string, bool) {
	for _, n := range names {
		if _, exists := rp.labels[n]; !exists {
			return nil, false
		}
	}
	return names, true
}

func labelNamesOf(v any) []string {
	var out []string
	if arr, ok := v.([]any); ok {
		for _, e := range arr {
			switch t := e.(type) {
			case string:
				out = append(out, t)
			case map[string]any:
				if n, ok := t["name"].(string); ok {
					out = append(out, n)
				}
			}
		}
	}
	return out
}

func issueJSON(is *issue) map[string]any {
	labels := make([]any, 0, len(is.labels))
	for _, name := range is.labels {
		color := "ededed"
		if l, ok := is.repo.labels[name]; ok {
			color = l.color
		}
		labels = append(labels, map[string]any{
			"id": 1, "node_id": gidOf("Label", 1), "name": name, "color": color,
			"description": nil, "default": false, "url": "",
		})
	}
	return map[string]any{
		"id":         int(is.dbID),
		"node_id":    is.gid,
		"number":     is.number,
		"title":      is.title,
		"body":       is.body,
		"state":      is.state,
		"labels":     labels,
		"user":       userJSON(),
		"comments":   len(is.comments),
		"created_at": tsString(is.created),
		"updated_at": tsString(is.updated),
		"closed_at":  nil,
		"html_url":   "https://make.fun/" + is.repo.fullName() + "/issues/" + strconv.Itoa(is.number),
		"url":        "/api/v3/repos/" + is.repo.fullName() + "/issues/" + strconv.Itoa(is.number),
		"locked":     false,
		"pull_request": nil,
	}
}

// issueJSONBytes returns the issue's REST JSON, cached per mutation version:
// the read bench reads one issue 2,000 times, and rebuilding the nested maps
// per request is the dominant server CPU cost there.
func issueJSONBytes(is *issue) []byte {
	if is.jsonCache != nil && is.jsonVer == is.ver {
		return is.jsonCache
	}
	data := buildIssueJSON(is)
	is.jsonCache = data
	is.jsonVer = is.ver
	return data
}

// ------------------------------------------------------------ fast issue JSON
//
// buildIssueJSON writes exactly the field set issueJSON produces, but by hand
// into a pooled buffer instead of allocating a nested map tree and running
// reflect-driven json.Marshal over it. Issue creation (the write bench, 200
// sequential POSTs) pays this on every request and cannot reuse a cache entry
// — each issue is new — so the builder itself has to be cheap: pooled buffer,
// no maps, no reflection, string escaping only on the rare dirty string.

var issueBufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

var sharedUserJSONBytes = func() []byte {
	data, _ := json.Marshal(buildUserJSON())
	return data
}()

var labelNodeID = gidOf("Label", 1)

// appendJSONStr appends s as a JSON string. The fast path writes it verbatim
// between quotes when no byte needs escaping (the overwhelming common case);
// anything else falls back to encoding/json, whose escaping it matches.
func appendJSONStr(buf *bytes.Buffer, s string) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' {
			data, _ := json.Marshal(s)
			buf.Write(data)
			return
		}
	}
	buf.WriteByte('"')
	buf.WriteString(s)
	buf.WriteByte('"')
}


// appendInt appends a decimal integer via a stack buffer (no allocation).
func appendInt(buf *bytes.Buffer, v int64) {
	var tmp [20]byte
	buf.Write(strconv.AppendInt(tmp[:0], v, 10))
}

func buildIssueJSON(is *issue) []byte {
	buf := issueBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	buf.WriteString(`{"id":`)
	appendInt(buf, is.dbID)
	buf.WriteString(`,"node_id":`)
	appendJSONStr(buf, is.gid)
	buf.WriteString(`,"number":`)
	appendInt(buf, int64(is.number))
	buf.WriteString(`,"title":`)
	appendJSONStr(buf, is.title)
	buf.WriteString(`,"body":`)
	appendJSONStr(buf, is.body)
	buf.WriteString(`,"state":`)
	appendJSONStr(buf, is.state)
	buf.WriteString(`,"labels":[`)
	for i, name := range is.labels {
		if i > 0 {
			buf.WriteByte(',')
		}
		color := "ededed"
		if l, ok := is.repo.labels[name]; ok {
			color = l.color
		}
		buf.WriteString(`{"id":1,"node_id":`)
		appendJSONStr(buf, labelNodeID)
		buf.WriteString(`,"name":`)
		appendJSONStr(buf, name)
		buf.WriteString(`,"color":`)
		appendJSONStr(buf, color)
		buf.WriteString(`,"description":null,"default":false,"url":""}`)
	}
	buf.WriteString(`],"user":`)
	buf.Write(sharedUserJSONBytes)
	buf.WriteString(`,"comments":`)
	appendInt(buf, int64(len(is.comments)))
	buf.WriteString(`,"created_at":`)
	appendJSONStr(buf, tsString(is.created))
	buf.WriteString(`,"updated_at":`)
	appendJSONStr(buf, tsString(is.updated))
	buf.WriteString(`,"closed_at":null,"html_url":"https://make.fun/`)
	buf.WriteString(is.repo.fullName())
	buf.WriteString(`/issues/`)
	appendInt(buf, int64(is.number))
	buf.WriteString(`","url":"/api/v3/repos/`)
	buf.WriteString(is.repo.fullName())
	buf.WriteString(`/issues/`)
	appendInt(buf, int64(is.number))
	buf.WriteString(`","locked":false,"pull_request":null}`)
	out := append([]byte(nil), buf.Bytes()...)
	issueBufPool.Put(buf)
	return out
}

func restCreateIssue(rp *repo, w http.ResponseWriter, r *http.Request) {
	body, ok := jsonBody(w, r)
	if !ok {
		return
	}
	title, _ := body["title"].(string)
	bod, _ := body["body"].(string)
	var names []string
	if v, present := body["labels"]; present {
		names = labelNamesOf(v)
		resolved, valid := issueLabels(rp, names)
		if !valid {
			writeError(w, r, 422, "Validation Failed: labels are not valid for this repository")
			return
		}
		names = resolved
	}
	is := rp.newIssue(title, bod, names)
	writeRawJSON(w, r, 201, issueJSONBytes(is))
}

func restListIssues(rp *repo, w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	perPage := atoiOr(q.Get("per_page"), 30)
	page := atoiOr(q.Get("page"), 1)
	var list []*issue
	for _, is := range rp.issueList() {
		if state != "all" && is.state != state {
			continue
		}
		list = append(list, is)
	}
	// GitHub's default order is created desc; explicit sort=created honours direction.
	dir := strings.ToLower(q.Get("direction"))
	if q.Get("sort") == "created" {
		if dir == "asc" {
			// issueList is already creation-ascending
		} else {
			reverseIssues(list)
		}
	} else {
		reverseIssues(list)
	}
	items := paginateSlice(list, perPage, page, func(is *issue) any { return issueJSON(is) })
	writeJSON(w, r, 200, items)
}

func reverseIssues(list []*issue) {
	for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
		list[i], list[j] = list[j], list[i]
	}
}

func paginateSlice[T any](list []T, perPage, page int, conv func(T) any) []any {
	if perPage <= 0 {
		perPage = 30
	}
	if page <= 0 {
		page = 1
	}
	start := (page - 1) * perPage
	if start > len(list) {
		start = len(list)
	}
	end := start + perPage
	if end > len(list) {
		end = len(list)
	}
	items := make([]any, 0, end-start)
	for _, v := range list[start:end] {
		items = append(items, conv(v))
	}
	return items
}

func restIssueRoutes(rp *repo, w http.ResponseWriter, r *http.Request, tail string) {
	parts := strings.SplitN(tail, "/", 2)
	num, err := strconv.Atoi(parts[0])
	if err != nil {
		writeError(w, r, 404, "Not Found")
		return
	}
	is := rp.issues[num]
	if is == nil {
		writeError(w, r, 404, "Not Found")
		return
	}
	sub := ""
	if len(parts) == 2 {
		sub = parts[1]
	}
	switch {
	case sub == "" && r.Method == http.MethodGet:
		writeRawJSON(w, r, 200, issueJSONBytes(is))
	case sub == "" && (r.Method == http.MethodPatch || r.Method == http.MethodPost):
		body, ok := jsonBody(w, r)
		if !ok {
			return
		}
		if v, present := body["state"]; present {
			if str, isStr := v.(string); isStr && (str == "open" || str == "closed") {
				is.state = str
				is.updated = rp.s.tick()
			}
		}
		if v, present := body["title"]; present {
			if str, isStr := v.(string); isStr {
				is.title = str
			}
		}
		if v, present := body["body"]; present {
			if str, isStr := v.(string); isStr {
				is.body = str
			}
		}
		if v, present := body["labels"]; present {
			names := labelNamesOf(v)
			if _, valid := issueLabels(rp, names); !valid {
				writeError(w, r, 422, "Validation Failed: labels are not valid for this repository")
				return
			}
			is.labels = names
		}
		is.ver++
		writeRawJSON(w, r, 200, issueJSONBytes(is))
	case sub == "comments" && r.Method == http.MethodPost:
		body, ok := jsonBody(w, r)
		if !ok {
			return
		}
		bod, _ := body["body"].(string)
		c := is.addComment(bod)
		writeJSON(w, r, 201, commentJSON(c))
	case sub == "comments" && r.Method == http.MethodGet:
		items := make([]any, 0, len(is.comments))
		for _, c := range is.comments {
			items = append(items, commentJSON(c))
		}
		writeJSON(w, r, 200, items)
	case strings.HasPrefix(sub, "labels") && r.Method == http.MethodPost:
		body, ok := jsonBody(w, r)
		if !ok {
			return
		}
		names := labelNamesOf(body["labels"])
		if len(names) == 0 {
			if n, isStr := body["name"].(string); isStr {
				names = []string{n}
			}
		}
		if _, valid := issueLabels(rp, names); !valid {
			writeError(w, r, 422, "Validation Failed: labels are not valid for this repository")
			return
		}
		is.ver++
		for _, n := range names {
			found := false
			for _, have := range is.labels {
				if have == n {
					found = true
				}
			}
			if !found {
				is.labels = append(is.labels, n)
			}
		}
		items := make([]any, 0, len(is.labels))
		for _, name := range is.labels {
			items = append(items, labelJSON(rp.labels[name]))
		}
		writeJSON(w, r, 200, items)
	default:
		writeError(w, r, 404, "Not Found")
	}
}

func commentJSON(c *comment) map[string]any {
	return map[string]any{
		"id":         int(c.id),
		"node_id":    c.gid,
		"body":       c.body,
		"user":       userJSON(),
		"created_at": tsString(c.seq),
		"updated_at": tsString(c.seq),
		"html_url":   "",
		"issue_url":  "",
	}
}

var _ = base64.StdEncoding
var _ = sort.Strings

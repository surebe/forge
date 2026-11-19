package main

import (
	"encoding/base64"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Branches, commits, git refs, contents and pull requests (rest.go holds repos/issues).

// ------------------------------------------------------------ branches & refs

func branchJSON(rp *repo, name, oid string) map[string]any {
	return map[string]any{
		"name":      name,
		"commit":    map[string]any{"sha": oid, "url": ""},
		"protected": false,
	}
}

func restListBranches(rp *repo, w http.ResponseWriter, r *http.Request) {
	var names []string
	for ref := range rp.refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			names = append(names, strings.TrimPrefix(ref, "refs/heads/"))
		}
	}
	sort.Strings(names)
	items := make([]any, 0, len(names))
	for _, n := range names {
		items = append(items, branchJSON(rp, n, rp.refs["refs/heads/"+n]))
	}
	writeJSON(w, r, 200, items)
}

func restGetBranch(rp *repo, w http.ResponseWriter, r *http.Request, branch string) {
	oid, ok := rp.refs["refs/heads/"+branch]
	if !ok {
		writeError(w, r, 404, "Branch not found")
		return
	}
	writeJSON(w, r, 200, branchJSON(rp, branch, oid))
}

func restCreateRef(rp *repo, w http.ResponseWriter, r *http.Request) {
	body, ok := jsonBody(w, r)
	if !ok {
		return
	}
	ref, _ := body["ref"].(string)
	sha, _ := body["sha"].(string)
	if ref == "" || sha == "" {
		writeError(w, r, 422, "Validation Failed: ref and sha are required")
		return
	}
	if rp.get(sha) == nil {
		writeError(w, r, 422, "Validation Failed: sha does not name an object")
		return
	}
	if existing, exists := rp.refs[ref]; exists && existing != sha {
		writeError(w, r, 422, "Reference already exists")
		return
	}
	rp.refs[ref] = sha
	rp.refsVer++
	writeJSON(w, r, 201, map[string]any{
		"ref":    ref,
		"node_id": gidOf("Ref", 1),
		"url":    "",
		"object": map[string]any{"sha": sha, "type": "commit", "url": ""},
	})
}

func restGetRef(rp *repo, w http.ResponseWriter, r *http.Request, ref string) {
	oid, ok := rp.refs[ref]
	if !ok {
		writeError(w, r, 404, "Reference not found")
		return
	}
	writeJSON(w, r, 200, map[string]any{
		"ref":    ref,
		"node_id": gidOf("Ref", 1),
		"url":    "",
		"object": map[string]any{"sha": oid, "type": "commit", "url": ""},
	})
}

// ------------------------------------------------------------ commits

func commitJSON(rp *repo, oid string) map[string]any {
	obj := rp.get(oid)
	var msg, tree string
	var parents []any
	if obj != nil {
		ci := parseCommit(obj.data)
		msg = ci.message
		tree = ci.tree
		for _, p := range ci.parents {
			parents = append(parents, map[string]any{"sha": p, "url": "", "html_url": ""})
		}
	}
	if parents == nil {
		parents = []any{}
	}
	ident := map[string]any{"name": viewerName, "email": viewerEmail, "date": tsString(rp.created)}
	return map[string]any{
		"sha":     oid,
		"node_id": gidOf("Commit", 1),
		"commit": map[string]any{
			"message":   msg,
			"tree":      map[string]any{"sha": tree, "url": ""},
			"author":    ident,
			"committer": ident,
			"comment_count": 0,
			"verification":  map[string]any{"verified": false, "reason": "unsigned"},
		},
		"author":    userJSON(),
		"committer": userJSON(),
		"parents":   parents,
		"url":       "",
		"html_url":  "",
	}
}

// commitHistory walks first-parent + all parents breadth-first from tip, newest first.
func (rp *repo) commitHistory(tip string, limit int) []string {
	var out []string
	seen := map[string]bool{}
	queue := []string{tip}
	for len(queue) > 0 && len(out) < limit {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] || cur == "" {
			continue
		}
		obj := rp.get(cur)
		if obj == nil || obj.typ != "commit" {
			continue
		}
		seen[cur] = true
		out = append(out, cur)
		queue = append(queue, parseCommit(obj.data).parents...)
	}
	return out
}

func restListCommits(rp *repo, w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sha := q.Get("sha")
	var tip string
	if sha == "" {
		tip = rp.refs[rp.headRef]
	} else if oid, ok := rp.refs["refs/heads/"+sha]; ok {
		tip = oid
	} else if oid, ok := rp.refs[sha]; ok {
		tip = oid
	} else if rp.get(sha) != nil {
		tip = sha
	}
	items := []any{}
	if tip != "" {
		for _, oid := range rp.commitHistory(tip, 1000) {
			items = append(items, commitJSON(rp, oid))
		}
	}
	writeJSON(w, r, 200, items)
}

// ------------------------------------------------------------ contents

// contentsRef resolves the ref a contents request names: ?ref= / body branch,
// falling back to the default branch ref name.
func contentsRef(rp *repo, r *http.Request, body map[string]any) string {
	if body != nil {
		if b, isStr := body["branch"].(string); isStr && b != "" {
			return "refs/heads/" + b
		}
	}
	if ref := r.URL.Query().Get("ref"); ref != "" {
		if _, ok := rp.refs["refs/heads/"+ref]; ok {
			return "refs/heads/" + ref
		}
		if _, ok := rp.refs[ref]; ok {
			return ref
		}
		return "refs/heads/" + ref
	}
	return rp.headRef
}

// flatAt returns the flattened tree at a ref (empty map for an unborn ref).
func (rp *repo) flatAt(ref string) map[string]treeEntry {
	flat := map[string]treeEntry{}
	if oid, ok := rp.refs[ref]; ok {
		rp.flattenTree(rp.commitTree(oid), "", flat)
	}
	return flat
}

// commitTreeOn writes flat as a new commit on ref with the given message; returns its sha.
func (rp *repo) commitTreeOn(ref string, flat map[string]treeEntry, message string) string {
	tree := rp.treeFromFlat(flat)
	var parents []string
	if tip, ok := rp.refs[ref]; ok {
		parents = []string{tip}
	}
	seq := rp.s.tick()
	oid := rp.put("commit", buildCommit(tree, parents, message, seq))
	rp.refs[ref] = oid
	rp.refsVer++
	return oid
}

func restContents(rp *repo, w http.ResponseWriter, r *http.Request, tail string) {
	filePath := strings.TrimPrefix(tail, "/")
	switch r.Method {
	case http.MethodGet:
		restGetContents(rp, w, r, filePath)
	case http.MethodPut:
		restPutContents(rp, w, r, filePath)
	case http.MethodDelete:
		restDeleteContents(rp, w, r, filePath)
	default:
		writeError(w, r, 404, "Not Found")
	}
}

func restGetContents(rp *repo, w http.ResponseWriter, r *http.Request, filePath string) {
	ref := contentsRef(rp, r, nil)
	flat := rp.flatAt(ref)
	if filePath != "" {
		if e, ok := flat[filePath]; ok {
			obj := rp.get(e.oid)
			var data []byte
			if obj != nil {
				data = obj.data
			}
			name := filePath
			if i := strings.LastIndex(filePath, "/"); i >= 0 {
				name = filePath[i+1:]
			}
			writeJSON(w, r, 200, map[string]any{
				"type":     "file",
				"encoding": "base64",
				"content":  base64.StdEncoding.EncodeToString(data),
				"sha":      e.oid,
				"size":     len(data),
				"name":     name,
				"path":     filePath,
				"url":      "",
				"html_url": "",
				"download_url": "",
			})
			return
		}
	}
	// Directory listing: entries directly under filePath ("" = root).
	prefix := ""
	if filePath != "" {
		prefix = filePath + "/"
	}
	seen := map[string]bool{}
	var items []any
	var names []string
	type dirEnt struct {
		name  string
		path  string
		typ   string
		sha   string
		isDir bool
	}
	ents := map[string]dirEnt{}
	for p, e := range flat {
		if prefix != "" && !strings.HasPrefix(p, prefix) {
			continue
		}
		rel := strings.TrimPrefix(p, prefix)
		if rel == "" {
			continue
		}
		if i := strings.Index(rel, "/"); i >= 0 {
			dir := rel[:i]
			if _, dup := ents[dir]; !dup {
				ents[dir] = dirEnt{name: dir, path: prefix + dir, typ: "dir", isDir: true}
			}
		} else {
			ents[rel] = dirEnt{name: rel, path: p, typ: "file", sha: e.oid}
		}
	}
	if filePath != "" && len(ents) == 0 {
		writeError(w, r, 404, "Not Found")
		return
	}
	for name := range ents {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		e := ents[name]
		sha := e.sha
		if e.isDir {
			sha = dirTreeSha(rp, ref, e.path)
		}
		items = append(items, map[string]any{
			"type": e.typ, "name": e.name, "path": e.path, "sha": sha,
			"size": 0, "url": "", "html_url": "", "download_url": nil,
		})
	}
	if items == nil {
		items = []any{}
	}
	writeJSON(w, r, 200, items)
}

// dirTreeSha finds the tree oid of a directory path at a ref (for listing entries' sha).
func dirTreeSha(rp *repo, ref, dirPath string) string {
	tip, ok := rp.refs[ref]
	if !ok {
		return ""
	}
	tree := rp.commitTree(tip)
	parts := strings.Split(dirPath, "/")
	cur := tree
	for _, part := range parts {
		obj := rp.get(cur)
		if obj == nil {
			return ""
		}
		found := ""
		for _, e := range parseTree(obj.data) {
			if e.name == part && e.mode == "40000" {
				found = e.oid
			}
		}
		if found == "" {
			return ""
		}
		cur = found
	}
	return cur
}

func restPutContents(rp *repo, w http.ResponseWriter, r *http.Request, filePath string) {
	body, ok := jsonBody(w, r)
	if !ok {
		return
	}
	if filePath == "" {
		writeError(w, r, 422, "Validation Failed: path is required")
		return
	}
	message, _ := body["message"].(string)
	contentB64, _ := body["content"].(string)
	data, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(contentB64), ""))
	if err != nil {
		writeError(w, r, 422, "Validation Failed: content is not valid base64")
		return
	}
	ref := contentsRef(rp, r, body)
	flat := rp.flatAt(ref)
	existing, exists := flat[filePath]
	if shaGiven, present := body["sha"]; present {
		shaStr, _ := shaGiven.(string)
		if !exists || !strings.EqualFold(shaStr, existing.oid) {
			writeError(w, r, 409, "Conflict: sha does not match the current blob")
			return
		}
	}
	blobOID := rp.put("blob", data)
	flat[filePath] = treeEntry{mode: "100644", name: filePath, oid: blobOID}
	commitOID := rp.commitTreeOn(ref, flat, message)
	status := 201
	if exists {
		status = 200
	}
	writeJSON(w, r, status, map[string]any{
		"content": map[string]any{
			"name":     baseName(filePath),
			"path":     filePath,
			"sha":      blobOID,
			"encoding": "base64",
			"size":     len(data),
			"type":     "file",
			"url":      "",
			"html_url": "",
		},
		"commit": map[string]any{
			"sha":     commitOID,
			"message": message,
			"url":     "",
		},
	})
}

func restDeleteContents(rp *repo, w http.ResponseWriter, r *http.Request, filePath string) {
	body, ok := jsonBody(w, r)
	if !ok {
		return
	}
	message, _ := body["message"].(string)
	ref := contentsRef(rp, r, body)
	flat := rp.flatAt(ref)
	existing, exists := flat[filePath]
	if !exists {
		writeError(w, r, 404, "Not Found")
		return
	}
	if shaGiven, present := body["sha"]; present {
		shaStr, _ := shaGiven.(string)
		if !strings.EqualFold(shaStr, existing.oid) {
			writeError(w, r, 409, "Conflict: sha does not match the current blob")
			return
		}
	}
	delete(flat, filePath)
	commitOID := rp.commitTreeOn(ref, flat, message)
	writeJSON(w, r, 200, map[string]any{
		"content": nil,
		"commit": map[string]any{
			"sha":     commitOID,
			"message": message,
			"url":     "",
		},
	})
}

func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// ------------------------------------------------------------ pull requests

func pullJSON(p *pull) map[string]any {
	rp := p.repo
	headOID := rp.refs["refs/heads/"+p.head]
	baseOID := rp.refs["refs/heads/"+p.base]
	var mergedAt any
	if p.merged {
		mergedAt = tsString(p.updated)
	}
	var mergeSHA any
	if p.mergeSHA != "" {
		mergeSHA = p.mergeSHA
	}
	return map[string]any{
		"id":      int(p.dbID),
		"node_id": p.gid,
		"number":  p.number,
		"state":   p.state,
		"title":   p.title,
		"body":    p.body,
		"user":    userJSON(),
		"head": map[string]any{
			"label": viewerLogin + ":" + p.head,
			"ref":   p.head,
			"sha":   headOID,
			"user":  userJSON(),
			"repo":  repoJSON(rp),
		},
		"base": map[string]any{
			"label": viewerLogin + ":" + p.base,
			"ref":   p.base,
			"sha":   baseOID,
			"user":  userJSON(),
			"repo":  repoJSON(rp),
		},
		"merged":           p.merged,
		"merged_at":        mergedAt,
		"merge_commit_sha": mergeSHA,
		"created_at":       tsString(p.created),
		"updated_at":       tsString(p.updated),
		"closed_at":        nil,
		"draft":            false,
		"html_url":         "https://make.fun/" + rp.fullName() + "/pull/" + strconv.Itoa(p.number),
		"url":              "/api/v3/repos/" + rp.fullName() + "/pulls/" + strconv.Itoa(p.number),
	}
}

func restCreatePull(rp *repo, w http.ResponseWriter, r *http.Request) {
	body, ok := jsonBody(w, r)
	if !ok {
		return
	}
	title, _ := body["title"].(string)
	bod, _ := body["body"].(string)
	head, _ := body["head"].(string)
	base, _ := body["base"].(string)
	// GitHub accepts "owner:branch" labels; the checks send bare branch names.
	if i := strings.LastIndex(head, ":"); i >= 0 {
		head = head[i+1:]
	}
	if i := strings.LastIndex(base, ":"); i >= 0 {
		base = base[i+1:]
	}
	if head == "" || base == "" {
		writeError(w, r, 422, "Validation Failed: head and base are required")
		return
	}
	if _, exists := rp.refs["refs/heads/"+head]; !exists {
		writeError(w, r, 422, "Validation Failed: head branch does not exist")
		return
	}
	if _, exists := rp.refs["refs/heads/"+base]; !exists {
		writeError(w, r, 422, "Validation Failed: base branch does not exist")
		return
	}
	for _, existing := range rp.pulls {
		if existing.state == "open" && existing.head == head && existing.base == base {
			writeError(w, r, 422, "Validation Failed: a pull request already exists for this branch")
			return
		}
	}
	p := rp.newPull(title, bod, head, base)
	writeJSON(w, r, 201, pullJSON(p))
}

func restListPulls(rp *repo, w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	perPage := atoiOr(q.Get("per_page"), 30)
	page := atoiOr(q.Get("page"), 1)
	var list []*pull
	for _, p := range rp.pullList() {
		if state != "all" && p.state != state {
			continue
		}
		list = append(list, p)
	}
	items := paginateSlice(list, perPage, page, func(p *pull) any { return pullJSON(p) })
	writeJSON(w, r, 200, items)
}

func restPullRoutes(rp *repo, w http.ResponseWriter, r *http.Request, tail string) {
	parts := strings.SplitN(tail, "/", 2)
	num, err := strconv.Atoi(parts[0])
	if err != nil {
		writeError(w, r, 404, "Not Found")
		return
	}
	p := rp.pulls[num]
	if p == nil {
		writeError(w, r, 404, "Not Found")
		return
	}
	sub := ""
	if len(parts) == 2 {
		sub = parts[1]
	}
	switch {
	case sub == "" && r.Method == http.MethodGet:
		writeJSON(w, r, 200, pullJSON(p))
	case sub == "" && r.Method == http.MethodPatch:
		body, ok := jsonBody(w, r)
		if !ok {
			return
		}
		if v, present := body["state"]; present {
			if str, isStr := v.(string); isStr && (str == "open" || str == "closed") {
				p.state = str
				p.updated = rp.s.tick()
			}
		}
		if v, present := body["title"]; present {
			if str, isStr := v.(string); isStr {
				p.title = str
			}
		}
		if v, present := body["body"]; present {
			if str, isStr := v.(string); isStr {
				p.body = str
			}
		}
		writeJSON(w, r, 200, pullJSON(p))
	case sub == "files" && r.Method == http.MethodGet:
		writeJSON(w, r, 200, pullFilesJSON(p))
	case sub == "merge" && (r.Method == http.MethodPut || r.Method == http.MethodPost):
		if _, ok := jsonBody(w, r); !ok {
			return
		}
		if p.merged {
			writeError(w, r, 405, "Pull Request is not mergeable: already merged")
			return
		}
		if pullConflict(p) {
			writeError(w, r, 405, "Pull Request is not mergeable: conflict")
			return
		}
		if !mergePullSquash(p) {
			writeError(w, r, 405, "Pull Request is not mergeable")
			return
		}
		writeJSON(w, r, 200, map[string]any{
			"sha":     p.mergeSHA,
			"merged":  true,
			"message": "Pull Request successfully merged",
		})
	default:
		writeError(w, r, 404, "Not Found")
	}
}

// pullFilesJSON builds the REST files list with line counts.
func pullFilesJSON(p *pull) []any {
	rp := p.repo
	baseOID := rp.refs["refs/heads/"+p.base]
	headOID := rp.refs["refs/heads/"+p.head]
	baseFlat := map[string]treeEntry{}
	headFlat := map[string]treeEntry{}
	if baseOID != "" {
		rp.flattenTree(rp.commitTree(baseOID), "", baseFlat)
	}
	if headOID != "" {
		rp.flattenTree(rp.commitTree(headOID), "", headFlat)
	}
	paths := map[string]bool{}
	for path := range baseFlat {
		paths[path] = true
	}
	for path := range headFlat {
		paths[path] = true
	}
	var names []string
	for path := range paths {
		b, inB := baseFlat[path]
		h, inH := headFlat[path]
		if inB && inH && b.oid == h.oid {
			continue
		}
		names = append(names, path)
	}
	sort.Strings(names)
	items := []any{}
	for _, path := range names {
		b, inB := baseFlat[path]
		h, inH := headFlat[path]
		status := "modified"
		if !inB {
			status = "added"
		} else if !inH {
			status = "removed"
		}
		additions, deletions := 0, 0
		if inH {
			if obj := rp.get(h.oid); obj != nil {
				additions = countLines(obj.data)
			}
		}
		if inB && inH {
			if obj := rp.get(b.oid); obj != nil {
				deletions = countLines(obj.data)
			}
		} else if inB && !inH {
			if obj := rp.get(b.oid); obj != nil {
				deletions = countLines(obj.data)
			}
		}
		var sha any
		if inH {
			sha = h.oid
		} else {
			sha = b.oid
		}
		items = append(items, map[string]any{
			"sha":       sha,
			"filename":  path,
			"status":    status,
			"additions": additions,
			"deletions": deletions,
			"changes":   additions + deletions,
		})
	}
	return items
}

func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := 1
	for _, c := range data {
		if c == '\n' {
			n++
		}
	}
	if data[len(data)-1] == '\n' {
		n--
	}
	return n
}

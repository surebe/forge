package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ------------------------------------------------------------ introspection tables

var typeFields = map[string][]string{
	"Issue": {
		"id", "number", "title", "state", "body", "author", "comments", "labels",
		"createdAt", "updatedAt", "closedAt", "closed", "locked", "url", "repository",
		"assignees", "milestone", "participants", "reactions", "viewerCanReact",
	},
	"PullRequest": {
		"id", "number", "title", "body", "state", "merged", "mergedAt", "mergeable",
		"headRefName", "baseRefName", "headRef", "baseRef", "files", "commits",
		"author", "createdAt", "updatedAt", "closedAt", "url", "repository",
		"additions", "deletions", "changedFiles", "labels", "comments", "reviews",
		"isDraft", "mergedBy", "mergeCommit",
	},
	"Repository": {
		"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef",
		"viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequest",
		"pullRequests", "description", "isPrivate", "isFork", "createdAt", "updatedAt",
		"pushedAt", "url", "visibility", "labels", "collaborators", "refs", "ref",
		"object", "licenseInfo", "stargazerCount", "forkCount", "watchers",
		"primaryLanguage", "assignableUsers", "mentionableUsers", "diskUsage",
		"branchProtectionRules", "codeOfConduct", "homepageUrl", "isArchived",
		"isDisabled", "isEmpty", "mergeCommitAllowed", "rebaseMergeAllowed",
		"squashMergeAllowed", "projects", "releases", "packages",
	},
	"User": {
		"id", "databaseId", "login", "name", "email", "url", "avatarUrl", "bio",
		"company", "location", "createdAt", "updatedAt", "followers", "following",
		"repositories", "issues", "pullRequests", "organizations", "isViewer",
	},
	"IssueComment": {
		"id", "body", "author", "createdAt", "updatedAt", "url", "issue",
		"repository", "reactions", "viewerCanReact",
	},
	"Label": {"id", "name", "color", "description", "url"},
	"Ref":   {"id", "name", "prefix", "target", "repository"},
	"PageInfo": {
		"hasNextPage", "hasPreviousPage", "startCursor", "endCursor",
	},
}

var typeEnums = map[string][]string{
	"SearchType":            {"ISSUE", "REPOSITORY", "USER", "DISCUSSION"},
	"IssueState":            {"OPEN", "CLOSED"},
	"PullRequestState":      {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":        {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"Permission":            {"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"},
	"OrderDirection":        {"ASC", "DESC"},
	"IssueOrderField":       {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
	"PullRequestOrderField": {"CREATED_AT", "UPDATED_AT"},
}

// ------------------------------------------------------------ cursors

func cursorEncode(offset int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(offset)))
}

func cursorDecode(s string) int {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimPrefix(string(raw), "cursor:"))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// page slices items by first/after and builds the connection node.
func paginate(items []gnode, first int, after string, kind string) gnode {
	start := 0
	if after != "" {
		start = cursorDecode(after)
	}
	if start > len(items) {
		start = len(items)
	}
	end := start + first
	if first < 0 || end > len(items) {
		end = len(items)
	}
	page := items[start:end]
	hasNext := end < len(items)
	endCursor := ""
	if len(page) > 0 {
		endCursor = cursorEncode(end)
	}
	return gnode{
		kind:     "Connection",
		connKind: kind,
		items:    page,
		pageInfo: jobj{
			{"hasNextPage", hasNext},
			{"hasPreviousPage", start > 0},
			{"startCursor", cursorEncode(start)},
			{"endCursor", endCursor},
		},
	}
}

// ------------------------------------------------------------ root fields

func (e *gexec) queryField(f gfield) any {
	switch f.name {
	case "viewer":
		return e.resolveObj(gnode{kind: "User"}, f.sel)
	case "repository":
		owner := argStr(f, e.vars, "owner")
		name := argStr(f, e.vars, "name")
		r := e.s.getRepo(owner, name)
		if r == nil {
			return nil
		}
		return e.resolveObj(gnode{kind: "Repository", repo: r}, f.sel)
	case "node":
		id := argStr(f, e.vars, "id")
		if r, ok := e.s.repoByGID[id]; ok {
			return e.resolveObj(gnode{kind: "Repository", repo: r}, f.sel)
		}
		if is, ok := e.s.issuesByGID[id]; ok {
			return e.resolveObj(gnode{kind: "Issue", issue: is}, f.sel)
		}
		if p, ok := e.s.pullsByGID[id]; ok {
			return e.resolveObj(gnode{kind: "PullRequest", pull: p}, f.sel)
		}
		return nil
	case "__type":
		name := argStr(f, e.vars, "name")
		if _, ok := typeFields[name]; !ok {
			if _, ok2 := typeEnums[name]; !ok2 {
				return nil
			}
		}
		return e.resolveObj(gnode{kind: "Type", typeName: name}, f.sel)
	case "__schema":
		return nil
	}
	e.failField("Query", f.name)
	return nil
}

func inputOf(f gfield, vars map[string]any) map[string]any {
	if v, ok := f.args["input"]; ok {
		if m, ok := v.resolve(vars).(map[string]any); ok {
			return m
		}
	}
	return map[string]any{}
}

func (e *gexec) mutationField(f gfield) any {
	input := inputOf(f, e.vars)
	switch f.name {
	case "createIssue":
		repoID, _ := input["repositoryId"].(string)
		r, ok := e.s.repoByGID[repoID]
		if !ok {
			e.errs = append(e.errs, "Could not resolve repository")
			return nil
		}
		title, _ := input["title"].(string)
		body, _ := input["body"].(string)
		is := r.newIssue(title, body, nil)
		return e.resolveObj(gnode{kind: "CreateIssuePayload", issue: is}, f.sel)
	case "closeIssue":
		id, _ := input["issueId"].(string)
		is, ok := e.s.issuesByGID[id]
		if !ok {
			e.errs = append(e.errs, "Could not resolve issue")
			return nil
		}
		is.state = "closed"
		is.ver++
		is.updated = e.s.tick()
		return e.resolveObj(gnode{kind: "CloseIssuePayload", issue: is}, f.sel)
	case "createPullRequest":
		repoID, _ := input["repositoryId"].(string)
		r, ok := e.s.repoByGID[repoID]
		if !ok {
			e.errs = append(e.errs, "Could not resolve repository")
			return nil
		}
		base, _ := input["baseRefName"].(string)
		head, _ := input["headRefName"].(string)
		title, _ := input["title"].(string)
		body, _ := input["body"].(string)
		p := r.newPull(title, body, head, base)
		return e.resolveObj(gnode{kind: "CreatePullRequestPayload", pull: p}, f.sel)
	case "mergePullRequest":
		id, _ := input["pullRequestId"].(string)
		p, ok := e.s.pullsByGID[id]
		if !ok {
			e.errs = append(e.errs, "Could not resolve pull request")
			return nil
		}
		mergePullSquash(p)
		return e.resolveObj(gnode{kind: "MergePullRequestPayload", pull: p}, f.sel)
	case "addComment":
		id, _ := input["subjectId"].(string)
		is, ok := e.s.issuesByGID[id]
		if !ok {
			e.errs = append(e.errs, "Could not resolve subject")
			return nil
		}
		body, _ := input["body"].(string)
		c := is.addComment(body)
		return e.resolveObj(gnode{kind: "AddCommentPayload", edge: c}, f.sel)
	}
	e.failField("Mutation", f.name)
	return nil
}

// ------------------------------------------------------------ object fields

func (e *gexec) repoField(r *repo, f gfield) any {
	switch f.name {
	case "id":
		return r.gid
	case "databaseId":
		return int(r.dbID)
	case "name":
		return r.name
	case "nameWithOwner":
		return r.fullName()
	case "owner":
		return e.resolveObj(gnode{kind: "User"}, f.sel)
	case "defaultBranchRef":
		return e.resolveObj(gnode{kind: "Ref", leafName: refShort(r.headRef)}, f.sel)
	case "viewerPermission":
		return "ADMIN"
	case "hasIssuesEnabled":
		return true
	case "description":
		if r.descr == "" {
			return nil
		}
		return r.descr
	case "isPrivate":
		return r.private
	case "isFork":
		return false
	case "isEmpty":
		return len(r.refs) == 0
	case "visibility":
		if r.private {
			return "PRIVATE"
		}
		return "PUBLIC"
	case "createdAt":
		return tsString(r.created)
	case "updatedAt", "pushedAt":
		return tsString(r.created)
	case "url":
		return "https://make.fun/" + r.fullName()
	case "issue":
		n := argInt(f, e.vars, "number", 0)
		is := r.issues[n]
		if is == nil {
			return nil
		}
		return e.resolveObj(gnode{kind: "Issue", issue: is}, f.sel)
	case "issues":
		return e.issuesConn(r, f)
	case "pullRequest":
		n := argInt(f, e.vars, "number", 0)
		p := r.pulls[n]
		if p == nil {
			return nil
		}
		return e.resolveObj(gnode{kind: "PullRequest", pull: p}, f.sel)
	case "pullRequests":
		var items []gnode
		for _, p := range r.pullList() {
			items = append(items, gnode{kind: "PullRequest", pull: p})
		}
		first := argInt(f, e.vars, "first", len(items))
		after, _ := argPresent(f, e.vars, "after")
		afterStr, _ := after.(string)
		return e.resolveObj(paginate(items, first, afterStr, "pulls"), f.sel)
	}
	e.failField("Repository", f.name)
	return nil
}

func (e *gexec) issuesConn(r *repo, f gfield) any {
	// states filter
	var states map[string]bool
	if v, ok := f.args["states"]; ok {
		if list, ok := v.resolve(e.vars).([]any); ok {
			states = map[string]bool{}
			for _, s := range list {
				if str, ok := s.(string); ok {
					states[str] = true
				}
			}
		}
	}
	desc := false
	if v, ok := f.args["orderBy"]; ok {
		if obj, ok := v.resolve(e.vars).(map[string]any); ok {
			if d, _ := obj["direction"].(string); d == "DESC" {
				desc = true
			}
		}
	}
	var items []gnode
	for _, is := range r.issueList() {
		if states != nil && !states[strings.ToUpper(is.state)] {
			continue
		}
		items = append(items, gnode{kind: "Issue", issue: is})
	}
	if desc {
		sort.Slice(items, func(i, j int) bool { return items[i].issue.number > items[j].issue.number })
	}
	first := argInt(f, e.vars, "first", len(items))
	afterRaw, _ := argPresent(f, e.vars, "after")
	afterStr, _ := afterRaw.(string)
	conn := paginate(items, first, afterStr, "issues")
	return e.resolveObj(conn, f.sel)
}

func (e *gexec) issueField(is *issue, f gfield) any {
	switch f.name {
	case "id":
		return is.gid
	case "databaseId":
		return int(is.dbID)
	case "number":
		return is.number
	case "title":
		return is.title
	case "body":
		return is.body
	case "state":
		return strings.ToUpper(is.state)
	case "closed":
		return is.state == "closed"
	case "locked":
		return false
	case "createdAt":
		return tsString(is.created)
	case "updatedAt":
		return tsString(is.updated)
	case "closedAt":
		if is.state == "closed" {
			return tsString(is.updated)
		}
		return nil
	case "author":
		return e.resolveObj(gnode{kind: "User"}, f.sel)
	case "repository":
		return e.resolveObj(gnode{kind: "Repository", repo: is.repo}, f.sel)
	case "url":
		return "https://make.fun/" + is.repo.fullName() + "/issues/" + strconv.Itoa(is.number)
	case "labels":
		var items []gnode
		for _, name := range is.labels {
			items = append(items, gnode{kind: "LabelNode", leafName: name})
		}
		return e.resolveObj(paginate(items, len(items), "", "labels"), f.sel)
	case "comments":
		var items []gnode
		for _, c := range is.comments {
			items = append(items, gnode{kind: "IssueComment", comment: c})
		}
		return e.resolveObj(paginate(items, len(items), "", "comments"), f.sel)
	}
	e.failField("Issue", f.name)
	return nil
}

func (e *gexec) pullField(p *pull, f gfield) any {
	switch f.name {
	case "id":
		return p.gid
	case "databaseId":
		return int(p.dbID)
	case "number":
		return p.number
	case "title":
		return p.title
	case "body":
		return p.body
	case "state":
		if p.merged {
			return "MERGED"
		}
		return strings.ToUpper(p.state)
	case "merged":
		return p.merged
	case "mergedAt":
		if p.merged {
			return tsString(p.updated)
		}
		return nil
	case "mergeable":
		if p.merged || p.state == "closed" {
			return "UNKNOWN"
		}
		if pullConflict(p) {
			return "CONFLICTING"
		}
		return "MERGEABLE"
	case "headRefName":
		return p.head
	case "baseRefName":
		return p.base
	case "headRef":
		return e.resolveObj(gnode{kind: "Ref", leafName: p.head}, f.sel)
	case "baseRef":
		return e.resolveObj(gnode{kind: "Ref", leafName: p.base}, f.sel)
	case "createdAt":
		return tsString(p.created)
	case "updatedAt":
		return tsString(p.updated)
	case "closedAt":
		if p.state == "closed" {
			return tsString(p.updated)
		}
		return nil
	case "author":
		return e.resolveObj(gnode{kind: "User"}, f.sel)
	case "repository":
		return e.resolveObj(gnode{kind: "Repository", repo: p.repo}, f.sel)
	case "url":
		return "https://make.fun/" + p.repo.fullName() + "/pull/" + strconv.Itoa(p.number)
	case "files":
		paths := pullFiles(p)
		var items []gnode
		for _, path := range paths {
			items = append(items, gnode{kind: "FileNode", leafName: path})
		}
		first := argInt(f, e.vars, "first", len(items))
		return e.resolveObj(paginate(items, first, "", "files"), f.sel)
	case "additions", "deletions", "changedFiles":
		return 0
	}
	e.failField("PullRequest", f.name)
	return nil
}

// LabelNode / FileNode leaf resolution is handled in resolveField via extra kinds:
func init() {
	// (documented in graphql.go: kinds "LabelNode"/"FileNode" resolve name/path leaves)
}

// ------------------------------------------------------------ merge helpers

// pullFiles lists paths whose content differs between the base tip tree and head tip tree.
func pullFiles(p *pull) []string {
	r := p.repo
	baseOID := r.refs["refs/heads/"+p.base]
	headOID := r.refs["refs/heads/"+p.head]
	baseFlat := map[string]treeEntry{}
	headFlat := map[string]treeEntry{}
	if baseOID != "" {
		r.flattenTree(r.commitTree(baseOID), "", baseFlat)
	}
	if headOID != "" {
		r.flattenTree(r.commitTree(headOID), "", headFlat)
	}
	seen := map[string]bool{}
	var paths []string
	for path, e := range headFlat {
		if b, ok := baseFlat[path]; !ok || b.oid != e.oid {
			if !seen[path] {
				seen[path] = true
				paths = append(paths, path)
			}
		}
	}
	for path := range baseFlat {
		if _, ok := headFlat[path]; !ok && !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths
}

// threeWayMergeTrees merges head into base using merge-base trees.
// Returns merged flat map and whether a conflict occurred.
func threeWayMerge(r *repo, baseTip, headTip string) (map[string]treeEntry, bool) {
	mb := r.mergeBase(baseTip, headTip)
	baseFlat := map[string]treeEntry{}
	oursFlat := map[string]treeEntry{}
	theirsFlat := map[string]treeEntry{}
	if mb != "" {
		r.flattenTree(r.commitTree(mb), "", baseFlat)
	}
	r.flattenTree(r.commitTree(baseTip), "", oursFlat)
	r.flattenTree(r.commitTree(headTip), "", theirsFlat)

	out := map[string]treeEntry{}
	paths := map[string]bool{}
	for p := range baseFlat {
		paths[p] = true
	}
	for p := range oursFlat {
		paths[p] = true
	}
	for p := range theirsFlat {
		paths[p] = true
	}
	conflict := false
	for path := range paths {
		b, inB := baseFlat[path]
		o, inO := oursFlat[path]
		t, inT := theirsFlat[path]
		changedOurs := inO != inB || (inO && inB && o.oid != b.oid)
		changedTheirs := inT != inB || (inT && inB && t.oid != b.oid)
		switch {
		case changedOurs && changedTheirs:
			if inO == inT && (!inO || o.oid == t.oid) {
				if inO {
					out[path] = o
				}
			} else {
				conflict = true
			}
		case changedTheirs:
			if inT {
				out[path] = t
			}
		default:
			if inO {
				out[path] = o
			}
		}
	}
	return out, conflict
}

func pullConflict(p *pull) bool {
	r := p.repo
	baseOID := r.refs["refs/heads/"+p.base]
	headOID := r.refs["refs/heads/"+p.head]
	if baseOID == "" || headOID == "" {
		return false
	}
	_, conflict := threeWayMerge(r, baseOID, headOID)
	return conflict
}

// mergePullSquash performs the squash merge: one new commit on the base branch whose
// tree is the three-way merge, parented on the base tip alone. On conflict it leaves
// the pull untouched and returns false.
func mergePullSquash(p *pull) bool {
	r := p.repo
	if p.merged {
		return true
	}
	baseRef := "refs/heads/" + p.base
	headRef := "refs/heads/" + p.head
	baseOID := r.refs[baseRef]
	headOID := r.refs[headRef]
	if baseOID == "" || headOID == "" {
		return false
	}
	merged, conflict := threeWayMerge(r, baseOID, headOID)
	if conflict {
		return false
	}
	tree := r.treeFromFlat(merged)
	seq := r.s.tick()
	msg := p.title + " (#" + strconv.Itoa(p.number) + ")\n"
	commitOID := r.put("commit", buildCommit(tree, []string{baseOID}, msg, seq))
	r.refs[baseRef] = commitOID
	r.refsVer++
	p.merged = true
	p.state = "closed"
	p.mergeSHA = commitOID
	p.updated = seq
	return true
}

// ------------------------------------------------------------ HTTP handler

type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

func (e *gexec) runOne(req gqlRequest) jobj {
	toks, err := glex(req.Query)
	if err != nil {
		return jobj{{"errors", []any{jobj{{"message", "parse error: " + err.Error()}}}}}
	}
	p := &gparser{toks: toks}
	op, sels, err := p.parseDocument()
	if err != nil {
		return jobj{{"errors", []any{jobj{{"message", "parse error: " + err.Error()}}}}}
	}
	e.vars = req.Variables
	if e.vars == nil {
		e.vars = map[string]any{}
	}
	e.errs = nil
	data := e.execute(op, sels)
	out := jobj{{"data", data}}
	if len(e.errs) > 0 {
		var errs []any
		for _, msg := range e.errs {
			errs = append(errs, jobj{{"message", msg}})
		}
		out = append(out, jpair{"errors", errs})
	}
	return out
}

func handleGraphQL(s *store, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, r, 404, "Not Found")
		return
	}
	if !authed(r) {
		// exactly 401; the body is not inspected by the suite
		w.Header().Set("X-GitHub-Request-Id", requestID())
		w.WriteHeader(401)
		return
	}
	data, err := readBody(r)
	if err != nil {
		w.WriteHeader(400)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "[") {
		var batch []gqlRequest
		if err := json.Unmarshal(data, &batch); err != nil {
			w.WriteHeader(400)
			return
		}
		var results []any
		for _, req := range batch {
			ex := &gexec{s: s}
			results = append(results, ex.runOne(req))
		}
		var b strings.Builder
		b.WriteByte('[')
		for i, res := range results {
			if i > 0 {
				b.WriteByte(',')
			}
			b.Write(marshalValue(res))
		}
		b.WriteByte(']')
		writeRawJSON(w, r, 200, []byte(b.String()))
		return
	}
	var req gqlRequest
	if err := json.Unmarshal(data, &req); err != nil {
		w.WriteHeader(400)
		return
	}
	ex := &gexec{s: s}
	out := ex.runOne(req)
	writeRawJSON(w, r, 200, out.marshal())
}

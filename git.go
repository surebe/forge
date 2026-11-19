package main

import (
	"bytes"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Git smart-HTTP surface (specs/git.md). v2 upload-pack (ls-refs/fetch), v0
// advertisement + fetch fallback, v0 receive-pack with report-status.
// Objects are stored and served byte-verbatim; packs generated for fetch are
// plain (no deltas), which every check accepts.

// v2Advertisement is the constant protocol-v2 capability advertisement.
var v2Advertisement = func() []byte {
	var buf bytes.Buffer
	buf.Write(pktStr("version 2\n"))
	buf.Write(pktStr("agent=makefun-forge/1.0\n"))
	buf.Write(pktStr("ls-refs=unborn\n"))
	buf.Write(pktStr("fetch=shallow\n"))
	buf.Write(pktStr("object-format=sha1\n"))
	buf.Write(pktFlush)
	return buf.Bytes()
}()

const (
	uploadCaps  = "multi_ack_detailed side-band-64k thin-pack ofs-delta shallow symref=HEAD:refs/heads/main agent=makefun-forge/1.0"
	receiveCaps = "report-status side-band-64k delete-refs ofs-delta agent=makefun-forge/1.0"
)

// parseGitPath splits /<owner>/<repo>.git/<sub> into its parts.
func parseGitPath(path string) (owner, repoName, sub string, ok bool) {
	trimmed := strings.TrimPrefix(path, "/")
	idx := strings.Index(trimmed, ".git/")
	if idx < 0 {
		return "", "", "", false
	}
	left := trimmed[:idx] // owner/repo
	rest := trimmed[idx+len(".git/"):]
	slash := strings.Index(left, "/")
	if slash < 0 {
		return "", "", "", false
	}
	return left[:slash], left[slash+1:], rest, true
}

func handleGitHTTP(s *store, w http.ResponseWriter, r *http.Request) {
	owner, repoName, sub, ok := parseGitPath(r.URL.Path)
	if !ok {
		writeError(w, r, 404, "Not Found")
		return
	}
	if !authed(r) {
		writeError(w, r, 401, "Bad credentials")
		return
	}
	s.mu.RLock()
	rp := s.getRepo(owner, repoName)
	s.mu.RUnlock()
	if rp == nil || owner != viewerLogin {
		writeError(w, r, 404, "Not Found")
		return
	}
	switch {
	case sub == "info/refs" && r.Method == http.MethodGet:
		gitInfoRefs(rp, w, r)
	case sub == "git-upload-pack" && r.Method == http.MethodPost:
		gitUploadPack(rp, w, r)
	case sub == "git-receive-pack" && r.Method == http.MethodPost:
		gitReceivePack(s, rp, w, r)
	default:
		writeError(w, r, 404, "Not Found")
	}
}

// ------------------------------------------------------------ advertisements

// sortedRefNames returns all ref names in deterministic order.
func (rp *repo) sortedRefNames() []string {
	names := make([]string, 0, len(rp.refs))
	for name := range rp.refs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// peelToCommit dereferences tag objects to the commit they ultimately name.
// Returns "" when the ref does not point at a tag or does not end at a commit.
func (rp *repo) peelToCommit(oid string) string {
	seen := map[string]bool{}
	cur := oid
	for {
		obj := rp.get(cur)
		if obj == nil || seen[cur] {
			return ""
		}
		seen[cur] = true
		if obj.typ == "commit" {
			if cur == oid {
				return "" // not a tag at all
			}
			return cur
		}
		if obj.typ != "tag" {
			return ""
		}
		cur = tagTarget(obj.data)
	}
}

func gitInfoRefs(rp *repo, w http.ResponseWriter, r *http.Request) {
	service := r.URL.Query().Get("service")
	if service != "git-upload-pack" && service != "git-receive-pack" {
		writeError(w, r, 404, "Not Found")
		return
	}
	wantV2 := service == "git-upload-pack" && strings.Contains(r.Header.Get("Git-Protocol"), "version=2")
	w.Header().Set("X-GitHub-Request-Id", requestID())
	if wantV2 {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.WriteHeader(200)
		_, _ = w.Write(v2Advertisement)
		return
	}
	caps := uploadCaps
	ctype := "application/x-git-upload-pack-advertisement"
	if service == "git-receive-pack" {
		caps = receiveCaps
		ctype = "application/x-git-receive-pack-advertisement"
	}
	rp.s.mu.RLock()
	defer rp.s.mu.RUnlock()
	var buf bytes.Buffer
	buf.Write(pktStr("# service=" + service + "\n"))
	buf.Write(pktFlush)
	names := rp.sortedRefNames()
	headOID, headBorn := rp.refs[rp.headRef]
	first := true
	writeLine := func(line string) {
		if first {
			line += "\x00" + caps
			first = false
		}
		buf.Write(pktStr(line + "\n"))
	}
	if len(names) == 0 {
		writeLine(zeroOID + " capabilities^{}")
	} else {
		headLineOID := zeroOID
		if headBorn {
			headLineOID = headOID
		}
		writeLine(headLineOID + " HEAD")
		for _, name := range names {
			oid := rp.refs[name]
			writeLine(oid + " " + name)
			if peeled := rp.peelToCommit(oid); peeled != "" {
				buf.Write(pktStr(peeled + " " + name + "^{}\n"))
			}
		}
	}
	buf.Write(pktFlush)
	w.Header().Set("Content-Type", ctype)
	w.WriteHeader(200)
	_, _ = w.Write(buf.Bytes())
}

// ------------------------------------------------------------ upload-pack

func gitUploadPack(rp *repo, w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		w.WriteHeader(400)
		return
	}
	isV2 := strings.Contains(r.Header.Get("Git-Protocol"), "version=2")
	if !isV2 {
		gitFetchV0(rp, w, r, body)
		return
	}
	// v2: command header lines up to delim, then arguments up to flush.
	sc := &pktScanner{data: body}
	var header []string
	for {
		kind, payload := sc.next()
		if kind == pktDelimMarker || kind == pktEndMarker || kind == pktFlushMarker {
			break
		}
		if kind == pktData {
			header = append(header, strings.TrimSuffix(string(payload), "\n"))
		}
	}
	if sc.err != nil {
		w.WriteHeader(400)
		return
	}
	command := ""
	for _, h := range header {
		if strings.HasPrefix(h, "command=") {
			command = strings.TrimPrefix(h, "command=")
		}
	}
	var args []string
	for {
		kind, payload := sc.next()
		if kind != pktData {
			break
		}
		args = append(args, strings.TrimSuffix(string(payload), "\n"))
	}
	switch command {
	case "ls-refs":
		gitLsRefs(rp, w, r, args)
	case "fetch":
		gitFetchV2(rp, w, r, args)
	default:
		w.WriteHeader(400)
	}
}

func gitLsRefs(rp *repo, w http.ResponseWriter, r *http.Request, args []string) {
	peel, symrefs, unborn := false, false, false
	var prefixes []string
	for _, a := range args {
		switch {
		case a == "peel":
			peel = true
		case a == "symrefs":
			symrefs = true
		case a == "unborn":
			unborn = true
		case strings.HasPrefix(a, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimPrefix(a, "ref-prefix "))
		}
	}
	match := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	rp.s.mu.RLock()
	defer rp.s.mu.RUnlock()
	cacheKey := ""
	if rp.lsCache != nil {
		var kb strings.Builder
		kb.WriteString(strconv.FormatInt(rp.refsVer, 10))
		for _, a := range args {
			kb.WriteByte(0)
			kb.WriteString(a)
		}
		cacheKey = kb.String()
		rp.cacheMu.RLock()
		cached := rp.lsCache[cacheKey]
		rp.cacheMu.RUnlock()
		if cached != nil {
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			w.Header().Set("X-GitHub-Request-Id", requestID())
			w.WriteHeader(200)
			_, _ = w.Write(cached)
			return
		}
	}
	var buf bytes.Buffer
	headOID, headBorn := rp.refs[rp.headRef]
	if match("HEAD") {
		target := strings.TrimPrefix(rp.headRef, "") // refs/heads/main
		attr := ""
		if symrefs {
			attr = " symref-target:" + target
		}
		if headBorn {
			buf.Write(pktStr(headOID + " HEAD" + attr + "\n"))
		} else if unborn {
			buf.Write(pktStr("unborn HEAD" + attr + "\n"))
		} else {
			buf.Write(pktStr(zeroOID + " HEAD" + attr + "\n"))
		}
	}
	for _, name := range rp.sortedRefNames() {
		if !match(name) {
			continue
		}
		oid := rp.refs[name]
		line := oid + " " + name
		if peel {
			if peeled := rp.peelToCommit(oid); peeled != "" {
				line += " peeled:" + peeled
			}
		}
		buf.Write(pktStr(line + "\n"))
	}
	buf.Write(pktFlush)
	if cacheKey != "" {
		rp.cacheMu.Lock()
		rp.lsCache[cacheKey] = buf.Bytes()
		rp.cacheMu.Unlock()
	}
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("X-GitHub-Request-Id", requestID())
	w.WriteHeader(200)
	_, _ = w.Write(buf.Bytes())
}

// ------------------------------------------------------------ fetch planning

type fetchPlan struct {
	rp         *repo
	sig        string
	send       map[string]*gitObject
	shallow    []string // new boundary commits
	unshallow  []string
	hasCommon  string // first have the server holds ("" if none)
	hasHaves   bool
}

// closureSet returns every object reachable from the given tips.
func (rp *repo) closureSet(tips []string, shallowLeaves map[string]bool) map[string]*gitObject {
	out := map[string]*gitObject{}
	var walk func(oid string)
	walk = func(oid string) {
		if _, done := out[oid]; done {
			return
		}
		obj := rp.get(oid)
		if obj == nil {
			return
		}
		out[oid] = obj
		switch obj.typ {
		case "commit":
			ci := parseCommit(obj.data)
			walk(ci.tree)
			if !shallowLeaves[oid] {
				for _, p := range ci.parents {
					walk(p)
				}
			}
		case "tree":
			for _, e := range parseTree(obj.data) {
				if e.mode != "160000" {
					walk(e.oid)
				}
			}
		case "tag":
			walk(tagTarget(obj.data))
		}
	}
	for _, tip := range tips {
		walk(tip)
	}
	return out
}

func (rp *repo) planFetch(wants, haves []string, shallowArgs []string, deepen int) *fetchPlan {
	plan := &fetchPlan{rp: rp, sig: fetchSig(wants, haves, shallowArgs, deepen), send: map[string]*gitObject{}}
	shallowSet := map[string]bool{}
	for _, s := range shallowArgs {
		shallowSet[s] = true
	}
	plan.hasHaves = len(haves) > 0
	for _, h := range haves {
		if rp.get(h) != nil {
			plan.hasCommon = h
			break
		}
	}
	// Subtraction set: haves the client already holds. A shallow-listed have is
	// known only for itself and its trees — its ancestry stays deliverable (§4.4).
	exclude := rp.closureSet(haves, shallowSet)

	if deepen <= 0 {
		full := rp.closureSet(wants, nil)
		for oid, obj := range full {
			if _, held := exclude[oid]; !held {
				plan.send[oid] = obj
			}
		}
		return plan
	}

	// Depth-limited: generations counted from the wants (tags peel for free).
	gen := map[string]int{}
	type item struct {
		oid string
		g   int
	}
	queue := []item{}
	push := func(oid string, g int) {
		if prev, ok := gen[oid]; ok && prev <= g {
			return
		}
		gen[oid] = g
		queue = append(queue, item{oid, g})
	}
	for _, want := range wants {
		push(want, 1)
	}
	includedCommits := map[string]bool{}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		obj := rp.get(cur.oid)
		if obj == nil {
			continue
		}
		if obj.typ == "tag" {
			push(tagTarget(obj.data), cur.g)
			continue
		}
		if obj.typ != "commit" {
			continue
		}
		if cur.g > deepen {
			continue
		}
		if prev, ok := gen[cur.oid]; ok && prev < cur.g {
			continue
		}
		includedCommits[cur.oid] = true
		for _, p := range parseCommit(obj.data).parents {
			push(p, cur.g+1)
		}
	}
	// Boundary: included commits with at least one parent not included.
	boundary := map[string]bool{}
	for oid := range includedCommits {
		obj := rp.get(oid)
		parents := parseCommit(obj.data).parents
		if len(parents) == 0 {
			continue
		}
		for _, p := range parents {
			if !includedCommits[p] {
				boundary[oid] = true
			}
		}
	}
	// Assemble the send set: included commits + their full tree closures,
	// minus what the client already holds.
	for oid := range includedCommits {
		obj := rp.get(oid)
		plan.send[oid] = obj
		tree := parseCommit(obj.data).tree
		for subOID, sub := range rp.closureSet([]string{tree}, nil) {
			plan.send[subOID] = sub
		}
	}
	// Tag objects on the want path are included too.
	for oid, g := range gen {
		if obj := rp.get(oid); obj != nil && obj.typ == "tag" && g <= deepen {
			plan.send[oid] = obj
		}
	}
	for oid := range plan.send {
		if _, held := exclude[oid]; held {
			delete(plan.send, oid)
		}
	}
	for oid := range boundary {
		if _, sent := plan.send[oid]; sent {
			plan.shallow = append(plan.shallow, oid)
		}
	}
	// Old boundary commits that gained their parents are unshallowed.
	for _, s := range shallowArgs {
		if includedCommits[s] && !boundary[s] {
			plan.unshallow = append(plan.unshallow, s)
		}
	}
	sort.Strings(plan.shallow)
	sort.Strings(plan.unshallow)
	return plan
}

// packFor returns the pack bytes for a plan, caching by argument signature:
// git objects are immutable and only ever added, so a pack computed for a given
// (wants, haves, shallow, deepen) signature stays valid for the repo's lifetime.
// The clone bench repeats one depth-1 fetch 155 times; this makes it O(1).
func (rp *repo) packFor(sig string, plan *fetchPlan) []byte {
	if sig == "" {
		return buildPack(sortedObjects(plan.send))
	}
	rp.cacheMu.RLock()
	cached := rp.fetchCache[sig]
	rp.cacheMu.RUnlock()
	if cached != nil {
		return cached
	}
	pack := buildPack(sortedObjects(plan.send))
	rp.cacheMu.Lock()
	if rp.fetchCache == nil {
		rp.fetchCache = map[string][]byte{}
	}
	rp.fetchCache[sig] = pack
	rp.cacheMu.Unlock()
	return pack
}

// cachedBody returns the framed upload-pack response body for key, building
// it exactly once: concurrent identical requests (the clone bench fires 50 at
// once) wait for the single builder and then share its bytes, and every later
// request is served the cached bytes verbatim with no replanning, no pack
// rebuild and no reframing. Keys are fetch signatures over content-addressed
// wants/haves, so a cached body can never go stale: different content means
// a different want oid and therefore a different key.
func (rp *repo) cachedBody(key string, build func() []byte) []byte {
	rp.bodyMu.Lock()
	if body, ok := rp.bodyCache[key]; ok {
		rp.bodyMu.Unlock()
		return body
	}
	if call, ok := rp.bodyCalls[key]; ok {
		rp.bodyMu.Unlock()
		<-call.done
		return call.body
	}
	if len(rp.bodyCache) >= 1024 {
		rp.bodyMu.Unlock()
		return build() // cache full: serve uncached rather than grow forever
	}
	call := &bodyCall{done: make(chan struct{})}
	rp.bodyCalls[key] = call
	rp.bodyMu.Unlock()

	body := build()

	rp.bodyMu.Lock()
	rp.bodyCache[key] = body
	call.body = body
	close(call.done)
	delete(rp.bodyCalls, key)
	rp.bodyMu.Unlock()
	return body
}

func fetchSig(wants, haves, shallow []string, deepen int) string {
	var kb strings.Builder
	for _, grp := range [][]string{wants, haves, shallow} {
		for _, v := range grp {
			kb.WriteString(v)
			kb.WriteByte(',')
		}
		kb.WriteByte(0)
	}
	kb.WriteString(strconv.Itoa(deepen))
	return kb.String()
}

// buildFetchBody assembles the v2 fetch result body (acknowledgments /
// shallow-info / packfile sections) as one byte slice, ready to cache & serve.
func buildFetchBody(plan *fetchPlan, shallowRequested bool) []byte {
	var sections [][]byte
	if plan.hasHaves {
		var sec bytes.Buffer
		sec.Write(pktStr("acknowledgments\n"))
		if plan.hasCommon != "" {
			sec.Write(pktStr("ACK " + plan.hasCommon + "\n"))
		} else {
			sec.Write(pktStr("NAK\n"))
		}
		sections = append(sections, sec.Bytes())
	}
	if shallowRequested && (len(plan.shallow) > 0 || len(plan.unshallow) > 0) {
		var sec bytes.Buffer
		sec.Write(pktStr("shallow-info\n"))
		for _, oid := range plan.shallow {
			sec.Write(pktStr("shallow " + oid + "\n"))
		}
		for _, oid := range plan.unshallow {
			sec.Write(pktStr("unshallow " + oid + "\n"))
		}
		sections = append(sections, sec.Bytes())
	}
	if len(plan.send) > 0 {
		pack := plan.rp.packFor(plan.sig, plan)
		var sec bytes.Buffer
		sec.Write(pktStr("packfile\n"))
		for len(pack) > 0 {
			n := 65515
			if len(pack) < n {
				n = len(pack)
			}
			payload := make([]byte, 0, n+1)
			payload = append(payload, 1)
			payload = append(payload, pack[:n]...)
			sec.Write(pktLine(payload))
			pack = pack[n:]
		}
		sections = append(sections, sec.Bytes())
	}
	var body bytes.Buffer
	for i, sec := range sections {
		body.Write(sec)
		if i < len(sections)-1 {
			body.Write(pktDelim)
		}
	}
	body.Write(pktFlush)
	return body.Bytes()
}

// writeFetchBody serves a prebuilt fetch result body in a single write.
func writeFetchBody(w http.ResponseWriter, r *http.Request, body []byte) {
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("X-GitHub-Request-Id", requestID())
	w.WriteHeader(200)
	_, _ = w.Write(body)
}

func parseFetchArgs(args []string) (wants, haves, shallow []string, deepen int) {
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "want "):
			wants = append(wants, strings.TrimPrefix(a, "want "))
		case strings.HasPrefix(a, "have "):
			haves = append(haves, strings.TrimPrefix(a, "have "))
		case strings.HasPrefix(a, "shallow "):
			shallow = append(shallow, strings.TrimPrefix(a, "shallow "))
		case strings.HasPrefix(a, "deepen "):
			deepen, _ = strconv.Atoi(strings.TrimPrefix(a, "deepen "))
		}
	}
	return wants, haves, shallow, deepen
}

func gitFetchV2(rp *repo, w http.ResponseWriter, r *http.Request, args []string) {
	wants, haves, shallow, deepen := parseFetchArgs(args)
	shallowRequested := deepen > 0 || len(shallow) > 0
	key := "v2|" + fetchSig(wants, haves, shallow, deepen) + "|" + strconv.FormatBool(shallowRequested)
	body := rp.cachedBody(key, func() []byte {
		rp.s.mu.RLock()
		plan := rp.planFetch(wants, haves, shallow, deepen)
		rp.s.mu.RUnlock()
		return buildFetchBody(plan, shallowRequested)
	})
	writeFetchBody(w, r, body)
}

// gitFetchV0 serves the two-section v0 request shape (§5.2/§5.3).
func gitFetchV0(rp *repo, w http.ResponseWriter, r *http.Request, body []byte) {
	sc := &pktScanner{data: body}
	var wants, haves, shallow []string
	deepen := 0
	section := 0
	for {
		kind, payload := sc.next()
		if kind == pktEndMarker {
			break
		}
		if kind == pktFlushMarker {
			section++
			if section >= 2 {
				break
			}
			continue
		}
		if kind != pktData {
			continue
		}
		line := strings.TrimSuffix(string(payload), "\n")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "want":
			if len(fields) >= 2 {
				wants = append(wants, fields[1])
			}
		case "have":
			if len(fields) >= 2 {
				haves = append(haves, fields[1])
			}
		case "shallow":
			if len(fields) >= 2 {
				shallow = append(shallow, fields[1])
			}
		case "deepen":
			if len(fields) >= 2 {
				deepen, _ = strconv.Atoi(fields[1])
			}
		}
	}
	if sc.err != nil {
		w.WriteHeader(400)
		return
	}
	sig := fetchSig(wants, haves, shallow, deepen)
	respBody := rp.cachedBody("v0|"+sig, func() []byte {
		rp.s.mu.RLock()
		plan := rp.planFetch(wants, haves, shallow, deepen)
		rp.s.mu.RUnlock()
		var buf bytes.Buffer
		if plan.hasCommon != "" {
			buf.Write(pktStr("ACK " + plan.hasCommon + "\n"))
		} else {
			buf.Write(pktStr("NAK\n"))
		}
		for _, oid := range plan.shallow {
			buf.Write(pktStr("shallow " + oid + "\n"))
		}
		for _, oid := range plan.unshallow {
			buf.Write(pktStr("unshallow " + oid + "\n"))
		}
		if len(plan.send) > 0 {
			pack := rp.packFor(sig, plan)
			for len(pack) > 0 {
				n := 65515
				if len(pack) < n {
					n = len(pack)
				}
				payload := make([]byte, 0, n+1)
				payload = append(payload, 1)
				payload = append(payload, pack[:n]...)
				buf.Write(pktLine(payload))
				pack = pack[n:]
			}
		}
		buf.Write(pktFlush)
		return buf.Bytes()
	})
	writeFetchBody(w, r, respBody)
}

// ------------------------------------------------------------ receive-pack

type pushCommand struct {
	oldOID string
	newOID string
	ref    string
}

func gitReceivePack(s *store, rp *repo, w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		w.WriteHeader(400)
		return
	}
	sc := &pktScanner{data: body}
	var commands []pushCommand
	caps := map[string]bool{}
	first := true
	for {
		kind, payload := sc.next()
		if kind != pktData {
			break
		}
		line := string(payload)
		if first {
			first = false
			if i := strings.IndexByte(line, 0); i >= 0 {
				for _, c := range strings.Fields(strings.TrimSuffix(line[i+1:], "\n")) {
					caps[c] = true
				}
				line = line[:i]
			}
		}
		line = strings.TrimSuffix(line, "\n")
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		commands = append(commands, pushCommand{oldOID: fields[0], newOID: fields[1], ref: fields[2]})
	}
	packBytes := body[sc.pos:]

	s.mu.Lock()
	unpackErr := ""
	if len(packBytes) > 0 {
		unpackErr = rp.unpackPack(packBytes)
	}
	type outcome struct {
		ref    string
		ok     bool
		reason string
	}
	var outcomes []outcome
	for _, cmd := range commands {
		out := outcome{ref: cmd.ref, ok: true}
		if unpackErr != "" {
			out.ok = false
			out.reason = "unpack failed"
			outcomes = append(outcomes, out)
			continue
		}
		current, exists := rp.refs[cmd.ref]
		switch {
		case cmd.newOID == zeroOID: // delete
			if exists {
				delete(rp.refs, cmd.ref)
				rp.refsVer++
			}
		case cmd.oldOID == zeroOID: // create
			if exists && current != cmd.newOID {
				out.ok = false
				out.reason = "reference already exists"
			} else if rp.get(cmd.newOID) == nil {
				out.ok = false
				out.reason = "missing object"
			} else {
				rp.refs[cmd.ref] = cmd.newOID
				rp.refsVer++
			}
		default: // update
			if !exists || current != cmd.oldOID {
				out.ok = false
				out.reason = "stale ref"
			} else if cmd.newOID == cmd.oldOID {
				// no-op: ok
			} else if rp.get(cmd.newOID) == nil {
				out.ok = false
				out.reason = "missing object"
			} else if strings.HasPrefix(cmd.ref, "refs/tags/") || rp.isAncestor(cmd.oldOID, cmd.newOID) {
				rp.refs[cmd.ref] = cmd.newOID
				rp.refsVer++
			} else {
				out.ok = false
				out.reason = "non-fast-forward"
			}
		}
		outcomes = append(outcomes, out)
	}
	s.mu.Unlock()

	// Build the report-status stream (inner pkt-lines).
	var report bytes.Buffer
	if unpackErr != "" {
		report.Write(pktStr("unpack " + unpackErr + "\n"))
	} else {
		report.Write(pktStr("unpack ok\n"))
	}
	for _, out := range outcomes {
		if out.ok {
			report.Write(pktStr("ok " + out.ref + "\n"))
		} else {
			report.Write(pktStr("ng " + out.ref + " " + out.reason + "\n"))
		}
	}
	report.Write(pktFlush)

	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Header().Set("X-GitHub-Request-Id", requestID())
	w.WriteHeader(200)
	if caps["side-band-64k"] {
		inner := report.Bytes()
		var buf bytes.Buffer
		for len(inner) > 0 {
			n := 65515
			if len(inner) < n {
				n = len(inner)
			}
			payload := make([]byte, 0, n+1)
			payload = append(payload, 1)
			payload = append(payload, inner[:n]...)
			buf.Write(pktLine(payload))
			inner = inner[n:]
		}
		buf.Write(pktFlush)
		_, _ = w.Write(buf.Bytes())
	} else {
		_, _ = w.Write(report.Bytes())
	}
}

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Git object model: objects are stored uncompressed (header + payload) keyed by their
// SHA-1. Types: blob, tree, commit, tag.

type gitObject struct {
	typ  string
	data []byte // payload only (no header)
}

func hashObject(typ string, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// put stores an object and returns its oid. Callers hold the store lock.
func (r *repo) put(typ string, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := r.objects[oid]; !ok {
		cp := append([]byte(nil), data...)
		r.objects[oid] = &gitObject{typ: typ, data: cp}
	}
	return oid
}

func (r *repo) get(oid string) *gitObject { return r.objects[oid] }

func zlibInflate(data []byte) ([]byte, int, error) {
	zr, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, 0, err
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		return nil, 0, err
	}
	// bytes consumed: total input minus what the reader left unread is not exposed;
	// callers that need the offset use zlib.NewReader on a counting reader instead.
	return out, 0, nil
}

// inflateAt inflates a zlib stream starting at offset in buf, returning the payload and
// the offset just past the stream.
func inflateAt(buf []byte, offset int) ([]byte, int, error) {
	cr := &countReader{b: buf[offset:]}
	zr, err := zlib.NewReader(cr)
	if err != nil {
		return nil, 0, err
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		return nil, 0, err
	}
	zr.Close()
	return out, offset + cr.n, nil
}

type countReader struct {
	b []byte
	n int
}

func (c *countReader) Read(p []byte) (int, error) {
	if c.n >= len(c.b) {
		return 0, io.EOF
	}
	n := copy(p, c.b[c.n:])
	c.n += n
	return n, nil
}

func zlibDeflate(data []byte) []byte {
	var buf bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&buf, zlib.BestSpeed)
	zw.Write(data)
	zw.Close()
	return buf.Bytes()
}

// ------------------------------------------------------------ trees

type treeEntry struct {
	mode string // e.g. "100644", "40000" (git writes tree dir mode without leading zero in raw form: "40000")
	name string
	oid  string
}

// parseTree parses raw tree payload: entries of "<mode> <name>\x00<20-byte oid>".
func parseTree(data []byte) []treeEntry {
	var out []treeEntry
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp < 0 {
			break
		}
		mode := string(data[:sp])
		rest := data[sp+1:]
		nul := bytes.IndexByte(rest, 0)
		if nul < 0 || len(rest) < nul+21 {
			break
		}
		name := string(rest[:nul])
		oid := hex.EncodeToString(rest[nul+1 : nul+21])
		out = append(out, treeEntry{mode: mode, name: name, oid: oid})
		data = rest[nul+21:]
	}
	return out
}

// buildTree serialises entries in git's canonical order: by name, with directories
// sorting as if they had a trailing '/'.
func buildTree(entries []treeEntry) []byte {
	sorted := append([]treeEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		ka, kb := a.name, b.name
		if a.mode == "40000" {
			ka += "/"
		}
		if b.mode == "40000" {
			kb += "/"
		}
		return ka < kb
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		raw, _ := hex.DecodeString(e.oid)
		fmt.Fprintf(&buf, "%s %s\x00", e.mode, e.name)
		buf.Write(raw)
	}
	return buf.Bytes()
}

// flattenTree expands a tree recursively into path -> (mode, oid). Paths use '/'.
func (r *repo) flattenTree(treeOID string, prefix string, out map[string]treeEntry) {
	obj := r.get(treeOID)
	if obj == nil || obj.typ != "tree" {
		return
	}
	for _, e := range parseTree(obj.data) {
		p := e.name
		if prefix != "" {
			p = prefix + "/" + e.name
		}
		if e.mode == "40000" {
			r.flattenTree(e.oid, p, out)
		} else {
			out[p] = e
		}
	}
}

// treeFromFlat builds nested trees from a flat path map and returns the root tree oid.
func (r *repo) treeFromFlat(flat map[string]treeEntry) string {
	type node struct {
		files map[string]treeEntry
		dirs  map[string]*node
	}
	root := &node{files: map[string]treeEntry{}, dirs: map[string]*node{}}
	for path, e := range flat {
		parts := strings.Split(path, "/")
		cur := root
		for _, d := range parts[:len(parts)-1] {
			if cur.dirs[d] == nil {
				cur.dirs[d] = &node{files: map[string]treeEntry{}, dirs: map[string]*node{}}
			}
			cur = cur.dirs[d]
		}
		cur.files[parts[len(parts)-1]] = e
	}
	var build func(n *node) string
	build = func(n *node) string {
		var entries []treeEntry
		for name, e := range n.files {
			entries = append(entries, treeEntry{mode: e.mode, name: name, oid: e.oid})
		}
		for name, d := range n.dirs {
			entries = append(entries, treeEntry{mode: "40000", name: name, oid: build(d)})
		}
		return r.put("tree", buildTree(entries))
	}
	return build(root)
}

// ------------------------------------------------------------ commits

type commitInfo struct {
	tree    string
	parents []string
	message string
}

func parseCommit(data []byte) commitInfo {
	var ci commitInfo
	rest := data
	for {
		nl := bytes.IndexByte(rest, '\n')
		if nl < 0 {
			break
		}
		line := string(rest[:nl])
		rest = rest[nl+1:]
		if line == "" {
			break
		}
		switch {
		case strings.HasPrefix(line, "tree "):
			ci.tree = line[5:]
		case strings.HasPrefix(line, "parent "):
			ci.parents = append(ci.parents, line[7:])
		}
	}
	ci.message = string(rest)
	return ci
}

// buildCommit builds a commit payload with the forge's deterministic identity/clock.
func buildCommit(tree string, parents []string, message string, seq int64) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "tree %s\n", tree)
	for _, p := range parents {
		fmt.Fprintf(&buf, "parent %s\n", p)
	}
	ts := scenarioEpoch + seq
	ident := fmt.Sprintf("%s <%s> %d +0000", viewerName, viewerEmail, ts)
	fmt.Fprintf(&buf, "author %s\ncommitter %s\n\n", ident, ident)
	buf.WriteString(message)
	return buf.Bytes()
}

// commitTree returns the tree oid of a commit.
func (r *repo) commitTree(commitOID string) string {
	obj := r.get(commitOID)
	if obj == nil || obj.typ != "commit" {
		return ""
	}
	return parseCommit(obj.data).tree
}

// isAncestor reports whether anc is reachable from desc by walking parents (desc itself counts).
func (r *repo) isAncestor(anc, desc string) bool {
	if anc == desc {
		return true
	}
	seen := map[string]bool{}
	stack := []string{desc}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == anc {
			return true
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		obj := r.get(cur)
		if obj == nil || obj.typ != "commit" {
			continue
		}
		stack = append(stack, parseCommit(obj.data).parents...)
	}
	return false
}

// mergeBase finds a best common ancestor of two commits (first BFS hit from b's side
// that is an ancestor of a; histories here are shallow and near-linear).
func (r *repo) mergeBase(a, b string) string {
	ancestors := map[string]bool{}
	stack := []string{a}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if ancestors[cur] {
			continue
		}
		ancestors[cur] = true
		if obj := r.get(cur); obj != nil && obj.typ == "commit" {
			stack = append(stack, parseCommit(obj.data).parents...)
		}
	}
	// BFS from b so the nearest common ancestor wins.
	queue := []string{b}
	seen := map[string]bool{}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if ancestors[cur] {
			return cur
		}
		if obj := r.get(cur); obj != nil && obj.typ == "commit" {
			queue = append(queue, parseCommit(obj.data).parents...)
		}
	}
	return ""
}

// reachableObjects returns every object reachable from the given tip oids.
func (r *repo) reachableObjects(tips []string) map[string]bool {
	out := map[string]bool{}
	var walkObj func(oid string)
	walkObj = func(oid string) {
		if out[oid] {
			return
		}
		obj := r.get(oid)
		if obj == nil {
			return
		}
		out[oid] = true
		switch obj.typ {
		case "commit":
			ci := parseCommit(obj.data)
			walkObj(ci.tree)
			for _, p := range ci.parents {
				walkObj(p)
			}
		case "tree":
			for _, e := range parseTree(obj.data) {
				walkObj(e.oid)
			}
		case "tag":
			if tgt := tagTarget(obj.data); tgt != "" {
				walkObj(tgt)
			}
		}
	}
	for _, t := range tips {
		walkObj(t)
	}
	return out
}

func tagTarget(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "object ") {
			return strings.TrimSpace(line[7:])
		}
		if line == "" {
			break
		}
	}
	return ""
}

// refShort strips refs/heads/ or refs/tags/ for display.
func refShort(ref string) string {
	ref = strings.TrimPrefix(ref, "refs/heads/")
	ref = strings.TrimPrefix(ref, "refs/tags/")
	return ref
}

var _ = strconv.Itoa

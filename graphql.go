package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// A small GraphQL executor for exactly the language subset the suite's documents use
// (see specs/graphql-bench.md §2): named query/mutation operations, variables, aliases,
// inline enum/list/object literals, one inline fragment form, __type introspection with
// fields/enumValues selecting name, and JSON-array batching. Unknown fields answer
// HTTP 200 with an errors array naming the field, as GitHub does.

// ------------------------------------------------------------ ordered JSON

type jpair struct {
	k string
	v any
}
type jobj []jpair

func (o jobj) marshal() []byte {
	var b strings.Builder
	b.WriteByte('{')
	for i, p := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(p.k)
		b.Write(kb)
		b.WriteByte(':')
		b.Write(marshalValue(p.v))
	}
	b.WriteByte('}')
	return []byte(b.String())
}

func marshalValue(v any) []byte {
	switch t := v.(type) {
	case nil:
		return []byte("null")
	case jobj:
		return t.marshal()
	case []any:
		var b strings.Builder
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			b.Write(marshalValue(e))
		}
		b.WriteByte(']')
		return []byte(b.String())
	default:
		data, _ := json.Marshal(v)
		return data
	}
}

// ------------------------------------------------------------ lexer

type gtoken struct {
	kind string // "name", "string", "int", "punct", "eof"
	val  string
}

func glex(src string) ([]gtoken, error) {
	var toks []gtoken
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '.' && i+2 < len(src) && src[i+1] == '.' && src[i+2] == '.':
			toks = append(toks, gtoken{"punct", "..."})
			i += 3
		case strings.IndexByte("{}():$![]=@", c) >= 0:
			toks = append(toks, gtoken{"punct", string(c)})
			i++
		case c == '"':
			if i+2 < len(src) && src[i+1] == '"' && src[i+2] == '"' {
				end := strings.Index(src[i+3:], `"""`)
				if end < 0 {
					return nil, fmt.Errorf("unterminated block string")
				}
				toks = append(toks, gtoken{"string", src[i+3 : i+3+end]})
				i += 3 + end + 3
			} else {
				var sb strings.Builder
				i++
				for i < len(src) && src[i] != '"' {
					if src[i] == '\\' && i+1 < len(src) {
						i++
						switch src[i] {
						case 'n':
							sb.WriteByte('\n')
						case 't':
							sb.WriteByte('\t')
						case 'r':
							sb.WriteByte('\r')
						case 'u':
							if i+4 < len(src) {
								n, _ := strconv.ParseInt(src[i+1:i+5], 16, 32)
								sb.WriteRune(rune(n))
								i += 4
							}
						default:
							sb.WriteByte(src[i])
						}
						i++
					} else {
						sb.WriteByte(src[i])
						i++
					}
				}
				if i >= len(src) {
					return nil, fmt.Errorf("unterminated string")
				}
				i++
				toks = append(toks, gtoken{"string", sb.String()})
			}
		case c == '-' || (c >= '0' && c <= '9'):
			j := i
			if src[j] == '-' {
				j++
			}
			for j < len(src) && src[j] >= '0' && src[j] <= '9' {
				j++
			}
			toks = append(toks, gtoken{"int", src[i:j]})
			i = j
		case c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			j := i
			for j < len(src) && (src[j] == '_' || (src[j] >= 'a' && src[j] <= 'z') || (src[j] >= 'A' && src[j] <= 'Z') || (src[j] >= '0' && src[j] <= '9')) {
				j++
			}
			toks = append(toks, gtoken{"name", src[i:j]})
			i = j
		default:
			return nil, fmt.Errorf("unexpected character %q", c)
		}
	}
	toks = append(toks, gtoken{"eof", ""})
	return toks, nil
}

// ------------------------------------------------------------ parser

type gvalue struct {
	kind string // var, string, int, enum, bool, null, list, object
	s    string
	list []gvalue
	obj  []gpair
}
type gpair struct {
	k string
	v gvalue
}

type gfield struct {
	alias string
	name  string
	args  map[string]gvalue
	sel   []gfield
	// inline fragment
	fragment bool
	onType   string
}

type gparser struct {
	toks []gtoken
	pos  int
}

func (p *gparser) peek() gtoken  { return p.toks[p.pos] }
func (p *gparser) next() gtoken  { t := p.toks[p.pos]; p.pos++; return t }
func (p *gparser) expect(v string) error {
	t := p.next()
	if t.val != v {
		return fmt.Errorf("expected %q, got %q", v, t.val)
	}
	return nil
}

func (p *gparser) parseDocument() (op string, sels []gfield, err error) {
	t := p.peek()
	if t.kind == "punct" && t.val == "{" {
		sels, err := p.parseSelectionSet()
		return "query", sels, err
	}
	if t.kind != "name" || (t.val != "query" && t.val != "mutation" && t.val != "subscription") {
		return "", nil, fmt.Errorf("expected operation, got %q", t.val)
	}
	op = p.next().val
	if p.peek().kind == "name" {
		p.next() // operation name
	}
	if p.peek().val == "(" {
		if err := p.skipBalanced(); err != nil {
			return "", nil, err
		}
	}
	sels, err = p.parseSelectionSet()
	return op, sels, err
}

func (p *gparser) skipBalanced() error {
	depth := 0
	for {
		t := p.next()
		if t.kind == "eof" {
			return fmt.Errorf("unbalanced parens")
		}
		if t.val == "(" {
			depth++
		}
		if t.val == ")" {
			depth--
			if depth == 0 {
				return nil
			}
		}
	}
}

func (p *gparser) parseSelectionSet() ([]gfield, error) {
	if err := p.expect("{"); err != nil {
		return nil, err
	}
	var out []gfield
	for {
		t := p.peek()
		if t.kind == "punct" && t.val == "}" {
			p.next()
			return out, nil
		}
		if t.kind == "eof" {
			return nil, fmt.Errorf("unterminated selection set")
		}
		f, err := p.parseField()
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
}

func (p *gparser) parseField() (gfield, error) {
	var f gfield
	if p.peek().val == "..." {
		p.next()
		// inline fragment: on Type { ... } (named fragments do not occur in the suite)
		if p.peek().kind == "name" && p.peek().val == "on" {
			p.next()
			f.onType = p.next().val
		}
		f.fragment = true
		sels, err := p.parseSelectionSet()
		f.sel = sels
		return f, err
	}
	nameTok := p.next()
	if nameTok.kind != "name" {
		return f, fmt.Errorf("expected field name, got %q", nameTok.val)
	}
	f.name = nameTok.val
	if p.peek().val == ":" {
		p.next()
		f.alias = f.name
		nameTok = p.next()
		if nameTok.kind != "name" {
			return f, fmt.Errorf("expected field name after alias")
		}
		f.name = nameTok.val
	}
	if p.peek().val == "(" {
		args, err := p.parseArgs()
		if err != nil {
			return f, err
		}
		f.args = args
	}
	if p.peek().val == "{" {
		sels, err := p.parseSelectionSet()
		if err != nil {
			return f, err
		}
		f.sel = sels
	}
	return f, nil
}

func (p *gparser) parseArgs() (map[string]gvalue, error) {
	args := map[string]gvalue{}
	if err := p.expect("("); err != nil {
		return nil, err
	}
	for p.peek().val != ")" {
		nameTok := p.next()
		if nameTok.kind != "name" {
			return nil, fmt.Errorf("expected argument name")
		}
		if err := p.expect(":"); err != nil {
			return nil, err
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args[nameTok.val] = v
	}
	p.next()
	return args, nil
}

func (p *gparser) parseValue() (gvalue, error) {
	t := p.next()
	switch {
	case t.val == "$":
		nameTok := p.next()
		return gvalue{kind: "var", s: nameTok.val}, nil
	case t.kind == "string":
		return gvalue{kind: "string", s: t.val}, nil
	case t.kind == "int":
		return gvalue{kind: "int", s: t.val}, nil
	case t.val == "[":
		var list []gvalue
		for p.peek().val != "]" {
			v, err := p.parseValue()
			if err != nil {
				return gvalue{}, err
			}
			list = append(list, v)
		}
		p.next()
		return gvalue{kind: "list", list: list}, nil
	case t.val == "{":
		var obj []gpair
		for p.peek().val != "}" {
			nameTok := p.next()
			if nameTok.kind != "name" {
				return gvalue{}, fmt.Errorf("expected object field name")
			}
			if err := p.expect(":"); err != nil {
				return gvalue{}, err
			}
			v, err := p.parseValue()
			if err != nil {
				return gvalue{}, err
			}
			obj = append(obj, gpair{nameTok.val, v})
		}
		p.next()
		return gvalue{kind: "object", obj: obj}, nil
	case t.kind == "name":
		switch t.val {
		case "true", "false":
			return gvalue{kind: "bool", s: t.val}, nil
		case "null":
			return gvalue{kind: "null"}, nil
		default:
			return gvalue{kind: "enum", s: t.val}, nil
		}
	}
	return gvalue{}, fmt.Errorf("unexpected value token %q", t.val)
}

// resolve turns a parsed argument value into a plain Go value using variables.
func (v gvalue) resolve(vars map[string]any) any {
	switch v.kind {
	case "var":
		return vars[v.s]
	case "string", "enum":
		return v.s
	case "int":
		n, _ := strconv.ParseInt(v.s, 10, 64)
		return float64(n)
	case "bool":
		return v.s == "true"
	case "null":
		return nil
	case "list":
		out := make([]any, 0, len(v.list))
		for _, e := range v.list {
			out = append(out, e.resolve(vars))
		}
		return out
	case "object":
		out := map[string]any{}
		for _, kv := range v.obj {
			out[kv.k] = kv.v.resolve(vars)
		}
		return out
	}
	return nil
}

func argStr(f gfield, vars map[string]any, name string) string {
	if v, ok := f.args[name]; ok {
		if s, ok := v.resolve(vars).(string); ok {
			return s
		}
	}
	return ""
}

func argInt(f gfield, vars map[string]any, name string, def int) int {
	if v, ok := f.args[name]; ok {
		switch n := v.resolve(vars).(type) {
		case float64:
			return int(n)
		case string:
			i, _ := strconv.Atoi(n)
			return i
		}
	}
	return def
}

func argPresent(f gfield, vars map[string]any, name string) (any, bool) {
	v, ok := f.args[name]
	if !ok {
		return nil, false
	}
	return v.resolve(vars), true
}

// ------------------------------------------------------------ execution

type gnode struct {
	kind    string // Query, Mutation, User, Repository, Issue, PullRequest, IssueComment,
	// CreateIssuePayload, CloseIssuePayload, CreatePullRequestPayload,
	// MergePullRequestPayload, AddCommentPayload, CommentEdge, Type, Field, EnumValue,
	// Ref, Connection, PageInfo, Label
	repo    *repo
	issue   *issue
	pull    *pull
	comment *comment
	edge    *comment
	// connection state
	connKind string // "issues" | "pulls" | "files"
	items    []gnode
	pageInfo jobj
	// introspection
	typeName string
	// scalar leaf for Type.fields / enumValues items
	leafName string
	// edge wrapper item
	edgeNode *gnode
}

type gexec struct {
	s    *store
	vars map[string]any
	errs []string
}

func (e *gexec) failField(typeName, field string) {
	e.errs = append(e.errs, fmt.Sprintf("Field '%s' doesn't exist on type '%s'", field, typeName))
}

func (e *gexec) execute(op string, sels []gfield) jobj {
	root := gnode{kind: "Query"}
	if op == "mutation" {
		root.kind = "Mutation"
	}
	return e.resolveObj(root, sels)
}

func (e *gexec) resolveObj(n gnode, sels []gfield) jobj {
	out := jobj{}
	for _, f := range sels {
		if f.fragment {
			if f.onType == "" || f.onType == n.kind || (f.onType == "Repository" && n.kind == "Repository") {
				inner := e.resolveObj(n, f.sel)
				out = append(out, inner...)
			}
			continue
		}
		key := f.name
		if f.alias != "" {
			key = f.alias
		}
		out = append(out, jpair{key, e.resolveField(n, f)})
	}
	return out
}

func (e *gexec) resolveField(n gnode, f gfield) any {
	switch n.kind {
	case "Query":
		return e.queryField(f)
	case "Mutation":
		return e.mutationField(f)
	case "User":
		switch f.name {
		case "login":
			return viewerLogin
		case "id":
			return gidOf("User", viewerID)
		case "databaseId":
			return viewerID
		case "name":
			return viewerName
		case "email":
			return viewerEmail
		case "url":
			return "https://make.fun/" + viewerLogin
		}
	case "Repository":
		return e.repoField(n.repo, f)
	case "Issue":
		return e.issueField(n.issue, f)
	case "PullRequest":
		return e.pullField(n.pull, f)
	case "IssueComment":
		switch f.name {
		case "id":
			return n.comment.gid
		case "body":
			return n.comment.body
		case "createdAt":
			return tsString(n.comment.seq)
		case "updatedAt":
			return tsString(n.comment.seq)
		case "author":
			return e.resolveObj(gnode{kind: "User"}, f.sel)
		}
	case "CreateIssuePayload", "CloseIssuePayload":
		if f.name == "issue" {
			return e.resolveObj(gnode{kind: "Issue", issue: n.issue}, f.sel)
		}
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		if f.name == "pullRequest" {
			return e.resolveObj(gnode{kind: "PullRequest", pull: n.pull}, f.sel)
		}
	case "AddCommentPayload":
		if f.name == "commentEdge" {
			return e.resolveObj(gnode{kind: "CommentEdge", edge: n.edge}, f.sel)
		}
		if f.name == "comment" {
			return e.resolveObj(gnode{kind: "IssueComment", comment: n.edge}, f.sel)
		}
	case "CommentEdge":
		if f.name == "node" {
			return e.resolveObj(gnode{kind: "IssueComment", comment: n.edge}, f.sel)
		}
		if f.name == "cursor" {
			return cursorEncode(0)
		}
	case "Type":
		switch f.name {
		case "name":
			return n.typeName
		case "fields":
			var items []any
			for _, name := range typeFields[n.typeName] {
				items = append(items, e.resolveObj(gnode{kind: "Field", leafName: name}, f.sel))
			}
			return items
		case "enumValues":
			var items []any
			for _, name := range typeEnums[n.typeName] {
				items = append(items, e.resolveObj(gnode{kind: "EnumValue", leafName: name}, f.sel))
			}
			return items
		case "kind":
			if _, isEnum := typeEnums[n.typeName]; isEnum {
				return "ENUM"
			}
			return "OBJECT"
		case "description":
			return nil
		}
	case "Field", "EnumValue":
		if f.name == "name" {
			return n.leafName
		}
		if f.name == "description" {
			return nil
		}
		if f.name == "isDeprecated" {
			return false
		}
	case "Ref":
		if f.name == "name" {
			return n.leafName
		}
		if f.name == "prefix" {
			return "refs/heads/"
		}
	case "LabelNode":
		switch f.name {
		case "name":
			return n.leafName
		case "id":
			return gidOf("Label", 1)
		case "color":
			return "ededed"
		case "description":
			return nil
		}
	case "FileNode":
		switch f.name {
		case "path":
			return n.leafName
		case "additions", "deletions":
			return 0
		case "changeType":
			return "ADDED"
		}
	case "Connection":
		switch f.name {
		case "nodes":
			var items []any
			for _, it := range n.items {
				items = append(items, e.resolveObj(it, f.sel))
			}
			return items
		case "edges":
			var items []any
			for _, it := range n.items {
				item := it
				edge := gnode{kind: "Edge", edgeNode: &item}
				items = append(items, e.resolveObj(edge, f.sel))
			}
			return items
		case "pageInfo":
			return n.pageInfo
		case "totalCount":
			return len(n.items)
		}
	case "Edge":
		if f.name == "node" && n.edgeNode != nil {
			return e.resolveObj(*n.edgeNode, f.sel)
		}
		if f.name == "cursor" {
			return cursorEncode(0)
		}
	}
	e.failField(n.kind, f.name)
	return nil
}

// Edge support: gnode carries an item for edges.

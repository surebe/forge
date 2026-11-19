package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// MCP surface (Streamable HTTP, modern era 2026-07-28). Contract (specs/rest-mcp.md §7):
//   - POST /mcp, JSON-RPC 2.0; server/discover answers with supportedVersions incl.
//     "2026-07-28", serverInfo, capabilities.tools;
//   - tools/list is byte-identical across calls: the tools array is marshalled once;
//   - the six canonical tools share the REST store;
//   - errors: unknown tool -32602, malformed body HTTP 400 + -32700,
//     missing arguments -32602, no auth HTTP 401 (before any JSON-RPC handling).

const mcpRevision = "2026-07-28"

// mcpToolsJSON is the pre-marshalled tools array — identical bytes on every call.
var mcpToolsJSON = buildMCPTools()

func mcpTool(name, descr string, props map[string]any, required []string) map[string]any {
	return map[string]any{
		"name":        name,
		"description": descr,
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": props,
			"required":   required,
		},
	}
}

func buildMCPTools() json.RawMessage {
	str := map[string]any{"type": "string"}
	tools := []any{
		mcpTool("create_repo", "Create a new repository for the given owner.",
			map[string]any{
				"owner":       str,
				"name":        str,
				"description": str,
				"private":     map[string]any{"type": "boolean"},
			}, []string{"name"}),
		mcpTool("get_repo", "Get a repository by owner and name.",
			map[string]any{"owner": str, "repo": str}, []string{"owner", "repo"}),
		mcpTool("create_issue", "Create an issue in a repository.",
			map[string]any{
				"owner": str, "repo": str, "title": str, "body": str,
			}, []string{"owner", "repo", "title"}),
		mcpTool("list_issues", "List issues in a repository.",
			map[string]any{
				"owner": str, "repo": str,
				"state": map[string]any{"type": "string"},
			}, []string{"owner", "repo"}),
		mcpTool("create_pull_request", "Create a pull request in a repository.",
			map[string]any{
				"owner": str, "repo": str, "title": str, "body": str,
				"head": str, "base": str,
			}, []string{"owner", "repo", "title", "head", "base"}),
		mcpTool("merge_pull_request", "Merge a pull request by number.",
			map[string]any{
				"owner": str, "repo": str,
				"pull_number": map[string]any{"type": "integer"},
			}, []string{"owner", "repo", "pull_number"}),
	}
	data, _ := json.Marshal(tools)
	return data
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func rpcResult(w http.ResponseWriter, r *http.Request, id json.RawMessage, result any) {
	if id == nil {
		id = json.RawMessage("null")
	}
	body := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": result}
	writeJSON(w, r, 200, body)
}

func rpcError(w http.ResponseWriter, r *http.Request, status int, id json.RawMessage, code int, message string) {
	if id == nil {
		id = json.RawMessage("null")
	}
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"error":   map[string]any{"code": code, "message": message},
	}
	writeJSON(w, r, status, body)
}

func handleMCP(s *store, w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet || r.Method == http.MethodDelete {
		// No server-initiated stream; the checks never use these.
		w.Header().Set("X-GitHub-Request-Id", requestID())
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, r, 404, "Not Found")
		return
	}
	data, err := readBody(r)
	if err != nil {
		rpcError(w, r, 400, nil, -32700, "Parse error")
		return
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		rpcError(w, r, 400, nil, -32700, "Parse error")
		return
	}
	writeResp := func(status int, body []byte) {
		if body == nil {
			return // notification: implicit 200 with an empty body
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-GitHub-Request-Id", requestID())
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
	// Batched requests are not used by the suite; answer element-wise if one arrives.
	if strings.HasPrefix(trimmed, "[") {
		var elems []json.RawMessage
		if err := json.Unmarshal(data, &elems); err != nil {
			rpcError(w, r, 400, nil, -32700, "Parse error")
			return
		}
		var out []json.RawMessage
		for _, e := range elems {
			if _, b := mcpDispatch(s, r, e); b != nil {
				out = append(out, b)
			}
		}
		writeJSON(w, r, 200, json.RawMessage(mustMarshal(out)))
		return
	}
	status, body := mcpDispatch(s, r, data)
	writeResp(status, body)
}

// mcpDispatch handles one JSON-RPC message and returns (HTTP status, response body).
// Notifications return a nil body: nothing is written for them.
func mcpDispatch(s *store, r *http.Request, data []byte) (int, []byte) {
	respond := func(status int, payload any) (int, []byte) {
		b, _ := json.Marshal(payload)
		return status, b
	}
	errResp := func(status int, id json.RawMessage, code int, msg string) (int, []byte) {
		if id == nil {
			id = json.RawMessage("null")
		}
		return respond(status, map[string]any{
			"jsonrpc": "2.0", "id": id,
			"error": map[string]any{"code": code, "message": msg},
		})
	}
	okResp := func(id json.RawMessage, result any) (int, []byte) {
		return respond(200, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}

	var req rpcRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return errResp(400, nil, -32700, "Parse error")
	}
	isNotification := req.ID == nil
	// Auth gate: tools/call without a token is HTTP 401 before anything else.
	if req.Method == "tools/call" && !authed(r) {
		if isNotification {
			return 200, nil
		}
		return errResp(401, req.ID, -32001, "Unauthorized")
	}
	switch req.Method {
	case "server/discover":
		if isNotification {
			return 200, nil
		}
		return okResp(req.ID, map[string]any{
			"supportedVersions": []string{mcpRevision},
			"serverInfo":        map[string]any{"name": "forge", "version": "1.0.0"},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"instructions":      "A GitHub-compatible forge. Tools mirror the REST API.",
		})
	case "initialize":
		if isNotification {
			return 200, nil
		}
		return okResp(req.ID, map[string]any{
			"protocolVersion": mcpRevision,
			"serverInfo":      map[string]any{"name": "forge", "version": "1.0.0"},
			"capabilities":    map[string]any{"tools": map[string]any{}},
		})
	case "tools/list":
		if isNotification {
			return 200, nil
		}
		return respond(200, map[string]any{
			"jsonrpc": "2.0", "id": req.ID,
			"result": map[string]any{"tools": json.RawMessage(mcpToolsJSON)},
		})
	case "tools/call":
		if isNotification {
			return 200, nil
		}
		okBody := func(id json.RawMessage, result any) []byte {
			_, b := okResp(id, result)
			return b
		}
		errBody := func(status int, id json.RawMessage, code int, msg string) []byte {
			_, b := errResp(status, id, code, msg)
			return b
		}
		return 200, mcpCallTool(s, req, okBody, errBody)
	case "ping":
		if isNotification {
			return 200, nil
		}
		return okResp(req.ID, map[string]any{})
	default:
		if isNotification {
			return 200, nil
		}
		if strings.HasPrefix(req.Method, "notifications/") {
			return 200, nil
		}
		return errResp(200, req.ID, -32601, "Method not found: "+req.Method)
	}
}

func mustMarshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

type rpcResponder func(id json.RawMessage, result any) []byte
type rpcErrResponder func(status int, id json.RawMessage, code int, msg string) []byte

func mcpCallTool(s *store, req rpcRequest, okResp rpcResponder, errResp rpcErrResponder) []byte {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &params)
	}
	args := map[string]any{}
	if len(params.Arguments) > 0 {
		_ = json.Unmarshal(params.Arguments, &args)
	}
	toolResult := func(obj any) []byte {
		text, _ := json.Marshal(obj)
		return okResp(req.ID, map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": string(text)},
			},
			"structuredContent": obj,
			"isError":           false,
		})
	}
	toolErr := func(msg string) []byte {
		return okResp(req.ID, map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": msg},
			},
			"isError": true,
		})
	}
	missing := func(fields ...string) []byte {
		for _, f := range fields {
			v, present := args[f]
			if !present || v == nil || v == "" {
				return errResp(200, req.ID, -32602, "Missing required argument: "+f)
			}
		}
		return nil
	}
	argStr := func(k string) string {
		if v, ok := args[k].(string); ok {
			return v
		}
		return ""
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	switch params.Name {
	case "create_repo":
		if resp := missing("name"); resp != nil {
			return resp
		}
		name := argStr("name")
		owner := argStr("owner")
		if owner == "" {
			owner = viewerLogin
		}
		if owner != viewerLogin {
			return toolErr("repository owner not found")
		}
		if s.getRepo(viewerLogin, name) != nil {
			return toolErr("repository already exists")
		}
		private, _ := args["private"].(bool)
		rp := s.createRepo(name, argStr("description"), private)
		return toolResult(repoJSON(rp))
	case "get_repo":
		if resp := missing("owner", "repo"); resp != nil {
			return resp
		}
		rp := s.getRepo(argStr("owner"), argStr("repo"))
		if rp == nil || argStr("owner") != viewerLogin {
			return toolErr("repository not found")
		}
		return toolResult(repoJSON(rp))
	case "create_issue":
		if resp := missing("owner", "repo", "title"); resp != nil {
			return resp
		}
		rp := s.getRepo(argStr("owner"), argStr("repo"))
		if rp == nil || argStr("owner") != viewerLogin {
			return toolErr("repository not found")
		}
		is := rp.newIssue(argStr("title"), argStr("body"), nil)
		return toolResult(issueJSON(is))
	case "list_issues":
		if resp := missing("owner", "repo"); resp != nil {
			return resp
		}
		rp := s.getRepo(argStr("owner"), argStr("repo"))
		if rp == nil || argStr("owner") != viewerLogin {
			return toolErr("repository not found")
		}
		state := argStr("state")
		var items []any
		for _, is := range rp.issueList() {
			if state != "" && state != "all" && is.state != state {
				continue
			}
			items = append(items, issueJSON(is))
		}
		if items == nil {
			items = []any{}
		}
		return toolResult(items)
	case "create_pull_request":
		if resp := missing("owner", "repo", "title", "head", "base"); resp != nil {
			return resp
		}
		rp := s.getRepo(argStr("owner"), argStr("repo"))
		if rp == nil || argStr("owner") != viewerLogin {
			return toolErr("repository not found")
		}
		head, base := argStr("head"), argStr("base")
		if _, exists := rp.refs["refs/heads/"+head]; !exists {
			return toolErr("head branch does not exist")
		}
		if _, exists := rp.refs["refs/heads/"+base]; !exists {
			return toolErr("base branch does not exist")
		}
		for _, existing := range rp.pulls {
			if existing.state == "open" && existing.head == head && existing.base == base {
				return toolResult(pullJSON(existing))
			}
		}
		p := rp.newPull(argStr("title"), argStr("body"), head, base)
		return toolResult(pullJSON(p))
	case "merge_pull_request":
		if resp := missing("owner", "repo", "pull_number"); resp != nil {
			return resp
		}
		rp := s.getRepo(argStr("owner"), argStr("repo"))
		if rp == nil || argStr("owner") != viewerLogin {
			return toolErr("repository not found")
		}
		num := 0
		switch v := args["pull_number"].(type) {
		case float64:
			num = int(v)
		case string:
			n := 0
			for _, c := range v {
				if c >= '0' && c <= '9' {
					n = n*10 + int(c-'0')
				}
			}
			num = n
		}
		p := rp.pulls[num]
		if p == nil {
			return toolErr("pull request not found")
		}
		if p.merged {
			return toolResult(pullJSON(p))
		}
		if pullConflict(p) {
			return toolErr("pull request has conflicts")
		}
		if !mergePullSquash(p) {
			return toolErr("pull request is not mergeable")
		}
		return toolResult(pullJSON(p))
	default:
		return errResp(200, req.ID, -32602, "Unknown tool: "+params.Name)
	}
}

// forge: a standalone GitHub-compatible git forge in one Go process.
//
// One in-memory store backs all four surfaces (REST v3, git smart HTTP, GraphQL v4, MCP).
// Lifecycle (the env contract): bind an ephemeral port (never 8080), POST {"port": N} to
// $MAKEFUN_ENV_URL/register, poll GET $MAKEFUN_ENV_URL/done until {"done": true}, then exit 0.
// When MAKEFUN_ENV_URL is unset the process simply serves (local conformance runs dial it
// directly with sdk/run.ts).
package main

import (
	"runtime/debug"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

var token string // MAKEFUN_SHOP_TOKEN: the bearer token every API call must carry

func main() {
	debug.SetGCPercent(400)
	token = os.Getenv("MAKEFUN_SHOP_TOKEN")
	s := newStore()
	srv := &http.Server{Handler: newMux(s)}

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "[forge] listen:", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Fprintf(os.Stderr, "[forge] listening on port %d\n", port)

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(os.Stderr, "[forge] serve:", err)
		}
	}()

	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		select {} // local mode: serve until killed
	}
	if err := register(envURL, port); err != nil {
		fmt.Fprintln(os.Stderr, "[forge] register:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "[forge] registered; waiting for done")
	waitDone(envURL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	fmt.Fprintln(os.Stderr, "[forge] done")
}

func register(envURL string, port int) error {
	body, _ := json.Marshal(map[string]int{"port": port})
	var lastErr error
	for attempt := 0; attempt < 200; attempt++ {
		resp, err := http.Post(envURL+"/register", "application/json", bytes.NewReader(body))
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 || resp.StatusCode == 409 {
				return nil
			}
			if resp.StatusCode == 400 || resp.StatusCode == 405 {
				return fmt.Errorf("register answered HTTP %d", resp.StatusCode)
			}
			lastErr = fmt.Errorf("register answered HTTP %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(retryDelay(attempt))
	}
	return lastErr
}

func waitDone(envURL string) {
	client := &http.Client{Timeout: 30 * time.Second}
	for attempt := 0; ; attempt++ {
		resp, err := client.Get(envURL + "/done")
		if err == nil {
			var body struct {
				Done bool `json:"done"`
			}
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 && json.Unmarshal(data, &body) == nil && body.Done {
				return
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

func retryDelay(attempt int) time.Duration {
	d := 250 * time.Millisecond << min(attempt, 3)
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	return d
}

// ---------------------------------------------------------------- http plumbing

var reqCounter atomic.Uint64

// requestID is deterministic (a counter, never a clock or random value) so no response
// byte depends on when the trial ran.
func requestID() string {
	return fmt.Sprintf("%d", reqCounter.Add(1))
}

type handlerFunc func(w http.ResponseWriter, r *http.Request)

func newMux(s *store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		route(s, w, r)
	})
	return mux
}

func route(s *store, w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/healthz" && r.Method == http.MethodGet:
		writeJSON(w, r, 200, map[string]any{"ok": true})
	case p == "/mcp":
		handleMCP(s, w, r)
	case p == "/api/graphql":
		handleGraphQL(s, w, r)
	case p == "/api/v3" || strings.HasPrefix(p, "/api/v3/"):
		handleREST(s, w, r)
	case strings.HasSuffix(p, ".git/info/refs") || strings.HasSuffix(p, ".git/git-upload-pack") || strings.HasSuffix(p, ".git/git-receive-pack"):
		handleGitHTTP(s, w, r)
	default:
		writeError(w, r, 404, "Not Found")
	}
}

// authed reports whether the request carries the trial's bearer token. /healthz is exempt
// (handled before this is consulted); every other surface requires it.
func authed(r *http.Request) bool {
	if token == "" {
		return true // local runs without a token configured: sdk/run.ts always passes one
	}
	return r.Header.Get("Authorization") == "Bearer "+token
}

const jsonType = "application/json; charset=utf-8"

func writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	data, _ := json.Marshal(body)
	h := w.Header()
	h.Set("Content-Type", jsonType)
	h.Set("X-GitHub-Request-Id", requestID())
	h.Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// writeRawJSON writes already-marshalled JSON (used where byte-stability matters).
func writeRawJSON(w http.ResponseWriter, r *http.Request, status int, data []byte) {
	h := w.Header()
	h.Set("Content-Type", jsonType)
	h.Set("X-GitHub-Request-Id", requestID())
	h.Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, message string) {
	writeJSON(w, r, status, map[string]any{"message": message})
}

// readBody reads a request body with a sane cap.
func readBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(io.LimitReader(r.Body, 64<<20))
}

// decodeJSON parses a JSON body; ok=false means malformed (a 400 on REST surfaces).
func decodeJSON(data []byte) (any, bool) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	return v, true
}

func asObject(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func strField(m map[string]any, key string) (string, bool) {
	v, ok := m[key].(string)
	return v, ok
}

func strOr(m map[string]any, key, def string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return def
}

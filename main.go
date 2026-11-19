// forge: a standalone GitHub-compatible git forge in one Go process.
//
// One in-memory store backs all four surfaces (REST v3, git smart HTTP, GraphQL v4, MCP).
// Lifecycle (the env contract): bind an ephemeral port (never 8080), POST {"port": N} to
// $MAKEFUN_ENV_URL/register, poll GET $MAKEFUN_ENV_URL/done until {"done": true}, then exit 0.
// When MAKEFUN_ENV_URL is unset the process simply serves (local conformance runs dial it
// directly with sdk/run.ts).
package main

import (
	"runtime"
	"runtime/debug"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var token string // MAKEFUN_SHOP_TOKEN: the bearer token every API call must carry

// cpuProcs picks the GOMAXPROCS budget for this process. The trial host may
// expose many CPUs while the container's CPU quota is far smaller; Go's
// default (one P per visible CPU) then thrashes the scheduler under burst
// load and steals cycles from everything else on the quota. Round-3/4
// interleaved measurements on this 1.5-CPU cell (full evidence in
// PROGRESS.md): with an effective budget of ~2 CPUs or less, ONE P wins the
// sequential workloads (read/write tails are scheduler stalls, not work —
// per-op cost is identical across P counts and the GC never cycles under
// SetGCPercent(400)); no workload ever benefited from more than 2 Ps (the
// only concurrent workload, clone, is client-bound). So: a readable quota
// of <= 2 CPUs -> 1; a larger quota -> 2; no readable quota -> 2 (the
// raw-frontend build measured =2 >= =1 in that configuration).
func cpuProcs() int {
	ncpu := runtime.NumCPU()
	if ncpu < 1 {
		return 1
	}
	if q, ok := cgroupCPUQuota(); ok {
		if q <= 2 {
			return 1
		}
		if ncpu < 2 {
			return ncpu
		}
		return 2
	}
	if ncpu < 2 {
		return ncpu
	}
	return 2
}

// cgroupCPUQuota returns the process's CPU quota in CPUs (quota/period)
// from cgroup v2 (cpu.max) or v1 (cpu.cfs_quota_us/cpu.cfs_period_us),
// looking at the hierarchy root and at the process's own cgroup path.
// ok=false means no finite quota could be read (unset, "max", or absent).
func cgroupCPUQuota() (float64, bool) {
	// Candidate directories: the v2 root and the process's own v2 path.
	v2dirs := []string{"/sys/fs/cgroup"}
	v1dirs := []string{"/sys/fs/cgroup/cpu"}
	if data, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			parts := strings.SplitN(line, ":", 3)
			if len(parts) != 3 || parts[2] == "/" || parts[2] == "" {
				continue
			}
			if parts[1] == "" { // v2 line: "0::/path"
				v2dirs = append(v2dirs, "/sys/fs/cgroup"+parts[2])
			}
			if strings.Contains(parts[1], "cpu") && !strings.Contains(parts[1], "cpuset") {
				v1dirs = append(v1dirs, "/sys/fs/cgroup/cpu"+parts[2])
			}
		}
	}
	for _, d := range v2dirs {
		b, err := os.ReadFile(d + "/cpu.max")
		if err != nil {
			continue
		}
		f := strings.Fields(string(b))
		if len(f) == 2 && f[0] != "max" {
			quota, e1 := strconv.ParseFloat(f[0], 64)
			period, e2 := strconv.ParseFloat(f[1], 64)
			if e1 == nil && e2 == nil && quota > 0 && period > 0 {
				return quota / period, true
			}
		}
	}
	for _, d := range v1dirs {
		qb, e1 := os.ReadFile(d + "/cpu.cfs_quota_us")
		pb, e2 := os.ReadFile(d + "/cpu.cfs_period_us")
		if e1 != nil || e2 != nil {
			continue
		}
		quota, err1 := strconv.ParseFloat(strings.TrimSpace(string(qb)), 64)
		period, err2 := strconv.ParseFloat(strings.TrimSpace(string(pb)), 64)
		if err1 == nil && err2 == nil && quota > 0 && period > 0 {
			return quota / period, true
		}
	}
	return 0, false
}

func main() {
	procs := cpuProcs()
	runtime.GOMAXPROCS(procs)
	fmt.Fprintf(os.Stderr, "[forge] GOMAXPROCS=%d (NumCPU=%d)\n", procs, runtime.NumCPU())
	debug.SetGCPercent(400)
	token = os.Getenv("MAKEFUN_SHOP_TOKEN")
	s := newStore()
	handler := newMux(s)

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "[forge] listen:", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Fprintf(os.Stderr, "[forge] listening on port %d\n", port)

	shutdown := startServer(ln, handler)

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
	_ = shutdown(ctx)
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

// Controller is the hub of netcat. Agents running in any number of
// clusters dial out to it, register, and keep an SSE connection open.
// When a user submits a probe in the UI the controller fans the job
// out over those connections and streams results back to the browser
// as they arrive.
package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/*
var webFS embed.FS

// agentResult is one row in the UI: the outcome of a single probe from
// a single node.
type agentResult struct {
	Cluster    string  `json:"cluster"`
	Node       string  `json:"node"`
	Pod        string  `json:"pod"`
	Host       string  `json:"host"`
	Port       int     `json:"port"`
	Proto      string  `json:"proto"`
	OK         bool    `json:"ok"`
	LatencyMs  float64 `json:"latency_ms"`
	Error      string  `json:"error,omitempty"`
	ResolvedIP string  `json:"resolved_ip,omitempty"`
}

type config struct {
	ListenAddr     string
	ProbeTimeoutMs int
	AgentTimeout   time.Duration
	HubToken       string
}

func loadConfig() config {
	return config{
		ListenAddr:     getenv("LISTEN_ADDR", ":8080"),
		ProbeTimeoutMs: getenvInt("PROBE_TIMEOUT_MS", 3000),
		AgentTimeout:   time.Duration(getenvInt("AGENT_TIMEOUT_MS", 8000)) * time.Millisecond,
		HubToken:       getenv("HUB_TOKEN", ""),
	}
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func getenvInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

type server struct {
	cfg config
	hub *hub
}

func (s *server) handleClusters(w http.ResponseWriter, _ *http.Request) {
	type item struct {
		Name  string `json:"name"`
		Nodes int    `json:"nodes"`
	}
	out := make([]item, 0)
	for name, n := range s.hub.clusters() {
		out = append(out, item{Name: name, Nodes: n})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// handleCheck streams results over Server-Sent Events so the UI can
// render rows as each node responds rather than waiting on the slowest
// probe. Query params: host, port, proto (tcp|udp).
func (s *server) handleCheck(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	host := strings.TrimSpace(q.Get("host"))
	port, _ := strconv.Atoi(q.Get("port"))
	proto := q.Get("proto")
	if proto == "" {
		proto = "tcp"
	}
	if host == "" || port <= 0 || port > 65535 {
		http.Error(w, "host and port (1-65535) are required", http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	var mu sync.Mutex
	emit := func(event string, payload any) {
		b, _ := json.Marshal(payload)
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}

	emit("start", map[string]any{"host": host, "port": port, "proto": proto})

	totals := struct {
		Targets int `json:"targets"`
		OK      int `json:"ok"`
		Fail    int `json:"fail"`
	}{}
	var totalsMu sync.Mutex

	writeResult := func(res agentResult) {
		totalsMu.Lock()
		if res.OK {
			totals.OK++
		} else {
			totals.Fail++
		}
		totalsMu.Unlock()
		emit("result", res)
	}

	agents := s.hub.snapshot()
	if len(agents) == 0 {
		emit("done", totals)
		return
	}

	perCluster := map[string]int{}
	for _, a := range agents {
		perCluster[a.Cluster]++
	}
	for name, n := range perCluster {
		emit("cluster_ready", map[string]any{"cluster": name, "nodes": n})
	}
	totalsMu.Lock()
	totals.Targets = len(agents)
	totalsMu.Unlock()

	ctx := r.Context()
	var wg sync.WaitGroup
	for _, a := range agents {
		wg.Add(1)
		go func(a *hubAgent) {
			defer wg.Done()
			ch, cancel := s.hub.dispatch(a, host, port, proto, s.cfg.ProbeTimeoutMs)
			select {
			case res := <-ch:
				writeResult(res)
			case <-time.After(s.cfg.AgentTimeout):
				cancel()
				writeResult(agentResult{
					Cluster: a.Cluster, Node: a.Node, Pod: a.Pod,
					Host: host, Port: port, Proto: proto,
					Error: "agent response timeout",
				})
			case <-ctx.Done():
				cancel()
			}
		}(a)
	}
	wg.Wait()
	emit("done", totals)
}

func (s *server) handleUI(w http.ResponseWriter, r *http.Request) {
	sub, _ := fs.Sub(webFS, "web")
	if r.URL.Path == "/" {
		r.URL.Path = "/index.html"
	}
	http.FileServer(http.FS(sub)).ServeHTTP(w, r)
}

func main() {
	cfg := loadConfig()
	s := &server{cfg: cfg, hub: newHub(cfg.HubToken)}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/clusters", s.handleClusters)
	mux.HandleFunc("/api/check", s.handleCheck)
	mux.HandleFunc("/api/agent/stream", s.hub.handleStream)
	mux.HandleFunc("/api/agent/result", s.hub.handleResult)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("/", s.handleUI)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("netcat-controller listening on %s (token required: %t)", cfg.ListenAddr, cfg.HubToken != "")
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

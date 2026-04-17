// Controller is a single deployment that:
//   1. Discovers netcat-agent pods across one or more Kubernetes clusters.
//   2. Fans a connectivity check out to every agent in parallel.
//   3. Serves a web UI that streams results back to the browser via SSE.
package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

//go:embed web/*
var webFS embed.FS

// clusterClient bundles a Kubernetes clientset with the human-friendly
// name we want to show in the UI.
type clusterClient struct {
	Name   string
	Client *kubernetes.Clientset
}

// agentResult is one row in the UI: the outcome of a single probe from a
// single node in a single cluster.
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
	AgentNamespace string
	AgentLabel     string
	AgentPort      int
	KubeconfigDir  string
	LocalCluster   string
	ProbeTimeoutMs int
	HTTPTimeout    time.Duration
	HubEnabled     bool
	HubToken       string
}

func loadConfig() config {
	c := config{
		ListenAddr:     getenv("LISTEN_ADDR", ":8080"),
		AgentNamespace: getenv("AGENT_NAMESPACE", "netcat"),
		AgentLabel:     getenv("AGENT_LABEL", "app=netcat-agent"),
		AgentPort:      getenvInt("AGENT_PORT", 8080),
		KubeconfigDir:  getenv("KUBECONFIG_DIR", "/etc/netcat/kubeconfigs"),
		LocalCluster:   getenv("LOCAL_CLUSTER_NAME", "local"),
		ProbeTimeoutMs: getenvInt("PROBE_TIMEOUT_MS", 3000),
		HTTPTimeout:    time.Duration(getenvInt("HTTP_TIMEOUT_MS", 8000)) * time.Millisecond,
		HubEnabled:     getenv("HUB_ENABLED", "") == "true",
		HubToken:       getenv("HUB_TOKEN", ""),
	}
	return c
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

// loadClusters reads every kubeconfig file from KubeconfigDir and, when
// running inside Kubernetes, also loads the in-cluster config under the
// name LocalCluster. Each kubeconfig file is treated as one cluster and
// its filename (minus extension) is used as the cluster's display name.
func loadClusters(cfg config) ([]clusterClient, error) {
	var clusters []clusterClient
	seen := map[string]bool{}

	if inc, err := rest.InClusterConfig(); err == nil {
		cs, err := kubernetes.NewForConfig(inc)
		if err != nil {
			return nil, fmt.Errorf("in-cluster client: %w", err)
		}
		clusters = append(clusters, clusterClient{Name: cfg.LocalCluster, Client: cs})
		seen[cfg.LocalCluster] = true
		log.Printf("loaded in-cluster config as %q", cfg.LocalCluster)
	}

	entries, err := os.ReadDir(cfg.KubeconfigDir)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("kubeconfig dir %s does not exist, skipping", cfg.KubeconfigDir)
			return clusters, nil
		}
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		path := filepath.Join(cfg.KubeconfigDir, e.Name())
		kc, err := clientcmd.BuildConfigFromFlags("", path)
		if err != nil {
			log.Printf("skip %s: %v", path, err)
			continue
		}
		cs, err := kubernetes.NewForConfig(kc)
		if err != nil {
			log.Printf("skip %s: %v", path, err)
			continue
		}
		name := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		if seen[name] {
			name = name + "-" + e.Name()
		}
		seen[name] = true
		clusters = append(clusters, clusterClient{Name: name, Client: cs})
		log.Printf("loaded kubeconfig %q from %s", name, path)
	}

	if len(clusters) == 0 && !cfg.HubEnabled {
		return nil, fmt.Errorf("no clusters configured (no in-cluster config and no kubeconfigs in %s); set HUB_ENABLED=true to run in hub-only mode", cfg.KubeconfigDir)
	}
	return clusters, nil
}

// listAgents returns all ready agent pods in the given cluster.
func listAgents(ctx context.Context, c clusterClient, cfg config) ([]corev1.Pod, error) {
	list, err := c.Client.CoreV1().Pods(cfg.AgentNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: cfg.AgentLabel,
	})
	if err != nil {
		return nil, err
	}
	out := make([]corev1.Pod, 0, len(list.Items))
	for _, p := range list.Items {
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		ready := false
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Ready {
				ready = true
				break
			}
		}
		if ready {
			out = append(out, p)
		}
	}
	return out, nil
}

// probeAgent calls the /check endpoint on a specific agent pod through
// the Kubernetes API server's pod proxy. That way the controller never
// needs direct network access to agent pods, which is what makes cross-
// cluster probing viable: the only thing the controller needs is an API
// server URL and credentials (the kubeconfig).
func probeAgent(ctx context.Context, c clusterClient, cfg config, pod corev1.Pod, host string, port int, proto string) agentResult {
	res := agentResult{
		Cluster: c.Name,
		Node:    pod.Spec.NodeName,
		Pod:     pod.Name,
		Host:    host,
		Port:    port,
		Proto:   proto,
	}

	body, _ := json.Marshal(map[string]any{
		"host":       host,
		"port":       port,
		"proto":      proto,
		"timeout_ms": cfg.ProbeTimeoutMs,
	})

	req := c.Client.CoreV1().RESTClient().Post().
		Namespace(cfg.AgentNamespace).
		Resource("pods").
		SubResource("proxy").
		Name(fmt.Sprintf("%s:%d", pod.Name, cfg.AgentPort)).
		Suffix("check").
		Body(bytes.NewReader(body))

	ctx, cancel := context.WithTimeout(ctx, cfg.HTTPTimeout)
	defer cancel()

	raw, err := req.DoRaw(ctx)
	if err != nil {
		res.OK = false
		res.Error = "agent unreachable: " + err.Error()
		return res
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		res.Error = "bad agent response: " + err.Error()
	}
	// The agent fills Node itself from downward API, but if it was
	// empty for some reason fall back to the pod's scheduled node.
	if res.Node == "" {
		res.Node = pod.Spec.NodeName
	}
	res.Cluster = c.Name
	res.Pod = pod.Name
	return res
}

// ---- HTTP handlers ---------------------------------------------------------

type server struct {
	cfg      config
	clusters []clusterClient
	hub      *hub
}

func (s *server) handleClusters(w http.ResponseWriter, _ *http.Request) {
	type item struct {
		Name  string `json:"name"`
		Nodes int    `json:"nodes"`
		Mode  string `json:"mode"`
		Error string `json:"error,omitempty"`
	}
	out := make([]item, 0, len(s.clusters))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, c := range s.clusters {
		it := item{Name: c.Name, Mode: "kubeconfig"}
		pods, err := listAgents(ctx, c, s.cfg)
		if err != nil {
			it.Error = err.Error()
		} else {
			it.Nodes = len(pods)
		}
		out = append(out, it)
	}
	if s.hub != nil {
		for name, n := range s.hub.clusters() {
			out = append(out, item{Name: name, Nodes: n, Mode: "hub"})
		}
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

	emit := func(event string, payload any) {
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}

	ctx := r.Context()
	emit("start", map[string]any{"host": host, "port": port, "proto": proto})

	var wg sync.WaitGroup
	var mu sync.Mutex
	totals := struct {
		Targets int `json:"targets"`
		OK      int `json:"ok"`
		Fail    int `json:"fail"`
	}{}

	writeResult := func(res agentResult) {
		mu.Lock()
		defer mu.Unlock()
		if res.OK {
			totals.OK++
		} else {
			totals.Fail++
		}
		b, _ := json.Marshal(res)
		fmt.Fprintf(w, "event: result\ndata: %s\n\n", b)
		flusher.Flush()
	}

	for _, c := range s.clusters {
		pods, err := listAgents(ctx, c, s.cfg)
		if err != nil {
			emit("cluster_error", map[string]any{"cluster": c.Name, "error": err.Error()})
			continue
		}
		emit("cluster_ready", map[string]any{"cluster": c.Name, "nodes": len(pods)})
		mu.Lock()
		totals.Targets += len(pods)
		mu.Unlock()

		for _, pod := range pods {
			wg.Add(1)
			go func(cc clusterClient, p corev1.Pod) {
				defer wg.Done()
				writeResult(probeAgent(ctx, cc, s.cfg, p, host, port, proto))
			}(c, pod)
		}
	}

	if s.hub != nil {
		hubAgents := s.hub.snapshot()
		perCluster := map[string]int{}
		for _, a := range hubAgents {
			perCluster[a.Cluster]++
		}
		for name, n := range perCluster {
			emit("cluster_ready", map[string]any{"cluster": name, "nodes": n})
		}
		mu.Lock()
		totals.Targets += len(hubAgents)
		mu.Unlock()

		for _, a := range hubAgents {
			wg.Add(1)
			go func(a *hubAgent) {
				defer wg.Done()
				ch, cancel := s.hub.dispatch(a, host, port, proto, s.cfg.ProbeTimeoutMs)
				select {
				case res := <-ch:
					writeResult(res)
				case <-time.After(s.cfg.HTTPTimeout):
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
	clusters, err := loadClusters(cfg)
	if err != nil {
		log.Fatal(err)
	}

	s := &server{cfg: cfg, clusters: clusters}
	if cfg.HubEnabled {
		s.hub = newHub(cfg.HubToken)
		log.Printf("hub mode enabled (token required: %t)", cfg.HubToken != "")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/clusters", s.handleClusters)
	mux.HandleFunc("/api/check", s.handleCheck)
	if s.hub != nil {
		mux.HandleFunc("/api/agent/stream", s.hub.handleStream)
		mux.HandleFunc("/api/agent/result", s.hub.handleResult)
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("/", s.handleUI)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("netcat-controller listening on %s (clusters=%d)", cfg.ListenAddr, len(clusters))
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

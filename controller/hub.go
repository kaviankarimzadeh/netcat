// Hub mode. Agents that can't (or shouldn't) be reached through the
// Kubernetes API server instead dial OUT to the controller and keep an
// SSE connection open. The controller dispatches probe jobs down that
// connection; agents POST the results back. No kubeconfig required —
// only outbound HTTPS from the agent pods to the controller.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type hubJob struct {
	ID        string `json:"id"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Proto     string `json:"proto"`
	TimeoutMs int    `json:"timeout_ms"`
}

type hubAgent struct {
	ID      string
	Cluster string
	Node    string
	Pod     string
	Send    chan hubJob
}

type hub struct {
	token   string
	mu      sync.RWMutex
	agents  map[string]*hubAgent        // agentID -> agent
	results map[string]chan agentResult // jobID  -> result channel
}

func newHub(token string) *hub {
	return &hub{
		token:   token,
		agents:  make(map[string]*hubAgent),
		results: make(map[string]chan agentResult),
	}
}

func randID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (h *hub) authorized(r *http.Request) bool {
	if h.token == "" {
		return true
	}
	return r.Header.Get("Authorization") == "Bearer "+h.token
}

func (h *hub) snapshot() []*hubAgent {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*hubAgent, 0, len(h.agents))
	for _, a := range h.agents {
		out = append(out, a)
	}
	return out
}

// clusters returns a map of clusterName -> agent count for UI rendering.
func (h *hub) clusters() map[string]int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	m := map[string]int{}
	for _, a := range h.agents {
		m[a.Cluster]++
	}
	return m
}

func (h *hub) register(a *hubAgent) {
	h.mu.Lock()
	h.agents[a.ID] = a
	h.mu.Unlock()
}

func (h *hub) deregister(id string) {
	h.mu.Lock()
	delete(h.agents, id)
	h.mu.Unlock()
}

// dispatch sends a job to a single agent and returns a channel on which
// the result will arrive. The caller MUST call cancel() when it's done
// waiting (e.g. after timeout) so the result map doesn't leak.
func (h *hub) dispatch(a *hubAgent, host string, port int, proto string, timeoutMs int) (<-chan agentResult, func()) {
	jobID := randID()
	ch := make(chan agentResult, 1)

	h.mu.Lock()
	h.results[jobID] = ch
	h.mu.Unlock()

	cancel := func() {
		h.mu.Lock()
		delete(h.results, jobID)
		h.mu.Unlock()
	}

	select {
	case a.Send <- hubJob{ID: jobID, Host: host, Port: port, Proto: proto, TimeoutMs: timeoutMs}:
	default:
		// Agent send buffer is full — treat as a transient failure
		// rather than blocking the whole probe.
		cancel()
		fakeCh := make(chan agentResult, 1)
		fakeCh <- agentResult{
			Cluster: a.Cluster, Node: a.Node, Pod: a.Pod,
			Host: host, Port: port, Proto: proto,
			Error: "agent busy (send queue full)",
		}
		return fakeCh, func() {}
	}
	return ch, cancel
}

// handleStream: agent side of the reverse tunnel.
//   GET /api/agent/stream?cluster=<>&node=<>&pod=<>
//   Authorization: Bearer <HUB_TOKEN>
// Server responds with text/event-stream; each "job" event is a JSON
// hubJob the agent should execute.
func (h *hub) handleStream(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	q := r.URL.Query()
	cluster := q.Get("cluster")
	node := q.Get("node")
	pod := q.Get("pod")
	if cluster == "" || node == "" {
		http.Error(w, "cluster and node are required", http.StatusBadRequest)
		return
	}

	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	a := &hubAgent{
		ID:      randID(),
		Cluster: cluster,
		Node:    node,
		Pod:     pod,
		Send:    make(chan hubJob, 32),
	}
	h.register(a)
	defer h.deregister(a.ID)

	fmt.Fprintf(w, "event: hello\ndata: {\"agent_id\":%q}\n\n", a.ID)
	fl.Flush()

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case j := <-a.Send:
			b, _ := json.Marshal(j)
			fmt.Fprintf(w, "event: job\ndata: %s\n\n", b)
			fl.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

type hubResultBody struct {
	JobID      string  `json:"job_id"`
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

// handleResult: agents POST probe results here.
func (h *hub) handleResult(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var body hubResultBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.mu.Lock()
	ch, ok := h.results[body.JobID]
	if ok {
		delete(h.results, body.JobID)
	}
	h.mu.Unlock()
	if !ok {
		// Probe timed out on the controller side before the agent
		// replied. Silently drop.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ch <- agentResult{
		Cluster: body.Cluster, Node: body.Node, Pod: body.Pod,
		Host: body.Host, Port: body.Port, Proto: body.Proto,
		OK: body.OK, LatencyMs: body.LatencyMs, Error: body.Error, ResolvedIP: body.ResolvedIP,
	}
	w.WriteHeader(http.StatusNoContent)
}

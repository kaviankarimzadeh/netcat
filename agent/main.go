// Agent runs as a DaemonSet on every node. It dials outbound to the
// controller, keeps an SSE connection open, runs probe jobs pushed down
// that stream, and POSTs the results back. The only HTTP endpoint the
// agent itself exposes is /healthz for the kubelet liveness probe.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// probeRequest / probeResult are internal only; nothing is serialized over
// the wire with these types anymore. The hub wire format lives in
// hubJobMsg / hubResultMsg below.
type probeRequest struct {
	Host      string
	Port      int
	Proto     string
	TimeoutMs int
}

type probeResult struct {
	Node       string
	Host       string
	Port       int
	Proto      string
	OK         bool
	LatencyMs  float64
	Error      string
	ResolvedIP string
}

func nodeName() string {
	if n := os.Getenv("NODE_NAME"); n != "" {
		return n
	}
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "unknown"
}

func resolve(host string) string {
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return ""
	}
	return ips[0].String()
}

func probe(req probeRequest) probeResult {
	if req.Proto == "" {
		req.Proto = "tcp"
	}
	if req.TimeoutMs <= 0 {
		req.TimeoutMs = 3000
	}
	resp := probeResult{
		Node:  nodeName(),
		Host:  req.Host,
		Port:  req.Port,
		Proto: req.Proto,
	}
	resp.ResolvedIP = resolve(req.Host)

	addr := net.JoinHostPort(req.Host, strconv.Itoa(req.Port))
	timeout := time.Duration(req.TimeoutMs) * time.Millisecond
	start := time.Now()

	switch req.Proto {
	case "tcp":
		c, err := net.DialTimeout("tcp", addr, timeout)
		resp.LatencyMs = float64(time.Since(start).Microseconds()) / 1000.0
		if err != nil {
			resp.Error = err.Error()
			return resp
		}
		_ = c.Close()
		resp.OK = true
	case "udp":
		// UDP is connectionless; we can only detect a handful of error
		// cases (ICMP port-unreachable, DNS failure). A "successful"
		// send is the best we can report without an app-level probe.
		c, err := net.DialTimeout("udp", addr, timeout)
		if err != nil {
			resp.LatencyMs = float64(time.Since(start).Microseconds()) / 1000.0
			resp.Error = err.Error()
			return resp
		}
		_ = c.SetDeadline(time.Now().Add(timeout))
		if _, err := c.Write([]byte{0}); err != nil {
			resp.Error = err.Error()
		} else {
			resp.OK = true
		}
		_ = c.Close()
		resp.LatencyMs = float64(time.Since(start).Microseconds()) / 1000.0
	default:
		resp.Error = "unsupported proto: " + req.Proto
	}
	return resp
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	fmt.Fprintf(w, "ok %s\n", nodeName())
}

// ---- hub (reverse tunnel) client ------------------------------------------

type hubJobMsg struct {
	ID        string `json:"id"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Proto     string `json:"proto"`
	TimeoutMs int    `json:"timeout_ms"`
}

type hubResultMsg struct {
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

func runHubClient(hubURL, token, cluster, pod string) {
	backoff := time.Second
	for {
		err := hubConnect(hubURL, token, cluster, pod)
		if err != nil {
			log.Printf("hub connection closed: %v (retrying in %s)", err, backoff)
		}
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func hubConnect(hubURL, token, cluster, pod string) error {
	u, err := url.Parse(hubURL)
	if err != nil {
		return fmt.Errorf("bad HUB_URL: %w", err)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/agent/stream"
	q := u.Query()
	q.Set("cluster", cluster)
	q.Set("node", nodeName())
	q.Set("pod", pod)
	u.RawQuery = q.Encode()

	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("hub returned %d: %s", resp.StatusCode, string(body))
	}
	log.Printf("connected to hub %s as cluster=%s node=%s", hubURL, cluster, nodeName())

	// Minimal SSE parser: accumulate "event:" and "data:" lines,
	// dispatch on blank line.
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var event, data string
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if event == "job" && data != "" {
				var j hubJobMsg
				if err := json.Unmarshal([]byte(data), &j); err == nil {
					go hubRunJob(hubURL, token, cluster, pod, j)
				}
			}
			event, data = "", ""
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // comment / keepalive
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(line[len("event:"):])
		}
		if strings.HasPrefix(line, "data:") {
			data = strings.TrimSpace(line[len("data:"):])
		}
	}
	return sc.Err()
}

func hubRunJob(hubURL, token, cluster, pod string, j hubJobMsg) {
	res := probe(probeRequest{
		Host:      j.Host,
		Port:      j.Port,
		Proto:     j.Proto,
		TimeoutMs: j.TimeoutMs,
	})
	msg := hubResultMsg{
		JobID:      j.ID,
		Cluster:    cluster,
		Node:       res.Node,
		Pod:        pod,
		Host:       res.Host,
		Port:       res.Port,
		Proto:      res.Proto,
		OK:         res.OK,
		LatencyMs:  res.LatencyMs,
		Error:      res.Error,
		ResolvedIP: res.ResolvedIP,
	}
	body, _ := json.Marshal(msg)

	u, err := url.Parse(hubURL)
	if err != nil {
		return
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/agent/result"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("hub result POST failed: %v", err)
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func main() {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealth)

	hubURL := os.Getenv("HUB_URL")
	if hubURL != "" {
		u, err := url.Parse(hubURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			log.Fatalf("HUB_URL must be a full URL with scheme (http:// or https://), got %q", hubURL)
		}
		cluster := os.Getenv("HUB_CLUSTER")
		if cluster == "" {
			cluster = "unknown"
		}
		token := os.Getenv("HUB_TOKEN")
		pod := os.Getenv("POD_NAME")
		go runHubClient(hubURL, token, cluster, pod)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("netcat-agent listening on %s (node=%s, hub=%t)", addr, nodeName(), hubURL != "")
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

# netcat — multi-cluster node-level connectivity probe

A tiny tool that answers the question:

> "Can **every node** of **every one of my clusters** reach `host:port`?"

It ships as two binaries:

| Component    | What it is                           | Where it runs                                 |
| ------------ | ------------------------------------ | --------------------------------------------- |
| `agent`      | DaemonSet with `hostNetwork: true`   | Every node of every cluster you want to probe |
| `controller` | Deployment + web UI (SSE streaming)  | One "hub" cluster                             |

Agents dial **out** to the controller and keep a persistent SSE
connection open. The controller pushes probe jobs down that tunnel;
agents POST the results back. No kubeconfigs, no API-server access
across clusters, no cross-cluster networking — just outbound HTTPS
from the agent pods to a single URL, authenticated with a shared
bearer token.

```
                Browser
                   │
                   ▼
              controller  ◄── SSE ── agents (DaemonSet)
               (UI + hub)                  in every cluster
                                           dialing OUT over HTTPS
                                           with a shared bearer token
```

## Install with Helm

One chart, two install invocations.

### 1. Generate a shared token

```bash
export NETCAT_TOKEN=$(openssl rand -base64 32)
```

### 2. Install the controller on the hub cluster

```bash
helm install netcat ./charts/netcat \
  --namespace netcat --create-namespace \
  --set agent.enabled=false \
  --set controller.enabled=true \
  --set controller.hub.token="$NETCAT_TOKEN" \
  --set-json 'controller.httpRoute.parentRefs=[{"name":"gateway","namespace":"gateway"}]' \
  --set-json 'controller.httpRoute.hostnames=["netcat.example.com"]'
```

> HTTPRoute (Gateway API v1) is the default. If your cluster doesn't
> run the Gateway API, disable it and use Ingress instead — see
> [Exposing the UI](#exposing-the-ui).

### 3. Install the agent on every source cluster

```bash
helm install netcat-agent ./charts/netcat \
  --namespace netcat --create-namespace \
  --set controller.enabled=false \
  --set agent.enabled=true \
  --set agent.hub.url=https://netcat.example.com \
  --set agent.hub.cluster=prod-eu \
  --set agent.hub.token="$NETCAT_TOKEN"
```

`agent.hub.cluster` is whatever display name you want to see for that
cluster in the UI. Use the same `$NETCAT_TOKEN` on every cluster.

### Keeping the token out of Helm values

For anything beyond a scratch install, create the Secret yourself and
reference it:

```bash
# In every cluster (hub and sources), in the netcat namespace:
kubectl -n netcat create secret generic netcat-hub-token \
  --from-literal=token="$NETCAT_TOKEN"
```

Then install without the inline token:

```bash
# hub
helm install netcat ./charts/netcat \
  --set agent.enabled=false --set controller.enabled=true \
  --set controller.hub.existingSecret=netcat-hub-token

# source clusters
helm install netcat-agent ./charts/netcat \
  --set controller.enabled=false --set agent.enabled=true \
  --set agent.hub.url=https://netcat.example.com \
  --set agent.hub.cluster=prod-eu \
  --set agent.hub.existingSecret=netcat-hub-token
```

## Exposing the UI

The chart defaults to **HTTPRoute**:

```yaml
controller:
  httpRoute:
    enabled: true
    parentRefs:
      - name: gateway
        namespace: gateway
        sectionName: https
    hostnames:
      - netcat.example.com
```

Or use Ingress instead:

```yaml
controller:
  httpRoute:
    enabled: false
  ingress:
    enabled: true
    className: nginx
    host: netcat.example.com
    tls: true
    tlsSecretName: netcat-tls
```

Or just port-forward for local access:

```bash
kubectl -n netcat port-forward svc/netcat-netcat-controller 8080:80
open http://localhost:8080
```

## Build images

Images target `linux/amd64` (Kubernetes node architecture). Built with
`docker buildx` — works fine on Apple Silicon hosts too:

```bash
make images push REGISTRY=ghcr.io/<you> TAG=v0.1.0
```

## Environment variables

### Controller

| Variable           | Default  | Purpose                                        |
| ------------------ | -------- | ---------------------------------------------- |
| `LISTEN_ADDR`      | `:8080`  | HTTP bind address                              |
| `PROBE_TIMEOUT_MS` | `3000`   | Per-probe timeout sent to the agent            |
| `AGENT_TIMEOUT_MS` | `8000`   | How long to wait for an agent to reply         |
| `HUB_TOKEN`        | _unset_  | Required bearer token (empty = no auth)        |

### Agent

| Variable      | Default | Purpose                                              |
| ------------- | ------- | ---------------------------------------------------- |
| `LISTEN_ADDR` | `:8080` | HTTP bind address (health probes only)               |
| `NODE_NAME`   |   —     | Downward-API-supplied node id                        |
| `POD_NAME`    |   —     | Downward-API-supplied pod name (shown in UI)         |
| `HUB_URL`     |   —     | Controller URL the agent dials out to                |
| `HUB_CLUSTER` |   —     | Cluster display name reported to the hub             |
| `HUB_TOKEN`   |   —     | Bearer token sent to the hub                         |

## How a probe works

When the user submits a probe in the UI:

1. Browser opens an SSE stream to `GET /api/check?host=…&port=…&proto=…`.
2. Controller snapshots the set of connected agents and dispatches one
   job per agent down each SSE tunnel.
3. Each agent runs the probe in its own node's netns and POSTs the
   result to `/api/agent/result`.
4. Controller forwards each result to the browser as soon as it
   arrives.

Agent probe semantics:

- `tcp`: a full `connect()` (success means the three-way handshake
  completed). Reports latency.
- `udp`: best-effort — can only detect local errors and ICMP
  port-unreachable responses. A successful datagram send does **not**
  prove the peer received it.
- DNS resolution happens in the agent's netns, so it reflects the
  node's DNS setup.

## Security notes

- The SSE endpoint and result endpoint both require
  `Authorization: Bearer $HUB_TOKEN`. Leaving the token empty disables
  authentication; don't do that with a publicly reachable controller.
- Use TLS on the controller's Ingress/HTTPRoute. The token travels on
  every agent request — plain HTTP would leak it.
- Rotate the token by updating the Secret on every cluster and
  restarting the DaemonSets and controller.

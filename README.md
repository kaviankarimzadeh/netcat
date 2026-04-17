# netcat — multi-cluster node-level connectivity probe

A tiny tool that answers the question:

> "Can **every node** of **every one of my clusters** reach `host:port`?"

It ships as two binaries:

| Component    | What it is                           | Where it runs                                 |
| ------------ | ------------------------------------ | --------------------------------------------- |
| `agent`      | DaemonSet with `hostNetwork: true`   | Every node of every cluster you want to probe |
| `controller` | Deployment + web UI (SSE streaming)  | One "hub" cluster                             |

It supports **two deployment modes**, which you can mix and match:

1. **Kubeconfig mode** — the controller reaches each agent through the
   Kubernetes API server's pod-proxy. Give the controller a kubeconfig
   per cluster; no cross-cluster networking needed.
2. **Hub mode** — agents dial **out** to the controller and keep an SSE
   connection open. No kubeconfigs, no API-server access from the
   controller side. Just outbound HTTPS from the agents to a single
   URL. Ideal for clusters behind NAT, strict egress, or when you
   simply don't want to manage kubeconfigs.

```
  Kubeconfig mode                      Hub mode
  ───────────────                      ────────

         Browser                         Browser
           │                               │
           ▼                               ▼
      controller ──kubeconfig──┐      controller ◄──SSE── agents
           │                    │           ▲              (dial out
      pods/proxy            pods/proxy      │               from every
           │                    │           │               cluster)
      agents (DS)           agents (DS)     │
                                       (no K8s creds,
                                        just one shared
                                        bearer token)
```

## Install with Helm

A single chart ships both components, toggled with flags.

### Hub mode (no kubeconfigs — recommended)

On the **hub** cluster (controller + UI):

```bash
helm install netcat ./charts/netcat \
  --namespace netcat --create-namespace \
  --set agent.enabled=false \
  --set controller.enabled=true \
  --set controller.kubeconfig.enabled=false \
  --set controller.hub.enabled=true \
  --set controller.hub.token=$SHARED_TOKEN \
  --set-json 'controller.httpRoute.parentRefs=[{"name":"gateway","namespace":"gateway"}]' \
  --set-json 'controller.httpRoute.hostnames=["netcat.example.com"]'
```

> HTTPRoute is enabled by default. If you don't have the Gateway API
> installed or would rather use an Ingress, disable HTTPRoute and enable
> Ingress instead (see [Exposing the UI](#exposing-the-ui) below).

On **every source cluster** (agent DaemonSet, dials out):

```bash
helm install netcat-agent ./charts/netcat \
  --namespace netcat --create-namespace \
  --set controller.enabled=false \
  --set agent.enabled=true \
  --set agent.hub.enabled=true \
  --set agent.hub.url=https://netcat.example.com \
  --set agent.hub.cluster=prod-eu \
  --set agent.hub.token=$SHARED_TOKEN
```

That's it. No kubeconfigs, no API-server access across clusters. Open
`https://netcat.example.com` and every connected cluster/node will show
up in the UI.

### Kubeconfig mode

On every **source** cluster:

```bash
helm install netcat-agent ./charts/netcat \
  --namespace netcat --create-namespace \
  --set controller.enabled=false \
  --set agent.enabled=true \
  --set agent.remoteAccess.enabled=true
```

On the **hub** cluster:

```bash
helm install netcat ./charts/netcat \
  --namespace netcat --create-namespace \
  --set agent.enabled=false \
  --set controller.enabled=true
```

Then mint a kubeconfig per remote cluster and load it into the Secret
the controller mounts. On each remote cluster, point `kubectl` at it,
`helm install` the agent, then build a kubeconfig from the created SA
token:

```bash
# run on each remote cluster, with kubectl pointing at it
NAME=prod-eu
SERVER=$(kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.server}')
CA=$(kubectl -n netcat get secret netcat-remote-token -o jsonpath='{.data.ca\.crt}')
TOKEN=$(kubectl -n netcat get secret netcat-remote-token -o jsonpath='{.data.token}' | base64 -d)
cat > /tmp/kc-$NAME.yaml <<EOF
apiVersion: v1
kind: Config
clusters:  [{name: $NAME, cluster: {server: $SERVER, certificate-authority-data: $CA}}]
users:     [{name: netcat-remote, user: {token: $TOKEN}}]
contexts:  [{name: $NAME, context: {cluster: $NAME, user: netcat-remote, namespace: netcat}}]
current-context: $NAME
EOF
```

Then load all the kubeconfigs into the controller's Secret on the hub:

```bash
kubectl config use-context hub
kubectl -n netcat create secret generic netcat-kubeconfigs \
  --from-file=prod-eu=/tmp/kc-prod-eu.yaml \
  --from-file=prod-us=/tmp/kc-prod-us.yaml \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl -n netcat rollout restart deploy/netcat-netcat-controller
```

The file's key (`prod-eu`, `prod-us`, …) becomes the cluster's display
name in the UI.

### Exposing the UI

The chart defaults to **HTTPRoute** (Gateway API v1). Point it at an
existing Gateway with the `parentRefs` value. Example:

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

If your cluster doesn't run the Gateway API, disable `httpRoute` and
use an Ingress instead:

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

Or, for a quick local check, just port-forward:

```bash
kubectl -n netcat port-forward svc/netcat-netcat-controller 8080:80
open http://localhost:8080
```

## Build images

Images target `linux/amd64` explicitly (Kubernetes node architecture).
Build with `docker buildx` — works on Apple Silicon too:

```bash
make images push REGISTRY=ghcr.io/<you> TAG=v0.1.0
```

## Local development

Run the controller against your current kube context, no container
build needed:

```bash
make run-local
```

Then visit http://localhost:8080. It will show a single cluster named
after your `kubectl` context, with one row per node where the agent
DaemonSet is running.

## Environment variables (controller)

| Variable             | Default                   | Purpose                                      |
| -------------------- | ------------------------- | -------------------------------------------- |
| `LISTEN_ADDR`        | `:8080`                   | HTTP bind address                            |
| `AGENT_NAMESPACE`    | `netcat`                  | Namespace where agents run                   |
| `AGENT_LABEL`        | `app=netcat-agent`        | Label selector for agent pods                |
| `AGENT_PORT`         | `8080`                    | Port the agent listens on                    |
| `KUBECONFIG_DIR`     | `/etc/netcat/kubeconfigs` | Directory of per-cluster kubeconfig files    |
| `LOCAL_CLUSTER_NAME` | `local`                   | Display name for the in-cluster context      |
| `PROBE_TIMEOUT_MS`   | `3000`                    | Per-probe timeout sent to the agent          |
| `HTTP_TIMEOUT_MS`    | `8000`                    | Controller → API server / agent timeout      |
| `HUB_ENABLED`        | _unset_                   | Set to `true` to accept inbound agent tunnels |
| `HUB_TOKEN`          | _unset_                   | Shared bearer token agents must present      |

## Environment variables (agent)

| Variable      | Default | Purpose                                              |
| ------------- | ------- | ---------------------------------------------------- |
| `LISTEN_ADDR` | `:8080` | HTTP bind address                                    |
| `NODE_NAME`   |   —     | Downward-API-supplied node id                        |
| `POD_NAME`    |   —     | Downward-API-supplied pod name (shown in UI)         |
| `HUB_URL`     | _unset_ | If set, agent dials out and enters hub mode         |
| `HUB_CLUSTER` | _unset_ | Cluster display name reported to the hub             |
| `HUB_TOKEN`   | _unset_ | Bearer token sent to the hub (must match controller) |

## How the probe works

Agent endpoint (called by the controller via pod-proxy):

```
POST /check
{ "host": "api.example.com", "port": 443, "proto": "tcp", "timeout_ms": 3000 }

→ {
    "node": "ip-10-0-1-23",
    "host": "api.example.com",
    "port": 443,
    "proto": "tcp",
    "ok": true,
    "latency_ms": 14.2,
    "resolved_ip": "54.12.34.56"
  }
```

- `tcp`: a full TCP `connect()` (success means the three-way handshake
  completed).
- `udp`: best-effort — we can only detect hard local errors and ICMP
  port-unreachable responses; a successful datagram send does **not**
  prove the peer received it. Use a real protocol probe for UDP
  services when correctness matters.
- DNS resolution happens in the agent's netns (so it reflects the
  node's DNS setup, not the controller's).

## UI

Dark, glassy, streaming. As each node replies the result slides into
its cluster's card with a success/failure dot and the measured latency.

## Notes

- The `hostNetwork: true` + `hostPort: 8080` combo on the agent means
  node port 8080 must be free. Remove `hostPort` if you don't want to
  bind the host — pod-proxy still works without it.
- RBAC for the controller is minimal: `get`/`list` on `pods` and
  `get`/`create` on `pods/proxy` in the `netcat` namespace only.

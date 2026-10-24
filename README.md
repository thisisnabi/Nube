# Nube

**The orchestrator for AI agents, and for the containers around them. Written from scratch in Go.**

Nube **runs AI agents as workloads**. You declare an agent (a model, instructions and tools), and every task it gets
runs in its own pod: the commands the model writes go to a separate sandbox container, risky tool calls wait for a
human, and every run has a budget, a full trace, memory and, if you like, a schedule. Agents can also **operate Nube**:
it has a built-in MCP server, an `agent` role whose destructive changes wait for approval, and an audit log.

Underneath is a small but real Kubernetes-like orchestrator. One binary (`nube`) is the control plane, the node daemon
and the CLI, with a web dashboard built in. It runs containers on Docker or Podman across one or many Linux machines,
networks them with WireGuard, and survives the loss of a node or a control-plane server.

[فارسی](README.fa.md) · [AGENTS.md](AGENTS.md) (for AI agents operating Nube) · [examples/agents](examples/agents)

```
  nube CLI · dashboard · Claude Code and other agents (MCP)
                   │ HTTPS · tokens with roles
  ┌───────────────────────────────────────────────────────────────────┐
  │ control plane (1 or 3 servers, Raft)                              │
  │ API · MCP server · approvals · audit log · dashboard              │
  │ scheduler · controllers · agent queue, budgets, traces            │
  └───────────────────────────────────────────────────────────────────┘
                   │                                  │   sync (mutual TLS)
  ┌───────────────────────────────┐   ┌───────────────────────────────┐
  │ node (Docker)                 │   │ node (Podman)                 │
  │ ┌ agent run pod ────────────┐ │   │ your services and databases   │
  │ │ runtime ──► Claude / LLM  │ │◄─►│ model servers (vLLM, Ollama)  │
  │ │ sandbox: model's commands │ │ WG│ more agent runs               │
  │ └───────────────────────────┘ │   └───────────────────────────────┘
  └───────────────────────────────┘
```

> Nube is built for homelabs, personal projects and learning how orchestrators really work.
> It is not a drop-in replacement for Kubernetes in production; see [Limitations](#limitations).

## Why run AI agents on Nube

| You need | Nube gives you |
|---|---|
| **Isolation** | Every run is its own pod. Commands the model writes run in a sandbox container next to the runtime, never in it, and never next to your API keys |
| **A human in the loop** | Tools marked `requireApproval` wait for `nube approve` (or a click in the dashboard); the agent hears the reason if you reject |
| **Budgets** | Step, token and time limits per run; a queue with a concurrency limit per agent |
| **Visibility** | A trace of every model call and tool call with tokens and timings (`nube trace -w`, dashboard) and an audit log of every change |
| **Any model** | Claude through the official Anthropic SDK, or any OpenAI-compatible server (vLLM, Ollama ...) running in the same cluster |
| **Real tools** | Services in the cluster over HTTP, MCP servers, a shell sandbox, other agents (delegation), and Nube itself |
| **Memory and schedules** | Conversation memory per session; cron schedules that start runs on their own |
| **An agent that runs the cluster** | Give an agent Nube's own tools (`nube: {role: view}`, or `agent` to make changes under approval) |

## AI agents in a minute

```yaml
kind: Agent
metadata: { name: ops-assistant }
spec:
  model: { name: claude-opus-5-5, apiKeySecret: anthropic }   # Secret with ANTHROPIC_API_KEY
  instructions: You help operate this cluster. Diagnose before changing anything.
  tools:
    - nube: { role: view }                       # Nube's own tools: diagnose, logs, events ...
    - name: shell
      sandbox: { image: "alpine:3.20" }          # commands run in a separate container
      requireApproval: true                      # a human approves each call
    - name: search_kb
      http: { service: kb, port: 8080, path: /search }
      inputSchema: { type: object, properties: { q: { type: string } }, required: [q] }
    - agent: { name: researcher }                # delegate to another agent (tool ask_researcher)
  memory: { enabled: true }
  limits: { maxSteps: 25, maxTokens: 400000, timeoutSeconds: 900, concurrency: 2 }
  schedules: [{ cron: "0 8 * * 1-5", input: "Summarize what needs attention today" }]
```

```bash
nube run ops-assistant "Why is web degraded?" --session alice --wait
nube trace RUN -w          # model calls, tool calls, tokens, output
nube approvals             # tool calls waiting for a human; nube approve|reject ID
nube memory ops-assistant --session alice
```

How it works: the control plane queues each run (per-agent concurrency), creates a pod with the **runtime** (an image
every node builds from its own `nube` binary) and one container per sandbox tool, and gives the runtime a token that is
valid for that run only and never stored. The runtime runs the agent loop with the official Anthropic Go SDK (or an
OpenAI-compatible client), reports every step, asks for approvals and stops at the step, token or time budget.
More in [`examples/agents`](examples/agents) and `nube explain agent`.

## Operated by AI agents

```bash
nube token create claude --role agent -n staging     # changes allowed; destructive ones wait for a human
claude mcp add nube -- nube mcp                      # Claude Code (or any MCP client) gets Nube's tools
```

The MCP tools (`cluster_overview`, `diagnose`, `get_logs`, `explain`, `apply_manifest` with `dry_run`, `scale`,
`set_image`, `rollout`, `delete_resource`, `run_agent` ...) are ordinary API calls with the caller's token: roles,
namespaces, approvals and the audit log apply. See [AGENTS.md](AGENTS.md).

## Features

| Area | What you get |
|---|---|
| **Pods** | Multiple containers sharing `localhost` (a pause container owns the network, like Kubernetes), init containers that run in order, shared `emptyDir` scratch volumes on disk or in memory with size limits |
| **Workloads** | `Deployment` with rolling updates, **automatic rollback of failed rollouts**, pause/resume and CPU autoscaling · pods **roll automatically when a ConfigMap or Secret they use changes** · start-up ordering with `dependsOn` · `StatefulSet` with stable names (`db-0`, `db-1`), ordered start-up, a volume per replica and per-pod DNS · `DaemonSet` with one pod per (selected) node and rolling updates · `Job` with completions, parallelism, backoff limit and deadline · `CronJob` with cron schedules, time zones and concurrency policy |
| **Scheduler** | Filters on readiness, cordoning, `nodeSelector`, CPU/memory requests, host ports and volume location; spreads replicas across nodes |
| **Self-healing** | Per-container restart with exponential back-off (CrashLoopBackOff), HTTP and TCP health checks, rescheduling when a node is lost |
| **Networking** | A pod subnet per node, an encrypted **WireGuard** overlay (or host-gw), direct pod-to-pod traffic |
| **Services** | **TCP and UDP**, kernel-level routing with iptables DNAT on every node (source IP preserved; a userspace proxy where the overlay is off), `headless` and `internal` services, a load balancer on every control-plane server |
| **Network policies** | `NetworkPolicy` with ingress and egress rules by pod labels, namespaces, IP blocks and ports, enforced with iptables on every node |
| **Ingress** | HTTP/HTTPS routing by host (including `*.example.com`) and path on ports 80/443, **automatic Let's Encrypt certificates** (`acme: true`, shared by all servers), TLS from a `tls` Secret or a self-signed fallback, HTTP→HTTPS redirect |
| **DNS** | `web`, `web.prod`, `web.default.svc.nube`, `db-0.db`, `POD.default.pod.nube` |
| **Storage** | `local` volumes with an **enforced size** (each is its own ext4 filesystem — writes past the size fail), `nfs` (shared, or `ReadWriteOnce` with an enforced size that follows the pod to any node) and `hostPath`; live usage per volume |
| **Snapshots and backups** | `VolumeSnapshot`s stored on (and replicated across) the control plane, scheduled backups with retention, new volumes restored from a snapshot, `nube volume move` to another node, automatic recovery onto a healthy node when a volume's node is lost |
| **Extensibility** | `CustomResourceDefinition`s with schema validation and defaults, custom objects with status, watch and garbage collection, a Go client (`pkg/client`) and an example operator |
| **Security** | **HTTPS API** with a cluster CA that clients pin by hash (or a Let's Encrypt or your own certificate for the dashboard), **mutual TLS** between servers and agents, **role-based tokens** (`admin`, `edit`, `view`, `node`) optionally scoped to namespaces, masked secret values |
| **High availability** | 3 or 5 servers replicated with **Raft**, which ships only what changed in each write; a new leader is elected within seconds and agents fail over automatically |
| **Runtimes** | Docker or Podman (through its Docker-compatible API), chosen per node |
| **Observability** | Live dashboard, events, streaming logs per container, CPU/memory/disk usage, `nube top`, **CPU/memory request recommendations** from observed usage |
| **AI agents** | `kind: Agent`: a model (Claude, or any OpenAI-compatible server such as vLLM or Ollama), instructions and tools (HTTP services, MCP servers, a sandbox container for shell commands, other agents, Nube itself). Every task is an `AgentRun` in its own pod with a full trace, token and step budgets, timeouts, a queue per agent, session memory, cron schedules and human approval for risky tools |
| **Agent-friendly** | Built-in **MCP server** (`nube mcp`), `agent` role whose destructive changes wait for **human approval**, **audit log**, `nube diagnose` (problem, evidence, fix), `--dry-run` / `nube diff`, `nube explain`, JSON errors with stable codes and exit codes |

## What you get that Kubernetes leaves to add-ons

Kubernetes is far more mature and has a huge ecosystem. But a lot of what people need day to day is not in Kubernetes
itself: you find, install, configure and upgrade a separate project for each. In Nube these are part of the one binary.

| Everyday problem | In Kubernetes | In Nube |
|---|---|---|
| A ConfigMap or Secret changes, but running pods keep the old values until something restarts them | Install Reloader, or add a hash annotation by hand | Built in: the workload rolls its pods one by one; history says `configmap/app changed` |
| A rollout's new pods crash or the image does not exist | The rollout just stops; `progressDeadlineSeconds` only marks it failed and you run `kubectl rollout undo` | Built in: after `progressDeadlineSeconds` the previous revision comes back automatically, without downtime, and an event says why |
| The API must wait for the database | No ordering; people write init containers with wait loops | `dependsOn: [db]`: the pod waits until the `db` service has a ready pod |
| What should my CPU/memory requests be? | Install the Vertical Pod Autoscaler | `nube top workloads` and the dashboard compare requests with observed usage and suggest values |
| HTTPS certificates | Install cert-manager | `acme: true` on an Ingress |
| Volume backups, restore, moving data to another node | Install Velero and a CSI snapshot controller | Built in: snapshots, scheduled backups, `restoreFrom`, `nube volume move`, `autoRecover` |
| A local volume's size is not enforced | `hostPath` and local PVs have no quota | Each local volume is its own filesystem: writes stop at its size |
| Ingress and a load balancer on bare metal | Install an ingress controller and MetalLB | Built in on every server |
| Network policies | Only with a CNI that supports them (Calico, Cilium ...) | Built in |
| Running the control plane | etcd plus several components, certificates to manage | One binary; Raft and the cluster CA are built in |
| Running AI agents | A framework in your own image, plus your own queue, budgets, traces and approval flow | `kind: Agent`: runs, traces, budgets, sandboxes, memory and approvals are built in |
| Letting an AI agent operate the cluster | A third-party MCP server with the full rights of a kubeconfig | Built-in MCP server; the `agent` role holds destructive changes for a human; every change is audited |

Where Kubernetes is the better choice: big clusters, `kubectl`/Helm/operator ecosystem compatibility, managed offerings,
and anything that needs a certified, battle-tested platform.

## What has actually been tested

On a Docker test cluster (3 HA servers with TLS + Docker-in-Docker and Podman nodes with WireGuard) and on a fresh Ubuntu 24.04 install:

- an init container wrote a page, nginx served it and a sidecar reached it over `localhost`;
- `db-1` only started once `db-0` was ready, each replica got its own volume, `db-0.db` resolved to the right pod, and a rolling restart went from the highest ordinal down;
- writing 100 MB into a 64Mi volume stopped with "No space left on device";
- a Job ran exactly 3 completions two at a time; a CronJob fired every minute and kept its history limit;
- UDP services worked inside the cluster and through the load balancer;
- Ingress routed by host and path and served the certificate from a Secret;
- a `view` token could not change anything, an `edit` token scoped to `dev` could not see `default`, a revoked token stopped working immediately, and a wrong CA hash was rejected;
- killing the Raft leader elected a new one and writes were forwarded to it over HTTPS;
- a bad image never took down the running replicas, and rollback restored them;
- a DaemonSet ran exactly one pod per node; services kept the caller's source IP and balanced across nodes;
- a NetworkPolicy blocked and allowed traffic on both nodes, and removing it opened the pods again;
- the example operator created, scaled and garbage-collected the objects of a custom `Website` kind, and the schema rejected invalid objects;
- snapshots were replicated to all three servers and downloaded; a volume moved to another node, and another recovered after its node was lost, with their data;
- an NFS `ReadWriteOnce` volume enforced its size and followed its pod to another node;
- with a test ACME CA (Pebble), every server served the issued certificates for the API and Ingress hosts, and after the leader was stopped the new leader issued a certificate for a new host;
- on a Podman node, multi-container pods, DNS, services, sized volumes and snapshots worked;
- on two Podman nodes: changing a ConfigMap rolled the pods and the service answered with the new value; a client with
  `dependsOn: [db]` waited until db was ready; a rollout to an image that does not exist kept the old pods serving and was
  rolled back automatically after its 40-second deadline;
- an agent run on two Podman nodes (with a stand-in for the model API): its runtime pod used Nube's own tools, held a
  sandbox command until it was approved, ran it in a busybox sandbox container, recorded the trace, kept the session
  memory and was cleaned up; a rejected command reached the model as a refusal; the stdio MCP bridge served `diagnose`
  and `run_agent`; the Claude (official Go SDK) and OpenAI-compatible adapters, approvals, token scoping, queueing and
  timeouts are covered by tests against mock APIs.

---

## Install on Ubuntu

### 1. Build

On Windows:
```powershell
powershell -ExecutionPolicy Bypass -File .\build.ps1
```
On Linux/macOS:
```bash
make bundle
```
This produces `dist/nube-0.2.0-linux.tar.gz` (amd64 and arm64 binaries, installer, systemd units, examples) and a CLI for your machine.

You can also install just the CLI with Go 1.25+:
```bash
go install github.com/thisisnabi/Nube/cmd/nube@latest
```

### 2. Everything on one machine

```bash
scp dist/nube-0.2.0-linux.tar.gz user@SERVER:~
ssh user@SERVER
tar xzf nube-0.2.0-linux.tar.gz && cd nube-0.2.0-linux
sudo bash install.sh single
```

The installer:
- installs Docker (`docker.io` from Ubuntu's own repository), WireGuard, iptables, nfs-common and e2fsprogs;
- creates the cluster CA, an admin token and a `node` token for the agent;
- installs the `nube-server` and `nube-agent` systemd services and saves the CLI config for your user.

It finishes by printing the dashboard URL (`https://SERVER_IP:7070`), the CA hash and the command to add more nodes.
To avoid the browser warning, download `https://SERVER_IP:7070/ca.crt` and import it as a trusted CA.

```bash
nube get nodes
nube apply -f examples/web.yaml
curl http://SERVER_IP:8080
```

### 3. Add a worker node

Run the command the installer printed on the other machine:
```bash
sudo bash install.sh agent --server https://SERVER_IP:7070 --token nube_xxx --ca-hash sha256:...
```
Add `--runtime podman` to run the node's pods on Podman instead of Docker.
A node token can only register a node and sync its pods; the admin token never leaves the servers.

### 4. Highly-available control plane (3 servers)

```bash
# server 1
sudo bash install.sh server --ha --id s1 --bootstrap
# servers 2 and 3 — admin token from /var/lib/nube/token and hash from /var/lib/nube/ca.hash on server 1
sudo bash install.sh server --ha --id s2 --join https://S1_IP:7070 --token ADMIN_TOKEN --ca-hash sha256:...
sudo bash install.sh server --ha --id s3 --join https://S1_IP:7070 --token ADMIN_TOKEN --ca-hash sha256:...
```
Use `single` instead of `server` if the servers should run pods too. Three servers tolerate the loss of one; five tolerate two.

### 5. Let's Encrypt

```bash
sudo bash install.sh server ... --acme-email you@example.com [--acme-domain nube.example.com]
```
Ingress hosts with `acme: true` then get certificates automatically: DNS must point at the servers and port 80 or 443
must be reachable from the internet. `--acme-domain` also serves the dashboard and API with a real certificate.
Only the Raft leader talks to Let's Encrypt; certificates and challenge answers are replicated, so any server can answer.
To use your own certificate for the API instead, run the server with `--tls-cert` and `--tls-key`.

### Ports

| Port | Used for |
|---|---|
| 7070/tcp | API and dashboard (HTTPS) |
| 7071/tcp | Agent API (mutual TLS, servers only) |
| 7072/tcp | Raft (between servers) |
| 51820/udp | WireGuard (between nodes) |
| 80, 443/tcp | Ingress |
| your service ports | Load balancer on the servers |

Uninstall with `sudo bash install.sh uninstall` (data and running containers are kept).

---

## CLI

```bash
nube login --server https://SERVER:7070 --token TOKEN    # shows the CA fingerprint and asks before trusting it
nube apply -f app.yaml                                   # file, directory or - for stdin
nube get nodes|deploy|sts|ds|jobs|cronjobs|pods|svc|ing|netpol|cm|secrets|volumes|snapshots|crd [-n NS | -A] [-o wide|json|yaml] [-w]
nube get websites                                        # any custom kind, by plural or short name
nube describe pod NAME
nube logs POD [-c CONTAINER] [-f] [--tail N]
nube scale deploy|sts NAME --replicas 5
nube set image deploy NAME nginx:1.27 [-c CONTAINER]
nube rollout status|history|undo|restart|pause|resume deploy|sts NAME
nube trigger cronjob NAME                                # run now
nube snapshot VOLUME [--name NAME]                       # take a snapshot
nube snapshot download NAME FILE                         # save it as a tar file
nube volume move VOLUME --to NODE                        # move a local volume (and its pod) to another node
nube suspend|resume cronjob NAME
nube token create ci --role edit -n prod                 # the secret is shown once
nube token list | nube token delete ci
nube whoami
nube top nodes|pods|workloads                            # workloads: requests vs. recommended
nube diagnose [KIND NAME] [-n NS | -A]                   # what is broken, the evidence and the fix
nube apply -f app.yaml --dry-run | nube diff -f app.yaml # what would change, field by field
nube explain [KIND]                                      # fields of a kind and an example
nube run AGENT "task" [--session S] [--wait] | nube trace RUN [-w] | nube cancel RUN
nube approvals | nube approve|reject ID [--note TEXT] | nube audit
nube mcp                                                 # MCP server on stdio
nube cordon|uncordon|drain NODE
nube label node NAME key=value
nube events -w
nube cluster status
```

### Roles

| Role | Can |
|---|---|
| `admin` | everything, including nodes, tokens and HA membership |
| `edit` | read and change workloads, config and secrets (only in its namespaces, if any are given) |
| `view` | read everything except secret values |
| `node` | agents only: register a node and sync its pods |

---

## Manifest reference (short)

```yaml
kind: Deployment              # or StatefulSet / DaemonSet / Job / Pod
metadata: { name: app, namespace: default }
spec:
  replicas: 3
  strategy: { maxSurge: 1, maxUnavailable: 0 }
  autoscale: { min: 2, max: 10, targetCPUPercent: 70 }
  progressDeadlineSeconds: 600   # failing new pods after this long → automatic rollback
  # autoRollback: false          # only report it
  template:
    dependsOn: [db]              # schedule only once service db has a ready pod
    # reloadOnConfigChange: false   # do not roll pods when their ConfigMaps/Secrets change
    # One container: put image, command, ports ... directly here.
    # Several containers:
    emptyDirs: [{ name: cache, sizeLimit: 256Mi, medium: Memory }]
    initContainers:
      - { name: migrate, image: myapp, command: ["./migrate"] }
    containers:
      - name: app
        image: myapp:1.2
        ports: [{ containerPort: 8080 }, { containerPort: 9000, protocol: UDP }]
        env: { KEY: value }
        envFrom: [{ configMap: app-config }, { secret: app-secret }]
        resources: { cpu: 250m, memory: 256Mi }
        healthCheck: { httpGet: { path: /healthz, port: 8080 } }
        volumes:
          - { volume: data, containerPath: /data }      # cluster Volume
          - { emptyDir: cache, containerPath: /cache }   # shared by the pod's containers
      - name: sidecar
        image: busybox
    restartPolicy: Always    # Always | OnFailure | Never
---
kind: Service
metadata: { name: app }
spec:
  ports: [{ port: 8080 }, { port: 9000, protocol: UDP }]
  # headless: true   → DNS returns the pod IPs (for StatefulSets)
  # internal: true   → reachable inside the cluster only
---
kind: Volume
metadata: { name: data }
spec:
  type: local
  size: 10Gi                                  # enforced
  backup: { schedule: "0 3 * * *", keep: 7 }  # nightly snapshots
  autoRecover: true                           # restore on another node if this one is lost
  # restoreFrom: data-snap                    # start from a VolumeSnapshot
---
kind: NetworkPolicy
metadata: { name: db-access }
spec:
  podSelector: { matchLabels: { app: db } }
  ingress:
    - from: [{ podSelector: { matchLabels: { role: api } } }]
      ports: [{ port: 5432 }]
---
kind: Ingress
metadata: { name: web }
spec:
  tls: [{ hosts: [app.example.com], acme: true }]
  rules:
    - host: app.example.com
      paths: [{ path: /, backend: { service: app, port: 8080 } }]
```

With `containers:`, put `image`, `command`, `ports`, `volumes` and so on inside each container. The same fields at the
template level are the single-container shorthand, and mixing the two is rejected.

Complete examples in [`examples/`](examples):

| File | What it shows |
|---|---|
| `web.yaml` | Replicated web server behind a service |
| `redis.yaml` | Redis with a persistent volume |
| `postgres.yaml` | PostgreSQL with a Secret and a volume |
| `api-autoscale.yaml` | Autoscaled API with a ConfigMap |
| `sidecar.yaml` | Init container and sidecar sharing localhost |
| `statefulset.yaml` | PostgreSQL as a StatefulSet |
| `jobs.yaml` | Job and CronJob |
| `ingress.yaml` | Host and path routing with TLS |
| `udp.yaml` | UDP service |
| `nfs-volume.yaml` | NFS volume |
| `operator/` | A `Website` CRD and an operator (Go, using `pkg/client`) that turns websites into deployments, services and ingresses |
| `agents/` | An ops assistant (Nube tools, approved shell, memory, schedule), a two-agent team, an agent on a local model |

---

## How it works

- **State and reconciliation.** All objects live in one state. Every second the leader's controllers compare desired and
  actual state and act on the difference — the core idea of Kubernetes. See `internal/server/controllers.go` and `workloads.go`.
- **Agents.** Every 3 seconds an agent reports its node and pods and receives the pods it should run, its network peers and
  the service routes. It drives Docker directly through the Engine API on the unix socket. See `internal/agent/`.
- **Pods.** For each pod the agent starts a pause container that holds the IP, ports, DNS settings and hostname, runs the
  init containers one by one, then starts the app containers with `--network container:<pause>`. The pause image is built
  locally from the nube binary itself, so no registry access is needed. See `internal/agent/pod.go`.
- **Sized volumes.** Each local volume with a size is an ext4 image file, loop-mounted. Raising `size` grows it online;
  shrinking is refused. See `internal/agent/volumes.go`.
- **Security.** The first leader creates a CA, stored in the replicated state. Each server issues itself a certificate from
  it; agents send a CSR and get a certificate for their own IP; servers call agents with a client certificate.
  See `internal/pki` and `internal/server/auth.go`.
- **High availability.** Every write is a Raft log entry holding only the objects it changed (a patch), so the log stays
  small however big the cluster is. Heartbeats and usage metrics stay in memory on the leader. Only the leader runs
  controllers; followers forward writes to it. See `internal/store/raft.go` and `patch.go`.
- **Networking.** Each node owns a bridge network (`nube0`) with its own /24 of `10.244.0.0/16`. WireGuard (`nube-wg`)
  routes pod subnets between nodes without NAT; traffic to the internet is masqueraded. Services are iptables DNAT rules
  (`NUBE-SERVICES`); network policies are per-pod iptables chains. DNS listens on the bridge gateway (`10.244.N.1`).
- **Snapshots.** An agent tars a volume and uploads it to the servers, which replicate it among themselves; a restore
  streams it back into a fresh volume on the target node. Moves and recoveries are a snapshot plus a restore.
- **Configuration-aware rollouts.** A workload's revision is its template plus a keyed digest (HMAC) of the ConfigMaps
  and Secrets it reads, so a data change starts a normal rolling update, and the history does not reveal secret values.
- **Automatic rollback.** If a new revision's pods are failing (crash loop, image pull error, failing health check,
  missing ConfigMap) when `progressDeadlineSeconds` runs out, the controller puts the previous template back. Pods that
  are only waiting (scheduling, pulling, `dependsOn`) never trigger it, and a rollback is never rolled back itself.
  See `internal/server/rollout.go`.
- **Recommendations.** Once a minute the leader records the highest per-pod usage of every workload (one day, in memory).
  CPU is suggested at the 95th percentile + 15%, memory at the peak + 20%, because memory is also the container's limit.
- **Agents.** `internal/server/agents.go` (queue, pods, run tokens, memory), `internal/runtime` (the agent loop, model
  adapters, tools, MCP client, sandbox server), `internal/server/mcp.go` (Nube's MCP server) and `guard.go` (approvals, audit).
- **Custom resources.** A CRD adds a kind to the API, with a JSON-schema subset for validation and defaults. Operators use
  `watch` (long polling on resource versions) and write `status`; objects labelled `nube/owned-by=Kind/name` are deleted
  with their owner. See `internal/server/crd.go` and `examples/operator`.
- **Upgrades without downtime.** Pods keep running when the agent restarts (`KillMode=process`); the agent adopts them again.

## Limitations

- Kubernetes manifests and `kubectl` are not supported; Nube has its own, simpler manifest format and CLI.
- A `local` volume lives on one node. Moving or recovering it restores the latest snapshot, so writes made after that
  snapshot are lost if the node dies. Use `nfs` with `ReadWriteOnce` for data that must survive a node without a restore.
- Snapshots are stored on the control-plane servers, so the servers' disks limit their total size.
- Shared `nfs` volumes (`ReadWriteMany`) have no enforced size; that is up to the NFS server.
- Every server holds the whole state in memory: good for tens of nodes and a few thousand pods.
- Only the Docker and Podman runtimes are supported (no containerd/CRI).
- Pods get no isolation beyond what Docker or Podman provide (no user namespaces, custom seccomp profiles or sandbox runtimes).
  The same goes for agent sandboxes: use `requireApproval` for tools that run commands.
- Agent runs keep their trace and memory in the replicated state; long traces are cut at 4 KB per step and 100 runs per agent are kept.
- Agent workflows are model-driven (tools and delegation); there is no graph/workflow DSL.

## Development

```bash
go test ./...          # scheduler, rolling updates, Jobs, CronJobs, StatefulSets, RBAC, Ingress, UDP proxy, PKI ...
make linux             # or: powershell -ExecutionPolicy Bypass -File .\build.ps1
bash test/e2e/up.sh    # local test cluster on Docker: 3 HA servers with TLS + 2 nodes
```

Project layout:

```
cmd/nube/            main: server | agent | pause | agent-run | sandbox | CLI
internal/api/        resource types, manifest parsing, quantities
internal/store/      state, file store, Raft store
internal/server/     API, auth, scheduler, controllers, load balancer, ingress, dashboard (web/)
internal/agent/      Docker/Podman client, pods, networking, firewall, DNS, volumes, snapshots
internal/proxy/      TCP/UDP load balancer
internal/pki/        cluster CA and pinned TLS
internal/runtime/    AI agent runtime: agent loop, Claude / OpenAI-compatible adapters, tools, sandbox
internal/cli/        kubectl-style client
pkg/client/          Go client for operators
deploy/              installer and systemd units
test/e2e/            Docker-based multi-node test cluster
```

## Troubleshooting

| Problem | Fix |
|---|---|
| `server certificate is not signed by the pinned cluster CA` | Compare the hash with `/var/lib/nube/ca.hash` on a server |
| Node is `NotReady` | `journalctl -u nube-agent -n 50` — can the agent reach port 7070, and is its token a `node` token? |
| NETWORK shows `wireguard(down)` | The Nodes page shows the error; usually `wireguard-tools` is missing or UDP 51820 is blocked |
| Volume says `size not enforced` | `e2fsprogs` is missing on the node, or the volume existed before it had a size |
| Pod stuck in `Pending` or `Init` | `nube describe pod NAME` and `nube logs POD -c INIT_CONTAINER` |
| `403 ... needs role edit` | Your token's role is not enough (`nube whoami`) |
| `ErrImagePull` | Check the image name and that Docker on the node can reach the registry |
| Ingress shows an `acme:` error | DNS must point at the servers and port 80 or 443 must be reachable from the internet |
| `nube apply` says to put fields inside each container | You used `containers:` and also top-level fields like `volumes:`; move them into the container |

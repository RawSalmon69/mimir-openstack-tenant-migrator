# NIPA-Mimir

A Go migration service that streams historical Prometheus TSDB blocks into
Grafana Mimir on a per-tenant basis, plus the Kubernetes deployment stack
(Mimir + cortex-tenant + Redis + APISIX + Grafana) that runs it with
OpenStack Keystone–backed tenant isolation enforced at the storage layer.

Designed for migrating a multi-tenant OpenStack-style monitoring fleet —
where each tenant is identified by an OpenStack project ID present as a
label on every timeseries — from a legacy Prometheus deployment to Mimir,
preserving strict cross-tenant isolation on both the read and write paths.

---

## Table of contents

1. [Quickstart](#quickstart)
2. [What this repo is](#what-this-repo-is)
3. [Topology at a glance](#topology-at-a-glance)
4. [Repo layout](#repo-layout)
5. [Prerequisites](#prerequisites)
6. [Deploying a fresh cluster](#deploying-a-fresh-cluster)
7. [Running a migration](#running-a-migration)
8. [Local development](#local-development)
9. [Known limitations](#known-limitations)
10. [Troubleshooting](#troubleshooting)

---

## Quickstart

All scripts run **on the k3s master node**, not your workstation.
Clone the repo there, export credentials, and go:

```bash
# 1. SSH into master and clone
ssh <user>@<master-ip>
git clone https://github.com/RawSalmon69/mimir-openstack-tenant-migrator.git
cd mimir-openstack-tenant-migrator

# 2. Export credentials (or source your .env.prod)
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
export MIMIR_S3_ENDPOINT=https://s3.your-provider.example
export MIMIR_S3_ACCESS_KEY=...
export MIMIR_S3_SECRET_KEY=...
export MIMIR_S3_REGION=your-region
export MIMIR_S3_BUCKET_BLOCKS=mimir-blocks
export MIMIR_S3_BUCKET_RULER=mimir-ruler
export MIMIR_S3_BUCKET_ALERTS=mimir-alerts
export KEYSTONE_URL=https://keystone.your-openstack.example
export CLUSTER_GATEWAY_URL=http://<master-ip>:31545
export GRAFANA_ADMIN_PASSWORD='change-me'

# 3. Deploy the full stack (idempotent, safe to re-run)
cd deploy && sudo -E ./deploy.sh          # ends with verify.sh 16/16

# 4. Build + ship the migrator image to the worker node
#    TARGET_NODE_HOST = worker IP where the migrator pod runs (required)
sudo -E TARGET_NODE_HOST=<worker-ip> TARGET_NODE_PASS='...' ./rebuild-migrator.sh

# 5. Run a migration
echo "your-tenant-id" > tenants.txt
sudo -E ./run-migration.sh run --wait -f tenants.txt

# 6. Verify data is queryable (~1-2 min after migration)
kubectl run curl-test --rm -i --restart=Never -n monitoring \
  --image=curlimages/curl:latest -q --command -- \
  curl -sS "http://mimir-gateway.monitoring.svc.cluster.local/prometheus/api/v1/query?query=up" \
  -H "X-Scope-OrgID: your-tenant-id"
```

For k3s clusters, `kubectl` and `helm` require `sudo` or
`export KUBECONFIG=/etc/rancher/k3s/k3s.yaml`. All `deploy/` scripts
are designed to run on the master node with direct cluster access.

---

## What this repo is

Two things, in one repo:

1. **`services/migrator/`** — a Go service that streams a Prometheus TSDB directory tenant-by-tenant into Mimir via `cortex-tenant`, managing per-tenant OOO windows and ingestion rate limits on the fly via Kubernetes ConfigMap patches. Runs as an HTTP API + asynq worker in a single binary; also ships two CLI tools (`cmd/migrator`, `cmd/revert-overrides`).
2. **`deploy/`** — Kubernetes manifests, Helm values, Lua APISIX plugins, and operational scripts that deploy the whole monitoring stack. One `deploy.sh` bootstraps it end-to-end.

The end state: users log in with Keystone, get a token scoped to their `project_id`, and every query into Grafana/Mimir is physically isolated per tenant — enforced at the TSDB block level.

---

## Topology at a glance

```
┌──────────── Users (browser / Prometheus remote-write clients) ────────────┐
│                                                                           │
│     Keystone token  │  X-Auth-Token / cookie / Basic Auth (app creds)     │
└────────────────┬──────────────────────────────────┬───────────────────────┘
                 │                                  │
                 ▼                                  ▼
        ┌───────────────────────────────────────────────────────┐
        │              APISIX gateway  (:31545)                 │
        │                                                       │
        │  /grafana/*   → grafana-keystone-authz.lua            │
        │                 validates token, sets                 │
        │                 X-GRAFANAAUTH-USER = project_id       │
        │                                                       │
        │  /prometheus/* → mimir-keystone-authz.lua             │
        │                  validates token, injects             │
        │                  X-Scope-OrgID = project_id           │
        │                                                       │
        │  L1 in-proc LRU (1000)  +  L2 nginx shared dict (5Mi) │
        └────────┬──────────────────────────────────┬───────────┘
                 │                                  │
                 ▼                                  ▼
        ┌────────────────┐                ┌─────────────────────┐
        │    Grafana     │                │   Mimir (HA, 3 AZ)  │
        │                │                │                     │
        │ auth.proxy on  │                │ reads X-Scope-OrgID │
        │ X-GRAFANAAUTH- │   query path   │ returns ONLY tenant │
        │ USER creates   ├───────────────►│ data                │
        │ user = proj_id │                │                     │
        │                │                │ storage: S3         │
        │ Datasource     │                │   blocks / ruler /  │
        │ "Mimir" keeps  │                │   alerts buckets    │
        │ auth cookie,   │                └──────────▲──────────┘
        │ re-calls APISIX│                           │
        └────────────────┘                           │
                                                     │
                     ┌───────────────────────────────┴──────┐
                     │   write path: cortex-tenant (:8080)  │
                     │   injects X-Scope-OrgID from the     │
                     │   projectId label on each series     │
                     └───────────────────▲──────────────────┘
                                         │
                                         │  POST /push (protobuf+snappy)
                                         │
                     ┌───────────────────┴──────────────────┐
                     │        migrator service              │
                     │                                      │
                     │   HTTP :8090  (healthz, migrate,     │
                     │                status, tasks)        │
                     │                                      │
                     │   async worker (asynq, Redis-backed) │
                     │     ┌─────────┐   ┌──────────┐       │
                     │     │  TSDB   ├──►│ byte-    │       │
                     │     │ reader  │   │ budget   │       │
                     │     │(prom    │   │ batcher  │       │
                     │     │ tsdb)   │   │ (3 MiB)  │       │
                     │     └─────────┘   └────┬─────┘       │
                     │                        │             │
                     │          4× remotewrite writer       │
                     │                 goroutines           │
                     │                                      │
                     │   side effect: patches               │
                     │   ConfigMap monitoring/mimir-runtime │
                     │   (per-tenant OOO + rate limits)     │
                     └──────────────────────────────────────┘
                                         ▲
                                         │
                     hostPath /mnt/data_dump/data
                     mounted read-only at /data/tsdb
                     (source Prometheus blocks)
```

Key invariants:

- **Tenancy is label-driven.** Everything downstream of cortex-tenant relies on the `projectId` label on each series — cortex-tenant maps that label to the `X-Scope-OrgID` Mimir expects.
- **Migrator never talks to Mimir directly.** It only writes to `cortex-tenant`'s `/push` endpoint. This is enforced by a boot-time probe (`cortexcheck`) that hard-fails if `CORTEX_URL` doesn't end with `/push`.
- **No inbound auth on the migrator HTTP API.** It is expected to be reachable only from inside the cluster or via APISIX; the auth layer is Keystone-enforced upstream.

---

## Repo layout

```
NIPA-Mimir/
├── README.md
├── services/
│   └── migrator/                   Go module
│       ├── cmd/
│       │   ├── server/             production binary (HTTP + asynq worker)
│       │   ├── migrator/           single-shot CLI (no Redis, no k8s)
│       │   └── revert-overrides/   cleanup CLI (removes per-tenant overrides)
│       ├── internal/
│       │   ├── api/                HTTP mux, handlers, interfaces
│       │   ├── queue/              asynq task + progress store + history log
│       │   ├── migration/          pipeline.Run() — reader/batcher/writer stages
│       │   ├── tsdb/               Prometheus TSDB reader
│       │   ├── remotewrite/        protobuf+snappy remote-write client
│       │   ├── mimirconfig/        ConfigMap patch client (OOO + rate limits)
│       │   └── cortexcheck/        boot-time CORTEX_URL probe
│       ├── Dockerfile              multi-stage, distroless/static runtime
│       ├── go.mod                  Go 1.25
│       └── go.sum
├── deploy/                             Kubernetes manifests + Helm values
│   ├── deploy.sh                       one-shot deployment orchestrator
│   ├── dry-run.sh                      migration dry-run helper
│   ├── verify.sh                       post-deploy health checks
│   ├── run-migration.sh                run/status/wait wrapper over the migrator API
│   ├── rebuild-migrator.sh             build → save → scp → ctr-import → rollout
│   ├── import-dashboards.sh            Grafana dashboard bulk-import
│   ├── mimir-values.yaml               Mimir Helm values (S3 placeholders)
│   ├── cortex-tenant-values.yaml
│   ├── redis-values.yaml               Bitnami Redis (standalone, no auth)
│   ├── apisix-values.yaml              APISIX gateway values
│   ├── apisix-routes.json              APISIX route definitions (templated)
│   ├── configmap-mimir-keystone-authz.yaml    Lua plugin ConfigMap
│   ├── configmap-grafana-keystone-authz.yaml  Lua plugin ConfigMap
│   ├── grafana-values.yaml             Grafana Helm values
│   ├── migrator-rbac.yaml              ServiceAccount + Role + RoleBinding
│   ├── migrator-service.yaml           ClusterIP Service
│   ├── migrator-tsdb-pvc.yaml          PVC template for TSDB hostPath
│   └── migrator-deployment.yaml
├── grafana-monitor-system-main-grafana-dashboards-general/
│                                   Grafana dashboard JSON (bulk-imported)
└── get-token.sh                    Keystone token helper (env-var overridable)
```

Conventions worth knowing before adding code:

| You want to... | Put it here |
|---|---|
| Add an HTTP endpoint | handler → `internal/api/handlers.go`, route → `internal/api/server.go` (`NewServer`), interface → `server.go` if a new dep is needed, then test in `handlers_test.go` |
| Add a new pipeline stage | modify `internal/migration/pipeline.go` — keep it `errgroup`-orchestrated |
| Add a new CLI tool | new dir under `services/migrator/cmd/<name>/` using the `parseFlags` / `run` split pattern (see `cmd/revert-overrides`) |
| Add a k8s manifest | `deploy/<kebab-case>.yaml` with label `app.kubernetes.io/name: migrator` or appropriate component label |
| Add a shared package | new `services/migrator/internal/<name>/` — do not stash shared code inside `api/` |

---

## Prerequisites

### Toolchain (local workstation)

- `go` **1.25+**
- `kubectl` configured for the target cluster
- `helm` 3.x
- `docker` (for building the migrator image)
- `python3` (used by `deploy.sh` for APISIX route JSON parsing)
- `bash` (not `sh`) — `deploy.sh` uses `/dev/tcp` and arrays

### Target cluster

- Kubernetes 1.28+
- A worker node with the source TSDB dump mounted at `/mnt/data_dump/data`.
  The migrator pod is pinned to a single node via `nodeSelector` because it
  reads the TSDB through a `hostPath` volume — see
  [Known limitations](#known-limitations) below. Edit the `nodeSelector` in
  `deploy/migrator-deployment.yaml` to match your node name.
- An S3-compatible object store for Mimir with three buckets (`blocks`,
  `ruler`, `alerts`). All three must exist before deploy — a non-existent
  bucket is a hard failure, not a warning.
- A reachable OpenStack Keystone endpoint (Keystone v3, password auth).
- A public gateway URL / node-port for APISIX (used by Grafana for
  `root_url` and by the Lua plugins as the token-redirect target).

### Cluster resource requirements

The full stack deploys 28 pods. Total resource **requests** at default
settings:

| Component | Pods | CPU request | Memory request | Storage |
|---|---:|---|---|---|
| Mimir (all components)¹ | 18 | 1.5 CPU | 4.5 Gi | S3 (remote) + local PVCs for WAL/cache |
| Mimir Kafka | 1 | 1 CPU | 1 Gi | 5 Gi PVC |
| Migrator | 1 | 2 CPU | 8 Gi | hostPath (read-only TSDB) |
| APISIX + etcd (×3) | 4 | 750m | 768 Mi | 3× 8 Gi PVC (etcd) |
| cortex-tenant (×2) | 2 | 200m | 256 Mi | — |
| Grafana | 1 | 100m | 128 Mi | 1 Gi PVC |
| Redis | 1 | 50m | 64 Mi | 1 Gi PVC |
| **Total** | **28** | **~5.6 CPU** | **~14.7 Gi** | **~50 Gi PVC + S3** |

¹ Includes distributor, ingesters (×3 zones), store-gateways (×3 zones),
queriers (×2), query-frontend, query-scheduler (×2), compactor,
alertmanager, ruler, overrides-exporter, rollout-operator, gateway.

Mimir component resource requests come from the `mimir-distributed`
chart defaults and are not overridden in `mimir-values.yaml`. The
migrator is the heaviest single pod (2 CPU / 8 Gi request, up to
6 CPU / 28 Gi limit during active migration). To adjust its resource
allocation, edit the `resources` block in `deploy/migrator-deployment.yaml`.

### Required environment variables for `deploy.sh`

Export these before running the deploy script:

```bash
export KUBECONFIG=/path/to/your/kubeconfig
export MIMIR_S3_ENDPOINT=https://s3.your-provider.example
export MIMIR_S3_ACCESS_KEY=...
export MIMIR_S3_SECRET_KEY=...
export MIMIR_S3_REGION=your-region
export MIMIR_S3_BUCKET_BLOCKS=mimir-blocks
export MIMIR_S3_BUCKET_RULER=mimir-ruler
export MIMIR_S3_BUCKET_ALERTS=mimir-alerts
export KEYSTONE_URL=https://keystone.your-openstack.example
export CLUSTER_GATEWAY_URL=http://<your-node-ip>:31545
export GRAFANA_ADMIN_PASSWORD='change-me'

# optional
export NAMESPACE=monitoring        # default: monitoring
export APISIX_ADMIN_KEY=...        # default: compiled into apisix-values.yaml
export DRY_RUN=true                # print commands instead of executing
```

---

## Deploying a fresh cluster

All `deploy/` scripts run **on the k3s master node** (where `kubectl` and `helm` have direct cluster access). `deploy.sh` is the single entry point. It is idempotent (`helm upgrade --install`) and safe to re-run.

### 1. Build and load the migrator image

`migrator-deployment.yaml` uses `imagePullPolicy: Never` and expects the
image to exist in the target node's containerd image store. Use
`rebuild-migrator.sh` to automate the build → save → scp → import →
rollout sequence:

```bash
cd deploy
TARGET_NODE_PASS='your-ssh-password' ./rebuild-migrator.sh
```

The script accepts these environment overrides for SSH access to the
target worker node:

| Variable | Default | Description |
|---|---|---|
| `TARGET_NODE_HOST` | *(required)* | SSH host of the worker node pinned by `nodeSelector` |
| `TARGET_NODE_USER` | `nc-user` | SSH user |
| `TARGET_NODE_PASS` | *(empty — assumes key auth)* | SSH password (used via `sshpass`) |

Edit these defaults in the script header or export them before running.
If your cluster uses a registry instead, change `imagePullPolicy` to
`IfNotPresent` in `migrator-deployment.yaml` and push to your registry.

### 2. Run the deploy script

```bash
cd deploy
./deploy.sh
```

What it does, step by step (matches `deploy.sh` exactly):

| # | Step | Notes |
|---|---|---|
| 1 | Add Helm repos | grafana, apisix, bitnami, cortex-tenant |
| 2 | Create namespace | default `monitoring` |
| 3 | Install **Mimir** | `grafana/mimir-distributed` with S3 placeholders substituted |
| 4 | Install **cortex-tenant** | the `X-Scope-OrgID` injector in front of Mimir |
| 5 | Install **Redis** | `bitnami/redis` standalone, no auth — used by asynq + migrator progress store |
| 6 | Apply Lua plugin **ConfigMaps** | `mimir-keystone-authz.lua`, `grafana-keystone-authz.lua` |
| 7 | Install **APISIX** | mounts the Lua plugin ConfigMaps |
| 8 | Install **Grafana** | with `root_url` = `CLUSTER_GATEWAY_URL` |
| 9 | Wait for all pods Ready | 300 s timeout |
| 10 | PUT APISIX routes | via admin API from inside the APISIX pod (uses `/dev/tcp` because the pod has no curl) |
| 11 | Apply **migrator** RBAC, Service, PVC, Deployment | requires the image from step 1 to be loaded on the target node |
| 12 | Import **Grafana dashboards** | runs `import-dashboards.sh` inline |
| 13 | Run `verify.sh` | see next section |

> **Dry-run first.** `DRY_RUN=true ./deploy.sh` will print every command without executing — highly recommended on a first run against a real cluster.

### 3. Verify

`verify.sh` runs a battery of health checks:

```bash
cd deploy
./verify.sh
```

It checks:

- all pods Running
- APISIX admin API reachable + routes present
- Grafana `/api/health` returning `ok`
- Redis master accepting connections
- cortex-tenant `/push` reachable from inside the cluster
- Grafana dashboard count > 0
- unauthenticated `GET /prometheus/*` returns **401** (proves the Keystone plugin is active)

### 4. Import dashboards

```bash
cd deploy
./import-dashboards.sh
```

This bulk-imports every JSON under
`grafana-monitor-system-main-grafana-dashboards-general/grafana/dashboards/general/`
via the Grafana API.

### 5. Get a Keystone token and log in

```bash
# Configure your Keystone endpoint + project
export OS_AUTH_URL=https://keystone.your-openstack.example/
export OS_PROJECT_ID=<your-project-uuid>
export OS_USERNAME=<your-username>
export OS_USER_DOMAIN_NAME=<your-domain>
export APISIX_HOST=<your-node-ip>:31545

./get-token.sh
```

The script prompts for a password, obtains a scoped Keystone token, and
prints a single URL of the form `http://<host>/set-token?token=...`. Open
that URL in a browser: APISIX sets the `token` cookie and redirects you
into Grafana, scoped to your project.

What you see inside Grafana depends on your Keystone roles. If your user
has the `nipa_grafana_admin` role (or whatever role is listed in
`admin_role` on the `grafana-proxy` APISIX route), the Lua plugin gives
you a cross-tenant admin view. Otherwise you are scoped to your own
project's tenant in Mimir, and dashboards show only your project's data.

---

## Running a migration

Once the stack is up, the migrator service exposes a small REST API on port
**8090** inside the cluster. For day-to-day operations, use the helper
script — it wraps the required ConfigMap patch, the runtime-reload wait,
and the port-forward into a single command.

### The easy way: `run-migration.sh`

```bash
cd deploy

# Kick off a migration for one tenant
./run-migration.sh run <tenant-id>

# Multiple tenants in one shot — one ConfigMap patch, one POST
./run-migration.sh run tenantA tenantB tenantC

# Big batch — tenant IDs in a file, one per line, `#` comments allowed
./run-migration.sh run -f deploy/tenants.txt

# Kick off 30 tenants and block until the whole fleet finishes
./run-migration.sh run --wait -f tenants.txt

# Show status of every tracked task
./run-migration.sh status

# Show status of a single task
./run-migration.sh status <task-id>

# Block until a single task reaches a terminal state
./run-migration.sh wait <task-id>

# Block until every tracked task reaches terminal state (active+pending → 0)
./run-migration.sh wait --all

# Cancel + clear one tenant's tasks, or everything
./run-migration.sh delete <tenant-id>
./run-migration.sh delete --all
```

What `run-migration.sh run` does, in order:

1. **Patches** `monitoring/mimir-runtime` with per-tenant overrides
   (`out_of_order_time_window: 2880h`, `ingestion_rate: 10000000`,
   `ingestion_burst_size: 20000000`) for every tenant passed in (positional
   args AND any loaded via `-f FILE`). The `runtime.yaml` key is
   **replaced** with a freshly-built overrides block — any prior overrides
   for other tenants in the same ConfigMap are overwritten. Pass every
   tenant you want to migrate in a single invocation.
2. **Sleeps** `RUNTIME_RELOAD_WAIT_SECS` (default 45 s) so Mimir's
   `runtime_config` poller has time to pick up the new file through
   the ConfigMap-mount and file-watcher propagation chain.
3. **Starts** a background `kubectl port-forward` on the migrator
   deployment, waits for `/healthz` to respond, then **POSTs** the tenant
   list as a single `/migrate` call. The handler enqueues one asynq task
   per tenant in the batch.
4. **Prints** the enqueued task IDs. With `--wait` / `-w`, the script
   then polls `/status` until `active + pending` drops to zero and
   exits with `0` on all-success or `1` if any task ended in `failed`.
   Without `--wait`, the script exits immediately — the asynq worker
   processes tasks in the background. Default `QUEUE_CONCURRENCY` is
   **1** so that `promtsdb.Open` never runs for two tenants at the same
   time (its lock file on the source TSDB would otherwise cause one of
   the simultaneous runs to fail with "resource temporarily unavailable").
   Parallelism within each single-tenant migration comes from
   `WRITER_CONCURRENCY` (default 4) on the remote-write side.

### Watching progress

Everything the migrator knows is reachable through `/status`, and the
script exposes it with a few verbs:

```bash
# One-line summary + per-task table of every tracked task
./run-migration.sh status

# Full JSON for a single task (all fields: series_read, samples_read,
# samples_sent, batches_sent, state, error, eta_seconds, started_at, ...)
./run-migration.sh status <task-id>

# Poll a single task every 10s until it terminates; prints one line per poll
./run-migration.sh wait <task-id>

# Poll the whole fleet every 10s, prints one line per poll, exits when
# active+pending → 0
./run-migration.sh wait --all
```

Typical progress line from `wait --all`:

```
[  5] total=30 completed=7 active=1 pending=22 failed=0
```

And from per-task `wait`:

```
[ 12] state=active     series=  142  samples_read=  74321098  samples_sent=  74100000
```

When you kick a run off with `--wait`, the script transitions into the
`wait --all` loop automatically once the POST succeeds, so you get
progress for the whole batch without running a second command.

If you need raw HTTP:

```bash
kubectl -n monitoring port-forward deploy/migrator 8090:8090 &
curl -sS http://localhost:8090/status | jq
curl -sS http://localhost:8090/status/<task-id> | jq
```

Terminal history — every `completed` / `failed` task outcome — is also
appended to `/var/log/migrator/history.jsonl` inside the pod. Tail it:

```bash
kubectl -n monitoring exec deploy/migrator -- tail -f /var/log/migrator/history.jsonl
```

> **Note:** the history log is ephemeral (`emptyDir`) and wiped on pod
> restart. If you need durability, change the `history-log` volume in
> `migrator-deployment.yaml` to a PVC or hostPath.

Environment overrides for `run-migration.sh`:

```bash
NAMESPACE                  Kubernetes namespace         (default: monitoring)
RUNTIME_CM                 Mimir runtime ConfigMap name (default: mimir-runtime)
OOO_WINDOW                 per-tenant OOO window        (default: 2880h)
INGESTION_RATE             per-tenant samples/sec cap   (default: 10000000)
INGESTION_BURST_SIZE       per-tenant burst cap         (default: 20000000)
RUNTIME_RELOAD_WAIT_SECS   wait after patch             (default: 45)
PORT_FORWARD_PORT          local port for migrator API  (default: 8090)
MIGRATOR_DEPLOYMENT        deployment name              (default: migrator)
```

> **Why the ConfigMap patch is done from the script, not only from the
> handler.** The migrator's `/migrate` handler also calls
> `mimirconfig.Client.ApplyOverrides` against the in-cluster ConfigMap
> (merge-preserving) — so a fresh image built from this repo will patch
> the overrides on its own. The script still performs the patch as
> belt-and-suspenders: it guarantees that **even if** the migrator
> pod is running a stale image without the applier wired in, or the
> ServiceAccount can't patch ConfigMaps, the migration will still see
> the right overrides. Running the script's patch first is idempotent
> and strictly safer than trusting the handler.

### API reference

Even with the script, it's useful to know the underlying API:

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/healthz` | liveness / readiness |
| `POST` | `/migrate` | start migration for one or more tenants |
| `GET` | `/status` | list progress for all tasks |
| `GET` | `/status/{task_id}` | single task progress |
| `DELETE` | `/tasks?project_id=<id>` | cancel + clear progress for one tenant |
| `DELETE` | `/tasks?all=true` | cancel + clear everything (used between runs) |

### Under the hood

When `/migrate` is called, the handler runs this sequence for each tenant:

1. `POST /migrate` calls `mimirconfig.Applier.ApplyOverrides()` — intended
   to be a **merge-safe** patch of `monitoring/mimir-runtime` adding an
   `overrides:` block per tenant (OOO window + rate/burst). **If this
   fails, no tasks are enqueued** (fail-fast). See the limitation note
   above — in the current image this is a no-op.
2. One asynq task per tenant is enqueued on queue `migration` with
   `MaxRetry(3)` and a 6 h timeout.
3. Each task's progress is seeded as `pending` in Redis hash
   `migrate:progress:<task_id>`.
4. The asynq worker picks up the task, marks it `active`, and runs the
   three-stage pipeline:
   - **Reader**: `tsdb.Open(/data/tsdb)` → query series filtered by
     `projectId` label → stream `SeriesData` over a buffered channel.
   - **Batcher**: byte-budget accumulator, default **3 MiB**
     (`MAX_BATCH_BYTES`). Oversized single-series are split into sample
     windows so we never blow past the budget.
   - **Writers**: 4 parallel goroutines (`WRITER_CONCURRENCY`) each calling
     `remotewrite.Writer.Send()` — snappy-compressed protobuf POST to
     `CORTEX_URL`.
5. Progress in Redis is updated every 100 series.
6. On completion (or terminal failure after retries), the hash is marked
   `completed` / `failed` and an entry is appended to
   `/var/log/migrator/history.jsonl`.

### Raw curl equivalents

If you prefer not to use the helper, the same operations via `kubectl
port-forward` + `curl`:

```bash
kubectl -n monitoring port-forward deploy/migrator 8090:8090 &

curl -sS -X POST http://localhost:8090/migrate \
  -H 'content-type: application/json' \
  -d '{"project_ids": ["tenantA", "tenantB", "tenantC"]}'

curl -sS http://localhost:8090/status | jq
curl -sS http://localhost:8090/status/<task_id> | jq
curl -sS -X DELETE 'http://localhost:8090/tasks?all=true'
```

Remember to patch `mimir-runtime` yourself if you take this path — see
`run-migration.sh` for the merge logic.

### The CLI path (no Redis, no Kubernetes)

When you want a one-shot migration from a shell — useful for debugging a single tenant against a test TSDB without enqueueing:

```bash
cd services/migrator
go build -o migrator ./cmd/migrator
./migrator \
  --tsdb-path /path/to/tsdb \
  --cortex-url http://cortex-tenant.monitoring.svc.cluster.local:8080/push \
  --tenant tenantA
```

No ConfigMap patches are applied by this path — you either set overrides by hand beforehand, or rely on whatever the cluster already has.

### Dry-runs

For a no-write rehearsal that exercises the reader and batcher but skips the POST:

```bash
kubectl -n monitoring set env deployment/migrator DRY_RUN=true
# ...kick off migrations as usual...
kubectl -n monitoring set env deployment/migrator DRY_RUN=false
```

Or use `deploy/dry-run.sh` which wraps this plus an end-to-end verification.

---

## Known limitations

These are design choices and operational notes that keep the deployment
simple for a one-shot migration. Re-evaluate them if you plan to run
this continuously.

### Kafka message size limits

mimir-distributed 6.0.6 uses Kafka for ingest storage. The default
`message.max.bytes` (~1 MiB) is too small for bulk-migration batches.
`mimir-values.yaml` raises `KAFKA_MESSAGE_MAX_BYTES`,
`KAFKA_REPLICA_FETCH_MAX_BYTES`, and `KAFKA_SOCKET_REQUEST_MAX_BYTES`
to 100 MiB via `kafka.extraEnv`. If you are deploying into an existing
cluster not managed by this repo, patch the StatefulSet directly:

```bash
kubectl -n monitoring set env sts/mimir-kafka \
  KAFKA_MESSAGE_MAX_BYTES=104857600 \
  KAFKA_REPLICA_FETCH_MAX_BYTES=104857600 \
  KAFKA_SOCKET_REQUEST_MAX_BYTES=104857600
```

**Important:** the chart key is `kafka.extraEnv`, **not**
`kafka.extraEnvVars` (the bitnami convention). The latter is silently
ignored — confirmed via
`helm show values grafana/mimir-distributed --version 6.0.6`.

### Migrator `runAsUser` must match the TSDB owner

The source TSDB directory is typically owned by UID 65534 (the Linux
`nobody` user, which is also the Prometheus container default). The
migrator image (`gcr.io/distroless/static:nonroot`) defaults to UID
65532. The pod can read block data (directory is 0755) but cannot
create the TSDB lock file required by `promtsdb.Open`.
`migrator-deployment.yaml` sets
`securityContext.runAsUser/runAsGroup/fsGroup: 65534`. If your TSDB
has different ownership, adjust those three fields to match.

### `mimir-gateway` service name (not `mimir-nginx`)

mimir-distributed 6.0.6 names the gateway service `mimir-gateway`.
Older charts used `mimir-nginx`. Both `cortex-tenant-values.yaml`
and `apisix-routes.json` must reference `mimir-gateway`.

### Post-migration query visibility delay

After a bulk migration completes, data is in S3 but must propagate
through the compactor bucket-index update and store-gateway sync
before queries return results. With Mimir defaults (15 min each)
this takes up to 30 minutes. `mimir-values.yaml` overrides both
intervals to 1 minute:

```yaml
blocks_storage:
  bucket_store:
    sync_interval: 1m
compactor:
  cleanup_interval: 1m
```

If you revert to defaults, expect a delay after migrations — or
restart `mimir-compactor-0` and the store-gateway pods to force an
immediate sync.

### Single migrator replica

The migrator is pinned to one worker node via `nodeSelector` because
it reads the source TSDB through a hostPath volume.
`replicas: 1` is the only supported configuration. This does not
affect steady-state ingest through `cortex-tenant`.

### Per-tenant work granularity

The migration queue enqueues one asynq task per tenant. Parallelism
within a tenant is limited to the writer-goroutine pool (default 4
writers, configured via `WRITER_CONCURRENCY`). The 4 writers pull
from a shared batch channel — they do not write overlapping data.

### Ephemeral history log

`/var/log/migrator/history.jsonl` lives on an `emptyDir` volume and
is lost on pod restart. Change the volume to a PVC if you need
durable history.

---

## Local development

```bash
cd services/migrator
go test ./...                 # all unit tests, uses miniredis for in-proc Redis
go test -race ./...           # same with race detector (what CI should run)
go vet ./...
```

All tests are colocated with sources (`<file>.go` + `<file>_test.go`). Nothing here depends on a live Kubernetes cluster — `mimirconfig.NoopApplier` is wired in whenever `KUBECONFIG` is absent, so you can run the server against a local Redis and a fake cortex-tenant HTTP server.

### Run the server against miniature dependencies

```bash
# in one terminal
redis-server --port 6379 &

# in another
cd services/migrator
export CORTEX_URL=http://localhost:9009/push   # anything that ends in /push
export TSDB_PATH=/tmp/fake-tsdb
export REDIS_ADDR=localhost:6379
go run ./cmd/server
```

`CORTEX_URL` is the only truly required variable; the service will exit on startup if it is missing or doesn't end with `/push`. The `cortexcheck` probe will also try a live HTTP round-trip — any non-404 counts as a pass, so a minimal test server returning 401/200/etc. is fine.

### Rebuild the container image

```bash
cd services/migrator
docker build -t docker.io/mimir-openstack-tenant-migrator/migrator:latest .
```

The Dockerfile is a multi-stage build: `golang:1.25-alpine` for compilation (static `CGO_ENABLED=0`, `-trimpath -ldflags="-s -w"`), then `gcr.io/distroless/static:nonroot` for the runtime image.

---

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Migrator pod `CrashLoopBackOff`, logs say `CORTEX_URL must end with /push` | `CORTEX_URL` env var missing `/push` suffix | Update `migrator-deployment.yaml` |
| Migrator pod `CrashLoopBackOff`, logs say `cortexcheck: ...` | cortex-tenant not reachable | `kubectl -n monitoring get pods -l app.kubernetes.io/name=cortex-tenant` |
| Migration tasks fail with `out of order sample` | OOO override didn't apply | Check `mimir-runtime` ConfigMap has `overrides.<tenant>.out_of_order_time_window: 2880h` |
| Migration tasks fail with `rate limit exceeded` | Ingestion rate override didn't apply | Check `mimir-runtime` ConfigMap has `ingestion_rate` / `ingestion_burst_size` under the tenant |
| `POST /migrate` returns 500 `failed to apply mimir overrides` | Migrator SA lacks RBAC | `kubectl apply -f deploy/migrator-rbac.yaml` |
| Unauthenticated `GET /prometheus/labels` returns 200 through APISIX | Lua plugin ConfigMap missing | Check `kubectl -n monitoring get cm mimir-keystone-authz-plugin`; check APISIX logs |
| Migration tasks fail with `HTTP 429` after ~240k samples | Default 10k/s rate limit in effect | Verify `mimir-runtime` ConfigMap overrides; check migrator SA has `patch configmaps` permission |
| Pod stuck `Pending` on scheduling | `nodeSelector` mismatch | Edit `migrator-deployment.yaml` to match your node's `kubernetes.io/hostname` |
| Queries return empty after successful migration | Compactor/store-gateway sync delay | Wait 1–2 minutes (with tuned intervals) or restart `mimir-compactor-0` and store-gateway pods |

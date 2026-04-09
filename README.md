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

1. [What this repo is](#what-this-repo-is)
2. [Topology at a glance](#topology-at-a-glance)
3. [Repo layout](#repo-layout)
4. [Prerequisites](#prerequisites)
5. [Deploying a fresh cluster](#deploying-a-fresh-cluster)
6. [Running a migration](#running-a-migration)
7. [Day-2 operations](#day-2-operations)
8. [Local development](#local-development)
9. [Known limitations](#known-limitations)
10. [Troubleshooting](#troubleshooting)

---

## What this repo is

Two things, in one repo:

1. **`services/migrator/`** — a Go service that streams a Prometheus TSDB directory tenant-by-tenant into Mimir via `cortex-tenant`, managing per-tenant OOO windows and ingestion rate limits on the fly via Kubernetes ConfigMap patches. Runs as an HTTP API + asynq worker in a single binary; also ships two CLI tools (`cmd/migrator`, `cmd/revert-overrides`).
2. **`deploy/new-cluster/`** — the Kubernetes manifests, Helm values, and Lua APISIX plugins that stand up the whole monitoring stack. One `deploy.sh` bootstraps it end-to-end.

The end state: users log in with Keystone, get a token scoped to their `project_id`, and every query into Grafana/Mimir is physically isolated per tenant — enforced at the TSDB block level, not just at the UI.

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
├── deploy/
│   └── new-cluster/                Kubernetes manifests + Helm values
│       ├── deploy.sh               one-shot deployment orchestrator
│       ├── dry-run.sh              migration dry-run helper
│       ├── verify.sh               post-deploy health checks
│       ├── run-migration.sh        run/status/wait wrapper over the migrator API
│       ├── import-dashboards.sh    Grafana dashboard bulk-import
│       ├── mimir-values.yaml       Mimir Helm values (S3 placeholders)
│       ├── cortex-tenant-values.yaml
│       ├── redis-values.yaml       Bitnami Redis (standalone, no auth)
│       ├── apisix-values.yaml      APISIX gateway values
│       ├── apisix-routes.json      APISIX route definitions (templated)
│       ├── configmap-mimir-keystone-authz.yaml    Lua plugin ConfigMap
│       ├── configmap-grafana-keystone-authz.yaml  Lua plugin ConfigMap
│       ├── grafana-values.yaml     Grafana Helm values
│       ├── migrator-rbac.yaml      ServiceAccount + Role + RoleBinding
│       ├── migrator-service.yaml   ClusterIP Service
│       ├── migrator-tsdb-pvc.yaml  PVC template for TSDB hostPath
│       └── migrator-deployment.yaml
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
| Add a k8s manifest | `deploy/new-cluster/<kebab-case>.yaml` with label `app.kubernetes.io/name: migrator` |
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
  `deploy/new-cluster/migrator-deployment.yaml` to match your node name
  before running `deploy.sh`.
- An S3-compatible object store for Mimir with three buckets (`blocks`,
  `ruler`, `alerts`). All three must exist before deploy — a non-existent
  bucket is a hard failure, not a warning.
- A reachable OpenStack Keystone endpoint (Keystone v3, password auth).
- A public gateway URL / node-port for APISIX (used by Grafana for
  `root_url` and by the Lua plugins as the token-redirect target).

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

`deploy/new-cluster/deploy.sh` is the single source of truth. It is idempotent (`helm upgrade --install`) and safe to re-run.

### 1. Build and load the migrator image

`migrator-deployment.yaml` uses `imagePullPolicy: Never` and expects the image to exist locally on the worker node. Pick the flow that matches your cluster:

```bash
# Option A: plain Docker → single-node / kind-style
cd services/migrator
docker build -t docker.io/nipa-mimir/migrator:latest .

# Option B: remote worker — save and scp
docker save docker.io/nipa-mimir/migrator:latest \
  | ssh worker 'docker load'     # or containerd: `| ssh worker ctr -n k8s.io images import -`
```

If you'd rather pull from a registry, change `imagePullPolicy` to `IfNotPresent` and push to your registry.

### 2. Run the deploy script

```bash
cd deploy/new-cluster
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
| 12 | Run `verify.sh` | see next section |

> **Dry-run first.** `DRY_RUN=true ./deploy.sh` will print every command without executing — highly recommended on a first run against a real cluster.

### 3. Verify

`verify.sh` runs a battery of health checks:

```bash
cd deploy/new-cluster
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
cd deploy/new-cluster
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
cd deploy/new-cluster

# Kick off a migration for one tenant
./run-migration.sh run <tenant-id>

# Multiple tenants in one shot — one ConfigMap patch, one POST
./run-migration.sh run tenantA tenantB tenantC

# Big batch — tenant IDs in a file, one per line, `#` comments allowed
./run-migration.sh run -f deploy/new-cluster/tenants.txt

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

Or use `deploy/new-cluster/dry-run.sh` which wraps this plus an end-to-end verification.

---

## Day-2 operations

### Reverting per-tenant overrides

After a migration, the per-tenant OOO + rate-limit overrides are still in the `mimir-runtime` ConfigMap. To remove them cleanly (without touching any tenant you didn't migrate):

**Option A — from inside the migrator pod** (uses the bundled binary):

```bash
POD=$(kubectl -n monitoring get pod -l app.kubernetes.io/name=migrator -o jsonpath='{.items[0].metadata.name}')
kubectl -n monitoring exec "$POD" -- /revert-overrides -tenants=tenantA,tenantB
```

**Option B — as a `kubectl run` one-shot:**

```bash
kubectl -n monitoring run revert --rm -it --restart=Never \
  --image=docker.io/nipa-mimir/migrator:latest \
  --serviceaccount=migrator \
  --command -- /revert-overrides -tenants=tenantA,tenantB
```

The CLI calls `mimirconfig.Client.RevertOverrides()` which deletes only the listed keys via a merge patch. Other tenants' overrides are untouched.

### Logs

The service logs structured JSON on stdout via `log/slog`:

```bash
kubectl -n monitoring logs -f deploy/migrator
```

Every log line has a `component` field (`api`, `queue`, `migration`, `tsdb`, `remotewrite`, `mimirconfig`, `cortexcheck`) for filtering.

### Metrics

The migrator **does not** expose a `/metrics` endpoint today. If you need
visibility, scrape Mimir itself (it's self-instrumented) or add a
Prometheus client registry.

---

## Known limitations

These are design choices that keep the deployment simple for a one-shot
migration of a fixed dataset. Re-evaluate them if you plan to run this
continuously or against multiple source datasets.

### Kafka `message.max.bytes` must be raised above the default

The mimir-distributed chart runs a Kafka broker as the ingest-storage
backend (Mimir 3.x). Kafka's default `message.max.bytes` is ~1 MiB, which
is too small for the migrator's bulk-write batches (1–3 MiB each). Hitting
the limit produces a misleading Mimir error:

```
remote write failed: HTTP 400: send data to partitions:
failed pushing to partition 0: the write request contains a
timeseries or metadata item which is larger that the maximum
allowed size of 15983616 bytes
(err-mimir-distributor-max-write-request-data-item-size)
```

The reported 15 MiB limit in that error is **not** the real cap —
mimir-distributor's logs show the underlying Kafka `MESSAGE_TOO_LARGE`
rejection. `mimir-values.yaml` in this repo bumps
`KAFKA_MESSAGE_MAX_BYTES` / `KAFKA_REPLICA_FETCH_MAX_BYTES` /
`KAFKA_SOCKET_REQUEST_MAX_BYTES` to 100 MiB via `kafka.extraEnvVars`, so a
fresh `deploy.sh` run bakes in the fix automatically. If you are
migrating into a pre-existing cluster that wasn't deployed from this
repo, apply the same env vars to the `mimir-kafka` StatefulSet by hand.

### Single migrator replica pinned to one node

The migrator reads the source TSDB through a `hostPath` volume mounted
read-write at `/data/tsdb`, so the pod is pinned to one worker node via
`nodeSelector`. Consequences:

- `replicas: 1` is the only supported configuration out of the box.
- If the pinned node is drained or fails, the migrator cannot reschedule
  elsewhere until the TSDB dump is copied to another node and the
  `nodeSelector` is updated.
- Horizontal scaling would require either (a) copying the TSDB to every
  eligible node, (b) moving the dump to an RWX `PersistentVolumeClaim`
  backed by NFS / CephFS / similar, or (c) sharding the migration payload
  to smaller units than "one tenant" so asynq can fan work out across
  replicas. None of these are implemented — the current design trades
  horizontal scale for operational simplicity.

This limitation does **not** affect read-path tenancy or steady-state
ingest through `cortex-tenant` — only the migration write path.

### Per-tenant work granularity

The migration queue enqueues **one asynq task per tenant**
(`MigratePayload.ProjectID`). Parallelism within a tenant is limited to
the writer-goroutine pool inside the pipeline (default 4 writers). If a
single tenant's TSDB slice is huge, a single task will still take as long
as that slice takes — splitting across workers requires a code change to
the payload shape.

### Ephemeral history log

`/var/log/migrator/history.jsonl` lives on an `emptyDir` volume — it is
lost when the pod restarts. If you need durable migration history, change
the `history-log` volume in `migrator-deployment.yaml` to a PVC or
hostPath.

### No `/metrics` endpoint on the migrator

Covered above. Mimir itself is self-instrumented, so operational metrics
for the wider stack are available once Mimir is up; the migrator's own
throughput is only visible via its `/status` API and `history.jsonl`.

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
docker build -t docker.io/nipa-mimir/migrator:latest .
```

The Dockerfile is a multi-stage build: `golang:1.25-alpine` for compilation (static `CGO_ENABLED=0`, `-trimpath -ldflags="-s -w"`), then `gcr.io/distroless/static:nonroot` for the runtime image.

---

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Migrator pod `CrashLoopBackOff`, logs say `CORTEX_URL must end with /push` | `CORTEX_URL` env var missing `/push` suffix | update `migrator-deployment.yaml` — the boot-time `cortexcheck.Probe` is doing its job |
| Migrator pod `CrashLoopBackOff`, logs say `cortexcheck: ...` | cortex-tenant not reachable / not Ready | `kubectl -n monitoring get pods -l app.kubernetes.io/name=cortex-tenant`; check the service/endpoints |
| Migration tasks fail with `out of order sample` in Mimir | the OOO override didn't apply | check the `mimir-runtime` ConfigMap has an `overrides.<tenant>.out_of_order_time_window: 2880h` key — `kubectl -n monitoring get cm mimir-runtime -o yaml` |
| Migration tasks fail with `rate limit exceeded` | the ingestion rate/burst override didn't apply | same as above — look for `ingestion_rate` / `ingestion_burst_size` under the tenant |
| `POST /migrate` returns 500 `failed to apply mimir overrides` | migrator SA lacks `patch configmaps` in `monitoring` | `kubectl apply -f deploy/new-cluster/migrator-rbac.yaml` |
| `POST /migrate` returns 500, logs mention `kubernetes` | pod not running in-cluster and no `KUBECONFIG` | expected when running locally — you'll fall back to `NoopApplier` automatically only when `KUBECONFIG` is unset *and* there's no in-cluster config |
| Unauthenticated `GET /prometheus/labels` returns 200 through APISIX | Lua plugin ConfigMap missing / not mounted | `kubectl -n monitoring get cm configmap-mimir-keystone-authz`; check APISIX logs for plugin load errors |
| `mimir-ruler` pod in `CrashLoopBackOff`, logs: `ruler storage: ... bucket does not exist` | the ruler's sanity-check aborts if its S3 bucket is missing — this **is** blocking. An *empty* bucket is fine; a *non-existent* bucket crash-loops the pod. Typos in `MIMIR_S3_BUCKET_RULER` are a common cause. | `kubectl -n monitoring get cm mimir-config -o jsonpath='{.data.mimir\.yaml}' \| grep -A2 ruler_storage` — create the bucket or fix the name, then `kubectl -n monitoring rollout restart deploy/mimir-ruler` |
| Migration tasks fail with `HTTP 429: the request has been limited` after ~240k samples | per-tenant `ingestion_rate` / `ingestion_burst_size` overrides not applied — Mimir's default 10k/s rate + 200k burst is far below what a bulk migration needs. | Verify `kubectl -n monitoring get cm mimir-runtime -o yaml` contains an `overrides.<tenant>` block with `ingestion_rate: 1000000` and `ingestion_burst_size: 2000000`; if not, either grant the migrator ServiceAccount patch permission (`migrator-rbac.yaml`) or patch the ConfigMap by hand before enqueueing. |
| Pod stuck `Pending` on scheduling | `nodeSelector` doesn't match any node | edit `migrator-deployment.yaml` to match your node's `kubernetes.io/hostname`, reapply |
| TSDB volume empty in pod | `hostPath` directory doesn't exist on the selected node | mount / bind-mount the source Prometheus data dir there, or change the volume to a PVC |

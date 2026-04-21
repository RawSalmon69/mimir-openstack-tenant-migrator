# Mimir-OpenStack-Tenant-Migrator

Streams historical Prometheus TSDB blocks into a multi-tenant Grafana
Mimir deployment. Tenancy is enforced at the storage layer: each series
carries a `projectId` label, and `cortex-tenant` converts that label into
the `X-Scope-OrgID` header Mimir expects.

The repo contains two things:

- `services/migrator/` — a Go service (Redis-backed asynq worker + HTTP
  API) that reads a Prometheus TSDB, filters by tenant, and streams
  snappy-compressed remote-write batches to `cortex-tenant`.
- `deploy/` — Kubernetes manifests, Helm values, and bash orchestrators
  that deploy the full stack on k3s: Mimir, cortex-tenant, Redis,
  APISIX (with Lua Keystone auth), and Grafana.

## Architecture

```
TSDB reader → byte-budget batcher → snappy remote-write
                                       │
                                       ▼
                              cortex-tenant
                      (projectId label → X-Scope-OrgID)
                                       │
                                       ▼
                              Mimir (per-tenant blocks)
```

APISIX in front of Grafana and Mimir validates Keystone tokens on every
request and injects the tenant header, so unauthenticated access to
`/prometheus/*` returns 401.

## Prerequisites

- Kubernetes 1.28+ (tested on k3s)
- Worker node with the source TSDB mounted at `/mnt/data_dump/data`.
  Recommended size: **16 vCPU / 64 GiB RAM** per worker running the
  ingester/kafka write path at the default `QUEUE_CONCURRENCY=8`. Scale
  down (`QUEUE_CONCURRENCY=4` on 30 GiB, `=2` on 16 GiB) for smaller
  nodes.
- Worker disk **sized for bulk migration**: Mimir's ingester WAL +
  Kafka log grow during ingestion and only drain after blocks are
  shipped to S3. Budget ~2× the in-flight dataset size on each worker
  hosting an ingester or Kafka broker. `local-path` does not enforce
  PVC quotas, so a too-small disk will fill silently and OOM the node.
- S3-compatible object store with three buckets (blocks, ruler, alerts)
- OpenStack Keystone v3 endpoint (password auth)
- `kubectl`, `helm` 3.x, `docker`, `bash`, `python3`
- Go 1.25+ for building the migrator image

## Deploy

All scripts run on the k3s master node. `deploy.sh` is idempotent.

```bash
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
export MIMIR_S3_ENDPOINT=https://s3.example
export MIMIR_S3_ACCESS_KEY=... MIMIR_S3_SECRET_KEY=...
export MIMIR_S3_REGION=your-region
export MIMIR_S3_BUCKET_BLOCKS=mimir-blocks
export MIMIR_S3_BUCKET_RULER=mimir-ruler
export MIMIR_S3_BUCKET_ALERTS=mimir-alerts
export KEYSTONE_URL=https://keystone.example/
export CLUSTER_GATEWAY_URL=http://<master-ip>:31545
export GRAFANA_ADMIN_PASSWORD='change-me'

cd deploy
TARGET_NODE_HOST=<worker-ip> ./rebuild-migrator.sh
./deploy.sh
./verify.sh
```

For 3-node clusters, set `MIMIR_VALUES_FILE=mimir-values-3node.yaml`
before `./deploy.sh`.

## Run a migration

```bash
cd deploy
./run-migration.sh run --wait -f tenants.txt
./run-migration.sh status
./run-migration.sh wait --all
./run-migration.sh delete --all
```

The script patches `monitoring/mimir-runtime` with per-tenant overrides,
waits for Mimir to reload, port-forwards the migrator API, and POSTs the
tenant list. With `--wait` it blocks until `active + pending` reaches
zero and exits non-zero on any failure.

## Configuration

Migrator env vars (`deploy/migrator-deployment.yaml`):

| Variable | Default | Purpose |
|---|---|---|
| `CORTEX_URL` | *(required)* | Must end with `/push`. Boot probe fails fast otherwise. |
| `REDIS_ADDR` | `localhost:6379` | asynq + progress store |
| `TSDB_PATH` | `/data/tsdb` | Source TSDB mount |
| `QUEUE_CONCURRENCY` | `8` | Parallel tenant migrations. Requires 64 GiB workers + Kafka tuning (below). Drop to 4 on 30 GiB, 2 on 16 GiB. |
| `WRITER_CONCURRENCY` | `4` | Remote-write goroutines per tenant |
| `MAX_BATCH_BYTES` | `3145728` | 3 MiB request budget (25% headroom under Mimir's 4 MiB limit) |
| `HTTP_ADDR` | `:8090` | API bind address |
| `DRY_RUN` | `false` | Skip the POST but still read + batch |

### Kafka tuning for QUEUE_CONCURRENCY ≥ 6

Mimir v2.15+ uses Kafka-backed ingest-storage. Chart defaults
(2 partitions, 10s `write_timeout`) saturate at higher migrator
concurrency. `deploy/mimir-values-3node.yaml` ships these overrides:

- `KAFKA_CFG_NUM_PARTITIONS=8` — takes effect only at topic creation
- `write_timeout: 30s`
- `producer_max_buffered_bytes: 2147483648` (2 GiB)

If re-using an existing Kafka, delete and recreate the `ingest` topic.

## Constraints

- **Single migrator replica.** The pod reads the TSDB through a
  `hostPath` volume, so `replicas: 1` and `nodeSelector` pin it to one
  worker. Edit the selector in `deploy/migrator-deployment.yaml`.
- **runAsUser must match TSDB owner.** The manifest sets UID/GID 65534
  (Prometheus default); adjust if your TSDB has different ownership.
- **Query visibility delay.** After migration, the bucket-index and
  store-gateway sync with defaults of 15 min each. `mimir-values.yaml`
  overrides both to 1 min.

## Local development

```bash
cd services/migrator
go test ./...
go test -race ./...
go vet ./...

docker build -t mimir-openstack-tenant-migrator:latest .
```

Tests use `miniredis` in-process; no live cluster needed.
`mimirconfig.NoopApplier` is wired when `KUBECONFIG` is unset.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `CrashLoopBackOff`, logs say `CORTEX_URL must end with /push` | Misconfigured env | Fix `migrator-deployment.yaml` |
| Tasks fail with `out of order sample` / `rate limit exceeded` | Per-tenant overrides didn't apply | Check `mimir-runtime` ConfigMap |
| `POST /migrate` returns 500 | Missing RBAC | `kubectl apply -f deploy/migrator-rbac.yaml` |
| `records have timed out before they were able to be produced` | Kafka saturated | Apply the Kafka tuning above |
| Unauthenticated `/prometheus/*` returns 200 | Lua plugin not mounted | Check `cm/mimir-keystone-authz-plugin` |
| Pod stuck `Pending` | `nodeSelector` mismatch | Match your worker's hostname |
| Empty queries after migration | Compactor / store-gateway sync | Wait 1–2 min or restart `mimir-compactor-0` + store-gateway |

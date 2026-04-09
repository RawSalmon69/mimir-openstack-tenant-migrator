#!/usr/bin/env bash
#
# deploy.sh — Full deployment orchestration for Mimir monitoring stack.
# Reads environment variables, substitutes placeholders in values files,
# then installs all components in dependency order.
#
# Required environment variables:
#   KUBECONFIG               — path to kubeconfig for target cluster
#   MIMIR_S3_ENDPOINT        — S3-compatible endpoint
#   MIMIR_S3_ACCESS_KEY      — S3 access key
#   MIMIR_S3_SECRET_KEY      — S3 secret key
#   MIMIR_S3_REGION          — S3 region
#   MIMIR_S3_BUCKET_BLOCKS   — bucket for blocks
#   MIMIR_S3_BUCKET_RULER    — bucket for ruler
#   MIMIR_S3_BUCKET_ALERTS   — bucket for alerts
#   KEYSTONE_URL             — OpenStack Keystone auth URL
#   CLUSTER_GATEWAY_URL      — public gateway URL (e.g. http://<node-ip>:<port>)
#   GRAFANA_ADMIN_PASSWORD   — Grafana admin password
#
# Optional:
#   NAMESPACE                — Kubernetes namespace (default: monitoring)
#   APISIX_ADMIN_KEY         — APISIX admin API key (default: from apisix-values.yaml)
#   DRY_RUN                  — set to "true" to print commands without executing

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NAMESPACE="${NAMESPACE:-monitoring}"
DRY_RUN="${DRY_RUN:-false}"
APISIX_ADMIN_KEY="${APISIX_ADMIN_KEY:-edd1c9f034335f136f87ad84b625c8f1}"

# ── Validate required env vars ──────────────────────────────────────────────
REQUIRED_VARS=(
  KUBECONFIG
  MIMIR_S3_ENDPOINT MIMIR_S3_ACCESS_KEY MIMIR_S3_SECRET_KEY MIMIR_S3_REGION
  MIMIR_S3_BUCKET_BLOCKS MIMIR_S3_BUCKET_RULER MIMIR_S3_BUCKET_ALERTS
  KEYSTONE_URL CLUSTER_GATEWAY_URL GRAFANA_ADMIN_PASSWORD
)
MISSING=()
for var in "${REQUIRED_VARS[@]}"; do
  if [[ -z "${!var:-}" ]]; then
    MISSING+=("$var")
  fi
done
if [[ ${#MISSING[@]} -gt 0 ]]; then
  echo "ERROR: Missing required environment variables:" >&2
  printf '  %s\n' "${MISSING[@]}" >&2
  exit 1
fi

# ── Helper: run or print ────────────────────────────────────────────────────
run() {
  echo "+ $*" >&2
  if [[ "$DRY_RUN" != "true" ]]; then
    "$@"
  fi
}

# ── Prepare working copies of values files with placeholders substituted ────
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

echo "==> Preparing values files in $WORK_DIR"
for f in mimir-values.yaml grafana-values.yaml; do
  cp "$SCRIPT_DIR/$f" "$WORK_DIR/$f"
done

# Strip scheme from S3 endpoint — Mimir's thanos S3 client expects host[:port] only;
# the insecure: false flag already controls HTTPS usage.
MIMIR_S3_ENDPOINT_HOST="${MIMIR_S3_ENDPOINT#https://}"
MIMIR_S3_ENDPOINT_HOST="${MIMIR_S3_ENDPOINT_HOST#http://}"

# Substitute Mimir S3 placeholders
sed -i.bak \
  -e "s|\${MIMIR_S3_ENDPOINT}|${MIMIR_S3_ENDPOINT_HOST}|g" \
  -e "s|\${MIMIR_S3_ACCESS_KEY}|${MIMIR_S3_ACCESS_KEY}|g" \
  -e "s|\${MIMIR_S3_SECRET_KEY}|${MIMIR_S3_SECRET_KEY}|g" \
  -e "s|\${MIMIR_S3_REGION}|${MIMIR_S3_REGION}|g" \
  -e "s|\${MIMIR_S3_BUCKET_BLOCKS}|${MIMIR_S3_BUCKET_BLOCKS}|g" \
  -e "s|\${MIMIR_S3_BUCKET_RULER}|${MIMIR_S3_BUCKET_RULER}|g" \
  -e "s|\${MIMIR_S3_BUCKET_ALERTS}|${MIMIR_S3_BUCKET_ALERTS}|g" \
  "$WORK_DIR/mimir-values.yaml"

# Substitute Grafana root_url placeholder
sed -i.bak \
  -e "s|\${CLUSTER_GATEWAY_URL}|${CLUSTER_GATEWAY_URL}|g" \
  "$WORK_DIR/grafana-values.yaml"

# Prepare routes JSON with Keystone/gateway substitutions
cp "$SCRIPT_DIR/apisix-routes.json" "$WORK_DIR/apisix-routes.json"
sed -i.bak \
  -e "s|\${KEYSTONE_URL}|${KEYSTONE_URL}|g" \
  -e "s|\${CLUSTER_GATEWAY_URL}|${CLUSTER_GATEWAY_URL}|g" \
  "$WORK_DIR/apisix-routes.json"

echo "==> Values files prepared."

# Deployment uses helm install (via upgrade --install for idempotency)
# ── Step 1: Add Helm repos ──────────────────────────────────────────────────
echo "==> Step 1: Adding Helm repos"
run helm repo add grafana https://grafana.github.io/helm-charts 2>/dev/null || true
run helm repo add apisix https://charts.apiseven.com 2>/dev/null || true
run helm repo add bitnami https://charts.bitnami.com/bitnami 2>/dev/null || true
run helm repo add cortex-tenant https://blind-oracle.github.io/cortex-tenant 2>/dev/null || true
run helm repo update

# ── Step 2: Create namespace ────────────────────────────────────────────────
echo "==> Step 2: Creating namespace $NAMESPACE"
run kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | run kubectl apply -f -

# ── Step 3: Install Mimir ───────────────────────────────────────────────────
echo "==> Step 3: Installing Mimir"
run helm upgrade --install mimir grafana/mimir-distributed \
  -n "$NAMESPACE" \
  -f "$WORK_DIR/mimir-values.yaml" \
  --wait --timeout 10m

# ── Step 4: Install cortex-tenant ───────────────────────────────────────────
echo "==> Step 4: Installing cortex-tenant"
run helm upgrade --install cortex-tenant cortex-tenant/cortex-tenant \
  -n "$NAMESPACE" \
  -f "$SCRIPT_DIR/cortex-tenant-values.yaml" \
  --wait --timeout 5m

# ── Step 5: Install Redis ───────────────────────────────────────────────────
echo "==> Step 5: Installing Redis"
run helm upgrade --install redis bitnami/redis \
  -n "$NAMESPACE" \
  -f "$SCRIPT_DIR/redis-values.yaml" \
  --wait --timeout 5m

# ── Step 6: Apply Lua plugin ConfigMaps ─────────────────────────────────────
echo "==> Step 6: Applying Lua plugin ConfigMaps"
run kubectl apply -n "$NAMESPACE" -f "$SCRIPT_DIR/configmap-mimir-keystone-authz.yaml"
run kubectl apply -n "$NAMESPACE" -f "$SCRIPT_DIR/configmap-grafana-keystone-authz.yaml"

# ── Step 7: Install APISIX ─────────────────────────────────────────────────
echo "==> Step 7: Installing APISIX"
run helm upgrade --install apisix apisix/apisix \
  -n "$NAMESPACE" \
  -f "$SCRIPT_DIR/apisix-values.yaml" \
  --wait --timeout 10m

# ── Step 8: Install Grafana ─────────────────────────────────────────────────
echo "==> Step 8: Installing Grafana"
run helm upgrade --install grafana grafana/grafana \
  -n "$NAMESPACE" \
  -f "$WORK_DIR/grafana-values.yaml" \
  --set adminPassword="${GRAFANA_ADMIN_PASSWORD}" \
  --wait --timeout 5m

# ── Step 9: Wait for pods ──────────────────────────────────────────────────
echo "==> Step 9: Waiting for all pods to be ready"
run kubectl -n "$NAMESPACE" wait --for=condition=Ready pods --all --timeout=300s

# ── Step 10: Configure APISIX routes via admin API ─────────────────────────
echo "==> Step 10: Configuring APISIX routes"
# K005: APISIX pod has no curl — use bash /dev/tcp for admin API
APISIX_POD=$(kubectl -n "$NAMESPACE" get pods -l app.kubernetes.io/name=apisix -o jsonpath='{.items[0].metadata.name}')
ROUTES_JSON="$WORK_DIR/apisix-routes.json"
NUM_ROUTES=$(python3 -c "import json; print(len(json.load(open('$ROUTES_JSON'))))")

for i in $(seq 0 $((NUM_ROUTES - 1))); do
  ROUTE_ID=$(python3 -c "import json; print(json.load(open('$ROUTES_JSON'))[$i]['id'])")
  ROUTE_BODY=$(python3 -c "import json; r=json.load(open('$ROUTES_JSON'))[$i]; del r['id']; print(json.dumps(r))")

  echo "  Creating route: $ROUTE_ID"
  if [[ "$DRY_RUN" != "true" ]]; then
    kubectl -n "$NAMESPACE" exec "$APISIX_POD" -- bash -c "
      exec 3<>/dev/tcp/127.0.0.1/9180
      BODY='${ROUTE_BODY//\'/\\\'}'
      LEN=\${#BODY}
      printf 'PUT /apisix/admin/routes/${ROUTE_ID} HTTP/1.1\r\nHost: 127.0.0.1\r\nX-API-KEY: ${APISIX_ADMIN_KEY}\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s' \"\$LEN\" \"\$BODY\" >&3
      cat <&3
      exec 3>&-
    "
    echo ""
  fi
done

# ── Step 11: Apply migrator RBAC and Deployment ────────────────────────────
echo "==> Step 11: Applying migrator RBAC and Deployment"
run kubectl apply -n "$NAMESPACE" -f "$SCRIPT_DIR/migrator-rbac.yaml"
run kubectl apply -n "$NAMESPACE" -f "$SCRIPT_DIR/migrator-service.yaml"
run kubectl apply -n "$NAMESPACE" -f "$SCRIPT_DIR/migrator-tsdb-pvc.yaml"
run kubectl apply -n "$NAMESPACE" -f "$SCRIPT_DIR/migrator-deployment.yaml"

# ── Step 12: Run verification ──────────────────────────────────────────────
echo "==> Step 12: Running verification"
run bash "$SCRIPT_DIR/verify.sh"

echo ""
echo "=== Deployment complete ==="

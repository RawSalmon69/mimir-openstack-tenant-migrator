#!/usr/bin/env bash
# deploy.sh — Deploy the full Mimir monitoring stack.
# Run on the k3s master node. See README for required env vars.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NAMESPACE="${NAMESPACE:-monitoring}"
DRY_RUN="${DRY_RUN:-false}"
APISIX_ADMIN_KEY="${APISIX_ADMIN_KEY:-edd1c9f034335f136f87ad84b625c8f1}"
MIMIR_VALUES_FILE="${MIMIR_VALUES_FILE:-mimir-values.yaml}"

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

run() {
  echo "+ $*" >&2
  if [[ "$DRY_RUN" != "true" ]]; then
    "$@"
  fi
}

# Prepare working copies with placeholders substituted
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

echo "==> Preparing values files in $WORK_DIR (mimir values: $MIMIR_VALUES_FILE)"
cp "$SCRIPT_DIR/$MIMIR_VALUES_FILE" "$WORK_DIR/mimir-values.yaml"
cp "$SCRIPT_DIR/grafana-values.yaml" "$WORK_DIR/grafana-values.yaml"

# Strip scheme — thanos S3 client expects host[:port] only
MIMIR_S3_ENDPOINT_HOST="${MIMIR_S3_ENDPOINT#https://}"
MIMIR_S3_ENDPOINT_HOST="${MIMIR_S3_ENDPOINT_HOST#http://}"

sed -i.bak \
  -e "s|\${MIMIR_S3_ENDPOINT}|${MIMIR_S3_ENDPOINT_HOST}|g" \
  -e "s|\${MIMIR_S3_ACCESS_KEY}|${MIMIR_S3_ACCESS_KEY}|g" \
  -e "s|\${MIMIR_S3_SECRET_KEY}|${MIMIR_S3_SECRET_KEY}|g" \
  -e "s|\${MIMIR_S3_REGION}|${MIMIR_S3_REGION}|g" \
  -e "s|\${MIMIR_S3_BUCKET_BLOCKS}|${MIMIR_S3_BUCKET_BLOCKS}|g" \
  -e "s|\${MIMIR_S3_BUCKET_RULER}|${MIMIR_S3_BUCKET_RULER}|g" \
  -e "s|\${MIMIR_S3_BUCKET_ALERTS}|${MIMIR_S3_BUCKET_ALERTS}|g" \
  "$WORK_DIR/mimir-values.yaml"

sed -i.bak \
  -e "s|\${CLUSTER_GATEWAY_URL}|${CLUSTER_GATEWAY_URL}|g" \
  "$WORK_DIR/grafana-values.yaml"

cp "$SCRIPT_DIR/apisix-routes.json" "$WORK_DIR/apisix-routes.json"
sed -i.bak \
  -e "s|\${KEYSTONE_URL}|${KEYSTONE_URL}|g" \
  -e "s|\${CLUSTER_GATEWAY_URL}|${CLUSTER_GATEWAY_URL}|g" \
  "$WORK_DIR/apisix-routes.json"

echo "==> Values files prepared."

# Step 1: Helm repos
echo "==> Step 1: Adding Helm repos"
run helm repo add grafana https://grafana.github.io/helm-charts 2>/dev/null || true
run helm repo add apisix https://charts.apiseven.com 2>/dev/null || true
run helm repo add bitnami https://charts.bitnami.com/bitnami 2>/dev/null || true
run helm repo add cortex-tenant https://blind-oracle.github.io/cortex-tenant 2>/dev/null || true
run helm repo update

# Step 2: Namespace
echo "==> Step 2: Creating namespace $NAMESPACE"
run kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | run kubectl apply -f -

# Step 2.5: Node sysctl tuning (vm.max_map_count for Mimir ingester mmap fan-out)
echo "==> Step 2.5: Applying node sysctl DaemonSet"
run kubectl apply -n "$NAMESPACE" -f "$SCRIPT_DIR/node-sysctl-daemonset.yaml"
run kubectl -n "$NAMESPACE" rollout status ds/node-sysctl --timeout=2m

# Step 3: Mimir
echo "==> Step 3: Installing Mimir"
run helm upgrade --install mimir grafana/mimir-distributed \
  -n "$NAMESPACE" \
  -f "$WORK_DIR/mimir-values.yaml" \
  --wait --timeout 10m

# Step 4: cortex-tenant
echo "==> Step 4: Installing cortex-tenant"
run helm upgrade --install cortex-tenant cortex-tenant/cortex-tenant \
  -n "$NAMESPACE" \
  -f "$SCRIPT_DIR/cortex-tenant-values.yaml" \
  --wait --timeout 5m

# Step 5: Redis
echo "==> Step 5: Installing Redis"
run helm upgrade --install redis bitnami/redis \
  -n "$NAMESPACE" \
  -f "$SCRIPT_DIR/redis-values.yaml" \
  --wait --timeout 5m

# Step 6: Lua plugin ConfigMaps
echo "==> Step 6: Applying Lua plugin ConfigMaps"
run kubectl apply -n "$NAMESPACE" -f "$SCRIPT_DIR/configmap-mimir-keystone-authz.yaml"
run kubectl apply -n "$NAMESPACE" -f "$SCRIPT_DIR/configmap-grafana-keystone-authz.yaml"

# Step 7: APISIX
echo "==> Step 7: Installing APISIX"
run helm upgrade --install apisix apisix/apisix \
  -n "$NAMESPACE" \
  -f "$SCRIPT_DIR/apisix-values.yaml" \
  --wait --timeout 10m

# Step 8: Grafana
echo "==> Step 8: Installing Grafana"
run helm upgrade --install grafana grafana/grafana \
  -n "$NAMESPACE" \
  -f "$WORK_DIR/grafana-values.yaml" \
  --set adminPassword="${GRAFANA_ADMIN_PASSWORD}" \
  --wait --timeout 5m

# Step 9: Wait for pods (exclude Completed helm hook pods)
echo "==> Step 9: Waiting for all pods to be ready"
run kubectl -n "$NAMESPACE" wait --for=condition=Ready pods --all \
  --field-selector=status.phase!=Succeeded --timeout=300s

# Step 10: APISIX routes (pod has no curl, uses /dev/tcp)
echo "==> Step 10: Configuring APISIX routes"
APISIX_POD=$(kubectl -n "$NAMESPACE" get pods -l app.kubernetes.io/name=apisix -o jsonpath='{.items[0].metadata.name}')
ROUTES_JSON="$WORK_DIR/apisix-routes.json"
NUM_ROUTES=$(python3 -c "import json; print(len(json.load(open('$ROUTES_JSON'))))")

for i in $(seq 0 $((NUM_ROUTES - 1))); do
  ROUTE_ID=$(python3 -c "import json; print(json.load(open('$ROUTES_JSON'))[$i]['id'])")
  ROUTE_BODY=$(python3 -c "import json; r=json.load(open('$ROUTES_JSON'))[$i]; del r['id']; print(json.dumps(r))")

  echo "  Creating route: $ROUTE_ID"
  if [[ "$DRY_RUN" != "true" ]]; then
    # Pipe body via stdin to avoid single-quote escaping issues
    kubectl -n "$NAMESPACE" exec -i "$APISIX_POD" -- bash -c "
      BODY=\$(cat)
      exec 3<>/dev/tcp/127.0.0.1/9180
      LEN=\${#BODY}
      printf 'PUT /apisix/admin/routes/${ROUTE_ID} HTTP/1.1\r\nHost: 127.0.0.1\r\nX-API-KEY: ${APISIX_ADMIN_KEY}\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s' \"\$LEN\" \"\$BODY\" >&3
      cat <&3
      exec 3>&-
    " <<< "$ROUTE_BODY"
    echo ""
  fi
done

# Step 10.5: Disk watchdog (pauses migrator at 80% disk to prevent WAL-fill cascades)
echo "==> Step 10.5: Applying disk-watchdog CronJob"
run kubectl apply -n "$NAMESPACE" -f "$SCRIPT_DIR/disk-watchdog.yaml"

# Step 11: Migrator
echo "==> Step 11: Applying migrator RBAC and Deployment"
run kubectl apply -n "$NAMESPACE" -f "$SCRIPT_DIR/migrator-rbac.yaml"
run kubectl apply -n "$NAMESPACE" -f "$SCRIPT_DIR/migrator-service.yaml"
run kubectl apply -n "$NAMESPACE" -f "$SCRIPT_DIR/migrator-tsdb-pvc.yaml"
run kubectl apply -n "$NAMESPACE" -f "$SCRIPT_DIR/migrator-deployment.yaml"

# Step 12: Dashboards
echo "==> Step 12: Importing Grafana dashboards"
run bash "$SCRIPT_DIR/import-dashboards.sh"

# Step 13: Verify
echo "==> Step 13: Running verification"
run bash "$SCRIPT_DIR/verify.sh"

echo ""
echo "=== Deployment complete ==="

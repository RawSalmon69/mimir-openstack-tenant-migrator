#!/usr/bin/env bash
#
# import-dashboards.sh — Import all 7 Grafana dashboards via port-forward.
# K010: Uses kubectl port-forward to reach Grafana admin API.
# K012: Dashboard import with inputs[] remapping DS_PROMETHEUS-PROD to mimir-tenant.
#
# Required environment variables:
#   GRAFANA_ADMIN_PASSWORD   — Grafana admin password
#
# Optional:
#   NAMESPACE                — Kubernetes namespace (default: monitoring)
#   GRAFANA_DS_UID           — Datasource UID in Grafana (default: mimir-tenant)
#   LOCAL_PORT               — Local port for port-forward (default: 3000)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NAMESPACE="${NAMESPACE:-monitoring}"
GRAFANA_DS_UID="${GRAFANA_DS_UID:-mimir-tenant}"
LOCAL_PORT="${LOCAL_PORT:-3000}"
DASHBOARD_DIR="$SCRIPT_DIR/../../grafana-monitor-system-main-grafana-dashboards-general/grafana/dashboards/general"

if [[ -z "${GRAFANA_ADMIN_PASSWORD:-}" ]]; then
  echo "ERROR: GRAFANA_ADMIN_PASSWORD is required" >&2
  exit 1
fi

GRAFANA_URL="http://admin:${GRAFANA_ADMIN_PASSWORD}@localhost:${LOCAL_PORT}"

# ── Start port-forward ──────────────────────────────────────────────────────
echo "==> Starting kubectl port-forward to Grafana on localhost:${LOCAL_PORT}"
kubectl -n "$NAMESPACE" port-forward svc/grafana "${LOCAL_PORT}:80" &
PF_PID=$!
trap 'kill $PF_PID 2>/dev/null || true' EXIT

# Wait for port-forward to be ready
echo "  Waiting for port-forward..."
for i in $(seq 1 30); do
  if curl -sf "${GRAFANA_URL}/api/health" >/dev/null 2>&1; then
    echo "  Grafana reachable."
    break
  fi
  if [[ $i -eq 30 ]]; then
    echo "ERROR: Grafana not reachable after 30s" >&2
    exit 1
  fi
  sleep 1
done

# ── Ensure datasource exists ───────────────────────────────────────────────
echo "==> Checking/creating datasource '${GRAFANA_DS_UID}'"
DS_EXISTS=$(curl -sf "${GRAFANA_URL}/api/datasources/uid/${GRAFANA_DS_UID}" -o /dev/null -w '%{http_code}' || echo "000")
if [[ "$DS_EXISTS" != "200" ]]; then
  echo "  Datasource not found, creating..."
  curl -sf -X POST "${GRAFANA_URL}/api/datasources" \
    -H "Content-Type: application/json" \
    -d "{
      \"name\": \"Prometheus-Prod\",
      \"type\": \"prometheus\",
      \"uid\": \"${GRAFANA_DS_UID}\",
      \"url\": \"http://mimir-nginx.${NAMESPACE}.svc.cluster.local:80/prometheus\",
      \"access\": \"proxy\",
      \"isDefault\": true
    }" || echo "  WARNING: datasource creation failed (may already exist with different uid)"
fi

# ── Import dashboards ──────────────────────────────────────────────────────
DASHBOARDS=(
  "Compute_Instances.json"
  "DBaaS_cadvisor.json"
  "DBaaS_mysql.json"
  "DBaaS_node.json"
  "ING_Instances.json"
  "Load_Balancers.json"
  "Objects_Storage.json"
)

IMPORTED=0
FAILED=0

for db_file in "${DASHBOARDS[@]}"; do
  db_path="$DASHBOARD_DIR/$db_file"
  if [[ ! -f "$db_path" ]]; then
    echo "  WARNING: $db_file not found, skipping"
    FAILED=$((FAILED + 1))
    continue
  fi

  echo "  Importing $db_file ..."

  # Build the import payload with inputs[] remapping (K012)
  # Dynamically map ALL __inputs datasource entries to our single Mimir datasource
  # Write to temp file to avoid "Argument list too long" with large dashboards (K-new)
  PAYLOAD_FILE=$(mktemp /tmp/dashboard_import_XXXXXX.json)
  python3 -c "
import json, sys
with open('$db_path') as f:
    dashboard = json.load(f)
# Remove id so Grafana assigns a new one
dashboard.pop('id', None)
# Build inputs[] for every datasource __input in the dashboard
inputs = []
for inp in dashboard.get('__inputs', []):
    if inp.get('type') == 'datasource':
        inputs.append({
            'name': inp['name'],
            'type': 'datasource',
            'pluginId': inp.get('pluginId', 'prometheus'),
            'value': '${GRAFANA_DS_UID}'
        })
if not inputs:
    # Fallback: map DS_PROMETHEUS-PROD if no __inputs found
    inputs.append({
        'name': 'DS_PROMETHEUS-PROD',
        'type': 'datasource',
        'pluginId': 'prometheus',
        'value': '${GRAFANA_DS_UID}'
    })
payload = {
    'dashboard': dashboard,
    'overwrite': True,
    'inputs': inputs,
    'folderId': 0
}
print(json.dumps(payload))
" > "$PAYLOAD_FILE"

  RESP=$(curl -sf -X POST "${GRAFANA_URL}/api/dashboards/import" \
    -H "Content-Type: application/json" \
    -d @"$PAYLOAD_FILE" 2>&1) && {
    echo "    OK: $(echo "$RESP" | python3 -c 'import json,sys; r=json.load(sys.stdin); print(r.get("slug","imported"))' 2>/dev/null || echo 'imported')"
    IMPORTED=$((IMPORTED + 1))
  } || {
    echo "    FAILED: $db_file"
    FAILED=$((FAILED + 1))
  }
  rm -f "$PAYLOAD_FILE"
done

echo ""
echo "=== Dashboard import complete: ${IMPORTED} imported, ${FAILED} failed ==="

#!/usr/bin/env bash
# import-dashboards.sh — Import Grafana dashboards via port-forward.
# Uses the auth.proxy X-GRAFANAAUTH-USER: admin header (chart config has
# disable_login_form: true which blocks basic auth for API calls — this
# is the same path production Keystone users take, just as "admin").

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NAMESPACE="${NAMESPACE:-monitoring}"
GRAFANA_DS_UID="${GRAFANA_DS_UID:-mimir-tenant}"
LOCAL_PORT="${LOCAL_PORT:-3000}"
DASHBOARD_DIR="$SCRIPT_DIR/../grafana-monitor-system-main-grafana-dashboards-general/grafana/dashboards/general"

GRAFANA_URL="http://localhost:${LOCAL_PORT}"
# Grafana is deployed with auth.proxy enabled (X-GRAFANAAUTH-USER header) +
# disable_login_form: true. Basic auth returns 302 to /login for API calls.
# Import must use the auth-proxy header; "admin" is the Grafana admin user
# created by the chart with --set adminPassword, inheriting Org Admin rights.
AUTH_HEADER="X-GRAFANAAUTH-USER: admin"

# ── Start port-forward ──────────────────────────────────────────────────────
echo "==> Starting kubectl port-forward to Grafana on localhost:${LOCAL_PORT}"
kubectl -n "$NAMESPACE" port-forward svc/grafana "${LOCAL_PORT}:80" &
PF_PID=$!
trap 'kill $PF_PID 2>/dev/null || true' EXIT

# Wait for port-forward + auth-proxy handshake to be ready
echo "  Waiting for Grafana..."
for i in $(seq 1 30); do
  if curl -sf -H "$AUTH_HEADER" "${GRAFANA_URL}/api/user" >/dev/null 2>&1; then
    echo "  Grafana reachable (auth-proxy accepted admin)."
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
DS_EXISTS=$(curl -sf -H "$AUTH_HEADER" "${GRAFANA_URL}/api/datasources/uid/${GRAFANA_DS_UID}" -o /dev/null -w '%{http_code}' || echo "000")
if [[ "$DS_EXISTS" != "200" ]]; then
  echo "  Datasource not found, creating..."
  curl -sf -X POST -H "$AUTH_HEADER" "${GRAFANA_URL}/api/datasources" \
    -H "Content-Type: application/json" \
    -d "{
      \"name\": \"Prometheus-Prod\",
      \"type\": \"prometheus\",
      \"uid\": \"${GRAFANA_DS_UID}\",
      \"url\": \"http://mimir-gateway.${NAMESPACE}.svc.cluster.local:80/prometheus\",
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

  # Build the import payload with inputs[] remapping
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

  # POST + validate response body actually contains imported:true.
  # `curl -sf` accepts 302 redirects to /login as success, so import
  # must be verified by parsing the response body.
  RESP=$(curl -sS -X POST -H "$AUTH_HEADER" "${GRAFANA_URL}/api/dashboards/import" \
    -H "Content-Type: application/json" \
    -d @"$PAYLOAD_FILE" 2>&1)

  OK=$(echo "$RESP" | python3 -c 'import json,sys
try:
    d=json.loads(sys.stdin.read())
    print("yes" if d.get("imported") is True and d.get("slug") else "no")
except Exception:
    print("no")
' 2>/dev/null)

  if [[ "$OK" == "yes" ]]; then
    SLUG=$(echo "$RESP" | python3 -c 'import json,sys; print(json.loads(sys.stdin.read()).get("slug",""))')
    echo "    OK: $SLUG"
    IMPORTED=$((IMPORTED + 1))
  else
    echo "    FAILED: $db_file — response: $(echo "$RESP" | head -c 200)"
    FAILED=$((FAILED + 1))
  fi
  rm -f "$PAYLOAD_FILE"
done

echo ""
echo "=== Dashboard import complete: ${IMPORTED} imported, ${FAILED} failed ==="

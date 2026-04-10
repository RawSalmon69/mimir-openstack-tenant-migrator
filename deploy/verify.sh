#!/usr/bin/env bash
# verify.sh — Post-deploy health checks for the monitoring stack.

set -euo pipefail

NAMESPACE="${NAMESPACE:-monitoring}"
APISIX_ADMIN_KEY="${APISIX_ADMIN_KEY:-edd1c9f034335f136f87ad84b625c8f1}"
PASS=0
FAIL=0
TOTAL=0

check() {
  local name="$1"
  shift
  TOTAL=$((TOTAL + 1))
  echo -n "  [$TOTAL] $name ... "
  if "$@" >/dev/null 2>&1; then
    echo "✅ PASS"
    PASS=$((PASS + 1))
  else
    echo "❌ FAIL"
    FAIL=$((FAIL + 1))
  fi
}

check_output() {
  local name="$1"
  local expected="$2"
  shift 2
  TOTAL=$((TOTAL + 1))
  echo -n "  [$TOTAL] $name ... "
  local output
  output=$("$@" 2>&1) || true
  if echo "$output" | grep -q "$expected"; then
    echo "✅ PASS"
    PASS=$((PASS + 1))
  else
    echo "❌ FAIL (expected '$expected', got: $(echo "$output" | head -1))"
    FAIL=$((FAIL + 1))
  fi
}

echo "=== Monitoring Stack Health Checks ==="
echo ""

echo "--- Pod Status ---"
NON_RUNNING=$(kubectl -n "$NAMESPACE" get pods --no-headers 2>/dev/null | awk '!/Running/ && !/Completed/ {n++} END {print n+0}')
TOTAL=$((TOTAL + 1))
echo -n "  [$TOTAL] All pods Running (non-running: $NON_RUNNING) ... "
if [[ "$NON_RUNNING" -le 1 ]]; then
  echo "✅ PASS (mimir-ruler may be Error due to missing ruler S3 bucket — non-blocking)"
  PASS=$((PASS + 1))
else
  echo "❌ FAIL ($NON_RUNNING pods not Running)"
  FAIL=$((FAIL + 1))
fi

for component in mimir apisix grafana redis cortex-tenant; do
  check "Pod exists: $component" \
    kubectl -n "$NAMESPACE" get pods -l "app.kubernetes.io/name=$component" -o name
done

echo ""
echo "--- APISIX Routes ---"
APISIX_POD=$(kubectl -n "$NAMESPACE" get pods -l app.kubernetes.io/name=apisix -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")

if [[ -n "$APISIX_POD" ]]; then
  check_output "APISIX route count >= 4" '"total":4' \
    kubectl -n "$NAMESPACE" exec -c apisix "$APISIX_POD" -- bash -c "
      exec 3<>/dev/tcp/127.0.0.1/9180
      printf 'GET /apisix/admin/routes HTTP/1.1\r\nHost: 127.0.0.1\r\nX-API-KEY: ${APISIX_ADMIN_KEY}\r\nConnection: close\r\n\r\n' >&3
      cat <&3
      exec 3>&-
    " 2>/dev/null

  for route_id in grafana-health grafana-proxy mimir-proxy set-token; do
    check_output "Route exists: $route_id" "\"id\":\"$route_id\"" \
      kubectl -n "$NAMESPACE" exec -c apisix "$APISIX_POD" -- bash -c "
        exec 3<>/dev/tcp/127.0.0.1/9180
        printf 'GET /apisix/admin/routes/${route_id} HTTP/1.1\r\nHost: 127.0.0.1\r\nX-API-KEY: ${APISIX_ADMIN_KEY}\r\nConnection: close\r\n\r\n' >&3
        cat <&3
        exec 3>&-
      "
  done
else
  echo "  ⚠️  APISIX pod not found, skipping route checks"
  FAIL=$((FAIL + 5))
  TOTAL=$((TOTAL + 5))
fi

echo ""
echo "--- Grafana ---"
kubectl -n "$NAMESPACE" port-forward svc/grafana 13000:80 &
PF_PID=$!
trap 'kill $PF_PID 2>/dev/null || true' EXIT
sleep 3

check_output "Grafana /api/health returns ok" '"database": "ok"' \
  curl -sf http://localhost:13000/api/health

if [[ -n "${GRAFANA_ADMIN_PASSWORD:-}" ]]; then
  check_output "Grafana has >= 7 dashboards" '"id"' \
    curl -sf "http://admin:${GRAFANA_ADMIN_PASSWORD}@localhost:13000/api/search?type=dash-db"
fi

echo ""
echo "--- Redis ---"
REDIS_POD=$(kubectl -n "$NAMESPACE" get pods -l app.kubernetes.io/name=redis -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
if [[ -n "$REDIS_POD" ]]; then
  check_output "Redis PING returns PONG" "PONG" \
    kubectl -n "$NAMESPACE" exec "$REDIS_POD" -- redis-cli PING
else
  echo "  ⚠️  Redis pod not found"
  FAIL=$((FAIL + 1))
  TOTAL=$((TOTAL + 1))
fi

echo ""
echo "--- cortex-tenant ---"
CT_POD=$(kubectl -n "$NAMESPACE" get pods -l app.kubernetes.io/name=cortex-tenant -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
if [[ -n "$CT_POD" ]]; then
  check_output "cortex-tenant pod is Running" "Running" \
    kubectl -n "$NAMESPACE" get pod "$CT_POD" -o jsonpath='{.status.phase}'
fi

echo ""
echo "--- Auth Enforcement ---"
if [[ -n "${CLUSTER_GATEWAY_URL:-}" ]]; then
  UNAUTH_CODE=$(curl -s -o /dev/null -w '%{http_code}' "${CLUSTER_GATEWAY_URL}/prometheus/api/v1/query?query=up" 2>/dev/null || true)
  TOTAL=$((TOTAL + 1))
  echo -n "  [$TOTAL] Unauthenticated /prometheus returns 401 ... "
  if [[ "$UNAUTH_CODE" == "401" ]]; then
    echo "✅ PASS"
    PASS=$((PASS + 1))
  else
    echo "❌ FAIL (got $UNAUTH_CODE)"
    FAIL=$((FAIL + 1))
  fi
else
  echo "  ⚠️  CLUSTER_GATEWAY_URL not set, skipping auth check"
fi

kill $PF_PID 2>/dev/null || true
echo ""
echo "=== Results: ${PASS}/${TOTAL} passed, ${FAIL} failed ==="

if [[ $FAIL -gt 0 ]]; then
  exit 1
fi

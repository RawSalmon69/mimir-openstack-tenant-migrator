#!/usr/bin/env bash
#
# dry-run.sh — Integration dry-run orchestration for Mimir migration.
#
# Orchestrates the full lifecycle: apply OOO config for test tenants,
# deploy migrator manifests, trigger migration, poll until complete,
# verify data queryable per-tenant through APISIX, and revert OOO config.
#
# Required environment variables:
#   NAMESPACE              — Kubernetes namespace (e.g. monitoring)
#   TEST_PROJECT_IDS       — Comma-separated tenant/project IDs to migrate
#   KEYSTONE_TOKEN_TENANT_A — Keystone auth token for verifying tenant data queries
#   CLUSTER_GATEWAY_URL    — APISIX gateway URL (e.g. https://mimir.example.com)
#   MIGRATOR_IMAGE         — Full image reference for migrator (e.g. registry/migrator:v1)
#
# Optional:
#   KUBECONFIG             — Path to kubeconfig (uses default if unset)
#   POLL_INTERVAL          — Seconds between status polls (default: 15)
#   POLL_TIMEOUT           — Maximum seconds to wait for migration (default: 1800 = 30 min)
#   OOO_WINDOW             — Out-of-order time window (default: 2160h = 90 days)
#   MIGRATOR_PORT          — Local port for port-forward (default: 18090)
#
set -euo pipefail

# ── Required env vars ───────────────────────────────────────────────────────
: "${NAMESPACE:?NAMESPACE is required}"
: "${TEST_PROJECT_IDS:?TEST_PROJECT_IDS is required (comma-separated)}"
: "${KEYSTONE_TOKEN_TENANT_A:?KEYSTONE_TOKEN_TENANT_A is required}"
: "${CLUSTER_GATEWAY_URL:?CLUSTER_GATEWAY_URL is required}"
: "${MIGRATOR_IMAGE:?MIGRATOR_IMAGE is required}"

# ── Defaults ────────────────────────────────────────────────────────────────
POLL_INTERVAL="${POLL_INTERVAL:-15}"
POLL_TIMEOUT="${POLL_TIMEOUT:-1800}"
OOO_WINDOW="${OOO_WINDOW:-2160h}"
MIGRATOR_PORT="${MIGRATOR_PORT:-18090}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# ── Helpers ─────────────────────────────────────────────────────────────────
PF_PID=""
PHASE=""
START_TS=""

ts() { date '+%Y-%m-%dT%H:%M:%S%z'; }

log()  { echo "[$(ts)] $*"; }
info() { echo "[$(ts)] ℹ️  $*"; }
ok()   { echo "[$(ts)] ✅ $*"; }
fail() { echo "[$(ts)] ❌ $*"; }

phase_start() {
  PHASE="$1"
  START_TS="$(date +%s)"
  echo ""
  log "════════════════════════════════════════════════════════"
  log "PHASE: $PHASE"
  log "════════════════════════════════════════════════════════"
}

phase_end() {
  local elapsed=$(( $(date +%s) - START_TS ))
  ok "PHASE COMPLETE: $PHASE (${elapsed}s)"
}

cleanup() {
  if [[ -n "$PF_PID" ]]; then
    kill "$PF_PID" 2>/dev/null || true
    wait "$PF_PID" 2>/dev/null || true
    PF_PID=""
  fi
}
trap cleanup EXIT

die() {
  fail "$1"
  exit 1
}

# Parse comma-separated project IDs into an array.
IFS=',' read -ra PROJECT_IDS <<< "$TEST_PROJECT_IDS"
if [[ ${#PROJECT_IDS[@]} -eq 0 ]]; then
  die "TEST_PROJECT_IDS is empty"
fi
info "Test project IDs: ${PROJECT_IDS[*]}"

# ════════════════════════════════════════════════════════════════════════════
# Phase 1 — Apply OOO config
# ════════════════════════════════════════════════════════════════════════════
phase_start "Apply OOO config"

# Build per-tenant overrides block from the project IDs.
OVERRIDES=""
for pid in "${PROJECT_IDS[@]}"; do
  pid="$(echo "$pid" | xargs)"  # trim whitespace
  OVERRIDES="${OVERRIDES}      \"${pid}\":
        out_of_order_time_window: ${OOO_WINDOW}
"
done

# Generate the ConfigMap with runtime.yaml containing the OOO overrides.
OOO_CONFIGMAP=$(cat <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: mimir-runtime
  namespace: ${NAMESPACE}
data:
  runtime.yaml: |
    overrides:
${OVERRIDES}
EOF
)

info "Applying OOO ConfigMap with ${OOO_WINDOW} window for ${#PROJECT_IDS[@]} tenant(s)"
echo "$OOO_CONFIGMAP" | kubectl apply -f - || die "Failed to apply OOO ConfigMap"
ok "OOO config applied"

# Give Mimir time to pick up the runtime config reload.
info "Waiting 10s for Mimir runtime config reload..."
sleep 10

phase_end

# ════════════════════════════════════════════════════════════════════════════
# Phase 2 — Deploy migrator
# ════════════════════════════════════════════════════════════════════════════
phase_start "Deploy migrator"

info "Applying migrator PVC, Deployment (image: ${MIGRATOR_IMAGE}), and Service"

kubectl apply -f "${SCRIPT_DIR}/migrator-tsdb-pvc.yaml" \
  || die "Failed to apply migrator-tsdb-pvc.yaml"

# Apply deployment with image substitution.
sed "s|image:.*|image: ${MIGRATOR_IMAGE}|" "${SCRIPT_DIR}/migrator-deployment.yaml" \
  | kubectl apply -f - \
  || die "Failed to apply migrator-deployment.yaml"

kubectl apply -f "${SCRIPT_DIR}/migrator-service.yaml" \
  || die "Failed to apply migrator-service.yaml"

info "Waiting for migrator pod to be ready..."
kubectl -n "$NAMESPACE" rollout status deployment/migrator --timeout=120s \
  || die "Migrator deployment did not become ready within 120s"

ok "Migrator deployed and ready"
phase_end

# ════════════════════════════════════════════════════════════════════════════
# Phase 3 — Trigger migration
# ════════════════════════════════════════════════════════════════════════════
phase_start "Trigger migration"

# Set up port-forward to the migrator service.
kubectl -n "$NAMESPACE" port-forward svc/migrator "${MIGRATOR_PORT}:8090" &
PF_PID=$!
sleep 3

# Verify port-forward is alive.
if ! kill -0 "$PF_PID" 2>/dev/null; then
  die "Port-forward to migrator service failed"
fi

# Build the JSON array of project IDs.
JSON_IDS=$(printf '%s\n' "${PROJECT_IDS[@]}" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//' | jq -R . | jq -s .)
MIGRATE_BODY="{\"project_ids\": ${JSON_IDS}}"

info "POST /migrate with project_ids: ${JSON_IDS}"
MIGRATE_RESP=$(curl -sf -X POST \
  -H "Content-Type: application/json" \
  -d "$MIGRATE_BODY" \
  "http://localhost:${MIGRATOR_PORT}/migrate") \
  || die "POST /migrate failed"

info "Migration response: ${MIGRATE_RESP}"
TASK_COUNT=$(echo "$MIGRATE_RESP" | jq '.tasks | length')
if [[ "$TASK_COUNT" -eq 0 ]]; then
  die "No migration tasks were created"
fi
ok "Triggered ${TASK_COUNT} migration task(s)"

phase_end

# ════════════════════════════════════════════════════════════════════════════
# Phase 4 — Poll status until complete or timeout
# ════════════════════════════════════════════════════════════════════════════
phase_start "Poll migration status"

ELAPSED=0
while true; do
  STATUS_RESP=$(curl -sf "http://localhost:${MIGRATOR_PORT}/status") \
    || die "GET /status failed"

  TOTAL=$(echo "$STATUS_RESP" | jq '.total')
  COMPLETED=$(echo "$STATUS_RESP" | jq '.completed')
  FAILED=$(echo "$STATUS_RESP" | jq '.failed')
  ACTIVE=$(echo "$STATUS_RESP" | jq '.active')
  PENDING=$(echo "$STATUS_RESP" | jq '.pending')

  info "Status — total: ${TOTAL}, completed: ${COMPLETED}, active: ${ACTIVE}, pending: ${PENDING}, failed: ${FAILED} (${ELAPSED}s elapsed)"

  # All tasks settled (completed or failed) — no more active/pending.
  if [[ "$ACTIVE" -eq 0 && "$PENDING" -eq 0 ]]; then
    if [[ "$FAILED" -gt 0 ]]; then
      fail "${FAILED} migration task(s) failed"
      echo "$STATUS_RESP" | jq '.tasks[] | select(.state == "failed")'
      die "Migration had failures — aborting"
    fi
    ok "All ${COMPLETED} migration task(s) completed successfully"
    break
  fi

  if [[ "$ELAPSED" -ge "$POLL_TIMEOUT" ]]; then
    die "Migration timed out after ${POLL_TIMEOUT}s (completed: ${COMPLETED}/${TOTAL})"
  fi

  sleep "$POLL_INTERVAL"
  ELAPSED=$(( ELAPSED + POLL_INTERVAL ))
done

# Clean up port-forward — no longer needed for migration.
cleanup

phase_end

# ════════════════════════════════════════════════════════════════════════════
# Phase 5 — Verify data queryable per-tenant through APISIX
# ════════════════════════════════════════════════════════════════════════════
phase_start "Verify data via APISIX"

VERIFY_PASS=0
VERIFY_FAIL=0

for pid in "${PROJECT_IDS[@]}"; do
  pid="$(echo "$pid" | xargs)"
  info "Querying APISIX for tenant: ${pid}"

  # Query the Prometheus API through APISIX with Keystone auth.
  # The Keystone token identifies the tenant; cortex-tenant extracts X-Scope-OrgID.
  HTTP_CODE=$(curl -s -o /tmp/mimir-verify-response.json -w '%{http_code}' \
    -H "X-Auth-Token: ${KEYSTONE_TOKEN_TENANT_A}" \
    "${CLUSTER_GATEWAY_URL}/prometheus/api/v1/query?query=up" 2>/dev/null || echo "000")

  if [[ "$HTTP_CODE" != "200" ]]; then
    fail "Tenant ${pid}: HTTP ${HTTP_CODE} (expected 200)"
    cat /tmp/mimir-verify-response.json 2>/dev/null || true
    VERIFY_FAIL=$((VERIFY_FAIL + 1))
    continue
  fi

  # Check for non-empty result set.
  RESULT_COUNT=$(jq '.data.result | length' /tmp/mimir-verify-response.json 2>/dev/null || echo "0")
  if [[ "$RESULT_COUNT" -gt 0 ]]; then
    ok "Tenant ${pid}: ${RESULT_COUNT} series returned"
    VERIFY_PASS=$((VERIFY_PASS + 1))
  else
    fail "Tenant ${pid}: query returned empty results"
    VERIFY_FAIL=$((VERIFY_FAIL + 1))
  fi
done

info "Verification: ${VERIFY_PASS} passed, ${VERIFY_FAIL} failed out of ${#PROJECT_IDS[@]} tenant(s)"
if [[ "$VERIFY_FAIL" -gt 0 ]]; then
  die "Data verification failed for ${VERIFY_FAIL} tenant(s)"
fi
ok "All tenant data verified via APISIX"

phase_end

# ════════════════════════════════════════════════════════════════════════════
# Phase 6 — Revert OOO config
# ════════════════════════════════════════════════════════════════════════════
phase_start "Revert OOO config"

# Apply mimir-runtime ConfigMap with empty overrides (runtime.yaml: {}).
REVERT_CONFIGMAP=$(cat <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: mimir-runtime
  namespace: ${NAMESPACE}
data:
  runtime.yaml: "{}"
EOF
)

info "Reverting OOO config — applying empty runtime.yaml overrides"
echo "$REVERT_CONFIGMAP" | kubectl apply -f - || die "Failed to revert OOO ConfigMap"
ok "OOO config reverted to empty overrides"

phase_end

# ════════════════════════════════════════════════════════════════════════════
# Summary
# ════════════════════════════════════════════════════════════════════════════
echo ""
log "════════════════════════════════════════════════════════"
log "DRY-RUN COMPLETE"
log "  Tenants migrated: ${#PROJECT_IDS[@]}"
log "  Data verified via APISIX: ${VERIFY_PASS}/${#PROJECT_IDS[@]}"
log "  OOO config: reverted"
log "════════════════════════════════════════════════════════"

#!/usr/bin/env bash
# run-migration.sh — patch overrides, port-forward, POST /migrate.
# Usage: ./run-migration.sh run [-f FILE] [--wait] <tenant-id>...
#        ./run-migration.sh status [task-id]
#        ./run-migration.sh wait <task-id|--all>
#        ./run-migration.sh delete <tenant-id|--all>
# Requires: kubectl, curl, python3.

set -euo pipefail

NAMESPACE="${NAMESPACE:-monitoring}"
RUNTIME_CM="${RUNTIME_CM:-mimir-runtime}"
OOO_WINDOW="${OOO_WINDOW:-2880h}"
INGESTION_RATE="${INGESTION_RATE:-10000000}"
INGESTION_BURST_SIZE="${INGESTION_BURST_SIZE:-20000000}"
RUNTIME_RELOAD_WAIT_SECS="${RUNTIME_RELOAD_WAIT_SECS:-45}"
PORT_FORWARD_PORT="${PORT_FORWARD_PORT:-8090}"
MIGRATOR_DEPLOYMENT="${MIGRATOR_DEPLOYMENT:-migrator}"

PF_PID=""

cleanup() {
  if [ -n "$PF_PID" ] && kill -0 "$PF_PID" 2>/dev/null; then
    kill "$PF_PID" 2>/dev/null || true
    wait "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT INT TERM

usage() {
  sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'
  exit 1
}

require() {
  local tool="$1"
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "ERROR: required tool '$tool' not found in PATH" >&2
    exit 1
  fi
}

# wait_for_runtime_reload polls a Mimir distributor pod's /runtime_config
# endpoint until the given tenant's override key appears (signal that the
# patched ConfigMap has been picked up via fsnotify), or until the timeout
# ceiling elapses. A fast reload exits early; a stalled Mimir hits the
# ceiling and logs a warning instead of hanging silently.
#
# Args:
#   $1 — sentinel tenant id (any one of the patched tenants; first appearance
#        in the YAML response indicates the reload propagated).
#   $2 — ceiling in seconds (defaults to RUNTIME_RELOAD_WAIT_SECS=45).
wait_for_runtime_reload() {
  local sentinel_tenant="$1"
  local ceiling="${2:-45}"
  local interval=2
  local elapsed=0

  # Pick a Mimir distributor pod via standard label selector. Fall back to
  # querier if no distributor exists; fall back to blind sleep if neither.
  local pod
  pod=$(kubectl -n "$NAMESPACE" get pods \
    -l app.kubernetes.io/name=mimir,app.kubernetes.io/component=distributor \
    -o name 2>/dev/null | head -1)
  if [ -z "$pod" ]; then
    pod=$(kubectl -n "$NAMESPACE" get pods \
      -l app.kubernetes.io/name=mimir,app.kubernetes.io/component=querier \
      -o name 2>/dev/null | head -1)
  fi
  if [ -z "$pod" ]; then
    echo "  ⚠ no Mimir distributor/querier pod found — falling back to blind ${ceiling}s sleep"
    sleep "$ceiling"
    return 0
  fi

  while [ "$elapsed" -lt "$ceiling" ]; do
    # Mimir's /runtime_config returns the merged YAML; the first match for
    # ^<tenant>:  in the overrides block confirms the reload propagated.
    if kubectl -n "$NAMESPACE" exec "$pod" -- \
        wget -qO- http://localhost:8080/runtime_config 2>/dev/null \
        | grep -qE "^[[:space:]]+${sentinel_tenant}:[[:space:]]*$"; then
      echo "  ✓ tenant override visible to Mimir after ${elapsed}s (sentinel: $sentinel_tenant)"
      return 0
    fi
    sleep "$interval"
    elapsed=$((elapsed + interval))
  done

  echo "  ⚠ Mimir runtime_config reload did not appear within ${ceiling}s — proceeding anyway; tenants may briefly see DEFAULT overrides" >&2
  return 0
}

start_port_forward() {
  # Port-forward to migrator and wait for /healthz.
  kubectl -n "$NAMESPACE" port-forward "deploy/$MIGRATOR_DEPLOYMENT" \
    "$PORT_FORWARD_PORT:8090" >/dev/null 2>&1 &
  PF_PID=$!

  local i
  for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
    if curl -fsS --max-time 1 "http://localhost:$PORT_FORWARD_PORT/healthz" \
         >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.5
  done

  echo "ERROR: port-forward to $MIGRATOR_DEPLOYMENT did not become ready" >&2
  exit 1
}

patch_runtime_config() {
  # Patch mimir-runtime ConfigMap with per-tenant overrides.
  # Replaces all existing overrides — pass every tenant in one invocation.
  local tenants=("$@")

  local runtime_yaml
  runtime_yaml=$(
    OOO_WINDOW="$OOO_WINDOW" \
    INGESTION_RATE="$INGESTION_RATE" \
    INGESTION_BURST_SIZE="$INGESTION_BURST_SIZE" \
    TENANTS="${tenants[*]}" \
    python3 <<'PY'
import os, sys

tenants = [t for t in os.environ["TENANTS"].split() if t]
if not tenants:
    print("ERROR: no tenants supplied to runtime patch", file=sys.stderr)
    sys.exit(1)

ooo  = os.environ["OOO_WINDOW"]
rate = int(os.environ["INGESTION_RATE"])
burst = int(os.environ["INGESTION_BURST_SIZE"])

lines = ["overrides:"]
for t in tenants:
    lines.append(f"  {t}:")
    lines.append(f"    out_of_order_time_window: {ooo}")
    lines.append(f"    ingestion_rate: {rate}")
    lines.append(f"    ingestion_burst_size: {burst}")

print("\n".join(lines) + "\n", end="")
PY
  )

  local patch_json
  patch_json=$(
    RUNTIME_YAML="$runtime_yaml" \
    python3 -c '
import json, os
body = {"data": {"runtime.yaml": os.environ["RUNTIME_YAML"]}}
print(json.dumps(body))
'
  )

  kubectl -n "$NAMESPACE" patch cm "$RUNTIME_CM" --type merge \
    -p "$patch_json" >/dev/null
}

read_tenants_from_file() {
  # Read tenant IDs from file (one per line, # comments allowed).
  local file="$1"
  if [ ! -r "$file" ]; then
    echo "ERROR: cannot read tenants file: $file" >&2
    exit 1
  fi
  local line
  while IFS= read -r line || [ -n "$line" ]; do
    line="${line%%#*}"
    line="$(echo "$line" | tr -d '[:space:]')"
    if [ -n "$line" ]; then
      echo "$line"
    fi
  done < "$file"
}

cmd_run() {
  local tenants=()
  local wait_after=0

  while [ $# -gt 0 ]; do
    case "$1" in
      -f|--file)
        if [ $# -lt 2 ]; then
          echo "ERROR: $1 requires a file path" >&2
          usage
        fi
        shift
        while IFS= read -r t; do
          tenants+=("$t")
        done < <(read_tenants_from_file "$1")
        shift
        ;;
      -w|--wait)
        wait_after=1
        shift
        ;;
      --)
        shift
        while [ $# -gt 0 ]; do
          tenants+=("$1")
          shift
        done
        ;;
      -*)
        echo "ERROR: unknown flag for 'run': $1" >&2
        usage
        ;;
      *)
        tenants+=("$1")
        shift
        ;;
    esac
  done

  if [ ${#tenants[@]} -eq 0 ]; then
    echo "ERROR: 'run' requires at least one tenant id (positional or via -f FILE)" >&2
    usage
  fi

  require kubectl
  require curl
  require python3

  echo "→ Patching $RUNTIME_CM with overrides for ${#tenants[@]} tenant(s)..."
  patch_runtime_config "${tenants[@]}"
  echo "  ooo_window=$OOO_WINDOW ingestion_rate=$INGESTION_RATE ingestion_burst_size=$INGESTION_BURST_SIZE"
  if [ ${#tenants[@]} -le 10 ]; then
    echo "  tenants: ${tenants[*]}"
  else
    echo "  tenants: ${tenants[0]} ${tenants[1]} ${tenants[2]} ... (${#tenants[@]} total)"
  fi

  echo "→ Polling Mimir runtime_config (timeout ${RUNTIME_RELOAD_WAIT_SECS}s) for tenant override visibility..."
  wait_for_runtime_reload "${tenants[0]}" "$RUNTIME_RELOAD_WAIT_SECS"

  echo "→ Starting port-forward to deploy/$MIGRATOR_DEPLOYMENT on :$PORT_FORWARD_PORT..."
  start_port_forward

  local ids_json
  ids_json=$(python3 -c '
import json, sys
print(json.dumps({"project_ids": sys.argv[1:]}))
' "${tenants[@]}")

  echo "→ POST /migrate (${#tenants[@]} tenant(s))"
  local resp
  resp=$(curl -fsS -X POST \
    "http://localhost:$PORT_FORWARD_PORT/migrate" \
    -H 'content-type: application/json' \
    -d "$ids_json")

  echo "$resp" | python3 -c '
import json, sys
d = json.load(sys.stdin)
print("")
print("→ Tasks enqueued:")
for t in d.get("tasks", []):
    tid = t.get("id", "?")
    pid = t.get("project_id", "?")
    print("  {}  {}".format(tid, pid))
'

  if [ "$wait_after" = "1" ]; then
    echo ""
    wait_all_loop
  else
    echo ""
    echo "Use '$0 status' to see all tasks, '$0 wait <task-id>' for a single task,"
    echo "or '$0 wait --all' to block until every tracked task is terminal."
  fi
}

cmd_status() {
  require kubectl
  require curl
  require python3

  start_port_forward

  if [ $# -eq 0 ]; then
    curl -fsS "http://localhost:$PORT_FORWARD_PORT/status" \
      | python3 -c '
import json, sys
d = json.load(sys.stdin)
total     = d.get("total", 0)
completed = d.get("completed", 0)
active    = d.get("active", 0)
pending   = d.get("pending", 0)
failed    = d.get("failed", 0)
print("total={} completed={} active={} pending={} failed={}".format(
    total, completed, active, pending, failed))
print()
for t in d.get("tasks", []):
    state = t.get("state", "?")
    pid   = t.get("project_id", "")
    sr    = t.get("series_read", 0)
    ss    = t.get("samples_sent", 0)
    err   = t.get("error", "")
    suffix = (" err=" + err[:60]) if err else ""
    print("  {:10s} {:34s}  series={:>5}  samples_sent={:>12}{}".format(
        state, pid, sr, ss, suffix))
'
  else
    local tid="$1"
    curl -fsS "http://localhost:$PORT_FORWARD_PORT/status/$tid" \
      | python3 -m json.tool
  fi
}

restart_port_forward() {
  # Restart port-forward (recovers after pod restart).
  if [ -n "$PF_PID" ] && kill -0 "$PF_PID" 2>/dev/null; then
    kill "$PF_PID" 2>/dev/null || true
    wait "$PF_PID" 2>/dev/null || true
  fi
  PF_PID=""
  start_port_forward
}

wait_all_loop() {
  # Poll /status until all tasks are terminal.
  local interval=10
  local max_polls=720
  local consecutive_failures=0
  local i

  for i in $(seq 1 "$max_polls"); do
    local resp
    resp=$(curl -fsS --max-time 5 "http://localhost:$PORT_FORWARD_PORT/status" 2>/dev/null || true)
    if [ -z "$resp" ]; then
      consecutive_failures=$((consecutive_failures + 1))
      echo "[$i] (status fetch failed — retry $consecutive_failures)"
      if [ "$consecutive_failures" -ge 3 ]; then
        echo "[$i] (restarting port-forward — pod likely restarted)"
        restart_port_forward || true
        consecutive_failures=0
      fi
      sleep "$interval"
      continue
    fi
    consecutive_failures=0

    local counters
    counters=$(echo "$resp" | python3 -c '
import json, sys
d = json.load(sys.stdin)
total     = d.get("total", 0)
completed = d.get("completed", 0)
active    = d.get("active", 0)
pending   = d.get("pending", 0)
failed    = d.get("failed", 0)
print("{} {} {} {} {}".format(total, completed, active, pending, failed))
')
    # shellcheck disable=SC2206
    local arr=($counters)
    local total="${arr[0]}" completed="${arr[1]}" active="${arr[2]}" pending="${arr[3]}" failed="${arr[4]}"

    printf "[%3d] total=%s completed=%s active=%s pending=%s failed=%s\n" \
      "$i" "$total" "$completed" "$active" "$pending" "$failed"

    if [ "$active" = "0" ] && [ "$pending" = "0" ]; then
      echo ""
      if [ "$failed" = "0" ]; then
        echo "✅ All $total tracked tasks reached terminal state ($completed completed, 0 failed)"
        return 0
      else
        echo "⚠️  All tasks terminal: $completed completed, $failed failed"
        return 1
      fi
    fi

    sleep "$interval"
  done

  echo "ERROR: tasks did not all reach terminal state within $((max_polls * interval))s" >&2
  exit 1
}

cmd_wait() {
  if [ $# -ne 1 ]; then
    echo "ERROR: 'wait' requires exactly one task-id or '--all'" >&2
    usage
  fi

  require kubectl
  require curl
  require python3

  start_port_forward

  if [ "$1" = "--all" ] || [ "$1" = "-a" ]; then
    wait_all_loop
    return
  fi

  local tid="$1"
  local interval=10
  local max_polls=360
  local consecutive_failures=0

  local i
  for i in $(seq 1 "$max_polls"); do
    local resp
    resp=$(curl -fsS --max-time 5 "http://localhost:$PORT_FORWARD_PORT/status/$tid" 2>/dev/null || true)
    if [ -z "$resp" ]; then
      consecutive_failures=$((consecutive_failures + 1))
      echo "[$i] (status fetch failed — retry $consecutive_failures)"
      if [ "$consecutive_failures" -ge 3 ]; then
        echo "[$i] (restarting port-forward — pod likely restarted)"
        restart_port_forward || true
        consecutive_failures=0
      fi
      sleep "$interval"
      continue
    fi
    consecutive_failures=0

    local state
    state=$(echo "$resp" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("state","?"))')

    local summary
    summary=$(echo "$resp" | python3 -c '
import json,sys
d = json.load(sys.stdin)
sr = d.get("series_read", 0)
sR = d.get("samples_read", 0)
ss = d.get("samples_sent", 0)
print("series={:>5}  samples_read={:>10}  samples_sent={:>10}".format(sr, sR, ss))
')

    printf "[%3d] state=%-10s %s\n" "$i" "$state" "$summary"

    case "$state" in
      completed|failed)
        echo ""
        echo "→ Final state: $state"
        echo "$resp" | python3 -m json.tool
        [ "$state" = "completed" ] && return 0 || return 1
        ;;
    esac

    sleep "$interval"
  done

  echo "ERROR: task $tid did not reach terminal state within $((max_polls * interval))s" >&2
  exit 1
}

cmd_delete() {
  if [ $# -ne 1 ]; then
    echo "ERROR: 'delete' requires one argument: a tenant-id or '--all'" >&2
    usage
  fi

  require kubectl
  require curl

  start_port_forward

  local target="$1"
  local url

  if [ "$target" = "--all" ] || [ "$target" = "-a" ]; then
    url="http://localhost:$PORT_FORWARD_PORT/tasks?all=true"
    echo "→ DELETE /tasks?all=true"
  else
    # Basic validation on tenant ID format.
    url="http://localhost:$PORT_FORWARD_PORT/tasks?project_id=$target"
    echo "→ DELETE /tasks?project_id=$target"
  fi

  local resp
  if ! resp=$(curl -fsS -X DELETE --max-time 30 "$url" 2>&1); then
    echo "ERROR: delete request failed: $resp" >&2
    exit 1
  fi

  echo "$resp" | python3 -m json.tool
}

main() {
  if [ $# -eq 0 ]; then
    usage
  fi

  local subcommand="$1"
  shift

  case "$subcommand" in
    run)    cmd_run    "$@" ;;
    status) cmd_status "$@" ;;
    wait)   cmd_wait   "$@" ;;
    delete) cmd_delete "$@" ;;
    -h|--help|help) usage ;;
    *)
      echo "ERROR: unknown subcommand '$subcommand'" >&2
      usage
      ;;
  esac
}

main "$@"

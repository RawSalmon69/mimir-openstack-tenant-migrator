#!/usr/bin/env bash
#
# rebuild-migrator.sh — build the migrator image from source, import it
# into the target k3s node's containerd image store, and roll the live
# deployment so the new binary takes over.
#
# Because the migrator uses `imagePullPolicy: Never` and is pinned to a
# specific worker node via `nodeSelector`, the image must exist in
# containerd's image cache on that node — it is not pulled from a
# registry. This script wraps the build → save → transfer → import →
# rollout sequence into one command.
#
# Prerequisites on the machine running this script:
#   - docker (or docker-compatible) for building
#   - kubectl configured for the target cluster
#   - ssh + sshpass (or key-based auth) to the target node
#
# Environment overrides:
#
#   MIGRATOR_IMAGE      image tag to build       (default: docker.io/nipa-mimir/migrator:latest)
#   NAMESPACE           kubernetes namespace     (default: monitoring)
#   DEPLOYMENT          deployment name          (default: migrator)
#   TARGET_NODE_HOST    ssh host of target node  (default: 183.90.173.185, i.e. worker-1)
#   TARGET_NODE_USER    ssh user                 (default: nc-user)
#   TARGET_NODE_PASS    ssh password             (default: empty — assumes key auth)
#   CTR_NAMESPACE       containerd namespace     (default: k8s.io)
#   SKIP_BUILD          set to 1 to skip `docker build`
#   SKIP_IMPORT         set to 1 to skip save+scp+import (useful for dry-run)
#   SKIP_ROLLOUT        set to 1 to skip `kubectl rollout restart`
#   WAIT_TIMEOUT        seconds to wait for new pod Ready (default: 180)

set -euo pipefail

MIGRATOR_IMAGE="${MIGRATOR_IMAGE:-docker.io/nipa-mimir/migrator:latest}"
NAMESPACE="${NAMESPACE:-monitoring}"
DEPLOYMENT="${DEPLOYMENT:-migrator}"
TARGET_NODE_HOST="${TARGET_NODE_HOST:-183.90.173.185}"
TARGET_NODE_USER="${TARGET_NODE_USER:-nc-user}"
TARGET_NODE_PASS="${TARGET_NODE_PASS:-}"
CTR_NAMESPACE="${CTR_NAMESPACE:-k8s.io}"
SKIP_BUILD="${SKIP_BUILD:-}"
SKIP_IMPORT="${SKIP_IMPORT:-}"
SKIP_ROLLOUT="${SKIP_ROLLOUT:-}"
WAIT_TIMEOUT="${WAIT_TIMEOUT:-180}"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
MIGRATOR_DIR="$REPO_ROOT/services/migrator"

TMP_TAR=""
cleanup() {
  [ -n "$TMP_TAR" ] && [ -f "$TMP_TAR" ] && rm -f "$TMP_TAR"
}
trap cleanup EXIT INT TERM

require() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "ERROR: required tool '$1' not found in PATH" >&2
    exit 1
  fi
}

ssh_cmd() {
  if [ -n "$TARGET_NODE_PASS" ]; then
    require sshpass
    sshpass -p "$TARGET_NODE_PASS" ssh \
      -o StrictHostKeyChecking=no \
      -o UserKnownHostsFile=/dev/null \
      -o LogLevel=ERROR \
      "$TARGET_NODE_USER@$TARGET_NODE_HOST" "$@"
  else
    ssh -o StrictHostKeyChecking=no \
        -o UserKnownHostsFile=/dev/null \
        -o LogLevel=ERROR \
        "$TARGET_NODE_USER@$TARGET_NODE_HOST" "$@"
  fi
}

scp_to_node() {
  local src="$1" dst="$2"
  if [ -n "$TARGET_NODE_PASS" ]; then
    require sshpass
    sshpass -p "$TARGET_NODE_PASS" scp \
      -o StrictHostKeyChecking=no \
      -o UserKnownHostsFile=/dev/null \
      -o LogLevel=ERROR \
      "$src" "$TARGET_NODE_USER@$TARGET_NODE_HOST:$dst"
  else
    scp -o StrictHostKeyChecking=no \
        -o UserKnownHostsFile=/dev/null \
        -o LogLevel=ERROR \
        "$src" "$TARGET_NODE_USER@$TARGET_NODE_HOST:$dst"
  fi
}

build_image() {
  if [ -n "$SKIP_BUILD" ]; then
    echo "→ SKIP_BUILD set — skipping docker build"
    return
  fi
  require docker
  echo "→ Building $MIGRATOR_IMAGE from $MIGRATOR_DIR"
  ( cd "$MIGRATOR_DIR" && docker build -t "$MIGRATOR_IMAGE" . )
}

import_to_node() {
  if [ -n "$SKIP_IMPORT" ]; then
    echo "→ SKIP_IMPORT set — skipping save + transfer + ctr import"
    return
  fi
  require docker
  TMP_TAR=$(mktemp -t migrator-image.XXXXXX.tar)
  echo "→ Saving image to $TMP_TAR"
  docker save "$MIGRATOR_IMAGE" -o "$TMP_TAR"

  local remote_tar="/tmp/migrator-image.tar"
  echo "→ Copying image to $TARGET_NODE_USER@$TARGET_NODE_HOST:$remote_tar"
  scp_to_node "$TMP_TAR" "$remote_tar"

  echo "→ Importing into containerd namespace '$CTR_NAMESPACE' on $TARGET_NODE_HOST"
  ssh_cmd "sudo ctr -n $CTR_NAMESPACE images import $remote_tar && sudo rm -f $remote_tar"
}

rollout_restart() {
  if [ -n "$SKIP_ROLLOUT" ]; then
    echo "→ SKIP_ROLLOUT set — skipping kubectl rollout restart"
    return
  fi
  require kubectl
  echo "→ Restarting deploy/$DEPLOYMENT in namespace $NAMESPACE"
  kubectl -n "$NAMESPACE" rollout restart "deploy/$DEPLOYMENT"
  echo "→ Waiting for rollout (timeout ${WAIT_TIMEOUT}s)"
  kubectl -n "$NAMESPACE" rollout status "deploy/$DEPLOYMENT" \
    --timeout="${WAIT_TIMEOUT}s"
  echo "→ Current pods:"
  kubectl -n "$NAMESPACE" get pods -l "app.kubernetes.io/name=$DEPLOYMENT"
}

main() {
  echo "rebuild-migrator.sh"
  echo "  image:      $MIGRATOR_IMAGE"
  echo "  namespace:  $NAMESPACE"
  echo "  deployment: $DEPLOYMENT"
  echo "  target:     $TARGET_NODE_USER@$TARGET_NODE_HOST  (ctr ns: $CTR_NAMESPACE)"
  echo ""

  build_image
  import_to_node
  rollout_restart

  echo ""
  echo "✅ rebuild complete"
}

main "$@"

#!/usr/bin/env bash
# env-down.sh — tear down the parallax kind dev environment.
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-parallax-dev}"
KUBECONFIG_OUT="${KUBECONFIG_OUT:-hack/kind-kubeconfig}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }

command -v kind >/dev/null 2>&1 || { echo "error: 'kind' not found on PATH" >&2; exit 1; }

if kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
  log "deleting kind cluster '${CLUSTER_NAME}'"
  kind delete cluster --name "${CLUSTER_NAME}"
else
  log "kind cluster '${CLUSTER_NAME}' not found — nothing to delete"
fi

rm -f "${REPO_ROOT}/${KUBECONFIG_OUT}"
log "done"

#!/usr/bin/env bash
set -euo pipefail

# E2E tests for flux-drift-webhook using kind
# Deploys the webhook to a kind cluster and runs admission test assertions

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-flux-drift-webhook-e2e}"
WEBHOOK_IMAGE="${WEBHOOK_IMAGE:-flux-drift-webhook:e2e}"
TIMEOUT="${TIMEOUT:-120s}"
# cert-manager manifest must be available locally (works offline, no cluster internet needed)
CERT_MANAGER_MANIFEST="${CERT_MANAGER_MANIFEST:-${SCRIPT_DIR}/cert-manager.yaml}"

log() {
    echo "[$(date +'%Y-%m-%d %H:%M:%S')] $*"
}

cleanup() {
    log "Cleaning up..."
    kind delete cluster --name "${KIND_CLUSTER_NAME}" 2>/dev/null || true
}

trap cleanup EXIT

# Create kind cluster
log "Creating kind cluster..."
kind create cluster --name "${KIND_CLUSTER_NAME}" --wait "${TIMEOUT}"

# Build and load image
log "Building webhook image..."
docker build -t "${WEBHOOK_IMAGE}" "${ROOT_DIR}"
kind load docker-image "${WEBHOOK_IMAGE}" --name "${KIND_CLUSTER_NAME}"

# Install cert-manager (from a local manifest, so no cluster internet access is needed)
log "Installing cert-manager..."
if [[ ! -f "${CERT_MANAGER_MANIFEST}" ]]; then
    log "ERROR: cert-manager manifest not found at ${CERT_MANAGER_MANIFEST}"
    log "Download it first: curl -Lo e2e/cert-manager.yaml https://github.com/cert-manager/cert-manager/releases/download/v1.14.0/cert-manager.yaml"
    exit 1
fi
kubectl apply -f "${CERT_MANAGER_MANIFEST}"
kubectl wait --for=condition=Available deployment/cert-manager-webhook -n cert-manager --timeout="${TIMEOUT}"

# Create flux-system namespace
log "Creating flux-system namespace..."
kubectl create namespace flux-system || true

# Deploy webhook
log "Deploying webhook..."
cd "${ROOT_DIR}"
kustomize build deploy/overlays/dev | sed "s|flux-drift-webhook:latest|${WEBHOOK_IMAGE}|g" | kubectl apply -f -

# Wait for webhook to be ready
log "Waiting for webhook to be ready..."
kubectl wait --for=condition=Available deployment/flux-drift-webhook -n flux-system --timeout="${TIMEOUT}"

# Run admission test assertions against the deployed webhook
log "Running webhook integration tests..."
bash "${SCRIPT_DIR}/test-webhook.sh"

log "All E2E tests PASSED!"

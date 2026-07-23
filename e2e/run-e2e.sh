#!/usr/bin/env bash
set -euo pipefail

# E2E tests for flux-drift-webhook using kind
# Deploys the webhook to a kind cluster and runs admission test assertions

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-flux-drift-webhook-e2e}"
WEBHOOK_IMAGE="${WEBHOOK_IMAGE:-flux-drift-webhook:e2e}"
TIMEOUT="${TIMEOUT:-120s}"
# Third-party manifests, vendored under e2e/ and committed so this script runs
# offline straight after a clone. Bump them with `make e2e-vendor`.
CERT_MANAGER_MANIFEST="${CERT_MANAGER_MANIFEST:-${SCRIPT_DIR}/cert-manager.yaml}"
# deploy/base ships a PodMonitor, so the Prometheus Operator CRD must exist or
# the whole overlay fails to apply.
PODMONITOR_CRD="${PODMONITOR_CRD:-${SCRIPT_DIR}/podmonitor-crd.yaml}"
# Real Flux, so the owner-inventory paths run against genuine Kustomization and
# HelmRelease CRDs instead of a cluster where they do not exist — without it the
# CREATE checks can only ever fail closed on an unreadable owner.
FLUX_INSTALL="${FLUX_INSTALL:-${SCRIPT_DIR}/flux-install.yaml}"

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
    log "It is committed under e2e/; restore it with 'make e2e-vendor'"
    exit 1
fi
kubectl apply -f "${CERT_MANAGER_MANIFEST}"
kubectl wait --for=condition=Available deployment/cert-manager-webhook -n cert-manager --timeout="${TIMEOUT}"

# Install the PodMonitor CRD (deploy/base ships a PodMonitor; without the CRD the
# overlay apply fails with "no matches for kind PodMonitor").
log "Installing the PodMonitor CRD..."
if [[ ! -f "${PODMONITOR_CRD}" ]]; then
    log "ERROR: PodMonitor CRD not found at ${PODMONITOR_CRD}"
    log "It is committed under e2e/; restore it with 'make e2e-vendor'"
    exit 1
fi
# --server-side: the CRD exceeds the annotation size limit of client-side apply.
kubectl apply --server-side -f "${PODMONITOR_CRD}"
kubectl wait --for=condition=Established crd/podmonitors.monitoring.coreos.com --timeout="${TIMEOUT}"

# Install Flux.
log "Installing Flux..."
if [[ ! -f "${FLUX_INSTALL}" ]]; then
    log "ERROR: Flux install manifest not found at ${FLUX_INSTALL}"
    log "It is committed under e2e/; restore it with 'make e2e-vendor'"
    exit 1
fi
kubectl apply --server-side -f "${FLUX_INSTALL}"
for crd in kustomizations.kustomize.toolkit.fluxcd.io helmreleases.helm.toolkit.fluxcd.io; do
    kubectl wait --for=condition=Established "crd/${crd}" --timeout="${TIMEOUT}"
done
kubectl wait --for=condition=Available deployment/kustomize-controller -n flux-system --timeout="${TIMEOUT}"

# Create flux-system namespace
log "Ensuring the flux-system namespace exists..."
kubectl create namespace flux-system 2>/dev/null || true

# Deploy webhook
log "Deploying webhook..."
cd "${ROOT_DIR}"
# Rewrite the image line whole: the base manifest carries a registry prefix
# (ghcr.io/pmialon/...), so substituting only the tag would leave the pod
# pulling from the registry instead of using the image kind just loaded.
kustomize build deploy/overlays/dev \
    | sed -E "s|^([[:space:]]*image: ).*flux-drift-webhook:.*|\1${WEBHOOK_IMAGE}|" \
    | kubectl apply -f -

# Wait for webhook to be ready
log "Waiting for webhook to be ready..."
kubectl wait --for=condition=Available deployment/flux-drift-webhook -n flux-system --timeout="${TIMEOUT}"

# Run admission test assertions against the deployed webhook (audit-only)
log "Running webhook integration tests (audit mode)..."
bash "${SCRIPT_DIR}/test-webhook.sh"

# Switch to enforce mode and check that the requests that should be refused
# actually are. Audit mode never rejects anything, so this is the only phase
# that proves the webhook blocks rather than merely warns.
log "Switching the webhook to enforce mode..."
kustomize build deploy/overlays/prod \
    | sed -E "s|^([[:space:]]*image: ).*flux-drift-webhook:.*|\1${WEBHOOK_IMAGE}|" \
    | kubectl apply -f -
kubectl rollout status deployment/flux-drift-webhook -n flux-system --timeout="${TIMEOUT}"

log "Running enforce-mode tests..."
bash "${SCRIPT_DIR}/test-enforce.sh"

log "All E2E tests PASSED!"

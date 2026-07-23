#!/usr/bin/env bash
# Enforce-mode tests for flux-drift-webhook.
#
# test-webhook.sh runs the webhook in audit-only mode, where every decision is
# reported as a warning and nothing is ever refused. This script is the other
# half: it runs against a webhook with --audit-only=false and asserts that the
# API server actually REJECTS the requests that should be blocked.
#
# It also needs real Flux CRDs. Several decision paths are driven by the owning
# Kustomization's .status.inventory, and on a cluster without Flux they can only
# ever fail closed on an unreadable owner — which proves nothing about the
# inventory logic itself.
#
# Fixtures use suspended Kustomizations with a hand-written .status.inventory.
# The webhook only ever reads that field, so how it got there is irrelevant, and
# suspending keeps kustomize-controller from reconciling it away. That avoids
# needing a Git or OCI source, keeping the suite offline.
#
# Prerequisites:
#   - kubectl configured with access to the target cluster
#   - Webhook deployed in flux-system with --audit-only=false
#   - Flux CRDs installed (kustomizations.kustomize.toolkit.fluxcd.io)
#
# Usage: bash e2e/test-enforce.sh
set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEST_NS="drift-enforce-test"
WEBHOOK_NS="flux-system"
OWNER_NAME="e2e-app"
TMPDIR_TEST=$(mktemp -d)

PASS=0
FAIL=0
TOTAL=0

log()  { echo "[$(date +'%H:%M:%S')] $*"; }
pass() { ((PASS++)) || true; ((TOTAL++)) || true; log "  PASS: $1"; }
fail() { ((FAIL++)) || true; ((TOTAL++)) || true; log "  FAIL: $1"; }

# run_kubectl runs kubectl and captures stdout/stderr/rc without tripping set -e.
run_kubectl() {
    local tmpout="${TMPDIR_TEST}/stdout" tmperr="${TMPDIR_TEST}/stderr"
    set +e
    kubectl "$@" >"${tmpout}" 2>"${tmperr}"
    KUBECTL_RC=$?
    set -e
    KUBECTL_STDERR=$(cat "${tmperr}")
}

# assert_denied requires the request to have been refused, and the refusal to
# name the expected decision. Checking the reason matters: a request rejected
# for an unrelated cause (RBAC, validation) would otherwise look like a pass.
assert_denied() {
    local name="$1" expected="$2"
    if [[ "${KUBECTL_RC}" -eq 0 ]]; then
        fail "${name} — request was ALLOWED, expected a denial"
        return
    fi
    if ! echo "${KUBECTL_STDERR}" | grep -qF "denied the request"; then
        fail "${name} — failed, but not by the admission webhook"
        log "    stderr: ${KUBECTL_STDERR}"
        return
    fi
    if [[ -n "${expected}" ]] && ! echo "${KUBECTL_STDERR}" | grep -qF "${expected}"; then
        fail "${name} — denied, but the message does not mention '${expected}'"
        log "    stderr: ${KUBECTL_STDERR}"
        return
    fi
    pass "${name}"
}

assert_allowed() {
    local name="$1"
    if [[ "${KUBECTL_RC}" -ne 0 ]]; then
        fail "${name} — request was REJECTED, expected it to be allowed"
        log "    stderr: ${KUBECTL_STDERR}"
        return
    fi
    pass "${name}"
}

manifest() {
    # Separate statements: `local a=$1 b=${a}` expands every argument before the
    # builtin runs, so ${a} would still be unbound (fatal under set -u).
    local name="$1"
    local file="${TMPDIR_TEST}/${name}.yaml"
    cat > "${file}"
    echo "${file}"
}

cleanup() {
    kubectl delete namespace "${TEST_NS}" --wait=false >/dev/null 2>&1 || true
    kubectl delete kustomization "${OWNER_NAME}" -n "${WEBHOOK_NS}" --wait=false >/dev/null 2>&1 || true
    rm -rf "${TMPDIR_TEST}"
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Setup
# ---------------------------------------------------------------------------
log "=========================================="
log "flux-drift-webhook enforce-mode tests"
log "=========================================="

if ! kubectl get crd kustomizations.kustomize.toolkit.fluxcd.io >/dev/null 2>&1; then
    log "ERROR: Flux CRDs not found — install Flux first (e2e/run-e2e.sh does this)"
    exit 1
fi

AUDIT_ARG=$(kubectl get deploy flux-drift-webhook -n "${WEBHOOK_NS}" \
    -o jsonpath='{.spec.template.spec.containers[0].args}' 2>/dev/null | grep -o "audit-only=true" || true)
if [[ -n "${AUDIT_ARG}" ]]; then
    log "ERROR: the webhook is running with --audit-only=true; these tests need enforce mode"
    exit 1
fi

# The Deployment spec saying "enforce" is not enough. `kubectl rollout status`
# returns once the new ReplicaSet is available, but pods from the old one can
# still be terminating and still be in the Service endpoints — and an audit-mode
# pod allows everything, so a request served by one looks exactly like a webhook
# that fails to block. Wait until every live pod runs the enforce config.
log "Waiting for every webhook pod to serve the enforce config..."
ENFORCING="no"
for _ in $(seq 1 60); do
    PODS=$(kubectl get pods -n "${WEBHOOK_NS}" -l app.kubernetes.io/name=flux-drift-webhook \
        -o jsonpath='{range .items[*]}{.spec.containers[0].args}{"|"}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"\n"}{end}' 2>/dev/null || true)
    TOTAL_PODS=$(echo "${PODS}" | grep -c . || true)
    STALE=$(echo "${PODS}" | grep -c "audit-only=true" || true)
    NOT_READY=$(echo "${PODS}" | grep -cv "|True$" || true)
    if [[ "${TOTAL_PODS}" -gt 0 && "${STALE}" -eq 0 && "${NOT_READY}" -eq 0 ]]; then
        ENFORCING="yes"
        break
    fi
    sleep 2
done
if [[ "${ENFORCING}" != "yes" ]]; then
    log "ERROR: webhook pods did not converge on the enforce config"
    kubectl get pods -n "${WEBHOOK_NS}" -l app.kubernetes.io/name=flux-drift-webhook
    exit 1
fi
# Endpoint propagation to the API server's webhook client lags pod readiness.
sleep 5

kubectl delete namespace "${TEST_NS}" --wait=true >/dev/null 2>&1 || true
kubectl create namespace "${TEST_NS}" >/dev/null

FLUX_LABELS='
    kustomize.toolkit.fluxcd.io/name: '"${OWNER_NAME}"'
    kustomize.toolkit.fluxcd.io/namespace: '"${WEBHOOK_NS}"

# The owning Kustomization. Suspended so kustomize-controller leaves the status
# we write alone; it has no source, so it could not reconcile anyway.
MANIFEST=$(manifest owner <<YAML
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: ${OWNER_NAME}
  namespace: ${WEBHOOK_NS}
spec:
  interval: 1h
  suspend: true
  path: ./
  prune: true
  sourceRef:
    kind: GitRepository
    name: does-not-exist
YAML
)
kubectl apply -f "${MANIFEST}" >/dev/null

# .status.inventory declares one ConfigMap as Flux-managed. The id format is
# <namespace>_<name>_<group>_<kind>; the core group is empty, hence "__".
kubectl patch kustomization "${OWNER_NAME}" -n "${WEBHOOK_NS}" \
    --subresource=status --type=merge \
    -p "{\"status\":{\"inventory\":{\"entries\":[{\"id\":\"${TEST_NS}_managed-cm__ConfigMap\",\"v\":\"v1\"}]}}}" >/dev/null

log "Owner Kustomization ${WEBHOOK_NS}/${OWNER_NAME} ready, inventory declares ${TEST_NS}/managed-cm"

# The protected object, applied exactly as Flux applies it: server-side, with the
# kustomize-controller field manager, AS the kustomize-controller service
# account. The identity matters as much as the field manager — its id is in the
# inventory, so any other actor creating it is refused (that is E3).
FLUX_SA="--as=system:serviceaccount:flux-system:kustomize-controller"
MANIFEST=$(manifest managed <<YAML
apiVersion: v1
kind: ConfigMap
metadata:
  name: managed-cm
  namespace: ${TEST_NS}
  labels:${FLUX_LABELS}
data:
  managed: "from-git"
YAML
)
if ! kubectl apply --server-side --field-manager=kustomize-controller ${FLUX_SA} -f "${MANIFEST}" >/dev/null 2>"${TMPDIR_TEST}/setup.err"; then
    log "ERROR: could not create the Flux-applied fixture"
    log "    $(cat "${TMPDIR_TEST}/setup.err")"
    exit 1
fi
log "Flux-applied ConfigMap ${TEST_NS}/managed-cm created (as kustomize-controller)"

# ---------------------------------------------------------------------------
# E1: UPDATE of a Flux-managed field is refused
# ---------------------------------------------------------------------------
log ""
log "--- E1: UPDATE a Flux-managed field (denied_update_flux_managed_fields) ---"
MANIFEST=$(manifest e1 <<YAML
apiVersion: v1
kind: ConfigMap
metadata:
  name: managed-cm
  namespace: ${TEST_NS}
  labels:${FLUX_LABELS}
data:
  managed: "hand-edited"
YAML
)
run_kubectl apply -f "${MANIFEST}"
assert_denied "E1: manual edit of a Flux-owned field is blocked" "Flux-managed fields"

# ---------------------------------------------------------------------------
# E2: DELETE of a Flux-applied resource is refused
# ---------------------------------------------------------------------------
log ""
log "--- E2: DELETE a Flux-applied resource (denied_delete_flux_managed) ---"
run_kubectl delete configmap managed-cm -n "${TEST_NS}"
assert_denied "E2: manual delete of a Flux-applied resource is blocked" "cannot delete Flux-managed resource"

# ---------------------------------------------------------------------------
# E3: CREATE squatting on an id declared in the inventory is refused
#
# Only reachable with a readable owner: without Flux the owner lookup fails and
# the request is refused as denied_create_inventory_unavailable instead, which
# says nothing about the inventory veto.
# ---------------------------------------------------------------------------
log ""
log "--- E3: CREATE an id declared in the owner inventory (denied_create_flux_labels) ---"
kubectl delete configmap managed-cm -n "${TEST_NS}" --ignore-not-found ${FLUX_SA} >/dev/null 2>&1 || true
MANIFEST=$(manifest e3 <<YAML
apiVersion: v1
kind: ConfigMap
metadata:
  name: managed-cm
  namespace: ${TEST_NS}
  labels:${FLUX_LABELS}
data:
  squatted: "yes"
YAML
)
run_kubectl apply -f "${MANIFEST}"
assert_denied "E3: squatting an inventory-declared id is blocked" "Flux inventory"

# ---------------------------------------------------------------------------
# E4: CREATE of an id absent from the inventory is allowed
#
# The derived-object case: an operator generates a child that inherits its
# parent's Flux labels. Before Flux was installed this could only fail closed.
# ---------------------------------------------------------------------------
log ""
log "--- E4: CREATE an id absent from the inventory (allowed_not_in_owner_inventory) ---"
MANIFEST=$(manifest e4 <<YAML
apiVersion: v1
kind: ConfigMap
metadata:
  name: derived-cm
  namespace: ${TEST_NS}
  labels:${FLUX_LABELS}
data:
  derived: "yes"
YAML
)
run_kubectl apply -f "${MANIFEST}"
assert_allowed "E4: an object outside the inventory is allowed (derived resource)"

# ---------------------------------------------------------------------------
# E5: the owning Flux controller is not blocked
# ---------------------------------------------------------------------------
log ""
log "--- E5: the owning Flux controller may still reconcile (allowed_owning_flux_controller) ---"
MANIFEST=$(manifest e5 <<YAML
apiVersion: v1
kind: ConfigMap
metadata:
  name: managed-cm
  namespace: ${TEST_NS}
  labels:${FLUX_LABELS}
data:
  managed: "reconciled-by-flux"
YAML
)
run_kubectl apply --server-side --force-conflicts --field-manager=kustomize-controller \
    ${FLUX_SA} -f "${MANIFEST}"
assert_allowed "E5: kustomize-controller reconciles its own resource"

# ---------------------------------------------------------------------------
# E6: the bypass annotation, applied via Flux, releases the object
# ---------------------------------------------------------------------------
log ""
log "--- E6: bypass annotation applied by Flux (allowed_bypass_annotation) ---"
MANIFEST=$(manifest e6-setup <<YAML
apiVersion: v1
kind: ConfigMap
metadata:
  name: managed-cm
  namespace: ${TEST_NS}
  labels:${FLUX_LABELS}
  annotations:
    fluxcd.io/drift-prevention-bypass: disabled
data:
  managed: "reconciled-by-flux"
YAML
)
kubectl apply --server-side --force-conflicts --field-manager=kustomize-controller \
    ${FLUX_SA} -f "${MANIFEST}" >/dev/null

MANIFEST=$(manifest e6 <<YAML
apiVersion: v1
kind: ConfigMap
metadata:
  name: managed-cm
  namespace: ${TEST_NS}
  labels:${FLUX_LABELS}
  annotations:
    fluxcd.io/drift-prevention-bypass: disabled
data:
  managed: "hand-edited-with-bypass"
YAML
)
run_kubectl apply -f "${MANIFEST}"
assert_allowed "E6: a bypass annotation applied via Flux lets a manual edit through"

# ---------------------------------------------------------------------------
# E7: a non-Flux actor cannot introduce the bypass annotation itself
# ---------------------------------------------------------------------------
log ""
log "--- E7: introducing the bypass annotation by hand (denied_bypass_annotation_added) ---"
MANIFEST=$(manifest e7-setup <<YAML
apiVersion: v1
kind: ConfigMap
metadata:
  name: guarded-cm
  namespace: ${TEST_NS}
  labels:${FLUX_LABELS}
data:
  managed: "from-git"
YAML
)
kubectl apply --server-side --field-manager=kustomize-controller \
    ${FLUX_SA} -f "${MANIFEST}" >/dev/null

MANIFEST=$(manifest e7 <<YAML
apiVersion: v1
kind: ConfigMap
metadata:
  name: guarded-cm
  namespace: ${TEST_NS}
  labels:${FLUX_LABELS}
  annotations:
    fluxcd.io/drift-prevention-bypass: disabled
data:
  managed: "from-git"
YAML
)
run_kubectl apply -f "${MANIFEST}"
assert_denied "E7: adding the bypass annotation by hand is blocked" "bypass"

# ---------------------------------------------------------------------------
# E8: fields the owner excludes via .spec.ignore are waived
# ---------------------------------------------------------------------------
log ""
log "--- E8: .spec.ignore waiver (allowed_drift_ignored_field) ---"
kubectl patch kustomization "${OWNER_NAME}" -n "${WEBHOOK_NS}" --type=merge \
    -p '{"spec":{"ignore":[{"paths":["/data"]}]}}' >/dev/null
# The controller-runtime cache needs a moment to see the updated owner.
sleep 3
MANIFEST=$(manifest e8 <<YAML
apiVersion: v1
kind: ConfigMap
metadata:
  name: guarded-cm
  namespace: ${TEST_NS}
  labels:${FLUX_LABELS}
data:
  managed: "edited-but-ignored"
YAML
)
run_kubectl apply -f "${MANIFEST}"
assert_allowed "E8: an edit to a path the owner ignores is allowed"

# ---------------------------------------------------------------------------
# podinfo: a real workload rather than a ConfigMap
#
# podinfo ships a Deployment with no .spec.replicas plus an HPA, which is
# exactly the shape the README prescribes for the field-level case: Flux owns
# .spec.template, an autoscaler owns .spec.replicas. Applied as
# kustomize-controller via SSA, which is how Flux itself applies.
# ---------------------------------------------------------------------------
log ""
log "--- Deploying podinfo as Flux ---"
if ! kustomize build "${SCRIPT_DIR}/podinfo-flux" \
    | kubectl apply --server-side --field-manager=kustomize-controller ${FLUX_SA} -f - \
      >/dev/null 2>"${TMPDIR_TEST}/podinfo.err"; then
    log "ERROR: could not deploy podinfo as kustomize-controller"
    log "    $(cat "${TMPDIR_TEST}/podinfo.err")"
    exit 1
fi
log "podinfo Deployment/Service/HPA applied (fieldManager kustomize-controller)"

# ---------------------------------------------------------------------------
# E9: the canonical drift — changing the image of a Flux-managed Deployment
# ---------------------------------------------------------------------------
log ""
log "--- E9: kubectl set image on a Flux-managed Deployment (denied_update_flux_managed_fields) ---"
run_kubectl set image deployment/podinfo -n "${TEST_NS}" podinfod=ghcr.io/stefanprodan/podinfo:6.0.0
assert_denied "E9: changing a Flux-managed container image is blocked" "Flux-managed fields"

# ---------------------------------------------------------------------------
# E10: the headline field-level case. Flux never declared .spec.replicas, so it
# does not own it and an autoscaler may write it. This is the README's central
# promise and nothing exercised it end to end before.
# ---------------------------------------------------------------------------
log ""
log "--- E10: writing .spec.replicas, which Flux does not own (allowed_no_field_conflict) ---"
run_kubectl patch deployment podinfo -n "${TEST_NS}" --type=merge -p '{"spec":{"replicas":3}}'
assert_allowed "E10: an autoscaler-style replicas update is allowed"

# ---------------------------------------------------------------------------
# E11: the same change through the scale subresource, which the HPA actually
# uses. Subresources carry no parent object, so they take the earlier
# allowed_subresource path rather than the field check.
# ---------------------------------------------------------------------------
log ""
log "--- E11: scale subresource, the path a real HPA uses (allowed_subresource) ---"
run_kubectl scale deployment/podinfo -n "${TEST_NS}" --replicas=2
assert_allowed "E11: scaling via the scale subresource is allowed"

# ---------------------------------------------------------------------------
# E12/E13: ownership is per key, not per map. Both patches write into
# .spec.template.metadata.annotations, and only one is refused — the difference
# is whether Flux declared that particular key. Getting this wrong in either
# direction is a real failure mode: too coarse and every sidecar annotation is
# blocked, too fine and drift on a Flux-declared value slips through.
# ---------------------------------------------------------------------------
log ""
log "--- E12: adding an annotation key Flux never declared (allowed_no_field_conflict) ---"
run_kubectl patch deployment podinfo -n "${TEST_NS}" --type=merge \
    -p '{"spec":{"template":{"metadata":{"annotations":{"hand-edited":"true"}}}}}'
assert_allowed "E12: a new annotation key Flux does not own is allowed"

log ""
log "--- E13: changing an annotation key Flux DID declare (denied_update_flux_managed_fields) ---"
# podinfo declares prometheus.io/port: "9797" in .spec.template.metadata.annotations.
run_kubectl patch deployment podinfo -n "${TEST_NS}" --type=merge \
    -p '{"spec":{"template":{"metadata":{"annotations":{"prometheus.io/port":"1234"}}}}}'
assert_denied "E13: changing a Flux-declared annotation is blocked" "Flux-managed fields"

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
log ""
log "=========================================="
log "Enforce Results: ${PASS} passed, ${FAIL} failed, ${TOTAL} total"
log "=========================================="

if [[ "${FAIL}" -gt 0 ]]; then
    log "SOME ENFORCE TESTS FAILED"
    exit 1
fi

log "ALL ENFORCE TESTS PASSED"
exit 0

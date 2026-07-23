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

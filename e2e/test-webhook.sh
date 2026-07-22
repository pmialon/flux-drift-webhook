#!/usr/bin/env bash
# Integration test script for flux-drift-webhook running in audit-only mode.
# Exercises every decision path by creating test resources and checking for
# audit warnings (kubectl stderr) and webhook logs.
#
# Prerequisites:
#   - kubectl configured with access to the target cluster
#   - Webhook deployed in flux-system namespace with --audit-only=true
#   - VWC namespaceSelector excludes kube-system, kube-public, kube-node-lease, flux-system
#
# Usage: bash e2e/test-webhook.sh
set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
TEST_NS="drift-webhook-test"
WEBHOOK_DEPLOY="flux-drift-webhook"
WEBHOOK_NS="flux-system"
WEBHOOK_CONTAINER="webhook"
AUDIT_MARKER="AUDIT MODE: This operation would be BLOCKED"
TMPDIR_TEST=$(mktemp -d)

# Counters
PASS=0
FAIL=0
TOTAL=0

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
log()  { echo "[$(date +'%H:%M:%S')] $*"; }
pass() { ((PASS++)) || true; ((TOTAL++)) || true; log "  PASS: $1"; }
fail() { ((FAIL++)) || true; ((TOTAL++)) || true; log "  FAIL: $1"; }

# run_kubectl_file applies a manifest file and captures stdout+stderr.
# Sets KUBECTL_STDOUT, KUBECTL_STDERR, KUBECTL_RC.
run_kubectl_file() {
    local manifest="$1"
    local tmpout="${TMPDIR_TEST}/stdout"
    local tmperr="${TMPDIR_TEST}/stderr"
    set +e
    kubectl apply -f "${manifest}" >"${tmpout}" 2>"${tmperr}"
    KUBECTL_RC=$?
    set -e
    KUBECTL_STDOUT=$(cat "${tmpout}")
    KUBECTL_STDERR=$(cat "${tmperr}")
}

# run_kubectl_cmd executes a kubectl command and captures stdout+stderr.
run_kubectl_cmd() {
    local tmpout="${TMPDIR_TEST}/stdout"
    local tmperr="${TMPDIR_TEST}/stderr"
    set +e
    kubectl "$@" >"${tmpout}" 2>"${tmperr}"
    KUBECTL_RC=$?
    set -e
    KUBECTL_STDOUT=$(cat "${tmpout}")
    KUBECTL_STDERR=$(cat "${tmperr}")
}

# assert_no_audit checks that KUBECTL_STDERR does NOT contain the audit marker.
assert_no_audit() {
    local test_name="$1"
    if echo "${KUBECTL_STDERR}" | grep -qF "${AUDIT_MARKER}"; then
        fail "${test_name} — unexpected audit warning in stderr"
        log "    stderr: ${KUBECTL_STDERR}"
    else
        pass "${test_name}"
    fi
}

# assert_has_audit checks that KUBECTL_STDERR DOES contain the audit marker.
assert_has_audit() {
    local test_name="$1"
    if echo "${KUBECTL_STDERR}" | grep -qF "${AUDIT_MARKER}"; then
        pass "${test_name}"
    else
        fail "${test_name} — expected audit warning not found in stderr"
        log "    stderr: ${KUBECTL_STDERR}"
    fi
}

# assert_success checks that the kubectl command succeeded.
assert_success() {
    local test_name="$1"
    if [[ "${KUBECTL_RC}" -ne 0 ]]; then
        fail "${test_name} — kubectl exited with ${KUBECTL_RC}"
        log "    stderr: ${KUBECTL_STDERR}"
        return 1
    fi
    return 0
}

# check_webhook_log searches webhook logs (since START_TIME) for a decision reason.
check_webhook_log() {
    local decision="$1"
    local resource_name="$2"
    local log_output
    log_output=$(kubectl logs "deploy/${WEBHOOK_DEPLOY}" \
        -n "${WEBHOOK_NS}" \
        -c "${WEBHOOK_CONTAINER}" \
        --since-time="${START_TIME}" 2>/dev/null || true)
    if echo "${log_output}" | grep -q "\"decision\":\"${decision}\"" ||
       echo "${log_output}" | grep -q "\"reason\":\"${decision}\""; then
        log "    log check: found '${decision}' in webhook logs"
        return 0
    fi
    log "    log check: '${decision}' not found for '${resource_name}' (may appear later)"
    return 0  # Non-fatal — logs may be buffered or at debug level
}

# write_manifest writes a YAML manifest to a temp file and returns its path.
write_manifest() {
    local name="$1"
    local file="${TMPDIR_TEST}/${name}.yaml"
    cat > "${file}"
    echo "${file}"
}

cleanup() {
    kubectl delete namespace "${TEST_NS}" --wait=false >/dev/null 2>&1 || true
    rm -rf "${TMPDIR_TEST}"
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Setup
# ---------------------------------------------------------------------------
log "=========================================="
log "flux-drift-webhook integration tests"
log "=========================================="

# Record start time for log filtering (RFC3339 for --since-time)
START_TIME=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

# Verify webhook is running
log "Checking webhook deployment..."
if ! kubectl get deploy "${WEBHOOK_DEPLOY}" -n "${WEBHOOK_NS}" >/dev/null 2>&1; then
    log "ERROR: Webhook deployment not found in ${WEBHOOK_NS}"
    exit 1
fi
READY=$(kubectl get deploy "${WEBHOOK_DEPLOY}" -n "${WEBHOOK_NS}" -o jsonpath='{.status.readyReplicas}')
log "Webhook replicas ready: ${READY:-0}"

# Create test namespace (wait for any previous Terminating state to clear)
log "Creating test namespace ${TEST_NS}..."
for attempt in $(seq 1 10); do
    if kubectl create namespace "${TEST_NS}" >/dev/null 2>&1; then
        break
    fi
    state=$(kubectl get namespace "${TEST_NS}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "none")
    if [[ "${state}" == "Active" ]]; then
        break
    fi
    log "  Namespace in '${state}' state, retrying in 3s... (${attempt}/10)"
    sleep 3
done
log "Waiting for namespace to be ready..."
kubectl wait --for=jsonpath='{.status.phase}'=Active "namespace/${TEST_NS}" --timeout=30s >/dev/null 2>&1 || true
sleep 1

# ---------------------------------------------------------------------------
# T1: Non-Flux resource CREATE — allowed_not_managed
# ---------------------------------------------------------------------------
log ""
log "--- T1: Non-Flux resource CREATE (allowed_not_managed) ---"
MANIFEST=$(write_manifest t1 <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: test-no-flux
  namespace: drift-webhook-test
data:
  key: value
YAML
)
run_kubectl_file "${MANIFEST}"
if assert_success "T1-create"; then
    assert_no_audit "T1: CREATE non-Flux ConfigMap — no audit warning"
    check_webhook_log "allowed_not_managed" "test-no-flux"
fi

# ---------------------------------------------------------------------------
# T2: Non-Flux resource UPDATE — allowed_not_managed
# ---------------------------------------------------------------------------
log ""
log "--- T2: Non-Flux resource UPDATE (allowed_not_managed) ---"
MANIFEST=$(write_manifest t2 <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: test-no-flux
  namespace: drift-webhook-test
data:
  key: updated-value
YAML
)
run_kubectl_file "${MANIFEST}"
if assert_success "T2-update"; then
    assert_no_audit "T2: UPDATE non-Flux ConfigMap — no audit warning"
    check_webhook_log "allowed_not_managed" "test-no-flux"
fi

# ---------------------------------------------------------------------------
# T3: Non-Flux resource DELETE — allowed_not_managed
# ---------------------------------------------------------------------------
log ""
log "--- T3: Non-Flux resource DELETE (allowed_not_managed) ---"
run_kubectl_cmd delete configmap test-no-flux -n "${TEST_NS}"
if assert_success "T3-delete"; then
    assert_no_audit "T3: DELETE non-Flux ConfigMap — no audit warning"
    check_webhook_log "allowed_not_managed" "test-no-flux"
fi

# ---------------------------------------------------------------------------
# T4: CREATE with Flux labels — denied (audit warning). The "my-app" owner
# Kustomization does not exist on the e2e cluster, so the inventory is
# unreadable and the deny carries denied_create_inventory_unavailable.
# ---------------------------------------------------------------------------
log ""
log "--- T4: CREATE with Flux labels (denied_create_inventory_unavailable) ---"
MANIFEST=$(write_manifest t4 <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: test-flux-labelled
  namespace: drift-webhook-test
  labels:
    kustomize.toolkit.fluxcd.io/name: my-app
    kustomize.toolkit.fluxcd.io/namespace: flux-system
data:
  key: value
YAML
)
run_kubectl_file "${MANIFEST}"
if assert_success "T4-create-succeeds-in-audit-mode"; then
    assert_has_audit "T4: CREATE with Flux labels — audit warning present"
    check_webhook_log "denied_create_inventory_unavailable" "test-flux-labelled"
fi

# ---------------------------------------------------------------------------
# T5: UPDATE Flux-labelled resource — depends on SSA managedFields
# Without SSA managedFields (no Flux controller has applied), expect:
#   allowed_no_flux_managed_fields
# ---------------------------------------------------------------------------
log ""
log "--- T5: UPDATE Flux-labelled resource (allowed_no_flux_managed_fields) ---"
MANIFEST=$(write_manifest t5 <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: test-flux-labelled
  namespace: drift-webhook-test
  labels:
    kustomize.toolkit.fluxcd.io/name: my-app
    kustomize.toolkit.fluxcd.io/namespace: flux-system
data:
  key: modified-value
YAML
)
run_kubectl_file "${MANIFEST}"
if assert_success "T5-update"; then
    # No Flux controller has applied this resource via SSA, so no Flux-managed
    # fields exist in managedFields. The webhook should allow.
    assert_no_audit "T5: UPDATE Flux-labelled (no SSA fields) — no audit warning"
    check_webhook_log "allowed_no_flux_managed_fields" "test-flux-labelled"
fi

# ---------------------------------------------------------------------------
# T6: DELETE genuinely Flux-applied resource — denied_delete_flux_managed
# A resource is "Flux-managed" only when a Flux fieldManager is present in
# .metadata.managedFields (the non-inheritable proof of management). Apply it
# via server-side apply as the kustomize-controller field manager so the
# managedFields records a Flux Apply entry, then deletion is denied.
# ---------------------------------------------------------------------------
log ""
log "--- T6: DELETE Flux-applied resource (denied_delete_flux_managed) ---"
MANIFEST=$(write_manifest t6 <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: test-flux-ssa
  namespace: drift-webhook-test
  labels:
    kustomize.toolkit.fluxcd.io/name: my-app
    kustomize.toolkit.fluxcd.io/namespace: flux-system
data:
  key: value
YAML
)
run_kubectl_cmd apply --server-side --force-conflicts --field-manager=kustomize-controller -f "${MANIFEST}"
assert_success "T6-setup-flux-ssa-apply" || true
run_kubectl_cmd delete configmap test-flux-ssa -n "${TEST_NS}"
if assert_success "T6-delete-succeeds-in-audit-mode"; then
    assert_has_audit "T6: DELETE Flux-applied ConfigMap — audit warning present"
    check_webhook_log "denied_delete_flux_managed" "test-flux-ssa"
fi

# ---------------------------------------------------------------------------
# T6b: DELETE inherited-label resource — allowed_no_flux_managed_fields
# test-flux-labelled only carries Flux labels (applied client-side, no Flux
# fieldManager), like an Endpoints/EndpointSlice that inherited its parent's
# labels. Deletion must be allowed (no audit warning).
# ---------------------------------------------------------------------------
log ""
log "--- T6b: DELETE inherited-label resource (allowed_no_flux_managed_fields) ---"
run_kubectl_cmd delete configmap test-flux-labelled -n "${TEST_NS}"
if assert_success "T6b-delete"; then
    assert_no_audit "T6b: DELETE label-only resource — no audit warning (not Flux-applied)"
    check_webhook_log "allowed_no_flux_managed_fields" "test-flux-labelled"
fi

# ---------------------------------------------------------------------------
# T7: Bypass annotation on existing object — allowed_bypass_annotation
# First create the resource with both Flux labels AND bypass annotation,
# then update it. The webhook checks the OLD object for the bypass annotation.
# ---------------------------------------------------------------------------
log ""
log "--- T7: Bypass annotation on existing object (allowed_bypass_annotation) ---"
# Step 1: Create the resource with bypass annotation (will trigger audit for CREATE)
MANIFEST=$(write_manifest t7-create <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: test-bypass
  namespace: drift-webhook-test
  labels:
    kustomize.toolkit.fluxcd.io/name: my-app
    kustomize.toolkit.fluxcd.io/namespace: flux-system
  annotations:
    fluxcd.io/drift-prevention-bypass: disabled
data:
  key: original
YAML
)
run_kubectl_file "${MANIFEST}"
assert_success "T7-create-with-bypass" || true
sleep 1

# Step 2: Update the resource — old object now has bypass annotation
MANIFEST=$(write_manifest t7-update <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: test-bypass
  namespace: drift-webhook-test
  labels:
    kustomize.toolkit.fluxcd.io/name: my-app
    kustomize.toolkit.fluxcd.io/namespace: flux-system
  annotations:
    fluxcd.io/drift-prevention-bypass: disabled
data:
  key: modified
YAML
)
run_kubectl_file "${MANIFEST}"
if assert_success "T7-update-with-bypass"; then
    assert_no_audit "T7: UPDATE with bypass annotation — no audit warning"
    check_webhook_log "allowed_bypass_annotation" "test-bypass"
fi

# Cleanup T7 resource (will trigger audit for DELETE but that is expected)
kubectl delete configmap test-bypass -n "${TEST_NS}" >/dev/null 2>&1 || true

# ---------------------------------------------------------------------------
# T8: Excluded namespace (kube-system) — not processed by webhook
# The VWC namespaceSelector excludes kube-system, so the webhook never
# receives the request. No audit warning expected.
# ---------------------------------------------------------------------------
log ""
log "--- T8: Excluded namespace — kube-system (not intercepted) ---"
MANIFEST=$(write_manifest t8 <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: test-excluded-ns
  namespace: kube-system
  labels:
    kustomize.toolkit.fluxcd.io/name: my-app
    kustomize.toolkit.fluxcd.io/namespace: flux-system
data:
  key: value
YAML
)
run_kubectl_file "${MANIFEST}"
if assert_success "T8-create-in-kube-system"; then
    assert_no_audit "T8: CREATE in kube-system — no audit warning (VWC excluded)"
fi
# Cleanup
kubectl delete configmap test-excluded-ns -n kube-system >/dev/null 2>&1 || true

# ---------------------------------------------------------------------------
# T9: Update non-Flux fields (no SSA managedFields) — allowed_no_flux_managed_fields
# Create a Flux-labelled ConfigMap, then update only data (no SSA fields exist
# because the resource was created by kubectl, not by a Flux controller).
# ---------------------------------------------------------------------------
log ""
log "--- T9: UPDATE non-Flux fields, no SSA managedFields (allowed_no_flux_managed_fields) ---"
# Create first (will trigger CREATE audit)
MANIFEST=$(write_manifest t9-create <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: test-no-ssa-fields
  namespace: drift-webhook-test
  labels:
    kustomize.toolkit.fluxcd.io/name: another-app
    kustomize.toolkit.fluxcd.io/namespace: flux-system
data:
  config: initial
YAML
)
run_kubectl_file "${MANIFEST}"
assert_success "T9-create" || true
sleep 1

# Update only the data field
MANIFEST=$(write_manifest t9-update <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: test-no-ssa-fields
  namespace: drift-webhook-test
  labels:
    kustomize.toolkit.fluxcd.io/name: another-app
    kustomize.toolkit.fluxcd.io/namespace: flux-system
data:
  config: updated
YAML
)
run_kubectl_file "${MANIFEST}"
if assert_success "T9-update"; then
    assert_no_audit "T9: UPDATE with no SSA managedFields — no audit warning"
    check_webhook_log "allowed_no_flux_managed_fields" "test-no-ssa-fields"
fi

# Cleanup T9
kubectl delete configmap test-no-ssa-fields -n "${TEST_NS}" >/dev/null 2>&1 || true

# ---------------------------------------------------------------------------
# T10: Namespace teardown cascade delete — allowed_namespace_terminating
# Deleting a namespace makes the namespace-controller issue direct DELETEs on
# finalizer-free children, which never carry their own deletionTimestamp. The
# webhook must allow these (parent namespace terminating) instead of flagging
# denied_delete_flux_managed — under enforce mode a deny would wedge the
# namespace in Terminating. Exercises the list/watch RBAC needed by the
# cache-backed namespace lookup.
# ---------------------------------------------------------------------------
log ""
log "--- T10: namespace teardown cascade delete (allowed_namespace_terminating) ---"
TEARDOWN_NS="drift-webhook-test-teardown"
kubectl delete namespace "${TEARDOWN_NS}" --wait=true >/dev/null 2>&1 || true
run_kubectl_cmd create namespace "${TEARDOWN_NS}"
assert_success "T10-create-namespace" || true
MANIFEST=$(write_manifest t10 <<YAML
apiVersion: v1
kind: ConfigMap
metadata:
  name: test-teardown-child
  namespace: ${TEARDOWN_NS}
  labels:
    kustomize.toolkit.fluxcd.io/name: my-app
    kustomize.toolkit.fluxcd.io/namespace: flux-system
data:
  key: value
YAML
)
run_kubectl_cmd apply --server-side --force-conflicts --field-manager=kustomize-controller -f "${MANIFEST}"
assert_success "T10-setup-flux-ssa-apply" || true

# kubectl delete ns waits for finalization, so the cascade has completed on return.
run_kubectl_cmd delete namespace "${TEARDOWN_NS}" --timeout=90s
if assert_success "T10-namespace-delete"; then
    sleep 3  # let webhook logs flush
    # Fetch logs from ALL replicas — the cascade DELETE may hit any pod.
    TEARDOWN_LOGS=$(kubectl logs -n "${WEBHOOK_NS}" \
        -l app.kubernetes.io/name=flux-drift-webhook \
        -c "${WEBHOOK_CONTAINER}" --tail=-1 \
        --since-time="${START_TIME}" 2>/dev/null || true)
    if echo "${TEARDOWN_LOGS}" | grep "\"namespace\":\"${TEARDOWN_NS}\"" | grep -q "would deny"; then
        fail "T10: cascade DELETE flagged as drift during namespace teardown"
        log "    logs: $(echo "${TEARDOWN_LOGS}" | grep "\"namespace\":\"${TEARDOWN_NS}\"" | head -3)"
    else
        pass "T10: namespace teardown — no would-deny for cascade deletes"
    fi
    check_webhook_log "allowed_namespace_terminating" "test-teardown-child"
fi

# ---------------------------------------------------------------------------
# T11: DELETE a Flux-applied Namespace — denied_delete_flux_managed (audit)
# VWC rules carry Scope "*": cluster-scoped objects are selected too. A
# namespace applied by Flux (SSA fieldManager kustomize-controller) must not
# be manually deletable — the legitimate teardown path is removing it from
# Git so Flux prunes it as the owning reconciler.
# ---------------------------------------------------------------------------
log ""
log "--- T11: DELETE Flux-applied Namespace (denied_delete_flux_managed) ---"
CS_NS="drift-webhook-test-clusterscope"
kubectl delete namespace "${CS_NS}" --wait=true >/dev/null 2>&1 || true
MANIFEST=$(write_manifest t11 <<YAML
apiVersion: v1
kind: Namespace
metadata:
  name: ${CS_NS}
  labels:
    kustomize.toolkit.fluxcd.io/name: my-app
    kustomize.toolkit.fluxcd.io/namespace: flux-system
YAML
)
run_kubectl_cmd apply --server-side --force-conflicts --field-manager=kustomize-controller -f "${MANIFEST}"
assert_success "T11-setup-flux-ssa-namespace" || true
run_kubectl_cmd delete namespace "${CS_NS}" --timeout=90s
if assert_success "T11-delete-succeeds-in-audit-mode"; then
    assert_has_audit "T11: DELETE Flux-applied Namespace — audit warning present"
    check_webhook_log "denied_delete_flux_managed" "${CS_NS}"
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
log ""
log "=========================================="
log "Test Results: ${PASS} passed, ${FAIL} failed, ${TOTAL} total"
log "=========================================="

if [[ "${FAIL}" -gt 0 ]]; then
    log "SOME TESTS FAILED"
    exit 1
fi

log "ALL TESTS PASSED"
exit 0

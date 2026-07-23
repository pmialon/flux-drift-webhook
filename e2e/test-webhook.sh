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

# T12 (readiness gate) settings.
WEBHOOK_CLUSTERROLE="${WEBHOOK_CLUSTERROLE:-flux-drift-webhook}"
# Restoring the revoked verbs, rather than re-applying a whole-object backup:
# `kubectl apply` of a backup taken before the patch carries a stale
# resourceVersion and is rejected as a conflict.
NS_VERBS_BACKUP="${TMPDIR_TEST}/ns-verbs.json"
NS_RULE_INDEX="${TMPDIR_TEST}/ns-rule-index"
# Local port for the /readyz port-forward; override if 18081 is taken.
READYZ_LOCAL_PORT="${READYZ_LOCAL_PORT:-18081}"
# How long the replacement pod must stay not-Ready for the gate to be proven.
# Comfortably above a healthy startup (~10s) so the assertion is not a race.
READY_GATE_SECONDS="${READY_GATE_SECONDS:-45}"

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

# readyz_verbose fetches /readyz?verbose from a pod through a port-forward and
# sets READYZ_BODY / READYZ_CODE. A port-forward is used rather than an in-cluster
# curl pod because the e2e cluster is expected to work without internet access,
# and the webhook image is distroless (no shell to exec into).
readyz_verbose() {
    local pod="$1"
    local pf_log="${TMPDIR_TEST}/port-forward.log"
    READYZ_BODY=""
    READYZ_CODE="000"

    kubectl port-forward -n "${WEBHOOK_NS}" "pod/${pod}" \
        "${READYZ_LOCAL_PORT}:8081" >"${pf_log}" 2>&1 &
    local pf_pid=$!

    # Plain `cmd && break` would abort the script under `set -e` on every
    # iteration where the grep does not match yet, so keep the tests in `if`.
    local i
    for i in $(seq 1 50); do
        if grep -q "Forwarding from" "${pf_log}" 2>/dev/null; then
            break
        fi
        sleep 0.2
    done

    local raw
    set +e
    raw=$(curl -s -m 5 -w '\n%{http_code}' "http://127.0.0.1:${READYZ_LOCAL_PORT}/readyz?verbose")
    set -e
    kill "${pf_pid}" >/dev/null 2>&1 || true
    wait "${pf_pid}" 2>/dev/null || true

    if [[ -n "${raw}" ]]; then
        READYZ_CODE=$(echo "${raw}" | tail -n1)
        READYZ_BODY=$(echo "${raw}" | sed '$d')
    fi
}

# first_pod_with_ready prints the first webhook pod whose Ready condition equals
# the requested value ("True" or "False"), or nothing.
first_pod_with_ready() {
    local want="$1"
    kubectl get pods -n "${WEBHOOK_NS}" -l app.kubernetes.io/name=flux-drift-webhook \
        -o jsonpath='{range .items[*]}{.metadata.name}{" "}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"\n"}{end}' \
        2>/dev/null | awk -v w="${want}" '$2==w {print $1; exit}'
}

# restore_namespace_verbs puts the revoked verbs back on the ClusterRole.
restore_namespace_verbs() {
    kubectl patch clusterrole "${WEBHOOK_CLUSTERROLE}" --type=json \
        -p "[{\"op\":\"replace\",\"path\":\"/rules/$(cat "${NS_RULE_INDEX}")/verbs\",\"value\":$(cat "${NS_VERBS_BACKUP}")}]"
}

# webhook_pod_names prints the current webhook pod names, one per line.
webhook_pod_names() {
    kubectl get pods -n "${WEBHOOK_NS}" -l app.kubernetes.io/name=flux-drift-webhook \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null
}

# pod_ready prints True/False/"" for a pod's Ready condition.
pod_ready() {
    kubectl get pod -n "${WEBHOOK_NS}" "$1" \
        -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.status}{end}' 2>/dev/null
}

cleanup() {
    kubectl delete namespace "${TEST_NS}" --wait=false >/dev/null 2>&1 || true
    # T12 temporarily revokes the webhook's namespace list/watch. Restore it here
    # too, so a failure part-way through never leaves the cluster with crippled
    # RBAC and pods that can never become Ready.
    if [[ -s "${NS_VERBS_BACKUP}" && -s "${NS_RULE_INDEX}" ]]; then
        restore_namespace_verbs >/dev/null 2>&1 || true
    fi
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
    # Only DELETEs matter here. The fixture's own CREATE is legitimately flagged
    # (denied_create_inventory_unavailable: the e2e cluster has no Flux, so the
    # owning Kustomization cannot be read — the same behaviour T4 asserts), and
    # matching it would fail this test for the wrong reason.
    TEARDOWN_DELETES=$(echo "${TEARDOWN_LOGS}" \
        | grep "\"namespace\":\"${TEARDOWN_NS}\"" \
        | grep "\"operation\":\"DELETE\"" || true)
    if echo "${TEARDOWN_DELETES}" | grep -q "would deny"; then
        fail "T10: cascade DELETE flagged as drift during namespace teardown"
        log "    logs: $(echo "${TEARDOWN_DELETES}" | head -3)"
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
# T12: readiness is gated on the informer cache sync
#
# controller-runtime starts the webhook server BEFORE the caches, so a readiness
# check on the TLS listener alone reports Ready while every cache-backed lookup
# still fails — and those are fail-closed (namespace-terminating cascade, CREATE
# owner inventory, tenant service-account resolution). The manager therefore
# registers a second readyz check, "cache-sync".
#
# Proving the gate needs a cache that cannot sync, so this test revokes the
# webhook's list/watch on namespaces (keeping get, which is exactly the case the
# ClusterRole comment warns about: get alone breaks the informer). A restarted
# pod must then stay NOT Ready. Without the gate it would go Ready in seconds.
#
# Existing replicas keep serving throughout (minReplicas 3, PDB minAvailable 1),
# so the cluster is never left without an admission webhook.
# ---------------------------------------------------------------------------
log ""
log "--- T12: readiness gated on cache sync ---"

if ! command -v curl >/dev/null 2>&1; then
    log "  SKIP: curl not available on this host, cannot read /readyz?verbose"
else
    READY_POD=$(first_pod_with_ready "True") || true
    if [[ -z "${READY_POD}" ]]; then
        fail "T12-precondition — no Ready webhook pod to probe"
    else
        # T12a: in steady state both checks are registered and green.
        readyz_verbose "${READY_POD}"
        if [[ "${READYZ_CODE}" == "200" ]] && echo "${READYZ_BODY}" | grep -qF "[+]cache-sync ok"; then
            pass "T12a: /readyz reports cache-sync ok in steady state"
        else
            fail "T12a: cache-sync not reported ok (HTTP ${READYZ_CODE})"
            log "    body: ${READYZ_BODY}"
        fi

        # T12b: with the namespace informer unable to list, a replacement pod must
        # not become Ready. One pod is deleted rather than doing a rollout
        # restart: each pod requests 2 CPU, so on a single-node cluster a surge
        # pod just stays Pending — the rollout would stall for lack of capacity
        # and the assertion would pass without proving anything about readiness.
        NS_RULE_LINE=$(kubectl get clusterrole "${WEBHOOK_CLUSTERROLE}" \
            -o jsonpath='{range .rules[*]}{.resources}{"\n"}{end}' 2>/dev/null \
            | grep -n 'namespaces' | head -1 | cut -d: -f1) || true

        if [[ -z "${NS_RULE_LINE}" ]]; then
            fail "T12b-setup — could not locate the namespaces rule in ClusterRole/${WEBHOOK_CLUSTERROLE}"
        else
            NS_RULE_IDX=$((NS_RULE_LINE - 1))
            echo "${NS_RULE_IDX}" > "${NS_RULE_INDEX}"
            kubectl get clusterrole "${WEBHOOK_CLUSTERROLE}" \
                -o jsonpath="{.rules[${NS_RULE_IDX}].verbs}" > "${NS_VERBS_BACKUP}" 2>/dev/null

            BEFORE_PODS=$(webhook_pod_names)
            run_kubectl_cmd patch clusterrole "${WEBHOOK_CLUSTERROLE}" --type=json \
                -p "[{\"op\":\"replace\",\"path\":\"/rules/${NS_RULE_IDX}/verbs\",\"value\":[\"get\"]}]"

            if assert_success "T12b-revoke-namespace-list-watch"; then
                VICTIM=$(echo "${BEFORE_PODS}" | head -1)
                run_kubectl_cmd delete pod -n "${WEBHOOK_NS}" "${VICTIM}" --wait=true
                assert_success "T12b-delete-one-pod" || true

                # The replacement is whichever pod was not in the pre-delete set.
                NEW_POD=""
                for _ in $(seq 1 30); do
                    NEW_POD=$(webhook_pod_names | grep -vxF "${BEFORE_PODS}" | head -1) || true
                    if [[ -n "${NEW_POD}" ]]; then
                        break
                    fi
                    sleep 2
                done

                if [[ -z "${NEW_POD}" ]]; then
                    fail "T12b — no replacement pod appeared after deleting ${VICTIM}"
                else
                    log "    replacement pod: ${NEW_POD}"
                    # It must come up (container running) but never report Ready.
                    WENT_READY="no"
                    for _ in $(seq 1 "${READY_GATE_SECONDS}"); do
                        if [[ "$(pod_ready "${NEW_POD}")" == "True" ]]; then
                            WENT_READY="yes"
                            break
                        fi
                        sleep 1
                    done

                    if [[ "${WENT_READY}" == "yes" ]]; then
                        fail "T12b: pod became Ready with an unsyncable cache — readiness is not gated on cache sync"
                    else
                        pass "T12b: pod stays not-Ready while the namespace informer cannot sync"
                    fi

                    # T12c: pinpoint the cause — the TLS listener is up
                    # (webhook-server ok) while cache-sync is red. That pairing is
                    # the whole point; a bare "not Ready" could have any cause.
                    readyz_verbose "${NEW_POD}"
                    if echo "${READYZ_BODY}" | grep -qF "[-]cache-sync failed" &&
                        echo "${READYZ_BODY}" | grep -qF "[+]webhook-server ok"; then
                        pass "T12c: /readyz shows webhook-server ok but cache-sync failed"
                    else
                        fail "T12c: expected webhook-server ok + cache-sync failed (HTTP ${READYZ_CODE})"
                        log "    body: ${READYZ_BODY}"
                    fi
                fi
            fi

            # T12d: restoring the RBAC lets the blocked pod finish its sync.
            set +e
            KUBECTL_STDERR=$(restore_namespace_verbs 2>&1 >/dev/null)
            KUBECTL_RC=$?
            set -e
            assert_success "T12d-restore-clusterrole" || true
            : > "${NS_VERBS_BACKUP}"  # cleanup() no longer needs to restore

            RECOVERED="no"
            for _ in $(seq 1 120); do
                if [[ "$(pod_ready "${NEW_POD}")" == "True" ]]; then
                    RECOVERED="yes"
                    break
                fi
                sleep 1
            done
            if [[ "${RECOVERED}" == "yes" ]]; then
                pass "T12d: pod becomes Ready once namespace list/watch is restored"
            else
                fail "T12d: pod ${NEW_POD} did not become Ready after restoring the ClusterRole"
                kubectl get pods -n "${WEBHOOK_NS}" -l app.kubernetes.io/name=flux-drift-webhook 2>&1 | while read -r line; do
                    log "    ${line}"
                done
            fi
        fi
    fi
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

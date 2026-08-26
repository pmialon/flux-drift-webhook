/*
Copyright 2026 Qube Research & Technologies

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package webhook

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/pmialon/flux-drift-webhook/internal/config"
	"github.com/pmialon/flux-drift-webhook/internal/metrics"
)

// recordedDecision runs req through handler with a fresh registry and returns
// the single decision label recorded on flux_drift_webhook_requests_total.
func recordedDecision(t *testing.T, handler *DriftPreventionHandler, req admission.Request) string {
	t.Helper()
	reg := prometheus.NewRegistry()
	handler.Metrics = metrics.NewMetricsWithRegistry(reg)

	handler.Handle(context.Background(), req)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var decisions []string
	for _, mf := range families {
		if mf.GetName() != "flux_drift_webhook_requests_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "decision" {
					decisions = append(decisions, l.GetValue())
				}
			}
		}
	}
	if len(decisions) != 1 {
		t.Fatalf("expected exactly one recorded decision, got %v", decisions)
	}
	return decisions[0]
}

// adminUser is a plain human requester — no Flux or control-plane identity.
func adminUser() authenticationv1.UserInfo {
	return authenticationv1.UserInfo{Username: "admin@example.com", Groups: []string{"system:authenticated"}}
}

// jsonWithDeletionTimestamp is a Flux-applied object already being deleted.
func jsonWithDeletionTimestamp() []byte {
	obj := map[string]interface{}{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]interface{}{
			"name": "test-pod", "namespace": "default",
			"labels":            map[string]string{config.KustomizeLabelName: "my-app", config.KustomizeLabelNamespace: "flux-system"},
			"deletionTimestamp": "2026-01-01T00:00:00Z",
		},
	}
	raw, _ := json.Marshal(obj)
	return raw
}

// TestDecisionLabelContract drives Handle with one representative request per
// documented decision reason and asserts the exact label recorded on
// flux_drift_webhook_requests_total. The `decision` label is the operator
// contract (dashboards, alerts, enforce-rollout triage): this test is what
// keeps code, docs and metrics from drifting apart — reasons had already
// drifted once (an undocumented denied_internal_error, an unreachable
// denied_diff_error, and metrics_test using a reason that never existed).
func TestDecisionLabelContract(t *testing.T) {
	fluxMF := fluxKustomizeManagedFields()

	tests := []struct {
		reason string
		build  func(t *testing.T) (*DriftPreventionHandler, admission.Request)
	}{
		{ReasonAllowedSubresource, func(t *testing.T) (*DriftPreventionHandler, admission.Request) {
			req := createAdmissionRequest(admissionv1.Update, createTestObject(nil, nil), "default", "test-pod")
			req.SubResource = "status"
			return newTestHandler(), req
		}},
		{ReasonAllowedExcludedGroup, func(t *testing.T) (*DriftPreventionHandler, admission.Request) {
			req := createAdmissionRequest(admissionv1.Update, createTestObject(nil, nil), "", "some-vwc")
			req.Resource.Group = config.ExcludedGroupAdmission
			return newTestHandler(), req
		}},
		{ReasonAllowedNamespaceFilter, func(t *testing.T) (*DriftPreventionHandler, admission.Request) {
			return newTestHandler(), createAdmissionRequest(admissionv1.Create, createTestObject(nil, nil), "kube-system", "test-pod")
		}},
		{ReasonAllowedNotManaged, func(t *testing.T) (*DriftPreventionHandler, admission.Request) {
			return newTestHandler(), createAdmissionRequest(admissionv1.Create, createTestObject(nil, nil), "default", "test-pod")
		}},
		{ReasonDeniedMetadataError, func(t *testing.T) (*DriftPreventionHandler, admission.Request) {
			req := createAdmissionRequest(admissionv1.Create,
				runtime.RawExtension{Raw: []byte(`{"metadata": not-json`)}, "default", "test-pod")
			return newTestHandler(), req
		}},
		{ReasonAllowedOwningFluxController, func(t *testing.T) (*DriftPreventionHandler, admission.Request) {
			old := buildTestJSON(fluxKustomizeLabels, nil, fluxMF, map[string]interface{}{"template": "a"})
			updated := buildTestJSON(fluxKustomizeLabels, nil, fluxMF, map[string]interface{}{"template": "b"})
			req := createAdmissionRequest(admissionv1.Update, runtime.RawExtension{Raw: updated}, "default", "test-pod")
			req.OldObject = runtime.RawExtension{Raw: old}
			req.UserInfo = fluxControllerUserInfo
			return newTestHandler(), req
		}},
		{ReasonDeniedWrongFluxController, func(t *testing.T) (*DriftPreventionHandler, admission.Request) {
			otherLabels := map[string]string{config.KustomizeLabelName: "other-app", config.KustomizeLabelNamespace: "flux-system"}
			old := buildTestJSON(fluxKustomizeLabels, nil, fluxMF, nil)
			updated := buildTestJSON(otherLabels, nil, fluxMF, nil)
			req := createAdmissionRequest(admissionv1.Update, runtime.RawExtension{Raw: updated}, "default", "test-pod")
			req.OldObject = runtime.RawExtension{Raw: old}
			req.UserInfo = fluxControllerUserInfo
			return newTestHandler(), req
		}},
		{ReasonAllowedBypassAnnotation, func(t *testing.T) (*DriftPreventionHandler, admission.Request) {
			ann := map[string]string{config.BypassAnnotation: config.BypassValue}
			old := buildTestJSON(fluxKustomizeLabels, ann, fluxMF, map[string]interface{}{"template": "a"})
			updated := buildTestJSON(fluxKustomizeLabels, ann, fluxMF, map[string]interface{}{"template": "b"})
			req := createAdmissionRequest(admissionv1.Update, runtime.RawExtension{Raw: updated}, "default", "test-pod")
			req.OldObject = runtime.RawExtension{Raw: old}
			req.UserInfo = adminUser()
			return newTestHandler(), req
		}},
		{ReasonAllowedReconcileDisabled, func(t *testing.T) (*DriftPreventionHandler, admission.Request) {
			ann := map[string]string{config.KustomizeReconcileAnnotation: config.ReconcileDisabledValue}
			old := buildTestJSON(fluxKustomizeLabels, ann, fluxMF, map[string]interface{}{"template": "a"})
			updated := buildTestJSON(fluxKustomizeLabels, ann, fluxMF, map[string]interface{}{"template": "b"})
			req := createAdmissionRequest(admissionv1.Update, runtime.RawExtension{Raw: updated}, "default", "test-pod")
			req.OldObject = runtime.RawExtension{Raw: old}
			req.UserInfo = adminUser()
			return newTestHandler(), req
		}},
		{ReasonAllowedDeletionInProgress, func(t *testing.T) (*DriftPreventionHandler, admission.Request) {
			// DELETE of an object already carrying its own deletionTimestamp
			// (the check is DELETE-only: an UPDATE during teardown still gets
			// the normal field evaluation).
			req := createAdmissionRequest(admissionv1.Delete, runtime.RawExtension{}, "default", "test-pod")
			req.OldObject = runtime.RawExtension{Raw: jsonWithDeletionTimestamp()}
			req.UserInfo = adminUser()
			return newTestHandler(), req
		}},
		{ReasonAllowedNamespaceTerminating, func(t *testing.T) (*DriftPreventionHandler, admission.Request) {
			handler := newHandlerWithNamespace(t, "doomed", true)
			old := buildTestJSON(fluxKustomizeLabels, nil, fluxMF, nil)
			req := createAdmissionRequest(admissionv1.Delete, runtime.RawExtension{}, "doomed", "test-pod")
			req.OldObject = runtime.RawExtension{Raw: old}
			req.UserInfo = adminUser()
			return handler, req
		}},
		{ReasonAllowedNoFluxManagedFields, func(t *testing.T) (*DriftPreventionHandler, admission.Request) {
			old := buildTestJSON(fluxKustomizeLabels, nil, nil, map[string]interface{}{"template": "a"})
			updated := buildTestJSON(fluxKustomizeLabels, nil, nil, map[string]interface{}{"template": "b"})
			req := createAdmissionRequest(admissionv1.Update, runtime.RawExtension{Raw: updated}, "default", "test-pod")
			req.OldObject = runtime.RawExtension{Raw: old}
			req.UserInfo = adminUser()
			return newTestHandler(), req
		}},
		{ReasonDeniedManagedFieldsError, func(t *testing.T) (*DriftPreventionHandler, admission.Request) {
			// Valid JSON document, invalid fieldsV1 STRUCTURE: parses at the
			// metadata layer, fails in the fieldpath parser.
			raw := []byte(`{"apiVersion":"v1","kind":"Pod","metadata":{
				"name":"test-pod","namespace":"default",
				"labels":{"` + config.KustomizeLabelName + `":"my-app","` + config.KustomizeLabelNamespace + `":"flux-system"},
				"managedFields":[{"manager":"kustomize-controller","operation":"Apply","fieldsV1":{"f:spec":"not-a-map"}}]}}`)
			req := createAdmissionRequest(admissionv1.Delete, runtime.RawExtension{}, "default", "test-pod")
			req.OldObject = runtime.RawExtension{Raw: raw}
			req.UserInfo = adminUser()
			return newTestHandler(), req
		}},
		{ReasonDeniedDeleteFluxManaged, func(t *testing.T) (*DriftPreventionHandler, admission.Request) {
			old := buildTestJSON(fluxKustomizeLabels, nil, fluxMF, nil)
			req := createAdmissionRequest(admissionv1.Delete, runtime.RawExtension{}, "default", "test-pod")
			req.OldObject = runtime.RawExtension{Raw: old}
			req.UserInfo = adminUser()
			return newTestHandler(), req
		}},
		{ReasonAllowedSystemController, func(t *testing.T) (*DriftPreventionHandler, admission.Request) {
			// DELETE of a Flux-applied object by the garbage collector.
			old := buildTestJSON(fluxKustomizeLabels, nil, fluxMF, nil)
			req := createAdmissionRequest(admissionv1.Delete, runtime.RawExtension{}, "default", "test-pod")
			req.OldObject = runtime.RawExtension{Raw: old}
			req.UserInfo = gcControllerUserInfo
			return newTestHandlerWithSystemSAs(), req
		}},
		{ReasonDeniedUpdateFluxManaged, func(t *testing.T) (*DriftPreventionHandler, admission.Request) {
			oldRaw := deploymentJSON(fluxKustomizeLabels, realisticDeploymentManagedFields(), "nginx:1.0", 1)
			newRaw := deploymentJSON(fluxKustomizeLabels, realisticDeploymentManagedFields(), "nginx:2.0", 1)
			return newTestHandler(), deploymentUpdateRequest(oldRaw, newRaw)
		}},
		{ReasonDeniedManagedFieldsTampered, func(t *testing.T) (*DriftPreventionHandler, admission.Request) {
			oldRaw := deploymentJSON(fluxKustomizeLabels, realisticDeploymentManagedFields(), "nginx:1.0", 1)
			newRaw := deploymentJSON(fluxKustomizeLabels, nil, "nginx:1.0", 1)
			return newTestHandler(), deploymentUpdateRequest(oldRaw, newRaw)
		}},
		{ReasonAllowedNoFieldConflict, func(t *testing.T) (*DriftPreventionHandler, admission.Request) {
			oldRaw := deploymentJSON(fluxKustomizeLabels, realisticDeploymentManagedFields(), "nginx:1.0", 1)
			newRaw := deploymentJSON(fluxKustomizeLabels, realisticDeploymentManagedFields(), "nginx:1.0", 5)
			return newTestHandler(), deploymentUpdateRequest(oldRaw, newRaw)
		}},
		{ReasonAllowedDriftIgnoredField, func(t *testing.T) (*DriftPreventionHandler, admission.Request) {
			handler := newHandlerWithOwnerIgnore(t, ignoreEntry(nil, "/spec/replicas"))
			oldRaw := deploymentJSON(fluxKustomizeLabels, fluxManagedReplicasFields(), "nginx:1.0", 1)
			newRaw := deploymentJSON(fluxKustomizeLabels, fluxManagedReplicasFields(), "nginx:1.0", 5)
			return handler, deploymentUpdateRequest(oldRaw, newRaw)
		}},
		{ReasonDeniedBypassAnnotationAdded, func(t *testing.T) (*DriftPreventionHandler, admission.Request) {
			ann := map[string]string{config.BypassAnnotation: config.BypassValue}
			old := buildTestJSON(fluxKustomizeLabels, nil, fluxMF, nil)
			updated := buildTestJSON(fluxKustomizeLabels, ann, fluxMF, nil)
			req := createAdmissionRequest(admissionv1.Update, runtime.RawExtension{Raw: updated}, "default", "test-pod")
			req.OldObject = runtime.RawExtension{Raw: old}
			req.UserInfo = adminUser()
			return newTestHandler(), req
		}},
		{ReasonDeniedCreateFluxLabels, func(t *testing.T) (*DriftPreventionHandler, admission.Request) {
			handler := newHandlerWithOwnerInventory(t, "default_test-pod__Pod")
			req := createAdmissionRequest(admissionv1.Create, createTestObject(fluxKustomizeLabels, nil), "default", "test-pod")
			req.UserInfo = adminUser()
			return handler, req
		}},
		{ReasonAllowedNotInOwnerInventory, func(t *testing.T) (*DriftPreventionHandler, admission.Request) {
			handler := newHandlerWithOwnerInventory(t, "default_some-other-object__ConfigMap")
			req := createAdmissionRequest(admissionv1.Create, createTestObject(fluxKustomizeLabels, nil), "default", "test-pod")
			req.UserInfo = vmOperatorUser()
			return handler, req
		}},
		{ReasonAllowedOwnedResource, func(t *testing.T) (*DriftPreventionHandler, admission.Request) {
			// Inventory unreadable (nil client) + controller ownerReference.
			obj := createTestObjectWithOwner(fluxKustomizeLabels, controllerOwnerRef())
			req := createAdmissionRequest(admissionv1.Create, obj, "default", "test-pod")
			req.UserInfo = adminUser()
			return newTestHandler(), req
		}},
		{ReasonDeniedCreateInventoryUnavail, func(t *testing.T) (*DriftPreventionHandler, admission.Request) {
			req := createAdmissionRequest(admissionv1.Create, createTestObject(fluxKustomizeLabels, nil), "default", "test-pod")
			req.UserInfo = adminUser()
			return newTestHandler(), req
		}},
		{ReasonAllowedUnknownOperation, func(t *testing.T) (*DriftPreventionHandler, admission.Request) {
			req := createAdmissionRequest(admissionv1.Connect, createTestObject(fluxKustomizeLabels, nil), "default", "test-pod")
			req.UserInfo = adminUser()
			return newTestHandler(), req
		}},
	}

	covered := map[string]bool{}
	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			handler, req := tt.build(t)
			if got := recordedDecision(t, handler, req); got != tt.reason {
				t.Errorf("recorded decision = %q, want %q", got, tt.reason)
			}
		})
		covered[tt.reason] = true
	}

	// denied_parse_error is defence in depth: any payload that fails
	// parseAndDiff's map unmarshal fails metadata extraction first
	// (denied_metadata_error), so it is unreachable through Handle by
	// construction — pin it at the function level instead.
	t.Run(ReasonDeniedParseError, func(t *testing.T) {
		h := newTestHandler()
		req := deploymentUpdateRequest([]byte(`{"bad"`), []byte(`{}`))
		_, denied := h.parseAndDiff(req, logr.Discard())
		if denied == nil || denied.reason != ReasonDeniedParseError {
			t.Fatalf("parseAndDiff on malformed old object = %v, want %s", denied, ReasonDeniedParseError)
		}
	})
	covered[ReasonDeniedParseError] = true

	// Every documented reason must be exercised above; every exercised reason
	// must be documented. Either direction failing means the operator contract
	// and the code have drifted.
	for _, reason := range AllDecisionReasons {
		if !covered[reason] {
			t.Errorf("documented reason %q has no contract-test fixture", reason)
		}
	}
	if len(covered) != len(AllDecisionReasons) {
		t.Errorf("contract test covers %d reasons, AllDecisionReasons documents %d", len(covered), len(AllDecisionReasons))
	}
}

// TestAllDecisionReasonsUnique guards the contract list itself.
func TestAllDecisionReasonsUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range AllDecisionReasons {
		if seen[r] {
			t.Errorf("duplicate reason %q in AllDecisionReasons", r)
		}
		seen[r] = true
		if r == "" {
			t.Error("empty reason in AllDecisionReasons")
		}
	}
}

var _ = metav1.Now // keep metav1 imported for future fixtures

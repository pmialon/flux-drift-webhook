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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/pmialon/flux-drift-webhook/internal/config"
	"github.com/pmialon/flux-drift-webhook/internal/metrics"
)

func newTestMetrics() *metrics.Metrics {
	return metrics.NewMetricsWithRegistry(prometheus.NewRegistry())
}

func newTestHandler() *DriftPreventionHandler {
	return &DriftPreventionHandler{
		Log:           logr.Discard(),
		FluxNamespace: "flux-system",
		AuditOnly:     false,
		Metrics:       newTestMetrics(),
	}
}

var fluxKustomizeLabels = map[string]string{
	config.KustomizeLabelName:      "my-app",
	config.KustomizeLabelNamespace: "flux-system",
}

var fluxHelmLabels = map[string]string{
	config.HelmLabelName:      "my-release",
	config.HelmLabelNamespace: "flux-system",
}

var helmControllerUserInfo = authenticationv1.UserInfo{
	Username: "system:serviceaccount:flux-system:helm-controller",
	Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:flux-system", "system:authenticated"},
}

var fluxControllerUserInfo = authenticationv1.UserInfo{
	Username: "system:serviceaccount:flux-system:kustomize-controller",
	Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:flux-system", "system:authenticated"},
}

var gcControllerUserInfo = authenticationv1.UserInfo{
	Username: "system:serviceaccount:kube-system:generic-garbage-collector",
	Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:kube-system", "system:authenticated"},
}

var endpointControllerUserInfo = authenticationv1.UserInfo{
	Username: "system:serviceaccount:kube-system:endpoint-controller",
	Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:kube-system", "system:authenticated"},
}

var ttlControllerUserInfo = authenticationv1.UserInfo{
	Username: "system:serviceaccount:kube-system:ttl-after-finished-controller",
	Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:kube-system", "system:authenticated"},
}

// newTestHandlerWithSystemSAs returns a handler whose system-controller
// allow-list is populated with the built-in defaults (newTestHandler leaves it
// empty, matching a deployment with no configured SAs).
func newTestHandlerWithSystemSAs() *DriftPreventionHandler {
	h := newTestHandler()
	h.SystemControllerSAs = config.DefaultSystemControllerServiceAccounts()
	return h
}

func fluxKustomizeManagedFields() []metav1.ManagedFieldsEntry {
	return []metav1.ManagedFieldsEntry{
		{
			Manager:   "kustomize-controller",
			Operation: metav1.ManagedFieldsOperationApply,
			FieldsV1:  &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:template":{}}}`)},
		},
	}
}

func controllerOwnerRef() []metav1.OwnerReference {
	controller := true
	return []metav1.OwnerReference{{
		APIVersion: "v1", Kind: "Service", Name: "parent-svc", UID: "uid-1", Controller: &controller,
	}}
}

func nonControllerOwnerRef() []metav1.OwnerReference {
	controller := false
	return []metav1.OwnerReference{{
		APIVersion: "v1", Kind: "Service", Name: "parent-svc", UID: "uid-1", Controller: &controller,
	}}
}

// createTestObjectWithOwner is createTestObject plus optional ownerReferences.
func createTestObjectWithOwner(labels, annotations map[string]string, owners []metav1.OwnerReference) runtime.RawExtension {
	metadata := map[string]interface{}{
		"name":        "test-pod",
		"namespace":   "default",
		"labels":      labels,
		"annotations": annotations,
	}
	if owners != nil {
		ownersJSON, _ := json.Marshal(owners)
		var o []interface{}
		_ = json.Unmarshal(ownersJSON, &o)
		metadata["ownerReferences"] = o
	}
	obj := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   metadata,
	}
	raw, _ := json.Marshal(obj)
	return runtime.RawExtension{Raw: raw}
}

func TestHandle_NotManagedByFlux(t *testing.T) {
	handler := newTestHandler()
	obj := createTestObject(map[string]string{"app": "test"}, nil)
	req := createAdmissionRequest(admissionv1.Update, obj, "default", "test-pod")
	req.OldObject = obj

	resp := handler.Handle(context.Background(), req)

	if !resp.Allowed {
		t.Errorf("expected allowed=true for non-Flux managed resource, got allowed=%v", resp.Allowed)
	}
}

func TestHandle_FluxControllerAllowed(t *testing.T) {
	handler := newTestHandler()
	obj := createTestObject(fluxKustomizeLabels, nil)
	req := createAdmissionRequest(admissionv1.Update, obj, "default", "test-pod")
	req.OldObject = obj
	req.UserInfo = fluxControllerUserInfo

	resp := handler.Handle(context.Background(), req)

	if !resp.Allowed {
		t.Errorf("expected allowed=true for Flux controller, got allowed=%v", resp.Allowed)
	}
}

func TestHandle_WrongFluxControllerDenied(t *testing.T) {
	handler := newTestHandler()

	oldObj := createTestObject(map[string]string{
		config.KustomizeLabelName:      "app-a",
		config.KustomizeLabelNamespace: "flux-system",
	}, nil)
	newObj := createTestObject(map[string]string{
		config.KustomizeLabelName:      "app-b",
		config.KustomizeLabelNamespace: "flux-system",
	}, nil)

	req := createAdmissionRequest(admissionv1.Update, newObj, "default", "test-pod")
	req.OldObject = oldObj
	req.UserInfo = fluxControllerUserInfo

	resp := handler.Handle(context.Background(), req)

	if resp.Allowed {
		t.Error("expected denied for wrong Flux controller, got allowed=true")
	}
	if !strings.Contains(resp.Result.Message, "Flux controller mismatch") {
		t.Errorf("expected mismatch message, got: %s", resp.Result.Message)
	}
}

func TestHandle_DeletionInProgressAllowed(t *testing.T) {
	handler := newTestHandler()

	now := metav1.Now()
	metadata := map[string]interface{}{
		"name":              "test-pod",
		"namespace":         "default",
		"labels":            fluxKustomizeLabels,
		"deletionTimestamp": now.Format(time.RFC3339),
	}
	obj := marshalTestObj(metadata, nil)

	req := createAdmissionRequest(admissionv1.Delete, obj, "default", "test-pod")
	req.OldObject = obj
	req.Object = runtime.RawExtension{}
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if !resp.Allowed {
		t.Errorf("expected allowed for deletion in progress, got allowed=%v", resp.Allowed)
	}
}

func TestHandle_BypassAnnotation(t *testing.T) {
	handler := newTestHandler()

	bypassAnnotations := map[string]string{config.BypassAnnotation: config.BypassValue}
	oldObj := createTestObject(fluxKustomizeLabels, bypassAnnotations)
	newObj := createTestObject(fluxKustomizeLabels, bypassAnnotations)
	req := createAdmissionRequest(admissionv1.Update, newObj, "default", "test-pod")
	req.OldObject = oldObj
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if !resp.Allowed {
		t.Errorf("expected allowed=true with bypass annotation on old object, got allowed=%v", resp.Allowed)
	}
}

func TestHandle_BypassAnnotation_SingleStepAttackDenied(t *testing.T) {
	handler := newTestHandler()

	managedFields := []metav1.ManagedFieldsEntry{
		{
			Manager:   "kustomize-controller",
			Operation: metav1.ManagedFieldsOperationApply,
			FieldsV1:  &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:template":{}}}`)},
		},
	}

	oldRaw := buildTestJSON(fluxKustomizeLabels, nil, managedFields, map[string]interface{}{"template": "original"})
	oldObj := runtime.RawExtension{Raw: oldRaw}
	newRaw := buildTestJSON(fluxKustomizeLabels,
		map[string]string{config.BypassAnnotation: config.BypassValue},
		managedFields, map[string]interface{}{"template": "modified"})
	newObj := runtime.RawExtension{Raw: newRaw}
	req := createAdmissionRequest(admissionv1.Update, newObj, "default", "test-pod")
	req.OldObject = oldObj
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if resp.Allowed {
		t.Error("expected denied for single-step bypass attack, got allowed=true")
	}
}

func TestHandle_DeleteFluxManagedDenied(t *testing.T) {
	handler := newTestHandler()

	// Genuinely Flux-applied: carries a kustomize-controller fieldManager.
	oldRaw := buildTestJSON(fluxKustomizeLabels, nil, fluxKustomizeManagedFields(), nil)
	req := createAdmissionRequest(admissionv1.Delete, runtime.RawExtension{Raw: oldRaw}, "default", "test-pod")
	req.OldObject = runtime.RawExtension{Raw: oldRaw}
	req.Object = runtime.RawExtension{}
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if resp.Allowed {
		t.Error("expected denied for DELETE of Flux-managed resource, got allowed=true")
	}
}

func TestHandle_DeleteFluxControllerAllowed(t *testing.T) {
	handler := newTestHandler()

	obj := createTestObject(fluxKustomizeLabels, nil)
	req := createAdmissionRequest(admissionv1.Delete, obj, "default", "test-pod")
	req.OldObject = obj
	req.Object = runtime.RawExtension{}
	req.UserInfo = fluxControllerUserInfo

	resp := handler.Handle(context.Background(), req)

	if !resp.Allowed {
		t.Errorf("expected allowed for DELETE by Flux controller, got allowed=%v", resp.Allowed)
	}
}

func TestHandle_DeleteFluxManagedBySystemControllerAllowed(t *testing.T) {
	handler := newTestHandlerWithSystemSAs()

	// Genuinely Flux-applied Job (kustomize-controller fieldManager) deleted by
	// the TTL-after-finished controller -> legitimate control-plane lifecycle.
	oldRaw := buildTestJSON(fluxKustomizeLabels, nil, fluxKustomizeManagedFields(), nil)
	req := createAdmissionRequest(admissionv1.Delete, runtime.RawExtension{Raw: oldRaw}, "default", "test-job")
	req.OldObject = runtime.RawExtension{Raw: oldRaw}
	req.Object = runtime.RawExtension{}
	req.UserInfo = ttlControllerUserInfo

	resp := handler.Handle(context.Background(), req)

	if !resp.Allowed {
		t.Errorf("expected allowed for DELETE by recognised control-plane controller, got allowed=%v", resp.Allowed)
	}
}

func TestHandle_DeleteFluxManagedByHumanDeniedWithSystemSAs(t *testing.T) {
	// Even with the system-controller allow-list configured, a human deleting a
	// genuinely Flux-applied resource is still denied.
	handler := newTestHandlerWithSystemSAs()

	oldRaw := buildTestJSON(fluxKustomizeLabels, nil, fluxKustomizeManagedFields(), nil)
	req := createAdmissionRequest(admissionv1.Delete, runtime.RawExtension{Raw: oldRaw}, "default", "test-job")
	req.OldObject = runtime.RawExtension{Raw: oldRaw}
	req.Object = runtime.RawExtension{}
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if resp.Allowed {
		t.Error("expected denied for DELETE of Flux-managed resource by a human, got allowed=true")
	}
}

func TestHandle_CreateWithFluxLabelsDenied(t *testing.T) {
	handler := newTestHandler()

	obj := createTestObject(fluxKustomizeLabels, nil)
	req := createAdmissionRequest(admissionv1.Create, obj, "default", "test-pod")
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if resp.Allowed {
		t.Error("expected denied for CREATE with Flux labels by non-Flux user, got allowed=true")
	}
}

func TestHandle_CreateByFluxControllerAllowed(t *testing.T) {
	handler := newTestHandler()

	obj := createTestObject(fluxKustomizeLabels, nil)
	req := createAdmissionRequest(admissionv1.Create, obj, "default", "test-pod")
	req.UserInfo = fluxControllerUserInfo

	resp := handler.Handle(context.Background(), req)

	if !resp.Allowed {
		t.Errorf("expected allowed for CREATE by Flux controller, got allowed=%v", resp.Allowed)
	}
}

func TestHandle_MetadataExtractionErrorDenied(t *testing.T) {
	handler := newTestHandler()

	invalidObj := runtime.RawExtension{Raw: []byte(`{invalid json`)}
	req := createAdmissionRequest(admissionv1.Update, invalidObj, "default", "test-pod")
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if resp.Allowed {
		t.Error("expected denied for invalid JSON metadata, got allowed=true")
	}
}

func TestHandle_AuditOnlyMode(t *testing.T) {
	handler := newTestHandler()
	handler.AuditOnly = true

	// Genuinely Flux-applied (kustomize-controller fieldManager) so the DELETE
	// would be denied -> produces an audit warning in audit-only mode.
	oldRaw := buildTestJSON(fluxKustomizeLabels, nil, fluxKustomizeManagedFields(), nil)
	req := createAdmissionRequest(admissionv1.Delete, runtime.RawExtension{Raw: oldRaw}, "default", "test-pod")
	req.OldObject = runtime.RawExtension{Raw: oldRaw}
	req.Object = runtime.RawExtension{}
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if !resp.Allowed {
		t.Errorf("expected allowed=true in audit-only mode, got allowed=%v", resp.Allowed)
	}
	if len(resp.Warnings) == 0 {
		t.Error("expected audit warnings in response")
	}
}

func TestHandle_ParseErrorOldObjectDenied(t *testing.T) {
	handler := newTestHandler()

	managedFields := []metav1.ManagedFieldsEntry{
		{
			Manager:   "kustomize-controller",
			Operation: metav1.ManagedFieldsOperationApply,
			FieldsV1:  &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:template":{}}}`)},
		},
	}

	oldRaw := buildTestJSON(fluxKustomizeLabels, nil, managedFields, nil)
	oldRaw = corruptJSONSpec(oldRaw)

	newRaw := buildTestJSON(fluxKustomizeLabels, nil, managedFields, map[string]interface{}{"template": "modified"})

	req := createAdmissionRequest(admissionv1.Update, runtime.RawExtension{Raw: newRaw}, "default", "test-pod")
	req.OldObject = runtime.RawExtension{Raw: oldRaw}
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if resp.Allowed {
		t.Error("expected denied when old object fails full parse (fail-closed), got allowed=true")
	}
}

func TestHandle_ParseErrorNewObjectDenied(t *testing.T) {
	handler := newTestHandler()

	managedFields := []metav1.ManagedFieldsEntry{
		{
			Manager:   "kustomize-controller",
			Operation: metav1.ManagedFieldsOperationApply,
			FieldsV1:  &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:template":{}}}`)},
		},
	}

	oldRaw := buildTestJSON(fluxKustomizeLabels, nil, managedFields, map[string]interface{}{"template": "original"})

	newRaw := buildTestJSON(fluxKustomizeLabels, nil, managedFields, nil)
	newRaw = corruptJSONSpec(newRaw)

	req := createAdmissionRequest(admissionv1.Update, runtime.RawExtension{Raw: newRaw}, "default", "test-pod")
	req.OldObject = runtime.RawExtension{Raw: oldRaw}
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if resp.Allowed {
		t.Error("expected denied when new object fails full parse (fail-closed), got allowed=true")
	}
}

func TestHandle_ManagedFieldsExtractionErrorDenied(t *testing.T) {
	handler := newTestHandler()

	// Inject invalid fieldsV1 directly as raw JSON (buildTestJSON would
	// base64-encode FieldsV1.Raw, losing the corruption).
	rawTemplate := `{
		"apiVersion":"v1","kind":"Pod",
		"metadata":{
			"name":"test-pod","namespace":"default",
			"labels":{"kustomize.toolkit.fluxcd.io/name":"my-app","kustomize.toolkit.fluxcd.io/namespace":"flux-system"},
			"managedFields":[{"manager":"kustomize-controller","operation":"Apply","fieldsV1":{invalid}}]
		},
		"spec":{"template":"%s"}
	}`
	oldRaw := []byte(fmt.Sprintf(rawTemplate, "original"))
	newRaw := []byte(fmt.Sprintf(rawTemplate, "modified"))

	req := createAdmissionRequest(admissionv1.Update, runtime.RawExtension{Raw: newRaw}, "default", "test-pod")
	req.OldObject = runtime.RawExtension{Raw: oldRaw}
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if resp.Allowed {
		t.Error("expected denied when managedFields extraction fails (fail-closed), got allowed=true")
	}
}

func TestHandle_UpdateNoFluxManagedFieldsAllowed(t *testing.T) {
	handler := newTestHandler()

	obj := createTestObject(fluxKustomizeLabels, nil)
	req := createAdmissionRequest(admissionv1.Update, obj, "default", "test-pod")
	req.OldObject = obj
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if !resp.Allowed {
		t.Errorf("expected allowed when no SSA managed fields exist, got allowed=%v", resp.Allowed)
	}
}

func TestHandle_UpdateFluxManagedFieldConflictDenied(t *testing.T) {
	handler := newTestHandler()

	managedFields := []metav1.ManagedFieldsEntry{
		{
			Manager:   "kustomize-controller",
			Operation: metav1.ManagedFieldsOperationApply,
			FieldsV1:  &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:template":{}}}`)},
		},
	}

	oldRaw := buildTestJSON(fluxKustomizeLabels, nil, managedFields, map[string]interface{}{"template": "original"})
	newRaw := buildTestJSON(fluxKustomizeLabels, nil, managedFields, map[string]interface{}{"template": "modified"})

	req := createAdmissionRequest(admissionv1.Update, runtime.RawExtension{Raw: newRaw}, "default", "test-pod")
	req.OldObject = runtime.RawExtension{Raw: oldRaw}
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if resp.Allowed {
		t.Error("expected denied when modifying Flux-managed fields, got allowed=true")
	}
	if !strings.Contains(resp.Result.Message, "cannot modify Flux-managed fields") {
		t.Errorf("expected conflict message, got: %s", resp.Result.Message)
	}
}

func TestHandle_UpdateNonFluxFieldsAllowed(t *testing.T) {
	handler := newTestHandler()

	managedFields := []metav1.ManagedFieldsEntry{
		{
			Manager:   "kustomize-controller",
			Operation: metav1.ManagedFieldsOperationApply,
			FieldsV1:  &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:template":{}}}`)},
		},
	}

	oldRaw := buildTestJSON(fluxKustomizeLabels, nil, managedFields, map[string]interface{}{
		"template": "original",
		"replicas": float64(1),
	})
	newRaw := buildTestJSON(fluxKustomizeLabels, nil, managedFields, map[string]interface{}{
		"template": "original",
		"replicas": float64(3),
	})

	req := createAdmissionRequest(admissionv1.Update, runtime.RawExtension{Raw: newRaw}, "default", "test-pod")
	req.OldObject = runtime.RawExtension{Raw: oldRaw}
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if !resp.Allowed {
		t.Errorf("expected allowed when updating non-Flux fields (HPA scenario), got allowed=%v", resp.Allowed)
	}
}

func createTestObject(labels, annotations map[string]string) runtime.RawExtension {
	obj := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]interface{}{
			"name":        "test-pod",
			"namespace":   "default",
			"labels":      labels,
			"annotations": annotations,
		},
	}
	raw, _ := json.Marshal(obj)
	return runtime.RawExtension{Raw: raw}
}

func marshalTestObj(metadata map[string]interface{}, spec map[string]interface{}) runtime.RawExtension {
	obj := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   metadata,
	}
	if spec != nil {
		obj["spec"] = spec
	}
	raw, _ := json.Marshal(obj)
	return runtime.RawExtension{Raw: raw}
}

func buildTestJSON(labels, annotations map[string]string, managedFields []metav1.ManagedFieldsEntry, spec map[string]interface{}) []byte {
	metadata := map[string]interface{}{
		"name":      "test-pod",
		"namespace": "default",
		"labels":    labels,
	}
	if annotations != nil {
		metadata["annotations"] = annotations
	}
	if managedFields != nil {
		mfJSON, _ := json.Marshal(managedFields)
		var mf []interface{}
		_ = json.Unmarshal(mfJSON, &mf)
		metadata["managedFields"] = mf
	}
	obj := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   metadata,
	}
	if spec != nil {
		obj["spec"] = spec
	}
	raw, _ := json.Marshal(obj)
	return raw
}

// corruptJSONSpec makes the spec un-parseable while keeping metadata valid.
func corruptJSONSpec(raw []byte) []byte {
	s := string(raw)
	return []byte(s[:len(s)-1] + `,"spec":{"bad":` + "}")
}

func createAdmissionRequest(op admissionv1.Operation, obj runtime.RawExtension, ns, name string) admission.Request {
	return admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "test-uid",
			Operation: op,
			Namespace: ns,
			Name:      name,
			Kind:      metav1.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"},
			Object:    obj,
		},
	}
}

func TestHandle_HelmRelease_DeleteDenied(t *testing.T) {
	handler := newTestHandler()

	managedFields := []metav1.ManagedFieldsEntry{
		{
			Manager:   "helm-controller",
			Operation: metav1.ManagedFieldsOperationApply,
			FieldsV1:  &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:template":{}}}`)},
		},
	}
	oldRaw := buildTestJSON(fluxHelmLabels, nil, managedFields, nil)
	req := createAdmissionRequest(admissionv1.Delete, runtime.RawExtension{Raw: oldRaw}, "default", "test-pod")
	req.OldObject = runtime.RawExtension{Raw: oldRaw}
	req.Object = runtime.RawExtension{}
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if resp.Allowed {
		t.Error("expected denied for DELETE of HelmRelease-managed resource, got allowed=true")
	}
	if !strings.Contains(resp.Result.Message, "my-release") {
		t.Errorf("expected HelmRelease name in message, got: %s", resp.Result.Message)
	}
}

// --- Derived / inherited-label resources (structural false positives) ---

func TestHandle_CreateOwnedResourceAllowed(t *testing.T) {
	// Controller ownerReference (e.g. EndpointSlice owned by a Service, or
	// CertificateRequest owned by a Certificate) created by a non-Flux,
	// non-kube-system actor: Flux labels are inherited -> allowed. Uses the
	// default handler (empty system-SA list) to prove the ownerReference signal
	// alone suffices and covers cert-manager.
	handler := newTestHandler()
	obj := createTestObjectWithOwner(fluxKustomizeLabels, nil, controllerOwnerRef())
	req := createAdmissionRequest(admissionv1.Create, obj, "default", "test-pod")
	req.UserInfo = authenticationv1.UserInfo{
		Username: "system:serviceaccount:cert-manager:cert-manager",
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:cert-manager"},
	}

	resp := handler.Handle(context.Background(), req)

	if !resp.Allowed {
		t.Errorf("expected allowed for CREATE of owned resource, got allowed=%v", resp.Allowed)
	}
	if !strings.Contains(resp.Result.Message, "inherited") {
		t.Errorf("expected owned-resource reason, got: %s", resp.Result.Message)
	}
}

func TestHandle_CreateBySystemControllerNoOwnerAllowed(t *testing.T) {
	// Classic Endpoints carry NO ownerReference; only the actor identifies them.
	handler := newTestHandlerWithSystemSAs()
	obj := createTestObject(fluxKustomizeLabels, nil)
	req := createAdmissionRequest(admissionv1.Create, obj, "default", "test-pod")
	req.UserInfo = endpointControllerUserInfo

	resp := handler.Handle(context.Background(), req)

	if !resp.Allowed {
		t.Errorf("expected allowed for CREATE by endpoint-controller, got allowed=%v", resp.Allowed)
	}
	if !strings.Contains(resp.Result.Message, "control-plane") {
		t.Errorf("expected system-controller reason, got: %s", resp.Result.Message)
	}
}

func TestHandle_CreateFluxLabelsNoOwnerHumanDenied(t *testing.T) {
	// Core protection intact: a human creating a Flux-labelled object with no
	// ownerReference is still blocked.
	handler := newTestHandlerWithSystemSAs()
	obj := createTestObject(fluxKustomizeLabels, nil)
	req := createAdmissionRequest(admissionv1.Create, obj, "default", "test-pod")
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if resp.Allowed {
		t.Error("expected denied for human CREATE of Flux-labelled resource without ownerReference")
	}
}

func TestHandle_CreateNonControllerOwnerHumanDenied(t *testing.T) {
	// A non-controller (controller:false) ownerReference must NOT bypass.
	handler := newTestHandlerWithSystemSAs()
	obj := createTestObjectWithOwner(fluxKustomizeLabels, nil, nonControllerOwnerRef())
	req := createAdmissionRequest(admissionv1.Create, obj, "default", "test-pod")
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if resp.Allowed {
		t.Error("expected denied for CREATE with non-controller ownerReference by human")
	}
}

func TestHandle_CreateUnknownKubeSystemSADenied(t *testing.T) {
	handler := newTestHandlerWithSystemSAs()
	obj := createTestObject(fluxKustomizeLabels, nil)
	req := createAdmissionRequest(admissionv1.Create, obj, "default", "test-pod")
	req.UserInfo = authenticationv1.UserInfo{
		Username: "system:serviceaccount:kube-system:daemon-set-controller",
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:kube-system"},
	}

	resp := handler.Handle(context.Background(), req)

	if resp.Allowed {
		t.Error("expected denied for CREATE by unrecognised kube-system SA")
	}
}

func TestHandle_DeleteInheritedLabelAllowed(t *testing.T) {
	// Object carries a Flux label but NO Flux fieldManager (label inherited):
	// deletion by the garbage collector is allowed.
	handler := newTestHandler()
	oldRaw := buildTestJSON(fluxKustomizeLabels, nil, nil, nil)
	req := createAdmissionRequest(admissionv1.Delete, runtime.RawExtension{Raw: oldRaw}, "default", "test-pod")
	req.OldObject = runtime.RawExtension{Raw: oldRaw}
	req.Object = runtime.RawExtension{}
	req.UserInfo = gcControllerUserInfo

	resp := handler.Handle(context.Background(), req)

	if !resp.Allowed {
		t.Errorf("expected allowed for DELETE of inherited-label resource, got allowed=%v", resp.Allowed)
	}
	if !strings.Contains(resp.Result.Message, "inheritance") {
		t.Errorf("expected inherited-label delete reason, got: %s", resp.Result.Message)
	}
}

func TestHandle_DeleteInheritedLabelByHumanAllowed(t *testing.T) {
	// Even a human deleting a never-Flux-applied object (label inherited) is
	// allowed: it is not actually Flux-managed.
	handler := newTestHandler()
	oldRaw := buildTestJSON(fluxKustomizeLabels, nil, nil, nil)
	req := createAdmissionRequest(admissionv1.Delete, runtime.RawExtension{Raw: oldRaw}, "default", "test-pod")
	req.OldObject = runtime.RawExtension{Raw: oldRaw}
	req.Object = runtime.RawExtension{}
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if !resp.Allowed {
		t.Errorf("expected allowed for DELETE of inherited-label resource by human, got allowed=%v", resp.Allowed)
	}
}

func TestHandle_SubresourceAllowed(t *testing.T) {
	handler := newTestHandler()
	obj := createTestObject(fluxKustomizeLabels, nil)
	req := createAdmissionRequest(admissionv1.Update, obj, "default", "test-pod")
	req.SubResource = "status"
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if !resp.Allowed {
		t.Errorf("expected allowed for subresource request, got allowed=%v", resp.Allowed)
	}
	if !strings.Contains(resp.Result.Message, "subresource") {
		t.Errorf("expected subresource reason, got: %s", resp.Result.Message)
	}
}

func TestHandle_HelmRelease_UpdateFieldConflictDenied(t *testing.T) {
	handler := newTestHandler()

	managedFields := []metav1.ManagedFieldsEntry{
		{
			Manager:   "helm-controller",
			Operation: metav1.ManagedFieldsOperationApply,
			FieldsV1:  &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:template":{}}}`)},
		},
	}

	oldRaw := buildTestJSON(fluxHelmLabels, nil, managedFields, map[string]interface{}{"template": "original"})
	newRaw := buildTestJSON(fluxHelmLabels, nil, managedFields, map[string]interface{}{"template": "modified"})

	req := createAdmissionRequest(admissionv1.Update, runtime.RawExtension{Raw: newRaw}, "default", "test-pod")
	req.OldObject = runtime.RawExtension{Raw: oldRaw}
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if resp.Allowed {
		t.Error("expected denied when modifying HelmRelease-managed fields, got allowed=true")
	}
	if !strings.Contains(resp.Result.Message, "my-release") {
		t.Errorf("expected HelmRelease name in message, got: %s", resp.Result.Message)
	}
}

func TestHandle_HelmRelease_ControllerAllowed(t *testing.T) {
	handler := newTestHandler()

	obj := createTestObject(fluxHelmLabels, nil)
	req := createAdmissionRequest(admissionv1.Update, obj, "default", "test-pod")
	req.OldObject = obj
	req.UserInfo = helmControllerUserInfo

	resp := handler.Handle(context.Background(), req)

	if !resp.Allowed {
		t.Errorf("expected allowed for helm-controller on HelmRelease-managed resource, got allowed=%v", resp.Allowed)
	}
}

func TestShouldProcessNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	tests := []struct {
		name            string
		namespace       string
		namespaceLabels map[string]string
		configLabel     string
		configValue     string
		expectedProcess bool
	}{
		{
			name:            "kube-system always excluded",
			namespace:       "kube-system",
			namespaceLabels: map[string]string{"drift-prevention": "enabled"},
			configLabel:     "drift-prevention",
			expectedProcess: false,
		},
		{
			name:            "flux-system always excluded",
			namespace:       "flux-system",
			namespaceLabels: map[string]string{"drift-prevention": "enabled"},
			configLabel:     "drift-prevention",
			expectedProcess: false,
		},
		{
			name:            "no filter - process all non-excluded",
			namespace:       "prod",
			configLabel:     "",
			expectedProcess: true,
		},
		{
			name:            "label exists - any value",
			namespace:       "prod",
			namespaceLabels: map[string]string{"drift-prevention": "anything"},
			configLabel:     "drift-prevention",
			expectedProcess: true,
		},
		{
			name:            "label missing",
			namespace:       "dev",
			namespaceLabels: map[string]string{},
			configLabel:     "drift-prevention",
			expectedProcess: false,
		},
		{
			name:            "label and value match",
			namespace:       "prod",
			namespaceLabels: map[string]string{"drift-prevention": "enabled"},
			configLabel:     "drift-prevention",
			configValue:     "enabled",
			expectedProcess: true,
		},
		{
			name:            "label exists but value mismatch",
			namespace:       "dev",
			namespaceLabels: map[string]string{"drift-prevention": "disabled"},
			configLabel:     "drift-prevention",
			configValue:     "enabled",
			expectedProcess: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:   tt.namespace,
					Labels: tt.namespaceLabels,
				},
			}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(ns).
				Build()

			handler := &DriftPreventionHandler{
				Log:                 logr.Discard(),
				NamespaceLabel:      tt.configLabel,
				NamespaceLabelValue: tt.configValue,
				Client:              fakeClient,
			}

			shouldProcess, err := handler.shouldProcessNamespace(context.Background(), tt.namespace, logr.Discard())
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}

			if shouldProcess != tt.expectedProcess {
				t.Errorf("expected shouldProcess=%v, got %v", tt.expectedProcess, shouldProcess)
			}
		})
	}
}

func saUser(username string) authenticationv1.UserInfo {
	return authenticationv1.UserInfo{Username: username, Groups: []string{"system:serviceaccounts"}}
}

func TestIsOwningFluxReconciler(t *testing.T) {
	ksGVK := schema.GroupVersionKind{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization"}
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(ksGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(ksGVK.GroupVersion().WithKind("KustomizationList"), &unstructured.UnstructuredList{})

	// Owning Kustomization kafka/kafka-plain impersonates kafka:flux-reconciler.
	owner := &unstructured.Unstructured{}
	owner.SetGroupVersionKind(ksGVK)
	owner.SetNamespace("kafka")
	owner.SetName("kafka-plain")
	_ = unstructured.SetNestedField(owner.Object, "flux-reconciler", "spec", "serviceAccountName")

	// Owning Kustomization redis/redis-app sets no serviceAccountName.
	ownerNoSA := &unstructured.Unstructured{}
	ownerNoSA.SetGroupVersionKind(ksGVK)
	ownerNoSA.SetNamespace("redis")
	ownerNoSA.SetName("redis-app")

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, ownerNoSA).Build()
	handler := &DriftPreventionHandler{Log: logr.Discard(), FluxNamespace: "flux-system", Client: client}

	kafka := FluxManagementInfo{IsManaged: true, ManagedBy: "kustomization", ControllerName: "kafka-plain", ControllerNS: "kafka"}
	redis := FluxManagementInfo{IsManaged: true, ManagedBy: "kustomization", ControllerName: "redis-app", ControllerNS: "redis"}
	missing := FluxManagementInfo{IsManaged: true, ManagedBy: "kustomization", ControllerName: "gone", ControllerNS: "ghost"}

	tests := []struct {
		name     string
		user     authenticationv1.UserInfo
		fluxInfo FluxManagementInfo
		want     bool
	}{
		{"core controller short-circuits", fluxControllerUserInfo, kafka, true},
		{"impersonated SA matches owner serviceAccountName", saUser("system:serviceaccount:kafka:flux-reconciler"), kafka, true},
		{"other SA in tenant namespace rejected (owner SA known)", saUser("system:serviceaccount:kafka:evil"), kafka, false},
		{"matching SA name but wrong namespace rejected", saUser("system:serviceaccount:other:flux-reconciler"), kafka, false},
		{"owner unreadable, SA in static fallback list", saUser("system:serviceaccount:tenant:flux-reconciler"), missing, true},
		{"owner unreadable, SA not in fallback list", saUser("system:serviceaccount:tenant:random"), missing, false},
		{"owner without serviceAccountName falls back, fallback matches", saUser("system:serviceaccount:redis:flux-reconciler"), redis, true},
		{"owner without serviceAccountName falls back, no match", saUser("system:serviceaccount:redis:random"), redis, false},
		{"non service account rejected", authenticationv1.UserInfo{Username: "admin@example.com"}, kafka, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := admission.Request{}
			req.UserInfo = tt.user
			if got := handler.isOwningFluxReconciler(context.Background(), req, tt.fluxInfo, logr.Discard()); got != tt.want {
				t.Errorf("isOwningFluxReconciler() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- Namespace teardown cascade deletes (allowed_namespace_terminating) ---

// newHandlerWithNamespace returns a handler whose cache-backed client holds the
// named namespace, optionally with a deletionTimestamp (Terminating).
func newHandlerWithNamespace(t *testing.T, namespace string, terminating bool) *DriftPreventionHandler {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	if terminating {
		now := metav1.Now()
		ns.DeletionTimestamp = &now
		ns.Finalizers = []string{"kubernetes"} // fake client rejects a deletionTimestamp without finalizers
	}

	h := newTestHandler()
	h.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns).Build()
	return h
}

func TestHandle_DeleteDuringNamespaceTeardownAllowed(t *testing.T) {
	// The kube namespace-controller cascade-deletes a Flux-applied child
	// (kustomize-controller fieldManager) that has no own deletionTimestamp. The
	// parent namespace is Terminating -> allowed, so teardown cannot wedge.
	handler := newHandlerWithNamespace(t, "default", true)
	oldRaw := buildTestJSON(fluxKustomizeLabels, nil, fluxKustomizeManagedFields(), nil)
	req := createAdmissionRequest(admissionv1.Delete, runtime.RawExtension{Raw: oldRaw}, "default", "test-pod")
	req.OldObject = runtime.RawExtension{Raw: oldRaw}
	req.Object = runtime.RawExtension{}
	req.UserInfo = authenticationv1.UserInfo{
		Username: "system:serviceaccount:kube-system:namespace-controller",
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:kube-system"},
	}

	resp := handler.Handle(context.Background(), req)

	if !resp.Allowed {
		t.Errorf("expected allowed for DELETE during namespace teardown, got allowed=%v", resp.Allowed)
	}
	if !strings.Contains(resp.Result.Message, "terminating") {
		t.Errorf("expected namespace-terminating reason, got: %s", resp.Result.Message)
	}
}

func TestHandle_DeleteFluxManagedActiveNamespaceDenied(t *testing.T) {
	// Same Flux-applied child, but the namespace is Active: the teardown bypass
	// must NOT fire and deletion stays denied.
	handler := newHandlerWithNamespace(t, "default", false)
	oldRaw := buildTestJSON(fluxKustomizeLabels, nil, fluxKustomizeManagedFields(), nil)
	req := createAdmissionRequest(admissionv1.Delete, runtime.RawExtension{Raw: oldRaw}, "default", "test-pod")
	req.OldObject = runtime.RawExtension{Raw: oldRaw}
	req.Object = runtime.RawExtension{}
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if resp.Allowed {
		t.Error("expected denied for DELETE of Flux-managed resource in an active namespace")
	}
}

func TestNamespaceIsTerminating(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	now := metav1.Now()
	terminating := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "dying", DeletionTimestamp: &now, Finalizers: []string{"kubernetes"},
	}}
	active := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "live"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(terminating, active).Build()
	handler := &DriftPreventionHandler{Log: logr.Discard(), Client: c}

	tests := []struct {
		name      string
		namespace string
		want      bool
	}{
		{"terminating namespace", "dying", true},
		{"active namespace", "live", false},
		{"cluster-scoped (empty namespace)", "", false},
		{"unreadable namespace fails closed", "ghost", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := handler.namespaceIsTerminating(context.Background(), tt.namespace, logr.Discard()); got != tt.want {
				t.Errorf("namespaceIsTerminating(%q) = %v, want %v", tt.namespace, got, tt.want)
			}
		})
	}
}

// --- Inventory double-condition for CREATE (allowed_not_in_owner_inventory) ---

var kustomizationGVK = schema.GroupVersionKind{
	Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization",
}

// newHandlerWithOwnerInventory returns a handler whose cache-backed client holds
// the owning Kustomization flux-system/my-app (matching fluxKustomizeLabels) with
// the given inventory entry ids in .status.inventory.
func newHandlerWithOwnerInventory(t *testing.T, entryIDs ...string) *DriftPreventionHandler {
	t.Helper()
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(kustomizationGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(kustomizationGVK.GroupVersion().WithKind("KustomizationList"), &unstructured.UnstructuredList{})

	owner := &unstructured.Unstructured{}
	owner.SetGroupVersionKind(kustomizationGVK)
	owner.SetNamespace("flux-system")
	owner.SetName("my-app")
	entries := make([]interface{}, 0, len(entryIDs))
	for _, id := range entryIDs {
		entries = append(entries, map[string]interface{}{"id": id, "v": "v1"})
	}
	_ = unstructured.SetNestedSlice(owner.Object, entries, "status", "inventory", "entries")

	h := newTestHandlerWithSystemSAs()
	h.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner).Build()
	return h
}

func vmOperatorUser() authenticationv1.UserInfo {
	return authenticationv1.UserInfo{
		Username: "system:serviceaccount:vm-system:victoria-metrics-operator",
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:vm-system"},
	}
}

func TestHandle_CreateNotInOwnerInventoryAllowed(t *testing.T) {
	// Operator-derived object (e.g. a VMServiceScrape generated from a Flux-applied
	// ServiceMonitor): inherited Flux labels, no ownerReference, created by an
	// operator SA, ABSENT from the owner inventory -> allowed.
	handler := newHandlerWithOwnerInventory(t,
		"default_some-monitor_monitoring.coreos.com_ServiceMonitor")
	obj := createTestObject(fluxKustomizeLabels, nil)
	req := createAdmissionRequest(admissionv1.Create, obj, "default", "test-pod")
	req.UserInfo = vmOperatorUser()

	resp := handler.Handle(context.Background(), req)

	if !resp.Allowed {
		t.Errorf("expected allowed for CREATE absent from owner inventory, got allowed=%v", resp.Allowed)
	}
	if !strings.Contains(resp.Result.Message, "inventory") {
		t.Errorf("expected not-in-inventory reason, got: %s", resp.Result.Message)
	}
}

func TestHandle_CreateInOwnerInventoryDenied(t *testing.T) {
	// A non-Flux actor creating an object whose id IS in the owner inventory (a
	// genuinely Flux-declared object) stays denied: squat protection retained.
	handler := newHandlerWithOwnerInventory(t, "default_test-pod__Pod")
	obj := createTestObject(fluxKustomizeLabels, nil)
	req := createAdmissionRequest(admissionv1.Create, obj, "default", "test-pod")
	req.UserInfo = vmOperatorUser()

	resp := handler.Handle(context.Background(), req)

	if resp.Allowed {
		t.Error("expected denied for CREATE of object present in owner inventory by non-Flux SA")
	}
}

func TestHandle_CreateEmptyOwnerInventoryDenied(t *testing.T) {
	// Owner is readable but its inventory is empty (e.g. not yet reconciled):
	// "available" is false, so the deny is preserved (no derived-object allow).
	handler := newHandlerWithOwnerInventory(t)
	obj := createTestObject(fluxKustomizeLabels, nil)
	req := createAdmissionRequest(admissionv1.Create, obj, "default", "test-pod")
	req.UserInfo = vmOperatorUser()

	resp := handler.Handle(context.Background(), req)

	if resp.Allowed {
		t.Error("expected denied for CREATE when owner inventory is empty/unpopulated")
	}
}

func TestHandle_CreateRBACColonNameInInventoryDenied(t *testing.T) {
	// RBAC names may contain colons; Flux transcodes ":" -> "__" in the inventory
	// id. A non-Flux CREATE of a ClusterRole named "system:foo" that IS declared by
	// Flux must still be denied — the id builder must transcode to match.
	handler := newHandlerWithOwnerInventory(t, "_system__foo_rbac.authorization.k8s.io_ClusterRole")
	obj := map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "ClusterRole",
		"metadata":   map[string]interface{}{"name": "system:foo", "labels": fluxKustomizeLabels},
	}
	raw, _ := json.Marshal(obj)
	req := createAdmissionRequest(admissionv1.Create, runtime.RawExtension{Raw: raw}, "", "system:foo")
	req.Kind = metav1.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}
	req.UserInfo = vmOperatorUser()

	resp := handler.Handle(context.Background(), req)

	if resp.Allowed {
		t.Error("expected denied for CREATE of colon-named RBAC object present in owner inventory")
	}
}

func TestInventoryID(t *testing.T) {
	tests := []struct {
		name, group, kind, namespace, objName, want string
	}{
		{"core group is empty segment", "", "Service", "flux-system", "my-svc", "flux-system_my-svc__Service"},
		{"apps group", "apps", "Deployment", "default", "web", "default_web_apps_Deployment"},
		{"custom CRD group", "operator.victoriametrics.com", "VMServiceScrape", "mon", "scrape",
			"mon_scrape_operator.victoriametrics.com_VMServiceScrape"},
		{"cluster-scoped empty namespace", "", "Namespace", "", "prod", "_prod__Namespace"},
		{"rbac colon name transcoded", "rbac.authorization.k8s.io", "ClusterRole", "", "system:foo",
			"_system__foo_rbac.authorization.k8s.io_ClusterRole"},
		{"non-rbac colon name not transcoded", "", "ConfigMap", "default", "a:b", "default_a:b__ConfigMap"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := inventoryID(tt.group, tt.kind, tt.namespace, tt.objName); got != tt.want {
				t.Errorf("inventoryID() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- UPDATE protection against real drift (old-object set, keyed lists) ---

// deploymentJSON builds an apps/v1 Deployment with one container, optional
// Flux labels and managedFields — shaped like real apiserver output.
func deploymentJSON(labels map[string]string, managedFields []metav1.ManagedFieldsEntry, image string, replicas int64) []byte {
	metadata := map[string]interface{}{
		"name":      "web",
		"namespace": "default",
	}
	if labels != nil {
		metadata["labels"] = labels
	}
	if managedFields != nil {
		mfJSON, _ := json.Marshal(managedFields)
		var mf []interface{}
		_ = json.Unmarshal(mfJSON, &mf)
		metadata["managedFields"] = mf
	}
	obj := map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   metadata,
		"spec": map[string]interface{}{
			"replicas": replicas,
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{"name": "app", "image": image},
					},
				},
			},
		},
	}
	raw, _ := json.Marshal(obj)
	return raw
}

// realisticDeploymentManagedFields mirrors what the apiserver records for a
// kustomize-controller SSA apply of the Deployment above: keyed-list members
// (k:{"name":"app"}) nested under f:containers.
func realisticDeploymentManagedFields() []metav1.ManagedFieldsEntry {
	return []metav1.ManagedFieldsEntry{{
		Manager:   "kustomize-controller",
		Operation: metav1.ManagedFieldsOperationApply,
		FieldsV1: &metav1.FieldsV1{Raw: []byte(`{
			"f:metadata":{"f:labels":{"f:kustomize.toolkit.fluxcd.io/name":{},"f:kustomize.toolkit.fluxcd.io/namespace":{}}},
			"f:spec":{
				"f:template":{
					"f:spec":{
						"f:containers":{
							"k:{\"name\":\"app\"}":{".":{},"f:name":{},"f:image":{}}
						}
					}
				}
			}
		}`)},
	}}
}

func deploymentUpdateRequest(oldRaw, newRaw []byte) admission.Request {
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "test-uid",
			Operation: admissionv1.Update,
			Namespace: "default",
			Name:      "web",
			Kind:      metav1.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
			Object:    runtime.RawExtension{Raw: newRaw},
			OldObject: runtime.RawExtension{Raw: oldRaw},
		},
	}
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}
	return req
}

func TestHandle_UpdateContainerImageDenied(t *testing.T) {
	// kubectl set image on a Flux-managed Deployment. Two real-world subtleties:
	// (1) the apiserver transfers f:image ownership to kubectl BEFORE validating
	// admission, so the NEW object's Flux entry no longer lists the field — the
	// protected set must come from the OLD object; (2) the value diff records
	// the keyed list atomically (.containers), so only hierarchy-aware overlap
	// connects it to Flux's k:{"name":"app"} member.
	handler := newTestHandler()

	transferred := []metav1.ManagedFieldsEntry{{
		Manager:   "kustomize-controller",
		Operation: metav1.ManagedFieldsOperationApply,
		FieldsV1: &metav1.FieldsV1{Raw: []byte(`{
			"f:metadata":{"f:labels":{"f:kustomize.toolkit.fluxcd.io/name":{},"f:kustomize.toolkit.fluxcd.io/namespace":{}}},
			"f:spec":{
				"f:template":{
					"f:spec":{
						"f:containers":{
							"k:{\"name\":\"app\"}":{".":{},"f:name":{}}
						}
					}
				}
			}
		}`)},
	}}

	oldRaw := deploymentJSON(fluxKustomizeLabels, realisticDeploymentManagedFields(), "nginx:1.0", 1)
	newRaw := deploymentJSON(fluxKustomizeLabels, transferred, "nginx:2.0", 1)

	resp := handler.Handle(context.Background(), deploymentUpdateRequest(oldRaw, newRaw))

	if resp.Allowed {
		t.Error("expected denied for container image drift on Flux-managed Deployment")
	}
}

func TestHandle_UpdateReplicasRealisticAllowed(t *testing.T) {
	// HPA scenario with REALISTIC fieldsV1: Flux owns the containers keyed list
	// but not .spec.replicas — scaling must stay allowed.
	handler := newTestHandler()

	oldRaw := deploymentJSON(fluxKustomizeLabels, realisticDeploymentManagedFields(), "nginx:1.0", 1)
	newRaw := deploymentJSON(fluxKustomizeLabels, realisticDeploymentManagedFields(), "nginx:1.0", 5)

	resp := handler.Handle(context.Background(), deploymentUpdateRequest(oldRaw, newRaw))

	if !resp.Allowed {
		t.Errorf("expected allowed for replicas update (field not Flux-managed), got allowed=%v: %s",
			resp.Allowed, resp.Result.Message)
	}
}

func TestHandle_UpdateLabelStripDenied(t *testing.T) {
	// Single-request bypass attempt: strip the Flux labels AND drift the spec
	// in one PUT. The management gate must consider the OLD object's labels.
	handler := newTestHandler()

	oldRaw := deploymentJSON(fluxKustomizeLabels, realisticDeploymentManagedFields(), "nginx:1.0", 1)
	newRaw := deploymentJSON(nil, realisticDeploymentManagedFields(), "nginx:2.0", 1)

	resp := handler.Handle(context.Background(), deploymentUpdateRequest(oldRaw, newRaw))

	if resp.Allowed {
		t.Error("expected denied when Flux labels are stripped and spec drifted in one request")
	}
}

func TestHandle_UpdateManagedFieldsWipeDenied(t *testing.T) {
	// Tampering: no value changes, but the request wipes metadata.managedFields
	// (or SSA-rewrites the Flux entry), which would disarm the field check for
	// every later request.
	handler := newTestHandler()

	oldRaw := deploymentJSON(fluxKustomizeLabels, realisticDeploymentManagedFields(), "nginx:1.0", 1)
	newRaw := deploymentJSON(fluxKustomizeLabels, nil, "nginx:1.0", 1)

	resp := handler.Handle(context.Background(), deploymentUpdateRequest(oldRaw, newRaw))

	if resp.Allowed {
		t.Error("expected denied when Flux managedFields are released without a value change")
	}
	if !strings.Contains(resp.Result.Message, "release") {
		t.Errorf("expected field-release reason, got: %s", resp.Result.Message)
	}
}

// --- CREATE inventory veto (forged ownerReference / unavailable inventory) ---

func TestHandle_CreateForgedOwnerRefInInventoryDenied(t *testing.T) {
	// The API server does not validate ownerReferences on CREATE: a squatter can
	// attach a forged controller reference to an object whose id IS declared in
	// the owner inventory. The inventory veto must win over the ownerReference
	// heuristic.
	handler := newHandlerWithOwnerInventory(t, "default_test-pod__Pod")
	obj := createTestObjectWithOwner(fluxKustomizeLabels, nil, controllerOwnerRef())
	req := createAdmissionRequest(admissionv1.Create, obj, "default", "test-pod")
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if resp.Allowed {
		t.Error("expected denied for CREATE of inventory-declared object with forged controller ownerReference")
	}
	if !strings.Contains(resp.Result.Message, "inventory") {
		t.Errorf("expected inventory-squat reason, got: %s", resp.Result.Message)
	}
}

func TestHandle_CreateSystemControllerInInventoryDenied(t *testing.T) {
	// The veto also outranks the system-controller identity: a control-plane SA
	// never legitimately creates a Flux-declared object.
	handler := newHandlerWithOwnerInventory(t, "default_test-pod__Pod")
	handler.SystemControllerSAs = config.DefaultSystemControllerServiceAccounts()
	obj := createTestObject(fluxKustomizeLabels, nil)
	req := createAdmissionRequest(admissionv1.Create, obj, "default", "test-pod")
	req.UserInfo = endpointControllerUserInfo

	resp := handler.Handle(context.Background(), req)

	if resp.Allowed {
		t.Error("expected denied for CREATE of inventory-declared object by a system controller")
	}
}

func TestHandle_CreateInventoryUnavailableDistinctReason(t *testing.T) {
	// Owner unreadable (no client): the deny must carry the distinct
	// inventory-unavailable reason so rollouts can tell cache/RBAC trouble from
	// genuine squats.
	handler := newTestHandler()
	obj := createTestObject(fluxKustomizeLabels, nil)
	req := createAdmissionRequest(admissionv1.Create, obj, "default", "test-pod")
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if resp.Allowed {
		t.Error("expected denied when the owner inventory cannot be read")
	}
	if !strings.Contains(resp.Result.Message, "cannot verify against the Flux inventory") {
		t.Errorf("expected inventory-unavailable reason, got: %s", resp.Result.Message)
	}
}

// --- Bypass annotation introduction (two-step bypass) ---

var bypassAnnotations = map[string]string{config.BypassAnnotation: config.BypassValue}

func TestHandle_UpdateAddBypassAnnotationDenied(t *testing.T) {
	// Two-step bypass: adding the annotation never overlaps the Flux field set
	// (Flux never applied that key), but the NEXT request would be waved
	// through by checkBypassAnnotation. Introducing it outside Git is denied.
	handler := newTestHandler()

	oldRaw := buildTestJSON(fluxKustomizeLabels, nil, fluxKustomizeManagedFields(), map[string]interface{}{"template": "same"})
	newRaw := buildTestJSON(fluxKustomizeLabels, bypassAnnotations, fluxKustomizeManagedFields(), map[string]interface{}{"template": "same"})

	req := createAdmissionRequest(admissionv1.Update, runtime.RawExtension{Raw: newRaw}, "default", "test-pod")
	req.OldObject = runtime.RawExtension{Raw: oldRaw}
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if resp.Allowed {
		t.Error("expected denied when a non-Flux UPDATE adds the bypass annotation")
	}
	if !strings.Contains(resp.Result.Message, "via Git") {
		t.Errorf("expected via-Git reason, got: %s", resp.Result.Message)
	}
}

func TestHandle_UpdateAddBypassAnnotationByFluxAllowed(t *testing.T) {
	// The legitimate path: Flux applies the annotation from Git and is allowed
	// as the owning reconciler before the annotation guard runs.
	handler := newTestHandler()

	oldRaw := buildTestJSON(fluxKustomizeLabels, nil, fluxKustomizeManagedFields(), nil)
	newRaw := buildTestJSON(fluxKustomizeLabels, bypassAnnotations, fluxKustomizeManagedFields(), nil)

	req := createAdmissionRequest(admissionv1.Update, runtime.RawExtension{Raw: newRaw}, "default", "test-pod")
	req.OldObject = runtime.RawExtension{Raw: oldRaw}
	req.UserInfo = fluxControllerUserInfo

	resp := handler.Handle(context.Background(), req)

	if !resp.Allowed {
		t.Errorf("expected allowed for Flux applying the bypass annotation, got allowed=%v", resp.Allowed)
	}
}

func TestHandle_UpdateAddBypassAnnotationInheritedAllowed(t *testing.T) {
	// An inherited-label object (no Flux fieldManager) is not drift-protected at
	// all; annotating it stays allowed (the guard must not fire there).
	handler := newTestHandler()

	oldRaw := buildTestJSON(fluxKustomizeLabels, nil, nil, nil)
	newRaw := buildTestJSON(fluxKustomizeLabels, bypassAnnotations, nil, nil)

	req := createAdmissionRequest(admissionv1.Update, runtime.RawExtension{Raw: newRaw}, "default", "test-pod")
	req.OldObject = runtime.RawExtension{Raw: oldRaw}
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if !resp.Allowed {
		t.Errorf("expected allowed for annotating a non-Flux-applied object, got allowed=%v", resp.Allowed)
	}
}

// --- Cluster-scoped objects (VWC Scope "*") ---

func TestShouldProcessNamespace_ClusterScoped(t *testing.T) {
	// Cluster-scoped requests carry an empty namespace. They are always in
	// scope and must NOT trigger a namespace lookup — the handler has no client
	// here, so any lookup attempt would panic.
	handler := newTestHandler()
	handler.NamespaceLabel = "drift-prevention"

	shouldProcess, err := handler.shouldProcessNamespace(context.Background(), "", logr.Discard())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !shouldProcess {
		t.Error("expected cluster-scoped requests to always be in scope")
	}
}

// clusterScopedDeleteRequest builds a DELETE admission request for a
// cluster-scoped object (empty request namespace), e.g. a ClusterRole.
func clusterScopedDeleteRequest(raw []byte, kind, name string) admission.Request {
	return admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "test-uid",
			Operation: admissionv1.Delete,
			Namespace: "",
			Name:      name,
			Kind:      metav1.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: kind},
			OldObject: runtime.RawExtension{Raw: raw},
		},
	}
}

func TestHandle_DeleteClusterScopedFluxManagedDenied(t *testing.T) {
	// A Flux-applied cluster-scoped object (ClusterRole): human deletion is
	// denied. Previously Scope: Namespaced left these entirely unprotected.
	handler := newTestHandler()

	obj := map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "ClusterRole",
		"metadata": map[string]interface{}{
			"name":   "flux-managed-role",
			"labels": fluxKustomizeLabels,
			"managedFields": []interface{}{map[string]interface{}{
				"manager":   "kustomize-controller",
				"operation": "Apply",
				"fieldsV1":  map[string]interface{}{"f:rules": map[string]interface{}{}},
			}},
		},
	}
	raw, _ := json.Marshal(obj)
	req := clusterScopedDeleteRequest(raw, "ClusterRole", "flux-managed-role")
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if resp.Allowed {
		t.Error("expected denied for human DELETE of Flux-applied cluster-scoped object")
	}
}

func TestHandle_DeleteClusterScopedByFluxAllowed(t *testing.T) {
	handler := newTestHandler()

	obj := map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "ClusterRole",
		"metadata": map[string]interface{}{
			"name":   "flux-managed-role",
			"labels": fluxKustomizeLabels,
		},
	}
	raw, _ := json.Marshal(obj)
	req := clusterScopedDeleteRequest(raw, "ClusterRole", "flux-managed-role")
	req.UserInfo = fluxControllerUserInfo

	resp := handler.Handle(context.Background(), req)

	if !resp.Allowed {
		t.Errorf("expected allowed for Flux controller DELETE of cluster-scoped object, got allowed=%v", resp.Allowed)
	}
}

func TestHandle_DeleteFluxManagedNamespaceDenied(t *testing.T) {
	// For Namespace objects the apiserver sets request.namespace to the
	// namespace's own name, so the namespaced code paths run: the terminating
	// lookup targets the namespace itself (Active here) and the deny holds.
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	liveNs := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "prober-e2e"}}

	handler := newTestHandler()
	handler.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(liveNs).Build()

	obj := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]interface{}{
			"name":   "prober-e2e",
			"labels": fluxKustomizeLabels,
			"managedFields": []interface{}{map[string]interface{}{
				"manager":   "kustomize-controller",
				"operation": "Apply",
				"fieldsV1":  map[string]interface{}{"f:metadata": map[string]interface{}{"f:labels": map[string]interface{}{}}},
			}},
		},
	}
	raw, _ := json.Marshal(obj)
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "test-uid",
			Operation: admissionv1.Delete,
			Namespace: "prober-e2e",
			Name:      "prober-e2e",
			Kind:      metav1.GroupVersionKind{Group: "", Version: "v1", Kind: "Namespace"},
			OldObject: runtime.RawExtension{Raw: raw},
		},
	}
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if resp.Allowed {
		t.Error("expected denied for human DELETE of a Flux-applied Namespace")
	}
}

// --- reconcile:disabled abandonment + CREATE annotation squat hole ---

var reconcileDisabledAnnotations = map[string]string{config.KustomizeReconcileAnnotation: config.ReconcileDisabledValue}

func TestHandle_CreateWithBypassAnnotationDenied(t *testing.T) {
	// Squat hole: including the bypass annotation in the CREATED object used to
	// be honoured (there is no old object proving Git provenance), defeating
	// the inventory veto. The annotation must never be honoured on CREATE.
	handler := newHandlerWithOwnerInventory(t, "default_test-pod__Pod")
	obj := createTestObject(fluxKustomizeLabels, map[string]string{config.BypassAnnotation: config.BypassValue})
	req := createAdmissionRequest(admissionv1.Create, obj, "default", "test-pod")
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if resp.Allowed {
		t.Error("expected denied for CREATE of inventory-declared object carrying the bypass annotation")
	}
}

func TestHandle_UpdateReconcileDisabledAllowed(t *testing.T) {
	// kustomize-controller skips reconcile:disabled objects entirely, so drift
	// prevention on them is incoherent. Annotation present on the OLD object
	// (Git-applied) -> manual update allowed.
	handler := newTestHandler()

	oldRaw := buildTestJSON(fluxKustomizeLabels, reconcileDisabledAnnotations, fluxKustomizeManagedFields(), map[string]interface{}{"template": "original"})
	newRaw := buildTestJSON(fluxKustomizeLabels, reconcileDisabledAnnotations, fluxKustomizeManagedFields(), map[string]interface{}{"template": "modified"})

	req := createAdmissionRequest(admissionv1.Update, runtime.RawExtension{Raw: newRaw}, "default", "test-pod")
	req.OldObject = runtime.RawExtension{Raw: oldRaw}
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if !resp.Allowed {
		t.Errorf("expected allowed for UPDATE of reconcile-disabled object, got allowed=%v: %s",
			resp.Allowed, resp.Result.Message)
	}
}

func TestHandle_DeleteReconcileDisabledAllowed(t *testing.T) {
	handler := newTestHandler()

	oldRaw := buildTestJSON(fluxKustomizeLabels, reconcileDisabledAnnotations, fluxKustomizeManagedFields(), nil)
	req := createAdmissionRequest(admissionv1.Delete, runtime.RawExtension{Raw: oldRaw}, "default", "test-pod")
	req.OldObject = runtime.RawExtension{Raw: oldRaw}
	req.Object = runtime.RawExtension{}
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if !resp.Allowed {
		t.Errorf("expected allowed for DELETE of reconcile-disabled object, got allowed=%v", resp.Allowed)
	}
}

func TestHandle_HelmReconcileDisabledNotHonoured(t *testing.T) {
	// The annotation is kustomize-controller-specific: helm-controller keeps
	// reconciling regardless, so HelmRelease-owned objects stay protected.
	handler := newTestHandler()

	helmManagedFields := []metav1.ManagedFieldsEntry{{
		Manager:   "helm-controller",
		Operation: metav1.ManagedFieldsOperationApply,
		FieldsV1:  &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:template":{}}}`)},
	}}
	oldRaw := buildTestJSON(fluxHelmLabels, reconcileDisabledAnnotations, helmManagedFields, nil)
	req := createAdmissionRequest(admissionv1.Delete, runtime.RawExtension{Raw: oldRaw}, "default", "test-pod")
	req.OldObject = runtime.RawExtension{Raw: oldRaw}
	req.Object = runtime.RawExtension{}
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if resp.Allowed {
		t.Error("expected denied: reconcile:disabled must not bypass HelmRelease-owned objects")
	}
}

func TestHandle_UpdateAddReconcileDisabledDenied(t *testing.T) {
	// Planting reconcile:disabled outside Git is the same two-step bypass as
	// the drift-prevention annotation; the generalised guard denies it.
	handler := newTestHandler()

	oldRaw := buildTestJSON(fluxKustomizeLabels, nil, fluxKustomizeManagedFields(), map[string]interface{}{"template": "same"})
	newRaw := buildTestJSON(fluxKustomizeLabels, reconcileDisabledAnnotations, fluxKustomizeManagedFields(), map[string]interface{}{"template": "same"})

	req := createAdmissionRequest(admissionv1.Update, runtime.RawExtension{Raw: newRaw}, "default", "test-pod")
	req.OldObject = runtime.RawExtension{Raw: oldRaw}
	req.UserInfo = authenticationv1.UserInfo{Username: "admin@example.com"}

	resp := handler.Handle(context.Background(), req)

	if resp.Allowed {
		t.Error("expected denied when a non-Flux UPDATE adds the reconcile:disabled annotation")
	}
	if !strings.Contains(resp.Result.Message, config.KustomizeReconcileAnnotation) {
		t.Errorf("expected the offending annotation in the message, got: %s", resp.Result.Message)
	}
}

// --- Non-SA control-plane identities ---

func TestHandle_DeleteByAPIServerCRDFinalizerAllowed(t *testing.T) {
	// When a Flux-pruned CRD is finalised, the apiserver's CRD cleanup deletes
	// every instance as the non-SA identity system:apiserver. Without the
	// full-username allow-list entry, Flux-applied instances would wedge the
	// CRD in Terminating under enforce mode.
	handler := newTestHandlerWithSystemSAs()

	oldRaw := buildTestJSON(fluxKustomizeLabels, nil, fluxKustomizeManagedFields(), nil)
	req := createAdmissionRequest(admissionv1.Delete, runtime.RawExtension{Raw: oldRaw}, "default", "test-pod")
	req.OldObject = runtime.RawExtension{Raw: oldRaw}
	req.Object = runtime.RawExtension{}
	req.UserInfo = authenticationv1.UserInfo{
		Username: "system:apiserver",
		Groups:   []string{"system:masters", "system:authenticated"},
	}

	resp := handler.Handle(context.Background(), req)

	if !resp.Allowed {
		t.Errorf("expected allowed for DELETE by system:apiserver (CRD finalizer), got allowed=%v", resp.Allowed)
	}
	if !strings.Contains(resp.Result.Message, "control-plane") {
		t.Errorf("expected system-controller reason, got: %s", resp.Result.Message)
	}
}

// --- Kustomization .spec.ignore (DriftIgnoreRules) waiver on UPDATE ---

// ignoreEntry builds a .spec.ignore list entry (JSON-compatible for SetNestedSlice).
func ignoreEntry(target map[string]interface{}, paths ...string) map[string]interface{} {
	p := make([]interface{}, len(paths))
	for i, s := range paths {
		p[i] = s
	}
	entry := map[string]interface{}{"paths": p}
	if target != nil {
		entry["target"] = target
	}
	return entry
}

// newHandlerWithOwnerIgnore returns a handler whose cache-backed client holds the
// owning Kustomization flux-system/my-app (matching fluxKustomizeLabels) with the
// given .spec.ignore entries.
func newHandlerWithOwnerIgnore(t *testing.T, ignore ...interface{}) *DriftPreventionHandler {
	t.Helper()
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(kustomizationGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(kustomizationGVK.GroupVersion().WithKind("KustomizationList"), &unstructured.UnstructuredList{})

	owner := &unstructured.Unstructured{}
	owner.SetGroupVersionKind(kustomizationGVK)
	owner.SetNamespace("flux-system")
	owner.SetName("my-app")
	if len(ignore) > 0 {
		_ = unstructured.SetNestedSlice(owner.Object, ignore, "spec", "ignore")
	}

	h := newTestHandler()
	h.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner).Build()
	return h
}

// fluxManagedFieldsRaw builds a single kustomize-controller Apply entry with the
// given fieldsV1 body.
func fluxManagedFieldsRaw(fieldsV1 string) []metav1.ManagedFieldsEntry {
	return []metav1.ManagedFieldsEntry{{
		Manager:   "kustomize-controller",
		Operation: metav1.ManagedFieldsOperationApply,
		FieldsV1:  &metav1.FieldsV1{Raw: []byte(fieldsV1)},
	}}
}

func fluxManagedReplicasFields() []metav1.ManagedFieldsEntry {
	return fluxManagedFieldsRaw(`{"f:spec":{"f:replicas":{}}}`)
}

func TestHandle_UpdateIgnoredReplicasWaived(t *testing.T) {
	// Flux owns .spec.replicas and the Kustomization ignores it: a manual scale
	// must be allowed (Flux would not correct it either).
	handler := newHandlerWithOwnerIgnore(t, ignoreEntry(nil, "/spec/replicas"))
	oldRaw := deploymentJSON(fluxKustomizeLabels, fluxManagedReplicasFields(), "nginx:1.0", 1)
	newRaw := deploymentJSON(fluxKustomizeLabels, fluxManagedReplicasFields(), "nginx:1.0", 5)

	resp := handler.Handle(context.Background(), deploymentUpdateRequest(oldRaw, newRaw))

	if !resp.Allowed {
		t.Fatalf("expected allowed for ignored .spec.replicas edit, got: %s", resp.Result.Message)
	}
	if !strings.Contains(resp.Result.Message, "excluded from drift detection") {
		t.Errorf("expected drift-ignored reason, got: %s", resp.Result.Message)
	}
}

func TestHandle_UpdateIgnoredFieldOwnershipTransferWaived(t *testing.T) {
	// Realistic transfer: editing the ignored field moves it out of Flux's
	// managedFields on the new object. The tampering guard must not fire for a
	// field the Kustomization ignores.
	handler := newHandlerWithOwnerIgnore(t, ignoreEntry(nil, "/spec/replicas"))
	oldRaw := deploymentJSON(fluxKustomizeLabels, fluxManagedReplicasFields(), "nginx:1.0", 1)
	newRaw := deploymentJSON(fluxKustomizeLabels, fluxManagedFieldsRaw(`{}`), "nginx:1.0", 5)

	resp := handler.Handle(context.Background(), deploymentUpdateRequest(oldRaw, newRaw))

	if !resp.Allowed {
		t.Fatalf("expected allowed when the ignored field is transferred out of Flux ownership, got: %s",
			resp.Result.Message)
	}
}

func TestHandle_UpdateIgnoreCannotDisarmOtherFields(t *testing.T) {
	// Security: ignoring one field must not let a request release Flux ownership of
	// OTHER fields. Flux owns replicas + a label; only replicas is ignored; the
	// request edits replicas and wipes all managedFields -> the non-ignored label
	// loss is still caught as tampering.
	handler := newHandlerWithOwnerIgnore(t, ignoreEntry(nil, "/spec/replicas"))
	oldRaw := deploymentJSON(fluxKustomizeLabels,
		fluxManagedFieldsRaw(`{"f:spec":{"f:replicas":{}},"f:metadata":{"f:labels":{"f:app":{}}}}`), "nginx:1.0", 1)
	newRaw := deploymentJSON(fluxKustomizeLabels, nil, "nginx:1.0", 5)

	resp := handler.Handle(context.Background(), deploymentUpdateRequest(oldRaw, newRaw))

	if resp.Allowed {
		t.Error("expected denied: ignoring replicas must not waive release of other Flux-managed fields")
	}
	if !strings.Contains(resp.Result.Message, "release") {
		t.Errorf("expected managedFields-tampering reason, got: %s", resp.Result.Message)
	}
}

func TestHandle_UpdateIgnoreTargetMismatchDenied(t *testing.T) {
	// The ignore rule targets StatefulSet; the object is a Deployment -> no waiver.
	handler := newHandlerWithOwnerIgnore(t,
		ignoreEntry(map[string]interface{}{"kind": "StatefulSet"}, "/spec/replicas"))
	oldRaw := deploymentJSON(fluxKustomizeLabels, fluxManagedReplicasFields(), "nginx:1.0", 1)
	newRaw := deploymentJSON(fluxKustomizeLabels, fluxManagedReplicasFields(), "nginx:1.0", 5)

	resp := handler.Handle(context.Background(), deploymentUpdateRequest(oldRaw, newRaw))

	if resp.Allowed {
		t.Error("expected denied: ignore rule target does not match the object kind")
	}
}

func TestHandle_UpdateIgnoreLabelSelectorWaived(t *testing.T) {
	// Ignore rule scoped by labelSelector that matches the object -> waived.
	handler := newHandlerWithOwnerIgnore(t, ignoreEntry(
		map[string]interface{}{"labelSelector": config.KustomizeLabelName + "=my-app"}, "/spec/replicas"))
	oldRaw := deploymentJSON(fluxKustomizeLabels, fluxManagedReplicasFields(), "nginx:1.0", 1)
	newRaw := deploymentJSON(fluxKustomizeLabels, fluxManagedReplicasFields(), "nginx:1.0", 5)

	resp := handler.Handle(context.Background(), deploymentUpdateRequest(oldRaw, newRaw))

	if !resp.Allowed {
		t.Fatalf("expected allowed for labelSelector-matched ignore rule, got: %s", resp.Result.Message)
	}
}

func TestHandle_UpdateIgnoreOwnerUnreadableDenied(t *testing.T) {
	// No client -> owner unreadable -> fail closed (no waiver), conflict stands.
	handler := newTestHandler()
	oldRaw := deploymentJSON(fluxKustomizeLabels, fluxManagedReplicasFields(), "nginx:1.0", 1)
	newRaw := deploymentJSON(fluxKustomizeLabels, fluxManagedReplicasFields(), "nginx:1.0", 5)

	resp := handler.Handle(context.Background(), deploymentUpdateRequest(oldRaw, newRaw))

	if resp.Allowed {
		t.Error("expected denied when the owning Kustomization cannot be read (fail closed)")
	}
}

func TestHandle_UpdateIgnoreHelmOwnerDenied(t *testing.T) {
	// HelmRelease has no .spec.ignore; a Helm-owned object is never waived.
	handler := newTestHandler()
	oldRaw := deploymentJSON(fluxHelmLabels,
		[]metav1.ManagedFieldsEntry{{
			Manager:   "helm-controller",
			Operation: metav1.ManagedFieldsOperationApply,
			FieldsV1:  &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:replicas":{}}}`)},
		}}, "nginx:1.0", 1)
	newRaw := deploymentJSON(fluxHelmLabels,
		[]metav1.ManagedFieldsEntry{{
			Manager:   "helm-controller",
			Operation: metav1.ManagedFieldsOperationApply,
			FieldsV1:  &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:replicas":{}}}`)},
		}}, "nginx:1.0", 5)

	resp := handler.Handle(context.Background(), deploymentUpdateRequest(oldRaw, newRaw))

	if resp.Allowed {
		t.Error("expected denied: HelmRelease-owned objects have no .spec.ignore waiver")
	}
}

func TestHandle_UpdateIgnoreListPathWaived(t *testing.T) {
	// Ignoring the container list path waives a container-image drift (the value
	// diff collapses the keyed list to .../containers, an ancestor of the ignore).
	handler := newHandlerWithOwnerIgnore(t, ignoreEntry(nil, "/spec/template/spec/containers"))
	transferred := fluxManagedFieldsRaw(`{
		"f:metadata":{"f:labels":{"f:kustomize.toolkit.fluxcd.io/name":{},"f:kustomize.toolkit.fluxcd.io/namespace":{}}},
		"f:spec":{"f:template":{"f:spec":{"f:containers":{"k:{\"name\":\"app\"}":{".":{},"f:name":{}}}}}}
	}`)
	oldRaw := deploymentJSON(fluxKustomizeLabels, realisticDeploymentManagedFields(), "nginx:1.0", 1)
	newRaw := deploymentJSON(fluxKustomizeLabels, transferred, "nginx:2.0", 1)

	resp := handler.Handle(context.Background(), deploymentUpdateRequest(oldRaw, newRaw))

	if !resp.Allowed {
		t.Fatalf("expected allowed when the container list path is ignored, got: %s", resp.Result.Message)
	}
}

func TestHandle_UpdateIgnoreIndexPathDenied(t *testing.T) {
	// Documented limitation: an ignore path that dives into an array index cannot
	// match the collapsed list conflict path, so it does NOT waive.
	handler := newHandlerWithOwnerIgnore(t, ignoreEntry(nil, "/spec/template/spec/containers/0/image"))
	transferred := fluxManagedFieldsRaw(`{
		"f:metadata":{"f:labels":{"f:kustomize.toolkit.fluxcd.io/name":{},"f:kustomize.toolkit.fluxcd.io/namespace":{}}},
		"f:spec":{"f:template":{"f:spec":{"f:containers":{"k:{\"name\":\"app\"}":{".":{},"f:name":{}}}}}}
	}`)
	oldRaw := deploymentJSON(fluxKustomizeLabels, realisticDeploymentManagedFields(), "nginx:1.0", 1)
	newRaw := deploymentJSON(fluxKustomizeLabels, transferred, "nginx:2.0", 1)

	resp := handler.Handle(context.Background(), deploymentUpdateRequest(oldRaw, newRaw))

	if resp.Allowed {
		t.Error("expected denied: array-index ignore path cannot match the collapsed list conflict")
	}
}

// TestCachedObjectTypes pins the pre-warm list to what the handler actually
// reads through the cache. A type dropped here is created lazily by the first
// admission request that needs it, which pays the list+watch inline while the
// fail-closed checks that depend on it deny legitimate traffic.
func TestCachedObjectTypes(t *testing.T) {
	want := []schema.GroupVersionKind{
		{Group: "", Version: "v1", Kind: "Namespace"},
		{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization"},
		{Group: "helm.toolkit.fluxcd.io", Version: "v2", Kind: "HelmRelease"},
	}

	objs := CachedObjectTypes()
	if len(objs) != len(want) {
		t.Fatalf("CachedObjectTypes() returned %d types, want %d", len(objs), len(want))
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	for i, obj := range objs {
		gvk, err := apiutil.GVKForObject(obj, scheme)
		if err != nil {
			t.Fatalf("GVKForObject(%T): %v", obj, err)
		}
		if gvk != want[i] {
			t.Errorf("CachedObjectTypes()[%d] = %v, want %v", i, gvk, want[i])
		}
	}
}

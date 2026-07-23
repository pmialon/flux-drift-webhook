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
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/go-logr/logr"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"

	"github.com/pmialon/flux-drift-webhook/internal/config"
	"github.com/pmialon/flux-drift-webhook/internal/metrics"
)

// DefaultNamespaceFetchTimeout is the default timeout for namespace label
// lookups used by the optional namespace filter.
const DefaultNamespaceFetchTimeout = 2 * time.Second

// DriftPreventionHandler is the admission webhook handler that prevents manual
// drift on Flux-managed resources.
type DriftPreventionHandler struct {
	Log                   logr.Logger
	FluxNamespace         string
	AuditOnly             bool
	Metrics               *metrics.Metrics
	NamespaceLabel        string        // Optional: filter by namespace label key
	NamespaceLabelValue   string        // Optional: filter by namespace label value
	NamespaceFetchTimeout time.Duration // Timeout for namespace label lookups (default: 2s)
	Client                client.Client
	// SystemControllerSAs is the effective allow-list of "namespace:name"
	// control-plane service accounts permitted to CREATE resources that merely
	// inherit a parent's Flux labels (e.g. the endpoints/endpointslice
	// controllers) and to DELETE Flux-applied resources as part of normal
	// cluster lifecycle (garbage collection, Job TTL/CronJob cleanup). See
	// IsSystemController.
	SystemControllerSAs []string
}

// Handle evaluates an admission request and allows or denies it according to
// the drift-prevention rules. It implements the controller-runtime
// admission.Handler interface.
func (h *DriftPreventionHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	log := h.Log.WithValues(
		"operation", req.Operation,
		"namespace", req.Namespace,
		"name", req.Name,
		"kind", req.Kind.Kind,
		"user", req.UserInfo.Username,
	)

	// Sub-resource requests (status, scale, ...) do not carry the parent object
	// required for drift evaluation, and our VWC rules (Resources: ["*"]) do not
	// select them. Allow defensively in case the rules ever change.
	if req.SubResource != "" {
		h.Metrics.RecordRequest(string(req.Operation), "allowed_subresource")
		return admission.Allowed("subresource not subject to drift prevention")
	}

	timer := h.Metrics.StartTimer(string(req.Operation))
	defer timer.ObserveDuration()

	if shouldSkip, resp := h.checkNamespaceScope(ctx, req, log); shouldSkip {
		return resp
	}

	objMeta, oldObjMeta, err := h.extractRequestMetadata(req)
	if err != nil {
		log.Error(err, "failed to extract metadata")
		return admission.Denied("failed to extract metadata from request")
	}

	fluxInfo := GetFluxManagementInfo(objMeta.Labels)
	if !fluxInfo.IsManaged && req.Operation == admissionv1.Update {
		// A single UPDATE may strip the Flux labels and drift the spec in one
		// request; the management gate must also consider the pre-request labels.
		fluxInfo = GetFluxManagementInfo(oldObjMeta.Labels)
	}
	if !fluxInfo.IsManaged {
		h.Metrics.RecordRequest(string(req.Operation), "allowed_not_managed")
		return admission.Allowed("not managed by Flux")
	}

	log = log.WithValues("managedBy", fluxInfo.ManagedBy, "controller", fluxInfo.ControllerName)
	decision := h.checkBypass(ctx, req, objMeta, oldObjMeta, fluxInfo, log)

	return h.buildResponse(req, decision, log)
}

func (h *DriftPreventionHandler) checkNamespaceScope(
	ctx context.Context, req admission.Request, log logr.Logger,
) (skip bool, resp admission.Response) {
	shouldProcess, err := h.shouldProcessNamespace(ctx, req.Namespace, log)
	if shouldProcess {
		return false, admission.Response{}
	}
	if err != nil {
		log.V(1).Info("namespace fetch error, allowing request", "error", err)
	}
	h.Metrics.RecordRequest(string(req.Operation), "allowed_namespace_filter")
	return true, admission.Allowed("namespace not in webhook scope")
}

func (h *DriftPreventionHandler) extractRequestMetadata(
	req admission.Request,
) (metav1.ObjectMeta, metav1.ObjectMeta, error) {
	var objMeta, oldObjMeta metav1.ObjectMeta

	if req.Operation == admissionv1.Delete {
		return objMeta, oldObjMeta, h.extractMetadata(req.OldObject.Raw, &objMeta)
	}

	if err := h.extractMetadata(req.Object.Raw, &objMeta); err != nil {
		return objMeta, oldObjMeta, err
	}

	if req.Operation == admissionv1.Update {
		return objMeta, oldObjMeta, h.extractMetadata(req.OldObject.Raw, &oldObjMeta)
	}

	return objMeta, oldObjMeta, nil
}

func (h *DriftPreventionHandler) buildResponse(
	req admission.Request, decision decisionResult, log logr.Logger,
) admission.Response {
	h.Metrics.RecordRequest(string(req.Operation), decision.reason)
	if !decision.allowed {
		h.Metrics.RecordDenial(string(req.Operation), req.Kind.Kind)
	}

	if h.AuditOnly && !decision.allowed {
		log.Info("AUDIT: would deny request", "reason", decision.reason, "message", decision.message)
		resp := admission.Allowed(fmt.Sprintf("audit-only: %s", decision.message))
		resp.Warnings = []string{
			"⚠️  AUDIT MODE: This operation would be BLOCKED in enforce mode",
			decision.message,
		}
		return resp
	}

	if decision.allowed {
		log.V(1).Info("allowing request", "reason", decision.reason)
		return admission.Allowed(decision.message)
	}

	log.Info("denying request", "reason", decision.reason)
	resp := admission.Denied(decision.message)
	resp.Warnings = []string{
		"This operation is blocked by Flux drift prevention",
		"To fix: modify the resource in Git; for manual intervention, apply the " +
			"fluxcd.io/drift-prevention-bypass: disabled annotation via Git first",
	}
	return resp
}

// Shared decision reason/message for managedFields extraction failures
// (fail-closed), used by the DELETE and UPDATE paths.
const (
	reasonManagedFieldsError  = "denied_managed_fields_error"
	messageManagedFieldsError = "failed to extract managed fields from Flux-managed resource"
)

type decisionResult struct {
	allowed bool
	reason  string
	message string
}

func (h *DriftPreventionHandler) checkBypass(
	ctx context.Context,
	req admission.Request,
	objMeta metav1.ObjectMeta,
	oldObjMeta metav1.ObjectMeta,
	fluxInfo FluxManagementInfo,
	log logr.Logger,
) decisionResult {
	if h.isOwningFluxReconciler(ctx, req, fluxInfo, log) {
		return h.checkFluxOwnership(req, objMeta, oldObjMeta, fluxInfo)
	}

	if d, ok := h.checkBypassAnnotation(req, objMeta, oldObjMeta); ok {
		// Log at info level so bypass usage is auditable
		log.Info("bypass annotation used", "user", req.UserInfo.Username)
		return d
	}

	if d, ok := h.checkReconcileDisabled(req, objMeta, oldObjMeta, fluxInfo); ok {
		log.Info("reconcile-disabled annotation honoured", "user", req.UserInfo.Username)
		return d
	}

	if req.Operation == admissionv1.Delete {
		if IsBeingDeleted(objMeta) {
			return decisionResult{true, "allowed_deletion_in_progress", "resource already being deleted"}
		}
		// Cascade deletes during namespace teardown: the kube namespace-controller
		// issues a direct DELETE on each finalizer-free child, so the child never
		// carries its own deletionTimestamp. Blocking these cannot prevent drift —
		// the parent namespace is already gone — it only wedges the namespace in
		// Terminating. Allow once the parent namespace is itself being deleted.
		if h.namespaceIsTerminating(ctx, req.Namespace, log) {
			return decisionResult{true, "allowed_namespace_terminating",
				"namespace is terminating; cascade deletion allowed"}
		}
	}

	return h.operationDecision(ctx, req, objMeta, oldObjMeta, fluxInfo, log)
}

func (h *DriftPreventionHandler) checkFluxOwnership(
	req admission.Request,
	objMeta, oldObjMeta metav1.ObjectMeta,
	fluxInfo FluxManagementInfo,
) decisionResult {
	if !isOwningController(req.Operation, objMeta.Labels, oldObjMeta.Labels) {
		// Dual/multiple ownership: the Flux owner labels flipped between two
		// reconcilers. Record the conflicting owner pair so the culprits are
		// directly identifiable in the metric.
		h.Metrics.RecordOwnershipConflict(
			req.Kind.Kind,
			fluxOwnerKey(GetFluxManagementInfo(oldObjMeta.Labels)),
			fluxOwnerKey(fluxInfo),
		)
		return decisionResult{
			allowed: false,
			reason:  "denied_wrong_flux_controller",
			message: fmt.Sprintf(
				"Flux controller mismatch: resource managed by %s/%s, but modification attempted by different controller",
				fluxInfo.ControllerNS, fluxInfo.ControllerName,
			),
		}
	}
	return decisionResult{true, "allowed_owning_flux_controller", "request from owning Flux controller"}
}

// fluxOwnerKey renders a Flux owner as "<namespace>/<name>" for metric labels,
// or "<none>" when the object carries no Flux owner labels.
func fluxOwnerKey(info FluxManagementInfo) string {
	if !info.IsManaged {
		return "<none>"
	}
	return info.ControllerNS + "/" + info.ControllerName
}

// isOwningFluxReconciler reports whether the request comes from the Flux
// controller (or impersonated reconciler service account) that owns the
// resource. In multi-tenant mode, Flux reconciles via a service account named
// in the owning Kustomization/HelmRelease's .spec.serviceAccountName.
func (h *DriftPreventionHandler) isOwningFluxReconciler(
	ctx context.Context, req admission.Request, fluxInfo FluxManagementInfo, log logr.Logger,
) bool {
	// Core controllers run as themselves in the Flux namespace (no impersonation).
	if IsFluxController(req.UserInfo, h.FluxNamespace) {
		return true
	}

	saNamespace, saName, ok := ParseServiceAccount(req.UserInfo)
	if !ok {
		return false
	}

	// Preferred: match the service account configured on the owning object.
	if ns, name, found := h.owningReconcilerSA(ctx, fluxInfo, log); found {
		return saNamespace == ns && saName == name
	}

	// Fallback when the owner cannot be read or sets no serviceAccountName:
	// recognise well-known reconciler SA names in any namespace.
	return slices.Contains(config.FluxReconcilerServiceAccounts(), saName)
}

// owningReconcilerSA reads .spec.serviceAccountName from the owning
// Kustomization/HelmRelease. The service account lives in the owner's namespace.
func (h *DriftPreventionHandler) owningReconcilerSA(
	ctx context.Context, fluxInfo FluxManagementInfo, log logr.Logger,
) (namespace, name string, found bool) {
	owner, ok := h.getOwner(ctx, fluxInfo, log)
	if !ok {
		return "", "", false
	}

	sa, _, _ := unstructured.NestedString(owner.Object, "spec", "serviceAccountName")
	if sa == "" {
		return "", "", false
	}
	return fluxInfo.ControllerNS, sa, true
}

// getOwner fetches the owning Kustomization/HelmRelease named by the Flux labels
// through the controller-runtime cache. The same object backs reconciler-SA
// resolution, inventory membership and .spec.ignore evaluation; cache-backed
// reads keep repeat fetches within a request cheap.
func (h *DriftPreventionHandler) getOwner(
	ctx context.Context, fluxInfo FluxManagementInfo, log logr.Logger,
) (*unstructured.Unstructured, bool) {
	gvk, ok := ownerGVK(fluxInfo.ManagedBy)
	if !ok || h.Client == nil || fluxInfo.ControllerName == "" || fluxInfo.ControllerNS == "" {
		return nil, false
	}

	fetchCtx, cancel := context.WithTimeout(ctx, cmp.Or(h.NamespaceFetchTimeout, DefaultNamespaceFetchTimeout))
	defer cancel()

	owner := &unstructured.Unstructured{}
	owner.SetGroupVersionKind(gvk)
	key := client.ObjectKey{Namespace: fluxInfo.ControllerNS, Name: fluxInfo.ControllerName}
	if err := h.Client.Get(fetchCtx, key, owner); err != nil {
		log.V(1).Info("could not read owning Flux object", "error", err)
		return nil, false
	}
	return owner, true
}

// ownerGVK maps a Flux management kind to the GVK of the owning object.
func ownerGVK(managedBy string) (schema.GroupVersionKind, bool) {
	switch managedBy {
	case ManagedByKustomization:
		return schema.GroupVersionKind{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization"}, true
	case ManagedByHelmRelease:
		return schema.GroupVersionKind{Group: "helm.toolkit.fluxcd.io", Version: "v2", Kind: "HelmRelease"}, true
	default:
		return schema.GroupVersionKind{}, false
	}
}

// CachedObjectTypes returns every object the handler reads through the
// controller-runtime cache: namespaces (namespaceIsTerminating and the
// --namespace-label filter) and the owning Kustomization/HelmRelease (getOwner,
// for multi-tenant service-account resolution, the CREATE inventory veto and
// the .spec.ignore waiver).
//
// The manager pre-warms these informers at startup. Kept next to ownerGVK so the
// pre-warm list cannot silently drift from what the handler actually reads —
// a type missing here is created lazily by the first request that needs it,
// which pays the list+watch inline and, since every cache-backed check is
// fail-closed, gets denied while it completes.
func CachedObjectTypes() []client.Object {
	objs := []client.Object{&corev1.Namespace{}}
	for _, managedBy := range []string{ManagedByKustomization, ManagedByHelmRelease} {
		gvk, ok := ownerGVK(managedBy)
		if !ok {
			continue
		}
		owner := &unstructured.Unstructured{}
		owner.SetGroupVersionKind(gvk)
		objs = append(objs, owner)
	}
	return objs
}

func (h *DriftPreventionHandler) checkBypassAnnotation(
	req admission.Request, objMeta, oldObjMeta metav1.ObjectMeta,
) (decisionResult, bool) {
	// Only the PRE-REQUEST state proves the annotation came through Git. On
	// CREATE there is no pre-request state, so the annotation is never
	// honoured — otherwise including it in the created object would defeat the
	// CREATE protection (inventory squat veto included). On UPDATE the old
	// object is checked to prevent single-step bypass attacks.
	if req.Operation == admissionv1.Create {
		return decisionResult{}, false
	}
	bypassMeta := objMeta
	if req.Operation == admissionv1.Update {
		bypassMeta = oldObjMeta
	}
	if HasBypassAnnotation(bypassMeta.Annotations) {
		return decisionResult{true, "allowed_bypass_annotation", "bypass annotation present"}, true
	}
	return decisionResult{}, false
}

// checkReconcileDisabled honours kustomize.toolkit.fluxcd.io/reconcile:
// disabled as a bypass for Kustomization-owned objects: kustomize-controller
// skips such objects entirely, so drift prevention on them is incoherent —
// Flux would neither correct nor reapply manual changes. The pre-request state
// is authoritative (same anti-single-step rule as the bypass annotation), and
// CREATE is never honoured. helm-controller has no per-object equivalent, so
// HelmRelease-owned objects are unaffected.
func (h *DriftPreventionHandler) checkReconcileDisabled(
	req admission.Request, objMeta, oldObjMeta metav1.ObjectMeta, fluxInfo FluxManagementInfo,
) (decisionResult, bool) {
	if fluxInfo.ManagedBy != ManagedByKustomization || req.Operation == admissionv1.Create {
		return decisionResult{}, false
	}
	meta := objMeta
	if req.Operation == admissionv1.Update {
		meta = oldObjMeta
	}
	if IsReconcileDisabled(meta.Annotations) {
		return decisionResult{true, "allowed_reconcile_disabled",
			"Flux reconciliation is disabled for this object (kustomize.toolkit.fluxcd.io/reconcile: disabled)"}, true
	}
	return decisionResult{}, false
}

func (h *DriftPreventionHandler) operationDecision(
	ctx context.Context,
	req admission.Request,
	objMeta metav1.ObjectMeta,
	oldObjMeta metav1.ObjectMeta,
	fluxInfo FluxManagementInfo,
	log logr.Logger,
) decisionResult {
	switch req.Operation {
	case admissionv1.Create:
		return h.checkDerivedResourceCreate(ctx, req, objMeta, fluxInfo, log)
	case admissionv1.Update:
		return h.checkUpdateFieldConflict(ctx, req, objMeta, oldObjMeta, fluxInfo, log)
	case admissionv1.Delete:
		return h.checkDeleteManaged(req, objMeta, fluxInfo, log)
	default:
		log.Info("unknown operation type received, allowing", "operation", req.Operation)
		return decisionResult{true, "allowed_unknown_operation", "unknown operation, allowing"}
	}
}

// checkDerivedResourceCreate decides CREATEs of resources that carry Flux
// labels but do not come from the owning Flux reconciler. The owner
// Kustomization/HelmRelease .status.inventory is consulted first and is
// authoritative when readable:
//   - id present => the object is genuinely Flux-declared; only Flux may
//     create it. Denied regardless of any ownerReference or requester
//     identity — the API server does not validate ownerReferences on CREATE,
//     so a forged controller reference must not enable squatting.
//   - id absent => Flux does not manage it; the labels are inherited from a
//     parent (e.g. an operator-generated VMServiceScrape derived from a
//     Flux-applied ServiceMonitor). Allowed.
//
// When the inventory cannot be read (owner missing, empty inventory, cache
// lag), two non-inheritable heuristics identify derived objects:
//  1. a controller ownerReference => derived/owned child (EndpointSlice,
//     CertificateRequest, any operator's owned children, incl. cert-manager).
//  2. the requester is a recognised Kubernetes control-plane controller =>
//     covers classic Endpoints, which carry NO ownerReference.
//
// Otherwise fail closed, with a reason distinct from the squat deny so
// enforce-mode rollouts can tell owner/cache trouble from real squats.
//
// managedFields cannot be used here: the API server populates them only after
// admission on CREATE.
func (h *DriftPreventionHandler) checkDerivedResourceCreate(
	ctx context.Context, req admission.Request, objMeta metav1.ObjectMeta,
	fluxInfo FluxManagementInfo, log logr.Logger,
) decisionResult {
	found, available := h.ownerInventoryContains(ctx, req, objMeta, fluxInfo, log)
	if available && found {
		return decisionResult{
			allowed: false,
			reason:  "denied_create_flux_labels",
			message: fmt.Sprintf(
				"cannot create resource declared in the Flux inventory of %s/%s",
				fluxInfo.ControllerNS, fluxInfo.ControllerName,
			),
		}
	}
	if metav1.GetControllerOf(&objMeta) != nil {
		return decisionResult{true, "allowed_owned_resource",
			"resource has a controller ownerReference; Flux labels are inherited from its parent"}
	}
	if IsSystemController(req.UserInfo, h.SystemControllerSAs) {
		return decisionResult{true, "allowed_system_controller",
			"request from a recognised Kubernetes control-plane controller"}
	}
	if available {
		return decisionResult{true, "allowed_not_in_owner_inventory",
			"object is not in the owning Flux inventory; Flux labels are inherited from a parent"}
	}
	return decisionResult{
		allowed: false,
		reason:  "denied_create_inventory_unavailable",
		message: fmt.Sprintf(
			"cannot verify against the Flux inventory of %s/%s (owner unreadable or not yet reconciled); resource carries Flux management labels",
			fluxInfo.ControllerNS, fluxInfo.ControllerName,
		),
	}
}

// ownerInventoryContains reports whether the object's cli-utils id appears in the
// owning Kustomization/HelmRelease .status.inventory. The second return is true
// only when that inventory could actually be read (a non-empty entries list), so
// callers can distinguish "proven absent" from "could not determine".
func (h *DriftPreventionHandler) ownerInventoryContains(
	ctx context.Context, req admission.Request, objMeta metav1.ObjectMeta,
	fluxInfo FluxManagementInfo, log logr.Logger,
) (found, available bool) {
	owner, ok := h.getOwner(ctx, fluxInfo, log)
	if !ok {
		return false, false
	}

	entries, _, err := unstructured.NestedSlice(owner.Object, "status", "inventory", "entries")
	if err != nil || len(entries) == 0 {
		return false, false
	}

	id := inventoryID(
		req.Kind.Group, req.Kind.Kind,
		cmp.Or(objMeta.Namespace, req.Namespace), cmp.Or(objMeta.Name, req.Name),
	)
	for _, e := range entries {
		entry, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		if entryID, _ := entry["id"].(string); entryID == id {
			return true, true
		}
	}
	return false, true
}

// rbacKinds are the rbac.authorization.k8s.io kinds whose names may contain
// colons; Flux's cli-utils inventory id transcodes ":" -> "__" for them.
var rbacKinds = map[string]bool{
	"Role": true, "ClusterRole": true, "RoleBinding": true, "ClusterRoleBinding": true,
}

// inventoryID builds the cli-utils object id used in Flux .status.inventory
// entries: "<namespace>_<name>_<group>_<kind>". The group segment is empty for
// the core API group; the version is stored separately and is not part of the id.
// For rbac.authorization.k8s.io kinds, colons in the name are transcoded to "__"
// to match Flux's serialisation (cli-utils ObjMetadata.String).
func inventoryID(group, kind, namespace, name string) string {
	if group == "rbac.authorization.k8s.io" && rbacKinds[kind] {
		name = strings.ReplaceAll(name, ":", "__")
	}
	return fmt.Sprintf("%s_%s_%s_%s", namespace, name, group, kind)
}

// namespaceIsTerminating reports whether the given namespace currently has a
// deletionTimestamp (is being torn down). Cluster-scoped requests (empty
// namespace) are never terminating. Fails closed (false) when the namespace
// cannot be read, preserving the default deny.
func (h *DriftPreventionHandler) namespaceIsTerminating(ctx context.Context, namespace string, log logr.Logger) bool {
	if namespace == "" || h.Client == nil {
		return false
	}

	fetchCtx, cancel := context.WithTimeout(ctx, cmp.Or(h.NamespaceFetchTimeout, DefaultNamespaceFetchTimeout))
	defer cancel()

	ns := &corev1.Namespace{}
	if err := h.Client.Get(fetchCtx, client.ObjectKey{Name: namespace}, ns); err != nil {
		log.V(1).Info("could not read namespace for terminating check", "namespace", namespace, "error", err)
		return false
	}
	return ns.DeletionTimestamp != nil
}

// checkDeleteManaged denies a DELETE only when the object was actually applied
// by Flux, proven by a Flux fieldManager in .metadata.managedFields. Two
// deletions are nonetheless allowed:
//   - objects that merely inherit a Flux label
//     (Endpoints/EndpointSlice/CertificateRequest) carry no Flux fieldManager,
//     so their deletion is allowed (mirrors the UPDATE field-conflict logic and
//     Config Sync's "require real management evidence" approach);
//   - deletions by a recognised Kubernetes control-plane controller (garbage
//     collector cascade, completed-Job cleanup by the TTL-after-finished and
//     CronJob controllers) are legitimate lifecycle, not human drift.
func (h *DriftPreventionHandler) checkDeleteManaged(
	req admission.Request, objMeta metav1.ObjectMeta, fluxInfo FluxManagementInfo, log logr.Logger,
) decisionResult {
	fluxManagedFields, err := FluxManagedFields(objMeta.ManagedFields)
	if err != nil {
		log.Error(err, "failed to extract Flux managed fields")
		return decisionResult{false, reasonManagedFieldsError, messageManagedFieldsError}
	}

	if fluxManagedFields.Empty() {
		log.V(1).Info("no Flux-managed fields found, label is inherited; allowing delete")
		return decisionResult{true, "allowed_no_flux_managed_fields",
			"resource carries Flux labels by inheritance but was not applied by Flux; deletion allowed"}
	}

	// Recognised control-plane controllers (garbage collector, Job TTL/CronJob
	// cleanup) legitimately delete Flux-applied resources as part of normal
	// cluster lifecycle. Humans and tenants never carry these identities.
	if IsSystemController(req.UserInfo, h.SystemControllerSAs) {
		log.V(1).Info("delete by recognised control-plane controller; allowing", "user", req.UserInfo.Username)
		return decisionResult{true, "allowed_system_controller",
			"deletion by a recognised Kubernetes control-plane controller"}
	}

	return decisionResult{
		allowed: false,
		reason:  "denied_delete_flux_managed",
		message: fmt.Sprintf(
			"cannot delete Flux-managed resource (managed by %s/%s)",
			fluxInfo.ControllerNS, fluxInfo.ControllerName,
		),
	}
}

// checkUpdateFieldConflict allows updates to fields NOT managed by Flux
// (e.g. HPA updating .spec.replicas) while blocking changes to Flux-owned fields.
//
// The protected set MUST come from the OLD object: the API server transfers
// field ownership to the requester BEFORE validating admission runs, so on the
// new object the drifted fields are no longer attributed to Flux and the check
// would never fire.
func (h *DriftPreventionHandler) checkUpdateFieldConflict(
	ctx context.Context,
	req admission.Request,
	objMeta metav1.ObjectMeta,
	oldObjMeta metav1.ObjectMeta,
	fluxInfo FluxManagementInfo,
	log logr.Logger,
) decisionResult {
	fluxManagedFields, err := FluxManagedFields(oldObjMeta.ManagedFields)
	if err != nil {
		log.Error(err, "failed to extract Flux managed fields")
		return decisionResult{false, reasonManagedFieldsError, messageManagedFieldsError}
	}

	if fluxManagedFields.Empty() {
		log.V(1).Info("no Flux-managed fields found in managedFields, allowing update")
		return decisionResult{true, "allowed_no_flux_managed_fields", "no Flux-managed fields detected"}
	}

	// Protection-disabling annotations (drift-prevention bypass, reconcile:
	// disabled) may only be introduced through Git: Flux applies them and the
	// request is then allowed as the owning reconciler before reaching this
	// check. A non-Flux UPDATE adding one never overlaps the Flux field set
	// (Flux never applied that key), yet it would disarm drift prevention for
	// every subsequent request — a two-step bypass.
	if key, added := protectionDisablingAnnotationAdded(oldObjMeta.Annotations, objMeta.Annotations); added {
		log.Info("protection-disabling annotation added by non-Flux requester",
			"annotation", key, "user", req.UserInfo.Username)
		return decisionResult{
			allowed: false,
			reason:  "denied_bypass_annotation_added",
			message: fmt.Sprintf(
				"the %s annotation must be applied via Git (managed by %s/%s), not added directly",
				key, fluxInfo.ControllerNS, fluxInfo.ControllerName,
			),
		}
	}

	modifiedFields, err := h.parseAndDiff(req, log)
	if err != nil {
		var fce fieldCheckError
		if errors.As(err, &fce) {
			return fce.result
		}
		return decisionResult{false, "denied_internal_error", "unexpected internal error"}
	}

	conflict := GetConflictingFields(modifiedFields, fluxManagedFields)

	// The owning Kustomization may exclude some fields from drift detection via
	// .spec.ignore (Flux DriftIgnoreRules); Flux neither corrects nor reapplies
	// those, so the webhook must not block them either. Resolve the ignore set
	// only when there is a conflict to waive, keeping the owner fetch off the
	// common no-conflict path. The same set is applied to the managedFields
	// tampering check below: a legitimate edit to an ignored field transfers that
	// field out of Flux's managedFields on the new object, which must not read as
	// tampering.
	ignoreSet := fieldpath.NewSet()
	if !conflict.Empty() {
		ignoreSet = h.driftIgnoreSet(ctx, req, oldObjMeta, fluxInfo, log)
	}

	if remaining := WaiveIgnoredConflicts(conflict, ignoreSet); !remaining.Empty() {
		log.Info("update conflicts with Flux-managed fields",
			"conflictingFields", remaining.String(), "user", req.UserInfo.Username)
		return decisionResult{
			allowed: false,
			reason:  "denied_update_flux_managed_fields",
			message: fmt.Sprintf("cannot modify Flux-managed fields (managed by %s/%s): %s",
				fluxInfo.ControllerNS, fluxInfo.ControllerName, remaining.String()),
		}
	}

	// No (non-ignored) value conflict, yet the Flux entry shrank between old and
	// new: managedFields tampering (wiping .metadata.managedFields, or SSA-applying
	// a reduced config under the kustomize-/helm-controller manager name).
	// Legitimate ownership transfers only accompany a value change caught above —
	// so any remaining shrinkage (beyond ignored fields) would silently disarm the
	// field check for every later request.
	newFluxFields, err := FluxManagedFields(objMeta.ManagedFields)
	if err != nil {
		log.Error(err, "failed to extract Flux managed fields from new object")
		return decisionResult{false, reasonManagedFieldsError, messageManagedFieldsError}
	}
	if removed := WaiveIgnoredConflicts(fluxManagedFields.Difference(newFluxFields), ignoreSet); !removed.Empty() {
		log.Info("update releases Flux-managed fields without a value change",
			"releasedFields", removed.String(), "user", req.UserInfo.Username)
		return decisionResult{
			allowed: false,
			reason:  "denied_managed_fields_tampered",
			message: fmt.Sprintf(
				"cannot release Flux field ownership (managed by %s/%s): %s",
				fluxInfo.ControllerNS, fluxInfo.ControllerName, removed.String()),
		}
	}

	if !conflict.Empty() {
		log.Info("update conflicts waived by Kustomization .spec.ignore",
			"waivedFields", conflict.String(), "user", req.UserInfo.Username)
		return decisionResult{true, reasonDriftIgnored,
			"modified fields are excluded from drift detection by the owning Kustomization's .spec.ignore"}
	}

	log.V(1).Info("update does not affect Flux-managed fields, allowing", "user", req.UserInfo.Username)
	return decisionResult{true, "allowed_no_field_conflict",
		"modified fields do not overlap with Flux-managed fields"}
}

// driftIgnoreSet returns the field paths the owning Kustomization excludes from
// drift detection via .spec.ignore (Flux DriftIgnoreRules) for this object, or an
// empty set when there are none. Only Kustomization owners carry .spec.ignore, so
// HelmRelease-owned objects always yield an empty set. Fails closed to an empty
// set (no waiver) when the owner cannot be read or a matching rule is malformed.
// The owner is fetched (cache-backed) only on the would-deny path, so the common
// no-conflict UPDATE is unaffected. Matching uses the OLD object's identity and
// metadata, consistent with the rest of the field check.
func (h *DriftPreventionHandler) driftIgnoreSet(
	ctx context.Context,
	req admission.Request,
	oldObjMeta metav1.ObjectMeta,
	fluxInfo FluxManagementInfo,
	log logr.Logger,
) *fieldpath.Set {
	if fluxInfo.ManagedBy != ManagedByKustomization {
		return fieldpath.NewSet()
	}
	owner, ok := h.getOwner(ctx, fluxInfo, log)
	if !ok {
		return fieldpath.NewSet()
	}
	rules := parseIgnoreRules(owner)
	if len(rules) == 0 {
		return fieldpath.NewSet()
	}

	gvk := schema.GroupVersionKind{Group: req.Kind.Group, Version: req.Kind.Version, Kind: req.Kind.Kind}
	ignoreSet, err := ignoreSetForObject(
		rules, gvk,
		cmp.Or(oldObjMeta.Name, req.Name), cmp.Or(oldObjMeta.Namespace, req.Namespace),
		oldObjMeta.Labels, oldObjMeta.Annotations,
	)
	if err != nil {
		log.V(1).Info("could not evaluate .spec.ignore rules; not waiving", "error", err)
		return fieldpath.NewSet()
	}
	return ignoreSet
}

type fieldCheckError struct {
	result decisionResult
}

func (e fieldCheckError) Error() string { return e.result.message }

// parseAndDiff parses old/new objects and computes the field diff.
// Fail-closed: denies on any parse failure.
func (h *DriftPreventionHandler) parseAndDiff(
	req admission.Request, log logr.Logger,
) (*fieldpath.Set, error) {
	oldObj := &unstructured.Unstructured{}
	if err := json.Unmarshal(req.OldObject.Raw, &oldObj.Object); err != nil {
		log.Error(err, "failed to parse old object for field diff")
		return nil, fieldCheckError{decisionResult{false, "denied_parse_error",
			"failed to parse old object for Flux-managed resource"}}
	}

	newObj := &unstructured.Unstructured{}
	if err := json.Unmarshal(req.Object.Raw, &newObj.Object); err != nil {
		log.Error(err, "failed to parse new object for field diff")
		return nil, fieldCheckError{decisionResult{false, "denied_parse_error",
			"failed to parse new object for Flux-managed resource"}}
	}

	modifiedFields, err := ComputeFieldDiff(oldObj, newObj)
	if err != nil {
		log.Error(err, "failed to compute field diff")
		return nil, fieldCheckError{decisionResult{false, "denied_diff_error",
			"failed to compute field diff for Flux-managed resource"}}
	}

	return modifiedFields, nil
}

// isOwningController returns false if Flux management labels changed between
// old and new objects, indicating an ownership conflict.
func isOwningController(operation admissionv1.Operation, newLabels, oldLabels map[string]string) bool {
	if operation != admissionv1.Update {
		return true
	}

	for _, key := range []string{
		config.KustomizeLabelName,
		config.KustomizeLabelNamespace,
		config.HelmLabelName,
		config.HelmLabelNamespace,
	} {
		if oldLabels[key] != newLabels[key] {
			return false
		}
	}

	return true
}

func (h *DriftPreventionHandler) extractMetadata(raw []byte, meta *metav1.ObjectMeta) error {
	var obj struct {
		metav1.ObjectMeta `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return fmt.Errorf("unmarshal metadata: %w", err)
	}
	*meta = obj.ObjectMeta
	return nil
}

func (h *DriftPreventionHandler) shouldProcessNamespace(ctx context.Context, namespace string, log logr.Logger) (bool, error) {
	// Cluster-scoped objects (empty namespace) are always in scope: the
	// namespace label filter is a namespaced-only concept and must not trigger
	// a lookup of a namespace named "".
	if namespace == "" {
		return true, nil
	}

	if slices.Contains(config.ExcludedNamespaces(), namespace) {
		return false, nil
	}

	if h.NamespaceLabel == "" {
		return true, nil
	}

	fetchCtx, cancel := context.WithTimeout(ctx, cmp.Or(h.NamespaceFetchTimeout, DefaultNamespaceFetchTimeout))
	defer cancel()

	ns := &corev1.Namespace{}
	if err := h.Client.Get(fetchCtx, client.ObjectKey{Name: namespace}, ns); err != nil {
		log.Error(err, "failed to get namespace", "namespace", namespace)
		return true, err
	}

	labelValue, hasLabel := ns.Labels[h.NamespaceLabel]
	if !hasLabel {
		return false, nil
	}

	if h.NamespaceLabelValue != "" && labelValue != h.NamespaceLabelValue {
		return false, nil
	}

	return true, nil
}

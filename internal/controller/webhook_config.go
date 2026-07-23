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

package controller

import (
	"context"
	"fmt"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	kuberecorder "k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/pmialon/flux-drift-webhook/internal/config"
	"github.com/pmialon/flux-drift-webhook/internal/metrics"
)

// WebhookConfigReconciler reconciles the ValidatingWebhookConfiguration that
// selects Flux-managed resources, applying it via server-side apply.
//
// The rules are static: one wildcard rule per webhook entry, with the API-group
// exclusions carried by CEL matchConditions. Reconciling is therefore only about
// re-asserting the configuration, not rebuilding it.
type WebhookConfigReconciler struct {
	client.Client
	// Metrics records configuration-update outcomes.
	Metrics *metrics.Metrics
	// EventRecorder emits Kubernetes Events for configuration-update outcomes.
	// It is optional; when nil, no Events are emitted.
	EventRecorder kuberecorder.EventRecorder
	// WebhookName is the name of the managed ValidatingWebhookConfiguration.
	// The two webhook entries are derived from it ("kustomize."/"helm." prefixes).
	WebhookName string
	// WebhookNamespace is the namespace of the backing webhook Service.
	WebhookNamespace string
	// WebhookService is the name of the backing webhook Service.
	WebhookService string
	// WebhookPath is the admission request path served by the webhook.
	WebhookPath string
	// ResyncInterval is the requeue interval between re-applies.
	ResyncInterval time.Duration
}

// Reconcile applies the ValidatingWebhookConfiguration via server-side apply
// and requeues after ResyncInterval.
func (r *WebhookConfigReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx).WithValues("webhook", r.WebhookName)

	ref := &admissionregistrationv1.ValidatingWebhookConfiguration{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admissionregistration.k8s.io/v1",
			Kind:       "ValidatingWebhookConfiguration",
		},
		ObjectMeta: metav1.ObjectMeta{Name: r.WebhookName},
	}

	rules := wildcardRules()

	// SSA merges by webhook entry name, so they must match the deployed manifest
	vwc := &admissionregistrationv1.ValidatingWebhookConfiguration{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admissionregistration.k8s.io/v1",
			Kind:       "ValidatingWebhookConfiguration",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: r.WebhookName,
		},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{
			r.buildWebhookEntry("kustomize."+r.WebhookName, config.KustomizeLabelName, rules),
			r.buildWebhookEntry("helm."+r.WebhookName, config.HelmLabelName, rules),
		},
	}

	// Server-side apply via the typed Client.Apply() (controller-runtime v0.24+).
	// The VWC is converted to an unstructured apply configuration; its TypeMeta
	// above carries the GVK required by SSA.
	uns, err := runtime.DefaultUnstructuredConverter.ToUnstructured(vwc)
	if err != nil {
		r.Metrics.RecordConfigUpdate("error")
		r.event(ref, corev1.EventTypeWarning, "ConfigUpdateFailed", "failed to convert ValidatingWebhookConfiguration: %v", err)
		log.Error(err, "failed to convert ValidatingWebhookConfiguration to unstructured")
		return ctrl.Result{}, err
	}
	applyConfig := client.ApplyConfigurationFromUnstructured(&unstructured.Unstructured{Object: uns})
	if err := r.Apply(ctx, applyConfig, client.ForceOwnership, client.FieldOwner("flux-drift-webhook")); err != nil {
		r.Metrics.RecordConfigUpdate("error")
		r.event(ref, corev1.EventTypeWarning, "ConfigUpdateFailed", "failed to apply ValidatingWebhookConfiguration: %v", err)
		log.Error(err, "failed to apply ValidatingWebhookConfiguration")
		return ctrl.Result{}, err
	}

	r.Metrics.RecordConfigUpdate("success")
	r.event(ref, corev1.EventTypeNormal, "ConfigUpdated", "updated ValidatingWebhookConfiguration with %d rules", len(rules))
	log.V(1).Info("updated ValidatingWebhookConfiguration", "rulesCount", len(rules))

	return ctrl.Result{RequeueAfter: r.ResyncInterval}, nil
}

// buildWebhookEntry assembles one webhook entry scoped by an objectSelector on
// the given Flux ownership label. Two entries (Kustomization and HelmRelease
// labels) express the OR that a single label selector cannot: the API server
// only sends requests for objects carrying at least one Flux ownership label,
// sparing every unrelated write a webhook round-trip. The selector matches the
// old OR the new object on UPDATE/DELETE, so label stripping cannot dodge
// interception; the in-handler label gate remains as defence in depth.
func (r *WebhookConfigReconciler) buildWebhookEntry(
	name, labelKey string, rules []admissionregistrationv1.RuleWithOperations,
) admissionregistrationv1.ValidatingWebhook {
	sideEffects := admissionregistrationv1.SideEffectClassNone
	return admissionregistrationv1.ValidatingWebhook{
		Name:  name,
		Rules: rules,
		ClientConfig: admissionregistrationv1.WebhookClientConfig{
			Service: &admissionregistrationv1.ServiceReference{
				Name:      r.WebhookService,
				Namespace: r.WebhookNamespace,
				Path:      &r.WebhookPath,
			},
		},
		ObjectSelector: &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: labelKey, Operator: metav1.LabelSelectorOpExists},
			},
		},
		SideEffects:             &sideEffects,
		AdmissionReviewVersions: []string{"v1"},
		MatchConditions:         matchConditions(),
	}
}

// matchConditions returns the CEL pre-filters carrying the API-group exclusions,
// one per entry in config.ExcludedGroups.
//
// The API server evaluates matchConditions after the rules, namespaceSelector
// and objectSelector, so these only run for objects already carrying a Flux
// ownership label — the cost is negligible and the exclusion is exact.
func matchConditions() []admissionregistrationv1.MatchCondition {
	excluded := config.ExcludedGroups()
	conditions := make([]admissionregistrationv1.MatchCondition, 0, len(excluded))
	for _, group := range excluded {
		conditions = append(conditions, admissionregistrationv1.MatchCondition{
			Name:       "exclude-" + group,
			Expression: fmt.Sprintf("request.resource.group != %q", group),
		})
	}
	return conditions
}

// wildcardRules returns the single rule the webhook registers: every group,
// version and resource, at every scope. The API-group exclusions live in the CEL
// matchConditions instead of in the rule list.
//
// This replaced a rule per discovered GroupVersion, refreshed on a timer. A CRD
// installed at any moment is now covered immediately rather than after up to a
// refresh interval, and the ValidatingWebhookConfiguration no longer carries a
// rule list that grows with the cluster's API surface.
func wildcardRules() []admissionregistrationv1.RuleWithOperations {
	return []admissionregistrationv1.RuleWithOperations{{
		Rule: admissionregistrationv1.Rule{
			APIGroups:   []string{"*"},
			APIVersions: []string{"*"},
			Resources:   []string{"*"},
			Scope:       ptr(admissionregistrationv1.AllScopes),
		},
		Operations: []admissionregistrationv1.OperationType{
			admissionregistrationv1.Create,
			admissionregistrationv1.Update,
			admissionregistrationv1.Delete,
		},
	}}
}

// event emits a Kubernetes Event for obj when an EventRecorder is configured.
// It is a no-op when EventRecorder is nil, so the reconciler stays usable in
// unit tests that omit a recorder.
func (r *WebhookConfigReconciler) event(obj runtime.Object, eventtype, reason, messageFmt string, args ...any) {
	if r.EventRecorder != nil {
		r.EventRecorder.Eventf(obj, eventtype, reason, messageFmt, args...)
	}
}

// SetupWithManager registers the reconciler with the manager. It watches only
// the managed ValidatingWebhookConfiguration (by name) and deliberately ignores
// Update events to avoid a reconcile loop with cert-manager-cainjector.
func (r *WebhookConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Only react to Create/Delete, never Update.
	// Updates from cert-manager-cainjector (caBundle injection) would
	// trigger a reconcile loop: APPLY → cainjector PUT → watch fires →
	// APPLY → ... flooding the API server. Periodic reconciliation via
	// RequeueAfter (ResyncInterval) is sufficient.
	nameFilter := func(obj client.Object) bool {
		return obj.GetName() == r.WebhookName
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("webhook-config").
		For(&admissionregistrationv1.ValidatingWebhookConfiguration{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				return nameFilter(e.Object)
			},
			UpdateFunc: func(_ event.UpdateEvent) bool {
				return false
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				return nameFilter(e.Object)
			},
			GenericFunc: func(_ event.GenericEvent) bool {
				return false
			},
		}).
		Complete(r)
}

func ptr[T any](v T) *T {
	return &v
}

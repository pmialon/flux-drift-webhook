//go:build integration

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
	"testing"
	"time"

	. "github.com/onsi/gomega"

	"github.com/go-logr/logr/testr"
	"github.com/prometheus/client_golang/prometheus"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	discoveryclient "k8s.io/client-go/discovery"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/pmialon/flux-drift-webhook/internal/config"
	"github.com/pmialon/flux-drift-webhook/internal/discovery"
	"github.com/pmialon/flux-drift-webhook/internal/metrics"
)

// itWebhookName is a domain with ≥3 segments so the derived webhook entry names
// ("kustomize."/"helm."+itWebhookName) pass apiserver validation.
const itWebhookName = "flux-drift-webhook-it.fluxcd.io"

// newReconciler builds a reconciler wired like production (cmd/webhook/main.go):
// a real discovery client off testEnv.Config and the cacheless k8sClient.
func newReconciler(t *testing.T, rec record.EventRecorder) *WebhookConfigReconciler {
	t.Helper()
	dc, err := discoveryclient.NewDiscoveryClientForConfig(testEnv.Config)
	NewWithT(t).Expect(err).NotTo(HaveOccurred())

	return &WebhookConfigReconciler{
		Client:            k8sClient,
		Discoverer:        discovery.NewDiscoverer(dc, testr.New(t)),
		Metrics:           metrics.NewMetricsWithRegistry(prometheus.NewRegistry()),
		EventRecorder:     rec,
		WebhookName:       itWebhookName,
		WebhookNamespace:  "flux-system",
		WebhookService:    "flux-drift-webhook",
		WebhookPath:       config.WebhookPath,
		DiscoveryInterval: 5 * time.Minute,
	}
}

// newMatchConditionsReconciler is newReconciler with wildcard rules and CEL
// matchConditions enabled.
func newMatchConditionsReconciler(t *testing.T) *WebhookConfigReconciler {
	t.Helper()
	r := newReconciler(t, nil)
	r.UseMatchConditions = true
	return r
}

// seedVWC pre-creates the empty ValidatingWebhookConfiguration the reconciler
// applies into, and registers its deletion as test cleanup.
func seedVWC(t *testing.T, g *WithT) {
	t.Helper()
	vwc := &admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: itWebhookName},
	}
	g.Expect(k8sClient.Create(context.Background(), vwc)).To(Succeed())
	t.Cleanup(func() {
		g.Expect(client.IgnoreNotFound(
			k8sClient.Delete(context.Background(), vwc))).To(Succeed())
	})
}

// TestIntegration_Reconcile_SSAAndDiscovery exercises the real discovery +
// Server-Side Apply path against a live apiserver — the field-management
// semantics and live discovery that the fake client cannot reproduce.
func TestIntegration_Reconcile_SSAAndDiscovery(t *testing.T) {
	g := NewWithT(t)
	// Reconcile reads its logger from the context (ctrl.LoggerFrom).
	ctx := ctrl.LoggerInto(context.Background(), testr.New(t))
	seedVWC(t, g)

	rec := record.NewFakeRecorder(16)
	r := newReconciler(t, rec)

	res, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: itWebhookName},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(5 * time.Minute))

	var got admissionregistrationv1.ValidatingWebhookConfiguration
	g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: itWebhookName}, &got)).To(Succeed())

	// Two entries: one per Flux ownership label (kustomize/helm objectSelector).
	g.Expect(got.Webhooks).To(HaveLen(2))
	wh := got.Webhooks[0]

	// Each entry pre-filters on a Flux ownership label at the apiserver.
	for _, entry := range got.Webhooks {
		g.Expect(entry.ObjectSelector).NotTo(BeNil(), "entry %s must carry an objectSelector", entry.Name)
		g.Expect(entry.ObjectSelector.MatchExpressions).To(HaveLen(1))
	}

	// Rules came from REAL cluster discovery: the core ("" group, v1) and
	// apps/v1 are always present on any apiserver. Assert PRESENCE only — the
	// full discovered set is non-deterministic across apiserver versions.
	g.Expect(wh.Rules).NotTo(BeEmpty())
	g.Expect(ruleGroups(wh.Rules)).To(ContainElements("", "apps"))

	// Cluster-scoped objects are protected too: the apiserver accepted and
	// kept the explicit AllScopes on every rule.
	for _, rule := range wh.Rules {
		g.Expect(rule.Scope).NotTo(BeNil())
		g.Expect(*rule.Scope).To(Equal(admissionregistrationv1.AllScopes))
	}

	// The admissionregistration.k8s.io group is filtered out by the Discoverer,
	// proving the production exclusion runs against live data (and stops the
	// webhook selecting its own configuration).
	g.Expect(ruleGroups(wh.Rules)).NotTo(ContainElement(config.ExcludedGroupAdmission))

	// Real SSA field management: the apiserver recorded our FieldOwner as an
	// Apply entry in managedFields. The fake client never populates this.
	g.Expect(hasApplyManager(got.ManagedFields, "flux-drift-webhook")).To(BeTrue(),
		"expected an Apply managedFields entry for fieldManager flux-drift-webhook")

	// apiserver defaulting accepted and defaulted the object (the fake client
	// never defaults): matchPolicy and timeoutSeconds receive server defaults.
	g.Expect(wh.MatchPolicy).NotTo(BeNil())
	g.Expect(wh.TimeoutSeconds).NotTo(BeNil())

	// A Normal ConfigUpdated event was emitted (Phase 3 event recorder).
	g.Eventually(rec.Events).Should(Receive(ContainSubstring("Normal ConfigUpdated")))
}

// TestIntegration_Reconcile_ForceOwnershipIdempotent proves the SSA path is
// idempotent under a real apiserver: a second Apply with ForceOwnership does
// not raise a field-manager conflict (the fake client cannot surface conflicts).
func TestIntegration_Reconcile_ForceOwnershipIdempotent(t *testing.T) {
	g := NewWithT(t)
	ctx := ctrl.LoggerInto(context.Background(), testr.New(t))
	seedVWC(t, g)

	r := newReconciler(t, nil) // EventRecorder nil -> event() is a no-op.

	for i := 0; i < 2; i++ {
		_, err := r.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: itWebhookName},
		})
		g.Expect(err).NotTo(HaveOccurred(), "Reconcile pass %d", i+1)
	}

	var got admissionregistrationv1.ValidatingWebhookConfiguration
	g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: itWebhookName}, &got)).To(Succeed())
	g.Expect(got.Webhooks).To(HaveLen(2))
	g.Expect(got.Webhooks[0].Rules).NotTo(BeEmpty())
}

// ruleGroups flattens the API groups referenced by the webhook rules.
func ruleGroups(rules []admissionregistrationv1.RuleWithOperations) []string {
	out := make([]string, 0, len(rules))
	for _, rule := range rules {
		out = append(out, rule.APIGroups...)
	}
	return out
}

// hasApplyManager reports whether managedFields contains an Apply-operation
// entry owned by manager.
func hasApplyManager(fields []metav1.ManagedFieldsEntry, manager string) bool {
	for _, f := range fields {
		if f.Manager == manager && f.Operation == metav1.ManagedFieldsOperationApply {
			return true
		}
	}
	return false
}

// TestIntegration_Reconcile_MatchConditionsAccepted is the only place the CEL
// expressions are actually validated. The apiserver compiles matchConditions
// when the ValidatingWebhookConfiguration is written and rejects malformed ones,
// so a typo or a wrong variable name (request.resource vs request.kind, ...)
// fails here and nowhere else — the unit tests run against a fake client that
// stores whatever string it is given.
func TestIntegration_Reconcile_MatchConditionsAccepted(t *testing.T) {
	g := NewWithT(t)
	ctx := ctrl.LoggerInto(context.Background(), testr.New(t))
	seedVWC(t, g)

	r := newMatchConditionsReconciler(t)

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: itWebhookName},
	})
	g.Expect(err).NotTo(HaveOccurred(), "the apiserver rejected the generated matchConditions")

	var got admissionregistrationv1.ValidatingWebhookConfiguration
	g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: itWebhookName}, &got)).To(Succeed())
	g.Expect(got.Webhooks).To(HaveLen(2))

	excluded := config.ExcludedGroups()
	for _, entry := range got.Webhooks {
		// A single wildcard rule replaces the per-GroupVersion list.
		g.Expect(entry.Rules).To(HaveLen(1), "entry %s", entry.Name)
		g.Expect(entry.Rules[0].APIGroups).To(Equal([]string{"*"}), "entry %s", entry.Name)
		g.Expect(entry.Rules[0].APIVersions).To(Equal([]string{"*"}), "entry %s", entry.Name)
		g.Expect(entry.Rules[0].Resources).To(Equal([]string{"*"}), "entry %s", entry.Name)
		g.Expect(entry.Rules[0].Scope).NotTo(BeNil(), "entry %s", entry.Name)
		g.Expect(*entry.Rules[0].Scope).To(Equal(admissionregistrationv1.AllScopes), "entry %s", entry.Name)

		// The exclusions survived the round-trip. If the apiserver had pruned
		// them, the wildcard rule would cover the webhook's own configuration.
		g.Expect(entry.MatchConditions).To(HaveLen(len(excluded)), "entry %s", entry.Name)
		for i, group := range excluded {
			g.Expect(entry.MatchConditions[i].Expression).To(ContainSubstring(group), "entry %s", entry.Name)
		}
	}
}

// TestIntegration_Reconcile_MatchConditionsIdempotent guards the repeated-apply
// path: matchConditions are an atomic list keyed by name, so a second Apply must
// not duplicate or drop them.
func TestIntegration_Reconcile_MatchConditionsIdempotent(t *testing.T) {
	g := NewWithT(t)
	ctx := ctrl.LoggerInto(context.Background(), testr.New(t))
	seedVWC(t, g)

	r := newMatchConditionsReconciler(t)

	for i := 0; i < 2; i++ {
		_, err := r.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: itWebhookName},
		})
		g.Expect(err).NotTo(HaveOccurred(), "Reconcile pass %d", i+1)
	}

	var got admissionregistrationv1.ValidatingWebhookConfiguration
	g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: itWebhookName}, &got)).To(Succeed())
	for _, entry := range got.Webhooks {
		g.Expect(entry.Rules).To(HaveLen(1), "entry %s", entry.Name)
		g.Expect(entry.MatchConditions).To(HaveLen(len(config.ExcludedGroups())), "entry %s", entry.Name)
	}
}

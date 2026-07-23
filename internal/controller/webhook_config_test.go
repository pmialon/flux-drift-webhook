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
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	discoveryfake "k8s.io/client-go/discovery/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/pmialon/flux-drift-webhook/internal/config"
	"github.com/pmialon/flux-drift-webhook/internal/discovery"
	"github.com/pmialon/flux-drift-webhook/internal/metrics"
)

func TestBuildRules(t *testing.T) {
	r := &WebhookConfigReconciler{}

	gvs := []schema.GroupVersion{
		{Group: "", Version: "v1"},
		{Group: "apps", Version: "v1"},
		{Group: "batch", Version: "v1"},
	}

	rules := r.buildRules(gvs)

	if len(rules) != 3 {
		t.Errorf("expected 3 rules, got %d", len(rules))
	}

	for i, rule := range rules {
		if len(rule.APIGroups) != 1 {
			t.Errorf("rule %d: expected 1 APIGroup, got %d", i, len(rule.APIGroups))
		}
		if len(rule.APIVersions) != 1 {
			t.Errorf("rule %d: expected 1 APIVersion, got %d", i, len(rule.APIVersions))
		}
		if len(rule.Resources) != 1 || rule.Resources[0] != "*" {
			t.Errorf("rule %d: expected Resources=[*], got %v", i, rule.Resources)
		}
		if len(rule.Operations) != 3 {
			t.Errorf("rule %d: expected 3 operations, got %d", i, len(rule.Operations))
		}
		if rule.Scope == nil || *rule.Scope != admissionregistrationv1.AllScopes {
			t.Errorf("rule %d: expected Scope=AllScopes (cluster-scoped objects need protection too), got %v", i, rule.Scope)
		}
	}
}

func TestBuildRules_Empty(t *testing.T) {
	r := &WebhookConfigReconciler{}

	rules := r.buildRules([]schema.GroupVersion{})

	if len(rules) != 0 {
		t.Errorf("expected 0 rules for empty input, got %d", len(rules))
	}
}

func TestBuildRules_SkipsEmptyVersion(t *testing.T) {
	r := &WebhookConfigReconciler{}

	gvs := []schema.GroupVersion{
		{Group: "", Version: "v1"},
		{Group: "apps", Version: ""},
		{Group: "batch", Version: "v1"},
	}

	rules := r.buildRules(gvs)

	if len(rules) != 2 {
		t.Errorf("expected 2 rules (empty version skipped), got %d", len(rules))
	}
}

func TestReconcile_Success(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = admissionregistrationv1.AddToScheme(scheme)

	existingVWC := &admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-webhook",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(existingVWC).
		Build()

	fakeDiscovery := &discoveryfake.FakeDiscovery{
		Fake: &ktesting.Fake{},
	}
	fakeDiscovery.Resources = []*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: []metav1.APIResource{{Name: "pods", Kind: "Pod"}}},
		{GroupVersion: "apps/v1", APIResources: []metav1.APIResource{{Name: "deployments", Kind: "Deployment"}}},
	}

	disc := discovery.NewDiscoverer(fakeDiscovery, logr.Discard())
	m := metrics.NewMetricsWithRegistry(prometheus.NewRegistry())
	webhookPath := "/validate"

	r := &WebhookConfigReconciler{
		Client:            fakeClient,
		Discoverer:        disc,
		Metrics:           m,
		WebhookName:       "test-webhook",
		WebhookNamespace:  "flux-system",
		WebhookService:    "flux-drift-webhook",
		WebhookPath:       webhookPath,
		DiscoveryInterval: 5 * time.Minute,
	}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-webhook"},
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if result.RequeueAfter != 5*time.Minute {
		t.Errorf("expected RequeueAfter=5m, got %v", result.RequeueAfter)
	}

	var vwc admissionregistrationv1.ValidatingWebhookConfiguration
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-webhook"}, &vwc); err != nil {
		t.Fatalf("failed to get VWC: %v", err)
	}

	if len(vwc.Webhooks) != 2 {
		t.Fatalf("expected 2 webhook entries (kustomize + helm), got %d", len(vwc.Webhooks))
	}

	wantSelectors := map[string]string{
		"kustomize.test-webhook": "kustomize.toolkit.fluxcd.io/name",
		"helm.test-webhook":      "helm.toolkit.fluxcd.io/name",
	}
	for _, wh := range vwc.Webhooks {
		labelKey, known := wantSelectors[wh.Name]
		if !known {
			t.Errorf("unexpected webhook entry name %q", wh.Name)
			continue
		}
		if len(wh.Rules) != 2 {
			t.Errorf("entry %s: expected 2 rules (v1 + apps/v1), got %d", wh.Name, len(wh.Rules))
		}
		if wh.ObjectSelector == nil || len(wh.ObjectSelector.MatchExpressions) != 1 {
			t.Errorf("entry %s: expected an objectSelector with 1 matchExpression", wh.Name)
			continue
		}
		expr := wh.ObjectSelector.MatchExpressions[0]
		if expr.Key != labelKey || expr.Operator != metav1.LabelSelectorOpExists {
			t.Errorf("entry %s: expected Exists selector on %s, got %s %s", wh.Name, labelKey, expr.Key, expr.Operator)
		}
	}
}

func TestReconcile_EmitsConfigUpdatedEvent(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = admissionregistrationv1.AddToScheme(scheme)

	existingVWC := &admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "test-webhook"},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(existingVWC).
		Build()

	fakeDiscovery := &discoveryfake.FakeDiscovery{Fake: &ktesting.Fake{}}
	fakeDiscovery.Resources = []*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: []metav1.APIResource{{Name: "pods", Kind: "Pod"}}},
		{GroupVersion: "apps/v1", APIResources: []metav1.APIResource{{Name: "deployments", Kind: "Deployment"}}},
	}

	disc := discovery.NewDiscoverer(fakeDiscovery, logr.Discard())
	m := metrics.NewMetricsWithRegistry(prometheus.NewRegistry())
	recorder := record.NewFakeRecorder(10)

	r := &WebhookConfigReconciler{
		Client:            fakeClient,
		Discoverer:        disc,
		Metrics:           m,
		EventRecorder:     recorder,
		WebhookName:       "test-webhook",
		WebhookNamespace:  "flux-system",
		WebhookService:    "flux-drift-webhook",
		WebhookPath:       "/validate",
		DiscoveryInterval: 5 * time.Minute,
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-webhook"},
	}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	select {
	case ev := <-recorder.Events:
		if !strings.Contains(ev, "Normal ConfigUpdated") {
			t.Errorf("expected a Normal ConfigUpdated event, got %q", ev)
		}
	default:
		t.Error("expected a ConfigUpdated event to be emitted, got none")
	}
}

func TestReconcile_DiscoveryFailure(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = admissionregistrationv1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	fakeDiscovery := &discoveryfake.FakeDiscovery{
		Fake: &ktesting.Fake{},
	}

	disc := discovery.NewDiscoverer(fakeDiscovery, logr.Discard())
	m := metrics.NewMetricsWithRegistry(prometheus.NewRegistry())

	r := &WebhookConfigReconciler{
		Client:            fakeClient,
		Discoverer:        disc,
		Metrics:           m,
		WebhookName:       "test-webhook",
		DiscoveryInterval: 5 * time.Minute,
	}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-webhook"},
	})

	if err != nil {
		if result.RequeueAfter != time.Minute {
			t.Errorf("expected RequeueAfter=1m on error, got %v", result.RequeueAfter)
		}
	}
}

func TestWildcardRules(t *testing.T) {
	rules := wildcardRules()

	if len(rules) != 1 {
		t.Fatalf("wildcardRules() returned %d rules, want exactly 1", len(rules))
	}
	rule := rules[0]

	for _, tc := range []struct {
		field string
		got   []string
	}{
		{"APIGroups", rule.APIGroups},
		{"APIVersions", rule.APIVersions},
		{"Resources", rule.Resources},
	} {
		if len(tc.got) != 1 || tc.got[0] != "*" {
			t.Errorf("%s = %v, want [*]", tc.field, tc.got)
		}
	}

	// Cluster-scoped Flux-managed objects must stay covered; Namespaced scope
	// would make `kubectl delete namespace` an unguarded mass-delete primitive.
	if rule.Scope == nil || *rule.Scope != admissionregistrationv1.AllScopes {
		t.Errorf("Scope = %v, want %q", rule.Scope, admissionregistrationv1.AllScopes)
	}

	wantOps := []admissionregistrationv1.OperationType{
		admissionregistrationv1.Create,
		admissionregistrationv1.Update,
		admissionregistrationv1.Delete,
	}
	if len(rule.Operations) != len(wantOps) {
		t.Fatalf("Operations = %v, want %v", rule.Operations, wantOps)
	}
	for i, op := range wantOps {
		if rule.Operations[i] != op {
			t.Errorf("Operations[%d] = %v, want %v", i, rule.Operations[i], op)
		}
	}
}

func TestMatchConditions(t *testing.T) {
	t.Run("absent in discovery mode", func(t *testing.T) {
		r := &WebhookConfigReconciler{UseMatchConditions: false}
		if got := r.matchConditions(); got != nil {
			t.Errorf("matchConditions() = %v, want nil when the feature is off", got)
		}
		entry := r.buildWebhookEntry("kustomize.test", "kustomize.toolkit.fluxcd.io/name", nil)
		if entry.MatchConditions != nil {
			t.Errorf("webhook entry carries MatchConditions %v in discovery mode", entry.MatchConditions)
		}
	})

	t.Run("one condition per excluded group", func(t *testing.T) {
		r := &WebhookConfigReconciler{UseMatchConditions: true}
		excluded := config.ExcludedGroups()

		got := r.matchConditions()
		if len(got) != len(excluded) {
			t.Fatalf("matchConditions() returned %d conditions, want %d (one per excluded group)", len(got), len(excluded))
		}
		for i, group := range excluded {
			wantExpr := `request.resource.group != "` + group + `"`
			if got[i].Expression != wantExpr {
				t.Errorf("condition %d expression = %q, want %q", i, got[i].Expression, wantExpr)
			}
			if !strings.Contains(got[i].Name, group) {
				t.Errorf("condition %d name = %q, want it to mention %q", i, got[i].Name, group)
			}
		}

		// Both webhook entries must carry them, or the unguarded one reopens the
		// self-interception hole the exclusion exists to close.
		for _, entry := range []admissionregistrationv1.ValidatingWebhook{
			r.buildWebhookEntry("kustomize.test", "kustomize.toolkit.fluxcd.io/name", wildcardRules()),
			r.buildWebhookEntry("helm.test", "helm.toolkit.fluxcd.io/name", wildcardRules()),
		} {
			if len(entry.MatchConditions) != len(excluded) {
				t.Errorf("entry %q carries %d match conditions, want %d",
					entry.Name, len(entry.MatchConditions), len(excluded))
			}
		}
	})

	t.Run("admissionregistration is excluded", func(t *testing.T) {
		r := &WebhookConfigReconciler{UseMatchConditions: true}
		var found bool
		for _, c := range r.matchConditions() {
			if strings.Contains(c.Expression, config.ExcludedGroupAdmission) {
				found = true
			}
		}
		if !found {
			t.Errorf("no match condition excludes %q — the webhook could intercept writes to its own configuration",
				config.ExcludedGroupAdmission)
		}
	})
}

// TestReconcile_MatchConditionsSkipsDiscovery pins the point of the mode: with
// wildcard rules there is nothing to discover, so a discovery client that would
// fail must not be consulted at all.
func TestReconcile_MatchConditionsSkipsDiscovery(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = admissionregistrationv1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(&admissionregistrationv1.ValidatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: "test-webhook"},
		}).
		Build()

	// No Resources set: DiscoverGroupVersions would return an empty rule set,
	// which TestReconcile_DiscoveryFailure shows is the failure path.
	failingDiscovery := &discoveryfake.FakeDiscovery{Fake: &ktesting.Fake{}}
	webhookPath := "/validate"

	r := &WebhookConfigReconciler{
		Client:             fakeClient,
		Discoverer:         discovery.NewDiscoverer(failingDiscovery, logr.Discard()),
		Metrics:            metrics.NewMetricsWithRegistry(prometheus.NewRegistry()),
		WebhookName:        "test-webhook",
		WebhookNamespace:   "flux-system",
		WebhookService:     "flux-drift-webhook",
		WebhookPath:        webhookPath,
		DiscoveryInterval:  5 * time.Minute,
		UseMatchConditions: true,
	}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-webhook"},
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v, want success without consulting discovery", err)
	}
	if result.RequeueAfter != 5*time.Minute {
		t.Errorf("RequeueAfter = %v, want 5m", result.RequeueAfter)
	}

	var applied admissionregistrationv1.ValidatingWebhookConfiguration
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-webhook"}, &applied); err != nil {
		t.Fatalf("reading back the applied VWC: %v", err)
	}
	if len(applied.Webhooks) != 2 {
		t.Fatalf("applied VWC has %d webhook entries, want 2", len(applied.Webhooks))
	}
	for _, entry := range applied.Webhooks {
		if len(entry.Rules) != 1 {
			t.Errorf("entry %q has %d rules, want the single wildcard rule", entry.Name, len(entry.Rules))
			continue
		}
		if entry.Rules[0].APIGroups[0] != "*" {
			t.Errorf("entry %q rule APIGroups = %v, want [*]", entry.Name, entry.Rules[0].APIGroups)
		}
		if len(entry.MatchConditions) != len(config.ExcludedGroups()) {
			t.Errorf("entry %q has %d match conditions, want %d",
				entry.Name, len(entry.MatchConditions), len(config.ExcludedGroups()))
		}
	}
}

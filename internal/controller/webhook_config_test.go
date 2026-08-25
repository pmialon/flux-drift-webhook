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

	"github.com/prometheus/client_golang/prometheus"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/pmialon/flux-drift-webhook/internal/config"
	"github.com/pmialon/flux-drift-webhook/internal/metrics"
)

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

	m := metrics.NewMetricsWithRegistry(prometheus.NewRegistry())
	webhookPath := "/validate"

	r := &WebhookConfigReconciler{
		Client:           fakeClient,
		Metrics:          m,
		WebhookName:      "test-webhook",
		WebhookNamespace: "flux-system",
		WebhookService:   "flux-drift-webhook",
		WebhookPath:      webhookPath,
		ResyncInterval:   5 * time.Minute,
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
		// One wildcard rule replaces what used to be a rule per discovered
		// GroupVersion, with the exclusions carried by CEL matchConditions.
		if len(wh.Rules) != 1 {
			t.Errorf("entry %s: expected the single wildcard rule, got %d", wh.Name, len(wh.Rules))
		} else if wh.Rules[0].APIGroups[0] != "*" {
			t.Errorf("entry %s: rule APIGroups = %v, want [*]", wh.Name, wh.Rules[0].APIGroups)
		}
		if len(wh.MatchConditions) != len(config.ExcludedGroups()) {
			t.Errorf("entry %s: expected %d match conditions, got %d",
				wh.Name, len(config.ExcludedGroups()), len(wh.MatchConditions))
		}
		// The safety-critical fields must be part of the apply: a VWC recreated
		// from scratch after an out-of-band delete would otherwise get the
		// apiserver defaults (failurePolicy Fail, timeout 10s, no
		// namespaceSelector) and, with no caBundle, fail closed cluster-wide.
		if wh.FailurePolicy == nil || *wh.FailurePolicy != admissionregistrationv1.Ignore {
			t.Errorf("entry %s: FailurePolicy = %v, want Ignore", wh.Name, wh.FailurePolicy)
		}
		if wh.TimeoutSeconds == nil || *wh.TimeoutSeconds != 3 {
			t.Errorf("entry %s: TimeoutSeconds = %v, want 3", wh.Name, wh.TimeoutSeconds)
		}
		if wh.MatchPolicy == nil || *wh.MatchPolicy != admissionregistrationv1.Equivalent {
			t.Errorf("entry %s: MatchPolicy = %v, want Equivalent", wh.Name, wh.MatchPolicy)
		}
		if wh.NamespaceSelector == nil || len(wh.NamespaceSelector.MatchExpressions) != 1 {
			t.Errorf("entry %s: expected a namespaceSelector with 1 matchExpression", wh.Name)
		} else {
			nsExpr := wh.NamespaceSelector.MatchExpressions[0]
			if nsExpr.Key != "kubernetes.io/metadata.name" || nsExpr.Operator != metav1.LabelSelectorOpNotIn {
				t.Errorf("entry %s: namespaceSelector = %s %s, want NotIn on kubernetes.io/metadata.name",
					wh.Name, nsExpr.Key, nsExpr.Operator)
			}
			// Order matters: the values list is atomic under SSA, so it must
			// mirror the deployed manifests exactly or the two appliers churn.
			wantNS := []string{"kube-system", "kube-public", "kube-node-lease", "flux-system"}
			if len(nsExpr.Values) != len(wantNS) {
				t.Errorf("entry %s: namespaceSelector values = %v, want %v", wh.Name, nsExpr.Values, wantNS)
			} else {
				for i, ns := range wantNS {
					if nsExpr.Values[i] != ns {
						t.Errorf("entry %s: namespaceSelector values[%d] = %q, want %q", wh.Name, i, nsExpr.Values[i], ns)
					}
				}
			}
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

	m := metrics.NewMetricsWithRegistry(prometheus.NewRegistry())
	recorder := record.NewFakeRecorder(10)

	r := &WebhookConfigReconciler{
		Client:           fakeClient,
		Metrics:          m,
		EventRecorder:    recorder,
		WebhookName:      "test-webhook",
		WebhookNamespace: "flux-system",
		WebhookService:   "flux-drift-webhook",
		WebhookPath:      "/validate",
		ResyncInterval:   5 * time.Minute,
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

	// Steady state must not warn: the VWC existed before the apply.
	select {
	case ev := <-recorder.Events:
		t.Errorf("expected no further event when the VWC already exists, got %q", ev)
	default:
	}
}

// A VWC deleted out-of-band is recreated by the reconciler, but without the
// cert-manager caBundle annotation (manifest-only) admission calls fail TLS.
// That window is fail-open only because the apply carries failurePolicy Ignore,
// and it must be observable: the reconciler emits a Warning event.
func TestReconcile_RecreationEmitsWarning(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = admissionregistrationv1.AddToScheme(scheme)

	// No pre-existing VWC: this reconcile recreates it from scratch.
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	m := metrics.NewMetricsWithRegistry(prometheus.NewRegistry())
	recorder := record.NewFakeRecorder(10)

	r := &WebhookConfigReconciler{
		Client:           fakeClient,
		Metrics:          m,
		EventRecorder:    recorder,
		WebhookName:      "test-webhook",
		WebhookNamespace: "flux-system",
		WebhookService:   "flux-drift-webhook",
		WebhookPath:      "/validate",
		ResyncInterval:   5 * time.Minute,
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-webhook"},
	}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	select {
	case ev := <-recorder.Events:
		if !strings.Contains(ev, "Warning ConfigRecreated") {
			t.Errorf("expected a Warning ConfigRecreated event first, got %q", ev)
		}
	default:
		t.Fatal("expected a ConfigRecreated event on from-scratch recreation, got none")
	}

	// The recreated object must carry the fail-open safety defaults.
	var vwc admissionregistrationv1.ValidatingWebhookConfiguration
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-webhook"}, &vwc); err != nil {
		t.Fatalf("failed to get recreated VWC: %v", err)
	}
	for _, wh := range vwc.Webhooks {
		if wh.FailurePolicy == nil || *wh.FailurePolicy != admissionregistrationv1.Ignore {
			t.Errorf("recreated entry %s: FailurePolicy = %v, want Ignore (fail-open without a caBundle)",
				wh.Name, wh.FailurePolicy)
		}
		if wh.NamespaceSelector == nil {
			t.Errorf("recreated entry %s: no namespaceSelector — system namespaces would be intercepted", wh.Name)
		}
	}
}

// The webhook's own namespace joins the exclusion list exactly once, appended
// after the control-plane trio, then the operator extras in their given order —
// matching the deployed manifests exactly (the list is atomic under SSA).
func TestExcludedNamespaces(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		extras    []string
		want      []string
	}{
		{"flux-system appended", "flux-system", nil,
			[]string{"kube-system", "kube-public", "kube-node-lease", "flux-system"}},
		{"custom namespace appended", "webhook-system", nil,
			[]string{"kube-system", "kube-public", "kube-node-lease", "webhook-system"}},
		{"control-plane namespace not duplicated", "kube-system", nil,
			[]string{"kube-system", "kube-public", "kube-node-lease"}},
		{"empty namespace ignored", "", nil,
			[]string{"kube-system", "kube-public", "kube-node-lease"}},
		{"extras appended in order after the built-ins", "flux-system", []string{"ci", "preview"},
			[]string{"kube-system", "kube-public", "kube-node-lease", "flux-system", "ci", "preview"}},
		{"extras deduplicated against built-ins and themselves", "flux-system", []string{"flux-system", "ci", "", "ci"},
			[]string{"kube-system", "kube-public", "kube-node-lease", "flux-system", "ci"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &WebhookConfigReconciler{WebhookNamespace: tt.namespace, ExtraExcludedNamespaces: tt.extras}
			got := r.excludedNamespaces()
			if len(got) != len(tt.want) {
				t.Fatalf("excludedNamespaces() = %v, want %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("excludedNamespaces()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// The failurePolicy/timeoutSeconds knobs flow into both entries; empty/zero
// fall back to the safe defaults (Ignore, 3) so a zero-value reconciler — the
// pre-knob configuration — behaves identically.
func TestBuildWebhookEntry_Knobs(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		r := &WebhookConfigReconciler{WebhookNamespace: "flux-system"}
		entry := r.buildWebhookEntry("kustomize.test", "kustomize.toolkit.fluxcd.io/name", wildcardRules())
		if entry.FailurePolicy == nil || *entry.FailurePolicy != admissionregistrationv1.Ignore {
			t.Errorf("default FailurePolicy = %v, want Ignore", entry.FailurePolicy)
		}
		if entry.TimeoutSeconds == nil || *entry.TimeoutSeconds != 3 {
			t.Errorf("default TimeoutSeconds = %v, want 3", entry.TimeoutSeconds)
		}
	})
	t.Run("overrides", func(t *testing.T) {
		r := &WebhookConfigReconciler{
			WebhookNamespace:        "flux-system",
			FailurePolicy:           admissionregistrationv1.Fail,
			TimeoutSeconds:          10,
			ExtraExcludedNamespaces: []string{"ci"},
		}
		entry := r.buildWebhookEntry("helm.test", "helm.toolkit.fluxcd.io/name", wildcardRules())
		if entry.FailurePolicy == nil || *entry.FailurePolicy != admissionregistrationv1.Fail {
			t.Errorf("FailurePolicy = %v, want Fail", entry.FailurePolicy)
		}
		if entry.TimeoutSeconds == nil || *entry.TimeoutSeconds != 10 {
			t.Errorf("TimeoutSeconds = %v, want 10", entry.TimeoutSeconds)
		}
		values := entry.NamespaceSelector.MatchExpressions[0].Values
		if values[len(values)-1] != "ci" {
			t.Errorf("namespaceSelector values = %v, want the extra namespace appended last", values)
		}
	})
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
	t.Run("one condition per excluded group", func(t *testing.T) {
		r := &WebhookConfigReconciler{}
		excluded := config.ExcludedGroups()

		got := matchConditions()
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
		var found bool
		for _, c := range matchConditions() {
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

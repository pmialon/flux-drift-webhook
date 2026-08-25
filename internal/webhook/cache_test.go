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
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// A cached Kustomization keeps only what the handler reads. The fat parts of
// the spec (patches, postBuild, images, ...) and the status conditions must
// not survive into the informer store: the cache is cluster-wide and the
// container runs under a small memory limit.
func TestStripOwnerForCache(t *testing.T) {
	fat := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1",
		"kind":       "Kustomization",
		"metadata": map[string]any{
			"name":            "apps",
			"namespace":       "flux-system",
			"resourceVersion": "12345",
			"uid":             "abc-def",
			"labels":          map[string]any{"team": "platform"},
			"managedFields":   []any{map[string]any{"manager": "kustomize-controller"}},
		},
		"spec": map[string]any{
			"serviceAccountName": "flux-reconciler",
			"ignore":             []any{map[string]any{"paths": []any{"/spec/replicas"}}},
			"path":               "./apps",
			"interval":           "10m",
			"patches":            []any{map[string]any{"patch": "very large strategic merge patch"}},
			"postBuild":          map[string]any{"substitute": map[string]any{"k": "v"}},
		},
		"status": map[string]any{
			"inventory":  map[string]any{"entries": []any{map[string]any{"id": "default_app_apps_Deployment", "v": "v1"}}},
			"conditions": []any{map[string]any{"type": "Ready", "status": "True", "message": "Applied revision"}},
		},
	}}

	out, err := StripOwnerForCache(fat)
	if err != nil {
		t.Fatalf("StripOwnerForCache() error = %v", err)
	}
	slim, ok := out.(*unstructured.Unstructured)
	if !ok {
		t.Fatalf("StripOwnerForCache() returned %T, want *unstructured.Unstructured", out)
	}

	want := map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1",
		"kind":       "Kustomization",
		"metadata": map[string]any{
			"name":            "apps",
			"namespace":       "flux-system",
			"resourceVersion": "12345",
			"uid":             "abc-def",
			"labels":          map[string]any{"team": "platform"},
		},
		"spec": map[string]any{
			"serviceAccountName": "flux-reconciler",
			"ignore":             []any{map[string]any{"paths": []any{"/spec/replicas"}}},
		},
		"status": map[string]any{
			"inventory": map[string]any{"entries": []any{map[string]any{"id": "default_app_apps_Deployment", "v": "v1"}}},
		},
	}
	if !reflect.DeepEqual(slim.Object, want) {
		t.Errorf("StripOwnerForCache() = %#v\nwant %#v", slim.Object, want)
	}
}

// Sparse owners (no spec.serviceAccountName, no status yet) survive the
// transform without gaining empty sections, and non-unstructured inputs (watch
// tombstones) pass through untouched.
func TestStripOwnerForCache_SparseAndPassthrough(t *testing.T) {
	sparse := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "helm.toolkit.fluxcd.io/v2",
		"kind":       "HelmRelease",
		"metadata":   map[string]any{"name": "redis", "namespace": "db"},
		"spec":       map[string]any{"chart": map[string]any{"spec": map[string]any{"chart": "redis"}}},
	}}
	out, err := StripOwnerForCache(sparse)
	if err != nil {
		t.Fatalf("StripOwnerForCache() error = %v", err)
	}
	slim := out.(*unstructured.Unstructured)
	if _, found := slim.Object["spec"]; found {
		t.Errorf("expected the spec section to be dropped when no read field is present, got %#v", slim.Object["spec"])
	}
	if _, found := slim.Object["status"]; found {
		t.Errorf("expected no status section on a status-less owner, got %#v", slim.Object["status"])
	}
	if slim.GetName() != "redis" || slim.GetNamespace() != "db" {
		t.Errorf("identity fields lost: got %s/%s", slim.GetNamespace(), slim.GetName())
	}

	tombstone := struct{ any }{}
	if got, err := StripOwnerForCache(tombstone); err != nil || got != tombstone {
		t.Errorf("non-unstructured input must pass through untouched, got (%v, %v)", got, err)
	}
}

// The single DefaultTransform must strip owners to the read fields and strip
// managedFields from everything else. Behavioral test: the transform is what
// stands between a cluster-wide informer and the container's memory limit.
// (Owners deliberately do NOT go through cache.Options.ByObject — its keys are
// REST-mapped at cache construction, which fails on a cluster where a CRD,
// e.g. HelmRelease, is absent.)
func TestCacheOptions(t *testing.T) {
	opts := CacheOptions()
	if opts.DefaultTransform == nil {
		t.Fatal("CacheOptions().DefaultTransform is nil — cached objects would keep their full shape")
	}
	if len(opts.ByObject) != 0 {
		t.Errorf("CacheOptions().ByObject must stay empty (its keys are REST-mapped at construction and fail on absent CRDs), got %d entries", len(opts.ByObject))
	}

	for _, gvk := range ownerGVKs() {
		owner := &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "o", "namespace": "ns", "managedFields": []any{map[string]any{}}},
			"spec":     map[string]any{"serviceAccountName": "sa", "path": "./big"},
			"status":   map[string]any{"inventory": map[string]any{"entries": []any{}}, "conditions": []any{map[string]any{}}},
		}}
		owner.SetGroupVersionKind(gvk)
		out, err := opts.DefaultTransform(owner)
		if err != nil {
			t.Fatalf("DefaultTransform(%s): %v", gvk, err)
		}
		slim := out.(*unstructured.Unstructured)
		if _, found, _ := unstructured.NestedString(slim.Object, "spec", "path"); found {
			t.Errorf("%s: spec.path survived the transform — owners are cached full-size", gvk.Kind)
		}
		if _, found, _ := unstructured.NestedMap(slim.Object, "status", "inventory"); !found {
			t.Errorf("%s: status.inventory was stripped — the CREATE veto would always fail closed", gvk.Kind)
		}
		if slim.GetManagedFields() != nil {
			t.Errorf("%s: managedFields survived the transform", gvk.Kind)
		}
	}

	// Any other object (typed or unstructured of a non-owner GVK) keeps its
	// shape minus managedFields.
	other := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "cm", "managedFields": []any{map[string]any{}}},
		"data":     map[string]any{"k": "v"},
	}}
	out, err := opts.DefaultTransform(other)
	if err != nil {
		t.Fatalf("DefaultTransform(ConfigMap): %v", err)
	}
	cm := out.(*unstructured.Unstructured)
	if v, _, _ := unstructured.NestedString(cm.Object, "data", "k"); v != "v" {
		t.Error("non-owner object lost its payload in the transform")
	}
	if cm.GetManagedFields() != nil {
		t.Error("non-owner object kept managedFields")
	}
}

// The metadata-only namespace reads must work against a client that stores
// full corev1.Namespace objects — the shape of the production setup, where the
// API server serves PartialObjectMetadata for any object.
func TestNamespaceMetadataGet(t *testing.T) {
	handler := newHandlerWithNamespace(t, "team-a", false)

	got := NamespaceMetadata()
	if err := handler.Client.Get(t.Context(), client.ObjectKey{Name: "team-a"}, got); err != nil {
		t.Fatalf("Get(PartialObjectMetadata) against the fake client: %v", err)
	}
	if got.Name != "team-a" {
		t.Errorf("got name %q, want team-a", got.Name)
	}
	if _, err := apiutil.GVKForObject(NamespaceMetadata(), fake.NewClientBuilder().Build().Scheme()); err != nil {
		t.Errorf("GVKForObject(NamespaceMetadata()): %v — the pre-warm loop relies on this resolving", err)
	}
}

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/cache"
)

// CacheOptions returns the manager cache configuration for the types the
// handler reads. The informers are cluster-wide and their memory footprint
// grows with the whole GitOps estate (every Namespace; every Kustomization/
// HelmRelease, whose .status.inventory enumerates every object Flux manages),
// while the deployment caps the container at a modest memory limit — so the
// cache stores only what the handler actually consumes:
//
//   - every object is cached without .metadata.managedFields (the handler
//     reads managedFields from the admission REQUEST payload, never from the
//     cache);
//   - Kustomizations and HelmReleases are stripped to the three read fields —
//     .spec.serviceAccountName (multi-tenant identity), .spec.ignore (drift
//     waivers) and .status.inventory (CREATE veto) — dropping the arbitrarily
//     large parts (patches, postBuild substitutions, values, conditions);
//   - Namespaces are watched metadata-only (see NamespaceMetadata): the
//     handler reads only labels and deletionTimestamp, so the spec/status of
//     every namespace in the cluster never enters memory.
//
// The owner stripping dispatches on GVK inside the DefaultTransform rather
// than through cache.Options.ByObject: ByObject keys are REST-mapped when the
// cache is BUILT, so an entry for an absent CRD (e.g. HelmRelease on a cluster
// without helm-controller) would make manager construction fail — while the
// informer pre-warm deliberately tolerates absent CRDs with a warning.
func CacheOptions() cache.Options {
	return cache.Options{DefaultTransform: cacheTransform}
}

var stripManagedFields = cache.TransformStripManagedFields()

// cacheTransform is the informer transform for every cached object: owning
// Kustomizations/HelmReleases are stripped to the fields the handler reads
// (StripOwnerForCache); everything else keeps its shape minus managedFields.
func cacheTransform(obj any) (any, error) {
	if u, ok := obj.(*unstructured.Unstructured); ok && isOwnerGVK(u.GroupVersionKind()) {
		return StripOwnerForCache(u)
	}
	return stripManagedFields(obj)
}

// isOwnerGVK reports whether gvk is one of the owning Flux object kinds.
func isOwnerGVK(gvk schema.GroupVersionKind) bool {
	for _, managedBy := range []string{ManagedByKustomization, ManagedByHelmRelease} {
		if owner, ok := ownerGVK(managedBy); ok && owner == gvk {
			return true
		}
	}
	return false
}

// NamespaceMetadata returns the object the handler uses for cached Namespace
// reads: a PartialObjectMetadata, which the controller-runtime cache serves
// from a metadata-only informer (PartialObjectMetadata list+watch). The
// handler needs only labels and deletionTimestamp, and this keeps the full
// spec/status of every namespace in the cluster out of the cache.
func NamespaceMetadata() *metav1.PartialObjectMetadata {
	return &metav1.PartialObjectMetadata{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Namespace",
		},
	}
}

// ownerCacheSpecFields and ownerCacheStatusFields are the only spec/status
// fields the handler reads from a cached Kustomization/HelmRelease. Everything
// else is dropped before the object enters the informer store.
var (
	ownerCacheSpecFields   = []string{"serviceAccountName", "ignore"}
	ownerCacheStatusFields = []string{"inventory"}
)

// StripOwnerForCache is the informer transform for cached Kustomizations and
// HelmReleases. It keeps the identity fields the informer machinery needs
// (metadata minus managedFields) plus exactly the fields the handler reads,
// and passes through anything that is not an unstructured object (e.g. watch
// tombstones).
func StripOwnerForCache(obj any) (any, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return obj, nil
	}

	slim := map[string]any{}
	for _, key := range []string{"apiVersion", "kind", "metadata"} {
		if v, found := u.Object[key]; found {
			slim[key] = v
		}
	}
	if meta, ok := slim["metadata"].(map[string]any); ok {
		delete(meta, "managedFields")
	}
	copyFields(u.Object, slim, "spec", ownerCacheSpecFields)
	copyFields(u.Object, slim, "status", ownerCacheStatusFields)

	return &unstructured.Unstructured{Object: slim}, nil
}

// copyFields copies src[section][field] for each named field into
// dst[section][field], creating the section only when at least one field is
// present.
func copyFields(src, dst map[string]any, section string, fields []string) {
	from, ok := src[section].(map[string]any)
	if !ok {
		return
	}
	var to map[string]any
	for _, f := range fields {
		v, found := from[f]
		if !found {
			continue
		}
		if to == nil {
			to = map[string]any{}
			dst[section] = to
		}
		to[f] = v
	}
}

// ownerGVKs lists the GVKs of the owning Flux objects, for tests that need to
// pin CacheOptions coverage to what the handler reads.
func ownerGVKs() []schema.GroupVersionKind {
	gvks := make([]schema.GroupVersionKind, 0, 2)
	for _, managedBy := range []string{ManagedByKustomization, ManagedByHelmRelease} {
		if gvk, ok := ownerGVK(managedBy); ok {
			gvks = append(gvks, gvk)
		}
	}
	return gvks
}

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
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"
)

const (
	// FluxKustomizeManager is the SSA field manager used by kustomize-controller.
	FluxKustomizeManager = "kustomize-controller"
	// FluxHelmManager is the SSA field manager used by helm-controller.
	FluxHelmManager = "helm-controller"

	maxDiffDepth = 50
)

var fluxManagerPrefixes = []string{
	FluxKustomizeManager + "/",
	FluxHelmManager + "/",
}

// FluxManagedFields returns the union of fields applied by a Flux SSA field
// manager (kustomize-controller or helm-controller, with optional version
// suffix) across the given managedFields entries.
func FluxManagedFields(managedFields []metav1.ManagedFieldsEntry) (*fieldpath.Set, error) {
	result := fieldpath.NewSet()

	for _, mf := range managedFields {
		if mf.Operation != metav1.ManagedFieldsOperationApply {
			continue
		}
		if !isFluxFieldManager(mf.Manager) {
			continue
		}
		if mf.FieldsV1 == nil {
			continue
		}

		fields := &fieldpath.Set{}
		if err := fields.FromJSON(strings.NewReader(mf.FieldsV1.GetRawString())); err != nil {
			return nil, err
		}

		result = result.Union(fields)
	}

	return result, nil
}

// isFluxFieldManager matches exact name or name with version suffix
// (e.g. "kustomize-controller/v2.0.0").
func isFluxFieldManager(manager string) bool {
	return manager == FluxKustomizeManager ||
		manager == FluxHelmManager ||
		strings.HasPrefix(manager, fluxManagerPrefixes[0]) ||
		strings.HasPrefix(manager, fluxManagerPrefixes[1])
}

// ComputeFieldDiff returns the set of field paths whose values differ between
// oldObj and newObj (added, removed, or changed). A nil operand yields an empty
// set. Recursion is bounded by maxDiffDepth.
func ComputeFieldDiff(oldObj, newObj *unstructured.Unstructured) (*fieldpath.Set, error) {
	if oldObj == nil || newObj == nil {
		return fieldpath.NewSet(), nil
	}

	set := fieldpath.NewSet()
	diffMaps(oldObj.Object, newObj.Object, fieldpath.Path{}, set, 0)
	return set, nil
}

func diffMaps(oldMap, newMap map[string]interface{}, prefix fieldpath.Path, set *fieldpath.Set, depth int) {
	if depth >= maxDiffDepth {
		return
	}

	for key, newVal := range newMap {
		fieldName := key
		currentPath := copyAppend(prefix, fieldpath.PathElement{FieldName: &fieldName})

		oldVal, exists := oldMap[key]
		if !exists {
			set.Insert(currentPath)
			continue
		}

		diffValues(oldVal, newVal, currentPath, set, depth+1)
	}

	for key := range oldMap {
		if _, exists := newMap[key]; !exists {
			fieldName := key
			currentPath := copyAppend(prefix, fieldpath.PathElement{FieldName: &fieldName})
			set.Insert(currentPath)
		}
	}
}

func diffValues(oldVal, newVal interface{}, path fieldpath.Path, set *fieldpath.Set, depth int) {
	switch oldTyped := oldVal.(type) {
	case map[string]interface{}:
		if newTyped, ok := newVal.(map[string]interface{}); ok {
			diffMaps(oldTyped, newTyped, path, set, depth)
			return
		}
	case []interface{}:
		if newTyped, ok := newVal.([]interface{}); ok {
			if !slicesEqual(oldTyped, newTyped) {
				set.Insert(path)
			}
			return
		}
	default:
		if oldVal == newVal {
			return
		}
	}

	set.Insert(path)
}

func slicesEqual(a, b []interface{}) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !valuesEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

func valuesEqual(a, b interface{}) bool {
	switch aTyped := a.(type) {
	case map[string]interface{}:
		bTyped, ok := b.(map[string]interface{})
		if !ok || len(aTyped) != len(bTyped) {
			return false
		}
		for k, v := range aTyped {
			bv, exists := bTyped[k]
			if !exists || !valuesEqual(v, bv) {
				return false
			}
		}
		return true
	case []interface{}:
		bTyped, ok := b.([]interface{})
		if !ok {
			return false
		}
		return slicesEqual(aTyped, bTyped)
	default:
		return a == b
	}
}

// copyAppend appends an element to a path without mutating the original slice.
// This prevents slice aliasing bugs when the same prefix is reused across
// sibling keys in a map iteration.
func copyAppend(prefix fieldpath.Path, elem fieldpath.PathElement) fieldpath.Path {
	result := make(fieldpath.Path, len(prefix)+1)
	copy(result, prefix)
	result[len(prefix)] = elem
	return result
}

// HasFluxManagedFieldConflict reports whether any modified field overlaps with
// a Flux-managed field.
func HasFluxManagedFieldConflict(modifiedFields, fluxManagedFields *fieldpath.Set) bool {
	return !GetConflictingFields(modifiedFields, fluxManagedFields).Empty()
}

// GetConflictingFields returns the fields where the modified set and the
// Flux-managed set overlap, including hierarchical overlap: a modified path
// that is an ancestor or a descendant of a Flux-managed path conflicts.
//
// The hierarchy matters because the schema-blind value diff records a keyed
// list edit as the whole list path (it cannot know the list keys), while Flux's
// SSA fieldsV1 records members INSIDE the list (k:{...} entries). An exact
// intersection never connects the two, silently allowing e.g. a container
// image edit on a Flux-managed Deployment. The trade-off is conservative:
// any change inside a keyed list that Flux partially owns counts as a
// conflict, even for entries Flux does not declare.
//
// A nil operand yields an empty set.
func GetConflictingFields(modifiedFields, fluxManagedFields *fieldpath.Set) *fieldpath.Set {
	if modifiedFields == nil || fluxManagedFields == nil {
		return fieldpath.NewSet()
	}
	// Flux members at or below a modified path (modified ".containers" vs
	// Flux `containers[name="app"].image`).
	fluxUnderModified := fluxManagedFields.Difference(fluxManagedFields.RecursiveDifference(modifiedFields))
	// Modified members at or below a Flux-owned path (Flux owning a subtree
	// root while the request edits a leaf inside it).
	modifiedUnderFlux := modifiedFields.Difference(modifiedFields.RecursiveDifference(fluxManagedFields))
	return fluxUnderModified.Union(modifiedUnderFlux)
}

// WaiveIgnoredConflicts removes from the conflict set every path at or below a
// path in ignoreSet (the owning Kustomization's .spec.ignore / DriftIgnoreRules):
// a field Flux has agreed to leave alone must not block a manual edit. A nil
// ignoreSet leaves the conflict unchanged. Note the asymmetry: an ignored leaf
// does not waive a broader conflict on one of its ancestors, because
// RecursiveDifference only removes descendants of ignored paths — matching the
// conservative stance of GetConflictingFields.
func WaiveIgnoredConflicts(conflict, ignoreSet *fieldpath.Set) *fieldpath.Set {
	if conflict == nil {
		return fieldpath.NewSet()
	}
	if ignoreSet == nil {
		return conflict
	}
	return conflict.RecursiveDifference(ignoreSet)
}

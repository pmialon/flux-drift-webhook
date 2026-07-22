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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"
)

func TestFluxManagedFields(t *testing.T) {
	tests := []struct {
		name          string
		managedFields []metav1.ManagedFieldsEntry
		wantEmpty     bool
		wantErr       bool
	}{
		{
			name:          "nil managed fields",
			managedFields: nil,
			wantEmpty:     true,
		},
		{
			name:          "empty managed fields",
			managedFields: []metav1.ManagedFieldsEntry{},
			wantEmpty:     true,
		},
		{
			name: "kustomize-controller managed fields",
			managedFields: []metav1.ManagedFieldsEntry{
				{
					Manager:   "kustomize-controller",
					Operation: metav1.ManagedFieldsOperationApply,
					FieldsV1:  &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:replicas":{}}}`)},
				},
			},
			wantEmpty: false,
		},
		{
			name: "helm-controller managed fields",
			managedFields: []metav1.ManagedFieldsEntry{
				{
					Manager:   "helm-controller",
					Operation: metav1.ManagedFieldsOperationApply,
					FieldsV1:  &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:template":{}}}`)},
				},
			},
			wantEmpty: false,
		},
		{
			name: "non-Flux controller ignored",
			managedFields: []metav1.ManagedFieldsEntry{
				{
					Manager:   "kubectl",
					Operation: metav1.ManagedFieldsOperationApply,
					FieldsV1:  &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:replicas":{}}}`)},
				},
			},
			wantEmpty: true,
		},
		{
			name: "Update operation ignored (only Apply)",
			managedFields: []metav1.ManagedFieldsEntry{
				{
					Manager:   "kustomize-controller",
					Operation: metav1.ManagedFieldsOperationUpdate,
					FieldsV1:  &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:replicas":{}}}`)},
				},
			},
			wantEmpty: true,
		},
		{
			name: "mixed managers - only Flux extracted",
			managedFields: []metav1.ManagedFieldsEntry{
				{
					Manager:   "kubectl",
					Operation: metav1.ManagedFieldsOperationApply,
					FieldsV1:  &metav1.FieldsV1{Raw: []byte(`{"f:metadata":{"f:labels":{}}}`)},
				},
				{
					Manager:   "kustomize-controller",
					Operation: metav1.ManagedFieldsOperationApply,
					FieldsV1:  &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:replicas":{}}}`)},
				},
				{
					Manager:   "horizontal-pod-autoscaler",
					Operation: metav1.ManagedFieldsOperationApply,
					FieldsV1:  &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:replicas":{}}}`)},
				},
			},
			wantEmpty: false,
		},
		{
			name: "concurrent kustomize and helm managers - union of fields",
			managedFields: []metav1.ManagedFieldsEntry{
				{
					Manager:   "kustomize-controller",
					Operation: metav1.ManagedFieldsOperationApply,
					FieldsV1:  &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:replicas":{}}}`)},
				},
				{
					Manager:   "helm-controller",
					Operation: metav1.ManagedFieldsOperationApply,
					FieldsV1:  &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:template":{}}}`)},
				},
			},
			wantEmpty: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := FluxManagedFields(tt.managedFields)
			if (err != nil) != tt.wantErr {
				t.Errorf("FluxManagedFields() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantEmpty && !got.Empty() {
				t.Errorf("FluxManagedFields() expected empty set, got non-empty")
			}
			if !tt.wantEmpty && got.Empty() {
				t.Errorf("FluxManagedFields() expected non-empty set, got empty")
			}
		})
	}
}

func TestIsFluxFieldManager(t *testing.T) {
	tests := []struct {
		manager string
		want    bool
	}{
		{"kustomize-controller", true},
		{"kustomize-controller/v2.0.0", true},
		{"helm-controller", true},
		{"helm-controller/v0.30.0", true},
		{"kubectl", false},
		{"kubectl-edit", false},
		{"horizontal-pod-autoscaler", false},
		{"vpa-recommender", false},
		{"", false},
		{"kustomize-controller-evil", false},
		{"helm-controller-backdoor", false},
		{"kustomize-controllers", false},
	}

	for _, tt := range tests {
		t.Run(tt.manager, func(t *testing.T) {
			if got := isFluxFieldManager(tt.manager); got != tt.want {
				t.Errorf("isFluxFieldManager(%q) = %v, want %v", tt.manager, got, tt.want)
			}
		})
	}
}

func TestComputeFieldDiff(t *testing.T) {
	makeObj := func(data map[string]interface{}) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: data}
	}

	tests := []struct {
		name         string
		old          map[string]interface{}
		new          map[string]interface{}
		wantEmpty    bool
		wantContains []string // top-level field names expected in diff
	}{
		{
			name:      "identical objects produce empty diff",
			old:       map[string]interface{}{"spec": map[string]interface{}{"replicas": float64(3)}},
			new:       map[string]interface{}{"spec": map[string]interface{}{"replicas": float64(3)}},
			wantEmpty: true,
		},
		{
			name:         "changed leaf value detected",
			old:          map[string]interface{}{"spec": map[string]interface{}{"replicas": float64(3)}},
			new:          map[string]interface{}{"spec": map[string]interface{}{"replicas": float64(5)}},
			wantEmpty:    false,
			wantContains: []string{"spec"},
		},
		{
			name:         "added field detected",
			old:          map[string]interface{}{"spec": map[string]interface{}{"replicas": float64(3)}},
			new:          map[string]interface{}{"spec": map[string]interface{}{"replicas": float64(3), "paused": true}},
			wantEmpty:    false,
			wantContains: []string{"spec"},
		},
		{
			name:         "removed field detected",
			old:          map[string]interface{}{"spec": map[string]interface{}{"replicas": float64(3), "paused": true}},
			new:          map[string]interface{}{"spec": map[string]interface{}{"replicas": float64(3)}},
			wantEmpty:    false,
			wantContains: []string{"spec"},
		},
		{
			name: "unchanged sibling not included in diff",
			old: map[string]interface{}{
				"spec":   map[string]interface{}{"replicas": float64(3)},
				"status": map[string]interface{}{"phase": "Running"},
			},
			new: map[string]interface{}{
				"spec":   map[string]interface{}{"replicas": float64(5)},
				"status": map[string]interface{}{"phase": "Running"},
			},
			wantEmpty:    false,
			wantContains: []string{"spec"},
		},
		{
			name:      "nil old object returns empty",
			old:       nil,
			new:       map[string]interface{}{"spec": map[string]interface{}{"replicas": float64(3)}},
			wantEmpty: true,
		},
		{
			name:         "array value changed",
			old:          map[string]interface{}{"spec": map[string]interface{}{"args": []interface{}{"--flag=a"}}},
			new:          map[string]interface{}{"spec": map[string]interface{}{"args": []interface{}{"--flag=b"}}},
			wantEmpty:    false,
			wantContains: []string{"spec"},
		},
		{
			name:      "array value unchanged",
			old:       map[string]interface{}{"spec": map[string]interface{}{"args": []interface{}{"--flag=a"}}},
			new:       map[string]interface{}{"spec": map[string]interface{}{"args": []interface{}{"--flag=a"}}},
			wantEmpty: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var oldObj, newObj *unstructured.Unstructured
			if tt.old != nil {
				oldObj = makeObj(tt.old)
			}
			newObj = makeObj(tt.new)

			got, err := ComputeFieldDiff(oldObj, newObj)
			if err != nil {
				t.Fatalf("ComputeFieldDiff() error = %v", err)
			}

			if tt.wantEmpty && !got.Empty() {
				t.Errorf("expected empty diff, got: %s", got.String())
			}
			if !tt.wantEmpty && got.Empty() {
				t.Errorf("expected non-empty diff, got empty")
			}
		})
	}
}

func TestHasFluxManagedFieldConflict(t *testing.T) {
	createFieldSet := func(paths ...string) *fieldpath.Set {
		set := fieldpath.NewSet()
		for _, p := range paths {
			fieldName := p
			set.Insert(fieldpath.Path{{FieldName: &fieldName}})
		}
		return set
	}

	tests := []struct {
		name           string
		modifiedFields *fieldpath.Set
		fluxFields     *fieldpath.Set
		wantConflict   bool
	}{
		{
			name:           "nil modified fields",
			modifiedFields: nil,
			fluxFields:     createFieldSet("spec"),
			wantConflict:   false,
		},
		{
			name:           "nil flux fields",
			modifiedFields: createFieldSet("spec"),
			fluxFields:     nil,
			wantConflict:   false,
		},
		{
			name:           "no overlap",
			modifiedFields: createFieldSet("status"),
			fluxFields:     createFieldSet("spec"),
			wantConflict:   false,
		},
		{
			name:           "overlap exists",
			modifiedFields: createFieldSet("spec"),
			fluxFields:     createFieldSet("spec"),
			wantConflict:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasFluxManagedFieldConflict(tt.modifiedFields, tt.fluxFields); got != tt.wantConflict {
				t.Errorf("HasFluxManagedFieldConflict() = %v, want %v", got, tt.wantConflict)
			}
		})
	}
}

// setFromFieldsV1 parses a fieldsV1 JSON document into a fieldpath.Set.
func setFromFieldsV1(t *testing.T, fieldsV1 string) *fieldpath.Set {
	t.Helper()
	s := &fieldpath.Set{}
	if err := s.FromJSON(strings.NewReader(fieldsV1)); err != nil {
		t.Fatalf("FromJSON: %v", err)
	}
	return s
}

// realisticContainersFieldsV1 is the shape the apiserver records for a
// kustomize-controller SSA apply of a Deployment: keyed-list members
// (k:{"name":...}) nested under f:containers.
const realisticContainersFieldsV1 = `{
	"f:spec":{
		"f:template":{
			"f:spec":{
				"f:containers":{
					"k:{\"name\":\"app\"}":{
						".":{},
						"f:name":{},
						"f:image":{}
					}
				}
			}
		}
	}
}`

func TestGetConflictingFields_Hierarchy(t *testing.T) {
	tests := []struct {
		name         string
		modified     string // fieldsV1 JSON
		flux         string // fieldsV1 JSON
		wantConflict bool
	}{
		{
			// The reproduced kubectl-set-image bug: the schema-blind diff
			// records the whole list path, Flux's fieldsV1 records members
			// inside the list. Exact intersection missed this entirely.
			name:         "modified list path is ancestor of Flux keyed-list member",
			modified:     `{"f:spec":{"f:template":{"f:spec":{"f:containers":{}}}}}`,
			flux:         realisticContainersFieldsV1,
			wantConflict: true,
		},
		{
			name:         "modified leaf is descendant of Flux-owned subtree root",
			modified:     `{"f:spec":{"f:template":{"f:metadata":{"f:annotations":{}}}}}`,
			flux:         `{"f:spec":{"f:template":{}}}`,
			wantConflict: true,
		},
		{
			name:         "exact member overlap",
			modified:     `{"f:spec":{"f:replicas":{}}}`,
			flux:         `{"f:spec":{"f:replicas":{}}}`,
			wantConflict: true,
		},
		{
			name:         "disjoint paths (HPA scaling a non-Flux field)",
			modified:     `{"f:spec":{"f:replicas":{}}}`,
			flux:         realisticContainersFieldsV1,
			wantConflict: false,
		},
		{
			name:         "sibling map keys do not conflict",
			modified:     `{"f:metadata":{"f:labels":{"f:team":{}}}}`,
			flux:         `{"f:metadata":{"f:labels":{"f:app":{}}}}`,
			wantConflict: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			modified := setFromFieldsV1(t, tt.modified)
			flux := setFromFieldsV1(t, tt.flux)
			got := GetConflictingFields(modified, flux)
			if got.Empty() == tt.wantConflict {
				t.Errorf("GetConflictingFields() empty=%v, wantConflict=%v (set: %s)",
					got.Empty(), tt.wantConflict, got.String())
			}
		})
	}
}

func TestWaiveIgnoredConflicts(t *testing.T) {
	tests := []struct {
		name      string
		conflict  string // fieldsV1 JSON
		ignore    string // fieldsV1 JSON
		wantEmpty bool   // remaining conflict empty (fully waived)?
	}{
		{
			name:      "exact path waived",
			conflict:  `{"f:spec":{"f:replicas":{}}}`,
			ignore:    `{"f:spec":{"f:replicas":{}}}`,
			wantEmpty: true,
		},
		{
			name:      "descendant of ignored subtree waived",
			conflict:  `{"f:metadata":{"f:annotations":{"f:foo":{}}}}`,
			ignore:    `{"f:metadata":{"f:annotations":{}}}`,
			wantEmpty: true,
		},
		{
			name:      "ancestor of ignored path NOT waived",
			conflict:  `{"f:spec":{}}`,
			ignore:    `{"f:spec":{"f:replicas":{}}}`,
			wantEmpty: false,
		},
		{
			name:      "unrelated conflict unchanged",
			conflict:  `{"f:data":{"f:key":{}}}`,
			ignore:    `{"f:spec":{"f:replicas":{}}}`,
			wantEmpty: false,
		},
		{
			name:      "empty ignore set waives nothing",
			conflict:  `{"f:spec":{"f:replicas":{}}}`,
			ignore:    `{}`,
			wantEmpty: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conflict := setFromFieldsV1(t, tt.conflict)
			ignore := setFromFieldsV1(t, tt.ignore)
			if got := WaiveIgnoredConflicts(conflict, ignore); got.Empty() != tt.wantEmpty {
				t.Errorf("WaiveIgnoredConflicts() empty=%v, want %v (remaining: %s)",
					got.Empty(), tt.wantEmpty, got.String())
			}
		})
	}

	t.Run("nil conflict yields empty", func(t *testing.T) {
		if got := WaiveIgnoredConflicts(nil, fieldpath.NewSet()); !got.Empty() {
			t.Errorf("WaiveIgnoredConflicts(nil, _) not empty: %s", got.String())
		}
	})
	t.Run("nil ignore leaves conflict unchanged", func(t *testing.T) {
		conflict := setFromFieldsV1(t, `{"f:spec":{"f:replicas":{}}}`)
		if got := WaiveIgnoredConflicts(conflict, nil); got.Empty() {
			t.Error("WaiveIgnoredConflicts(_, nil) unexpectedly empty")
		}
	})
}

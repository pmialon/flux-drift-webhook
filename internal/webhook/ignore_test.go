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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// pathFieldNames renders a fieldpath.Path as its ordered field-name segments,
// for concise assertions. Non-field elements (keys/indices) render as "?".
func pathFieldNames(t *testing.T, ptr string) []string {
	t.Helper()
	p, err := jsonPointerToPath(ptr)
	if err != nil {
		t.Fatalf("jsonPointerToPath(%q): %v", ptr, err)
	}
	names := make([]string, 0, len(p))
	for _, e := range p {
		if e.FieldName == nil {
			names = append(names, "?")
			continue
		}
		names = append(names, *e.FieldName)
	}
	return names
}

func TestJSONPointerToPath(t *testing.T) {
	tests := []struct {
		name    string
		ptr     string
		want    []string
		wantErr bool
	}{
		{name: "simple", ptr: "/spec/replicas", want: []string{"spec", "replicas"}},
		{name: "root is empty path", ptr: "", want: []string{}},
		{name: "escaping ~1 then ~0", ptr: "/a~1b/~0c", want: []string{"a/b", "~c"}},
		{name: "escaped slash order (~01 -> ~1)", ptr: "/~01", want: []string{"~1"}},
		{name: "trailing empty segment", ptr: "/spec/", want: []string{"spec", ""}},
		{name: "single slash is empty-key segment", ptr: "/", want: []string{""}},
		{name: "numeric segment kept as field name", ptr: "/spec/x/0", want: []string{"spec", "x", "0"}},
		{name: "missing leading slash errors", ptr: "spec/replicas", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.wantErr {
				if _, err := jsonPointerToPath(tt.ptr); err == nil {
					t.Fatalf("jsonPointerToPath(%q): expected error", tt.ptr)
				}
				return
			}
			got := pathFieldNames(t, tt.ptr)
			if strings.Join(got, "\x00") != strings.Join(tt.want, "\x00") {
				t.Errorf("jsonPointerToPath(%q) = %v, want %v", tt.ptr, got, tt.want)
			}
		})
	}
}

func TestRuleMatchesObject(t *testing.T) {
	depGVK := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	objLabels := map[string]string{"app": "web", "tier": "frontend"}
	objAnnotations := map[string]string{"team": "payments"}

	tests := []struct {
		name    string
		target  map[string]interface{}
		want    bool
		wantErr bool
	}{
		{name: "nil target matches all", target: nil, want: true},
		{name: "empty target matches all", target: map[string]interface{}{}, want: true},
		{name: "kind match", target: map[string]interface{}{"kind": "Deployment"}, want: true},
		{name: "kind anchored mismatch (prefix)", target: map[string]interface{}{"kind": "Deploy"}, want: false},
		{name: "kind regex", target: map[string]interface{}{"kind": "Deploy.*"}, want: true},
		{name: "gvk full match", target: map[string]interface{}{"group": "apps", "version": "v1", "kind": "Deployment"}, want: true},
		{name: "group mismatch", target: map[string]interface{}{"group": "batch"}, want: false},
		{name: "name regex match", target: map[string]interface{}{"name": "w.*"}, want: true},
		{name: "namespace mismatch", target: map[string]interface{}{"namespace": "kube-system"}, want: false},
		{name: "labelSelector match", target: map[string]interface{}{"labelSelector": "app=web"}, want: true},
		{name: "labelSelector set-based match", target: map[string]interface{}{"labelSelector": "tier in (frontend,api)"}, want: true},
		{name: "labelSelector mismatch", target: map[string]interface{}{"labelSelector": "app=api"}, want: false},
		{name: "annotationSelector match", target: map[string]interface{}{"annotationSelector": "team=payments"}, want: true},
		{name: "annotationSelector mismatch", target: map[string]interface{}{"annotationSelector": "team=platform"}, want: false},
		{name: "combined all match", target: map[string]interface{}{"kind": "Deployment", "labelSelector": "app=web"}, want: true},
		{name: "combined one fails", target: map[string]interface{}{"kind": "Deployment", "labelSelector": "app=api"}, want: false},
		{name: "invalid regex errors", target: map[string]interface{}{"name": "["}, wantErr: true},
		{name: "invalid labelSelector errors", target: map[string]interface{}{"labelSelector": "!!bad"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ruleMatchesObject(tt.target, depGVK, "web", "default", objLabels, objAnnotations)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ruleMatchesObject(%v): expected error", tt.target)
				}
				return
			}
			if err != nil {
				t.Fatalf("ruleMatchesObject(%v): unexpected error: %v", tt.target, err)
			}
			if got != tt.want {
				t.Errorf("ruleMatchesObject(%v) = %v, want %v", tt.target, got, tt.want)
			}
		})
	}
}

func TestParseIgnoreRules(t *testing.T) {
	owner := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"ignore": []interface{}{
				map[string]interface{}{"paths": []interface{}{"/spec/replicas"}},
				map[string]interface{}{
					"paths":  []interface{}{"/metadata/annotations/foo"},
					"target": map[string]interface{}{"kind": "Deployment"},
				},
				map[string]interface{}{"target": map[string]interface{}{"kind": "Service"}}, // no paths -> skipped
			},
		},
	}}

	rules := parseIgnoreRules(owner)
	if len(rules) != 2 {
		t.Fatalf("parseIgnoreRules: got %d rules, want 2", len(rules))
	}
	if rules[0].paths[0] != "/spec/replicas" || rules[0].target != nil {
		t.Errorf("rule[0] = %+v, want paths=[/spec/replicas] target=nil", rules[0])
	}
	if rules[1].target["kind"] != "Deployment" {
		t.Errorf("rule[1].target = %v, want kind=Deployment", rules[1].target)
	}

	// No .spec.ignore -> no rules.
	if got := parseIgnoreRules(&unstructured.Unstructured{Object: map[string]interface{}{}}); got != nil {
		t.Errorf("parseIgnoreRules(no ignore) = %v, want nil", got)
	}
}

func TestIgnoreSetForObject(t *testing.T) {
	depGVK := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}

	t.Run("matching rules union their paths", func(t *testing.T) {
		rules := []ignoreRule{
			{paths: []string{"/spec/replicas"}},
			{paths: []string{"/metadata/annotations/foo"}, target: map[string]interface{}{"kind": "Deployment"}},
		}
		set, err := ignoreSetForObject(rules, depGVK, "web", "default", nil, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		wantReplicas, _ := jsonPointerToPath("/spec/replicas")
		wantAnn, _ := jsonPointerToPath("/metadata/annotations/foo")
		if !set.Has(wantReplicas) || !set.Has(wantAnn) {
			t.Errorf("ignoreSetForObject missing expected paths: %s", set.String())
		}
	})

	t.Run("non-matching target excluded", func(t *testing.T) {
		rules := []ignoreRule{{paths: []string{"/spec/replicas"}, target: map[string]interface{}{"kind": "StatefulSet"}}}
		set, err := ignoreSetForObject(rules, depGVK, "web", "default", nil, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !set.Empty() {
			t.Errorf("ignoreSetForObject: expected empty set for non-matching target, got %s", set.String())
		}
	})

	t.Run("invalid pointer in matching rule fails closed", func(t *testing.T) {
		rules := []ignoreRule{{paths: []string{"no-leading-slash"}}}
		if _, err := ignoreSetForObject(rules, depGVK, "web", "default", nil, nil); err == nil {
			t.Error("ignoreSetForObject: expected error for invalid JSON pointer")
		}
	})

	t.Run("invalid selector in matching rule fails closed", func(t *testing.T) {
		rules := []ignoreRule{{paths: []string{"/spec/replicas"}, target: map[string]interface{}{"name": "["}}}
		if _, err := ignoreSetForObject(rules, depGVK, "web", "default", nil, nil); err == nil {
			t.Error("ignoreSetForObject: expected error for invalid selector regex")
		}
	})
}

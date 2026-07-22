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
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"
)

// reasonDriftIgnored is the decision reason for an UPDATE whose only conflicting
// fields are excluded from drift detection by the owning Kustomization's
// .spec.ignore (Flux DriftIgnoreRules).
const reasonDriftIgnored = "allowed_drift_ignored_field"

// ignoreRule is a parsed Kustomization .spec.ignore entry: RFC 6901 JSON pointer
// paths, optionally scoped to objects matching target (a kustomize Selector).
type ignoreRule struct {
	paths  []string
	target map[string]interface{}
}

// parseIgnoreRules extracts .spec.ignore from the owning Kustomization. Only
// Kustomization owners carry this field; a nil, empty or malformed value yields
// no rules. Entries without paths are skipped.
func parseIgnoreRules(owner *unstructured.Unstructured) []ignoreRule {
	entries, _, err := unstructured.NestedSlice(owner.Object, "spec", "ignore")
	if err != nil || len(entries) == 0 {
		return nil
	}

	rules := make([]ignoreRule, 0, len(entries))
	for _, e := range entries {
		entry, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		paths, _, _ := unstructured.NestedStringSlice(entry, "paths")
		if len(paths) == 0 {
			continue
		}
		target, _, _ := unstructured.NestedMap(entry, "target")
		rules = append(rules, ignoreRule{paths: paths, target: target})
	}
	return rules
}

// ignoreSetForObject returns the union of JSON-pointer paths from every ignore
// rule whose target matches the object. Fails closed: an invalid pointer or
// selector in a matching rule returns an error so the caller keeps the deny.
func ignoreSetForObject(
	rules []ignoreRule,
	gvk schema.GroupVersionKind,
	name, namespace string,
	objLabels, objAnnotations map[string]string,
) (*fieldpath.Set, error) {
	set := fieldpath.NewSet()
	for _, rule := range rules {
		match, err := ruleMatchesObject(rule.target, gvk, name, namespace, objLabels, objAnnotations)
		if err != nil {
			return nil, err
		}
		if !match {
			continue
		}
		for _, ptr := range rule.paths {
			path, err := jsonPointerToPath(ptr)
			if err != nil {
				return nil, err
			}
			set.Insert(path)
		}
	}
	return set, nil
}

// jsonPointerToPath converts an RFC 6901 JSON pointer (e.g. "/spec/replicas")
// into a fieldpath.Path of field-name elements. The empty pointer "" addresses
// the whole document (root). Numeric segments are treated as field names, never
// list indices or keys: the value diff is schema-blind and collapses a list edit
// to the list path, so an ignore pointer that descends into a list index can
// never match a conflict and simply never waives.
func jsonPointerToPath(ptr string) (fieldpath.Path, error) {
	if ptr == "" {
		return fieldpath.Path{}, nil
	}
	if !strings.HasPrefix(ptr, "/") {
		return nil, fmt.Errorf("invalid JSON pointer %q: must start with '/'", ptr)
	}

	segments := strings.Split(ptr[1:], "/")
	path := make(fieldpath.Path, 0, len(segments))
	for _, seg := range segments {
		name := unescapeJSONPointer(seg)
		path = append(path, fieldpath.PathElement{FieldName: &name})
	}
	return path, nil
}

// unescapeJSONPointer reverses RFC 6901 escaping: ~1 -> "/" then ~0 -> "~".
// Order matters so that "~01" decodes to "~1", not "/".
func unescapeJSONPointer(seg string) string {
	seg = strings.ReplaceAll(seg, "~1", "/")
	seg = strings.ReplaceAll(seg, "~0", "~")
	return seg
}

// ruleMatchesObject reports whether an ignore rule's target selector matches the
// object under admission. A nil or empty target matches every object. The group,
// version, kind, name and namespace fields are anchored regular expressions
// (empty = match any); labelSelector and annotationSelector use Kubernetes
// selector syntax. This mirrors fluxcd/pkg/ssa/jsondiff selector semantics.
// Fails closed: an invalid regex or selector returns an error so the rule
// contributes no waiver.
func ruleMatchesObject(
	target map[string]interface{},
	gvk schema.GroupVersionKind,
	name, namespace string,
	objLabels, objAnnotations map[string]string,
) (bool, error) {
	if len(target) == 0 {
		return true, nil
	}

	patterns := map[string]string{
		"group": gvk.Group, "version": gvk.Version, "kind": gvk.Kind,
		"name": name, "namespace": namespace,
	}
	for key, value := range patterns {
		pattern, _, _ := unstructured.NestedString(target, key)
		ok, err := anchoredRegexMatch(pattern, value)
		if err != nil || !ok {
			return ok, err
		}
	}

	if ok, err := selectorMatches(target, "labelSelector", objLabels); err != nil || !ok {
		return ok, err
	}
	return selectorMatches(target, "annotationSelector", objAnnotations)
}

// anchoredRegexMatch reports whether value matches pattern as an anchored regex.
// An empty pattern matches anything.
func anchoredRegexMatch(pattern, value string) (bool, error) {
	if pattern == "" {
		return true, nil
	}
	re, err := regexp.Compile("^(?:" + pattern + ")$")
	if err != nil {
		return false, fmt.Errorf("invalid selector regex %q: %w", pattern, err)
	}
	return re.MatchString(value), nil
}

// selectorMatches reports whether the object's labels or annotations satisfy the
// Kubernetes selector at target[key]. An empty selector matches anything.
func selectorMatches(target map[string]interface{}, key string, set map[string]string) (bool, error) {
	expr, _, _ := unstructured.NestedString(target, key)
	if expr == "" {
		return true, nil
	}
	sel, err := labels.Parse(expr)
	if err != nil {
		return false, fmt.Errorf("invalid %s %q: %w", key, expr, err)
	}
	return sel.Matches(labels.Set(set)), nil
}

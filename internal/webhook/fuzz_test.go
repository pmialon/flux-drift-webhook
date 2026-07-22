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
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Fuzz_extractMetadata feeds arbitrary bytes to the admission-object metadata
// parser. It must never panic; a parse error is an acceptable outcome.
func Fuzz_extractMetadata(f *testing.F) {
	f.Add([]byte(`{"metadata":{"name":"x","labels":{"a":"b"}}}`))
	f.Add([]byte(`{"metadata":{"deletionTimestamp":"2024-01-01T00:00:00Z"}}`))
	f.Add([]byte(`{"metadata":{}}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte(``))

	h := &DriftPreventionHandler{}
	f.Fuzz(func(_ *testing.T, raw []byte) {
		var meta metav1.ObjectMeta
		_ = h.extractMetadata(raw, &meta)
	})
}

// Fuzz_ComputeFieldDiff feeds two arbitrary JSON documents through the
// Server-Side Apply field diff. The bounded recursion (maxDiffDepth) must
// prevent any panic or stack overflow on deeply nested or malformed input.
func Fuzz_ComputeFieldDiff(f *testing.F) {
	f.Add([]byte(`{"spec":{"replicas":1}}`), []byte(`{"spec":{"replicas":2}}`))
	f.Add([]byte(`{"a":[1,2,3]}`), []byte(`{"a":[1,2]}`))
	f.Add([]byte(`{"x":{"y":{"z":1}}}`), []byte(`{"x":{"y":{"z":2}}}`))
	f.Add([]byte(`{}`), []byte(`{}`))

	f.Fuzz(func(_ *testing.T, oldRaw, newRaw []byte) {
		oldObj := &unstructured.Unstructured{}
		newObj := &unstructured.Unstructured{}
		if json.Unmarshal(oldRaw, &oldObj.Object) != nil {
			return
		}
		if json.Unmarshal(newRaw, &newObj.Object) != nil {
			return
		}
		_, _ = ComputeFieldDiff(oldObj, newObj)
	})
}

// Fuzz_FluxManagedFields feeds arbitrary FieldsV1 JSON to the managed-fields
// extractor (structured-merge-diff FromJSON). It must never panic; a parse
// error is an acceptable outcome.
func Fuzz_FluxManagedFields(f *testing.F) {
	f.Add([]byte(`{"f:spec":{"f:replicas":{}}}`))
	f.Add([]byte(`{"f:metadata":{"f:labels":{}}}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`not-json`))

	f.Fuzz(func(_ *testing.T, raw []byte) {
		entries := []metav1.ManagedFieldsEntry{
			{
				Manager:   FluxKustomizeManager,
				Operation: metav1.ManagedFieldsOperationApply,
				FieldsV1:  &metav1.FieldsV1{Raw: raw},
			},
		}
		_, _ = FluxManagedFields(entries)
	})
}

// Fuzz_jsonPointerToPath feeds arbitrary strings to the RFC 6901 JSON pointer
// parser used for Kustomization .spec.ignore paths. It must never panic; a parse
// error is an acceptable outcome.
func Fuzz_jsonPointerToPath(f *testing.F) {
	f.Add("/spec/replicas")
	f.Add("/a~1b/~0c")
	f.Add("")
	f.Add("no-leading-slash")
	f.Add("/")
	f.Add("///")
	f.Add("/~")

	f.Fuzz(func(_ *testing.T, ptr string) {
		_, _ = jsonPointerToPath(ptr)
	})
}

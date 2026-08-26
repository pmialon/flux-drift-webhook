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

// Handle runs on every write of every Flux-labelled object in the cluster;
// diffMaps/valuesEqual and the fieldpath set algebra are recursive per-request
// work. These benchmarks pin the hot path so an accidental quadratic (large
// keyed lists — the Endpoints shape) shows up in a benchstat diff instead of
// in production latency (where >timeoutSeconds means silent fail-open).

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// benchEndpointsPair builds an Endpoints-shaped object pair with n addresses,
// one address changed — the large-keyed-list worst case.
func benchEndpointsPair(n int) (oldObj, newObj *unstructured.Unstructured) {
	build := func(lastIP string) *unstructured.Unstructured {
		addrs := make([]interface{}, n)
		for i := 0; i < n-1; i++ {
			addrs[i] = map[string]interface{}{"ip": fmt.Sprintf("10.0.%d.%d", i/250, i%250)}
		}
		addrs[n-1] = map[string]interface{}{"ip": lastIP}
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1", "kind": "Endpoints",
			"metadata": map[string]interface{}{"name": "big-svc", "namespace": "default"},
			"subsets": []interface{}{map[string]interface{}{
				"addresses": addrs,
				"ports":     []interface{}{map[string]interface{}{"port": int64(8080)}},
			}},
		}}
	}
	return build("10.99.0.1"), build("10.99.0.2")
}

func Benchmark_ComputeFieldDiff_Deployment(b *testing.B) {
	oldObj := &unstructured.Unstructured{}
	newObj := &unstructured.Unstructured{}
	if err := json.Unmarshal(deploymentJSON(fluxKustomizeLabels, realisticDeploymentManagedFields(), "nginx:1.0", 1), &oldObj.Object); err != nil {
		b.Fatal(err)
	}
	if err := json.Unmarshal(deploymentJSON(fluxKustomizeLabels, realisticDeploymentManagedFields(), "nginx:2.0", 1), &newObj.Object); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = ComputeFieldDiff(oldObj, newObj)
	}
}

func Benchmark_ComputeFieldDiff_LargeKeyedList(b *testing.B) {
	oldObj, newObj := benchEndpointsPair(1000)
	b.ReportAllocs()
	for b.Loop() {
		_ = ComputeFieldDiff(oldObj, newObj)
	}
}

func Benchmark_GetConflictingFields(b *testing.B) {
	oldObj := &unstructured.Unstructured{}
	newObj := &unstructured.Unstructured{}
	_ = json.Unmarshal(deploymentJSON(fluxKustomizeLabels, realisticDeploymentManagedFields(), "nginx:1.0", 1), &oldObj.Object)
	_ = json.Unmarshal(deploymentJSON(fluxKustomizeLabels, realisticDeploymentManagedFields(), "nginx:2.0", 1), &newObj.Object)
	modified := ComputeFieldDiff(oldObj, newObj)

	var meta struct {
		Metadata struct {
			ManagedFields []interface{} `json:"managedFields"`
		} `json:"metadata"`
	}
	raw := deploymentJSON(fluxKustomizeLabels, realisticDeploymentManagedFields(), "nginx:1.0", 1)
	if err := json.Unmarshal(raw, &meta); err != nil {
		b.Fatal(err)
	}
	objMeta, _, err := (&DriftPreventionHandler{}).extractRequestMetadata(deploymentUpdateRequest(raw, raw))
	if err != nil {
		b.Fatal(err)
	}
	fluxFields, err := FluxManagedFields(objMeta.ManagedFields)
	if err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	for b.Loop() {
		_ = GetConflictingFields(modified, fluxFields)
	}
}

// Benchmark_Handle_Update measures the full pipeline on the headline scenario
// (Flux-owned Deployment, denied image drift): request parsing, managedFields
// extraction, diff, hierarchy-aware conflict detection, response build.
func Benchmark_Handle_Update(b *testing.B) {
	handler := newTestHandler()
	oldRaw := deploymentJSON(fluxKustomizeLabels, realisticDeploymentManagedFields(), "nginx:1.0", 1)
	newRaw := deploymentJSON(fluxKustomizeLabels, realisticDeploymentManagedFields(), "nginx:2.0", 1)
	req := deploymentUpdateRequest(oldRaw, newRaw)
	ctx := context.Background()

	b.ReportAllocs()
	for b.Loop() {
		_ = handler.Handle(ctx, req)
	}
}

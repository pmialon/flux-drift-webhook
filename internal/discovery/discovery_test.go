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

package discovery

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestDiscoverGroupVersions(t *testing.T) {
	fakeDiscovery := &fake.FakeDiscovery{
		Fake: &ktesting.Fake{},
	}
	fakeDiscovery.Resources = []*metav1.APIResourceList{
		{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{Name: "pods", Namespaced: true, Kind: "Pod"},
				{Name: "services", Namespaced: true, Kind: "Service"},
			},
		},
		{
			GroupVersion: "apps/v1",
			APIResources: []metav1.APIResource{
				{Name: "deployments", Namespaced: true, Kind: "Deployment"},
			},
		},
	}

	d := NewDiscoverer(fakeDiscovery, logr.Discard())

	gvs, err := d.DiscoverGroupVersions(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(gvs) != 2 {
		t.Errorf("expected 2 GroupVersions, got %d", len(gvs))
	}

	expectedGVs := map[schema.GroupVersion]bool{
		{Group: "", Version: "v1"}:     true,
		{Group: "apps", Version: "v1"}: true,
	}

	for _, gv := range gvs {
		if !expectedGVs[gv] {
			t.Errorf("unexpected GroupVersion: %v", gv)
		}
	}
}

func TestDiscoverGroupVersions_ExcludedGroups(t *testing.T) {
	fakeDiscovery := &fake.FakeDiscovery{
		Fake: &ktesting.Fake{},
	}
	fakeDiscovery.Resources = []*metav1.APIResourceList{
		{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{Name: "pods", Namespaced: true, Kind: "Pod"},
			},
		},
		{
			GroupVersion: "admissionregistration.k8s.io/v1",
			APIResources: []metav1.APIResource{
				{Name: "validatingwebhookconfigurations", Namespaced: false, Kind: "ValidatingWebhookConfiguration"},
			},
		},
	}

	d := NewDiscoverer(fakeDiscovery, logr.Discard())

	gvs, err := d.DiscoverGroupVersions(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(gvs) != 1 {
		t.Errorf("expected 1 GroupVersion (excluded admissionregistration), got %d", len(gvs))
	}

	for _, gv := range gvs {
		if gv.Group == "admissionregistration.k8s.io" {
			t.Error("admissionregistration.k8s.io should be excluded")
		}
	}
}

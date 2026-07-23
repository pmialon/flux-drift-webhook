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
	"errors"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery"
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

// errServerVersion is a DiscoveryInterface whose ServerVersion always fails,
// covering the "cannot determine the API server version" fallback path.
type errServerVersion struct {
	discovery.DiscoveryInterface
}

func (errServerVersion) ServerVersion() (*version.Info, error) {
	return nil, errors.New("boom")
}

func TestSupportsMatchConditions(t *testing.T) {
	tests := []struct {
		name      string
		info      *version.Info
		want      bool
		wantErr   bool
		wantMatch string // expected substring of the reported version
	}{
		{name: "1.28 is the first supported minor", info: &version.Info{Major: "1", Minor: "28", GitVersion: "v1.28.0"}, want: true, wantMatch: "v1.28.0"},
		{name: "1.30 supported", info: &version.Info{Major: "1", Minor: "30", GitVersion: "v1.30.4"}, want: true, wantMatch: "v1.30.4"},
		{name: "1.27 too old", info: &version.Info{Major: "1", Minor: "27", GitVersion: "v1.27.9"}, want: false, wantMatch: "v1.27.9"},
		{name: "1.25 too old", info: &version.Info{Major: "1", Minor: "25", GitVersion: "v1.25.0"}, want: false, wantMatch: "v1.25.0"},
		// Managed distributions decorate the minor version.
		{name: "decorated minor 28+", info: &version.Info{Major: "1", Minor: "28+", GitVersion: "v1.28.7-eks-1234"}, want: true, wantMatch: "eks"},
		{name: "decorated minor 27+ still too old", info: &version.Info{Major: "1", Minor: "27+", GitVersion: "v1.27.3-gke.100"}, want: false, wantMatch: "gke"},
		{name: "a future major is supported", info: &version.Info{Major: "2", Minor: "0", GitVersion: "v2.0.0"}, want: true, wantMatch: "v2.0.0"},
		// Falls back to major.minor when GitVersion is absent.
		{name: "no GitVersion reports major.minor", info: &version.Info{Major: "1", Minor: "29"}, want: true, wantMatch: "1.29"},
		{name: "unparseable minor errors", info: &version.Info{Major: "1", Minor: "unknown", GitVersion: "v1.x"}, wantErr: true},
		{name: "empty version errors", info: &version.Info{}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeDiscovery := &fake.FakeDiscovery{Fake: &ktesting.Fake{}, FakedServerVersion: tt.info}
			d := NewDiscoverer(fakeDiscovery, logr.Discard())

			got, reported, err := d.SupportsMatchConditions()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got supported=%v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("SupportsMatchConditions() = %v, want %v", got, tt.want)
			}
			if !strings.Contains(reported, tt.wantMatch) {
				t.Errorf("reported version %q does not contain %q", reported, tt.wantMatch)
			}
		})
	}
}

func TestSupportsMatchConditions_ServerVersionError(t *testing.T) {
	d := NewDiscoverer(errServerVersion{}, logr.Discard())

	supported, _, err := d.SupportsMatchConditions()
	if err == nil {
		t.Fatal("expected an error when the server version cannot be read")
	}
	if supported {
		t.Error("supported must be false when the version cannot be determined (fail closed to discovery mode)")
	}
}

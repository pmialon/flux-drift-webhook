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
	"testing"

	"github.com/pmialon/flux-drift-webhook/internal/config"
)

func TestGetFluxManagementInfo(t *testing.T) {
	tests := []struct {
		name        string
		labels      map[string]string
		wantManaged bool
		wantBy      string
	}{
		{
			name:        "nil labels",
			labels:      nil,
			wantManaged: false,
		},
		{
			name:        "empty labels",
			labels:      map[string]string{},
			wantManaged: false,
		},
		{
			name: "kustomization managed",
			labels: map[string]string{
				config.KustomizeLabelName:      "my-app",
				config.KustomizeLabelNamespace: "flux-system",
			},
			wantManaged: true,
			wantBy:      "kustomization",
		},
		{
			name: "helm managed",
			labels: map[string]string{
				config.HelmLabelName:      "my-release",
				config.HelmLabelNamespace: "flux-system",
			},
			wantManaged: true,
			wantBy:      "helmrelease",
		},
		{
			name: "other labels only",
			labels: map[string]string{
				"app": "test",
			},
			wantManaged: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := GetFluxManagementInfo(tt.labels)
			if info.IsManaged != tt.wantManaged {
				t.Errorf("IsManaged = %v, want %v", info.IsManaged, tt.wantManaged)
			}
			if tt.wantManaged && info.ManagedBy != tt.wantBy {
				t.Errorf("ManagedBy = %v, want %v", info.ManagedBy, tt.wantBy)
			}
		})
	}
}

func TestHasBypassAnnotation(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		want        bool
	}{
		{
			name:        "nil annotations",
			annotations: nil,
			want:        false,
		},
		{
			name: "bypass disabled",
			annotations: map[string]string{
				config.BypassAnnotation: config.BypassValue,
			},
			want: true,
		},
		{
			name: "bypass with wrong value",
			annotations: map[string]string{
				config.BypassAnnotation: "enabled",
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasBypassAnnotation(tt.annotations); got != tt.want {
				t.Errorf("HasBypassAnnotation() = %v, want %v", got, tt.want)
			}
		})
	}
}

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

	authenticationv1 "k8s.io/api/authentication/v1"

	"github.com/pmialon/flux-drift-webhook/internal/config"
)

func TestIsFluxController(t *testing.T) {
	saGroups := []string{"system:serviceaccounts", "system:serviceaccounts:flux-system", "system:authenticated"}

	tests := []struct {
		name          string
		username      string
		groups        []string
		fluxNamespace string
		want          bool
	}{
		{
			name:          "kustomize-controller",
			username:      "system:serviceaccount:flux-system:kustomize-controller",
			groups:        saGroups,
			fluxNamespace: "flux-system",
			want:          true,
		},
		{
			name:          "helm-controller",
			username:      "system:serviceaccount:flux-system:helm-controller",
			groups:        saGroups,
			fluxNamespace: "flux-system",
			want:          true,
		},
		{
			name:          "source-controller",
			username:      "system:serviceaccount:flux-system:source-controller",
			groups:        saGroups,
			fluxNamespace: "flux-system",
			want:          true,
		},
		{
			name:          "wrong namespace",
			username:      "system:serviceaccount:other-ns:kustomize-controller",
			groups:        []string{"system:serviceaccounts", "system:serviceaccounts:other-ns"},
			fluxNamespace: "flux-system",
			want:          false,
		},
		{
			name:          "tenant reconciler SA is not a core controller",
			username:      "system:serviceaccount:kafka:flux-reconciler",
			groups:        []string{"system:serviceaccounts", "system:serviceaccounts:kafka"},
			fluxNamespace: "flux-system",
			want:          false,
		},
		{
			name:          "unknown service account",
			username:      "system:serviceaccount:flux-system:unknown-controller",
			groups:        saGroups,
			fluxNamespace: "flux-system",
			want:          false,
		},
		{
			name:          "regular user",
			username:      "admin@example.com",
			groups:        []string{"system:authenticated"},
			fluxNamespace: "flux-system",
			want:          false,
		},
		{
			name:          "empty username",
			username:      "",
			fluxNamespace: "flux-system",
			want:          false,
		},
		{
			name:          "extra colons in username",
			username:      "system:serviceaccount:flux-system:kustomize-controller:extra",
			groups:        saGroups,
			fluxNamespace: "flux-system",
			want:          false,
		},
		{
			name:          "prefix only",
			username:      "system:serviceaccount:",
			fluxNamespace: "flux-system",
			want:          false,
		},
		{
			name:          "SA username without SA group rejects",
			username:      "system:serviceaccount:flux-system:kustomize-controller",
			groups:        []string{"system:authenticated"},
			fluxNamespace: "flux-system",
			want:          false,
		},
		{
			name:          "nil groups rejects",
			username:      "system:serviceaccount:flux-system:kustomize-controller",
			groups:        nil,
			fluxNamespace: "flux-system",
			want:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			userInfo := authenticationv1.UserInfo{
				Username: tt.username,
				Groups:   tt.groups,
			}
			if got := IsFluxController(userInfo, tt.fluxNamespace); got != tt.want {
				t.Errorf("IsFluxController() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsSystemController(t *testing.T) {
	kubeSystemGroups := []string{"system:serviceaccounts", "system:serviceaccounts:kube-system", "system:authenticated"}
	// Effective list = built-in defaults + one operator-configured entry,
	// proving the list is extensible without a rebuild.
	nsNames := append([]string{"custom-ns:my-operator"}, config.DefaultSystemControllerServiceAccounts()...)

	tests := []struct {
		name     string
		username string
		groups   []string
		want     bool
	}{
		{
			name:     "garbage collector",
			username: "system:serviceaccount:kube-system:generic-garbage-collector",
			groups:   kubeSystemGroups,
			want:     true,
		},
		{
			name:     "endpoint controller",
			username: "system:serviceaccount:kube-system:endpoint-controller",
			groups:   kubeSystemGroups,
			want:     true,
		},
		{
			name:     "endpointslice controller",
			username: "system:serviceaccount:kube-system:endpointslice-controller",
			groups:   kubeSystemGroups,
			want:     true,
		},
		{
			name:     "endpointslicemirroring controller",
			username: "system:serviceaccount:kube-system:endpointslicemirroring-controller",
			groups:   kubeSystemGroups,
			want:     true,
		},
		{
			name:     "operator-configured custom entry",
			username: "system:serviceaccount:custom-ns:my-operator",
			groups:   []string{"system:serviceaccounts", "system:serviceaccounts:custom-ns"},
			want:     true,
		},
		{
			name:     "known name but wrong namespace",
			username: "system:serviceaccount:flux-system:generic-garbage-collector",
			groups:   []string{"system:serviceaccounts", "system:serviceaccounts:flux-system"},
			want:     false,
		},
		{
			name:     "cert-manager not in list (covered by ownerReference instead)",
			username: "system:serviceaccount:cert-manager:cert-manager",
			groups:   []string{"system:serviceaccounts", "system:serviceaccounts:cert-manager"},
			want:     false,
		},
		{
			name:     "unknown kube-system service account",
			username: "system:serviceaccount:kube-system:daemon-set-controller",
			groups:   kubeSystemGroups,
			want:     false,
		},
		{
			name:     "SA username without SA group rejects",
			username: "system:serviceaccount:kube-system:generic-garbage-collector",
			groups:   []string{"system:authenticated"},
			want:     false,
		},
		{
			name:     "regular user",
			username: "admin@example.com",
			groups:   []string{"system:authenticated"},
			want:     false,
		},
		{
			name:     "extra colons in username",
			username: "system:serviceaccount:kube-system:generic-garbage-collector:extra",
			groups:   kubeSystemGroups,
			want:     false,
		},
		{
			name:     "flux controller is not a system controller",
			username: "system:serviceaccount:flux-system:kustomize-controller",
			groups:   []string{"system:serviceaccounts", "system:serviceaccounts:flux-system"},
			want:     false,
		},
		{
			name:     "apiserver CRD finalizer (non-SA full username)",
			username: "system:apiserver",
			groups:   []string{"system:masters", "system:authenticated"},
			want:     true,
		},
		{
			name:     "kube-controller-manager without SA credentials (non-SA full username)",
			username: "system:kube-controller-manager",
			groups:   []string{"system:authenticated"},
			want:     true,
		},
		{
			name:     "non-system username cannot match an ns:name shorthand entry",
			username: "kube-system:cronjob-controller",
			groups:   []string{"system:authenticated"},
			want:     false,
		},
		{
			name:     "system username not in the list",
			username: "system:kube-scheduler",
			groups:   []string{"system:authenticated"},
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			userInfo := authenticationv1.UserInfo{
				Username: tt.username,
				Groups:   tt.groups,
			}
			if got := IsSystemController(userInfo, nsNames); got != tt.want {
				t.Errorf("IsSystemController() = %v, want %v", got, tt.want)
			}
		})
	}
}

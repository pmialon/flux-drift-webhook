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
	"slices"
	"strings"

	"github.com/pmialon/flux-drift-webhook/internal/config"
	authenticationv1 "k8s.io/api/authentication/v1"
)

// ParseServiceAccount extracts the namespace and name from a service-account
// username (system:serviceaccount:<ns>:<name>), validating both the username
// format and system:serviceaccounts group membership as defence-in-depth.
func ParseServiceAccount(userInfo authenticationv1.UserInfo) (namespace, name string, ok bool) {
	if !strings.HasPrefix(userInfo.Username, "system:serviceaccount:") {
		return "", "", false
	}

	parts := strings.Split(userInfo.Username, ":")
	if len(parts) != 4 {
		return "", "", false
	}

	if !slices.Contains(userInfo.Groups, "system:serviceaccounts") {
		return "", "", false
	}

	return parts[2], parts[3], true
}

// IsFluxController reports whether the request comes from a core Flux controller
// running in the Flux namespace (no impersonation).
func IsFluxController(userInfo authenticationv1.UserInfo, fluxNamespace string) bool {
	ns, name, ok := ParseServiceAccount(userInfo)
	if !ok {
		return false
	}

	return ns == fluxNamespace && slices.Contains(config.FluxServiceAccounts(), name)
}

// IsSystemController reports whether userInfo is a recognised Kubernetes
// control-plane controller that legitimately acts on Flux-labelled objects as
// part of normal cluster lifecycle: creating resources that merely inherit a
// parent's Flux labels (endpoints/endpointslice controllers), or deleting
// Flux-applied resources (the garbage collector during cascade deletion, the
// TTL-after-finished and CronJob controllers cleaning up completed Jobs, the
// apiserver's CRD finalizer removing instances of a deleted CRD).
// entries is the effective allow-list (built-in defaults union
// operator-configured) of "namespace:name" service-account shorthands or full
// usernames for non-SA component identities (e.g. system:apiserver, or
// system:kube-controller-manager when KCM runs without
// --use-service-account-credentials).
func IsSystemController(userInfo authenticationv1.UserInfo, entries []string) bool {
	// Non-SA component identities authenticate with reserved "system:"
	// usernames, never issued to users or tenants. The prefix guard stops a
	// crafted external username from matching a namespace:name shorthand.
	if strings.HasPrefix(userInfo.Username, "system:") &&
		!strings.HasPrefix(userInfo.Username, "system:serviceaccount:") &&
		slices.Contains(entries, userInfo.Username) {
		return true
	}
	ns, name, ok := ParseServiceAccount(userInfo)
	if !ok {
		return false
	}
	return slices.Contains(entries, ns+":"+name)
}

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

package config

import "time"

const (
	WebhookName        = "flux-drift-webhook.fluxcd.io"
	WebhookPath        = "/validate"
	DefaultWebhookPort = 9443
	DefaultMetricsPort = 8080
	DefaultHealthPort  = 8081
	DefaultCertDir     = "/certs"

	KustomizeLabelName      = "kustomize.toolkit.fluxcd.io/name"
	KustomizeLabelNamespace = "kustomize.toolkit.fluxcd.io/namespace"
	HelmLabelName           = "helm.toolkit.fluxcd.io/name"
	HelmLabelNamespace      = "helm.toolkit.fluxcd.io/namespace"

	BypassAnnotation = "fluxcd.io/drift-prevention-bypass"
	BypassValue      = "disabled"

	// KustomizeReconcileAnnotation set to "disabled" makes kustomize-controller
	// skip the object entirely; drift prevention on such an object would be
	// incoherent (Flux neither corrects nor reapplies it).
	KustomizeReconcileAnnotation = "kustomize.toolkit.fluxcd.io/reconcile"
	ReconcileDisabledValue       = "disabled"

	FluxNamespaceDefault = "flux-system"

	DefaultDiscoveryInterval = 5 * time.Minute

	// Excluded to avoid infinite loops with our own VWC
	ExcludedGroupAdmission = "admissionregistration.k8s.io"
)

var fluxServiceAccounts = []string{
	"kustomize-controller",
	"helm-controller",
	"source-controller",
	"notification-controller",
	"image-reflector-controller",
	"image-automation-controller",
}

// fluxReconcilerServiceAccounts are the impersonation service accounts used by
// Flux in multi-tenant mode. Kustomizations reconcile tenant resources by
// impersonating these SAs in the tenant namespace, so they are recognised as
// owning Flux controllers regardless of their namespace.
var fluxReconcilerServiceAccounts = []string{
	"flux-reconciler",
}

// defaultSystemControllerServiceAccounts are Kubernetes control-plane
// controllers that legitimately act on Flux-labelled objects as part of normal
// cluster lifecycle:
//   - on CREATE they create objects that inherit a parent's Flux labels (e.g.
//     the endpoints/endpointslice controllers copy a Service's labels onto its
//     Endpoints/EndpointSlice);
//   - on DELETE they remove Flux-applied objects (the garbage collector during
//     cascade deletion; the TTL-after-finished and CronJob controllers cleaning
//     up completed Jobs).
//
// Entries are "namespace:name" service-account shorthands, or full usernames
// for non-SA component identities (reserved "system:" names). Operators can
// extend the list via --system-controller-sas without rebuilding the operator.
// Consulted by IsSystemController on both CREATE and DELETE.
var defaultSystemControllerServiceAccounts = []string{
	"kube-system:generic-garbage-collector",
	"kube-system:endpoint-controller",
	"kube-system:endpointslice-controller",
	"kube-system:endpointslicemirroring-controller",
	"kube-system:ttl-after-finished-controller",
	"kube-system:cronjob-controller",
	// The apiserver's CRD finalizer (customresourcecleanup) deletes all
	// instances of a deleted CRD as system:apiserver; without this, a
	// Flux-pruned CRD wedges in Terminating.
	"system:apiserver",
	// KCM runs every controller under this single identity when started
	// without --use-service-account-credentials.
	"system:kube-controller-manager",
}

var excludedNamespaces = []string{
	"kube-system",
	"kube-public",
	"kube-node-lease",
	"flux-system",
}

var excludedGroups = []string{
	ExcludedGroupAdmission,
}

// FluxServiceAccounts returns the names of the Flux controller service accounts.
func FluxServiceAccounts() []string { return fluxServiceAccounts }

// FluxReconcilerServiceAccounts returns the impersonation service accounts used
// by Flux in multi-tenant mode.
func FluxReconcilerServiceAccounts() []string { return fluxReconcilerServiceAccounts }

// ExcludedNamespaces returns the namespaces excluded from drift prevention.
func ExcludedNamespaces() []string { return excludedNamespaces }

// ExcludedGroups returns the API groups excluded from the webhook rules.
func ExcludedGroups() []string { return excludedGroups }

// DefaultSystemControllerServiceAccounts returns the built-in control-plane
// service accounts allowed to create Flux-labelled derived resources.
func DefaultSystemControllerServiceAccounts() []string { return defaultSystemControllerServiceAccounts }

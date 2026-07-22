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
	"github.com/pmialon/flux-drift-webhook/internal/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ManagedBy values identifying which Flux controller owns a resource.
const (
	ManagedByKustomization = "kustomization"
	ManagedByHelmRelease   = "helmrelease"
)

// FluxManagementInfo describes whether a resource is Flux-managed and, if so,
// which controller owns it.
type FluxManagementInfo struct {
	IsManaged      bool
	ManagedBy      string // ManagedByKustomization or ManagedByHelmRelease
	ControllerName string
	ControllerNS   string
}

// GetFluxManagementInfo returns Flux ownership info for the given labels.
// Kustomisation takes precedence when both label sets are present.
func GetFluxManagementInfo(labels map[string]string) FluxManagementInfo {
	if name, ok := labels[config.KustomizeLabelName]; ok {
		return FluxManagementInfo{
			IsManaged:      true,
			ManagedBy:      ManagedByKustomization,
			ControllerName: name,
			ControllerNS:   labels[config.KustomizeLabelNamespace],
		}
	}

	if name, ok := labels[config.HelmLabelName]; ok {
		return FluxManagementInfo{
			IsManaged:      true,
			ManagedBy:      ManagedByHelmRelease,
			ControllerName: name,
			ControllerNS:   labels[config.HelmLabelNamespace],
		}
	}

	return FluxManagementInfo{IsManaged: false}
}

// HasBypassAnnotation reports whether the bypass annotation is set to the
// disabling value.
func HasBypassAnnotation(annotations map[string]string) bool {
	return annotations[config.BypassAnnotation] == config.BypassValue
}

// IsBeingDeleted reports whether the object has a deletion timestamp set.
func IsBeingDeleted(obj metav1.ObjectMeta) bool {
	return obj.DeletionTimestamp != nil
}

// IsReconcileDisabled reports whether kustomize-controller is told to skip the
// object (kustomize.toolkit.fluxcd.io/reconcile: disabled).
func IsReconcileDisabled(annotations map[string]string) bool {
	return annotations[config.KustomizeReconcileAnnotation] == config.ReconcileDisabledValue
}

// protectionDisablingAnnotationAdded reports whether the new annotations
// introduce an active protection-disabling annotation (drift-prevention bypass
// or Flux reconcile: disabled) absent from the old annotations, returning the
// offending key. Such annotations must reach the object through Git.
func protectionDisablingAnnotationAdded(oldAnnotations, newAnnotations map[string]string) (string, bool) {
	if !HasBypassAnnotation(oldAnnotations) && HasBypassAnnotation(newAnnotations) {
		return config.BypassAnnotation, true
	}
	if !IsReconcileDisabled(oldAnnotations) && IsReconcileDisabled(newAnnotations) {
		return config.KustomizeReconcileAnnotation, true
	}
	return "", false
}

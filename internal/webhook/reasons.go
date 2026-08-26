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

// Decision reasons — the values of the `decision` label on
// flux_drift_webhook_requests_total. This label is the operator contract:
// dashboards, alerts and the enforce-rollout triage key off it, so every
// reason lives here as a constant (a typo in a literal would compile and
// silently fork a metric series) and AllDecisionReasons pins the full set for
// the contract test and the documentation.
const (
	ReasonAllowedNotManaged            = "allowed_not_managed"
	ReasonAllowedNamespaceFilter       = "allowed_namespace_filter"
	ReasonAllowedOwningFluxController  = "allowed_owning_flux_controller"
	ReasonAllowedBypassAnnotation      = "allowed_bypass_annotation"
	ReasonAllowedReconcileDisabled     = "allowed_reconcile_disabled"
	ReasonAllowedDeletionInProgress    = "allowed_deletion_in_progress"
	ReasonAllowedNamespaceTerminating  = "allowed_namespace_terminating"
	ReasonAllowedNoFluxManagedFields   = "allowed_no_flux_managed_fields"
	ReasonAllowedNoFieldConflict       = "allowed_no_field_conflict"
	ReasonAllowedDriftIgnoredField     = "allowed_drift_ignored_field"
	ReasonAllowedOwnedResource         = "allowed_owned_resource"
	ReasonAllowedSystemController      = "allowed_system_controller"
	ReasonAllowedNotInOwnerInventory   = "allowed_not_in_owner_inventory"
	ReasonAllowedSubresource           = "allowed_subresource"
	ReasonAllowedExcludedGroup         = "allowed_excluded_group"
	ReasonAllowedUnknownOperation      = "allowed_unknown_operation"
	ReasonDeniedMetadataError          = "denied_metadata_error"
	ReasonDeniedParseError             = "denied_parse_error"
	ReasonDeniedManagedFieldsError     = "denied_managed_fields_error"
	ReasonDeniedManagedFieldsTampered  = "denied_managed_fields_tampered"
	ReasonDeniedBypassAnnotationAdded  = "denied_bypass_annotation_added"
	ReasonDeniedCreateFluxLabels       = "denied_create_flux_labels"
	ReasonDeniedCreateInventoryUnavail = "denied_create_inventory_unavailable"
	ReasonDeniedDeleteFluxManaged      = "denied_delete_flux_managed"
	ReasonDeniedUpdateFluxManaged      = "denied_update_flux_managed_fields"
	ReasonDeniedWrongFluxController    = "denied_wrong_flux_controller"
)

// AllDecisionReasons is the complete decision-label contract, allowed reasons
// first. Adding a reason to the handler without adding it here (and to the
// docs) fails the contract test.
var AllDecisionReasons = []string{
	ReasonAllowedNotManaged,
	ReasonAllowedNamespaceFilter,
	ReasonAllowedOwningFluxController,
	ReasonAllowedBypassAnnotation,
	ReasonAllowedReconcileDisabled,
	ReasonAllowedDeletionInProgress,
	ReasonAllowedNamespaceTerminating,
	ReasonAllowedNoFluxManagedFields,
	ReasonAllowedNoFieldConflict,
	ReasonAllowedDriftIgnoredField,
	ReasonAllowedOwnedResource,
	ReasonAllowedSystemController,
	ReasonAllowedNotInOwnerInventory,
	ReasonAllowedSubresource,
	ReasonAllowedExcludedGroup,
	ReasonAllowedUnknownOperation,
	ReasonDeniedMetadataError,
	ReasonDeniedParseError,
	ReasonDeniedManagedFieldsError,
	ReasonDeniedManagedFieldsTampered,
	ReasonDeniedBypassAnnotationAdded,
	ReasonDeniedCreateFluxLabels,
	ReasonDeniedCreateInventoryUnavail,
	ReasonDeniedDeleteFluxManaged,
	ReasonDeniedUpdateFluxManaged,
	ReasonDeniedWrongFluxController,
}

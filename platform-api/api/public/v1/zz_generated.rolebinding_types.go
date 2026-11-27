package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// RoleBinding binds an email principal to a PlatformRole or a namespace Role.
// PlatformRole is cluster-scoped, but the binding itself remains scoped to the
// namespace in which access is granted.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
type RoleBinding struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec RoleBindingSpec `json:"spec"`
}

// RoleBindingList is a list of RoleBinding resources.
//
// +kubebuilder:object:root=true
type RoleBindingList struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ListMeta `json:"metadata,omitempty"`

	// +optional
	Items []RoleBinding `json:"items"`
}

// RoleBindingSpec identifies the principal and role granted in a namespace.
//
//nolint:kubeapilinter,geckoconventions // spec.conditions contains authorization requirements, not status conditions; only emitted Cedar is bounded.
type RoleBindingSpec struct {

	// +kubebuilder:validation:MinLength=1
	// +required
	Subject string `json:"subject"`

	// +required
	RoleRef RoleRef `json:"roleRef"`

	// conditions narrow this binding's grants. Requirements are ANDed, including
	// repeated keys. Values are ORed; '*' is the only wildcard. Values are opaque
	// strings, not validated region/name identifiers. Omitted or empty conditions
	// add no restriction. PUT omission clears; merge PATCH omission preserves.
	// Name requirements need an explicit metadata.name at authorization time;
	// generateName alone cannot match. An independent grant may allow generation.

	// +optional
	// +listType=atomic
	Conditions []RoleBindingRequirement `json:"conditions,omitempty"`
}

// RoleRef identifies either a cluster-scoped PlatformRole or a namespaced
// Role. APIGroup is explicit so references cannot silently cross API groups.
type RoleRef struct {

	// +required
	Kind string `json:"kind"`

	// +required
	Name string `json:"name"`

	// +required
	APIGroup string `json:"apiGroup"`
}

// RoleBindingConditionKey selects a normalized target attribute.
// The name key requires explicit metadata.name; generateName alone cannot match.
// An independent grant without a name requirement may allow name generation.
// +kubebuilder:validation:Enum=name;region
type RoleBindingConditionKey string

// RoleBindingConditionOperator determines membership in the value alternatives.
// +kubebuilder:validation:Enum=In;NotIn
type RoleBindingConditionOperator string

// RoleBindingRequirement narrows the target objects accessible through a binding.
//
//nolint:geckoconventions // Schema-only string values and arrays have no per-field size/count limits; emitted Cedar is bounded.
type RoleBindingRequirement struct {
	// key is name or region. Region applies only to Cluster and NodePool targets.

	// +required
	Key RoleBindingConditionKey `json:"key"`
	// operator is In (any match) or NotIn (no match). Both require a present attribute.

	// +required
	Operator RoleBindingConditionOperator `json:"operator"`
	// values are literal strings except '*' matches zero or more characters.
	// Matching is case-sensitive and whole-string. No escape language is supported.
	// Empty or whitespace-only strings are rejected; other values are not trimmed.
	// At least one value is required; duplicates are allowed.
	// Misspelled exclusions may broaden access.

	// +required
	// +listType=atomic
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:MinLength=1
	Values []string `json:"values"`
}

func init() {
	register(&RoleBinding{}, &RoleBindingList{})
}

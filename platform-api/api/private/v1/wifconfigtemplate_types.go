package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// WIFConfigTemplate describes an immutable installation IAM contract.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +orlop:public-verbs: get,list
type WIFConfigTemplate struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// spec contains the IAM contract and its publication eligibility.
	// +orlop:public
	// +required
	Spec WIFConfigTemplateSpec `json:"spec"`
}

// WIFConfigTemplateList contains IAM templates.
// +kubebuilder:object:root=true
type WIFConfigTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ListMeta `json:"metadata,omitempty"`
	// +orlop:public
	// +kubebuilder:validation:MaxItems=10000
	Items []WIFConfigTemplate `json:"items"`
}

// WIFConfigTemplateSpec defines a versioned contract and mutable eligibility.
type WIFConfigTemplateSpec struct {
	// verified permits new use when explicitly set to true. Omission means false.
	// Changing verification does not change the IAM contract digest.
	// +orlop:public
	// +optional
	Verified *bool `json:"verified,omitempty"` //nolint:kubeapilinter // Explicit binary GitOps eligibility is the agreed public contract.
	// schemaVersion identifies the executable IAM protocol, independently of the API envelope.
	// +orlop:public
	// +required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1
	SchemaVersion int32 `json:"schemaVersion"`
	// iam contains the immutable identities, naming rules and grants.
	// +orlop:public
	// +required
	IAM WIFConfigIAM `json:"iam"`
}

// WIFConfigIAM defines project-scoped component IAM and exact-worker-SA actAs.
type WIFConfigIAM struct {
	// requiredComponents enumerates the complete component identity set.
	// +orlop:public
	// +required
	// +listType=set
	// +kubebuilder:validation:MinItems=7
	// +kubebuilder:validation:MaxItems=7
	// +kubebuilder:validation:items:MaxLength=32
	RequiredComponents []string `json:"requiredComponents"`
	// naming defines deterministic installation-owned resource names.
	// +orlop:public
	// +required
	Naming WIFConfigNaming `json:"naming"`
	// serviceAccounts defines component accounts and guest federation subjects.
	// +orlop:public
	// +required
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=7
	// +kubebuilder:validation:MaxItems=7
	ServiceAccounts []WIFConfigServiceAccount `json:"serviceAccounts"`
	// roles defines installation-owned custom-role permissions.
	// +orlop:public
	// +required
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	Roles []WIFConfigRole `json:"roles"`
	// roleBindings grants custom roles at project scope.
	// +orlop:public
	// +required
	// +listType=atomic
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=128
	RoleBindings []WIFConfigRoleBinding `json:"roleBindings"`
	// workloadIdentityBindings defines guest federation access to component accounts.
	// +orlop:public
	// +required
	WorkloadIdentityBindings WIFConfigWorkloadIdentityBindings `json:"workloadIdentityBindings"`
	// serviceAccountRoleBindings grants actAs on the exact installation worker account.
	// +orlop:public
	// +required
	// +listType=atomic
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	ServiceAccountRoleBindings []WIFConfigServiceAccountRoleBinding `json:"serviceAccountRoleBindings"`
}

// WIFConfigNaming bounds deterministic naming without truncation.
type WIFConfigNaming struct {
	// maxInfraIDLength bounds the installation identifier.
	// +orlop:public
	// +required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=12
	MaxInfraIDLength int32 `json:"maxInfraIDLength"`
	// maxCustomRoleIDLength bounds rendered custom role identifiers.
	// +orlop:public
	// +required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	MaxCustomRoleIDLength int32 `json:"maxCustomRoleIDLength"`
	// maxServiceAccountIDLength bounds rendered service account identifiers.
	// +orlop:public
	// +required
	// +kubebuilder:validation:Minimum=6
	// +kubebuilder:validation:Maximum=30
	MaxServiceAccountIDLength int32 `json:"maxServiceAccountIDLength"`
	// maxWorkloadIdentityIDLength bounds rendered pool and provider identifiers.
	// +orlop:public
	// +required
	// +kubebuilder:validation:Minimum=4
	// +kubebuilder:validation:Maximum=32
	MaxWorkloadIdentityIDLength int32 `json:"maxWorkloadIdentityIDLength"`
	// customRoleIDTemplate uses the supported infraIDHex and role placeholders.
	// +orlop:public
	// +required
	// +kubebuilder:validation:MaxLength=128
	CustomRoleIDTemplate string `json:"customRoleIDTemplate"`
	// workloadIdentityPoolIDTemplate uses the supported infraID placeholder.
	// +orlop:public
	// +required
	// +kubebuilder:validation:MaxLength=128
	WorkloadIdentityPoolIDTemplate string `json:"workloadIdentityPoolIDTemplate"`
	// workloadIdentityProviderIDTemplate uses the supported infraID placeholder.
	// +orlop:public
	// +required
	// +kubebuilder:validation:MaxLength=128
	WorkloadIdentityProviderIDTemplate string `json:"workloadIdentityProviderIDTemplate"`
}

// WIFConfigServiceAccount defines one component identity.
type WIFConfigServiceAccount struct {
	// name identifies this component within the contract.
	// +orlop:public
	// +required
	// +kubebuilder:validation:MaxLength=32
	Name string `json:"name"`
	// accountIDTemplate defines its installation-owned account ID.
	// +orlop:public
	// +required
	// +kubebuilder:validation:MaxLength=128
	AccountIDTemplate string `json:"accountIDTemplate"`
	// workloadIdentitySubjects lists exact guest Kubernetes service-account subjects.
	// +orlop:public
	// +required
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=256
	WorkloadIdentitySubjects []string `json:"workloadIdentitySubjects"`
}

// WIFConfigRole defines one logical custom role.
type WIFConfigRole struct {
	// name is the logical role identifier, not a customer project role name.
	// +orlop:public
	// +required
	// +kubebuilder:validation:MaxLength=32
	Name string `json:"name"`
	// permissions is the exact set of GCP permissions to grant.
	// +orlop:public
	// +required
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=512
	// +kubebuilder:validation:items:MaxLength=128
	Permissions []string `json:"permissions"`
}

// WIFConfigProjectScope restricts custom-role grants to the installation project.
type WIFConfigProjectScope struct {
	// kind identifies project scope.
	// +orlop:public
	// +required
	// +kubebuilder:validation:Enum=Project
	// +kubebuilder:validation:MaxLength=7
	Kind string `json:"kind"`
}

// WIFConfigRoleBinding grants a custom role to one component.
type WIFConfigRoleBinding struct {
	// serviceAccountRef references a component account.
	// +orlop:public
	// +required
	// +kubebuilder:validation:MaxLength=32
	ServiceAccountRef string `json:"serviceAccountRef"`
	// roleRef references a logical custom role.
	// +orlop:public
	// +required
	// +kubebuilder:validation:MaxLength=32
	RoleRef string `json:"roleRef"`
	// scope restricts this grant to the customer project.
	// +orlop:public
	// +required
	Scope WIFConfigProjectScope `json:"scope"`
}

// WIFConfigWorkloadIdentityBindings specifies guest federation grants.
type WIFConfigWorkloadIdentityBindings struct {
	// role must be the standard workload identity user role.
	// +orlop:public
	// +required
	// +kubebuilder:validation:Enum="roles/iam.workloadIdentityUser"
	// +kubebuilder:validation:MaxLength=30
	Role string `json:"role"`
	// serviceAccountRefs identifies all federated component accounts.
	// +orlop:public
	// +required
	// +listType=set
	// +kubebuilder:validation:MinItems=7
	// +kubebuilder:validation:MaxItems=7
	// +kubebuilder:validation:items:MaxLength=32
	ServiceAccountRefs []string `json:"serviceAccountRefs"`
}

// WIFConfigWorkerScope identifies the single exact worker account installation input.
type WIFConfigWorkerScope struct {
	// kind identifies service-account scope rather than project-wide actAs.
	// +orlop:public
	// +required
	// +kubebuilder:validation:Enum=ServiceAccount
	// +kubebuilder:validation:MaxLength=14
	Kind string `json:"kind"`
	// reference selects the exact worker service account supplied at installation.
	// +orlop:public
	// +required
	// +kubebuilder:validation:Enum=WorkerServiceAccount
	// +kubebuilder:validation:MaxLength=20
	Reference string `json:"reference"` //nolint:kubeapilinter // SchemaVersion 1 symbolic installation input, not an API object reference; preserves the reviewed protocol.
}

// WIFConfigServiceAccountRoleBinding defines an exact-worker-SA grant.
type WIFConfigServiceAccountRoleBinding struct {
	// role must be the standard service account user role.
	// +orlop:public
	// +required
	// +kubebuilder:validation:Enum="roles/iam.serviceAccountUser"
	// +kubebuilder:validation:MaxLength=28
	Role string `json:"role"`
	// scope identifies the exact worker account.
	// +orlop:public
	// +required
	Scope WIFConfigWorkerScope `json:"scope"`
	// memberServiceAccountRefs lists the components granted actAs.
	// +orlop:public
	// +required
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=6
	// +kubebuilder:validation:items:MaxLength=32
	MemberServiceAccountRefs []string `json:"memberServiceAccountRefs"`
}

// WIFConfigTemplateReference pins both resource identity and executable content.
type WIFConfigTemplateReference struct {
	// name is the canonical release boundary of the template.
	// +orlop:public
	// +required
	// +kubebuilder:validation:MaxLength=32
	Name string `json:"name"`
	// uid identifies the exact retained template resource.
	// +orlop:public
	// +required
	// +kubebuilder:validation:MaxLength=128
	UID string `json:"uid"`
	// digest is the SHA-256 of the canonical schemaVersion and IAM contract.
	// +orlop:public
	// +required
	// +kubebuilder:validation:MaxLength=71
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`
}

// ClusterIAMSpec binds application history to one immutable worker identity.
type ClusterIAMSpec struct {
	// workerServiceAccount is the exact default Compute service-account email.
	// +orlop:public
	// +required
	// +kubebuilder:validation:MaxLength=254
	WorkerServiceAccount string `json:"workerServiceAccount"`
	// templateApplications records exact contracts asserted applied by the customer.
	// Records are immutable and append-only; this is not independent cloud attestation.
	// +orlop:public
	// +required
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=128
	TemplateApplications []WIFConfigTemplateReference `json:"templateApplications"`
}

func init() { register(&WIFConfigTemplate{}, &WIFConfigTemplateList{}) }

package v1

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
)

var (
	wifBoundary    = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	wifRoleName    = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]{0,31}$`)
	wifPermission  = regexp.MustCompile(`^[a-z][a-z0-9]*\.[a-zA-Z][a-zA-Z0-9]*\.[a-zA-Z][a-zA-Z0-9]*$`)
	wifSubject     = regexp.MustCompile(`^system:serviceaccount:[a-z0-9]([-a-z0-9]*[a-z0-9])?:[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	wifDigest      = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	wifWorkerEmail = regexp.MustCompile(`^[1-9][0-9]{0,19}-compute@developer\.gserviceaccount\.com$`)
	wifInfraID     = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,10}[a-z0-9])?$`)
)

var wifComponents = []string{"cloud-controller", "cloud-network", "ctrlplane-op", "gcp-pd-csi", "image-registry", "nodepool-mgmt"}

// ValidateCreate checks structure, completeness and supported execution semantics.
func (w *WIFConfigTemplate) ValidateCreate(_ context.Context) error {
	if len(w.Name) > 32 || !wifBoundary.MatchString(w.Name) {
		return fmt.Errorf("metadata.name must be a canonical numeric X.Y.Z boundary of at most 32 characters")
	}
	return validateWIFConfigSpec(w.Spec)
}

// ValidateUpdate permits eligibility and metadata changes, but not contract edits.
func (w *WIFConfigTemplate) ValidateUpdate(ctx context.Context, oldObj runtime.Object) error {
	old, ok := oldObj.(*WIFConfigTemplate)
	if !ok {
		return fmt.Errorf("expected a WIFConfigTemplate old object")
	}
	if err := w.ValidateCreate(ctx); err != nil {
		return err
	}
	nextDigest, err := WIFConfigDigest(w.Spec)
	if err != nil {
		return err
	}
	oldDigest, err := WIFConfigDigest(old.Spec)
	if err != nil {
		return err
	}
	if nextDigest != oldDigest {
		return fmt.Errorf("spec.schemaVersion and spec.iam are immutable")
	}
	return nil
}

// ValidateDelete retains catalog contracts and eliminates reference/delete races.
// Deliberate retirement is not supported by this initial API.
func (w *WIFConfigTemplate) ValidateDelete(_ context.Context) error {
	return fmt.Errorf("WIFConfigTemplate deletion is disabled; set spec.verified=false to disable new use")
}

func exactWIFSet(values, expected []string) bool {
	a, b := slices.Clone(values), slices.Clone(expected)
	sort.Strings(a)
	sort.Strings(b)
	return slices.Equal(a, b)
}

func uniqueWIFStrings(values []string, maxCount, maxLength int, valid func(string) bool) bool {
	if len(values) == 0 || len(values) > maxCount {
		return false
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if value == "" || len(value) > maxLength || seen[value] || !valid(value) {
			return false
		}
		seen[value] = true
	}
	return true
}

// SchemaVersion 1 validation is a compatibility contract: never tighten it for
// previously accepted executable data. New execution semantics need a new schema.
func validateWIFConfigSpec(spec WIFConfigTemplateSpec) error {
	if spec.SchemaVersion != 1 {
		return fmt.Errorf("spec.schemaVersion must be 1")
	}
	iam := spec.IAM
	if !exactWIFSet(iam.RequiredComponents, wifComponents) || len(iam.ServiceAccounts) != len(wifComponents) {
		return fmt.Errorf("spec.iam must define all six required components exactly once")
	}
	if err := validateWIFNaming(iam); err != nil {
		return err
	}
	return validateWIFGrants(iam)
}

func validateWIFNaming(iam WIFConfigIAM) error {
	n := iam.Naming
	if n.MaxInfraIDLength < 1 || n.MaxInfraIDLength > 12 || n.MaxCustomRoleIDLength < 1 || n.MaxCustomRoleIDLength > 64 || n.MaxServiceAccountIDLength < 6 || n.MaxServiceAccountIDLength > 30 || n.MaxWorkloadIdentityIDLength < 4 || n.MaxWorkloadIdentityIDLength > 32 {
		return fmt.Errorf("spec.iam.naming contains unsupported identifier bounds")
	}
	if n.CustomRoleIDTemplate != "hcp_{infraIDHex}_{role}" || n.WorkloadIdentityPoolIDTemplate != "{infraID}-wi-pool" || n.WorkloadIdentityProviderIDTemplate != "{infraID}-k8s-provider" {
		return fmt.Errorf("spec.iam.naming must use the supported schemaVersion 1 naming templates")
	}
	if n.MaxInfraIDLength+int32(len("-k8s-provider")) > n.MaxWorkloadIdentityIDLength {
		return fmt.Errorf("spec.iam.naming cannot render the maximum infra ID without truncation")
	}
	accounts := map[string]bool{}
	for _, account := range iam.ServiceAccounts {
		if !slices.Contains(wifComponents, account.Name) || accounts[account.Name] || account.AccountIDTemplate != "{infraID}-"+account.Name {
			return fmt.Errorf("spec.iam.serviceAccounts must contain unique supported components and account templates")
		}
		if n.MaxInfraIDLength+1+int32(len(account.Name)) > n.MaxServiceAccountIDLength {
			return fmt.Errorf("spec.iam.serviceAccounts cannot render the maximum infra ID without truncation")
		}
		if !uniqueWIFStrings(account.WorkloadIdentitySubjects, 16, 256, wifSubject.MatchString) {
			return fmt.Errorf("spec.iam.serviceAccounts requires bounded unique exact guest service-account subjects")
		}
		accounts[account.Name] = true
	}
	return nil
}

func validateWIFGrants(iam WIFConfigIAM) error {
	n := iam.Naming
	accounts := map[string]bool{}
	for _, name := range wifComponents {
		accounts[name] = true
	}
	if len(iam.Roles) == 0 || len(iam.Roles) > 32 {
		return fmt.Errorf("spec.iam.roles must contain between 1 and 32 roles")
	}
	roles := map[string]bool{}
	for _, role := range iam.Roles {
		if !wifRoleName.MatchString(role.Name) || roles[role.Name] || !uniqueWIFStrings(role.Permissions, 512, 128, wifPermission.MatchString) {
			return fmt.Errorf("spec.iam.roles requires unique logical names and bounded unique GCP permissions")
		}
		if 5+2*n.MaxInfraIDLength+int32(len(role.Name)) > n.MaxCustomRoleIDLength {
			return fmt.Errorf("spec.iam.roles cannot render the maximum infra ID without truncation")
		}
		roles[role.Name] = true
	}
	if len(iam.RoleBindings) == 0 || len(iam.RoleBindings) > 128 {
		return fmt.Errorf("spec.iam.roleBindings must contain between 1 and 128 grants")
	}
	grants, usedRoles, boundAccounts := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, grant := range iam.RoleBindings {
		key := grant.ServiceAccountRef + "/" + grant.RoleRef
		if !accounts[grant.ServiceAccountRef] || !roles[grant.RoleRef] || grant.Scope.Kind != "Project" || grants[key] {
			return fmt.Errorf("spec.iam.roleBindings requires unique project grants referencing defined accounts and roles")
		}
		grants[key], usedRoles[grant.RoleRef], boundAccounts[grant.ServiceAccountRef] = true, true, true
	}
	if len(usedRoles) != len(roles) || len(boundAccounts) != len(accounts) {
		return fmt.Errorf("spec.iam.roleBindings must cover every component and custom role")
	}
	if iam.WorkloadIdentityBindings.Role != "roles/iam.workloadIdentityUser" || !exactWIFSet(iam.WorkloadIdentityBindings.ServiceAccountRefs, wifComponents) {
		return fmt.Errorf("spec.iam.workloadIdentityBindings must grant workloadIdentityUser to all six components")
	}
	if len(iam.ServiceAccountRoleBindings) != 1 {
		return fmt.Errorf("spec.iam.serviceAccountRoleBindings must define one exact-worker-SA actAs grant")
	}
	grant := iam.ServiceAccountRoleBindings[0]
	if grant.Role != "roles/iam.serviceAccountUser" || grant.Scope.Kind != "ServiceAccount" || grant.Scope.Reference != "WorkerServiceAccount" || !exactWIFSet(grant.MemberServiceAccountRefs, []string{"gcp-pd-csi", "nodepool-mgmt"}) {
		return fmt.Errorf("spec.iam.serviceAccountRoleBindings must grant only CSI and NodePool management actAs on WorkerServiceAccount")
	}
	return nil
}

// WIFConfigDigest returns sha256:<hex> over encoding/json of the schemaVersion
// and IAM fields, with all set/map lists and grants sorted by their complete keys.
// Verification and resource metadata are excluded. Sorting never mutates input.
func WIFConfigDigest(spec WIFConfigTemplateSpec) (string, error) {
	data, err := json.Marshal(spec.IAM)
	if err != nil {
		return "", fmt.Errorf("encode IAM contract: %w", err)
	}
	var iam WIFConfigIAM
	if err := json.Unmarshal(data, &iam); err != nil {
		return "", fmt.Errorf("copy IAM contract: %w", err)
	}
	sort.Strings(iam.RequiredComponents)
	sort.Slice(iam.ServiceAccounts, func(i, j int) bool { return iam.ServiceAccounts[i].Name < iam.ServiceAccounts[j].Name })
	for i := range iam.ServiceAccounts {
		sort.Strings(iam.ServiceAccounts[i].WorkloadIdentitySubjects)
	}
	sort.Slice(iam.Roles, func(i, j int) bool { return iam.Roles[i].Name < iam.Roles[j].Name })
	for i := range iam.Roles {
		sort.Strings(iam.Roles[i].Permissions)
	}
	sort.Slice(iam.RoleBindings, func(i, j int) bool {
		a, b := iam.RoleBindings[i], iam.RoleBindings[j]
		return a.ServiceAccountRef+"/"+a.RoleRef < b.ServiceAccountRef+"/"+b.RoleRef
	})
	sort.Strings(iam.WorkloadIdentityBindings.ServiceAccountRefs)
	for i := range iam.ServiceAccountRoleBindings {
		sort.Strings(iam.ServiceAccountRoleBindings[i].MemberServiceAccountRefs)
	}
	spec.Verified, spec.IAM = nil, iam
	data, err = json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("encode canonical IAM contract: %w", err)
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(data)), nil
}

func validateWIFReferenceShape(ref WIFConfigTemplateReference) error {
	if len(ref.Name) > 32 || !wifBoundary.MatchString(ref.Name) || ref.UID == "" || len(ref.UID) > 128 || !wifDigest.MatchString(ref.Digest) {
		return fmt.Errorf("template reference requires canonical name, resource UID and sha256 digest")
	}
	return nil
}

func validateWIFReference(ctx context.Context, ref WIFConfigTemplateReference, requireVerified bool, infraID string) error {
	if err := validateWIFReferenceShape(ref); err != nil {
		return err
	}
	lookup := getValidatorDeps().WIFConfigTemplate
	if lookup == nil {
		return fmt.Errorf("IAM catalog validation is unavailable")
	}
	template, err := lookup(ctx, ref.Name)
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("template is not present in the IAM catalog")
	}
	if err != nil {
		return fmt.Errorf("IAM catalog is temporarily unavailable")
	}
	if template == nil || template.Name != ref.Name || string(template.UID) != ref.UID {
		return fmt.Errorf("template reference does not identify the retained catalog resource")
	}
	digest, err := WIFConfigDigest(template.Spec)
	if err != nil || digest != ref.Digest {
		return fmt.Errorf("template reference does not match the catalog contract digest")
	}
	if requireVerified && (template.Spec.Verified == nil || !*template.Spec.Verified) {
		return fmt.Errorf("template must be verified before recording a new application")
	}
	if int32(len(infraID)) > template.Spec.IAM.Naming.MaxInfraIDLength {
		return fmt.Errorf("installation infraID exceeds the referenced template's naming limit")
	}
	return nil
}

func (c *Cluster) validateIAM(ctx context.Context, old *Cluster) error {
	var nextIAM, oldIAM *ClusterIAMSpec
	if c.Spec.Platform.GCP != nil {
		nextIAM = c.Spec.Platform.GCP.IAM
	}
	if old != nil && old.Spec.Platform.GCP != nil {
		oldIAM = old.Spec.Platform.GCP.IAM
	}
	if oldIAM != nil {
		if nextIAM == nil || oldIAM.WorkerServiceAccount != nextIAM.WorkerServiceAccount {
			return fmt.Errorf("spec.platform.gcp.iam worker identity and existing applications cannot be removed or changed")
		}
		// Existing applications are meaningful only under the original immutable installation identity.
		if c.Spec.InfraID != old.Spec.InfraID || c.Spec.IssuerURL != old.Spec.IssuerURL || c.Spec.Platform.GCP.ProjectID != old.Spec.Platform.GCP.ProjectID || !reflect.DeepEqual(c.Spec.Platform.GCP.WorkloadIdentity, old.Spec.Platform.GCP.WorkloadIdentity) {
			return fmt.Errorf("IAM application installation identity is immutable")
		}
	}
	if nextIAM == nil {
		return nil
	} // Dispatch applicability is introduced in Phase 2.
	if err := c.validateIAMIdentity(nextIAM); err != nil {
		return err
	}
	return validateIAMApplications(ctx, nextIAM, oldIAM, c.Spec.InfraID)
}

func (c *Cluster) validateIAMIdentity(nextIAM *ClusterIAMSpec) error {
	if !wifWorkerEmail.MatchString(nextIAM.WorkerServiceAccount) || c.Spec.InfraID == "" || c.Spec.IssuerURL == "" || c.Spec.Platform.GCP.ProjectID == "" || c.Spec.Platform.GCP.WorkloadIdentity.ServiceAccountsRef == nil {
		return fmt.Errorf("IAM applications require an explicit installation identity, component accounts and default Compute worker account")
	}
	gcp := c.Spec.Platform.GCP
	if !wifInfraID.MatchString(c.Spec.InfraID) || gcp.WorkloadIdentity.PoolID != c.Spec.InfraID+"-wi-pool" || gcp.WorkloadIdentity.ProviderID != c.Spec.InfraID+"-k8s-provider" || nextIAM.WorkerServiceAccount != gcp.WorkloadIdentity.ProjectNumber+"-compute@developer.gserviceaccount.com" {
		return fmt.Errorf("IAM applications must use the installation's pool, provider and default Compute worker account")
	}
	identities := gcp.WorkloadIdentity.ServiceAccountsRef
	emails := map[string]string{"cloud-controller": identities.CloudControllerEmail, "cloud-network": identities.NetworkEmail, "ctrlplane-op": identities.ControlPlaneEmail, "gcp-pd-csi": identities.StorageEmail, "image-registry": identities.ImageRegistryEmail, "nodepool-mgmt": identities.NodePoolEmail}
	for component, email := range emails {
		if email != c.Spec.InfraID+"-"+component+"@"+gcp.ProjectID+".iam.gserviceaccount.com" {
			return fmt.Errorf("IAM applications must use the contract's installation-owned component accounts")
		}
	}
	return nil
}

func validateIAMApplications(ctx context.Context, nextIAM, oldIAM *ClusterIAMSpec, infraID string) error {
	if len(nextIAM.TemplateApplications) == 0 || len(nextIAM.TemplateApplications) > 128 {
		return fmt.Errorf("templateApplications must contain between 1 and 128 entries")
	}
	previous := map[string]WIFConfigTemplateReference{}
	if oldIAM != nil {
		for _, ref := range oldIAM.TemplateApplications {
			previous[ref.Name] = ref
		}
	}
	seen := map[string]bool{}
	for _, ref := range nextIAM.TemplateApplications {
		if seen[ref.Name] {
			return fmt.Errorf("templateApplications cannot contain duplicate template names")
		}
		seen[ref.Name] = true
		if err := validateWIFReferenceShape(ref); err != nil {
			return err
		}
		if prior, exists := previous[ref.Name]; exists {
			if prior != ref {
				return fmt.Errorf("templateApplications entries are immutable")
			}
			delete(previous, ref.Name)
			continue // Retained history stays valid even after verification is revoked.
		}
		if !getValidatorDeps().AllowIAMApplicationWrites {
			return fmt.Errorf("new IAM application writes are disabled until compatible API and controllers are deployed")
		}
		if err := validateWIFReference(ctx, ref, true, infraID); err != nil {
			return err
		}
	}
	if len(previous) != 0 {
		return fmt.Errorf("templateApplications entries cannot be removed")
	}
	return nil
}

// ValidateCreate validates an optional Version reference; resolution belongs to versionsync.
func (v *Version) ValidateCreate(ctx context.Context) error {
	if v.Spec.WIFConfigTemplateRef == nil {
		return nil
	}
	return validateWIFReference(ctx, *v.Spec.WIFConfigTemplateRef, false, "")
}

// ValidateUpdate pins a reference once set, allowing unrelated catalog metadata updates.
func (v *Version) ValidateUpdate(ctx context.Context, oldObj runtime.Object) error {
	old, ok := oldObj.(*Version)
	if !ok {
		return fmt.Errorf("expected a Version old object")
	}
	if old.Spec.WIFConfigTemplateRef != nil {
		if !reflect.DeepEqual(v.Spec.WIFConfigTemplateRef, old.Spec.WIFConfigTemplateRef) {
			return fmt.Errorf("spec.wifConfigTemplateRef is immutable once set")
		}
		return nil
	}
	return v.ValidateCreate(ctx)
}

// ValidateDelete permits ordinary upstream Version catalog aging.
func (v *Version) ValidateDelete(_ context.Context) error { return nil }

// IsWIFConfigVerified reports the single platform publication assertion.
func IsWIFConfigVerified(spec WIFConfigTemplateSpec) bool {
	return spec.Verified != nil && *spec.Verified
}

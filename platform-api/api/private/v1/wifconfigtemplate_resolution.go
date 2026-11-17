package v1

import (
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilversion "k8s.io/apimachinery/pkg/util/version"
)

const (
	// VersionAvailable is the version-sync-owned release eligibility condition.
	VersionAvailable = "Available"
	// IAMPermissionsReady describes exact-contract coverage for the dispatch target.
	// HC owns it on Clusters; the NodePool dispatcher owns it on NodePools.
	IAMPermissionsReady          = "IAMPermissionsReady"
	WIFTemplateNotFound          = "WIFConfigTemplateNotFound"
	WIFTemplateUnverified        = "WIFConfigTemplateUnverified"
	WIFTemplateIntegrityMismatch = "WIFConfigTemplateIntegrityMismatch"
	WIFTemplateReadFailed        = "WIFConfigTemplateReadFailed"
	WIFConfigUpdateRequired      = "WIFConfigUpdateRequired"
)

// SelectWIFConfigTemplate selects the highest same-major/minor numeric boundary
// at or below the release's numeric core. Prereleases explicitly inherit their
// X.Y.Z base; the original release identity is not modified. Verification does
// not influence selection. Nil means a successful complete snapshot had no match.
func SelectWIFConfigTemplate(release string, templates []WIFConfigTemplate) (*WIFConfigTemplate, error) {
	v, err := utilversion.ParseSemantic(release)
	if err != nil || v.String() != release {
		return nil, fmt.Errorf("release must be a canonical semantic version")
	}
	major, minor, patch := strconv.FormatUint(uint64(v.Major()), 10), strconv.FormatUint(uint64(v.Minor()), 10), strconv.FormatUint(uint64(v.Patch()), 10)
	var selected *WIFConfigTemplate
	var selectedPatch string
	for i := range templates {
		t := &templates[i]
		if len(t.Name) > 32 || !wifBoundary.MatchString(t.Name) {
			return nil, fmt.Errorf("catalog contains a malformed template boundary")
		}
		parts := strings.Split(t.Name, ".")
		if parts[0] != major || parts[1] != minor || compareWIFNumber(parts[2], patch) > 0 {
			continue
		}
		if selected == nil || compareWIFNumber(parts[2], selectedPatch) > 0 {
			selected, selectedPatch = t, parts[2]
		}
	}
	return selected, nil
}

// Inputs are canonical nonnegative decimal strings; length-first comparison is
// numeric and avoids overflow for the API's bounded but potentially large values.
func compareWIFNumber(a, b string) int {
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return strings.Compare(a, b)
}

// WIFConfigReference pins the resource and executable content, even if unverified.
func WIFConfigReference(t *WIFConfigTemplate) (*WIFConfigTemplateReference, error) {
	if t == nil || t.UID == "" {
		return nil, fmt.Errorf("template resource identity is missing")
	}
	if err := validateWIFConfigSpec(t.Spec); err != nil {
		return nil, err
	}
	digest, err := WIFConfigDigest(t.Spec)
	if err != nil {
		return nil, err
	}
	ref := &WIFConfigTemplateReference{Name: t.Name, UID: string(t.UID), Digest: digest}
	if err := validateWIFReferenceShape(*ref); err != nil {
		return nil, err
	}
	return ref, nil
}

// CheckWIFConfigReference checks an already-selected identity and eligibility.
// It never selects a different boundary or infers coverage from version ordering.
func CheckWIFConfigReference(ref *WIFConfigTemplateReference, t *WIFConfigTemplate) (string, string) {
	if ref == nil || t == nil {
		return WIFTemplateNotFound, "The release has no retained IAM template association"
	}
	actual, err := WIFConfigReference(t)
	if err != nil || *ref != *actual {
		return WIFTemplateIntegrityMismatch, "The pinned IAM template identity or content does not match the catalog"
	}
	if !IsWIFConfigVerified(t.Spec) {
		return WIFTemplateUnverified, "The pinned IAM template is not verified for new use"
	}
	return "", ""
}

// IsVersionAvailable requires a current positive condition and complete metadata.
// Consumers must also check the pinned contract before new dispatch, because
// asynchronous status alone cannot close a verification-revocation race.
func IsVersionAvailable(v *Version) bool {
	if v == nil || v.Spec.ReleaseImage == "" || len(v.Spec.ChannelGroups) == 0 || v.Spec.WIFConfigTemplateRef == nil {
		return false
	}
	if validateWIFReferenceShape(*v.Spec.WIFConfigTemplateRef) != nil {
		return false
	}
	c := meta.FindStatusCondition(v.Status.Conditions, VersionAvailable)
	return c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == v.Generation
}

// HasWIFConfigCoverage evaluates exact immutable contract identity, not ordering.
func HasWIFConfigCoverage(c *Cluster, ref *WIFConfigTemplateReference) bool {
	if c == nil || ref == nil || c.Spec.Platform.GCP == nil || c.Spec.Platform.GCP.IAM == nil {
		return false
	}
	for _, applied := range c.Spec.Platform.GCP.IAM.TemplateApplications {
		if applied == *ref {
			return true
		}
	}
	return false
}

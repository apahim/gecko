package versionsync

import (
	"fmt"
	"strconv"
	"strings"

	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"
)

// parseNumericBase returns the canonical numeric X.Y.Z base of a raw release
// name, dropping any prerelease or build suffix: prereleases inherit their
// numeric base's contract. Non-canonical names do not parse.
func parseNumericBase(name string) (major, minor, patch int, ok bool) {
	base, _, _ := strings.Cut(name, "-")
	parts := strings.Split(base, ".")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	nums := [3]int{}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 || strconv.Itoa(n) != part {
			return 0, 0, 0, false
		}
		nums[i] = n
	}
	return nums[0], nums[1], nums[2], true
}

// selectTemplateBoundary returns the greatest canonical numeric boundary at
// or below the Version's numeric base within the same major/minor. Selection
// never falls back across minors and never orders lexically (5.0.10 is a
// higher floor than 5.0.6). Unverified templates are included: the
// unverified highest match blocks availability instead of falling back to a
// lower verified boundary. The result is deterministic regardless of input
// order; boundary names are unique resource names, so there are no ties.
func selectTemplateBoundary(versionName string, templates []privatev1.WIFConfigTemplate) *privatev1.WIFConfigTemplate {
	major, minor, patch, ok := parseNumericBase(versionName)
	if !ok {
		return nil
	}
	var selected *privatev1.WIFConfigTemplate
	selectedPatch := -1
	for i := range templates {
		candidate := &templates[i]
		candidateMajor, candidateMinor, candidatePatch, candidateOK := parseNumericBase(candidate.Name)
		if !candidateOK || candidateMajor != major || candidateMinor != minor || candidatePatch > patch {
			continue
		}
		if selected == nil || candidatePatch > selectedPatch {
			selected = candidate
			selectedPatch = candidatePatch
		}
	}
	return selected
}

// resolvePin builds the immutable reference (name, UID and executable digest)
// for the selected boundary of a Version, or nil when no applicable boundary
// exists. The reference always identifies live catalog state, so admission
// validation accepts it without requiring verification.
func resolvePin(versionName string, templates []privatev1.WIFConfigTemplate) (*privatev1.WIFConfigTemplateReference, error) {
	selected := selectTemplateBoundary(versionName, templates)
	if selected == nil {
		return nil, nil
	}
	digest, err := privatev1.WIFConfigDigest(selected.Spec)
	if err != nil {
		return nil, fmt.Errorf("digest template %q: %w", selected.Name, err)
	}
	return &privatev1.WIFConfigTemplateReference{
		Name:   selected.Name,
		UID:    string(selected.UID),
		Digest: digest,
	}, nil
}

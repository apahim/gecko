package versionsync

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/openshift-online/gecko/controllers/util/logger"
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
// lower verified boundary. Retained templates failing current contract
// validation are likewise included and block: they can never be pinned, and
// selection never falls back to a lower boundary. The result is
// deterministic regardless of input order; boundary names are unique
// resource names, so there are no ties.
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

// pinnableBoundary reports whether a selected boundary satisfies the current
// executable-contract validation. The IAM catalog is append-only and
// contracts are immutable, so a retained boundary published under an earlier
// contract revision can permanently fail current validation; such a boundary
// can never be admitted as a pin. Callers treat unpinnable boundaries as
// fail-closed unavailability instead of attempting writes admission rejects.
func pinnableBoundary(t *privatev1.WIFConfigTemplate) error {
	return t.ValidateCreate(context.Background())
}

// nonCanonicalTemplateNames returns the names of catalog entries that do not
// parse as canonical X.Y.Z boundaries. Selection silently skips such
// templates, so a typo'd boundary name would otherwise never apply to any
// Version with no signal to operators.
func nonCanonicalTemplateNames(templates []privatev1.WIFConfigTemplate) []string {
	var names []string
	for i := range templates {
		if _, _, _, ok := parseNumericBase(templates[i].Name); !ok {
			names = append(names, templates[i].Name)
		}
	}
	return names
}

// warnNonCanonicalTemplateNames surfaces non-canonical boundary names once
// per pass, aggregated, so operators see catalog entries that can never
// apply. Template names are platform-GitOps-controlled catalog identifiers,
// not customer data.
func warnNonCanonicalTemplateNames(ctx context.Context, log logger.Logger, templates []privatev1.WIFConfigTemplate) {
	if names := nonCanonicalTemplateNames(templates); len(names) > 0 {
		log.Warnf(ctx, "ignoring %d WIFConfigTemplate(s) with non-canonical boundary names; they never apply to any Version: %v", len(names), names)
	}
}

// resolvePin builds the immutable reference (name, UID and executable digest)
// for the selected boundary of a Version, or nil when no applicable boundary
// exists or the selected boundary is unpinnable. The reference always
// identifies live catalog state, so admission validation accepts it without
// requiring verification.
func resolvePin(versionName string, templates []privatev1.WIFConfigTemplate) (*privatev1.WIFConfigTemplateReference, error) {
	selected := selectTemplateBoundary(versionName, templates)
	if selected == nil {
		return nil, nil
	}
	if err := pinnableBoundary(selected); err != nil {
		return nil, nil //nolint:nilerr // Unpinnable boundaries are an expected catalog state, not an error.
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

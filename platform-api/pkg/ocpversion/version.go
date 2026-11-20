// Package ocpversion compares OpenShift releases across the supported 4.x/5.x
// naming bridge. It must not be used to rewrite release or IAM catalog identity.
package ocpversion

import (
	"fmt"

	"k8s.io/apimachinery/pkg/util/version"
)

const v5MinorOffset uint = 23

// Normalize returns a comparison-only copy: 5.x maps to 4.(23+x), following
// HyperShift's dual-version scheme. Patch, prerelease and build metadata survive.
// Unknown majors and arithmetic overflow fail closed; input is never mutated.
func Normalize(v *version.Version) (*version.Version, error) {
	if v == nil {
		return nil, fmt.Errorf("missing OpenShift release version")
	}
	switch v.Major() {
	case 4:
		return v.WithMajor(4), nil
	case 5:
		if v.Minor() > ^uint(0)-v5MinorOffset {
			return nil, fmt.Errorf("OpenShift release minor overflow")
		}
		return v.WithMajor(4).WithMinor(v.Minor() + v5MinorOffset), nil
	default:
		return nil, fmt.Errorf("unsupported OpenShift release major")
	}
}

// Parse parses and normalizes a complete release for comparison only. Error
// categories deliberately omit raw input, which can be customer-controlled.
func Parse(value string) (*version.Version, error) {
	v, err := version.ParseSemantic(value)
	if err != nil {
		return nil, fmt.Errorf("invalid OpenShift release version")
	}
	return Normalize(v)
}

// NextMinor returns a channel-discovery version in the preferred naming series.
// A fleet named 5.x queries 5.0 after 4.22; an explicitly 4.x fleet retains its
// 4.x channel names. This selects no payload and authorizes no upgrade edge.
func NextMinor(current *version.Version, preferredMajor uint) (*version.Version, error) {
	v, err := Normalize(current)
	if err != nil {
		return nil, err
	}
	if preferredMajor != 4 && preferredMajor != 5 {
		return nil, fmt.Errorf("unsupported OpenShift channel major")
	}
	if v.Minor() == ^uint(0) {
		return nil, fmt.Errorf("OpenShift release minor overflow")
	}
	minor := v.Minor() + 1
	major := uint(4)
	if preferredMajor == 5 && minor >= v5MinorOffset {
		major, minor = 5, minor-v5MinorOffset
	}
	return version.ParseSemantic(fmt.Sprintf("%d.%d.0", major, minor))
}

// ObservedTransition is for conservative observation/locking, never admission.
// Equal logical releases with different raw identities may be an already-started
// alias transition; retain the lock and exact dispatched identity in that case.
func ObservedTransition(current, desired string) (bool, error) {
	c, err := Parse(current)
	if err != nil {
		return false, err
	}
	d, err := Parse(desired)
	if err != nil {
		return false, err
	}
	return c.LessThan(d) || (c.EqualTo(d) && current != desired), nil
}

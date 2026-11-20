package controlplaneupgrade

import (
	"fmt"

	"github.com/openshift-online/gecko/platform-api/pkg/ocpversion"

	utilversion "k8s.io/apimachinery/pkg/util/version"
)

// selectZStreamCandidate returns the newest advertised patch release in the same
// major.minor stream as completed, or "" if none is newer than completed.
func selectZStreamCandidate(completed string, advertised []string) (string, error) {
	completedVersion, err := ocpversion.Parse(completed)
	if err != nil {
		return "", fmt.Errorf("parse completed version: %w", err)
	}

	var selected string
	var selectedVersion *utilversion.Version
	for _, candidate := range advertised {
		candidateVersion, err := ocpversion.Parse(candidate)
		if err != nil {
			// AvailableUpdates is HyperShift-owned; skip anything Gecko cannot parse
			// rather than fail the whole reconciliation over one bad entry.
			continue
		}
		if candidateVersion.Major() != completedVersion.Major() || candidateVersion.Minor() != completedVersion.Minor() {
			continue
		}
		if !completedVersion.LessThan(candidateVersion) {
			continue
		}
		if selectedVersion == nil || selectedVersion.LessThan(candidateVersion) || (selectedVersion.EqualTo(candidateVersion) && candidate > selected) {
			selected = candidate
			selectedVersion = candidateVersion
		}
	}
	return selected, nil
}

// isAdvertised reports whether version appears verbatim in the advertised list.
func isAdvertised(version string, advertised []string) bool {
	for _, v := range advertised {
		if v == version {
			return true
		}
	}
	return false
}

// isValidCriticalPin reports whether pin is a valid emergency z-stream override
// for the given completed version. It delegates to selectZStreamCandidate with a
// single-element list: the pin is valid iff selectZStreamCandidate would select it,
// meaning it parses, shares the same major.minor as completed, and is strictly newer.
// This prevents a cross-minor or downgrade pin from bypassing y-stream health gates
// and Channel FleetMinorVersion authorization.
func isValidCriticalPin(pin, completed string) bool {
	if pin == "" {
		return false
	}
	// Do not expand emergency bypass authority across raw naming streams.
	rawPin, a := utilversion.ParseSemantic(pin)
	rawCompleted, b := utilversion.ParseSemantic(completed)
	if a != nil || b != nil || rawPin.Major() != rawCompleted.Major() || rawPin.Minor() != rawCompleted.Minor() {
		return false
	}
	selected, err := selectZStreamCandidate(completed, []string{pin})
	return err == nil && selected == pin
}

// selectYStreamCandidate returns the next sequential minor version from advertised
// that does not exceed fleetMinorVersion authorization. Returns "" if no valid
// y-stream candidate exists.
//
// Y-stream upgrades are sequential (4.22 → 4.23 → 4.24), never skip a minor version.
// The fleetMinorVersion is the platform authorization boundary; it must be greater
// than or equal to the candidate's minor version.
func selectYStreamCandidate(completed string, advertised []string, fleetMinorVersion string) (string, error) {
	completedVersion, err := ocpversion.Parse(completed)
	if err != nil {
		return "", fmt.Errorf("parse completed version: %w", err)
	}

	fleetMinor, err := ocpversion.Parse(fleetMinorVersion + ".0")
	if err != nil {
		return "", fmt.Errorf("parse fleet minor version: %w", err)
	}

	targetMinor := completedVersion.Minor() + 1

	// Find the newest patch in the next minor version that's authorized by fleetMinorVersion
	var selected string
	var selectedVersion *utilversion.Version
	for _, candidate := range advertised {
		candidateVersion, err := ocpversion.Parse(candidate)
		if err != nil {
			continue
		}

		// Must be same major, next minor
		if candidateVersion.Major() != completedVersion.Major() {
			continue
		}
		if candidateVersion.Minor() != targetMinor {
			continue
		}

		// Must not exceed fleet authorization
		if candidateVersion.Minor() > fleetMinor.Minor() {
			continue
		}

		if selectedVersion == nil || selectedVersion.LessThan(candidateVersion) || (selectedVersion.EqualTo(candidateVersion) && candidate > selected) {
			selected = candidate
			selectedVersion = candidateVersion
		}
	}

	return selected, nil
}

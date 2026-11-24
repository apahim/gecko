package transport

import (
	"fmt"
	"strings"
)

// DeleteInitiatedMessage returns the message for a deletion that has just
// been initiated for applyDesiresCount resources.
func DeleteInitiatedMessage(applyDesiresCount int) string {
	return fmt.Sprintf("Deletion of %d resources initiated", applyDesiresCount)
}

// DeletePendingMessage summarizes a not-yet-complete deletion: how many
// resources are still being deleted (with their references), plus the
// details of any deletions kube-applier-gcp reported as failed.
func DeletePendingMessage(ds *DeleteStatus) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d of %d resources still being deleted", ds.PendingCount, ds.TotalCount)
	if len(ds.PendingResources) > 0 {
		b.WriteString(": ")
		b.WriteString(strings.Join(ds.PendingResources, ", "))
		if hidden := ds.PendingCount - len(ds.PendingResources); hidden > 0 {
			fmt.Fprintf(&b, " (+%d more)", hidden)
		}
	}
	if ds.FailedCount > 0 {
		parts := make([]string, 0, len(ds.Failures))
		for _, f := range ds.Failures {
			if f.Message != "" {
				parts = append(parts, fmt.Sprintf("%s (%s: %s)", f.Resource, f.Reason, f.Message))
			} else {
				parts = append(parts, fmt.Sprintf("%s (%s)", f.Resource, f.Reason))
			}
		}
		if hidden := ds.FailedCount - len(parts); hidden > 0 {
			parts = append(parts, fmt.Sprintf("+%d more", hidden))
		}
		fmt.Fprintf(&b, "; deletion failed for %d resources: %s", ds.FailedCount, strings.Join(parts, "; "))
	}
	return b.String()
}

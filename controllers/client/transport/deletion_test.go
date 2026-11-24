package transport

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDeleteInitiatedMessage(t *testing.T) {
	require.Equal(t, "Deletion of 12 resources initiated", DeleteInitiatedMessage(12))
	require.Equal(t, "Deletion of 1 resources initiated", DeleteInitiatedMessage(1))
}

func TestDeletePendingMessage(t *testing.T) {
	tests := []struct {
		name string
		ds   *DeleteStatus
		want string
	}{
		{
			name: "counts only",
			ds:   &DeleteStatus{PendingCount: 2, TotalCount: 5},
			want: "2 of 5 resources still being deleted",
		},
		{
			name: "counts with pending resources",
			ds: &DeleteStatus{
				PendingCount:     2,
				TotalCount:       5,
				PendingResources: []string{"hypershift.openshift.io/v1beta1/hostedclusters/clusters-abc/hc-1", "v1/namespaces/foo"},
			},
			want: "2 of 5 resources still being deleted: " +
				"hypershift.openshift.io/v1beta1/hostedclusters/clusters-abc/hc-1, v1/namespaces/foo",
		},
		{
			name: "capped pending resources note the hidden remainder",
			ds: &DeleteStatus{
				PendingCount:     12,
				TotalCount:       15,
				PendingResources: []string{"g/v1/rs/ns/a", "g/v1/rs/ns/b"},
			},
			want: "12 of 15 resources still being deleted: g/v1/rs/ns/a, g/v1/rs/ns/b (+10 more)",
		},
		{
			name: "failures are appended with reason and message",
			ds: &DeleteStatus{
				PendingCount: 1,
				TotalCount:   3,
				Failures: []DeleteFailure{{
					Resource: "g/v1/rs/ns/failed",
					Reason:   "KubeAPIError",
					Message:  "boom",
				}},
				FailedCount: 1,
			},
			want: "1 of 3 resources still being deleted; " +
				"deletion failed for 1 resources: g/v1/rs/ns/failed (KubeAPIError: boom)",
		},
		{
			name: "failure without message omits the message part",
			ds: &DeleteStatus{
				PendingCount: 1,
				TotalCount:   3,
				Failures: []DeleteFailure{{
					Resource: "g/v1/rs/ns/failed",
					Reason:   "PreCheckFailed",
				}},
				FailedCount: 1,
			},
			want: "1 of 3 resources still being deleted; " +
				"deletion failed for 1 resources: g/v1/rs/ns/failed (PreCheckFailed)",
		},
		{
			name: "capped failures note the hidden remainder",
			ds: &DeleteStatus{
				PendingCount: 6,
				TotalCount:   6,
				Failures: []DeleteFailure{{
					Resource: "g/v1/rs/ns/failed",
					Reason:   "KubeAPIError",
					Message:  "boom",
				}},
				FailedCount: 4,
			},
			want: "6 of 6 resources still being deleted; " +
				"deletion failed for 4 resources: g/v1/rs/ns/failed (KubeAPIError: boom); +3 more",
		},
		{
			name: "pending resources and failures combined",
			ds: &DeleteStatus{
				PendingCount:     2,
				TotalCount:       3,
				PendingResources: []string{"g/v1/rs/ns/a", "g/v1/rs/ns/failed"},
				Failures: []DeleteFailure{{
					Resource: "g/v1/rs/ns/failed",
					Reason:   "Failed",
					Message:  "Had an error while syncing",
				}},
				FailedCount: 1,
			},
			want: "2 of 3 resources still being deleted: g/v1/rs/ns/a, g/v1/rs/ns/failed; " +
				"deletion failed for 1 resources: g/v1/rs/ns/failed (Failed: Had an error while syncing)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, DeletePendingMessage(tt.ds))
		})
	}
}

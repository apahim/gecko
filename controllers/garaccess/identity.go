package garaccess

import (
	"context"
	"fmt"

	"google.golang.org/api/compute/v1"
	"google.golang.org/api/iam/v1"
)

type identityLookup struct {
	compute *compute.Service
	iam     *iam.Service
}

func newIdentityLookup(ctx context.Context) (*identityLookup, error) {
	computeClient, err := compute.NewService(ctx)
	if err != nil {
		return nil, fmt.Errorf("create Compute client: %w", err)
	}
	iamClient, err := iam.NewService(ctx)
	if err != nil {
		return nil, fmt.Errorf("create IAM client: %w", err)
	}
	return &identityLookup{compute: computeClient, iam: iamClient}, nil
}

func (l *identityLookup) customerMember(ctx context.Context, projectID string) (string, error) {
	project, err := l.compute.Projects.Get(projectID).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("get Compute project %s: %w", projectID, err)
	}
	// The number comes from Google, never from a customer-supplied Cluster field.
	email := fmt.Sprintf("%d-compute@developer.gserviceaccount.com", project.Id)
	if project.Name != projectID || project.Id == 0 || project.DefaultServiceAccount != email {
		return "", fmt.Errorf("project %s has no valid default Compute service account", projectID)
	}
	name := fmt.Sprintf("projects/%s/serviceAccounts/%s", projectID, email)
	account, err := l.iam.Projects.ServiceAccounts.Get(name).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("get default Compute account for %s: %w", projectID, err)
	}
	if account.ProjectId != projectID || account.Email != email || account.Disabled {
		return "", fmt.Errorf("default Compute account for %s must belong to the project and be enabled", projectID)
	}
	return "serviceAccount:" + email, nil
}

// Package garaccess reconciles shared customer GAR Reader grants.
package garaccess

import (
	"fmt"
	"regexp"

	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// DefaultRepository is the preconfigured shared dev Quay pull-through cache.
	DefaultRepository = "projects/gcp-hcp-commons-dev/locations/northamerica-northeast2/repositories/gcp-hcp-quay"
	// DefaultIAMNamespace outlives individual HostedClusters and NodePools.
	DefaultIAMNamespace = "gecko-system"
)

// Config is shared by the grant controller and the NodePool provisioning gate.
type Config struct {
	Repository   string
	IAMNamespace string
}

// Validate rejects incomplete configuration before either controller starts.
func (c Config) Validate() error {
	if !regexp.MustCompile(`^projects/[a-z0-9-]+/locations/[a-z0-9-]+/repositories/[a-z0-9._-]+$`).MatchString(c.Repository) {
		return fmt.Errorf("gar-repository must be projects/PROJECT/locations/LOCATION/repositories/REPOSITORY")
	}
	if c.IAMNamespace == "" || len(validation.IsDNS1123Label(c.IAMNamespace)) != 0 {
		return fmt.Errorf("iam-namespace must name an existing regional Kubernetes namespace")
	}
	return nil
}

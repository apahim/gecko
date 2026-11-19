// Package garaccess reconciles shared customer GAR Reader grants.
package garaccess

import (
	"fmt"
	"regexp"

	"k8s.io/apimachinery/pkg/util/validation"
)

// DefaultIAMNamespace outlives individual HostedClusters and NodePools.
const DefaultIAMNamespace = "gecko-system"

// Config configures the background GAR grant controller.
type Config struct {
	Repository   string
	IAMNamespace string
}

// Validate rejects incomplete configuration before the grant controller starts.
func (c Config) Validate() error {
	if c.Repository == "" {
		return fmt.Errorf("gar-repository must be set")
	}
	if !regexp.MustCompile(`^projects/[a-z0-9-]+/locations/[a-z0-9-]+/repositories/[a-z0-9._-]+$`).MatchString(c.Repository) {
		return fmt.Errorf("gar-repository must be projects/PROJECT/locations/LOCATION/repositories/REPOSITORY")
	}
	if c.IAMNamespace == "" || len(validation.IsDNS1123Label(c.IAMNamespace)) != 0 {
		return fmt.Errorf("iam-namespace must name an existing regional Kubernetes namespace")
	}
	return nil
}

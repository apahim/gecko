package garaccess

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfigValidate(t *testing.T) {
	const repository = "projects/registry-project/locations/us-central1/repositories/quay"
	tests := []struct {
		name       string
		repository string
		namespace  string
		wantErr    string
	}{
		{name: "valid", repository: repository, namespace: DefaultIAMNamespace},
		{name: "repository punctuation", repository: "projects/project-1/locations/europe-west2/repositories/quay.images_1", namespace: "iam-1"},
		{name: "maximum namespace length", repository: repository, namespace: strings.Repeat("a", 63)},
		{name: "missing repository", namespace: DefaultIAMNamespace, wantErr: "gar-repository"},
		{name: "missing project", repository: "projects//locations/us-central1/repositories/quay", namespace: DefaultIAMNamespace, wantErr: "gar-repository"},
		{name: "missing location", repository: "projects/registry-project/locations//repositories/quay", namespace: DefaultIAMNamespace, wantErr: "gar-repository"},
		{name: "missing repository name", repository: "projects/registry-project/locations/us-central1/repositories/", namespace: DefaultIAMNamespace, wantErr: "gar-repository"},
		{name: "URL instead of resource path", repository: "https://artifactregistry.googleapis.com/" + repository, namespace: DefaultIAMNamespace, wantErr: "gar-repository"},
		{name: "trailing path", repository: repository + "/images", namespace: DefaultIAMNamespace, wantErr: "gar-repository"},
		{name: "uppercase project", repository: "projects/Registry/locations/us-central1/repositories/quay", namespace: DefaultIAMNamespace, wantErr: "gar-repository"},
		{name: "invalid repository character", repository: repository + "!", namespace: DefaultIAMNamespace, wantErr: "gar-repository"},
		{name: "empty namespace", repository: repository, wantErr: "iam-namespace"},
		{name: "uppercase namespace", repository: repository, namespace: "Gecko", wantErr: "iam-namespace"},
		{name: "dotted namespace", repository: repository, namespace: "gecko.system", wantErr: "iam-namespace"},
		{name: "namespace starts with hyphen", repository: repository, namespace: "-gecko", wantErr: "iam-namespace"},
		{name: "namespace ends with hyphen", repository: repository, namespace: "gecko-", wantErr: "iam-namespace"},
		{name: "namespace too long", repository: repository, namespace: strings.Repeat("a", 64), wantErr: "iam-namespace"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := (Config{Repository: tt.repository, IAMNamespace: tt.namespace}).Validate()
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

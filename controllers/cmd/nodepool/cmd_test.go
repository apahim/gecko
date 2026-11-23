package nodepool

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewCommandGARAccessDefaults(t *testing.T) {
	cmd := NewCommand(nil)
	enabled, err := cmd.Flags().GetBool("require-gar-access")
	require.NoError(t, err)
	require.False(t, enabled)
	repository, err := cmd.Flags().GetString("gar-repository")
	require.NoError(t, err)
	require.Empty(t, repository)
	namespace, err := cmd.Flags().GetString("iam-namespace")
	require.NoError(t, err)
	require.Equal(t, "gecko-system", namespace)
	require.Equal(t, "false", cmd.Flags().Lookup("require-gar-access").DefValue)
	require.Empty(t, cmd.Flags().Lookup("gar-repository").DefValue)
	require.Equal(t, "gecko-system", cmd.Flags().Lookup("iam-namespace").DefValue)
}

func TestNewCommandRejectsInvalidGARAccessBeforeClientSetup(t *testing.T) {
	const repository = "projects/customer-project/locations/us-central1/repositories/images"
	for _, tc := range []struct {
		name      string
		args      []string
		wantError string
	}{
		{name: "missing repository", wantError: "gar-repository must be set"},
		{name: "invalid repository", args: []string{"--gar-repository=images"}, wantError: "gar-repository must be projects/"},
		{name: "empty namespace", args: []string{"--gar-repository=" + repository, "--iam-namespace="}, wantError: "iam-namespace"},
		{name: "invalid namespace", args: []string{"--gar-repository=" + repository, "--iam-namespace=Invalid_Namespace"}, wantError: "iam-namespace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A nil RootFlags makes accidental logger/client setup fail immediately.
			cmd := NewCommand(nil)
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			cmd.SetArgs(append([]string{"--require-gar-access"}, tc.args...))
			err := cmd.Execute()
			require.ErrorContains(t, err, tc.wantError)
		})
	}
}

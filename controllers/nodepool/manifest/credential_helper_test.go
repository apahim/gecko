package manifest

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCredentialHelper(t *testing.T) {
	jq, err := exec.LookPath("jq")
	if errors.Is(err, exec.ErrNotFound) {
		t.Skip("jq is required to exercise the credential helper's response parsing")
	}
	require.NoError(t, err)
	jq, err = filepath.Abs(jq)
	require.NoError(t, err)

	const token = "test-token"
	const validResponse = `{"access_token":"` + token + `","expires_in":3600,"token_type":"Bearer"}`
	const metadataError = "gcp: metadata credential request failed\n"
	const responseError = "gcp: metadata returned an invalid credential\n"
	const actionError = "gcp: only credential lookup is supported\n"
	tests := []struct {
		name       string
		action     string
		response   string
		curlStatus int
		wantStderr string
	}{
		{name: "valid bearer token", action: "get", response: validResponse},
		{name: "metadata unavailable", action: "get", response: validResponse, curlStatus: 28, wantStderr: metadataError},
		{name: "malformed JSON", action: "get", response: `{"access_token":"` + token + `"`, wantStderr: responseError},
		{name: "multiple JSON objects", action: "get", response: validResponse + "\n{}", wantStderr: responseError},
		{name: "missing access token", action: "get", response: `{"expires_in":3600,"token_type":"Bearer"}`, wantStderr: responseError},
		{name: "expired token", action: "get", response: `{"access_token":"` + token + `","expires_in":0,"token_type":"Bearer"}`, wantStderr: responseError},
		{name: "non-bearer token", action: "get", response: `{"access_token":"` + token + `","expires_in":3600,"token_type":"Other"}`, wantStderr: responseError},
		{name: "store unsupported", action: "store", wantStderr: actionError},
		{name: "erase unsupported", action: "erase", wantStderr: actionError},
	}

	// Rewrite only executable paths in the test copy. Refuse to run if the
	// expected curl call changes so the test cannot contact real metadata.
	script := string(credentialHelper)
	require.Equal(t, 1, strings.Count(script, "/usr/bin/curl"))
	require.Equal(t, 1, strings.Count(script, "/usr/bin/jq"))
	script = strings.NewReplacer("/usr/bin/curl", `"$MOCK_CURL"`, "/usr/bin/jq", `"$TEST_JQ"`).Replace(script)

	dir := t.TempDir()
	helper := filepath.Join(dir, "docker-credential-gcp")
	mockCurl := filepath.Join(dir, "curl")
	require.NoError(t, os.WriteFile(helper, []byte(script), 0o600))
	require.NoError(t, os.WriteFile(mockCurl, []byte(`#!/bin/sh
set -eu
: > "$MOCK_CURL_CALLED"
printf '%s\n' "$MOCK_CURL_RESPONSE"
if [ "$MOCK_CURL_STATUS" != 0 ]; then
    printf '%s\n' "$MOCK_CURL_RESPONSE" >&2
fi
exit "$MOCK_CURL_STATUS"
`), 0o700))

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			called := filepath.Join(t.TempDir(), "curl-called")
			cmd := exec.CommandContext(t.Context(), "/bin/sh", helper, tc.action)
			cmd.Env = []string{
				"MOCK_CURL=" + mockCurl,
				"MOCK_CURL_CALLED=" + called,
				"MOCK_CURL_RESPONSE=" + tc.response,
				"MOCK_CURL_STATUS=" + strconv.Itoa(tc.curlStatus),
				"TEST_JQ=" + jq,
			}
			cmd.Stdin = strings.NewReader("https://us-west1-docker.pkg.dev\n")
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr

			err := cmd.Run()
			if tc.action == "get" {
				require.FileExists(t, called)
			} else {
				require.NoFileExists(t, called)
			}
			require.Equal(t, tc.wantStderr, stderr.String())
			if tc.wantStderr != "" {
				var exitErr *exec.ExitError
				require.ErrorAs(t, err, &exitErr)
				require.Equal(t, 1, exitErr.ExitCode())
				require.Empty(t, stdout.String())
				return
			}
			require.NoError(t, err)
			require.JSONEq(t, `{"Username":"oauth2accesstoken","Secret":"`+token+`"}`, stdout.String())
		})
	}
}

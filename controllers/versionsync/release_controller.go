package versionsync

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/openshift-online/gecko/controllers/versionresolution"
)

// ReleaseControllerClient reads accepted releases from explicit CI streams.
// The endpoint selects the architecture, for example the amd64 release controller.
// Graph nodes cannot be used here: the CI graph includes nodes from other streams.
type ReleaseControllerClient struct {
	baseURL    string
	httpClient *http.Client
}

func NewReleaseControllerClient(endpoint string) (*ReleaseControllerClient, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("release-controller endpoint must be an HTTP(S) base URL without query or fragment")
	}
	return &ReleaseControllerClient{baseURL: strings.TrimRight(endpoint, "/"), httpClient: &http.Client{Timeout: 30 * time.Second}}, nil
}

// ListReleases uses the unpaginated tags endpoint, not the latest-release endpoint.
func (c *ReleaseControllerClient) ListReleases(ctx context.Context, stream string) ([]versionresolution.ReleaseInfo, error) {
	if stream == "" || strings.ContainsAny(stream, "/?#") || stream == "." || stream == ".." {
		return nil, fmt.Errorf("invalid release stream %q", stream)
	}
	endpoint := c.baseURL + "/api/v1/releasestream/" + url.PathEscape(stream) + "/tags?phase=Accepted"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create release stream request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch release stream %q: %w", stream, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release stream %q returned HTTP %d", stream, resp.StatusCode)
	}
	var result struct {
		Name string          `json:"name"`
		Tags json.RawMessage `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode release stream %q: %w", stream, err)
	}
	if result.Name != stream {
		return nil, fmt.Errorf("requested stream %q, received %q", stream, result.Name)
	}
	var tags []struct {
		Name     string `json:"name"`
		Phase    string `json:"phase"`
		PullSpec string `json:"pullSpec"`
	}
	if err := json.Unmarshal(result.Tags, &tags); err != nil {
		return nil, fmt.Errorf("decode tags for stream %q: %w", stream, err)
	}
	releases := make([]versionresolution.ReleaseInfo, 0, len(tags))
	for _, tag := range tags {
		// Check the phase even when the server was asked to filter it.
		if tag.Phase != "Accepted" {
			continue
		}
		if tag.Name == "" || tag.PullSpec == "" {
			return nil, fmt.Errorf("accepted tag in stream %q has no name or pull spec", stream)
		}
		releases = append(releases, versionresolution.ReleaseInfo{Version: tag.Name, Payload: tag.PullSpec})
	}
	return releases, nil
}

package versionsync

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/openshift-online/gecko/controllers/util/logger"
	"github.com/openshift-online/gecko/controllers/versionresolution"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	utilversion "k8s.io/apimachinery/pkg/util/version"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	syncInterval = 15 * time.Minute

	// Stop probing a major after this many missing minor channels.
	maxConsecutiveMissingMinors = 3

	// Probe future major versions, including 5.x and later.
	maxMajorGap = 2

	// Bound one synchronization independently of Cincinnati response content.
	maxChannelProbes = 256
)

// Controller synchronizes Version resources from Cincinnati.
type Controller struct {
	cincinnatiClient *versionresolution.CincinnatiClient
	log              logger.Logger
	apiClient        client.Client
}

func NewController(
	cincinnatiClient *versionresolution.CincinnatiClient,
	log logger.Logger,
	apiClient client.Client,
) *Controller {
	return &Controller{
		cincinnatiClient: cincinnatiClient,
		log:              log,
		apiClient:        apiClient,
	}
}

// Start implements manager.Runnable.
func (c *Controller) Start(ctx context.Context) error {
	log := c.log.With("controller", "version-sync-controller")
	log.Info(ctx, "starting")

	c.sync(ctx, log)

	ticker := time.NewTicker(syncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			c.sync(ctx, log)
		}
	}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (c *Controller) NeedLeaderElection() bool {
	return true
}

// sync fetches the desired version snapshot from Cincinnati and applies it to the API.
func (c *Controller) sync(ctx context.Context, log logger.Logger) {
	channels, err := c.channels(ctx)
	if err != nil {
		log.Errorf(ctx, "list channels failed, preserving previous version snapshot: %v", err)
		return
	}

	if len(channels) == 0 {
		log.Error(ctx, "no Channel resources found, preserving previous version snapshot")
		return
	}

	desired, err := c.fetchVersions(ctx, log, channels)
	if err != nil {
		log.Errorf(ctx, "fetch failed, preserving previous version snapshot: %v", err)
		return
	}

	if err := c.apply(ctx, log, desired); err != nil {
		log.Errorf(ctx, "apply failed: %v", err)
	}
}

func (c *Controller) fetchVersions(
	ctx context.Context,
	log logger.Logger,
	channels []privatev1.Channel,
) (map[string]privatev1.VersionSpec, error) {
	versions := make(map[string]privatev1.VersionSpec)
	channelCount := 0
	probeCount := 0

	for _, configuredChannel := range channels {
		minimumMajor, minimumMinor, err := channelMinimum(configuredChannel)
		if err != nil {
			return nil, err
		}
		group := configuredChannel.Name
		emptyMajorCount := 0

		for major := minimumMajor; emptyMajorCount < maxMajorGap; major++ {
			startMinor := 0
			if major == minimumMajor {
				startMinor = minimumMinor
			}

			foundMajor := false
			emptyMinorCount := 0

			for minor := startMinor; emptyMinorCount < maxConsecutiveMissingMinors; minor++ {
				if probeCount >= maxChannelProbes {
					return nil, fmt.Errorf("cincinnati channel probe limit %d exceeded", maxChannelProbes)
				}
				probeCount++

				channel := fmt.Sprintf(
					"%s-%d.%d",
					group,
					major,
					minor,
				)

				releases, err := c.cincinnatiClient.ListReleases(ctx, channel)
				if err != nil {
					// Do not apply a partial snapshot.
					return nil, fmt.Errorf("fetch channel %q: %w", channel, err)
				}

				if len(releases) == 0 {
					emptyMinorCount++
					continue
				}

				emptyMinorCount = 0
				foundMajor = true
				channelCount++

				for _, release := range releases {
					if !isSupportedVersion(release.Version, minimumMajor, minimumMinor) {
						continue
					}
					if release.Payload == "" {
						return nil, fmt.Errorf("release %q has no payload", release.Version)
					}

					spec, exists := versions[release.Version]
					if !exists {
						spec = privatev1.VersionSpec{
							ReleaseImage:  release.Payload,
							ChannelGroups: []string{group},
						}
					} else {
						if spec.ReleaseImage != release.Payload {
							return nil, fmt.Errorf("version %q has conflicting release payloads", release.Version)
						}
						if !containsString(spec.ChannelGroups, group) {
							spec.ChannelGroups = append(spec.ChannelGroups, group)
							sort.Strings(spec.ChannelGroups)
						}
					}

					versions[release.Version] = spec
				}
			}

			if foundMajor {
				emptyMajorCount = 0
			} else {
				emptyMajorCount++
			}
		}
	}

	if channelCount == 0 || len(versions) == 0 {
		return nil, fmt.Errorf("no Cincinnati channels returned releases")
	}

	log.Infof(
		ctx,
		"fetched %d versions from %d channels",
		len(versions),
		channelCount,
	)

	return versions, nil
}

func (c *Controller) channels(ctx context.Context) ([]privatev1.Channel, error) {
	var channels privatev1.ChannelList
	if err := c.apiClient.List(ctx, &channels); err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}

	sort.Slice(channels.Items, func(i, j int) bool { return channels.Items[i].Name < channels.Items[j].Name })
	return channels.Items, nil
}

func (c *Controller) apply(
	ctx context.Context,
	log logger.Logger,
	desired map[string]privatev1.VersionSpec,
) error {
	var current privatev1.VersionList
	if err := c.apiClient.List(ctx, &current); err != nil {
		return fmt.Errorf("list versions: %w", err)
	}

	existing := make(map[string]*privatev1.Version, len(current.Items))

	for i := range current.Items {
		version := &current.Items[i]
		existing[version.Name] = version
	}

	// Update existing Versions first.
	for name, spec := range desired {
		version, found := existing[name]
		if !found || specEqual(&version.Spec, &spec) {
			continue
		}

		version.Spec = spec
		if err := c.apiClient.Update(ctx, version); err != nil {
			return fmt.Errorf("update version %q: %w", name, err)
		}

		log.Infof(ctx, "updated Version %s", name)
	}

	// Create missing Versions.
	for name, spec := range desired {
		if _, found := existing[name]; found {
			continue
		}

		version := &privatev1.Version{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
			Spec: spec,
		}

		if err := c.apiClient.Create(ctx, version); err != nil {
			return fmt.Errorf("create version %q: %w", name, err)
		}

		log.Infof(ctx, "created Version %s", name)
	}

	// Delete stale Versions last. If cincinnati is not returning a version, it is considered stale.
	for i := range current.Items {
		version := &current.Items[i]
		if _, found := desired[version.Name]; found {
			continue
		}

		if err := c.apiClient.Delete(ctx, version); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale version %q: %w", version.Name, err)
		}

		log.Infof(ctx, "deleted stale Version %s", version.Name)
	}

	return nil
}

func parseMajorMinor(version string) (int, int, bool) {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}

	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}

	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, false
	}

	return major, minor, true
}

// channelMinimum uses the Channel's fleet policy as the inclusive catalog floor.
// Discovery continues beyond this minor, including subsequent major versions.
func channelMinimum(channel privatev1.Channel) (int, int, error) {
	value := channel.Spec.FleetMinorVersion
	major, minor, valid := parseMajorMinor(value)
	if !valid || major < 0 || minor < 0 || fmt.Sprintf("%d.%d", major, minor) != value {
		return 0, 0, fmt.Errorf("channel %q has invalid fleetMinorVersion %q", channel.Name, value)
	}
	return major, minor, nil
}

func isSupportedVersion(version string, minimumMajor, minimumMinor int) bool {
	semanticVersion, err := utilversion.ParseSemantic(version)
	if err != nil || semanticVersion.String() != version {
		return false
	}
	if len(validation.IsDNS1123Subdomain(version)) != 0 {
		return false
	}

	major, minor, valid := parseMajorMinor(version)
	if !valid {
		return false
	}

	return major > minimumMajor ||
		(major == minimumMajor && minor >= minimumMinor)
}

func specEqual(a, b *privatev1.VersionSpec) bool {
	if a.ReleaseImage != b.ReleaseImage {
		return false
	}
	if len(a.ChannelGroups) != len(b.ChannelGroups) {
		return false
	}
	for i := range a.ChannelGroups {
		if a.ChannelGroups[i] != b.ChannelGroups[i] {
			return false
		}
	}
	return true
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

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

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	utilversion "k8s.io/apimachinery/pkg/util/version"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	syncInterval = 15 * time.Minute

	// eligibilityInterval re-observes template verification changes and pins
	// unpinned Versions faster than the discovery sync. The POC default is
	// deliberately short; production deployments set it explicitly.
	defaultEligibilityInterval = 30 * time.Second

	// Stop probing a major after this many missing minor channels.
	maxConsecutiveMissingMinors = 3

	// Probe future major versions, including 5.x and later.
	maxMajorGap = 2

	// Bound one synchronization independently of Cincinnati response content.
	maxChannelProbes = 256
)

// Controller synchronizes Version resources from the environment's catalog source.
type Controller struct {
	releaseControllerClient *ReleaseControllerClient
	cincinnatiClient        *versionresolution.CincinnatiClient
	log                     logger.Logger
	apiClient               client.Client
	eligibilityInterval     time.Duration
}

func NewController(
	cincinnatiClient *versionresolution.CincinnatiClient,
	log logger.Logger,
	apiClient client.Client,
) *Controller {
	return &Controller{
		cincinnatiClient:    cincinnatiClient,
		log:                 log,
		apiClient:           apiClient,
		eligibilityInterval: defaultEligibilityInterval,
	}
}

// NewCIController synchronizes the accepted releases selected by each Channel.
func NewCIController(source *ReleaseControllerClient, log logger.Logger, apiClient client.Client) *Controller {
	return &Controller{releaseControllerClient: source, log: log, apiClient: apiClient, eligibilityInterval: defaultEligibilityInterval}
}

// SetEligibilityInterval configures the fast availability-refresh cadence.
// Non-positive values keep the default.
func (c *Controller) SetEligibilityInterval(interval time.Duration) *Controller {
	if interval > 0 {
		c.eligibilityInterval = interval
	}
	return c
}

// Start implements manager.Runnable.
func (c *Controller) Start(ctx context.Context) error {
	log := c.log.With("controller", "version-sync-controller")
	log.Info(ctx, "starting")

	c.sync(ctx, log)

	discovery := time.NewTicker(syncInterval)
	defer discovery.Stop()
	eligibility := time.NewTicker(c.eligibilityInterval)
	defer eligibility.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-discovery.C:
			c.sync(ctx, log)
		case <-eligibility.C:
			c.refreshEligibility(ctx, log)
		}
	}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (c *Controller) NeedLeaderElection() bool {
	return true
}

// sync fetches the desired version snapshot and applies it to the API.
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
	var desired map[string]privatev1.VersionSpec
	if c.releaseControllerClient != nil {
		desired, err = c.fetchCIVersions(ctx, channels)
	} else {
		desired, err = c.fetchVersions(ctx, log, channels)
	}
	if err != nil {
		log.Errorf(ctx, "fetch failed, preserving previous version snapshot: %v", err)
		c.reportDefaults(ctx, log, channels, nil, "FetchFailed")
		return
	}
	if err := c.apply(ctx, log, desired); err != nil {
		log.Errorf(ctx, "apply failed: %v", err)
		c.reportDefaults(ctx, log, channels, nil, "ApplyFailed")
		return
	}
	c.reportDefaults(ctx, log, channels, desired, "")
	// Refresh availability immediately so post-apply generation bumps get a
	// current observedGeneration instead of waiting for the fast loop.
	c.refreshEligibility(ctx, log)
}

// fetchCIVersions gathers the complete snapshot before any Version is changed.
func (c *Controller) fetchCIVersions(ctx context.Context, channels []privatev1.Channel) (map[string]privatev1.VersionSpec, error) {
	availableStreams, err := c.releaseControllerClient.ListStreams(ctx)
	if err != nil {
		return nil, fmt.Errorf("discover CI streams: %w", err)
	}
	versions := make(map[string]privatev1.VersionSpec)
	streams := make(map[string][]versionresolution.ReleaseInfo)
	for _, channel := range channels {
		minimumMajor, minimumMinor, err := channelMinimum(channel)
		if err != nil {
			return nil, err
		}
		matched := false
		for _, stream := range availableStreams {
			if !streamMatchesChannel(stream, channel.Name, minimumMajor, minimumMinor) {
				continue
			}
			matched = true
			releases, found := streams[stream]
			if !found {
				var err error
				releases, err = c.releaseControllerClient.ListReleases(ctx, stream)
				if err != nil {
					return nil, fmt.Errorf("channel %q: %w", channel.Name, err)
				}
				streams[stream] = releases
			}
			for _, release := range releases {
				if !isSupportedVersion(release.Version, minimumMajor, minimumMinor) {
					continue
				}
				spec, exists := versions[release.Version]
				if exists && spec.ReleaseImage != release.Payload {
					return nil, fmt.Errorf("version %q has conflicting release payloads", release.Version)
				}
				spec.ReleaseImage = release.Payload
				if !containsString(spec.ChannelGroups, channel.Name) {
					spec.ChannelGroups = append(spec.ChannelGroups, channel.Name)
					sort.Strings(spec.ChannelGroups)
				}
				versions[release.Version] = spec
			}
		}
		if !matched {
			return nil, fmt.Errorf("channel %q has no matching CI streams at or above minimumSupportedVersion %q", channel.Name, channel.Spec.MinimumSupportedVersion)
		}
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("no discovered CI streams returned supported accepted releases")
	}
	return versions, nil
}

// reportDefaults never changes the operator's pinned default. A default must
// belong to its own Channel, not merely exist elsewhere in the catalog.
func (c *Controller) reportDefaults(ctx context.Context, log logger.Logger, channels []privatev1.Channel, desired map[string]privatev1.VersionSpec, failureReason string) {
	for i := range channels {
		channel := &channels[i]
		before := channel.DeepCopy()
		condition := metav1.Condition{
			Type:               privatev1.ChannelDefaultVersionAvailable,
			ObservedGeneration: channel.Generation,
			Status:             metav1.ConditionUnknown,
			Reason:             failureReason,
			Message:            "Catalog synchronization failed; default availability could not be determined",
		}
		if failureReason == "" {
			condition.Status = metav1.ConditionFalse
			condition.Reason = "DefaultVersionUnavailable"
			condition.Message = fmt.Sprintf("Pinned default %q is not in this Channel's synchronized catalog", channel.Spec.InstallDefaultVersion)
			if spec, ok := desired[channel.Spec.InstallDefaultVersion]; ok && containsString(spec.ChannelGroups, channel.Name) {
				condition.Status = metav1.ConditionTrue
				condition.Reason = "DefaultVersionAvailable"
				condition.Message = fmt.Sprintf("Pinned default %q is in this Channel's synchronized catalog", channel.Spec.InstallDefaultVersion)
			}
		}
		meta.SetStatusCondition(&channel.Status.Conditions, condition)
		if equality.Semantic.DeepEqual(before.Status, channel.Status) {
			continue
		}
		// ResourceVersion from the list prevents reporting against a newer spec.
		if err := c.apiClient.Status().Update(ctx, channel); err != nil {
			log.Errorf(ctx, "update Channel %q default availability failed: %v", channel.Name, err)
		}
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

func (c *Controller) templates(ctx context.Context) ([]privatev1.WIFConfigTemplate, error) {
	var templates privatev1.WIFConfigTemplateList
	if err := c.apiClient.List(ctx, &templates); err != nil {
		return nil, fmt.Errorf("list wifconfigtemplates: %w", err)
	}
	return templates.Items, nil
}

// nextSpec builds the Version spec to persist: desired catalog-owned fields
// plus the existing pin carried forward (never wiped), or a freshly resolved
// pin for an unpinned Version when template reads succeeded.
func (c *Controller) nextSpec(
	ctx context.Context,
	log logger.Logger,
	name string,
	desired privatev1.VersionSpec,
	existing *privatev1.WIFConfigTemplateReference,
	templates []privatev1.WIFConfigTemplate,
	templateErr error,
) privatev1.VersionSpec {
	next := desired
	if existing != nil {
		next.WIFConfigTemplateReference = existing
		return next
	}
	if templateErr != nil {
		return next
	}
	pin, err := resolvePin(name, templates)
	if err != nil {
		// A single undigestable template must not fail the whole snapshot;
		// the Version stays unpinned and unavailable, which is fail-closed.
		log.Errorf(ctx, "resolve IAM template reference for Version %q failed: %v", name, err)
		return next
	}
	next.WIFConfigTemplateReference = pin
	return next
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

	templates, templateErr := c.templates(ctx)

	existing := make(map[string]*privatev1.Version, len(current.Items))

	for i := range current.Items {
		version := &current.Items[i]
		existing[version.Name] = version
	}

	// Update existing Versions first. A pinned template reference is never
	// wiped; an unpinned Version is pinned to its selected boundary.
	for name, spec := range desired {
		version, found := existing[name]
		if !found {
			continue
		}

		next := c.nextSpec(ctx, log, name, spec, version.Spec.WIFConfigTemplateReference, templates, templateErr)
		if specEqual(&version.Spec, &next) {
			continue
		}

		version.Spec = next
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
			Spec: c.nextSpec(ctx, log, name, spec, nil, templates, templateErr),
		}

		if err := c.apiClient.Create(ctx, version); err != nil {
			return fmt.Errorf("create version %q: %w", name, err)
		}

		log.Infof(ctx, "created Version %s", name)
	}

	// Delete stale Versions only after the complete snapshot has been applied.
	// Failed template reads block destructive stale-Version aging: pins
	// cannot be resolved and availability is unknown, so stale Versions are
	// retained until template reads recover.
	if templateErr != nil {
		log.Errorf(ctx, "preserving stale Versions because template reads failed: %v", templateErr)
		return nil
	}

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

func channelMinimum(channel privatev1.Channel) (int, int, error) {
	value := channel.Spec.MinimumSupportedVersion
	major, minor, valid := parseMajorMinor(value)
	if !valid || major < 0 || minor < 0 || fmt.Sprintf("%d.%d", major, minor) != value {
		return 0, 0, fmt.Errorf("channel %q has invalid minimumSupportedVersion %q", channel.Name, value)
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
	// The pinned template reference participates in equality: a carried or
	// freshly resolved pin is a deliberate spec change, never a wipe.
	return equality.Semantic.DeepEqual(a.WIFConfigTemplateReference, b.WIFConfigTemplateReference)
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

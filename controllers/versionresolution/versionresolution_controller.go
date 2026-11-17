package versionresolution

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/openshift-online/gecko/controllers/util/catalog"
	channelutil "github.com/openshift-online/gecko/controllers/util/channel"
	"github.com/openshift-online/gecko/controllers/util/logger"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilversion "k8s.io/apimachinery/pkg/util/version"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const requeueStable = 30 * time.Second

// Reconciler resolves the OCP release image for a cluster from the Version catalog.
type Reconciler struct {
	log       logger.Logger
	client    client.Client
	apiReader client.Reader
}

func (r *Reconciler) WithAPIReader(reader client.Reader) *Reconciler { r.apiReader = reader; return r }
func (r *Reconciler) reader() client.Reader {
	if r.apiReader != nil {
		return r.apiReader
	}
	return r.client
}

// NewReconciler creates a new version-resolution Reconciler.
func NewReconciler(log logger.Logger, c client.Client) *Reconciler {
	return &Reconciler{
		log:    log,
		client: c,
	}
}

// Reconcile runs the version-resolution loop for one cluster event.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	clusterID := req.Name

	var cluster privatev1.Cluster
	if err := r.reader().Get(ctx, req.NamespacedName, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			r.log.Infof(ctx, "vr: cluster %s not found, skipping", clusterID)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("vr: get cluster %s: %w", clusterID, err)
	}

	if !cluster.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}
	version := privatev1.ClusterInstallationVersion(&cluster)
	if version == "" {
		r.log.Infof(ctx, "vr: cluster %s: release version not set, waiting for next event", clusterID)
		if meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               "VersionResolved",
			Status:             metav1.ConditionUnknown,
			Reason:             "ReleaseVersionNotSet",
			Message:            "Release version not set in spec",
			ObservedGeneration: cluster.Generation,
		}) {
			if err := r.client.Status().Update(ctx, &cluster); err != nil && !apierrors.IsConflict(err) {
				return reconcile.Result{}, fmt.Errorf("vr: update cluster status %s: %w", clusterID, err)
			}
		}
		return reconcile.Result{}, nil
	}
	// Catalog rejection is terminal for this spec, including across restarts
	// and unrelated status or Channel events. A new generation permits retry.
	if !privatev1.CommittedIAMDispatch(cluster.Status.IAMDispatch, string(cluster.UID), string(cluster.UID), cluster.Status.VersionResolution) && catalog.ResolutionRejected(meta.FindStatusCondition(cluster.Status.Conditions, "VersionResolved"), cluster.Generation) {
		return reconcile.Result{}, nil
	}

	channelGroup := channelutil.DefaultChannelGroup
	if cluster.Spec.Release.ChannelGroup != "" {
		channelGroup = cluster.Spec.Release.ChannelGroup
	}

	// Pending release/channel intent must not invalidate the currently installed
	// image. Keep its previously resolved channel until the new release is approved.
	if !privatev1.ClusterReleaseApproved(&cluster) && cluster.Status.VersionResolution != nil && cluster.Status.VersionResolution.ChannelGroup != "" {
		channelGroup = cluster.Status.VersionResolution.ChannelGroup
	}

	channelVersion := version
	var platformChannel privatev1.Channel
	if err := r.client.Get(ctx, client.ObjectKey{Name: channelGroup}, &platformChannel); err != nil {
		if !apierrors.IsNotFound(err) {
			return reconcile.Result{}, fmt.Errorf("vr: get channel %s for cluster %s: %w", channelGroup, clusterID, err)
		}
		r.log.Infof(ctx, "vr: channel %s not found, resolving cluster %s in its current minor channel", channelGroup, clusterID)
	} else {
		// TODO: Revisit whether version-resolution should advance the concrete
		// channel directly when progressive rollout adds per-cluster admission.
		channelVersion, err = nextChannelVersion(version, platformChannel.Spec.FleetMinorVersion)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("vr: select channel version for cluster %s: %w", clusterID, err)
		}
	}

	channel, err := buildChannel(channelVersion, channelGroup)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("vr: build channel for cluster %s: %w", clusterID, err)
	}

	// Revalidate even a cached resolution. Keep its image pinned, but never let
	// cached success authorize new work after readiness/verification revocation.
	committed := cluster.Status.VersionResolution != nil && cluster.Status.VersionResolution.ReleaseVersion == version && cluster.Status.VersionResolution.ChannelGroup == channelGroup && privatev1.CommittedIAMDispatch(cluster.Status.IAMDispatch, string(cluster.UID), string(cluster.UID), cluster.Status.VersionResolution)
	var release *privatev1.Version
	if !committed {
		release, err = catalog.Available(ctx, r.reader(), version, channelGroup)
		if err != nil {
			return r.catalogFailure(ctx, &cluster, err)
		}
	}
	// Pin the image for this version and channel group. Fleet authorization can
	// advance the CVO channel independently without resolving the image again.
	if vr := cluster.Status.VersionResolution; catalog.ResolutionMatches(vr, version, channelGroup, release, committed) {
		changed := meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{Type: "VersionResolved", Status: metav1.ConditionTrue, Reason: "VersionResolved", Message: "Installation release has a pinned payload and IAM contract", ObservedGeneration: cluster.Generation})
		if vr.CincinnatiChannel != channel {
			vr.CincinnatiChannel = channel
			changed = true
		}
		if changed {
			if err := r.client.Status().Update(ctx, &cluster); err != nil && !apierrors.IsConflict(err) {
				return reconcile.Result{}, fmt.Errorf("vr: update cluster channel %s: %w", clusterID, err)
			}
		}
		return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Write VR result and VersionResolved condition to status.
	cluster.Status.VersionResolution = &privatev1.VersionResolutionResult{
		ReleaseImage:         release.Spec.ReleaseImage,
		WIFConfigTemplateRef: release.Spec.WIFConfigTemplateRef.DeepCopy(),
		ReleaseVersion:       version,
		CincinnatiChannel:    channel,
		ChannelGroup:         channelGroup,
	}
	meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               "VersionResolved",
		Status:             metav1.ConditionTrue,
		Reason:             "VersionResolved",
		Message:            fmt.Sprintf("Version %s resolved from the available catalog", version),
		ObservedGeneration: cluster.Generation,
	})
	if err := r.client.Status().Update(ctx, &cluster); err != nil {
		if apierrors.IsConflict(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("vr: update cluster status %s: %w", clusterID, err)
	}

	r.log.Info(ctx, "release resolved from the available catalog")
	return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *Reconciler) catalogFailure(ctx context.Context, c *privatev1.Cluster, err error) (reconcile.Result, error) {
	var unavailable *catalog.UnavailableError
	if !errors.As(err, &unavailable) {
		return reconcile.Result{}, err
	}
	if meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{Type: "VersionResolved", Status: metav1.ConditionFalse, Reason: unavailable.Reason, Message: unavailable.Message, ObservedGeneration: c.Generation}) {
		if err := r.client.Status().Update(ctx, c); err != nil && !apierrors.IsConflict(err) {
			return reconcile.Result{}, fmt.Errorf("vr: update cluster status failed")
		}
	}
	if catalog.ResolutionRejected(meta.FindStatusCondition(c.Status.Conditions, "VersionResolved"), c.Generation) {
		return reconcile.Result{}, nil
	}
	return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
}

// buildChannel constructs the Cincinnati channel name from a version string and channel group.
// e.g. "4.22.0-ec.4" + "stable" → "stable-4.22"
func buildChannel(version, channelGroup string) (string, error) {
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid version %q: expected at least major.minor", version)
	}
	return fmt.Sprintf("%s-%s.%s", channelGroup, parts[0], parts[1]), nil
}

// nextChannelVersion returns a version in the minor channel that CVO should
// query. Minor upgrades are exposed one at a time even when the fleet minor is
// more than one release ahead of the cluster.
func nextChannelVersion(version, fleetMinorVersion string) (string, error) {
	if fleetMinorVersion == "" {
		return version, nil
	}

	current, err := utilversion.ParseSemantic(version)
	if err != nil {
		return "", fmt.Errorf("parse current version %q: %w", version, err)
	}
	fleetMinor, err := utilversion.ParseSemantic(fleetMinorVersion + ".0")
	if err != nil {
		return "", fmt.Errorf("parse fleet minor version %q: %w", fleetMinorVersion, err)
	}

	if current.Major() != fleetMinor.Major() || current.Minor() >= fleetMinor.Minor() {
		return version, nil
	}

	return fmt.Sprintf("%d.%d.0", current.Major(), current.Minor()+1), nil
}

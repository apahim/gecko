package nodepoolvrresolution

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/openshift-online/gecko/controllers/util/catalog"
	"github.com/openshift-online/gecko/controllers/util/logger"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	adapterName         = "nodepool-vr-controller"
	defaultChannelGroup = "candidate"
	requeueStable       = 30 * time.Second
)

// Reconciler resolves the OCP release image for a node pool from the Version catalog.
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

// NewReconciler creates a new nodepool-vr Reconciler.
func NewReconciler(log logger.Logger, c client.Client) *Reconciler {
	return &Reconciler{
		log:    log,
		client: c,
	}
}

// Reconcile runs the nodepool-vr loop for one nodepool event.
// req.Namespace = project namespace, req.Name = nodepoolID.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	nodepoolID := req.Name

	var np privatev1.NodePool
	if err := r.reader().Get(ctx, req.NamespacedName, &np); err != nil {
		if apierrors.IsNotFound(err) {
			r.log.Infof(ctx, "nodepool-vr: nodepool %s not found, skipping", nodepoolID)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("nodepool-vr: get nodepool %s: %w", nodepoolID, err)
	}

	if !np.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}
	// Verify the parent cluster still exists.
	clusterID := np.Spec.ClusterID
	clusterKey := types.NamespacedName{Namespace: req.Namespace, Name: clusterID}
	var cluster privatev1.Cluster
	if err := r.reader().Get(ctx, clusterKey, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			r.log.Infof(ctx, "nodepool-vr: cluster %s not found for nodepool %s, skipping", clusterID, nodepoolID)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("nodepool-vr: get cluster %s: %w", clusterID, err)
	}

	if np.Spec.Release.Version == "" {
		r.log.Infof(ctx, "nodepool-vr: nodepool %s: release version not set, waiting for next event", nodepoolID)
		if meta.SetStatusCondition(&np.Status.Conditions, metav1.Condition{
			Type:               "NodePoolVersionResolved",
			Status:             metav1.ConditionUnknown,
			Reason:             "ReleaseVersionNotSet",
			Message:            "Release version not set in spec",
			ObservedGeneration: np.Generation,
		}) {
			if err := r.client.Status().Update(ctx, &np); err != nil && !apierrors.IsConflict(err) {
				return reconcile.Result{}, fmt.Errorf("nodepool-vr: update nodepool status %s: %w", nodepoolID, err)
			}
		}
		return reconcile.Result{}, nil
	}
	// Catalog rejection is terminal for this spec, including across restarts
	// and unrelated status or Channel events. A new generation permits retry.
	// Keep the last applied release until the upgrade coordinator has approved
	// exactly this desired version, channel, and schedule. Initial installs pass.
	if !privatev1.NodePoolReleaseApproved(&np) {
		return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if !privatev1.CommittedIAMDispatch(np.Status.IAMDispatch, string(np.UID), string(cluster.UID), np.Status.VersionResolution) && catalog.ResolutionRejected(meta.FindStatusCondition(np.Status.Conditions, "NodePoolVersionResolved"), np.Generation) {
		return reconcile.Result{}, nil
	}
	version := np.Spec.Release.Version

	channelGroup := defaultChannelGroup
	if np.Spec.Release.ChannelGroup != "" {
		channelGroup = np.Spec.Release.ChannelGroup
	}

	channel, err := buildChannel(version, channelGroup)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("nodepool-vr: build channel for nodepool %s: %w", nodepoolID, err)
	}

	committed := np.Status.VersionResolution != nil && np.Status.VersionResolution.ReleaseVersion == version && np.Status.VersionResolution.ChannelGroup == channelGroup && privatev1.CommittedIAMDispatch(np.Status.IAMDispatch, string(np.UID), string(cluster.UID), np.Status.VersionResolution)
	var release *privatev1.Version
	if !committed {
		release, err = catalog.Available(ctx, r.reader(), version, channelGroup)
		if err != nil {
			return r.catalogFailure(ctx, &np, err)
		}
	}
	// Check already resolved — retain the pinned image, not cached readiness.
	if catalog.ResolutionMatches(np.Status.VersionResolution, version, channelGroup, release, committed) {
		if meta.SetStatusCondition(&np.Status.Conditions, metav1.Condition{Type: "NodePoolVersionResolved", Status: metav1.ConditionTrue, Reason: "VersionResolved", Message: "Installation release has a pinned payload and IAM contract", ObservedGeneration: np.Generation}) {
			if err := r.client.Status().Update(ctx, &np); err != nil && !apierrors.IsConflict(err) {
				return reconcile.Result{}, err
			}
		}
		return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
	}
	r.log.Infof(ctx, "nodepool-vr: nodepool %s: resolving version %s via channel %s", nodepoolID, version, channel)

	// Write VR result and NodePoolVersionResolved condition to status.
	np.Status.VersionResolution = &privatev1.VersionResolutionResult{
		ReleaseImage:         release.Spec.ReleaseImage,
		WIFConfigTemplateRef: release.Spec.WIFConfigTemplateRef.DeepCopy(),
		ReleaseVersion:       version,
		CincinnatiChannel:    channel,
		ChannelGroup:         channelGroup,
	}
	meta.SetStatusCondition(&np.Status.Conditions, metav1.Condition{
		Type:               "NodePoolVersionResolved",
		Status:             metav1.ConditionTrue,
		Reason:             "VersionResolved",
		Message:            fmt.Sprintf("Version %s resolved from the available catalog", version),
		ObservedGeneration: np.Generation,
	})
	if err := r.client.Status().Update(ctx, &np); err != nil {
		if apierrors.IsConflict(err) {
			return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
		}
		return reconcile.Result{}, fmt.Errorf("nodepool-vr: update nodepool status %s: %w", nodepoolID, err)
	}
	r.log.Info(ctx, "release resolved from the available catalog")
	return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *Reconciler) catalogFailure(ctx context.Context, p *privatev1.NodePool, err error) (reconcile.Result, error) {
	var unavailable *catalog.UnavailableError
	if !errors.As(err, &unavailable) {
		return reconcile.Result{}, err
	}
	if meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{Type: "NodePoolVersionResolved", Status: metav1.ConditionFalse, Reason: unavailable.Reason, Message: unavailable.Message, ObservedGeneration: p.Generation}) {
		if err := r.client.Status().Update(ctx, p); err != nil && !apierrors.IsConflict(err) {
			return reconcile.Result{}, fmt.Errorf("update nodepool status failed")
		}
	}
	if catalog.ResolutionRejected(meta.FindStatusCondition(p.Status.Conditions, "NodePoolVersionResolved"), p.Generation) {
		return reconcile.Result{}, nil
	}
	return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
}

// buildChannel constructs the Cincinnati channel name from a version string and channel group.
// e.g. "4.22.0-rc.5" + "candidate" → "candidate-4.22"
func buildChannel(version, channelGroup string) (string, error) {
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid version %q: expected at least major.minor", version)
	}
	return fmt.Sprintf("%s-%s.%s", channelGroup, parts[0], parts[1]), nil
}

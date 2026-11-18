// Package hc implements the hc-controller reconciler for managing HostedClusters via kube-applier-gcp.
package hc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/openshift-online/gecko/controllers/client/transport"
	"github.com/openshift-online/gecko/controllers/hc/manifest"
	"github.com/openshift-online/gecko/controllers/util/constants"
	"github.com/openshift-online/gecko/controllers/util/logger"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	adapterName            = "hc-controller"
	NodePoolClusterIDField = "spec.clusterID"

	requeuePending = 15 * time.Second
	requeueStable  = 5 * time.Minute
)

// Reconciler implements the hc-controller reconcile loop.
type Reconciler struct {
	transport           transport.Client
	log                 logger.Logger
	client              client.Client
	customerLabels      map[string]string
	pullSecretStoreName string
	pullSecretGCPKey    string
}

// New creates a new Reconciler.
func New(transport transport.Client, log logger.Logger, c client.Client, customerLabels map[string]string, pullSecretStoreName, pullSecretGCPKey string) *Reconciler {
	return &Reconciler{
		transport:           transport,
		log:                 log,
		client:              c,
		customerLabels:      customerLabels,
		pullSecretStoreName: pullSecretStoreName,
		pullSecretGCPKey:    pullSecretGCPKey,
	}
}

// Reconcile runs the hc-controller loop for one cluster event.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	groupKey, err := transport.ClusterGroupKey(req.Namespace, req.Name)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("build group key: %w", err)
	}
	log := r.log.With("controller", adapterName).With("cluster_id", req.Name)

	var cluster privatev1.Cluster
	if err := r.client.Get(ctx, req.NamespacedName, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			log.Infof(ctx, "cluster not found, skipping")
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("%s: get cluster: %w", adapterName, err)
	}

	// Handle deletion.
	if !cluster.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &cluster, log)
	}

	// Ensure finalizer is present.
	if !controllerutil.ContainsFinalizer(&cluster, constants.FinalizerCluster) {
		controllerutil.AddFinalizer(&cluster, constants.FinalizerCluster)
		if err := r.client.Update(ctx, &cluster); err != nil {
			return reconcile.Result{}, fmt.Errorf("%s: add finalizer: %w", adapterName, err)
		}
		return reconcile.Result{}, nil // re-reconcile with finalizer in place
	}

	ready, err := r.prerequisitesReady(ctx, &cluster, log)
	if err != nil {
		return reconcile.Result{}, err
	}
	if !ready {
		return reconcile.Result{RequeueAfter: requeuePending}, nil
	}

	placement := cluster.Status.PlacementResult
	vr := cluster.Status.VersionResolution
	safeName := cluster.Spec.SafeName
	if safeName == "" {
		safeName = privatev1.DefaultSafeName(cluster.Name, cluster.UID)
	}

	// Extract platform fields.
	var gcpProjectID, gcpRegion, gcpNetwork, gcpSubnet, gcpEndpointAccess string
	var wifProjectNumber, wifPoolID, wifProviderID string
	var nodePoolEmail, controlPlaneEmail, cloudControllerEmail string
	var storageEmail, imageRegistryEmail, networkEmail, ingressEmail string
	if gcp := cluster.Spec.Platform.GCP; gcp != nil {
		gcpProjectID = gcp.ProjectID
		gcpRegion = gcp.Region
		gcpNetwork = gcp.Network
		gcpSubnet = gcp.Subnet
		gcpEndpointAccess = gcp.EndpointAccess
		wif := gcp.WorkloadIdentity
		wifProjectNumber = wif.ProjectNumber
		wifPoolID = wif.PoolID
		wifProviderID = wif.ProviderID
		if sa := wif.ServiceAccountsRef; sa != nil {
			nodePoolEmail = sa.NodePoolEmail
			controlPlaneEmail = sa.ControlPlaneEmail
			cloudControllerEmail = sa.CloudControllerEmail
			storageEmail = sa.StorageEmail
			imageRegistryEmail = sa.ImageRegistryEmail
			networkEmail = sa.NetworkEmail
			if sa.IngressEmail != nil {
				ingressEmail = *sa.IngressEmail
			}
		}
	}

	// Build manifests. The ingress default certificate is staged: request it
	// once HyperShift has published the ACME delegation CNAME, then reference it
	// once cert-manager has produced the serving secret.
	enableIngress := ingressEmail != ""
	includeIngressCertificate := enableIngress && ingressCertificateRequested(&cluster)
	includeDefaultIngressCert := includeIngressCertificate &&
		(ingressCertificateReady(&cluster) || ingressDefaultCertificateApplied(&cluster))

	// TODO: CreatedBy is not yet in the orlop ClusterSpec.
	mwInput := manifest.Input{
		ClusterID:                 string(cluster.UID),
		ClusterName:               safeName,
		Generation:                cluster.Generation,
		CreatedBy:                 cluster.Annotations[constants.AnnotationCreatedBy],
		InfraID:                   cluster.Spec.InfraID,
		IssuerURL:                 cluster.Spec.IssuerURL,
		GCPProjectID:              gcpProjectID,
		GCPRegion:                 gcpRegion,
		GCPNetwork:                gcpNetwork,
		GCPSubnet:                 gcpSubnet,
		GCPEndpointAccess:         gcpEndpointAccess,
		WIFProjectNumber:          wifProjectNumber,
		WIFPoolID:                 wifPoolID,
		WIFProviderID:             wifProviderID,
		NodePoolEmail:             nodePoolEmail,
		ControlPlaneEmail:         controlPlaneEmail,
		CloudControllerEmail:      cloudControllerEmail,
		StorageEmail:              storageEmail,
		ImageRegistryEmail:        imageRegistryEmail,
		NetworkEmail:              networkEmail,
		IngressEmail:              ingressEmail,
		IncludeIngressCertificate: includeIngressCertificate,
		IncludeDefaultIngressCert: includeDefaultIngressCert,
		ReleaseImage:              vr.ReleaseImage,
		ReleaseChannel:            vr.CincinnatiChannel,
		BaseDomain:                placement.BaseDomain,
		PullSecretStoreName:       r.pullSecretStoreName,
		PullSecretGCPKey:          r.pullSecretGCPKey,
		ResourceLabels:            r.customerLabels,
	}

	manifests, err := manifest.Build(mwInput)
	if err != nil {
		return reconcile.Result{}, r.preparationFailure(ctx, &cluster, err)
	}

	mwStatus, err := r.applyResources(ctx, &cluster, placement.ManagementClusterName, groupKey, manifests)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("%s: apply resources: %w", adapterName, err)
	}

	// If status is stale, skip condition updates and requeue quickly.
	if mwStatus != nil && mwStatus.Stale {
		log.Infof(ctx, "hc-controller: cluster %s status is stale, requeueing after %s", req.Name, requeuePending)
		return reconcile.Result{RequeueAfter: requeuePending}, nil
	}

	// Write status conditions — only update if something changed.
	statusChanged, err := r.applyStatusConditions(&cluster, mwStatus, enableIngress, includeIngressCertificate, includeDefaultIngressCert)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("%s: apply status feedback: %w", adapterName, err)
	}
	if statusChanged {
		if err := r.client.Status().Update(ctx, &cluster); err != nil {
			if apierrors.IsConflict(err) {
				return reconcile.Result{}, nil
			}
			return reconcile.Result{}, fmt.Errorf("%s: update cluster status: %w", adapterName, err)
		}
	}

	if !meta.IsStatusConditionTrue(cluster.Status.Conditions, "ResourcesApplied") {
		log.Infof(ctx, "hc-controller: cluster %s resources not yet applied, requeueing after %s", req.Name, requeuePending)
		return reconcile.Result{RequeueAfter: requeuePending}, nil
	}

	if !meta.IsStatusConditionTrue(cluster.Status.Conditions, "HostedClusterAvailable") {
		log.Infof(ctx, "hc-controller: cluster %s not yet available, requeueing after %s", req.Name, requeuePending)
		return reconcile.Result{RequeueAfter: requeuePending}, nil
	}

	log.Infof(ctx, "hc-controller: cluster %s reconciled, requeueing after %s", req.Name, requeueStable)
	return reconcile.Result{RequeueAfter: requeueStable}, nil
}

// prerequisitesReady records why a Cluster cannot yet be applied to its HostedCluster.
func (r *Reconciler) prerequisitesReady(ctx context.Context, cluster *privatev1.Cluster, log logger.Logger) (bool, error) {
	if cluster.Status.PlacementResult == nil || cluster.Status.PlacementResult.ManagementClusterName == "" {
		if r.setWaitingConditions(cluster, "PlacementNotReady", "Waiting for placement to select a management cluster") {
			if err := r.client.Status().Update(ctx, cluster); err != nil && !apierrors.IsConflict(err) {
				return false, fmt.Errorf("%s: update cluster status: %w", adapterName, err)
			}
		}
		log.Infof(ctx, "placement not ready, requeueing after %s", requeuePending)
		return false, nil
	}

	if cluster.Status.VersionResolution == nil {
		if r.setWaitingConditions(cluster, "VersionResolutionNotReady", "Waiting for version resolution") {
			if err := r.client.Status().Update(ctx, cluster); err != nil && !apierrors.IsConflict(err) {
				return false, fmt.Errorf("%s: update cluster status: %w", adapterName, err)
			}
		}
		log.Infof(ctx, "version resolution not ready, requeueing after %s", requeuePending)
		return false, nil
	}

	if cluster.Status.VersionResolution.ReleaseVersion != privatev1.ClusterInstallationVersion(cluster) {
		msg := fmt.Sprintf("VR version %q does not match spec version %q",
			cluster.Status.VersionResolution.ReleaseVersion, privatev1.ClusterInstallationVersion(cluster))
		if r.setWaitingConditions(cluster, "VersionMismatch", msg) {
			if err := r.client.Status().Update(ctx, cluster); err != nil && !apierrors.IsConflict(err) {
				return false, fmt.Errorf("%s: update cluster status: %w", adapterName, err)
			}
		}
		log.Infof(ctx, "vr version %q does not match spec version %q, requeueing after %s",
			cluster.Status.VersionResolution.ReleaseVersion, privatev1.ClusterInstallationVersion(cluster), requeuePending)
		return false, nil
	}

	return true, nil
}

// handleDeletion cleans up management-cluster resources and removes the finalizer.
// Deletion flow:
// 1. Call transport.Delete to enqueue DeleteDesires (async)
// 2. Requeue, wait for kube-applier-gcp to process (poll GetDeleteStatus)
// 3. Once all DeleteDesires report Successful=True, cleanup DeleteDesires
// 4. Remove finalizer
func (r *Reconciler) handleDeletion(ctx context.Context, cluster *privatev1.Cluster, log logger.Logger) (reconcile.Result, error) {
	if !controllerutil.ContainsFinalizer(cluster, constants.FinalizerCluster) {
		return reconcile.Result{}, nil
	}

	var nodePools privatev1.NodePoolList
	if err := r.client.List(ctx, &nodePools,
		client.InNamespace(cluster.Namespace),
		client.MatchingFields{NodePoolClusterIDField: cluster.Name},
	); err != nil {
		return reconcile.Result{}, fmt.Errorf("%s: list nodepools: %w", adapterName, err)
	}

	// Start child deletion before cleaning up Cluster resources. Both cleanup paths
	// may progress together, but the Cluster finalizer waits for every NodePool.
	nodePoolsRemain := false
	var deleteErrors []error
	for i := range nodePools.Items {
		nodePool := &nodePools.Items[i]
		nodePoolsRemain = true
		if !nodePool.DeletionTimestamp.IsZero() {
			continue
		}
		if err := r.client.Delete(ctx, nodePool); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			deleteErrors = append(deleteErrors, fmt.Errorf("%s: delete nodepool %s: %w", adapterName, nodePool.Name, err))
			continue
		}
		log.Infof(ctx, "%s: deleting nodepool %s for cluster %s", adapterName, nodePool.Name, cluster.Name)
	}
	if err := errors.Join(deleteErrors...); err != nil {
		return reconcile.Result{}, err
	}

	// Only call transport.Delete if resources were applied to an MC.
	if meta.FindStatusCondition(cluster.Status.Conditions, "ResourcesApplied") != nil &&
		cluster.Status.PlacementResult != nil && cluster.Status.PlacementResult.ManagementClusterName != "" {
		mcName := cluster.Status.PlacementResult.ManagementClusterName

		groupKey, err := transport.ClusterGroupKey(cluster.Namespace, cluster.Name)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("%s: build group key: %w", adapterName, err)
		}

		// Check if deletion already in progress by querying delete status first.
		deleteStatus, err := r.transport.GetDeleteStatus(ctx, mcName, groupKey)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("%s: get delete status: %w", adapterName, err)
		}

		if deleteStatus.TotalCount == 0 {
			// No DeleteDesires exist.
			if deleteStatus.ApplyDesiresCount > 0 {
				// ApplyDesires still present → deletion never started, call Delete().
				log.Infof(ctx, "%s: deleting resources for cluster %s from %s", adapterName, cluster.Name, mcName)
				if err := r.transport.Delete(ctx, mcName, groupKey); err != nil {
					return reconcile.Result{}, fmt.Errorf("%s: delete resources: %w", adapterName, err)
				}
				log.Infof(ctx, "%s: delete initiated for cluster %s, requeueing to poll status", adapterName, cluster.Name)
				return reconcile.Result{RequeueAfter: requeuePending}, nil
			}
			// TotalCount=0 and ApplyDesiresCount=0 → deletion already complete (no-op), proceed to finalizer.
		}

		if !deleteStatus.AllSuccessful {
			// Deletion in progress — wait for completion.
			log.Infof(ctx, "%s: deletion in progress for cluster %s (%d/%d pending), requeueing",
				adapterName, cluster.Name, deleteStatus.PendingCount, deleteStatus.TotalCount)
			return reconcile.Result{RequeueAfter: requeuePending}, nil
		}

		// All DeleteDesires successful — cleanup before removing finalizer.
		log.Infof(ctx, "%s: deletion complete for cluster %s, cleaning up %d DeleteDesires",
			adapterName, cluster.Name, deleteStatus.TotalCount)
		if err := r.transport.CleanupDeleteDesires(ctx, mcName, groupKey); err != nil {
			return reconcile.Result{}, fmt.Errorf("%s: cleanup delete desires: %w", adapterName, err)
		}
	}

	if nodePoolsRemain {
		log.Infof(ctx, "%s: waiting for nodepools to be deleted for cluster %s, requeueing", adapterName, cluster.Name)
		return reconcile.Result{RequeueAfter: requeuePending}, nil
	}

	controllerutil.RemoveFinalizer(cluster, constants.FinalizerCluster)
	if err := r.client.Update(ctx, cluster); err != nil {
		return reconcile.Result{}, fmt.Errorf("%s: remove finalizer: %w", adapterName, err)
	}

	log.Infof(ctx, "%s: finalizer removed for cluster %s", adapterName, cluster.Name)
	return reconcile.Result{}, nil
}

// setWaitingConditions sets ResourcesApplied and HostedClusterAvailable to Unknown.
// Returns true if either condition changed.
func (r *Reconciler) setWaitingConditions(cluster *privatev1.Cluster, reason, message string) bool {
	gen := cluster.Generation
	a := meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               "ResourcesApplied",
		Status:             metav1.ConditionUnknown,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: gen,
	})
	b := meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               "HostedClusterAvailable",
		Status:             metav1.ConditionUnknown,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: gen,
	})
	return a || b
}

// applyStatusConditions derives conditions from the resource status and writes them to the cluster.
// Returns true if any condition changed.
func (r *Reconciler) applyStatusConditions(cluster *privatev1.Cluster, mwStatus *transport.Status, enableIngress, ingressCertRequested, ingressCertApplied bool) (bool, error) {
	gen := cluster.Generation

	if mwStatus == nil {
		a := meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               "ResourcesApplied",
			Status:             metav1.ConditionFalse,
			Reason:             "ResourcesNotFound",
			Message:            "Resources have not been applied yet",
			ObservedGeneration: gen,
		})
		b := meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               "HostedClusterAvailable",
			Status:             metav1.ConditionFalse,
			Reason:             "ResourcesNotFound",
			Message:            "Resources have not been applied yet",
			ObservedGeneration: gen,
		})
		c := meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               "ApiCertificateReady",
			Status:             metav1.ConditionFalse,
			Reason:             "ResourcesNotFound",
			Message:            "Resources have not been applied yet",
			ObservedGeneration: gen,
		})
		e, f := false, false
		if enableIngress {
			e = meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
				Type:               "IngressCredentialsReady",
				Status:             metav1.ConditionFalse,
				Reason:             "ResourcesNotFound",
				Message:            "Resources have not been applied yet",
				ObservedGeneration: gen,
			})
		}
		if ingressCertRequested {
			f = meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
				Type:               "IngressCertificateReady",
				Status:             metav1.ConditionFalse,
				Reason:             "ResourcesNotFound",
				Message:            "Resources have not been applied yet",
				ObservedGeneration: gen,
			})
		}
		return a || b || c || e || f, nil
	}

	// Derive ResourcesApplied from top-level conditions.
	appliedStatus, appliedReason, appliedMessage := firstCondition(mwStatus.Conditions, "Applied")

	// Derive HostedClusterAvailable and HostedClusterResult fields from HC resource status.
	clusterNS := fmt.Sprintf("clusters-%s", cluster.UID)
	safeName := cluster.Spec.SafeName
	if safeName == "" {
		safeName = privatev1.DefaultSafeName(cluster.Name, cluster.UID)
	}
	hcKey := transport.ResourceKey(constants.HyperShiftGroup, constants.HyperShiftVersion, "hostedclusters",
		clusterNS, safeName)
	hcFeedback := mwStatus.ResourceStatuses[hcKey]
	// The map key can be present but empty: extractResourceStatuses inserts an empty
	// map when the HostedCluster's live content has not synced yet (KubeContent nil),
	// which is a different condition than the "read status document never arrived"
	// case mwStatus.Stale already covers. Treat an empty map the same as no feedback —
	// key presence alone is not proof this is fresh data. extractHCFields always sets
	// at least availableVersions and versionConditions when it actually runs, so a
	// non-empty map reliably means live content was read.
	hasHCFeedback := len(hcFeedback) > 0
	availableStatus := string(metav1.ConditionFalse)
	if v, ok := hcFeedback["availableCondition"]; ok {
		availableStatus = v
	}

	// HostedClusterResult (including the observed HyperShift conditions) is only
	// touched once something has been observed at least once. Before that, there
	// is nothing meaningful to report and the field stays nil.
	resultChanged := false
	if hasHCFeedback || cluster.Status.HostedClusterResult != nil {
		var err error
		resultChanged, err = r.applyHostedClusterResult(cluster, hasHCFeedback, hcFeedback, gen)
		if err != nil {
			return false, fmt.Errorf("%s: apply hosted cluster result: %w", adapterName, err)
		}
	}

	// Derive ApiCertificateReady from Certificate resource status.
	certStatus, certReason, certMessage := certificateReadyCondition(mwStatus.ResourceStatuses,
		transport.ResourceKey("cert-manager.io", "v1", "certificates", clusterNS, "external-api-cert"))

	// Ingress readiness is only reported for clusters that enable Ingress.
	var ingressCredentials, ingressDefaultCert *metav1.Condition
	if enableIngress && cluster.Status.HostedClusterResult != nil {
		ingressCredentials = meta.FindStatusCondition(cluster.Status.HostedClusterResult.ObservedConditions, "IngressCredentialsFound")
		ingressDefaultCert = meta.FindStatusCondition(cluster.Status.HostedClusterResult.ObservedConditions, "IngressDefaultCertificateSynced")
	}

	// Gate availability on ingress readiness. HyperShift treats a missing ingress
	// certificate as non-fatal, but Gecko does not report such a cluster as available.
	availableReason := "HostedClusterNotAvailable"
	if availableStatus == "True" {
		availableReason = "HostedClusterAvailable"
	}
	availableMessage := ""
	if availableStatus == "True" {
		switch {
		case ingressCredentials != nil && ingressCredentials.Status == metav1.ConditionFalse:
			availableStatus = string(metav1.ConditionFalse)
			availableReason = ingressCredentials.Reason
			availableMessage = ingressCredentials.Message
		case ingressDefaultCert != nil && ingressDefaultCert.Status == metav1.ConditionFalse:
			availableStatus = string(metav1.ConditionFalse)
			availableReason = ingressDefaultCert.Reason
			availableMessage = ingressDefaultCert.Message
		}
	}

	a := meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               "ResourcesApplied",
		Status:             metav1.ConditionStatus(appliedStatus),
		Reason:             appliedReason,
		Message:            appliedMessage,
		ObservedGeneration: gen,
	})
	b := meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               "HostedClusterAvailable",
		Status:             metav1.ConditionStatus(availableStatus),
		Reason:             availableReason,
		Message:            availableMessage,
		ObservedGeneration: gen,
	})
	d := meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               "ApiCertificateReady",
		Status:             metav1.ConditionStatus(certStatus),
		Reason:             certReason,
		Message:            certMessage,
		ObservedGeneration: gen,
	})

	changed := a || b || d || resultChanged
	changed = applyIngressConditions(cluster, mwStatus, ingressCredentials, ingressDefaultCert,
		ingressCertRequested, ingressCertApplied, hasHCFeedback, clusterNS, gen) || changed

	return changed, nil
}

// applyIngressConditions writes the Gecko-owned ingress conditions: the
// HyperShift-observed credentials and default-certificate sync, the
// cert-manager certificate readiness, and the locally-owned reference marker.
func applyIngressConditions(cluster *privatev1.Cluster, mwStatus *transport.Status, ingressCredentials, ingressDefaultCert *metav1.Condition,
	ingressCertRequested, ingressCertApplied, hasHCFeedback bool, clusterNS string, gen int64) bool {
	changed := false
	if ingressCredentials != nil {
		changed = meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               "IngressCredentialsReady",
			Status:             ingressCredentials.Status,
			Reason:             ingressCredentials.Reason,
			Message:            ingressCredentials.Message,
			ObservedGeneration: gen,
		}) || changed
	}
	if ingressDefaultCert != nil {
		changed = meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               "IngressDefaultCertificateReady",
			Status:             ingressDefaultCert.Status,
			Reason:             ingressDefaultCert.Reason,
			Message:            ingressDefaultCert.Message,
			ObservedGeneration: gen,
		}) || changed
	}
	if ingressCertRequested {
		ingressCertStatus, ingressCertReason, ingressCertMessage := certificateReadyCondition(mwStatus.ResourceStatuses,
			transport.ResourceKey("cert-manager.io", "v1", "certificates", clusterNS, "ingress-default-cert"))
		changed = meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               "IngressCertificateReady",
			Status:             metav1.ConditionStatus(ingressCertStatus),
			Reason:             ingressCertReason,
			Message:            ingressCertMessage,
			ObservedGeneration: gen,
		}) || changed
	}
	// The reference marker is owned by Gecko, not derived from HyperShift
	// feedback: HyperShift can only report IngressDefaultCertificateSynced
	// after the reference has been applied, so a feedback-derived marker could
	// not cover the window between the first apply and the first report.
	if ingressCertApplied {
		changed = meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               "IngressDefaultCertificateApplied",
			Status:             metav1.ConditionTrue,
			Reason:             "ReferenceApplied",
			Message:            "HostedCluster references the ingress default certificate",
			ObservedGeneration: gen,
		}) || changed
	}
	// When fresh HostedCluster feedback omits an ingress observation that was
	// previously reported, mark the derived condition Unknown rather than
	// leaving the last value in place: conditions are consumed by automations
	// and must not go stale. A missing feedback document (empty read) is not
	// evidence the observation disappeared, so the reset only applies to fresh
	// feedback.
	if hasHCFeedback {
		if ingressCredentials == nil {
			changed = resetConditionToUnknown(cluster, "IngressCredentialsReady", "IngressCredentialsFound", gen) || changed
		}
		if ingressDefaultCert == nil {
			changed = resetConditionToUnknown(cluster, "IngressDefaultCertificateReady", "IngressDefaultCertificateSynced", gen) || changed
		}
	}
	return changed
}

// resetConditionToUnknown marks an existing condition Unknown, reporting which
// HyperShift observation is no longer provided. Conditions that do not exist
// yet are left absent.
func resetConditionToUnknown(cluster *privatev1.Cluster, conditionType, observation string, gen int64) bool {
	if meta.FindStatusCondition(cluster.Status.Conditions, conditionType) == nil {
		return false
	}
	return meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             metav1.ConditionUnknown,
		Reason:             "ObservationNotReported",
		Message:            "HyperShift is not reporting " + observation,
		ObservedGeneration: gen,
	})
}

// certificateReadyCondition derives a Ready-style condition from a cert-manager
// Certificate resource status.
func certificateReadyCondition(statuses map[string]map[string]string, key string) (status, reason, message string) {
	status, reason, message = string(metav1.ConditionFalse), "CertificateNotReady", ""
	if feedback, ok := statuses[key]; ok {
		if v, ok := feedback["readyCondition"]; ok {
			if v == string(metav1.ConditionTrue) || v == string(metav1.ConditionFalse) || v == string(metav1.ConditionUnknown) {
				status = v
				if v == string(metav1.ConditionTrue) {
					reason = "CertificateReady"
				}
			}
		}
	}
	return status, reason, message
}

// observedHCCondition returns a condition from the previous reconciliation's
// HostedCluster feedback.
func observedHCCondition(cluster *privatev1.Cluster, conditionType string) *metav1.Condition {
	if cluster.Status.HostedClusterResult == nil {
		return nil
	}
	return meta.FindStatusCondition(cluster.Status.HostedClusterResult.ObservedConditions, conditionType)
}

// ingressCertificateRequested reports whether the ingress Certificate has been
// requested. HyperShift reports GCPACMECNAMEAvailable=True once the ACME
// delegation CNAME exists. Once the Certificate has reported status, keep
// managing it even if feedback is momentarily absent.
func ingressCertificateRequested(cluster *privatev1.Cluster) bool {
	if c := observedHCCondition(cluster, "GCPACMECNAMEAvailable"); c != nil && c.Status == metav1.ConditionTrue {
		return true
	}
	return meta.FindStatusCondition(cluster.Status.Conditions, "IngressCertificateReady") != nil
}

// ingressCertificateReady reports whether cert-manager produced the ingress
// serving secret.
func ingressCertificateReady(cluster *privatev1.Cluster) bool {
	c := meta.FindStatusCondition(cluster.Status.Conditions, "IngressCertificateReady")
	return c != nil && c.Status == metav1.ConditionTrue
}

// ingressDefaultCertificateApplied reports whether the HostedCluster already
// references the ingress Certificate. Once referenced, keep the reference so a
// transient feedback gap cannot remove it. The marker is set by Gecko when the
// reference is rendered, not derived from HyperShift feedback, so it also
// covers the window before HyperShift first reports the reference.
func ingressDefaultCertificateApplied(cluster *privatev1.Cluster) bool {
	c := meta.FindStatusCondition(cluster.Status.Conditions, "IngressDefaultCertificateApplied")
	return c != nil && c.Status == metav1.ConditionTrue
}

// applyHostedClusterResult writes HC-owned HostedCluster feedback to
// cluster.Status.HostedClusterResult. It owns parsing hcFeedback end to end,
// including the fresh-vs-sticky decision for each field:
//
// APIEndpoint, Version, DesiredVersion, and AvailableUpdates are sticky: when
// hasHCFeedback is false they retain their previous value instead of resetting,
// because currentVersion/targetVersion must not change while feedback is
// momentarily unavailable (e.g. a transient transport read).
//
// ObservedConditions is different: it is a verbatim mirror of whatever
// HostedCluster feedback reported this reconcile (only "Degraded" is renamed to
// "HostedClusterDegraded" to avoid colliding with a future Gecko-native condition
// of the same short name). It does not carry forward previous values and does not
// synthesize placeholder entries for types HostedCluster did not report — the
// hc-controller does not hardcode which condition types matter, that is decided by
// whichever controller reads ObservedConditions. A consumer that needs a specific
// type and finds it absent here must treat that as Unknown itself, exactly as it
// would if the type were present with Status=Unknown; either way, health/readiness
// signals must never be trusted once feedback stops arriving, even though the
// version fields above stay sticky.
//
// ObservedConditions is private HC feedback and is intentionally not exposed on the
// public API; it is the only place the hc-controller writes raw HyperShift/CVO
// conditions, including their original LastTransitionTime, which callers may use to
// tell a fresh signal apart from one that has persisted for a while. See
// ControlPlaneUpgradeResult for the customer-facing upgrade conditions the
// control-plane-upgrade controller derives from this data.
//
// Returns true if HostedClusterResult changed.
func (r *Reconciler) applyHostedClusterResult(
	cluster *privatev1.Cluster,
	hasHCFeedback bool,
	hcFeedback map[string]string,
	gen int64,
) (bool, error) {
	previous := cluster.Status.HostedClusterResult

	var apiEndpoint, version, desiredVersion string
	var availableUpdates []string
	var observed []metav1.Condition

	switch {
	case hasHCFeedback:
		apiEndpoint = hcFeedback["controlPlaneEndpoint"]
		version = hcFeedback["version"]
		desiredVersion = hcFeedback["desiredVersion"]
		if v, ok := hcFeedback["availableVersions"]; ok {
			if err := json.Unmarshal([]byte(v), &availableUpdates); err != nil {
				return false, fmt.Errorf("decode available updates: %w", err)
			}
		}
		var versionConditions []metav1.Condition
		if v, ok := hcFeedback["versionConditions"]; ok {
			if err := json.Unmarshal([]byte(v), &versionConditions); err != nil {
				return false, fmt.Errorf("decode version conditions: %w", err)
			}
		}
		observed = make([]metav1.Condition, 0, len(versionConditions))
		for _, condition := range versionConditions {
			if condition.Type == "Degraded" {
				condition.Type = "HostedClusterDegraded"
			}
			condition.ObservedGeneration = gen
			observed = append(observed, condition)
		}
	case previous != nil:
		// No fresh feedback this reconcile — identity fields stay sticky, but
		// ObservedConditions is deliberately left empty rather than carried
		// forward: we cannot currently confirm any of it.
		apiEndpoint = previous.APIEndpoint
		version = previous.Version
		desiredVersion = previous.DesiredVersion
		availableUpdates = previous.AvailableUpdates
	}
	// Empty slices are omitted from the stored status and read back as nil. Keep
	// the desired value canonical so identical empty feedback does not trigger a
	// status write on every reconciliation.
	if len(availableUpdates) == 0 {
		availableUpdates = nil
	}
	if len(observed) == 0 {
		observed = nil
	}

	desired := &privatev1.HostedClusterResult{
		APIEndpoint:        apiEndpoint,
		Version:            version,
		DesiredVersion:     desiredVersion,
		AvailableUpdates:   availableUpdates,
		ObservedConditions: observed,
	}
	if equality.Semantic.DeepEqual(previous, desired) {
		return false, nil
	}
	cluster.Status.HostedClusterResult = desired
	return true, nil
}

// firstCondition returns the status, reason, and message of the first condition matching condType.
// Defaults: status="False", reason="Unknown", message="".
func firstCondition(conds []metav1.Condition, condType string) (status, reason, message string) {
	for _, c := range conds {
		if c.Type == condType {
			return string(c.Status), c.Reason, c.Message
		}
	}
	return "False", "Unknown", ""
}

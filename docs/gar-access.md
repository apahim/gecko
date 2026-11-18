# Customer GAR Reader access

This development prototype uses a background `gar-access` controller for Google
Artifact Registry (GAR) access. Creating a Gecko `Cluster` activates a shared
repository Reader grant for all current and future service accounts owned by that
customer GCP project. Cross-project service accounts used by a cluster are not
covered. The controller does not create service accounts, install credential
helpers, or configure image mirrors.

GAR reconciliation does not gate Cluster or NodePool provisioning. Existing
workloads are not paused or recreated because GAR access is pending or failing.
Users provisioning workers before the grant takes effect can encounter downstream
image-pull or bootstrap failures.

Config Connector (KCC) reconciles Kubernetes declarations into Google Cloud
resources. Here it applies the repository IAM grant declared by an
`IAMPolicyMember` in the regional Kubernetes API.

## Cluster identity

`controllers/garaccess/identity.go` derives the IAM member from accepted Gecko
Cluster setup data, with no hardcoded project mapping:

* `spec.platform.gcp.projectID` must match the Cluster's customer-project namespace.
* `spec.platform.gcp.workloadIdentity.projectNumber` must be a positive decimal
  without leading zeros.
* All extant Clusters in a customer project must agree on that number.

The resulting IAM member is:

```text
principalSet://cloudresourcemanager.googleapis.com/projects/PROJECT_NUMBER/type/ServiceAccount
```

This flow assumes WIF and worker resources belong to the same customer project,
as in `gcphcpctl cluster create --setup-infra`. A WIF pool hosted in a different
project requires a separate worker-project identity contract before using this
flow. These checks validate input consistency, not the project-ID/number
association through Google. No public API field or cloud lookup is added.

Missing or conflicting identity data blocks grant reconciliation and is reported
in controller errors. It is never treated as empty consumer inventory or used to
remove an existing grant. It does not block NodePool reconciliation.

## Shared configuration

The `gecko-gar-access` chart and controller command use these development defaults.
Override them for other environments:

```yaml
garAccess:
  repository: projects/gcp-hcp-commons-dev/locations/northamerica-northeast2/repositories/gcp-hcp-quay
  iamNamespace: gecko-system
```

The repository must be a full resource name. The IAM namespace must already
exist, be configured for KCC, and outlive individual Clusters. Both settings are
validated by the GAR controller. Both controller charts require an explicit
`image.tag` and currently use the `Recreate` Deployment strategy.

Only the GAR controller consumes `--gar-repository` and `--iam-namespace`.
The NodePool controller still accepts these legacy flags as deprecated, ignored
options so existing deployment arguments do not prevent startup during rollout.
The updated NodePool chart no longer passes GAR arguments or grants KCC access.

## Required bootstrap cloud IAM

These permissions are prerequisites, not granted by this controller:

* On the configured GAR repository, delegate
  `artifactregistry.repositories.getIamPolicy` and
  `artifactregistry.repositories.setIamPolicy` through a scoped role to the
  regional KCC Google service account. This delegation belongs in
  infrastructure Terraform, not in Gecko.

Neither the GAR-access controller nor the NodePool controller requires
`compute.projects.get` or `iam.serviceAccounts.get` on customer projects.

The charts provide Kubernetes RBAC separately: the GAR controller can manage
IAMPolicyMembers only in the configured namespace and get/list/watch Clusters.
It does not write Cluster status. The NodePool controller has no KCC permissions.
No static Google credential is required.

Infrastructure-to-Argo repository wiring and automated new/existing customer
bootstrap in `gcp-hcp-ctl` remain companion work; neither is wired by this slice.
The development defaults do not grant cloud IAM access. Missing bootstrap
permissions prevent the Reader grant from becoming ready; they do not prevent
NodePool dispatch.

## Reconciliation

* Gecko `Cluster` creation drives access, not a downstream HyperShift
  `HostedCluster` watch. The logical namespace is the customer GCP project, and
  every Cluster's `GCP.ProjectID` must match that namespace.
* Every extant Cluster activates the grant, including Clusters with no NodePools
  and deleting Clusters still present in inventory. Multiple Clusters in one
  customer project share one grant per repository; different projects require
  separate grants derived from each project's own Cluster identity.
* Only Cluster and KCC grant events trigger GAR reconciliation. GAR does not watch
  or count NodePools. Inventory or identity-resolution failures are errors,
  not evidence that Clusters are absent.
* A deterministic `IAMPolicyMember` grants repository-level
  `roles/artifactregistry.reader` to the project's ServiceAccount principalSet.
  It has no consumer owner references. Reconciliation is queued by project, and
  the existing KCC grant watch remains in place.
* The `gcp.managed.openshift.io/gar-consumers` annotation lists Cluster names,
  not Cluster/NodePool pairs.
* KCC is the single source of grant reconciliation status. A declaration's
  existence is desired state, not proof of success: inspect its `Ready=True`
  condition and check that `status.observedGeneration` matches
  `metadata.generation`. Even KCC readiness is not proof of an authenticated image
  pull or completed IAM propagation.
* `HostedClusterAvailable` and CLI "Ready" remain independent of GAR access.
  No GAR condition is mirrored onto Clusters and no readiness gate is added.
  Inspect IAMPolicyMembers through the regional Kubernetes API; this status is
  not exposed through Gecko's public Cluster API.
* NodePools independently record their management-cluster cleanup target before dispatch and
  use the recorded target for worker cleanup.
  Pools with neither a recorded target nor a recoverable placement retain their
  finalizer rather than silently skipping cleanup.
  An empty annotation is never treated as proof that no dispatch occurred.
* Deleting the last NodePool does not revoke access while any Cluster remains.
  Cluster deletion retains its finalizer until child NodePool and management-cluster
  cleanup finish. GAR uses the Gecko Cluster's presence, not readiness, deletion
  timestamp, or downstream HyperShift HostedCluster presence, as its lifecycle signal.
* Once the last Gecko Cluster is fully removed, the controller marks its grant
  draining, rechecks live Cluster inventory, and deletes only a matching
  Gecko-managed declaration with UID/resource-version preconditions. Grant
  reconciliation retries while a declaration is deleting and recreates it when
  needed; provisioning is not blocked. Forcibly removing Cluster finalizers
  bypasses this cleanup contract; an orphan NodePool does not independently retain
  the grant.

Policy names hash the repository, member, and role. Changing from a default
service-account member to a principalSet creates a different IAMPolicyMember;
old default-SA grants are not automatically converted. No live rollout has been
performed.

## Progressive rollout

The Cluster watch also discovers existing Clusters when the GAR controller
starts. Their project grants reconcile in the background, without
requiring Cluster recreation. Reconciling a new principal-set declaration does
not remove existing default-SA grants while Clusters remain.

NodePool no longer checks GAR readiness or creates a KCC client. Existing
placement/version prerequisites and worker-cleanup bookkeeping are unchanged.
The previous cleanup annotation key is retained so already provisioned workers
can still be deleted safely. Legacy `NodePoolGARAccessReady` conditions are no
longer consulted and are removed during normal NodePool status reconciliation.

## Development validation

These are manual checks for a future authorized development rollout, not results
already verified. Use the normal Argo application configuration with both charts
on the same controller image and configure GAR settings only on `gecko-gar-access`;
do not independently Helm-install over an Argo-managed NodePool deployment.

1. Start the GAR controller with existing Clusters, or create a Cluster without
   NodePools: expect one IAMPolicyMember per project with Cluster names in its
   consumers annotation. Inspect its KCC status in the regional Kubernetes API
   to confirm the current generation reconciles.
2. Add another Cluster in the same project, then pools under either Cluster:
   expect the same grant and both Cluster names in its annotation. Pending/error
   GAR readiness must not block worker dispatch or pause existing workers.
3. Create a Cluster in a second customer project with its own project number:
   expect a distinct grant. Verify missing/invalid numbers and conflicting
   numbers across same-project Clusters block reconciliation without deleting
   existing grants. Inspect controller errors for these cases; normal HostedCluster
   and NodePool reconciliation remain independent.
4. Remove the last pool while keeping a Cluster: verify worker cleanup and
   NodePool disappearance, but retain the shared grant.
5. Remove one of multiple same-project Clusters: retain the shared grant. Delete
   the last Cluster normally: retain access while its finalized deletion is in
   progress, then expect draining and removal of the matching declaration/IAM
   membership only after the Gecko Cluster object is gone. Other projects' grants
   must remain untouched. Do not bypass worker cleanup by removing finalizers.
6. Check allowed image pulls with project-owned service accounts, including a
   non-default account, and confirm unrelated IAM memberships and accounts are
   preserved. Cross-project service accounts need separate authorization.

No actual Google image pull has been verified by this change. Unit tests are
deferred. The pinned kube-applier implementation reports successful deletion only
after observing the target object absent, so a target retained by finalizers is
not yet successful. Manual validation must confirm the deployed image has that
behavior and HyperShift finalizers complete actual worker cleanup. Inventory and
policy deletion also span separate APIs: draining and deletion preconditions are
safeguards, not a cross-API transaction. Exercise concurrent arrivals and
deletion/recreation in development.

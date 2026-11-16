# Customer GAR Reader access

This development slice adds a `gar-access` controller and a mandatory NodePool
provisioning gate for Google Artifact Registry (GAR) access. It grants only the
customer's existing default Compute Engine service account access to the
configured repository. It does not create service accounts, install credential
helpers, or configure image mirrors.

Config Connector (KCC) reconciles Kubernetes declarations into Google Cloud
resources. Here it applies the repository IAM grant declared by an
`IAMPolicyMember` in the regional Kubernetes API.

## Shared configuration

Both charts and controller commands use these development defaults. Keep
`gecko-gar-access` and `gecko-nodepool` configured with the same coordinates,
overriding them together for other environments:

```yaml
garAccess:
  repository: projects/gcp-hcp-commons-dev/locations/northamerica-northeast2/repositories/gcp-hcp-quay
  iamNamespace: gecko-system
```

The repository must be a full resource name. The IAM namespace must already
exist, be configured for KCC, and outlive individual HostedClusters and NodePools.
Both settings are always validated. Both charts use the same controllers image,
including images from a development registry, and require an explicit `image.tag`.

GAR access is always required before NodePool worker dispatch. Both commands
accept `--gar-repository` and `--iam-namespace` to override the coordinates, and
both charts always pass them. Both controller deployments always use `Recreate`
to avoid overlapping old ungated replicas during the initial rollout. The
rollout strategy is fixed, so NodePool no longer exposes `strategy` values.

## Required bootstrap cloud IAM

These permissions are prerequisites, not granted by this controller:

* On each customer project, grant `compute.projects.get` and
  `iam.serviceAccounts.get` through a scoped role to the regional GAR-access and
  NodePool Kubernetes service-account Workload Identity principals. Use the
  service-account names from the rendered charts.
* On the configured GAR repository, delegate
  `artifactregistry.repositories.getIamPolicy` and
  `artifactregistry.repositories.setIamPolicy` through a scoped role to the
  regional KCC Google service account. This delegation belongs in
  infrastructure Terraform, not in Gecko.

The charts provide Kubernetes RBAC separately: the GAR controller can manage
IAMPolicyMembers only in the configured namespace; NodePool can only get them.
No static Google credential is required.

Infrastructure-to-Argo repository wiring and automated new/existing customer
bootstrap in `gcp-hcp-ctl` remain companion work; neither is wired by this slice.
The development defaults do not grant cloud IAM access. Provision these
permissions and the grant controller before expecting NodePool provisioning to
proceed: the mandatory gate blocks dispatch until the grant is ready.

## Reconciliation

* The NodePool's logical namespace is its customer GCP project.
* Live inventory counts every pool, including zero-replica and deleting pools.
  Multiple HostedClusters in one customer project share the grant.
* Compute and IAM lookups validate the default account; customer-supplied project
  numbers are not trusted. Lookup or inventory failures are errors, not absence.
* A deterministic `IAMPolicyMember` grants repository-level
  `roles/artifactregistry.reader`. It has no consumer owner references.
* The `gcp.managed.openshift.io/gar-consumers` annotation lists `clusterID/nodePoolID`
  consumers. NodePool's `NodePoolGARAccessReady` condition explains blocked
  provisioning and is retried after corrections.
* Dispatch requires the exact active, non-deleting grant, with current
  `status.observedGeneration` and `Ready=True`.
* NodePools record their management-cluster cleanup target before dispatch and
  use the recorded target for worker cleanup.
  Pools with neither a recorded target nor a recoverable placement retain their
  finalizer rather than silently skipping cleanup.
  An empty annotation is never treated as proof that no dispatch occurred.
* After the last pool disappears, the controller marks its grant draining,
  rechecks live inventory, and deletes only a matching Gecko-managed declaration
  with UID/resource-version preconditions. New consumers wait while a grant is
  draining or deleting; subsequent reconciliation recreates it if needed.

## Development validation

Use the normal Argo application configuration to deploy both charts with the
same image and GAR settings; do not independently Helm-install over an
Argo-managed NodePool deployment.

1. Create a pool in customer project A: expect one grant for A's default Compute
   account, then a current-generation Ready condition before worker dispatch.
2. Add another pool, including under a second HostedCluster in A: expect the
   same grant and both consumers in its annotation.
3. Add a pool in customer project B: expect a separate grant for B's account.
4. Remove one pool in A: its shared grant must remain.
5. Remove A's last pool: verify worker cleanup, NodePool disappearance, and then
   removal of A's declaration/IAM membership. B's grant must remain untouched.
6. Check an allowed image pull using the customer's actual default Compute
   identity, and confirm unrelated IAM memberships and the account are preserved.

Unit coverage is intentionally deferred for this first dev slice. The pinned
kube-applier implementation reports successful deletion only after observing the
target object absent, so a target retained by finalizers is not yet successful.
Dev validation must confirm the deployed image has that behavior and HyperShift
finalizers complete actual worker cleanup. Inventory and policy deletion also
span separate APIs: draining and deletion preconditions are safeguards, not a
cross-API transaction. Exercise concurrent arrivals and deletion/recreation in dev.

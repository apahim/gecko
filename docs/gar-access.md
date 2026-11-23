# Customer GAR Reader access

`gar-access` watches Gecko Clusters and maintains one repository-scoped
`roles/artifactregistry.reader` IAMPolicyMember per customer project. Config
Connector (KCC) applies the declaration to Google Cloud.

Grant reconciliation is asynchronous and does not change Cluster readiness or
pause running workers. The optional NodePool gate below waits for KCC-confirmed
access before initial downstream provisioning. With the gate disabled, early
provisioning can encounter downstream image-pull/bootstrap failures. No service
accounts, credential helpers, or image mirrors are created.

## Cluster identity

`controllers/garaccess/identity.go` uses accepted Cluster setup data:

- `spec.platform.gcp.projectID` must match the customer-project namespace.
- `spec.platform.gcp.workloadIdentity.projectNumber` must be a positive decimal
  without leading zeros and agree across all Clusters in that project.

```text
principalSet://cloudresourcemanager.googleapis.com/projects/PROJECT_NUMBER/type/ServiceAccount
```

This covers current/future project-owned service accounts, not accounts owned by
another project. It assumes WIF and workers share the customer project, as in
`gcphcpctl cluster create --setup-infra`; it does not verify that association with
Google. Invalid/conflicting identity data returns an error and never triggers
grant cleanup. An enabled NodePool gate waits for a valid matching grant; existing
NodePools continue reconciling. No hardcoded mapping or cloud lookup is used.

## Configuration and permissions

`garAccess.repository` is required and must be the full Terraform-selected GAR
repository name. Replace the placeholders below; there is no development fallback:

```yaml
garAccess:
  repository: projects/PROJECT/locations/LOCATION/repositories/gcp-hcp-quay
  iamNamespace: gecko-system
```

Regional Terraform publishes this name as the Argo root Helm value
`gar_quay_repository`. The regional Argo Application must pass it to the chart as
`garAccess.repository`; the chart supplies `--gar-repository` to the controller.
Gecko does not read the `argocd-cluster` Secret directly. Missing or malformed
repository values fail Helm rendering or controller startup.

`garAccess.iamNamespace` defaults to `gecko-system`. The namespace must already
exist, support KCC, and outlive Clusters. Customer bootstrap remains separate work.

Infrastructure Terraform must delegate these permissions on the selected
repository to the regional KCC Google service account:

- `artifactregistry.repositories.getIamPolicy`
- `artifactregistry.repositories.setIamPolicy`

No customer `compute.projects.get` or `iam.serviceAccounts.get` grant is required.
GAR has Cluster read/watch and namespace-scoped IAMPolicyMember RBAC, but no Cluster
status writes. When its gate is enabled, NodePool receives namespace-scoped
IAMPolicyMember `get` permission only; it never creates, updates, or deletes grants.

## Optional NodePool provisioning gate

The `gecko-nodepool` chart defaults to `garAccess.requireReadyGrant: false`.
Deployments without GAR, including current E2E targets, require no GAR flags or
KCC permissions. To enable the gate, pass the same repository and IAM namespace
used by `gecko-gar-access`:

```yaml
garAccess:
  requireReadyGrant: true
  repository: projects/PROJECT/locations/LOCATION/repositories/gcp-hcp-quay
  iamNamespace: gecko-system
```

These values supply `--require-gar-access`, `--gar-repository`, and
`--iam-namespace` to NodePool. The regional Argo NodePool Application must forward
them explicitly; deploying the GAR-access Application alone does not enable the
gate. KCC reads always use the regional Kubernetes API, even with `--orlop-url`.

After Cluster placement, a never-dispatched NodePool requires the exact matching
unconditional Reader declaration to be active, not deleting, and `Ready=True`,
with `status.observedGeneration` equal to `metadata.generation`. Missing or unready
declarations set the existing NodePool waiting conditions to `GARAccessPending`
and requeue after 15 seconds. Identity mismatches and read errors fail closed and
are retried. The Gecko NodePool API object is still accepted; only downstream
dispatch waits.

Deletion is handled before the gate. An observed installed release or any
persisted transport ApplyDesire bypasses it, even if dispatch acknowledgement or
downstream status is missing. Thus existing-pool recovery, scaling, upgrades, and
status reconciliation do not depend on KCC availability. If both installation
status and transport dispatch evidence are lost, the gate treats the pool as new.

KCC readiness is not a live IAM lookup or a cross-API transaction. External IAM
changes can leave `Ready=True` stale until KCC's next reconciliation, and IAM
propagation can outlast a successful reconciliation. This gate cannot guarantee
uninterrupted access or eliminate image-pull retries.

## Lifecycle

- Existing Clusters are discovered on startup; no recreation is needed.
- Same-project Clusters share a grant; different projects get separate grants.
- Every extant Cluster retains access, regardless of readiness or deletion state.
- Remove the declaration only after the last Gecko Cluster is fully gone.
  Normal Cluster finalization owns worker teardown; GAR does not watch NodePools.
- Cleanup marks the declaration draining, rechecks live Cluster inventory, and
  deletes only matching Gecko-managed objects with UID/resource-version guards.
  This is not a cross-API transaction; forced finalizer removal bypasses teardown.

Declarations use `gar-quay-reader-<hash>` names (hashing repository, member, and
role), project labels, a
`gcp.managed.openshift.io/gar-consumers` annotation listing Cluster names, and no
per-Cluster owner references. Existing default-SA grants are not automatically
converted or removed while Clusters remain.

## Status and development validation

KCC owns reconciliation status. Declaration existence is not proof of success:
inspect `Ready=True` and ensure `status.observedGeneration` matches
`metadata.generation`. Status is available through the regional Kubernetes API,
not Gecko's public Cluster status. An authenticated pull must verify effective access.

For an authorized dev rollout, check:

1. Existing/new Clusters are adopted; same-project Clusters share one grant and
   another project gets a separate grant.
2. With the NodePool gate disabled, provisioning remains unchanged. When enabled,
   missing, mismatched, pending, failed, deleting, draining, or stale-generation
   grants prevent first dispatch; valid KCC readiness releases it. These checks
   never remove grants, stop workers, or block deletion.
3. Access remains through deleting Clusters and is revoked only after the last
   Cluster is fully removed through normal worker teardown.
4. Project-owned default and non-default accounts can pull from GAR; unrelated IAM
    bindings/accounts remain untouched. Exercise concurrent arrivals and deletion/recreation.
5. Already-dispatched and installed NodePools continue reconciling after a restart
   or loss of GAR/KCC availability, including dispatches with missing status feedback.

An earlier dev build (`53fb6ad`) demonstrated declaration reconciliation and
automatic restoration of a manually removed Reader membership. That result does
not validate this optional gate or authenticated image pulls. Gate rollout and
image-pull validation remain pending. Unit tests are deferred.

# Customer GAR Reader access

`gar-access` watches Gecko Clusters and maintains one repository-scoped
`roles/artifactregistry.reader` IAMPolicyMember per customer project. Config
Connector (KCC) applies the declaration to Google Cloud.

Reconciliation is asynchronous: it does not gate provisioning, change Cluster
readiness, or pause running workers. Early provisioning can encounter downstream
image-pull/bootstrap failures. No service accounts, credential helpers, or image
mirrors are created.

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
Google. Invalid/conflicting identity data returns an error, never triggers grant
cleanup, and does not block NodePools. No hardcoded mapping or cloud lookup is used.

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
status writes. NodePool has no KCC access and consumes no GAR configuration.

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
2. Invalid identity and pending/failed KCC reconciliation do not block workers or
   remove existing grants.
3. Access remains through deleting Clusters and is revoked only after the last
   Cluster is fully removed through normal worker teardown.
4. Project-owned default and non-default accounts can pull from GAR; unrelated IAM
   bindings/accounts remain untouched. Exercise concurrent arrivals and deletion/recreation.

Live rollout/image-pull validation has not been performed. Unit tests are deferred.

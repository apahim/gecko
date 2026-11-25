# WIFConfigTemplate API contract and rollout

`gcp.managed.openshift.io/v1` exposes a platform-scoped, retained IAM catalog.
Public routes offer get/list only, but catalog reads are **not** exempt from
authorization. Until explicit platform-scoped Cedar grants and frontend routing
are delivered, authenticated callers without a grant are denied. Writes use the
delegated private API and platform RBAC. This is not a separate Kubernetes CRD
or a cloud IAM controller.

## Contract and identity

- The resource name is a canonical numeric `X.Y.Z` release boundary.
- `spec.schemaVersion: 1` identifies the executable protocol independently of the
  Kubernetes API envelope. Its accepted semantics must remain compatible; do not
  tighten version 1 validation against previously accepted stored contracts.
- `spec.iam` contains typed naming, seven component identities, custom-role
  permissions, exact guest WIF subjects and exact-worker-SA actAs grants. The
  ingress component is contract-required but optional at the Cluster level:
  `serviceAccountsRef.ingressEmail` is validated as installation-owned only
  when set, matching the opt-in Ingress capability.
- `spec.verified` is the only eligibility assertion. Omission means false. It is
  mutable independently of the executable contract. Setting false prevents future
  use by consumers once their availability/dispatch enforcement is deployed; it
  does not revoke IAM or cancel a rollout that has already started.
- IAM content and schema version are immutable. Catalog deletion is disabled in
  this initial API, avoiding reference/deletion races and retaining historical
  contracts. Deliberate retirement is a future policy.
- A reference identifies template name, UID and executable-content digest. Version
  references are set once; ordinary Version disappearance/aging remains allowed.

### Digest protocol

The digest is `sha256:` followed by 64 lowercase hexadecimal characters. Its input
is UTF-8 Go `encoding/json` output of a `WIFConfigTemplateSpec` with `verified`
omitted, using field order `schemaVersion`, then `iam`, and the declared typed IAM
field order. There are no insignificant whitespace characters or trailing newline.
This is **not RFC 8785**. Resource metadata, release-boundary name and verification
are not part of the executable contract.

Before encoding, sort `requiredComponents`; sort service accounts and roles by
name; sort subjects, permissions, WIF account refs and actAs member refs byte-wise;
sort role bindings by `serviceAccountRef + "/" + roleRef`. The current protocol
supports one worker-SA grant, so it requires no outer-list sort. Computation clones
input and does not mutate a caller's lists. The baseline golden digest is pinned
in API and Helm integration tests. A new spec field requires an explicit protocol
and immutability decision, not incidental inclusion/exclusion by a refactor.

## Cluster assertions and write activation

`spec.platform.gcp.iam` lives beside, not inside, immutable `workloadIdentity`.
It records the exact immutable default Compute worker-SA email and 1–128
append-only template applications keyed by template name. Applications must match
the template's current UID/digest and verified assertion when first appended.
Identical retained entries do not require current eligibility or new catalog reads
during unrelated updates. These are authenticated customer assertions, not proof
that the cloud configuration was actually applied or remains unchanged.

New application writes are **disabled by default** using the server rollout gate
`--enable-iam-application-writes=false` (Helm `iamApplications.enabled: false`).
This is a server rollout control, not another template readiness field. It rejects
initial assertions and appends while preserving valid existing history.

Safe activation order:

1. Deploy schema/API support with application writes disabled. Confirm that **all**
   API replicas are compatible; do not write IAM history while old replicas remain.
2. Deploy matching typed controllers and non-bypassable exact-target dispatch
   prerequisites. Older typed Cluster/Version updates drop unknown IAM fields or
   references and will be rejected once those records/pins exist.
3. Only then enable new application writes. Initial applicability and dispatch
   enforcement must not permit omission of this optional stored field as an opt-out.

After the first IAM assertion or Version pin, rolling back to an older API or
controller that does not understand these fields is unsupported. Retain compatible
artifacts or use a reviewed forward fix. Never erase history or switch persistent
storage to memory to make an old binary work.

## GitOps ownership and packaging

Helm is the authority for verification. Change the owner/region GitOps values and
let Argo reconcile; a direct private-API patch is temporary and may be overwritten
by self-heal. Do not use a direct patch for a durable revocation while GitOps still
declares true. Rendered manifests retain `Prune=false,Delete=false` and tolerate
initial aggregated-API registration with `SkipDryRunOnMissingResource=true`.

The catalog is opt-in (`wifConfigTemplates.enabled`, `entries`). Initially every
entry uses the reviewed project-wide baseline from the chart's `files/` directory.
Do not edit this immutable file after publication to update an existing boundary.
A genuinely different future contract needs its own retained contract data and a
new version-boundary manifest; the current single-baseline packaging must be
extended explicitly for that use case. Admission rejecting an immutable change is
the intended fail-safe, not a reason to delete/recreate the template.

The chart defaults publish no templates and disable new Cluster assertions.
Run the local Helm-to-API contract check with:

```sh
HELM_BIN=/path/to/helm go test ./cmd/platform-api-server -run TestWIFConfigTemplateHelmRendering
```

Phase 1 introduces the API and opt-in packaging only. Versionsync ownership of
`Available`, prerelease numeric-base floor selection, preservation of pinned
references during catalog synchronization, minimal status RBAC and server dispatch
enforcement must ship before activation in Phase 2. A template's verified assertion
alone does not authorize a Cluster or independently establish runtime permissions.

## Read-only IAM requirements API

Cluster and NodePool `status.iamRequirements` are optional public observations,
not dispatch receipts, cloud IAM attestation or client-writable fields. A missing
view or `state=Incomplete` is never proof that no permissions are required. The
view carries the exact release/image/group/template reference of dispatched and
pending work, resource and parent UIDs, observed generation and reconciliation
refresh time. Refresh time is **not** the age of cloud verification or transport
feedback. Consumers must require a complete, fresh view with matching UIDs and
generation for every related NodePool and re-read before writing. No controller
publishes this view in this foundation stage; until publishers and durable
dispatch receipts are deployed, consumers must treat it as incomplete.

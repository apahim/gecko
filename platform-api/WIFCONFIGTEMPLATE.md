# WIFConfigTemplate API contract and rollout

`gcp.managed.openshift.io/v1` exposes a platform-scoped, retained IAM catalog.
Authenticated customers may get/list templates; writes use the delegated private
API and platform RBAC. This is not a separate Kubernetes CRD or a cloud IAM controller.

## Contract and identity

- The resource name is a canonical numeric `X.Y.Z` release boundary.
- `spec.schemaVersion: 1` identifies the executable protocol independently of the
  Kubernetes API envelope. Its accepted semantics must remain compatible; do not
  tighten version 1 validation against previously accepted stored contracts.
- `spec.iam` contains typed naming, six component identities, custom-role
  permissions, exact guest WIF subjects and exact-worker-SA actAs grants.
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

## Version availability and dispatch (Phase 2)

Versionsync selects the greatest numeric boundary at or below the release's X.Y.Z
core in the same major/minor. Prereleases inherit that core's contract without
changing their full release name or image. Selection includes unverified templates:
an intact matching template is pinned once, but remains unavailable until verified.
There is no fallback to an older verified boundary. A newly published higher
effective boundary blocks a stale association for platform review; it never repins.

Versionsync alone owns Version `Available`. True requires complete release metadata,
current generation and an intact verified reference. Its separate 30-second loop
refreshes eligibility even during upstream discovery failure. Failed template reads
preserve pins and prevent catalog aging; no-op refreshes preserve transition times.
Channel defaults use their exact pinned Version, never an implicit alternative.

Resolution, control-plane/NodePool approval and final new dispatch require current
availability. Final dispatch additionally uses fresh API reads to check the exact
template identity, digest, verification and Cluster application coverage. Missing
IAM is a block, not an exemption. Coverage compares name, UID and digest, never
version ordering. The HC dispatcher owns Cluster `IAMPermissionsReady`; the NodePool
dispatcher owns the corresponding pool condition. Upgrade coordinators report
future-target failures through their upgrade conditions, separately from readiness
of the currently running target. Blocked prerequisites retry without a spec edit.

### Durable submission boundary

Before the first transport Apply for a new target, a private status receipt is
persisted with an optimistic concurrency check. It records object/parent UID, full
release version, image, channel group and exact template reference. The existing
upgrade submission-unknown condition, when applicable, is committed in the same
write. No Apply occurs on a failed/conflicting receipt write. Derived Cincinnati
channel and whole-object generation are not receipt identity.

Pending and last-dispatched receipts are separate. Non-stale observation of the
pending release promotes it; uncertain delivery retains it. A committed exact target
may be redelivered after restart, verification revocation or upstream aging, because
transport delivery can be partial. This is recovery of an authorized intent, not
authorization of a different release/image/contract or a legacy exemption. Pending
dispatch locks release changes but permits unrelated updates. Deletion and observation
of known work are not new-dispatch authorization.

A committed target that never becomes observable (for example, an invalid image)
remains locked: transport may already contain a partial delivery. This phase adds
no force-unlock or alternate-target escape. Operators must diagnose/recover the
same target or use ordinary deletion and confirmed cleanup before a fresh install.
Absence of version feedback alone is not proof that no external write occurred.

There is no atomic transaction spanning catalog verification and external transport.
Revocation is checked before committing a new exact-target intent, not used to recall
already-committed work. Controllers never apply or inspect customer cloud IAM. The
worker manifest receives only the Cluster's explicit immutable default Compute SA.

Phase 2 deliberately keeps application writes disabled. Positive coverage and
upgrade tests use local fixtures only. A template's verified assertion alone does
not authorize a Cluster or independently establish runtime permissions.

## Read-only IAM requirements (Phase 4 code-first)

Cluster and NodePool public `status.iamRequirements` are **observations**, not
dispatch receipts or cloud IAM attestation. They expose the exact release,
payload and immutable template reference of the last observed dispatch and of
any committed pending delivery. Their `state` is `Incomplete` when a required
identity/receipt cannot be established, the observed version differs from known
receipts, progress feedback is missing/Unknown/True for an observed release, or
the desired release is not represented by the running/pending work. A missing
`iamRequirements` field is also incomplete. Historic Cluster application
assertions are not themselves evidence that a workload is still running.

`observedGeneration` binds the view to the object's generation. `objectUID` and
`clusterUID` bind it to exact resource instances. `lastRefreshTime` records the
controller's last reconciliation of this view; it is **not** the age of cloud IAM
verification or a guarantee of fresh transport feedback. On a healthy stable
reconcile the view is refreshed at most once every five minutes. A future IAM
update client must require `state=Complete`, matching generation/UIDs, a bounded
age (and no future timestamp), and a complete, freshly re-read Cluster/NodePool
inventory. If feedback is stale or missing, or any of these checks fail, it must
refuse to narrow running permissions. A complete view is not authorization to
start an upgrade or to append an IAM application record.

This field is additive, optional and controller-owned. API/schema support must
be deployed before compatible controllers write it. Old controllers may drop it
during a mixed-version rollout; missing means incomplete. No IAM update executor,
new template, gate activation or live upgrade is included in this code-first
slice.

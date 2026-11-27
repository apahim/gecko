# WIFConfigTemplate API contract and rollout

`gcp.managed.openshift.io/v1` exposes a platform-scoped, retained IAM catalog.
Public routes offer get/list only; those reads are authorization-exempt for
authenticated callers, matching Version, Channel and PlatformRole (GCP-1266
precedent). Authentication remains mandatory. Explicit platform-scoped Cedar
grants (GCP-1406) will replace the exemption and must remove it in the same
rollout. Writes use the delegated private API and platform RBAC. This is not a
separate Kubernetes CRD or a cloud IAM controller.

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
  `spec.wifConfigTemplateReference` is set once; ordinary Version disappearance/aging
  remains allowed. Requirements and private dispatch receipts use `templateReference`.

### Digest protocol

The digest is `sha256:` followed by 64 lowercase hexadecimal characters. Its input
is UTF-8 Go `encoding/json` output of a `WIFConfigTemplateSpec` with `verified`
omitted, enveloped as `schemaVersion`, then `iam`, then re-encoded through generic
JSON so that every object key sorts alphabetically. The digest therefore depends
only on contract content, never on Go struct declaration order at any nesting
level. There are no insignificant whitespace characters or trailing newline.
This is **not RFC 8785**. Resource metadata, release-boundary name and verification
are not part of the executable contract.

Before encoding, sort `requiredComponents`; sort service accounts and roles by
name; sort subjects, permissions, WIF account references and actAs member references
byte-wise; sort role bindings by `serviceAccountReference + "/" + roleReference`.
The current protocol supports one worker-SA grant, so it requires no outer-list
sort. Computation clones input and does not mutate a caller's lists. The baseline
golden digest is pinned in API and Helm integration tests. A new spec field requires
an explicit protocol and immutability decision, not incidental inclusion/exclusion
by a refactor.

The reviewed baseline digest is
`sha256:63975f7b264368634bc10a4bff7474142492e833257578a42c912d57336f2971`.
Before first publication, the new reference fields were expanded from `*Ref`/`*Refs`
to `*Reference`/`*References`. This changes the executable JSON keys and digest,
not the baseline permissions, subjects or grant topology. It is not a migration
mechanism for any published contract.

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

The catalog is opt-in (`wifConfigTemplates.enabled`, `entries`). Each entry
selects its own immutable contract manifest through `contractFile`, defaulting
to the reviewed project-wide baseline from the chart's `files/` directory. Do
not edit a published file to update an existing boundary. A genuinely different
future contract needs its own retained contract data and a new version-boundary
manifest selected by a new entry. Admission rejecting an immutable change is
the intended fail-safe, not a reason to delete/recreate the template.

The Spanner DDL migration Job image is pinned independently from the API image
(`migrationImage`) so API image bumps cannot recreate the `Force=true,Replace=true`
Job and re-run migrations. Release checklist: any change shipping new Spanner
DDL must bump `migrationImage.tag` in the same release — the migration Job is
the only runner of DDL, and a missed bump means the migrations silently never
run. The golden rendering test pins the current tag but cannot catch a missed
future bump.

The chart defaults publish no templates and disable new Cluster assertions.
Run the local Helm-to-API contract check with:

```sh
HELM_BIN=/path/to/helm go test ./cmd/platform-api-server -run TestWIFConfigTemplateHelmRendering
```

Phase 1 introduced the API and opt-in packaging. Phase 1.5 delivers versionsync
ownership of `Available`, prerelease numeric-base floor selection, preservation
of pinned references during catalog synchronization and minimal status RBAC,
with an independent fast eligibility loop (production-safe default cadence;
POC deployments configure a shorter interval deliberately). Because the catalog is append-only and contracts
immutable, a boundary retained from an earlier contract revision can
permanently fail current validation; such a boundary is unpinnable and its
selection blocks the release fail-closed (`TemplateReferenceInvalid`) instead
of falling back to a lower boundary, without attempting writes admission
rejects. Server dispatch enforcement must still ship
before activation in Phase 2. A template's verified assertion alone does not
authorize a Cluster or independently establish runtime permissions.

## Read-only IAM requirements API

Cluster and NodePool `status.iamRequirements` are optional public observations,
not dispatch receipts, cloud IAM attestation or client-writable fields. A missing
view or `state=Incomplete` is never proof that no permissions are required. The
view carries the exact release/image/group/template reference of dispatched and
pending work, resource and parent UIDs, observed generation and reconciliation
refresh time. Refresh time is **not** the age of cloud verification or transport
feedback. Consumers must require a complete, fresh view with matching UIDs and
generation for every related NodePool and re-read before writing. Since Phase 2
the hc and nodepool controllers publish this view in the same status write as
their dispatch receipts; objects without receipts keep no view at all, which
consumers must read as Incomplete.

## Dispatch protocol (Phase 2)

One shared dispatch predicate gates every boundary between approval and external
Apply. It is evaluated fresh at each boundary — a cached version-resolution
success can never bypass the final check — by the coordinators (control-plane
upgrade approval for customer, automatic and critical-override targets, and
NodePool upgrade scheduling) and independently again by the installer Apply
boundaries (hc and nodepool controllers).

The predicate reads the target Version from the catalog and the Cluster's
append-only application assertions, and produces exactly one of:

- **Permitted** — the release is available at its current generation with a
  verified pinned template, and the Cluster asserts that exact reference.
- `WIFConfigUpdateRequired` — the target exists and is advertised, but the
  Cluster has not asserted the exact pinned contract. Customer action (an IAM
  update and application append) releases it. This reason is never surfaced as
  `NoUpgradeAvailable`, and no alternative target is silently selected.
- `VersionIAMUnavailable` — the pinned contract is missing, unverified, stale or
  not published in the Cluster's channel group. Platform action releases it.

An infrastructure read failure is a retryable error that must produce zero
external Apply. A missing Version or channel mismatch is a fail-closed
evaluation, not an error.

### Receipts

The dispatch intent (release version, release image actually dispatched,
channel group, pinned template reference) is durably committed as a
UID-and-parent-bound `status.iamDispatch` receipt:

- The `Pending` receipt is persisted in the same status write as the dispatch
  marker, **before** any external Apply. Failed persistence means zero Apply.
- A successful non-stale delivery promotes the receipt to `Dispatched` while
  preserving the committed `Pending` intent, which remains the authority for
  same-target redelivery across restarts and revocations.
- A committed receipt (Pending or Dispatched matching the exact intent)
  bypasses re-gating for its own target only; it never authorizes a different
  target, and there is no force-unlock or clear-status escape hatch.

### Gating scope

Only new work is gated: an initial installation or an upgrade. Continued
reconciliation of the observed release is never gated, so a verification
revocation never recalls a committed rollout. When enforcement later activates,
already-committed targets are grandfathered and only new targets are gated.

The dispatcher-owned conditions `IAMPermissionsReady` (Cluster) and
`NodePoolIAMPermissionsReady` (NodePool) report the gate result. They are only
written while enforcement is enabled, in the same pre-Apply status write.

### Staged activation

Enforcement is off by default (`--iam-dispatch-enforcement=false` on the hc,
nodepool, control-plane-upgrade and nodepool-upgrade controllers, exposed as
the `iamDispatchEnforcement` chart value). Receipts and the requirements views
are maintained even while enforcement is off, so the staged rollout can
validate them before activation. The versionresolution and nodepool-vr
controllers are intentionally not gated: resolution never blocks on IAM by
design, and the final Apply boundary is the non-bypassable check.

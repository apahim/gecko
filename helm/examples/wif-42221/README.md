# Inactive 4.22.21 → 5.0.0 dev preparation

These files are opt-in examples, not referenced by any Argo Application. No
shared chart default changes. Do not apply/install them as standalone values.

- Merge `version-sync-values.yaml` over the existing personal-dev version-sync
  values: only candidate's discovery floor changes. Retain other Channels,
  install defaults, fleet authorization, source and deployed image settings.
- Merge `platform-api-values.yaml` over reviewed live API values. The common
  immutable schemaVersion/IAM content renders at both raw boundaries. Both
  templates remain unverified and assertion writes remain disabled.
- Preserve the live Spanner database, original migration Job/image pin,
  `ApplyOutOfSyncOnly=true`, `prune=false`, ESP and rolling-update safeguards.
  Render and independently prove the completed Job unchanged before any rollout.

Publication/pinning, verification, assertion writes, IAM, the new cluster's cost/
footprint, and actual upgrade execution need separate approval. Neither this
example nor a local build authorizes those operations. Protect the retained
cluster before enabling the shared 5.0.0 template: its existing assertion can
already cover other releases. Customer-requested upgrades bypass maintenance
windows; submitting a missing target assertion may release work immediately.

Comparison-only 5.x → 4.(23+x) normalization follows pinned HyperShift semantics.
Mapping reference: [running HyperShift source, supportedversion/version.go](https://github.com/openshift/hypershift/blob/d3173a012ac0a2b188095aaa6722438b33ff7602/support/supportedversion/version.go#L390-L408).
Never normalize stored versions, payloads, raw WIF floor selection or references.
Emergency critical-pin maintenance bypass still requires the same **raw** stream.

No new CPO inspection/credential prerequisite is introduced by this preparation.
Use normal configured image pulls and HyperShift validation in a later approved
live test; diagnose a specific capability error if one actually occurs.

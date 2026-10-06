# Version catalog synchronization

The controller selects one source for the environment:

- `--source-type=cincinnati` (default) probes production graph channels named
  `<Channel name>-<major.minor>`. `--source-url` selects the graph endpoint; when
  omitted, the existing `--cincinnati-url` and `--arch` settings still apply.
- `--source-type=release-controller --source-url=<base URL>` reads all accepted
  tags from each Channel's explicit `spec.releaseStreams`. Use an architecture
  specific endpoint, such as `https://amd64.ocp.releases.ci.openshift.org`, without
  `/graph`. The endpoint determines architecture; `--arch` is only for Cincinnati.

Helm exposes `source.type` and `source.url`. CI Channel definitions must include
`releaseStreams`; there are no built-in stream or Channel names. Multiple streams
are unioned, and the same release can belong to several Channels. The existing
minimum supported version (4.22) and canonical Kubernetes resource name checks
apply. Tags in Pending, Ready, Rejected, or Failed phases are not published.

CI membership comes from `/api/v1/releasestream/<name>/tags?phase=Accepted`, which
returns all tags without pagination. The CI graph is unsuitable for this catalog:
its nodes span streams, and its channel filter only selects upgrade edges.

A fetch failure, malformed response, conflicting payload for a version, missing
stream configuration, or entirely empty supported catalog preserves the previous
Version snapshot. An empty individual stream is allowed when other configured
streams still supply supported releases. Stale Versions are deleted only after
all sources have been fetched and desired Versions created/updated. An API write
failure can leave partially applied updates; the next sync retries them.

After applying a snapshot, each Channel's `DefaultVersionAvailable` condition
reports whether its pinned `installDefaultVersion` belongs to that Channel.
Unavailable defaults do not prevent synchronization and are never repinned.
Fetch/apply failures report `Unknown`; status write failures are logged and retried
on the next sync. Status includes the observed Channel generation. Unchanged
conditions retain their transition time and do not cause repeated status writes.

This controller requires the Channel API and status subresource from Gecko #428.
Deploy those first, then the controller/chart, then opt environments into CI.
Cluster/NodePool APIs, admission validation, and release resolution are unchanged.
Environment Channel choices (including integration's stable/nightly) belong in
infrastructure overrides, not this chart's defaults.

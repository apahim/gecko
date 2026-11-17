# GECKO

<p align="center">
	<img src="docs/logos/gecko-logo.png" width=400px>
</p>

---

GCP Engine for Cluster Kubernetes Orchestration. Bringing the HyperShift project to Google Cloud Platform.

---

## Running pull request builds

Konflux publishes the images built for every pull request against `main` to a
development Artifact Registry repository, tagged with the PR's head commit SHA
(full and 7-character short):

```
northamerica-northeast2-docker.pkg.dev/gcp-hcp-commons-dev/gcp-hcp-pr-images/gecko-controllers:<sha>
northamerica-northeast2-docker.pkg.dev/gcp-hcp-commons-dev/gcp-hcp-pr-images/gecko-platform-api-server:<sha>
```

The optional `<component>-publish-pr-images` check on the PR turns green once
the images are pushed. Dev-all-in-one and platform-ci e2e environments can pull
them directly; members of `gcp-hcp-eng@redhat.com` can pull them with their own
gcloud credentials. Images are deleted after 14 days; push to the PR or re-run
the check to publish them again.

## Fast local images on macOS

Cross-compile Linux binaries with the host Go toolchain, then copy them into a
runtime-only image. The packaging Containerfile has no `RUN` instructions, so an
Apple Silicon Mac does not emulate an amd64 Go compiler. The existing containerized
source builds remain unchanged.

From the repository root, with Go and Podman installed:

```sh
# Both linux/amd64 development images (also the default target architecture).
make local-images LOCAL_IMAGE_TAG=my-test

# Build just the API image, or use Docker instead of Podman.
make local-image-api LOCAL_IMAGE_TAG=my-test
make local-image-controllers CONTAINER_ENGINE=docker LOCAL_IMAGE_TAG=my-test

# Build for arm64 instead, or use a different destination repository.
make local-images LOCAL_ARCH=arm64 LOCAL_IMAGE_REPOSITORY=quay.io/example/gecko LOCAL_IMAGE_TAG=my-test
```

These commands build locally only; they do not push images or deploy anything.
The default repository is `localhost/gecko`; tags have `-api` and `-controllers`
suffixes. Without `LOCAL_IMAGE_TAG`, the prefix is `local-<source-sha>-<arch>`.
Dirty working trees append `-dirty` to the default tag and revision label, including
when the tree contains untracked files. Image targets do not need Git to be present
in an exported source tree; that case uses `unknown` as the revision.
When supplying an explicit tag, use a different tag per architecture to avoid
overwriting an earlier single-platform image.

`make local-binaries` builds just the static Linux binaries under ignored
`bin/local/linux-<arch>/<component>/app/gecko`. Separate per-component build contexts
exclude source, credentials and generated API-test certificates. The runtime keeps
the normal images' UBI micro base, CA bundle, non-root UID and writable `/app`
directory, while the binary is read-only. Target architecture is explicit: a Mac
executable is never copied into the image. Builds disable ignored Go workspaces
and use readonly module files, preserving the repository's local module replaces.

For reproducible dev deployments, build from a clean source commit, choose a
unique tag, record the host `go version` and image digest, then push explicitly:

```sh
make local-images LOCAL_IMAGE_REPOSITORY=quay.io/apahim/gecko LOCAL_IMAGE_TAG=my-test
podman push quay.io/apahim/gecko:my-test-api
podman push quay.io/apahim/gecko:my-test-controllers
```

The host Go toolchain (not the containerized builder's Go version) compiles these
artifacts. Binary execution smoke tests may still need target-architecture
emulation when testing an amd64 image on an arm64 host; image assembly does not.

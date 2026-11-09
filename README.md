# Embedding

Embedding is AppThrust's Project-local EmbeddingGemma endpoint for Knowledge
search. The Go server supervises `ollama serve` and exposes an OpenAI-compatible
`POST /v1/embeddings` endpoint with 768-dimensional vectors. Its image includes
the `embeddinggemma` model under `/models`: no API key or runtime model download
is needed. The default upstream is loopback, so document content stays inside
the Project. The app is released independently of the platform.

Project publication supplies authenticated edge access; the server itself does
not perform OIDC authentication. Ollama management endpoints are not exposed.
The server also provides `/healthz`, `/readyz`, `/releasez`, application health,
and model metadata at `/`. See [the server contract](cmd/server/README.md) for
configuration, request limits, warm-up, process supervision, and shutdown.

## Develop

Use Go 1.26 or newer and Bun 1.4.1. The public Go SDK is
[`github.com/appthrust/appserver-go`](https://github.com/appthrust/appserver-go) v0.3.0.
There is no platform checkout or module replacement in the app's build.

```sh
go build ./...
go vet ./...
go test ./...
bun test hack/
```

Go tests use fake upstreams and need no live Ollama service. Publisher renderer
and mocked API tests need no packages or live services.

With Ollama already running on port 11435 and `embeddinggemma` installed:

```sh
EMBEDDING_OLLAMA_BIN="" \
  EMBEDDING_UPSTREAM=http://127.0.0.1:11435 \
  go run ./cmd/server --listen 127.0.0.1:8080
```

Readiness requires a successful 768-dimensional warm-up, not just a running
Ollama process. The default listen address is `:8080`.

```sh
# Compile only the Go image stage, without downloading Ollama or its model:
docker build --platform linux/amd64 --target server-builder .
# Full runtime image, including the pinned Ollama base and model download:
docker build -t embedding:local .
```

The image runs `/embedding-server` as UID 65532. The final stage runs Ollama to
bake the model, so a full image must build on the target architecture. A
builder-only check does not prove model download or the non-root runtime.

## Release

Next release: **`v0.3.0`**, upgrading the shared server SDK to v0.3.0.
The catalog publisher appends the built image; existing version digests stay immutable.

Once the bootstrap prerequisites below are complete, create and push a new
SemVer tag, starting with `v0.2.0` (the existing catalog floor owns `0.1.0`):

```sh
git tag v0.2.0
git push origin v0.2.0
```

`.github/workflows/publish.yml` builds Linux amd64 and arm64 on native Blacksmith
runners, not under QEMU. Each build pushes `ghcr.io/appthrust/embedding:0.2.0-<arch>`
and uploads its immutable digest. A separate job creates the multi-architecture
index `ghcr.io/appthrust/embedding:0.2.0`, resolves its digest, and signs that
digest with Sigstore keyless signing. The source repository is public; the image
must also be public. `GITHUB_TOKEN` needs `packages:write` and OIDC needs `id-token:write`.

Configure repository secret `CLOUD_CATALOG_ADMIN_TOKEN` and, if needed, variable
`CLOUD_API_URL` (default `https://api.appthrust.com`). Grant this repository access
to organization secrets `APPTHRUST_GITHUB_ACTIONS_APP_ID` and
`APPTHRUST_GITHUB_ACTIONS_APP_PRIVATE_KEY`. The GitHub App must be installed for
`appthrust/cloud` with Contents write permission to send repository dispatches.
Cloud's API needs no GitHub credential.

Publication has two phases:

```sh
bun hack/publish-entry.ts submit --version 0.2.0 --digest sha256:<image-index-digest>
# If submit reports state=snapshot, dispatch with an installation token for Cloud:
gh api -X POST repos/appthrust/cloud/dispatches -f event_type=catalog-publish -f 'client_payload[snapshot_id]=<snapshot-id>'
bun hack/publish-entry.ts await --version 0.2.0 --digest sha256:<image-index-digest>
```

`submit` reads the existing Cloud entry, merges its version history with
`catalog/entry.yaml`, and PUTs the complete `CatalogEntry` to
`/api/v1/catalog/entries/embedding`. It then POSTs `/api/v1/catalog/publish`,
accepting either HTTP 202 `{id, state: "queued"}` (Cloud dispatched publication)
or HTTP 200 `{id, state: "snapshot"}` (the immutable snapshot needs dispatch).
It writes `snapshot_id` and `state` to `$GITHUB_OUTPUT` in Actions and exits
without polling. For `snapshot` only, the workflow mints a short-lived GitHub App
installation token scoped to `appthrust/cloud` and dispatches
the `catalog-publish` repository event with that snapshot id. For `queued`, it skips dispatch.
Cloud's workflow requires its own `CLOUD_CATALOG_API_URL` repository variable and
`CLOUD_CATALOG_ADMIN_TOKEN` secret to fetch the snapshot.

`await` requires no Cloud admin token. Dispatch is not success: every 15 seconds
for at most 10 minutes it fetches layer 0 (`index.json`) of
`ghcr.io/appthrust/catalog:latest` through ORAS, checks its digest, and requires
the exact Embedding version and image digest. Before succeeding it verifies
that immutable catalog's signature against issuer
`https://token.actions.githubusercontent.com` and identity
`https://github.com/appthrust/cloud/.github/workflows/catalog-publish.yml@refs/heads/main`.
Timeout, API failure, dispatch failure, or an unverified artifact fails the release.
ORAS 1.3.0 and Cosign 3.1.3 are installed by the workflow; manual publication
needs them too.

## Catalog and image contract

- `catalog/entry.yaml` is the complete ApplicationStackDefinition. Its versions
  section is the publisher's append point, not an image tag substitution.
- The seed `0.1.0` retains its original
  `ghcr.io/appthrust/platform/embedding@sha256:...` image. All subsequent versions
  use `ghcr.io/appthrust/embedding@sha256:<index-digest>`, `stream: <major.minor>`,
  `served: true`, and `default: true`. Earlier versions stay served with their
  defaults cleared. The definition and top-level catalog versions agree.
- A version's images are immutable. Repeating the same version/digest is
  idempotent; attempting to replace its digest fails. The release workflow
  serializes Embedding writers to protect its read/merge/PUT sequence.
- Changing from the seed's repository requires an explicit installation version
  update with `replaceRepository: true`. Afterwards the `follow` policy follows
  its stream; `pinned` requires an explicit version update.
- `/releasez` reports catalog-rendered `EMBEDDING_RELEASE_VERSION` and
  `EMBEDDING_ARTIFACT_DIGEST`; no platform release closure is involved.

## Bootstrap prerequisites

The repository secret `CLOUD_CATALOG_ADMIN_TOKEN`, variable `CLOUD_API_URL`, and
access to the organization GitHub App secrets are provisioned for this repo.
Cloud must have its `CLOUD_CATALOG_API_URL` variable and
`CLOUD_CATALOG_ADMIN_TOKEN` secret. Make the GHCR package
`ghcr.io/appthrust/embedding` public after the first push; visibility is set per
package, independently of source-repository visibility.

The Go SDK prerequisite is complete: `go.mod` uses the public `v0.1.0` release,
without a local replacement or pseudo-version. There is no web build or npm
registry dependency.

Architecture and migration authority:
[`first-party app repositories`](https://github.com/appthrust/platform/blob/main/design/platform/marketplace-apps/first-party-app-repositories.md),
section **App repository**. Original server: platform `cmd/embedding-server/`;
catalog template: Cloud's Embedding seed.

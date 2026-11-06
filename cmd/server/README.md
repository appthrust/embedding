# Embedding built-in app

`embedding-server` is the Project-local EmbeddingGemma endpoint used by Knowledge.
The image runs one Go entrypoint as PID 1, supervises `ollama serve`, and includes
the `embeddinggemma` model under `/models`. It requires neither an API key nor a
runtime model download. The default upstream is loopback: document content stays
inside the Project. Project publication supplies authenticated edge access; the
server itself does not perform OIDC authentication.

## HTTP contract

Only these routes are exposed on port 8080:

| Route | Behavior |
| --- | --- |
| `POST /v1/embeddings` | OpenAI-compatible input/output, forwarded only to the upstream's `/v1/embeddings`. Missing/empty `model` is filled with `EMBEDDING_MODEL`; a different model is rejected with 400 `invalid_request`. Other JSON fields are preserved. |
| `GET /healthz` | 200 while the Go HTTP process is alive, even during an Ollama outage. |
| `GET /readyz` | 503 until Ollama answers `/api/tags` and a warm-up embedding returns one 768-dimensional vector; 200 after that. |
| `GET /releasez` | Release identity from complete environment values, otherwise the configured release file; 503 when unavailable. |
| `GET /` | `{"model":"embeddinggemma","dimensions":768}` (model follows configuration). |

All other paths return 404. In particular, `/api/tags`, `/api/pull`, `/api/delete`
and other Ollama management endpoints are **not** reachable through the server.
The child binds `127.0.0.1:11434`, regardless of an inherited `OLLAMA_HOST`.
Upstream redirects are not followed. Client authorization headers and URL query
parameters are not forwarded to Ollama. GET routes also support HEAD through the
standard Go mux.

The embedding endpoint accepts one JSON object of at most **8 MiB** (413
`request_too_large` above that), admits at most **four in-flight requests** (503
`embedding_busy` instead of queueing), and has a **120-second** request/upstream
timeout (504 `embedding_timeout`). Before readiness it returns 503
`embedding_unavailable`; connection failures return 502
`embedding_upstream_unavailable`. Local errors use the shared built-in app envelope
`{"error":"<code>","message":"..."}`. Upstream statuses and response bodies are
passed through unchanged.

## Configuration

The only flag is `--listen` (default `:8080`). Unexpected positional arguments
are rejected.

| Environment variable | Default / meaning |
| --- | --- |
| `EMBEDDING_MODEL` | `embeddinggemma`; must already be installed upstream and return 768 dimensions. Changing this does not pull another model. |
| `EMBEDDING_UPSTREAM` | `http://127.0.0.1:11434`; HTTP(S) origin, with no credentials, path (except a trailing slash), query or fragment. |
| `EMBEDDING_OLLAMA_BIN` | `ollama` if **unset**. An explicitly empty string disables spawning and uses the external upstream. Otherwise invoked with the single argument `serve`. |
| `PLATFORM_PROJECT_ID` | Project identity supplied by the platform ComponentConnection; included in the startup log. Optional when running locally. The other `PLATFORM_*` values are unused: this internal inference service neither calls the Platform API nor performs identity verification. |
| `OLLAMA_MODELS` | `/models` in the image; inherited by the child. |
| `OLLAMA_NO_CLOUD` | Forced to `1` for the supervised Ollama child, overriding inherited values and `ExtraEnv`; disables cloud features, including background model-recommendation fetches. |
| `EMBEDDING_RELEASE_VERSION` | Release version. |
| `EMBEDDING_SOURCE_REVISION` | Source revision. |
| `EMBEDDING_ARTIFACT_DIGEST` | Image artifact digest. |
| `EMBEDDING_RELEASE_FILE` | Optional mounted JSON release identity file, used when the three identity variables are not all populated. |

Like Knowledge, `/releasez` returns the artifact identity: `releaseVersion`,
`sourceRevision`, and `artifactDigest`. A complete environment identity wins
over a file. The file is read verbatim on each request, supporting mounted-file
updates; responses disable caching.

Delivery identity (closure digest) is not injected into built-in apps.

The supervisor checks upstream health every second with a two-second probe
timeout. It warms once per healthy lifecycle, re-warms after an upstream outage,
and clears readiness while the child is down. Unexpected exits or failed starts
are retried with exponential backoff from one to 30 seconds, resetting after a
minute of child uptime. Warm-up failure never exits the Go process or blocks
probe handlers. Model warm-up uses a short fixed text, not customer content.

Process ownership now lives in
`github.com/appthrust/appserver-go/appserver/ollamasupervisor`, shared with LLM; Embedding supplies
the unchanged 768-dimensional warm-up callback and its 120-second client.
The child overrides `OLLAMA_KEEP_ALIVE=-1`, `OLLAMA_NO_CLOUD=1`, `OLLAMA_NUM_PARALLEL=1`, and
`OLLAMA_MAX_LOADED_MODELS=1`. It also sets `LLAMA_ARG_THREADS` to the rounded-up
cgroup CPU quota (v2 `cpu.max`, then v1 quota/period; `runtime.NumCPU()` for
unlimited or unavailable quotas) and logs that effective value at readiness.
Embedding adds no context-length override. The shared
`github.com/appthrust/appserver-go/appserver/releaseidentity` helper retains the same release
environment/file precedence and response shape.

SIGINT/SIGTERM stops accepting HTTP requests and allows up to ten seconds for
in-flight HTTP work to drain, then forwards SIGTERM to the Ollama process group.
The supervisor waits up to ten seconds before SIGKILL and reaps the child before
the Go process exits. Model-runner descendants are also reclaimed. In external
mode, shutdown never signals or stops the external Ollama process.

## Local external-Ollama run

With Ollama already running on port 11435 and `embeddinggemma` installed:

```sh
go build -o /tmp/embedding-server ./cmd/server
EMBEDDING_OLLAMA_BIN="" \
  EMBEDDING_UPSTREAM=http://127.0.0.1:11435 \
  /tmp/embedding-server --listen 127.0.0.1:8080
```

The `ollama model ready (threads=N)` log marks completed warm-up:

```sh
curl -i http://127.0.0.1:8080/readyz
curl --fail http://127.0.0.1:8080/v1/embeddings \
  -H 'Content-Type: application/json' -d '{"input":["Knowledge semantic search"]}' \
  | jq '{model, dimensions: (.data[0].embedding | length)}'
curl -i http://127.0.0.1:8080/api/tags # 404
```

## Image and focused checks

The image pins Ollama `0.40.4` and its multi-architecture index digest. This tag
was verified as the current stable GitHub release and an active Docker Hub tag
on 2026-10-15. The final image pulls `embeddinggemma` at build time and gives UID
65532 access to its model directory and writable home. Build-time Ollama identity
files are removed. The `server-builder` stage copies only `go.mod go.sum`,
and `cmd/server` from this module, and downloads the public
`github.com/appthrust/appserver-go` module at the version pinned in `go.mod`.

```sh
go build ./...
go vet ./...
go test ./...
# Compile only the Go image stage, without downloading Ollama or its model:
docker build --platform linux/amd64 --target server-builder .
# Full runtime image (large Ollama base and model download):
docker build -t embedding:local .
```

Unit tests use fake upstreams and a subprocess fake Ollama. They cover endpoint
isolation, model enforcement, exact request-size/concurrency boundaries,
readiness/warm-up and outage recovery, release identity precedence, child
restart/backoff and SIGTERM forwarding/reaping (in the shared supervisor tests).
The real external-Ollama smoke
covers the OpenAI-compatible path and 768-dimensional output; a builder-only
image check does not prove the final model-pull layer or non-root runtime.

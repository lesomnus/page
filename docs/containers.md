# Container deployment

The image runs only `cmd/page` as UID/GID `65532`. It includes the system CA
bundle for GitHub and S3 HTTPS. Go, Node, the TypeScript example and the SQL
scaffold are not runtime dependencies. The image supports Linux amd64 and arm64.

Build and inspect it locally:

```sh
docker buildx bake -f docker-bake.hcl app --set app.platform=linux/amd64 \
  --set app.tags=page:local --load
docker run --rm --network none --read-only page:local version
```

Mount the deployment configuration and provide credentials at runtime. For
example, the following publishes HTTP on port 8080 using a pre-existing S3 bucket:

```sh
docker run --rm --read-only --tmpfs /tmp:rw,nosuid,nodev,size=384m \
  --cap-drop ALL --security-opt no-new-privileges \
  -v "$PWD/page.yaml:/etc/page/page.yaml:ro" -p 8080:8080 \
  -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY -e AWS_REGION -e GITHUB_TOKEN \
  page:local run --config /etc/page/page.yaml
```

Use a reachable S3 endpoint: `127.0.0.1` inside the container refers to that
container. The mounted configuration must allow UID 65532 to read it. Archive
extraction needs writable temporary storage; Kubernetes can mount an `emptyDir`
at `/tmp` with a size limit above the configured `max_archive_bytes`. Prefer
disk-backed temporary storage when memory is limited. Set `api_listen: ""` to
disable the read-only gRPC endpoint when it is not used.

One active sync owns each source/site. A combined `page run` Deployment must use
`replicas: 1` and `strategy: Recreate`: the default rolling update can overlap
two writers even with one replica. For independent serving availability, run
`page serve` separately and keep `page sync` single-replica/Recreate. The serving
process needs object read/list access; sync also needs write/delete and source
credentials. Page's durable data lives in S3, so its own container needs no PVC.

`run` refreshes its hostname index after its source commits a publication. The
five-minute reconciliation discovers missed hostname changes and new sites.
HTTP readers share a bounded metadata cache: Site data has a five-second
freshness interval and compact immutable release routing is reused by release
identity. Concurrent cold reads coalesce. Source sync and API reads bypass it.

`metadata_cache.memory_bytes` defaults to 4 MiB of accounted cache weight,
including a conservative decoded-value estimate; it is not a process RSS limit.
`disk_bytes` defaults to 64 MiB per storage namespace. `directory` defaults to `/tmp/page-cache`; an
empty string disables disk caching. The Docker example above mounts `/tmp` as
`tmpfs`, so its default cache uses RAM. For a disk L2, set `directory` to
`/var/cache/page` and mount a disk-backed directory or volume writable by UID
65532 there. Mount a dedicated disk-backed `emptyDir`
when using a read-only container filesystem. The directory is disposable local
cache for one serving process, needs UID 65532 write access, and is never exposed
as static content. It needs no PVC. Cache files are scoped by the S3
endpoint/bucket/prefix identity. Disk cache failure falls back to memory/origin.

Freshness expiry keeps the cached value. Site ETag revalidation renews unchanged
data without a GET, and changed data is replaced. Local publication invalidates
mutable entries immediately. Memory eviction leaves a disk copy; mutable data
loaded from disk is revalidated, while immutable release descriptors are reused
without an S3 read. Disk eviction is capacity-based. Origin revalidation failures
return 503 rather than serving expired Site data. File inventories, per-file
metadata and bodies stay out of these caches; bodies stream from Storage.
Source validation and GC can still allocate complete inventories temporarily.
Optional GitHub webhooks
use a separate `webhook_listen` port with `GITHUB_WEBHOOK_SECRET` at runtime.
Forward only `POST /webhooks/github` to that port through HTTPS and keep the
static-serving and gRPC routes under their existing access policy. See
[notification configuration](mvp.md#변경-알림과-재확인) for events, limits and
the independent source polling fallback.

The server currently has no dedicated health URL. Its `/` response depends on
the Host and published site, so an ordinary Kubernetes HTTP probe can receive
404. A TCP startup/readiness probe on 8080 works for the serving listener.
Temporary GitHub or S3 failures should not cause liveness restarts.

## Image publication

The `ci` workflow verifies the application before its `image` job runs. That
job smoke-tests the stamped binary as non-root with a read-only filesystem,
then builds amd64 and arm64 with `docker-bake.hcl`. PRs build without
publication. Main, `v*` release tags and manual runs on main can publish to
`ghcr.io/lesomnus/page`. The job follows the variable and tagging contract in
[cr's CI](https://github.com/lesomnus/cr/blob/main/.github/workflows/ci.yml).

| Variable | Meaning / default |
| --- | --- |
| `REPO` | Image repository, `ghcr.io/lesomnus/page` |
| `TAG` | `local` for local Bake; CI sets `edge`, or the `v*` release tag |
| `BUILD_HASH` | OCI revision label; CI supplies the checked-out `github.sha` |
| `BUILD_TIMESTAMP` | UTC build timestamp; CI shares one value across builds |
| `BUILD_DATE` | `YYMMDD`, derived from `BUILD_TIMESTAMP` |
| `BUILD_ID` | `r0` locally, `r<github.run_id>` in CI |
| `APP_VERSION` | `YYMMDD-r<run>`; release CI overrides it with the Git tag |
| `PLATFORMS` | `linux/amd64`, `linux/arm64` |
| `PUBLISH` | Workflow guard for main/release publication; not a Bake variable |

Each publication has four tags: `TAG`, `BUILD_ID`, `BUILD_DATE` and
`BUILD_DATE-BUILD_ID`. Thus main publishes `edge`, `r<run>`, `YYMMDD` and
`YYMMDD-r<run>`; a release replaces `edge` with its Git tag. The binary receives
`APP_VERSION` through payday's version linker variable, and `page version`
reports it. `BUILD_HASH` remains the full revision label on the image.

Local builds accept the same environment variables:

```sh
TAG=dev BUILD_ID=r42 BUILD_HASH="$(git rev-parse HEAD)" \
  docker buildx bake -f docker-bake.hcl app
APP_VERSION=v1.2.3 TAG=v1.2.3 docker buildx bake -f docker-bake.hcl app
```

The Actions job summary records the published image index digest. Production
manifests should pin `ghcr.io/lesomnus/page@sha256:...`. A PR's new image is
available from the registry after a successful main or release publication.

GitHub initially creates container packages as **private**, even for a public
repository. After first publication, the package owner must change the page
package's visibility to **public** before an unauthenticated cluster can pull
it, or configure a registry pull secret. The workflow uses `GITHUB_TOKEN` and
links the package to this repository; it does not change package visibility.
Confirm anonymous access before merging an installation PR:

```sh
DOCKER_CONFIG="$(mktemp -d)" docker buildx imagetools inspect \
  ghcr.io/lesomnus/page@sha256:...
```

See GitHub's [Container registry documentation](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry)
for publication permissions and package visibility.

# Container deployment

The image runs only `cmd/page` as UID/GID `65532`. It includes the system CA
bundle for GitHub and S3 HTTPS. Go, Node, the TypeScript example and the SQL
scaffold are not runtime dependencies. The image supports Linux amd64 and arm64.

Build and inspect it locally:

```sh
docker build -t page:local .
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

The server currently has no dedicated health URL. Its `/` response depends on
the Host and published site, so an ordinary Kubernetes HTTP probe can receive
404. A TCP startup/readiness probe on 8080 works for the serving listener.
Temporary GitHub or S3 failures should not cause liveness restarts.

## Image publication

The `image` workflow smoke-tests the container on every PR, then builds both
architectures. Main, manual runs, and PRs whose source branch is in this
repository publish to `ghcr.io/lesomnus/page`. Fork PRs build without registry
credentials or publication.

Every publication has a `sha-<full-source-commit>` tag. Same-repository PRs also
update `pr-<number>`; main updates `main`. The Actions job summary records the
immutable image index digest. Production manifests should pin
`ghcr.io/lesomnus/page@sha256:...`, including when reviewing a PR image.

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

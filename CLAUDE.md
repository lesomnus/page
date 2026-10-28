# Working on page

The product uses payday-generated messages and read service interfaces with
custom object-storage implementations. The S3 data plane runs independently
of GitHub and of the SQL scaffold. See README.md and docs/mvp.md.

## Boundaries

- `internal/storage`: streaming object contract and S3 adapter. Memory storage
  exists for tests. Logical keys never depend on POSIX operations.
- `internal/deploy`: versioned publication metadata, immutable snapshots,
  stable source ownership, durable control intents, publication history,
  previous-production asset leases and owned deletion helpers.
- `internal/routes`: pure pages.json parser, inventory validation and resolver.
  Publication, local route inspection and HTTP serving share this contract.
- `internal/githubsync`: discovery, fixed-SHA publication, Git ref selection,
  preview aliases, retention and GC. Policies belong to the source sync.
- `internal/serving`: retains only hostname ownership and reads Site, compact
  release routing and candidate file metadata from Storage on every request.
  Never add cross-request Site/release/inventory/compiled-engine caches. Bodies
  stream from Storage. It never downloads from a source or decides retention.
- `internal/api`: directly implements generated Site/Deployment Get/List over
  the catalog. No SQL Sink, driver binding or generated mutation stack.
- `internal/app`, `cmd/page`: serving/sync, local route inspection, deployment
  control, status/config/version commands.
- `cmd/scaffold`, `cli`, `cmd/*.go`, `ts/src`: retained upstream SQL/React
  example; use examples/scaffold.yaml. The product does not import its wiring.

## Generate and verify

```sh
npm ci --prefix ts
go tool pd gen --ts .
go mod tidy
./scripts/check.sh
go test -race ./internal/api ./internal/app ./internal/deploy ./internal/githubsync ./internal/routes ./internal/serving ./internal/storage
./scripts/test-s3.sh
```

The S3 script starts the development S3 API when no endpoint is set. See the
README for endpoint overrides and remote Docker. S3 tests use isolated prefixes.
The live GitHub organization and live AWS are separate deployment checks.

Do not edit generated `*.pb.go`, `*.g.go`, `server/bare/`, `server/pd/`,
`internal/ent/`, `proto/app/payday/`, `proto/**/*_svc.g.proto`, or `ts/gen/`.
Edit source proto/overlays and regenerate. A schema change is complete only
when `go tool pd gen --check --ts .` passes.

payday is pinned by commit. Use `go tool pd upgrade --payday <commit>` for an
upgrade, then inspect regenerated files and run the checks.

## Lifecycle invariants

One active writer owns each source/site. Ownership uses a persistent source
configuration ID, not a process ID; origin metadata records the actual source.
Upload files before the completion manifest, then replace the small site
publication object. A failed pass keeps the last successful public state.
Source listing failures are not deletion signals.

External deployment commands only write operator intent to control.json. The
owning source applies that intent to site.json, serialized with its GC; use
ControlAppliedAt to distinguish saved intent from publication. Do not let an
external command publish a release between source validation and deletion.
Pinned/paused production still allows preview updates. Disabled intent is a
durable tombstone and must survive discovery and source outages.

Validate pages.json before writing the completion manifest. Route settings
belong to the release. Previous-production asset leases expose only explicitly
immutable non-HTML files on the production host until expiry, checked on every
request. Preserve protected URL content across routing changes and rollback.
Leases protect GC; publication history alone does not.

Retire public references before GC. Check ownership and current references,
remove the serving projection and then completion manifest before deleting
content/sidecars, and keep the upload
marker until the end so interrupted GC is recoverable. A policy reselecting
an interrupted deletion must reupload a complete snapshot.

Branches/tags are source-specific moving aliases; commit snapshots are fixed.
The first-parent history policy is implemented only in Git sync. Add an OCI
source using its own selection policy when needed; do not invent a universal
reference parser or revision graph.

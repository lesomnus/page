# Working on page

The product uses payday-generated messages and read service interfaces with
custom object-storage implementations. The S3 data plane runs independently
of GitHub and of the SQL scaffold. See README.md and docs/mvp.md.

## Boundaries

- `internal/storage`: streaming object contract and S3 adapter. Memory storage
  exists for tests. Logical keys never depend on POSIX operations.
- `internal/deploy`: versioned publication metadata, immutable snapshots,
  stable source ownership, publication and owned deletion helpers.
- `internal/githubsync`: discovery, fixed-SHA publication, Git ref selection,
  preview aliases, retention and GC. Policies belong to the source sync.
- `internal/serving`: reads published snapshots and serves HTTP. It never
  downloads from a source or makes retention decisions.
- `internal/api`: directly implements generated Site/Deployment Get/List over
  the catalog. No SQL Sink, driver binding or generated mutation stack.
- `internal/app`, `cmd/page`: serve/sync/run/status/config/version commands.
- `cmd/scaffold`, `cli`, `cmd/*.go`, `ts/src`: retained upstream SQL/React
  example; use examples/scaffold.yaml. The product does not import its wiring.

## Generate and verify

```sh
npm ci --prefix ts
go tool pd gen --ts .
go mod tidy
./scripts/check.sh
go test -race ./internal/api ./internal/app ./internal/deploy ./internal/githubsync ./internal/serving ./internal/storage
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

Retire public references before GC. Check ownership and current references,
remove the completion manifest before deleting content, and keep the upload
marker until the end so interrupted GC is recoverable. A policy reselecting
an interrupted deletion must reupload a complete snapshot.

Branches/tags are source-specific moving aliases; commit snapshots are fixed.
The first-parent history policy is implemented only in Git sync. Add an OCI
source using its own selection policy when needed; do not invent a universal
reference parser or revision graph.

# Testing and CI

The suite is pure Go unit tests. Nothing in `make test` needs Docker, a network
call, a real ACME directory or a real RPC socket, which is why the whole thing
runs in one CI job on one runner.

## Shape of the suite

| Package | Test files | `Test*` functions |
|---|---|---|
| `internal/server` | 98 | 977 |
| `internal/cmd` | 11 | 64 |
| `internal/server/acme/providers` | 3 | 28 |
| `internal/server/acme` | 2 | 6 |
| `internal/metrics` | 1 | 5 |
| **total** | **115** | **1080** |

Plus 12 `Benchmark*` functions, run by `make bench` and by nothing else
(`-run=^#` excludes the tests).

The convention is `TestSubject_WhatItDoes`, and the names carry the
specification: `TestBatchGuard_TriggerDomainIsNeverDroppedFromItsOwnBatch`,
`TestCertRenewer_ARIMarkerSurvivesAFailedFirstPartition`,
`TestCappedReader_ExactlyAtTheLimitIsNotOversized`. A rule in `../review/` cites
these names because the name is the claim.

## Helpers

`internal/server/testing.go` holds the shared fixtures, and a new test reuses
them rather than hand-rolling a server:

- `testBackend(t, body, status)` / `testBackendWithHandler(t, handler)` — an
  `httptest.Server` registered with `t.Cleanup`
- `testCountingBackend(t, handler)` — a backend counting accepted connections,
  the only way to observe transport connection reuse from outside. It cannot
  reuse `testBackendWithHandler`, because `httptest.NewServer` starts listening
  before there is a seam to wrap the listener in.
- `testTarget` / `testReadOnlyTarget` / `testTargetWithOptions`
- `testConfig(t)` — ephemeral ports, `t.TempDir()` data dir, the run command's
  listener timeout defaults
- `testServer(t, http3Enabled)` / `testServerWithConfig`
- `testRoutedHandler(t, router)` — wraps a router the way `Server.buildHandler`
  does, so proxy-generated statuses render through the default error pages
  rather than falling back to `http.Error`. A bare router has no error-page
  middleware, so a test asserting on a body needs this.
- the `default*Options` values (`defaultServiceOptions`, `defaultTargetOptions`,
  `defaultHealthCheckConfig`, `defaultDeploymentOptions`, `defaultEmptyReaders`)

`testRouter(t)` lives in `router_test.go` and
`testLoadBalancerWithHandlers(t, …)` in `load_balancer_test.go`.

`testify/require` for setup that must stop the test (`require.NoError` before
dereferencing), `testify/assert` for the assertions themselves. Table-driven
subtests are the house style for anything with more than two cases.

A stub handler that runs on an `httptest` goroutine reports with `t.Error`, not
`require` — `require` calls `t.FailNow`, which is only legal on the test's own
goroutine, and there it aborts the handler without failing the test.

## What is never done in a test

- No real ACME or Let's Encrypt calls. `SANCertManagerConfig.Directory` defaults
  to production, so a test passes `LetsEncryptStaging` or a local stub directory.
- No real DNS-01 provider calls — the `acme/providers` tests exercise factory
  and config wiring, not live provider APIs.
- No Docker, no real RPC socket, no filesystem outside `t.TempDir()`.
- `internal/cmd` tests exercise flag and argument validation (`preRun`)
  directly rather than starting an RPC server.

## CI

`.github/workflows/ci.yml` — on push to `main` and on pull requests into it.
Two jobs, both `permissions: contents: read`, both checking out with
`persist-credentials: false`:

| Job | Steps |
|---|---|
| `lint-actions` | `rhysd/actionlint`, then `zizmorcore/zizmor-action` (`advanced-security: false`) |
| `build` | set up Go from `go-version-file: go.mod`, install golangci-lint **v2.11.3**, `go mod download`, `make build`, `make test`, `make lint` |

Every `uses:` is pinned to a commit SHA with the version in a trailing comment.
There is no matrix: one Go version, taken from `go.mod` (currently `go 1.26.7`).

Nothing measures or gates coverage — `make test` passes no `-cover`. The
coverage thresholds in `../../.claude/rules/testing.md` are a convention, not an
enforced check.

`.golangci.yml` is deliberately small: format with `gofmt`, and `errcheck`
disabled. `make lint` is `golangci-lint run`.

`.github/workflows/security.yml` — `govulncheck` on a cron (`17 6 * * *`) and on
demand, never on a pull request: advisories are published against code that has
not changed, so gating PRs on it would turn every newly published CVE into a red
build on unrelated work. It scans each released target (`linux/amd64`,
`linux/arm64`) under `CGO_ENABLED=0`, because govulncheck's source mode only
analyses the files the current build configuration selects. It reads and writes a
per-ref tracking issue, which is why `concurrency` is keyed by ref with
`cancel-in-progress: false`.

`.github/workflows/docker-publish.yml` — on a tag matching
`v[0-9]+.[0-9]+.[0-9]+` or `v[0-9]+.[0-9]+.[0-9]+.[0-9]+`, or on
`workflow_dispatch` with a `tagInput`. Buildx, QEMU, multi-arch
`linux/amd64,linux/arm64`, pushed to `ghcr.io/zoolutions/dash-proxy:<tag>` and
`:latest` with `org.opencontainers.image.title=dash-proxy`. The tag filter and
`script/release-dash`'s grammar must stay in step: a tag the filter does not
match pushes silently and never builds an image.

Dependabot runs weekly for `github-actions` and `gomod`, each grouped into one
PR, with a cooldown (7 days default; gomod 7/3/2 by semver level).

## Releasing

`script/release-dash vX.Y.Z.N` validates the grammar
(`^v[0-9]+\.[0-9]+\.[0-9]+(\.[0-9]+)?$`), refuses a tag that already exists, runs
`make test`, creates an annotated tag and pushes that single tag. CI publishes.

`script/release` is upstream's and is **not** this repo's release path — it
`docker login`s and pushes `basecamp/kamal-proxy` images itself. See the seeding
discrepancies.

## Related

- `../../.claude/rules/testing.md` — the TDD workflow and the coverage convention
- `../cli-and-rpc/summary.md` — what the `internal/cmd` tests exercise

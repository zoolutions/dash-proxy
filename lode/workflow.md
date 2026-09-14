# Workflow profile

Everything the shared workflow skills (`/lode:lfg`, `/lode:review-pr`,
`/lode:finish-prs`, `/lode:debug-flaky`, `/lode:tdd`, `/lode:plan`) need to know
about dash-proxy that is not already in `../CLAUDE.md`, `../.claude/rules/` or
the rest of `lode/`.

## Commands

| Purpose | Command | Notes |
|---|---|---|
| fast loop (one package) | `go test ./internal/server/` (or `-run TestName`) | no network, no Docker; safe anywhere |
| full suite | `make test` (`go test ./...`) | no network, no Docker, no services. Safe in two worktrees at once: every test uses ephemeral ports and `t.TempDir()`. |
| vet | `go vet ./...` | not run by CI — `make lint` covers it via golangci-lint |
| lint | `make lint` (`golangci-lint run`) | `.golangci.yml`: `gofmt` formatter, `errcheck` disabled |
| formatting check | `gofmt -l internal/ cmd/` | must print nothing; CI fails on it through `make lint` |
| one CI cell locally | `make build && make test && make lint` | there is no matrix — one Go version, from `go.mod` |
| benchmarks | `make bench` (`go test -bench=. -benchmem -run=^# ./...`) | 12 `Benchmark*` functions |
| docs build / check | n/a | no docs site; `README.md` carries the reference |
| regenerate the provider table | `go generate ./internal/server/acme/providers` | rewrites README between the `dns-provider-table` markers |
| run the app | `make build && ./bin/dash-proxy run` | `./bin/dash-proxy -h` prints `kamal-proxy` — the binary moved, `rootCmd.Use` did not |
| image smoke test | `make docker && docker run --rm dash-proxy dash-proxy -h` | needs Docker; not part of `make test` |

**Version pin that matters:** CI installs golangci-lint **v2.11.3**
(`.github/workflows/ci.yml`). A newer local binary reports findings CI will not,
and misses none it will. Install the pinned one before trusting a clean run:
`go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.3`.

## Branches and PRs

- Default branch: `main` — the only long-lived branch since the 2026-08 clean
  break. There is no `upstream` remote and no `dash` branch; `origin` is
  `zoolutions/dash-proxy`.
- Work branches: `feature/*`, `fix/*`, `refactor/*`, `ci/*`, `chore/*`, rooted
  off fresh `origin/main`.
- Commits: conventional (`feat:`, `fix:`, `refactor:`, `perf:`, `docs:`,
  `test:`, `chore:`, `ci:`), scoped to the area (`san-cert`, `wildcard-certs`,
  `router`, `rpc`, `cache`, `acme`, `health-check`, `cmd`, `deps`, `metrics`).
  The body says **why**.
- PR body sections, in order: Summary, Test plan, Deviations & judgment calls,
  Gate.
- Merge policy: squash on `main` after green and approval — the ruleset requires
  linear history. Never force-push a published branch; merge `main` forward
  into it instead.
- Attribution: **no** `Co-Authored-By: Claude` and no "Generated with" line.
  Commit messages end with the session's `Claude-Session:` line when one is
  supplied; PR bodies end with the session URL.

Dormant branches still on `origin`: `san-certificate-batching`,
`wildcard-certs` (merged history), `feat/loadbalancing` (superseded), and
several merged `feature/cache-*` branches. Do not branch from any of them.

## Layers

| Layer | Files | Edit rule |
|---|---|---|
| entry point | `cmd/dash-proxy/main.go` | owned here; a thin `cmd.Execute()` |
| CLI + RPC client | `internal/cmd/` | owned here. Flags are registered once per command — pflag **panics** on a duplicate registration, which is how a union-shaped merge conflict becomes a boot crash. |
| server, routing, certs, cache | `internal/server/` | owned here. The largest package by far; new behaviour goes in a new file next to its siblings, not into `service.go` or `san_cert_manager.go`. |
| ACME provider registry | `internal/server/acme/`, `internal/server/acme/providers/` | owned here, but `registry.go` is the single source — `NewProvider`, `detectionOrder`, the flag help and the README table all derive from it |
| generated | the README's `dns-provider-table` block | edit `registry.go`, then `go generate ./internal/server/acme/providers`; `TestREADMEProviderTable_MatchesRegistry` fails the build on drift |
| metrics | `internal/metrics/` | owned here |
| error pages | `internal/pages/` | embedded HTML for 404/413/502/503/504 |
| frozen names | the module path `github.com/basecamp/kamal-proxy`, the RPC service name `kamal-proxy`, the `/.kamal-proxy/` internal path prefix | **never rename.** 16 client call sites and the deployed gem depend on the RPC name; the path prefix is a deployed contract with the gem. |
| upstream's leftovers | `script/release` | prefer leaving it as basecamp wrote it; the fork's release path is `script/release-dash` |

## Shapes

Check a change against these before calling it done; a reviewer will name the
one you forgot.

- **A restored state file written by an older binary** — an added option is
  `omitempty`/`omitzero` with a zero value meaning "off", or the restore changes
  behaviour for services that predate it. `Service.UnmarshalJSON` still maps four
  pre-plural legacy keys.
- **A `Config` built directly, not by cobra** — tests, restored state, rollouts
  and older RPC clients all bypass the CLI, so defaulting belongs server-side.
- **A gob wire type during a proxy replacement** — CLI and server can be
  different versions; `ServiceDescription` and the `*Args` types are append-only.
- **A registered host versus a dynamic (source-learned) one** — they differ on
  synchronous issuance, the compaction window, and probe eligibility.
- **A wildcard identifier** — has no single name to answer on, no service of its
  own, and needs DNS-01. Every domain-resolution path needs a wildcard branch.
- **A domain in a DNS-01 zone versus an HTTP-01 one** — probe eligibility and
  order partitioning both turn on it.
- **A second ACME directory** (`--tls-staging`) — a second account, a second key
  file, a second rate-limit bucket, and a certificate that records which one
  issued it.
- **An empty, truncated or malformed poll body** — a payload-level error keeps
  the last good set; an entry-level one skips that entry.
- **A varying response, and a non-varying one** — the cache stores them as two
  record shapes.
- **A shared store versus a local one** — only the shared one implements
  `CacheLeaser`; every leaser call site needs the nil check.
- **The zero `netip.Addr`** — allow-list, deny-list and rate-limiter each answer
  differently, on purpose.
- **`--proxy-protocol` and a trusted-proxy chain** — `r.RemoteAddr` is only as
  trustworthy as the peers allowed to assert it.
- **Both TLS listeners plus HTTP/3** — HTTP/3 is pinned to TLS 1.3, so
  `--min-tls` neither lowers nor raises it.
- **Go 1.26 (the `go.mod` floor)** — CI takes its toolchain from `go.mod`, not
  from your laptop.

## Constraints

Reviewer suggestions that are wrong in this repository.

| Suggestion | Why it is wrong here |
|---|---|
| "Rename the module / RPC service / socket to dash-proxy" | The binary, user, data directory, socket file and image label already moved (stage 3c). The **module path** and the **`net/rpc` service name** are deliberately deferred, each with its own follow-up issue; renaming them breaks every deployed gem mid-upgrade for no gain. |
| "Rename `/.kamal-proxy/`" | It is a deployed contract with the gem; the prefix kept its spelling through the rename on purpose. |
| "Make the pre-flight probes honour the caller's context" | Each is already bounded by a 5s client timeout and a 16-way concurrency cap, and a completed probe benefits the next handshake even when this one gave up. Reviewed and declined (PR #97). |
| "Publish partial renewer gauges on cancellation" | Every renewer gauge updates only after a completed reconcile; a half-loop count is wrong in a less predictable way, and the path only runs at shutdown. Declined (PR #105). |
| "Fall back to the quarantine ladder when `Retry-After` is stale" | The margin already guarantees the retry lands at least a minute after the advertised time; the ladder would delay a permitted retry by 15+ minutes. Declined (PR #112). |
| "Enumerate Cloudflare's mixed-namespace credential pairs" | That gap fails **closed** with an actionable error. Adding them would double the registry and document a hygiene nobody should adopt. Declined (PR #116). |
| "Extract a shared persisted-state helper for the two dynamic managers" | Different schemas; the shared part is already shared (`writeFileAtomic`, `sourcePoller`, `refreshNudge`). Declined (PR #92). |
| "Remove the duplicated config parsing in `preRun`" | Deliberate: `preRun` fails fast before an ACME account is registered, and the use-site parse protects a `Config` built without the CLI. |
| "Add `-cover` gating to CI" | Nothing measures coverage today; the thresholds in `.claude/rules/testing.md` are a convention. Adding a gate is a decision, not a fix. |
| "Use `require` in an `httptest` handler" | `t.FailNow` is illegal off the test goroutine — it aborts the handler without failing the test. Use `t.Error`. |
| "Tag a release candidate `v1.0.0-rc1`" | The gem compares the image tag with `Gem::Version`, which sorts a hyphen suffix *below* the release it names. |

## Docs

- User-facing docs: `README.md` (68 KB, the whole reference — there is no docs
  site). A new flag or command is documented there in the same PR.
- The DNS-provider table inside `README.md` is **generated** between
  `<!-- BEGIN GENERATED: dns-provider-table … -->` and its `END` marker; edit
  `internal/server/acme/providers/registry.go` and run
  `go generate ./internal/server/acme/providers`.
- `ROADMAP.md` — proxy-side roadmap with code anchors.
- Changelog: **none**. There is no `CHANGELOG.md`; the GitHub release and the
  commit history are the record.
- A change to a flag, a command, an output line or a provider always updates
  `README.md` in the same PR.
- Files that pin a version and drift after a release: none in this repo. The
  `dash` gem's `MINIMUM_VERSION` must name an already-published tag, so the
  proxy image releases **first** and the gem second.

## CI

- Workflows:
  - `ci.yml` — on push to `main` and PRs into it. Jobs: `lint-actions`
    (actionlint + zizmor) and `build` (`make build`, `make test`, `make lint`).
  - `security.yml` — `govulncheck` on cron `17 6 * * *` and `workflow_dispatch`
    only, never on a PR. Scans `linux/amd64` and `linux/arm64` under
    `CGO_ENABLED=0`; writes a per-ref tracking issue.
  - `docker-publish.yml` — on `v[0-9]+.[0-9]+.[0-9]+` or
    `v[0-9]+.[0-9]+.[0-9]+.[0-9]+` tags, or `workflow_dispatch` with `tagInput`.
    Multi-arch buildx to `ghcr.io/zoolutions/dash-proxy`.
- Matrix: **none.** One Go version, from `go-version-file: go.mod`. Cells that
  differ from local: the Go toolchain (`go.mod` says 1.26.7) and golangci-lint
  (pinned v2.11.3).
- Fetch a failure: `gh run view --job <id> --log-failed`, or
  `gh pr checks <n>` to find the job.
- "Green" means both `ci.yml` jobs pass. `security.yml` never runs on a PR, so
  it is never part of a PR's green.
- Known not-this-branch failures: none standing. A Dependabot `gomod` PR can
  turn `make lint` red on a transitive API change — that is the PR's own
  problem, not the branch's.
- Shared or rate-limited services the checks hit: **none.** No live ACME, no
  registry, no Docker in `make test`, so PRs may run in parallel freely.

## Flake sources

- **Wall-clock and backoff timing.** The health-check backoff
  (`initialHealthCheckDelay` 50ms → `maxPreHealthyDelay` 2s), the quarantine
  ladder, `refreshMinInterval` (10s) and the renewal jitter all read real time.
  Tests that assert on a deadline rather than injecting a clock are the first
  suspects.
- **Goroutine lifetime past the test.** Pollers, health checks, the release
  prober and the renewer all run on their own goroutines; background work
  outliving its test is exactly why `metrics.Tracker` holds its delegate behind
  an `atomic.Pointer`.
- **Process-wide singletons.** `metrics.enableOnce`, the Prometheus default
  registry (which panics on re-registration) and `rpc.RegisterName`'s
  `sync.Once` all mean `go test -count=2` exercises a different path than
  `-count=1`.
- **Map iteration order.** Go randomizes it, so anything that must be
  deterministic sorts first — `compileRedirectMap` walks host keys sorted for
  exactly this reason.
- **`httptest` port reuse** under heavy parallelism; `testConfig` binds port 0
  to avoid it.

Not a flake source: live CDNs or registries. Nothing in `make test` leaves the
machine.

## Conflicts

| File | Rule |
|---|---|
| `go.sum` | never hand-merge: resolve `go.mod`, then `go mod tidy` |
| `go.mod` | take `main`'s toolchain and dependency graph, keep `go-acme/lego/v4`, then `go mod tidy` |
| `internal/cmd/run.go` | union of the flag registrations, but each flag registered **exactly once** — pflag panics on a duplicate, so a naive union boots and crashes |
| `internal/server/config.go`, `router.go` | union of both sides' fields and methods; neither side's are optional |
| `internal/server/service.go` | read both sides — one usually carries a behaviour change and the other feature wiring; preserve both |
| `README.md`'s generated table | do not merge it: take either side, then `go generate ./internal/server/acme/providers` |
| `Dockerfile`, `Makefile`, `script/release` | prefer upstream's shape; the fork's release path is `script/release-dash` |
| test fixtures | add a second fixture rather than merging two shapes into one |

`git rerere` is **not** enabled in this checkout. If you turn it on, always
`git diff --staged` before trusting a replayed resolution — one recorded in a
different context can be wrong.

## Verification

- The manual check a user of this change would do: `make build`, then
  `./bin/dash-proxy run --http-port 8080 …` in one shell and
  `./bin/dash-proxy deploy …` in another, and look at the access log and
  `curl localhost:<metrics-port>/metrics`. For a flag, `./bin/dash-proxy <cmd> -h`
  is the surface the user reads first.
- A hot-path change (`Router.ServeHTTP`, `Target.ServeHTTP`, `LoadBalancer`
  selection, `SANCertManager.GetCertificate`) carries a `make bench`
  before/after on the same machine, reporting ns/op **and** allocs/op — see
  `../.claude/rules/performance.md`.
- Stress iterations for a flake proof: `go test -race -count=50 -run <TestName>
  ./internal/server/` — `-race` as well as `-count`, because most of this
  repo's non-determinism is concurrency rather than ordering.
- Where evidence goes: `lode/tmp/` (git-ignored), unless the PR needs an
  auditable trail.

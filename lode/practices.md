# Practices

Patterns this codebase holds to that `../.claude/rules/` does not already state.
Style, file size, error wrapping, mutex discipline, TDD and the git workflow
live in `../.claude/rules/coding-style.md`, `../.claude/rules/testing.md`,
`../.claude/rules/git-workflow.md` and `../.claude/rules/performance.md`.

## State files evolve by addition only

An option added to `ServiceOptions` or `TargetOptions` carries `omitempty` or
`omitzero` and a zero value that means "the feature is off", because the state
file outlives the binary that wrote it and a restored service must behave the
way it did before the option existed. The rule governs additions, not the whole
struct: 26 of `ServiceOptions`' 42 JSON fields and 10 of `TargetOptions`' 21
carry the tag, and the bare ones are the original kamal-proxy fields (`hosts`,
`tls_enabled`, `response_timeout` …) that every state file has always had. Copy
the newer neighbours, not the older ones. `Service.UnmarshalJSON` still maps four
pre-plural legacy keys (`active_target`, `rollout_target`, `hosts`,
`path_prefixes`) onto their current fields. `IdleState` is persisted as a *name*
rather than an enum ordinal, and `ParseIdleState` folds anything unrecognised —
including the empty string every pre-scale-to-zero file carries — to active.

`ServiceDescription` (`internal/server/router.go`) carries the same rule for the
wire: the CLI and the server can briefly be different versions during a proxy
replacement, and gob tolerates an added field but not a changed one, so fields
there are append-only.

`Router.decodeStateServices` accepts both the bare JSON array the writer emits
and a `{"version":…,"services":[…]}` envelope, so the writer can switch once
every deployed generation reads both.

## Every durable write is staged, and the durability level is stated

There are four shapes, and which one a file gets says how much its durability is
worth:

- `writeFileAtomic` (`internal/server/util.go`) — fixed `<path>.tmp`, explicit
  `Chmod` because `O_CREATE` applies `perm` only to a file it creates and umask
  masks it even then, `fsync`, rename. Used for the routing table and its
  `.bak` (`internal/server/router.go`) and `dynamic-redirects.state`
  (`internal/server/dynamic_redirect_state.go`).
- Hand-rolled `os.WriteFile` to `<path>.tmp` then `os.Rename`, with no fsync:
  `dynamic-domains.state` (`internal/server/dynamic_domain_state.go`) and
  `acme.state` (`writeManagerStateFile`). Torn reads are prevented; power loss
  is not, which is acceptable because both are rebuilt from a poll or a
  handshake.
- `writeFileStaged` (`internal/server/cert_store_restore.go`) — unique
  same-directory temp, mode 0600, fsync, rename, removed on every failure path.
  The restore path only, where a planted symlink or an inherited permission on a
  private key is the threat.
- Plain `os.WriteFile` at 0600 for the ACME account key
  (`SANCertManager`, saving `acme_user*.json`): it is written once at
  registration, and a torn one is re-registered on the next boot.

Certificate pairs go through `writeCertificateFiles`, the same staged path the
offline importers use, so a crash or a concurrent reader never sees a torn pair;
callers hold `stateMu`.
- Directory fsync policy lives in exactly one place, `syncOpenDir`, which
  excuses `ENOTSUP`/`EINVAL` and nothing else; `syncDir` (pathname) and
  `syncRootDir` (pinned `os.Root`) delegate to it.
- `Router.RestoreLastSavedState` deletes a leftover `.tmp` before reading: under
  the atomic protocol a surviving temp file is by definition an aborted write.

## Configuration is parsed twice, deliberately

`runCommand.preRun` rejects a bad `--min-tls`, `--log-format`,
`--trace-context`, `--acme-dns-provider` or `--cache-store` before anything
registers an ACME account. The same values are parsed again where they are used
(`Server.startHTTPServers`, `Server.startMetricsServer`), because a `Config`
built directly — by a test, or by a future caller — must not start a listener
that silently ignores a setting. The duplication is the point; do not remove
either half.

The same reasoning puts defaulting on the server side: `TargetOptions.poolSettings`
resolves every zero to the proxy's default rather than relying on cobra flag
defaults, because restored state, rollouts and older RPC clients all bypass the
CLI.

## Fail open or fail closed, decided per subsystem and written down

- A cache store that is unreachable turns lookups into misses and writes into
  logged errors, never a failed request (`CacheStore`'s contract). A lease in
  any doubt **grants**: a duplicate fetch is cheaper than an origin outage.
- `ipAllowList` refuses the zero `netip.Addr`; `denyList` matches nothing for
  it. Each fails in the direction it exists for.
- A certificate export aborts when `acme.state` exists but does not parse, and
  when a containment check cannot read part of the certificate tree — a backup
  that cannot restore must not be reported as taken.
- An unattributable ACME order failure quarantines the whole batch, so retries
  back off rather than looping into a rate limit.

Anywhere a new failure path is added, say which direction is the harmless one in
the comment, the way these do.

## Network probes are bounded by count and by client timeout, not by context

`probeDomains` runs at most `maxConcurrentProbes` (16) pre-flight probes at once
and each probe is bounded by `preflightTimeout` (5s) on the shared client. The
probes deliberately do **not** honour the caller's context: a probe that
completes benefits the next handshake even when this one has given up. This is a
reviewed decision, not an oversight — see `review/certs-issuance.md`.

## Seams instead of live dependencies

Anything that would otherwise need a live external service is an interface the
production path fills in: `certObtainer` (ACME), `ContainerLifecycle` (Docker),
`CacheStore`/`CacheLeaser` (Redis), `serviceResolver` (the router),
`metrics.EventTracker` (Prometheus), `renewalInfoGetter` / `directoryObtainer`
(optional ACME capabilities discovered with a type assertion). Adding a
capability to an obtainer means adding a small optional interface and asserting
for it, not widening `certObtainer`.

## One table, generated docs, and a drift test

The DNS provider registry (`internal/server/acme/providers/registry.go`) is the
only list of providers: `GetProviderInfo`, `NewProvider`, `detectionOrder`, the
`--acme-dns-provider` help string and the README table all derive from it.
`go generate ./internal/server/acme/providers` rewrites the README between the
`BEGIN/END GENERATED: dns-provider-table` markers, and
`TestREADMEProviderTable_MatchesRegistry` fails the build when the committed
table drifts. A second hand-maintained provider list is the thing this design
exists to prevent.

## Locks are named for what they serialize, and the order is written down

`SANCertManager` has `mu` (in-memory maps) and `stateMu` (every disk write to
the certificate store); `adoptCertificateAt` takes `stateMu` then `mu` so a
state-file snapshot can never name a certificate whose files are not written
yet. `Router` has `serviceLock` and `saveLock`, and `Service.MarshalJSON`
documents the router-then-service order that keeps it from inverting. Metrics
calls are made after the relevant lock is released (`memoryCacheStore.store`
returns evictions for the caller to report), so the Prometheus registry never
sits on a request-path mutex.

## Internal endpoints live under `/.kamal-proxy/` and are exempt from redirects

`redirectExemptPrefixes` (`internal/server/redirect_map.go`) keeps ACME
challenges and the proxy's own ping, pre-flight and refresh endpoints reachable
on every host; `isRedirectExemptPath` is checked before both the dynamic map and
the static `--redirect` rules. The TLS/canonical hop still applies to them. The
prefix is part of the deployed contract with the gem — renaming it is a breaking
change, and it kept the `kamal-proxy` spelling through the 3c rename.

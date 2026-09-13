# CLI and RPC

Two processes, one binary. `dash-proxy run` is the server; every other verb is a
client that dials a unix socket and makes one `net/rpc` call. The split is what
keeps the request path free of configuration parsing and the command path free
of request-path locks.

## The binary

`cmd/dash-proxy/main.go` calls `cmd.Execute` (`internal/cmd/root.go`), which
registers fourteen top-level commands: `run`, `deploy`, `remove`, `pause`,
`stop`, `resume`, `list`, `rollout`, `domains`, `cache`, `drain`, `import`,
`export`, `hold`.

`rootCmd.Use` is still `kamal-proxy`, so `dash-proxy -h` prints
`kamal-proxy [command]`; the environment prefix in `internal/cmd/util.go` is
still `KAMAL_PROXY_`. Both are noted in the seeding discrepancies — the stage-3c
rename moved the binary, socket, user, data directory and image label, not
these.

There is **no `version` command**. The image tag is the version: the `dash` gem
docker-inspects the running container and compares the tag with `Gem::Version`.

## The socket and the RPC name

`CommandHandler.Start` (`internal/server/commands.go`) registers the service as
`"kamal-proxy"` once, behind a `sync.Once`, and listens on
`Config.SocketPath()`. That path is `DASH_PROXY_SOCKET`, else
`KAMAL_PROXY_SOCKET` (the gem sets it on containers booted before the rename,
and a running proxy is reached over whatever socket it opened), else
`<runtime dir>/dash-proxy.sock`.

The RPC name is load-bearing and does not move: **16** call sites in
`internal/cmd` dial `client.Call("kamal-proxy.<Verb>", …)`, one per verb —
`Deploy`, `Remove`, `Pause`, `Stop`, `Resume`, `List`, `Drain`,
`RolloutDeploy`, `RolloutSet`, `RolloutStop`, `DomainsStatus`, `DomainsRefresh`,
`DomainsRetry`, `CertsExport`, `CachePurge`, `CacheStats`. It is internal,
spoken over a unix socket inside one container, and renaming it buys nothing
while breaking every deployed gem mid-upgrade.

Each connection is served on its own goroutine; `net.ErrClosed` on `Accept` is
the shutdown signal, and any other accept error is logged and retried.

## Wire types are append-only

`DeployArgs` carries everything a deploy configures: the service name, target
and reader URLs, `DeploymentOptions`, `ServiceOptions` and `TargetOptions`.
`ServiceDescription` (`internal/server/router.go`) is the listing shape.

Both are gob on the wire, and the CLI and the server can briefly be different
versions during a proxy replacement — gob tolerates an added field but not a
changed one. Fields are therefore added, never renamed or retyped.

The status types carry their own documentation of the domain model:
`DomainsServiceStatus` (source, domains, `fetched_at`, `held_removals`),
`QuarantineStatus` (`until`, `failures`, and a `kind` of `preflight`, `acme` or
`rate_limited`), `RegisteredDomainStatus` (service, certified, `expires_at`).

## Where configuration is parsed

Twice, deliberately. `runCommand.preRun` rejects a bad `--min-tls`,
`--log-format`, `--trace-context`, `--acme-dns-provider` or `--cache-store`
before anything registers an ACME account. The same values are parsed again
where they are used (`Server.startHTTPServers`, `Server.startMetricsServer`),
because a `Config` built directly — by a test, or a future caller — must not
start a listener that silently ignores a setting.

Defaulting lives on the **server** side for the same reason:
`TargetOptions.poolSettings` resolves every zero to the proxy's default rather
than relying on cobra flag defaults, because restored state, rollouts and older
RPC clients all bypass the CLI entirely.

Per-service validation is `ServiceOptions.Validate`
(`internal/server/service.go:235`) and `TargetOptions.Validate`
(`internal/server/target_pool.go:105`), run once on the deploy so anything a
deploy could get wrong fails *there* rather than on a request. The individual
`validate*` helpers they call live in
`internal/server/service_options_validation.go`.

## Environment variables

`internal/cmd/util.go` reads `KAMAL_PROXY_<KEY>` first and bare `<KEY>` second,
through `getEnvString` / `getEnvInt` / `getEnvDuration` / `getEnvBool`. An
unparseable value falls back to the default rather than failing — the flag is
the authoritative surface and the env var is the convenience.

`ensureDataDir` creates `--data-dir` when one was supplied, shared by the two
commands that write into it (`run`, `import certs`).

## Offline commands

`import certs` and `export certs --verify` run against a **stopped** proxy and
never dial the socket. `export certs` dials first and falls back to an offline
read only when nothing answers — see `../review/cert-store-export.md` for why
an outdated-but-live proxy is a failure rather than a fallback.

`hold` is a hidden command that blocks on a signal, so a container can own a
shared network namespace without running a proxy in it.

`drain` and `SIGTERM` converge on the same path: close the public listeners,
finish in-flight requests up to a timeout, save state, exit.

## Related

- `../request-path/summary.md` — what `DeployArgs` becomes
- `../certs/store-and-recovery.md` — the `import`/`export` verbs in detail
- `../review/cert-store-export.md`, `../review/cert-store-restore.md`

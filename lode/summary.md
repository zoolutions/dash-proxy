# dash-proxy

An HTTP/HTTPS reverse proxy for zero-downtime container deploys, published as
`ghcr.io/zoolutions/dash-proxy` and driven by the `dash` gem. It began as a fork
of basecamp/kamal-proxy; since the 2026-08 clean break there is no upstream
remote and `main` is the only long-lived branch, so upstream code arrives only
by deliberate cherry-pick. What the fork adds over kamal-proxy is mostly
certificate work — SAN batching, wildcard certs over DNS-01, per-service ACME
directories, a quarantine ladder, and an exportable certificate store — plus a
response cache, dynamic domain and redirect sources, scale-to-zero, and a large
set of per-service request-path options.

The process is two halves that meet at `Router`. A **request path** (listener →
middleware chain → `Router` → `Service` → `LoadBalancer` → `Target`) does no
disk or registry I/O beyond what a cache store and a certificate lookup need,
and a **command path** (`bin/dash-proxy <verb>` → `net/rpc` over a unix socket →
`CommandHandler` → `Router`) mutates it. Everything an operator configures
arrives over that socket as a `DeployArgs`, is validated once in
`ServiceOptions.Validate` / `TargetOptions.Validate`, and is persisted to
`dash-proxy.state` so a restart restores exactly what was serving.

Three invariants govern changes. **The Go module path
(`github.com/basecamp/kamal-proxy`) and the `net/rpc` service name
(`"kamal-proxy"`) do not move** — 16 client call sites and the deployed gem
depend on the latter (`internal/server/commands.go#Start`). **No name provisions
a certificate unless something asked for it** — `Router.GetCertificate` refuses a
server name that routes nowhere, and `SANCertManager.GetCertificate` gates
issuance on the registered/dynamic allowlist. **Every persisted file is written
through a temp file and a rename, and read forgivingly** — a `.bak` fallback for
the routing table, and every added JSON field optional so an older state file
still restores. The staging helper differs per subsystem and the difference is
deliberate (`practices.md`).

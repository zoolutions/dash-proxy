# Request path

Everything between a client connection and the backend. Nothing here reads the
registry or the ACME directory; the only I/O on this path is the cache store,
the certificate lookup for a handshake, and the proxied request itself.

## Listeners

`Server.Start` (`internal/server/server.go`) opens, in order, the metrics
listener, the HTTP and HTTPS listeners (plus HTTP/3 when `--http3`), and the RPC
command socket. `--reuse-port` puts `SO_REUSEPORT` on every listener so an
overlapping proxy generation can hold the same ports during a handoff;
`--proxy-protocol` wraps the two TCP listeners *before* TLS layering, because
the PROXY preamble arrives ahead of the ClientHello.

Both TLS configs take `GetCertificate: s.router.GetCertificate`.
`clientCertificateConfig` is installed as `GetConfigForClient`: for a host whose
service was deployed with `--tls-client-ca-path` it returns a **clone** of the
listener's config with `ClientAuth`/`ClientCAs` set — a fresh config would drop
ALPN and the minimum version, downgrading mTLS hosts to HTTP/1.1 and breaking
`tls-alpn-01`. HTTP/3 is pinned to TLS 1.3 because QUIC is defined only over it,
so `--min-tls` neither lowers nor needs to raise that listener.

## The root middleware chain

`Server.buildHandler` (`internal/server/server.go:390-423`) composes handlers
inside-out; the outermost runs first:

| Order | Handler | Why it is where it is |
|---|---|---|
| 1 | `WithPingMiddleware` | outermost and unconditional, which is what keeps `/.kamal-proxy/ping` out of the access log |
| 2 | `DynamicRedirectManager.WrapHandler` | `/.kamal-proxy/redirects/refresh`, only when a redirect manager exists |
| 3 | `DynamicDomainManager.WrapHandler` | `/.kamal-proxy/domains/refresh` and `/.kamal-proxy/preflight/<nonce>`, only when ACME is on |
| 4 | `SANCertManager.HTTPHandler` | HTTP-01 challenges, ahead of ordinary routing |
| 5 | `WithRequestStartMiddleware` | stamps the start time |
| 6 | `WithRequestIDMiddleware` | generates `X-Request-ID` |
| 7 | `WithLoggingMiddleware` | creates the logging request context everything below writes into |
| 8 | `WithTraceContextMiddleware` | inside logging, because the trace is recorded on that context |
| 9 | `WithErrorPageMiddleware(pages.DefaultErrorPages, root=true, …)` | the built-in 404/413/502/503/504 pages |
| 10 | `Router` | |

A liveness probe therefore carries no request ID and never renders an error
page — the deliberate cost of an endpoint a monitor hits forever
(`internal/server/ping_handler.go`). There is no readiness variant: the
listeners only exist between `Start` and `BeginDrain`.

## Router

`Router` (`internal/server/router.go`) owns a `ServiceMap` behind
`serviceLock` (RWMutex) and a separate `saveLock` for state writes.
`ServeHTTP` resolves `(service, matchedPrefix)` through
`ServiceMap.ServiceForRequest`, answers 404 when nothing matches, and attaches a
`routingContext` carrying the matched prefix when the service strips prefixes.
`RoutedTargetPath(r)` is what every downstream check — health-check detection,
path timeouts, metrics exclusion — matches on.

`Router.GetCertificate` (`internal/server/router.go:565-603`) is the allowlist
gate for the whole handshake path. An empty SNI falls back to
`ServiceMap.DefaultTLSHostname`; a name that routes to no service, or to a
service with no `certManager`, is refused with `ErrorUnknownServerName`. Before
that refusal existed, any DNS record pointed at the proxy could drive a real
Let's Encrypt order.

State: `RestoreLastSavedState` removes a stale `.tmp`, reads
`dash-proxy.state`, falls back to `dash-proxy.state.bak` on a decode failure
(repairing the primary from it), and accepts both the bare-array and the
versioned-envelope encodings. `saveStateSnapshot` marshals under the read lock
and writes under `saveLock` via `writeFileAtomic`.

## Service

`Service` (`internal/server/service.go`) is where per-service behaviour lives.
`initialize` — called by `NewService`, `UpdateOptions` and `UnmarshalJSON` —
builds the cert manager, loads the client CA bundle, composes the middleware
chain, compiles the redirect and rewrite rules, builds the cache handler and
configures the idle controller. Anything a deploy could get wrong is resolved
**here**, not on a request: a bad CA path fails the deploy rather than silently
serving without client verification.

`Service.createMiddleware` (`internal/server/service.go:845-893`) wraps
`serviceRequestWithTarget` inside-out: request deadline (innermost), error
interception, custom error pages, compression, the cert manager's ACME handler,
then `--client-ip-header`. The ACME handler sits above compression because a
challenge is a handful of bytes served to a CA.

`Service.serviceRequestWithTarget` (`internal/server/service.go:895-943`) is the
per-request order, and each step's position is load-bearing:

1. deny rules (before the allow list — an address on both is denied)
2. IP allow list
3. plaintext-only service refusing a TLS request → 503
4. redirects (TLS, canonical host, dynamic map, static rules)
5. rate limit — after the redirect so a 301 spends no token, before the auth
   challenge so a password flood is limited
6. basic auth — after the redirect so credentials are never solicited over
   plaintext, before the pause check so protection does not lapse while paused
7. paused/stopped handling (health checks still answer 200 so downstreams do
   not drop the proxy)
8. rewrite rules — last, so everything above saw the client's own path
9. the cache handler, which is the load balancer directly when no cache is on

Because the cache sits below every check, a stored response can only ever reach
a client the target would have been asked on behalf of.

`Service.redirectURLIfNeeded` (`internal/server/service.go:992-1043`) composes
the TLS and canonical-host hop with the rule sets: exempt paths
(`/.well-known/acme-challenge/`, `/.kamal-proxy/`) skip both rule sources but
still get the TLS/canonical hop, the dynamic per-host map answers before the
static service-wide rules, and a rule naming a path is completed with the scheme
and host the request was already headed for — which is also what keeps a
captured `//evil.example.com` a path on this host.

## LoadBalancer and Target

`LoadBalancer` (`internal/server/load_balancer.go`) holds every target plus the
healthy writers and readers, rebuilt by `updateHealthyTargets` on each state
change. `StartRequest` returns a closure so target selection and the pause wait
happen after the service lock is released; with a retry policy it defers
selection entirely into `serveWithRetries`.

Read/write split: a GET or HEAD is a read request unless it is a WebSocket
upgrade and `--read-target-websockets` is off. A read is served by a reader
unless the client holds an unexpired `kamal-writer` cookie, which
`loadBalancerResponseWriter` sets on every write response unless the target
answered `X-Writer-Affinity: false`. Weighted pools go through
`nextWeighted`; unweighted ones use the plain rotating index, so a deployment
that sets no weights behaves exactly as it did before weights existed.

A single-target pool stops health-checking once its target is healthy — taking
it out of the pool would not help — **except** when `persistentHealthChecks` is
set by `RecheckHealth`, which is how `--recheck-targets-on-restore` keeps a
restored target under observation.

`Target` (`internal/server/target.go`) owns one `httputil.ReverseProxy` per
response timeout in play (`pathProxyHandlers` plus the default), each with its
own transport built by `newProxyTransport` — `Proxy: nil`, HTTP/1 only, because
targets are always plain `http` (`parseTargetURL` supplies the scheme and
`hostRegex` rejects any other). `rewrite` sets the forwarded headers, preserves
the raw query verbatim (Go's default drops unparseable params), rewrites the
path through `RoutedTargetPath`, and applies the request header rules **last**
so a rule can override the proxy's own `X-Forwarded-*`.

`handleProxyError` maps a failure to a status in a fixed order: a retryable
error is recorded on the attempt and rendered by nobody, then
`MaxBytesError` → 413, a `net.Error` timeout → 504, the request's own deadline
→ 504, client cancellation → 499, draining → 504, a chunked-encoding error →
400, everything else → 502.

`Drain` cancels hijacked requests immediately (they are long-lived), waits out
the rest until the deadline, then cancels what is left.

## Related

- `../resilience/summary.md` — health checks, retries, limits, auth, scale-to-zero
- `../cache/summary.md` — what sits between the checks and the load balancer
- `../certs/summary.md` — what answers `Router.GetCertificate`

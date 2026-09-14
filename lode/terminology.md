# Terminology

The words this repository uses, and what they mean in its code.

## Routing

- **service** — one deployed application, named on the CLI (`deploy <service>`).
  A `*Service` holds its `ServiceOptions`, `TargetOptions`, up to two load
  balancers, and its per-service middleware chain.
- **target** — one backend address the proxy forwards to (`host[:port]`). A
  `*Target` owns a health check and one `httputil.ReverseProxy` handler per
  response timeout in play.
- **write target / read target** — `--target` vs `--read-target`. Reads
  (GET/HEAD) rotate over readers unless the client holds a fresh writer-affinity
  cookie; everything else goes to writers.
- **slot** — `TargetSlotActive` or `TargetSlotRollout`: the two load balancers a
  service may hold at once (`internal/server/service.go`).
- **rollout** — a second target set plus a `RolloutController` that sends a
  percentage (or an allowlist) of requests to it.
- **weight** — `--target 'host;weight=n'`. Weighted pools rotate by nginx's
  smooth weighted round-robin (`internal/server/target_weight.go#nextWeighted`).
- **spec vs name** — a target's *name* is its address; its *spec* is the address
  plus a non-default weight. Persisted state stores specs, listings show names.
- **path prefix** — `--path-prefix`. With `--strip-path-prefix` (default true)
  the matched prefix is removed before forwarding; `RoutedTargetPath` is what
  everything downstream matches on.

## Certificates

- **SAN certificate manager** — `SANCertManager`, the proxy's only ACME system
  when `--acme-email` is set. One account per ACME directory, one cache, one
  allowlist, HTTP-01 and DNS-01.
- **registered domain** — a host named by `deploy --host` on a TLS service. It
  may provision synchronously on a handshake.
- **dynamic domain** — a host learned from a service's `--tls-domains-source`
  poll. It is queued for asynchronous issuance, never issued on the handshake.
- **batch** — the set of domains put into one ACME order. Bounded by
  `MaxSANsPerCertificate` (100) for handshake batches and by
  `--tls-domains-batch-size` (max `MaxTLSDomainsBatchSize`, 25) for dynamic ones.
- **batch-mate** — a pending domain that joins another domain's order because it
  shares that domain's service and ACME directory.
- **pre-flight probe** — an HTTP request the proxy makes to a domain for
  `/.kamal-proxy/preflight/<per-boot nonce>`, proving the name routes back here
  before an order is spent (`DynamicDomainManager#preflightProbe`).
- **quarantine** — a per-domain hold with an escalating backoff ladder, kinded
  `preflight`, `acme` or `rate_limited` (`internal/server/domain_quarantine.go`).
  Only the first two are lifted early by the release prober.
- **release prober** — the sweep that re-probes held domains and lifts holds the
  probe contradicts, so a DNS cutover costs a probe interval, not a ladder step.
- **directory** — an ACME directory URL. Run-level (`--acme-directory`) or
  per-service (`deploy --tls-staging`); a certificate records the one that
  issued it, and a service moving between them forces a replacement.
- **ARI `replaces`** — the RFC 9773 renewal-information marker. It rides at most
  one renewal order, and only one issued at the certificate's recorded directory.
- **compaction window** — how close to expiry a renewal may keep deferring while
  members are held. Deploy-registered coverage compacts a week earlier.
- **shrink guard** — the rule that a poll removing more than 30% of a service's
  applied domains is held until three consecutive polls confirm it.

## Cache

- **store** — where cached responses live: `memory`, `file://<dir>`, or a
  `redis://`/`rediss://` URL shared by a fleet (`NewCacheStore`).
- **lease** — a shared store's claim that one proxy is fetching a key, so a
  lifetime rolling over costs the origin one request for the fleet.
- **inflight group** — the in-process single flight. The lease is the same idea
  one level up, and only a node's inflight leader ever touches it.
- **variant index** — a record at a resource's own key naming the fields it
  varies on; the response itself lives at the key those values produce.
- **refusal** — a low-cardinality reason a response was not stored, surfaced as
  `cache_refusals_total{reason=…}` and as operator advice.

## Process and state

- **data directory** — `--data-dir`, else `$HOME/.config/dash-proxy`. Holds
  `dash-proxy.state`, `acme.state`, `dynamic-domains.state`,
  `dynamic-redirects.state` and `certs/`.
- **drain** — close the public listeners, finish in-flight requests up to a
  timeout, save state, exit. `SIGTERM` and `dash-proxy drain` converge on it.
- **hold** (the command) — `dash-proxy hold`, a hidden command that blocks on a
  signal so a container can own a shared network namespace. Unrelated to a
  quarantine hold.
- **idle controller** — the scale-to-zero state machine per service: active →
  sleeping → waking, stopping containers after `--sleep-after` and starting them
  on the next request.
- **refresh nudge** — an authenticated `POST` that makes a poller re-poll now.
  The poll stays the source of truth; the nudge only removes latency.

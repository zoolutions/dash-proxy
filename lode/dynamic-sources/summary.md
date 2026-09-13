# Dynamic sources: domains and redirects

Two subsystems that poll an application endpoint and turn the answer into proxy
behaviour: `--tls-domains-source` feeds tenant hostnames into certificate
issuance, `--redirects-source` feeds a host-scoped redirect map into the request
path. They are deliberately separate managers with different responsibilities
and different persisted schemas; what they share is `sourcePoller` and
`refreshNudge`.

## sourcePoller

`internal/server/source_poller.go`. One poller per service per subsystem. It
owns the schedule, the conditional-request state and the transport; the payload
semantics live entirely in the `OnBody` callback.

- **Endpoint** — a `Source` starting with `/` is path mode, resolved at poll
  time against one healthy target of the service (`endpointFor`), with the
  service's first configured host sent as `Host`. A path-mode poller with no
  resolver fails the poll rather than panicking in the poll goroutine. Anything
  else is used as an absolute URL.
- **Transport** — path mode gets `&http.Transport{Proxy: nil}`: an `HTTP_PROXY`
  variable must neither intercept a request to one specific container nor see
  the bearer token on that plain-HTTP leg. Absolute URLs keep the default
  transport, where proxy variables are intentional.
- **Schedule** — an immediate poll on `Start`, then `jitteredInterval` (±10%) so
  a fleet does not thundering-herd the app. `Refresh` collapses onto a
  single-slot channel.
- **ETag** — `If-None-Match` from the last response. The new ETag is stored
  **before** `OnBody` so the parser's consumers can read it, and rolled back
  when `OnBody` returns an error, so a broken payload keeps being fetched
  instead of 304ing until the content next changes.
- **Failure reporting** — `OnPollError` fires for an unresolvable endpoint, a
  transport error, an unexpected status, and an unreadable gzip body. A 304 is a
  healthy poll and is not counted.

## refreshNudge

`internal/server/refresh_nudge.go`, served by both subsystems so an ordering or
status fix cannot drift between them. `POST /.kamal-proxy/domains/refresh` and
`POST /.kamal-proxy/redirects/refresh`, in this order:

1. **Hidden first, whatever the method** — no token configured, or no sources —
   is a 404 for every method, because a 405 would reveal the endpoint.
2. Non-POST → 405 with `Allow: POST`.
3. Bearer token compared with `tokensEqual` (constant time, over SHA-256
   digests so length does not leak) against `KAMAL_PROXY_REFRESH_TOKEN`.
4. One nudge per `refreshMinInterval` (10s) → 429 with `Retry-After`.
5. 202 with an empty body. The count of sources nudged is logged, not
   returned — the caller has nothing to act on either way.

The nudge carries no data: the poll stays the single source of truth, replays
are harmless, and it works from any host.

## Dynamic domains

`DynamicDomainManager` (`internal/server/dynamic_domains.go`) owns the pollers,
`domainIssuer`, `domainQuarantine`, `certRenewer` and `releaseProber`, plus
`dynamic-domains.state`.

Payload: `{"domains": ["tenant.example.com", …]}`, capped by `parseDomainList`
at `maxDomainListBody` (1 MB) and `maxDomainListEntries` (10 000) — either
exceeded rejects the whole payload. Entries are lowercased, trimmed and
de-dotted, then de-duplicated; wildcards are skipped (they need DNS-01 and have
no name to answer on) and anything failing the RFC 1123 grammar in
`validDynamicDomain` is skipped with a warning. A malformed payload is an
error, so the last good set keeps serving.

`applyDomains` (`internal/server/dynamic_domains.go:429-507`) is the whole
reconcile:

- A poll for a service the router no longer knows, or whose poller entry is
  gone, is dropped.
- **Shrink guard** — removals above `shrinkGuardThreshold` (30% of the applied
  set) are held until `shrinkGuardConfirmations` (3) consecutive over-threshold
  polls. While held, the applied set is the previous set plus whatever the poll
  added, and the poller's ETag is cleared (`SeedETag("")`) — otherwise an
  unchanged source answers 304 forever and the confirmation count can never
  advance. So a single or transient empty or truncated response evicts nothing;
  three consecutive confirming polls do evict.
- Issuance follows the **polled** list, not the held union: a name the source
  stopped reporting keeps its allowlist entry and any live certificate but earns
  no new orders while its removal is in question.
- `ServiceDeployed` clears the service's hold, so confirmations counted against
  a replaced source never carry over.

`Retry` (the CLI's `domains retry`) is the operator's blunt escape hatch: unlike
the release prober it also clears rate-limit holds.

Boot order matters: `loadState` serves the last-known domain sets before the app
is up, `ServiceDeployed` serves the persisted set immediately and lets the first
poll reconcile.

## Dynamic redirects

`DynamicRedirectManager` (`internal/server/dynamic_redirects.go`) and the map
itself (`internal/server/redirect_map.go`). Unlike the domain manager this does
not need ACME — redirects are useful on a plain HTTP proxy — so `run.go` always
builds it.

Payload: `{"hosts": {"<host>": {redirect_to, status, preserve_path,
trailing_slash, paths:[{from,to,status}]}}}`, capped by `parseRedirectPayload` at
`maxRedirectListBody` (10 MB), `maxRedirectHosts` (100 000) and
`maxRedirectPathRules` (100 000). A document with **no `hosts` key at all** is an error, so a
half-deployed app answering with the wrong document cannot wipe live redirects;
an explicit `{"hosts": {}}` is the sanctioned way to clear. A parseable payload
whose every entry fails validation is also refused.

`compileRedirectMap` walks host keys in sorted order and skips normalized
duplicates with a warning, so a collision resolves identically on every proxy in
a fleet, and counts rules only from the retained entry. One tenant's broken
regex is skipped rather than failing the payload. Patterns are compiled once and
the compiled rule retained. `normalizeRedirectHost` is applied both at compile
time and at lookup, so `Old.Example.COM.` meets its entry either way.

`applyPayload` checks that the poller delivering the body is still the
service's registered poller before installing anything, so an in-flight poll
from a previous deployment cannot apply stale redirects. The compiled map is
swapped into the service through an `atomic.Pointer`, so matching on the request
path takes no lock. `Service.initialize` stores nil whenever `RedirectsSource`
is empty, so removing the source cannot leave stale redirects serving until the
manager catches up.

`Router.DeployService` calls `dynamicRedirectManager.ServiceDeployed`
immediately after `installLoadBalancer` and **before** draining the old
targets — new traffic must see the new source's redirects as soon as the load
balancer is installed, not after a full drain timeout. The domain manager is
reconciled after the drain; that is a separate subsystem decision.

`PublishMetrics` exists because maps restored from state are installed before
`metrics.Enable()` runs; `run.go` calls it once the server is up, or a proxy
serving only persisted redirects would report no map at all.

## Persistence

`dynamic-domains.state` (domain sets, ETags, fetch times, quarantine records)
and `dynamic-redirects.state` (the raw host map as the app sent it, its ETag,
fetch time and source identity) are separate schemas with separate load/save
code, and they do not even share a staging helper: the redirect state goes
through `writeFileAtomic`, the domain state through its own `os.WriteFile` to
`<path>.tmp` plus `os.Rename`. Both save paths hold a dedicated save
lock across snapshot, marshal and rename, because concurrent polls share one
temp file path. Runtime-only data (the `Host` header for path-mode polls) is
cached in memory; source identity lives in persisted state so a changed source
resets its ETag before the first new poll while the persisted map keeps serving.

## Related

- `../certs/summary.md` — what the domain sets feed
- `../request-path/summary.md` — where the redirect map is consulted
- `../review/dynamic-domains.md`, `../review/dynamic-redirects.md`

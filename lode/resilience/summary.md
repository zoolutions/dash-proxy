# Resilience: health, retries, limits, access control, scale-to-zero

The per-service machinery that decides whether a request is served at all, and
which target serves it. Everything here runs inside
`Service.serviceRequestWithTarget` or below the load balancer; the order of the
checks is in `../request-path/summary.md` and every position is load-bearing.

## Health checks

`HealthCheck` (`internal/server/health_check.go`) probes one target on
`--health-check-path` and reports to a `HealthCheckConsumer`. Two regimes:

- **Pre-healthy** — nothing is routed to the target and a probe is cheap, so the
  delay doubles from `initialHealthCheckDelay` (50ms) but never past
  `maxPreHealthyDelay` (2s). Without the cap a 20s configured interval let the
  gap grow to 12.75s and then 25.55s, so a target ready at 13s was not noticed
  until 25.55s.
- **Healthy** — the configured interval governs.

`preHealthyFastWindow` (60s) bounds how long the ceiling applies. A normal
deploy disposes a target that misses its deploy timeout, but `deploy --force`
skips that wait, and a target that never comes up must not be probed at boot
cadence forever; past the window the backoff resumes doubling toward the
configured interval.

A single-target pool stops health-checking once its target is healthy — taking
it out of the pool would not help — **except** under `persistentHealthChecks`,
set by `RecheckHealth`, which is how `--recheck-targets-on-restore` keeps a
restored target under observation.

## Retries

`RetryPolicy` (`internal/server/retry.go`) is `TargetTryDuration` plus
`TargetTryInterval` (`DefaultTargetTryInterval`, 250ms, when unset). A zero
duration means one attempt — exactly how the proxy behaves with no retries
configured, so an untouched deployment is unaffected. `--target-try-interval`
without `--target-try-duration` is a validation error rather than a silently
ignored flag.

With a policy, `LoadBalancer.StartRequest` defers target selection entirely into
`serveWithRetries`, so each attempt picks a currently-healthy target rather than
retrying into the one that just failed. `Target.handleProxyError` (472-517)
records a retryable error on the attempt and renders nothing — no bytes may
reach the client before the retry.

`WithRetryPolicy` and `WithSessionAffinity` are chained onto `NewLoadBalancer`
rather than folded into its signature, so its call sites stay uniform.

## Limits and access control

All three per-client decisions embed one `forwardedResolver`
(`internal/server/ip_allow_list.go`), so the trust rules have exactly one
implementation. Three bypasses closed there — repeated header lines,
IPv4-mapped IPv6, unresolvable chains — would otherwise have to be fixed once
per caller, and the one that got missed would be the vulnerable one.

The rule: with no trusted proxies declared, the client is always the peer.
Otherwise, when the peer is one of ours, the client is taken from the forwarded
chain by walking it from the nearest hop backwards past any other proxy of ours.
`X-Forwarded-For`, `X-Real-IP` and whatever `--client-ip-header` names are
written by the client and are consulted only when the peer is a declared proxy.
Under `--proxy-protocol` the peer itself is rewritten from the PROXY preamble,
and is then only as trustworthy as `--proxy-protocol-allow-ip` makes it.

| Thing | Zero address | Because |
|---|---|---|
| `ipAllowList` | never permitted, not even by a default route | an unparseable peer is not a reason to admit |
| `denyList` | matches nothing | each fails in the direction it exists for |
| `rateLimiter` | shares the overflow bucket | a cap, not a refusal |

`denyList` (`internal/server/deny_list.go`) is `--deny-ip` and
`--deny-user-agent`. `emptyUserAgentPattern` (`^$`) is the one pattern an absent
User-Agent matches: every other rule, including `.*`, requires an agent to
actually be present, because half the non-browser HTTP clients in the world send
none and absence is not a crime. Deny runs before allow, so an address on both
lists is denied.

`rateLimiter` (`internal/server/rate_limit.go`) counts IPv6 clients at a `/64`
(`rateLimitIPv6PrefixBits`): keying on the full `/128` would make the limit
decorative, since every IPv6 client holds at least a `/64` and can pick a fresh
source address per request — which both escapes the budget and allocates a
bucket each time. `rateLimitMaxTrackedClients` (50 000) bounds the bucket map,
otherwise a memory-exhaustion primitive handed to anyone who can vary a source
address; clients past the cap share an overflow bucket. Sweeping is lazy
(`rateLimitSweepInterval`, 1 minute) rather than run from a goroutine, so a
redeploy cannot leak one. Throttle logging is burst-limited per service
(`rateLimitLogBurst` 10, `rateLimitLogInterval` 10s).

`basicAuthCredential` (`internal/server/basic_auth.go`) stores a `sha256`
digest and a 16-byte salt, deliberately **not** an adaptive hash: verification
runs on the *unauthenticated* request path, so a slow one hands an uncacheable
CPU-exhaustion primitive to exactly the people the password is there to keep
out. The salt defeats precomputation against a leaked state file; it does not
defend a weak password against an offline dictionary attack. A credential that
could not be read back from saved state sets `denyAll`, and that service refuses
every request rather than serving unprotected.

## Pause, stop, rollout, affinity

`PauseController` holds requests for up to a timeout (`pause`) or answers them
with a message (`stop`). Health checks still answer 200 while paused, so a
downstream load balancer does not drop the proxy itself. The pause wait happens
inside the closure `StartRequest` returns, after the service lock is released.

`RolloutController` (`internal/server/rollout_controller.go`) sends a percentage
— or an explicit allowlist, matched on the `kamal-rollout` cookie — to the
`TargetSlotRollout` load balancer.

`SessionAffinityPolicy` (`internal/server/session_affinity.go`) pins a client to
the target that first served it, for apps whose session state lives in the
instance rather than a shared store. The cookie is
`DefaultSessionAffinityCookieName` (`kamal-session`) unless the deployment names
one. Disabled is the default, and leaves target selection exactly as it is
without it.

## Path-scoped timeouts

`PathTimeout` (`internal/server/path_timeouts.go`) overrides a timeout below a
path prefix. A **zero** `Timeout` disables the timeout for that prefix rather
than falling back to the service-wide value — the distinction an upload or an
SSE endpoint needs. `NormalizePathTimeouts` canonicalizes prefixes the way
service path prefixes are canonicalized and orders them longest-first, so
`ResolvePathTimeout` returns the most specific match without sorting on every
request. `Target` builds one `httputil.ReverseProxy` per distinct response
timeout in play, so an override costs no per-request work.

## Scale to zero

`IdleController` (`internal/server/idle_controller.go`) is a four-state machine
per service: `IdleStateActive`, `IdleStateStopping`, `IdleStateSleeping`,
`IdleStateWaking`. It stops the containers after `--sleep-after` and starts them
on the next request that needs them, within `--wake-timeout`.

Everything it needs from its `Service` arrives as a hook — `Suspend`, `Resume`,
`Persist`, plus a `ContainerLifecycle` — so the controller never touches a load
balancer, a target, or their locks.

State is persisted as a **name**, not an enum ordinal. `ParseIdleState` folds
`stopping` and `waking` down to `sleeping` in both directions: a proxy that died
mid-transition cannot know whether the container moved, and waking from sleeping
is the safe assumption because starting an already-running container succeeds.
Anything unrecognised — including the empty string every pre-feature state file
carries — restores `active`.

`Service.UnmarshalJSON` (578-630) records the restored state rather than acting
on it: the lifecycle is still nil at decode time, so a controller built there
could reach `StopContainer` on a nil interface. `SetContainerLifecycle` creates
it, after the router has decoded the whole state file.

`ContainerLifecycle` (`internal/server/container_lifecycle.go`) is the seam. The
shipped implementation talks to the Docker socket directly — the smallest
opt-in approach, and also root-equivalent access to the host; a restricted
host-side start/stop service can replace it without the controller changing.

## Related

- `../request-path/summary.md` — where each of these sits in the per-request order
- `../cache/summary.md` — what sits between the checks and the load balancer
- `../observability/summary.md` — what these emit

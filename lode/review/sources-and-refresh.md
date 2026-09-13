The two pieces both dynamic subsystems share: the poller that fetches an
application endpoint, and the endpoint that asks a poller to run now.

### Path-mode polls use a `Proxy: nil` transport; absolute-URL sources keep the default
- **Holds because:** a path-mode source resolves to one specific container on the internal network, over plain HTTP, carrying a bearer token. An `HTTP_PROXY` variable in the proxy's environment must neither intercept that request nor observe the token on that leg. An absolute URL is a different intent — the operator named an external endpoint, and proxy variables apply to it deliberately.
- **Where:** `internal/server/source_poller.go`
- **Proven by:** `TestDynamicRedirectManager_DeployedServicePollsAndApplies`, `TestDynamicDomainManager_DeployedServicePollsAndIssues`
- **Origin:** cubic learning 35ea345c; PR #92 review thread

### A path-mode poller with no endpoint resolver fails the poll rather than panicking
- **Holds because:** the poll runs on its own goroutine, so a nil dereference there takes the process down with a stack that names nothing useful about which service was misconfigured. The base URL's trailing slash is trimmed before `Source` is appended, so a resolver answering `http://host/` does not produce `//path`.
- **Where:** `internal/server/source_poller.go#endpoint` (240-257: the nil check and the trim); the resolver itself is each manager's `endpointFor`
- **Proven by:** `TestDynamicRedirectManager_PollFailuresAreCounted`
- **Origin:** cubic learning 0f7fa0f8

### The new ETag is stored before `OnBody` runs, and rolled back if `OnBody` errors
- **Holds because:** the parser's consumers read the ETag during the apply, so storing it afterwards hands them the previous one. But a body that fails to parse must keep being fetched — holding the new ETag would 304 every subsequent poll until the *content* next changes, so a broken payload could persist for as long as the application kept serving it unchanged.
- **Where:** `internal/server/source_poller.go`
- **Proven by:** `TestDynamicRedirectManager_KeepsLastGoodOnInvalidPayload`, `TestDynamicRedirectManager_SourceChangeDropsETag`
- **Origin:** verified from the code during seeding

### Poll failures are counted as `outcome="error"`; a 304 is a healthy poll and is not counted
- **Holds because:** the four ways a poll can fail — an unresolvable endpoint, a transport error, an unexpected status, an unreadable (gzip) body — are all "the source did not answer", and an operator watching the metric needs them together. A 304 *is* the source answering, so counting it as an error would make a correctly-behaving conditional source look permanently broken.
- **Where:** `internal/server/source_poller.go` (`OnPollError`), `internal/metrics/metrics.go` (`dynamic_redirect_polls_total`)
- **Proven by:** `TestDynamicRedirectManager_PollFailuresAreCounted`
- **Origin:** cubic learning 4576a7a6

### Polls are scheduled with a ±10% jitter, and `Refresh` collapses onto a single slot
- **Holds because:** a fleet of proxies deployed together would otherwise poll the same application endpoint in lockstep forever. The refresh channel is single-slot so a burst of nudges costs one extra poll, not one per nudge.
- **Where:** `internal/server/source_poller.go#jitteredInterval`, `#Refresh`
- **Proven by:** `TestDynamicDomainManager_RefreshEndpoint`, `TestDynamicRedirectManager_RefreshEndpoint`
- **Origin:** verified from the code during seeding

### Both refresh endpoints serve through one `refreshNudge`, in a fixed order
- **Holds because:** two copies of an ordering rule is two places for it to drift, and the ordering is the security property. `internal/server/refresh_nudge.go#serve`:
  1. **hidden first, whatever the method** — no token or no sources is a 404 for every method, because a 405 would reveal that the endpoint exists
  2. non-POST → 405 with `Allow: POST` set before the write
  3. bearer token via `tokensEqual`
  4. one nudge per `refreshMinInterval` (10s) → 429 with `Retry-After`
  5. 202 with an empty body
- **Where:** `internal/server/refresh_nudge.go#serve` (35-66)
- **Proven by:** `TestDynamicDomainManager_RefreshEndpoint`, `TestDynamicDomainManager_RefreshEndpointDisabledWithoutToken`, `TestDynamicRedirectManager_RefreshEndpoint`, `TestDynamicRedirectManager_RefreshEndpointHiddenWithoutToken`
- **Origin:** cubic learnings 98ed443a, 6bb13b5e, a549c218

### Token comparison is constant time over SHA-256 digests, so length does not leak either
- **Holds because:** `subtle.ConstantTimeCompare` on the raw strings returns early on a length mismatch, which leaks the token's length to a timing attacker. Hashing first makes both operands 32 bytes.
- **Where:** `internal/server/refresh_nudge.go#tokensEqual` (74-78)
- **Proven by:** `TestDynamicDomainManager_RefreshEndpoint`, `TestDynamicRedirectManager_RefreshEndpoint`
- **Origin:** verified from the code during seeding

### The nudge carries no data — the poll stays the source of truth
- **Holds because:** it means a replayed nudge is harmless, a lost one costs only latency, and it works from any host. Accepting a payload would make the endpoint a second, unauthenticated-in-substance way to set proxy state.
- **Where:** `internal/server/refresh_nudge.go#serve`
- **Proven by:** `TestDynamicDomainManager_RefreshEndpoint`, `TestDynamicRedirectManager_RefreshEndpoint`
- **Origin:** verified from the code during seeding

### The two tokens have different jobs, and the help text says which is which
- **Holds because:** `KAMAL_PROXY_REDIRECTS_TOKEN` is sent *by* the proxy when it polls the application; `KAMAL_PROXY_REFRESH_TOKEN` is what a caller must present *to* the proxy to nudge it. Both are read from the running proxy's environment, which is the part an operator gets wrong — setting one in the application's environment and wondering why the poll is unauthorized.
- **Where:** `internal/cmd/deploy.go` (`--redirects-source` help), `internal/server/refresh_nudge.go`
- **Proven by:** `TestServiceOptions_ValidateDynamicRedirects`
- **Origin:** cubic learning cc5a91bf

### An absolute `--redirects-source` is validated at deploy time for scheme and host
- **Holds because:** a typo in a source URL otherwise fails silently, once per interval, on a background goroutine — the deploy succeeds and the feature never works. The parse requires `http` or `https` and a non-empty hostname.
- **Where:** `internal/server/service_options_validation.go#validateDynamicRedirects` (23-46)
- **Proven by:** `TestServiceOptions_ValidateDynamicRedirects`
- **Origin:** cubic learning 85646590

### Not a bug: a rejected refresh logs at Warn, and the rate-limit slot is claimed after authentication
- **Holds because:** every request to the path already writes an access-log line through the logging middleware, so the extra Warn does not change attacker-controlled log volume — it is one line on top of one written regardless. Downgrading to Debug would hide the common real cause, a token mismatch between app and proxy, unless `--debug` is on. And claiming the rate-limit slot *before* authenticating would let an unauthenticated client exhaust the one refresh per 10s that legitimate callers need.
- **Where:** `internal/server/refresh_nudge.go#serve`
- **Proven by:** `TestDynamicDomainManager_RefreshEndpoint`, `TestDynamicRedirectManager_RefreshEndpoint`
- **Origin:** PR #92 review thread ("Not changed, with reasoning")

## Related

- `dynamic-domains.md`, `dynamic-redirects.md` — the two subsystems
- `../dynamic-sources/summary.md` — both, as they stand

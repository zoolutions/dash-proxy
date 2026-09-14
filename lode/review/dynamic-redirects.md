A host-scoped redirect map polled from an application. Same poller as the domain
source, entirely different persisted schema and failure policy — a redirect map
is served on the request path, so correctness rules here are about matching and
about what a bad deployment can do to live traffic.

### A payload with no `hosts` key at all is an error; an explicit `{"hosts": {}}` clears
- **Holds because:** a half-deployed application answering with the wrong document — an index page, an error body, a different endpoint's JSON — decodes to a zero-value payload, and treating that as "no redirects" wipes every live redirect. Requiring the key present makes clearing an explicit act. A parseable payload whose every entry fails validation is refused for the same reason.
- **Where:** `internal/server/redirect_map.go#parseRedirectPayload`, `internal/server/dynamic_redirects.go#applyPayload`
- **Proven by:** `TestDynamicRedirectManager_KeepsLastGoodOnInvalidPayload`, `TestDynamicRedirectManager_ExplicitEmptyPayloadClearsRedirects`, `TestParseRedirectPayload`
- **Origin:** cubic learning 2802d104

### An explicit empty map with an ETag is valid cleared state, preserved across a 304 and a restart
- **Holds because:** "cleared" and "never loaded" look identical in a naive schema, so a restart after a deliberate clear would resurrect the previous map from state — or refetch and clear again, depending on the ETag. The persisted state distinguishes them, and a 304 preserves the cleared map rather than discarding it.
- **Where:** `internal/server/dynamic_redirect_state.go`, `internal/server/dynamic_redirects.go#applyPayload`
- **Proven by:** `TestDynamicRedirectManager_StateSurvivesRestart`, `TestDynamicRedirectManager_ExplicitEmptyPayloadClearsRedirects`
- **Origin:** cubic learning 2802d104

### A payload is applied only if the poller that delivered it is still the service's registered poller
- **Holds because:** a redeploy that changes `--redirects-source` leaves the old poller's in-flight request outstanding. Its body arrives after the new source is installed and overwrites it with the previous deployment's map. Each `OnBody` callback is bound to its originating poller and `applyPayload` checks identity before installing anything.
- **Where:** `internal/server/dynamic_redirects.go#applyPayload`
- **Proven by:** `TestDynamicRedirectManager_SupersededPollerCannotApply`
- **Origin:** cubic learning ff82e9f6

### A changed source records the new identity and clears its ETag before polling, while the persisted map keeps serving
- **Holds because:** the ETag belongs to the *old* endpoint. Sending it to a new one either gets a meaningless 304 or matches by coincidence, and either way the new source's first answer is never applied. Source identity lives in persisted state precisely so the comparison survives a restart. The old map keeps serving until the first new-source poll lands, so changing a source is not an outage.
- **Where:** `internal/server/dynamic_redirect_state.go`, `internal/server/dynamic_redirects.go`
- **Proven by:** `TestDynamicRedirectManager_SourceChangeDropsETag`
- **Origin:** cubic learnings 007cfef5, 967954fb

### `compileRedirectMap` walks host keys in sorted order and skips normalized duplicates with a warning
- **Holds because:** Go map iteration is randomized, so `Old.Example.com` and `old.example.com.` colliding after normalization would resolve to a different winner on each proxy in a fleet — and on each restart. Sorting makes the winner deterministic everywhere. Rules are counted only from the retained entry, so the reported rule count matches the map that is actually installed.
- **Where:** `internal/server/redirect_map.go#compileRedirectMap`, `#normalizeRedirectHost`
- **Proven by:** `TestCompileRedirectMap_NormalizationCollisionsAreDeterministic`, `TestCompileRedirectMap_SkipsInvalidEntries`
- **Origin:** cubic learning e95966e2

### Request-time lookup applies the same `normalizeRedirectHost` that compilation applies
- **Holds because:** a host arrives from the client with whatever case and trailing dot it likes. Normalizing only at compile time means `Old.Example.COM.` never meets the entry written for it — the rule silently does nothing, which is the failure nobody reports as a bug.
- **Where:** `internal/server/redirect_map.go#normalizeRedirectHost`, used at compile and at lookup
- **Proven by:** `TestDynamicRedirectMap_HostRedirect`, `TestCompileRedirectMap_NormalizationCollisionsAreDeterministic`
- **Origin:** cubic learning 88204e7a

### Exempt paths are checked before both rule sources
- **Holds because:** `redirectExemptPrefixes` protects `/.well-known/acme-challenge/` (issuance) and `/.kamal-proxy/` (ping, pre-flight, refresh nudges). A tenant redirect rule that shadowed either would stop certificates issuing or make the proxy's own endpoints unreachable on that host, and the tenant who wrote the rule would never see why. The check runs ahead of the dynamic map *and* the static `--redirect` rules; the TLS and canonical-host hop still applies to exempt paths.
- **Where:** `internal/server/service.go#redirectURLIfNeeded` (992-1043); `internal/server/redirect_map.go#isRedirectExemptPath`
- **Proven by:** `TestIsRedirectExemptPath`, `TestRouter_RedirectRulesNeverShadowInternalPaths`
- **Origin:** cubic learning ba5d6cf1

### `preserve_path` carries the request's escaped path, not its decoded one
- **Holds because:** `URL.Path` has already decoded `%2F`, so rebuilding the `Location` from it turns an encoded slash into a real path separator and sends the client somewhere else. `RawPath`/`EscapedPath` preserve what the client actually sent.
- **Where:** `internal/server/redirect_map.go` (host redirect construction)
- **Proven by:** `TestDynamicRedirectMap_PreservePathKeepsEncodedSlashes`, `TestDynamicRedirectMap_PathRules`, `TestDynamicRedirectMap_TrailingSlash`
- **Origin:** cubic learning a925a3b5

### An absolute `redirect_to` needs a non-empty *hostname*, not merely a non-empty authority
- **Holds because:** `http://:8080/` parses with a non-empty raw authority and an empty host — a `Location` no client can follow. The check is on the parsed hostname.
- **Where:** `internal/server/redirect_map.go#compileHostRedirect`
- **Proven by:** `TestCompileRedirectMap_SkipsInvalidEntries`
- **Origin:** cubic learning 6a3f1a38

### The redirect-loop guard compares URLs as a request would
- **Holds because:** a rule sending `/` to `` (or `http://Example.com/` to `http://example.com/`) is a loop the client will follow forever, but a byte comparison calls them different. Empty path and `/` are equal, scheme and host compare case-insensitively, and the query must match too — two URLs differing only in query are genuinely different destinations.
- **Where:** `internal/server/redirect_map.go` (loop guard)
- **Proven by:** `TestDynamicRedirectMap_HostRedirect`, `TestCompileRedirectMap_SkipsInvalidEntries`
- **Origin:** cubic learning efaecd01

### Each path pattern is compiled once and the compiled rule retained
- **Holds because:** validating through `newPathRuleSet` and then recompiling for use pays regex compilation twice per rule, on a map that may carry up to `maxRedirectPathRules` (100 000) of them, every poll. The compiled rule is what is kept.
- **Where:** `internal/server/redirect_map.go#compileHostRedirect`
- **Proven by:** `TestDynamicRedirectMap_PathRules`, `TestParseRedirectPayload_RejectsTooManyRules`
- **Origin:** cubic learning 0e0270b3

### One tenant's broken regex is skipped, not fatal to the payload
- **Holds because:** the map is multi-tenant. A single bad pattern failing the whole document would let one tenant's typo disable every other tenant's redirects.
- **Where:** `internal/server/redirect_map.go#compileRedirectMap`
- **Proven by:** `TestCompileRedirectMap_SkipsInvalidEntries`
- **Origin:** verified from the code during seeding

### `Router.DeployService` reconciles redirects immediately after `installLoadBalancer`, before draining old targets
- **Holds because:** the moment the new load balancer is installed, new traffic is being served by the new deployment — and must see the new source's redirects. Waiting until after a drain that can run to the full drain timeout serves the previous deployment's redirects to the new deployment's traffic. The domain manager is reconciled *after* the drain; that is a separate subsystem decision, not an inconsistency.
- **Where:** `internal/server/router.go#DeployService`
- **Proven by:** `TestDynamicRedirectManager_DeployedServicePollsAndApplies`, `TestDynamicRedirectManager_EndToEndThroughRouter`, `TestRouter_DynamicRedirectsAnswerRequests`
- **Origin:** cubic learning e7b34a30

### `Service.initialize` stores nil whenever `RedirectsSource` is empty
- **Holds because:** removing the source is an instruction to stop redirecting, and waiting for the manager to catch up leaves the old compiled map serving on the request path in the meantime. Clearing at initialize makes the redeploy itself the moment it stops.
- **Where:** `internal/server/service.go#initialize`
- **Proven by:** `TestService_RedeployWithoutSourceClearsDynamicRedirects`, `TestDynamicRedirectManager_RedeployWithoutSourceEvictsRedirects`, `TestDynamicRedirectManager_ServiceRemovedEvictsRedirects`
- **Origin:** cubic learning 4f83fc43

### `PublishMetrics` is called once after server startup, because restored maps are installed before `metrics.Enable()`
- **Holds because:** the gauges are set when a map is installed, and a map restored from state is installed during boot, before the real tracker exists — so the emission goes to the no-op delegate. A proxy serving only persisted redirects would then report no map at all. The counts are cached and republished once `run.go` has the server up.
- **Where:** `internal/server/dynamic_redirects.go#PublishMetrics`, called from `internal/cmd/run.go`
- **Proven by:** `TestDynamicRedirectManager_StateSurvivesRestart`
- **Origin:** cubic learning 3cc25f3d

### The redirect save path holds a dedicated save lock across snapshot, marshal and rename
- **Holds because:** concurrent polls for different services share one temp file path, so two interleaved saves write each other's bytes. The lock is separate from the map lock so a save never blocks a lookup.
- **Where:** `internal/server/dynamic_redirect_state.go`
- **Proven by:** `TestDynamicRedirectManager_StateSurvivesRestart`
- **Origin:** cubic learning 540e5b33

### The two dynamic subsystems stay separate managers with separate schemas
- **Holds because:** they persist different shapes — domains carry quarantine records and issuance state, redirects carry raw host maps and source identity — and they are wired at different points of the deploy flow. A generic persisted-state helper would abstract about forty lines of glue while coupling two subsystems' schemas to each other, and a shared `ServiceLifecycleManager` interface threaded through the router buys less than the two near-identical setters cost. What *is* shared is the part that must not drift: `sourcePoller` and `refreshNudge`. The persistence is not even shared at the primitive level — redirects go through `writeFileAtomic`, domains through their own `os.WriteFile`-to-`.tmp` plus `os.Rename` — so a change to one staging path does not silently change the other's durability. A third manager would tip the balance.
- **Where:** `internal/server/dynamic_domains.go`, `internal/server/dynamic_redirects.go`, `internal/server/router.go#DeployService`
- **Proven by:** structural; no single test
- **Origin:** cubic learnings 1aa22802, 2176ec95, 967954fb; PR #92 review threads (declined with reasoning)

### Not a bug: a superseded poller's ETag is not rolled back when its service is gone
- **Holds because:** when the resolver no longer knows the service, the poller is orphaned and about to be stopped — its in-memory ETag dies with it, and the persisted ETag lives in `states[service]`, which `ServiceRemoved` has already deleted. Rolling back an ETag on a dying poller has no observable effect. The race that *was* real — a **replaced** poller applying — is closed by the poller-identity check instead.
- **Where:** `internal/server/dynamic_redirects.go#applyPayload`, `#ServiceRemoved`
- **Proven by:** `TestDynamicRedirectManager_ServiceRemovedEvictsRedirects`, `TestDynamicRedirectManager_SupersededPollerCannotApply`
- **Origin:** PR #92 review thread ("Not changed, with reasoning")

## Related

- `dynamic-domains.md` — the sibling subsystem
- `sources-and-refresh.md` — the poller and the refresh nudge both serve
- `../dynamic-sources/summary.md` — both subsystems as they stand

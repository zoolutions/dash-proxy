Tenant hostnames polled from an application, and what the proxy refuses to do
with them. The recurring theme: a source that is temporarily wrong must not be
able to take live domains off the air.

### A mass removal is held until three consecutive polls confirm it
- **Holds because:** the source is an application endpoint, and a half-deployed app answering with an empty or truncated list is a routine event. Evicting on it drops every tenant's certificate at once. Removals above `shrinkGuardThreshold` (0.30 of the applied set, `internal/server/dynamic_domains.go:35`) are held until `shrinkGuardConfirmations` (3, line 39) consecutive over-threshold polls. So a single or transient empty poll evicts nothing; three consecutive confirming polls do evict — the guard defers a real shrink, it does not refuse one.
- **Where:** `internal/server/dynamic_domains.go#applyDomains` (429-507)
- **Proven by:** `TestDynamicDomainManager_ShrinkGuardHoldsMassRemovals`, `TestDynamicDomainManager_ShrinkGuardHoldsEmptyPoll`, `TestDynamicDomainManager_ShrinkGuardAppliesSmallRemovals`, `TestDynamicDomainManager_ShrinkGuardConfirmsAfterConsecutivePolls`, `TestDynamicDomainManager_ShrinkGuardCancelsOnRecovery`
- **Origin:** cubic learning 1ec3375d

### While a shrink is held, both the poller's and the persisted ETag are cleared
- **Holds because:** the confirmation count only advances on a poll that *delivers a body*. With `If-None-Match` still set, an unchanged source answers 304 forever, the count never reaches three, and a genuine removal is held permanently. `SeedETag("")` on the poller plus clearing the persisted copy forces a refetch each interval for as long as the hold lasts.
- **Where:** `internal/server/dynamic_domains.go#applyDomains`; `internal/server/source_poller.go#SeedETag`
- **Proven by:** `TestDynamicDomainManager_ShrinkGuardClearsETagWhileHolding`
- **Origin:** cubic learning fe0938a1

### Issuance follows the polled list, not the held union
- **Holds because:** while a removal is in question the applied set is the previous set plus whatever the poll added, so live certificates keep serving. But spending ACME orders on a name the source has stopped reporting is a different decision: the domain keeps its allowlist entry and any certificate it has, and earns no *new* orders until its removal is resolved.
- **Where:** `internal/server/dynamic_domains.go#applyDomains`
- **Proven by:** `TestDynamicDomainManager_ShrinkGuardHoldsMassRemovals`
- **Origin:** cubic learning 1ec3375d

### `ServiceDeployed` clears the service's shrink-hold confirmation state
- **Holds because:** a redeploy may point the service at a different source. Confirmations counted against the old source's answers say nothing about the new one, and carrying them over lets two polls from a replaced source plus one from its successor evict a set nobody confirmed. Stale callbacks from a *replaced poller* are a separate identity race, closed separately — clearing the hold does not address it.
- **Where:** `internal/server/dynamic_domains.go#ServiceDeployed`
- **Proven by:** `TestDynamicDomainManager_ShrinkGuardResetsOnRedeploy`
- **Origin:** cubic learning d33e8005

### A poll for a service the router no longer knows, or whose poller entry is gone, is dropped
- **Holds because:** an in-flight poll outlives the deployment that started it. Applying it re-registers domains for a service that has been removed, and the allowlist entry then outlives everything that could ever clean it up.
- **Where:** `internal/server/dynamic_domains.go#applyDomains`
- **Proven by:** `TestDynamicDomainManager_ServiceRemovedEvictsDomains`, `TestDynamicDomainManager_RedeployWithoutSourceEvictsDomains`
- **Origin:** cubic learning d33e8005

### A malformed payload is an error, so the last good set keeps serving
- **Holds because:** the alternative is that a JSON syntax error in the application's response deletes every tenant. `parseDomainList` returns an error for an oversize body (`maxDomainListBody`, 1 MB) or too many entries (`maxDomainListEntries`, 10 000) and for unparseable JSON; the poller then rolls its ETag back so the broken body keeps being refetched rather than 304ing until the content next changes. Individual bad *entries* are different: a wildcard or a name failing the RFC 1123 grammar is skipped with a warning, because one tenant's typo must not cost the rest.
- **Where:** `internal/server/domain_source.go#parseDomainList`, `#validDynamicDomain`; `internal/server/source_poller.go`
- **Proven by:** `TestDynamicDomainManager_LoadStateSkipsNilEntries`, `TestDynamicDomainManager_DeployedServicePollsAndIssues`
- **Origin:** verified from the code during seeding

### The release prober excludes rate-limit holds and unprobeable domains in `candidates`, not in `probeDomains`
- **Holds because:** `probeDomains` *skips* a domain it cannot speak for and reports no failure for it. A sweep that read "not failed" as "passed" would release every wildcard hold on its first tick — the exact opposite of the quarantine's purpose. Selecting the candidates up front is what makes "no failure reported" mean "the probe succeeded". The prober also records nothing on a failing probe: it only ever lifts holds, never adds to the ladder.
- **Where:** `internal/server/domain_release.go#candidates`, `internal/server/domain_failure.go#probeDomains`
- **Proven by:** `TestDomainQuarantine_ReleaseLiftsTheHoldButKeepsTheLadder`, `TestDomainQuarantine_ReleaseReportsWhetherItHeld`
- **Origin:** verified from the code during seeding

### `Release` keeps the failure count; `Clear` drops it
- **Holds because:** they answer different questions. A probe that now succeeds says the current hold is wrong, not that the domain has stopped flapping — keeping the count means a domain that keeps failing keeps climbing the ladder. A successful issuance, or the domain leaving its source, really does end the history.
- **Where:** `internal/server/domain_quarantine.go#Release`, `#Clear`
- **Proven by:** `TestDomainQuarantine_ReleaseLiftsTheHoldButKeepsTheLadder`, `TestDomainQuarantine_ExpiresAndClears`, `TestDomainQuarantine_ACMEBackoffProgression`, `TestDomainQuarantine_PreflightBackoffStartsGentler`
- **Origin:** verified from the code during seeding

### A quarantine record written before `Kind` existed decodes as `quarantineACME`
- **Holds because:** ACME is the conservative default — it is the kind the release prober will *not* lift early. Decoding an unknown record as `preflight` would have the first sweep after an upgrade release holds that were placed for CA rejections.
- **Where:** `internal/server/domain_quarantine.go`
- **Proven by:** `TestDomainQuarantine_LegacyEntryWithoutKindDecodesAsACME`, `TestDomainQuarantine_KindSurvivesSnapshotRestore`, `TestDomainQuarantine_RecordsTheKindOfHold`, `TestDomainQuarantine_KindReflectsTheMostRecentFailure`
- **Origin:** verified from the code during seeding

### `domains retry` clears rate-limit holds too; the release prober does not
- **Holds because:** the prober is automatic and must never argue with a CA that has dictated an end time. `retry` is an operator typing a command about a situation they have looked at, so it is the blunt escape hatch — it clears every kind.
- **Where:** `internal/server/dynamic_domains.go#Retry`, `internal/cmd/domains.go`
- **Proven by:** `TestDynamicDomainManager_RetryClearsRateLimitHoldsToo`, `TestDynamicDomainManager_RetryClearsAHoldAndResetsTheLadder`, `TestDynamicDomainManager_RetryWithoutADomainClearsEveryHold`, `TestDynamicDomainManager_RetryReportsNothingForAnUnheldDomain`
- **Origin:** verified from the code during seeding

### The pre-flight probe refuses redirects
- **Holds because:** the probe asks whether `/.kamal-proxy/preflight/<per-boot nonce>` is answered *by this proxy*. Following a redirect lets a host that merely points somewhere that redirects here pass a check about where it points.
- **Where:** `internal/server/dynamic_domains.go#preflightProbe`
- **Proven by:** `TestDynamicDomainManager_PreflightProbeRefusesRedirects`, `TestDynamicDomainManager_PreflightProbe`, `TestDynamicDomainManager_PreflightEndpoint`
- **Origin:** verified from the code during seeding

### `domains list` builds each service's held-removal lookup once, before the row loop
- **Holds because:** rebuilding the membership set per row is quadratic in a listing whose whole purpose is a tenant estate that can run to thousands of domains.
- **Where:** `internal/cmd/domains.go#domainsListCommand.run`
- **Proven by:** `TestDomainsListCommand_JSONOutputShape`, `TestSummarizeDomains`
- **Origin:** cubic learning 7c1248c1

## Related

- `dynamic-redirects.md` — the sibling subsystem, and the rules they share
- `../dynamic-sources/summary.md` — both subsystems as they stand
- `certs-issuance.md` — what the domain sets feed

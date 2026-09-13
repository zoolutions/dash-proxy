How a handshake turns into an ACME order: the batch, the probes, the token, and
what a failure does to the domains that were in it.

### A handshake's provisioning single-flight is keyed on the service, never globally
- **Holds because:** batches never span services (the candidate loop in `provisionCertificate` skips any pending domain whose `registeredDomains` owner differs), so a global slot would make one service's handshake block on another service's unrelated order — and then be *satisfied* by it, returning without a certificate for the name that was asked for. The key is `"service:" + owner`; same-service handshakes still coalesce, which is the whole point of the flight.
- **Where:** `internal/server/san_cert_manager.go#provisionCertificate` (525-672)
- **Proven by:** `TestSANCertManager_HandshakeBatchNeverMixesServices`, `TestSANCertManager_HandshakeDoesNotWaitOnAnotherServicesOrder`
- **Origin:** cubic learning b8fc16fa

### The triggering domain is probed before any lock is taken, and it is never dropped from its own batch
- **Holds because:** a domain that cannot answer a pre-flight probe must cost nothing — not an order, not a lock, not a wait. `preflightTrigger` runs first, outside the single flight. The inverse matters as much: `filterBatchMates` drops unreachable *mates*, but the trigger is the name a real client is handshaking for, so removing it would produce a certificate that does not cover the request that paid for it.
- **Where:** `internal/server/san_cert_manager.go#provisionCertificate`, `#filterBatchMates`
- **Proven by:** `TestBatchGuard_UnreachableTriggerIsRefusedWithoutBurningAnOrder`, `TestBatchGuard_TriggerDomainIsNeverDroppedFromItsOwnBatch`, `TestBatchGuard_ReachableTriggerProvisionsDespiteAStaleHold`
- **Origin:** cubic learnings 94f61938, c9fb7a60

### Every non-trigger batch-mate is probed, whatever its certificate history
- **Holds because:** history is not a pass. A host that held a certificate for a year and whose DNS has since moved away will fail its renewal and, riding along in someone else's batch, take that whole order down with it. Each unreachable mate is quarantined after its *first* failed probe, so the cost is paid once per backoff cycle rather than once per handshake.
- **Where:** `internal/server/san_cert_manager.go#filterBatchMates`
- **Proven by:** `TestBatchGuard_ExpiringBatchMateIsProbedAndExcludedWhenUnreachable`, `TestBatchGuard_UnreachableBatchMateIsQuarantinedWithoutBurningAnOrder`
- **Origin:** cubic learning c9fb7a60

### A domain that needs no probe is not probed: wildcards, and names in a zone with a DNS-01 provider
- **Holds because:** the probe answers one question — does this name route back to this proxy — and neither of those needs to. A wildcard has no single name to answer on, and a DNS-01 zone is validated at the registrar, not at the listener. Probing them would quarantine names whose issuance never depended on where they point.
- **Where:** `internal/server/san_cert_manager.go#filterBatchMates`, `internal/server/domain_release.go` (`Unprobeable`)
- **Proven by:** `TestBatchGuard_DNSSolvableBatchMateIsNotProbed`, `TestBatchGuard_DNSSolvableTriggerIsNotProbed`, `TestBatchGuard_HTTPOnlyBatchMateIsStillProbedAlongsideADNSZone`
- **Origin:** cubic learning 94f61938

### Probes are bounded by a concurrency cap and a client timeout, and deliberately ignore the caller's context
- **Holds because:** `probeDomains` runs at most `maxConcurrentProbes` (16, `internal/server/domain_failure.go:20`) at a time, each bounded by `preflightTimeout` (5s, `internal/server/dynamic_domains.go:29`) on the shared client. Cancelling on the handshake's context would throw away a probe result that the *next* handshake needs, and the bound is already fixed and small. This exemption is scoped to these probes; it is not a licence for unbounded external calls.
- **Where:** `internal/server/domain_failure.go#probeDomains`, used by `#filterBatchMates` and `#identifyFailedDomains`
- **Proven by:** the bounds are constants read by `probeDomains`; no test asserts the concurrency figure directly
- **Origin:** cubic learnings 94f61938, 406abada; PR #97 (mhenrixon: "Left the probes non-ctx-aware deliberately")

### Quarantined pending domains are skipped before the batch-size limit is applied, not after
- **Holds because:** filtering after the `MaxSANsPerCertificate` (100) cut would let held domains occupy slots they cannot use, so a later batch never refills past them and healthy pending domains wait behind names that are known to fail.
- **Where:** `internal/server/san_cert_manager.go#provisionCertificate`, the inline candidate loop (there is no separate collector function)
- **Proven by:** `TestBatchGuard_QuarantinedDomainsDoNotConsumeBatchSlots`, `TestBatchGuard_QuarantinedBatchMateIsSkippedButStaysPending`
- **Origin:** cubic learning 218bf465

### A successful adoption clears the quarantine history of every domain in the batch, not just the trigger
- **Holds because:** the ladder's failure count is what decides the *next* hold's length. Leaving a count behind on a domain that has just been certified means its next unrelated failure starts several steps up the ladder, which is how a transient problem turns into a day-long hold. `clearBatchQuarantine` runs on the whole requested set after `adoptCertificate`.
- **Where:** `internal/server/san_cert_manager.go#provisionCertificate`, `#clearBatchQuarantine`
- **Proven by:** `TestBatchGuard_SuccessfulBatchClearsQuarantineHistory`
- **Origin:** cubic learning a934f6ab

### Quarantine mutations notify the dynamic-domain save callback, so a hold survives a restart
- **Holds because:** the ladder exists to stop a failing domain looping against a CA's rate limits. A hold held only in memory is lifted by every proxy restart, and a crash-looping proxy would then hammer the CA exactly when it is least able to answer. Both the record and the clear notify.
- **Where:** `internal/server/san_cert_manager.go` (issuance-guard mutations) → `DynamicDomainManager`'s save callback
- **Proven by:** `TestBatchGuard_MutationsNotifyChangeForPersistence`, `TestDomainQuarantine_SnapshotRestore`
- **Origin:** cubic learning dda400b7

### An unattributable order failure quarantines the whole batch; a named one quarantines only the culprits
- **Holds because:** when the CA names the identifiers it refused, holding the innocent members would stall tenants for someone else's misconfiguration. When it names nobody, releasing everyone means the same batch re-forms on the next handshake and loops — so the safe direction inverts. A rate limit with no named identifier and no advertised retry time is the one unattributable case that restores everything: the CA has told us nothing to act on.
- **Where:** `internal/server/san_cert_manager.go#attributeBatchFailure`, `internal/server/domain_failure.go#identifyFailedDomains`
- **Proven by:** `TestBatchGuard_QuarantinesCulpritsAndRestoresSurvivorsOnFailure`, `TestBatchGuard_UnattributableFailureRestoresEverythingUnquarantined`, `TestBatchGuard_RateLimitedIdentifierIsHeldAndSurvivorsRestored`, `TestBatchGuard_UnnamedRateLimitWithRetryTimeHoldsWholeBatch`, `TestBatchGuard_UnnamedRateLimitWithoutRetryTimeRestoresEverything`
- **Origin:** cubic learnings 5e9e37e0, 406abada; PR #112

### One order never spans two DNS providers, and a wildcard order never leaves its provider's zone
- **Holds because:** DNS-01 is solved by writing a TXT record, and a provider can only write in the zones it holds credentials for. An order mixing zones fails at validation for the members the armed provider cannot answer for, taking the reachable ones with it. `splitByProviderZone` narrows to one partition and returns the deferred members to pending; `orderObtainer` refuses a batch that still spans providers, as a second line.
- **Where:** `internal/server/san_cert_zones.go#splitByProviderZone`, `#wildcardAnchors`, `#outsideWildcardZones`
- **Proven by:** `TestSANCertManager_ProvisionCertificate_NarrowsBatchToOnePartition`, `TestSANCertManager_ObtainCertificate_RefusesOrderSpanningProviders`, `TestSANCertManager_ProvisionCertificate_WildcardOrderExcludesForeignZones`, `TestSANCertManager_SplitByProviderZone_UnsolvableWildcardIsolatedAlone`, `TestSANCertManager_SplitByProviderZone_NestedWildcardZonesLongestWins`
- **Origin:** cubic learnings 90fb8c5f, 9b8e7442; PR #113

### A waiter on someone else's provisioning re-checks expiry and directory before it is served anything
- **Holds because:** a waiter wakes when the flight settles, not when it succeeded. Serving whatever certificate is in the map at that moment hands the client an expired leaf, or one from a directory its service has moved away from — and, worse, makes a failed order look locally successful, so nothing retries. `getServableCertForDomain` rejects past `NotAfter`, and a registered domain whose owner's directory still mismatches gets `ErrCertNotFound`.
- **Where:** `internal/server/san_cert_manager.go#getServableCertForDomain`
- **Proven by:** `TestSANCertManager_GetCertificate_WaiterRefusesExpiredCert`, `TestSANCertManager_GetCertificate_WaiterRefusesStillMismatchedCert`
- **Origin:** cubic learnings c3c755d5, 477c65ef

### A certificate whose dynamic domains were all evicted keeps serving until it expires, and is reusable if they come back
- **Holds because:** eviction from a source is a statement about issuance, not about the bytes already on disk. Dropping the certificate would break live handshakes for a tenant whose source blipped, and re-issuing on recovery would spend an order for a certificate that is still valid. Normal replacement and renewal rules continue to apply to it.
- **Where:** `internal/server/san_cert_manager.go#GetCertificate`, `internal/server/domain_renewal.go#reconcile`
- **Proven by:** `TestSANCertManager_GetCertificate_ServesExistingCertForEvictedDomain`, `TestCertRenewer_KeepsFullyEvictedCertificateUntilExpiry`, `TestCertRenewer_EvictedCertificateSurvivesSourceRecovery`, `TestCertRenewer_RemovesFullyEvictedCertificateAfterExpiry`
- **Origin:** cubic learning 32580bd5

### The legacy HTTP-01 cache import runs with no manager or store lock held
- **Holds because:** adoption takes `stateMu` and `mu` itself. Calling it under either from `Initialize` deadlocks the proxy at boot, before any listener is up — the failure mode with the least diagnostic output available.
- **Where:** `internal/server/san_cert_import.go#importLegacyHTTP01Cache`, called from `SANCertManager.Initialize`
- **Proven by:** `TestSANCertManager_InitializeAdoptsLegacyCacheWithoutDeadlock`
- **Origin:** cubic learning d2c76f1e

### `stateMu` spans both the in-memory publication and the file writes
- **Holds because:** an export or a concurrent persist that interleaves between the two captures a state file naming certificates whose `cert.pem` is not written yet — an archive that restores into a broken estate. `adoptCertificateAt` takes `stateMu` then `mu`, and `removeCertificate` holds the same span.
- **Where:** `internal/server/san_cert_manager.go#adoptCertificateAt`, `#removeCertificate`
- **Proven by:** `TestSANCertManager_ExportStoreHoldsTheDiskLock`
- **Origin:** cubic learning abe274ee

### Not a bug: a stale `Retry-After` still ends the hold, rather than falling back to the ladder
- **Holds because:** `RecordRateLimited` holds until the advertised time plus `rateLimitHoldMargin` (1 minute, `internal/server/domain_failure.go:89`), so even a timestamp 30 seconds stale ends the hold 60 seconds *after* the CA said to retry — which is the margin's contract. Falling back to the ladder would delay an already-permitted retry by 15 minutes or more. A still-throttling server returns the same now-staler timestamp, `until` is no longer after `now`, and the existing condition drops to the ladder at failures=2; no tight loop is reachable.
- **Where:** `internal/server/domain_quarantine.go#RecordRateLimited`
- **Proven by:** `TestDomainQuarantine_RecordRateLimitedHoldsUntilAdvertisedTime`, `TestDomainQuarantine_RecordRateLimitedFallsBackToLadder`
- **Origin:** PR #112 review thread (rejected after analysis)

## Related

- `certs-directories-and-renewal.md` — per-service directories, ARI, renewal partitions
- `../certs/summary.md` — the subsystem as a whole

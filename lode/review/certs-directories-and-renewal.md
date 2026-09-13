Per-service ACME directories (`--tls-staging`), and what the renewer does with a
certificate whose members no longer agree about where they should be issued.

### A covering certificate only counts if it was issued at the owning service's current directory
- **Holds because:** a staging certificate covers the same names as a production one and is untrusted by every client. Without the directory check, a service flipped from `--tls-staging` to production would keep serving the staging leaf forever, because coverage alone said it was fine. `RegisterDomain`, `GetCertificate` and `HasValidCertificate` all require the match; a mismatch keeps the domain pending and reprovisions it.
- **Where:** `internal/server/san_cert_manager.go#RegisterDomain`, `#GetCertificate` (430-512), `#HasValidCertificate`; `internal/server/san_cert_directories.go#certMatchesServiceDirectoryLocked`
- **Proven by:** `TestSANCertManager_RegisterDomainRejectsCoverageFromAnotherDirectory`, `TestSANCertManager_CertDirectoryMismatched`, `TestSANCertManager_GetCertificate_MismatchedDirectoryCertReprovisionsSynchronously`
- **Origin:** cubic learning 905438d7

### A registered domain reprovisions synchronously on a directory mismatch; a dynamic one is served stale while the issuer replaces it
- **Holds because:** the serve-stale rule was written for the *degraded renewal* case — reaching the expiry window means proactive renewal was already failing, so a synchronous order on the handshake is most likely to fail too. A directory mismatch is the opposite: fresh operator intent, with a presumably healthy CA, and the staging leaf is untrusted by exactly the clients a staging→production flip is for. Dynamic domains stay on the serve-stale path even then, or one flag flip fails every tenant handshake at once while the issuer drains a rate-limited queue.
- **Where:** `internal/server/san_cert_manager.go#GetCertificate` (430-512)
- **Proven by:** `TestSANCertManager_GetCertificate_MismatchedDirectoryCertReprovisionsSynchronously`, `TestSANCertManager_GetCertificate_MismatchedDynamicCertServedWhileIssuerReplaces`, `TestSANCertManager_GetCertificate_ServesExpiringRegisteredCertAndQueuesReplacement`, `TestSANCertManager_GetCertificate_ExpiredRegisteredCertReprovisionsSynchronously`
- **Origin:** PR #103 review thread (accepted with a deliberate deviation)

### `createCertManager` clears the service's directory override first, on every branch
- **Holds because:** the override is keyed by service name in a manager the service may no longer be using. Clearing only inside the shared-SAN branch leaves a stale `--tls-staging` behind when a service redeploys onto static certificates or off TLS entirely — and the next service to reuse that name inherits it. The clear happens before the TLS-mode branches; `ACMEDirectory` is then recorded before any host is registered, so the first `RegisterDomain` already sees the right directory.
- **Where:** `internal/server/service.go#createCertManager`; `internal/server/san_cert_directories.go#SetServiceDirectory`
- **Proven by:** `TestRouter_RedeployOffTheSANManagerClearsTheDirectoryOverride`, `TestRouter_DeployRegistersServiceDirectoryWithSANManager`, `TestSANCertManager_SetServiceDirectory`
- **Origin:** cubic learning 2fb499ef

### A wildcard's directory comes from the concrete domains in its batch, and failing that only from names the certificate actually serves
- **Holds because:** a synthesized `*.example.com` has no service of its own. Resolving it by scanning everything the wildcard could cover picks up hosts from unrelated services and issues at the wrong directory. `directoryForDomains` prefers the concrete batch domains so the wildcard inherits its originating service; without batch context `wildcardOwnerLocked` consults only the certificate's own `Domains` and picks the smallest covered name, so the answer is deterministic.
- **Where:** `internal/server/san_cert_directories.go#directoryForDomains`, `#wildcardOwnerLocked`
- **Proven by:** `TestSANCertManager_DirectoryForDomains`, `TestSANCertManager_WildcardOwnerConsultsOnlyDomainsTheCertServes`
- **Origin:** cubic learning 90fb8c5f

### Account keys are named per directory, and the naming predicate is closed
- **Holds because:** two directories are two ACME accounts; sharing one key between them makes the second registration fail. The run-level directory uses `acme_user.json`, Let's Encrypt staging `acme_user_staging.json`, anything else `acme_user_<8 lowercase hex>.json`. `isExtraAccountKeyFile` accepts exactly those two extra shapes and nothing else — an open `acme_user_*.json` glob would let a planted file in a restored archive be adopted as this proxy's ACME identity.
- **Where:** `internal/server/san_cert_directories.go#accountFileForDirectory`, `#clientsForDirectory`; `internal/server/cert_store_export.go#isExtraAccountKeyFile`
- **Proven by:** `TestSANCertManager_AccountFileForDirectory`, `TestSANCertManager_AccountFileForStagingWhenDefaultIsProduction`, `TestIsExtraAccountKeyFile`, `TestRestoreCertificateStore_RoundTripsExtraAccountKeysAndDirectories`
- **Origin:** cubic learning 800d1de2

### A renewal is partitioned by desired directory first, then by provider zone, and each partition issues under its own owner's identity
- **Holds because:** one order has exactly one ACME identity and one DNS provider. A certificate whose members' services have drifted to different directories cannot be renewed as one order at all; issuing it at the run-level default would silently move every member. `renew` resolves every owner it can, splits by directory, splits each of those by provider zone, and records each partition under the directory it was issued at.
- **Where:** `internal/server/domain_renewal.go#renew` (239-358), `#renewPartition`
- **Proven by:** `TestCertRenewer_SplitsMixedDirectoryCertificateAtRenewal`, `TestCertRenewer_SplitsMixedZoneCertificateAcrossProviders`, `TestCertRenewer_MixedZoneSplitKeepsOldCertWhenAPartitionFails`
- **Origin:** cubic learning 9b8e7442

### A partition whose owner cannot be resolved is pinned to the certificate's *recorded* directory, not the run-level default
- **Holds because:** an unresolved owner means the service is gone or renamed — which says nothing about where the certificate belongs. Falling back to the run-level directory silently migrates a staging certificate into production (or the reverse) on a routine renewal. Both the issuance and the adoption are pinned to the recorded value, so the record stays true.
- **Where:** `internal/server/domain_renewal.go#renew`, `#renewPartition`
- **Proven by:** `TestCertRenewer_UnresolvedOwnerRenewsAtRecordedDirectory`
- **Origin:** cubic learning 7e50719a

### The ARI `replaces` marker rides at most one partition — the first still at the recorded directory — and is spent on CA acceptance
- **Holds because:** RFC 9773's marker identifies the certificate being replaced, and a CA rejects a second order naming an identifier it has already honoured. So it must not be attached to a partition that has switched directories (a different CA has never heard of it), and it must be spent at *order acceptance*, not at local adoption: `Obtain` succeeding means the replacement exists at the CA, and re-sending the marker after a locally failed adoption would have the next order refused. `renewPartition` returns `(renewed, adopted, ordered, accountLimit)` to make that boundary explicit, and the marker survives to a later same-directory partition when an earlier order is refused outright.
- **Where:** `internal/server/domain_renewal.go#renew`, `#renewPartition`
- **Proven by:** `TestCertRenewer_ARIMarkerSurvivesAFailedFirstPartition`, `TestCertRenewer_DirectorySwitchDropsARIReplaces`, `TestCertRenewer_HonorsARIWindow`
- **Origin:** cubic learning 19d3eba8; PR #105 review thread

### An account-level rate limit halts that certificate's partition loop, and only that directory's
- **Holds because:** an account-level rejection names no identifiers, so there is nothing to quarantine and nothing to learn from trying the next partition — every one of them would be refused at the same account, spending attempts against a limit that is already tripped. `handleRenewalFailure` returns halt and `renew` breaks the loop for this pass. Another directory is a separate account with a separate bucket, so it still proceeds.
- **Where:** `internal/server/domain_renewal.go#handleRenewalFailure`, `#renew`
- **Proven by:** `TestCertRenewer_AccountLevelRateLimitStopsThePartitionLoop`, `TestCertRenewer_AccountLevelRateLimitIsScopedToItsDirectory`
- **Origin:** cubic learning 5e9e37e0

### Renewal defers rather than shrinks while there is still time, and compacts only inside the compaction window
- **Holds because:** dropping a quarantined member from the identifier set unmaps a host that is still being served, and it forfeits the CA's identical-set renewal exemption, so the shrunken order costs a fresh rate-limit slot. Deferring keeps both. `compactionWindowFor` is where the deferral stops being safe.
- **Where:** `internal/server/domain_renewal.go#renew` (239-358), `#compactionWindowFor`
- **Proven by:** `TestCertRenewer_DefersPartiallyQuarantinedBatchWhenTimeAllows`, `TestCertRenewer_CompactsQuarantinedDomainsNearExpiry`, `TestCertRenewer_SkipsRenewalWhenAllDomainsQuarantined`
- **Origin:** cubic learning 86e6a9df

### A certificate covering any deploy-registered host compacts a week earlier — wildcards included
- **Holds because:** a registered host is one an operator named in `deploy --host`, so losing it is a visible outage rather than one tenant's page failing. The extra week is the margin that buys. A wildcard hides the fact: `compactionWindowFor` resolves the registered domains a wildcard member covers rather than looking only at literal names, or a `*.example.com` protecting a registered host would compact on the dynamic schedule.
- **Where:** `internal/server/domain_renewal.go#compactionWindowFor`
- **Proven by:** `TestCertRenewer_RegisteredCertCompactsEarlierThanDynamic`, `TestCertRenewer_WildcardCertCoveringRegisteredHostCompactsEarly`
- **Origin:** cubic learning 86e6a9df

### The renewer's gauges are published only after a completed reconcile pass
- **Holds because:** a mid-loop `ctx.Err()` return means the counts describe part of the estate. A partial count is wrong in a less predictable way than a one-interval-stale one, and the cancellation path only runs at shutdown, when the metrics endpoint is going away anyway. `certificate_renewals_deferred` follows the same rule as every other renewer gauge — consistency here is the point, not a special case.
- **Where:** `internal/server/domain_renewal.go#reconcile`, `#reportMetrics`
- **Proven by:** `TestCertRenewer_ReportsDeferredRenewals`
- **Origin:** cubic learning f54bad2d; PR #105 review thread ("Not changed, deliberately")

### Renewal timing comes from ARI when the CA offers it, and otherwise from a fraction of the certificate's own lifetime
- **Holds because:** hard-coding "renew at 60 days" breaks the moment a CA changes its lifetime — and Let's Encrypt now issues both 90-day and 6-day certificates. `shouldRenew` asks the obtainer for ARI first (via the optional `renewalInfoGetter` assertion) and falls back to `NotAfter - lifetime/3` — a third of the *observed* lifetime remaining, where the lifetime is `NotAfter - leaf.NotBefore` — plus a per-certificate `certJitter`, so a fleet that booted together does not renew together.
- **Where:** `internal/server/domain_renewal.go#shouldRenew` (211-232), `#reconcile`
- **Proven by:** `TestCertRenewer_HonorsARIWindow`, `TestCertRenewer_RenewsInsideFallbackWindow`, `TestCertRenewer_LeavesFreshCertificatesAlone`
- **Origin:** verified from the code during seeding; no cubic learning

### A state file written before directories existed loads as the default directory
- **Holds because:** the field is absent, not empty-meaning-something-else, in every pre-feature state file. Decoding it as "" and then treating "" as a mismatch would make every restored certificate reprovision on its first handshake after an upgrade.
- **Where:** `internal/server/san_cert_directories.go` (state decode); `internal/server/san_cert_manager.go#adoptCertificateAt` stamps the directory on the way in
- **Proven by:** `TestSANCertManager_OldStateWithoutDirectoryLoadsAsDefault`, `TestSANCertManager_StateRoundTripsDirectory`, `TestSANCertManager_AdoptCertificateStampsDirectory`
- **Origin:** cubic learning 905438d7

## Related

- `certs-issuance.md` — batching, probes, quarantine
- `../certs/summary.md` — the subsystem as a whole

The DNS-01 provider registry, and the rule that there is exactly one list of
providers in this codebase.

### `internal/server/acme/providers/registry.go` is the only provider list; everything else derives from it
- **Holds because:** a second hand-maintained list — a `switch` in the factory, an ordered detection slice, a flag help string, a README table — drifts the first time a provider is added, and the copy that drifts is the one nobody is looking at. The registry map holds all eight entries (`ProviderCloudflare`, `ProviderRoute53`, `ProviderDigitalOcean`, `ProviderGoogleCloud`, `ProviderNamecheap`, `ProviderGoDaddy`, `ProviderHetzner`, `ProviderVultr`); `GetProviderInfo`, `NewProvider`, `detectionOrder`, the `--acme-dns-provider` help and the README table all read it.
- **Where:** `internal/server/acme/providers/registry.go` (`registry` at 65, `detectionOrder` at 139)
- **Proven by:** `TestREADMEProviderTable_MatchesRegistry`, `TestRunCommand_DNSProviderHelpMatchesRegistry`
- **Origin:** cubic learnings d6216872, fc516966

### Provider-name validation goes through `acme.ParseProviderName`, never through `providers.Names()`
- **Holds because:** `providers` imports `acme`, so validating from the registry package would create an import cycle. `ParseProviderName` carries the aliases as well as the canonical names (`cf`, `aws`/`r53`, `do`, `google`/`gcp`/`googledns`, `nc`, `gd`, `hz`, `vr`), which a derived list would silently drop.
- **Where:** `internal/server/acme/provider.go#ParseProviderName` (44-69), `internal/server/acme/mapping.go#ParseProviderEntries`
- **Proven by:** `TestRunCommand_ACMEDNSProviderZoneMappings`, `TestRunCommand_ACMEDNSProviderEnvSupportsMappings`
- **Origin:** cubic learning d6216872

### The only values that disable DNS-01 are the empty string and `none`
- **Holds because:** an undocumented alias such as `off` that quietly parsed as "disabled" would leave an operator believing DNS-01 was armed. `ParseProviderName` returns `ErrProviderNotSupported` for anything not in the switch, so a typo is a startup error rather than a silent HTTP-01 fallback. The generated help and the README stay aligned with the same set.
- **Where:** `internal/server/acme/provider.go#ParseProviderName` (44-69)
- **Proven by:** `TestRunCommand_DNSProviderDefaultsOff`, `TestRunCommand_DNSProviderHelpMatchesRegistry`
- **Origin:** cubic learning 468019dc

### The generated table's `Name` column carries the exact parser-accepted flag value, not the display name
- **Holds because:** the table is what an operator copies into `--acme-dns-provider`. Printing "AWS Route53" there gives them a value the parser rejects. `DisplayName` is a separate field for the human-readable column.
- **Where:** `internal/server/acme/providers/registry.go` (`DisplayName`), `internal/server/acme/providers/docs.go`
- **Proven by:** `TestREADMEProviderTable_MatchesRegistry`
- **Origin:** cubic learning acaac3bf

### The README table is generated, and a drift test fails the build
- **Holds because:** a committed table is the one artefact that cannot be derived at runtime, so it needs a guard. `go generate ./internal/server/acme/providers` rewrites `README.md` between `<!-- BEGIN GENERATED: dns-provider-table … -->` and `<!-- END GENERATED: dns-provider-table -->` (README.md:1142-1153). When the test detects drift it reports *every* stale or missing row — including changed credential or optional columns — and names the command to regenerate, because a failure listing one row sends the author back for a second run.
- **Where:** `internal/server/acme/providers/docs.go` (`//go:generate go run ./gen`, markers at 19-20), `internal/server/acme/providers/gen/`
- **Proven by:** `TestREADMEProviderTable_MatchesRegistry`
- **Origin:** cubic learning fc516966

### Cloudflare's credential rule lists exactly the four canonical same-namespace sets lego accepts
- **Holds because:** the boot check exists to refuse a configuration that cannot construct. Vouching for one that can't is the failure that shipped (`CF_API_TOKEN`, which lego never reads, #115) — it fails **open**. The `Alternatives` OR-of-ANDs is `{CF_DNS_API_TOKEN}`, `{CLOUDFLARE_DNS_API_TOKEN}`, `{CF_API_KEY, CF_API_EMAIL}`, `{CLOUDFLARE_API_KEY, CLOUDFLARE_EMAIL}` — token or key+email, in either namespace, never mixed across namespaces.
- **Where:** `internal/server/acme/providers/registry.go` (66-83)
- **Proven by:** `TestNewProvider_CloudflareCredentialSetsConstruct`
- **Origin:** cubic learning 23da0116; PR #116

### Not a bug: a mixed-namespace Cloudflare pair is refused even though lego would construct it
- **Holds because:** lego resolves each field independently, so `CF_API_KEY` + `CLOUDFLARE_EMAIL` constructs there and fails this check. The two directions are not symmetric: #115's bug failed **open** (the check vouched for a config that could not construct), while this gap fails **closed**, with an error naming exactly which variables to set — safe and self-correcting. Enumerating the cross-namespace mixes would grow the registry to six credential sets, all rendered into the generated README table, documenting a mixed-namespace hygiene nobody should adopt.
- **Where:** `internal/server/acme/providers/registry.go` (66-83), `#NewProvider`
- **Proven by:** `TestNewProvider_CloudflareCredentialSetsConstruct` (the canonical sets); the mixed-namespace direction is asserted to fail closed
- **Origin:** PR #116 review thread (partially accepted, remainder declined with reasoning)

### `NoBootCheck` and `Detect` are separate fields because Route53 needs them to differ
- **Holds because:** the AWS SDK resolves credentials from the environment, shared config or an IAM role on its own, so an empty environment is not an error and the boot check must be skipped. Auto-detection still has to key off something *visible*, which is what `Detect` (`AWS_ACCESS_KEY_ID` or `AWS_PROFILE`) supplies. Folding them into one field would either break detection on an instance role or make an empty environment fatal.
- **Where:** `internal/server/acme/providers/registry.go` (84-95), `#credentialSets`, `#detectSets`
- **Proven by:** `TestSANCertManager_InitDNSClients_MappedProviderWithoutCredentialsIsFatal`, `TestSANCertManager_InitDNSClients_DefaultProviderFallbackStaysSoft`
- **Origin:** verified from the code during seeding

### With `auto`, the boot warning names the provider that actually constructed
- **Holds because:** `DetectProviderName`'s credential match is looser than construction — it can point at a provider whose `New()` then fails, so the warning would name one provider while another (or none) is armed. The name is taken from the successful construction walk.
- **Where:** `internal/server/san_cert_manager.go#initDNSClients`
- **Proven by:** `TestSANCertManager_InitDNSClients_NoProviderConfiguredStaysHTTP01`, `TestSANCertManager_InitDNSClients_DefaultProviderFallbackStaysSoft`
- **Origin:** cubic learning 3120d37a

### A mapped provider with no credentials is fatal at boot; a *default* provider's fallback stays soft
- **Holds because:** a zone mapping is an explicit statement that this zone needs DNS-01 — booting anyway leaves wildcards in that zone quietly unissuable. A default provider is a preference for zones that have no mapping, and those zones can still be served over HTTP-01, so it degrades rather than refusing to start.
- **Where:** `internal/server/san_cert_manager.go#initDNSClients`
- **Proven by:** `TestSANCertManager_InitDNSClients_MappedProviderWithoutCredentialsIsFatal`, `TestSANCertManager_InitDNSClients_DefaultProviderFallbackStaysSoft`, `TestSANCertManager_InitDNSClients_NoProviderConfiguredStaysHTTP01`
- **Origin:** cubic learning 3120d37a

### `ProviderFor` picks the longest matching zone suffix, on label boundaries
- **Holds because:** a fleet can map `example.com` to one host and `eu.example.com` to another, and the more specific mapping has to win. Matching on a bare string suffix would make `notexample.com` match `example.com`; the comparison requires the candidate to equal the zone or end in `"." + zone`. The matched zone doubles as the partition key that keeps one ACME order on one provider.
- **Where:** `internal/server/acme/mapping.go#ProviderFor` (31-49)
- **Proven by:** `TestSANCertManager_SplitByProviderZone_NestedWildcardZonesLongestWins`, `TestSANCertManager_SplitByProviderZone_MappedSubzoneStaysOutOfWildcardOrder`, `TestRunCommand_ACMEDNSProviderZoneMappings`
- **Origin:** verified from the code during seeding

### `auto` cannot be mapped to a zone
- **Holds because:** `auto` means "pick the one default from visible credentials", and a zone mapping is by definition explicit. Allowing `zone=auto` would make the answer depend on the environment rather than on what the operator wrote.
- **Where:** `internal/server/acme/mapping.go#ParseProviderEntries` (56-103)
- **Proven by:** `TestRunCommand_ACMEDNSProviderZoneMappings`
- **Origin:** verified from the code during seeding

### A stub ACME directory handler reports encoding failures with `t.Error`, not `require.NoError`
- **Holds because:** the handler runs on the `httptest` server's goroutine, and `require` calls `t.FailNow`, which is only legal on the test's own goroutine — there it aborts the handler without failing the test, so the assertion silently stops asserting.
- **Where:** `internal/server/san_cert_manager_test.go` (stub directory handler)
- **Proven by:** the convention itself; it is a test-code rule
- **Origin:** cubic learning 982751b2

## Related

- `certs-issuance.md` — how a provider zone partitions an order
- `../certs/summary.md` — the subsystem as a whole

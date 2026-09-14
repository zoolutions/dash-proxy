# Lode map

The index of this repository's durable memory. Read this first; it beats a
directory listing. Every file describes dash-proxy as it is now, with the
reasoning behind it — never what changed.

- `summary.md` — what the proxy is, the two paths, the three invariants
- `terminology.md` — the words this repo uses (slot, batch-mate, pre-flight probe, quarantine, shrink guard, variant index, refusal, idle controller…)
- `practices.md` — patterns `../.claude/rules/` does not state: additive state schemas, staged writes and stated durability, double-parsed configuration, fail-open-or-closed per subsystem, seams instead of live dependencies, lock ordering, the generated provider table
- `workflow.md` — the profile the shared `/lode:` workflow skills read: commands, branches, layers, shapes, constraints, docs, CI, flake sources, conflicts, verification
- `plans/README.md` — plans live in `../docs/plans/`; scratch in `tmp/` (git-ignored)

## Subsystems

- `request-path/summary.md` — listeners, the ten-handler root chain, `Router`, `Service`'s per-request order, `LoadBalancer` and `Target`; the only I/O is the cache store, a certificate lookup and the proxied request
- `certs/summary.md` — `SANCertManager`: the two allowlists, handshake batching, provider zones, per-service ACME directories, quarantine and release, asynchronous issuance, renewal, and the three other cert managers
- `certs/store-and-recovery.md` — the archive layout, export, the shared archive reader, restore and verify, the Traefik and legacy importers
- `dynamic-sources/summary.md` — `sourcePoller` and `refreshNudge`, then the domain manager (shrink guard, issuance) and the redirect manager (compiled map, poller identity), and their two separate persisted schemas
- `cache/summary.md` — the three stores and the optional leaser, two-level variant keys, the single flight, and the 18 refusal reasons
- `resilience/summary.md` — health-check regimes, retries, the one `forwardedResolver` behind allow/deny/rate-limit, basic auth, pause and rollout, path timeouts, scale-to-zero
- `cli-and-rpc/summary.md` — the fourteen commands, the unix socket, the `kamal-proxy` RPC name and its 16 call sites, append-only wire types, where configuration is parsed
- `observability/summary.md` — the sixteen metrics and the atomic tracker swap, log formats, trace-context modes
- `testing-and-ci/summary.md` — 1080 tests across 115 files, the shared helpers, what a test never does, the three workflows, and the release script

## Review rules (`review/`)

Accepted review findings rewritten as rules about the system, each verified
against the current code and carrying the test that proves it. `/lode:gate`
reads every file here before reviewing a diff; `/lode:learn` adds to them.

- `review/certs-issuance.md` — service-scoped single flight, trigger and batch-mate probing, quarantine slots and clearing, failure attribution, provider partitions, waiter re-checks; one *Not a bug* (a stale `Retry-After` still ends the hold)
- `review/certs-directories-and-renewal.md` — directory-matched coverage, the registered/dynamic split on a mismatch, wildcard ownership, per-directory account keys, renewal partitioning, the ARI `replaces` boundary, compaction windows, completed-pass gauges
- `review/cert-store-export.md` — the live-proxy fallback rule, resolved-path plus identity validation, pinned roots, fail-closed containment, staged and self-verified archives, what counts as a certificate
- `review/cert-store-restore.md` — archive bounds and capped-reader semantics, entry and state validation, the ECDSA account-key contract, write ordering, stale-directory removal, import flag modes; one *Not a bug* (per-file Traefik replacement)
- `review/dynamic-domains.md` — the shrink guard and its ETag clearing, issuance following the polled list, hold resets, release-prober candidate selection, quarantine kinds
- `review/dynamic-redirects.md` — the `hosts`-key requirement, cleared state, poller identity, deterministic normalization, exempt paths, encoded paths, compile-once; two declined suggestions
- `review/sources-and-refresh.md` — path-versus-URL transports, ETag ordering, poll-failure counting, the five-step nudge, constant-time tokens; one *Not a bug* (Warn-level rejection logging)
- `review/acme-providers.md` — the registry as the single provider list, `ParseProviderName`, the generated README table and its drift test, Cloudflare's credential sets, zone matching; one *Not a bug* (mixed-namespace credentials)

## Not memory

- `tmp/` — git-ignored: gate reports, handovers, scratch

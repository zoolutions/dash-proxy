# Response cache

An opt-in shared HTTP cache sitting between the per-service checks and the load
balancer. Storing is opt-in twice over: the service must be deployed with
`--cache`, and the target must mark the response `public` with a lifetime.
Because the cache sits *below* every access check, a stored response can only
ever reach a client the target would have been asked on behalf of.

The rules follow RFC 9111 and RFC 5861, and are deliberately stricter than
either in two places: storing requires an explicit `public` directive rather
than a heuristic, and a request carrying credentials never touches the cache.

## Stores

`NewCacheStore` (`internal/server/cache_store.go`) accepts three `--cache-store`
forms; `ParseCacheStoreURL` rejects a typo at startup rather than at the first
cached request.

| Form | Type | Shared? | Leases? |
|---|---|---|---|
| `memory`, or empty | `memoryCacheStore` | no — per process | no |
| `file://<dir>` | `fileCacheStore` | no — single host | no |
| `redis://`, `rediss://` | `redisCacheStore` | yes — the fleet | yes |

A bare `file://` naming no directory is a typo rather than a request for some
default location, so it is refused at startup instead of quietly caching into
the working directory. `DefaultCacheMemorySize` (256 MB) caps the in-process
store and the file store's index budget; `DefaultCacheStoreTimeout` (100ms)
bounds each shared-store operation, short on purpose — a cache that takes
longer than that has already cost more than the miss it was meant to save.

Every implementation **fails open**: an unreachable store turns lookups into
misses and writes into logged errors, never into a failed request.

`CacheLeaser` is the optional half, implemented only by the shared store. On a
single proxy the in-process single flight already *is* the lease, and
arbitrating with nobody would cost a round trip to learn what is already known
— so the memory and file stores deliberately have none of these methods, and
the middleware's nil check is the whole of what a single-node deployment pays.
Every leaser method fails open too: a lease in any doubt **grants**, because a
duplicate fetch is cheaper than an origin outage. `ProbeLease` returns the entry
and the lease-held flag in one round trip on purpose — a probe that took two
could see the lease released between them and give up on an entry that was
already there.

## Two-level keys: index and variant

A resource that negotiates is stored as two records
(`internal/server/cache_variant.go`): a **variant index** at the resource's own
key naming the fields it varies on, and a **variant** at the key those fields'
values produce. A non-varying resource — the overwhelming majority — is one
record at its own key, one lookup, one write, which is why the index is a
separate record rather than a wrapper around every entry.

`entryIsResponse` (`StatusCode >= 100`) and `entryIsIndex` (`StatusCode == 0`
and a non-empty `VaryOn`) are how the two are told apart, and every path that
hands bytes to a client goes through the former.

`cacheKeyVersion` (`"2"`) salts every key this build computes so entries written
by a proxy predating variant indexes live in a disjoint key space. This is not
belt and braces: gob silently drops fields the receiving type lacks, so an older
binary decodes an index as a response with `StatusCode` 0 and a nil body, and
replaying that calls `WriteHeader(0)`, which panics `net/http`. The cost is one
lifetime of cold shared cache at one release, on a fleet that just restarted.

`maxVaryFields` (8) bounds how many dimensions one response may negotiate on.
`unkeyableVaryFields` — `authorization`, `cookie`, `referer`, `user-agent` —
differ for practically every client, so keying on them would store one copy per
client. Naming one in `--cache-vary-header` is the override, and it is
deliberately awkward: it moves the field into the primary key for the whole
service. `DefaultCacheMaxVariants` (32) bounds representations per resource.

`Accept-Encoding` is the one Vary dimension the key deliberately does not carry:
the cache stores the representation the target produced and sits *inside* the
compression middleware, so every response is encoded for its own client on the
way out and one stored entry serves them all. A response the target encoded
itself is refused instead (`cacheRefusalContentEncoding`), because that one
really is encoding-specific.

## Serving and fetching

`CacheMiddleware.ServeHTTP` (105-133) looks up, following an index to its
variant when it finds one, and hands off to `serve` (137-157):

- needs revalidation → replay the stale copy now and refresh behind it (RFC 5861)
- servable → replay as a hit
- otherwise → fall through to `fetch`

`fetch` (161-222) keys the in-process single flight on the **resource**, not the
variant: with a two-level scheme nobody knows which variant this is until the
response arrives. That makes the key a collapsing hint and nothing more —
`entryAnswers` re-derives the variant from each waiter's own headers, so a wrong
guess costs a fetch and never a wrong body. A follower whose variant does not
match the leader's entry goes to the origin itself, and does **not** register as
a new leader: whatever made the response unshareable will do so again.

Only the node's leader touches the lease, so fleet-wide lease traffic is one
operation per node per key rather than one per request. When a fetch turns out
to have nothing storable, the key is handed back immediately rather than held
for the whole TTL.

A request may refuse the stored copy for itself and still populate the cache for
everyone behind it — which is what makes a reload useful.

## Refusals

`cacheRefusal` (`internal/server/cache_refusal.go`) is a low-cardinality reason
a response was not stored, surfaced as `cache_refusals_total{reason=…}` and, for
the ones an operator can act on, as advice. There are 18 reasons plus
`cacheRefusalNone` (the empty string, meaning storable):

`disabled`, `status`, `not_public`, `no_store`, `private`, `no_cache`,
`no_lifetime`, `set_cookie`, `content_range`, `content_encoding`,
`event_stream`, `vary`, `vary_unkeyable`, `vary_too_many`, `variant_limit`,
`head_request`, `too_large`, `hijacked`.

`cacheRefusal.advice` (54-75) names the lever for the subset an operator can do
something about; the rest report the reason only. The offending Vary *field* is
logged but never used as a metric label — an application-chosen string there is
a cardinality bomb.

`cacheableStatuses` (`internal/server/cache_policy.go`) is RFC 9111 §15.1's list
minus 206: a partial response describes a byte range this proxy never tracks,
and storing one would answer a later full request with a fragment.

`s-maxage` wins outright over `max-age` when present — it exists precisely to
say something different to shared caches, which is the only kind this is.
`must-revalidate` and `proxy-revalidate` are folded together for the same
reason. `MaxTTL` caps the lifetime the target asks for, so a mistaken
`s-maxage` of a year cannot pin content until the next restart.

## Keys and namespacing

`cacheKeyPrefix` (`kp:c:`) namespaces every key this proxy writes, so a Redis
instance shared with an application's own data cannot collide with it.

## Related

- `../request-path/summary.md` — where the cache sits in the per-request order
- `../observability/summary.md` — the five cache metric families

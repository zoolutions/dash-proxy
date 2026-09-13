# Observability: metrics, logs, traces

Three surfaces, all opt-in-ish and all built so a monitoring concern can never
fail a request.

## Metrics

`internal/metrics/metrics.go`. Sixteen collectors in four families:

| Family | Metrics |
|---|---|
| Requests | `http_requests_total`, `http_request_duration_seconds`, `http_in_flight_requests` |
| Certificates | `certificates_total`, `certificate_expiry_timestamp_seconds`, `certificate_renewals_total`, `certificate_renewals_deferred` |
| Cache | `cache_events_total`, `cache_refusals_total`, `cache_evictions_total`, `cache_leases_total`, `cache_lease_waits_total` |
| Dynamic redirects, denials | `dynamic_redirects_total`, `dynamic_redirect_map_size`, `dynamic_redirect_polls_total`, `denials_total` |

`EventTracker` is the interface every emission goes through; `Tracker` is a
process-wide `delegatingTracker` holding its delegate behind an
`atomic.Pointer`. Installation must not race with request goroutines emitting
through it — and in tests, background work outlives the test that started it,
which is what the atomic swap is really for.

`Enable()` registers the Prometheus collectors exactly once per process
(`enableOnce`): they live in the default registry, which panics on
re-registration, so a second metrics-enabled server — or `go test -count=2` —
must reuse them. Later calls also leave the *active* tracker alone, so a test
fake installed in between keeps receiving events. `SetTracker` returns the
tracker it replaced, so a test can restore it.

Until `Enable` runs, `Tracker` is a no-op. That is why
`DynamicRedirectManager.PublishMetrics` exists: maps restored from state are
installed during boot, before the real tracker, so `run.go` republishes their
gauges once the server is up.

Metrics calls are made **after** the relevant lock is released —
`memoryCacheStore.store` returns its evictions for the caller to report — so
the Prometheus registry never sits behind a request-path mutex.

`--metrics-port` (0 disables) and `--metrics-allow-ip`. `startMetricsServer`
parses the allow list **before** the disabled check, so a typo fails the boot
rather than waiting until someone turns metrics on. The metrics listener is the
first thing `Server.Start` opens.

Per-service, `--exclude-metrics-path` drops a path from request metrics; the
match is on `RoutedTargetPath(r)`, the same value every other downstream check
uses, so a path prefix being stripped does not change what an operator has to
write.

## Logging

One `slog` handler for the whole process: the access log in
`LoggingMiddleware` and the server's own messages both go through
`slog.Default()`.

`--log-format` is `json` (`DefaultLogFormat`, the shape kamal-proxy has always
written, so the default changes nothing) or `text`. `logfmt` is accepted as a
synonym for `text` — slog's text handler writes logfmt, which is what an
operator asking for it wants, and accepting the name saves them guessing. An
empty or blank value means the default, so an unset `LOG_FORMAT` boots rather
than failing.

`--log-request-header` and `--log-response-header` add named headers to the
access log line.

`WithLoggingMiddleware` sits at position 7 of the root chain, which puts it
*below* the ping handler — a liveness probe writes no access-log line at all,
the deliberate cost of an endpoint a monitor hits forever.

## Trace context

`internal/server/trace_context.go`. `--trace-context` is one of three modes:

- `off` — the header is ignored entirely
- `propagate` (`DefaultTraceContextMode`) — the W3C `traceparent` is left
  untouched on the wire, but its trace and span ids are recorded on the logging
  request context
- `generate` — a `traceparent` is minted when the client sent none

`ParseTraceContextMode` treats an empty value as the default. The middleware
sits *inside* `WithLoggingMiddleware` (position 8), because the trace is
recorded on the request context that the logging middleware creates.

## Related

- `../request-path/summary.md` — the middleware chain positions
- `../cache/summary.md` — what each cache metric counts
- `../resilience/summary.md` — what `denials_total` counts

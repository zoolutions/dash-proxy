package server

import (
	"context"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"sync"
	"time"
)

var contextKeyQueueTrace = contextKey("queue-trace")

// queueTrace collects one attempt's timings from the transport. The hooks do not
// all run on the request's goroutine: writes are reported from the transport's
// write loop and the first byte from its read loop, and a target can answer
// before the request is fully written, so every field is behind the lock.
type queueTrace struct {
	lock        sync.Mutex
	waitStarted time.Time
	wait        time.Duration
	lastWrite   time.Time
	firstByteAt time.Time
}

func (q *queueTrace) getConn(now time.Time) {
	q.lock.Lock()
	defer q.lock.Unlock()

	q.waitStarted = now
}

func (q *queueTrace) gotConn(now time.Time) {
	q.lock.Lock()
	defer q.lock.Unlock()

	// Added up, as the transport goes through this again when a pooled connection
	// turns out to be dead, and the time spent trying that one was not a wait.
	q.wait += now.Sub(q.waitStarted)
}

func (q *queueTrace) wroteRequest(now time.Time) {
	q.lock.Lock()
	defer q.lock.Unlock()

	q.lastWrite = now
}

func (q *queueTrace) gotFirstResponseByte(now time.Time) {
	q.lock.Lock()
	defer q.lock.Unlock()

	q.firstByteAt = now
}

// connectionWait is the time spent waiting for a connection to the target, which
// is the time spent queueing behind --target-max-conns.
func (q *queueTrace) connectionWait() time.Duration {
	q.lock.Lock()
	defer q.lock.Unlock()

	return q.wait
}

// upstreamTTFB is zero when the target answered before the request was written.
func (q *queueTrace) upstreamTTFB() time.Duration {
	q.lock.Lock()
	defer q.lock.Unlock()

	if q.lastWrite.IsZero() || q.firstByteAt.IsZero() {
		return 0
	}

	return max(q.firstByteAt.Sub(q.lastWrite), 0)
}

func (q *queueTrace) clientTrace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn:              func(string) { q.getConn(time.Now()) },
		GotConn:              func(httptrace.GotConnInfo) { q.gotConn(time.Now()) },
		WroteRequest:         func(httptrace.WroteRequestInfo) { q.wroteRequest(time.Now()) },
		GotFirstResponseByte: func() { q.gotFirstResponseByte(time.Now()) },
	}
}

// traceRequest arranges for the transport to report on this attempt. It belongs
// in the Rewrite hook, the last place that sees the request before RoundTrip.
func (t *Target) traceRequest(req *httputil.ProxyRequest) {
	trace := &queueTrace{}

	ctx := context.WithValue(req.Out.Context(), contextKeyQueueTrace, trace)
	req.Out = req.Out.WithContext(httptrace.WithClientTrace(ctx, trace.clientTrace()))
}

// recordQueueTime turns an attempt's timings into one observation. What the app
// says wins over what the proxy saw: only the app can see its own backlog.
//
// Only a response from the target gets here. An error the proxy renders itself,
// or an attempt it retries, never reaches ModifyResponse, so a saturated target
// that stops answering records nothing at all.
func (t *Target) recordQueueTime(res *http.Response) {
	trace, _ := res.Request.Context().Value(contextKeyQueueTrace).(*queueTrace)
	if trace == nil {
		return
	}

	queue, source := trace.connectionWait(), queueSourceProxy
	if reported, ok := parseServerTimingQueue(res.Header["Server-Timing"]); ok {
		queue, source = reported, queueSourceApp
	}
	ttfb := trace.upstreamTTFB()

	logging := LoggingRequestContext(res.Request)
	logging.QueueTime = queue
	logging.UpstreamTTFB = ttfb
	logging.Measured = true

	if logging.ExcludeMetrics {
		return
	}

	t.queue.Observe(time.Now(), queue, source)
	t.trackRequest(logging.Service, queue, ttfb, source)
}

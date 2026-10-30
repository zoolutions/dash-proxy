package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testLoggedRequest is a request carrying the logging context the server gives
// every request, so a test can read back what the access log would.
func testLoggedRequest(service string) (*http.Request, *loggingRequestContext) {
	logging := &loggingRequestContext{Service: service}
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	return req.WithContext(context.WithValue(req.Context(), contextKeyRequestContext, logging)), logging
}

func testQueueSnapshot(target *Target) QueueDescription {
	return target.queue.Snapshot(time.Now())
}

func TestTarget_RecordsTheAppsQueueTime(t *testing.T) {
	fake := installFakeTracker(t)

	target := testTarget(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server-Timing", "db;dur=3, queue;dur=12.5")
	})

	req, logging := testLoggedRequest("service1")
	testServeRequestWithTarget(t, target, httptest.NewRecorder(), req)

	queue := testQueueSnapshot(target)
	assert.Equal(t, 1, queue.Requests)
	assert.Equal(t, 1, queue.App)
	assert.Equal(t, 0, queue.Proxy)
	assert.InDelta(t, 12.5, queue.SumMs, 0.001)

	assert.True(t, logging.Measured)
	assert.Equal(t, 12500*time.Microsecond, logging.QueueTime)
	assert.Positive(t, logging.UpstreamTTFB)

	samples := fake.targetRequestSamples()
	require.Len(t, samples, 1)
	assert.Equal(t, "service1", samples[0].service)
	assert.Equal(t, target.Address(), samples[0].target)
	assert.Equal(t, 12500*time.Microsecond, samples[0].queue)
	assert.Equal(t, logging.UpstreamTTFB, samples[0].ttfb)
	assert.Equal(t, queueSourceApp, samples[0].source)
}

// Without the app's word for it, the wait is what the transport spent queueing
// behind --target-max-conns: the second request has nothing to connect to until
// the first one gives its connection back.
func TestTarget_FallsBackOnTheWaitForAConnection(t *testing.T) {
	const held = 100 * time.Millisecond

	var wg sync.WaitGroup
	release := make(chan struct{})
	defer wg.Wait()

	arrived := make(chan struct{}, 2)
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()

	options := defaultTargetOptions
	options.MaxConnsPerHost = 1

	target := testTargetWithOptions(t, options, func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-release
	})

	send := func() {
		defer wg.Done()
		req, _ := testLoggedRequest("service1")
		if claimed, err := target.StartRequest(req); assert.NoError(t, err) {
			target.SendRequest(httptest.NewRecorder(), claimed)
		}
	}

	wg.Add(1)
	go send()

	select {
	case <-arrived:
	case <-time.After(testPoolArrivalTimeout):
		t.Fatal("the first request never reached the target")
	}

	wg.Add(1)
	go send()

	require.Eventually(t, func() bool { return target.InFlight() == 2 }, testPoolArrivalTimeout, time.Millisecond)

	time.Sleep(held)
	unblock()
	wg.Wait()

	queue := testQueueSnapshot(target)
	assert.Equal(t, 2, queue.Requests)
	assert.Equal(t, 2, queue.Proxy)
	assert.Equal(t, 0, queue.App)
	assert.GreaterOrEqual(t, queue.MaxMs, float64(40), "the second request waited for the first one's connection")
}

func TestTarget_RecordsTheUpstreamTTFB(t *testing.T) {
	const wait = 30 * time.Millisecond

	target := testTarget(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(wait)
	})

	req, logging := testLoggedRequest("service1")
	testServeRequestWithTarget(t, target, httptest.NewRecorder(), req)

	assert.True(t, logging.Measured)
	assert.GreaterOrEqual(t, logging.UpstreamTTFB, wait/2)
	assert.Less(t, logging.QueueTime, wait/2, "a slow app is not a queue")
}

// The proxy's response rules run last, so a service stripping Server-Timing from
// what clients see must not blind the measurement.
func TestTarget_ReadsServerTimingBeforeTheResponseRules(t *testing.T) {
	options := defaultTargetOptions
	options.ResponseHeaderRules = HeaderRules{Remove: []string{"Server-Timing"}}

	target := testTargetWithOptions(t, options, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server-Timing", "queue;dur=7")
	})

	req, _ := testLoggedRequest("service1")
	w := httptest.NewRecorder()
	testServeRequestWithTarget(t, target, w, req)

	assert.Equal(t, 1, testQueueSnapshot(target).App)
	assert.Empty(t, w.Header().Get("Server-Timing"))
}

// A value a window could not carry leaves the request measured all the same, by
// the only other number there is.
func TestTarget_UnusableServerTimingFallsBackOnTheProxy(t *testing.T) {
	target := testTarget(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server-Timing", "queue;dur=NaN")
	})

	req, _ := testLoggedRequest("service1")
	testServeRequestWithTarget(t, target, httptest.NewRecorder(), req)

	queue := testQueueSnapshot(target)
	assert.Equal(t, 1, queue.Proxy)
	assert.Equal(t, 0, queue.App)
}

// Documented limit: what the proxy answers itself never reaches ModifyResponse,
// so a saturated target that stops answering reads as empty, not as slow.
func TestTarget_ProxyErrorsLeaveTheWindowEmpty(t *testing.T) {
	fake := installFakeTracker(t)

	release := make(chan struct{})
	defer close(release)

	options := defaultTargetOptions
	options.ResponseTimeout = 20 * time.Millisecond

	target := testTargetWithOptions(t, options, func(w http.ResponseWriter, r *http.Request) {
		<-release
	})

	req, logging := testLoggedRequest("service1")
	w := httptest.NewRecorder()
	testServeRequestWithTarget(t, target, w, req)

	assert.Equal(t, http.StatusGatewayTimeout, w.Code)
	assert.Equal(t, 0, testQueueSnapshot(target).Requests)
	assert.False(t, logging.Measured)
	assert.Empty(t, fake.targetRequestSamples())
}

func TestTarget_ExcludedRequestsAreLoggedButNotCounted(t *testing.T) {
	fake := installFakeTracker(t)

	target := testTarget(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server-Timing", "queue;dur=7")
	})

	req, logging := testLoggedRequest("service1")
	logging.ExcludeMetrics = true
	testServeRequestWithTarget(t, target, httptest.NewRecorder(), req)

	assert.Empty(t, fake.targetRequestSamples())
	assert.Equal(t, 0, testQueueSnapshot(target).Requests)
	assert.True(t, logging.Measured, "excluded requests are still logged")
	assert.Equal(t, 7*time.Millisecond, logging.QueueTime)
}

func TestTarget_ReportsItsInFlightCount(t *testing.T) {
	fake := installFakeTracker(t)

	target := testTarget(t, func(w http.ResponseWriter, r *http.Request) {})

	first, _ := testLoggedRequest("service1")
	second, _ := testLoggedRequest("service1")

	first, err := target.StartRequest(first)
	require.NoError(t, err)
	second, err = target.StartRequest(second)
	require.NoError(t, err)

	count, ok := fake.targetInFlightCount("service1", target.Address())
	assert.True(t, ok)
	assert.Equal(t, 2, count)
	assert.Equal(t, 2, target.InFlight())

	target.endInflightRequest(first)
	target.endInflightRequest(second)

	count, _ = fake.targetInFlightCount("service1", target.Address())
	assert.Equal(t, 0, count)
	assert.Equal(t, 0, target.InFlight())
}

// A target already dropped from the load balancer can still be finishing
// requests. Anything it emits past that point would re-create the series the
// router just deleted, with nothing left to delete them again.
func TestTarget_EmitsNothingOnceForgotten(t *testing.T) {
	fake := installFakeTracker(t)

	target := testTarget(t, func(w http.ResponseWriter, r *http.Request) {})

	req, _ := testLoggedRequest("service1")
	req, err := target.StartRequest(req)
	require.NoError(t, err)

	target.forgetMetrics("service1")
	require.Equal(t, []string{"service1/" + target.Address()}, fake.forgotten())

	before, _ := fake.targetInFlightCount("service1", target.Address())

	target.SendRequest(httptest.NewRecorder(), req)

	after, _ := fake.targetInFlightCount("service1", target.Address())
	assert.Equal(t, before, after)
	assert.Empty(t, fake.targetRequestSamples())

	assert.Equal(t, 1, testQueueSnapshot(target).Requests, "the window dies with the target, so it can go on counting")
}

func TestRetry_OnlyTheAttemptThatAnsweredIsMeasured(t *testing.T) {
	lb := testRetryingLoadBalancer(t, RetryPolicy{TryDuration: time.Second},
		func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("one")) },
		func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("two")) },
	)
	require.NoError(t, lb.WaitUntilHealthy(time.Second))

	lb.Targets()[0].updateState(TargetStateDraining)

	w := httptest.NewRecorder()
	lb.StartRequest(w, httptest.NewRequest(http.MethodGet, "/", nil))()

	assert.Equal(t, "two", w.Body.String())
	assert.Equal(t, 0, testQueueSnapshot(lb.Targets()[0]).Requests)
	assert.Equal(t, 1, testQueueSnapshot(lb.Targets()[1]).Requests)
}

// Unlike the draining target above, this one is really tried: the first attempt
// is forwarded and fails on the wire before the second gets its answer.
func TestRetry_AnAttemptThatFailedOnTheWireIsNotMeasured(t *testing.T) {
	fake := installFakeTracker(t)

	dead, deadAddress := testBackendWithHandler(t, func(w http.ResponseWriter, r *http.Request) {})
	_, liveAddress := testBackendWithHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server-Timing", "queue;dur=7")
	})

	targets, err := NewTargetList([]string{deadAddress, liveAddress}, []string{}, defaultTargetOptions)
	require.NoError(t, err)

	lb := NewLoadBalancer(targets, 0, false).WithRetryPolicy(RetryPolicy{TryDuration: time.Second})
	t.Cleanup(lb.Dispose)
	lb.MarkAllHealthy()
	targets.StopHealthChecks()

	dead.Close()

	// Round robin hands each target every other request, so sending two puts one
	// first attempt on the dead target whichever it starts from.
	for range 2 {
		req, logging := testLoggedRequest("service1")
		w := httptest.NewRecorder()
		lb.StartRequest(w, req)()

		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, liveAddress, logging.Target)
		assert.Equal(t, 7*time.Millisecond, logging.QueueTime)
	}

	assert.Equal(t, 0, testQueueSnapshot(targets[0]).Requests)
	assert.Equal(t, 2, testQueueSnapshot(targets[1]).Requests)

	samples := fake.targetRequestSamples()
	require.Len(t, samples, 2)
	for _, sample := range samples {
		assert.Equal(t, liveAddress, sample.target)
	}
}

// Every response comes through the Rewrite hook today. Should a handler ever not,
// it has to pass through unmeasured rather than panic on the request's goroutine.
func TestTarget_AResponseWithoutATraceIsNotMeasured(t *testing.T) {
	fake := installFakeTracker(t)
	target := testTarget(t, func(w http.ResponseWriter, r *http.Request) {})

	req, logging := testLoggedRequest("service1")
	res := &http.Response{Request: req, Header: http.Header{"Server-Timing": {"queue;dur=7"}}}

	require.NotPanics(t, func() { require.NoError(t, target.modifyResponse(res)) })

	assert.False(t, logging.Measured)
	assert.Equal(t, 0, testQueueSnapshot(target).Requests)
	assert.Empty(t, fake.targetRequestSamples())
}

// A target replaced by another at the same address must stop writing to the
// series the two share, or its last count outlives it.
func TestTarget_EmitsNothingOnceSilenced(t *testing.T) {
	fake := installFakeTracker(t)
	target := testTarget(t, func(w http.ResponseWriter, r *http.Request) {})

	req, _ := testLoggedRequest("service1")
	req, err := target.StartRequest(req)
	require.NoError(t, err)

	target.silenceMetrics()
	target.SendRequest(httptest.NewRecorder(), req)

	count, _ := fake.targetInFlightCount("service1", target.Address())
	assert.Equal(t, 1, count, "the request's end went unreported")
	assert.Empty(t, fake.targetRequestSamples())
	assert.Empty(t, fake.forgotten(), "the series are not this target's alone to delete")
}

// The transport re-fires the hooks when a pooled connection turns out to be dead,
// and writes and reads on goroutines of its own, in whichever order the target
// answers.
func TestQueueTrace(t *testing.T) {
	t.Run("adds up the waits of every attempt", func(t *testing.T) {
		trace := &queueTrace{}
		base := time.Now()

		trace.getConn(base)
		trace.gotConn(base.Add(10 * time.Millisecond))
		trace.getConn(base.Add(50 * time.Millisecond))
		trace.gotConn(base.Add(55 * time.Millisecond))

		assert.Equal(t, 15*time.Millisecond, trace.connectionWait())
	})

	t.Run("measures the first byte from the last write", func(t *testing.T) {
		trace := &queueTrace{}
		base := time.Now()

		trace.wroteRequest(base)
		trace.wroteRequest(base.Add(5 * time.Millisecond))
		trace.gotFirstResponseByte(base.Add(25 * time.Millisecond))

		assert.Equal(t, 20*time.Millisecond, trace.upstreamTTFB())
	})

	t.Run("a target answering before the request is written took no time", func(t *testing.T) {
		trace := &queueTrace{}
		base := time.Now()

		trace.gotFirstResponseByte(base)
		assert.Equal(t, time.Duration(0), trace.upstreamTTFB())

		trace.wroteRequest(base.Add(time.Millisecond))
		assert.Equal(t, time.Duration(0), trace.upstreamTTFB())
	})

	t.Run("nothing recorded is nothing measured", func(t *testing.T) {
		trace := &queueTrace{}

		assert.Equal(t, time.Duration(0), trace.connectionWait())
		assert.Equal(t, time.Duration(0), trace.upstreamTTFB())
	})
}

package metrics

import (
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingTracker captures cache events; everything else is a no-op.
type recordingTracker struct {
	nullTracker
	mu     sync.Mutex
	events map[string]int
}

func (r *recordingTracker) TrackCacheEvent(service, result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.events == nil {
		r.events = map[string]int{}
	}
	r.events[service+":"+result]++
}

func (r *recordingTracker) eventCount(service, result string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.events[service+":"+result]
}

func TestSetTracker_DelegatesEventsAndRestores(t *testing.T) {
	recording := &recordingTracker{}
	previous := SetTracker(recording)
	t.Cleanup(func() { SetTracker(previous) })

	Tracker.TrackCacheEvent("service1", "hit")
	assert.Equal(t, 1, recording.eventCount("service1", "hit"))

	restored := SetTracker(previous)
	assert.Same(t, recording, restored, "SetTracker returns the tracker it replaced")

	Tracker.TrackCacheEvent("service1", "hit")
	assert.Equal(t, 1, recording.eventCount("service1", "hit"), "events after restore must not reach the removed tracker")
}

// Two Enable calls happen whenever a second metrics-enabled Server starts in
// the same process -- most importantly `go test -count=2`. The second call must
// reuse the registered collectors instead of panicking on re-registration.
func TestEnable_IsIdempotent(t *testing.T) {
	require.NotPanics(t, func() {
		require.NotNil(t, Enable())
		require.NotNil(t, Enable())
	})
}

// A tracker installed after Enable (a test fake) must survive further Enable
// calls -- Enable clobbering it is what made cache-metric assertions depend on
// which tests ran before them.
func TestEnable_DoesNotReplaceATrackerInstalledAfterIt(t *testing.T) {
	Enable()

	recording := &recordingTracker{}
	previous := SetTracker(recording)
	t.Cleanup(func() { SetTracker(previous) })

	Enable()

	Tracker.TrackCacheEvent("service1", "miss")
	assert.Equal(t, 1, recording.eventCount("service1", "miss"))
}

// Swapping the tracker while request goroutines emit through it must be free
// of data races (run with -race).
func TestSetTracker_IsSafeUnderConcurrentEmission(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 1000 {
			Tracker.TrackRequest("service1", "GET", 200, time.Millisecond)
		}
	}()

	original := SetTracker(&recordingTracker{})
	for range 100 {
		SetTracker(&recordingTracker{})
	}
	SetTracker(original)
	<-done
}

// testTargetSeries reports, per metric, how many series carry this service and
// target, read back from what a scrape would see.
func testTargetSeries(t *testing.T, service, target string) map[string]int {
	t.Helper()

	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)

	series := map[string]int{}
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}

			if labels["service"] == service && labels["target"] == target {
				series[family.GetName()]++
			}
		}
	}

	return series
}

func TestPrometheusTracker_TargetMetrics(t *testing.T) {
	Enable()

	Tracker.TrackTargetRequest("service1", "web-1:3000", 12*time.Millisecond, 40*time.Millisecond, "app")
	Tracker.SetTargetInFlight("service1", "web-1:3000", 3)

	assert.Equal(t, map[string]int{
		"kamal_proxy_target_queue_seconds":         1,
		"kamal_proxy_target_upstream_ttfb_seconds": 1,
		"kamal_proxy_target_in_flight":             1,
	}, testTargetSeries(t, "service1", "web-1:3000"))
}

func TestPrometheusTracker_ForgetTargetDropsOnlyThatTarget(t *testing.T) {
	Enable()

	for _, target := range []string{"web-2:3000", "web-3:3000"} {
		Tracker.TrackTargetRequest("service2", target, time.Millisecond, time.Millisecond, "proxy")
		Tracker.SetTargetInFlight("service2", target, 1)
	}
	Tracker.SetTargetInFlight("service3", "web-2:3000", 1)

	Tracker.ForgetTarget("service2", "web-2:3000")

	assert.Empty(t, testTargetSeries(t, "service2", "web-2:3000"))
	assert.Len(t, testTargetSeries(t, "service2", "web-3:3000"), 3)
	assert.Len(t, testTargetSeries(t, "service3", "web-2:3000"), 1, "the same address on another service is a different target")
}

// The window and the histogram must bucket a request alike, so a dashboard and
// the autoscaler cannot disagree about which side of a bound it fell on.
func TestTargetQueueBucketsSeconds_MirrorTheWindowsBounds(t *testing.T) {
	buckets := targetQueueBucketsSeconds()

	require.Len(t, buckets, len(TargetQueueBucketsMs))
	assert.InDelta(t, 0.001, buckets[0], 1e-9)
	assert.InDelta(t, 5, buckets[len(buckets)-1], 1e-9)
}

// What each request pays with --metrics-port set. SetTargetInFlight runs twice per
// request, and on the way in that is under the load balancer's lock.
func BenchmarkPrometheusTracker_SetTargetInFlight(b *testing.B) {
	Enable()

	b.ReportAllocs()
	for b.Loop() {
		Tracker.SetTargetInFlight("service1", "web-1:3000", 3)
	}
}

func BenchmarkPrometheusTracker_TrackTargetRequest(b *testing.B) {
	Enable()

	b.ReportAllocs()
	for b.Loop() {
		Tracker.TrackTargetRequest("service1", "web-1:3000", 12*time.Millisecond, 40*time.Millisecond, "app")
	}
}

func TestNormalizeMethod(t *testing.T) {
	assert.Equal(t, "GET", normalizeMethod("GET"))
	assert.Equal(t, "POST", normalizeMethod("POST"))
	assert.Equal(t, "PATCH", normalizeMethod("PATCH"))

	assert.Equal(t, "OTHER", normalizeMethod("CUSTOM"))
	assert.Equal(t, "OTHER", normalizeMethod("OTHER"))
	assert.Equal(t, "OTHER", normalizeMethod(""))
}

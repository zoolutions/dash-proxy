package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testQueueNow = time.Unix(1_800_000_000, 0)

func testQueueBucket(t *testing.T, queue QueueDescription, leMs float64) int {
	t.Helper()

	for _, bucket := range queue.Buckets {
		if bucket.LeMs == leMs {
			return bucket.Count
		}
	}

	require.Failf(t, "no such bucket", "le_ms=%v", leMs)
	return 0
}

func TestQueueWindow_EmptyWindow(t *testing.T) {
	queue := newQueueWindow().Snapshot(testQueueNow)

	assert.Equal(t, queueWindowSeconds, queue.WindowSeconds)
	assert.Equal(t, 0, queue.Requests)
	assert.Equal(t, float64(0), queue.P95Ms)
	assert.Equal(t, float64(0), queue.SumMs)
	assert.Equal(t, float64(0), queue.MaxMs)
	assert.Len(t, queue.Buckets, 12)
}

func TestQueueWindow_ObservationsLandInTheirBucket(t *testing.T) {
	tests := []struct {
		name  string
		queue time.Duration
		leMs  float64
		below float64
	}{
		{name: "zero", queue: 0, leMs: 1},
		{name: "exactly on a bound", queue: 5 * time.Millisecond, leMs: 5, below: 2},
		{name: "just over a bound", queue: 5*time.Millisecond + time.Microsecond, leMs: 10, below: 5},
		{name: "the last finite bound", queue: 5 * time.Second, leMs: 5000, below: 2500},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			window := newQueueWindow()
			window.Observe(testQueueNow, tt.queue, queueSourceProxy)

			queue := window.Snapshot(testQueueNow)

			assert.Equal(t, 1, testQueueBucket(t, queue, tt.leMs))
			if tt.below > 0 {
				assert.Equal(t, 0, testQueueBucket(t, queue, tt.below))
			}
		})
	}
}

func TestQueueWindow_BucketsAreCumulative(t *testing.T) {
	window := newQueueWindow()
	window.Observe(testQueueNow, time.Millisecond, queueSourceProxy)
	window.Observe(testQueueNow, 7*time.Millisecond, queueSourceProxy)
	window.Observe(testQueueNow, 7*time.Second, queueSourceProxy)

	queue := window.Snapshot(testQueueNow)

	assert.Equal(t, 1, testQueueBucket(t, queue, 1))
	assert.Equal(t, 1, testQueueBucket(t, queue, 5))
	assert.Equal(t, 2, testQueueBucket(t, queue, 10))
	assert.Equal(t, 2, testQueueBucket(t, queue, 5000), "what exceeds the last bound shows only in requests")
	assert.Equal(t, 3, queue.Requests)
}

func TestQueueWindow_TalliesSumMaxAndSource(t *testing.T) {
	window := newQueueWindow()
	window.Observe(testQueueNow, 12500*time.Microsecond, queueSourceApp)
	window.Observe(testQueueNow.Add(time.Second), 30*time.Millisecond, queueSourceApp)
	window.Observe(testQueueNow.Add(time.Second), 2*time.Millisecond, queueSourceProxy)

	queue := window.Snapshot(testQueueNow.Add(time.Second))

	assert.Equal(t, 3, queue.Requests)
	assert.Equal(t, 2, queue.App)
	assert.Equal(t, 1, queue.Proxy)
	assert.InDelta(t, 44.5, queue.SumMs, 0.001)
	assert.InDelta(t, 30, queue.MaxMs, 0.001)
}

func TestQueueWindow_SlotsAgeOutOfTheSnapshot(t *testing.T) {
	window := newQueueWindow()
	window.Observe(testQueueNow, time.Millisecond, queueSourceProxy)

	assert.Equal(t, 1, window.Snapshot(testQueueNow.Add(59*time.Second)).Requests)
	assert.Equal(t, 0, window.Snapshot(testQueueNow.Add(60*time.Second)).Requests)
}

// A slot is reused every 60 seconds, so a quiet minute must not leave the
// previous lap's counts for the next observation to add to.
func TestQueueWindow_ReusedSlotStartsEmpty(t *testing.T) {
	window := newQueueWindow()
	window.Observe(testQueueNow, time.Second, queueSourceApp)

	later := testQueueNow.Add(3 * queueWindowSeconds * time.Second)
	window.Observe(later, time.Millisecond, queueSourceProxy)

	queue := window.Snapshot(later)

	assert.Equal(t, 1, queue.Requests)
	assert.Equal(t, 0, queue.App)
	assert.InDelta(t, 1, queue.MaxMs, 0.001)
}

func TestQueueWindow_SkippedSecondsReadAsEmpty(t *testing.T) {
	window := newQueueWindow()
	window.Observe(testQueueNow, time.Millisecond, queueSourceProxy)
	window.Observe(testQueueNow.Add(30*time.Second), time.Millisecond, queueSourceProxy)
	window.Observe(testQueueNow.Add(70*time.Second), time.Millisecond, queueSourceProxy)

	assert.Equal(t, 2, window.Snapshot(testQueueNow.Add(70*time.Second)).Requests)
}

// A wall clock stepped backwards leaves slots stamped in the future. They cannot
// be told from valid ones by age alone, and must not be read as current.
func TestQueueWindow_IgnoresSlotsFromTheFuture(t *testing.T) {
	window := newQueueWindow()
	window.Observe(testQueueNow.Add(time.Hour+7*time.Second), time.Millisecond, queueSourceProxy)
	window.Observe(testQueueNow, time.Millisecond, queueSourceProxy)

	assert.Equal(t, 1, window.Snapshot(testQueueNow).Requests)
}

func TestQueueWindow_P95(t *testing.T) {
	tests := []struct {
		name     string
		observe  map[time.Duration]int
		expected float64
	}{
		{
			name:     "interpolates inside the first bucket from zero",
			observe:  map[time.Duration]int{time.Millisecond: 100},
			expected: 0.95,
		},
		{
			name:     "interpolates inside a later bucket",
			observe:  map[time.Duration]int{8 * time.Millisecond: 100},
			expected: 5 + 5*0.95,
		},
		{
			name: "takes the bucket the rank falls in",
			observe: map[time.Duration]int{
				time.Millisecond:       50,
				200 * time.Millisecond: 50,
			},
			expected: 100 + 150*0.9,
		},
		{
			name: "above the last bound reports that bound",
			observe: map[time.Duration]int{
				time.Millisecond: 90,
				time.Minute:      10,
			},
			expected: 5000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			window := newQueueWindow()
			for queue, count := range tt.observe {
				for range count {
					window.Observe(testQueueNow, queue, queueSourceProxy)
				}
			}

			assert.InDelta(t, tt.expected, window.Snapshot(testQueueNow).P95Ms, 0.001)
		})
	}
}

// encoding/json refuses +Inf, so the implicit last bucket must never appear as
// one, or `list --json` stops working.
func TestQueueWindow_SnapshotMarshalsToJSON(t *testing.T) {
	window := newQueueWindow()
	window.Observe(testQueueNow, time.Hour, queueSourceApp)

	encoded, err := json.Marshal(window.Snapshot(testQueueNow))
	require.NoError(t, err)

	for _, key := range []string{`"window_seconds":60`, `"requests":1`, `"sum_ms":`, `"max_ms":`, `"p95_ms":5000`, `"app":1`, `"proxy":0`, `"buckets":[{"le_ms":1,"count":0}`} {
		assert.Contains(t, string(encoded), key)
	}
}

func BenchmarkQueueWindow_Observe(b *testing.B) {
	window := newQueueWindow()
	now := time.Now()

	b.ReportAllocs()
	for b.Loop() {
		window.Observe(now, 12*time.Millisecond, queueSourceApp)
	}
}

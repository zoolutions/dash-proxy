package server

import (
	"sort"
	"sync"
	"time"

	"github.com/basecamp/kamal-proxy/internal/metrics"
)

const (
	// queueWindowSeconds is how far back a target's queue times are kept. It is a
	// constant rather than a flag: a new flag fails the gem's flag-coverage test
	// at the version bump. list --json reports it as window_seconds.
	queueWindowSeconds = 60

	queueSourceApp   = "app"
	queueSourceProxy = "proxy"

	queueQuantile = 0.95
)

// QueueBucket is one cumulative bucket: how many of the window's requests waited
// LeMs or less. Counts are cumulative, as a Prometheus le is, so that a reader
// can merge targets and hosts by adding them up. There is no +Inf bucket, which
// JSON could not carry: Requests is that count.
type QueueBucket struct {
	LeMs  float64 `json:"le_ms"`
	Count int     `json:"count"`
}

// QueueDescription is a target's trailing window of queue times. P95Ms is the
// proxy's own reading of Buckets, for humans.
type QueueDescription struct {
	WindowSeconds int           `json:"window_seconds"`
	Requests      int           `json:"requests"`
	SumMs         float64       `json:"sum_ms"`
	MaxMs         float64       `json:"max_ms"`
	P95Ms         float64       `json:"p95_ms"`
	App           int           `json:"app"`
	Proxy         int           `json:"proxy"`
	Buckets       []QueueBucket `json:"buckets"`
}

// queueSlot is one second of observations. The extra bucket is +Inf.
type queueSlot struct {
	second  int64
	buckets [len(metrics.TargetQueueBucketsMs) + 1]int
	count   int
	app     int
	proxy   int
	sumMs   float64
	maxMs   float64
}

// queueWindow keeps the last queueWindowSeconds of queue times in a ring of
// one-second slots, so that a single read describes the recent past on its own:
// a one-shot reader has no earlier scrape to diff cumulative histograms against.
//
// A slot is only ever reused for the second that maps to it, and a slot's second
// is checked on the way out, so a second nobody observed in reads as empty
// without anything having to sweep the ring.
type queueWindow struct {
	lock  sync.Mutex
	slots [queueWindowSeconds]queueSlot
}

func newQueueWindow() *queueWindow {
	return &queueWindow{}
}

func (w *queueWindow) Observe(now time.Time, queue time.Duration, source string) {
	second := now.Unix()
	ms := float64(queue) / float64(time.Millisecond)
	bucket := sort.SearchFloat64s(metrics.TargetQueueBucketsMs[:], ms)

	w.lock.Lock()
	defer w.lock.Unlock()

	slot := &w.slots[second%queueWindowSeconds]
	if slot.second != second {
		*slot = queueSlot{second: second}
	}

	slot.buckets[bucket]++
	slot.count++
	slot.sumMs += ms
	slot.maxMs = max(slot.maxMs, ms)

	if source == queueSourceApp {
		slot.app++
	} else {
		slot.proxy++
	}
}

func (w *queueWindow) Snapshot(now time.Time) QueueDescription {
	second := now.Unix()
	description := QueueDescription{WindowSeconds: queueWindowSeconds}

	var counts [len(metrics.TargetQueueBucketsMs) + 1]int

	w.lock.Lock()
	for i := range w.slots {
		slot := &w.slots[i]

		// Negative when the wall clock stepped back after this slot was written.
		age := second - slot.second
		if age < 0 || age >= queueWindowSeconds {
			continue
		}

		for bucket, count := range slot.buckets {
			counts[bucket] += count
		}
		description.Requests += slot.count
		description.App += slot.app
		description.Proxy += slot.proxy
		description.SumMs += slot.sumMs
		description.MaxMs = max(description.MaxMs, slot.maxMs)
	}
	w.lock.Unlock()

	description.Buckets = make([]QueueBucket, len(metrics.TargetQueueBucketsMs))
	cumulative := 0
	for i, bound := range metrics.TargetQueueBucketsMs {
		cumulative += counts[i]
		description.Buckets[i] = QueueBucket{LeMs: bound, Count: cumulative}
	}

	description.P95Ms = bucketQuantile(queueQuantile, description.Buckets, description.Requests)

	return description
}

// bucketQuantile follows Prometheus' histogram_quantile: find the bucket the
// rank falls in and interpolate linearly inside it. A rank past the last finite
// bound reports that bound, there being nothing above it to interpolate towards.
func bucketQuantile(quantile float64, buckets []QueueBucket, total int) float64 {
	if total == 0 {
		return 0
	}

	rank := quantile * float64(total)
	lowerBound, lowerCount := 0.0, 0

	for _, bucket := range buckets {
		if float64(bucket.Count) >= rank {
			position := (rank - float64(lowerCount)) / float64(bucket.Count-lowerCount)
			return lowerBound + (bucket.LeMs-lowerBound)*position
		}
		lowerBound, lowerCount = bucket.LeMs, bucket.Count
	}

	return lowerBound
}

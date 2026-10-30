package server

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestParseServerTimingQueue(t *testing.T) {
	tests := []struct {
		name     string
		headers  []string
		expected time.Duration
		found    bool
	}{
		{name: "no header"},
		{name: "empty header", headers: []string{""}},
		{name: "another metric", headers: []string{"db;dur=3"}},
		{name: "a metric the name only starts with", headers: []string{"queuetime;dur=3"}},

		{name: "alone", headers: []string{"queue;dur=12.5"}, expected: 12500 * time.Microsecond, found: true},
		{name: "zero is a reading", headers: []string{"queue;dur=0"}, found: true},
		{name: "among others", headers: []string{"db;dur=3, queue;dur=12.5, cache;desc=hit"}, expected: 12500 * time.Microsecond, found: true},
		{name: "params in any order", headers: []string{`queue;desc="in puma";dur=5`}, expected: 5 * time.Millisecond, found: true},
		{name: "case-insensitive", headers: []string{"QUEUE;DUR=7"}, expected: 7 * time.Millisecond, found: true},
		{name: "optional whitespace", headers: []string{"db ; dur=1 ,  queue ; dur = 7 "}, expected: 7 * time.Millisecond, found: true},
		{name: "quoted value", headers: []string{`queue;dur="7"`}, expected: 7 * time.Millisecond, found: true},
		{name: "several header lines", headers: []string{"db;dur=1", "queue;dur=4"}, expected: 4 * time.Millisecond, found: true},
		{name: "the first of several wins", headers: []string{"queue;dur=1, queue;dur=2"}, expected: time.Millisecond, found: true},

		{name: "missing dur", headers: []string{"queue"}},
		{name: "missing dur with another param", headers: []string{`queue;desc="x"`}},
		{name: "empty dur", headers: []string{"queue;dur="}},
		{name: "dur without a value", headers: []string{"queue;dur"}},
		{name: "non-numeric dur", headers: []string{"queue;dur=soon"}},

		// All of these reach the window otherwise, and JSON cannot carry the first
		// three: one of them from the app would fail every `list --json`.
		{name: "NaN", headers: []string{"queue;dur=NaN"}},
		{name: "infinity", headers: []string{"queue;dur=Inf"}},
		{name: "out of float range", headers: []string{"queue;dur=1e999"}},
		{name: "out of duration range", headers: []string{"queue;dur=1e300"}},
		{name: "negative", headers: []string{"queue;dur=-1"}},

		{name: "a separator inside a quoted string", headers: []string{`db;desc="a, queue;dur=9"`}},
		{name: "an escaped quote inside a quoted string", headers: []string{`db;desc="a\", queue;dur=9"`}},
		{name: "past a quoted separator", headers: []string{`db;desc="a, queue;dur=9", queue;dur=2`}, expected: 2 * time.Millisecond, found: true},
		{name: "an unterminated quote swallows the rest", headers: []string{`db;desc="a, queue;dur=9`}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			queue, found := parseServerTimingQueue(tt.headers)

			assert.Equal(t, tt.found, found)
			assert.Equal(t, tt.expected, queue)
		})
	}
}

func BenchmarkParseServerTimingQueue(b *testing.B) {
	headers := []string{`db;dur=53.2, app;desc="render";dur=47.2, queue;dur=12.5`}

	b.ReportAllocs()
	for b.Loop() {
		parseServerTimingQueue(headers)
	}
}

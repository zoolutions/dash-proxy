package server

import (
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	serverTimingQueueMetric = "queue"
	serverTimingDuration    = "dur"

	// maxServerTimingMs is the longest dur a time.Duration can hold.
	maxServerTimingMs = float64(math.MaxInt64 / int64(time.Millisecond))
)

// parseServerTimingQueue finds the app's `Server-Timing: queue;dur=<ms>` among a
// response's Server-Timing header lines. The first queue entry with a usable dur
// wins. A dur that is not a usable duration counts as no answer at all, so the
// caller falls back on what the proxy measured itself.
func parseServerTimingQueue(headers []string) (time.Duration, bool) {
	for _, header := range headers {
		for _, entry := range splitUnquoted(header, ',') {
			params := splitUnquoted(entry, ';')
			if !strings.EqualFold(strings.TrimSpace(params[0]), serverTimingQueueMetric) {
				continue
			}

			for _, param := range params[1:] {
				if queue, ok := parseServerTimingDuration(param); ok {
					return queue, true
				}
			}
		}
	}

	return 0, false
}

// parseServerTimingDuration rejects more than ParseFloat does. It accepts NaN and
// Inf, and the app is what wrote this: either one in a window would make every
// `list --json` fail to marshal.
func parseServerTimingDuration(param string) (time.Duration, bool) {
	name, value, found := strings.Cut(param, "=")
	if !found || !strings.EqualFold(strings.TrimSpace(name), serverTimingDuration) {
		return 0, false
	}

	ms, err := strconv.ParseFloat(strings.Trim(strings.TrimSpace(value), `"`), 64)
	if err != nil || math.IsNaN(ms) || ms < 0 || ms > maxServerTimingMs {
		return 0, false
	}

	return time.Duration(ms * float64(time.Millisecond)), true
}

// splitUnquoted splits on separator, leaving separators inside a quoted string
// alone: a desc is free text, and may well read `queue;dur=9`.
func splitUnquoted(value string, separator byte) []string {
	parts := []string{}
	start := 0
	quoted := false

	for i := 0; i < len(value); i++ {
		switch {
		case quoted && value[i] == '\\':
			i++
		case value[i] == '"':
			quoted = !quoted
		case value[i] == separator && !quoted:
			parts = append(parts, value[start:i])
			start = i + 1
		}
	}

	return append(parts, value[start:])
}

package server

import (
	"fmt"
	"net/http"
	"time"
)

const (
	requestStartHeader     = "X-Request-Start"
	dashRequestStartHeader = "X-Dash-Request-Start"
)

type RequestStartMiddleware struct {
	next http.Handler
}

func WithRequestStartMiddleware(next http.Handler) http.Handler {
	return &RequestStartMiddleware{
		next: next,
	}
}

func (h *RequestStartMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestStart := fmt.Sprintf("t=%d", time.Now().UnixMilli())

	if r.Header.Get(requestStartHeader) == "" {
		r.Header.Set(requestStartHeader, requestStart)
	}

	// Overwritten at every hop, unlike the header above: an app behind a load
	// balancer has to read a timestamp from its own host's clock.
	r.Header.Set(dashRequestStartHeader, requestStart)

	h.next.ServeHTTP(w, r)
}

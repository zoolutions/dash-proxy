package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestStartMiddleware_AddsUnixMilliWhenNotPresent(t *testing.T) {
	handler := WithRequestStartMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headerValue := r.Header.Get("X-Request-Start")

		assert.Regexp(t, `^t=\d+$`, headerValue, "Value should be formatted as `t=<timestamp>`")

		requestStartMilli, _ := strconv.ParseInt(headerValue[2:], 10, 64)
		requestStart := time.UnixMilli(requestStartMilli)
		assert.WithinDuration(t, time.Now(), requestStart, time.Second)
	}))

	r := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRequestStartMiddleware_DashRequestStart(t *testing.T) {
	tests := []struct {
		name  string
		prior string
	}{
		{name: "set when absent"},
		{name: "overwritten when present", prior: "t=1234"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var headerValue string
			handler := WithRequestStartMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				headerValue = r.Header.Get("X-Dash-Request-Start")
			}))

			r := httptest.NewRequest("GET", "/", nil)
			if tt.prior != "" {
				r.Header.Set("X-Dash-Request-Start", tt.prior)
			}
			handler.ServeHTTP(httptest.NewRecorder(), r)

			assert.Regexp(t, `^t=\d+$`, headerValue)

			requestStartMilli, err := strconv.ParseInt(headerValue[2:], 10, 64)
			assert.NoError(t, err)
			assert.WithinDuration(t, time.Now(), time.UnixMilli(requestStartMilli), time.Second)
		})
	}
}

// An app behind a load balancer reads this header against its own host's clock,
// so a value from a previous hop must not survive under any spelling of it.
func TestRequestStartMiddleware_DashRequestStartReplacesEveryPriorValue(t *testing.T) {
	var values []string
	handler := WithRequestStartMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values = r.Header.Values("X-Dash-Request-Start")
	}))

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Add("X-Dash-Request-Start", "t=1")
	r.Header.Add("X-Dash-Request-Start", "t=2")
	handler.ServeHTTP(httptest.NewRecorder(), r)

	require.Len(t, values, 1)
	assert.Regexp(t, `^t=\d+$`, values[0])
	assert.NotContains(t, []string{"t=1", "t=2"}, values[0])
}

func TestRequestStartMiddleware_PreservesExistingHeaderWhenPresent(t *testing.T) {
	handler := WithRequestStartMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "t=1234", r.Header.Get("X-Request-Start"))
	}))

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set(requestStartHeader, "t=1234")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	assert.Equal(t, http.StatusOK, w.Code)
}

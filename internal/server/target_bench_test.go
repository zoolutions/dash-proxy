package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// The whole trip through a target: claim, proxy over loopback, release. The
// request carries a logging context, as every request behind the server does.
func BenchmarkTarget_SendRequest(b *testing.B) {
	target := testTarget(b, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	b.ReportAllocs()

	for b.Loop() {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		ctx := context.WithValue(req.Context(), contextKeyRequestContext, &loggingRequestContext{Service: "service1"})

		claimed, err := target.StartRequest(req.WithContext(ctx))
		require.NoError(b, err)

		target.SendRequest(httptest.NewRecorder(), claimed)
	}
}

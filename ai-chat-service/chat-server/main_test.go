package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMetricsAuth(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("metrics"))
	})

	t.Run("no token configured leaves handler open", func(t *testing.T) {
		rec := httptest.NewRecorder()
		metricsAuth(ok, "").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("rejects missing or wrong bearer", func(t *testing.T) {
		h := metricsAuth(ok, "secret")
		for _, auth := range []string{"", "Bearer nope", "Basic secret"} {
			req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			if auth != "" {
				req.Header.Set("Authorization", auth)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("Authorization %q: status = %d, want 401", auth, rec.Code)
			}
		}
	})

	t.Run("accepts correct bearer", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		metricsAuth(ok, "secret").ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})
}

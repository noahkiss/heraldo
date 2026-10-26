package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/noahkiss/heraldo/api"
)

func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	New("v-test").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	var h api.Health
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	if h.Status != api.HealthStatusOk || h.Version != "v-test" {
		t.Fatalf("body = %+v", h)
	}
}

func TestUnknownPath(t *testing.T) {
	rec := httptest.NewRecorder()
	New("v").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/events", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 until stage 1", rec.Code)
	}
}

func TestHealthzMethod(t *testing.T) {
	rec := httptest.NewRecorder()
	New("v").ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

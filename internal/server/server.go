// Package server holds the heraldo HTTP handlers.
package server

import (
	"encoding/json"
	"net/http"

	"github.com/noahkiss/heraldo/api"
)

// New returns the server's HTTP handler. version is reported by /healthz.
func New(version string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, api.Health{Status: api.HealthStatusOk, Version: version})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

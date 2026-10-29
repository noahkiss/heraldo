package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/store"
)

// PushTrace is the push trace of one event (GET /v1/events/{id}/why).
func PushTrace(id string, rows []store.DecisionRow) api.PushTrace {
	t := api.PushTrace{EventId: id, Decisions: make([]api.PushDecision, 0, len(rows))}
	if sum := store.DecisionSummary(rows); sum != "" {
		t.Why = &sum
	}
	for _, r := range rows {
		d := api.PushDecision{Stage: r.Stage, Outcome: api.PushDecisionOutcome(r.Outcome), Reason: r.Reason, At: r.At}
		if len(r.Detail) > 0 {
			var m map[string]any
			if json.Unmarshal(r.Detail, &m) == nil {
				d.Detail = &m
			}
		}
		t.Decisions = append(t.Decisions, d)
	}
	return t
}

func (s *server) getEventWhy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.Get(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no event with that id")
		return
	} else if err != nil {
		s.Logger.Error("why", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	rows, err := s.Store.Decisions(r.Context(), id)
	if err != nil {
		s.Logger.Error("why", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, PushTrace(id, rows))
}

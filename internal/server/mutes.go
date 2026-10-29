package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/ids"
	"github.com/noahkiss/heraldo/internal/store"
	"github.com/noahkiss/heraldo/internal/validate"
)

// MaxMute is the longest a mute may last.
const MaxMute = 366 * 24 * time.Hour

func (s *server) muteJSON(m store.Mute) api.Mute {
	out := api.Mute{Id: m.ID, Until: m.Until, CreatedAt: m.CreatedAt, Active: m.Active(s.Now())}
	if m.Source != "" {
		out.Source = &m.Source
	}
	if m.Key != "" {
		out.Key = &m.Key
	}
	if m.Project != "" {
		out.Project = &m.Project
	}
	if !m.EndedAt.IsZero() {
		t := m.EndedAt
		out.EndedAt = &t
	}
	if m.Remind {
		yes := true
		out.Remind = &yes
	}
	if !m.RemindedAt.IsZero() {
		t := m.RemindedAt
		out.RemindedAt = &t
	}
	return out
}

func (s *server) listMutes(w http.ResponseWriter, r *http.Request) {
	ms, err := s.Store.Mutes(r.Context(), s.Now())
	if err != nil {
		s.Logger.Error("mutes", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := api.MuteList{Mutes: make([]api.Mute, 0, len(ms))}
	for _, m := range ms {
		out.Mutes = append(out.Mutes, s.muteJSON(m))
	}
	writeJSON(w, http.StatusOK, out)
}

// createMute is POST /v1/mutes.
func (s *server) createMute(w http.ResponseWriter, r *http.Request) {
	var in api.MuteInput
	dec := json.NewDecoder(io.LimitReader(r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "want a MuteInput: %v", err)
		return
	}
	now := s.Now().UTC()
	m := store.Mute{CreatedAt: now, CreatedBy: tokenFrom(r).Name}
	var problems []string
	if in.Source != nil {
		m.Source = *in.Source
	}
	if in.Key != nil {
		m.Key = *in.Key
	}
	if in.Project != nil {
		switch p := validate.FoldProject(*in.Project); {
		case p == "":
		case validate.ValidProject(p):
			// Events filed under an alias of the name match too.
			m.Project = s.Rules.Rules().Alias(p)
		default:
			problems = append(problems, fmt.Sprintf("project: %q is not a project name", p))
		}
	}
	if m.Source == "" && m.Key == "" && m.Project == "" {
		problems = append(problems, "give source, key, project, or a mix")
	}
	if in.Remind != nil && *in.Remind {
		m.Remind = true
		if m.Key == "" {
			problems = append(problems, "remind needs key: a snooze reminds of one incident")
		}
	}
	if len(m.Source) > 128 || len(m.Key) > 255 {
		problems = append(problems, "source is at most 128 bytes and key at most 255")
	}
	switch {
	case in.Until != nil && in.For != nil:
		problems = append(problems, "give until or for, not both")
	case in.Until != nil:
		m.Until = in.Until.UTC()
	case in.For != nil:
		if d, err := time.ParseDuration(*in.For); err != nil {
			problems = append(problems, "for: want a duration such as 1h or 90m")
		} else {
			m.Until = now.Add(d)
		}
	default:
		problems = append(problems, "give until or for")
	}
	if !m.Until.IsZero() && (!m.Until.After(now) || m.Until.Sub(now) > MaxMute) {
		problems = append(problems, "the mute must end in the future, at most 366 days ahead")
	}
	if len(problems) > 0 {
		writeError(w, http.StatusBadRequest, "%s", strings.Join(problems, "; "))
		return
	}
	m.ID = "mute_" + ids.New(now)
	if err := s.Store.AddMute(r.Context(), m); err != nil {
		s.Logger.Error("mute", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.Logger.Info("mute", "id", m.ID, "source", m.Source, "key", m.Key, "project", m.Project, "until", m.Until, "remind", m.Remind, "token", m.CreatedBy)
	writeJSON(w, http.StatusCreated, s.muteJSON(m))
}

// endMute is DELETE /v1/mutes/{id}.
func (s *server) endMute(w http.ResponseWriter, r *http.Request) {
	m, err := s.Store.EndMute(r.Context(), r.PathValue("id"), tokenFrom(r).Name, s.Now())
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "no mute with that id")
	case err != nil:
		s.Logger.Error("end mute", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	default:
		writeJSON(w, http.StatusOK, s.muteJSON(m))
	}
}

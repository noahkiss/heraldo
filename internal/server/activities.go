package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/store"
)

// Live drives Live Activities. *live.Manager is one. Stored, Confirm and
// Report must not wait on APNs longer than one push.
type Live interface {
	// Stored is called for each new progress event; it must not block.
	Stored(eventID, key, action string)
	// Resync starts and refreshes every running activity, such as after a
	// device reported a new push-to-start token. Called in the background.
	Resync(ctx context.Context)
	Enqueue(job func(context.Context))
	Confirm(ctx context.Context, id, token string) error
	Report(ctx context.Context, id, state string) error
}

// timelineLimit caps the timeline on GET /v1/activities/{key}.
const timelineLimit = 500

var liveToken = regexp.MustCompile(`^[0-9A-Fa-f]{32,400}$`)

// activityJSON is api.Activity with the content kept as stored JSON.
type activityJSON struct {
	Key       string              `json:"key"`
	ID        string              `json:"id"`
	State     string              `json:"state"`
	Source    string              `json:"source"`
	Content   json.RawMessage     `json:"content"`
	StartedAt string              `json:"started_at"`
	UpdatedAt string              `json:"updated_at"`
	EndedAt   string              `json:"ended_at,omitempty"`
	EndReason string              `json:"end_reason,omitempty"`
	Instances []instanceJSON      `json:"instances"`
	Timeline  *[]timelineEntryOut `json:"timeline,omitempty"`
}

type instanceJSON struct {
	ID          string `json:"id"`
	Device      string `json:"device"`
	Chain       int    `json:"chain"`
	State       string `json:"state"`
	EndReason   string `json:"end_reason,omitempty"`
	RequestedAt string `json:"requested_at"`
	ConfirmedAt string `json:"confirmed_at,omitempty"`
	EndedAt     string `json:"ended_at,omitempty"`
	LastPushAt  string `json:"last_push_at,omitempty"`
}

type timelineEntryOut struct {
	ID         string          `json:"id"`
	ReceivedAt string          `json:"received_at"`
	Action     string          `json:"action"`
	Content    json.RawMessage `json:"content,omitempty"`
}

func rfc(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func instanceOut(i store.Instance) instanceJSON {
	return instanceJSON{
		ID: i.ID, Device: i.DeviceID, Chain: i.Chain, State: i.State, EndReason: i.EndReason,
		RequestedAt: rfc(i.RequestedAt), ConfirmedAt: rfc(i.ConfirmedAt), EndedAt: rfc(i.EndedAt), LastPushAt: rfc(i.LastPushAt),
	}
}

func (s *server) activityOut(ctx context.Context, a store.Activity) (activityJSON, error) {
	out := activityJSON{
		Key: a.Key, ID: a.ID, State: a.State, Source: a.Source, Content: a.Content,
		StartedAt: rfc(a.StartedAt), UpdatedAt: rfc(a.UpdatedAt), EndedAt: rfc(a.EndedAt), EndReason: a.EndReason,
		Instances: []instanceJSON{},
	}
	insts, err := s.Store.Instances(ctx, a.ID)
	if err != nil {
		return out, err
	}
	for _, i := range insts {
		out.Instances = append(out.Instances, instanceOut(i))
	}
	return out, nil
}

// listActivities is GET /v1/activities: running activities, or every one
// with all=1.
func (s *server) listActivities(w http.ResponseWriter, r *http.Request) {
	all := false
	switch r.URL.Query().Get("all") {
	case "", "0":
	case "1":
		all = true
	default:
		writeError(w, http.StatusBadRequest, "all: want 0 or 1")
		return
	}
	acts, err := s.Store.Activities(r.Context(), !all)
	if err != nil {
		s.Logger.Error("activities", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := struct {
		Activities []activityJSON `json:"activities"`
	}{Activities: []activityJSON{}}
	for _, a := range acts {
		j, err := s.activityOut(r.Context(), a)
		if err != nil {
			s.Logger.Error("activities", "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		out.Activities = append(out.Activities, j)
	}
	writeJSON(w, http.StatusOK, out)
}

// getActivity is GET /v1/activities/{key}: one activity with its instances
// and timeline.
func (s *server) getActivity(w http.ResponseWriter, r *http.Request) {
	a, err := s.Store.GetActivity(r.Context(), r.PathValue("key"))
	if errors.Is(err, store.ErrNotFound) || (err == nil && a.ID == "") {
		writeError(w, http.StatusNotFound, "no activity on that key")
		return
	}
	var out activityJSON
	if err == nil {
		out, err = s.activityOut(r.Context(), a)
	}
	var tl []store.TimelineEntry
	if err == nil {
		tl, err = s.Store.Timeline(r.Context(), a.Key, a.StartedAt, timelineLimit)
	}
	if err != nil {
		s.Logger.Error("activity", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	entries := make([]timelineEntryOut, 0, len(tl))
	for _, e := range tl {
		var ev struct {
			Content json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(e.Event, &ev)
		entries = append(entries, timelineEntryOut{ID: e.EventID, ReceivedAt: rfc(e.ReceivedAt), Action: e.Action, Content: ev.Content})
	}
	out.Timeline = &entries
	writeJSON(w, http.StatusOK, out)
}

// reportInstance is PUT /v1/activity-instances/{id}: the app reports an
// activity's update token, or that it was dismissed or ended.
func (s *server) reportInstance(w http.ResponseWriter, r *http.Request) {
	var in api.InstanceReport
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "want an InstanceReport: %v", err)
		return
	}
	if in.Token == nil && in.State == nil {
		writeError(w, http.StatusBadRequest, "give token, state, or both")
		return
	}
	if in.Token != nil && !liveToken.MatchString(*in.Token) {
		writeError(w, http.StatusBadRequest, "token: want 32 to 400 hex characters")
		return
	}
	if in.State != nil && *in.State != api.InstanceReportStateDismissed && *in.State != api.InstanceReportStateEnded {
		writeError(w, http.StatusBadRequest, "state: want dismissed or ended")
		return
	}
	id := r.PathValue("id")
	inst, err := s.Store.GetInstance(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no activity instance with that id")
		return
	}
	if err != nil {
		s.Logger.Error("instance", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if in.Device != nil && *in.Device != inst.DeviceID {
		writeError(w, http.StatusBadRequest, "device: the instance belongs to another device")
		return
	}
	if s.Live == nil {
		writeError(w, http.StatusServiceUnavailable, "push is off")
		return
	}
	if in.Token != nil {
		err = s.Live.Confirm(r.Context(), id, strings.ToLower(*in.Token))
	}
	if err == nil && in.State != nil {
		err = s.Live.Report(r.Context(), id, string(*in.State))
	}
	if err == nil {
		inst, err = s.Store.GetInstance(r.Context(), id)
	}
	if err != nil {
		s.Logger.Error("instance", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, instanceOut(inst))
}

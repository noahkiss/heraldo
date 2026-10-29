package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/noahkiss/heraldo/internal/digest"
	"github.com/noahkiss/heraldo/internal/store"
)

// DigestTick is how often the digest ticker checks whether the day's digest
// is due.
const DigestTick = time.Minute

// errStandbyDigest is why a standby refuses POST /v1/digest/send.
const errStandbyDigest = "this server is a standby; the primary sends digests"

func (s *server) newDigest() *digest.Runner {
	return digest.New(digest.Config{
		Store: s.Store, Settings: s.Settings, Now: s.Now, Logger: s.Logger.With("component", "digest"),
		Emit: func(ctx context.Context, fields map[string]any) (digest.Emitted, error) {
			in, err := s.Validator.Fields(fields)
			if err != nil {
				return digest.Emitted{}, err
			}
			rec, created, err := s.ingest(ctx, in, false)
			return digest.Emitted{ID: rec.ID, Created: created}, err
		},
	})
}

// RunDigest sends the daily digest when it is due, until ctx ends. Run it
// on a primary only: heraldo-server never runs it on a standby.
func (srv *Server) RunDigest(ctx context.Context, every time.Duration) {
	srv.s.digest.Run(ctx, every)
}

// Digest is the digest runner, for the wave 3 summary hook
// (digest.Runner.SetSummary) and for tests.
func (srv *Server) Digest() *digest.Runner { return srv.s.digest }

// digestPreview is GET /v1/digest/preview: the digest as it would go out
// now. It stores and sends nothing.
func (s *server) digestPreview(w http.ResponseWriter, r *http.Request) {
	d, err := s.digest.Preview(r.Context())
	if err != nil {
		s.Logger.Error("digest preview", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, s.digestAnswer(r.Context(), d, ""))
}

// digestSend is POST /v1/digest/send: build the digest and send it now. It
// does not count as the day's scheduled digest.
func (s *server) digestSend(w http.ResponseWriter, r *http.Request) {
	if s.Standby {
		writeError(w, http.StatusConflict, errStandbyDigest)
		return
	}
	d, ev, err := s.digest.Send(r.Context())
	if err != nil {
		s.Logger.Error("digest send", "err", err)
		writeError(w, http.StatusInternalServerError, "could not send the digest")
		return
	}
	writeJSON(w, http.StatusCreated, s.digestAnswer(r.Context(), d, ev.ID))
}

// digestPage is a Digest with the schedule around it.
type digestPage struct {
	*digest.Digest
	Enabled bool   `json:"enabled"`
	At      string `json:"at"`
	// NextAt is when the scheduled digest goes out next; missing while the
	// digest is off.
	NextAt *time.Time `json:"next_at,omitempty"`
	// LastSent is when the last scheduled digest went out.
	LastSent *time.Time `json:"last_sent,omitempty"`
	// EventID is the stored event of a send.
	EventID string `json:"event_id,omitempty"`
}

func (s *server) digestAnswer(ctx context.Context, d *digest.Digest, eventID string) digestPage {
	sch := s.digest.Schedule()
	p := digestPage{Digest: d, Enabled: sch.Enabled, At: sch.At, EventID: eventID}
	now := s.Now()
	if last, found, err := s.Store.LastDigest(ctx); err == nil && found {
		t := last.SentAt.UTC()
		p.LastSent = &t
	}
	if sch.Enabled {
		day, due := sch.Due(now)
		// Today's went out: the next is tomorrow's. Otherwise it is
		// today's, which a past due time sends on the next tick.
		if sent, err := s.Store.DigestSent(ctx, day); err == nil && sent {
			due = due.AddDate(0, 0, 1)
		}
		t := due.UTC()
		p.NextAt = &t
	}
	return p
}

// markStale sets incident.stale on each incident whose last event is older
// than after, and returns them; with only, it drops the others.
func markStale(incs []json.RawMessage, now time.Time, after time.Duration, only bool) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(incs))
	for _, raw := range incs {
		var ev map[string]json.RawMessage
		var at struct {
			ReceivedAt time.Time `json:"received_at"`
		}
		if json.Unmarshal(raw, &ev) != nil || json.Unmarshal(raw, &at) != nil {
			if !only {
				out = append(out, raw)
			}
			continue
		}
		if !digest.IsStale(at.ReceivedAt, now, after) {
			if !only {
				out = append(out, raw)
			}
			continue
		}
		inc := map[string]json.RawMessage{}
		if b, ok := ev["incident"]; ok {
			_ = json.Unmarshal(b, &inc)
		}
		inc["stale"] = json.RawMessage("true")
		ev["incident"], _ = json.Marshal(inc)
		b, err := json.Marshal(ev)
		if err != nil {
			b = raw
		}
		out = append(out, b)
	}
	return out
}

// staleState applies the stale rule to a state read: with stale.enabled on,
// or when the caller asked for stale=1, incidents past stale.after carry
// incident.stale; stale=1 keeps only them.
func (s *server) staleState(st *store.State, only bool) {
	after, on := digest.StaleAfter(s.Settings)
	if !on && !only {
		return
	}
	st.Incidents = markStale(st.Incidents, s.Now(), after, only)
}

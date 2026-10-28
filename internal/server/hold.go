package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/noahkiss/heraldo/internal/auth"
	"github.com/noahkiss/heraldo/internal/hold"
	"github.com/noahkiss/heraldo/internal/store"
)

// The alert hold (docs/api.md, "Hold"). An alert-rung post on the Apprise
// compatibility ingest that matches no bypass rule waits for triage: in
// mode on it is stored held, so it does not push, until triage releases
// one diagnosis for it, suppresses it, or its deadline forwards it. If
// nothing polls the hold for the dead-man time, the hold forwards what it
// holds and lets new alerts through until the next poll.
//
//	GET  /held, /v1/hold                  the queue; a poll unless peek=1
//	POST /held/release, /v1/hold/release  close entries with one message
//	POST /held/suppress, /v1/hold/suppress close entries, send nothing

// HoldOptions configure the hold.
type HoldOptions struct {
	Mode hold.Mode
	// Bypass is the bypass list; nil means none.
	Bypass *hold.Bypass
	// Deadline and Deadman are the timers; 0 means 30 and 20 minutes.
	Deadline time.Duration
	Deadman  time.Duration
}

// Verdicts a release takes.
var verdicts = []string{"REAL", "BLIP", "MAINTENANCE", "FALSE"}

// closedWindow is how long /held lists a closed entry.
const closedWindow = 24 * time.Hour

const deadmanPrefix = "[triage not polling] "

// holdSource is the source of the hold's own notice.
const holdSource = "heraldo-hold"

func (s *server) holdRoutes(mux *http.ServeMux) {
	for _, p := range []string{"/held", "/v1/hold"} {
		mux.HandleFunc("GET "+p, s.notifyAuth(auth.ScopeRead, false, s.heldList))
		mux.HandleFunc("POST "+p+"/release", s.notifyAuth(auth.ScopeWrite, false, s.heldRelease))
		mux.HandleFunc("POST "+p+"/suppress", s.notifyAuth(auth.ScopeWrite, false, s.heldSuppress))
	}
}

func (s *server) deadlinePrefix() string {
	return fmt.Sprintf("[not triaged in %d min] ", int(s.Hold.Deadline/time.Minute))
}

func newHoldID(now time.Time) string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return "h-" + now.Local().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

// deadman reports whether triage has not polled for the dead-man time.
// Before the first poll the server's start counts.
func (s *server) deadman(ctx context.Context, now time.Time) bool {
	last, err := s.Store.HoldLastPoll(ctx)
	if err != nil {
		s.Logger.Error("hold: read last poll", "err", err)
	}
	if last.IsZero() || last.Before(s.started) {
		last = s.started
	}
	return now.Sub(last) > s.Hold.Deadman
}

// classify is "none" (not a candidate), "bypass" or "held".
func (s *server) classify(m map[string]any) string {
	if s.Hold.Mode == hold.Off || !strings.EqualFold(str(m, "tag"), "alert") {
		return "none"
	}
	if s.Hold.Bypass.Bypassed(str(m, "tag"), str(m, "title"), str(m, "body")) {
		return "bypass"
	}
	return "held"
}

// holdResult is what became of a post.
type holdResult struct {
	rec    store.Record
	held   bool
	holdID string
	err    error
}

// holdIn stores a post, holding it when the hold says so.
func (s *server) holdIn(ctx context.Context, m, extra map[string]any, now time.Time) holdResult {
	class := s.classify(m)
	entry := store.HoldEntry{
		TS: now, Deadline: now.Add(s.Hold.Deadline), Tag: str(m, "tag"), Title: str(m, "title"),
		Body: str(m, "body"), Client: str(extra, "client"), Source: str(m, "source"), Input: m,
		Status: store.HoldHeld,
	}
	if class == "held" {
		entry.ID = newHoldID(now)
	}
	if class == "held" && s.Hold.Mode == hold.On && !s.deadman(ctx, now) {
		if err := s.Store.AddHold(ctx, entry); err != nil {
			// Without an entry nothing would ever forward it: send it now.
			s.Logger.Error("hold: store the entry; passing the alert through", "err", err)
		} else {
			rec, err := s.ingestNotify(ctx, m, extra, map[string]any{"id": entry.ID, "state": "held", "forwarded": false}, "")
			if err != nil {
				// The entry forwards it at the deadline all the same.
				s.Logger.Error("hold: store the held event", "hold", entry.ID, "err", err)
			} else if err := s.Store.SetHoldEvent(ctx, entry.ID, rec.ID); err != nil {
				s.Logger.Error("hold: link the event", "hold", entry.ID, "err", err)
			}
			return holdResult{rec: rec, held: true, holdID: entry.ID}
		}
	}
	var meta map[string]any
	switch class {
	case "held":
		// shadow, or on with the dead-man tripped: it pushes now, and the
		// hold records that it saw it.
		meta = map[string]any{"id": entry.ID, "state": "passed", "forwarded": true, "shadow": s.Hold.Mode == hold.Shadow}
	case "bypass":
		meta = map[string]any{"state": "bypass"}
	}
	rec, err := s.ingestNotify(ctx, m, extra, meta, "")
	if err != nil {
		return holdResult{err: err}
	}
	if class == "held" {
		entry.EventID, entry.Forwarded, entry.Shadow = rec.ID, true, s.Hold.Mode == hold.Shadow
		if err := s.Store.AddHold(ctx, entry); err != nil {
			s.Logger.Error("hold: record a passed alert", "hold", entry.ID, "err", err)
		}
	}
	return holdResult{rec: rec, holdID: entry.ID}
}

// RunHold runs the hold's loop each interval until ctx ends: deadlines,
// the dead-man and its forwards. It runs in every mode, so an entry left
// from an earlier on period still forwards. All its state is in the
// database.
func (srv *Server) RunHold(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		srv.s.tickHold(ctx)
	}
}

// forward stores entry e again as a pushing event, title prefixed.
func (s *server) forward(ctx context.Context, e store.HoldEntry, prefix, reason string) bool {
	m := e.Input
	if m == nil {
		m = map[string]any{"title": e.Title, "body": e.Body, "tag": e.Tag, "source": e.Source}
	}
	_, err := s.ingestNotify(ctx, m, map[string]any{"client": e.Client},
		map[string]any{"state": "forward", "reason": reason, "ids": []string{e.ID}}, prefix)
	if err == nil {
		err = s.Store.SetHoldForwarded(ctx, e.ID)
	}
	if err != nil {
		s.Logger.Error("hold: forward failed; retrying next tick", "hold", e.ID, "reason", reason, "err", err)
		return false
	}
	return true
}

func (s *server) tickHold(ctx context.Context) {
	now := s.Now()
	s.Hold.Bypass.Rules() // reload a changed file
	dm := s.deadman(ctx, now)
	s.hmu.Lock()
	changed := dm != s.deadmanActive
	s.deadmanActive = dm
	s.hmu.Unlock()
	if changed && dm {
		last, _ := s.Store.HoldLastPoll(ctx)
		if last.IsZero() || last.Before(s.started) {
			last = s.started
		}
		since := last.Local().Format("15:04")
		s.Logger.Warn("hold: DEAD-MAN TRIPPED: nothing has polled the hold; unforwarded alerts pass straight through",
			"since", since, "mode", s.Hold.Mode)
		if s.Hold.Mode == hold.On {
			notice := map[string]any{
				"tag": "general", "source": holdSource, "title": "alert hold off: triage not polling",
				"body": "Nothing has polled the heraldo hold since " + since + ". Alerts now pass straight through until the next poll.",
			}
			if _, err := s.ingestNotify(ctx, notice, nil,
				map[string]any{"state": "forward", "reason": "deadman-notice", "ids": []string{}}, ""); err != nil {
				s.Logger.Error("hold: dead-man notice", "err", err)
			}
		}
	} else if changed {
		s.Logger.Info("hold: dead-man cleared: the hold was polled again; holding resumes")
	}
	entries, err := s.Store.Holds(ctx, now.Add(-closedWindow))
	if err != nil {
		s.Logger.Error("hold: list", "err", err)
		return
	}
	pass := dm || s.Hold.Mode != hold.On
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		switch {
		case e.Open() && now.After(e.Deadline):
			if !e.Forwarded && !s.forward(ctx, e, s.deadlinePrefix(), "deadline") {
				continue
			}
			if _, err := s.Store.CloseHolds(ctx, []string{e.ID}, store.HoldExpired, "", "", now); err != nil {
				s.Logger.Error("hold: expire", "hold", e.ID, "err", err)
			}
		case (e.Open() || e.Status == store.HoldExpired) && !e.Forwarded && pass:
			s.forward(ctx, e, deadmanPrefix, "deadman")
		}
	}
}

// ---- endpoints ---------------------------------------------------------

// heldRow is an entry as /held lists it, with times in epoch seconds as the
// tee gave them.
func heldRow(e store.HoldEntry) map[string]any {
	epoch := func(t time.Time) float64 { return float64(t.UnixMilli()) / 1000 }
	row := map[string]any{
		"id": e.ID, "ts": epoch(e.TS), "tag": e.Tag, "title": e.Title, "body": e.Body,
		"client": e.Client, "source": e.Source, "deadline_ts": epoch(e.Deadline),
		"forwarded": e.Forwarded, "shadow": e.Shadow, "status": e.Status,
	}
	if e.EventID != "" {
		row["event_id"] = e.EventID
	}
	if !e.ClosedAt.IsZero() {
		row["closed_ts"] = epoch(e.ClosedAt)
	}
	if e.Verdict != "" {
		row["verdict"] = e.Verdict
	}
	if e.Reason != "" {
		row["reason"] = e.Reason
	}
	return row
}

// heldList is GET /held?state=pending&peek=1. Every GET without peek is a
// triage poll and resets the dead-man; a person or a CLI looking must use
// peek=1, or it would hold the dead-man off while triage is dead.
func (s *server) heldList(w http.ResponseWriter, r *http.Request) {
	now := s.Now()
	q := r.URL.Query()
	switch q.Get("peek") {
	case "1", "true", "yes":
	default:
		if err := s.Store.SetHoldLastPoll(r.Context(), now); err != nil {
			s.Logger.Error("hold: record the poll", "err", err)
		}
	}
	entries, err := s.Store.Holds(r.Context(), now.Add(-closedWindow))
	if err != nil {
		s.Logger.Error("hold: list", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	pending := q.Get("state") == "pending"
	rows := []map[string]any{}
	var oldest any
	for _, e := range entries {
		if !e.Forwarded && (e.Open() || e.Status == store.HoldExpired) && oldest == nil {
			oldest = float64(e.TS.UnixMilli()) / 1000
		}
		if pending && !e.Open() {
			continue
		}
		rows = append(rows, heldRow(e))
	}
	var lastPoll any
	if t, err := s.Store.HoldLastPoll(r.Context()); err == nil && !t.IsZero() {
		lastPoll = float64(t.UnixMilli()) / 1000
	}
	s.hmu.Lock()
	dm := s.deadmanActive
	s.hmu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"mode": s.Hold.Mode, "last_poll_ts": lastPoll, "oldest_unforwarded_ts": oldest,
		"deadman_active": dm, "held": rows,
	})
}

// holdBody reads a release or suppress body: a JSON object with ids.
func holdBody(w http.ResponseWriter, r *http.Request) (map[string]any, []string, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequestBytes))
	var req map[string]any
	if err != nil || json.Unmarshal(raw, &req) != nil || req == nil {
		writeError(w, http.StatusBadRequest, "the body must be a JSON object")
		return nil, nil, false
	}
	list, _ := req["ids"].([]any)
	var ids []string
	for _, v := range list {
		if id, ok := v.(string); ok && id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 || len(ids) != len(list) {
		writeError(w, http.StatusBadRequest, "ids must be a non-empty list of hold ids")
		return nil, nil, false
	}
	return req, ids, true
}

// splitOpen answers 409 when no id names an open entry.
func (s *server) splitOpen(w http.ResponseWriter, r *http.Request, ids []string) ([]string, []string, bool) {
	open, other, err := s.Store.SplitHolds(r.Context(), ids)
	if err != nil {
		s.Logger.Error("hold: look up", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return nil, nil, false
	}
	if len(open) == 0 {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "no open entries among ids", "unknown_or_closed": other})
		return nil, nil, false
	}
	return open, nonNil(other), true
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// heldRelease closes a group with one diagnosis. In mode on it stores one
// message at tag, from source triage; in shadow it only records.
func (s *server) heldRelease(w http.ResponseWriter, r *http.Request) {
	req, ids, ok := holdBody(w, r)
	if !ok {
		return
	}
	verdict := strings.ToUpper(str(req, "verdict"))
	valid := false
	for _, v := range verdicts {
		valid = valid || v == verdict
	}
	if !valid {
		writeError(w, http.StatusBadRequest, "verdict must be one of %s", strings.Join(verdicts, ", "))
		return
	}
	body, hasBody := req["body"].(string)
	if str(req, "tag") == "" || str(req, "title") == "" || !hasBody {
		writeError(w, http.StatusBadRequest, "title, body and tag are required")
		return
	}
	open, other, ok := s.splitOpen(w, r, ids)
	if !ok {
		return
	}
	out := map[string]any{"released": open, "unknown_or_closed": other, "mode": s.Hold.Mode, "sent": false}
	if s.Hold.Mode == hold.On {
		m := map[string]any{"title": str(req, "title"), "body": body, "tag": str(req, "tag"), "source": "triage"}
		rec, err := s.ingestNotify(r.Context(), m, nil,
			map[string]any{"state": "forward", "reason": "release", "ids": open, "verdict": verdict}, "")
		if err != nil {
			// Nothing is closed, so the caller can send the release again.
			s.Logger.Error("hold: release", "err", err)
			writeError(w, http.StatusInternalServerError, "could not store the release; nothing was closed")
			return
		}
		out["sent"], out["event_id"] = true, rec.ID
	}
	closed, err := s.Store.CloseHolds(r.Context(), open, store.HoldReleased, verdict, "", s.Now())
	if err != nil {
		s.Logger.Error("hold: close", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out["released"] = nonNil(closed)
	writeJSON(w, http.StatusOK, out)
}

// heldSuppress closes a group and sends nothing.
func (s *server) heldSuppress(w http.ResponseWriter, r *http.Request) {
	req, ids, ok := holdBody(w, r)
	if !ok {
		return
	}
	open, other, ok := s.splitOpen(w, r, ids)
	if !ok {
		return
	}
	closed, err := s.Store.CloseHolds(r.Context(), open, store.HoldSuppressed, "", str(req, "reason"), s.Now())
	if err != nil {
		s.Logger.Error("hold: close", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"suppressed": nonNil(closed), "unknown_or_closed": other, "mode": s.Hold.Mode})
}

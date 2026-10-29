package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/settings"
	"github.com/noahkiss/heraldo/internal/store"
)

// inboxCursor is the read cursor the badge counts against, as
// push.BadgeCursor.
const inboxCursor = "inbox"

// ---- event stream ------------------------------------------------------

// hub tells open event streams that something changed. A signal carries
// nothing: the stream reads the current revision vector and head when it
// wakes, so a burst of writes coalesces into one poke, and a missed signal
// loses nothing.
type hub struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

func newHub() *hub { return &hub{subs: map[chan struct{}]struct{}{}} }

func (h *hub) subscribe() chan struct{} {
	c := make(chan struct{}, 1)
	h.mu.Lock()
	h.subs[c] = struct{}{}
	h.mu.Unlock()
	return c
}

func (h *hub) unsubscribe(c chan struct{}) {
	h.mu.Lock()
	delete(h.subs, c)
	h.mu.Unlock()
}

// signal never blocks. A subscriber that has not woken for its last signal
// keeps that one.
func (h *hub) signal() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.subs {
		select {
		case c <- struct{}{}:
		default:
		}
	}
}

// Heartbeat bounds for GET /v1/stream?heartbeat=.
const (
	MinHeartbeat = 15 * time.Second
	MaxHeartbeat = 120 * time.Second
)

// streamHeartbeat is the keep-alive gap a stream asked for with
// ?heartbeat=<seconds>, clamped to MinHeartbeat through MaxHeartbeat; def
// when it asked for none.
func streamHeartbeat(v string, def time.Duration) (time.Duration, error) {
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("heartbeat: want whole seconds, not %q", v)
	}
	d := time.Duration(n) * time.Second
	return min(max(d, MinHeartbeat), MaxHeartbeat), nil
}

// pokeData is the data of a poke: the newest event's cursor, as before
// 0.10, and the revision vector.
type pokeData struct {
	Cursor string             `json:"cursor"`
	Boot   string             `json:"boot"`
	Rev    api.RevisionVector `json:"rev"`
}

// stream is GET /v1/stream: server-sent events. On connect, one revisions
// event with the vector, then one poke; after that one poke per change,
// with the cursor of the newest event and the vector.
func (s *server) stream(w http.ResponseWriter, r *http.Request) {
	every, err := streamHeartbeat(r.URL.Query().Get("heartbeat"), s.Heartbeat)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	rc := http.NewResponseController(w)
	// The server's write timeout would cut a stream off.
	_ = rc.SetWriteDeadline(time.Time{})
	c := s.hub.subscribe()
	defer s.hub.unsubscribe(c)
	// The vector is read before the head, so a write in between shows as
	// a newer vector on the next poke, never as a vector newer than the
	// data the client then fetches.
	rv := s.revs.snapshot()
	head, err := s.Store.Head(r.Context())
	if err != nil {
		s.Logger.Error("stream: head", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	write := func(event string, v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	poke := func(rv api.Revisions, head int64) bool {
		return write("poke", pokeData{Cursor: store.CursorFor(head), Boot: rv.Boot, Rev: rv.Rev})
	}
	if _, err := io.WriteString(w, "retry: 3000\n\n"); err != nil || !write("revisions", rv) || !poke(rv, head) {
		return
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.Done:
			return
		case <-c:
			rv := s.revs.snapshot()
			head, err := s.Store.Head(r.Context())
			if err != nil {
				if r.Context().Err() == nil {
					s.Logger.Error("stream: head", "err", err)
				}
				return
			}
			if !poke(rv, head) {
				return
			}
		case <-tick.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}

// ---- state -------------------------------------------------------------

type statePage struct {
	// Badge is the badge by badge.scope against the inbox cursor.
	Badge     *int              `json:"badge,omitempty"`
	Incidents []json.RawMessage `json:"incidents"`
	Progress  []json.RawMessage `json:"progress"`
	Checkins  []json.RawMessage `json:"checkins"`
	Statuses  []json.RawMessage `json:"statuses"`
}

func (s *server) state(w http.ResponseWriter, r *http.Request) {
	projects, err := s.projectFilter(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	only := false
	switch v := r.URL.Query().Get("stale"); v {
	case "", "0", "false":
	case "1", "true":
		only = true
	default:
		writeError(w, http.StatusBadRequest, "stale: want 1 or 0, not %q", v)
		return
	}
	st, err := s.Store.State(r.Context(), projects)
	if err != nil {
		s.Logger.Error("state", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.staleState(&st, only)
	scope, bp := s.badgeScope()
	b, err := s.Store.BadgeFor(r.Context(), inboxCursor, scope, bp)
	if err != nil {
		s.Logger.Error("state: badge", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, statePage{
		Badge:     &b,
		Incidents: st.Incidents, Progress: st.Progress, Checkins: st.Checkins, Statuses: st.Statuses,
	})
}

// ---- read cursors ------------------------------------------------------

var cursorName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type cursorBody struct {
	Name   string `json:"name,omitempty"`
	Cursor string `json:"cursor"`
	// Unread is set in answers: the Inbox's unread count after the cursor.
	Unread *int `json:"unread,omitempty"`
	// Badge is set in answers: the badge count by badge.scope.
	Badge *int `json:"badge,omitempty"`
}

// cursorAnswer writes the cursor called name at seq, with its unread count
// and its badge.
func (s *server) cursorAnswer(w http.ResponseWriter, r *http.Request, name string, seq int64) {
	n, err := s.Store.UnreadAfter(r.Context(), seq)
	if err != nil {
		s.Logger.Error("cursor: unread", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	scope, projects := s.badgeScope()
	b, err := s.Store.Badge(r.Context(), scope, projects, seq)
	if err != nil {
		s.Logger.Error("cursor: badge", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, cursorBody{Name: name, Cursor: store.CursorFor(seq), Unread: &n, Badge: &b})
}

// badgeScope is badge.scope and badge.projects in force.
func (s *server) badgeScope() (string, []string) {
	return settings.From[string](s.Settings, "badge.scope"), settings.From[[]string](s.Settings, "badge.projects")
}

func (s *server) cursorName(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if !cursorName.MatchString(name) {
		writeError(w, http.StatusBadRequest, "name: 1 to 64 of A-Z, a-z, 0-9, dot, underscore and dash")
		return "", false
	}
	return name, true
}

// getCursor is GET /v1/cursors/{name}: the read cursor called name, or 404.
// Every read token sees the same cursors.
func (s *server) getCursor(w http.ResponseWriter, r *http.Request) {
	name, ok := s.cursorName(w, r)
	if !ok {
		return
	}
	seq, found, err := s.Store.Cursor(r.Context(), name)
	if err != nil {
		s.Logger.Error("cursor", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "no cursor named %q", name)
		return
	}
	s.cursorAnswer(w, r, name, seq)
}

// putCursor is PUT /v1/cursors/{name} with {"cursor": "..."}. Any read token
// may move any cursor. A cursor only moves forward; the answer is the stored
// cursor.
func (s *server) putCursor(w http.ResponseWriter, r *http.Request) {
	name, ok := s.cursorName(w, r)
	if !ok {
		return
	}
	var b cursorBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "want {\"cursor\": \"...\"}")
		return
	}
	seq, err := store.ParseCursor(b.Cursor)
	if err != nil {
		writeError(w, http.StatusBadRequest, "cursor: not a cursor from this server")
		return
	}
	head, err := s.Store.Head(r.Context())
	if err != nil {
		s.Logger.Error("cursor: head", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if seq > head {
		writeError(w, http.StatusBadRequest, "cursor: past the newest event")
		return
	}
	stored, err := s.Store.SetCursor(r.Context(), name, seq, tokenFrom(r).Name)
	if err != nil {
		s.Logger.Error("cursor", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.cursorAnswer(w, r, name, stored)
}

package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/noahkiss/heraldo/internal/store"
)

// ---- event stream ------------------------------------------------------

// hub tells open event streams that an event was stored. A poke carries no
// event: the client fetches by cursor, so a missed poke loses nothing.
type hub struct {
	mu   sync.Mutex
	subs map[chan int64]struct{}
}

func newHub() *hub { return &hub{subs: map[chan int64]struct{}{}} }

func (h *hub) subscribe() chan int64 {
	c := make(chan int64, 1)
	h.mu.Lock()
	h.subs[c] = struct{}{}
	h.mu.Unlock()
	return c
}

func (h *hub) unsubscribe(c chan int64) {
	h.mu.Lock()
	delete(h.subs, c)
	h.mu.Unlock()
}

// poke never blocks. A subscriber that has not read its last poke gets the
// newer seq in its place.
func (h *hub) poke(seq int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.subs {
		select {
		case c <- seq:
		default:
			select {
			case <-c:
			default:
			}
			c <- seq
		}
	}
}

// stream is GET /v1/stream: server-sent events, one poke event per stored
// event, with the cursor of the newest event. The first poke, sent on
// connect, carries the current head.
func (s *server) stream(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	// The server's write timeout would cut a stream off.
	_ = rc.SetWriteDeadline(time.Time{})
	c := s.hub.subscribe()
	defer s.hub.unsubscribe(c)
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
	send := func(seq int64) bool {
		if _, err := fmt.Fprintf(w, "event: poke\ndata: {\"cursor\":%q}\n\n", store.CursorFor(seq)); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if _, err := io.WriteString(w, "retry: 3000\n\n"); err != nil || !send(head) {
		return
	}
	tick := time.NewTicker(s.Heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.Done:
			return
		case seq := <-c:
			if !send(seq) {
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
	Incidents []json.RawMessage `json:"incidents"`
	Progress  []json.RawMessage `json:"progress"`
	Checkins  []json.RawMessage `json:"checkins"`
	Statuses  []json.RawMessage `json:"statuses"`
}

func (s *server) state(w http.ResponseWriter, r *http.Request) {
	st, err := s.Store.State(r.Context())
	if err != nil {
		s.Logger.Error("state", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, statePage{
		Incidents: st.Incidents, Progress: st.Progress, Checkins: st.Checkins, Statuses: st.Statuses,
	})
}

// ---- read cursors ------------------------------------------------------

var cursorName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type cursorBody struct {
	Name   string `json:"name,omitempty"`
	Cursor string `json:"cursor"`
	// Unread is set in answers: the badge count after the cursor.
	Unread *int `json:"unread,omitempty"`
}

// cursorAnswer writes the cursor called name at seq, with its unread count.
func (s *server) cursorAnswer(w http.ResponseWriter, r *http.Request, name string, seq int64) {
	n, err := s.Store.UnreadAfter(r.Context(), seq)
	if err != nil {
		s.Logger.Error("cursor: unread", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, cursorBody{Name: name, Cursor: store.CursorFor(seq), Unread: &n})
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

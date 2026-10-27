// Package server holds the heraldo HTTP handlers.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/auth"
	"github.com/noahkiss/heraldo/internal/ids"
	"github.com/noahkiss/heraldo/internal/rules"
	"github.com/noahkiss/heraldo/internal/store"
	"github.com/noahkiss/heraldo/internal/validate"
)

// MaxRequestBytes caps a POST body. The body field alone may be 256 KiB, and
// JSON escaping can grow it, so the cap leaves room.
const MaxRequestBytes = 1 << 20

// Options configure the handler.
type Options struct {
	Version   string
	Store     *store.Store
	Tokens    *auth.Set
	Validator *validate.Validator
	// Rules types untyped inputs at ingest; nil means no rules, and an
	// untyped input is stored as a message.
	Rules  *rules.Watcher
	Logger *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Done, when closed, ends every open event stream, so a shutdown does
	// not wait on them. nil means streams end only when the client leaves.
	Done <-chan struct{}
	// Heartbeat is the gap between keep-alive comments on an event stream;
	// 0 means 25 seconds.
	Heartbeat time.Duration
	// Pusher gets every newly stored event; nil pushes nothing.
	Pusher Pusher
	// PushState is what /healthz reports as push; empty means off.
	PushState api.HealthPush
}

// Pusher queues stored events for push. Enqueue must not block.
type Pusher interface {
	Enqueue(store.Record) bool
}

type server struct {
	Options
	hub *hub
	// started is when the server started: no check goes down for a missed
	// ping before started plus its grace.
	started time.Time
	// cmu serialises check judging, so a ping and a scheduler tick never
	// open the same incident twice.
	cmu sync.Mutex
}

// Server is the HTTP handler and the check-in scheduler.
type Server struct {
	http.Handler
	s *server
}

// New returns the server's HTTP handler. It is a *Server.
func New(o Options) http.Handler { return NewServer(o) }

// NewServer returns the server. Serve its handler, and run RunCheckins for
// the check-in scheduler.
func NewServer(o Options) *Server {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Tokens == nil {
		o.Tokens = &auth.Set{}
	}
	if o.Heartbeat == 0 {
		o.Heartbeat = 25 * time.Second
	}
	if o.PushState == "" {
		o.PushState = api.HealthPushOff
	}
	s := &server{Options: o, hub: newHub(), started: o.Now()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("POST /v1/events", s.auth(auth.ScopeWrite, false, s.postEvent))
	mux.HandleFunc("GET /v1/events", s.auth(auth.ScopeRead, false, s.listEvents))
	mux.HandleFunc("GET /v1/events/{id}", s.auth(auth.ScopeRead, false, s.getEvent))
	mux.HandleFunc("GET /v1/send", s.auth(auth.ScopeWrite, true, s.sendEvent))
	mux.HandleFunc("GET /v1/stream", s.auth(auth.ScopeRead, false, s.stream))
	mux.HandleFunc("GET /v1/state", s.auth(auth.ScopeRead, false, s.state))
	mux.HandleFunc("GET /v1/cursors/{name}", s.auth(auth.ScopeRead, false, s.getCursor))
	mux.HandleFunc("PUT /v1/cursors/{name}", s.auth(auth.ScopeRead, false, s.putCursor))
	mux.HandleFunc("GET /v1/devices", s.auth(DeviceScope, false, s.listDevices))
	mux.HandleFunc("POST /v1/devices", s.auth(DeviceScope, false, s.registerDevice))
	mux.HandleFunc("GET /v1/devices/{id}", s.auth(DeviceScope, false, s.getDevice))
	mux.HandleFunc("PUT /v1/devices/{id}", s.auth(DeviceScope, false, s.updateDevice))
	mux.HandleFunc("DELETE /v1/devices/{id}", s.auth(DeviceScope, false, s.removeDevice))
	mux.HandleFunc("GET /v1/mutes", s.auth(auth.ScopeWrite, false, s.listMutes))
	mux.HandleFunc("POST /v1/mutes", s.auth(auth.ScopeWrite, false, s.createMute))
	mux.HandleFunc("DELETE /v1/mutes/{id}", s.auth(auth.ScopeWrite, false, s.endMute))
	mux.HandleFunc("GET /v1/icons", s.auth(auth.ScopeRead, false, s.listIcons))
	mux.HandleFunc("GET /v1/icons/{name}", s.auth(auth.ScopeRead, false, s.getIcon))
	mux.HandleFunc("PUT /v1/icons/{name}", s.auth(auth.ScopeAdmin, false, s.putIcon))
	mux.HandleFunc("DELETE /v1/icons/{name}", s.auth(auth.ScopeAdmin, false, s.deleteIcon))
	for _, m := range []string{"GET", "POST"} {
		mux.HandleFunc(m+" /ping/{slug}", s.auth(auth.ScopeWrite, false, s.ping))
		mux.HandleFunc(m+" /ping/{slug}/{op}", s.auth(auth.ScopeWrite, false, s.ping))
		// A ping key in the path is the credential: no bearer token.
		mux.HandleFunc(m+" /p/{key}", s.keyPing)
		mux.HandleFunc(m+" /p/{key}/{op}", s.keyPing)
	}
	mux.HandleFunc("GET /v1/checkins", s.auth(auth.ScopeRead, false, s.listChecks))
	mux.HandleFunc("GET /v1/checkins/{slug}", s.auth(auth.ScopeRead, false, s.getCheck))
	mux.HandleFunc("PUT /v1/checkins/{slug}", s.auth(auth.ScopeWrite, false, s.putCheck))
	mux.HandleFunc("POST /v1/checkins/{slug}/pause", s.auth(auth.ScopeWrite, false, s.pauseCheck))
	mux.HandleFunc("POST /v1/checkins/{slug}/resume", s.auth(auth.ScopeWrite, false, s.resumeCheck))
	mux.HandleFunc("POST /v1/checkins/{slug}/key", s.auth(auth.ScopeWrite, false, s.mintPingKey))
	mux.HandleFunc("DELETE /v1/checkins/{slug}/key", s.auth(auth.ScopeWrite, false, s.removePingKey))
	return &Server{Handler: s.logRequests(mux), s: s}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, format string, a ...any) {
	writeJSON(w, status, api.Error{Error: fmt.Sprintf(format, a...)})
}

// ---- middleware --------------------------------------------------------

type ctxKey struct{}

type tokenKey struct{}

// tokenFrom returns the token that authenticated r.
func tokenFrom(r *http.Request) auth.Token {
	t, _ := r.Context().Value(tokenKey{}).(auth.Token)
	return t
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach Flush and the write deadline.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// logRequests logs one line per request. It logs the path, never the query
// string, because /v1/send may carry a token there; and a ping key in a /p/
// path is logged as <key>.
func (s *server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		var who string
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, &who))
		next.ServeHTTP(sw, r)
		if r.URL.Path == "/healthz" && sw.status == http.StatusOK {
			return
		}
		s.Logger.Info("request", "method", r.Method, "path", redactPath(r.URL.Path), "status", sw.status,
			"ms", time.Since(start).Milliseconds(), "token", who)
	})
}

// redactPath hides the ping key in a /p/<key>[/op] path.
func redactPath(p string) string {
	rest, ok := strings.CutPrefix(p, "/p/")
	if !ok {
		return p
	}
	if _, op, found := strings.Cut(rest, "/"); found {
		return "/p/<key>/" + op
	}
	return "/p/<key>"
}

// auth requires a token with scope need. queryToken allows the token
// parameter as well as the Authorization header.
func (s *server) auth(need auth.Scope, queryToken bool, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		presented := ""
		if v := r.Header.Get("Authorization"); v != "" {
			scheme, tok, ok := strings.Cut(v, " ")
			if !ok || !strings.EqualFold(scheme, "Bearer") {
				w.Header().Set("WWW-Authenticate", `Bearer`)
				writeError(w, http.StatusUnauthorized, "Authorization must be Bearer <token>")
				return
			}
			presented = strings.TrimSpace(tok)
		}
		if presented == "" && r.URL.Query().Has("token") {
			if !queryToken {
				w.Header().Set("WWW-Authenticate", `Bearer`)
				writeError(w, http.StatusUnauthorized, "the token parameter is accepted only on /v1/send; use the Authorization header")
				return
			}
			presented = r.URL.Query().Get("token")
		}
		t, ok := s.Tokens.Lookup(presented)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer`)
			writeError(w, http.StatusUnauthorized, "missing or unknown token")
			return
		}
		if who, _ := r.Context().Value(ctxKey{}).(*string); who != nil {
			*who = t.Name
		}
		if !t.Scope.Allows(need) {
			writeError(w, http.StatusForbidden, "token %q has scope %s; this call needs %s", t.Name, t.Scope, need)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), tokenKey{}, t)))
	}
}

// ---- handlers ----------------------------------------------------------

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	n, err := s.Store.Count(r.Context())
	if err != nil {
		s.Logger.Error("health: database", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, api.Health{
			Status: api.HealthStatusError, Version: s.Version, Db: api.HealthDbError, Push: &s.PushState,
		})
		return
	}
	writeJSON(w, http.StatusOK, api.Health{
		Status: api.HealthStatusOk, Version: s.Version, Db: api.HealthDbOk, Events: &n, Push: &s.PushState,
	})
}

func (s *server) postEvent(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequestBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body over %d bytes", MaxRequestBytes)
			return
		}
		writeError(w, http.StatusBadRequest, "read body: %v", err)
		return
	}
	in, err := s.Validator.Input(raw)
	if err != nil {
		s.badInput(w, err)
		return
	}
	s.store(w, r, in)
}

// sendParams are the query parameters GET /v1/send maps to envelope fields.
// number and array parameters are listed apart.
var sendParams = map[string]bool{
	"source": true, "kind": true, "key": true, "action": true, "ts": true, "host": true,
	"title": true, "summary": true, "body": true, "level": true, "thread": true, "url": true,
	"image": true, "icon": true, "sound": true, "dedup": true, "expires_at": true,
}

func (s *server) sendEvent(w http.ResponseWriter, r *http.Request) {
	fields, err := sendFields(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	in, err := s.Validator.Fields(fields)
	if err != nil {
		s.badInput(w, err)
		return
	}
	s.store(w, r, in)
}

func sendFields(q url.Values) (map[string]any, error) {
	fields := map[string]any{}
	for name, vals := range q {
		switch {
		case name == "token":
		case name == "tags":
			tags := make([]any, len(vals))
			for i, v := range vals {
				tags[i] = v
			}
			fields["tags"] = tags
		case name == "relevance":
			if len(vals) != 1 {
				return nil, fmt.Errorf("relevance: give it once")
			}
			if _, err := strconv.ParseFloat(vals[0], 64); err != nil {
				return nil, fmt.Errorf("relevance: %q is not a number", vals[0])
			}
			fields["relevance"] = json.Number(vals[0])
		case sendParams[name]:
			if len(vals) != 1 {
				return nil, fmt.Errorf("%s: give it once", name)
			}
			fields[name] = vals[0]
		default:
			for _, k := range validate.ServerFields {
				if name == k {
					return nil, fmt.Errorf("%s: set by the server, not allowed in input", name)
				}
			}
			return nil, fmt.Errorf("unknown parameter %q; actions, content, checkin and details need POST /v1/events", name)
		}
	}
	return fields, nil
}

func (s *server) badInput(w http.ResponseWriter, err error) {
	if validate.IsValidation(err) {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	s.Logger.Error("validate", "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func (s *server) store(w http.ResponseWriter, r *http.Request, in *validate.Input) {
	rec, created, err := s.ingest(r.Context(), in, tokenFrom(r).Has(auth.CapTrustedIngest))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not store the event")
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(rec.Event, '\n'))
}

// ingest types, stores and announces one input: it pokes the event streams
// and queues the push. trusted is the trusted-ingest capability. It logs its
// own errors.
func (s *server) ingest(ctx context.Context, in *validate.Input, trusted bool) (store.Record, bool, error) {
	typed, _, err := rules.Type(s.Rules.Rules(), in, s.Validator)
	if typed == nil {
		s.Logger.Error("type", "err", err)
		return store.Record{}, false, err
	}
	if err != nil {
		// The sender's event is valid; a rule that cannot type it must not
		// lose it.
		s.Logger.Warn("rules: matched, but the typed event is not valid; stored as a message", "err", err)
	}
	in = typed
	now := s.Now().UTC()
	id := ids.New(now)
	f := in.Fields
	f["id"] = id
	f["received_at"] = now.Format(time.RFC3339Nano)
	if _, ok := f["ts"]; !ok {
		f["ts"] = now.Format(time.RFC3339Nano)
	}
	if trusted {
		if h := bridgedHold(f); h != nil {
			f["hold"] = h
		}
	}
	rec, created, err := s.Store.Insert(ctx, store.NewEvent{
		ID: id, ReceivedAt: now, Fields: f, Typed: in.Typed, DedupAnyAge: trusted,
	})
	if err != nil {
		s.Logger.Error("store", "err", err)
		return store.Record{}, false, err
	}
	if created {
		s.hub.poke(rec.Seq)
		if s.Pusher != nil {
			s.Pusher.Enqueue(rec)
		}
		if *in.Typed.Kind == api.KindCheckin && in.Typed.Key != nil {
			s.judgeCheck(ctx, *in.Typed.Key)
		}
	}
	return rec, created, nil
}

// bridgedHold returns the stored hold for a bridged sender's triage state,
// details.tee.hold, or nil when there is none. Only a trusted-ingest token
// gets here. The event is marked store_only: later stages never push it.
func bridgedHold(f map[string]any) map[string]any {
	details, _ := f["details"].(map[string]any)
	tee, _ := details["tee"].(map[string]any)
	src, ok := tee["hold"].(map[string]any)
	if !ok {
		return nil
	}
	h := make(map[string]any, len(src)+1)
	for k, v := range src {
		h[k] = v
	}
	h["store_only"] = true
	return h
}

func (s *server) getEvent(w http.ResponseWriter, r *http.Request) {
	rec, err := s.Store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no event with that id")
		return
	}
	if err == nil {
		recs := []store.Record{rec}
		err = s.Store.WithDeliveries(r.Context(), recs)
		rec = recs[0]
	}
	if err != nil {
		s.Logger.Error("get", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(append(rec.Event, '\n'))
}

// page is EventPage with the events kept as stored JSON.
type page struct {
	Events     []json.RawMessage `json:"events"`
	NextCursor *string           `json:"next_cursor,omitempty"`
}

func (s *server) listEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.Filter{
		Limit:  100,
		Source: q.Get("source"),
		Host:   q.Get("host"),
		Key:    q.Get("key"),
		Tag:    q.Get("tag"),
		Q:      q.Get("q"),
	}
	if v := q.Get("after"); v != "" {
		seq, err := store.ParseCursor(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "after: not a cursor from this server")
			return
		}
		f.AfterSeq = seq
	}
	if v := q.Get("before"); v != "" {
		seq, err := store.ParseCursor(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "before: not a cursor from this server")
			return
		}
		f.BeforeSeq = seq
	}
	switch q.Get("order") {
	case "", "asc":
	case "desc":
		f.Desc = true
	default:
		writeError(w, http.StatusBadRequest, "order: want asc or desc")
		return
	}
	if v := q.Get("hold"); v != "" {
		f.Hold = v
	}
	// A client can tell from this header that the order was applied; a
	// server before stage 3 ignored the parameter.
	if f.Desc {
		w.Header().Set("Heraldo-Order", "desc")
	} else {
		w.Header().Set("Heraldo-Order", "asc")
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			writeError(w, http.StatusBadRequest, "limit: want 1 to 500")
			return
		}
		f.Limit = n
	}
	if v := q.Get("kind"); v != "" {
		if !api.Kind(v).Valid() {
			writeError(w, http.StatusBadRequest, "kind: %q is not a kind", v)
			return
		}
		f.Kind = v
	}
	recs, err := s.Store.List(r.Context(), f)
	if err == nil {
		err = s.Store.WithDeliveries(r.Context(), recs)
	}
	if err != nil {
		s.Logger.Error("list", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	p := page{Events: make([]json.RawMessage, 0, len(recs))}
	for _, rec := range recs {
		p.Events = append(p.Events, rec.Event)
	}
	if len(recs) > 0 {
		c := store.CursorFor(recs[len(recs)-1].Seq)
		p.NextCursor = &c
	}
	writeJSON(w, http.StatusOK, p)
}

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
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/auth"
	"github.com/noahkiss/heraldo/internal/hold"
	"github.com/noahkiss/heraldo/internal/ids"
	"github.com/noahkiss/heraldo/internal/routine"
	"github.com/noahkiss/heraldo/internal/rules"
	"github.com/noahkiss/heraldo/internal/settings"
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
	// Live gets every newly stored progress event; nil drives no Live
	// Activities.
	Live Live
	// TrustedNets may post to the Apprise compatibility paths with no
	// credential. Empty means none.
	TrustedNets []netip.Prefix
	// Hold is the alert hold; the zero value is off.
	Hold HoldOptions
	// Settings is the settings store. nil means one over Store with only
	// the built-in defaults, no flags, env aliases or secret key.
	Settings *settings.Manager
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
	// hmu guards deadmanActive, the hold's dead-man as the last tick saw it.
	hmu           sync.Mutex
	deadmanActive bool
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
	if o.Hold.Mode == "" {
		o.Hold.Mode = hold.Off
	}
	if o.Hold.Deadline == 0 {
		o.Hold.Deadline = 30 * time.Minute
	}
	if o.Hold.Deadman == 0 {
		o.Hold.Deadman = 20 * time.Minute
	}
	if o.Settings == nil {
		var b settings.Backend
		if o.Store != nil {
			b = o.Store
		}
		m, err := settings.New(context.Background(), settings.Options{Backend: b, Now: o.Now, Logger: o.Logger})
		if err != nil {
			o.Logger.Error("settings: load; serving the defaults", "err", err)
			m, _ = settings.New(context.Background(), settings.Options{Now: o.Now, Logger: o.Logger})
		}
		o.Settings = m
	}
	s := &server{Options: o, hub: newHub(), started: o.Now()}
	if e, ok := o.Live.(interface {
		SetEmit(func(context.Context, map[string]any) error)
	}); ok {
		e.SetEmit(s.emit)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("POST /v1/events", s.auth(auth.ScopeWrite, false, s.postEvent))
	mux.HandleFunc("GET /v1/events", s.auth(auth.ScopeRead, false, s.listEvents))
	mux.HandleFunc("GET /v1/events/{id}", s.auth(auth.ScopeRead, false, s.getEvent))
	mux.HandleFunc("GET /v1/events/{id}/why", s.auth(auth.ScopeRead, false, s.getEventWhy))
	mux.HandleFunc("GET /v1/send", s.auth(auth.ScopeWrite, true, s.sendEvent))
	mux.HandleFunc("GET /v1/stream", s.auth(auth.ScopeRead, false, s.stream))
	mux.HandleFunc("GET /v1/state", s.auth(auth.ScopeRead, false, s.state))
	mux.HandleFunc("GET /v1/projects", s.auth(auth.ScopeRead, false, s.listProjects))
	mux.HandleFunc("GET /v1/cursors/{name}", s.auth(auth.ScopeRead, false, s.getCursor))
	mux.HandleFunc("PUT /v1/cursors/{name}", s.auth(auth.ScopeRead, false, s.putCursor))
	mux.HandleFunc("GET /v1/devices", s.auth(DeviceScope, false, s.listDevices))
	mux.HandleFunc("POST /v1/devices", s.auth(DeviceScope, false, s.registerDevice))
	mux.HandleFunc("GET /v1/devices/{id}", s.auth(DeviceScope, false, s.getDevice))
	mux.HandleFunc("PUT /v1/devices/{id}", s.auth(DeviceScope, false, s.updateDevice))
	mux.HandleFunc("DELETE /v1/devices/{id}", s.auth(DeviceScope, false, s.removeDevice))
	mux.HandleFunc("GET /v1/activities", s.auth(auth.ScopeRead, false, s.listActivities))
	mux.HandleFunc("GET /v1/activities/{key...}", s.auth(auth.ScopeRead, false, s.getActivity))
	mux.HandleFunc("PUT /v1/activity-instances/{id}", s.auth(DeviceScope, false, s.reportInstance))
	s.compatRoutes(mux)
	s.appriseRoutes(mux)
	s.holdRoutes(mux)
	s.standbyRoutes(mux)
	s.settingsRoutes(mux)
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
// string, because /v1/send may carry a token there; and a key in a /p/ or
// /notify/ path is logged as <key>.
func (s *server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		var who string
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, &who))
		clean := cleanPath(r.URL.Path)
		if (strings.HasPrefix(clean, "/p/") || strings.HasPrefix(clean, "/notify/")) && clean != r.URL.Path {
			// ServeMux would answer a redirect, which a cron line's curl does
			// not follow: serve the clean ping path instead.
			u := *r.URL
			u.Path, u.RawPath = clean, ""
			r.URL = &u
		}
		next.ServeHTTP(sw, r)
		if r.URL.Path == "/healthz" && sw.status == http.StatusOK {
			return
		}
		// The clean path, so no spelling of /p/<key> reaches the log.
		s.Logger.Info("request", "method", r.Method, "path", redactPath(clean), "status", sw.status,
			"ms", time.Since(start).Milliseconds(), "token", who)
	})
}

// cleanPath is p with empty, . and .. segments resolved, as ServeMux
// resolves them.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	return path.Clean(p)
}

// redactPath hides the ping key in a clean /p/<key>[/op] path, and the key
// in /notify/<key> unless it is Apprise's default key, apprise.
func redactPath(p string) string {
	if key, ok := strings.CutPrefix(p, "/notify/"); ok && key != "apprise" {
		return "/notify/<key>"
	}
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
				writeError(w, http.StatusUnauthorized, "the token parameter is accepted only on /v1/send and the Notify! compatibility paths; use the Authorization header")
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
	h := api.Health{Status: api.HealthStatusOk, Version: s.Version, Db: api.HealthDbOk, Events: &n, Push: &s.PushState}
	if s.PushState == api.HealthPushOn {
		now := s.Now()
		if u, err := s.Store.Unsent(r.Context(), now.Add(-UnsentWindow), now.Add(-UnsentAfter)); err != nil {
			s.Logger.Error("health: unsent", "err", err)
		} else {
			h.Unsent = &u
		}
	}
	writeJSON(w, http.StatusOK, h)
}

// /healthz counts as unsent the events received in the UnsentWindow before
// now that should have pushed and reached no device UnsentAfter after they
// arrived.
const (
	UnsentAfter  = 15 * time.Minute
	UnsentWindow = 24 * time.Hour
)

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
	"image": true, "icon": true, "sound": true, "dedup": true, "expires_at": true, "project": true,
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
	recs := []store.Record{rec}
	if err := store.WithCursors(recs); err != nil {
		s.Logger.Error("cursor", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(recs[0].Event, '\n'))
}

// ingest types, stores and announces one input: it pokes the event streams
// and queues the push. trusted is the trusted-ingest capability. It logs its
// own errors.
func (s *server) ingest(ctx context.Context, in *validate.Input, trusted bool) (store.Record, bool, error) {
	set := s.Rules.Rules()
	typed, rule, err := rules.Type(set, in, s.Validator)
	if typed == nil {
		s.Logger.Error("type", "err", err)
		return store.Record{}, false, err
	}
	if err != nil {
		// The sender's event is valid; a rule that cannot type it must not
		// lose it.
		s.Logger.Warn("rules: matched, but the typed event is not valid; stored as a message", "err", err)
	}
	return s.insert(ctx, typed, set.Hold(rule), trusted)
}

// ingestPing is ingest for the check-in event of a ping: the ping rules
// run on it instead of the rules for untyped inputs.
func (s *server) ingestPing(ctx context.Context, in *validate.Input) (store.Record, bool, error) {
	set := s.Rules.Rules()
	out, rule, err := rules.ApplyPing(set, in, s.Validator)
	if err != nil {
		s.Logger.Warn("rules: matched a ping, but the result is not valid; stored as sent", "err", err)
	}
	return s.insert(ctx, out, set.Hold(rule), false)
}

// insert stores and announces a typed input. hold, when set, is the hold a
// rule gave it. It files the input under its project first, on every path.
func (s *server) insert(ctx context.Context, in *validate.Input, hold map[string]any, trusted bool) (store.Record, bool, error) {
	in, _, err := rules.WithProject(s.Rules.Rules(), in, s.Validator)
	if err != nil {
		s.Logger.Warn("rules: the project is not valid; stored without it", "err", err)
	}
	now := s.Now().UTC()
	id := ids.New(now)
	f := in.Fields
	f["id"] = id
	f["received_at"] = now.Format(time.RFC3339Nano)
	if _, ok := f["ts"]; !ok {
		f["ts"] = now.Format(time.RFC3339Nano)
	}
	if hold != nil {
		f["hold"] = hold
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
		if s.Live != nil && in.Typed.Kind != nil && *in.Typed.Kind == api.KindProgress && in.Typed.Key != nil {
			action := ""
			if in.Typed.Action != nil {
				action = string(*in.Typed.Action)
			}
			s.Live.Stored(id, *in.Typed.Key, action)
		}
		if *in.Typed.Kind == api.KindCheckin && in.Typed.Key != nil {
			s.judgeCheck(ctx, *in.Typed.Key)
		}
	}
	return rec, created, nil
}

// emit stores an event the server makes itself, such as an activity's
// history event.
func (s *server) emit(ctx context.Context, fields map[string]any) error {
	in, err := s.Validator.Fields(fields)
	if err != nil {
		return err
	}
	_, _, err = s.ingest(ctx, in, false)
	return err
}

// bridgedHold returns the stored hold for a bridged sender's triage state,
// details.tee.hold, or nil when there is none. Only a trusted-ingest token
// gets here. Only an alert the hold kept back, state held and not
// forwarded, is marked store_only. Everything else the bridge sends went
// out on arrival and pushes at its level: a bypass, a held alert forwarded
// at once (shadow mode, or the dead-man tripped), and the later forwards of
// a release, a deadline or the dead-man notice ({forward, ids}).
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
	delete(h, "store_only")
	state, _ := src["state"].(string)
	forwarded, _ := src["forwarded"].(bool)
	if state == "held" && !forwarded {
		h["store_only"] = true
	}
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
		if err == nil {
			err = s.Store.WithWhy(r.Context(), recs)
		}
		if err == nil {
			err = store.WithCursors(recs)
		}
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
	projects, err := s.projectFilter(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	f := store.Filter{
		Limit:    100,
		Source:   q.Get("source"),
		Host:     q.Get("host"),
		Key:      q.Get("key"),
		Tag:      q.Get("tag"),
		Q:        q.Get("q"),
		Projects: projects,
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
	switch v := q.Get("routine"); v {
	case "":
	case routine.Exclude, routine.Only:
		f.Routine = v
		// A server before the filter ignores the parameter and sends no
		// header, so a client can tell.
		w.Header().Set("Heraldo-Routine", v)
	default:
		writeError(w, http.StatusBadRequest, "routine: want exclude or only")
		return
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
	if err == nil {
		err = s.Store.WithWhy(r.Context(), recs)
	}
	if err == nil {
		err = store.WithCursors(recs)
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

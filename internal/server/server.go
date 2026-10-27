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
	// Rules classifies untyped messages at ingest; nil means no rules.
	Rules  *rules.Watcher
	Logger *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

type server struct {
	Options
}

// New returns the server's HTTP handler.
func New(o Options) http.Handler {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Tokens == nil {
		o.Tokens = &auth.Set{}
	}
	s := &server{o}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("POST /v1/events", s.auth(auth.ScopeWrite, false, s.postEvent))
	mux.HandleFunc("GET /v1/events", s.auth(auth.ScopeRead, false, s.listEvents))
	mux.HandleFunc("GET /v1/events/{id}", s.auth(auth.ScopeRead, false, s.getEvent))
	mux.HandleFunc("GET /v1/send", s.auth(auth.ScopeWrite, true, s.sendEvent))
	return s.logRequests(mux)
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

// logRequests logs one line per request. It logs the path, never the query
// string, because /v1/send may carry a token there.
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
		s.Logger.Info("request", "method", r.Method, "path", r.URL.Path, "status", sw.status,
			"ms", time.Since(start).Milliseconds(), "token", who)
	})
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
			Status: api.HealthStatusError, Version: s.Version, Db: api.HealthDbError,
		})
		return
	}
	writeJSON(w, http.StatusOK, api.Health{
		Status: api.HealthStatusOk, Version: s.Version, Db: api.HealthDbOk, Events: &n,
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
	in, _, err := rules.Apply(s.Rules.Rules(), in, s.Validator)
	if err != nil {
		// The sender's event is valid; a rule that cannot type it must not
		// lose it.
		s.Logger.Warn("rules: matched, but the typed event is not valid; stored as sent", "err", err)
	}
	now := s.Now().UTC()
	id := ids.New(now)
	f := in.Fields
	f["id"] = id
	f["received_at"] = now.Format(time.RFC3339Nano)
	if _, ok := f["ts"]; !ok {
		f["ts"] = now.Format(time.RFC3339Nano)
	}
	trusted := tokenFrom(r).Has(auth.CapTrustedIngest)
	if trusted {
		if h := bridgedHold(f); h != nil {
			f["hold"] = h
		}
	}
	rec, created, err := s.Store.Insert(r.Context(), store.NewEvent{
		ID: id, ReceivedAt: now, Fields: f, Typed: in.Typed, DedupAnyAge: trusted,
	})
	if err != nil {
		s.Logger.Error("store", "err", err)
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

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/auth"
	"github.com/noahkiss/heraldo/internal/rules"
	"github.com/noahkiss/heraldo/internal/store"
	"github.com/noahkiss/heraldo/internal/validate"
)

// The Apprise compatibility ingest, so a sender written for apprise-api (or
// for an apprise-tee in front of it) needs only a new URL. docs/api.md,
// "Apprise compatibility".
//
//	POST /notify/{key}   Apprise's keyed notify: JSON, form or multipart
//	POST /v1/notify      the same message, with id, delivered and hold_id
//	GET  /recent         the newest events, in the tee catalogue's shape
//	GET  /entry/{id}     one event in that shape
//
// The hold's endpoints, /held and /v1/hold, are in hold.go.

// trustedName is the token name of a sender let in by HERALDO_NOTIFY_TRUSTED_NETS.
const trustedName = "trusted-net"

func (s *server) appriseRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /notify/{key}", s.notifyAuth(auth.ScopeWrite, true, s.notify))
	mux.HandleFunc("POST /v1/notify", s.notifyAuth(auth.ScopeWrite, false, s.notify))
	mux.HandleFunc("GET /recent", s.notifyAuth(auth.ScopeRead, false, s.recent))
	mux.HandleFunc("GET /entry/{id}", s.notifyAuth(auth.ScopeRead, false, s.entry))
}

// notifyAuth authenticates the compatibility paths. In order: a bearer
// token with scope need; for pathKey, a token with the notify-key
// capability as the {key} path segment; with no credential at all, a
// client address in a trusted network.
func (s *server) notifyAuth(need auth.Scope, pathKey bool, h http.HandlerFunc) http.HandlerFunc {
	bearer := s.auth(need, false, h)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.URL.Query().Has("token") {
			bearer(w, r)
			return
		}
		t, ok := auth.Token{}, false
		if pathKey {
			if t, ok = s.Tokens.Lookup(r.PathValue("key")); ok && !t.Has(auth.CapNotifyKey) {
				ok = false
			}
		}
		if !ok && s.trusted(r) {
			t, ok = auth.Token{Name: trustedName, Scope: need}, true
		}
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer`)
			writeError(w, http.StatusUnauthorized, "missing or unknown token: use a bearer token, a notify-key token as the key, or post from a trusted network")
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

// clientAddr is the address the request came from, without the port.
func clientAddr(r *http.Request) string {
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		return ap.Addr().Unmap().String()
	}
	return r.RemoteAddr
}

// trusted reports whether the request came from a trusted network. It reads
// the connection's address only, never a forwarding header.
func (s *server) trusted(r *http.Request) bool {
	if len(s.TrustedNets) == 0 {
		return false
	}
	a, err := netip.ParseAddr(clientAddr(r))
	if err != nil {
		return false
	}
	for _, p := range s.TrustedNets {
		if p.Contains(a.Unmap()) {
			return true
		}
	}
	return false
}

// ---- the message -------------------------------------------------------

// appriseAnswer is an Apprise-shaped answer: error null when sent, and
// detail lines. Senders look for "Sent" in the first 500 bytes.
func appriseAnswer(now time.Time, sent bool, lines ...string) map[string]any {
	ts := now.Format("2006-01-02 15:04:05,000")
	details := make([][]string, 0, len(lines))
	sev := "INFO"
	if !sent {
		sev = "WARNING"
	}
	for _, l := range lines {
		details = append(details, []string{sev, ts, l})
	}
	var e any
	if !sent {
		e = "One or more notification could not be sent."
	}
	return map[string]any{"error": e, "details": details}
}

// notifyFields reads a notify body: a JSON object, a form, or multipart
// (whose files are dropped). An empty body is an empty map; nil means the
// body does not parse.
func notifyFields(ctype string, raw []byte) map[string]any {
	mt, params, _ := mime.ParseMediaType(ctype)
	switch {
	case mt == "application/x-www-form-urlencoded":
		q, err := url.ParseQuery(string(raw))
		if err != nil {
			return nil
		}
		f := map[string]any{}
		for k, v := range q {
			f[k] = v[len(v)-1]
		}
		return f
	case strings.HasPrefix(mt, "multipart/"):
		mr := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
		f := map[string]any{}
		for {
			p, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				return f
			}
			if err != nil {
				return nil
			}
			if p.FileName() == "" && p.FormName() != "" {
				b, _ := io.ReadAll(io.LimitReader(p, MaxRequestBytes))
				f[p.FormName()] = string(b)
			}
		}
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}
	}
	var f map[string]any
	if json.Unmarshal(raw, &f) != nil || f == nil {
		return nil
	}
	return f
}

// tagOf is the post's rung: tag, else tags; a list joins with commas.
func tagOf(f map[string]any) string {
	v, ok := f["tag"]
	if !ok {
		v = f["tags"]
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case []any:
		parts := make([]string, 0, len(t))
		for _, x := range t {
			parts = append(parts, fmt.Sprint(x))
		}
		return strings.Join(parts, ",")
	}
	return ""
}

func str(f map[string]any, k string) string {
	switch v := f[k].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

var (
	srcSlash = regexp.MustCompile(`^([A-Za-z][\w.-]*/[\w.-]+)`)
	srcColon = regexp.MustCompile(`^([A-Za-z][\w.-]{0,23}):\s`)
)

// deriveSource is the sender named at the start of a title, as in
// "Gatus/web-1 ..." or "kopia: ...", or fallback.
func deriveSource(title, fallback string) string {
	t := strings.TrimSpace(title)
	if m := srcSlash.FindStringSubmatch(t); m != nil {
		return m[1]
	}
	if m := srcColon.FindStringSubmatch(t); m != nil {
		return m[1]
	}
	return fallback
}

// message is a post in the one message shape the hold and ingest use:
// title, body, tag (the rung; level, when set, replaces it), source,
// project, url, image, and Apprise's type and format. Every value is a
// string.
func message(f map[string]any, fallbackSource string) map[string]any {
	m := map[string]any{}
	for _, k := range []string{"title", "body", "project", "url", "image", "type", "format"} {
		if v := str(f, k); v != "" {
			m[k] = v
		}
	}
	tag := tagOf(f)
	if l := strings.TrimSpace(str(f, "level")); l != "" {
		tag = l
	}
	if tag != "" {
		m["tag"] = tag
	}
	src := strings.TrimSpace(str(f, "source"))
	if src == "" {
		src = deriveSource(str(m, "title"), fallbackSource)
	}
	m["source"] = src
	return m
}

// RungLevels maps a ladder rung, the first tag, to an envelope level.
var RungLevels = map[string]api.Level{
	"ops": api.LevelCritical, "page": api.LevelTimeSensitive, "alert": api.LevelTimeSensitive,
	"general": api.LevelActive, "quiet": api.LevelPassive,
}

var tagSplit = regexp.MustCompile(`[,\s]+`)

// cutBytes shortens s to at most n bytes on a character boundary.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func webURL(v string, httpsOnly bool) string {
	u, err := url.Parse(strings.TrimSpace(v))
	if err != nil || u.Host == "" || (u.Scheme != "https" && (httpsOnly || u.Scheme != "http")) {
		return ""
	}
	return u.String()
}

// envelope is message m as an untyped envelope input, so the ingest rules
// type it. extra lands in details.notify.
func envelope(m map[string]any, extra map[string]any) map[string]any {
	ev := map[string]any{"source": cut(str(m, "source"), 128)}
	if ev["source"] == "" {
		ev["source"] = "unknown"
	}
	if t := str(m, "title"); t != "" {
		ev["title"] = cut(t, 256)
	}
	if b := str(m, "body"); b != "" {
		ev["body"] = cutBytes(b, validate.MaxBodyBytes)
	}
	var tags []any
	for _, t := range tagSplit.Split(str(m, "tag"), -1) {
		if t != "" && len(tags) < 16 {
			tags = append(tags, cut(t, 64))
		}
	}
	if len(tags) > 0 {
		ev["tags"] = tags
		first := strings.ToLower(tags[0].(string))
		if l, ok := RungLevels[first]; ok {
			ev["level"] = string(l)
		} else if api.Level(first).Valid() {
			ev["level"] = first
		}
	}
	if p := validate.FoldProject(str(m, "project")); validate.ValidProject(p) {
		// A name the envelope refuses is dropped: the message still counts.
		ev["project"] = p
	}
	if u := webURL(str(m, "url"), false); u != "" {
		ev["url"] = u
	}
	if u := webURL(str(m, "image"), true); u != "" {
		ev["image"] = u
	}
	d := map[string]any{}
	for k, v := range extra {
		d[k] = v
	}
	for _, k := range []string{"type", "format"} {
		if v := str(m, k); v != "" {
			d[k] = v
		}
	}
	if len(d) > 0 {
		ev["details"] = map[string]any{"notify": d}
	}
	return ev
}

// ingestNotify types and stores message m. hold is the hold state to store;
// a rule's own hold wins unless hold says held. prefix, when set, goes in
// front of the title after the rules typed the event, so a forward types
// like the original.
func (s *server) ingestNotify(ctx context.Context, m, extra, hold map[string]any, prefix string) (store.Record, error) {
	in, err := s.Validator.Fields(envelope(m, extra))
	if err != nil {
		// A field the schema refuses (an odd url) must not lose the message.
		f := envelope(m, extra)
		delete(f, "url")
		delete(f, "image")
		if in, err = s.Validator.Fields(f); err != nil {
			return store.Record{}, err
		}
	}
	set := s.Rules.Rules()
	typed, rule, err := rules.Type(set, in, s.Validator)
	if typed == nil {
		return store.Record{}, err
	}
	if err != nil {
		s.Logger.Warn("rules: matched, but the typed event is not valid; stored as a message", "err", err)
	}
	if prefix != "" {
		f := make(map[string]any, len(typed.Fields))
		for k, v := range typed.Fields {
			f[k] = v
		}
		f["title"] = cut(prefix+str(f, "title"), 256)
		if t, err := s.Validator.Fields(f); err == nil {
			typed = t
		}
	}
	h := hold
	if rh := set.Hold(rule); rh != nil && (hold == nil || hold["state"] != "held") {
		h = rh
		if id, ok := hold["id"]; ok {
			h["id"] = id
		}
	}
	rec, _, err := s.insert(ctx, typed, h, false)
	return rec, err
}

// notify is POST /notify/{key} and POST /v1/notify.
func (s *server) notify(w http.ResponseWriter, r *http.Request) {
	now := s.Now()
	v1 := r.URL.Path == "/v1/notify"
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequestBytes))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, appriseAnswer(now, false, "the body is over 1 MiB"))
		return
	}
	f := notifyFields(r.Header.Get("Content-Type"), raw)
	if f == nil {
		writeJSON(w, http.StatusBadRequest, appriseAnswer(now, false, "the body is not JSON, a form or multipart"))
		return
	}
	if str(f, "body") == "" && (v1 || str(f, "title") == "") {
		writeJSON(w, http.StatusBadRequest, appriseAnswer(now, false, "the post needs a body"))
		return
	}
	client := clientAddr(r)
	fallback := tokenFrom(r).Name
	if fallback == trustedName {
		fallback = client
	}
	m := message(f, fallback)
	path := r.URL.Path
	if !v1 {
		path = "/notify/<key>"
		if r.PathValue("key") == "apprise" {
			path = "/notify/apprise"
		}
	}
	extra := map[string]any{"client": client, "path": path}
	res := s.holdIn(r.Context(), m, extra, now)
	if res.err != nil {
		s.Logger.Error("notify: store", "err", res.err)
		out := appriseAnswer(now, false, "heraldo did not store the message (HTTP 500).")
		if v1 {
			out["delivered"] = false
		}
		writeJSON(w, http.StatusFailedDependency, out)
		return
	}
	var out map[string]any
	if res.held {
		out = appriseAnswer(now, true, fmt.Sprintf("Held for triage as %s; Sent to the hold queue, not yet pushed.", res.holdID))
	} else {
		out = appriseAnswer(now, true, fmt.Sprintf("Sent via heraldo as %s.", res.rec.ID))
	}
	if v1 {
		out["id"] = res.rec.ID
		out["delivered"] = !res.held
		out["backends"] = map[string]any{"heraldo": map[string]any{
			"ok": true, "status": http.StatusCreated, "ms": s.Now().Sub(now).Milliseconds(), "required": true}}
		if res.holdID != "" {
			out["hold_id"] = res.holdID
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- the catalogue view -----------------------------------------------

// teeEntry is an event in the shape of an apprise-tee catalogue line, for
// readers of the tee's /recent: id, ts (epoch seconds), time, client,
// path, tag, title, body, status, delivered, source, and the hold keys.
func teeEntry(raw json.RawMessage) map[string]any {
	var ev map[string]any
	if json.Unmarshal(raw, &ev) != nil {
		return map[string]any{"corrupt": cut(string(raw), 200)}
	}
	out := map[string]any{"id": ev["id"], "status": 200, "delivered": true}
	for _, k := range []string{"source", "project", "title", "body", "kind", "key", "action", "level", "url", "image"} {
		if v, ok := ev[k]; ok {
			out[k] = v
		}
	}
	if t, err := time.Parse(time.RFC3339Nano, str(ev, "received_at")); err == nil {
		out["ts"] = float64(t.UnixMilli()) / 1000
		out["time"] = t.Local().Format("2006-01-02 15:04:05 MST")
	}
	var tags []string
	if ts, ok := ev["tags"].([]any); ok {
		for _, t := range ts {
			tags = append(tags, fmt.Sprint(t))
		}
	}
	out["tag"] = nil
	if len(tags) > 0 {
		out["tag"] = strings.Join(tags, ",")
	}
	details, _ := ev["details"].(map[string]any)
	for _, ns := range []string{"tee", "notify"} {
		d, _ := details[ns].(map[string]any)
		for _, k := range []string{"client", "path"} {
			if v, ok := d[k]; ok {
				out[k] = v
			}
		}
	}
	if h, ok := ev["hold"].(map[string]any); ok {
		state := str(h, "state")
		switch state {
		case "held":
			out["delivered"] = false
		case "passed":
			state = "held"
		}
		if so, _ := h["store_only"].(bool); so {
			out["delivered"] = false
		}
		out["hold"] = state
		if id := str(h, "id"); id != "" {
			out["hold_id"] = id
		}
		if v, ok := h["reason"]; ok && state == "forward" {
			out["hold_reason"] = v
		}
		if v, ok := h["ids"]; ok {
			out["hold_ids"] = v
		}
	}
	return out
}

func sourceMatches(have, want string) bool {
	have, want = strings.ToLower(have), strings.ToLower(want)
	before, _, _ := strings.Cut(have, "/")
	return have == want || before == want
}

// recentScan caps how many events a filtered /recent reads.
const recentScan = 5000

// recent is GET /recent?n=&tag=&source=: the newest n events, newest last.
// tag=none keeps untagged events; source matches exactly or by the part
// before "/", ignoring case.
func (s *server) recent(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	n := 50
	if v, err := strconv.Atoi(q.Get("n")); err == nil {
		n = min(max(v, 1), 1000)
	}
	tag, src := q.Get("tag"), q.Get("source")
	var out []map[string]any
	f := store.Filter{Desc: true, Limit: 500}
	for scanned := 0; len(out) < n && scanned < recentScan; {
		recs, err := s.Store.List(r.Context(), f)
		if err != nil {
			s.Logger.Error("recent", "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		for _, rec := range recs {
			e := teeEntry(rec.Event)
			t, _ := e["tag"].(string)
			if tag != "" && !(tag == "none" && t == "") && t != tag {
				continue
			}
			if src != "" && !sourceMatches(str(e, "source"), src) {
				continue
			}
			if out = append(out, e); len(out) == n {
				break
			}
		}
		scanned += len(recs)
		if len(recs) < f.Limit {
			break
		}
		f.BeforeSeq = recs[len(recs)-1].Seq
	}
	// Newest last, as the tee answered.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if out == nil {
		out = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, out)
}

// entry is GET /entry/{id}: one event in the catalogue shape.
func (s *server) entry(w http.ResponseWriter, r *http.Request) {
	rec, err := s.Store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no event with that id")
		return
	}
	if err != nil {
		s.Logger.Error("entry", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, teeEntry(rec.Event))
}

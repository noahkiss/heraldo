package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/noahkiss/heraldo/internal/auth"
	"github.com/noahkiss/heraldo/internal/ids"
	"github.com/noahkiss/heraldo/internal/store"
)

// The Notify! Live Activity API, so a sender written for Notify! (such as
// notify-bambuddy) needs only a new base URL and a heraldo write token.
// It maps each call onto kind progress events; heraldo then runs the
// activity as it runs any other. docs/api.md, "Notify! compatibility".
//
//	POST   /live-activity/{id}?token=&new=1   start (id is the sender's device id)
//	POST   /live-activity/{activityId}?token= update
//	DELETE /live-activity/{activityId}?token= end; body may carry keepFor
//	GET    /live-activity/{activityId}?token= status
//	GET    /live-activity/{id}?token=         the sender's running activities
//	GET    /link?id=&token=                   credential check
//	POST   /notify-json/{id}?token=           a plain notification

// compatPrefix starts every activity id the compat API hands out; the id
// is also the progress key.
const compatPrefix = "notify-la-"

// compatExpiry is the expiresAt the compat API reports. heraldo hands a long
// activity over to a new one itself, so a sender must not roll it over.
const compatExpiry = 10 * 365 * 24 * time.Hour

func (s *server) compatRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /live-activity/{id}", s.auth(auth.ScopeWrite, true, s.compatPost))
	mux.HandleFunc("DELETE /live-activity/{id}", s.auth(auth.ScopeWrite, true, s.compatEnd))
	mux.HandleFunc("GET /live-activity/{id}", s.auth(auth.ScopeWrite, true, s.compatGet))
	mux.HandleFunc("GET /link", s.auth(auth.ScopeWrite, true, s.compatLink))
	mux.HandleFunc("POST /notify-json/{id}", s.auth(auth.ScopeWrite, true, s.compatNotify))
}

func compatError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"success": false, "error": msg, "message": msg})
}

// compatBody reads a JSON object body; an empty body is an empty object.
func compatBody(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		compatError(w, http.StatusBadRequest, "could not read the body")
		return nil, false
	}
	body := map[string]any{}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			compatError(w, http.StatusBadRequest, "the body is not a JSON object")
			return nil, false
		}
	}
	return body, true
}

// compatContent maps Notify! content fields onto LiveContent. Notify!'s
// progress is a percentage, endsIn is seconds from now, steps and step are
// numbers, and the button is {title, url, open}. A JSON null clears the
// field, as in LiveContent.
func compatContent(b map[string]any, now time.Time) map[string]any {
	c := map[string]any{}
	for _, k := range []string{"title", "body", "symbol", "trailing", "status", "metrics"} {
		if v, ok := b[k]; ok {
			c[k] = v
		}
	}
	if v, ok := b["tint"]; ok {
		c["tint"] = compatTint(v)
	}
	if v, ok := b["progress"]; ok {
		if f, isNum := v.(float64); isNum {
			c["progress"] = min(max(f, 0), 100) / 100
		} else {
			c["progress"] = nil
		}
	}
	if v, ok := b["steps"]; ok {
		total, isNum := v.(float64)
		if !isNum || total < 1 {
			c["steps"] = nil
		} else {
			cur, _ := b["step"].(float64)
			c["steps"] = map[string]any{"total": int(total), "current": int(min(max(cur, 0), total))}
		}
	}
	if v, ok := b["endsIn"]; ok {
		if f, isNum := v.(float64); isNum && f > 0 {
			c["ends_at"] = now.Add(time.Duration(f) * time.Second).UTC().Format(time.RFC3339)
		} else {
			c["ends_at"] = nil
		}
	}
	if v, ok := b["button"]; ok {
		btn, isObj := v.(map[string]any)
		url, _ := btn["url"].(string)
		title, _ := btn["title"].(string)
		if !isObj || url == "" || title == "" {
			c["button"] = nil
		} else {
			typ := "http"
			if open, _ := btn["open"].(bool); open {
				typ = "open"
			}
			c["button"] = map[string]any{"id": "button", "title": title, "type": typ, "url": url}
		}
	}
	if v, ok := b["keepFor"].(float64); ok && v >= 0 {
		c["keep_for"] = int(v)
	}
	return c
}

// compatTint turns Notify!'s hex colors, with or without "#" and with the
// alpha first (AARRGGBB), into LiveContent's #RRGGBB or #RRGGBBAA. Anything
// else passes through for the schema to check.
func compatTint(v any) any {
	t, ok := v.(string)
	if !ok {
		return v
	}
	hex := strings.TrimPrefix(t, "#")
	if strings.Trim(hex, "0123456789abcdefABCDEF") != "" {
		return t
	}
	switch len(hex) {
	case 6:
		return "#" + hex
	case 8:
		return "#" + hex[2:] + hex[:2]
	}
	return t
}

// compatPost starts an activity (new=1, or an id that names no activity) or
// updates one.
func (s *server) compatPost(w http.ResponseWriter, r *http.Request) {
	body, ok := compatBody(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	now := s.Now()
	action := "update"
	a, err := s.Store.GetActivity(r.Context(), id)
	switch {
	case r.URL.Query().Get("new") == "1" || errors.Is(err, store.ErrNotFound) || !strings.HasPrefix(id, compatPrefix):
		action, id = "start", compatPrefix+strings.ToLower(ids.New(now))
	case err != nil:
		compatError(w, http.StatusInternalServerError, "internal error")
		return
	case !a.Active():
		compatError(w, http.StatusGone, "the Live Activity has ended")
		return
	}
	if action == "update" && s.allDismissed(r, a) {
		compatError(w, http.StatusGone, "the Live Activity was dismissed")
		return
	}
	if !s.compatStore(w, r, id, action, body, now) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "activityId": id, "expiresAt": now.Add(compatExpiry).UTC().Format(time.RFC3339),
	})
}

// compatEnd ends an activity. Ending an ended one succeeds.
func (s *server) compatEnd(w http.ResponseWriter, r *http.Request) {
	body, ok := compatBody(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	a, err := s.Store.GetActivity(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		compatError(w, http.StatusForbidden, "Invalid token or id not found")
		return
	}
	if err != nil {
		compatError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if a.Active() && !s.compatStore(w, r, id, "end", body, s.Now()) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "activityId": id})
}

func (s *server) compatStore(w http.ResponseWriter, r *http.Request, key, action string, body map[string]any, now time.Time) bool {
	f := map[string]any{
		"source": tokenFrom(r).Name, "kind": "progress", "key": key, "action": action,
		"content": compatContent(body, now),
	}
	if t, _ := body["title"].(string); t != "" && action == "start" {
		f["title"] = t
	}
	// Updates and the end keep the activity's project by its key.
	if p, _ := body["project"].(string); p != "" && action == "start" {
		f["project"] = p
	}
	in, err := s.Validator.Fields(f)
	if err != nil {
		compatError(w, http.StatusBadRequest, err.Error())
		return false
	}
	if _, _, err := s.ingest(r.Context(), in, false); err != nil {
		compatError(w, http.StatusInternalServerError, "could not store the event")
		return false
	}
	return true
}

// allDismissed reports whether every device's copy of a was dismissed or
// died, and at least one existed: the sender should stop.
func (s *server) allDismissed(r *http.Request, a store.Activity) bool {
	insts, err := s.Store.Instances(r.Context(), a.ID)
	if err != nil || len(insts) == 0 {
		return false
	}
	latest := map[string]store.Instance{}
	for _, in := range insts {
		if l, ok := latest[in.DeviceID]; !ok || in.Chain > l.Chain || (in.Chain == l.Chain && !in.RequestedAt.Before(l.RequestedAt)) {
			latest[in.DeviceID] = in
		}
	}
	for _, in := range latest {
		if in.State != store.InstanceDismissed && in.State != store.InstanceDead {
			return false
		}
	}
	return true
}

// compatStatus is one activity as Notify!'s status call shows it.
func (s *server) compatStatus(r *http.Request, a store.Activity) map[string]any {
	state, reason := "starting", any(nil)
	switch {
	case !a.Active():
		state = "ended"
		switch a.EndReason {
		case store.EndSender, store.EndRestarted:
			reason = "script"
		default:
			reason = a.EndReason
		}
	case s.allDismissed(r, a):
		state, reason = "dismissed", "dismissed"
	default:
		insts, _ := s.Store.Instances(r.Context(), a.ID)
		for _, in := range insts {
			if in.State == store.InstanceActive {
				state = "active"
			}
		}
	}
	return map[string]any{
		"activityId": a.Key, "state": state, "endReason": reason, "content": a.Content,
		"expiresAt": a.StartedAt.Add(compatExpiry).UTC().Format(time.RFC3339),
	}
}

// compatGet is the status of one activity, or, for any other id, the list
// of this sender's running activities.
func (s *server) compatGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if strings.HasPrefix(id, compatPrefix) {
		a, err := s.Store.GetActivity(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			compatError(w, http.StatusForbidden, "Invalid token or id not found")
			return
		}
		if err != nil {
			compatError(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, s.compatStatus(r, a))
		return
	}
	acts, err := s.Store.Activities(r.Context(), true)
	if err != nil {
		compatError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := []map[string]any{}
	for _, a := range acts {
		if strings.HasPrefix(a.Key, compatPrefix) && a.Source == tokenFrom(r).Name {
			out = append(out, s.compatStatus(r, a))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"activities": out})
}

// compatLink answers Notify!'s credential check: the token is good, and the
// id stands for this sender's devices.
func (s *server) compatLink(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "type": "device", "id": r.URL.Query().Get("id"), "name": "heraldo",
	})
}

// compatNotify stores a plain notification: a message event from the
// token's sender.
func (s *server) compatNotify(w http.ResponseWriter, r *http.Request) {
	body, ok := compatBody(w, r)
	if !ok {
		return
	}
	f := map[string]any{"source": tokenFrom(r).Name, "kind": "message"}
	if t, _ := body["text"].(string); t != "" {
		f["body"] = t
	}
	if t, _ := body["title"].(string); t != "" {
		f["title"] = cut(t, 256)
	}
	if g, _ := body["groupType"].(string); g != "" {
		f["thread"] = cut(g, 128)
	}
	if p, _ := body["project"].(string); p != "" {
		f["project"] = p
	}
	if u, _ := body["iconUrl"].(string); u != "" && len(u) <= 512 {
		f["icon"] = u
	}
	if ts, _ := body["timeSensitive"].(bool); ts {
		f["level"] = "time-sensitive"
	}
	in, err := s.Validator.Fields(f)
	if err != nil {
		compatError(w, http.StatusBadRequest, err.Error())
		return
	}
	rec, _, err := s.ingest(r.Context(), in, false)
	if err != nil {
		compatError(w, http.StatusInternalServerError, "could not store the event")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "id": rec.ID})
}

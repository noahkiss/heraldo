package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/settings"
)

// digestEvents lists the stored digest events.
func digestEvents(t *testing.T, e *env) []api.Event {
	t.Helper()
	page := decode[struct {
		Events []api.Event `json:"events"`
	}](t, e.do(t, http.MethodGet, "/v1/events?source=heraldo&limit=100", readTok, nil))
	var out []api.Event
	for _, ev := range page.Events {
		if ev.Thread != nil && *ev.Thread == "digest" {
			out = append(out, ev)
		}
	}
	return out
}

func mustPost(t *testing.T, e *env, body string) {
	t.Helper()
	if rec := e.post(t, body); rec.Code != http.StatusCreated {
		t.Fatalf("post %s: %d %s", body, rec.Code, rec.Body)
	}
}

// TestDigestOncePerDay: the digest goes out at digest.at, once a day,
// across a restart, and once after days of downtime.
func TestDigestOncePerDay(t *testing.T) {
	e := newEnv(t)
	// 2026-10-27 12:00 UTC is 13:00 in Europe/Paris; the digest is due at
	// 07:30 there.
	e.withSettings(t, settings.Options{Flags: map[string]string{
		"digest.enabled": "true", "digest.tz": "Europe/Paris", "stale.enabled": "true", "stale.after": "48h",
	}})
	ctx := context.Background()
	srv := e.h.(*Server)
	mustPost(t, e, `{"source":"mon","kind":"incident","key":"disk:a","action":"open","title":"Disk full","project":"lab"}`)

	sent, err := srv.Digest().Tick(ctx)
	if err != nil || !sent {
		t.Fatalf("first tick: %v %v", sent, err)
	}
	if sent, err := srv.Digest().Tick(ctx); err != nil || sent {
		t.Fatalf("second tick sent again: %v %v", sent, err)
	}
	// A restart: a new server over the same store.
	old := srv.s.Options
	e.h = New(old)
	srv = e.h.(*Server)
	if sent, _ := srv.Digest().Tick(ctx); sent {
		t.Fatal("sent again after a restart")
	}
	evs := digestEvents(t, e)
	if len(evs) != 1 {
		t.Fatalf("digests %d", len(evs))
	}
	d := evs[0]
	if d.Kind != api.KindMessage || d.Level == nil || *d.Level != api.LevelActive || d.Source != "heraldo" {
		t.Fatalf("digest event %+v", d)
	}
	if d.Body == nil || !strings.Contains(*d.Body, "`disk:a` Disk full (lab), open since") {
		t.Fatalf("body %q", deref(d.Body))
	}

	// Down for three days, back at 05:00 Paris: nothing is due yet. At
	// 07:30 one digest goes out, not one per missed day.
	e.clock.add(3*24*time.Hour - 8*time.Hour) // 2026-10-30 04:00 UTC, 05:00 Paris
	if sent, _ := srv.Digest().Tick(ctx); sent {
		t.Fatal("sent before digest.at")
	}
	e.clock.add(3 * time.Hour) // 08:00 Paris
	if sent, _ := srv.Digest().Tick(ctx); !sent {
		t.Fatal("no catch-up digest")
	}
	if sent, _ := srv.Digest().Tick(ctx); sent {
		t.Fatal("sent twice after downtime")
	}
	evs = digestEvents(t, e)
	if len(evs) != 2 {
		t.Fatalf("digests %d", len(evs))
	}
	// disk:a has had no update for 67 hours, past stale.after: stale.
	if !strings.Contains(*evs[1].Body, "### Stale") || !strings.Contains(*evs[1].Summary, "1 stale") {
		t.Fatalf("second digest %q / %q", *evs[1].Body, *evs[1].Summary)
	}
	// The window starts where the last one ended.
	last, found, err := e.st.LastDigest(ctx)
	if err != nil || !found || last.Day != "2026-10-30" || !last.Since.Equal(time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("last digest %+v %v %v", last, found, err)
	}
}

func TestDigestOff(t *testing.T) {
	e := newEnv(t)
	if sent, err := e.h.(*Server).Digest().Tick(context.Background()); err != nil || sent {
		t.Fatalf("digest off sent: %v %v", sent, err)
	}
}

func TestDigestPreviewAndSend(t *testing.T) {
	e := newEnv(t)
	m := e.withSettings(t, settings.Options{})
	ctx := context.Background()
	mustPost(t, e, `{"source":"mon","kind":"incident","key":"api:b","action":"open","title":"API down"}`)
	mustPost(t, e, `{"source":"mon","kind":"incident","key":"api:b","action":"resolve"}`)
	mustPost(t, e, `{"source":"x2d","kind":"progress","key":"print:1","action":"start","title":"Benchy","project":"printing"}`)
	mustPost(t, e, `{"source":"x2d","kind":"progress","key":"print:1","action":"end","title":"Benchy","project":"printing"}`)
	mustPost(t, e, `{"source":"cron","kind":"checkin","key":"backup","action":"fail"}`)
	mustPost(t, e, `{"source":"svc","title":"hello","project":"lab"}`)
	e.clock.add(time.Minute)

	rec := e.do(t, http.MethodGet, "/v1/digest/preview", readTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body)
	}
	// The answer is the documented shape, field for field.
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	var d api.DigestPreview
	if err := dec.Decode(&d); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	if d.Enabled || d.NextAt != nil || d.Level != "active" {
		t.Fatalf("preview schedule %+v", d)
	}
	if d.Incidents.Opened != 1 || len(d.Incidents.Resolved) != 1 || len(d.Incidents.Open) != 0 {
		t.Fatalf("incidents %+v", d.Incidents)
	}
	if len(d.Finished) != 1 || d.Finished[0].Title != "Benchy" {
		t.Fatalf("finished %+v", d.Finished)
	}
	if len(d.Checkins.Items) != 1 || d.Checkins.Items[0].Key != "backup" || d.Checkins.Items[0].Failures != 1 {
		t.Fatalf("checkins %+v", d.Checkins)
	}
	unfiled := noProject(d.Projects)
	if len(d.Projects) != 2 || unfiled != 3 {
		t.Fatalf("projects %+v", d.Projects)
	}
	if n := len(digestEvents(t, e)); n != 0 {
		t.Fatalf("preview stored %d digests", n)
	}

	if _, err := m.Set(ctx, "digest.enabled", json.RawMessage("true"), "test"); err != nil {
		t.Fatal(err)
	}
	d = decode[api.DigestPreview](t, e.do(t, http.MethodGet, "/v1/digest/preview", readTok, nil))
	// 12:01 UTC is past 07:30 UTC and nothing went out today: due now.
	if !d.Enabled || d.NextAt == nil || !d.NextAt.Equal(time.Date(2026, 10, 27, 7, 30, 0, 0, time.UTC)) {
		t.Fatalf("next_at %+v", d.NextAt)
	}

	if rec := e.do(t, http.MethodPost, "/v1/digest/send", readTok, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("send with a read token: %d", rec.Code)
	}
	rec = e.do(t, http.MethodPost, "/v1/digest/send", writeTok, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	sent := decode[api.DigestPreview](t, rec)
	if sent.EventId == nil || len(digestEvents(t, e)) != 1 {
		t.Fatalf("send answer %+v", sent)
	}
	// A manual send is not the day's digest: the tick still sends.
	if ok, err := e.h.(*Server).Digest().Tick(ctx); err != nil || !ok {
		t.Fatalf("tick after a manual send: %v %v", ok, err)
	}
	// The digests do not count themselves.
	after := decode[api.DigestPreview](t, e.do(t, http.MethodGet, "/v1/digest/preview", readTok, nil))
	if noProject(after.Projects) != unfiled {
		t.Fatalf("no-project events %d after the digests, %d before", noProject(after.Projects), unfiled)
	}
}

func TestDigestSendStandby(t *testing.T) {
	e := newEnvWith(t, func(o *Options) { o.Standby = true })
	if rec := e.do(t, http.MethodPost, "/v1/digest/send", writeTok, nil); rec.Code != http.StatusConflict {
		t.Fatalf("standby send: %d", rec.Code)
	}
	if rec := e.do(t, http.MethodGet, "/v1/digest/preview", readTok, nil); rec.Code != http.StatusOK {
		t.Fatalf("standby preview: %d", rec.Code)
	}
}

func TestStateStale(t *testing.T) {
	e := newEnv(t)
	m := e.withSettings(t, settings.Options{})
	ctx := context.Background()
	mustPost(t, e, `{"source":"mon","kind":"incident","key":"old","action":"open","title":"Old"}`)
	e.clock.add(73 * time.Hour)
	mustPost(t, e, `{"source":"mon","kind":"incident","key":"new","action":"open","title":"New"}`)

	staleKeys := func(path string) (all, stale []string) {
		t.Helper()
		rec := e.do(t, http.MethodGet, path, readTok, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		var raw struct {
			Incidents []json.RawMessage `json:"incidents"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &raw)
		s := storedSchema(t)
		for _, b := range raw.Incidents {
			checkStored(t, s, b)
			var ev api.Event
			_ = json.Unmarshal(b, &ev)
			all = append(all, *ev.Key)
			if ev.Incident != nil && ev.Incident.Stale != nil && *ev.Incident.Stale {
				stale = append(stale, *ev.Key)
			}
		}
		return all, stale
	}
	// stale.enabled off: no flags, but stale=1 still answers.
	if all, stale := staleKeys("/v1/state"); len(all) != 2 || len(stale) != 0 {
		t.Fatalf("off: %v %v", all, stale)
	}
	if all, stale := staleKeys("/v1/state?stale=1"); len(all) != 1 || len(stale) != 1 || stale[0] != "old" {
		t.Fatalf("stale=1: %v %v", all, stale)
	}
	if _, err := m.Set(ctx, "stale.enabled", json.RawMessage("true"), "test"); err != nil {
		t.Fatal(err)
	}
	if all, stale := staleKeys("/v1/state"); len(all) != 2 || len(stale) != 1 || stale[0] != "old" {
		t.Fatalf("on: %v %v", all, stale)
	}
	if rec := e.do(t, http.MethodGet, "/v1/state?stale=maybe", readTok, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("stale=maybe: %d", rec.Code)
	}
	// An update makes it fresh again.
	mustPost(t, e, `{"source":"mon","kind":"incident","key":"old","action":"update","title":"Old, still"}`)
	if _, stale := staleKeys("/v1/state?stale=1"); len(stale) != 0 {
		t.Fatalf("after an update: %v", stale)
	}
	// The stored event never carries the flag.
	evs, err := e.st.DigestIncidents(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range evs {
		if bytes.Contains(x.Event, []byte(`"stale"`)) {
			t.Fatalf("stored event has stale: %s", x.Event)
		}
	}
}

func noProject(ps []api.DigestProject) int {
	for _, p := range ps {
		if p.Name == "" {
			return p.Events
		}
	}
	return 0
}

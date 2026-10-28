package push

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/noahkiss/heraldo/api"
)

var testNow = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func event(t *testing.T, js string) *api.Event {
	t.Helper()
	var ev api.Event
	if err := json.Unmarshal([]byte(js), &ev); err != nil {
		t.Fatalf("%s: %v", js, err)
	}
	return &ev
}

// decideCases is the push table, by example. The pusher test runs the same
// cases through the store's SQL form of it. replace marks a passive push
// that goes out only over an earlier visible push of its key.
var decideCases = []struct {
	name    string
	ev      string
	push    bool
	level   api.Level
	replace bool
}{
	{"message", `{"source":"s","kind":"message"}`, true, api.LevelActive, false},
	{"message passive", `{"source":"s","kind":"message","level":"passive"}`, false, "", false},
	{"message passive with key", `{"source":"s","kind":"message","key":"k","level":"passive"}`, true, api.LevelPassive, true},
	{"message time-sensitive", `{"source":"s","kind":"message","level":"time-sensitive"}`, true, api.LevelTimeSensitive, false},
	{"critical downgraded", `{"source":"s","kind":"message","level":"critical"}`, true, api.LevelTimeSensitive, false},
	{"digest inbox-only", `{"source":"s","kind":"digest","level":"active"}`, false, "", false},
	{"incident open", `{"source":"s","kind":"incident","key":"k","action":"open","level":"time-sensitive"}`, true, api.LevelTimeSensitive, false},
	{"incident update", `{"source":"s","kind":"incident","key":"k","action":"update"}`, true, api.LevelActive, false},
	{"incident resolve", `{"source":"s","kind":"incident","key":"k","action":"resolve","level":"active"}`, true, api.LevelPassive, true},
	{"incident open passive", `{"source":"s","kind":"incident","key":"k","action":"open","level":"passive"}`, true, api.LevelPassive, true},
	{"checkin fail passive", `{"source":"s","kind":"checkin","action":"fail","level":"passive"}`, false, "", false},
	{"incident ack", `{"source":"s","kind":"incident","key":"k","action":"ack"}`, false, "", false},
	{"checkin fail", `{"source":"s","kind":"checkin","key":"k","action":"fail"}`, true, api.LevelActive, false},
	{"checkin exit code", `{"source":"s","kind":"checkin","key":"k","action":"success","checkin":{"exit_code":2}}`, true, api.LevelActive, false},
	{"checkin log with exit code", `{"source":"s","kind":"checkin","key":"k","action":"log","checkin":{"exit_code":2}}`, false, "", false},
	{"checkin success", `{"source":"s","kind":"checkin","key":"k","action":"success"}`, false, "", false},
	{"checkin start", `{"source":"s","kind":"checkin","key":"k","action":"start"}`, false, "", false},
	{"progress", `{"source":"s","kind":"progress","key":"k","action":"start"}`, false, "", false},
	{"status", `{"source":"s","kind":"status","key":"k","action":"set","content":{"title":"t"}}`, false, "", false},
	{"held", `{"source":"s","kind":"message","hold":{"state":"held"}}`, false, "", false},
	{"held, forwarded at once", `{"source":"s","kind":"message","hold":{"state":"held","forwarded":true}}`, true, api.LevelActive, false},
	{"bypass", `{"source":"s","kind":"message","hold":{"state":"bypass"}}`, true, api.LevelActive, false},
	{"store-only", `{"source":"s","kind":"message","hold":{"state":"released","store_only":true}}`, false, "", false},
	{"other hold state pushes", `{"source":"s","kind":"message","hold":{"state":"open"}}`, true, api.LevelActive, false},
}

func TestDecide(t *testing.T) {
	for _, c := range decideCases {
		d := Decide(event(t, c.ev), testNow)
		if d.Push != c.push || (c.push && d.Level != c.level) || d.Replace != c.replace {
			t.Errorf("%s: got %+v, want push %v level %q", c.name, d, c.push, c.level)
		}
		if !d.Push && d.Reason == "" {
			t.Errorf("%s: no reason", c.name)
		}
	}
	if d := Decide(event(t, `{"source":"s","kind":"message","level":"critical"}`), testNow); !d.Downgraded {
		t.Error("critical not marked downgraded")
	}
	past := `{"source":"s","kind":"message","expires_at":"2026-10-27T11:59:59Z"}`
	if d := Decide(event(t, past), testNow); d.Push || d.Reason != "expired" {
		t.Errorf("expired: %+v", d)
	}
}

func build(t *testing.T, js string, badge *int) (Payload, Headers, int) {
	t.Helper()
	ev := event(t, js)
	b, h, err := Build(ev, Decide(ev, testNow), Extra{Badge: badge})
	if err != nil {
		t.Fatal(err)
	}
	var p Payload
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	var raw map[string]map[string]any
	_ = json.Unmarshal(b, &raw)
	if raw["aps"]["mutable-content"] != float64(1) {
		t.Errorf("mutable-content: %v", raw["aps"]["mutable-content"])
	}
	return p, h, len(b)
}

func TestBuildIncident(t *testing.T) {
	n := 3
	p, h, _ := build(t, `{"id":"01JBA6Y8Q2K9M3T4V5W6X7Y8Z9","ts":"2026-10-27T12:00:00Z","source":"uptime",
		"kind":"incident","key":"unit:web-1:nginx","action":"open","title":"Web is down",
		"summary":"nginx failed","body":"long body","level":"critical","relevance":0.8,
		"url":"https://example.com/x","image":"https://example.com/i.png"}`, &n)
	a := p.APS
	if a.Alert.Title != "Web is down" || a.Alert.Body != "nginx failed" || !p.Heraldo.Truncated {
		t.Errorf("alert %+v truncated %v", a.Alert, p.Heraldo.Truncated)
	}
	if a.ThreadID != "uptime" || a.Category != CategoryIncident || a.InterruptionLevel != "time-sensitive" ||
		a.Sound != "default" || a.Badge == nil || *a.Badge != 3 || a.RelevanceScore == nil || *a.RelevanceScore != 0.8 {
		t.Errorf("aps %+v", a)
	}
	if p.Heraldo.ID != "01JBA6Y8Q2K9M3T4V5W6X7Y8Z9" || p.Heraldo.Kind != "incident" || p.Heraldo.Key != "unit:web-1:nginx" ||
		p.Heraldo.URL != "https://example.com/x" || p.Heraldo.Image != "https://example.com/i.png" {
		t.Errorf("heraldo %+v", p.Heraldo)
	}
	if h.CollapseID != CollapseID("unit:web-1:nginx") || len(h.CollapseID) != 32 || h.Priority != 10 {
		t.Errorf("headers %+v", h)
	}
}

func TestBuildQuietAndThread(t *testing.T) {
	// A keyed digest pushes only to replace a visible push of its key; when
	// it does, it is quiet.
	p, h, _ := build(t, `{"id":"x","ts":"2026-10-27T12:00:00Z","source":"backup","kind":"digest","key":"nightly",
		"thread":"nightly","body":"three jobs ran","sound":"chime"}`, nil)
	if p.APS.InterruptionLevel != "passive" || p.APS.Sound != "" || h.Priority != 5 || h.CollapseID != CollapseID("nightly") {
		t.Errorf("digest: aps %+v headers %+v", p.APS, h)
	}
	if p.APS.ThreadID != "nightly" || p.APS.Alert.Title != "backup" || p.APS.Alert.Body != "three jobs ran" ||
		p.APS.Category != CategoryEvent || p.APS.Badge != nil || p.Heraldo.Truncated {
		t.Errorf("digest: %+v", p)
	}
	p, _, _ = build(t, `{"id":"x","ts":"2026-10-27T12:00:00Z","source":"s","kind":"message","sound":"none"}`, nil)
	if p.APS.Sound != "" {
		t.Errorf("sound none: %q", p.APS.Sound)
	}
	p, h, _ = build(t, `{"id":"x","ts":"2026-10-27T12:00:00Z","source":"s","kind":"incident","key":"k","action":"resolve",
		"expires_at":"2026-10-28T00:00:00Z"}`, nil)
	if p.APS.Category != CategoryEvent || p.APS.InterruptionLevel != "passive" || h.Expiration != time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("resolve: aps %+v headers %+v", p.APS, h)
	}
}

func TestBuildTruncates(t *testing.T) {
	// Multi-byte characters and characters JSON escapes, so neither the cut
	// nor the size is a plain byte count.
	body := strings.Repeat("é\"<", 3000)
	js, _ := json.Marshal(map[string]any{"id": "x", "ts": "2026-10-27T12:00:00Z", "source": "s", "kind": "message",
		"title": "t", "body": body})
	p, _, size := build(t, string(js), nil)
	if size > MaxPayload || size < MaxPayload-16 {
		t.Fatalf("size %d, want just under %d", size, MaxPayload)
	}
	if !p.Heraldo.Truncated || !strings.HasSuffix(p.APS.Alert.Body, "…") || !utf8.ValidString(p.APS.Alert.Body) {
		t.Fatalf("truncated %v, body ends %q", p.Heraldo.Truncated, p.APS.Alert.Body[len(p.APS.Alert.Body)-8:])
	}
	if !strings.HasPrefix(body, strings.TrimSuffix(p.APS.Alert.Body, "…")) {
		t.Fatal("cut body is not a prefix of the body")
	}
	// A short body is sent whole.
	p, _, _ = build(t, `{"id":"x","ts":"2026-10-27T12:00:00Z","source":"s","kind":"message","body":"short"}`, nil)
	if p.Heraldo.Truncated || p.APS.Alert.Body != "short" {
		t.Fatalf("short: %+v", p)
	}
}

func TestBuildResolveDigestRealert(t *testing.T) {
	// A resolve says how long the incident was open, from the stored
	// incident snapshot, and keeps the open's collapse id.
	p, h, _ := build(t, `{"id":"x","ts":"2026-10-27T12:00:00Z","source":"uptime","kind":"incident","key":"web",
		"action":"resolve","title":"Web is up","summary":"nginx is back","body":"details",
		"incident":{"state":"resolved","opened_at":"2026-10-27T11:48:00Z","resolved_at":"2026-10-27T12:00:30Z"}}`, nil)
	if p.APS.Alert.Title != "Web is up" || p.APS.Alert.Body != "Resolved after 12 min" || p.Heraldo.Truncated ||
		h.CollapseID != CollapseID("web") || p.APS.InterruptionLevel != "passive" {
		t.Errorf("resolve: %+v %+v", p, h)
	}
	p, _, _ = build(t, `{"id":"x","ts":"2026-10-27T12:00:00Z","source":"s","kind":"incident","key":"k","action":"resolve",
		"incident":{"state":"resolved","opened_at":"2026-10-27T12:00:00Z","resolved_at":"2026-10-27T12:00:00Z"}}`, nil)
	if p.APS.Alert.Body != "Resolved" {
		t.Errorf("resolve without open: %q", p.APS.Alert.Body)
	}
	// A digest without a summary shows the first line of its body.
	p, _, _ = build(t, `{"id":"x","ts":"2026-10-27T12:00:00Z","source":"backup","kind":"digest",
		"body":"\n## Nightly: 3 jobs ran\n\n- a\n- b"}`, nil)
	if p.APS.Alert.Body != "Nightly: 3 jobs ran" || !p.Heraldo.Truncated {
		t.Errorf("digest: %q truncated %v", p.APS.Alert.Body, p.Heraldo.Truncated)
	}
	// A re-alert leads with how long the incident has been open; the icon
	// and an https icon URL travel in the payload.
	ev := event(t, `{"id":"x","ts":"2026-10-27T12:00:00Z","source":"uptime","kind":"incident","key":"web","action":"open",
		"title":"Web is down","summary":"nginx failed","level":"time-sensitive","icon":"https://example.com/i.png"}`)
	b, _, err := Build(ev, Decide(ev, testNow), Extra{OpenFor: 30 * time.Minute, Icon: "uptime"})
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(b, &p)
	if p.APS.Alert.Body != "Still open after 30 min. nginx failed" || p.Heraldo.Icon != "uptime" ||
		p.Heraldo.IconURL != "https://example.com/i.png" || p.APS.Category != CategoryIncident {
		t.Errorf("re-alert: %s", b)
	}
}

func TestDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Second:                "under a minute",
		12*time.Minute + 59*time.Second: "12 min",
		2 * time.Hour:                   "2 h",
		2*time.Hour + 5*time.Minute:     "2 h 5 min",
		72 * time.Hour:                  "3 d",
		76*time.Hour + 30*time.Minute:   "3 d 4 h",
	} {
		if got := Duration(d); got != want {
			t.Errorf("%s: %q, want %q", d, got, want)
		}
	}
	if got := FirstLine("**bold** start\nmore"); got != "**bold** start" {
		t.Errorf("first line %q", got)
	}
}

package mcptools

import (
	"net/http"
	"strings"
	"testing"

	"github.com/noahkiss/heraldo/internal/hold"
	"github.com/noahkiss/heraldo/internal/server"
)

func TestHoldReadAndAct(t *testing.T) {
	f := newFixtureWith(t, func(o *server.Options) { o.Hold.Mode = hold.On })
	for _, body := range []string{
		`{"title":"web-1 down","body":"` + strings.Repeat("x", 3000) + `","tag":"alert"}`,
		`{"title":"web-2 down","body":"2","tag":"alert"}`,
		`{"title":"db slow","body":"3","tag":"alert"}`,
	} {
		f.do(http.MethodPost, "/v1/notify", writeTok, body)
	}
	ro := f.connect(false)
	if _, ok := ro.tools["hold_act"]; ok {
		t.Fatal("hold_act listed without a write token")
	}
	out, text := ro.ok("hold_read", map[string]any{})
	entries := out["entries"].([]any)
	if out["mode"] != "on" || len(entries) != 3 || !strings.Contains(text, "web-1 down") {
		t.Fatalf("hold_read: %v\n%s", out, text)
	}
	first := entries[0].(map[string]any)
	if first["body_truncated"] != true || len(first["body"].(string)) > holdBodyCap+len("…") {
		t.Fatalf("body cap: %v", first)
	}
	// A look is not a poll.
	if q := f.do(http.MethodGet, "/v1/hold?peek=1", readTok, ""); q["last_poll_ts"] != nil {
		t.Fatalf("hold_read polled: %v", q)
	}
	ro.ok("hold_read", map[string]any{"poll": true})
	if q := f.do(http.MethodGet, "/v1/hold?peek=1", readTok, ""); q["last_poll_ts"] == nil {
		t.Fatalf("poll=true did not poll: %v", q)
	}

	s := f.connect(true)
	ids := []string{entries[0].(map[string]any)["id"].(string), entries[1].(map[string]any)["id"].(string)}
	dbID := entries[2].(map[string]any)["id"].(string)
	s.fail("hold_act", map[string]any{"action": "release", "ids": ids, "verdict": "REAL"}, "invalid")
	out, _ = s.ok("hold_act", map[string]any{"action": "release", "ids": ids, "verdict": "REAL",
		"title": "web tier down", "body": "both web nodes lost the database"})
	if out["sent"] != true || out["event_id"] == "" || len(out["closed"].([]any)) != 2 {
		t.Fatalf("release: %v", out)
	}
	ev := f.do(http.MethodGet, "/v1/events/"+out["event_id"].(string), readTok, "")
	if ev["source"] != "triage" || ev["title"] != "web tier down" {
		t.Fatalf("released message: %v", ev)
	}
	out, text = s.ok("hold_act", map[string]any{"action": "suppress", "ids": []string{dbID, ids[0]}, "reason": "BLIP"})
	if len(out["closed"].([]any)) != 1 || len(out["unknown_or_closed"].([]any)) != 1 || !strings.Contains(text, "left alone") {
		t.Fatalf("suppress: %v %s", out, text)
	}
	s.fail("hold_act", map[string]any{"action": "suppress", "ids": []string{dbID}}, "nothing_open")
	out, _ = s.ok("hold_read", map[string]any{})
	if len(out["entries"].([]any)) != 0 {
		t.Fatalf("open after close: %v", out)
	}
	out, _ = s.ok("hold_read", map[string]any{"include_closed": true})
	if len(out["entries"].([]any)) != 3 {
		t.Fatalf("include_closed: %v", out)
	}
}

func TestOverviewProjectsAndDevices(t *testing.T) {
	f := newFixture(t)
	f.post(`{"source":"s","kind":"message","title":"a","project":"web"}`)
	f.post(`{"source":"s","kind":"message","title":"b"}`)
	f.do(http.MethodPost, "/v1/devices", readTok,
		`{"token":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","environment":"production","topic":"org.example.heraldo","name":"Test Phone"}`)
	s := f.connect(false)
	out, _ := s.ok("overview", map[string]any{})
	if out["projects"] != nil || out["devices"] != nil {
		t.Fatalf("projects or devices by default: %v", out)
	}
	out, text := s.ok("overview", map[string]any{"sections": []string{"projects", "devices"}})
	p := out["projects"].(map[string]any)
	if p["total"].(float64) != 1 || p["unfiled"].(float64) != 1 || p["items"].([]any)[0].(map[string]any)["name"] != "web" {
		t.Fatalf("projects: %v", p)
	}
	d := out["devices"].(map[string]any)
	item := d["items"].([]any)[0].(map[string]any)
	if d["active"].(float64) != 1 || item["name"] != "Test Phone" || item["token_suffix"] != "89abcdef" {
		t.Fatalf("devices: %v", d)
	}
	if !strings.Contains(text, "devices: 1, 1 active") || !strings.Contains(text, "web: 1 events") {
		t.Fatalf("text: %s", text)
	}
}

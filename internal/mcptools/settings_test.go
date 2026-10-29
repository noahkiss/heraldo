package mcptools

import (
	"slices"
	"strings"
	"testing"
)

func TestSettingsTools(t *testing.T) {
	f := newFixture(t)
	if slices.Contains(f.connect(true).names(), "settings_write") {
		t.Fatal("settings_write listed without an admin token")
	}
	c := f.client(readTok, writeTok)
	c.AdminToken = adminTok
	s := connect(t, Options{API: c, Write: true, Admin: true, Host: "box"}, "claude-code")
	if a := s.tools["settings_write"].Annotations; a == nil || a.ReadOnlyHint {
		t.Fatalf("settings_write annotations %+v", a)
	}

	out, text := s.ok("settings_read", map[string]any{"prefix": "quiet."})
	list := out["settings"].([]any)
	if len(list) != 5 || !strings.Contains(text, "quiet.enabled = false [default]") {
		t.Fatalf("read prefix: %d %s", len(list), text)
	}
	out, _ = s.ok("settings_read", map[string]any{"key": "llm.token"})
	tok := out["settings"].([]any)[0].(map[string]any)
	if tok["set"] != false || tok["value"] != nil {
		t.Fatalf("secret read %v", tok)
	}
	s.fail("settings_read", map[string]any{"key": "no.such"}, "not_found")

	out, text = s.ok("settings_write", map[string]any{"key": "quiet.enabled", "value": true})
	if st := out["setting"].(map[string]any); st["value"] != true || st["source"] != "db" || !strings.HasPrefix(text, "set: ") {
		t.Fatalf("write %v %s", out, text)
	}
	s.ok("settings_write", map[string]any{"key": "stale.after", "value": "48h"})
	s.fail("settings_write", map[string]any{"key": "quiet.start", "value": "7pm"}, "invalid")
	s.fail("settings_write", map[string]any{"key": "quiet.start"}, "invalid")
	s.fail("settings_write", map[string]any{"key": "llm.token", "value": "sk-x"}, "secret")
	s.fail("settings_write", map[string]any{"key": "no.such", "value": 1}, "not_found")
	out, _ = s.ok("settings_write", map[string]any{"key": "quiet.enabled", "unset": true})
	if st := out["setting"].(map[string]any); st["value"] != false || st["source"] != "default" {
		t.Fatalf("unset %v", out)
	}

	// A write token in the admin slot is refused by the server.
	c2 := f.client(readTok, writeTok)
	c2.AdminToken = writeTok
	s2 := connect(t, Options{API: c2, Write: true, Admin: true, Host: "box"}, "claude-code")
	if text := s2.fail("settings_write", map[string]any{"key": "quiet.enabled", "value": true}, "forbidden"); !strings.Contains(text, "HERALDO_ADMIN_TOKEN") {
		t.Fatalf("forbidden text %s", text)
	}
}

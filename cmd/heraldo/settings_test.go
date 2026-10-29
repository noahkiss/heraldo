package main

import (
	"strings"
	"testing"
)

func TestSettingsCLI(t *testing.T) {
	f := newFixture(t)
	r := f.run("settings")
	if r.code != 0 || !strings.Contains(r.stdout, "quiet.enabled") || !strings.Contains(r.stdout, "(not set)") {
		t.Fatalf("ls: %+v", r)
	}
	if r := f.run("settings", "get", "stale.after"); r.code != 0 || r.stdout != "72h\n" || !strings.Contains(r.stderr, "source default") {
		t.Fatalf("get: %+v", r)
	}
	// Writes need an admin token.
	f.env["HERALDO_WRITE_TOKEN"] = writeTok
	if r := f.run("settings", "set", "quiet.enabled", "true"); r.code != 1 || !strings.Contains(r.stderr, "HERALDO_ADMIN_TOKEN") {
		t.Fatalf("set with write token: %+v", r)
	}
	f.env["HERALDO_ADMIN_TOKEN"] = adminTok
	for _, c := range []struct{ key, text, want string }{
		{"quiet.enabled", "true", "true"},
		{"flap.opens", "5", "5"},
		{"stale.after", "48h", "48h"},
		{"llm.exclude_projects", "home, work", `["home","work"]`},
		{"llm.redact", `["secret-[0-9]+"]`, `["secret-[0-9]+"]`},
	} {
		if r := f.run("settings", "set", c.key, c.text); r.code != 0 || r.stdout != c.want+"\n" || !strings.Contains(r.stderr, "source db") {
			t.Errorf("set %s %s: %+v", c.key, c.text, r)
		}
	}
	if r := f.run("settings", "set", "flap.opens", "many"); r.code != 1 || !strings.Contains(r.stderr, "whole number") {
		t.Fatalf("bad value: %+v", r)
	}
	if r := f.run("settings", "set", "quiet.start", "7pm"); r.code != 1 || !strings.Contains(r.stderr, "HH:MM") {
		t.Fatalf("server refusal: %+v", r)
	}
	if r := f.run("settings", "set", "no.such", "1"); r.code != 1 || !strings.Contains(r.stderr, "settings ls") {
		t.Fatalf("unknown key: %+v", r)
	}
	if r := f.run("settings", "unset", "quiet.enabled"); r.code != 0 || r.stdout != "false\n" {
		t.Fatalf("unset: %+v", r)
	}
	// A secret never comes from argv.
	if r := f.run("settings", "set", "llm.token", "sk-in-argv"); r.code != 1 || !strings.Contains(r.stderr, "stdin") {
		t.Fatalf("secret in argv: %+v", r)
	}
	// This test server has no secret key file: the server refuses.
	if r := f.runIn("sk-from-stdin\n", "settings", "set", "llm.token", "-"); r.code != 1 ||
		!strings.Contains(r.stderr, "HERALDO_SECRET_KEY_FILE") || strings.Contains(r.stderr, "sk-from-stdin") {
		t.Fatalf("secret without key: %+v", r)
	}
	if r := f.run("settings", "get", "--json", "llm.token"); r.code != 0 || !strings.Contains(r.stdout, `"set":false`) {
		t.Fatalf("secret get: %+v", r)
	}
}

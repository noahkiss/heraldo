package settings

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func newM(t *testing.T, o Options) *Manager {
	t.Helper()
	o.Logger = quiet
	m, err := New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAliases(t *testing.T) {
	k, _ := Lookup("llm.base_url")
	if k.Env() != "HERALDO_LLM_BASE_URL" || k.Flag() != "llm-base-url" {
		t.Fatalf("aliases %s %s", k.Env(), k.Flag())
	}
	tok, _ := Lookup("llm.token")
	if tok.Flag() != "" || tok.EnvFile() != "HERALDO_LLM_TOKEN_FILE" {
		t.Fatalf("secret aliases %q %q", tok.Flag(), tok.EnvFile())
	}
	// Every key's default is off or empty for the features.
	for _, name := range []string{"quiet.enabled", "digest.enabled", "stale.enabled", "flap.enabled", "llm.chat", "llm.hold_may_suppress"} {
		if From[bool](Default(), name) {
			t.Errorf("%s defaults on", name)
		}
	}
	if Get[string]("llm.mode") != "off" || Get[int]("llm.send_body_chars") != 0 ||
		Get[time.Duration]("stale.after") != 72*time.Hour || Get[string]("quiet.floor") != "time-sensitive" ||
		Get[string]("digest.at") != "07:30" {
		t.Fatal("defaults")
	}
}

func TestPrecedence(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	// A stored row for each of three keys.
	m := newM(t, Options{Backend: st})
	for k, v := range map[string]string{"quiet.start": `"21:00"`, "quiet.end": `"06:00"`, "quiet.tz": `"Europe/Paris"`} {
		if _, err := m.Set(ctx, k, json.RawMessage(v), "ops"); err != nil {
			t.Fatal(k, err)
		}
	}
	m = newM(t, Options{Backend: st,
		Getenv: envOf(map[string]string{"HERALDO_QUIET_START": "23:00", "HERALDO_QUIET_END": "05:00"}),
		Flags:  map[string]string{"quiet.start": "22:30"}})
	for _, c := range []struct {
		key, want string
		src       Source
		by        string
	}{
		{"quiet.start", "22:30", FromFlag, "--quiet-start"},
		{"quiet.end", "05:00", FromEnv, "HERALDO_QUIET_END"},
		{"quiet.tz", "Europe/Paris", FromDB, ""},
		{"digest.at", "07:30", FromDefault, ""},
	} {
		v, _ := m.View(c.key)
		if v.Value != c.want || v.Source != c.src || v.LockedBy != c.by {
			t.Errorf("%s: %+v", c.key, v)
		}
	}
	var le *LockedError
	if _, err := m.Set(ctx, "quiet.end", json.RawMessage(`"04:00"`), "ops"); !errors.As(err, &le) || le.By != "HERALDO_QUIET_END" {
		t.Fatalf("locked write: %v", err)
	}
	if _, err := m.Unset(ctx, "quiet.start", "ops"); !errors.As(err, &le) || le.By != "--quiet-start" {
		t.Fatalf("locked unset: %v", err)
	}
	if v, err := m.Unset(ctx, "quiet.tz", "ops"); err != nil || v.Source != FromDefault || v.Value != "UTC" {
		t.Fatalf("unset: %+v %v", v, err)
	}
}

func TestRefusals(t *testing.T) {
	ctx := context.Background()
	m := newM(t, Options{Backend: openStore(t)})
	if _, err := m.Set(ctx, "nope.key", json.RawMessage(`1`), "ops"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("unknown: %v", err)
	}
	for key, raw := range map[string]string{
		"quiet.enabled":        `"yes"`,
		"quiet.start":          `"25:00"`,
		"quiet.tz":             `"Mars/Base"`,
		"quiet.floor":          `"loud"`,
		"stale.after":          `"-1h"`,
		"flap.opens":           `-1`,
		"llm.base_url":         `"ftp://x"`,
		"llm.redact":           `["("]`,
		"llm.exclude_projects": `"a"`,
	} {
		var ie *InvalidError
		if _, err := m.Set(ctx, key, json.RawMessage(raw), "ops"); !errors.As(err, &ie) {
			t.Errorf("%s %s: %v", key, raw, err)
		}
	}
	if _, err := New(ctx, Options{Getenv: envOf(map[string]string{"HERALDO_FLAP_OPENS": "many"})}); err == nil ||
		!strings.Contains(err.Error(), "HERALDO_FLAP_OPENS") {
		t.Fatalf("bad env: %v", err)
	}
	if _, err := New(ctx, Options{Flags: map[string]string{"llm.token": "x"}}); err == nil {
		t.Fatal("secret flag accepted")
	}
	if err := RefuseSecretFlags([]string{"--quiet-start", "1", "--llm-token=abc"}); err == nil ||
		strings.Contains(err.Error(), "abc") {
		t.Fatalf("refuse secret flag: %v", err)
	}
	if err := RefuseSecretFlags([]string{"-llm-base-url", "https://x"}); err != nil {
		t.Fatal(err)
	}
}

func TestFlags(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	got := Flags(fs)
	if fs.Lookup("llm-token") != nil {
		t.Fatal("secret flag registered")
	}
	if err := fs.Parse([]string{"--llm-mode", "shadow", "--digest-enabled=true"}); err != nil {
		t.Fatal(err)
	}
	f := got()
	if len(f) != 2 || f["llm.mode"] != "shadow" || f["digest.enabled"] != "true" {
		t.Fatalf("flags %v", f)
	}
	m := newM(t, Options{Flags: f})
	if From[string](m, "llm.mode") != "shadow" || !From[bool](m, "digest.enabled") {
		t.Fatal("flag values")
	}
}

func writeKey(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret.key")
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const hexKey = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f\n"

func TestSecret(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	// No key file: the write is refused.
	m := newM(t, Options{Backend: st})
	if _, err := m.Set(ctx, "llm.token", json.RawMessage(`"sk-test-123"`), "ops"); !errors.Is(err, ErrNoSecretKey) {
		t.Fatalf("no key: %v", err)
	}
	key, err := LoadSecretKey(writeKey(t, hexKey))
	if err != nil {
		t.Fatal(err)
	}
	m = newM(t, Options{Backend: st, SecretKey: key})
	v, err := m.Set(ctx, "llm.token", json.RawMessage(`"sk-test-123"`), "ops")
	if err != nil || !v.Set || v.Value != nil || v.Source != FromDB {
		t.Fatalf("set: %+v %v", v, err)
	}
	// At rest: no plain text.
	rows, _ := st.Settings(ctx)
	if len(rows) != 1 || strings.Contains(string(rows[0].Value), "sk-test") || !strings.Contains(string(rows[0].Value), `"enc"`) {
		t.Fatalf("stored %s", rows[0].Value)
	}
	// A restart with the key reads it back for the server's own use.
	m = newM(t, Options{Backend: st, SecretKey: key})
	if From[string](m, "llm.token") != "sk-test-123" {
		t.Fatal("round trip")
	}
	// Without the key, or with another, it is set but unusable.
	other, _ := LoadSecretKey(writeKey(t, strings.Repeat("ab", 32)))
	for _, k := range [][]byte{nil, other} {
		m = newM(t, Options{Backend: st, SecretKey: k})
		v, _ := m.View("llm.token")
		if !v.Set || v.Error == "" || From[string](m, "llm.token") != "" {
			t.Fatalf("unusable: %+v", v)
		}
	}
	// The env var wins and locks it; the file form too.
	m = newM(t, Options{Backend: st, SecretKey: key, Getenv: envOf(map[string]string{"HERALDO_LLM_TOKEN": "from-env"})})
	if v, _ := m.View("llm.token"); v.Source != FromEnv || v.LockedBy != "HERALDO_LLM_TOKEN" || From[string](m, "llm.token") != "from-env" {
		t.Fatalf("env: %+v", v)
	}
	f := writeKey(t, "from-file\n")
	m = newM(t, Options{Getenv: envOf(map[string]string{"HERALDO_LLM_TOKEN_FILE": f})})
	if v, _ := m.View("llm.token"); v.LockedBy != "HERALDO_LLM_TOKEN_FILE" || From[string](m, "llm.token") != "from-file" {
		t.Fatalf("env file: %+v", v)
	}
	if _, err := New(ctx, Options{Getenv: envOf(map[string]string{"HERALDO_LLM_TOKEN_FILE": f, "HERALDO_LLM_TOKEN": "x"})}); err == nil {
		t.Fatal("both forms accepted")
	}
	if _, err := LoadSecretKey(writeKey(t, "short")); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestSubscribe(t *testing.T) {
	ctx := context.Background()
	m := newM(t, Options{})
	ch, cancel := m.Subscribe("quiet.")
	all, cancelAll := m.Subscribe()
	defer cancelAll()
	if _, err := m.Set(ctx, "digest.enabled", json.RawMessage(`true`), "ops"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Set(ctx, "quiet.enabled", json.RawMessage(`true`), "ops"); err != nil {
		t.Fatal(err)
	}
	if got := <-ch; got != "quiet.enabled" {
		t.Fatalf("quiet sub got %s", got)
	}
	if a, b := <-all, <-all; a != "digest.enabled" || b != "quiet.enabled" {
		t.Fatalf("all sub got %s %s", a, b)
	}
	if !From[bool](m, "quiet.enabled") {
		t.Fatal("value after set")
	}
	cancel()
	m.Unset(ctx, "quiet.enabled", "ops")
	select {
	case n := <-ch:
		t.Fatalf("cancelled sub got %s", n)
	default:
	}
}

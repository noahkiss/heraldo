package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/settings"
)

// withSettings replaces the env's handler with one whose settings come from
// m.
func (e *env) withSettings(t *testing.T, o settings.Options) *settings.Manager {
	t.Helper()
	o.Backend = e.st
	o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := settings.New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	old := e.h.(*Server).s.Options
	old.Settings = m
	e.h = New(old)
	return m
}

func TestSettingsEndpoints(t *testing.T) {
	e := newEnv(t)
	m := e.withSettings(t, settings.Options{
		Getenv: func(k string) string {
			return map[string]string{"HERALDO_DIGEST_AT": "06:00"}[k]
		},
		Flags: map[string]string{"llm.mode": "shadow"},
	})
	list := decode[api.SettingList](t, e.do(t, http.MethodGet, "/v1/settings", readTok, nil))
	by := map[string]api.Setting{}
	for _, s := range list.Settings {
		by[s.Key] = s
	}
	if len(by) != len(settings.Keys()) {
		t.Fatalf("listed %d keys", len(by))
	}
	if s := by["llm.mode"]; s.Source != "flag" || s.Value != "shadow" || s.LockedBy == nil || *s.LockedBy != "--llm-mode" {
		t.Fatalf("flag key %+v", s)
	}
	if s := by["digest.at"]; s.Source != "env" || s.Value != "06:00" || *s.LockedBy != "HERALDO_DIGEST_AT" || s.Default != "07:30" {
		t.Fatalf("env key %+v", s)
	}
	if s := by["stale.after"]; s.Source != "default" || s.Value != "72h" || *s.Flag != "--stale-after" || s.Env != "HERALDO_STALE_AFTER" {
		t.Fatalf("default key %+v", s)
	}
	if s := by["llm.token"]; s.Value != nil || s.Set == nil || *s.Set || s.Flag != nil || !s.Secret {
		t.Fatalf("secret key %+v", s)
	}

	// Writes need admin.
	for _, tok := range []string{readTok, writeTok} {
		if rec := e.do(t, http.MethodPut, "/v1/settings/quiet.enabled", tok, []byte(`{"value":true}`)); rec.Code != http.StatusForbidden {
			t.Fatalf("%s put: %d", tok, rec.Code)
		}
		if rec := e.do(t, http.MethodDelete, "/v1/settings/quiet.enabled", tok, nil); rec.Code != http.StatusForbidden {
			t.Fatalf("%s delete: %d", tok, rec.Code)
		}
	}
	changed, cancel := m.Subscribe("quiet.")
	defer cancel()
	rec := e.do(t, http.MethodPut, "/v1/settings/quiet.enabled", adminTok, []byte(`{"value":true}`))
	s := decode[api.Setting](t, rec)
	if rec.Code != http.StatusOK || s.Value != true || s.Source != "db" || s.UpdatedBy == nil || *s.UpdatedBy != "ops" {
		t.Fatalf("put: %d %+v", rec.Code, s)
	}
	if got := <-changed; got != "quiet.enabled" || !settings.From[bool](m, "quiet.enabled") {
		t.Fatalf("notify %s", got)
	}
	if s := decode[api.Setting](t, e.do(t, http.MethodGet, "/v1/settings/quiet.enabled", readTok, nil)); s.Value != true {
		t.Fatalf("get after put %+v", s)
	}
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodPut, "/v1/settings/no.such", `{"value":1}`, http.StatusNotFound},
		{http.MethodGet, "/v1/settings/no.such", ``, http.StatusNotFound},
		{http.MethodDelete, "/v1/settings/no.such", ``, http.StatusNotFound},
		{http.MethodPut, "/v1/settings/quiet.start", `{"value":"7pm"}`, http.StatusBadRequest},
		{http.MethodPut, "/v1/settings/quiet.start", `{}`, http.StatusBadRequest},
		{http.MethodPut, "/v1/settings/quiet.start", `{"value":null}`, http.StatusBadRequest},
		{http.MethodPut, "/v1/settings/quiet.start", `{"value":"21:00","x":1}`, http.StatusBadRequest},
		{http.MethodPut, "/v1/settings/digest.at", `{"value":"05:00"}`, http.StatusConflict},
		{http.MethodDelete, "/v1/settings/llm.mode", ``, http.StatusConflict},
		{http.MethodPut, "/v1/settings/llm.token", `{"value":"sk-abc"}`, http.StatusConflict},
	} {
		var body []byte
		if c.body != "" {
			body = []byte(c.body)
		}
		if rec := e.do(t, c.method, c.path, adminTok, body); rec.Code != c.want {
			t.Errorf("%s %s %s: %d %s", c.method, c.path, c.body, rec.Code, rec.Body)
		}
	}
	rec = e.do(t, http.MethodPut, "/v1/settings/digest.at", adminTok, []byte(`{"value":"05:00"}`))
	se := decode[api.SettingError](t, rec)
	if se.Code != "locked_by_env" || se.Variable == nil || *se.Variable != "HERALDO_DIGEST_AT" {
		t.Fatalf("locked %+v", se)
	}
	if se := decode[api.SettingError](t, e.do(t, http.MethodPut, "/v1/settings/llm.token", adminTok, []byte(`{"value":"sk-abc"}`))); se.Code != "no_secret_key" {
		t.Fatalf("no key %+v", se)
	}
	rec = e.do(t, http.MethodDelete, "/v1/settings/quiet.enabled", adminTok, nil)
	if s := decode[api.Setting](t, rec); rec.Code != http.StatusOK || s.Value != false || s.Source != "default" {
		t.Fatalf("delete: %d %+v", rec.Code, s)
	}
}

func TestSettingsSecret(t *testing.T) {
	e := newEnv(t)
	p := filepath.Join(t.TempDir(), "secret.key")
	if err := os.WriteFile(p, []byte(strings.Repeat("7f", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := settings.LoadSecretKey(p)
	if err != nil {
		t.Fatal(err)
	}
	m := e.withSettings(t, settings.Options{SecretKey: key})
	const secret = "sk-test-secret-value"
	rec := e.do(t, http.MethodPut, "/v1/settings/llm.token", adminTok, []byte(fmt.Sprintf(`{"value":%q}`, secret)))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	for _, path := range []string{"/v1/settings", "/v1/settings/llm.token"} {
		rec := e.do(t, http.MethodGet, path, readTok, nil)
		if strings.Contains(rec.Body.String(), secret) || !strings.Contains(rec.Body.String(), `"set":true`) {
			t.Fatalf("%s: %s", path, rec.Body)
		}
	}
	if settings.From[string](m, "llm.token") != secret {
		t.Fatal("the server cannot read its own secret")
	}
	if rec := e.do(t, http.MethodPut, "/v1/settings/llm.token", adminTok, []byte(`{"value":42}`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("number secret: %d", rec.Code)
	}
	rec = e.do(t, http.MethodDelete, "/v1/settings/llm.token", adminTok, nil)
	if s := decode[api.Setting](t, rec); *s.Set {
		t.Fatalf("unset %+v", s)
	}
}

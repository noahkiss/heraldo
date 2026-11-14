package server

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/noahkiss/heraldo/internal/rules"
)

// A Notify! notification is untyped, so the ingest rules can make it an
// incident; one no rule matches stays a message.
func TestCompatNotifyRunsRules(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.yaml")
	const yaml = `
rules:
  - name: printer-fault
    match: {title: '^X2D$', body: '(?i)overloaded'}
    set: {kind: incident, action: open, key: 'printer-fault', level: critical, realert: 5m}
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	rw, err := rules.NewWatcher(path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	e := newEnvRules(t, rw)

	post := func(body string) map[string]any {
		t.Helper()
		rec := e.do(t, http.MethodPost, "/notify-json/dev-1?token="+writeTok, "", []byte(body))
		if rec.Code != http.StatusOK {
			t.Fatalf("notify-json: %d %s", rec.Code, rec.Body)
		}
		id := jsonBody(t, rec)["id"].(string)
		return jsonBody(t, e.do(t, http.MethodGet, "/v1/events/"+id, readTok, nil))
	}

	ev := post(`{"title":"X2D","text":"The AMS B Slot 1 is overloaded","timeSensitive":true}`)
	if ev["kind"] != "incident" || ev["key"] != "printer-fault" || ev["level"] != "critical" {
		t.Fatalf("fault = %v", ev)
	}
	ev = post(`{"title":"X2D","text":"Done"}`)
	if ev["kind"] != "message" {
		t.Fatalf("other = %v", ev)
	}
}

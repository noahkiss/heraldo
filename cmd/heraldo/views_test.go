package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestViewsCLI(t *testing.T) {
	f := newFixture(t)
	f.post(writeTok, `{"source":"backup","title":"nas failed","project":"home"}`)
	f.post(writeTok, `{"source":"backup","title":"pi failed","project":"lab"}`)
	f.post(writeTok, `{"source":"web","title":"hello","project":"home"}`)

	if r := f.run("views"); r.code != 0 || !strings.Contains(r.stderr, "no saved views") {
		t.Fatalf("empty: %+v", r)
	}
	if r := f.run("views", "save", "--source", "backup", "Backups"); r.code != 1 || !strings.Contains(r.stderr, "HERALDO_WRITE_TOKEN is not set") {
		t.Fatalf("without a write token: %+v", r)
	}
	f.env["HERALDO_WRITE_TOKEN"] = writeTok
	r := f.run("views", "save", "--source", "backup", "--bundle", "15m", "Backups")
	if r.code != 0 || !strings.Contains(r.stdout, "saved view Backups: source=backup routine=exclude; bundles 15m") {
		t.Fatalf("save: %+v", r)
	}
	if r := f.run("views", "ls"); r.code != 0 || !strings.Contains(r.stdout, "Backups") || !strings.Contains(r.stdout, "15m") {
		t.Fatalf("ls: %+v", r)
	}
	if r := f.run("ls", "--view", "backups"); r.code != 0 || !strings.Contains(r.stdout, "nas failed") ||
		!strings.Contains(r.stdout, "pi failed") || strings.Contains(r.stdout, "hello") {
		t.Fatalf("ls --view: %+v", r)
	}
	if r := f.run("ls", "--view", "Backups", "--project", "lab"); r.code != 0 || strings.Contains(r.stdout, "nas failed") ||
		!strings.Contains(r.stdout, "pi failed") {
		t.Fatalf("ls --view --project: %+v", r)
	}
	// Saving the same name replaces the view.
	if r := f.run("views", "save", "--source", "backup", "--project", "home", "backups"); r.code != 0 || !strings.Contains(r.stdout, "replaced view") {
		t.Fatalf("replace: %+v", r)
	}
	if r := f.run("ls", "--view", "Backups"); r.code != 0 || strings.Contains(r.stdout, "pi failed") {
		t.Fatalf("ls after replace: %+v", r)
	}
	if r := f.run("ls", "--view", "nope"); r.code != 1 || !strings.Contains(r.stderr, `no view "nope"`) {
		t.Fatalf("unknown view: %+v", r)
	}
	if r := f.run("views", "rm", "Backups"); r.code != 0 || !strings.Contains(r.stdout, "removed view Backups") {
		t.Fatalf("rm: %+v", r)
	}
	if r := f.run("views", "rm", "Backups"); r.code != 1 || !strings.Contains(r.stderr, "no view") {
		t.Fatalf("rm again: %+v", r)
	}
}

func TestViewOldServer(t *testing.T) {
	f := newFixture(t)
	f.post(writeTok, `{"source":"s","title":"one"}`)
	real := f.srv.Config.Handler
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		q.Del("view")
		r.URL.RawQuery = q.Encode()
		real.ServeHTTP(w, r)
	}))
	t.Cleanup(old.Close)
	f.env["HERALDO_URL"] = old.URL
	// The old server ignores view= and sends no Heraldo-View header.
	if r := f.run("ls", "--view", "x"); r.code != 1 || !strings.Contains(r.stderr, "no saved views") {
		t.Fatalf("old server: %+v", r)
	}
}

func TestViewsMCP(t *testing.T) {
	f := newFixture(t)
	f.env["HERALDO_WRITE_TOKEN"] = writeTok
	f.post(writeTok, `{"source":"backup","title":"nas failed"}`)
	f.post(writeTok, `{"source":"web","title":"hello"}`)
	callJSON := func(id int, name, args string) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, id, name, args)
	}
	// One call per run: the server may answer calls in any order.
	calls := map[int][2]string{
		3: {"views_write", `{"operation":"save","name":"Backups","filter":{"source":"backup"},"bundle":{"window":"15m"}}`},
		4: {"views_read", `{}`},
		5: {"events_search", `{"view":"backups"}`},
		6: {"events_search", `{"view":"nope"}`},
		7: {"views_write", `{"operation":"delete","name":"Backups"}`},
	}
	lines := map[int]map[string]any{}
	for id := 3; id <= 7; id++ {
		got, _, _ := f.mcpRun(t, []string{mcpInit, mcpInited, callJSON(id, calls[id][0], calls[id][1])})
		lines[id] = got[id]
	}
	text := func(id int) (string, bool) {
		res := lines[id]["result"].(map[string]any)
		c := res["content"].([]any)[0].(map[string]any)["text"].(string)
		return c, res["isError"] == true
	}
	if s, bad := text(3); bad || !strings.Contains(s, "saved view Backups") || !strings.Contains(s, "15m") {
		t.Fatalf("save: %q", s)
	}
	if s, bad := text(4); bad || !strings.Contains(s, "source=backup") {
		t.Fatalf("read: %q", s)
	}
	if s, bad := text(5); bad || !strings.Contains(s, "nas failed") || strings.Contains(s, "hello") {
		t.Fatalf("search: %q", s)
	}
	if s, bad := text(6); !bad || !strings.Contains(s, "no view nope") {
		t.Fatalf("unknown view: %q", s)
	}
	if s, bad := text(7); bad || s != "deleted view Backups" {
		t.Fatalf("delete: %q", s)
	}
}

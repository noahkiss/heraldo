package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeliveryCLI(t *testing.T) {
	f := newFixture(t)
	if r := f.run("delivery"); r.code != 0 || !strings.Contains(r.stdout, "delivery settings off") ||
		!strings.Contains(r.stderr, "no delivery settings") {
		t.Fatalf("empty ls: %+v", r)
	}
	f.env["HERALDO_WRITE_TOKEN"] = writeTok
	if r := f.run("delivery", "add", "--source", "printer", "--deliver", "silent"); r.code != 1 || !strings.Contains(r.stderr, "HERALDO_ADMIN_TOKEN") {
		t.Fatalf("add with write token: %+v", r)
	}
	f.env["HERALDO_ADMIN_TOKEN"] = adminTok
	r := f.run("delivery", "add", "--source", "printer", "--level", "active,passive", "--deliver", "silent", "--note", "prints")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "dlv_") {
		t.Fatalf("add: %+v", r)
	}
	id := strings.TrimSpace(r.stdout)
	if r := f.run("delivery", "add", "--deliver", "digest", "--badge", "on"); r.code != 1 || !strings.Contains(r.stderr, "hides") {
		t.Fatalf("bad add: %+v", r)
	}
	if r := f.run("delivery", "edit", id, "--deliver", "drop", "--note", ""); r.code != 0 {
		t.Fatalf("edit: %+v", r)
	}
	r = f.run("delivery", "ls")
	if r.code != 0 || !strings.Contains(r.stdout, "drop") || !strings.Contains(r.stdout, "source=printer level=active,passive") ||
		strings.Contains(r.stdout, "# prints") {
		t.Fatalf("ls: %+v", r)
	}
	f.run("send", "--source", "printer", "--title", "done")
	rows := filepath.Join(t.TempDir(), "rows.json")
	if err := os.WriteFile(rows, []byte(`[{"match":{"source":"printer"},"deliver":"silent"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := f.run("delivery", "preview", "--file", rows); r.code != 0 || !strings.Contains(r.stdout, "row 0:") {
		t.Fatalf("preview: %+v", r)
	}
	if r := f.run("sources"); r.code != 0 || !strings.Contains(r.stdout, "SOURCE") {
		t.Fatalf("sources: %+v", r)
	}
	if r := f.run("ls", "--hidden", "only"); r.code != 0 {
		t.Fatalf("ls --hidden: %+v", r)
	}
	if r := f.run("ls", "--hidden", "maybe"); r.code == 0 {
		t.Fatalf("ls --hidden maybe: %+v", r)
	}
	if r := f.run("delivery", "rm", id); r.code != 0 {
		t.Fatalf("rm: %+v", r)
	}
	if r := f.run("delivery", "rm", id); r.code != 1 || !strings.Contains(r.stderr, "404") {
		t.Fatalf("rm again: %+v", r)
	}
}

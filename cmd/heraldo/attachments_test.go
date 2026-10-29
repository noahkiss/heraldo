package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestSendAttachShowAndGet(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	logf := filepath.Join(dir, "error.txt")
	if err := os.WriteFile(logf, []byte("disk full on /data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	png := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(png, append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...), 0o644); err != nil {
		t.Fatal(err)
	}
	// --attach needs the heraldo API's write token.
	if r := f.run("send", "-t", "look", "-m", "see files", "--attach", logf); r.code != 1 || !strings.Contains(r.stderr, "HERALDO_WRITE_TOKEN") {
		t.Fatalf("without a write token: %+v", r)
	}
	f.env["HERALDO_WRITE_TOKEN"] = writeTok
	if r := f.run("send", "-t", "look", "-m", "see files", "--project", "home", "--attach", logf, "--attach", png); r.code != 0 || !strings.Contains(r.stdout, sentLine) {
		t.Fatalf("send: %+v", r)
	}
	r := f.run("ls", "--json")
	id := regexp.MustCompile(`"id":"([0-9A-Z]{26})"`).FindStringSubmatch(r.stdout)
	if id == nil {
		t.Fatalf("ls: %+v", r)
	}
	r = f.run("show", id[1])
	rows := regexp.MustCompile(`attached:\s+(att_[0-9A-Z]{26}) \(([^)]*)\)`).FindAllStringSubmatch(r.stdout, -1)
	if r.code != 0 || len(rows) != 2 || rows[0][2] != "text/plain, 19 B" || !strings.HasPrefix(rows[1][2], "image/png") {
		t.Fatalf("show: %+v", r)
	}
	out := filepath.Join(dir, "got.txt")
	if r := f.run("attachment", "get", "-o", out, rows[0][1]); r.code != 0 {
		t.Fatalf("get: %+v", r)
	}
	if b, _ := os.ReadFile(out); string(b) != "disk full on /data\n" {
		t.Fatalf("got %q", b)
	}
	// -o never overwrites.
	if r := f.run("attachment", "get", "-o", out, rows[0][1]); r.code != 1 {
		t.Fatalf("overwrite: %+v", r)
	}
	if r := f.run("attachment", "get", rows[0][1]); r.stdout != "disk full on /data\n" {
		t.Fatalf("stdout: %+v", r)
	}
	if r := f.run("attachment", "get", "att_01JBA6Z0000000000000000099"); r.code != 1 || !strings.Contains(r.stderr, "404") {
		t.Fatalf("unknown: %+v", r)
	}
}

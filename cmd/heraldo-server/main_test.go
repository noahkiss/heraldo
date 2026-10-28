package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noahkiss/heraldo/internal/auth"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestHashToken(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"hash-token"}, strings.NewReader("secret\n"), &out, quiet); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != auth.Hash("secret") {
		t.Fatalf("got %q", out.String())
	}
	if err := run([]string{"hash-token"}, strings.NewReader(""), &out, quiet); err == nil {
		t.Fatal("want an error for empty stdin")
	}
}

func TestImportAndBackup(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERALDO_DB", filepath.Join(dir, "h.db"))
	if err := run([]string{"import-tee", "../../internal/teeimport/testdata/catalogue"}, nil, io.Discard, quiet); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"import-tee", "-since", "2026-09", "../../internal/teeimport/testdata/catalogue"}, nil, io.Discard, quiet); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"import-tee", "-since", "2026-10", "../../internal/teeimport/testdata/catalogue"}, nil, io.Discard, quiet); err == nil {
		t.Fatal("import-tee -since past every file succeeded")
	}
	dst := filepath.Join(dir, "b.db")
	if err := run([]string{"backup", dst}, nil, io.Discard, quiet); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"backup", dst}, nil, io.Discard, quiet); err == nil {
		t.Fatal("backup overwrote an existing file")
	}
}

func TestBackupNeedsDatabase(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERALDO_DB", filepath.Join(dir, "missing.db"))
	if err := run([]string{"backup", filepath.Join(dir, "b.db")}, nil, io.Discard, quiet); err == nil {
		t.Fatal("backup of a missing database succeeded")
	}
}

func TestUnknownCommand(t *testing.T) {
	if err := run([]string{"nope"}, nil, io.Discard, quiet); err == nil {
		t.Fatal("want an error")
	}
}

func TestHealthcheck(t *testing.T) {
	status := http.StatusOK
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
	}))
	defer ts.Close()
	t.Setenv("HERALDO_ADDR", strings.TrimPrefix(ts.URL, "http://"))
	if err := run([]string{"healthcheck"}, nil, io.Discard, quiet); err != nil {
		t.Fatal(err)
	}
	status = http.StatusServiceUnavailable
	if err := run([]string{"healthcheck"}, nil, io.Discard, quiet); err == nil {
		t.Fatal("want an error on 503")
	}
}

func TestReplayTee(t *testing.T) {
	t.Setenv("HERALDO_DB", filepath.Join(t.TempDir(), "never-opened.db"))
	var out bytes.Buffer
	args := []string{"replay-tee", "-rules", "../../internal/rules/testdata/rules.yaml", "../../internal/replay/testdata/catalogue"}
	if err := run(args, nil, &out, quiet); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "recoveries paired: 1 / 3") {
		t.Fatalf("report:\n%s", out.String())
	}
	if _, err := os.Stat(os.Getenv("HERALDO_DB")); !os.IsNotExist(err) {
		t.Fatal("replay-tee created a database")
	}
	t.Setenv("HERALDO_RULES", "../../internal/rules/testdata/rules.yaml")
	out.Reset()
	if err := run([]string{"replay-tee", "../../internal/replay/testdata/catalogue"}, nil, &out, quiet); err != nil ||
		!strings.Contains(out.String(), "rule hits:") {
		t.Fatalf("rules from HERALDO_RULES: %v\n%s", err, out.String())
	}
	if err := run([]string{"replay-tee"}, nil, io.Discard, quiet); err == nil {
		t.Fatal("no directory: want a usage error")
	}
	if err := run([]string{"replay-tee", "-rules", "missing.yaml", "x"}, nil, io.Discard, quiet); err == nil {
		t.Fatal("missing rules file: want an error")
	}
}

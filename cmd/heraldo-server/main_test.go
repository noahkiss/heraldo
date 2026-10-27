package main

import (
	"bytes"
	"io"
	"log/slog"
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

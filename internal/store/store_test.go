package store

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/api"
)

func open(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMigrateFromEmpty(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "h.db")
	s := open(t, path)
	// schemaVersion is the newest migration's number. Bump it with each new
	// migration, so a merge that drops or renumbers one fails here.
	const schemaVersion = 15
	if newest := embeddedVersion(t); newest != schemaVersion {
		t.Fatalf("newest migration is %d, want %d", newest, schemaVersion)
	}
	v, err := s.Version(ctx)
	if err != nil || v != schemaVersion {
		t.Fatalf("version = %d, %v; want %d", v, err, schemaVersion)
	}
	for _, table := range []string{"events", "events_fts", "incidents", "checkins", "activities", "status", "imports", "cursors", "devices", "deliveries", "mutes", "icons", "hold_entries", "hold_meta", "standby_state", "push_decisions", "attachments"} {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
	}
	var mode string
	if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v", mode, err)
	}
	s.Close()
	// A second open applies nothing and keeps the version.
	s = open(t, path)
	defer s.Close()
	if v, _ := s.Version(ctx); v != schemaVersion {
		t.Fatalf("version after reopen = %d", v)
	}
}

func TestRefusesNewerSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "h.db")
	s := open(t, path)
	if _, err := s.db.ExecContext(ctx, `PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(ctx, path); err == nil {
		t.Fatal("opened a database from a newer binary")
	}
}

func event(id string, at time.Time) NewEvent {
	k := api.KindMessage
	return NewEvent{
		ID: id, ReceivedAt: at,
		Fields: map[string]any{"id": id, "source": "s", "kind": "message"},
		Typed:  api.EventInput{Source: "s", Kind: &k},
	}
}

// The server types every input first; an untyped event never reaches the
// table.
func TestInsertNeedsKind(t *testing.T) {
	st := open(t, filepath.Join(t.TempDir(), "h.db"))
	defer st.Close()
	ev := event("01K3ZX0000AAAAAAAAAAAAAAAB", time.Now())
	ev.Typed.Kind = nil
	if _, _, err := st.Insert(context.Background(), ev); err == nil {
		t.Fatal("stored an event without a kind")
	}
}

func TestImportIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "h.db"))
	defer s.Close()
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i, want := range []bool{true, false} {
		got, err := s.Import(ctx, "tee", "n-1", event("01K3ZX0000AAAAAAAAAAAAAAAA", at))
		if err != nil || got != want {
			t.Fatalf("run %d: created = %v, %v; want %v", i, got, err, want)
		}
	}
	if n, _ := s.Count(ctx); n != 1 {
		t.Fatalf("count = %d", n)
	}
}

func TestBackup(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, filepath.Join(dir, "h.db"))
	defer s.Close()
	for i, id := range []string{"01K3ZX0000AAAAAAAAAAAAAAA1", "01K3ZX0000AAAAAAAAAAAAAAA2"} {
		if _, _, err := s.Insert(ctx, event(id, time.Unix(int64(i), 0))); err != nil {
			t.Fatal(err)
		}
	}
	dst := filepath.Join(dir, "backup.db")
	if err := s.Backup(ctx, dst); err != nil {
		t.Fatal(err)
	}
	for _, sfx := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(dst + sfx); !os.IsNotExist(err) {
			t.Fatalf("%s beside the backup: %v", sfx, err)
		}
	}
	hdr := make([]byte, 20)
	f, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadFull(f, hdr)
	f.Close()
	if err != nil || hdr[18] != 1 || hdr[19] != 1 {
		t.Fatalf("backup header read/write versions = %d/%d, %v; want rollback mode", hdr[18], hdr[19], err)
	}
	b := open(t, dst)
	defer b.Close()
	if n, err := b.Count(ctx); err != nil || n != 2 {
		t.Fatalf("backup count = %d, %v", n, err)
	}
	var ok string
	if err := b.db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&ok); err != nil || ok != "ok" {
		t.Fatalf("integrity_check = %q, %v", ok, err)
	}
}

func TestCursor(t *testing.T) {
	for _, n := range []int64{0, 1, 12345678} {
		got, err := ParseCursor(CursorFor(n))
		if err != nil || got != n {
			t.Fatalf("%d -> %d, %v", n, got, err)
		}
	}
	for _, bad := range []string{"", "12", "s", "s-1", "szz!"} {
		if _, err := ParseCursor(bad); err == nil {
			t.Fatalf("parsed %q", bad)
		}
	}
}

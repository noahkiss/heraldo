package store

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

// embeddedVersion is the number of the newest embedded migration.
func embeddedVersion(t *testing.T) int {
	t.Helper()
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	ms, err := loadMigrations(sub)
	if err != nil {
		t.Fatal(err)
	}
	return ms[len(ms)-1].version
}

func sqlFS(names ...string) fstest.MapFS {
	m := fstest.MapFS{}
	for _, n := range names {
		m[n] = &fstest.MapFile{Data: []byte("SELECT 1;")}
	}
	return m
}

func TestLoadMigrationsRules(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fsys  fstest.MapFS
		error string // empty: the set loads
	}{
		{"in turn", sqlFS("0001_a.sql", "0002_b.sql", "0003_c.sql"), ""},
		{"no padding needed", sqlFS("1_a.sql", "02_b.sql", "0003_c.sql"), ""},
		{"empty", sqlFS(), ""},
		{"gap", sqlFS("0001_a.sql", "0002_b.sql", "0004_d.sql"), "no migration has the number 3"},
		{"no first", sqlFS("0002_b.sql", "0003_c.sql"), "no migration has the number 1"},
		{"duplicate", sqlFS("0001_a.sql", "0002_b.sql", "0002_c.sql"), "share the number 2"},
		{"no number", sqlFS("0001_a.sql", "hold.sql"), "hold.sql"},
		{"no description", sqlFS("0001_a.sql", "0002.sql"), "0002.sql"},
		{"number zero", sqlFS("0000_a.sql", "0001_b.sql"), "1 or more"},
		{"number not first", sqlFS("0001_a.sql", "b_0002.sql"), "b_0002.sql"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadMigrations(tc.fsys)
			switch {
			case tc.error == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.error != "" && err == nil:
				t.Fatal("loaded")
			case tc.error != "" && !strings.Contains(err.Error(), tc.error):
				t.Fatalf("error %q, want it to name %q", err, tc.error)
			}
		})
	}
}

// The embedded set itself passes the rules.
func TestEmbeddedMigrationsLoad(t *testing.T) {
	if v := embeddedVersion(t); v < 1 {
		t.Fatalf("newest embedded migration %d", v)
	}
}

func rawStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &Store{db: db}
}

func chainFS(n int) fstest.MapFS {
	m := fstest.MapFS{}
	for i := 1; i <= n; i++ {
		body := fmt.Sprintf("CREATE TABLE t%d (x);", i)
		if i > 1 {
			// Fails unless migration i-1 ran first.
			body += fmt.Sprintf(" INSERT INTO t%d SELECT x FROM t%d;", i, i-1)
		}
		m[fmt.Sprintf("%d_t%d.sql", i, i)] = &fstest.MapFile{Data: []byte(body)}
	}
	return m
}

// Migrations run in number order, not name order: as names, 10_t10.sql sorts
// before 2_t2.sql.
func TestMigrateRunsInNumberOrder(t *testing.T) {
	ctx := context.Background()
	s := rawStore(t)
	if err := s.migrateFS(ctx, chainFS(10)); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Version(ctx); v != 10 {
		t.Fatalf("version %d, want 10", v)
	}
}

// A database at version 2 runs only the migrations above 2.
func TestMigrateAppliesOnlyNewer(t *testing.T) {
	ctx := context.Background()
	s := rawStore(t)
	if err := s.migrateFS(ctx, chainFS(2)); err != nil {
		t.Fatal(err)
	}
	// Re-running migration 1 or 2 would fail: the tables exist.
	if err := s.migrateFS(ctx, chainFS(4)); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Version(ctx); v != 4 {
		t.Fatalf("version %d, want 4", v)
	}
}

// A bad set refuses to start and touches nothing.
func TestMigrateRefusesBadSet(t *testing.T) {
	ctx := context.Background()
	s := rawStore(t)
	bad := chainFS(3)
	bad["5_t5.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE t5 (x);")}
	if err := s.migrateFS(ctx, bad); err == nil {
		t.Fatal("migrated a set with a gap")
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d schema objects, %v; want none", n, err)
	}
	if v, _ := s.Version(ctx); v != 0 {
		t.Fatalf("version %d, want 0", v)
	}
}

// A database a newer server ran on refuses to start.
func TestMigrateRefusesNewerDatabase(t *testing.T) {
	ctx := context.Background()
	s := rawStore(t)
	if err := s.migrateFS(ctx, chainFS(3)); err != nil {
		t.Fatal(err)
	}
	err := s.migrateFS(ctx, chainFS(2))
	if err == nil || !strings.Contains(err.Error(), "newer than this binary") {
		t.Fatalf("err = %v", err)
	}
}

// legacyMigrate is the runner heraldo used up to 0.5.0, kept to build test
// databases: it sorted the file names and recorded the count of files run.
// It stops after limit files.
func legacyMigrate(ctx context.Context, db *sql.DB, limit int) error {
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for i := 0; i < limit; i++ {
		body, err := migrations.ReadFile(names[i])
		if err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf("migration %s: %w", names[i], err)
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			return err
		}
	}
	return nil
}

func schemaOf(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT type || ' ' || name || ' ' || coalesce(sql, '') FROM sqlite_master ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// A database the old runner left at any version upgrades to the same schema
// a new database gets.
func TestUpgradeFromLegacyRunner(t *testing.T) {
	ctx := context.Background()
	fresh := open(t, filepath.Join(t.TempDir(), "fresh.db"))
	want := schemaOf(t, fresh.db)
	fresh.Close()
	newest := embeddedVersion(t)
	for v := 0; v <= newest; v++ {
		t.Run(fmt.Sprintf("from %d", v), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "h.db")
			db, err := sql.Open("sqlite", "file:"+path)
			if err != nil {
				t.Fatal(err)
			}
			if err := legacyMigrate(ctx, db, v); err != nil {
				t.Fatal(err)
			}
			db.Close()
			s := open(t, path)
			defer s.Close()
			if got, _ := s.Version(ctx); got != newest {
				t.Fatalf("version %d, want %d", got, newest)
			}
			got := schemaOf(t, s.db)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("schema differs from a new database:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}

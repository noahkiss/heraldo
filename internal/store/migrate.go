package store

import (
	"context"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
)

// migrationName is a migration file's name: a number, an underscore, a
// description and ".sql". The number is the schema version the file brings
// the database to.
var migrationName = regexp.MustCompile(`^([0-9]+)_[A-Za-z0-9_]+\.sql$`)

type migration struct {
	version int
	name    string
	body    string
}

// loadMigrations reads every .sql file in fsys and returns them in version
// order. It refuses a name without a number, two files with one number, and
// a gap: the versions must run 1, 2, 3, ... with none missing.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	var ms []migration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		m := migrationName.FindStringSubmatch(name)
		if m == nil {
			return nil, fmt.Errorf("migration %s: the name must be NNNN_description.sql", name)
		}
		v, err := strconv.Atoi(m[1])
		if err != nil || v < 1 {
			return nil, fmt.Errorf("migration %s: the number must be 1 or more", name)
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		ms = append(ms, migration{version: v, name: name, body: string(body)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	for i, m := range ms {
		if i > 0 && m.version == ms[i-1].version {
			return nil, fmt.Errorf("migrations %s and %s share the number %d", ms[i-1].name, m.name, m.version)
		}
		if m.version != i+1 {
			return nil, fmt.Errorf("migration %s: number %d follows %d; no migration has the number %d", m.name, m.version, i, i+1)
		}
	}
	return ms, nil
}

// migrate applies, in version order, each embedded migration above the
// database's version (PRAGMA user_version), one transaction per file.
func (s *Store) migrate(ctx context.Context) error {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return err
	}
	return s.migrateFS(ctx, sub)
}

func (s *Store) migrateFS(ctx context.Context, fsys fs.FS) error {
	ms, err := loadMigrations(fsys)
	if err != nil {
		return err
	}
	var have int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&have); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if have > len(ms) {
		// A newer server ran on this database. Its schema may not work
		// with this binary, so refuse rather than guess.
		return fmt.Errorf("database schema version %d is newer than this binary (%d)", have, len(ms))
	}
	for _, m := range ms[have:] {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, m.body); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
	}
	return nil
}

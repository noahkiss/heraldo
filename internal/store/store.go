// Package store keeps heraldo's events in SQLite: an append-only event log,
// a full-text index, and derived state tables updated on every write.
package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"modernc.org/sqlite"

	"github.com/noahkiss/heraldo/api"
)

//go:embed migrations/*.sql
var migrations embed.FS

// DedupWindow is how long a dedup id stays claimed.
const DedupWindow = 24 * time.Hour

// timeFormat is fixed width, so stored times compare as strings.
const timeFormat = "2006-01-02T15:04:05.000000000Z"

// ErrNotFound is returned by Get for an unknown id.
var ErrNotFound = errors.New("not found")

// Store is an open database.
type Store struct {
	db *sql.DB
	// wmu serialises writes, so the dedup check and the insert are atomic.
	wmu sync.Mutex
	now func() time.Time
}

// Open opens or creates the database at path and applies migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	q := url.Values{}
	for _, p := range []string{"journal_mode(WAL)", "busy_timeout(5000)", "synchronous(NORMAL)", "foreign_keys(1)"} {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, now: time.Now}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// SetClock replaces the clock used for the dedup window. Tests use it.
func (s *Store) SetClock(now func() time.Time) { s.now = now }

func (s *Store) migrate(ctx context.Context) error {
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	var have int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&have); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if have > len(names) {
		return fmt.Errorf("database schema version %d is newer than this binary (%d)", have, len(names))
	}
	for i := have; i < len(names); i++ {
		body, err := migrations.ReadFile(names[i])
		if err != nil {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", names[i], err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %s: %w", names[i], err)
		}
	}
	return nil
}

// Version returns the applied schema version.
func (s *Store) Version(ctx context.Context) (int, error) {
	var v int
	err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	return v, err
}

// NewEvent is an event ready to store.
type NewEvent struct {
	ID         string
	ReceivedAt time.Time
	// Fields is the full stored event: the sender's fields plus id,
	// received_at, ts and, for an import, hold.
	Fields map[string]any
	// Typed is the sender's part, used for the indexed columns and the
	// derived tables.
	Typed api.EventInput
	// DedupAnyAge matches the dedup id against every earlier event, not
	// only the last DedupWindow. Bridged senders whose dedup id is a
	// permanent record id use it, so a backfill and live traffic never
	// store the same record twice.
	DedupAnyAge bool
}

// Record is one stored event.
type Record struct {
	Seq   int64
	ID    string
	Event json.RawMessage
}

// Insert stores ev. When ev carries a dedup id already stored in the last
// DedupWindow, it stores nothing and returns the first event with created
// false.
func (s *Store) Insert(ctx context.Context, ev NewEvent) (Record, bool, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, false, err
	}
	defer tx.Rollback()
	if r, found, err := s.findDedup(ctx, tx, ev); err != nil || found {
		return r, false, err
	}
	r, err := insert(ctx, tx, ev)
	if err != nil {
		return Record{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Record{}, false, err
	}
	return r, true, nil
}

// findDedup returns the first event that claimed ev's dedup id.
func (s *Store) findDedup(ctx context.Context, tx *sql.Tx, ev NewEvent) (Record, bool, error) {
	d := ev.Typed.Dedup
	if d == nil {
		return Record{}, false, nil
	}
	since := ""
	if !ev.DedupAnyAge {
		since = s.now().Add(-DedupWindow).UTC().Format(timeFormat)
	}
	var r Record
	var raw string
	err := tx.QueryRowContext(ctx,
		`SELECT seq, id, event FROM events WHERE dedup = ? AND received_at > ? ORDER BY seq LIMIT 1`,
		*d, since).Scan(&r.Seq, &r.ID, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	r.Event = json.RawMessage(raw)
	return r, true, nil
}

// Import stores ev unless origin/originID was imported before, or an earlier
// event already claimed ev's dedup id (at any age, as with DedupAnyAge). It
// reports whether it stored the event.
func (s *Store) Import(ctx context.Context, origin, originID string, ev NewEvent) (bool, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var x string
	err = tx.QueryRowContext(ctx, `SELECT event_id FROM imports WHERE origin = ? AND origin_id = ?`,
		origin, originID).Scan(&x)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	ev.DedupAnyAge = true
	if _, found, err := s.findDedup(ctx, tx, ev); err != nil || found {
		return false, err
	}
	if _, err := insert(ctx, tx, ev); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO imports (origin, origin_id, event_id) VALUES (?, ?, ?)`,
		origin, originID, ev.ID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func insert(ctx context.Context, tx *sql.Tx, ev NewEvent) (Record, error) {
	raw, err := json.Marshal(ev.Fields)
	if err != nil {
		return Record{}, err
	}
	in := ev.Typed
	ts := ev.ReceivedAt
	if in.Ts != nil {
		ts = *in.Ts
	}
	var tags any
	if in.Tags != nil && len(*in.Tags) > 0 {
		b, _ := json.Marshal(*in.Tags)
		tags = string(b)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO events
		(id, received_at, ts, source, host, kind, key, action, level, tags, dedup, event)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.ID, ev.ReceivedAt.UTC().Format(timeFormat), ts.UTC().Format(timeFormat),
		in.Source, in.Host, string(in.Kind), in.Key, in.Action, in.Level, tags, in.Dedup, string(raw))
	if err != nil {
		return Record{}, err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return Record{}, err
	}
	if in.Title != nil || in.Summary != nil || in.Body != nil {
		if _, err := tx.ExecContext(ctx, `INSERT INTO events_fts (rowid, title, summary, body) VALUES (?, ?, ?, ?)`,
			seq, in.Title, in.Summary, in.Body); err != nil {
			return Record{}, err
		}
	}
	if err := derive(ctx, tx, ev); err != nil {
		return Record{}, err
	}
	return Record{Seq: seq, ID: ev.ID, Event: raw}, nil
}

// derive updates the state tables for one event.
func derive(ctx context.Context, tx *sql.Tx, ev NewEvent) error {
	in := ev.Typed
	if in.Key == nil {
		return nil
	}
	key := *in.Key
	at := ev.ReceivedAt.UTC().Format(timeFormat)
	action := ""
	if in.Action != nil {
		action = string(*in.Action)
	}
	switch in.Kind {
	case api.KindIncident:
		switch action {
		case "resolve":
			_, err := tx.ExecContext(ctx, `INSERT INTO incidents (key, state, opened_at, resolved_at, last_event)
				VALUES (?, 'resolved', ?, ?, ?)
				ON CONFLICT (key) DO UPDATE SET state = 'resolved', resolved_at = excluded.resolved_at,
				last_event = excluded.last_event`, key, at, at, ev.ID)
			return err
		case "ack":
			_, err := tx.ExecContext(ctx, `INSERT INTO incidents (key, state, opened_at, last_event)
				VALUES (?, 'acked', ?, ?)
				ON CONFLICT (key) DO UPDATE SET state = 'acked', last_event = excluded.last_event`, key, at, ev.ID)
			return err
		default: // open, update, or none
			// A resolved incident that gets a new open or update reopens.
			_, err := tx.ExecContext(ctx, `INSERT INTO incidents (key, state, opened_at, last_event)
				VALUES (?, 'open', ?, ?)
				ON CONFLICT (key) DO UPDATE SET
				  opened_at = CASE WHEN incidents.state = 'resolved' THEN excluded.opened_at ELSE incidents.opened_at END,
				  resolved_at = CASE WHEN incidents.state = 'resolved' THEN NULL ELSE incidents.resolved_at END,
				  state = CASE WHEN incidents.state = 'acked' THEN 'acked' ELSE 'open' END,
				  last_event = excluded.last_event`, key, at, ev.ID)
			return err
		}
	case api.KindCheckin:
		state := ""
		switch action {
		case "start":
			state = "running"
		case "fail":
			state = "down"
		case "log":
		default: // success, or none: a plain ping is a success
			state = "up"
		}
		if in.Checkin != nil && in.Checkin.ExitCode != nil && *in.Checkin.ExitCode != 0 && action != "log" {
			state = "down"
		}
		if state == "" {
			_, err := tx.ExecContext(ctx, `INSERT INTO checkins (key, last_ping, state, last_event)
				VALUES (?, ?, 'unknown', ?)
				ON CONFLICT (key) DO UPDATE SET last_ping = excluded.last_ping, last_event = excluded.last_event`,
				key, at, ev.ID)
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO checkins (key, last_ping, state, last_event)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (key) DO UPDATE SET last_ping = excluded.last_ping, state = excluded.state,
			last_event = excluded.last_event`, key, at, state, ev.ID)
		return err
	case api.KindProgress:
		state := "active"
		if action == "end" {
			state = "ended"
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO activities (key, state, last_event) VALUES (?, ?, ?)
			ON CONFLICT (key) DO UPDATE SET state = excluded.state, last_event = excluded.last_event`,
			key, state, ev.ID)
		return err
	case api.KindStatus:
		if action == "clear" {
			_, err := tx.ExecContext(ctx, `DELETE FROM status WHERE key = ?`, key)
			return err
		}
		var old string
		err := tx.QueryRowContext(ctx, `SELECT content FROM status WHERE key = ?`, key).Scan(&old)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		merged, err := mergeContent(old, ev.Fields["content"])
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO status (key, content, last_event) VALUES (?, ?, ?)
			ON CONFLICT (key) DO UPDATE SET content = excluded.content, last_event = excluded.last_event`,
			key, merged, ev.ID)
		return err
	}
	return nil
}

// mergeContent applies a LiveContent update: an absent field keeps its old
// value and a null field clears it.
func mergeContent(old string, update any) (string, error) {
	cur := map[string]any{}
	if old != "" {
		if err := json.Unmarshal([]byte(old), &cur); err != nil {
			return "", err
		}
	}
	if u, ok := update.(map[string]any); ok {
		for k, v := range u {
			if v == nil {
				delete(cur, k)
			} else {
				cur[k] = v
			}
		}
	}
	b, err := json.Marshal(cur)
	return string(b), err
}

// Get returns the event with id.
func (s *Store) Get(ctx context.Context, id string) (Record, error) {
	var r Record
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT seq, id, event FROM events WHERE id = ?`, id).Scan(&r.Seq, &r.ID, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	r.Event = json.RawMessage(raw)
	return r, err
}

// Filter selects events for List. Empty fields do not filter.
type Filter struct {
	AfterSeq int64
	Limit    int
	Kind     string
	Source   string
	Host     string
	Key      string
	Tag      string
	// Q is full-text search over title, summary and body. Every word must match.
	Q string
}

// List returns events after f.AfterSeq in arrival order.
func (s *Store) List(ctx context.Context, f Filter) ([]Record, error) {
	where := []string{"seq > ?"}
	args := []any{f.AfterSeq}
	for _, c := range []struct{ col, v string }{
		{"kind", f.Kind}, {"source", f.Source}, {"host", f.Host}, {"key", f.Key},
	} {
		if c.v != "" {
			where = append(where, c.col+" = ?")
			args = append(args, c.v)
		}
	}
	if f.Tag != "" {
		where = append(where, "EXISTS (SELECT 1 FROM json_each(events.tags) WHERE value = ?)")
		args = append(args, f.Tag)
	}
	if q := ftsQuery(f.Q); q != "" {
		where = append(where, "seq IN (SELECT rowid FROM events_fts WHERE events_fts MATCH ?)")
		args = append(args, q)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, id, event FROM events WHERE `+strings.Join(where, " AND ")+` ORDER BY seq LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var r Record
		var raw string
		if err := rows.Scan(&r.Seq, &r.ID, &raw); err != nil {
			return nil, err
		}
		r.Event = json.RawMessage(raw)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ftsQuery turns free text into an FTS5 query: each word a quoted string,
// all of them required, so no input is an FTS syntax error.
func ftsQuery(q string) string {
	var terms []string
	for _, w := range strings.Fields(q) {
		terms = append(terms, `"`+strings.ReplaceAll(w, `"`, `""`)+`"`)
	}
	return strings.Join(terms, " ")
}

// Count returns the number of stored events.
func (s *Store) Count(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM events`).Scan(&n)
	return n, err
}

// Row reads one derived-state row as column name to value. Tests and the
// later state API use it.
func (s *Store) Row(ctx context.Context, table, key string) (map[string]any, error) {
	switch table {
	case "incidents", "checkins", "activities", "status":
	default:
		return nil, fmt.Errorf("unknown table %q", table)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT * FROM `+table+` WHERE key = ?`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	if !rows.Next() {
		return nil, ErrNotFound
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	out := make(map[string]any, len(cols))
	for i, c := range cols {
		out[c] = vals[i]
	}
	return out, rows.Err()
}

// Backup copies the database to dst with SQLite's online backup API. The
// server can keep writing meanwhile. dst must not exist.
func (s *Store) Backup(ctx context.Context, dst string) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Raw(func(dc any) error {
		b, ok := dc.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("driver has no backup API")
		}
		bk, err := b.NewBackup(dst)
		if err != nil {
			return err
		}
		for {
			more, err := bk.Step(256)
			if err != nil {
				bk.Finish()
				return err
			}
			if !more {
				break
			}
		}
		return bk.Finish()
	})
}

// CursorFor encodes a list cursor.
func CursorFor(seq int64) string { return "s" + strconv.FormatInt(seq, 36) }

// ParseCursor decodes a cursor from CursorFor.
func ParseCursor(c string) (int64, error) {
	if !strings.HasPrefix(c, "s") {
		return 0, errors.New("bad cursor")
	}
	n, err := strconv.ParseInt(c[1:], 36, 64)
	if err != nil || n < 0 {
		return 0, errors.New("bad cursor")
	}
	return n, nil
}

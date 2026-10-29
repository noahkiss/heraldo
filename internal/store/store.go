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
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"modernc.org/sqlite"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/routine"
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
	// imported is set by Import: the event is history, so it never
	// schedules a re-alert.
	imported bool
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
	ev.DedupAnyAge, ev.imported = true, true
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
	in := ev.Typed
	// The server types every input before it stores it (rules.Type), so a
	// stored event always has a kind.
	if in.Kind == nil {
		return Record{}, errors.New("store: event has no kind")
	}
	if in.Attachments != nil {
		if err := linkAttachments(ctx, tx, ev.ID, *in.Attachments); err != nil {
			return Record{}, err
		}
	}
	// An event with a key and no project files under its key's project,
	// so an ack or a resolve from a sender that does not know it lands
	// beside the open.
	if in.Project == nil && in.Key != nil {
		p, err := keyProject(ctx, tx, *in.Kind, *in.Key)
		if err != nil {
			return Record{}, err
		}
		if p != "" {
			in.Project = &p
			ev.Typed.Project = &p
			ev.Fields["project"] = p
		}
	}
	// An incident event carries the incident as it stands after the event,
	// so a resolve knows how long the incident was open.
	var inc *incidentRow
	if *in.Kind == api.KindIncident && in.Key != nil {
		var err error
		if inc, err = nextIncident(ctx, tx, ev); err != nil {
			return Record{}, err
		}
		ev.Fields["incident"] = inc.snapshot()
	}
	raw, err := json.Marshal(ev.Fields)
	if err != nil {
		return Record{}, err
	}
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
		(id, received_at, ts, source, host, kind, key, action, level, tags, dedup, project, event)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.ID, ev.ReceivedAt.UTC().Format(timeFormat), ts.UTC().Format(timeFormat),
		in.Source, in.Host, string(*in.Kind), in.Key, in.Action, in.Level, tags, in.Dedup, in.Project, string(raw))
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
	if err := derive(ctx, tx, ev, inc); err != nil {
		return Record{}, err
	}
	if in.Project != nil && in.Key != nil {
		if t := derivedTable(*in.Kind); t != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE `+t+` SET project = ? WHERE key = ?`, *in.Project, *in.Key); err != nil {
				return Record{}, err
			}
		}
	}
	return Record{Seq: seq, ID: ev.ID, Event: raw}, nil
}

// derivedTable is the state table that keeps the events of kind by key, or
// "" for a kind without one.
func derivedTable(kind api.Kind) string {
	switch kind {
	case api.KindIncident:
		return "incidents"
	case api.KindCheckin:
		return "checkins"
	case api.KindProgress:
		return "activities"
	case api.KindStatus:
		return "status"
	}
	return ""
}

// keyProject is the project of key's row in the state table of kind, or "".
// A check-in incident with none takes its check's project.
func keyProject(ctx context.Context, q queryer, kind api.Kind, key string) (string, error) {
	t := derivedTable(kind)
	if t == "" {
		return "", nil
	}
	var p sql.NullString
	err := q.QueryRowContext(ctx, `SELECT project FROM `+t+` WHERE key = ?`, key).Scan(&p)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if p.String == "" && kind == api.KindIncident && strings.HasPrefix(key, CheckinKeyPrefix) {
		return keyProject(ctx, q, api.KindCheckin, strings.TrimPrefix(key, CheckinKeyPrefix))
	}
	return p.String, nil
}

// derive updates the state tables for one event. inc is the incident row
// after an incident event, from nextIncident.
func derive(ctx context.Context, tx *sql.Tx, ev NewEvent, inc *incidentRow) error {
	in := ev.Typed
	if in.Key == nil {
		return nil
	}
	key := *in.Key
	action := ""
	if in.Action != nil {
		action = string(*in.Action)
	}
	switch *in.Kind {
	case api.KindIncident:
		return writeIncident(ctx, tx, key, inc)
	case api.KindCheckin:
		return deriveCheckin(ctx, tx, ev, key)
	case api.KindProgress:
		return deriveActivity(ctx, tx, ev, key, action, ev.ReceivedAt.UTC().Format(timeFormat))
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
	// BeforeSeq, when above 0, keeps events with a lower seq only.
	BeforeSeq int64
	// Desc lists newest first.
	Desc   bool
	Limit  int
	Kind   string
	Source string
	Host   string
	Key    string
	Tag    string
	// Q is full-text search over title, summary and body. Every word must match.
	Q string
	// Hold keeps events whose hold.state is this value, such as held.
	Hold string
	// Projects keeps events in any of these projects; NoProject in it
	// keeps the events with none.
	Projects []string
	// Routine is routine.Exclude to leave routine events out, or
	// routine.Only to keep only them. Empty keeps both.
	Routine string
}

// RoutineWhere is the SQL form of routine.Is: progress and status events,
// and check-ins that do not push by the push table (push.Decide). The
// partial index events_main (migration 0010) is on NOT RoutineWhere, so a
// query must use this exact expression for SQLite to pick it.
const RoutineWhere = `(kind IN ('progress', 'status') OR (kind = 'checkin' AND NOT (coalesce(action, '') = 'fail'
	OR (coalesce(action, '') != 'log' AND coalesce(json_extract(event, '$.checkin.exit_code'), 0) != 0))))`

// NoProject is the project filter value for "no project".
const NoProject = "-"

// projectWhere is the SQL condition for a project filter on col, and its
// arguments, or "" when projects is empty.
func projectWhere(col string, projects []string) (string, []any) {
	if len(projects) == 0 {
		return "", nil
	}
	var or []string
	var args []any
	var names []string
	for _, p := range projects {
		if p == NoProject {
			or = append(or, col+" IS NULL")
			continue
		}
		names = append(names, "?")
		args = append(args, p)
	}
	if len(names) > 0 {
		or = append(or, col+" IN ("+strings.Join(names, ", ")+")")
	}
	return "(" + strings.Join(or, " OR ") + ")", args
}

// List returns events after f.AfterSeq in arrival order, or newest first
// with f.Desc.
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
	if f.BeforeSeq > 0 {
		where = append(where, "seq < ?")
		args = append(args, f.BeforeSeq)
	}
	if f.Hold != "" {
		where = append(where, "json_extract(event, '$.hold.state') = ?")
		args = append(args, f.Hold)
	}
	if w, a := projectWhere("project", f.Projects); w != "" {
		where = append(where, w)
		args = append(args, a...)
	}
	switch f.Routine {
	case routine.Exclude:
		where = append(where, "NOT "+RoutineWhere)
	case routine.Only:
		where = append(where, RoutineWhere)
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
	order := "seq"
	if f.Desc {
		order = "seq DESC"
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, id, event FROM events WHERE `+strings.Join(where, " AND ")+` ORDER BY `+order+` LIMIT ?`, args...)
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
// server can keep writing meanwhile. dst must not exist. The copy is one
// self-contained file in rollback-journal mode, so opening it later leaves no
// -wal or -shm file beside it.
func (s *Store) Backup(ctx context.Context, dst string) error {
	if err := s.backup(ctx, dst); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", "file:"+dst)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, `PRAGMA journal_mode=DELETE`)
	return err
}

func (s *Store) backup(ctx context.Context, dst string) error {
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

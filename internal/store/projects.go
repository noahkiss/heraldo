package store

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/noahkiss/heraldo/api"
)

// ProjectCount is one project the stored events name.
type ProjectCount struct {
	Name   string
	Events int64
	LastAt time.Time
}

// Projects returns the projects the stored events name, by name, and the
// number of events with none.
func (s *Store) Projects(ctx context.Context) ([]ProjectCount, int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT project, count(*), max(received_at) FROM events
		WHERE project IS NOT NULL GROUP BY project ORDER BY project`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []ProjectCount{}
	for rows.Next() {
		var p ProjectCount
		var last string
		if err := rows.Scan(&p.Name, &p.Events, &last); err != nil {
			return nil, 0, err
		}
		p.LastAt = parseTime(last)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var none int64
	err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE project IS NULL`).Scan(&none)
	return out, none, err
}

// Backfill is the outcome of BackfillProjects.
type Backfill struct {
	// Scanned counts the events with no project before the run.
	Scanned int
	// Changed counts the events that got one, per project.
	Changed map[string]int
	// Derived counts the state rows that got one.
	Derived int
}

// Total is the number of events that got a project.
func (b Backfill) Total() int {
	n := 0
	for _, c := range b.Changed {
		n += c
	}
	return n
}

// BackfillProjects gives a project to the stored events that have none:
// decide's answer for the stored event, else the project of an earlier
// event with the same key, as a live insert would inherit it. It sets the
// project column and the event's project field, and nothing else; it never
// pushes. Then each state row with no project takes the project of its
// key's newest event that has one. With dry, it reports what it would
// change and changes nothing. A rerun changes nothing new.
func (s *Store) BackfillProjects(ctx context.Context, decide func(fields map[string]any) string, dry bool) (Backfill, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Backfill{}, err
	}
	defer tx.Rollback()
	type change struct {
		seq     int64
		project string
	}
	var changes []change
	res := Backfill{Changed: map[string]int{}}
	byKey := map[string]string{}
	keyOf := func(kind, key string) string { return kind + "\x00" + key }
	rows, err := tx.QueryContext(ctx, `SELECT seq, kind, coalesce(key, ''), coalesce(project, ''), event FROM events ORDER BY seq`)
	if err != nil {
		return Backfill{}, err
	}
	for rows.Next() {
		var seq int64
		var kind, key, project, raw string
		if err := rows.Scan(&seq, &kind, &key, &project, &raw); err != nil {
			rows.Close()
			return Backfill{}, err
		}
		if project != "" {
			if key != "" {
				byKey[keyOf(kind, key)] = project
			}
			continue
		}
		res.Scanned++
		dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
		dec.UseNumber()
		var fields map[string]any
		if dec.Decode(&fields) == nil {
			project = decide(fields)
		}
		if project == "" && key != "" {
			project = byKey[keyOf(kind, key)]
			if project == "" && kind == string(api.KindIncident) && strings.HasPrefix(key, CheckinKeyPrefix) {
				project = byKey[keyOf(string(api.KindCheckin), strings.TrimPrefix(key, CheckinKeyPrefix))]
			}
		}
		if project == "" {
			continue
		}
		if key != "" {
			byKey[keyOf(kind, key)] = project
		}
		changes = append(changes, change{seq, project})
		res.Changed[project]++
	}
	if err := rows.Close(); err != nil {
		return Backfill{}, err
	}
	for _, c := range changes {
		if _, err := tx.ExecContext(ctx, `UPDATE events SET project = ?, event = json_set(event, '$.project', ?)
			WHERE seq = ?`, c.project, c.project, c.seq); err != nil {
			return Backfill{}, err
		}
	}
	for _, kind := range []api.Kind{api.KindIncident, api.KindCheckin, api.KindProgress, api.KindStatus} {
		t := derivedTable(kind)
		newest := `SELECT e.project FROM events e WHERE e.kind = ? AND e.key = ` + t + `.key
			AND e.project IS NOT NULL ORDER BY e.seq DESC LIMIT 1`
		r, err := tx.ExecContext(ctx, `UPDATE `+t+` SET project = (`+newest+`)
			WHERE project IS NULL AND EXISTS (`+newest+`)`, string(kind), string(kind))
		if err != nil {
			return Backfill{}, err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return Backfill{}, err
		}
		res.Derived += int(n)
	}
	if dry {
		return res, nil
	}
	return res, tx.Commit()
}

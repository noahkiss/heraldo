package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// FlapPolicy is how the store marks a flapping incident: one that opens
// Opens times within Window. It stops flapping once quiet for Settle. The
// server fills it from the flap.* settings; the zero policy is off.
type FlapPolicy struct {
	Enabled bool
	Opens   int
	Window  time.Duration
	Settle  time.Duration
}

// maxFlapOpens caps the open times an incident row keeps.
const maxFlapOpens = 64

// SetFlapPolicy makes f the policy every later incident write and State
// read use. nil turns flap tracking off. heraldo-server calls it once at
// start.
func (s *Store) SetFlapPolicy(f func() FlapPolicy) {
	if f == nil {
		s.flap.Store(nil)
		return
	}
	s.flap.Store(&f)
}

// FlapPolicy returns the policy in force; off without a source. Opens is
// at least 2.
func (s *Store) FlapPolicy() FlapPolicy {
	f := s.flap.Load()
	if f == nil {
		return FlapPolicy{}
	}
	p := (*f)()
	if p.Opens < 2 {
		p.Opens = 2
	}
	if p.Window <= 0 {
		p.Enabled = false
	}
	return p
}

// settled reports whether a flapping incident whose last open or resolve
// was at last has been quiet for settle at now.
func settled(last, now time.Time, settle time.Duration) bool {
	return !last.IsZero() && now.Sub(last) >= settle
}

// flapStep moves r's flap state on for one incident event at at. opened
// is set when the event opened the incident (from none or resolved),
// resolved when it resolved an unresolved one.
//
// An incident is flapping once it has opened p.Opens times within
// p.Window. flap_since is the first of those opens and flap_count counts
// every open from it. A flapping incident quiet for p.Settle stops
// flapping, and the next open starts the count over.
func flapStep(r *incidentRow, p FlapPolicy, at time.Time, opened, resolved bool) {
	if !p.Enabled {
		r.flapping, r.flapCount, r.flapSince, r.flapOpens, r.flapLast = false, 0, time.Time{}, nil, time.Time{}
		return
	}
	if r.flapping && settled(r.flapLast, at, p.Settle) {
		r.flapping, r.flapCount, r.flapSince, r.flapOpens = false, 0, time.Time{}, nil
	}
	if resolved {
		r.flapLast = at
	}
	if !opened {
		return
	}
	r.flapLast = at
	var keep []time.Time
	for _, t := range r.flapOpens {
		if at.Sub(t) < p.Window {
			keep = append(keep, t)
		}
	}
	keep = append(keep, at)
	if len(keep) > maxFlapOpens {
		keep = keep[len(keep)-maxFlapOpens:]
	}
	r.flapOpens = keep
	switch {
	case r.flapping:
		r.flapCount++
	case len(keep) >= p.Opens:
		r.flapping, r.flapSince, r.flapCount = true, keep[0], len(keep)
	}
}

func encodeOpens(ts []time.Time) any {
	if len(ts) == 0 {
		return nil
	}
	s := make([]string, len(ts))
	for i, t := range ts {
		s[i] = t.UTC().Format(timeFormat)
	}
	b, _ := json.Marshal(s)
	return string(b)
}

func decodeOpens(raw string) []time.Time {
	if raw == "" {
		return nil
	}
	var s []string
	if json.Unmarshal([]byte(raw), &s) != nil {
		return nil
	}
	out := make([]time.Time, 0, len(s))
	for _, x := range s {
		if t := parseTime(x); !t.IsZero() {
			out = append(out, t)
		}
	}
	return out
}

// unflap drops the flap fields from the incident snapshots in incidents
// whose key is no longer flapping at now: quiet for the policy's settle, or
// the policy is off. The stored snapshot is the incident as it stood after
// its last event, and settling needs no event.
func (s *Store) unflap(ctx context.Context, incidents []json.RawMessage, now time.Time) error {
	rows, err := s.db.QueryContext(ctx, `SELECT last_event, flap_last FROM incidents WHERE flapping = 1 AND state != 'resolved'`)
	if err != nil {
		return err
	}
	p := s.FlapPolicy()
	stale := map[string]bool{}
	for rows.Next() {
		var id string
		var last sql.NullString
		if err := rows.Scan(&id, &last); err != nil {
			rows.Close()
			return err
		}
		if !p.Enabled || settled(parseNullTime(last), now, p.Settle) {
			stale[id] = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(stale) == 0 {
		return nil
	}
	for i, raw := range incidents {
		var ev map[string]any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if dec.Decode(&ev) != nil {
			continue
		}
		id, _ := ev["id"].(string)
		inc, _ := ev["incident"].(map[string]any)
		if !stale[id] || inc == nil {
			continue
		}
		delete(inc, "flapping")
		delete(inc, "flap_count")
		delete(inc, "flap_since")
		if b, err := json.Marshal(ev); err == nil {
			incidents[i] = b
		}
	}
	return nil
}

// Flapping reports whether the incident key is flapping at now.
func (s *Store) Flapping(ctx context.Context, key string, now time.Time) (bool, error) {
	var flapping bool
	var last sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT flapping, flap_last FROM incidents WHERE key = ?`, key).Scan(&flapping, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	p := s.FlapPolicy()
	return flapping && p.Enabled && !settled(parseNullTime(last), now, p.Settle), nil
}

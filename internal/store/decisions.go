package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// DecisionRow is one stage's row in an event's push trace.
type DecisionRow struct {
	Stage   string
	Outcome string
	Reason  string
	// Detail is a JSON object, or nil.
	Detail json.RawMessage
	At     time.Time
}

// appendBase is the first row number of the rows AppendDecision adds. The
// pipeline's rows sit below it, so a new push decision replaces them and
// keeps the appended ones.
const appendBase = 1000

// AppendStage is the stage of the rows the escalation worker appends.
const AppendStage = "escalate"

// RecordDecisions replaces the push trace of an event with rows, numbered
// in order from 0. Rows AppendDecision added stay.
func (s *Store) RecordDecisions(ctx context.Context, eventID string, rows []DecisionRow) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM push_decisions WHERE event_id = ? AND n < ?`, eventID, appendBase); err != nil {
		return err
	}
	for i, r := range rows {
		var detail any
		if len(r.Detail) > 0 {
			detail = string(r.Detail)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO push_decisions (event_id, n, stage, outcome, reason, detail, at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, eventID, i, r.Stage, r.Outcome, r.Reason, detail,
			r.At.UTC().Format(timeFormat)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AppendDecision adds a row after an event's push trace. It is for work
// beside the push pipeline, such as an escalation, so it runs with push off
// too; a later push decision keeps it.
func (s *Store) AppendDecision(ctx context.Context, eventID string, r DecisionRow) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	var detail any
	if len(r.Detail) > 0 {
		detail = string(r.Detail)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO push_decisions (event_id, n, stage, outcome, reason, detail, at)
		SELECT ?, MAX(?, COALESCE(MAX(n) + 1, 0)), ?, ?, ?, ?, ? FROM push_decisions WHERE event_id = ?`,
		eventID, appendBase, r.Stage, r.Outcome, r.Reason, detail, r.At.UTC().Format(timeFormat), eventID)
	return err
}

// Decisions returns the push trace of an event in stage order: none when
// the pusher has not decided it.
func (s *Store) Decisions(ctx context.Context, eventID string) ([]DecisionRow, error) {
	by, err := s.decisions(ctx, []string{eventID})
	return by[eventID], err
}

func (s *Store) decisions(ctx context.Context, ids []string) (map[string][]DecisionRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, `SELECT event_id, stage, outcome, reason, detail, at FROM push_decisions
		WHERE event_id IN (?`+strings.Repeat(", ?", len(ids)-1)+`) ORDER BY event_id, n`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	by := map[string][]DecisionRow{}
	for rows.Next() {
		var ev, at string
		var detail sql.NullString
		var r DecisionRow
		if err := rows.Scan(&ev, &r.Stage, &r.Outcome, &r.Reason, &detail, &at); err != nil {
			return nil, err
		}
		if detail.Valid {
			r.Detail = json.RawMessage(detail.String)
		}
		r.At, _ = time.Parse(timeFormat, at)
		by[ev] = append(by[ev], r)
	}
	return by, rows.Err()
}

// LLMStage is the stage of the rows the LLM's decisions role appends.
const LLMStage = "llm"

// DecisionSummary is the one line that sums up a push trace: the reason of
// the stage that ended it, then each downgrade on the way, then the last
// LLM row's reason (LLMStage). Escalation rows (AppendStage) count only
// when the trace has nothing else, as on a server with push off. It is ""
// for an empty trace.
func DecisionSummary(rows []DecisionRow) string {
	var pipe []DecisionRow
	var llm string
	for _, r := range rows {
		switch r.Stage {
		case AppendStage:
		case LLMStage:
			llm = r.Reason
		default:
			pipe = append(pipe, r)
		}
	}
	if len(pipe) == 0 {
		if llm != "" {
			return llm
		}
		if len(rows) == 0 {
			return ""
		}
		return rows[len(rows)-1].Reason
	}
	out := pipe[len(pipe)-1].Reason
	for _, r := range pipe[:len(pipe)-1] {
		if r.Outcome == "downgrade" {
			out += "; " + r.Reason
		}
	}
	if llm != "" {
		out += "; " + llm
	}
	return out
}

// WithWhy adds each record's push summary to its event as why. A record
// the pusher has not decided is left as stored.
func (s *Store) WithWhy(ctx context.Context, recs []Record) error {
	ids := make([]string, len(recs))
	for i, r := range recs {
		ids[i] = r.ID
	}
	by, err := s.decisions(ctx, ids)
	if err != nil {
		return err
	}
	for i, r := range recs {
		sum := DecisionSummary(by[r.ID])
		if sum == "" {
			continue
		}
		raw, err := withField(r.Event, "why", sum)
		if err != nil {
			return err
		}
		recs[i].Event = raw
	}
	return nil
}

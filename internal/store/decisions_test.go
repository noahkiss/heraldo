package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// An appended escalate row outlives a later push decision, and the summary
// is the pipeline's unless the trace has nothing else.
func TestAppendDecision(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "h.db"))
	defer s.Close()
	at := time.Date(2026, 10, 29, 3, 0, 0, 0, time.UTC)
	esc := DecisionRow{Stage: AppendStage, Outcome: "pass", Reason: "escalated to Pushover", At: at}
	if err := s.AppendDecision(ctx, "ev1", esc); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.Decisions(ctx, "ev1")
	if len(rows) != 1 || DecisionSummary(rows) != "escalated to Pushover" {
		t.Fatalf("escalate only: %+v", rows)
	}
	pipe := []DecisionRow{
		{Stage: "table", Outcome: "downgrade", Reason: "lowered", At: at},
		{Stage: "devices", Outcome: "drop", Reason: "no active devices", At: at},
	}
	for range 2 { // a re-decision replaces the pipeline rows only
		if err := s.RecordDecisions(ctx, "ev1", pipe); err != nil {
			t.Fatal(err)
		}
	}
	rows, _ = s.Decisions(ctx, "ev1")
	if len(rows) != 3 || rows[2].Stage != AppendStage || DecisionSummary(rows) != "no active devices; lowered" {
		t.Fatalf("after the pipeline: %+v %q", rows, DecisionSummary(rows))
	}
	if err := s.AppendDecision(ctx, "ev1", esc); err != nil {
		t.Fatal(err)
	}
	if rows, _ = s.Decisions(ctx, "ev1"); len(rows) != 4 {
		t.Fatalf("second append: %+v", rows)
	}
}

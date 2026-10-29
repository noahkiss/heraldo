package mcptools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/internal/store"
)

func TestEventReadWhy(t *testing.T) {
	f := newFixture(t)
	id := f.post(`{"source":"s","kind":"message","title":"hi"}`)["id"].(string)
	s := f.connect(false)
	if out, _ := s.ok("event_read", map[string]any{"id": id}); out["why"] != nil || out["decisions"] != nil {
		t.Fatalf("undecided event: %v", out)
	}
	now := time.Now()
	if err := f.st.RecordDecisions(context.Background(), id, []store.DecisionRow{
		{Stage: "hold", Outcome: "pass", Reason: "not held", At: now},
		{Stage: "table", Outcome: "drop", Reason: "passive: inbox only", Detail: json.RawMessage(`{"rule":"passive"}`), At: now},
	}); err != nil {
		t.Fatal(err)
	}
	out, text := s.ok("event_read", map[string]any{"id": id})
	ds, _ := out["decisions"].([]any)
	if out["why"] != "passive: inbox only" || len(ds) != 2 || ds[1].(map[string]any)["stage"] != "table" ||
		!strings.Contains(text, "why: passive: inbox only") {
		t.Fatalf("event_read: %v\n%s", out, text)
	}
}

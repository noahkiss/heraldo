package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/internal/store"
)

func TestWhy(t *testing.T) {
	f := newFixture(t)
	id := f.post(writeTok, `{"source":"s","kind":"message","title":"hi"}`)
	if r := f.run("why", id); r.code != 0 || !strings.Contains(r.stdout, "not decided") {
		t.Fatalf("undecided: %+v", r)
	}
	// The fixture runs no pusher: write the trace it would.
	now := time.Now()
	rows := []store.DecisionRow{
		{Stage: "hold", Outcome: "pass", Reason: "not held", At: now},
		{Stage: "mute", Outcome: "drop", Reason: "muted by mute_x (project)", Detail: json.RawMessage(`{"mute":"mute_x"}`), At: now},
	}
	if err := f.st.RecordDecisions(context.Background(), id, rows); err != nil {
		t.Fatal(err)
	}
	r := f.run("why", id)
	if r.code != 0 || !strings.Contains(r.stdout, "mute  drop  muted by") || !strings.Contains(r.stdout, "mute=mute_x") ||
		!strings.HasSuffix(r.stdout, "\nmuted by mute_x (project)\n") {
		t.Fatalf("why: %+v", r)
	}
	if r := f.run("why", "--json", id); !strings.Contains(r.stdout, `"stage": "mute"`) {
		t.Fatalf("why --json: %+v", r)
	}
	if r := f.run("show", id); !strings.Contains(r.stdout, "why:      muted by mute_x (project)") {
		t.Fatalf("show: %+v", r)
	}
	if r := f.run("why", "nope"); r.code != 1 || !strings.Contains(r.stderr, "404") {
		t.Fatalf("why unknown: %+v", r)
	}
}

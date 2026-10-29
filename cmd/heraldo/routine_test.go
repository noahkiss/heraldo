package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/noahkiss/heraldo/internal/client"
	"github.com/noahkiss/heraldo/internal/routine"
)

func TestLsHidesRoutine(t *testing.T) {
	f := newFixture(t)
	f.post(writeTok, `{"source":"s","kind":"message","title":"hello"}`)
	f.post(writeTok, `{"source":"cron","kind":"checkin","key":"job","action":"start"}`)
	f.post(writeTok, `{"source":"cron","kind":"checkin","key":"job","action":"success"}`)
	f.post(writeTok, `{"source":"cron","kind":"checkin","key":"job","action":"fail"}`)
	for _, c := range []struct {
		args []string
		n    int
	}{
		{[]string{"ls"}, 2},
		{[]string{"ls", "--routine"}, 4},
		{[]string{"ls", "--kind", "checkin"}, 3},
		{[]string{"ls", "--key", "job"}, 3},
	} {
		r := f.run(c.args...)
		if r.code != 0 || strings.Count(r.stdout, "\n") != c.n {
			t.Errorf("%v: %+v; want %d lines", c.args, r, c.n)
		}
	}
}

// A server before the filter ignores routine= and sends no
// Heraldo-Routine header; the client filters the page itself.
func TestListRoutineOnOldServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Heraldo-Order", "desc")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"events":[
			{"id":"a","source":"s","kind":"message","received_at":"2026-10-28T12:00:00Z","ts":"2026-10-28T12:00:00Z"},
			{"id":"b","source":"s","kind":"checkin","key":"k","action":"success","received_at":"2026-10-28T12:00:00Z","ts":"2026-10-28T12:00:00Z"}
		],"next_cursor":"s1"}`))
	}))
	defer srv.Close()
	c := &client.Client{BaseURL: srv.URL, Token: "t"}
	p, err := c.List(context.Background(), client.Query{Desc: true, Routine: routine.Exclude})
	if err != nil || len(p.Events) != 1 || p.Events[0].Id != "a" || p.NextCursor != "s1" {
		t.Fatalf("page = %+v, %v", p, err)
	}
}

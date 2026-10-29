package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/internal/server"
	"github.com/noahkiss/heraldo/internal/settings"
)

func TestDigestAndStale(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	f := newFixtureWith(t, func(o *server.Options) {
		o.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
		m, err := settings.New(context.Background(), settings.Options{Backend: o.Store})
		if err != nil {
			t.Fatal(err)
		}
		o.Settings = m
	})
	f.post(writeTok, `{"source":"mon","kind":"incident","action":"open","key":"disk:a","title":"Disk full"}`)
	mu.Lock()
	now = now.Add(80 * time.Hour)
	mu.Unlock()
	f.post(writeTok, `{"source":"mon","kind":"incident","action":"open","key":"web:b","title":"Web slow"}`)

	r := f.run("digest")
	if r.code != 0 || !strings.Contains(r.stdout, "### Incidents") || !strings.Contains(r.stdout, "[active]") ||
		!strings.Contains(r.stderr, "the daily digest is off") {
		t.Fatalf("digest: %+v", r)
	}
	if r := f.run("digest", "--send"); r.code != 1 || !strings.Contains(r.stderr, "HERALDO_WRITE_TOKEN") {
		t.Fatalf("send without a write token: %+v", r)
	}
	f.env["HERALDO_WRITE_TOKEN"] = writeTok
	if r := f.run("digest", "--send"); r.code != 0 || !strings.Contains(r.stdout, "sent: ") {
		t.Fatalf("send: %+v", r)
	}
	if r := f.run("digest", "--json"); r.code != 0 || !strings.Contains(r.stdout, `"day":"2026-10-30"`) {
		t.Fatalf("json: %+v", r)
	}

	// stale=1 answers whether stale.enabled is on or not.
	r = f.run("ls", "--open", "--stale")
	if r.code != 0 || !strings.Contains(r.stdout, "disk:a") || strings.Contains(r.stdout, "web:b") {
		t.Fatalf("ls --open --stale: %+v", r)
	}
	if r := f.run("ls", "--stale"); r.code == 0 || !strings.Contains(r.stderr, "--stale needs --open") {
		t.Fatalf("ls --stale: %+v", r)
	}
}

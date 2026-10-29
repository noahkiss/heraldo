package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/store"
)

func TestLsOpenMarksFlapping(t *testing.T) {
	f := newFixture(t)
	f.st.SetFlapPolicy(func() store.FlapPolicy {
		return store.FlapPolicy{Enabled: true, Opens: 2, Window: time.Hour, Settle: time.Hour}
	})
	f.post(writeTok, `{"source":"s","kind":"incident","key":"web:down","action":"open","title":"web down"}`)
	f.post(writeTok, `{"source":"s","kind":"incident","key":"web:down","action":"resolve"}`)
	f.post(writeTok, `{"source":"s","kind":"incident","key":"web:down","action":"open","title":"web down"}`)
	if r := f.run("ls", "--open"); r.code != 0 || !strings.Contains(r.stdout, "(flapping) ") {
		t.Fatalf("ls --open: %+v", r)
	}
}

func TestSnooze(t *testing.T) {
	f := newFixture(t)
	f.env["HERALDO_WRITE_TOKEN"] = writeTok
	id := f.post(writeTok, `{"source":"s","kind":"incident","key":"web:down","action":"open","title":"web down"}`)
	if r := f.run("snooze"); r.code != 2 {
		t.Fatalf("no key: %+v", r)
	}
	// By event id: the event's key.
	r := f.run("snooze", "--for", "30m", id)
	if r.code != 0 || !strings.Contains(r.stdout, "snoozed key=web:down until") {
		t.Fatalf("snooze by id: %+v", r)
	}
	// By key.
	if r := f.run("snooze", "web:down"); r.code != 0 {
		t.Fatalf("snooze by key: %+v", r)
	}
	if r := f.run("mutes"); r.code != 0 || strings.Count(r.stdout, "key=web:down (snooze)") != 2 {
		t.Fatalf("mutes: %+v", r)
	}
	code, body := f.req(http.MethodGet, "/v1/mutes", writeTok, "")
	var ml api.MuteList
	if code != http.StatusOK || json.Unmarshal([]byte(body), &ml) != nil || len(ml.Mutes) != 2 ||
		ml.Mutes[0].Remind == nil || !*ml.Mutes[0].Remind {
		t.Fatalf("mutes API: %d %s", code, body)
	}
	plain := f.post(writeTok, `{"source":"s","title":"no key"}`)
	if r := f.run("snooze", plain); r.code != 1 || !strings.Contains(r.stderr, "has no key") {
		t.Fatalf("snooze a keyless event: %+v", r)
	}
}

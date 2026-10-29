package mcptools

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/internal/server"
	"github.com/noahkiss/heraldo/internal/settings"
)

func TestDigestPreviewAndStale(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	f := newFixtureWith(t, func(o *server.Options) {
		o.Now = clock
		m, err := settings.New(context.Background(), settings.Options{Backend: o.Store,
			Flags: map[string]string{"stale.enabled": "true"}})
		if err != nil {
			t.Fatal(err)
		}
		o.Settings = m
	})
	f.post(`{"source":"mon","kind":"incident","action":"open","key":"disk:a","title":"Disk full"}`)
	mu.Lock()
	now = now.Add(80 * time.Hour)
	mu.Unlock()
	f.post(`{"source":"mon","kind":"incident","action":"open","key":"web:b","title":"Web slow"}`)

	s := f.connect(false)
	out, text := s.ok("overview", map[string]any{})
	st := out["stale"].(map[string]any)
	items := st["items"].([]any)
	if st["total"].(float64) != 1 || items[0].(map[string]any)["key"] != "disk:a" {
		t.Fatalf("stale: %v", st)
	}
	if !strings.Contains(text, "stale: 1 open") {
		t.Fatalf("text:\n%s", text)
	}
	for _, it := range out["incidents"].(map[string]any)["items"].([]any) {
		m := it.(map[string]any)
		if (m["key"] == "disk:a") != (m["stale"] == true) {
			t.Fatalf("incident stale flag: %v", m)
		}
	}

	out, text = s.ok("digest_preview", map[string]any{})
	if out["level"] != "active" || out["enabled"] != false {
		t.Fatalf("digest: %v", out)
	}
	inc := out["incidents"].(map[string]any)
	if len(inc["stale"].([]any)) != 1 || len(inc["open"].([]any)) != 1 {
		t.Fatalf("digest incidents: %v", inc)
	}
	if !strings.Contains(text, "### Stale") || !strings.Contains(text, "the daily digest is off") {
		t.Fatalf("digest text:\n%s", text)
	}
}

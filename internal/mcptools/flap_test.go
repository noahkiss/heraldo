package mcptools

import (
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/internal/store"
)

func TestOverviewMarksFlapping(t *testing.T) {
	f := newFixture(t)
	f.st.SetFlapPolicy(func() store.FlapPolicy {
		return store.FlapPolicy{Enabled: true, Opens: 2, Window: time.Hour, Settle: time.Hour}
	})
	f.post(`{"source":"s","kind":"incident","key":"web:down","action":"open","title":"web down"}`)
	f.post(`{"source":"s","kind":"incident","key":"web:down","action":"resolve"}`)
	f.post(`{"source":"s","kind":"incident","key":"web:down","action":"open","title":"web down"}`)
	s := f.connect(false)
	out, text := s.ok("overview", map[string]any{"sections": []string{"incidents"}})
	items := out["incidents"].(map[string]any)["items"].([]any)
	it := items[0].(map[string]any)
	if it["flapping"] != true || it["flap_count"] != float64(2) || !strings.Contains(text, "[open, flapping (2 opens)]") {
		t.Fatalf("overview: %v %s", it, text)
	}
}

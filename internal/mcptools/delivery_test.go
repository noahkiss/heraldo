package mcptools

import (
	"strings"
	"testing"
)

func TestDeliveryRead(t *testing.T) {
	f := newFixture(t)
	f.post(`{"source":"printer","kind":"message","title":"done"}`)
	f.do("POST", "/v1/delivery", adminTok, `{"match":{"source":"printer"},"deliver":"silent","note":"prints"}`)
	s := f.connect(false)
	out, text := s.ok("delivery_read", map[string]any{})
	rows, _ := out["settings"].([]any)
	if out["enabled"] != false || len(rows) != 1 || rows[0].(map[string]any)["deliver"] != "silent" ||
		!strings.Contains(text, "source=printer -> silent (prints)") || out["sources"] != nil {
		t.Fatalf("%v\n%s", out, text)
	}
	out, text = s.ok("delivery_read", map[string]any{"sources": true})
	srcs, _ := out["sources"].([]any)
	if len(srcs) != 1 || srcs[0].(map[string]any)["new"] != true || !strings.Contains(text, "source printer (new)") {
		t.Fatalf("%v\n%s", out, text)
	}
	// events_search takes hidden.
	if out, _ := s.ok("events_search", map[string]any{"hidden": "only"}); len(out["events"].([]any)) != 0 {
		t.Fatalf("hidden only: %v", out)
	}
}

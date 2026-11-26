package digest

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestFiledItems(t *testing.T) {
	at := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	ev := func(i int, source, key, title, summary string) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"id":"e%d","source":%q,"key":%q,"title":%q,"summary":%q,"received_at":%q}`,
			i, source, key, title, summary, at.Add(time.Duration(i)*time.Hour).Format(time.RFC3339)))
	}
	items := fileItems([]json.RawMessage{
		ev(1, "printer", "maint:p1", "Maintenance due", "nozzle"),
		ev(2, "printer", "", "Filament low", ""),
		ev(3, "printer", "maint:p1", "Maintenance due", "nozzle, plate"),
		ev(4, "Printer", "", "Filament low", ""),
		ev(5, "printer", "maint:p2", "Maintenance due", "belt"),
	})
	if len(items) != 3 {
		t.Fatalf("%+v", items)
	}
	// Newest first: p2 (5), filament (4), p1 (3).
	if items[0].Key != "maint:p2" || items[1].Title != "Filament low" || items[1].Count != 2 ||
		items[2].Count != 2 || items[2].Summary != "nozzle, plate" || items[2].EventID != "e3" ||
		!items[2].FirstAt.Equal(at.Add(time.Hour)) || !items[2].LastAt.Equal(at.Add(3*time.Hour)) {
		t.Fatalf("%+v", items)
	}
	d := &Digest{Since: at, Until: at.Add(24 * time.Hour), Filed: items}
	body := d.render(time.UTC)
	if !strings.Contains(body, "### Filed\n5 events filed for the digest.") ||
		!strings.Contains(body, "- Maintenance due x2, Sun 09:00 to Sun 11:00: nozzle, plate (printer)") {
		t.Fatal(body)
	}
	f := d.Fields("")
	own := f["details"].(map[string]any)["heraldo"].(map[string]any)["digest"].(map[string]any)
	if own["filed"] != 5 || own["filed_items"] != 3 {
		t.Fatalf("details %+v", own)
	}
	// No filed events: no section, no detail.
	d.Filed = nil
	if strings.Contains(d.render(time.UTC), "Filed") {
		t.Fatal("empty Filed section rendered")
	}
}

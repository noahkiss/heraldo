package push

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/noahkiss/heraldo/internal/store"
)

// An alert to a Mac carries content-available: 1, so a running Mac app gets
// a callback; the same alert to an iPhone does not.
func TestMacAlertIsContentAvailable(t *testing.T) {
	r := newRig(t, 0)
	for _, d := range []struct{ token, env, platform string }{
		{prodToken, "production", "macOS"},
		{sandboxToken, "sandbox", "iOS"},
	} {
		rec := r.do(t, http.MethodPost, "/v1/devices", readTok,
			fmt.Sprintf(`{"token":%q,"environment":%q,"topic":%q,"name":"Test","platform":%q}`, d.token, d.env, topic, d.platform))
		if rec.Code != http.StatusCreated {
			t.Fatalf("register %s: %d %s", d.platform, rec.Code, rec.Body)
		}
	}
	r.post(t, writeTok, `{"source":"uptime","title":"Web is down","level":"active"}`)
	aps := func(f *fakeAPNs) map[string]any {
		t.Helper()
		reqs := f.requests()
		if len(reqs) != 1 {
			t.Fatalf("%d requests; want 1", len(reqs))
		}
		var p struct{ APS map[string]any }
		if err := json.Unmarshal(reqs[0].Body, &p); err != nil {
			t.Fatal(err)
		}
		return p.APS
	}
	mac, phone := aps(r.prod), aps(r.sand)
	if mac["content-available"] != float64(1) {
		t.Fatalf("Mac aps = %v; want content-available 1", mac)
	}
	if _, ok := phone["content-available"]; ok {
		t.Fatalf("iOS aps = %v; want no content-available", phone)
	}
	// The alert itself is the same.
	if mac["alert"].(map[string]any)["title"] != "Web is down" || phone["alert"].(map[string]any)["title"] != "Web is down" {
		t.Fatalf("alerts differ: %v / %v", mac["alert"], phone["alert"])
	}
}

func TestContentAvailableFor(t *testing.T) {
	for platform, want := range map[string]bool{"macOS": true, "macos": true, "iOS": false, "": false} {
		if got := ContentAvailableFor(store.Device{Platform: platform}); got != want {
			t.Errorf("%q: %v; want %v", platform, got, want)
		}
	}
}

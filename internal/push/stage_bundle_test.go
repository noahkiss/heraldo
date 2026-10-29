package push

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func (r *rig) view(t *testing.T, body string) string {
	t.Helper()
	rec := r.do(t, http.MethodPost, "/v1/views", writeTok, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("view: %d %s", rec.Code, rec.Body)
	}
	var v struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	return v.ID
}

type alert struct {
	APS struct {
		Alert struct{ Title, Body string }
		ThreadID          string `json:"thread-id"`
		InterruptionLevel string `json:"interruption-level"`
		Sound             string
	} `json:"aps"`
}

func lastAlert(t *testing.T, f *fakeAPNs) (fakeReq, alert) {
	t.Helper()
	reqs := f.requests()
	if len(reqs) == 0 {
		t.Fatal("no pushes")
	}
	req := reqs[len(reqs)-1]
	var a alert
	if err := json.Unmarshal(req.Body, &a); err != nil {
		t.Fatal(err)
	}
	return req, a
}

func TestBundle(t *testing.T) {
	r := newRig(t, 0)
	r.register(t, prodToken, "production")
	view := r.view(t, `{"name":"Backups","filter":{"source":"backup"},"bundle":{"window":"10m"}}`)

	// The first push in the window goes out on its own.
	first := r.post(t, writeTok, `{"source":"backup","title":"nas failed","key":"b:nas"}`)
	req, a := lastAlert(t, r.prod)
	if a.APS.Alert.Title != "nas failed" || req.CollapseID != CollapseID("b:nas") {
		t.Fatalf("first push %+v %+v", req, a)
	}
	if got := stagesOf(r.why(t, first)); !strings.Contains(got, "bundle:pass deliver:pass") {
		t.Fatalf("first stages %s", got)
	}

	// The second bundles: the view's collapse id, and it alerts.
	r.clock.add(time.Minute)
	second := r.post(t, writeTok, `{"source":"backup","title":"vm failed"}`)
	req, a = lastAlert(t, r.prod)
	if req.CollapseID != CollapseID("view:"+view) || a.APS.Alert.Title != "Backups" ||
		a.APS.Alert.Body != "2 new in Backups: vm failed" || a.APS.InterruptionLevel != "active" || a.APS.Sound == "" {
		t.Fatalf("second push %+v %+v", req, a)
	}
	tr := r.why(t, second)
	var row *struct {
		reason string
		detail map[string]any
	}
	for _, d := range tr.Decisions {
		if d.Stage == "bundle" {
			row = &struct {
				reason string
				detail map[string]any
			}{d.Reason, *d.Detail}
		}
	}
	if row == nil || row.reason != "bundled into view Backups (2 in 1 min)" || row.detail["view"] != view {
		t.Fatalf("bundle row %+v", row)
	}

	// The third replaces the bundle quietly.
	r.clock.add(time.Minute)
	r.post(t, writeTok, `{"source":"backup","title":"pi failed"}`)
	req, a = lastAlert(t, r.prod)
	if req.CollapseID != CollapseID("view:"+view) || a.APS.Alert.Body != "3 new in Backups: pi failed" ||
		a.APS.InterruptionLevel != "passive" || a.APS.Sound != "" {
		t.Fatalf("third push %+v %+v", req, a)
	}

	// Time-sensitive never bundles, and does not count.
	r.post(t, writeTok, `{"source":"backup","title":"all failed","level":"time-sensitive"}`)
	if _, a = lastAlert(t, r.prod); a.APS.Alert.Title != "all failed" {
		t.Fatalf("time-sensitive push %+v", a)
	}

	// Events outside the view push as usual.
	r.post(t, writeTok, `{"source":"web","title":"hello"}`)
	if _, a = lastAlert(t, r.prod); a.APS.Alert.Title != "hello" {
		t.Fatalf("other push %+v", a)
	}

	// After the window, a new one opens.
	r.clock.add(10 * time.Minute)
	r.post(t, writeTok, `{"source":"backup","title":"nas ok again"}`)
	if req, a = lastAlert(t, r.prod); a.APS.Alert.Title != "nas ok again" || req.CollapseID != "" {
		t.Fatalf("new window push %+v %+v", req, a)
	}
}

func TestBundleMaxVisible(t *testing.T) {
	r := newRig(t, 0)
	r.register(t, prodToken, "production")
	r.view(t, `{"name":"CI","filter":{"project":["ci"],"level":["active"]},"bundle":{"window":"5m","max_visible":2}}`)
	for i, want := range []string{"one", "two", "3 new in CI: three"} {
		r.post(t, writeTok, `{"source":"gh","project":"ci","title":"`+[]string{"one", "two", "three"}[i]+`"}`)
		_, a := lastAlert(t, r.prod)
		got := a.APS.Alert.Title
		if i == 2 {
			got = a.APS.Alert.Body
		}
		if got != want {
			t.Fatalf("push %d: %q, want %q", i, got, want)
		}
	}
}

package standby_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/store"
)

// primaryRow adds a delivery setting on the primary.
func (r *rig) primaryRow(t *testing.T, body string) {
	t.Helper()
	if code, b := r.primary.do(t, http.MethodPost, "/v1/delivery", adminTok, body); code != http.StatusCreated {
		t.Fatalf("row: %d %s", code, b)
	}
}

// The round copies the rows, the two scalar keys and the sources' review
// state; the standby then refuses its own row writes.
func TestStandbyCopiesDelivery(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.primary.post(t, `{"source":"printer","title":"done","level":"active"}`)
	r.primaryRow(t, `{"match":{"source":"printer"},"deliver":"silent"}`)
	for _, kv := range [][2]string{{"delivery.enabled", "true"}, {"delivery.new_source_level", `"active"`}} {
		if code, b := r.primary.do(t, http.MethodPut, "/v1/settings/"+kv[0], adminTok, `{"value":`+kv[1]+`}`); code != http.StatusOK {
			t.Fatalf("%s: %d %s", kv[0], code, b)
		}
	}
	if code, b := r.primary.do(t, http.MethodPost, "/v1/sources/printer/review", adminTok, ""); code != http.StatusOK {
		t.Fatalf("review: %d %s", code, b)
	}
	if err := r.tick(t); err != nil {
		t.Fatal(err)
	}
	rows, err := r.standby.st.DeliveryRows(ctx)
	if err != nil || len(rows) != 1 || rows[0].Deliver != "silent" {
		t.Fatalf("standby rows %+v %v", rows, err)
	}
	if n, _ := r.standby.st.SourceNew(ctx, "printer"); n {
		t.Fatal("printer is new on the standby")
	}
	code, b := r.standby.do(t, http.MethodGet, "/v1/delivery", phoneTok, "")
	var list api.DeliverySettingList
	_ = json.Unmarshal(b, &list)
	if code != http.StatusOK || !list.Enabled || list.NewSourceLevel != "active" {
		t.Fatalf("standby delivery: %d %s", code, b)
	}
	// A row removed on the primary goes on the next round.
	if code, b := r.primary.do(t, http.MethodDelete, "/v1/delivery/"+rows[0].ID, adminTok, ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", code, b)
	}
	if err := r.tick(t); err != nil {
		t.Fatal(err)
	}
	if rows, _ := r.standby.st.DeliveryRows(ctx); len(rows) != 0 {
		t.Fatalf("standby kept a removed row: %+v", rows)
	}
}

// A stamped event the standby stored while the primary was down imports
// with its stamp and its columns: nothing rejected, nothing lost.
func TestStampedEventHandsBack(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.primary.post(t, `{"source":"printer","title":"first","level":"active"}`)
	r.primaryRow(t, `{"match":{"source":"printer"},"deliver":"digest","badge":"off"}`)
	if code, b := r.primary.do(t, http.MethodPut, "/v1/settings/delivery.enabled", adminTok, `{"value":true}`); code != http.StatusOK {
		t.Fatalf("enable: %d %s", code, b)
	}
	if code, b := r.primary.do(t, http.MethodPut, "/v1/settings/digest.enabled", adminTok, `{"value":true}`); code != http.StatusOK {
		t.Fatalf("digest: %d %s", code, b)
	}
	if err := r.tick(t); err != nil {
		t.Fatal(err)
	}
	r.primary.down.Store(true)
	// The standby's own digest.enabled is off: digest acts as silent there.
	id := r.standby.post(t, `{"source":"printer","title":"maintenance due","level":"active"}`)
	_, b := r.standby.do(t, http.MethodGet, "/v1/events/"+id, phoneTok, "")
	var onStandby api.Event
	if err := json.Unmarshal(b, &onStandby); err != nil || onStandby.Deliver == nil || onStandby.Deliver.As != "silent" ||
		onStandby.Deliver.Wanted == nil || *onStandby.Deliver.Wanted != "digest" {
		t.Fatalf("standby stamp: %s", b)
	}
	if got := r.standby.apns.pushes(); len(got) != 0 {
		t.Fatalf("the standby pushed a silenced event: %v", got)
	}
	_ = r.tick(t)
	r.primary.down.Store(false)
	if err := r.tick(t); err != nil {
		t.Fatal(err)
	}
	code, b := r.primary.do(t, http.MethodGet, "/v1/events/"+id, phoneTok, "")
	if code != http.StatusOK {
		t.Fatalf("the primary lost the stamped event: %d %s", code, b)
	}
	var ev api.Event
	if err := json.Unmarshal(b, &ev); err != nil || ev.Deliver == nil || ev.Deliver.As != "silent" ||
		ev.Deliver.Badge != "off" || ev.Level == nil || *ev.Level != "passive" {
		t.Fatalf("primary stamp: %s", b)
	}
	// The columns came from the stamp: the event is badge off and shown.
	recs, err := r.primary.st.List(ctx, store.Filter{Hidden: store.HiddenExclude, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rec := range recs {
		found = found || rec.ID == id
	}
	if !found {
		t.Fatal("the imported silent event is hidden")
	}
	n, err := r.primary.st.Badge(ctx, store.BadgePushed, nil, 0)
	if err != nil || n != 1 {
		t.Fatalf("pushed badge on the primary: %d %v; want 1 (the first post only)", n, err)
	}
	// The import pushed nothing.
	r.primary.p.Wait()
	if got := r.primary.apns.pushes(); len(got) != 1 {
		t.Fatalf("primary pushes %v; want only the first post", got)
	}
}

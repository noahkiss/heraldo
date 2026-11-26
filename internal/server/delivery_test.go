package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/rules"
	"github.com/noahkiss/heraldo/internal/store"
)

// enableDelivery turns delivery.enabled on, as PUT /v1/settings does.
func (e *env) enableDelivery(t *testing.T) {
	t.Helper()
	rec := e.do(t, http.MethodPut, "/v1/settings/delivery.enabled", adminTok, []byte(`{"value":true}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body)
	}
}

// addRow adds a delivery setting with the admin token.
func (e *env) addRow(t *testing.T, body string) api.DeliverySetting {
	t.Helper()
	rec := e.do(t, http.MethodPost, "/v1/delivery", adminTok, []byte(body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("add row %s: %d %s", body, rec.Code, rec.Body)
	}
	return decode[api.DeliverySetting](t, rec)
}

func TestDeliveryCRUD(t *testing.T) {
	e := newEnv(t)
	if rec := e.do(t, http.MethodPost, "/v1/delivery", writeTok, []byte(`{"deliver":"silent"}`)); rec.Code != http.StatusForbidden {
		t.Fatalf("write token: %d", rec.Code)
	}
	for _, c := range []struct{ body, want string }{
		{`{}`, "does nothing"},
		{`{"deliver":"loud"}`, "is not banner, silent, digest or drop"},
		{`{"deliver":"digest","badge":"on"}`, "hides"},
		{`{"default_level":"passive","match":{"level":["active"]}}`, "may not match on level"},
		{`{"default_level":"critical"}`, "want passive or active"},
		{`{"deliver":"silent","match":{"kind":"progress"}}`, "routine"},
		{`{"deliver":"silent","colour":"red"}`, "unknown field"},
	} {
		rec := e.do(t, http.MethodPost, "/v1/delivery", adminTok, []byte(c.body))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s: %d %s, want 400 %q", c.body, rec.Code, rec.Body, c.want)
		}
	}
	a := e.addRow(t, `{"match":{"source":"Printer","tag":"x"},"deliver":"silent","note":"prints","position":2}`)
	b := e.addRow(t, `{"match":{"source":"tests"},"default_level":"passive","position":1}`)
	if !strings.HasPrefix(a.Id, "dlv_") || a.Badge != "auto" || *a.Deliver != "silent" || *a.UpdatedBy != "ops" {
		t.Fatalf("%+v", a)
	}
	list := decode[api.DeliverySettingList](t, e.do(t, http.MethodGet, "/v1/delivery", readTok, nil))
	if list.Enabled || list.NewSourceLevel != "passive" || len(list.Settings) != 2 || list.Settings[0].Id != b.Id {
		t.Fatalf("%+v", list)
	}
	rec := e.do(t, http.MethodPut, "/v1/delivery/"+a.Id, adminTok, []byte(`{"match":{"source":"printer"},"deliver":"drop"}`))
	if rec.Code != http.StatusOK || *decode[api.DeliverySetting](t, rec).Deliver != "drop" {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	if got := decode[api.DeliverySetting](t, e.do(t, http.MethodGet, "/v1/delivery/"+a.Id, readTok, nil)); got.CreatedAt != a.CreatedAt || got.Note != nil {
		t.Fatalf("get after put: %+v", got)
	}
	if rec := e.do(t, http.MethodDelete, "/v1/delivery/"+a.Id, adminTok, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		if rec := e.do(t, m, "/v1/delivery/"+a.Id, adminTok, []byte(`{"deliver":"silent"}`)); rec.Code != http.StatusNotFound {
			t.Errorf("%s gone row: %d", m, rec.Code)
		}
	}
}

func TestDeliveryRowChangeBumps(t *testing.T) {
	e := newEnv(t)
	_, before := e.revs(t)
	e.addRow(t, `{"deliver":"silent"}`)
	_, after := e.revs(t)
	for _, d := range []string{"delivery", "cursor", "state"} {
		if after[d] <= before[d] {
			t.Errorf("%s not bumped: %d -> %d", d, before[d], after[d])
		}
	}
}

// Every ingest path stamps: POST, GET /v1/send, a ping; the stored level
// column follows the stamp, so a silenced event is not unread.
func TestDeliveryAtIngest(t *testing.T) {
	rw, err := rules.NewWatcher("../rules/testdata/rules.yaml", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	e := newEnvRules(t, rw)
	ctx := context.Background()
	// Off: no stamp, whatever the rows say.
	e.addRow(t, `{"match":{"source":"printer"},"deliver":"silent"}`)
	if ev := decode[api.Event](t, e.post(t, `{"source":"printer","title":"done","level":"active"}`)); ev.Deliver != nil || *ev.Level != "active" {
		t.Fatalf("off: stamped %+v", ev.Deliver)
	}
	e.enableDelivery(t)
	e.addRow(t, `{"match":{"rule":"quiet-message"},"deliver":"drop"}`)
	e.addRow(t, `{"match":{"key":"checkin:"},"badge":"off"}`)
	e.addRow(t, `{"match":{"source":"jobs"},"deliver":"silent"}`)

	head, _ := e.st.Head(ctx)
	rec := e.post(t, `{"source":"printer","title":"done","level":"active"}`)
	ev := decode[api.Event](t, rec)
	if ev.Deliver == nil || ev.Deliver.As != "silent" || *ev.Level != "passive" || *ev.Deliver.LevelWas != "active" {
		t.Fatalf("post: %s", rec.Body)
	}
	checkStored(t, storedSchema(t), rec.Body.Bytes())
	if n, _ := e.st.UnreadAfter(ctx, head); n != 0 {
		t.Fatalf("a silent event counts as unread: %d", n)
	}
	// The rule's name comes from rules.Type, not the sender's details.
	rec = e.do(t, http.MethodGet, "/v1/send?source=s&title=t&tags=quiet", writeTok, nil)
	if ev := decode[api.Event](t, rec); ev.Deliver == nil || ev.Deliver.As != "drop" {
		t.Fatalf("send: %s", rec.Body)
	}
	forged := e.post(t, `{"source":"s","title":"t","details":{"heraldo":{"rule":"quiet-message"}}}`)
	if ev := decode[api.Event](t, forged); ev.Deliver != nil && ev.Deliver.As == "drop" {
		t.Fatalf("a forged rule name matched: %s", forged.Body)
	}
	// A failed ping: the floor keeps its level; the badge row still applies.
	if rec := e.do(t, http.MethodPut, "/v1/checkins/backup", writeTok, []byte(`{"period_s":3600}`)); rec.Code/100 != 2 {
		t.Fatalf("check: %d %s", rec.Code, rec.Body)
	}
	rec = e.do(t, http.MethodPost, "/ping/backup/fail", writeTok, nil)
	if rec.Code/100 != 2 {
		t.Fatalf("ping: %d %s", rec.Code, rec.Body)
	}
	recs, _ := e.st.List(ctx, store.Filter{Kind: "checkin", Limit: 100})
	var ping api.Event
	if err := json.Unmarshal(recs[len(recs)-1].Event, &ping); err != nil {
		t.Fatal(err)
	}
	if ping.Deliver != nil {
		t.Fatalf("a check-in with no matching row stamped: %+v", ping.Deliver)
	}
	// An input carrying deliver is refused.
	if rec := e.post(t, `{"source":"s","deliver":{"as":"banner","badge":"auto"}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("input with deliver: %d", rec.Code)
	}
	// The hidden filter.
	all := decode[page](t, e.do(t, http.MethodGet, "/v1/events?hidden=only", readTok, nil))
	if len(all.Events) != 1 {
		t.Fatalf("hidden=only: %d events", len(all.Events))
	}
	if rec := e.do(t, http.MethodGet, "/v1/events?hidden=maybe", readTok, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("hidden=maybe: %d", rec.Code)
	}
}

// A row change re-stamps the badge of unread events and open incidents.
func TestDeliveryRebadge(t *testing.T) {
	e := newEnv(t)
	e.enableDelivery(t)
	if rec := e.do(t, http.MethodPut, "/v1/settings/badge.scope", adminTok, []byte(`{"value":"attention"}`)); rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	e.post(t, `{"source":"uptime","kind":"incident","key":"web","action":"open","level":"time-sensitive","title":"down"}`)
	e.post(t, `{"source":"news","title":"hi","level":"active"}`)
	badge := func() int {
		st := decode[struct {
			Badge int `json:"badge"`
		}](t, e.do(t, http.MethodGet, "/v1/state", readTok, nil))
		return st.Badge
	}
	if n := badge(); n != 1 {
		t.Fatalf("badge %d, want 1 (the incident)", n)
	}
	row := e.addRow(t, `{"match":{"source":"uptime"},"badge":"off"}`)
	if n := badge(); n != 0 {
		t.Fatalf("after badge off: %d, want 0", n)
	}
	e.addRow(t, `{"match":{"source":"news"},"badge":"on"}`)
	// No cursor yet: the re-stamp covers only incidents, and an event with
	// no read cursor is not news.
	if n := badge(); n != 0 {
		t.Fatalf("after badge on without a cursor: %d", n)
	}
	if rec := e.do(t, http.MethodDelete, "/v1/delivery/"+row.Id, adminTok, nil); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}
	if n := badge(); n != 1 {
		t.Fatalf("after delete: %d, want 1", n)
	}
	// Off again: the stamps do nothing.
	if rec := e.do(t, http.MethodPut, "/v1/settings/delivery.enabled", adminTok, []byte(`{"value":false}`)); rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	if n := badge(); n != 1 {
		t.Fatalf("off: %d, want 1", n)
	}
}

// Rows and stamps written while delivery.enabled is off take effect on the
// badge as soon as it turns on, with no row change.
func TestDeliveryRebadgeOnTurnOn(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.h.(*Server).RunDeliveryWatch(ctx)
	if rec := e.do(t, http.MethodPut, "/v1/settings/badge.scope", adminTok, []byte(`{"value":"attention"}`)); rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	// The row exists first; the incident arrives while the switch is off,
	// so ingest stamps nothing and only the turn-on re-stamp can reach it.
	e.addRow(t, `{"match":{"source":"uptime"},"badge":"off"}`)
	e.post(t, `{"source":"uptime","kind":"incident","key":"web","action":"open","level":"time-sensitive","title":"down"}`)
	badge := func() int {
		return decode[struct {
			Badge int `json:"badge"`
		}](t, e.do(t, http.MethodGet, "/v1/state", readTok, nil)).Badge
	}
	if n := badge(); n != 1 {
		t.Fatalf("off: badge %d, want 1", n)
	}
	e.enableDelivery(t)
	deadline := time.Now().Add(5 * time.Second)
	for badge() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("badge still counts the incident after turn-on")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDeliveryPreviewAndSources(t *testing.T) {
	e := newEnv(t)
	e.post(t, `{"source":"printer","title":"done","level":"active"}`)
	e.post(t, `{"source":"printer","title":"done","level":"active"}`)
	e.post(t, `{"source":"uptime","kind":"incident","key":"w","action":"open","level":"time-sensitive","title":"down"}`)
	rec := e.do(t, http.MethodPost, "/v1/delivery/preview", readTok, []byte(`{"settings":[
		{"match":{"source":"printer"},"deliver":"drop"},
		{"match":{"source":"uptime"},"deliver":"silent","badge":"off"}]}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body)
	}
	p := decode[api.DeliveryPreview](t, rec)
	if p.Events != 3 || p.Outcomes.Drop != 2 || p.Outcomes.Banner != 1 || p.Rows[0].Hits != 2 || p.Rows[1].Refused != 1 ||
		p.BadgeLeft != 1 || len(p.Changed) != 3 || p.Days != 30 {
		t.Fatalf("%+v", p)
	}
	if rec := e.do(t, http.MethodPost, "/v1/delivery/preview", readTok, []byte(`{"settings":[{}]}`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad row: %d", rec.Code)
	}
	srcs := decode[api.SourceList](t, e.do(t, http.MethodGet, "/v1/sources", readTok, nil))
	if len(srcs.Sources) != 2 || !srcs.Sources[0].New || srcs.Sources[0].Source != "printer" || srcs.Sources[0].Events != 2 ||
		srcs.Sources[1].BadgeIn != 1 {
		t.Fatalf("%+v", srcs)
	}
	if rec := e.do(t, http.MethodPost, "/v1/sources/Printer/review", writeTok, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("write review: %d", rec.Code)
	}
	rec = e.do(t, http.MethodPost, "/v1/sources/Printer/review", adminTok, nil)
	if rec.Code != http.StatusOK || decode[api.Source](t, rec).New {
		t.Fatalf("review: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do(t, http.MethodPost, "/v1/sources/nobody/review", adminTok, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("review unknown: %d", rec.Code)
	}
}

func TestDeliveryOnStandby(t *testing.T) {
	e := newEnvWith(t, func(o *Options) { o.Standby = true })
	e.post(t, `{"source":"s","title":"x"}`)
	for _, c := range []struct{ method, target string }{
		{http.MethodPost, "/v1/delivery"},
		{http.MethodPut, "/v1/delivery/dlv_x"},
		{http.MethodDelete, "/v1/delivery/dlv_x"},
		{http.MethodPost, "/v1/sources/s/review"},
	} {
		if rec := e.do(t, c.method, c.target, adminTok, []byte(`{"deliver":"silent"}`)); rec.Code != http.StatusConflict {
			t.Errorf("%s %s: %d", c.method, c.target, rec.Code)
		}
	}
	if rec := e.do(t, http.MethodGet, "/v1/standby/delivery", readTok, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("plain read token: %d", rec.Code)
	}
	rec := e.do(t, http.MethodGet, "/v1/standby/delivery", adminTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("export: %d", rec.Code)
	}
	if d := decode[api.StandbyDelivery](t, rec); len(d.Sources) != 1 || d.Enabled {
		t.Fatalf("%+v", d)
	}
}

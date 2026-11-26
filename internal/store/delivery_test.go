package store

import (
	"context"
	"encoding/json"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/checkin"
	"github.com/noahkiss/heraldo/internal/delivery"
)

// migrationsUpTo is the embedded migrations numbered n and below.
func migrationsUpTo(t *testing.T, n int) fs.FS {
	t.Helper()
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	ms, err := loadMigrations(sub)
	if err != nil {
		t.Fatal(err)
	}
	out := fstest.MapFS{}
	entries, _ := fs.ReadDir(sub, ".")
	for _, e := range entries {
		var num int
		for _, m := range ms {
			if m.name == e.Name() {
				num = m.version
			}
		}
		if num == 0 || num > n {
			continue
		}
		b, err := fs.ReadFile(sub, e.Name())
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = &fstest.MapFile{Data: b}
	}
	return out
}

// The migration seeds sources from the stored events, case folded and
// reviewed, so at cut-over no source is new.
func TestDeliveryMigrationSeedsSources(t *testing.T) {
	ctx := context.Background()
	s := rawStore(t)
	if err := s.migrateFS(ctx, migrationsUpTo(t, 21)); err != nil {
		t.Fatal(err)
	}
	for i, src := range []string{"Printer", "printer", "backup"} {
		at := time.Date(2026, 10, 1+i, 0, 0, 0, 0, time.UTC).Format(timeFormat)
		if _, err := s.db.ExecContext(ctx, `INSERT INTO events (id, received_at, ts, source, kind, event)
			VALUES (?, ?, ?, ?, 'message', '{}')`, "e"+string(rune('a'+i)), at, at, src); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	srcs, err := s.Sources(ctx)
	if err != nil || len(srcs) != 2 {
		t.Fatalf("sources %+v, %v", srcs, err)
	}
	for _, x := range srcs {
		if x.New() {
			t.Errorf("%s is new after the seed", x.Source)
		}
	}
	if srcs[1].Source != "printer" || !srcs[1].FirstSeen.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) ||
		!srcs[1].LastSeen.Equal(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("printer: %+v", srcs[1])
	}
}

// stamped is a message from source at level with a deliver stamp.
func stamped(id, source string, level api.Level, as delivery.Outcome, badge delivery.Badge, at time.Time) NewEvent {
	ev := event(id, at)
	ev.Fields["source"] = source
	ev.Typed.Source = source
	if level != "" {
		ev.Fields["level"] = string(level)
		l := level
		ev.Typed.Level = &l
	}
	if as != "" {
		ev.Fields["deliver"] = &api.DeliverStamp{As: api.DeliverStampAs(as), Badge: api.DeliverStampBadge(badge)}
	}
	return ev
}

func TestStampColumnsHiddenAndSources(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "h.db"))
	defer s.Close()
	var domains []Domain
	s.OnChange(func(ds ...Domain) { domains = append(domains, ds...) })
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	evs := []NewEvent{
		stamped("01K3ZX0000AAAAAAAAAAAAAA01", "printer", api.LevelPassive, delivery.Silent, delivery.BadgeAuto, at),
		stamped("01K3ZX0000AAAAAAAAAAAAAA02", "printer", api.LevelPassive, delivery.Digest, delivery.BadgeOff, at),
		stamped("01K3ZX0000AAAAAAAAAAAAAA03", "Printer", api.LevelPassive, delivery.Drop, delivery.BadgeAuto, at),
		stamped("01K3ZX0000AAAAAAAAAAAAAA04", "backup", api.LevelActive, "", "", at),
	}
	for i, ev := range evs {
		domains = nil
		if _, _, err := s.Insert(ctx, ev); err != nil {
			t.Fatal(err)
		}
		first := strings.Contains(strings.Join(func() []string {
			var o []string
			for _, d := range domains {
				o = append(o, string(d))
			}
			return o
		}(), ","), "delivery")
		if want := i == 0 || i == 3; first != want {
			t.Errorf("event %d: delivery domain bumped %v, want %v", i, first, want)
		}
	}
	// An import keeps its stamp in the columns too.
	imp := stamped("01K3ZX0000AAAAAAAAAAAAAA05", "standby-src", api.LevelPassive, "", "", at)
	imp.Fields["deliver"] = map[string]any{"as": "digest", "badge": "on"}
	if ok, err := s.Import(ctx, "standby", imp.ID, imp); err != nil || !ok {
		t.Fatal(ok, err)
	}
	var as, badge string
	if err := s.db.QueryRowContext(ctx, `SELECT deliver_as, badge FROM events WHERE id = ?`, imp.ID).Scan(&as, &badge); err != nil ||
		as != "digest" || badge != "on" {
		t.Fatalf("import columns %q %q, %v", as, badge, err)
	}
	count := func(hidden string) int {
		recs, err := s.List(ctx, Filter{Hidden: hidden, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		return len(recs)
	}
	if a, x, o := count(""), count(HiddenExclude), count(HiddenOnly); a != 5 || x != 2 || o != 3 {
		t.Fatalf("all %d exclude %d only %d; want 5 2 3", a, x, o)
	}
	vf := ViewFilter{Hidden: HiddenOnly}.Filter(nil)
	if recs, _ := s.List(ctx, vf); len(recs) != 3 {
		t.Fatalf("view hidden only: %d", len(recs))
	}
	srcs, err := s.Sources(ctx)
	if err != nil || len(srcs) != 3 {
		t.Fatalf("sources %+v %v", srcs, err)
	}
	if n, _ := s.SourceNew(ctx, "PRINTER"); !n {
		t.Error("printer not new")
	}
	if _, err := s.ReviewSource(ctx, "Printer", at); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.SourceNew(ctx, "printer"); n {
		t.Error("printer new after review")
	}
	if n, _ := s.SourceNew(ctx, "never"); !n {
		t.Error("an unseen source is not new")
	}
	if _, err := s.ReviewSource(ctx, "never", at); err != ErrNotFound {
		t.Errorf("review of an unseen source: %v", err)
	}
}

// A silent event is passive in the level column, so the unread count and
// the pushed badge leave it out; badge on puts it back in the badge only
// while delivery settings are on.
func TestBadgeColumns(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "h.db"))
	defer s.Close()
	on := false
	s.SetDeliveryOn(func() bool { return on })
	at := time.Now().UTC()
	for _, ev := range []NewEvent{
		stamped("01K3ZX0000AAAAAAAAAAAAAB01", "a", api.LevelPassive, delivery.Silent, delivery.BadgeAuto, at),
		stamped("01K3ZX0000AAAAAAAAAAAAAB02", "a", api.LevelPassive, delivery.Silent, delivery.BadgeOn, at),
		stamped("01K3ZX0000AAAAAAAAAAAAAB03", "a", api.LevelActive, delivery.Banner, delivery.BadgeOff, at),
		stamped("01K3ZX0000AAAAAAAAAAAAAB04", "a", api.LevelTimeSensitive, delivery.Banner, delivery.BadgeOff, at),
		stamped("01K3ZX0000AAAAAAAAAAAAAB05", "a", api.LevelActive, "", "", at),
	} {
		if _, _, err := s.Insert(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := s.UnreadAfter(ctx, 0); n != 3 {
		t.Fatalf("unread = %d, want 3 (the active and time-sensitive ones)", n)
	}
	for _, c := range []struct {
		on    bool
		scope string
		want  int
	}{
		{false, BadgePushed, 3},    // today: the three that push
		{true, BadgePushed, 2},     // on: B02 joins, B03 and B04 leave
		{false, BadgeAttention, 1}, // today: B04
		{true, BadgeAttention, 1},  // on: B02 joins, B04 leaves
		{true, BadgeNone, 0},
	} {
		on = c.on
		if n, err := s.Badge(ctx, c.scope, nil, 0); err != nil || n != c.want {
			t.Errorf("on=%v %s: badge %d, %v; want %d", c.on, c.scope, n, err, c.want)
		}
	}
}

func TestBadgeIncidentsAndChecks(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "h.db"))
	defer s.Close()
	on := true
	s.SetDeliveryOn(func() bool { return on })
	if _, err := s.PutCheck(ctx, "backup", "", checkin.Schedule{}); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	inc := func(id, key string, badge delivery.Badge) NewEvent {
		ev := stamped(id, "uptime", api.LevelTimeSensitive, delivery.Banner, badge, at)
		k, a := api.KindIncident, api.EventAction("open")
		ev.Fields["kind"], ev.Fields["key"], ev.Fields["action"] = "incident", key, "open"
		ev.Typed.Kind, ev.Typed.Key, ev.Typed.Action = &k, &key, &a
		return ev
	}
	for _, ev := range []NewEvent{
		inc("01K3ZX0000AAAAAAAAAAAAAC01", "web", delivery.BadgeOff),
		inc("01K3ZX0000AAAAAAAAAAAAAC02", "db", delivery.BadgeAuto),
		inc("01K3ZX0000AAAAAAAAAAAAAC03", "checkin:backup", delivery.BadgeOff),
	} {
		if _, _, err := s.Insert(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE checkins SET state = 'down'`); err != nil {
		t.Fatal(err)
	}
	head, _ := s.Head(ctx)
	if n, _ := s.Badge(ctx, BadgeAttention, nil, head); n != 1 {
		t.Fatalf("on: badge %d, want 1 (db)", n)
	}
	// The check's incident resolves; the down check stays off.
	if _, err := s.db.ExecContext(ctx, `UPDATE incidents SET state = 'resolved' WHERE key = 'checkin:backup'`); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Badge(ctx, BadgeAttention, nil, head); n != 1 {
		t.Fatalf("down check with its incident off: badge %d, want 1", n)
	}
	on = false
	if n, _ := s.Badge(ctx, BadgeAttention, nil, head); n != 3 {
		t.Fatalf("off: badge %d, want 3 (web, db, the down check)", n)
	}
	on = true
	// A re-stamp turns web back on and leaves db alone.
	n, err := s.Rebadge(ctx, 0, func(ev *api.Event) string {
		if ev.Key != nil && *ev.Key == "web" {
			return "auto"
		}
		return ""
	})
	if err != nil || n != 2 {
		t.Fatalf("rebadge changed %d, %v; want 2 (the event and the incident)", n, err)
	}
	if n, _ := s.Badge(ctx, BadgeAttention, nil, head); n != 2 {
		t.Fatalf("after the re-stamp: badge %d, want 2", n)
	}
	rec, _ := s.Get(ctx, "01K3ZX0000AAAAAAAAAAAAAC01")
	var ev api.Event
	if err := json.Unmarshal(rec.Event, &ev); err != nil || ev.Deliver == nil || ev.Deliver.Badge != "auto" {
		t.Fatalf("stored stamp after the re-stamp: %s", rec.Event)
	}
}

// The digest's Filed list skips a held or store-only original: its
// forward, stamped too, stands for it.
func TestFiledEventsSkipHeld(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "h.db"))
	defer s.Close()
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	held := stamped("01K3ZX0000AAAAAAAAAAAAAD01", "p", api.LevelPassive, delivery.Digest, delivery.BadgeAuto, at)
	held.Fields["hold"] = map[string]any{"state": "held", "forwarded": false}
	only := stamped("01K3ZX0000AAAAAAAAAAAAAD02", "p", api.LevelPassive, delivery.Digest, delivery.BadgeAuto, at)
	only.Fields["hold"] = map[string]any{"store_only": true}
	fwd := stamped("01K3ZX0000AAAAAAAAAAAAAD03", "p", api.LevelPassive, delivery.Digest, delivery.BadgeAuto, at)
	fwd.Fields["hold"] = map[string]any{"state": "forward"}
	silent := stamped("01K3ZX0000AAAAAAAAAAAAAD04", "p", api.LevelPassive, delivery.Silent, delivery.BadgeAuto, at)
	for _, ev := range []NewEvent{held, only, fwd, silent} {
		if _, _, err := s.Insert(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	evs, err := s.FiledEvents(ctx, at.Add(-time.Hour), at.Add(time.Hour))
	if err != nil || len(evs) != 1 || !strings.Contains(string(evs[0]), "AD03") {
		t.Fatalf("%d %v %s", len(evs), err, evs)
	}
}

func TestDeliveryRowsCRUD(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "h.db"))
	defer s.Close()
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	a := delivery.Row{ID: "dlv_a", Position: 2, Match: delivery.Match{Source: "x", Level: []api.Level{"active"}},
		Deliver: delivery.Silent, Badge: delivery.BadgeOff, Note: "n", CreatedAt: at, UpdatedAt: at, UpdatedBy: "admin"}
	b := delivery.Row{ID: "dlv_b", Position: 1, DefaultLevel: api.LevelPassive, CreatedAt: at, UpdatedAt: at}
	for _, r := range []delivery.Row{a, b} {
		if err := s.AddDelivery(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.DeliveryRows(ctx)
	if err != nil || len(rows) != 2 || rows[0].ID != "dlv_b" || rows[1].Match.Level[0] != "active" ||
		rows[1].Badge != delivery.BadgeOff || rows[0].Badge != delivery.BadgeAuto {
		t.Fatalf("%+v %v", rows, err)
	}
	a.Deliver = delivery.Drop
	if err := s.PutDelivery(ctx, a); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetDelivery(ctx, "dlv_a"); got.Deliver != delivery.Drop {
		t.Fatalf("%+v", got)
	}
	if changed, err := s.MirrorDelivery(ctx, rows[:1]); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if changed, _ := s.MirrorDelivery(ctx, rows[:1]); changed {
		t.Fatal("an equal mirror changed")
	}
	if err := s.DeleteDelivery(ctx, "dlv_b"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDelivery(ctx, "dlv_b"); err != ErrNotFound {
		t.Fatal(err)
	}
}

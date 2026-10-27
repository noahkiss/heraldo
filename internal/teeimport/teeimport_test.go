package teeimport

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/noahkiss/heraldo/internal/ids"
	"github.com/noahkiss/heraldo/internal/store"
	"github.com/noahkiss/heraldo/internal/validate"
)

const fixture = "testdata/catalogue"

func setup(t *testing.T) (*store.Store, *validate.Validator) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	v, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	return st, v
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestImportFixture(t *testing.T) {
	ctx := context.Background()
	st, v := setup(t)
	s, err := Run(ctx, fixture, st, v, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if s != (Stats{Files: 1, Lines: 4, Imported: 3, Bad: 1}) {
		t.Fatalf("first run: %+v", s)
	}
	s, err = Run(ctx, fixture, st, v, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if s != (Stats{Files: 1, Lines: 4, Skipped: 3, Bad: 1}) {
		t.Fatalf("rerun: %+v", s)
	}

	c := jsonschema.NewCompiler()
	c.AssertFormat()
	stored, err := c.Compile("../../schema/envelope.v1.schema.json#/$defs/Event")
	if err != nil {
		t.Fatal(err)
	}
	recs, err := st.List(ctx, store.Filter{Limit: 10})
	if err != nil || len(recs) != 3 {
		t.Fatalf("list: %d, %v", len(recs), err)
	}
	evs := make([]map[string]any, len(recs))
	for i, r := range recs {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(r.Event))
		if err != nil {
			t.Fatal(err)
		}
		if err := stored.Validate(doc); err != nil {
			t.Fatalf("event %d fails $defs/Event: %v\n%s", i, err, r.Event)
		}
		if err := json.Unmarshal(r.Event, &evs[i]); err != nil {
			t.Fatal(err)
		}
		if !ids.Valid(r.ID) {
			t.Fatalf("id %q is not a ULID", r.ID)
		}
	}

	routed, held, legacy := evs[0], evs[1], evs[2]
	tee := func(e map[string]any) map[string]any { return e["details"].(map[string]any)["tee"].(map[string]any) }

	if routed["source"] != "backup" || routed["kind"] != "message" || routed["url"] != "https://example.org/backups/42" ||
		routed["ts"] != "2026-09-01T08:00:00.125Z" || routed["received_at"] != routed["ts"] {
		t.Errorf("routed: %v", routed)
	}
	if routed["dedup"] != "n-20260901-080000-a1b2c3" || tee(routed)["id"] != "n-20260901-080000-a1b2c3" || tee(routed)["level"] != "quiet" {
		t.Errorf("routed tee details: %v", tee(routed))
	}
	if _, has := routed["level"]; has {
		t.Error("a tee rung became an envelope level")
	}
	if tags := routed["tags"].([]any); len(tags) != 1 || tags[0] != "quiet" {
		t.Errorf("routed tags: %v", tags)
	}
	if _, has := routed["hold"]; has {
		t.Error("routed entry has a hold")
	}

	hold, ok := held["hold"].(map[string]any)
	if !ok || hold["state"] != "held" || hold["id"] != "h-20260901-090500-0a0b0c" || hold["store_only"] != true {
		t.Errorf("held: hold = %v", held["hold"])
	}

	if legacy["source"] != DefaultSource {
		t.Errorf("legacy source = %v", legacy["source"])
	}
	if _, has := legacy["title"]; has {
		t.Error("a null title was stored")
	}
	if tags := legacy["tags"].([]any); len(tags) != 2 || tags[0] != "general" || tags[1] != "ops" {
		t.Errorf("legacy tags: %v", tags)
	}

	// Ids follow the tee time, so they sort like the catalogue.
	if !(recs[0].ID < recs[1].ID && recs[1].ID < recs[2].ID) {
		t.Errorf("ids out of order: %s %s %s", recs[0].ID, recs[1].ID, recs[2].ID)
	}
}

func TestDerivedIDIsStable(t *testing.T) {
	_, v := setup(t)
	line := []byte(`{"id":"n-20260901-080000-a1b2c3","ts":1788249600.125,"title":"x"}`)
	a, _, err := Map(line, v)
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := Map(line, v)
	if a.ID != b.ID {
		t.Fatalf("%s != %s", a.ID, b.ID)
	}
	// A tee id that is already a ULID is kept.
	u, _, err := Map([]byte(`{"id":"01K3ZX0000AAAAAAAAAAAAAAAA","ts":1788249600}`), v)
	if err != nil || u.ID != "01K3ZX0000AAAAAAAAAAAAAAAA" {
		t.Fatalf("ulid tee id: %s, %v", u.ID, err)
	}
}

func TestMapRejects(t *testing.T) {
	_, v := setup(t)
	for _, bad := range []string{`not json`, `{"ts":1}`, `{"id":"n-1"}`, `{"id":"n-1","ts":-5}`} {
		if _, _, err := Map([]byte(bad), v); err == nil {
			t.Errorf("mapped %s", bad)
		}
	}
}

func TestMapCutsLongFields(t *testing.T) {
	_, v := setup(t)
	long := make([]byte, 300)
	for i := range long {
		long[i] = 'a'
	}
	line, _ := json.Marshal(map[string]any{"id": "n-x", "ts": 1788249600, "title": string(long), "source": string(long)})
	ev, _, err := Map(line, v)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.Fields["title"].(string)) != 256 || len(ev.Fields["source"].(string)) != 128 {
		t.Fatal("long fields not cut")
	}
}

func TestRunEmptyDir(t *testing.T) {
	st, v := setup(t)
	if _, err := Run(context.Background(), t.TempDir(), st, v, quiet); err == nil {
		t.Fatal("want an error for a directory without catalogue files")
	}
}

package api

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const schemaPath = "../schema/envelope.v1.schema.json"

func compile(t *testing.T, ref string) *jsonschema.Schema {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	s, err := c.Compile(ref)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// input is what a sender posts (the schema root); stored is what the read
// API returns ($defs/Event).
func input(t *testing.T) *jsonschema.Schema  { return compile(t, schemaPath) }
func stored(t *testing.T) *jsonschema.Schema { return compile(t, schemaPath+"#/$defs/Event") }

func examples(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("../schema/examples", dir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no examples in %s: %v", dir, err)
	}
	return files
}

func load(t *testing.T, path string) any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	v, err := jsonschema.UnmarshalJSON(f)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// decodeStrict decodes path into v and fails on a field the Go type lacks,
// so the schema and the generated types agree.
func decodeStrict(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// Every valid example passes the input schema and decodes into EventInput.
func TestValidExamples(t *testing.T) {
	s := input(t)
	for _, path := range examples(t, "valid") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			if err := s.Validate(load(t, path)); err != nil {
				t.Fatalf("schema: %v", err)
			}
			var e EventInput
			decodeStrict(t, path, &e)
			if e.Kind != nil && !e.Kind.Valid() {
				t.Fatalf("kind %q not valid", *e.Kind)
			}
		})
	}
}

func TestInvalidExamples(t *testing.T) {
	s := input(t)
	for _, path := range examples(t, "invalid") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			if err := s.Validate(load(t, path)); err == nil {
				t.Fatal("schema accepted an invalid envelope")
			}
		})
	}
}

// Every stored example passes $defs/Event, decodes into Event, and is
// refused as input because it carries server-owned fields.
func TestStoredExamples(t *testing.T) {
	in, out := input(t), stored(t)
	for _, path := range examples(t, "stored") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			v := load(t, path)
			if err := out.Validate(v); err != nil {
				t.Fatalf("schema: %v", err)
			}
			if err := in.Validate(v); err == nil {
				t.Fatal("input schema accepted a stored event")
			}
			var e Event
			decodeStrict(t, path, &e)
			if e.Id == "" || e.ReceivedAt.IsZero() {
				t.Fatal("id or received_at missing after decode")
			}
		})
	}
}

// A stored event needs the server-owned id and received_at.
func TestStoredNeedsServerFields(t *testing.T) {
	s := stored(t)
	v, err := jsonschema.UnmarshalJSON(strings.NewReader(
		`{"ts":"2026-10-26T12:00:00Z","source":"backup","kind":"message"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(v); err == nil {
		t.Fatal("stored schema accepted an event without id and received_at")
	}
}

// An input may leave kind out; a stored event always has one.
func TestStoredNeedsKind(t *testing.T) {
	v, err := jsonschema.UnmarshalJSON(strings.NewReader(
		`{"id":"01JBA6Z0000000000000000002","received_at":"2026-10-26T12:00:00Z","ts":"2026-10-26T12:00:00Z","source":"backup"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := stored(t).Validate(v); err == nil {
		t.Fatal("stored schema accepted an event without kind")
	}
}

// In LiveContent, null clears a field and absence keeps it; the Go type
// must tell them apart.
func TestLiveContentNullClears(t *testing.T) {
	var e EventInput
	decodeStrict(t, "../schema/examples/valid/progress-update.json", &e)
	c := e.Content
	if c == nil {
		t.Fatal("no content")
	}
	if !c.Trailing.IsSpecified() || !c.Trailing.IsNull() {
		t.Error("trailing: want specified and null")
	}
	if c.Title.IsSpecified() {
		t.Error("title: want unspecified")
	}
	if p, err := c.Progress.Get(); err != nil || p != 0.42 {
		t.Errorf("progress: got %v, %v", p, err)
	}
}

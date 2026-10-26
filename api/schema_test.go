package api

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const schemaPath = "../schema/envelope.v1.schema.json"

func compile(t *testing.T) *jsonschema.Schema {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	s, err := c.Compile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

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

// Every valid example passes the JSON Schema and decodes into the generated
// Envelope with no unknown fields, so the schema and the Go types agree.
func TestValidExamples(t *testing.T) {
	s := compile(t)
	for _, path := range examples(t, "valid") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			if err := s.Validate(load(t, path)); err != nil {
				t.Fatalf("schema: %v", err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			var e Envelope
			if err := dec.Decode(&e); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !e.Kind.Valid() {
				t.Fatalf("kind %q not valid", e.Kind)
			}
		})
	}
}

func TestInvalidExamples(t *testing.T) {
	s := compile(t)
	for _, path := range examples(t, "invalid") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			if err := s.Validate(load(t, path)); err == nil {
				t.Fatal("schema accepted an invalid envelope")
			}
		})
	}
}

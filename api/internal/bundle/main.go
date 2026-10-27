// Command bundle runs oapi-codegen on schema/openapi.yaml with the envelope
// JSON Schema inlined.
//
// oapi-codegen resolves only #/components/... references, but the envelope
// schema keeps its shared types in $defs. bundle hoists every $defs entry into
// components.schemas, rewrites the references to match, writes the result to a
// temporary file and runs oapi-codegen on it. The schema files stay the source
// of truth; nothing here is committed except the generated Go.
//
// Usage, from api/generate.go:
//
//	go run ./internal/bundle -config oapi-codegen.yaml ../schema/openapi.yaml
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

func main() {
	config := flag.String("config", "oapi-codegen.yaml", "oapi-codegen config file")
	keep := flag.String("keep", "", "also write the bundle to this path, for debugging")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: bundle -config oapi-codegen.yaml openapi.yaml")
		os.Exit(2)
	}
	if err := run(flag.Arg(0), *config, *keep); err != nil {
		fmt.Fprintln(os.Stderr, "bundle:", err)
		os.Exit(1)
	}
}

func run(specPath, config, keep string) error {
	out, err := bundle(specPath)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if keep != "" {
		if err := os.WriteFile(keep, data, 0o644); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp("", "heraldo-openapi-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	cmd := exec.Command("go", "tool", "oapi-codegen", "-config", config, tmp.Name())
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// bundle returns the OpenAPI document with every external JSON Schema file
// and its $defs moved into components.schemas.
func bundle(specPath string) (map[string]any, error) {
	raw, err := os.ReadFile(specPath)
	if err != nil {
		return nil, err
	}
	var spec map[string]any
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		return nil, fmt.Errorf("%s: %w", specPath, err)
	}
	components, _ := spec["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	if schemas == nil {
		return nil, fmt.Errorf("%s: no components.schemas", specPath)
	}

	// A component whose whole value is a $ref to a file root takes that
	// file's root schema; other references to the root point at it.
	rootNames := map[string]string{} // file -> component name
	files := map[string]map[string]any{}
	for name, v := range schemas {
		ref := refOf(v)
		if ref == "" || strings.HasPrefix(ref, "#") || strings.Contains(ref, "#") {
			continue
		}
		doc, err := loadJSON(filepath.Join(filepath.Dir(specPath), ref))
		if err != nil {
			return nil, err
		}
		if prev, ok := rootNames[ref]; ok {
			return nil, fmt.Errorf("components %s and %s both take the root of %s", prev, name, ref)
		}
		rootNames[ref] = name
		files[ref] = doc
	}

	// Hoist $defs. A component that is only a $ref to the same def is
	// replaced; any other name clash is an error.
	for file, doc := range files {
		defs, _ := doc["$defs"].(map[string]any)
		delete(doc, "$defs")
		delete(doc, "$schema")
		delete(doc, "$id")
		rewriteRefs(doc, file, rootNames)
		schemas[rootNames[file]] = doc
		for name, def := range defs {
			rewriteRefs(def, file, rootNames)
			if existing, ok := schemas[name]; ok && refOf(existing) != file+"#/$defs/"+name {
				return nil, fmt.Errorf("$defs/%s in %s clashes with components.schemas.%s", name, file, name)
			}
			schemas[name] = def
		}
	}
	rewriteRefs(spec, "", rootNames)
	return spec, nil
}

// rewriteRefs points every $ref under v at components.schemas. file is the
// document v came from, for its local "#/$defs/X" references.
func rewriteRefs(v any, file string, rootNames map[string]string) {
	switch t := v.(type) {
	case map[string]any:
		if ref, ok := t["$ref"].(string); ok {
			t["$ref"] = rewriteRef(ref, file, rootNames)
		}
		for _, child := range t {
			rewriteRefs(child, file, rootNames)
		}
	case []any:
		for _, child := range t {
			rewriteRefs(child, file, rootNames)
		}
	}
}

func rewriteRef(ref, file string, rootNames map[string]string) string {
	target, frag, _ := strings.Cut(ref, "#")
	if target == "" {
		target = file
	}
	if target == "" {
		return ref // a reference inside openapi.yaml itself
	}
	if frag == "" {
		if name, ok := rootNames[target]; ok {
			return "#/components/schemas/" + name
		}
		return ref
	}
	if name, ok := strings.CutPrefix(frag, "/$defs/"); ok && !strings.Contains(name, "/") {
		return "#/components/schemas/" + name
	}
	return ref
}

func refOf(v any) string {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 1 {
		return ""
	}
	ref, _ := m["$ref"].(string)
	return ref
}

func loadJSON(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return doc, nil
}

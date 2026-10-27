package rules

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noahkiss/heraldo/internal/validate"
)

func fixture(t *testing.T) *Set {
	t.Helper()
	s, err := Load("testdata/rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func input(t *testing.T, v *validate.Validator, raw string) *validate.Input {
	t.Helper()
	in, err := v.Input([]byte(raw))
	if err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return in
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"no name":            `rules: [{set: {kind: message}}]`,
		"unknown field":      `rules: [{name: a, set: {kind: message}, stop: true}]`,
		"unknown match":      `rules: [{name: a, match: {summary: x}, set: {kind: message}}]`,
		"bad regex":          `rules: [{name: a, match: {title: '('}, set: {kind: message}}]`,
		"no kind":            `rules: [{name: a, set: {}}]`,
		"rule cannot status": `rules: [{name: a, set: {kind: status, key: k, action: set}}]`,
		"action of kind":     `rules: [{name: a, set: {kind: incident, key: k, action: success}}]`,
		"action on message":  `rules: [{name: a, set: {kind: message, action: open}}]`,
		"bad level":          `rules: [{name: a, set: {kind: message, level: loud}}]`,
		"incident no key":    `rules: [{name: a, set: {kind: incident, action: open}}]`,
		"unknown capture":    `rules: [{name: a, match: {title: '(?P<x>.)'}, set: {kind: incident, key: '{y}'}}]`,
		"capture twice":      `rules: [{name: a, match: {title: '(?P<x>.)', body: '(?P<x>.)'}, set: {kind: message}}]`,
		"name twice":         "rules:\n  - {name: a, set: {kind: message}}\n  - {name: a, set: {kind: digest}}",
		"not yaml":           `rules: [`,
	}
	for name, raw := range cases {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: parsed, want an error", name)
		}
	}
	for _, raw := range []string{"", "# only a comment\n", "rules: []"} {
		s, err := Parse([]byte(raw))
		if err != nil || s.Len() != 0 {
			t.Errorf("%q: %v, %d rules; want no rules", raw, err, s.Len())
		}
	}
}

func TestMatch(t *testing.T) {
	s := fixture(t)
	cases := []struct {
		name   string
		fields map[string]any
		rule   string
		want   map[string]string
	}{
		{"captures fill the key",
			map[string]any{"title": "alpha: 2 unit(s) failed", "body": "system/a.service, system/b.service"},
			"unit-failed",
			map[string]string{"kind": "incident", "action": "open", "key": "unit:alpha:system/a.service", "host": "alpha", "thread": "units"}},
		{"first match wins over the tag rule",
			map[string]any{"title": "Nightly report 2026-01-05", "tags": []any{"quiet"}},
			"nightly-digest",
			map[string]string{"kind": "digest", "level": "passive", "thread": "reports"}},
		{"any tag matches",
			map[string]any{"title": "x", "tags": []any{"ops", "quiet"}},
			"quiet-message",
			map[string]string{"kind": "message", "level": "passive"}},
		{"every field must match",
			map[string]any{"source": "other", "title": "cleanup ok"},
			"", nil},
		{"source and title",
			map[string]any{"source": "jobs", "title": "cleanup ok"},
			"job-ok",
			map[string]string{"kind": "checkin", "action": "success", "key": "job:cleanup"}},
		{"a missing field reads as empty",
			map[string]any{"title": "alpha: 1 unit(s) failed"},
			"", nil},
	}
	for _, c := range cases {
		res, ok := s.Match(c.fields)
		if c.rule == "" {
			if ok {
				t.Errorf("%s: matched %s, want no match", c.name, res.Rule)
			}
			continue
		}
		if !ok || res.Rule != c.rule {
			t.Errorf("%s: rule %q, want %q", c.name, res.Rule, c.rule)
			continue
		}
		if len(res.Fields) != len(c.want) {
			t.Errorf("%s: fields %v, want %v", c.name, res.Fields, c.want)
		}
		for k, x := range c.want {
			if res.Fields[k] != x {
				t.Errorf("%s: %s = %q, want %q", c.name, k, res.Fields[k], x)
			}
		}
	}
}

func TestMatchEmptyTag(t *testing.T) {
	s, err := Parse([]byte(`rules: [{name: untagged, match: {tag: '^$'}, set: {kind: message}}]`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Match(map[string]any{"title": "x"}); !ok {
		t.Error("no tags: no match, want ^$ to match")
	}
	if _, ok := s.Match(map[string]any{"tags": []any{"quiet"}}); ok {
		t.Error("tagged: matched ^$")
	}
}

func TestKeyIsCut(t *testing.T) {
	s, err := Parse([]byte(`rules: [{name: a, match: {body: '(?P<all>.+)'}, set: {kind: incident, key: 'k:{all}'}}]`))
	if err != nil {
		t.Fatal(err)
	}
	res, _ := s.Match(map[string]any{"body": strings.Repeat("é", 400)})
	if n := len([]rune(res.Fields["key"])); n != 255 {
		t.Fatalf("key is %d characters, want 255", n)
	}
}

func TestApply(t *testing.T) {
	v, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	s := fixture(t)

	in := input(t, v, `{"source":"apprise-tee","kind":"message","level":"time-sensitive","tags":["alert"],
		"title":"alpha: 1 unit(s) failed","body":"system/x.service","details":{"tee":{"id":"n-1"}}}`)
	out, rule, err := Apply(s, in, v)
	if err != nil || rule != "unit-failed" {
		t.Fatalf("rule %q, err %v", rule, err)
	}
	ty := out.Typed
	if ty.Kind != "incident" || *ty.Key != "unit:alpha:system/x.service" || string(*ty.Action) != "open" ||
		*ty.Host != "alpha" || *ty.Thread != "units" || string(*ty.Level) != "time-sensitive" {
		t.Fatalf("typed = %+v", ty)
	}
	d, _ := json.Marshal(out.Fields["details"])
	if string(d) != `{"heraldo":{"rule":"unit-failed"},"tee":{"id":"n-1"}}` {
		t.Fatalf("details = %s", d)
	}
	if _, ok := in.Fields["key"]; ok {
		t.Fatal("Apply changed the input's fields")
	}

	// A sender that types its own event is left alone.
	for _, raw := range []string{
		`{"source":"x","kind":"message","key":"mine","title":"alpha: 1 unit(s) failed","body":"a"}`,
		`{"source":"x","kind":"incident","key":"mine","action":"open","title":"alpha: 1 unit(s) failed","body":"a"}`,
		`{"source":"x","kind":"digest","title":"alpha: 1 unit(s) failed","body":"a"}`,
	} {
		in := input(t, v, raw)
		out, rule, err := Apply(s, in, v)
		if out != in || rule != "" || err != nil {
			t.Errorf("%s: rule %q, err %v; want it untouched", raw, rule, err)
		}
	}

	// No rules, or no match: unchanged.
	in = input(t, v, `{"source":"x","kind":"message","title":"hello"}`)
	if out, rule, _ := Apply(nil, in, v); out != in || rule != "" {
		t.Error("nil set changed the input")
	}
	if out, rule, _ := Apply(s, in, v); out != in || rule != "" {
		t.Error("no match changed the input")
	}
}

// A rule whose capture comes out empty leaves an incident with no key; the
// sender's message is kept and the error says why.
func TestApplyInvalidKeepsInput(t *testing.T) {
	v, _ := validate.New()
	s, err := Parse([]byte(`rules: [{name: a, match: {body: '^(?P<x>\s*)$'}, set: {kind: incident, key: '{x}'}}]`))
	if err != nil {
		t.Fatal(err)
	}
	in := input(t, v, `{"source":"x","kind":"message","body":"   "}`)
	out, rule, err := Apply(s, in, v)
	if err == nil || out != in || rule != "" {
		t.Fatalf("rule %q, err %v; want the input back and an error", rule, err)
	}
}

func TestWatcher(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.yaml")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	if _, err := NewWatcher(path, quiet); err == nil {
		t.Fatal("missing file: want an error")
	}
	write(`rules: [`)
	if _, err := NewWatcher(path, quiet); err == nil {
		t.Fatal("invalid file: want an error")
	}

	write(`rules: [{name: a, set: {kind: message}}]`)
	w, err := NewWatcher(path, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if w.Reload() {
		t.Fatal("unchanged file reloaded")
	}
	write("rules:\n  - {name: a, set: {kind: message}}\n  - {name: b, set: {kind: digest}}\n")
	if !w.Reload() || w.Rules().Len() != 2 {
		t.Fatalf("changed file: %d rules, want 2", w.Rules().Len())
	}
	write(`rules: [{name: c, set: {kind: nonsense}}]`)
	if w.Reload() || w.Rules().Len() != 2 {
		t.Fatal("invalid file replaced the rules")
	}
	os.Remove(path)
	if w.Reload() || w.Rules().Len() != 2 {
		t.Fatal("deleted file replaced the rules")
	}
	write(`rules: [{name: d, set: {kind: message}}]`)
	if !w.Reload() || w.Rules().Rules[0].Name != "d" {
		t.Fatal("fixed file not loaded")
	}

	var none *Watcher
	if none.Rules().Len() != 0 {
		t.Fatal("nil watcher has rules")
	}
}

func TestExampleFileParses(t *testing.T) {
	s, err := Load("../../deploy/rules.example.yaml")
	if err != nil || s.Len() == 0 {
		t.Fatalf("deploy/rules.example.yaml: %d rules, %v", s.Len(), err)
	}
}

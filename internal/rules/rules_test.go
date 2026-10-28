package rules

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noahkiss/heraldo/api"
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
		"no name":             `rules: [{set: {kind: message}}]`,
		"unknown field":       `rules: [{name: a, set: {kind: message}, stop: true}]`,
		"unknown match":       `rules: [{name: a, match: {summary: x}, set: {kind: message}}]`,
		"bad regex":           `rules: [{name: a, match: {title: '('}, set: {kind: message}}]`,
		"no kind":             `rules: [{name: a, set: {}}]`,
		"rule cannot status":  `rules: [{name: a, set: {kind: status, key: k, action: set}}]`,
		"action of kind":      `rules: [{name: a, set: {kind: incident, key: k, action: success}}]`,
		"action on message":   `rules: [{name: a, set: {kind: message, action: open}}]`,
		"bad level":           `rules: [{name: a, set: {kind: message, level: loud}}]`,
		"incident no key":     `rules: [{name: a, set: {kind: incident, action: open}}]`,
		"unknown capture":     `rules: [{name: a, match: {title: '(?P<x>.)'}, set: {kind: incident, key: '{y}'}}]`,
		"capture twice":       `rules: [{name: a, match: {title: '(?P<x>.)', body: '(?P<x>.)'}, set: {kind: message}}]`,
		"name twice":          "rules:\n  - {name: a, set: {kind: message}}\n  - {name: a, set: {kind: digest}}",
		"not yaml":            `rules: [`,
		"realert on message":  `rules: [{name: a, set: {kind: message, realert: 10m}}]`,
		"realert too short":   `rules: [{name: a, set: {kind: incident, key: k, realert: 30s}}]`,
		"realert not a time":  `rules: [{name: a, set: {kind: incident, key: k, realert: often}}]`,
		"ping rule kind":      `rules: [{name: a, match: {kind: checkin}, set: {kind: checkin, key: k}}]`,
		"ping rule action":    `rules: [{name: a, match: {kind: checkin}, set: {action: fail}}]`,
		"ping rule realert":   `rules: [{name: a, match: {kind: checkin}, set: {realert: 10m}}]`,
		"inhibit no name":     `inhibit: [{parent: a, children: b}]`,
		"inhibit no parent":   `inhibit: [{name: a, children: b}]`,
		"inhibit bad parent":  `inhibit: [{name: a, parent: '(', children: b}]`,
		"inhibit capture":     `inhibit: [{name: a, parent: '^host:(?P<h>.+)$', children: '^unit:{x}:'}]`,
		"inhibit bad child":   `inhibit: [{name: a, parent: 'x', children: '('}]`,
		"inhibit twice":       "inhibit:\n  - {name: a, parent: x, children: y}\n  - {name: a, parent: x, children: z}",
		"bad set.project":     `rules: [{name: a, set: {kind: message, project: 'a b'}}]`,
		"set.project capture": `rules: [{name: a, set: {kind: message, project: '{x}'}}]`,
		"projects no project": `projects: [{match: {source: x}}]`,
		"projects bad name":   `projects: [{match: {source: x}, project: '-x'}]`,
		"projects bad field":  `projects: [{match: {summary: x}, project: p}]`,
		"projects capture":    `projects: [{match: {source: x}, project: '{y}'}]`,
		"projects twice":      "projects:\n  - {name: a, project: p}\n  - {name: a, project: q}",
		"alias bad name":      `aliases: {'a b': c}`,
		"alias itself":        `aliases: {a: A}`,
		"alias chain":         `aliases: {a: b, b: c}`,
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

func TestMatchDedup(t *testing.T) {
	s, err := Parse([]byte(`rules: [{name: page, match: {tag: '^page$', dedup: '^(?P<id>.+)$'}, set: {kind: incident, key: 'page:{id}'}}]`))
	if err != nil {
		t.Fatal(err)
	}
	res, ok := s.Match(map[string]any{"tags": []any{"page"}, "dedup": "n-42"})
	if !ok || res.Fields["key"] != "page:n-42" {
		t.Fatalf("matched %v, key %q; want page:n-42", ok, res.Fields["key"])
	}
	if _, ok := s.Match(map[string]any{"tags": []any{"page"}}); ok {
		t.Error("no dedup: matched, want no match")
	}
}

func TestApply(t *testing.T) {
	v, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	s := fixture(t)

	in := input(t, v, `{"source":"apprise-tee","level":"time-sensitive","tags":["alert"],
		"title":"alpha: 1 unit(s) failed","body":"system/x.service","details":{"tee":{"id":"n-1"}}}`)
	out, rule, err := Type(s, in, v)
	if err != nil || rule != "unit-failed" {
		t.Fatalf("rule %q, err %v", rule, err)
	}
	ty := out.Typed
	if *ty.Kind != api.KindIncident || *ty.Key != "unit:alpha:system/x.service" || *ty.Action != api.EventActionOpen ||
		*ty.Host != "alpha" || *ty.Thread != "units" || *ty.Level != api.LevelTimeSensitive {
		t.Fatalf("typed = %+v", ty)
	}
	d, _ := json.Marshal(out.Fields["details"])
	if string(d) != `{"heraldo":{"rule":"unit-failed"},"tee":{"id":"n-1"}}` {
		t.Fatalf("details = %s", d)
	}
	if _, ok := in.Fields["key"]; ok {
		t.Fatal("Type changed the input's fields")
	}

	// An explicit kind, message included, is final.
	for _, raw := range []string{
		`{"source":"x","kind":"message","title":"alpha: 1 unit(s) failed","body":"a"}`,
		`{"source":"x","kind":"incident","key":"mine","action":"open","title":"alpha: 1 unit(s) failed","body":"a"}`,
		`{"source":"x","kind":"digest","title":"alpha: 1 unit(s) failed","body":"a"}`,
	} {
		in := input(t, v, raw)
		out, rule, err := Type(s, in, v)
		if out != in || rule != "" || err != nil {
			t.Errorf("%s: rule %q, err %v; want it untouched", raw, rule, err)
		}
	}

	// Untyped with no rules or no match: stored as a message.
	in = input(t, v, `{"source":"x","title":"hello"}`)
	for name, set := range map[string]*Set{"nil set": nil, "no match": s} {
		out, rule, err := Type(set, in, v)
		if err != nil || rule != "" || out.Typed.Kind == nil || *out.Typed.Kind != api.KindMessage ||
			out.Fields["kind"] != "message" {
			t.Errorf("%s: rule %q, err %v, fields %v; want kind message", name, rule, err, out.Fields)
		}
	}
	if _, ok := in.Fields["kind"]; ok {
		t.Fatal("Type changed the input's fields")
	}

	// An untyped sender's own key survives when no rule matches.
	in = input(t, v, `{"source":"x","key":"mine","title":"hello"}`)
	if out, _, _ := Type(s, in, v); *out.Typed.Key != "mine" {
		t.Fatalf("key = %v", out.Typed.Key)
	}
}

// A ping rule sees only ping events, and keeps their kind, key and action;
// a hold rule's hold is store-only.
func TestPingRules(t *testing.T) {
	v, _ := validate.New()
	s, err := Parse([]byte(`rules:
  - name: quiet-backup
    match: {kind: '^checkin$', key: '^backup$'}
    set: {level: passive, hold: true}
  - name: all
    set: {kind: digest}
`))
	if err != nil {
		t.Fatal(err)
	}
	ping := input(t, v, `{"source":"checkin","kind":"checkin","key":"backup","action":"fail","title":"backup failed"}`)
	out, rule, err := ApplyPing(s, ping, v)
	if err != nil || rule != "quiet-backup" || *out.Typed.Kind != api.KindCheckin || *out.Typed.Key != "backup" ||
		*out.Typed.Action != api.EventActionFail || *out.Typed.Level != api.LevelPassive {
		t.Fatalf("rule %q, err %v, typed %+v", rule, err, out.Typed)
	}
	if h := s.Hold(rule); h["state"] != "held" || h["store_only"] != true {
		t.Fatalf("hold %v", h)
	}
	if s.Hold("all") != nil || s.Hold("") != nil {
		t.Fatal("a rule without hold holds")
	}
	// Another check: no ping rule matches, and the catch-all never sees it.
	other := input(t, v, `{"source":"checkin","kind":"checkin","key":"other","action":"fail"}`)
	if out, rule, _ := ApplyPing(s, other, v); out != other || rule != "" {
		t.Fatalf("other: rule %q", rule)
	}
	// An untyped input never meets a ping rule.
	if _, rule, _ := Type(s, input(t, v, `{"source":"checkin","key":"backup","title":"x"}`), v); rule != "all" {
		t.Fatalf("untyped: rule %q", rule)
	}
}

// A rule whose capture comes out empty leaves an incident with no key; the
// event is stored as a message and the error says why.
func TestTypeInvalidFallsBackToMessage(t *testing.T) {
	v, _ := validate.New()
	s, err := Parse([]byte(`rules: [{name: a, match: {body: '^(?P<x>\s*)$'}, set: {kind: incident, key: '{x}'}}]`))
	if err != nil {
		t.Fatal(err)
	}
	in := input(t, v, `{"source":"x","body":"   "}`)
	out, rule, err := Type(s, in, v)
	if err == nil || rule != "" || *out.Typed.Kind != api.KindMessage {
		t.Fatalf("rule %q, err %v, kind %v; want a message and an error", rule, err, out.Typed.Kind)
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

func TestRealertIsStored(t *testing.T) {
	v, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	s, err := Parse([]byte(`rules:
  - name: down
    match: {title: '^(?P<h>\w+) is down'}
    set: {kind: incident, action: open, key: 'host:{h}', level: time-sensitive, realert: 15m}`))
	if err != nil {
		t.Fatal(err)
	}
	out, rule, err := Apply(s, input(t, v, `{"source":"s","title":"web is down"}`), v)
	if err != nil || rule != "down" {
		t.Fatalf("apply %q %v", rule, err)
	}
	h := (*out.Typed.Details)["heraldo"].(map[string]any)
	if h["realert_s"] != float64(900) || h["rule"] != "down" {
		t.Fatalf("details.heraldo = %v", h)
	}
}

func TestInhibited(t *testing.T) {
	s, err := Parse([]byte(`inhibit:
  - name: host-down
    parent: '^host:(?P<host>[\w.-]+)$'
    children: '^(unit|container):{host}:'`))
	if err != nil {
		t.Fatal(err)
	}
	open := []string{"host:web.1", "unit:db:pg"}
	for _, c := range []struct {
		key, parent string
	}{
		{"unit:web.1:nginx", "host:web.1"},
		{"container:web.1:app", "host:web.1"},
		// The capture is quoted: the dot in web.1 matches a dot only.
		{"unit:webx1:nginx", ""},
		{"unit:db:pg", ""},
		{"host:web.1", ""},
		{"disk:web.1:sda", ""},
		{"", ""},
	} {
		rule, parent, ok := s.Inhibited(c.key, open)
		if ok != (c.parent != "") || parent != c.parent || (ok && rule != "host-down") {
			t.Errorf("%q: %q %q %v; want parent %q", c.key, rule, parent, ok, c.parent)
		}
	}
	if _, _, ok := (*Set)(nil).Inhibited("unit:web.1:x", open); ok {
		t.Fatal("nil set inhibits")
	}
}

func TestProjects(t *testing.T) {
	s, err := Parse([]byte(`
rules:
  - name: typed
    match: {title: '^(?P<team>[a-z]+): failed'}
    set: {kind: incident, key: 'job:{team}', action: open, project: '{team}'}
  - name: odd
    match: {title: '^(?P<team>.+)! failed'}
    set: {kind: message, project: '{team}'}
  - name: ping
    match: {kind: '^checkin$', key: '^nightly-'}
    set: {project: Batch}
projects:
  - name: printer
    match: {source: '^printer$'}
    project: printing
  - match: {thread: '^backups$'}
    project: old-fleet
aliases:
  old-fleet: fleet
  Infra: fleet
`))
	if err != nil {
		t.Fatal(err)
	}
	v, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	project := func(raw string) string {
		t.Helper()
		typed, _, err := Type(s, input(t, v, raw), v)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		out, _, err := WithProject(s, typed, v)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		p, _ := out.Fields["project"].(string)
		if out.Typed.Project != nil && *out.Typed.Project != p {
			t.Fatalf("%s: typed project %q, field %q", raw, *out.Typed.Project, p)
		}
		return p
	}
	for raw, want := range map[string]string{
		`{"source":"cron","title":"ops: failed"}`:                     "ops",
		`{"source":"cron","title":"ops: failed","project":"web"}`:     "web",
		`{"source":"cron","title":"no good! failed"}`:                 "",
		`{"source":"printer","kind":"message","title":"done"}`:        "printing",
		`{"source":"cron","kind":"message","thread":"backups"}`:       "fleet",
		`{"source":"cron","kind":"message","project":"infra"}`:        "fleet",
		`{"source":"cron","kind":"message","project":"INFRA"}`:        "fleet",
		`{"source":"printer","kind":"message","project":"elsewhere"}`: "elsewhere",
		`{"source":"cron","kind":"message"}`:                          "",
	} {
		if got := project(raw); got != want {
			t.Errorf("%s: project %q, want %q", raw, got, want)
		}
	}
	ping, rule, err := ApplyPing(s, input(t, v, `{"source":"checkin","kind":"checkin","key":"nightly-db","action":"success"}`), v)
	if err != nil || rule != "ping" || ping.Fields["project"] != "batch" {
		t.Fatalf("ping: %v %q %v", ping.Fields["project"], rule, err)
	}
	if got := strings.Join(s.Expand([]string{"fleet"}), ","); got != "fleet,infra,old-fleet" {
		t.Errorf("Expand(fleet) = %s", got)
	}
	if got := strings.Join(s.Expand([]string{"infra", "web"}), ","); got != "fleet,infra,old-fleet,web" {
		t.Errorf("Expand(infra, web) = %s", got)
	}
	var none *Set
	if got := strings.Join(none.Expand([]string{"web"}), ","); got != "web" || none.Alias("x") != "x" {
		t.Errorf("nil set: Expand = %s", got)
	}
	if p, entry := s.MapProject(map[string]any{"source": "printer"}); p != "printing" || entry != "printer" {
		t.Errorf("MapProject = %q, %q", p, entry)
	}
}

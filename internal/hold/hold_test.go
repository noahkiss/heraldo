package hold

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const sample = `[
  {"why": "a comment", "tag": "page"},
  {"title_regex": "^🚨 ZFS (pool|dataset) "},
  {"title_regex": "SMART .*(FAILED|[Ff]ailed|[Ff]ailure)"},
  {"body_regex": "hold-row@host"},
  {"why": "no pattern: skipped"}
]`

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestParseAndMatch(t *testing.T) {
	rules, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 4 {
		t.Fatalf("rules = %d, want 4", len(rules))
	}
	b := &Bypass{rules: rules}
	for _, c := range []struct {
		tag, title, body string
		want             bool
	}{
		{"PAGE", "", "", true},
		{"alert", "🚨 ZFS pool tank at 93%", "", true},
		{"alert", "⚠️ ZFS pool tank at 86%", "", false},
		{"alert", "SMART failure on sda", "", true},
		{"alert", "SMART WARNING on sda", "", false},
		{"alert", "Monitoring gone quiet", "rows: hold-row@host", true},
		{"alert", "disk full", "", false},
	} {
		if got := b.Bypassed(c.tag, c.title, c.body); got != c.want {
			t.Errorf("%q %q %q: bypassed = %v", c.tag, c.title, c.body, got)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, bad := range []string{`{}`, `[{"title_regex": "("}]`, `not json`} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestBypassReloadKeepsRulesOnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bypass.json")
	if err := os.WriteFile(path, []byte(`[{"tag": "page"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := NewBypass(path, quiet())
	if err != nil || len(b.Rules()) != 1 {
		t.Fatalf("load: %v %v", b, err)
	}
	// A broken file keeps the rules in force.
	later := time.Now().Add(time.Minute)
	os.WriteFile(path, []byte(`[{`), 0o600)
	os.Chtimes(path, later, later)
	if !b.Bypassed("page", "", "") {
		t.Fatal("a broken file dropped the rules")
	}
	// A good file replaces them.
	later = later.Add(time.Minute)
	os.WriteFile(path, []byte(`[{"tag": "page"}, {"tag": "ops"}]`), 0o600)
	os.Chtimes(path, later, later)
	if len(b.Rules()) != 2 {
		t.Fatalf("rules = %d after a good change", len(b.Rules()))
	}
	// A missing file means no rules.
	os.Remove(path)
	if b.Bypassed("page", "", "") {
		t.Fatal("a missing file kept the rules")
	}
}

func TestNewBypassRefusesABrokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bypass.json")
	os.WriteFile(path, []byte(`[{`), 0o600)
	if _, err := NewBypass(path, quiet()); err == nil {
		t.Fatal("started on a broken file")
	}
	if b, err := NewBypass(filepath.Join(t.TempDir(), "none.json"), quiet()); err != nil || len(b.Rules()) != 0 {
		t.Fatalf("missing file: %v %v", b, err)
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"": Off, "off": Off, " ON ": On, "shadow": Shadow} {
		if got, err := ParseMode(in); err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	if _, err := ParseMode("maybe"); err == nil {
		t.Error("accepted maybe")
	}
}

func TestExampleFileParses(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/hold-bypass.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if rules, err := Parse(raw); err != nil || len(rules) == 0 {
		t.Fatalf("example: %d rules, %v", len(rules), err)
	}
}

// Package rules classifies untyped messages at ingest. A rules file turns a
// legacy {title, body, tag} post into a typed event: an incident that opens
// or resolves, a digest, or a quiet message.
//
// The file is YAML (JSON is YAML too):
//
//	rules:
//	  - name: unit-failed
//	    match:
//	      title: '^(?P<host>[\w.-]+): \d+ unit\(s\) failed'
//	      body: '^(?P<unit>\S+)'
//	    set:
//	      kind: incident
//	      action: open
//	      key: 'unit:{host}:{unit}'
//	      host: '{host}'
//	      level: time-sensitive
//
// match holds regular expressions (Go RE2 syntax) on source, host, tag, title
// and body. Every field a rule names must match; a rule with no match fields
// matches everything. tag matches when any one tag does. A missing field
// reads as the empty string. Named groups, (?P<name>...), are captures:
// {name} in a set value is replaced with the captured text.
//
// set gives kind, key, action, level, thread and host. kind is required and
// must be message, digest, incident or checkin. A field left out keeps the
// sender's value.
//
// Rules run in file order and the first match wins. They apply only to an
// input that is kind message with no key and no action: a sender that types
// its own events is never reclassified. A classified event records the rule
// in details.heraldo.rule.
package rules

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/validate"
)

// matchFields are the event fields a rule can match on.
var matchFields = []string{"source", "host", "tag", "title", "body"}

// setLimits are the fields a rule can set that take a template, with the
// envelope's length limit for each.
var setLimits = map[string]int{"key": 255, "thread": 128, "host": 128}

// actions lists the actions each kind a rule may set accepts. progress and
// status need content, which a rule cannot give, so rules cannot set them.
var actions = map[api.Kind][]api.EventAction{
	api.KindMessage:  nil,
	api.KindDigest:   nil,
	api.KindIncident: {api.EventActionOpen, api.EventActionUpdate, api.EventActionAck, api.EventActionResolve},
	api.KindCheckin:  {api.EventActionStart, api.EventActionSuccess, api.EventActionFail, api.EventActionLog},
}

// file is the rules file as written.
type file struct {
	Rules []ruleSpec `yaml:"rules"`
}

type ruleSpec struct {
	Name  string            `yaml:"name"`
	Match map[string]string `yaml:"match"`
	Set   setSpec           `yaml:"set"`
}

type setSpec struct {
	Kind   string `yaml:"kind"`
	Key    string `yaml:"key"`
	Action string `yaml:"action"`
	Level  string `yaml:"level"`
	Thread string `yaml:"thread"`
	Host   string `yaml:"host"`
}

// Rule is one compiled rule.
type Rule struct {
	Name   string
	match  map[string]*regexp.Regexp
	kind   api.Kind
	action api.EventAction
	level  api.Level
	tmpl   map[string]string // key, thread, host
}

// Set is a compiled rules file. The zero value and nil hold no rules.
type Set struct {
	Rules []*Rule
}

// Len returns the number of rules.
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	return len(s.Rules)
}

// Load reads and compiles the rules file at path.
func Load(path string) (*Set, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Parse compiles a rules file.
func Parse(raw []byte) (*Set, error) {
	var f file
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	// An empty file holds no rules; yaml.v3 reports it as io.EOF.
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	s := &Set{}
	names := map[string]bool{}
	for i, spec := range f.Rules {
		r, err := compile(spec)
		if err != nil {
			label := spec.Name
			if label == "" {
				label = fmt.Sprintf("#%d", i+1)
			}
			return nil, fmt.Errorf("rule %s: %w", label, err)
		}
		if names[r.Name] {
			return nil, fmt.Errorf("rule %s: name used twice", r.Name)
		}
		names[r.Name] = true
		s.Rules = append(s.Rules, r)
	}
	return s, nil
}

var placeholder = regexp.MustCompile(`\{([^{}]*)\}`)

func compile(spec ruleSpec) (*Rule, error) {
	if spec.Name == "" {
		return nil, errors.New("name is required")
	}
	r := &Rule{Name: spec.Name, match: map[string]*regexp.Regexp{}, tmpl: map[string]string{}}
	groups := map[string]bool{}
	for field, expr := range spec.Match {
		if !isMatchField(field) {
			return nil, fmt.Errorf("match: unknown field %q; want one of %s", field, strings.Join(matchFields, ", "))
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("match.%s: %w", field, err)
		}
		for _, g := range re.SubexpNames() {
			if g == "" {
				continue
			}
			if groups[g] {
				return nil, fmt.Errorf("match.%s: capture %q is named twice", field, g)
			}
			groups[g] = true
		}
		r.match[field] = re
	}

	r.kind = api.Kind(spec.Set.Kind)
	allowed, ok := actions[r.kind]
	if !ok {
		return nil, fmt.Errorf("set.kind: %q; want message, digest, incident or checkin", spec.Set.Kind)
	}
	if spec.Set.Action != "" {
		r.action = api.EventAction(spec.Set.Action)
		found := false
		for _, a := range allowed {
			found = found || a == r.action
		}
		if !found {
			return nil, fmt.Errorf("set.action: %q is not an action of kind %s", spec.Set.Action, r.kind)
		}
	}
	if spec.Set.Level != "" {
		r.level = api.Level(spec.Set.Level)
		if !r.level.Valid() {
			return nil, fmt.Errorf("set.level: %q is not a level", spec.Set.Level)
		}
	}
	for field, t := range map[string]string{"key": spec.Set.Key, "thread": spec.Set.Thread, "host": spec.Set.Host} {
		if t == "" {
			continue
		}
		for _, m := range placeholder.FindAllStringSubmatch(t, -1) {
			if !groups[m[1]] {
				return nil, fmt.Errorf("set.%s: {%s} names no capture in match", field, m[1])
			}
		}
		r.tmpl[field] = t
	}
	if (r.kind == api.KindIncident || r.kind == api.KindCheckin) && r.tmpl["key"] == "" {
		return nil, fmt.Errorf("set.key: kind %s needs a key", r.kind)
	}
	return r, nil
}

func isMatchField(f string) bool {
	for _, m := range matchFields {
		if f == m {
			return true
		}
	}
	return false
}

// Eligible reports whether rules may classify an input: kind message with no
// key and no action.
func Eligible(fields map[string]any) bool {
	if k, _ := fields["kind"].(string); k != string(api.KindMessage) {
		return false
	}
	_, key := fields["key"]
	_, action := fields["action"]
	return !key && !action
}

// Result is what the first matching rule sets.
type Result struct {
	Rule string
	// Fields holds the envelope fields to set: kind always, and key,
	// action, level, thread and host when the rule gives them.
	Fields map[string]string
}

// Match returns the result of the first rule that matches fields, the
// sender's event. It does not check Eligible.
func (s *Set) Match(fields map[string]any) (Result, bool) {
	if s == nil {
		return Result{}, false
	}
	for _, r := range s.Rules {
		if caps, ok := r.matches(fields); ok {
			return r.result(caps), true
		}
	}
	return Result{}, false
}

func (r *Rule) matches(fields map[string]any) (map[string]string, bool) {
	caps := map[string]string{}
	for field, re := range r.match {
		var vals []string
		if field == "tag" {
			vals = tagsOf(fields)
			if len(vals) == 0 {
				vals = []string{""}
			}
		} else {
			s, _ := fields[field].(string)
			vals = []string{s}
		}
		hit := false
		for _, v := range vals {
			m := re.FindStringSubmatch(v)
			if m == nil {
				continue
			}
			for i, name := range re.SubexpNames() {
				if name != "" {
					caps[name] = m[i]
				}
			}
			hit = true
			break
		}
		if !hit {
			return nil, false
		}
	}
	return caps, true
}

func tagsOf(fields map[string]any) []string {
	switch t := fields["tags"].(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return t
	}
	return nil
}

func (r *Rule) result(caps map[string]string) Result {
	res := Result{Rule: r.Name, Fields: map[string]string{"kind": string(r.kind)}}
	if r.action != "" {
		res.Fields["action"] = string(r.action)
	}
	if r.level != "" {
		res.Fields["level"] = string(r.level)
	}
	for field, t := range r.tmpl {
		v := placeholder.ReplaceAllStringFunc(t, func(p string) string {
			return caps[p[1:len(p)-1]]
		})
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		res.Fields[field] = cut(v, setLimits[field])
	}
	return res
}

// Apply classifies a checked input with s. It returns the input unchanged,
// and an empty rule name, when the input is not Eligible, no rule matches, or
// the classified event would not be a valid envelope (the error says why).
func Apply(s *Set, in *validate.Input, v *validate.Validator) (*validate.Input, string, error) {
	if s.Len() == 0 || !Eligible(in.Fields) {
		return in, "", nil
	}
	res, ok := s.Match(in.Fields)
	if !ok {
		return in, "", nil
	}
	f := make(map[string]any, len(in.Fields)+4)
	for k, x := range in.Fields {
		f[k] = x
	}
	for k, x := range res.Fields {
		f[k] = x
	}
	details := map[string]any{}
	if d, ok := in.Fields["details"].(map[string]any); ok {
		for k, x := range d {
			details[k] = x
		}
	}
	own := map[string]any{}
	if h, ok := details["heraldo"].(map[string]any); ok {
		for k, x := range h {
			own[k] = x
		}
	}
	own["rule"] = res.Rule
	details["heraldo"] = own
	f["details"] = details
	out, err := v.Fields(f)
	if err != nil {
		return in, "", fmt.Errorf("rule %s: %w", res.Rule, err)
	}
	return out, res.Rule, nil
}

// cut shortens s to n characters.
func cut(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

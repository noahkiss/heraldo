// Package rules types untyped inputs at ingest. A rules file turns a
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
// match holds regular expressions (Go RE2 syntax) on source, host, tag, title,
// body, key, action, kind and dedup. Every field a rule names must match; a rule with no match fields
// matches everything. tag matches when any one tag does. A missing field
// reads as the empty string. Named groups, (?P<name>...), are captures:
// {name} in a set value is replaced with the captured text.
//
// set gives kind, key, action, level, thread, host and project. kind is
// required and must be message, digest, incident or checkin. A field left
// out keeps the sender's value, and a sender's project wins over the rule's.
// An incident rule may also set realert, a Go duration of a
// minute or more: an open time-sensitive or critical incident is pushed
// again at that interval until it is acked or resolved. It is stored as
// details.heraldo.realert_s. Any rule may set hold: true; the server then
// stores the event held and store-only, so it never pushes.
//
// A rule whose match names kind is a ping rule. It sees only the check-in
// events the ping API makes (ApplyPing), never an untyped input, and it
// keeps the event's kind, key and action: its set gives level, thread, host,
// project and hold only.
//
// projects, beside rules, files events under a project. It runs on every
// input after the rules typed it, typed senders and pings included, and only
// when the event has no project yet. The first entry that matches wins:
//
//	projects:
//	  - match: {source: '^printer-'}
//	    project: printing
//	  - match: {thread: '^(units|backups)$'}
//	    project: fleet
//
// match takes the fields a rule matches, plus thread. name is optional.
// project takes {name} placeholders. A project name folds to lower case; a
// result that is not a valid name sets nothing.
//
// aliases maps an old project name to its new one. An event that names an
// old one is stored under the new one, and a filter on the new one also
// matches the events stored under the old:
//
//	aliases:
//	  infra: fleet
//
// inhibit, beside rules, holds back the pushes of child incidents while a
// parent incident is not resolved:
//
//	inhibit:
//	  - name: host-down
//	    parent: '^host:(?P<host>[\w.-]+)$'
//	    children: '^(unit|container):{host}:'
//
// parent is a regular expression on an incident key. children is one too;
// {name} in it is replaced with the parent's capture, quoted. While an
// incident whose key matches parent is open or acked, an event whose key
// matches children, and is not the parent, is stored but not pushed.
//
// Rules run in file order and the first match wins. Apart from ping rules,
// they apply only to an untyped input, one without kind: an explicit kind,
// message included, is the sender's final word. Type stores an untyped input no rule matches as
// kind message. A classified event records the rule in details.heraldo.rule.
package rules

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/validate"
)

// matchFields are the event fields a rule can match on.
var matchFields = []string{"source", "host", "tag", "title", "body", "key", "action", "kind", "dedup"}

// projectMatchFields are the fields a projects entry can match on: a rule's,
// and the thread a rule may have set.
var projectMatchFields = append(append([]string{}, matchFields...), "thread")

// setLimits are the fields a rule can set that take a template, with the
// envelope's length limit for each.
var setLimits = map[string]int{"key": 255, "thread": 128, "host": 128, "project": 64}

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
	Rules    []ruleSpec        `yaml:"rules"`
	Inhibit  []inhibitSpec     `yaml:"inhibit"`
	Projects []projectSpec     `yaml:"projects"`
	Aliases  map[string]string `yaml:"aliases"`
}

type projectSpec struct {
	Name    string            `yaml:"name"`
	Match   map[string]string `yaml:"match"`
	Project string            `yaml:"project"`
}

type inhibitSpec struct {
	Name     string `yaml:"name"`
	Parent   string `yaml:"parent"`
	Children string `yaml:"children"`
}

type ruleSpec struct {
	Name  string            `yaml:"name"`
	Match map[string]string `yaml:"match"`
	Set   setSpec           `yaml:"set"`
}

type setSpec struct {
	Kind    string `yaml:"kind"`
	Key     string `yaml:"key"`
	Action  string `yaml:"action"`
	Level   string `yaml:"level"`
	Thread  string `yaml:"thread"`
	Host    string `yaml:"host"`
	Project string `yaml:"project"`
	// Realert is a Go duration, such as 15m.
	Realert string `yaml:"realert"`
	Hold    bool   `yaml:"hold"`
}

// Rule is one compiled rule.
type Rule struct {
	Name   string
	match  map[string]*regexp.Regexp
	kind   api.Kind
	action api.EventAction
	level  api.Level
	tmpl   map[string]string // key, thread, host, project
	// realert is the re-alert interval; 0 means none.
	realert time.Duration
	// ping is set on a rule whose match names kind: it applies to ping
	// events only. hold stores the event held.
	ping, hold bool
}

// MinRealert is the shortest re-alert interval a rule may set.
const MinRealert = time.Minute

// Inhibit is one compiled inhibit rule.
type Inhibit struct {
	Name     string
	parent   *regexp.Regexp
	children string
}

// ProjectRule is one compiled projects entry.
type ProjectRule struct {
	Name    string
	match   map[string]*regexp.Regexp
	project string
}

// Set is a compiled rules file. The zero value and nil hold no rules.
type Set struct {
	Rules    []*Rule
	Inhibits []*Inhibit
	Projects []*ProjectRule
	// Aliases maps an old project name to its new one.
	Aliases map[string]string
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
	inhibits := map[string]bool{}
	for i, spec := range f.Inhibit {
		in, err := compileInhibit(spec)
		if err != nil {
			label := spec.Name
			if label == "" {
				label = fmt.Sprintf("#%d", i+1)
			}
			return nil, fmt.Errorf("inhibit %s: %w", label, err)
		}
		if inhibits[in.Name] {
			return nil, fmt.Errorf("inhibit %s: name used twice", in.Name)
		}
		inhibits[in.Name] = true
		s.Inhibits = append(s.Inhibits, in)
	}
	projects := map[string]bool{}
	for i, spec := range f.Projects {
		label := spec.Name
		if label == "" {
			label = fmt.Sprintf("#%d", i+1)
		}
		pr, err := compileProject(spec, label)
		if err != nil {
			return nil, fmt.Errorf("projects %s: %w", label, err)
		}
		if projects[pr.Name] {
			return nil, fmt.Errorf("projects %s: name used twice", pr.Name)
		}
		projects[pr.Name] = true
		s.Projects = append(s.Projects, pr)
	}
	if err := s.compileAliases(f.Aliases); err != nil {
		return nil, err
	}
	return s, nil
}

// compileMatch compiles a match map whose fields are in allowed, and returns
// the names of its captures.
func compileMatch(spec map[string]string, allowed []string) (map[string]*regexp.Regexp, map[string]bool, error) {
	match := map[string]*regexp.Regexp{}
	groups := map[string]bool{}
	for field, expr := range spec {
		if !slices.Contains(allowed, field) {
			return nil, nil, fmt.Errorf("match: unknown field %q; want one of %s", field, strings.Join(allowed, ", "))
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, nil, fmt.Errorf("match.%s: %w", field, err)
		}
		for _, g := range re.SubexpNames() {
			if g == "" {
				continue
			}
			if groups[g] {
				return nil, nil, fmt.Errorf("match.%s: capture %q is named twice", field, g)
			}
			groups[g] = true
		}
		match[field] = re
	}
	return match, groups, nil
}

// checkProject checks a project template: its placeholders name captures,
// and without any it is a valid name once folded.
func checkProject(t string, groups map[string]bool) error {
	phs := placeholder.FindAllStringSubmatch(t, -1)
	for _, m := range phs {
		if !groups[m[1]] {
			return fmt.Errorf("{%s} names no capture in match", m[1])
		}
	}
	if len(phs) == 0 && !validate.ValidProject(validate.FoldProject(t)) {
		return fmt.Errorf("%q is not a project name: 1 to 64 of a-z, 0-9, dot, underscore and dash, starting with a letter or digit", t)
	}
	return nil
}

func compileProject(spec projectSpec, label string) (*ProjectRule, error) {
	if spec.Project == "" {
		return nil, errors.New("project is required")
	}
	match, groups, err := compileMatch(spec.Match, projectMatchFields)
	if err != nil {
		return nil, err
	}
	if err := checkProject(spec.Project, groups); err != nil {
		return nil, fmt.Errorf("project: %w", err)
	}
	return &ProjectRule{Name: label, match: match, project: spec.Project}, nil
}

func (s *Set) compileAliases(raw map[string]string) error {
	if len(raw) == 0 {
		return nil
	}
	s.Aliases = map[string]string{}
	for old, target := range raw {
		o, t := validate.FoldProject(old), validate.FoldProject(target)
		for _, n := range []string{o, t} {
			if !validate.ValidProject(n) {
				return fmt.Errorf("aliases: %q is not a project name", n)
			}
		}
		if o == t {
			return fmt.Errorf("aliases: %s names itself", o)
		}
		if _, dup := s.Aliases[o]; dup {
			return fmt.Errorf("aliases: %s is given twice", o)
		}
		s.Aliases[o] = t
	}
	for o, t := range s.Aliases {
		if _, chained := s.Aliases[t]; chained {
			return fmt.Errorf("aliases: %s maps to %s, which is an alias too; map it to %s", o, t, s.Aliases[t])
		}
	}
	return nil
}

// Alias returns the project name an event naming p is stored under: the
// alias target, or p itself.
func (s *Set) Alias(p string) string {
	if s == nil {
		return p
	}
	if t, ok := s.Aliases[p]; ok {
		return t
	}
	return p
}

// Expand returns the stored names a filter on projects matches: each name,
// and every alias that maps to it. A name that is itself an alias stands
// for its target. The answer is sorted, without repeats.
func (s *Set) Expand(projects []string) []string {
	seen := map[string]bool{}
	for _, p := range projects {
		t := s.Alias(p)
		seen[p], seen[t] = true, true
		if s == nil {
			continue
		}
		for o, x := range s.Aliases {
			if x == t {
				seen[o] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// MapProject returns the project the first matching projects entry gives
// fields, folded and through the aliases, and the entry's name. It returns
// "" when none matches or the result is not a valid name.
func (s *Set) MapProject(fields map[string]any) (project, entry string) {
	if s == nil {
		return "", ""
	}
	for _, pr := range s.Projects {
		caps, ok := matchAll(pr.match, fields)
		if !ok {
			continue
		}
		p := validate.FoldProject(fill(pr.project, caps))
		if !validate.ValidProject(p) {
			return "", pr.Name
		}
		return s.Alias(p), pr.Name
	}
	return "", ""
}

// WithProject files a typed input under its project: a name the sender or
// a rule gave goes through the aliases, and an input with none gets the
// projects map's answer. It returns in unchanged when there is nothing to
// do. entry names the projects entry that matched, or is empty.
func WithProject(s *Set, in *validate.Input, v *validate.Validator) (out *validate.Input, entry string, err error) {
	cur, _ := in.Fields["project"].(string)
	want := cur
	if cur != "" {
		want = s.Alias(cur)
	} else {
		want, entry = s.MapProject(in.Fields)
	}
	if want == cur {
		return in, entry, nil
	}
	f := make(map[string]any, len(in.Fields)+1)
	for k, x := range in.Fields {
		f[k] = x
	}
	f["project"] = want
	out, err = v.Fields(f)
	if err != nil {
		return in, entry, fmt.Errorf("project %s: %w", want, err)
	}
	return out, entry, nil
}

func compileInhibit(spec inhibitSpec) (*Inhibit, error) {
	if spec.Name == "" {
		return nil, errors.New("name is required")
	}
	if spec.Parent == "" || spec.Children == "" {
		return nil, errors.New("parent and children are required")
	}
	parent, err := regexp.Compile(spec.Parent)
	if err != nil {
		return nil, fmt.Errorf("parent: %w", err)
	}
	groups := map[string]bool{}
	for _, g := range parent.SubexpNames() {
		groups[g] = g != ""
	}
	for _, m := range placeholder.FindAllStringSubmatch(spec.Children, -1) {
		if !groups[m[1]] {
			return nil, fmt.Errorf("children: {%s} names no capture in parent", m[1])
		}
	}
	// Check that children compiles with any capture in its place.
	if _, err := regexp.Compile(placeholder.ReplaceAllString(spec.Children, "x")); err != nil {
		return nil, fmt.Errorf("children: %w", err)
	}
	return &Inhibit{Name: spec.Name, parent: parent, children: spec.Children}, nil
}

// InhibitRules returns the inhibit rules; a nil Set has none.
func (s *Set) InhibitRules() []*Inhibit {
	if s == nil {
		return nil
	}
	return s.Inhibits
}

// Inhibited reports whether an event with key is held back by an incident
// in open, the keys of the incidents that are not resolved. It returns the
// inhibit rule and the parent key.
func (s *Set) Inhibited(key string, open []string) (rule, parent string, ok bool) {
	if s == nil || key == "" {
		return "", "", false
	}
	for _, in := range s.Inhibits {
		for _, p := range open {
			if p == key {
				continue
			}
			m := in.parent.FindStringSubmatch(p)
			if m == nil {
				continue
			}
			expr := placeholder.ReplaceAllStringFunc(in.children, func(ph string) string {
				name := ph[1 : len(ph)-1]
				if i := in.parent.SubexpIndex(name); i >= 0 {
					return regexp.QuoteMeta(m[i])
				}
				return ""
			})
			re, err := regexp.Compile(expr)
			if err != nil {
				continue
			}
			if re.MatchString(key) {
				return in.Name, p, true
			}
		}
	}
	return "", "", false
}

var placeholder = regexp.MustCompile(`\{([^{}]*)\}`)

func compile(spec ruleSpec) (*Rule, error) {
	if spec.Name == "" {
		return nil, errors.New("name is required")
	}
	match, groups, err := compileMatch(spec.Match, matchFields)
	if err != nil {
		return nil, err
	}
	r := &Rule{Name: spec.Name, match: match, tmpl: map[string]string{}}

	r.hold = spec.Set.Hold
	if _, r.ping = r.match["kind"]; r.ping {
		if spec.Set.Kind != "" || spec.Set.Key != "" || spec.Set.Action != "" || spec.Set.Realert != "" {
			return nil, errors.New("set: a rule that matches kind keeps the event's kind, key and action; it sets level, thread, host, project and hold only")
		}
		spec.Set.Kind = string(api.KindCheckin)
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
	if spec.Set.Project != "" {
		if err := checkProject(spec.Set.Project, groups); err != nil {
			return nil, fmt.Errorf("set.project: %w", err)
		}
	}
	for field, t := range map[string]string{"key": spec.Set.Key, "thread": spec.Set.Thread, "host": spec.Set.Host, "project": spec.Set.Project} {
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
	if spec.Set.Realert != "" {
		d, err := time.ParseDuration(spec.Set.Realert)
		if err != nil {
			return nil, fmt.Errorf("set.realert: %w", err)
		}
		if r.kind != api.KindIncident {
			return nil, errors.New("set.realert: only an incident rule re-alerts")
		}
		if d < MinRealert {
			return nil, fmt.Errorf("set.realert: %s is under %s", d, MinRealert)
		}
		r.realert = d.Truncate(time.Second)
	}
	if (r.kind == api.KindIncident || r.kind == api.KindCheckin) && r.tmpl["key"] == "" && !r.ping {
		return nil, fmt.Errorf("set.key: kind %s needs a key", r.kind)
	}
	return r, nil
}

// Eligible reports whether rules may classify an input: it has no kind.
func Eligible(fields map[string]any) bool {
	_, typed := fields["kind"]
	return !typed
}

// Result is what the first matching rule sets.
type Result struct {
	Rule string
	// Fields holds the envelope fields to set: kind always, and key,
	// action, level, thread, host and project when the rule gives them.
	Fields map[string]string
	// Realert is the rule's re-alert interval; 0 means none.
	Realert time.Duration
	// Hold is set when the rule stores the event held.
	Hold bool
}

// Match returns the result of the first rule that matches fields, the
// sender's event. It does not check Eligible, and skips ping rules.
func (s *Set) Match(fields map[string]any) (Result, bool) {
	return s.match(fields, false)
}

// MatchPing returns the result of the first ping rule that matches fields,
// a check-in event the ping API made. Its Fields never hold kind.
func (s *Set) MatchPing(fields map[string]any) (Result, bool) {
	return s.match(fields, true)
}

func (s *Set) match(fields map[string]any, ping bool) (Result, bool) {
	if s == nil {
		return Result{}, false
	}
	for _, r := range s.Rules {
		if r.ping != ping {
			continue
		}
		if caps, ok := r.matches(fields); ok {
			return r.result(caps), true
		}
	}
	return Result{}, false
}

// Level returns the level the rule called name sets, or "" when there is
// no such rule or it sets none. A rule's critical needs no token
// capability: the owner writes the rules.
func (s *Set) Level(name string) api.Level {
	if s == nil || name == "" {
		return ""
	}
	for _, r := range s.Rules {
		if r.Name == name {
			return r.level
		}
	}
	return ""
}

// Hold returns the stored hold the rule called name gives an event, or nil
// when there is no such rule or it holds nothing: held and store-only, so
// the event never pushes.
func (s *Set) Hold(name string) map[string]any {
	if s == nil || name == "" {
		return nil
	}
	for _, r := range s.Rules {
		if r.Name == name && r.hold {
			return map[string]any{"state": "held", "reason": "rule " + name, "store_only": true}
		}
	}
	return nil
}

func (r *Rule) matches(fields map[string]any) (map[string]string, bool) {
	return matchAll(r.match, fields)
}

// matchAll reports whether every expression in match matches its field, and
// returns the captures.
func matchAll(match map[string]*regexp.Regexp, fields map[string]any) (map[string]string, bool) {
	caps := map[string]string{}
	for field, re := range match {
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
	res := Result{Rule: r.Name, Fields: map[string]string{}, Realert: r.realert, Hold: r.hold}
	if !r.ping {
		res.Fields["kind"] = string(r.kind)
	}
	if r.action != "" {
		res.Fields["action"] = string(r.action)
	}
	if r.level != "" {
		res.Fields["level"] = string(r.level)
	}
	for field, t := range r.tmpl {
		v := strings.TrimSpace(fill(t, caps))
		if field == "project" {
			// A capture that makes no valid name sets nothing, rather than
			// spoil the typed event.
			if v = validate.FoldProject(v); !validate.ValidProject(v) {
				continue
			}
		}
		if v == "" {
			continue
		}
		res.Fields[field] = cut(v, setLimits[field])
	}
	return res
}

// fill replaces each {name} in t with the capture called name.
func fill(t string, caps map[string]string) string {
	return placeholder.ReplaceAllStringFunc(t, func(p string) string {
		return caps[p[1:len(p)-1]]
	})
}

// Apply classifies a checked input with s. It returns the input unchanged,
// and an empty rule name, when the input is not Eligible, no rule matches, or
// the classified event would not be a valid envelope (the error says why).
// The result may still be untyped; Type finishes the job.
func Apply(s *Set, in *validate.Input, v *validate.Validator) (*validate.Input, string, error) {
	if s.Len() == 0 || !Eligible(in.Fields) {
		return in, "", nil
	}
	res, ok := s.Match(in.Fields)
	if !ok {
		return in, "", nil
	}
	return apply(in, res, v)
}

// ApplyPing runs the ping rules on a check-in event the ping API made. Like
// Apply, it returns the input unchanged, and an empty rule name, when no
// rule matches or the result is not a valid envelope.
func ApplyPing(s *Set, in *validate.Input, v *validate.Validator) (*validate.Input, string, error) {
	res, ok := s.MatchPing(in.Fields)
	if !ok {
		return in, "", nil
	}
	return apply(in, res, v)
}

func apply(in *validate.Input, res Result, v *validate.Validator) (*validate.Input, string, error) {
	f := make(map[string]any, len(in.Fields)+4)
	for k, x := range in.Fields {
		f[k] = x
	}
	for k, x := range res.Fields {
		if k == "project" && f["project"] != nil {
			// The sender's project wins over a rule's.
			continue
		}
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
	if res.Realert > 0 {
		own["realert_s"] = json.Number(strconv.FormatInt(int64(res.Realert/time.Second), 10))
	}
	details["heraldo"] = own
	f["details"] = details
	out, err := v.Fields(f)
	if err != nil {
		return in, "", fmt.Errorf("rule %s: %w", res.Rule, err)
	}
	return out, res.Rule, nil
}

// Type is what ingest runs on every checked input: Apply, then an input
// that is still untyped becomes kind message. The result always has a kind.
// err reports a rule that matched but made an invalid event; the input is
// then typed as a message all the same.
func Type(s *Set, in *validate.Input, v *validate.Validator) (*validate.Input, string, error) {
	out, rule, ruleErr := Apply(s, in, v)
	if out.Typed.Kind != nil {
		return out, rule, ruleErr
	}
	f := make(map[string]any, len(out.Fields)+1)
	for k, x := range out.Fields {
		f[k] = x
	}
	f["kind"] = string(api.KindMessage)
	msg, err := v.Fields(f)
	if err != nil {
		// The schema gives an untyped input the limits of a message, so
		// this means the two have drifted apart.
		return nil, "", fmt.Errorf("type an untyped input as message: %w", err)
	}
	return msg, "", ruleErr
}

// cut shortens s to n characters.
func cut(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

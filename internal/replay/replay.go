// Package replay runs an apprise-tee catalogue through a rules file in
// memory and reports how the rules classified it. It never opens a database.
//
// Incidents are paired the way the store derives them: an open, update or
// ack leaves the key open, and a resolve on an open key pairs with it. A
// resolve on a key that is not open is unpaired: its failure was never
// classified, or the rules gave the two different keys.
package replay

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/rules"
	"github.com/noahkiss/heraldo/internal/teeimport"
	"github.com/noahkiss/heraldo/internal/validate"
)

// Event is one replayed catalogue line, as the rules left it.
type Event struct {
	File  string
	Line  int
	TeeID string
	Ts    time.Time
	Rule  string // empty: no rule matched
	Kind  api.Kind
	Key   string
	Act   string
	Level string
	// Source and Tag group unclassified events. Tag is the first tag, or
	// empty.
	Source string
	Tag    string
	Title  string
	// Project is the project the event was filed under, or empty.
	Project string
}

// Group counts unclassified events from one source and tag.
type Group struct {
	Source, Tag string
	Count       int
	Example     string // the first title seen
}

// Report is the outcome of a replay.
type Report struct {
	Files, Lines, Bad int
	Events            []Event // in ts order
	// RuleErrors counts lines a rule matched that would not have made a
	// valid envelope; they stay plain messages.
	RuleErrors int
	// Hits counts events per rule, in file order of the rules.
	Hits []RuleHits
	// Recoveries are the incident resolves; Unpaired are those that found
	// no open incident under their key.
	Recoveries, Paired int
	Unpaired           []Event
	// StillOpen counts incident keys open at the end of the catalogue.
	StillOpen int
	// Unclassified groups the events no rule matched.
	Unclassified []Group
	// Projects counts events per project, by name.
	Projects []RuleHits
	// Unfiled groups the events left with no project.
	Unfiled []Group
}

// RuleHits is one rule's match count.
type RuleHits struct {
	Rule  string
	Count int
}

// Run replays the catalogue in dir through rs.
func Run(dir string, rs *rules.Set, v *validate.Validator) (*Report, error) {
	r := &Report{}
	files, err := teeimport.Each(dir, func(file string, n int, line []byte) error {
		r.Lines++
		m, err := teeimport.Classify(line, v, rs)
		if err != nil {
			r.Bad++
			return nil
		}
		if m.RuleErr != nil {
			r.RuleErrors++
		}
		in := m.Event.Typed
		e := Event{File: file, Line: n, TeeID: m.TeeID, Rule: m.Rule, Kind: *in.Kind, Source: in.Source}
		if in.Ts != nil {
			e.Ts = *in.Ts
		}
		if in.Key != nil {
			e.Key = *in.Key
		}
		if in.Action != nil {
			e.Act = string(*in.Action)
		}
		if in.Level != nil {
			e.Level = string(*in.Level)
		}
		if in.Tags != nil && len(*in.Tags) > 0 {
			e.Tag = (*in.Tags)[0]
		}
		if in.Title != nil {
			e.Title = *in.Title
		}
		if in.Project != nil {
			e.Project = *in.Project
		}
		r.Events = append(r.Events, e)
		return nil
	})
	r.Files = files
	if err != nil {
		return nil, err
	}
	sort.SliceStable(r.Events, func(i, j int) bool { return r.Events[i].Ts.Before(r.Events[j].Ts) })
	r.tally(rs)
	return r, nil
}

func (r *Report) tally(rs *rules.Set) {
	hits := map[string]int{}
	open := map[string]bool{}
	groups := map[[2]string]*Group{}
	projects := map[string]int{}
	unfiled := map[[2]string]*Group{}
	for _, e := range r.Events {
		if e.Project != "" {
			projects[e.Project]++
		} else {
			count(unfiled, e)
		}
		if e.Rule == "" {
			count(groups, e)
			continue
		}
		hits[e.Rule]++
		if e.Kind != api.KindIncident {
			continue
		}
		if e.Act == string(api.EventActionResolve) {
			r.Recoveries++
			if open[e.Key] {
				r.Paired++
			} else {
				r.Unpaired = append(r.Unpaired, e)
			}
			open[e.Key] = false
			continue
		}
		open[e.Key] = true
	}
	for _, o := range open {
		if o {
			r.StillOpen++
		}
	}
	if rs != nil {
		for _, rule := range rs.Rules {
			r.Hits = append(r.Hits, RuleHits{Rule: rule.Name, Count: hits[rule.Name]})
		}
	}
	r.Unclassified = sortGroups(groups)
	for p, n := range projects {
		r.Projects = append(r.Projects, RuleHits{Rule: p, Count: n})
	}
	sort.Slice(r.Projects, func(i, j int) bool { return r.Projects[i].Rule < r.Projects[j].Rule })
	r.Unfiled = sortGroups(unfiled)
}

// count adds e to its source and tag group.
func count(groups map[[2]string]*Group, e Event) {
	k := [2]string{e.Source, e.Tag}
	g := groups[k]
	if g == nil {
		g = &Group{Source: e.Source, Tag: e.Tag, Example: e.Title}
		groups[k] = g
	}
	g.Count++
}

// sortGroups lists groups biggest first.
func sortGroups(groups map[[2]string]*Group) []Group {
	var out []Group
	for _, g := range groups {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		return a.Tag < b.Tag
	})
	return out
}

// Classified counts the events a rule matched.
func (r *Report) Classified() int {
	n := 0
	for _, e := range r.Events {
		if e.Rule != "" {
			n++
		}
	}
	return n
}

// Write prints the report for a person to read.
func (r *Report) Write(w io.Writer) {
	fmt.Fprintf(w, "files %d, lines %d, events %d, bad lines %d\n", r.Files, r.Lines, len(r.Events), r.Bad)
	fmt.Fprintf(w, "classified %d of %d events", r.Classified(), len(r.Events))
	if r.RuleErrors > 0 {
		fmt.Fprintf(w, "; %d matched a rule that made an invalid event", r.RuleErrors)
	}
	fmt.Fprintln(w)

	kinds := map[api.Kind]int{}
	for _, e := range r.Events {
		kinds[e.Kind]++
	}
	var ks []string
	for k, n := range kinds {
		ks = append(ks, fmt.Sprintf("%s %d", k, n))
	}
	sort.Strings(ks)
	fmt.Fprintf(w, "kinds: %s\n", strings.Join(ks, ", "))

	pct := 100.0
	if r.Recoveries > 0 {
		pct = 100 * float64(r.Paired) / float64(r.Recoveries)
	}
	fmt.Fprintf(w, "\nrecoveries paired: %d / %d (%.1f%%); incidents still open at the end: %d\n",
		r.Paired, r.Recoveries, pct, r.StillOpen)
	if len(r.Unpaired) > 0 {
		fmt.Fprintln(w, "unpaired recoveries:")
		for _, e := range r.Unpaired {
			fmt.Fprintf(w, "  %s  %-24s %s\n", e.Ts.UTC().Format("2006-01-02 15:04"), e.Rule, e.Key)
		}
	}

	if len(r.Hits) > 0 {
		fmt.Fprintln(w, "\nrule hits:")
		for _, h := range r.Hits {
			fmt.Fprintf(w, "  %5d  %s\n", h.Count, h.Rule)
		}
	}

	fmt.Fprintf(w, "\nunclassified source/tag groups: %d\n", len(r.Unclassified))
	writeGroups(w, r.Unclassified)

	if len(r.Projects) > 0 {
		fmt.Fprintln(w, "\nprojects:")
		for _, p := range r.Projects {
			fmt.Fprintf(w, "  %5d  %s\n", p.Count, p.Rule)
		}
	}
	fmt.Fprintf(w, "\nsource/tag groups left without a project: %d\n", len(r.Unfiled))
	writeGroups(w, r.Unfiled)
}

func writeGroups(w io.Writer, groups []Group) {
	for _, g := range groups {
		tag := g.Tag
		if tag == "" {
			tag = "(no tag)"
		}
		fmt.Fprintf(w, "  %5d  %s / %s  e.g. %q\n", g.Count, g.Source, tag, g.Example)
	}
}

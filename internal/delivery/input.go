package delivery

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/noahkiss/heraldo/api"
)

// MatchFrom is an API match as a Match: text trimmed, the project folded
// to lower case as the server stores project names.
func MatchFrom(m api.DeliveryMatch) Match {
	val := func(p *string) string {
		if p == nil {
			return ""
		}
		return strings.TrimSpace(*p)
	}
	out := Match{Source: val(m.Source), Tag: val(m.Tag), Project: strings.ToLower(val(m.Project)),
		Rule: val(m.Rule), Key: val(m.Key)}
	if m.Kind != nil {
		out.Kind = *m.Kind
	}
	if m.Level != nil {
		out.Level = append([]api.Level{}, *m.Level...)
	}
	return out
}

// MatchJSON is m as the API serves it.
func MatchJSON(m Match) api.DeliveryMatch {
	opt := func(v string) *string {
		if v == "" {
			return nil
		}
		return &v
	}
	out := api.DeliveryMatch{Source: opt(m.Source), Tag: opt(m.Tag), Project: opt(m.Project), Rule: opt(m.Rule),
		Key: opt(m.Key)}
	if m.Kind != "" {
		k := m.Kind
		out.Kind = &k
	}
	if len(m.Level) > 0 {
		ls := append([]api.Level{}, m.Level...)
		out.Level = &ls
	}
	return out
}

// RowFromInput checks in and returns the row it describes, without id or
// times.
func RowFromInput(in api.DeliverySettingInput) (Row, error) {
	var d Row
	if in.Match != nil {
		d.Match = MatchFrom(*in.Match)
	}
	for _, v := range []struct {
		name string
		s    string
		n    int
	}{{"match.source", d.Match.Source, 128}, {"match.tag", d.Match.Tag, 64}, {"match.project", d.Match.Project, 64},
		{"match.rule", d.Match.Rule, 128}, {"match.key", d.Match.Key, 256}} {
		if len(v.s) > v.n {
			return d, fmt.Errorf("%s: at most %d bytes", v.name, v.n)
		}
	}
	if len(d.Match.Level) > 4 {
		return d, errors.New("match.level: at most 4 levels")
	}
	if in.Deliver != nil {
		d.Deliver = Outcome(*in.Deliver)
	}
	if in.Badge != nil {
		d.Badge = Badge(*in.Badge)
	}
	if in.DefaultLevel != nil {
		d.DefaultLevel = api.Level(*in.DefaultLevel)
	}
	if in.Position != nil {
		if *in.Position < 0 || *in.Position > 10000 {
			return d, errors.New("position: want 0 to 10000")
		}
		d.Position = *in.Position
	}
	if in.Note != nil {
		if len(*in.Note) > 256 {
			return d, errors.New("note: at most 256 bytes")
		}
		d.Note = strings.TrimSpace(*in.Note)
	}
	if d.Badge == "" {
		d.Badge = BadgeAuto
	}
	return d, d.Validate()
}

// ParseRows reads rows from JSON: a list of DeliverySettingInput, or the
// answer of GET /v1/delivery (an object with settings). Rows without an id
// get row0, row1, and so on, in the order given; the order is the order they
// run in.
func ParseRows(raw []byte) ([]Row, error) {
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		var obj struct {
			Settings []json.RawMessage `json:"settings"`
		}
		if err2 := json.Unmarshal(raw, &obj); err2 != nil {
			return nil, fmt.Errorf("want a list of rows or a GET /v1/delivery answer: %w", err)
		}
		list = obj.Settings
	}
	out := make([]Row, 0, len(list))
	for i, x := range list {
		var in api.DeliverySettingInput
		var id struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(x, &id)
		// A served row carries id and times; take the input fields from it.
		var loose map[string]json.RawMessage
		if err := json.Unmarshal(x, &loose); err != nil {
			return nil, fmt.Errorf("row %d: %w", i, err)
		}
		for _, k := range []string{"id", "created_at", "updated_at", "updated_by"} {
			delete(loose, k)
		}
		b, _ := json.Marshal(loose)
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			return nil, fmt.Errorf("row %d: %w", i, err)
		}
		r, err := RowFromInput(in)
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", i, err)
		}
		r.ID = id.ID
		if r.ID == "" {
			r.ID = fmt.Sprintf("row%d", i)
		}
		out = append(out, r)
	}
	return out, nil
}

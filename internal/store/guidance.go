package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Guidance is one guidance line: free text that informs the LLM's wording
// and grouping. It never sets a floor or overrides a rule.
type Guidance struct {
	ID      string
	Scope   string
	Text    string
	Enabled bool
	// Version starts at 1 and goes up on each edit.
	Version   int
	CreatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Guidance scope kinds. A scope is global, or a kind, a colon and a value.
const (
	GuidanceGlobal  = "global"
	GuidanceProject = "project"
	GuidanceSource  = "source"
	GuidanceKey     = "key"
)

// ParseGuidanceScope splits a scope into its kind and value and checks it:
// global, project:<p>, source:<s> or key:<regex>.
func ParseGuidanceScope(scope string) (kind, value string, err error) {
	scope = strings.TrimSpace(scope)
	if scope == GuidanceGlobal {
		return GuidanceGlobal, "", nil
	}
	kind, value, ok := strings.Cut(scope, ":")
	if !ok || value == "" {
		return "", "", fmt.Errorf("scope: want global, project:<p>, source:<s> or key:<regex>")
	}
	switch kind {
	case GuidanceProject, GuidanceSource:
		if len(value) > 128 {
			return "", "", fmt.Errorf("scope: %s at most 128 bytes", kind)
		}
	case GuidanceKey:
		if len(value) > 256 {
			return "", "", fmt.Errorf("scope: key regex at most 256 bytes")
		}
		if _, err := regexp.Compile(value); err != nil {
			return "", "", fmt.Errorf("scope: key regex: %v", err)
		}
	default:
		return "", "", fmt.Errorf("scope: %q is not global, project, source or key", kind)
	}
	return kind, value, nil
}

// Specificity orders scopes from least to most specific: global, source,
// project, key.
func (g Guidance) Specificity() int {
	kind, _, _ := ParseGuidanceScope(g.Scope)
	switch kind {
	case GuidanceGlobal:
		return 0
	case GuidanceSource:
		return 1
	case GuidanceProject:
		return 2
	default:
		return 3
	}
}

// Matches reports whether g applies to an event with project, source and
// key. A global line matches every event. An empty project, source or key
// matches only global lines and lines of the other kinds.
func (g Guidance) Matches(project, source, key string) bool {
	kind, value, err := ParseGuidanceScope(g.Scope)
	if err != nil {
		return false
	}
	switch kind {
	case GuidanceGlobal:
		return true
	case GuidanceProject:
		return project != "" && strings.EqualFold(value, project)
	case GuidanceSource:
		return source != "" && value == source
	case GuidanceKey:
		re, err := regexp.Compile(value)
		return err == nil && key != "" && re.MatchString(key)
	}
	return false
}

const guidanceCols = `id, scope, text, enabled, version, created_by, created_at, updated_at`

func scanGuidance(r scanner) (Guidance, error) {
	var g Guidance
	var created, updated string
	if err := r.Scan(&g.ID, &g.Scope, &g.Text, &g.Enabled, &g.Version, &g.CreatedBy, &created, &updated); err != nil {
		return Guidance{}, err
	}
	g.CreatedAt, g.UpdatedAt = parseTime(created), parseTime(updated)
	return g, nil
}

// AddGuidance stores g with version 1. g.ID and the times must be set.
func (s *Store) AddGuidance(ctx context.Context, g Guidance) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO guidance (`+guidanceCols+`) VALUES (?, ?, ?, ?, 1, ?, ?, ?)`,
		g.ID, g.Scope, g.Text, g.Enabled, g.CreatedBy, fmtTime(g.CreatedAt), fmtTime(g.UpdatedAt))
	return err
}

// PutGuidance replaces the scope, text and enabled flag of the line with
// g.ID, sets updated_at and raises its version by one. It returns the
// stored line.
func (s *Store) PutGuidance(ctx context.Context, g Guidance) (Guidance, error) {
	s.wmu.Lock()
	res, err := s.db.ExecContext(ctx, `UPDATE guidance SET scope = ?, text = ?, enabled = ?, version = version + 1,
		updated_at = ? WHERE id = ?`, g.Scope, g.Text, g.Enabled, fmtTime(g.UpdatedAt), g.ID)
	s.wmu.Unlock()
	if err != nil {
		return Guidance{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Guidance{}, ErrNotFound
	}
	return s.GetGuidance(ctx, g.ID)
}

// DeleteGuidance removes the line with id.
func (s *Store) DeleteGuidance(ctx context.Context, id string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	res, err := s.db.ExecContext(ctx, `DELETE FROM guidance WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetGuidance returns the line with id.
func (s *Store) GetGuidance(ctx context.Context, id string) (Guidance, error) {
	g, err := scanGuidance(s.db.QueryRowContext(ctx, `SELECT `+guidanceCols+` FROM guidance WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Guidance{}, ErrNotFound
	}
	return g, err
}

// AllGuidance returns every line, oldest first. With scope, only lines
// whose scope is exactly scope.
func (s *Store) AllGuidance(ctx context.Context, scope string) ([]Guidance, error) {
	q, args := `SELECT `+guidanceCols+` FROM guidance`, []any{}
	if scope != "" {
		q, args = q+` WHERE scope = ?`, append(args, scope)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Guidance
	for rows.Next() {
		g, err := scanGuidance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// MatchingGuidance returns the enabled lines that apply to an event with
// project, source and key, least specific first: global, source, project,
// then key. Within a kind, oldest first. With every argument empty, only
// global lines.
func (s *Store) MatchingGuidance(ctx context.Context, project, source, key string) ([]Guidance, error) {
	all, err := s.AllGuidance(ctx, "")
	if err != nil {
		return nil, err
	}
	var out []Guidance
	for _, g := range all {
		if g.Enabled && g.Matches(project, source, key) {
			out = append(out, g)
		}
	}
	// A stable sort keeps age order within a kind.
	slices.SortStableFunc(out, func(a, b Guidance) int { return a.Specificity() - b.Specificity() })
	return out, nil
}

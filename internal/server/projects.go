package server

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/store"
	"github.com/noahkiss/heraldo/internal/validate"
)

// projectFilter reads the project parameter: a comma-separated list, or the
// parameter repeated. Names fold to lower case; - is "no project". The
// answer holds the stored names to match: each name and its aliases. nil
// means no filter.
func (s *server) projectFilter(q url.Values) ([]string, error) {
	var names []string
	none := false
	for _, v := range q["project"] {
		for _, p := range strings.Split(v, ",") {
			p = validate.FoldProject(p)
			switch {
			case p == "":
			case p == store.NoProject:
				none = true
			case validate.ValidProject(p):
				names = append(names, p)
			default:
				return nil, fmt.Errorf("project: %q is not a project name", p)
			}
		}
	}
	if len(names) == 0 && !none {
		return nil, nil
	}
	out := []string{}
	if len(names) > 0 {
		out = s.Rules.Rules().Expand(names)
	}
	if none {
		out = append(out, store.NoProject)
	}
	return out, nil
}

// listProjects is GET /v1/projects: the projects the stored events name. A
// name the aliases map to another counts under that other name.
func (s *server) listProjects(w http.ResponseWriter, r *http.Request) {
	counts, none, err := s.Store.Projects(r.Context())
	if err != nil {
		s.Logger.Error("projects", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	set := s.Rules.Rules()
	merged := map[string]*api.Project{}
	for _, c := range counts {
		name := set.Alias(c.Name)
		p := merged[name]
		if p == nil {
			p = &api.Project{Name: name}
			merged[name] = p
		}
		p.Events += int(c.Events)
		if at := c.LastAt.UTC(); at.After(p.LastAt) {
			p.LastAt = at
		}
	}
	out := api.ProjectList{Projects: make([]api.Project, 0, len(merged)), Unfiled: int(none)}
	for _, p := range merged {
		out.Projects = append(out.Projects, *p)
	}
	sort.Slice(out.Projects, func(i, j int) bool { return out.Projects[i].Name < out.Projects[j].Name })
	writeJSON(w, http.StatusOK, out)
}

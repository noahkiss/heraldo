package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/auth"
	"github.com/noahkiss/heraldo/internal/ids"
	"github.com/noahkiss/heraldo/internal/routine"
	"github.com/noahkiss/heraldo/internal/store"
	"github.com/noahkiss/heraldo/internal/validate"
)

// Bundle window bounds.
const (
	MinBundleWindow = time.Minute
	MaxBundleWindow = 24 * time.Hour
)

func (s *server) viewRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/views", s.auth(auth.ScopeRead, false, s.listViews))
	mux.HandleFunc("POST /v1/views", s.auth(auth.ScopeWrite, false, s.createView))
	mux.HandleFunc("GET /v1/views/{id}", s.auth(auth.ScopeRead, false, s.getView))
	mux.HandleFunc("PUT /v1/views/{id}", s.auth(auth.ScopeWrite, false, s.putView))
	mux.HandleFunc("DELETE /v1/views/{id}", s.auth(auth.ScopeWrite, false, s.deleteView))
}

// ViewJSON is a stored view as the API serves it.
func ViewJSON(v store.View) api.View {
	out := api.View{Id: v.ID, Name: v.Name, Filter: viewFilterJSON(v.Filter), Position: v.Position,
		CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt}
	if v.Icon != "" {
		out.Icon = &v.Icon
	}
	if v.Bundle != nil {
		n := max(v.Bundle.MaxVisible, 1)
		out.Bundle = &api.ViewBundle{Window: v.Bundle.WindowString(), MaxVisible: &n}
	}
	return out
}

func viewFilterJSON(f store.ViewFilter) api.ViewFilter {
	var out api.ViewFilter
	opt := func(v string) *string {
		if v == "" {
			return nil
		}
		return &v
	}
	out.Source, out.Host, out.Tag, out.Q = opt(f.Source), opt(f.Host), opt(f.Tag), opt(f.Q)
	if f.Kind != "" {
		k := api.Kind(f.Kind)
		out.Kind = &k
	}
	if f.Routine != "" {
		r := api.ViewFilterRoutine(f.Routine)
		out.Routine = &r
	}
	if len(f.Project) > 0 {
		p := append([]string{}, f.Project...)
		out.Project = &p
	}
	if len(f.Level) > 0 {
		ls := make([]api.Level, len(f.Level))
		for i, l := range f.Level {
			ls[i] = api.Level(l)
		}
		out.Level = &ls
	}
	return out
}

// viewFromInput checks in and returns the view it describes, with problems
// a person reads.
func viewFromInput(in api.ViewInput) (store.View, []string) {
	var v store.View
	var problems []string
	v.Name = strings.TrimSpace(in.Name)
	switch {
	case v.Name == "" || len(v.Name) > 64:
		problems = append(problems, "name: want 1 to 64 bytes")
	case strings.HasPrefix(v.Name, "view_"):
		problems = append(problems, "name: must not start with view_")
	}
	if in.Position != nil {
		if *in.Position < 0 || *in.Position > 10000 {
			problems = append(problems, "position: want 0 to 10000")
		}
		v.Position = *in.Position
	}
	if in.Icon != nil {
		if len(*in.Icon) > 64 {
			problems = append(problems, "icon: at most 64 bytes")
		}
		v.Icon = strings.TrimSpace(*in.Icon)
	}
	if in.Bundle != nil {
		d, err := time.ParseDuration(in.Bundle.Window)
		switch {
		case err != nil:
			problems = append(problems, "bundle.window: want a duration such as 15m")
		case d < MinBundleWindow || d > MaxBundleWindow:
			problems = append(problems, "bundle.window: want 1m to 24h")
		}
		n := 1
		if in.Bundle.MaxVisible != nil {
			n = *in.Bundle.MaxVisible
		}
		if n < 1 || n > 20 {
			problems = append(problems, "bundle.max_visible: want 1 to 20")
		}
		v.Bundle = &store.ViewBundle{Window: d, MaxVisible: n}
	}
	if in.Filter != nil {
		f, ps := viewFilterFromInput(*in.Filter)
		v.Filter = f
		problems = append(problems, ps...)
	}
	return v, problems
}

func viewFilterFromInput(in api.ViewFilter) (store.ViewFilter, []string) {
	var f store.ViewFilter
	var problems []string
	str := func(name string, p *string, n int) string {
		if p == nil {
			return ""
		}
		v := strings.TrimSpace(*p)
		if len(v) > n {
			problems = append(problems, fmt.Sprintf("filter.%s: at most %d bytes", name, n))
		}
		return v
	}
	f.Source, f.Host, f.Tag, f.Q = str("source", in.Source, 128), str("host", in.Host, 128), str("tag", in.Tag, 64),
		str("q", in.Q, 256)
	if in.Kind != nil {
		if !in.Kind.Valid() {
			problems = append(problems, fmt.Sprintf("filter.kind: %q is not a kind", *in.Kind))
		}
		f.Kind = string(*in.Kind)
	}
	if in.Routine != nil {
		switch r := string(*in.Routine); r {
		case routine.Exclude, routine.Only:
			f.Routine = r
		default:
			problems = append(problems, "filter.routine: want exclude or only")
		}
	}
	if in.Project != nil {
		if len(*in.Project) > 32 {
			problems = append(problems, "filter.project: at most 32 names")
		}
		for _, p := range *in.Project {
			p = validate.FoldProject(p)
			switch {
			case p == "":
			case p == store.NoProject || validate.ValidProject(p):
				f.Project = append(f.Project, p)
			default:
				problems = append(problems, fmt.Sprintf("filter.project: %q is not a project name", p))
			}
		}
	}
	if in.Level != nil {
		for _, l := range *in.Level {
			if !l.Valid() {
				problems = append(problems, fmt.Sprintf("filter.level: %q is not a level", l))
				continue
			}
			f.Level = append(f.Level, string(l))
		}
	}
	return f, problems
}

func (s *server) decodeView(w http.ResponseWriter, r *http.Request) (store.View, bool) {
	var in api.ViewInput
	dec := json.NewDecoder(io.LimitReader(r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "want a ViewInput: %v", err)
		return store.View{}, false
	}
	v, problems := viewFromInput(in)
	if len(problems) > 0 {
		writeError(w, http.StatusBadRequest, "%s", strings.Join(problems, "; "))
		return store.View{}, false
	}
	return v, true
}

func (s *server) viewStoreErr(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, store.ErrViewName):
		writeError(w, http.StatusConflict, "name: another view has that name")
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "no view with that id or name")
	default:
		s.Logger.Error(what, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// listViews is GET /v1/views.
func (s *server) listViews(w http.ResponseWriter, r *http.Request) {
	vs, err := s.Store.Views(r.Context())
	if err != nil {
		s.viewStoreErr(w, "views", err)
		return
	}
	out := api.ViewList{Views: make([]api.View, 0, len(vs))}
	for _, v := range vs {
		out.Views = append(out.Views, ViewJSON(v))
	}
	writeJSON(w, http.StatusOK, out)
}

// createView is POST /v1/views.
func (s *server) createView(w http.ResponseWriter, r *http.Request) {
	v, ok := s.decodeView(w, r)
	if !ok {
		return
	}
	now := s.Now().UTC()
	v.ID, v.CreatedAt, v.UpdatedAt, v.CreatedBy = "view_"+ids.New(now), now, now, tokenFrom(r).Name
	if err := s.Store.AddView(r.Context(), v); err != nil {
		s.viewStoreErr(w, "add view", err)
		return
	}
	s.Logger.Info("view", "id", v.ID, "name", v.Name, "bundle", v.Bundle != nil, "token", v.CreatedBy)
	writeJSON(w, http.StatusCreated, ViewJSON(v))
}

// getView is GET /v1/views/{id}.
func (s *server) getView(w http.ResponseWriter, r *http.Request) {
	v, err := s.Store.GetView(r.Context(), r.PathValue("id"))
	if err != nil {
		s.viewStoreErr(w, "view", err)
		return
	}
	writeJSON(w, http.StatusOK, ViewJSON(v))
}

// putView is PUT /v1/views/{id}: replace every field.
func (s *server) putView(w http.ResponseWriter, r *http.Request) {
	old, err := s.Store.GetView(r.Context(), r.PathValue("id"))
	if err != nil {
		s.viewStoreErr(w, "view", err)
		return
	}
	v, ok := s.decodeView(w, r)
	if !ok {
		return
	}
	v.ID, v.CreatedAt, v.CreatedBy, v.UpdatedAt = old.ID, old.CreatedAt, old.CreatedBy, s.Now().UTC()
	if err := s.Store.PutView(r.Context(), v); err != nil {
		s.viewStoreErr(w, "put view", err)
		return
	}
	s.Logger.Info("view updated", "id", v.ID, "name", v.Name, "token", tokenFrom(r).Name)
	writeJSON(w, http.StatusOK, ViewJSON(v))
}

// deleteView is DELETE /v1/views/{id}.
func (s *server) deleteView(w http.ResponseWriter, r *http.Request) {
	v, err := s.Store.GetView(r.Context(), r.PathValue("id"))
	if err == nil {
		err = s.Store.DeleteView(r.Context(), v.ID)
	}
	if err != nil {
		s.viewStoreErr(w, "delete view", err)
		return
	}
	s.Logger.Info("view deleted", "id", v.ID, "name", v.Name, "token", tokenFrom(r).Name)
	w.WriteHeader(http.StatusNoContent)
}

// levelFilter reads the level parameter: a comma-separated list, or the
// parameter repeated.
func levelFilter(q url.Values) ([]string, error) {
	var out []string
	for _, v := range q["level"] {
		for _, l := range strings.Split(v, ",") {
			l = strings.TrimSpace(l)
			if l == "" {
				continue
			}
			if !api.Level(l).Valid() {
				return nil, fmt.Errorf("level: %q is not a level", l)
			}
			out = append(out, l)
		}
	}
	return out, nil
}

// applyView fills the fields of f that the query left empty from the saved
// view the view parameter names. It writes the error and returns false when
// the view is unknown.
func (s *server) applyView(w http.ResponseWriter, r *http.Request, f *store.Filter) bool {
	q := r.URL.Query()
	ref := q.Get("view")
	if ref == "" {
		return true
	}
	v, err := s.Store.GetView(r.Context(), ref)
	if err != nil {
		s.viewStoreErr(w, "view", err)
		return false
	}
	vf := v.Filter.Filter(s.Rules.Rules().Expand)
	fill := func(dst *string, param, from string) {
		if !q.Has(param) {
			*dst = from
		}
	}
	fill(&f.Source, "source", vf.Source)
	fill(&f.Kind, "kind", vf.Kind)
	fill(&f.Host, "host", vf.Host)
	fill(&f.Tag, "tag", vf.Tag)
	fill(&f.Q, "q", vf.Q)
	fill(&f.Routine, "routine", vf.Routine)
	if !q.Has("project") {
		f.Projects = vf.Projects
	}
	if !q.Has("level") {
		f.Levels = vf.Levels
	}
	w.Header().Set("Heraldo-View", v.ID)
	return true
}

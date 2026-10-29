package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/client"
)

const viewsUsage = `[ls] [--json] | save [flags] <name> | rm <name or id>`

// views lists, saves or removes saved views.
func (a *app) views(ctx context.Context, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "save":
			return a.viewSave(ctx, args[1:])
		case "rm":
			return a.viewRm(ctx, args[1:])
		case "ls":
			args = args[1:]
		}
	}
	fs := a.flags("views", viewsUsage)
	asJSON := fs.Bool("json", false, "print each view as JSON on one line")
	pos, err := parse(fs, args)
	if err := noPositional(fs, pos, err); err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	vs, err := c.Views(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		for _, v := range vs {
			a.printJSON(v)
		}
		return nil
	}
	if len(vs) == 0 {
		fmt.Fprintln(a.stderr, "no saved views")
		return nil
	}
	fmt.Fprintf(a.stdout, "%-24s %-12s %s\n", "NAME", "BUNDLE", "FILTER")
	for _, v := range vs {
		fmt.Fprintf(a.stdout, "%-24s %-12s %s\n", cutLine(oneLine(v.Name), 24), bundleText(v.Bundle), filterText(v.Filter))
	}
	return nil
}

// bundleText is a view's bundle for a person: "15m", "15m x2", or "-".
func bundleText(b *api.ViewBundle) string {
	if b == nil {
		return "-"
	}
	if b.MaxVisible != nil && *b.MaxVisible > 1 {
		return fmt.Sprintf("%s x%d", b.Window, *b.MaxVisible)
	}
	return b.Window
}

// filterText is a view's filter as key=value pairs.
func filterText(f api.ViewFilter) string {
	var parts []string
	add := func(k string, v *string) {
		if v != nil && *v != "" {
			parts = append(parts, k+"="+oneLine(*v))
		}
	}
	if f.Project != nil {
		parts = append(parts, "project="+strings.Join(*f.Project, ","))
	}
	add("source", f.Source)
	if f.Kind != nil {
		parts = append(parts, "kind="+string(*f.Kind))
	}
	add("host", f.Host)
	add("tag", f.Tag)
	if f.Level != nil {
		var ls []string
		for _, l := range *f.Level {
			ls = append(ls, string(l))
		}
		parts = append(parts, "level="+strings.Join(ls, ","))
	}
	add("q", f.Q)
	if f.Routine != nil {
		parts = append(parts, "routine="+string(*f.Routine))
	}
	if len(parts) == 0 {
		return "(every event)"
	}
	return strings.Join(parts, " ")
}

// viewSave makes a view, or replaces the one with that name.
func (a *app) viewSave(ctx context.Context, args []string) error {
	fs := a.flags("views save", "[flags] <name>")
	var f filters
	f.add(fs)
	q := fs.String("q", "", "full-text search over title, summary and body")
	level := fs.String("level", "", "only these levels, comma-separated: passive, active, time-sensitive, critical")
	withRoutine := fs.Bool("routine", false, "include routine events: passing check-ins, progress and status")
	bundle := fs.String("bundle", "", "fold a burst of the view's active pushes into one notification within this window, such as 15m")
	maxVisible := fs.Int("max-visible", 1, "with --bundle: how many pushes in a window go out on their own")
	icon := fs.String("icon", "", "an SF Symbol name the apps show")
	position := fs.Int("position", 0, "sort order in the apps, low first")
	asJSON := fs.Bool("json", false, "print the view as JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		fs.Usage()
		return errUsage
	}
	vf := api.ViewFilter{Source: optStr(f.source), Host: optStr(f.host), Tag: optStr(f.tag), Q: optStr(*q)}
	if f.key != "" {
		return fmt.Errorf("--key: a view does not filter by key")
	}
	if f.kind != "" {
		k := api.Kind(f.kind)
		vf.Kind = &k
	}
	if ps := projectList(f.project); len(ps) > 0 {
		vf.Project = &ps
	}
	if ls := projectList(*level); len(ls) > 0 {
		lv := make([]api.Level, len(ls))
		for i, l := range ls {
			lv[i] = api.Level(l)
		}
		vf.Level = &lv
	}
	if r := routineFilter(f, *withRoutine); r != "" {
		rt := api.ViewFilterRoutine(r)
		vf.Routine = &rt
	}
	in := api.ViewInput{Name: pos[0], Filter: &vf, Icon: optStr(*icon), Position: position}
	if *bundle != "" {
		in.Bundle = &api.ViewBundle{Window: *bundle, MaxVisible: maxVisible}
	}
	c, err := a.writeNeeded("saving a view")
	if err != nil {
		return err
	}
	verb := "saved"
	v, err := c.View(ctx, pos[0])
	switch {
	case err == nil:
		verb = "replaced"
		v, err = c.PutView(ctx, v.Id, in)
	case client.IsStatus(err, http.StatusNotFound):
		v, err = c.SaveView(ctx, in)
	}
	if client.IsStatus(err, http.StatusNotFound) || client.IsStatus(err, http.StatusMethodNotAllowed) {
		return client.ErrNoViews
	}
	if err != nil {
		return err
	}
	if *asJSON {
		a.printJSON(v)
		return nil
	}
	fmt.Fprintf(a.stdout, "%s view %s: %s", verb, v.Name, filterText(v.Filter))
	if v.Bundle != nil {
		fmt.Fprintf(a.stdout, "; bundles %s", bundleText(v.Bundle))
	}
	fmt.Fprintf(a.stdout, "\n%s\n", v.Id)
	return nil
}

func (a *app) viewRm(ctx context.Context, args []string) error {
	fs := a.flags("views rm", "<name or id>")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		fs.Usage()
		return errUsage
	}
	c, err := a.writeNeeded("removing a view")
	if err != nil {
		return err
	}
	if err := c.DeleteView(ctx, pos[0]); err != nil {
		if client.IsStatus(err, http.StatusNotFound) {
			return fmt.Errorf("no view %q (heraldo views lists them)", pos[0])
		}
		return err
	}
	fmt.Fprintf(a.stdout, "removed view %s\n", pos[0])
	return nil
}

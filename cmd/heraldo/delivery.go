package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/client"
	"github.com/noahkiss/heraldo/internal/delivery"
)

const deliveryUsage = `[ls] [--json] | add [flags] | edit <id> [flags] | rm <id> | preview [--file rows.json] [--days 30] [--json]`

// delivery lists, adds, edits, removes and previews the delivery settings.
// Reads use the read token; writes use HERALDO_ADMIN_TOKEN.
func (a *app) delivery(ctx context.Context, args []string) error {
	sub := "ls"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "ls", "list":
		return a.deliveryLs(ctx, args)
	case "add":
		return a.deliveryWrite(ctx, "", args)
	case "edit":
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			fmt.Fprintf(a.stderr, "usage: heraldo delivery edit <id> [flags]\n")
			return errUsage
		}
		return a.deliveryWrite(ctx, args[0], args[1:])
	case "rm":
		return a.deliveryRm(ctx, args)
	case "preview":
		return a.deliveryPreview(ctx, args)
	}
	fmt.Fprintf(a.stderr, "usage: heraldo delivery %s\n", deliveryUsage)
	return errUsage
}

func (a *app) adminClient() (*client.Client, error) {
	c, err := a.client()
	if err != nil {
		return nil, err
	}
	c.AdminToken = a.setting(envAdminToken)
	return c, nil
}

// deliveryErr adds what to do to a refused delivery call.
func deliveryErr(err error) error {
	switch {
	case client.IsStatus(err, http.StatusNotFound):
		return fmt.Errorf("%w (heraldo delivery ls lists the rows; a server before delivery settings has none)", err)
	case client.IsStatus(err, http.StatusForbidden):
		return fmt.Errorf("%w (delivery writes need an admin token in %s)", err, envAdminToken)
	case client.IsStatus(err, http.StatusConflict):
		return fmt.Errorf("%w (change the rows on the primary)", err)
	}
	return err
}

// matchText is a row's match for a person.
func matchText(m api.DeliveryMatch) string {
	var parts []string
	add := func(k string, v *string) {
		if v != nil && *v != "" {
			parts = append(parts, k+"="+*v)
		}
	}
	add("source", m.Source)
	add("tag", m.Tag)
	add("project", m.Project)
	add("rule", m.Rule)
	add("key", m.Key)
	if m.Kind != nil {
		parts = append(parts, "kind="+string(*m.Kind))
	}
	if m.Level != nil && len(*m.Level) > 0 {
		var ls []string
		for _, l := range *m.Level {
			ls = append(ls, string(l))
		}
		parts = append(parts, "level="+strings.Join(ls, ","))
	}
	if len(parts) == 0 {
		return "(every event)"
	}
	return strings.Join(parts, " ")
}

func (a *app) deliveryLs(ctx context.Context, args []string) error {
	fs := a.flags("delivery ls", "[--json]")
	asJSON := fs.Bool("json", false, "print each row as JSON on one line")
	pos, err := parse(fs, args)
	if err := noPositional(fs, pos, err); err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	l, err := c.Delivery(ctx)
	if err != nil {
		return deliveryErr(err)
	}
	if *asJSON {
		for _, d := range l.Settings {
			a.printJSON(d)
		}
		return nil
	}
	state := "off: the rows do nothing (heraldo settings set delivery.enabled true)"
	if l.Enabled {
		state = "on"
	}
	fmt.Fprintf(a.stdout, "delivery settings %s; a new source's level-less events get %s\n", state, l.NewSourceLevel)
	if len(l.Settings) == 0 {
		fmt.Fprintln(a.stderr, "no delivery settings")
		return nil
	}
	fmt.Fprintf(a.stdout, "%-30s %4s %-7s %-6s %-7s %s\n", "ID", "POS", "DELIVER", "BADGE", "DEFAULT", "MATCH")
	for _, d := range l.Settings {
		deliver, def := "-", "-"
		if d.Deliver != nil {
			deliver = string(*d.Deliver)
		}
		if d.DefaultLevel != nil {
			def = string(*d.DefaultLevel)
		}
		line := fmt.Sprintf("%-30s %4d %-7s %-6s %-7s %s", d.Id, d.Position, deliver, d.Badge, def, matchText(d.Match))
		if d.Note != nil {
			line += "  # " + oneLine(*d.Note)
		}
		fmt.Fprintln(a.stdout, line)
	}
	return nil
}

// rowFlags are the flags of delivery add and edit.
type rowFlags struct {
	source, tag, level, kind, project, rule, key, deliver, badge, def, note *string
	position                                                                *int
}

func addRowFlags(fs *flag.FlagSet) rowFlags {
	return rowFlags{
		source:   fs.String("source", "", "match: the event's source, without regard to case"),
		tag:      fs.String("tag", "", "match: any of the event's tags"),
		level:    fs.String("level", "", "match: levels, comma-separated"),
		kind:     fs.String("kind", "", "match: message, incident, checkin or digest"),
		project:  fs.String("project", "", "match: the event's project"),
		rule:     fs.String("rule", "", "match: the ingest rule that typed the event"),
		key:      fs.String("key", "", "match: a key prefix, such as checkin:"),
		deliver:  fs.String("deliver", "", "banner, silent, digest or drop"),
		badge:    fs.String("badge", "", "auto, on or off: count toward the badge"),
		def:      fs.String("default-level", "", "passive or active, for an event that carries no level"),
		note:     fs.String("note", "", "free text"),
		position: fs.Int("position", 0, "order, low first; the first matching row wins"),
	}
}

// input is the row the flags describe, over base: a flag given replaces
// base's field, and an empty value clears it.
func (f rowFlags) input(fs *flag.FlagSet, base api.DeliverySettingInput) api.DeliverySettingInput {
	in := base
	if in.Match == nil {
		in.Match = &api.DeliveryMatch{}
	}
	m := *in.Match
	set := map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	str := func(name string, v *string, dst **string) {
		if set[name] {
			*dst = optStr(strings.TrimSpace(*v))
		}
	}
	str("source", f.source, &m.Source)
	str("tag", f.tag, &m.Tag)
	str("project", f.project, &m.Project)
	str("rule", f.rule, &m.Rule)
	str("key", f.key, &m.Key)
	if set["kind"] {
		m.Kind = nil
		if *f.kind != "" {
			k := api.Kind(*f.kind)
			m.Kind = &k
		}
	}
	if set["level"] {
		m.Level = nil
		if *f.level != "" {
			var ls []api.Level
			for _, l := range strings.Split(*f.level, ",") {
				ls = append(ls, api.Level(strings.TrimSpace(l)))
			}
			m.Level = &ls
		}
	}
	in.Match = &m
	if set["deliver"] {
		in.Deliver = nil
		if *f.deliver != "" {
			o := api.DeliverOutcome(*f.deliver)
			in.Deliver = &o
		}
	}
	if set["badge"] {
		b := api.DeliveryBadge(*f.badge)
		in.Badge = &b
	}
	if set["default-level"] {
		in.DefaultLevel = nil
		if *f.def != "" {
			l := api.DeliveryDefaultLevel(*f.def)
			in.DefaultLevel = &l
		}
	}
	if set["note"] {
		in.Note = optStr(*f.note)
	}
	if set["position"] {
		in.Position = f.position
	}
	return in
}

// inputOf is a served row as an input, for edit.
func inputOf(d api.DeliverySetting) api.DeliverySettingInput {
	m := d.Match
	b := d.Badge
	p := d.Position
	return api.DeliverySettingInput{Match: &m, Deliver: d.Deliver, Badge: &b, DefaultLevel: d.DefaultLevel,
		Note: d.Note, Position: &p}
}

func (a *app) deliveryWrite(ctx context.Context, id string, args []string) error {
	name := "delivery add"
	if id != "" {
		name = "delivery edit <id>"
	}
	fs := a.flags(name, "[--source s] [--tag t] [--level l,l] [--kind k] [--project p] [--rule r] [--key prefix] [--deliver d] [--badge b] [--default-level l] [--position n] [--note text]")
	rf := addRowFlags(fs)
	asJSON := fs.Bool("json", false, "print the row as JSON")
	pos, err := parse(fs, args)
	if err := noPositional(fs, pos, err); err != nil {
		return err
	}
	c, err := a.adminClient()
	if err != nil {
		return err
	}
	var d api.DeliverySetting
	if id == "" {
		d, err = c.AddDelivery(ctx, rf.input(fs, api.DeliverySettingInput{}))
	} else {
		var cur api.DeliverySetting
		if cur, err = c.DeliveryRow(ctx, id); err == nil {
			d, err = c.PutDelivery(ctx, id, rf.input(fs, inputOf(cur)))
		}
	}
	if err != nil {
		return deliveryErr(err)
	}
	if *asJSON {
		a.printJSON(d)
		return nil
	}
	fmt.Fprintln(a.stdout, d.Id)
	return nil
}

func (a *app) deliveryRm(ctx context.Context, args []string) error {
	if len(args) != 1 {
		fmt.Fprintln(a.stderr, "usage: heraldo delivery rm <id>")
		return errUsage
	}
	c, err := a.adminClient()
	if err != nil {
		return err
	}
	return deliveryErr(c.DeleteDelivery(ctx, args[0]))
}

func (a *app) deliveryPreview(ctx context.Context, args []string) error {
	fs := a.flags("delivery preview", "[--file rows.json] [--days 30] [--limit 20] [--json]")
	file := fs.String("file", "", "rows to try: a JSON list of rows, or a GET /v1/delivery answer (default: the server's rows)")
	days := fs.Int("days", 30, "days of stored events, 1 to 90")
	limit := fs.Int("limit", 20, "the most changed events to list")
	asJSON := fs.Bool("json", false, "print the preview as JSON")
	pos, err := parse(fs, args)
	if err := noPositional(fs, pos, err); err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	in := api.DeliveryPreviewInput{Days: days, Limit: limit}
	if *file != "" {
		raw, err := os.ReadFile(*file)
		if err != nil {
			return err
		}
		rows, err := delivery.ParseRows(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", *file, err)
		}
		for _, r := range rows {
			m := delivery.MatchJSON(r.Match)
			x := api.DeliverySettingInput{Match: &m}
			if r.Deliver != "" {
				o := api.DeliverOutcome(r.Deliver)
				x.Deliver = &o
			}
			b := api.DeliveryBadge(r.Badge)
			x.Badge = &b
			if r.DefaultLevel != "" {
				l := api.DeliveryDefaultLevel(r.DefaultLevel)
				x.DefaultLevel = &l
			}
			in.Settings = append(in.Settings, x)
		}
	} else {
		l, err := c.Delivery(ctx)
		if err != nil {
			return deliveryErr(err)
		}
		for _, d := range l.Settings {
			in.Settings = append(in.Settings, inputOf(d))
		}
	}
	if in.Settings == nil {
		in.Settings = []api.DeliverySettingInput{}
	}
	p, err := c.PreviewDelivery(ctx, in)
	if err != nil {
		return deliveryErr(err)
	}
	if *asJSON {
		b, _ := json.Marshal(p)
		fmt.Fprintf(a.stdout, "%s\n", b)
		return nil
	}
	o := p.Outcomes
	fmt.Fprintf(a.stdout, "%d events in %d days: %d banner, %d silent, %d digest, %d drop, %d unchanged\n",
		p.Events, p.Days, o.Banner, o.Silent, o.Digest, o.Drop, o.Unchanged)
	fmt.Fprintf(a.stdout, "badge: %d join, %d leave\n", p.BadgeJoined, p.BadgeLeft)
	for _, r := range p.Rows {
		fmt.Fprintf(a.stdout, "row %d: %d hits, %d kept by a floor\n", r.Index, r.Hits, r.Refused)
	}
	for _, e := range p.Changed {
		title := ""
		if e.Title != nil {
			title = oneLine(*e.Title)
		}
		fmt.Fprintf(a.stdout, "%s %-20s %s -> %s %-6s badge %v->%v %s\n", e.Id, cutLine(e.Source, 20), e.LevelBefore,
			e.LevelAfter, e.As, e.BadgeBefore, e.BadgeAfter, cutLine(title, 60))
	}
	if p.Truncated {
		fmt.Fprintln(a.stderr, "more events changed; --limit lists more")
	}
	return nil
}

// ---- sources -----------------------------------------------------------

// sources lists the sources, new ones first, or reviews one.
func (a *app) sources(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "review" {
		if len(args) != 2 {
			fmt.Fprintln(a.stderr, "usage: heraldo sources review <source>")
			return errUsage
		}
		c, err := a.adminClient()
		if err != nil {
			return err
		}
		s, err := c.ReviewSource(ctx, args[1])
		if err != nil {
			return deliveryErr(err)
		}
		fmt.Fprintf(a.stdout, "%s reviewed\n", s.Source)
		return nil
	}
	if len(args) > 0 && args[0] == "ls" {
		args = args[1:]
	}
	fs := a.flags("sources", "[ls] [--json] | review <source>")
	asJSON := fs.Bool("json", false, "print each source as JSON on one line")
	pos, err := parse(fs, args)
	if err := noPositional(fs, pos, err); err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	srcs, err := c.Sources(ctx)
	if err != nil {
		return deliveryErr(err)
	}
	if *asJSON {
		for _, s := range srcs {
			a.printJSON(s)
		}
		return nil
	}
	fmt.Fprintf(a.stdout, "%-28s %-4s %6s %6s %6s %s\n", "SOURCE", "NEW", "30D", "BADGE", "QUIET", "LAST")
	for _, s := range srcs {
		n := ""
		if s.New {
			n = "new"
		}
		fmt.Fprintf(a.stdout, "%-28s %-4s %6d %6d %6d %s\n", cutLine(s.Source, 28), n, s.Events, s.BadgeIn, s.BadgeOut,
			localTime(s.LastSeen))
	}
	return nil
}

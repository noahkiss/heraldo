package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/client"
)

// printJSON prints v as one line of JSON.
func (a *app) printJSON(v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(a.stdout, "%s\n", b)
}

// noPositional parses flags and refuses positional arguments.
func noPositional(fs *flag.FlagSet, pos []string, err error) error {
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		fs.Usage()
		return errUsage
	}
	return nil
}

// writeNeeded is client, with the write token required for what.
func (a *app) writeNeeded(what string) (*client.Client, error) {
	c, err := a.client()
	if err != nil {
		return nil, err
	}
	if a.setting(envWriteToken) == "" {
		return nil, fmt.Errorf("%s is not set: %s needs a write token", envWriteToken, what)
	}
	return c, nil
}

func localTime(t time.Time) string { return t.Local().Format("01-02 15:04") }

// ---- mute and mutes ----------------------------------------------------

// mute is POST /v1/mutes.
func (a *app) mute(ctx context.Context, args []string) error {
	fs := a.flags("mute", "[--source s] [--key k] [--project p] [--for 1h | --until RFC3339]")
	source := fs.String("source", "", "mute events from this source")
	key := fs.String("key", "", "mute events with this key; checkin:<slug> mutes one check both ways")
	project := fs.String("project", "", "mute every event filed under this project")
	dur := fs.String("for", "", "how long, a Go duration such as 1h or 90m (default 1h)")
	until := fs.String("until", "", "when the mute ends, RFC 3339")
	asJSON := fs.Bool("json", false, "print the mute as JSON")
	pos, err := parse(fs, args)
	if err := noPositional(fs, pos, err); err != nil {
		return err
	}
	if *source == "" && *key == "" && *project == "" {
		return fmt.Errorf("give --source, --key, --project, or a mix")
	}
	if *dur != "" && *until != "" {
		return fmt.Errorf("give --for or --until, not both")
	}
	in := api.MuteInput{Source: optStr(*source), Key: optStr(*key), Project: optStr(*project)}
	switch {
	case *until != "":
		t, err := time.Parse(time.RFC3339, *until)
		if err != nil {
			return fmt.Errorf("--until: %v", err)
		}
		in.Until = &t
	case *dur != "":
		in.For = dur
	default:
		in.For = optStr("1h")
	}
	c, err := a.writeNeeded("mute")
	if err != nil {
		return err
	}
	m, err := c.Mute(ctx, in)
	if err != nil {
		return err
	}
	if *asJSON {
		a.printJSON(m)
		return nil
	}
	fmt.Fprintf(a.stdout, "muted %s until %s\n", muteWhat(m), localTime(m.Until))
	fmt.Fprintf(a.stdout, "%s\n", m.Id)
	return nil
}

func muteWhat(m api.Mute) string {
	var parts []string
	for _, f := range []struct {
		k string
		v *string
	}{{"source", m.Source}, {"key", m.Key}, {"project", m.Project}} {
		if f.v != nil {
			parts = append(parts, f.k+"="+oneLine(*f.v))
		}
	}
	if m.Remind != nil && *m.Remind {
		parts = append(parts, "(snooze)")
	}
	return strings.Join(parts, " ")
}

// mutes lists the mutes in force, or ends one.
func (a *app) mutes(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "end" {
		return a.muteEnd(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "ls" {
		args = args[1:]
	}
	fs := a.flags("mutes", "[ls] [--json] | end <id>")
	asJSON := fs.Bool("json", false, "print each mute as JSON on one line")
	pos, err := parse(fs, args)
	if err := noPositional(fs, pos, err); err != nil {
		return err
	}
	c, err := a.writeNeeded("listing mutes")
	if err != nil {
		return err
	}
	ms, err := c.Mutes(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		for _, m := range ms {
			a.printJSON(m)
		}
		return nil
	}
	if len(ms) == 0 {
		fmt.Fprintln(a.stderr, "no mutes in force")
		return nil
	}
	fmt.Fprintf(a.stdout, "%-31s %-11s %s\n", "ID", "UNTIL", "MUTES")
	for _, m := range ms {
		fmt.Fprintf(a.stdout, "%-31s %-11s %s\n", m.Id, localTime(m.Until), muteWhat(m))
	}
	return nil
}

func (a *app) muteEnd(ctx context.Context, args []string) error {
	fs := a.flags("mutes end", "<id>")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		fs.Usage()
		return errUsage
	}
	c, err := a.writeNeeded("ending a mute")
	if err != nil {
		return err
	}
	m, err := c.EndMute(ctx, pos[0])
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "ended %s (%s)\n", m.Id, muteWhat(m))
	return nil
}

// ---- hold --------------------------------------------------------------

const holdUsage = `[--pending] [--poll] [--json] | release --verdict V --title t --body b --tag t <id>... | suppress [--reason r] <id>...`

// hold shows the alert hold's queue, or closes entries in it.
func (a *app) hold(ctx context.Context, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "release":
			return a.holdRelease(ctx, args[1:])
		case "suppress":
			return a.holdSuppress(ctx, args[1:])
		}
	}
	fs := a.flags("hold", holdUsage)
	pending := fs.Bool("pending", false, "open entries only")
	poll := fs.Bool("poll", false, "count this call as a triage poll, which resets the dead-man; for a triage job only")
	asJSON := fs.Bool("json", false, "print the queue as JSON")
	pos, err := parse(fs, args)
	if err := noPositional(fs, pos, err); err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	q, err := c.Hold(ctx, client.HoldQuery{Pending: *pending, Poll: *poll})
	if err != nil {
		return err
	}
	if *asJSON {
		a.printJSON(q)
		return nil
	}
	dead := ""
	if q.DeadmanActive != nil && *q.DeadmanActive {
		dead = ", dead-man tripped"
	}
	fmt.Fprintf(a.stdout, "mode %s%s\n", q.Mode, dead)
	if len(q.Held) == 0 {
		fmt.Fprintln(a.stderr, "the hold is empty")
		return nil
	}
	fmt.Fprintf(a.stdout, "%-25s %-11s %-10s %-10s %s\n", "ID", "AT", "STATUS", "TAG", "TITLE")
	for _, h := range q.Held {
		at := time.Unix(int64(h.Ts), 0)
		fmt.Fprintf(a.stdout, "%-25s %-11s %-10s %-10s %s\n", h.Id, localTime(at), h.Status,
			oneLine(str(h.Tag)), cutLine(oneLine(str(h.Title)), 80))
	}
	return nil
}

func (a *app) holdRelease(ctx context.Context, args []string) error {
	fs := a.flags("hold release", "--verdict REAL|BLIP|MAINTENANCE|FALSE --title t --body b --tag t <id>...")
	verdict := fs.String("verdict", "", "REAL, BLIP, MAINTENANCE or FALSE")
	title := fs.String("title", "", "the diagnosis title")
	body := fs.String("body", "", "the diagnosis text")
	tagName := fs.String("tag", "", "the tag the message is stored at, such as general")
	ids, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(ids) == 0 || *verdict == "" || *title == "" || *tagName == "" {
		fs.Usage()
		return errUsage
	}
	c, err := a.writeNeeded("releasing held entries")
	if err != nil {
		return err
	}
	r, err := c.HoldRelease(ctx, api.HoldReleaseJSONBody{Ids: ids,
		Verdict: api.HoldReleaseJSONBodyVerdict(strings.ToUpper(*verdict)),
		Title:   *title, Body: *body, Tag: *tagName})
	if err != nil {
		return err
	}
	sent := "recorded only (mode " + r.Mode + ")"
	if r.Sent {
		sent = "sent " + r.EventID
	}
	fmt.Fprintf(a.stdout, "released %d, %s\n", len(r.Released), sent)
	a.holdLeftover(r)
	return nil
}

func (a *app) holdSuppress(ctx context.Context, args []string) error {
	fs := a.flags("hold suppress", "[--reason r] <id>...")
	reason := fs.String("reason", "", "why, such as BLIP: recovered before triage looked")
	ids, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		fs.Usage()
		return errUsage
	}
	c, err := a.writeNeeded("suppressing held entries")
	if err != nil {
		return err
	}
	r, err := c.HoldSuppress(ctx, api.HoldSuppressJSONBody{Ids: ids, Reason: optStr(*reason)})
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "suppressed %d\n", len(r.Suppressed))
	a.holdLeftover(r)
	return nil
}

func (a *app) holdLeftover(r client.HoldResult) {
	if len(r.UnknownOrClosed) > 0 {
		fmt.Fprintf(a.stderr, "not open, left alone: %s\n", strings.Join(r.UnknownOrClosed, " "))
	}
}

// ---- devices -----------------------------------------------------------

// devices lists the registered devices, or retires one.
func (a *app) devices(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "retire" {
		return a.deviceRetire(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "ls" {
		args = args[1:]
	}
	fs := a.flags("devices", "[ls] [--json] | retire <id>")
	asJSON := fs.Bool("json", false, "print each device as JSON on one line")
	pos, err := parse(fs, args)
	if err := noPositional(fs, pos, err); err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	ds, err := c.Devices(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		for _, d := range ds {
			a.printJSON(d)
		}
		return nil
	}
	if len(ds) == 0 {
		fmt.Fprintln(a.stderr, "no devices")
		return nil
	}
	fmt.Fprintf(a.stdout, "%-28s %-24s %-8s %-11s %-9s %s\n", "ID", "NAME", "PLATFORM", "ENV", "TOKEN", "STATE")
	for _, d := range ds {
		state := "active"
		if !d.Active {
			state = "retired " + oneLine(str(d.RetiredReason))
		}
		if f := client.FocusText(d.FocusFilter); f != "" {
			state += ", " + oneLine(f)
		}
		fmt.Fprintf(a.stdout, "%-28s %-24s %-8s %-11s %-9s %s\n", d.Id, cutLine(oneLine(str(d.Name)), 24),
			oneLine(str(d.Platform)), d.Environment, "…"+d.TokenSuffix, state)
	}
	return nil
}

func (a *app) deviceRetire(ctx context.Context, args []string) error {
	fs := a.flags("devices retire", "<id>")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		fs.Usage()
		return errUsage
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	d, err := c.RetireDevice(ctx, pos[0])
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "retired %s (%s); registering its token again brings it back\n", d.Id, oneLine(str(d.Name)))
	return nil
}

// ---- projects ----------------------------------------------------------

func (a *app) projects(ctx context.Context, args []string) error {
	fs := a.flags("projects", "[--json]")
	asJSON := fs.Bool("json", false, "print the list as JSON")
	pos, err := parse(fs, args)
	if err := noPositional(fs, pos, err); err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	l, err := c.Projects(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		a.printJSON(l)
		return nil
	}
	fmt.Fprintf(a.stdout, "%-32s %8s  %s\n", "PROJECT", "EVENTS", "LAST")
	for _, p := range l.Projects {
		fmt.Fprintf(a.stdout, "%-32s %8d  %s\n", oneLine(p.Name), p.Events, localTime(p.LastAt))
	}
	fmt.Fprintf(a.stdout, "%-32s %8d\n", "-", l.Unfiled)
	return nil
}

// ---- state -------------------------------------------------------------

func (a *app) state(ctx context.Context, args []string) error {
	fs := a.flags("state", "[--project names] [--json]")
	project := fs.String("project", "", "only these projects, comma-separated; - for none")
	asJSON := fs.Bool("json", false, "print the state as JSON")
	pos, err := parse(fs, args)
	if err := noPositional(fs, pos, err); err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	st, err := c.StateIn(ctx, projectList(*project))
	if err != nil {
		return stateErr(err)
	}
	if *asJSON {
		a.printJSON(st)
		return nil
	}
	if st.Badge != nil {
		fmt.Fprintf(a.stdout, "badge: %d\n", *st.Badge)
	}
	for _, sec := range []struct {
		name string
		evs  []client.Event
	}{{"incidents", st.Incidents}, {"progress", st.Progress}, {"checkins", st.Checkins}, {"statuses", st.Statuses}} {
		fmt.Fprintf(a.stdout, "%s (%d)\n", sec.name, len(sec.evs))
		for _, e := range sec.evs {
			fmt.Fprint(a.stdout, "  ")
			line(a.stdout, e)
		}
	}
	return nil
}

// ---- activities --------------------------------------------------------

func (a *app) activities(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "show" {
		return a.activityShow(ctx, args[1:])
	}
	fs := a.flags("activities", "[--all] [--json] | show <key>")
	all := fs.Bool("all", false, "ended activities too")
	asJSON := fs.Bool("json", false, "print each activity as JSON on one line")
	pos, err := parse(fs, args)
	if err := noPositional(fs, pos, err); err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	as, err := c.Activities(ctx, *all)
	if err != nil {
		return err
	}
	if *asJSON {
		for _, x := range as {
			a.printJSON(x)
		}
		return nil
	}
	if len(as) == 0 {
		fmt.Fprintln(a.stderr, "no Live Activities")
		return nil
	}
	fmt.Fprintf(a.stdout, "%-32s %-7s %-11s %-5s %s\n", "KEY", "STATE", "UPDATED", "DONE", "STATUS")
	for _, x := range as {
		fmt.Fprintf(a.stdout, "%-32s %-7s %-11s %-5s %s\n", oneLine(x.Key), x.State, localTime(x.UpdatedAt),
			progressText(x.Content), cutLine(oneLine(liveText(x.Content)), 60))
	}
	return nil
}

func progressText(c api.LiveContent) string {
	if p, err := c.Progress.Get(); err == nil {
		return fmt.Sprintf("%.0f%%", p*100)
	}
	return ""
}

// liveText is the title and status of a Live Activity's content.
func liveText(c api.LiveContent) string {
	title, _ := c.Title.Get()
	status, _ := c.Status.Get()
	switch {
	case title != "" && status != "":
		return title + ": " + status
	case title != "":
		return title
	}
	return status
}

func (a *app) activityShow(ctx context.Context, args []string) error {
	fs := a.flags("activities show", "[--json] <key>")
	asJSON := fs.Bool("json", false, "print the activity as JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		fs.Usage()
		return errUsage
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	x, err := c.Activity(ctx, pos[0])
	if err != nil {
		return err
	}
	if *asJSON {
		a.printJSON(x)
		return nil
	}
	row := func(k, v string) {
		if v != "" {
			fmt.Fprintf(a.stdout, "%-9s %s\n", k+":", oneLine(v))
		}
	}
	row("key", x.Key)
	row("id", x.Id)
	row("state", string(x.State))
	row("source", x.Source)
	row("content", strings.TrimSpace(liveText(x.Content)+" "+progressText(x.Content)))
	row("started", localTime(x.StartedAt))
	row("updated", localTime(x.UpdatedAt))
	if x.EndedAt != nil {
		row("ended", localTime(*x.EndedAt))
	}
	if x.EndReason != nil {
		row("reason", string(*x.EndReason))
	}
	for _, in := range x.Instances {
		st := "running"
		if in.EndedAt != nil {
			st = "ended " + str(in.EndReason)
		}
		fmt.Fprintf(a.stdout, "device:  %s chain %d %s\n", oneLine(in.Device), in.Chain, oneLine(st))
	}
	if x.Timeline != nil {
		tl := *x.Timeline
		if len(tl) > 20 {
			fmt.Fprintf(a.stdout, "timeline: newest 20 of %d\n", len(tl))
			tl = tl[len(tl)-20:]
		}
		for _, t := range tl {
			text := ""
			if t.Content != nil {
				text = strings.TrimSpace(liveText(*t.Content) + " " + progressText(*t.Content))
			}
			fmt.Fprintf(a.stdout, "  %s %s %-6s %s\n", t.Id, localTime(t.ReceivedAt), t.Action, oneLine(text))
		}
	}
	return nil
}

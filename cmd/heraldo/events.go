package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/client"
)

// filters are the event filters ls and tail share.
type filters struct {
	kind, source, host, key, tag string
}

func (f *filters) add(fs interface {
	StringVar(*string, string, string, string)
}) {
	fs.StringVar(&f.kind, "kind", "", "only this kind: message, digest, incident, progress, checkin, status")
	fs.StringVar(&f.source, "source", "", "only this source")
	fs.StringVar(&f.host, "host", "", "only this host")
	fs.StringVar(&f.key, "key", "", "only this key")
	fs.StringVar(&f.tag, "tag", "", "only events with this tag")
}

func (f filters) empty() bool { return f == filters{} }

// match applies the filters on the client, for tail, which reads every event.
func (f filters) match(e client.Event) bool {
	if f.kind != "" && string(e.Kind) != f.kind {
		return false
	}
	if f.source != "" && e.Source != f.source {
		return false
	}
	if f.host != "" && str(e.Host) != f.host {
		return false
	}
	if f.key != "" && str(e.Key) != f.key {
		return false
	}
	if f.tag != "" {
		if e.Tags == nil {
			return false
		}
		found := false
		for _, t := range *e.Tags {
			found = found || t == f.tag
		}
		return found
	}
	return true
}

func reverse(evs []client.Event) {
	for i, j := 0, len(evs)-1; i < j; i, j = i+1, j-1 {
		evs[i], evs[j] = evs[j], evs[i]
	}
}

// newest returns the newest limit events that match q, oldest first.
func newest(ctx context.Context, c *client.Client, q client.Query, limit int) ([]client.Event, error) {
	q.Desc = true
	q.Limit = limit
	p, err := c.List(ctx, q)
	if errors.Is(err, client.ErrNoDesc) {
		return newestForward(ctx, c, q, limit)
	}
	if err != nil {
		return nil, err
	}
	reverse(p.Events)
	return p.Events, nil
}

// newestForward is newest for a server that lists oldest first only: it
// pages through every event and keeps the last limit. It applies hold on
// the client, since such a server ignores it.
func newestForward(ctx context.Context, c *client.Client, q client.Query, limit int) ([]client.Event, error) {
	hold := q.Hold
	q.Desc, q.Hold, q.Limit, q.After = false, "", 500, ""
	var keep []client.Event
	for {
		p, err := c.List(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, e := range p.Events {
			if hold != "" {
				if e.Hold == nil {
					continue
				}
				if s, _ := (*e.Hold)["state"].(string); s != hold {
					continue
				}
			}
			keep = append(keep, e)
		}
		if len(keep) > limit {
			keep = append([]client.Event(nil), keep[len(keep)-limit:]...)
		}
		if len(p.Events) < q.Limit || p.NextCursor == "" {
			return keep, nil
		}
		q.After = p.NextCursor
	}
}

// newestWithCursor returns the newest limit events, oldest first, and the
// cursor just after the newest of them. A one-event page, newest first,
// carries the newest event's cursor.
func newestWithCursor(ctx context.Context, c *client.Client, limit int) ([]client.Event, string, error) {
	head, err := c.List(ctx, client.Query{Desc: true, Limit: 1})
	if err != nil || len(head.Events) == 0 {
		return nil, "", err
	}
	evs := head.Events
	if limit > 1 {
		rest, err := c.List(ctx, client.Query{Desc: true, Before: head.NextCursor, Limit: limit - 1})
		if err != nil {
			return nil, "", err
		}
		evs = append(evs, rest.Events...)
	}
	reverse(evs)
	return evs, head.NextCursor, nil
}

func (a *app) print(evs []client.Event, asJSON bool) {
	if asJSON {
		writeJSONLines(a.stdout, evs)
		return
	}
	for _, e := range evs {
		line(a.stdout, e)
	}
}

// ---- ls ----------------------------------------------------------------

func (a *app) ls(ctx context.Context, args []string) error {
	fs := a.flags("ls", "[flags]")
	var f filters
	f.add(fs)
	q := fs.String("q", "", "full-text search over title, summary and body")
	open := fs.Bool("open", false, "open incidents only: those not resolved, newest event per key")
	limit := fs.Int("limit", 20, "how many events, 1 to 500")
	asJSON := fs.Bool("json", false, "print each event's JSON on one line")
	if pos, err := parse(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		fs.Usage()
		return errUsage
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	if *open {
		st, err := c.State(ctx)
		if err != nil {
			return stateErr(err)
		}
		var evs []client.Event
		for _, e := range st.Incidents {
			if f.match(e) {
				evs = append(evs, e)
			}
		}
		// The state lists incidents by key. Keep the limit with the most
		// recent last events, and print them oldest first, as ls does.
		sort.SliceStable(evs, func(i, j int) bool { return evs[i].ReceivedAt.Before(evs[j].ReceivedAt) })
		if len(evs) > *limit {
			evs = evs[len(evs)-*limit:]
		}
		a.print(evs, *asJSON)
		if len(evs) == 0 && !*asJSON {
			fmt.Fprintln(a.stderr, "no open incidents")
		}
		return nil
	}
	evs, err := newest(ctx, c, client.Query{
		Kind: f.kind, Source: f.source, Host: f.host, Key: f.key, Tag: f.tag, Q: *q,
	}, *limit)
	if err != nil {
		return listErr(err)
	}
	a.print(evs, *asJSON)
	return nil
}

// listErr explains a 400 from a server that does not know order=desc yet.
func listErr(err error) error {
	if client.IsStatus(err, http.StatusBadRequest) {
		return fmt.Errorf("%w (a server older than the CLI may not know this query)", err)
	}
	return err
}

func stateErr(err error) error {
	if client.IsStatus(err, http.StatusNotFound) || client.IsStatus(err, http.StatusMethodNotAllowed) {
		return fmt.Errorf("the server has no /v1/state yet; it needs a newer heraldo-server")
	}
	return err
}

// ---- show --------------------------------------------------------------

func (a *app) show(ctx context.Context, args []string) error {
	fs := a.flags("show", "[--json] <id>")
	asJSON := fs.Bool("json", false, "print the stored JSON")
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
	e, err := c.Get(ctx, pos[0])
	if err != nil {
		return err
	}
	if *asJSON {
		fmt.Fprintf(a.stdout, "%s\n", e.Raw)
		return nil
	}
	detail(a.stdout, e)
	return nil
}

// ---- peek --------------------------------------------------------------

func (a *app) peek(ctx context.Context, args []string) error {
	fs := a.flags("peek", "[--mark] [--limit n] [--json]")
	mark := fs.Bool("mark", false, "move the read cursor past the events shown")
	limit := fs.Int("limit", 50, "most events to show, 1 to 500")
	asJSON := fs.Bool("json", false, "print each event's JSON on one line")
	if pos, err := parse(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		fs.Usage()
		return errUsage
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	name := a.cursorName()
	cur, found, err := c.Cursor(ctx, name)
	if client.IsStatus(err, http.StatusNotFound) {
		return fmt.Errorf("the server has no read cursors yet; it needs a newer heraldo-server")
	}
	if err != nil {
		return err
	}
	var evs []client.Event
	// last is the cursor just after the last event shown.
	last := ""
	more := false
	if !found {
		// No cursor yet: show the newest events rather than all history.
		if evs, last, err = newestWithCursor(ctx, c, *limit); err != nil {
			return listErr(err)
		}
		if !*asJSON {
			fmt.Fprintf(a.stderr, "no read cursor %q yet; the newest %d events. heraldo peek --mark starts one.\n",
				name, len(evs))
		}
	} else {
		p, err := c.List(ctx, client.Query{After: cur, Limit: *limit})
		if err != nil {
			return err
		}
		evs, last = p.Events, p.NextCursor
		if len(evs) == *limit {
			next, err := c.List(ctx, client.Query{After: p.NextCursor, Limit: 1})
			more = err == nil && len(next.Events) > 0
		}
	}
	a.print(evs, *asJSON)
	if !*asJSON {
		switch {
		case found && len(evs) == 0:
			fmt.Fprintln(a.stderr, "nothing unread")
		case more:
			fmt.Fprintf(a.stderr, "more unread after these %d; peek --mark, then peek again\n", len(evs))
		}
	}
	if *mark && last != "" {
		stored, err := c.SetCursor(ctx, name, last)
		if err != nil {
			return err
		}
		if !*asJSON {
			fmt.Fprintf(a.stderr, "read cursor %q at %s\n", name, stored)
		}
	}
	return nil
}

// ---- held --------------------------------------------------------------

func (a *app) held(ctx context.Context, args []string) error {
	fs := a.flags("held", "[--limit n] [--json]")
	limit := fs.Int("limit", 20, "how many events, 1 to 500")
	asJSON := fs.Bool("json", false, "print each event's JSON on one line")
	if pos, err := parse(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		fs.Usage()
		return errUsage
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	evs, err := newest(ctx, c, client.Query{Hold: "held"}, *limit)
	if err != nil {
		return listErr(err)
	}
	a.print(evs, *asJSON)
	if len(evs) == 0 && !*asJSON {
		fmt.Fprintln(a.stderr, "nothing held")
	}
	return nil
}

// ---- checkins ----------------------------------------------------------

func (a *app) checkins(ctx context.Context, args []string) error {
	fs := a.flags("checkins", "[--json]")
	asJSON := fs.Bool("json", false, "print each check's last event as JSON on one line")
	if pos, err := parse(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		fs.Usage()
		return errUsage
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	st, err := c.State(ctx)
	if err != nil {
		return stateErr(err)
	}
	if *asJSON {
		writeJSONLines(a.stdout, st.Checkins)
		return nil
	}
	if len(st.Checkins) == 0 {
		fmt.Fprintln(a.stderr, "no check-ins")
		return nil
	}
	for _, e := range st.Checkins {
		action := "ping"
		if e.Action != nil {
			action = string(*e.Action)
		}
		if e.Checkin != nil && e.Checkin.ExitCode != nil && *e.Checkin.ExitCode != 0 && action != "log" {
			action = fmt.Sprintf("fail (exit %d)", *e.Checkin.ExitCode)
		}
		fmt.Fprintf(a.stdout, "%-32s %-14s %s  %s\n", oneLine(str(e.Key)), action,
			when(e).Local().Format("01-02 15:04"), oneLine(e.Source))
	}
	return nil
}

// ---- ack and resolve ---------------------------------------------------

func (a *app) ack(ctx context.Context, args []string) error {
	return a.incident(ctx, "ack", args)
}

func (a *app) resolve(ctx context.Context, args []string) error {
	return a.incident(ctx, "resolve", args)
}

// incident posts an incident event with action ack or resolve for an open
// incident's key. The server's incident state follows from the event.
func (a *app) incident(ctx context.Context, action string, args []string) error {
	fs := a.flags(action, "[-m note] [--force] <key>")
	note := fs.String("m", "", "a note, stored as the event's body")
	force := fs.Bool("force", false, "post even when no open incident has the key")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		fs.Usage()
		return errUsage
	}
	key := pos[0]
	c, err := a.client()
	if err != nil {
		return err
	}
	if a.setting(envWriteToken) == "" {
		return fmt.Errorf("%s is not set: %s posts an event, which needs a write token", envWriteToken, action)
	}
	var open *client.Event
	if !*force {
		st, err := c.State(ctx)
		if err != nil {
			return stateErr(err)
		}
		for i, e := range st.Incidents {
			if str(e.Key) == key {
				open = &st.Incidents[i]
			}
		}
		if open == nil {
			return fmt.Errorf("no open incident has the key %q; --force posts anyway", key)
		}
		if action == "ack" && open.Action != nil && *open.Action == api.EventAction("ack") {
			fmt.Fprintf(a.stderr, "%s is already acknowledged\n", key)
			return nil
		}
	}
	source := a.setting(envSource)
	if source == "" {
		source = "heraldo-cli"
	}
	in := map[string]any{
		"source": source, "kind": "incident", "key": key, "action": action, "level": "passive",
	}
	verb := map[string]string{"ack": "Acknowledged", "resolve": "Resolved"}[action]
	title := verb + ": " + key
	if open != nil {
		if h := headline(*open); h != "" {
			title = verb + ": " + h
		}
		if open.Host != nil {
			in["host"] = *open.Host
		}
	}
	if len(title) > 256 {
		title = title[:256]
	}
	in["title"] = title
	if *note != "" {
		in["body"] = *note
	}
	e, err := c.Post(ctx, in)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "%s %s (%s)\n", strings.ToLower(verb), key, e.Id)
	return nil
}

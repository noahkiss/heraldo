package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/noahkiss/heraldo/internal/client"
)

// execTimeout caps one run of a tail --exec command.
const execTimeout = time.Minute

// tail follows new events: poke, then fetch. The server pokes over
// /v1/stream; the CLI then lists every event after its cursor. A server
// without the stream is polled.
func (a *app) tail(ctx context.Context, args []string) error {
	fs := a.flags("tail", "[filters] [--unread] [--mark] [--exec command] [--json]")
	var f filters
	f.add(fs)
	unread := fs.Bool("unread", false, "start at the read cursor, not at the newest event")
	mark := fs.Bool("mark", false, "move the read cursor as events are shown; not with filters")
	execCmd := fs.String("exec", "", "run this command once per event shown; it must be listed in the exec-allow file")
	asJSON := fs.Bool("json", false, "print each event's JSON on one line")
	if pos, err := parse(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		fs.Usage()
		return errUsage
	}
	if *mark && !f.empty() {
		return fmt.Errorf("--mark with filters would mark events it did not show")
	}
	cmd := ""
	if *execCmd != "" {
		var err error
		if cmd, err = a.allowedCommand(*execCmd); err != nil {
			return err
		}
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	name := a.cursorName()
	t := &tailer{a: a, c: c, f: f, name: name, mark: *mark, cmd: cmd, asJSON: *asJSON}
	if *unread {
		cur, found, err := c.Cursor(ctx, name)
		if err != nil {
			return err
		}
		if found {
			t.cursor = cur
			t.started = true
		}
	}
	if !t.started {
		head, err := headCursor(ctx, c)
		if err != nil {
			return listErr(err)
		}
		t.cursor, t.started = head, true
	}
	if err := t.fetch(ctx); err != nil {
		return err
	}
	backoff := time.Second
	for {
		err := c.Stream(ctx, func(string) error {
			backoff = time.Second
			return t.fetch(ctx)
		})
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, client.ErrNoStream) {
			fmt.Fprintf(a.stderr, "heraldo tail: the server has no event stream; polling every %s\n", a.pollEvery)
			return t.poll(ctx)
		}
		var fatal *client.Error
		if errors.As(err, &fatal) && (fatal.Status == 401 || fatal.Status == 403) {
			return err
		}
		fmt.Fprintf(a.stderr, "heraldo tail: stream: %v; reconnecting in %s\n", err, backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, a.retryMax)
	}
}

// headCursor is the cursor just after the newest event.
func headCursor(ctx context.Context, c *client.Client) (string, error) {
	_, head, err := newestWithCursor(ctx, c, 1)
	if !errors.Is(err, client.ErrNoDesc) {
		return head, err
	}
	// A server that lists oldest first only: page to the end.
	q := client.Query{Limit: 500}
	for {
		p, err := c.List(ctx, q)
		if err != nil {
			return "", err
		}
		if p.NextCursor != "" {
			head = p.NextCursor
		}
		if len(p.Events) < q.Limit {
			return head, nil
		}
		q.After = p.NextCursor
	}
}

type tailer struct {
	a       *app
	c       *client.Client
	f       filters
	name    string
	mark    bool
	cmd     string
	asJSON  bool
	cursor  string
	started bool
}

func (t *tailer) poll(ctx context.Context) error {
	tick := time.NewTicker(t.a.pollEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if err := t.fetch(ctx); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				fmt.Fprintf(t.a.stderr, "heraldo tail: %v\n", err)
			}
		}
	}
}

// fetch lists every event after the cursor, shows the ones that match, and
// moves the cursor.
func (t *tailer) fetch(ctx context.Context) error {
	for {
		p, err := t.c.List(ctx, client.Query{After: t.cursor, Limit: 500})
		if err != nil {
			return err
		}
		if len(p.Events) == 0 {
			return nil
		}
		for _, e := range p.Events {
			if !t.f.match(e) {
				continue
			}
			t.a.print([]client.Event{e}, t.asJSON)
			if t.cmd != "" {
				t.a.runExec(ctx, t.cmd, e)
			}
		}
		t.cursor = p.NextCursor
		if t.mark {
			if _, err := t.c.SetCursor(ctx, t.name, t.cursor); err != nil {
				fmt.Fprintf(t.a.stderr, "heraldo tail: read cursor: %v\n", err)
			}
		}
	}
}

// runExec runs cmd for one event. The event's JSON goes on stdin and its
// main fields in HERALDO_EVENT_* variables. No shell is involved, so event
// text never becomes part of a command line.
func (a *app) runExec(ctx context.Context, cmd string, e client.Event) {
	ctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	x := exec.CommandContext(ctx, cmd)
	x.Stdin = bytes.NewReader(compact(e.Raw))
	x.Stdout, x.Stderr = a.stdout, a.stderr
	env := []string{
		"HERALDO_EVENT_ID=" + e.Id,
		"HERALDO_EVENT_SOURCE=" + e.Source,
		"HERALDO_EVENT_KIND=" + string(e.Kind),
		"HERALDO_EVENT_LEVEL=" + level(e),
		"HERALDO_EVENT_KEY=" + str(e.Key),
		"HERALDO_EVENT_HOST=" + str(e.Host),
		"HERALDO_EVENT_PROJECT=" + str(e.Project),
		"HERALDO_EVENT_TITLE=" + oneLine(str(e.Title)),
		"HERALDO_EVENT_TS=" + when(e).UTC().Format(time.RFC3339),
	}
	if e.Action != nil {
		env = append(env, "HERALDO_EVENT_ACTION="+string(*e.Action))
	}
	if e.Tags != nil {
		env = append(env, "HERALDO_EVENT_TAGS="+strings.Join(*e.Tags, ","))
	}
	// The command gets the CLI's environment without its tokens.
	for _, kv := range x.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k != envToken && k != envWriteToken {
			env = append(env, kv)
		}
	}
	x.Env = env
	if err := x.Run(); err != nil {
		fmt.Fprintf(a.stderr, "heraldo tail: --exec %s for %s: %v\n", cmd, e.Id, err)
	}
}

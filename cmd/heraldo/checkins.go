package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/client"
)

// ---- checkins ----------------------------------------------------------

const checkinsUsage = `[--json] [--project names] | show <slug> | create <slug> [flags] | pause <slug> | resume <slug> | key [--remove] <slug>`

// checkins lists the checks, or runs one of its subcommands.
func (a *app) checkins(ctx context.Context, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "show":
			return a.checkShow(ctx, args[1:])
		case "create":
			return a.checkCreate(ctx, args[1:])
		case "pause", "resume":
			return a.checkPause(ctx, args[0], args[1:])
		case "key":
			return a.checkKey(ctx, args[1:])
		}
	}
	fs := a.flags("checkins", checkinsUsage)
	asJSON := fs.Bool("json", false, "print each check as JSON on one line")
	project := fs.String("project", "", "only checks in these projects, comma-separated; - for none")
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
	entries, err := c.ChecksIn(ctx, projectList(*project))
	if client.IsStatus(err, http.StatusNotFound) || client.IsStatus(err, http.StatusMethodNotAllowed) {
		return a.lastCheckins(ctx, c, asJSON)
	}
	if err != nil {
		return err
	}
	checks := make([]api.Check, len(entries))
	for i, e := range entries {
		checks[i] = e.Check
	}
	if *asJSON {
		for _, ch := range checks {
			b, _ := json.Marshal(ch)
			fmt.Fprintf(a.stdout, "%s\n", b)
		}
		return nil
	}
	if len(checks) == 0 {
		fmt.Fprintln(a.stderr, "no check-ins")
		return nil
	}
	fmt.Fprintf(a.stdout, "%-32s %-14s %-11s %-9s %s\n", "CHECK", "STATE", "NEXT DUE", "LAST RUN", "LAST LOG")
	for _, ch := range checks {
		fmt.Fprintf(a.stdout, "%-32s %-14s %-11s %-9s %s\n", oneLine(ch.Key), checkState(ch),
			stampOr(ch.NextDue), runLength(ch.LastDurationMs), cutLine(firstLine(str(ch.LastLog)), 60))
	}
	return nil
}

func checkState(ch api.Check) string {
	s := string(ch.State)
	if ch.DownReason != nil {
		s += " (" + string(*ch.DownReason) + ")"
	}
	return s
}

func stampOr(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Local().Format("01-02 15:04")
}

func runLength(ms *int) string {
	if ms == nil {
		return "-"
	}
	d := time.Duration(*ms) * time.Millisecond
	if d < time.Second {
		return d.String()
	}
	return d.Round(time.Second).String()
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	first, _, _ := strings.Cut(s, "\n")
	return first
}

func cutLine(s string, n int) string {
	r := []rune(oneLine(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}

// checkShow prints one check with its full last log.
func (a *app) checkShow(ctx context.Context, args []string) error {
	fs := a.flags("checkins show", "[--json] <slug>")
	asJSON := fs.Bool("json", false, "print the check as JSON")
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
	ch, err := c.Check(ctx, pos[0])
	if err != nil {
		return err
	}
	if *asJSON {
		b, _ := json.MarshalIndent(ch, "", "  ")
		fmt.Fprintf(a.stdout, "%s\n", b)
		return nil
	}
	a.printCheck(ch)
	return nil
}

func (a *app) printCheck(ch api.Check) {
	w := a.stdout
	fmt.Fprintf(w, "check:     %s\n", oneLine(ch.Key))
	fmt.Fprintf(w, "state:     %s since %s\n", checkState(ch), ch.StateSince.Local().Format("2006-01-02 15:04"))
	if ch.Host != nil {
		fmt.Fprintf(w, "host:      %s\n", oneLine(*ch.Host))
	}
	fmt.Fprintf(w, "schedule:  %s\n", scheduleText(ch.Schedule))
	fmt.Fprintf(w, "next due:  %s\n", stampOr(ch.NextDue))
	fmt.Fprintf(w, "down at:   %s\n", stampOr(ch.DownAfter))
	fmt.Fprintf(w, "last ping: %s\n", stampOr(ch.LastPing))
	fmt.Fprintf(w, "last run:  %s\n", runLength(ch.LastDurationMs))
	if ch.Incident != nil {
		fmt.Fprintf(w, "incident:  checkin:%s %s\n", oneLine(ch.Key), *ch.Incident)
	}
	if ch.PingKeyAt != nil {
		fmt.Fprintf(w, "ping key:  minted %s\n", stampOr(ch.PingKeyAt))
	}
	if ch.LastLog != nil {
		fmt.Fprintf(w, "last log (%s):\n%s\n", stampOr(ch.LastLogAt), strings.TrimRight(clean(*ch.LastLog), "\n"))
	}
}

func scheduleText(s api.CheckSchedule) string {
	var parts []string
	switch {
	case s.PeriodS != nil:
		parts = append(parts, "every "+secText(*s.PeriodS))
	case s.Cron != nil:
		c := "cron " + *s.Cron
		if s.Tz != nil {
			c += " " + *s.Tz
		}
		parts = append(parts, c)
	default:
		parts = append(parts, "unscheduled")
	}
	if s.GraceS != nil {
		parts = append(parts, "grace "+secText(*s.GraceS))
	}
	if s.MaxRuntimeS != nil {
		parts = append(parts, "max runtime "+secText(*s.MaxRuntimeS))
	}
	if s.ReminderS != nil && *s.ReminderS > 0 {
		parts = append(parts, "reminder "+secText(*s.ReminderS))
	}
	return strings.Join(parts, ", ")
}

func secText(n int) string { return (time.Duration(n) * time.Second).String() }

// checkCreate is checkins create: PUT /v1/checkins/<slug>. It replaces a
// known check's config.
func (a *app) checkCreate(ctx context.Context, args []string) error {
	fs := a.flags("checkins create", "<slug> [--period 1h | --cron '0 3 * * *' --tz Europe/Madrid] [--grace 1h] [--max-runtime 30m] [--reminder 24h] [--host h]")
	period := fs.Duration("period", 0, "expected time between pings")
	cronExpr := fs.String("cron", "", "cron expression for the expected pings")
	tz := fs.String("tz", "", "IANA time zone for --cron (default UTC)")
	grace := durFlag(fs, "grace", "time a ping may be late before the check goes down (server default 1h)")
	maxRun := fs.Duration("max-runtime", 0, "time a started run may take")
	reminder := durFlag(fs, "reminder", "time between reminders while down; 0 sends none (server default 24h)")
	host := fs.String("host", "", "the machine the job runs on")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		fs.Usage()
		return errUsage
	}
	cfg := api.CheckConfig{Host: optStr(*host), Cron: optStr(*cronExpr), Tz: optStr(*tz)}
	if *period > 0 {
		cfg.PeriodS = secs(*period)
	}
	if *maxRun > 0 {
		cfg.MaxRuntimeS = secs(*maxRun)
	}
	if grace.set {
		cfg.GraceS = secs(grace.d)
	}
	if reminder.set {
		cfg.ReminderS = secs(reminder.d)
	}
	c, err := a.writeClient()
	if err != nil {
		return err
	}
	ch, err := c.PutCheck(ctx, pos[0], cfg)
	if err != nil {
		return err
	}
	a.printCheck(ch)
	return nil
}

func (a *app) checkPause(ctx context.Context, op string, args []string) error {
	fs := a.flags("checkins "+op, "<slug>")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		fs.Usage()
		return errUsage
	}
	c, err := a.writeClient()
	if err != nil {
		return err
	}
	ch, err := c.SetPaused(ctx, pos[0], op == "pause")
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "%s: %s\n", oneLine(ch.Key), checkState(ch))
	return nil
}

// checkKey is checkins key: a new ping key for the check, printed once as
// its ping URL. The old key stops working. --remove removes the key.
func (a *app) checkKey(ctx context.Context, args []string) error {
	fs := a.flags("checkins key", "[--remove] <slug>")
	remove := fs.Bool("remove", false, "remove the key; pings with it get 404")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		fs.Usage()
		return errUsage
	}
	c, err := a.writeClient()
	if err != nil {
		return err
	}
	if *remove {
		if err := c.RemovePingKey(ctx, pos[0]); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "%s: ping key removed\n", oneLine(pos[0]))
		return nil
	}
	k, err := c.MintPingKey(ctx, pos[0])
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "%s%s\n", strings.TrimRight(c.BaseURL, "/"), k.Path)
	fmt.Fprintln(a.stderr, "The server shows this key once. Append /start, /fail, /log or an exit code to the URL.")
	return nil
}

// writeClient is client, with the write token required.
func (a *app) writeClient() (*client.Client, error) {
	c, err := a.client()
	if err != nil {
		return nil, err
	}
	if a.setting(envWriteToken) == "" {
		return nil, fmt.Errorf("%s is not set: changing a check needs a write token", envWriteToken)
	}
	return c, nil
}

func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func secs(d time.Duration) *int {
	n := int(d / time.Second)
	return &n
}

// durValue is a duration flag that knows whether it was given.
type durValue struct {
	d   time.Duration
	set bool
}

func (v *durValue) String() string { return v.d.String() }
func (v *durValue) Set(s string) error {
	d, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	if d < 0 {
		return errors.New("may not be negative")
	}
	v.d, v.set = d, true
	return nil
}

func durFlag(fs *flag.FlagSet, name, usage string) *durValue {
	v := &durValue{}
	fs.Var(v, name, usage)
	return v
}

// lastCheckins prints the last event of each check from /v1/state, for a
// server before stage 6.
func (a *app) lastCheckins(ctx context.Context, c *client.Client, asJSON *bool) error {
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

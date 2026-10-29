package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/client"
)

// eventID matches an event id: a ULID.
var eventID = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

// snooze is POST /v1/mutes with remind: a mute on one incident key that
// pushes the key's last event again when it ends, if the key is still open.
func (a *app) snooze(ctx context.Context, args []string) error {
	fs := a.flags("snooze", "[--for 1h | --until RFC3339] <key or event id>")
	dur := fs.String("for", "", "how long, a Go duration such as 1h or 90m (default 1h)")
	until := fs.String("until", "", "when the snooze ends, RFC 3339")
	asJSON := fs.Bool("json", false, "print the snooze as JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		fs.Usage()
		return errUsage
	}
	if *dur != "" && *until != "" {
		return fmt.Errorf("give --for or --until, not both")
	}
	c, err := a.writeNeeded("snooze")
	if err != nil {
		return err
	}
	key, err := snoozeKey(ctx, c, pos[0])
	if err != nil {
		return err
	}
	yes := true
	in := api.MuteInput{Key: &key, Remind: &yes}
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
	m, err := c.Mute(ctx, in)
	if err != nil {
		var ae *client.Error
		if errors.As(err, &ae) && ae.Status == http.StatusBadRequest && strings.Contains(ae.Message, "unknown field") {
			return fmt.Errorf("%w (a server before 0.7 has no snooze; heraldo mute --key holds the key back without the reminder)", err)
		}
		return err
	}
	if *asJSON {
		a.printJSON(m)
		return nil
	}
	fmt.Fprintf(a.stdout, "snoozed key=%s until %s; reminds then if it is still open\n", oneLine(key), localTime(m.Until))
	fmt.Fprintf(a.stdout, "%s\n", m.Id)
	return nil
}

// snoozeKey is the incident key arg names: an event id names its event's
// key, anything else is the key.
func snoozeKey(ctx context.Context, c *client.Client, arg string) (string, error) {
	if !eventID.MatchString(arg) {
		return arg, nil
	}
	e, err := c.Get(ctx, arg)
	var ae *client.Error
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return arg, nil
	}
	if err != nil {
		return "", err
	}
	if e.Key == nil || *e.Key == "" {
		return "", fmt.Errorf("event %s has no key; snooze one incident by its key", arg)
	}
	return *e.Key, nil
}

package main

import (
	"context"
	"fmt"
	"net/http"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/client"
)

// digest prints the digest as it would go out now; --send sends it.
func (a *app) digest(ctx context.Context, args []string) error {
	fs := a.flags("digest", "[--send] [--json]")
	send := fs.Bool("send", false, "send the digest now (HERALDO_WRITE_TOKEN); it does not replace the scheduled one")
	asJSON := fs.Bool("json", false, "print the digest as JSON")
	pos, err := parse(fs, args)
	if err := noPositional(fs, pos, err); err != nil {
		return err
	}
	var d api.DigestPreview
	if *send {
		c, err := a.writeNeeded("digest --send")
		if err != nil {
			return err
		}
		if d, err = c.SendDigest(ctx); err != nil {
			return digestErr(err)
		}
	} else {
		c, err := a.client()
		if err != nil {
			return err
		}
		if d, err = c.DigestPreview(ctx); err != nil {
			return digestErr(err)
		}
	}
	if *asJSON {
		a.printJSON(d)
		return nil
	}
	fmt.Fprintf(a.stdout, "%s [%s]\n%s\n\n%s", d.Title, d.Level, d.Summary, d.Body)
	switch {
	case d.EventId != nil:
		fmt.Fprintf(a.stdout, "\nsent: %s\n", *d.EventId)
	case !d.Enabled:
		fmt.Fprintln(a.stderr, "preview only; the daily digest is off (digest.enabled)")
	case d.NextAt != nil:
		fmt.Fprintf(a.stderr, "preview only; the next digest goes out %s\n", localTime(*d.NextAt))
	}
	return nil
}

func digestErr(err error) error {
	if client.IsStatus(err, http.StatusNotFound) || client.IsStatus(err, http.StatusMethodNotAllowed) {
		return fmt.Errorf("the server has no digest; it needs heraldo-server 0.7 or later")
	}
	return err
}

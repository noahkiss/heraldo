package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/noahkiss/heraldo/internal/client"
)

// why prints an event's push trace: one line per pipeline stage.
func (a *app) why(ctx context.Context, args []string) error {
	fs := a.flags("why", "[--json] <id>")
	asJSON := fs.Bool("json", false, "print the trace as JSON")
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
	t, err := c.Why(ctx, pos[0])
	if client.IsStatus(err, http.StatusNotFound) {
		// Tell a missing event from a server without the endpoint.
		if _, gerr := c.Get(ctx, pos[0]); gerr == nil {
			return fmt.Errorf("the server has no push trace yet; it needs a newer heraldo-server")
		}
	}
	if err != nil {
		return err
	}
	if *asJSON {
		b, _ := json.MarshalIndent(t, "", "  ")
		fmt.Fprintf(a.stdout, "%s\n", b)
		return nil
	}
	if len(t.Decisions) == 0 {
		fmt.Fprintln(a.stdout, "not decided: the server has push off, or the event was imported")
		return nil
	}
	tw := tabwriter.NewWriter(a.stdout, 0, 2, 2, ' ', 0)
	for _, d := range t.Decisions {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", d.Stage, d.Outcome, oneLine(d.Reason), detailLine(d.Detail))
	}
	_ = tw.Flush()
	if t.Why != nil {
		fmt.Fprintf(a.stdout, "\n%s\n", oneLine(*t.Why))
	}
	return nil
}

// detailLine prints a trace row's detail as key=value pairs, with the
// deliver stage's devices as device:outcome.
func detailLine(d *map[string]any) string {
	if d == nil {
		return ""
	}
	keys := make([]string, 0, len(*d))
	for k := range *d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		v := (*d)[k]
		if devs, ok := v.([]any); ok && k == "devices" {
			for _, x := range devs {
				m, _ := x.(map[string]any)
				s := fmt.Sprintf("%v:%v", m["device"], m["outcome"])
				if e, ok := m["error"].(string); ok {
					s += "(" + e + ")"
				}
				parts = append(parts, s)
			}
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	return oneLine(strings.Join(parts, " "))
}

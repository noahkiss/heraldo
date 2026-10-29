package decide

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/heraldo/internal/digest"
	"github.com/noahkiss/heraldo/internal/llm"
	"github.com/noahkiss/heraldo/internal/settings"
)

// The digest's paragraph: "overnight in one paragraph", gated by
// digest.prose. It is text only, so advise and enforce both add it; shadow
// writes it to the call log and the trace and sends the plain digest.

type digestOutput struct {
	Paragraph string `json:"paragraph"`
}

var digestSchema = &llm.Schema{Name: "digest_paragraph", Schema: json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["paragraph"],
  "properties": {"paragraph": {"type": "string", "maxLength": 700}}
}`)}

func validateDigest(o digestOutput) error {
	if p := strings.TrimSpace(o.Paragraph); p == "" || len([]rune(p)) > 700 || strings.Contains(p, "\n\n") {
		return errors.New("paragraph must be one paragraph of 1 to 700 characters")
	}
	return nil
}

// digestTraces keeps each built digest's trace, by its window's end, until
// the server stores the digest event.
type digestTraces struct {
	mu sync.Mutex
	by map[string]Trace
}

func (t *digestTraces) put(until time.Time, tr Trace) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.by == nil || len(t.by) > 16 {
		t.by = map[string]Trace{}
	}
	t.by[until.UTC().Format(time.RFC3339)] = tr
}

// DigestTrace returns and forgets the trace of the digest whose window
// ends at until (RFC 3339, UTC), for the stored digest event.
func (d *Decider) DigestTrace(until string) (Trace, bool) {
	d.digests.mu.Lock()
	defer d.digests.mu.Unlock()
	t, ok := d.digests.by[until]
	delete(d.digests.by, until)
	return t, ok
}

// digestInput is what of a digest the model sees: titles and counts, no
// bodies, no ids, nothing of an excluded project.
func (d *Decider) digestInput(dg *digest.Digest) (map[string]any, []Member) {
	pol := d.cfg.LLM.Policy()
	var ms []Member
	incidents := func(list []digest.Incident) []map[string]any {
		out := []map[string]any{}
		for _, i := range list {
			if pol.Excluded(i.Project) {
				continue
			}
			ms = append(ms, Member{Project: i.Project, Source: i.Source, Key: i.Key})
			row := map[string]any{"title": i.Title, "state": i.State, "source": i.Source, "level": i.Level}
			if i.Project != "" {
				row["project"] = i.Project
			}
			if i.New {
				row["new"] = true
			}
			out = append(out, row)
		}
		return out
	}
	checks := []map[string]any{}
	for _, c := range dg.Checkins.Items {
		checks = append(checks, map[string]any{"check": c.Key, "state": c.State, "failures": c.Failures})
	}
	jobs := []string{}
	for _, j := range dg.Finished {
		if !pol.Excluded(j.Project) {
			jobs = append(jobs, j.Title)
		}
	}
	held := []string{}
	for _, h := range dg.Held.Open {
		held = append(held, h.Title)
	}
	projects := []map[string]any{}
	for _, p := range dg.Projects {
		if !pol.Excluded(p.Name) {
			projects = append(projects, map[string]any{"project": p.Name, "events": p.Events})
		}
	}
	hours := int(dg.Until.Sub(dg.Since).Round(time.Hour).Hours())
	return map[string]any{
		"hours": hours, "summary": dg.Summary, "incidents_opened": dg.Incidents.Opened,
		"resolved": incidents(dg.Incidents.Resolved), "open": incidents(dg.Incidents.Open),
		"stale": incidents(dg.Incidents.Stale), "checks_ok": dg.Checkins.OK, "checks_total": dg.Checkins.Total,
		"checks": checks, "finished": jobs, "held": held, "held_closed": dg.Held.Closed, "projects": projects,
	}, ms
}

// DigestProse is the digest's Summarizer: one paragraph, or "" when
// digest.prose is off, the mode is off or shadow, or the call failed.
func (d *Decider) DigestProse(ctx context.Context, dg *digest.Digest) (string, error) {
	mode := d.Mode()
	if mode == llm.ModeOff || !settings.From[bool](d.cfg.Settings, "digest.prose") {
		return "", nil
	}
	input, ms := d.digestInput(dg)
	var out digestOutput
	res, guidance, err := d.call(ctx, "digest", input, ms, digestSchema, func(r llm.Response) error {
		var o digestOutput
		if err := llm.ParseJSON(r.Content, &o); err != nil {
			return err
		}
		return validateDigest(o)
	})
	if err == nil {
		err = llm.ParseJSON(res.Content, &out)
	}
	if err != nil {
		d.digests.put(dg.Until, failTrace(res, err, "the digest is the plain list"))
		return "", nil
	}
	p := strings.TrimSpace(out.Paragraph)
	det := d.baseDetail(mode, res, guidance)
	if mode == llm.ModeShadow {
		det["applied"] = false
		d.digests.put(dg.Until, Trace{Reason: who(mode, res) + " wrote the digest's paragraph; not sent (shadow)", Detail: det})
		return "", nil
	}
	det["applied"] = "text"
	d.digests.put(dg.Until, Trace{Reason: fmt.Sprintf("%s wrote the digest's paragraph (%d characters)", who(mode, res),
		len([]rune(p))), Detail: det})
	return p, nil
}

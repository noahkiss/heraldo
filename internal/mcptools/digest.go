package mcptools

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/noahkiss/heraldo/internal/client"
)

type digestPreviewIn struct{}

const digestPreviewDesc = `Build heraldo's daily digest as it would go out now: what happened since the last digest. It stores and sends nothing.

Best for: "what happened overnight", and a one-call summary of a day: incidents opened, resolved and still open, stale incidents, check-ins (N/N ok and the failures), jobs and prints that finished, held alerts, and event counts per project.
Returns: the digest's level, title, one-line summary, the Markdown body, and the same sections as lists, plus whether the daily digest is on and when it goes out next.
Not for: the current state of one thing; use overview, checkin_read or events_search key=<key>.`

func (t *tools) addDigestPreview(s *mcp.Server) {
	addTool(t, s, &mcp.Tool{Name: "digest_preview", Description: digestPreviewDesc, Annotations: readOnly("Preview the digest")},
		nil,
		map[string]*jsonschema.Schema{
			"day": str("YYYY-MM-DD in tz."), "tz": str(""), "since": str(""), "until": str(""),
			"level": str("passive, or active when an incident is open or a check is down."),
			"title": str(""), "summary": str("The one-line push text."), "body": str("Markdown."),
			"incidents": anyObj("opened (count), resolved, open, stale: lists of key, state, title, opened_at, last_at, event_id."),
			"checkins":  anyObj("total, ok, paused, items: key, state, down_reason, failures."),
			"finished":  arr("Progress keys that ended.", anyObj("key, title, project, ended_at, event_id")),
			"held":      anyObj("open: id, title, at; closed (count)."),
			"projects":  arr("Most events first; name is empty for none.", anyObj("name, events")),
			"enabled":   boolean("The daily digest is on."), "at": str("HH:MM in tz."),
			"next_at": str("When it goes out next; empty while off."),
		},
		t.digestPreview)
}

func (t *tools) digestPreview(ctx context.Context, _ *mcp.CallToolRequest, _ digestPreviewIn) (result, error) {
	d, err := t.API.DigestPreview(ctx)
	if err != nil {
		if client.IsStatus(err, http.StatusNotFound) || client.IsStatus(err, http.StatusMethodNotAllowed) {
			return result{}, toolErr("server_too_old", "this heraldo server has no digest", "it needs heraldo-server 0.7 or later")
		}
		return result{}, t.apiErr(err, call{})
	}
	out := map[string]any{
		"day": d.Day, "tz": d.Tz, "since": rfc(d.Since), "until": rfc(d.Until), "level": string(d.Level),
		"title": d.Title, "summary": d.Summary, "body": d.Body,
		"incidents": d.Incidents, "checkins": d.Checkins, "finished": d.Finished, "held": d.Held,
		"projects": d.Projects, "enabled": d.Enabled, "at": d.At,
	}
	if d.NextAt != nil {
		out["next_at"] = rfc(*d.NextAt)
	}
	state := "the daily digest is off"
	if d.Enabled && d.NextAt != nil {
		state = "next goes out " + rfc(*d.NextAt)
	}
	text := fmt.Sprintf("%s [%s] (%s)\n%s\n\n%s", d.Title, d.Level, state, d.Summary, d.Body)
	return result{out: out, text: text}, nil
}

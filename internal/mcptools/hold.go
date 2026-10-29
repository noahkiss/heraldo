package mcptools

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/client"
)

// holdCap caps hold_read's entries; holdBodyCap caps each entry's body.
const (
	holdCap     = 50
	holdBodyCap = 2 << 10
)

// ---- hold_read ----------------------------------------------------------

type holdReadIn struct {
	IncludeClosed bool `json:"include_closed,omitempty" jsonschema:"Also list entries closed in the last 24 hours. Default false: open entries only."`
	Poll          bool `json:"poll,omitempty" jsonschema:"Count this read as a triage poll, which resets the dead-man. Only for an agent that IS the triage job. Default false."`
}

const holdReadDesc = `Read heraldo's alert hold: alerts kept back from the owner's phone until triage reads them.

Best for: "what is held", and triage: read the held alerts, decide one diagnosis for a group, then close it with hold_act.
Returns: mode (off, shadow, on), whether the dead-man has tripped, and up to 50 entries, oldest first: id, status, tag, title, body (2 KiB each), arrival and deadline times.
By default this is a look, not a triage poll: it does not hold off the dead-man. Pass poll=true only when you are the triage job.
Not for: held events in history; use events_search hold=held.
Follow up with hold_act to release or suppress entries by id.`

func (t *tools) addHoldRead(s *mcp.Server) {
	entry := obj("", map[string]*jsonschema.Schema{
		"id": str("h-..., for hold_act."), "status": str("held, released, suppressed or expired."),
		"tag": str(""), "title": str(""), "body": str("At most 2 KiB."), "body_truncated": boolean(""),
		"source": str(""), "event_id": str("The stored event."), "at": str(""), "deadline": str(""),
		"forwarded": boolean("The alert already pushed."), "closed_at": str(""), "verdict": str(""), "reason": str(""),
	})
	addTool(t, s, &mcp.Tool{Name: "hold_read", Description: holdReadDesc, Annotations: readOnly("Read the alert hold")},
		nil,
		map[string]*jsonschema.Schema{
			"mode": str("off, shadow or on."), "deadman_active": boolean("Triage stopped polling; alerts pass through."),
			"last_poll": str(""), "oldest_waiting": str("The oldest entry not yet pushed; empty when none."),
			"total": integer("Entries the server listed."), "entries": arr("Oldest first.", entry),
		},
		t.holdRead)
}

func epoch(f float64) string {
	if f == 0 {
		return ""
	}
	return rfc(time.Unix(int64(f), 0))
}

func (t *tools) holdRead(ctx context.Context, _ *mcp.CallToolRequest, in holdReadIn) (result, error) {
	q, err := t.API.Hold(ctx, client.HoldQuery{Pending: !in.IncludeClosed, Poll: in.Poll})
	if err != nil {
		return result{}, t.apiErr(err, call{})
	}
	out := map[string]any{"mode": string(q.Mode), "total": len(q.Held)}
	dead := q.DeadmanActive != nil && *q.DeadmanActive
	out["deadman_active"] = dead
	if v, err := q.LastPollTs.Get(); err == nil {
		out["last_poll"] = epoch(v)
	}
	if v, err := q.OldestUnforwardedTs.Get(); err == nil {
		out["oldest_waiting"] = epoch(v)
	}
	var entries []map[string]any
	var lines []string
	head := fmt.Sprintf("hold mode %s, %d entries", q.Mode, len(q.Held))
	if dead {
		head += ", DEAD-MAN TRIPPED (triage not polling)"
	}
	lines = append(lines, head)
	for i, h := range q.Held {
		if i == holdCap {
			lines = append(lines, fmt.Sprintf("  ... %d more not shown", len(q.Held)-holdCap))
			break
		}
		e := map[string]any{"id": h.Id, "status": string(h.Status), "at": epoch(h.Ts), "deadline": epoch(h.DeadlineTs),
			"forwarded": h.Forwarded}
		for k, v := range map[string]*string{"tag": h.Tag, "title": h.Title, "source": h.Source, "event_id": h.EventId,
			"verdict": h.Verdict, "reason": h.Reason} {
			if v != nil && *v != "" {
				e[k] = *v
			}
		}
		if h.Body != nil {
			b := *h.Body
			if len(b) > holdBodyCap {
				b = cutRunes(b, holdBodyCap)
				e["body_truncated"] = true
			}
			e["body"] = b
		}
		if h.ClosedTs != nil {
			e["closed_at"] = epoch(*h.ClosedTs)
		}
		entries = append(entries, e)
		lines = append(lines, fmt.Sprintf("  %s [%s] %s tag=%s: %s", h.Id, h.Status, epoch(h.Ts), deref(h.Tag), firstLineOf(deref(h.Title))))
	}
	out["entries"] = nonNil(entries)
	return result{out: out, text: strings.Join(lines, "\n")}, nil
}

func firstLineOf(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return l
}

// ---- hold_act -----------------------------------------------------------

type holdActIn struct {
	Action  string   `json:"action"`
	IDs     []string `json:"ids" jsonschema:"Entry ids from hold_read, h-... One diagnosis covers them all."`
	Verdict string   `json:"verdict,omitempty" jsonschema:"release: REAL, BLIP, MAINTENANCE or FALSE."`
	Title   string   `json:"title,omitempty" jsonschema:"release: the diagnosis, one line."`
	Body    string   `json:"body,omitempty" jsonschema:"release: what happened and what to do."`
	Tag     string   `json:"tag,omitempty" jsonschema:"release: the tag the message is stored at, such as general. Default general."`
	Reason  string   `json:"reason,omitempty" jsonschema:"suppress: why nothing is sent, such as BLIP: recovered before triage looked."`
}

const holdActDesc = `Close held alerts in heraldo's alert hold: release them as one diagnosis, or suppress them.

Best for: triage, after hold_read. Group the entries that share a cause and close them with one call.
release: in mode on, stores and pushes ONE message (title, body, tag) from source triage instead of a push per alert; in shadow it only records. Needs verdict, title and body.
suppress: closes the entries and sends nothing. Use it for blips and known maintenance.
Returns: the ids closed, the ids that were not open (left alone), and for a release whether a message was sent and its event id.
Not for: incidents (incident_act) or mutes (mute).`

func (t *tools) addHoldAct(s *mcp.Server) {
	addTool(t, s, &mcp.Tool{Name: "hold_act", Description: holdActDesc,
		Annotations: &mcp.ToolAnnotations{Title: "Release or suppress held alerts", DestructiveHint: ptr(true),
			OpenWorldHint: ptr(true)}},
		func(is *jsonschema.Schema) {
			p := is.Properties
			enum(p["action"], "release", "suppress")
			strList(p["ids"], 64, 100)
			p["ids"].MinItems = ptr(1)
			enum(p["verdict"], "REAL", "BLIP", "MAINTENANCE", "FALSE")
			for k, n := range map[string]int{"title": 256, "body": 65536, "tag": 64, "reason": 1024} {
				maxLen(p[k], n)
			}
			def(p["tag"], "general")
		},
		map[string]*jsonschema.Schema{
			"closed":            arr("Entries this call closed.", str("")),
			"unknown_or_closed": arr("Ids that were not open; nothing changed for them.", str("")),
			"mode":              str(""), "sent": boolean("release: a message was stored."),
			"event_id": str("release: the stored message."),
		},
		t.holdAct)
}

func (t *tools) holdAct(ctx context.Context, _ *mcp.CallToolRequest, in holdActIn) (result, error) {
	if len(in.IDs) == 0 {
		return result{}, toolErr("invalid", "ids is required", "hold_read lists the open entries")
	}
	w := call{write: true}
	var r client.HoldResult
	var err error
	switch in.Action {
	case "release":
		if in.Verdict == "" || strings.TrimSpace(in.Title) == "" || strings.TrimSpace(in.Body) == "" {
			return result{}, toolErr("invalid", "release needs verdict, title and body", "")
		}
		tag := in.Tag
		if tag == "" {
			tag = "general"
		}
		r, err = t.API.HoldRelease(ctx, api.HoldReleaseJSONBody{Ids: in.IDs,
			Verdict: api.HoldReleaseJSONBodyVerdict(strings.ToUpper(in.Verdict)), Title: in.Title, Body: in.Body, Tag: tag})
	case "suppress":
		body := api.HoldSuppressJSONBody{Ids: in.IDs}
		if in.Reason != "" {
			body.Reason = &in.Reason
		}
		r, err = t.API.HoldSuppress(ctx, body)
	default:
		return result{}, toolErr("invalid", "action must be release or suppress", "")
	}
	if client.IsStatus(err, http.StatusConflict) {
		return result{}, &ToolError{Code: "nothing_open", Status: http.StatusConflict,
			Message: "no id names an open hold entry", Hint: "hold_read lists the open entries"}
	}
	if err != nil {
		return result{}, t.apiErr(err, w)
	}
	closed := r.Released
	if in.Action == "suppress" {
		closed = r.Suppressed
	}
	out := map[string]any{"closed": nonNil(closed), "unknown_or_closed": nonNil(r.UnknownOrClosed), "mode": r.Mode}
	text := fmt.Sprintf("%s %d entries (mode %s)", map[string]string{"release": "released", "suppress": "suppressed"}[in.Action],
		len(closed), r.Mode)
	if in.Action == "release" {
		out["sent"] = r.Sent
		if r.Sent {
			out["event_id"] = r.EventID
			text += ", sent " + r.EventID
		} else {
			text += ", recorded only"
		}
	}
	if len(r.UnknownOrClosed) > 0 {
		text += "; not open, left alone: " + strings.Join(r.UnknownOrClosed, " ")
	}
	return result{out: out, text: text}, nil
}

package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/noahkiss/heraldo/api"
)

const logCap = 8 << 10

var slugRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,200}$`)

var checkSchema = map[string]*jsonschema.Schema{
	"key": str("The check's slug."), "state": str("new, up, late, started, down or paused."),
	"down_reason": str(""), "state_since": str(""), "next_due": str(""), "down_after": str(""),
	"schedule": anyObj("period_s, or cron with tz; grace_s, max_runtime_s, reminder_s."),
	"incident": str("The state of the check's incident, checkin:<slug>."),
	"last_log": str("The end of the last log, at most 8 KiB."), "last_log_truncated": boolean(""),
	"last_log_bytes": integer(""), "previous_state": str("checkin_update: the state before the change."),
}

// checkOut renders a check with its log tail capped.
func checkOut(c api.Check) map[string]any {
	b, _ := json.Marshal(c)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	if c.LastLog != nil && len(*c.LastLog) > logCap {
		l := *c.LastLog
		start := len(l) - logCap
		for start < len(l) && !utf8.RuneStart(l[start]) {
			start++
		}
		m["last_log"] = l[start:]
		m["last_log_truncated"] = true
		m["last_log_bytes"] = len(l)
	}
	return m
}

func checkLine(c api.Check) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s since %s", c.Key, c.State, c.StateSince.UTC().Format(time.RFC3339))
	if c.DownReason != nil {
		b.WriteString(" (" + string(*c.DownReason) + ")")
	}
	if c.NextDue != nil {
		b.WriteString(", next due " + c.NextDue.UTC().Format(time.RFC3339))
	}
	if c.Incident != nil {
		b.WriteString(", incident " + string(*c.Incident))
	}
	return b.String()
}

func checkText(c api.Check, m map[string]any) string {
	s := checkLine(c)
	if l, ok := m["last_log"].(string); ok && l != "" {
		s += "\n\nlast log"
		if m["last_log_truncated"] == true {
			s += " (end only)"
		}
		s += ":\n" + l
	}
	return s
}

type checkinReadIn struct {
	Slug string `json:"slug" jsonschema:"The check's slug, from overview or events_search kind=checkin."`
}

const checkinReadDesc = `Read one heraldo check: schedule, state, due times, last run length and the last log.

Best for: a slug from overview or events_search (kind=checkin, key=<slug>).
Returns: the Check object; last_log capped at 8 KiB (the end is kept, since errors sit there).
Follow up with events_search kind=checkin key=<slug> for run history.`

func (t *tools) addCheckinRead(s *mcp.Server) {
	addTool(t, s, &mcp.Tool{Name: "checkin_read", Description: checkinReadDesc, Annotations: readOnly("Read check")},
		func(is *jsonschema.Schema) { is.Properties["slug"].Pattern = slugRe.String() },
		checkSchema, t.checkinRead)
}

func (t *tools) checkinRead(ctx context.Context, _ *mcp.CallToolRequest, in checkinReadIn) (result, error) {
	c, err := t.API.Check(ctx, in.Slug)
	if err != nil {
		return result{}, t.apiErr(err, call{what: "check " + in.Slug, lister: "overview"})
	}
	m := checkOut(c)
	return result{out: m, text: checkText(c, m)}, nil
}

type scheduleIn struct {
	Period     string `json:"period,omitempty" jsonschema:"Go duration, such as 24h. Not with cron."`
	Cron       string `json:"cron,omitempty" jsonschema:"Five-field cron. Not with period."`
	Tz         string `json:"tz,omitempty" jsonschema:"IANA zone for cron, such as Europe/Paris."`
	Grace      string `json:"grace,omitempty" jsonschema:"Go duration after the due time before the check goes down."`
	MaxRuntime string `json:"max_runtime,omitempty" jsonschema:"Go duration a started run may take."`
	Reminder   string `json:"reminder,omitempty" jsonschema:"Go duration between reminders while down."`
	Host       string `json:"host,omitempty"`
}

type checkinUpdateIn struct {
	Slug      string      `json:"slug"`
	Operation string      `json:"operation"`
	Schedule  *scheduleIn `json:"schedule,omitempty" jsonschema:"configure only: the whole schedule."`
}

const checkinUpdateDesc = `Pause, resume, or replace the schedule of a heraldo check.

Best for: the owner asks to pause a check during maintenance or a trip, or to change how often a job must ping.
pause resolves the check's open incident and stops judging it. resume sets it back to new until the next ping.
configure creates the check or REPLACES its whole schedule: a field you leave out takes its default. Read it with checkin_read first and send every field you want kept.
Returns: the check after the change, with previous_state.
Not for: ping URLs or keys; the CLI makes those.`

func (t *tools) addCheckinUpdate(s *mcp.Server) {
	addTool(t, s, &mcp.Tool{Name: "checkin_update", Description: checkinUpdateDesc,
		Annotations: &mcp.ToolAnnotations{Title: "Update check", IdempotentHint: true,
			DestructiveHint: ptr(true), OpenWorldHint: &noAnnot}},
		func(is *jsonschema.Schema) {
			is.Properties["slug"].Pattern = slugRe.String()
			enum(is.Properties["operation"], "pause", "resume", "configure")
			sc := is.Properties["schedule"]
			sc.Types, sc.Type = nil, "object"
			maxLen(sc.Properties["host"], 128)
		},
		checkSchema, t.checkinUpdate)
}

func durS(name, v string) (*int, error) {
	if v == "" {
		return nil, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < time.Second {
		return nil, toolErr("invalid", fmt.Sprintf("schedule.%s: %q is not a Go duration of 1s or more", name, v),
			"use a form such as 90s, 15m or 24h")
	}
	return ptr(int(d / time.Second)), nil
}

func (t *tools) checkinUpdate(ctx context.Context, _ *mcp.CallToolRequest, in checkinUpdateIn) (result, error) {
	if !slugRe.MatchString(in.Slug) {
		return result{}, toolErr("invalid", "slug must be 1 to 200 of A-Z a-z 0-9 . _ : -", "")
	}
	if in.Operation == "configure" && in.Schedule == nil {
		return result{}, toolErr("invalid", "configure needs schedule", "read the check with checkin_read and send its whole schedule")
	}
	if in.Operation != "configure" && in.Schedule != nil {
		return result{}, toolErr("invalid", "schedule goes with configure only", "leave schedule out for "+in.Operation)
	}
	what := call{write: true, what: "check " + in.Slug, lister: "overview"}
	prev := ""
	if old, err := t.API.Check(ctx, in.Slug); err == nil {
		prev = string(old.State)
	} else if in.Operation != "configure" {
		return result{}, t.apiErr(err, what)
	}
	var c api.Check
	var err error
	switch in.Operation {
	case "pause", "resume":
		c, err = t.API.SetPaused(ctx, in.Slug, in.Operation == "pause")
	case "configure":
		sc := in.Schedule
		if sc.Period != "" && sc.Cron != "" {
			return result{}, toolErr("invalid", "schedule takes period or cron, not both", "")
		}
		var cfg api.CheckConfig
		for _, f := range []struct {
			name, v string
			to      **int
		}{{"period", sc.Period, &cfg.PeriodS}, {"grace", sc.Grace, &cfg.GraceS},
			{"max_runtime", sc.MaxRuntime, &cfg.MaxRuntimeS}, {"reminder", sc.Reminder, &cfg.ReminderS}} {
			if *f.to, err = durS(f.name, f.v); err != nil {
				return result{}, err
			}
		}
		for _, f := range []struct {
			v  string
			to **string
		}{{sc.Cron, &cfg.Cron}, {sc.Tz, &cfg.Tz}, {sc.Host, &cfg.Host}} {
			if f.v != "" {
				*f.to = ptr(f.v)
			}
		}
		c, err = t.API.PutCheck(ctx, in.Slug, cfg)
	default:
		return result{}, toolErr("invalid", "operation must be pause, resume or configure", "")
	}
	if err != nil {
		return result{}, t.apiErr(err, what)
	}
	m := checkOut(c)
	text := checkText(c, m)
	if prev != "" {
		m["previous_state"] = prev
		text = fmt.Sprintf("%s (was %s)", text, prev)
	} else {
		text = "created " + text
	}
	return result{out: m, text: text}, nil
}

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/client"
)

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// headline is the event's one line of text: title, else summary, else the
// first line of the body.
func headline(e client.Event) string {
	for _, s := range []string{str(e.Title), str(e.Summary), str(e.Body)} {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		first, _, _ := strings.Cut(s, "\n")
		return first
	}
	return ""
}

func when(e client.Event) time.Time {
	if !e.Ts.IsZero() {
		return e.Ts
	}
	return e.ReceivedAt
}

func level(e client.Event) string {
	if e.Level == nil {
		return "active"
	}
	return string(*e.Level)
}

// tag is the kind, action and key in brackets, or "" for a plain message.
func tag(e client.Event) string {
	if e.Kind == api.KindMessage && e.Key == nil {
		return ""
	}
	parts := []string{string(e.Kind)}
	if e.Action != nil {
		parts = append(parts, string(*e.Action))
	}
	if e.Key != nil {
		parts = append(parts, *e.Key)
	}
	return "[" + strings.Join(parts, " ") + "] "
}

// line prints one event as a single line: id, local time, level, source, and
// the headline.
func line(w io.Writer, e client.Event) {
	held := ""
	if e.Hold != nil {
		if s, _ := (*e.Hold)["state"].(string); s != "" {
			held = "(" + s + ") "
		}
	}
	if inc := e.Incident; inc != nil && inc.Flapping != nil && *inc.Flapping {
		held += "(flapping) "
	}
	fmt.Fprintf(w, "%s  %s  %-14s %-16s %s%s%s\n", e.Id, when(e).Local().Format("01-02 15:04"),
		level(e), e.Source, held, tag(e), oneLine(headline(e)))
}

// oneLine drops control characters, so event text cannot move the cursor or
// recolor the terminal.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return ' '
		}
		return r
	}, s)
}

// clean is oneLine for multi-line text: it keeps newlines and tabs.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return ' '
		}
		return r
	}, s)
}

// detail prints one event in full.
func detail(w io.Writer, e client.Event) {
	row := func(k, v string) {
		if v != "" {
			fmt.Fprintf(w, "%-9s %s\n", k+":", oneLine(v))
		}
	}
	row("id", e.Id)
	row("time", when(e).Local().Format(time.RFC3339))
	row("received", e.ReceivedAt.Local().Format(time.RFC3339))
	row("source", e.Source)
	row("host", str(e.Host))
	row("kind", string(e.Kind))
	if e.Action != nil {
		row("action", string(*e.Action))
	}
	row("key", str(e.Key))
	row("level", level(e))
	row("why", str(e.Why))
	if e.Tags != nil {
		row("tags", strings.Join(*e.Tags, ", "))
	}
	row("url", str(e.Url))
	row("image", str(e.Image))
	if e.Hold != nil {
		b, _ := json.Marshal(*e.Hold)
		row("hold", string(b))
	}
	if e.Details != nil {
		keys := make([]string, 0, len(*e.Details))
		for k := range *e.Details {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		row("details", strings.Join(keys, ", "))
	}
	row("title", str(e.Title))
	row("summary", str(e.Summary))
	for _, a := range attachmentRows(e) {
		row("attached", a)
	}
	if b := str(e.Body); b != "" {
		fmt.Fprintf(w, "\n%s\n", strings.TrimRight(clean(b), "\n"))
	}
}

// writeJSONLines prints each event's stored JSON on its own line.
func writeJSONLines(w io.Writer, evs []client.Event) {
	for _, e := range evs {
		fmt.Fprintf(w, "%s\n", compact(e.Raw))
	}
}

func compact(raw json.RawMessage) []byte {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return raw
	}
	return b.Bytes()
}

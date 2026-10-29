package client

import (
	"strconv"
	"strings"
	"time"

	"github.com/noahkiss/heraldo/api"
)

// EventLink is the link that opens one event in the apps.
func EventLink(id string) string { return "heraldo://event/" + id }

// Markdown is one event as text to share: a heading, the summary and body,
// a list of facts, and the event's heraldo:// link on the last line. It is
// what `heraldo show --md` prints and what MCP event_read returns with
// markdown set. The apps build the same text (HeraldoKit
// `Event.shareMarkdown`); change both together.
func Markdown(e Event) string {
	var b strings.Builder
	head := mdLine(e.Source)
	for _, s := range []*string{e.Title, e.Summary} {
		if s != nil && strings.TrimSpace(*s) != "" {
			head = mdLine(*s)
			break
		}
	}
	b.WriteString("# " + head + "\n")
	if s := strings.TrimSpace(deref(e.Summary)); s != "" && mdLine(s) != head {
		b.WriteString("\n" + s + "\n")
	}
	if s := strings.TrimSpace(deref(e.Body)); s != "" {
		b.WriteString("\n" + s + "\n")
	}
	b.WriteString("\n")
	fact := func(k, v string) {
		if v = mdLine(v); v != "" {
			b.WriteString("- " + k + ": " + v + "\n")
		}
	}
	fact("Source", e.Source)
	fact("Host", deref(e.Host))
	if e.Kind != api.KindMessage || e.Key != nil {
		k := string(e.Kind)
		if e.Action != nil {
			k += " " + string(*e.Action)
		}
		fact("Kind", k)
	}
	fact("Key", deref(e.Key))
	lv := "active"
	if e.Level != nil {
		lv = string(*e.Level)
	}
	fact("Level", lv)
	fact("Project", deref(e.Project))
	t := e.Ts
	if t.IsZero() {
		t = e.ReceivedAt
	}
	if !t.IsZero() {
		fact("Time", t.UTC().Truncate(time.Second).Format(time.RFC3339))
	}
	fact("Link", deref(e.Url))
	if e.Tags != nil {
		fact("Tags", strings.Join(*e.Tags, ", "))
	}
	if e.AttachmentInfo != nil && len(*e.AttachmentInfo) > 0 {
		types := make([]string, 0, len(*e.AttachmentInfo))
		for _, a := range *e.AttachmentInfo {
			types = append(types, a.ContentType)
		}
		fact("Attachments", strconv.Itoa(len(types))+" ("+strings.Join(types, ", ")+")")
	}
	b.WriteString("\n" + EventLink(e.Id) + "\n")
	return b.String()
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// mdLine is s on one line: trimmed, with line breaks as spaces.
func mdLine(s string) string {
	s = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(s)
	return strings.TrimSpace(s)
}

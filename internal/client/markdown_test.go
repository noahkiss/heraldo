package client

import (
	"encoding/json"
	"testing"
)

// The apps build the same text; HeraldoKit's ShareTextTests holds the same
// two cases, so a change to one side shows on the other.
func TestMarkdown(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"id":"01JAAAAAAAAAAAAAAAAAAAAAAA","source":"watch","kind":"incident","action":"open","key":"disk:a",
		  "host":"box1","level":"time-sensitive","project":"infra","title":"Disk a full","summary":"98% used",
		  "body":"Free space is low.\n\nClean up.","url":"https://example.com/disk","tags":["disk","ops"],
		  "ts":"2026-10-29T07:30:00.123Z","received_at":"2026-10-29T07:30:01Z",
		  "attachment_info":[{"id":"x","content_type":"image/png","bytes":10},{"id":"y","content_type":"text/plain","bytes":3}]}`,
			"# Disk a full\n\n98% used\n\nFree space is low.\n\nClean up.\n\n" +
				"- Source: watch\n- Host: box1\n- Kind: incident open\n- Key: disk:a\n- Level: time-sensitive\n" +
				"- Project: infra\n- Time: 2026-10-29T07:30:00Z\n- Link: https://example.com/disk\n- Tags: disk, ops\n" +
				"- Attachments: 2 (image/png, text/plain)\n\nheraldo://event/01JAAAAAAAAAAAAAAAAAAAAAAA\n"},
		{`{"id":"01JBBBBBBBBBBBBBBBBBBBBBBB","source":"share:iphone","kind":"message","summary":"Line one\nline two",
		  "ts":"2026-10-29T08:00:00Z"}`,
			"# Line one line two\n\n- Source: share:iphone\n- Level: active\n" +
				"- Time: 2026-10-29T08:00:00Z\n\nheraldo://event/01JBBBBBBBBBBBBBBBBBBBBBBB\n"},
	}
	for i, c := range cases {
		var e Event
		if err := json.Unmarshal([]byte(c.in), &e); err != nil {
			t.Fatal(err)
		}
		if got := Markdown(e); got != c.want {
			t.Errorf("case %d:\n got %q\nwant %q", i, got, c.want)
		}
	}
}

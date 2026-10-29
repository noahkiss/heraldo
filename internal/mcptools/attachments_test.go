package mcptools

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var testPNG = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)

func TestSendAttachmentsAndRead(t *testing.T) {
	f := newFixture(t)
	s := f.connect(true)
	logText := strings.Repeat("line of a long log\n", 2000) // over one page
	out, _ := s.ok("send", map[string]any{
		"title": "see the screenshot", "level": "passive",
		"attachments": []map[string]any{
			{"data": base64.StdEncoding.EncodeToString(testPNG), "content_type": "image/png"},
			{"data": base64.StdEncoding.EncodeToString([]byte(logText)), "content_type": "text/plain"},
		},
	})
	ids, _ := out["attachments"].([]any)
	if len(ids) != 2 {
		t.Fatalf("send: %v", out)
	}
	ev, text := s.ok("event_read", map[string]any{"id": out["id"]})
	if info, _ := ev["attachment_info"].([]any); len(info) != 2 || !strings.Contains(text, "attached: "+ids[0].(string)) {
		t.Fatalf("event_read: %v\n%s", ev, text)
	}
	// Text comes inline, a page at a time.
	a, text := s.ok("attachment_read", map[string]any{"id": ids[1]})
	pg, _ := a["page"].(map[string]any)
	if a["content_type"] != "text/plain" || pg == nil || pg["next_offset"] == nil || !strings.HasPrefix(a["text"].(string), "line of a long log") {
		t.Fatalf("text page 1: %v", a)
	}
	rest, _ := s.ok("attachment_read", map[string]any{"id": ids[1], "offset": pg["next_offset"]})
	if a["text"].(string)+rest["text"].(string) != logText {
		t.Fatalf("pages do not join to the file")
	}
	_ = text
	// An image comes as image content.
	res, err := s.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "attachment_read", Arguments: map[string]any{"id": ids[0]}})
	if err != nil || res.IsError {
		t.Fatalf("image: %v %+v", err, res)
	}
	var img *mcp.ImageContent
	for _, c := range res.Content {
		if ic, ok := c.(*mcp.ImageContent); ok {
			img = ic
		}
	}
	if img == nil || img.MIMEType != "image/png" || !bytes.Equal(img.Data, testPNG) {
		t.Fatalf("image content: %+v", res.Content)
	}
	s.fail("attachment_read", map[string]any{"id": "att_01JBA6Z0000000000000000099"}, "not_found")
	s.fail("send", map[string]any{"title": "x", "attachments": []map[string]any{{"data": "!!not base64"}}}, "invalid")
}

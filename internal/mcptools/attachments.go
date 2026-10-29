package mcptools

import (
	"context"
	"encoding/base64"
	"fmt"
	"mime"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/noahkiss/heraldo/internal/client"
)

// Caps on send's attachments: at most this many, each at most the
// server's cap once decoded.
const (
	sendAttachmentsMax = 4
	sendAttachmentMax  = client.MaxAttachmentBytes
)

// attachmentTextPage is how much text attachment_read returns per call.
const attachmentTextPage = 32 << 10

var attachmentRe = regexp.MustCompile(`^att_[0-9A-HJKMNP-TV-Z]{26}$`)

// sendAttachment is one file in send's attachments.
type sendAttachment struct {
	Data        string `json:"data" jsonschema:"The file, base64 encoded."`
	ContentType string `json:"content_type,omitempty" jsonschema:"Its media type, such as image/png or text/plain. Default: the server reads the bytes."`
}

// uploadAll decodes and uploads send's attachments and returns their ids.
func (t *tools) uploadAll(ctx context.Context, in []sendAttachment) ([]string, error) {
	if len(in) > sendAttachmentsMax {
		return nil, toolErr("invalid", fmt.Sprintf("at most %d attachments", sendAttachmentsMax), "")
	}
	var ids []string
	for i, a := range in {
		data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(a.Data))
		if err != nil {
			return nil, toolErr("invalid", fmt.Sprintf("attachments[%d].data is not base64: %v", i, err), "")
		}
		if len(data) == 0 {
			return nil, toolErr("invalid", fmt.Sprintf("attachments[%d] is empty", i), "")
		}
		if len(data) > sendAttachmentMax {
			return nil, toolErr("too_large", fmt.Sprintf("attachments[%d] is %d bytes; the cap is %d", i, len(data), sendAttachmentMax), "")
		}
		ct, _, _ := mime.ParseMediaType(a.ContentType)
		up, err := t.API.UploadAttachment(ctx, ct, data)
		if err != nil {
			return nil, t.apiErr(err, call{write: true})
		}
		ids = append(ids, up.Id)
	}
	return ids, nil
}

// ---- attachment_read ----------------------------------------------------

type attachmentIn struct {
	ID     string `json:"id" jsonschema:"The attachment id, att_ and a ULID, from event_read's attachment_info."`
	Offset int    `json:"offset,omitempty" jsonschema:"Byte offset into a text attachment, from page.next_offset."`
}

const attachmentReadDesc = `Read one file attached to a heraldo event, such as a screenshot or a log the owner shared from their phone.

Best for: an id from event_read's attachment_info.
Returns: text and JSON inline (32 KiB per call; pass page.next_offset as offset to read on), images as image content. A PDF or other type returns its type and size only.
Not for: finding attachments; read the event with event_read first.`

func (t *tools) addAttachmentRead(s *mcp.Server) {
	addTool(t, s, &mcp.Tool{Name: "attachment_read", Description: attachmentReadDesc, Annotations: readOnly("Read attachment")},
		func(is *jsonschema.Schema) {
			is.Properties["id"].Pattern = attachmentRe.String()
			is.Properties["offset"].Minimum = ptr(0.0)
		},
		map[string]*jsonschema.Schema{
			"id": str(""), "content_type": str(""), "bytes": integer("The whole file's size."),
			"text": str("A text attachment's text, at most 32 KiB."),
			"page": obj("Set when the text was cut.", map[string]*jsonschema.Schema{
				"offset": integer(""), "next_offset": integer("Absent on the last page."),
			}),
			"image": boolean("True when the image comes as image content."),
		},
		t.attachmentRead)
}

func (t *tools) attachmentRead(ctx context.Context, _ *mcp.CallToolRequest, in attachmentIn) (result, error) {
	if !attachmentRe.MatchString(in.ID) {
		return result{}, toolErr("invalid", "id must be att_ and a 26-character ULID", "take it from event_read's attachment_info")
	}
	data, ctHeader, err := t.API.Attachment(ctx, in.ID)
	if err != nil {
		return result{}, t.apiErr(err, call{what: "attachment " + in.ID, lister: "event_read"})
	}
	ct, _, _ := mime.ParseMediaType(ctHeader)
	out := map[string]any{"id": in.ID, "content_type": ct, "bytes": len(data)}
	head := fmt.Sprintf("%s: %s, %d bytes", in.ID, ct, len(data))
	switch {
	case strings.HasPrefix(ct, "image/"):
		out["image"] = true
		return result{out: out, text: head, extra: []mcp.Content{&mcp.ImageContent{Data: data, MIMEType: ct}}}, nil
	case ct == "text/plain" || ct == "application/json":
		off := min(in.Offset, len(data))
		for off < len(data) && !utf8.RuneStart(data[off]) {
			off++
		}
		end := min(off+attachmentTextPage, len(data))
		for end < len(data) && !utf8.RuneStart(data[end]) {
			end--
		}
		text := strings.ToValidUTF8(string(data[off:end]), "�")
		out["text"] = text
		if off > 0 || end < len(data) {
			pg := map[string]any{"offset": off}
			if end < len(data) {
				pg["next_offset"] = end
				head += fmt.Sprintf(" (cut at %d: call again with offset=%d)", end, end)
			}
			out["page"] = pg
		}
		return result{out: out, text: head + "\n\n" + text}, nil
	}
	return result{out: out, text: head + "\nThis type is not shown inline. Fetch it with: heraldo attachment get -o <file> " + in.ID}, nil
}

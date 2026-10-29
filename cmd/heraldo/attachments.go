package main

import (
	"context"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/noahkiss/heraldo/internal/client"
	"github.com/noahkiss/heraldo/internal/version"
)

// attachmentRows are an event's attachments as show prints them: id, type
// and size. An id the server no longer holds shows as missing.
func attachmentRows(e client.Event) []string {
	if e.Attachments == nil {
		return nil
	}
	info := map[string]string{}
	if e.AttachmentInfo != nil {
		for _, a := range *e.AttachmentInfo {
			info[a.Id] = fmt.Sprintf("%s, %s", a.ContentType, byteSize(a.Bytes))
		}
	}
	var out []string
	for _, id := range *e.Attachments {
		d, ok := info[id]
		if !ok {
			d = "missing"
		}
		out = append(out, fmt.Sprintf("%s (%s)", id, d))
	}
	return out
}

func byteSize(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// listFlag is a flag given once per value.
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ", ") }
func (l *listFlag) Set(s string) error { *l = append(*l, s); return nil }

// uploadFiles stores each file on the server and returns the ids, in order.
func uploadFiles(ctx context.Context, c *client.Client, paths []string) ([]string, error) {
	var ids []string
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if fi.Size() > client.MaxAttachmentBytes {
			return nil, fmt.Errorf("%s: %d bytes; an attachment is at most %d", p, fi.Size(), client.MaxAttachmentBytes)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		ct, _, _ := mime.ParseMediaType(mime.TypeByExtension(filepath.Ext(p)))
		a, err := c.UploadAttachment(ctx, ct, data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		ids = append(ids, a.Id)
	}
	return ids, nil
}

// attachment is heraldo attachment get <id> [-o file].
func (a *app) attachment(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "get" {
		fmt.Fprintln(a.stderr, "usage: heraldo attachment get [-o file] <id>")
		return errUsage
	}
	fs := a.flags("attachment get", "[-o file] <id>")
	out := fs.String("o", "", "write the bytes to this file; default stdout")
	pos, err := parse(fs, args[1:])
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
	data, _, err := c.Attachment(ctx, pos[0])
	if err != nil {
		return err
	}
	if *out == "" || *out == "-" {
		_, err = a.stdout.Write(data)
		return err
	}
	// O_EXCL: never overwrite a file the user did not mean to lose.
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// sendAttached is send with --attach: it uploads the files to the heraldo
// API and posts ev, which names them, there too. The notify URL takes no
// files, so it is not used.
func (a *app) sendAttached(ctx context.Context, files []string, ev map[string]any, project string) error {
	u, tok := a.setting(envURL), a.setting(envWriteToken)
	if u == "" || tok == "" {
		return fmt.Errorf("--attach posts to the heraldo API: set %s and %s", envURL, envWriteToken)
	}
	c := &client.Client{BaseURL: u, Token: tok, WriteToken: tok, HTTP: a.http,
		UserAgent: "heraldo-cli/" + version.String()}
	ids, err := uploadFiles(ctx, c, files)
	if err != nil {
		return err
	}
	if project != "" {
		ev["project"] = project
	}
	ev["attachments"] = ids
	if _, err := c.Post(ctx, ev); err != nil {
		return err
	}
	fmt.Fprintln(a.stdout, sentLine)
	return nil
}

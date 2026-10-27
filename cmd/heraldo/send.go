package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Notify's output lines. Scripts that wrap a notify command match them.
const (
	sentLine   = "✅ Notification sent successfully."
	failedLine = "❌ Failed to send notification. HTTP Status: %s\n"
)

// send posts one notification to an Apprise-compatible notify URL, such as
// an apprise-tee keyed route. It keeps the contract of the notify script it
// replaces: the URL comes from the environment and, unset, makes send a
// silent no-op; the message comes from -m or stdin; success prints the sent
// line and exits 0, anything else prints the failed line and exits 1.
//
// Success means HTTP 200 with a "Sent" line in the answer, the delivery test
// the tee itself applies; an Apprise target that matched nothing answers
// without one.
func (a *app) send(ctx context.Context, args []string) error {
	fs := a.flags("send", "[-t title] [-m message] [--tag rung] [--source id] [--url u] [--image u] [--level rung]")
	var title, message, tag, source, link, image, lvl string
	for _, n := range []string{"t", "title"} {
		fs.StringVar(&title, n, "System Notification", "notification title")
	}
	for _, n := range []string{"m", "message"} {
		fs.StringVar(&message, n, "", "notification body; read from stdin when left out")
	}
	fs.StringVar(&tag, "tag", "", "Apprise tag, the ladder rung")
	fs.StringVar(&source, "source", "", "short sender id")
	fs.StringVar(&link, "url", "", "link to attach")
	fs.StringVar(&image, "image", "", "https image URL to attach")
	fs.StringVar(&lvl, "level", "", "rung that replaces the tag for routing")
	if pos, err := parse(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		fmt.Fprintf(a.stdout, "Unknown parameter: %s\n", pos[0])
		return errQuiet
	}
	target := a.setting(envSendURL)
	if target == "" {
		target = a.setting(envAppriseURL)
	}
	if target == "" {
		return nil
	}
	if message == "" {
		if a.stdinIsTTY() {
			fmt.Fprintln(a.stdout, "Error: Message is required.")
			fmt.Fprintln(a.stdout, "Usage: heraldo send -t 'Title' -m 'Message'")
			return errQuiet
		}
		b, err := io.ReadAll(io.LimitReader(a.stdin, 1<<20))
		if err != nil {
			return err
		}
		message = strings.TrimRight(string(b), "\n")
	}
	payload := map[string]any{}
	if extra := strings.TrimSpace(a.setting(envExtraJSON)); extra != "" {
		if err := json.Unmarshal([]byte("{"+extra+"}"), &payload); err != nil {
			return fmt.Errorf("%s: not JSON object fields: %v", envExtraJSON, err)
		}
	}
	payload["title"] = title
	payload["body"] = message
	for k, v := range map[string]string{"tag": tag, "source": source, "url": link, "image": image, "level": lvl} {
		if v != "" {
			payload[k] = v
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	hc := a.http
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		// curl reports a request that got no answer as status 000.
		fmt.Fprintf(a.stdout, failedLine, "000")
		fmt.Fprintf(a.stderr, "heraldo send: %v\n", redactURL(err.Error(), target))
		return errQuiet
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusOK && bytes.Contains(snippet, []byte("Sent")) {
		fmt.Fprintln(a.stdout, sentLine)
		return nil
	}
	fmt.Fprintf(a.stdout, failedLine, fmt.Sprint(resp.StatusCode))
	if resp.StatusCode == http.StatusOK {
		fmt.Fprintln(a.stderr, "heraldo send: HTTP 200 without a Sent line: no target took the message")
	}
	return errQuiet
}

// redactURL keeps a notify URL's query, which may hold a token, out of an
// error message.
func redactURL(msg, target string) string {
	if i := strings.IndexByte(target, '?'); i >= 0 {
		msg = strings.ReplaceAll(msg, target, target[:i]+"?…")
	}
	return msg
}

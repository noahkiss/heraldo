package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/noahkiss/heraldo/internal/validate"
)

// connectTimeout bounds the TCP and TLS setup of each send attempt, so a
// host that is down fails in seconds, not after the system's ~75 s.
const connectTimeout = 5 * time.Second

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
//
// With HERALDO_FALLBACK_URL and HERALDO_FALLBACK_TOKEN set, a failed notify
// URL is not the end: send posts the message as an event to that heraldo
// server, such as a standby, and prints the sent line when it stores it.
// stderr then names the fallback.
//
// --project, else HERALDO_PROJECT, names the message's project. It folds to
// lower case; a name the envelope refuses is dropped with a warning, so a
// bad name never costs the message.
func (a *app) send(ctx context.Context, args []string) error {
	fs := a.flags("send", "[-t title] [-m message] [--tag rung] [--source id] [--project name] [--url u] [--image u] [--level rung]")
	var title, message, tag, source, project, link, image, lvl string
	for _, n := range []string{"t", "title"} {
		fs.StringVar(&title, n, "System Notification", "notification title")
	}
	for _, n := range []string{"m", "message"} {
		fs.StringVar(&message, n, "", "notification body; read from stdin when left out")
	}
	fs.StringVar(&tag, "tag", "", "Apprise tag, the ladder rung")
	fs.StringVar(&source, "source", "", "short sender id")
	fs.StringVar(&project, "project", "", "the project; default $"+envProject)
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
	if project == "" {
		project = a.setting(envProject)
	}
	if project != "" {
		if p := validate.FoldProject(project); validate.ValidProject(p) {
			project = p
		} else {
			fmt.Fprintf(a.stderr, "heraldo send: %q is not a project name; sending without one\n", project)
			project = ""
		}
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
	for k, v := range map[string]string{"tag": tag, "source": source, "project": project, "url": link, "image": image, "level": lvl} {
		if v != "" {
			payload[k] = v
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	hc := a.http
	if hc == nil {
		hc = &http.Client{Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			DialContext:         (&net.Dialer{Timeout: connectTimeout}).DialContext,
			TLSHandshakeTimeout: connectTimeout,
		}}
	}
	fallback, fallbackTok := a.setting(envFallback), a.setting(envFallbackTk)
	limit := 30 * time.Second
	if fallback != "" {
		// Leave the fallback its own time.
		limit = 15 * time.Second
	}
	status, why := postNotify(ctx, hc, target, body, limit)
	if status == "" {
		fmt.Fprintln(a.stdout, sentLine)
		return nil
	}
	if fallback == "" {
		fmt.Fprintf(a.stdout, failedLine, status)
		if why != "" {
			fmt.Fprintf(a.stderr, "heraldo send: %s\n", redactURL(why, target))
		}
		return errQuiet
	}
	if fallbackTok == "" {
		fmt.Fprintf(a.stdout, failedLine, status)
		fmt.Fprintf(a.stderr, "heraldo send: %s is set, but %s is not\n", envFallback, envFallbackTk)
		return errQuiet
	}
	ev := fallbackEvent(title, message, tag, source, link, image, lvl)
	if project != "" {
		ev["project"] = project
	}
	if err := postFallback(ctx, hc, fallback, fallbackTok, ev); err != nil {
		fmt.Fprintf(a.stdout, failedLine, status)
		fmt.Fprintf(a.stderr, "heraldo send: notify URL: HTTP %s; fallback: %v\n", status, err)
		return errQuiet
	}
	fmt.Fprintln(a.stdout, sentLine)
	fmt.Fprintf(a.stderr, "heraldo send: notify URL failed (HTTP %s); the fallback %s stored it\n", status, hostOf(fallback))
	return nil
}

// postNotify posts body to an Apprise-compatible notify URL. It returns ""
// on success, else the status for the failed line ("000" when no answer
// came) and, when there is more to say, why.
func postNotify(ctx context.Context, hc *http.Client, target string, body []byte, limit time.Duration) (string, string) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return "000", err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	resp, err := hc.Do(req)
	if err != nil {
		// curl reports a request that got no answer as status 000.
		return "000", err.Error()
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusOK && bytes.Contains(snippet, []byte("Sent")) {
		return "", ""
	}
	if resp.StatusCode == http.StatusOK {
		return "200", "HTTP 200 without a Sent line: no target took the message"
	}
	return fmt.Sprint(resp.StatusCode), ""
}

// envelopeLevels are the envelope's levels. A --level that is not one is a
// ladder rung, passed on as a tag for the fallback's rules.
var envelopeLevels = map[string]bool{"passive": true, "active": true, "time-sensitive": true, "critical": true}

// fallbackEvent maps send's flags onto an envelope v1 input, as the tee
// import maps a catalogue line: the tag split into tags, a rung --level
// first among them, an envelope --level as level.
func fallbackEvent(title, message, tag, source, link, image, lvl string) map[string]any {
	if source == "" {
		source = "heraldo-send"
	}
	ev := map[string]any{"source": cutRunes(source, 128), "kind": "message", "title": cutRunes(title, 256)}
	if message != "" {
		ev["body"] = cutBytes(message, 256<<10)
	}
	var tags []string
	if lvl != "" && !envelopeLevels[lvl] {
		tags = append(tags, lvl)
	}
	tags = append(tags, strings.FieldsFunc(tag, func(r rune) bool { return r == ',' || r == ' ' })...)
	var keep []string
	for _, t := range tags {
		if len(keep) < 16 && utf8.RuneCountInString(t) <= 64 {
			keep = append(keep, t)
		}
	}
	if len(keep) > 0 {
		ev["tags"] = keep
	}
	if envelopeLevels[lvl] {
		ev["level"] = lvl
	}
	for k, v := range map[string]string{"url": link, "image": image} {
		if u, err := url.Parse(v); v != "" && err == nil && u.Scheme != "" {
			ev[k] = v
		}
	}
	return ev
}

// postFallback stores ev on the heraldo server at root with a write token.
func postFallback(ctx context.Context, hc *http.Client, root, token string, ev map[string]any) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(root, "/")+"/v1/events", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return ue.Err
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
}

// hostOf is the host of a URL, for a message that must not print a path or
// query.
func hostOf(u string) string {
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return "server"
	}
	return p.Host
}

func cutRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// redactURL keeps a notify URL's query, which may hold a token, out of an
// error message.
func redactURL(msg, target string) string {
	if i := strings.IndexByte(target, '?'); i >= 0 {
		msg = strings.ReplaceAll(msg, target, target[:i]+"?…")
	}
	return msg
}

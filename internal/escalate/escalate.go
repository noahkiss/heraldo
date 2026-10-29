// Package escalate sends critical events to Pushover's emergency priority,
// a second path that repeats an alert until someone acknowledges it. The
// server calls Pushover's API directly: POST /1/messages.json with priority
// 2, retry and expire, then POST /1/receipts/{receipt}/cancel.json when the
// incident is acked or resolved in heraldo.
//
// It is off by default (escalation.enabled). The application token and the
// user key are secrets in the settings store (escalation.pushover.token and
// escalation.pushover.user); they never reach a log or a trace.
//
// It runs beside the push pipeline, on its own queue: an emergency
// escalates with push off, with no device registered, and whatever APNs
// answers. Only a mute or an inhibit rule holds it back, as they hold back
// the push. Each outcome is a row of the event's trace (stage escalate).
package escalate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/push"
	"github.com/noahkiss/heraldo/internal/rules"
	"github.com/noahkiss/heraldo/internal/settings"
	"github.com/noahkiss/heraldo/internal/store"
)

// PushoverURL is Pushover's API root.
const PushoverURL = "https://api.pushover.net"

// Pushover's limits on an emergency message.
const (
	MinRetry  = 30 * time.Second
	MaxExpire = 3 * time.Hour
	maxTitle  = 250
	maxBody   = 1024
)

// Config configures a Pushover escalator.
type Config struct {
	Store *store.Store
	// Settings is where the escalation keys are read; nil means the
	// default Manager (settings.Get).
	Settings *settings.Manager
	// BaseURL replaces PushoverURL. Tests point it at a fake server.
	BaseURL string
	// HTTPClient nil means a client with a 10-second timeout: a slow
	// Pushover holds up the critical push by no more than that.
	HTTPClient *http.Client
	Now        func() time.Time
	Logger     *slog.Logger
	// Rules returns the rules in force, for mutes by project alias and the
	// inhibit rules; nil means none.
	Rules func() *rules.Set
	// Queue is the number of events that may wait; 0 means 1024.
	Queue int
}

// Pushover escalates to Pushover. The server hands it every stored event
// with Enqueue; Run works the queue.
type Pushover struct {
	cfg     Config
	queue   chan store.Record
	pending sync.WaitGroup
}

// New returns a Pushover escalator.
func New(cfg Config) *Pushover {
	if cfg.BaseURL == "" {
		cfg.BaseURL = PushoverURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Rules == nil {
		cfg.Rules = func() *rules.Set { return nil }
	}
	if cfg.Queue == 0 {
		cfg.Queue = 1024
	}
	return &Pushover{cfg: cfg, queue: make(chan store.Record, cfg.Queue)}
}

// Enqueue queues a stored event. It never blocks: with the queue full it
// logs the event and returns false.
func (p *Pushover) Enqueue(rec store.Record) bool {
	p.pending.Add(1)
	select {
	case p.queue <- rec:
		return true
	default:
		p.pending.Done()
		p.cfg.Logger.Error("escalate: queue full; event not checked", "event", rec.ID)
		return false
	}
}

// SweepWindow is how far back the start-up sweep looks.
const SweepWindow = 6 * time.Hour

// Sweep queues the critical events received since since that no
// escalation has seen, such as those still queued when the server last
// stopped, and returns how many it queued (store.Unescalated). Each one is
// decided as if it were new, so a mute or an expiry still holds it back.
// The server calls it once at start-up, after Run.
func (p *Pushover) Sweep(ctx context.Context, since time.Time) (int, error) {
	recs, err := p.cfg.Store.Unescalated(ctx, since)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, rec := range recs {
		if p.Enqueue(rec) {
			n++
		}
	}
	return n, nil
}

// Wait blocks until every queued event is handled. Tests use it.
func (p *Pushover) Wait() { p.pending.Wait() }

// Drain waits until the queue is empty or ctx ends, and reports whether it
// emptied. The server calls it at shutdown.
func (p *Pushover) Drain(ctx context.Context) bool {
	done := make(chan struct{})
	go func() { p.pending.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// Run works the queue until ctx ends. One worker: escalations are rare, and
// one at a time keeps "already escalated" exact.
func (p *Pushover) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case rec := <-p.queue:
			p.handle(ctx, rec)
			p.pending.Done()
		}
	}
}

// handle escalates a critical event or cancels the escalation an ack or
// resolve ends, and records the outcome in the event's trace.
func (p *Pushover) handle(ctx context.Context, rec store.Record) {
	var ev api.Event
	if err := json.Unmarshal(rec.Event, &ev); err != nil {
		p.cfg.Logger.Error("escalate: decode event", "event", rec.ID, "err", err)
		return
	}
	if ev.Kind == api.KindIncident && ev.Key != nil && *ev.Key != "" && ev.Action != nil &&
		(*ev.Action == api.EventActionAck || *ev.Action == api.EventActionResolve) {
		why := map[api.EventAction]string{api.EventActionAck: "acked", api.EventActionResolve: "resolved"}[*ev.Action]
		why += " by " + ev.Source + " (" + ev.Id + ")"
		if reason, detail, ok := p.Cancel(ctx, *ev.Key, why); ok {
			p.record(ctx, ev.Id, "pass", reason, detail)
		}
		return
	}
	if ev.Level == nil || *ev.Level != api.LevelCritical {
		return
	}
	now := p.cfg.Now()
	d := push.Decide(&ev, now)
	if !d.Push || d.Level != api.LevelCritical {
		// The pipeline's trace says why: held, expired, a kind that does
		// not push, or a critical nobody allowed.
		return
	}
	held, err := push.HeldBack(ctx, p.cfg.Store, p.cfg.Rules(), &ev, now)
	if err != nil {
		p.cfg.Logger.Error("escalate: mutes and inhibit rules", "err", err)
	}
	if held != "" {
		p.record(ctx, ev.Id, "drop", "not escalated: "+held, nil)
		return
	}
	reason, detail := p.Escalate(ctx, &ev)
	p.record(ctx, ev.Id, "pass", reason, detail)
}

// record appends one escalate row to an event's trace.
func (p *Pushover) record(ctx context.Context, id, outcome, reason string, detail map[string]any) {
	row := store.DecisionRow{Stage: store.AppendStage, Outcome: outcome, Reason: reason, At: p.cfg.Now()}
	if len(detail) > 0 {
		row.Detail, _ = json.Marshal(detail)
	}
	if err := p.cfg.Store.AppendDecision(context.WithoutCancel(ctx), id, row); err != nil {
		p.cfg.Logger.Error("escalate: record trace", "event", id, "err", err)
	}
}

func get[T any](p *Pushover, name string) T {
	if p.cfg.Settings != nil {
		return settings.From[T](p.cfg.Settings, name)
	}
	return settings.Get[T](name)
}

// Escalate sends ev at Pushover's emergency priority, unless escalation is
// off, a key or user is missing, or an escalation of the event's key still
// runs (a re-alert or a later update of the same incident).
func (p *Pushover) Escalate(ctx context.Context, ev *api.Event) (string, map[string]any) {
	if !get[bool](p, "escalation.enabled") {
		return "escalation off", nil
	}
	token, user := get[string](p, "escalation.pushover.token"), get[string](p, "escalation.pushover.user")
	if token == "" || user == "" {
		return "escalation on, but escalation.pushover.token or escalation.pushover.user is not set", nil
	}
	now := p.cfg.Now()
	key := ""
	if ev.Key != nil {
		key = *ev.Key
	}
	running, err := p.cfg.Store.RunningEscalations(ctx, key, now)
	if err != nil {
		p.cfg.Logger.Error("escalate: running escalations", "err", err)
		return "escalation lookup failed; not escalated", nil
	}
	if len(running) > 0 {
		e := running[0]
		return "already escalated to Pushover by " + e.EventID, map[string]any{"provider": "pushover", "receipt": e.Receipt, "event": e.EventID}
	}
	retry, expire := Limits(get[time.Duration](p, "escalation.retry"), get[time.Duration](p, "escalation.expire"))
	form := url.Values{
		"token":     {token},
		"user":      {user},
		"priority":  {"2"},
		"retry":     {strconv.Itoa(int(retry / time.Second))},
		"expire":    {strconv.Itoa(int(expire / time.Second))},
		"title":     {cutRunes(title(ev), maxTitle)},
		"message":   {cutRunes(message(ev), maxBody)},
		"timestamp": {strconv.FormatInt(ev.Ts.Unix(), 10)},
	}
	if ev.Url != nil && *ev.Url != "" {
		form.Set("url", *ev.Url)
	}
	var out struct {
		Status  int      `json:"status"`
		Receipt string   `json:"receipt"`
		Errors  []string `json:"errors"`
	}
	if err := p.post(ctx, "/1/messages.json", form, &out); err != nil {
		p.cfg.Logger.Warn("escalate: Pushover send failed", "event", ev.Id, "err", err)
		return "escalation to Pushover failed: " + err.Error(), map[string]any{"provider": "pushover"}
	}
	if out.Receipt == "" {
		return "escalation to Pushover failed: no receipt in the answer", map[string]any{"provider": "pushover"}
	}
	e := store.Escalation{EventID: ev.Id, Key: key, Provider: "pushover", Receipt: out.Receipt,
		SentAt: now, ExpiresAt: now.Add(expire)}
	if err := p.cfg.Store.AddEscalation(context.WithoutCancel(ctx), e); err != nil {
		p.cfg.Logger.Error("escalate: record", "event", ev.Id, "err", err)
	}
	detail := map[string]any{"provider": "pushover", "receipt": out.Receipt,
		"retry_s": int(retry / time.Second), "expire_s": int(expire / time.Second)}
	if key == "" {
		return "escalated to Pushover; no key, so only Pushover can acknowledge it", detail
	}
	return "escalated to Pushover", detail
}

// Cancel cancels every running escalation of key at Pushover. One that
// fails to cancel stays running, so a later ack or resolve tries again.
func (p *Pushover) Cancel(ctx context.Context, key, why string) (string, map[string]any, bool) {
	now := p.cfg.Now()
	running, err := p.cfg.Store.RunningEscalations(ctx, key, now)
	if err != nil {
		p.cfg.Logger.Error("escalate: running escalations", "err", err)
		return "", nil, false
	}
	if len(running) == 0 {
		return "", nil, false
	}
	token := get[string](p, "escalation.pushover.token")
	var done, failed []string
	var lastErr error
	for _, e := range running {
		err := errors.New("escalation.pushover.token is not set")
		if token != "" {
			err = p.post(ctx, "/1/receipts/"+url.PathEscape(e.Receipt)+"/cancel.json", url.Values{"token": {token}}, nil)
		}
		if err != nil {
			p.cfg.Logger.Warn("escalate: Pushover cancel failed", "event", e.EventID, "err", err)
			failed, lastErr = append(failed, e.EventID), err
			continue
		}
		if err := p.cfg.Store.CancelEscalation(context.WithoutCancel(ctx), e.EventID, now, why); err != nil {
			p.cfg.Logger.Error("escalate: record cancel", "event", e.EventID, "err", err)
		}
		done = append(done, e.EventID)
	}
	detail := map[string]any{"provider": "pushover"}
	if len(done) > 0 {
		detail["cancelled"] = done
	}
	if len(failed) > 0 {
		detail["failed"] = failed
		detail["error"] = lastErr.Error()
	}
	if len(done) == 0 {
		return "Pushover cancel failed: " + lastErr.Error(), detail, true
	}
	return "cancelled the Pushover escalation of " + strings.Join(done, ", ") + ": " + why, detail, true
}

// Limits clamps retry and expire to what Pushover takes: retry 30 s or
// more, expire 3 h or less and not below retry.
func Limits(retry, expire time.Duration) (time.Duration, time.Duration) {
	retry = max(retry, MinRetry)
	expire = min(expire, MaxExpire)
	expire = max(expire, retry)
	return retry, expire
}

// post sends form to path and decodes a status 1 answer into out. An
// error never carries the form: it holds the token and user key.
func (p *Pushover) post(ctx context.Context, path string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BaseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return errors.New("bad request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.cfg.HTTPClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("no answer: %v", ue.Err)
		}
		return errors.New("no answer")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var ans struct {
		Status int      `json:"status"`
		Errors []string `json:"errors"`
	}
	_ = json.Unmarshal(raw, &ans)
	if resp.StatusCode != http.StatusOK || ans.Status != 1 {
		msg := strings.Join(ans.Errors, "; ")
		if msg == "" {
			msg = "status " + strconv.Itoa(resp.StatusCode)
		}
		return errors.New(msg)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func title(ev *api.Event) string {
	if ev.Title != nil && *ev.Title != "" {
		return *ev.Title
	}
	return ev.Source
}

func message(ev *api.Event) string {
	switch {
	case ev.Summary != nil && *ev.Summary != "":
		return *ev.Summary
	case ev.Body != nil && *ev.Body != "":
		return *ev.Body
	}
	return "Critical from " + ev.Source
}

func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

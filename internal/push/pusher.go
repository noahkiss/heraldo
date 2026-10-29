// Package push sends heraldo events to Apple devices through APNs: HTTP/2
// to Apple, token auth with an ES256 provider token, one queue drained by
// workers so that ingest never waits on Apple.
package push

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/checkin"
	"github.com/noahkiss/heraldo/internal/rules"
	"github.com/noahkiss/heraldo/internal/store"
)

// Apple's APNs hosts.
const (
	ProductionURL = "https://api.push.apple.com"
	SandboxURL    = "https://api.sandbox.push.apple.com"
)

// BadgeCursor is the read cursor the badge counts unread events against.
const BadgeCursor = "inbox"

// DefaultBackoff is the wait before each retry of a push that APNs
// throttled (429) or failed on (5xx, or no answer).
var DefaultBackoff = []time.Duration{time.Second, 4 * time.Second, 15 * time.Second, time.Minute}

// Config configures a Pusher.
type Config struct {
	Key    *ecdsa.PrivateKey
	KeyID  string
	TeamID string
	Store  *store.Store
	Logger *slog.Logger
	// ProductionURL and SandboxURL replace Apple's hosts. Tests use them.
	ProductionURL string
	SandboxURL    string
	// HTTPClient must speak HTTP/2; nil means a client with a 30-second
	// timeout. The default transport negotiates HTTP/2 with Apple.
	HTTPClient *http.Client
	// Workers is the number of pushes in flight; 0 means 4.
	Workers int
	// Queue is the number of events that may wait; 0 means 1024.
	Queue int
	// Backoff is the wait before each retry; nil means DefaultBackoff.
	Backoff []time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Rules returns the rules in force, for their inhibit rules and the
	// project aliases a project mute matches through; nil means none.
	Rules func() *rules.Set
	// RealertTick is how often the pusher looks for due re-alerts; 0 means
	// 30 seconds.
	RealertTick time.Duration
}

// Pusher sends events to every active device.
type Pusher struct {
	cfg     Config
	sign    *signer
	queue   chan store.Record
	pending sync.WaitGroup
}

// New returns a Pusher. Start it with Run.
func New(cfg Config) *Pusher {
	if cfg.ProductionURL == "" {
		cfg.ProductionURL = ProductionURL
	}
	if cfg.SandboxURL == "" {
		cfg.SandboxURL = SandboxURL
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.Workers == 0 {
		cfg.Workers = 4
	}
	if cfg.Queue == 0 {
		cfg.Queue = 1024
	}
	if cfg.Backoff == nil {
		cfg.Backoff = DefaultBackoff
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
	if cfg.RealertTick == 0 {
		cfg.RealertTick = 30 * time.Second
	}
	return &Pusher{
		cfg:   cfg,
		sign:  &signer{key: cfg.Key, keyID: cfg.KeyID, teamID: cfg.TeamID, now: cfg.Now},
		queue: make(chan store.Record, cfg.Queue),
	}
}

// Enqueue queues a stored event for push. It never blocks: with the queue
// full it drops the event, logs it, and returns false. The app syncs history
// by cursor, so a dropped push loses no event.
func (p *Pusher) Enqueue(rec store.Record) bool {
	p.pending.Add(1)
	select {
	case p.queue <- rec:
		return true
	default:
		p.pending.Done()
		p.cfg.Logger.Warn("push: queue full; event not pushed", "event", rec.ID)
		return false
	}
}

// Wait blocks until every queued event is handled. Tests use it.
func (p *Pusher) Wait() { p.pending.Wait() }

// Drain waits until every queued event is handled or ctx ends, and reports
// whether the queue emptied. The server calls it at shutdown, after it stops
// taking events and before it stops Run.
func (p *Pusher) Drain(ctx context.Context) bool {
	done := make(chan struct{})
	go func() { p.pending.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// Sweep queues the events received since since that have no delivery yet,
// such as those still queued when the server last stopped, and returns how
// many it queued. The push table decides each one as if it were new. The
// server calls it once at start-up.
func (p *Pusher) Sweep(ctx context.Context, since time.Time) (int, error) {
	recs, err := p.cfg.Store.Unpushed(ctx, since)
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

// Run drains the queue and sends due re-alerts until ctx ends. Events still
// queued then are not pushed: call Drain first.
func (p *Pusher) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(p.cfg.RealertTick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				p.Realert(ctx)
			}
		}
	}()
	for range p.cfg.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case rec := <-p.queue:
					p.handle(ctx, rec)
					p.pending.Done()
				}
			}
		}()
	}
	wg.Wait()
}

func (p *Pusher) handle(ctx context.Context, rec store.Record) {
	log := p.cfg.Logger.With("event", rec.ID)
	var ev api.Event
	if err := json.Unmarshal(rec.Event, &ev); err != nil {
		log.Error("push: decode event", "err", err)
		return
	}
	d := Decide(&ev, p.cfg.Now())
	if !d.Push {
		return
	}
	if d.Replace {
		ok, err := p.cfg.Store.ReplacesVisible(ctx, *ev.Key, rec.Seq)
		if err != nil {
			log.Error("push: earlier pushes", "err", err)
		}
		if !ok {
			return
		}
	}
	p.push(ctx, log, &ev, d, 0)
}

// Realert pushes every due re-alert once and moves its schedule on. A
// server that was down for several intervals sends one re-alert, not one per
// missed interval. Run calls it on a ticker.
func (p *Pusher) Realert(ctx context.Context) {
	now := p.cfg.Now()
	due, err := p.cfg.Store.DueAlerts(ctx, now)
	if err != nil {
		p.cfg.Logger.Error("push: due re-alerts", "err", err)
		return
	}
	for _, a := range due {
		next := a.Due.Add(a.Every)
		if !next.After(now) {
			next = now.Add(a.Every)
		}
		// Move the schedule first: an ack meanwhile wins, and a failed push
		// is not retried every tick.
		if ok, err := p.cfg.Store.Realerted(ctx, a.Key, a.Due, next); err != nil || !ok {
			if err != nil {
				p.cfg.Logger.Error("push: re-alert schedule", "key", a.Key, "err", err)
			}
			continue
		}
		log := p.cfg.Logger.With("event", a.EventID, "realert", a.Key)
		rec, err := p.cfg.Store.Get(ctx, a.EventID)
		if err != nil {
			log.Error("push: re-alert event", "err", err)
			continue
		}
		var ev api.Event
		if err := json.Unmarshal(rec.Event, &ev); err != nil {
			log.Error("push: decode event", "err", err)
			continue
		}
		d := Decide(&ev, now)
		if !d.Push {
			continue
		}
		p.push(ctx, log, &ev, d, max(now.Sub(a.OpenedAt), time.Minute))
	}
}

// suppressed says why a mute or an inhibit rule holds back the push of ev,
// or "" when none does.
func (p *Pusher) suppressed(ctx context.Context, ev *api.Event) (string, error) {
	key := ""
	if ev.Key != nil {
		key = *ev.Key
	}
	set := p.cfg.Rules()
	// A project mute names the alias target; the event may carry the old
	// name.
	project := ""
	if ev.Project != nil {
		project = set.Alias(*ev.Project)
	}
	m, ok, err := p.cfg.Store.MatchMute(ctx, ev.Source, key, project, p.cfg.Now())
	if err == nil && !ok && key != "" && ev.Kind == api.KindCheckin {
		// A check-in's own events fall under its check's incident key, so
		// one mute on checkin:<slug> covers the incident and the runs.
		m, ok, err = p.cfg.Store.MatchMute(ctx, ev.Source, checkin.IncidentPrefix+key, project, p.cfg.Now())
	}
	if err != nil {
		return "", err
	}
	if ok {
		if m.Project != "" {
			return "muted by " + m.ID + " (project)", nil
		}
		return "muted by " + m.ID, nil
	}
	if key == "" || len(set.InhibitRules()) == 0 {
		return "", nil
	}
	open, err := p.cfg.Store.OpenIncidentKeys(ctx)
	if err != nil {
		return "", err
	}
	if rule, parent, ok := set.Inhibited(key, open); ok {
		return fmt.Sprintf("inhibited by %s: %s is open", rule, parent), nil
	}
	return "", nil
}

// icon returns the registry icon for ev: its icon field when the registry
// has that name, else the icon named after its source.
func (p *Pusher) icon(ctx context.Context, ev *api.Event) string {
	var names []string
	if ev.Icon != nil && *ev.Icon != "" {
		names = append(names, *ev.Icon)
	}
	for _, n := range append(names, ev.Source) {
		if ok, err := p.cfg.Store.HasIcon(ctx, n); err == nil && ok {
			return n
		}
	}
	return ""
}

// push sends ev with decision d to every active device and records the
// outcomes. openFor is set on a re-alert. A mute or an inhibit rule records
// skipped instead. A re-alert overwrites the event's delivery with its own
// outcome.
func (p *Pusher) push(ctx context.Context, log *slog.Logger, ev *api.Event, d Decision, openFor time.Duration) {
	devices, err := p.cfg.Store.Devices(ctx, true)
	if err != nil {
		log.Error("push: devices", "err", err)
		return
	}
	if len(devices) == 0 {
		return
	}
	why, err := p.suppressed(ctx, ev)
	if err != nil {
		log.Error("push: mutes and inhibit rules", "err", err)
	}
	if why != "" && openFor > 0 {
		// A held-back re-alert leaves the event's delivery as it was.
		log.Info("push: re-alert held back", "why", why)
		return
	}
	if why != "" {
		for _, dev := range devices {
			out := store.Delivery{EventID: ev.Id, DeviceID: dev.ID, Outcome: "skipped", Level: string(d.Level),
				Error: why, At: p.cfg.Now()}
			if err := p.cfg.Store.RecordDelivery(context.WithoutCancel(ctx), out); err != nil {
				log.Error("push: record delivery", "device", dev.ID, "err", err)
			}
		}
		log.Info("push: held back", "why", why)
		return
	}
	x := Extra{Icon: p.icon(ctx, ev), OpenFor: openFor}
	if n, err := p.cfg.Store.Unread(ctx, BadgeCursor); err != nil {
		log.Error("push: badge", "err", err)
	} else {
		x.Badge = &n
	}
	body, h, err := Build(ev, d, x)
	if err != nil {
		log.Error("push: payload", "err", err)
		return
	}
	for _, dev := range devices {
		out := p.deliver(ctx, dev, body, h)
		out.EventID, out.DeviceID, out.Level = ev.Id, dev.ID, string(d.Level)
		if d.Downgraded && out.Outcome == "sent" {
			out.Error = "critical sent as time-sensitive"
		}
		out.At = p.cfg.Now()
		if err := p.cfg.Store.RecordDelivery(context.WithoutCancel(ctx), out); err != nil {
			log.Error("push: record delivery", "device", dev.ID, "err", err)
		}
		if out.Outcome != "sent" {
			log.Warn("push: not delivered", "device", dev.ID, "error", out.Error, "attempts", out.Attempts)
		}
	}
}

// answer is one APNs response.
type answer struct {
	status     int
	reason     string
	apnsID     string
	retryAfter time.Duration
}

// retire reports whether the answer means the token is dead for good.
func (a answer) retire() bool {
	return a.status == http.StatusGone ||
		(a.status == http.StatusBadRequest && (a.reason == "BadDeviceToken" || a.reason == "DeviceTokenNotForTopic"))
}

func (a answer) transient() bool {
	return a.status == 0 || a.status == http.StatusTooManyRequests || a.status >= 500
}

func (a answer) staleToken() bool {
	return a.status == http.StatusForbidden && (a.reason == "ExpiredProviderToken" || a.reason == "InvalidProviderToken")
}

// deliver sends one push to one device, retrying as the answer allows.
func (p *Pusher) deliver(ctx context.Context, dev store.Device, body []byte, h Headers) store.Delivery {
	out := store.Delivery{Outcome: "failed"}
	var stale time.Time
	refreshed := false
	for attempt := 0; ; attempt++ {
		out.Attempts = attempt + 1
		a, err := p.send(ctx, dev, body, h, stale)
		if err != nil && a.status == 0 && ctx.Err() != nil {
			out.Error = "server shutting down"
			return out
		}
		switch {
		case err == nil && a.status == http.StatusOK:
			out.Outcome, out.APNsID, out.Error = "sent", a.apnsID, ""
			return out
		case a.staleToken() && !refreshed:
			// Sign a new provider token and try again at once.
			refreshed = true
			stale = p.cfg.Now()
			out.Error = a.reason
			continue
		case a.retire():
			out.Error = a.reason
			if out.Error == "" {
				out.Error = "Unregistered"
			}
			if err := p.cfg.Store.RetireToken(context.WithoutCancel(ctx), dev.ID, dev.Token, "apns: "+out.Error); err != nil {
				p.cfg.Logger.Error("push: retire device", "device", dev.ID, "err", err)
			}
			return out
		case a.transient() && attempt < len(p.cfg.Backoff):
			if err != nil {
				out.Error = err.Error()
			} else {
				out.Error = fmt.Sprintf("%d %s", a.status, a.reason)
			}
			wait := p.cfg.Backoff[attempt]
			if a.retryAfter > wait {
				wait = min(a.retryAfter, 5*time.Minute)
			}
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				out.Error = "server shutting down"
				return out
			case <-t.C:
			}
			continue
		default:
			switch {
			case err != nil:
				out.Error = err.Error()
			case a.reason != "":
				out.Error = a.reason
			default:
				out.Error = strconv.Itoa(a.status)
			}
			return out
		}
	}
}

func (p *Pusher) send(ctx context.Context, dev store.Device, body []byte, h Headers, stale time.Time) (answer, error) {
	base := p.cfg.ProductionURL
	if dev.Environment == "sandbox" {
		base = p.cfg.SandboxURL
	}
	jwt, _, err := p.sign.get(stale)
	if err != nil {
		return answer{status: -1}, fmt.Errorf("sign provider token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/3/device/"+dev.Token, bytes.NewReader(body))
	if err != nil {
		return answer{status: -1}, err
	}
	req.Header.Set("authorization", "bearer "+jwt)
	topic, pushType := dev.Topic, "alert"
	if h.Topic != "" {
		topic = h.Topic
	}
	if h.PushType != "" {
		pushType = h.PushType
	}
	req.Header.Set("apns-topic", topic)
	req.Header.Set("apns-push-type", pushType)
	req.Header.Set("apns-priority", strconv.Itoa(h.Priority))
	// Without apns-expiration, APNs keeps the push for a device that is
	// offline. 0 would mean "try once, keep nothing".
	if h.Expiration != 0 {
		req.Header.Set("apns-expiration", strconv.FormatInt(h.Expiration, 10))
	}
	if h.CollapseID != "" {
		req.Header.Set("apns-collapse-id", h.CollapseID)
	}
	req.Header.Set("content-type", "application/json")
	resp, err := p.cfg.HTTPClient.Do(req)
	if err != nil {
		return answer{}, err
	}
	defer resp.Body.Close()
	a := answer{status: resp.StatusCode, apnsID: resp.Header.Get("apns-id")}
	if v, err := strconv.Atoi(resp.Header.Get("retry-after")); err == nil && v > 0 {
		a.retryAfter = time.Duration(v) * time.Second
	}
	if resp.StatusCode != http.StatusOK {
		var r struct {
			Reason string `json:"reason"`
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if err := json.Unmarshal(raw, &r); err == nil {
			a.reason = r.Reason
		}
	}
	return a, nil
}

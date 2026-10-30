// Package live drives Live Activities for kind progress: one activity per
// progress key, shown on each iOS device as a chain of ActivityKit
// activities. It starts them with push-to-start, each on its own APNs
// broadcast channel when it can (else it waits for the app's update token),
// pushes updates (throttled),
// ends them, hands a long activity over to a new one before Apple's 8 hour
// limit, and notices activities that never started, were dismissed, or whose
// token died.
package live

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/heraldo/internal/ids"
	"github.com/noahkiss/heraldo/internal/push"
	"github.com/noahkiss/heraldo/internal/store"
)

// Defaults for Config.
const (
	// DefaultThrottle is the least time between two progress pushes to one
	// activity. A start, a catch-up and an end go out at once.
	DefaultThrottle = 15 * time.Second
	// DefaultChainAfter is the age at which an activity is ended and
	// started again. Apple ends one after 8 hours.
	DefaultChainAfter = 7*time.Hour + 30*time.Minute
	// DefaultStartTimeout is how long a push-to-start may wait for the app
	// to report the activity's update token.
	DefaultStartTimeout = 15 * time.Minute
	// DefaultAbandonAfter is the silence after which the server ends an
	// activity whose sender stopped posting.
	DefaultAbandonAfter = 2 * time.Hour
	// DefaultMaxStarts caps the push-to-starts per activity per device
	// that never produced an activity.
	DefaultMaxStarts = 3
	// DefaultTick is how often Run checks timers.
	DefaultTick = 5 * time.Second
	// defaultInterval stands in for the update interval before a second
	// update shows it.
	defaultInterval = 10 * time.Minute
	maxInterval     = time.Hour
	// minBackoff and maxBackoff bound the wait after a push-to-start or an
	// end push that failed for a passing reason (429, 5xx, no answer).
	minBackoff = 30 * time.Second
	maxBackoff = 10 * time.Minute
	// channelOffFor is how long the manager stops making channels for an
	// environment and topic after APNs refused one for good (the App ID
	// lacks the broadcast capability, or the 10,000 channel limit), and
	// channelRetryIn after any other failure. Starts go by token meanwhile.
	channelOffFor  = time.Hour
	channelRetryIn = 5 * time.Minute
	// channelKeep is the longest APNs stores a push on a channel.
	channelKeep = 8 * time.Hour
)

// reasonNoToken is the end reason of a token-started instance whose update
// token never came. It may well be on the device: the app could not reach
// the server.
const reasonNoToken = "no update token from the device"

// Sender sends one Live Activity push. *push.Pusher is one.
type Sender interface {
	SendLive(context.Context, push.LivePush) push.LiveResult
}

// Channels makes and deletes APNs broadcast channels. *push.Pusher is one.
type Channels interface {
	CreateChannel(ctx context.Context, environment, topic string, policy int) push.ChannelResult
	DeleteChannel(ctx context.Context, environment, topic, id string) push.ChannelResult
}

// Config configures a Manager.
type Config struct {
	Store  *store.Store
	Sender Sender
	// Channels, when set, starts each instance on its own broadcast channel
	// and sends its updates and end there, so the device need not reach the
	// server. Nil starts with input-push-token and waits for the app to
	// report the update token. A failed channel create falls back to that.
	Channels Channels
	Logger   *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Emit stores a server-made event (the history event at an activity's
	// end). The server passes its ingest.
	Emit func(ctx context.Context, fields map[string]any) error

	Throttle, ChainAfter, StartTimeout, AbandonAfter, Tick time.Duration
	MaxStarts                                              int
}

// Manager keeps the Live Activities of every device in step with the
// activities table. Its methods are safe to call from several goroutines;
// Run serialises the work from Enqueue.
type Manager struct {
	cfg  Config
	mu   sync.Mutex
	jobs chan func(context.Context)
	// lastPush is when each activity last pushed an update; pending is
	// when a throttled update is due.
	lastPush map[string]time.Time
	pending  map[string]time.Time
	// startBackoff holds back push-to-starts per activity and device after
	// one failed for a reason other than a dead token.
	startBackoff map[string]backoff
	// endRetry holds end pushes that failed for a passing reason, by
	// instance id. Tick sends them again.
	endRetry map[string]endRetry
	// channelOff holds back channel creates per environment and topic
	// until the time given; deleteBackoff holds back channel deletes by
	// instance id.
	channelOff    map[string]time.Time
	deleteBackoff map[string]backoff
}

type backoff struct {
	next  time.Time
	delay time.Duration
}

// grow returns the backoff after one more failure at now: 30 s, doubling
// to 10 minutes.
func (b backoff) grow(now time.Time) backoff {
	d := min(max(b.delay*2, minBackoff), maxBackoff)
	return backoff{next: now.Add(d), delay: d}
}

type endRetry struct {
	backoff
	dismissal time.Time
}

// New returns a Manager. Start it with Run.
func New(cfg Config) *Manager {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Throttle == 0 {
		cfg.Throttle = DefaultThrottle
	}
	if cfg.ChainAfter == 0 {
		cfg.ChainAfter = DefaultChainAfter
	}
	if cfg.StartTimeout == 0 {
		cfg.StartTimeout = DefaultStartTimeout
	}
	if cfg.AbandonAfter == 0 {
		cfg.AbandonAfter = DefaultAbandonAfter
	}
	if cfg.MaxStarts == 0 {
		cfg.MaxStarts = DefaultMaxStarts
	}
	if cfg.Tick == 0 {
		cfg.Tick = DefaultTick
	}
	return &Manager{
		cfg:      cfg,
		jobs:     make(chan func(context.Context), 256),
		lastPush: map[string]time.Time{},
		pending:  map[string]time.Time{},

		startBackoff:  map[string]backoff{},
		endRetry:      map[string]endRetry{},
		channelOff:    map[string]time.Time{},
		deleteBackoff: map[string]backoff{},
	}
}

// SetEmit sets Config.Emit. The server calls it with its ingest.
func (m *Manager) SetEmit(emit func(context.Context, map[string]any) error) {
	m.mu.Lock()
	m.cfg.Emit = emit
	m.mu.Unlock()
}

// Enqueue queues a job for Run. It never blocks: with the queue full it
// drops the job and logs it; the next tick or update catches up.
func (m *Manager) Enqueue(job func(context.Context)) {
	select {
	case m.jobs <- job:
	default:
		m.cfg.Logger.Warn("live: queue full; job dropped")
	}
}

// Stored queues the handling of a stored progress event.
func (m *Manager) Stored(eventID, key, action string) {
	m.Enqueue(func(ctx context.Context) { m.Handle(ctx, eventID, key, action) })
}

// Run works the queue and the timers until ctx ends. At start it pushes the
// current content of every active activity, since a restart may have
// dropped updates.
func (m *Manager) Run(ctx context.Context) {
	m.Resync(ctx)
	t := time.NewTicker(m.cfg.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-m.jobs:
			job(ctx)
		case <-t.C:
			m.Tick(ctx)
		}
	}
}

// Resync brings every active activity up to date on every device: it starts
// what is missing, silently, and pushes the current content. An activity
// silent for AbandonAfter is abandoned instead, so a new push-to-start token
// or a restart after downtime does not put up stale activities.
func (m *Manager) Resync(ctx context.Context) {
	acts, err := m.cfg.Store.Activities(ctx, true)
	if err != nil {
		m.cfg.Logger.Error("live: activities", "err", err)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.cfg.Now()
	for _, a := range acts {
		if now.Sub(a.UpdatedAt) >= m.cfg.AbandonAfter {
			m.abandon(ctx, a)
			continue
		}
		m.sync(ctx, a, false, true, false)
	}
}

// Handle brings the activity on key in step after the event eventID with
// action was stored on it.
func (m *Manager) Handle(ctx context.Context, eventID, key, action string) {
	a, err := m.cfg.Store.GetActivity(ctx, key)
	if err != nil {
		m.cfg.Logger.Error("live: activity", "key", key, "err", err)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.endOthers(ctx, a)
	if !a.Active() {
		if a.LastEvent == eventID && action == "end" {
			m.finish(ctx, a)
		}
		return
	}
	m.sync(ctx, a, action == "start", false, true)
}

// Tick runs the timers: throttled updates that are due, push-to-starts that
// timed out, chain hand-offs, and abandoned activities.
func (m *Manager) Tick(ctx context.Context) {
	now := m.cfg.Now()
	acts, err := m.cfg.Store.Activities(ctx, true)
	if err != nil {
		m.cfg.Logger.Error("live: activities", "err", err)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retryEnds(ctx)
	defer m.reapChannels(ctx)
	for _, a := range acts {
		if now.Sub(a.UpdatedAt) >= m.cfg.AbandonAfter {
			m.abandon(ctx, a)
			continue
		}
		insts, err := m.cfg.Store.Instances(ctx, a.ID)
		if err != nil {
			m.cfg.Logger.Error("live: instances", "err", err)
			continue
		}
		for _, in := range insts {
			if in.State == store.InstanceStarting && now.Sub(in.RequestedAt) >= m.cfg.StartTimeout {
				in.State, in.EndReason, in.EndedAt = store.InstanceNeverStarted, reasonNoToken, now
				m.save(ctx, in)
			}
		}
		if due, ok := m.pending[a.ID]; ok && !now.Before(due) {
			delete(m.pending, a.ID)
			m.sync(ctx, a, false, true, false)
			continue
		}
		m.chain(ctx, a, insts)
	}
}

// Confirm records the update token the app reported for instance id, and
// pushes the current content to it.
func (m *Manager) Confirm(ctx context.Context, id, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	in, err := m.cfg.Store.GetInstance(ctx, id)
	if err != nil {
		return err
	}
	late := in.State == store.InstanceNeverStarted
	// The server ended it while it was starting: an end, an abandon, a
	// restart. The activity is on the device all the same; end it there.
	ghost := in.State == store.InstanceEnded && in.UpdateToken == ""
	if !in.Open() && !late && !ghost {
		return nil
	}
	a, err := m.cfg.Store.GetActivity(ctx, in.Key)
	if err != nil {
		return err
	}
	dev, err := m.cfg.Store.GetDevice(ctx, in.DeviceID)
	if err != nil {
		return err
	}
	if ghost {
		in.UpdateToken = token
		if in.ConfirmedAt.IsZero() {
			in.ConfirmedAt = m.cfg.Now()
		}
		m.sendEnd(ctx, a, dev, &in, m.cfg.Now())
		m.save(ctx, in)
		return nil
	}
	if late {
		// The activity came up after the server gave up on it. Keep it when
		// it is still the newest link on the device, else end it: a retry
		// already put up another.
		insts, err := m.cfg.Store.Instances(ctx, in.ActivityID)
		if err != nil {
			return err
		}
		if l := lastOf(byDevice(insts, in.DeviceID)); l == nil || l.ID != in.ID || a.ID != in.ActivityID || !a.Active() {
			in.State, in.UpdateToken = store.InstanceActive, token
			m.end(ctx, a, dev, in, m.cfg.Now(), store.InstanceEnded, "duplicate")
			return nil
		}
		in.EndReason, in.EndedAt = "", time.Time{}
	}
	if in.OnChannel() && in.State == store.InstanceActive {
		// Its updates go by channel and its content went with the start;
		// keep the token as the fallback should the channel go.
		if in.UpdateToken != token {
			in.UpdateToken = token
			if in.ConfirmedAt.IsZero() {
				in.ConfirmedAt = m.cfg.Now()
			}
			m.save(ctx, in)
		}
		return nil
	}
	known := in.UpdateToken == token && in.State == store.InstanceActive
	in.UpdateToken = token
	in.State = store.InstanceActive
	if in.ConfirmedAt.IsZero() {
		in.ConfirmedAt = m.cfg.Now()
	}
	m.save(ctx, in)
	if known {
		return nil
	}
	if a.ID != in.ActivityID || !a.Active() {
		// It belongs to an activity that is over: end it.
		m.end(ctx, a, dev, in, m.cfg.Now(), store.InstanceEnded, "activity over")
		return nil
	}
	m.update(ctx, a, dev, &in, 10)
	return nil
}

// Report records what the app saw happen to instance id: dismissed (the
// person removed it) or ended (the system ended it). An instance the server
// already closed keeps its state.
func (m *Manager) Report(ctx context.Context, id, state string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	in, err := m.cfg.Store.GetInstance(ctx, id)
	if err != nil {
		return err
	}
	if !in.Open() {
		return nil
	}
	switch state {
	case store.InstanceDismissed:
		in.State, in.EndReason = store.InstanceDismissed, "dismissed on the device"
	case store.InstanceEnded:
		in.State, in.EndReason = store.InstanceEnded, "ended on the device"
	default:
		return fmt.Errorf("unknown state %q", state)
	}
	in.EndedAt = m.cfg.Now()
	m.save(ctx, in)
	return nil
}

// ---- work, with m.mu held ---------------------------------------------

// sync starts the activity on each device that lacks it and pushes the
// content to each instance that is open. A start goes at priority 10, never
// throttled. Every other update, a new status or tint included, goes at
// priority 5, at most once per Throttle unless now is set. loud lets the
// first start on a device sound; Resync and timers start silently.
func (m *Manager) sync(ctx context.Context, a store.Activity, start, now, loud bool) {
	prio := 5
	if start {
		prio = 10
	}
	if prio == 5 && !now {
		if last, ok := m.lastPush[a.ID]; ok && m.cfg.Now().Sub(last) < m.cfg.Throttle {
			m.pending[a.ID] = last.Add(m.cfg.Throttle)
			return
		}
	}
	delete(m.pending, a.ID)
	devices, err := m.cfg.Store.Devices(ctx, true)
	if err != nil {
		m.cfg.Logger.Error("live: devices", "err", err)
		return
	}
	insts, err := m.cfg.Store.Instances(ctx, a.ID)
	if err != nil {
		m.cfg.Logger.Error("live: instances", "err", err)
		return
	}
	pushed := false
	for _, dev := range devices {
		mine := byDevice(insts, dev.ID)
		latest := lastOf(mine)
		switch {
		case latest == nil:
			pushed = m.start(ctx, a, dev, 0, loud) || pushed
		case latest.State == store.InstanceActive:
			pushed = m.update(ctx, a, dev, latest, prio) || pushed
		case latest.State == store.InstanceStarting:
			// No update token yet; Confirm pushes the content when it comes.
		case latest.State == store.InstanceNeverStarted:
			// Start again only after refused push-to-starts. One that timed
			// out waiting for its token may be on the screen: a second start
			// would stack a copy. A late token report revives it (Confirm).
			if !anyReason(mine, reasonNoToken) && count(mine, store.InstanceNeverStarted) < m.cfg.MaxStarts {
				pushed = m.start(ctx, a, dev, latest.Chain, false) || pushed
			}
		case latest.State == store.InstanceEnded && latest.EndReason == "ended on the device":
			// The system ended it before the hand-off: start the next link.
			pushed = m.start(ctx, a, dev, latest.Chain+1, false) || pushed
		default:
			// Dismissed, or its token died: never respawn.
		}
	}
	if pushed {
		m.lastPush[a.ID] = m.cfg.Now()
	}
}

// chain hands each instance old enough over to a new one: it starts the
// next link with the current content, then ends the old one at once.
func (m *Manager) chain(ctx context.Context, a store.Activity, insts []store.Instance) {
	now := m.cfg.Now()
	for _, in := range insts {
		if in.State != store.InstanceActive || now.Sub(in.RequestedAt) < m.cfg.ChainAfter {
			continue
		}
		dev, err := m.cfg.Store.GetDevice(ctx, in.DeviceID)
		if err != nil || !dev.Active() {
			continue
		}
		if dev.LiveActivityToken == "" {
			// Without a push-to-start token there is no next link; Apple
			// ends this one at 8 hours.
			continue
		}
		// End the old link only once the new one is on its way, so a failed
		// start leaves the old one up and the next tick tries again.
		if m.start(ctx, a, dev, in.Chain+1, false) {
			m.end(ctx, a, dev, in, now, store.InstanceEnded, "chained")
		}
	}
}

// start sends a push-to-start for link chain of a on dev. first is the
// activity's first start on the device: only it may sound, at time-sensitive
// or above. A chain link and
// a retry come up silently. It reports whether APNs took it.
func (m *Manager) start(ctx context.Context, a store.Activity, dev store.Device, chain int, first bool) bool {
	if dev.LiveActivityToken == "" {
		return false
	}
	now := m.cfg.Now()
	bk := a.ID + "/" + dev.ID
	if b, ok := m.startBackoff[bk]; ok && now.Before(b.next) {
		return false
	}
	content := contentMap(a.Content)
	in := store.Instance{
		ID: "lai_" + ids.New(now), ActivityID: a.ID, Key: a.Key, DeviceID: dev.ID, Chain: chain,
		State: store.InstanceStarting, RequestedAt: now,
	}
	title, _ := content["title"].(string)
	if title == "" {
		title = a.Key
	}
	alert := &liveAlert{Title: title}
	if b, _ := content["body"].(string); b != "" {
		alert.Body = b
	} else if s, _ := content["status"].(string); s != "" {
		alert.Body = s
	}
	// Only the first start sounds, and only at time-sensitive or above: a
	// job started at night shows on the Lock Screen without waking anyone.
	// A device in a Focus filter gets a silent start: the activity shows,
	// and never buzzes. The title and body still come, because
	// push-to-start needs them.
	if first && (a.Level == "time-sensitive" || a.Level == "critical") && dev.FocusFilter == nil {
		alert.Sound = "default"
	}
	in.Channel = m.newChannel(ctx, dev)
	body, err := payload{
		event: "start", now: now, content: content, stale: m.staleDate(a, content),
		attrs: &Attributes{Key: a.Key, Instance: in.ID, Source: a.Source}, alert: alert, channel: in.Channel,
	}.marshal()
	if err != nil {
		m.cfg.Logger.Error("live: payload", "err", err)
		m.dropChannel(ctx, dev, in.Channel)
		return false
	}
	r := m.cfg.Sender.SendLive(ctx, push.LivePush{
		Environment: dev.Environment, Token: dev.LiveActivityToken, Topic: dev.Topic, Body: body,
		Priority: 10, Expiration: now.Add(m.cfg.StartTimeout).Unix(),
	})
	in.LastPushAt = now
	if r.Sent() || r.Gone() {
		delete(m.startBackoff, bk)
	}
	if !r.Sent() && in.Channel != "" {
		// No activity will listen on it.
		m.dropChannel(ctx, dev, in.Channel)
		in.Channel = ""
	}
	switch {
	case r.Sent():
		if in.Channel != "" {
			// It takes updates on its channel at once: no token to wait for.
			in.State = store.InstanceActive
		}
	case r.Gone():
		// The push-to-start token is dead, such as after a reinstall. The
		// app reports a new one when it next runs, and a later update
		// tries again, within MaxStarts.
		in.State, in.EndReason, in.EndedAt = store.InstanceNeverStarted, "push-to-start token refused: "+r.Error(), now
		if err := m.cfg.Store.SetLiveActivityToken(ctx, dev.ID, ""); err != nil {
			m.cfg.Logger.Error("live: clear push-to-start token", "device", dev.ID, "err", err)
		}
	default:
		m.startBackoff[bk] = m.startBackoff[bk].grow(now)
		m.cfg.Logger.Warn("live: push-to-start not sent", "device", dev.ID, "key", a.Key, "error", r.Error(),
			"retry_in", m.startBackoff[bk].delay)
		return false
	}
	m.save(ctx, in)
	return r.Sent()
}

// update pushes the content of a to the instance in. It reports whether
// APNs took it.
func (m *Manager) update(ctx context.Context, a store.Activity, dev store.Device, in *store.Instance, prio int) bool {
	now := m.cfg.Now()
	content := contentMap(a.Content)
	stamp := nextStamp(now, in.LastPushAt)
	body, err := payload{event: "update", now: now, stamp: stamp, content: content, stale: m.staleDate(a, content)}.marshal()
	if err != nil {
		m.cfg.Logger.Error("live: payload", "err", err)
		return false
	}
	lp := m.target(dev, *in)
	lp.Body, lp.Priority, lp.Expiration = body, prio, m.staleDate(a, content).Unix()
	if lp.Channel != "" {
		lp.Expiration = min(lp.Expiration, now.Add(channelKeep).Unix())
	}
	r := m.cfg.Sender.SendLive(ctx, lp)
	in.LastPushAt = time.Unix(stamp, 0)
	if lp.Channel != "" && m.channelLost(in, r) {
		if in.UpdateToken == "" {
			in.State, in.EndReason, in.EndedAt = store.InstanceDead, "channel refused: "+r.Error(), now
		} else if _, ok := m.pending[a.ID]; !ok {
			// The token takes over from the next push.
			m.pending[a.ID] = now
		}
	} else if r.Gone() {
		in.State, in.EndReason, in.EndedAt = store.InstanceDead, "update token refused: "+r.Error(), now
	} else if !r.Sent() {
		// The next update carries the same content; send it after the
		// throttle when none comes sooner.
		if _, ok := m.pending[a.ID]; !ok {
			m.pending[a.ID] = now.Add(m.cfg.Throttle)
		}
		m.cfg.Logger.Warn("live: update not sent", "device", dev.ID, "key", a.Key, "error", r.Error())
	}
	m.save(ctx, *in)
	return r.Sent()
}

// end sends the end push to in, with the activity's last content, and
// records state and reason. dismissal is when the ended activity leaves the
// screen; zero leaves it to the system.
func (m *Manager) end(ctx context.Context, a store.Activity, dev store.Device, in store.Instance, dismissal time.Time, state, reason string) {
	now := m.cfg.Now()
	if in.State == store.InstanceActive && (in.OnChannel() || in.UpdateToken != "") {
		m.sendEnd(ctx, a, dev, &in, dismissal)
	}
	in.State, in.EndReason, in.EndedAt = state, reason, now
	m.save(ctx, in)
}

// sendEnd sends the end push to in's channel, else its update token. Its
// timestamp is later than any push before it, so the device does not drop
// it as old. A passing failure queues it for retryEnds. It does not save in.
func (m *Manager) sendEnd(ctx context.Context, a store.Activity, dev store.Device, in *store.Instance, dismissal time.Time) bool {
	now := m.cfg.Now()
	stamp := nextStamp(now, in.LastPushAt)
	body, err := payload{event: "end", now: now, stamp: stamp, content: contentMap(a.Content), dismissal: dismissal}.marshal()
	if err != nil {
		m.cfg.Logger.Error("live: payload", "err", err)
		return false
	}
	lp := m.target(dev, *in)
	lp.Body, lp.Priority = body, 10
	if lp.Channel != "" {
		// A channel needs apns-expiration; keep the end for a device that
		// is offline as long as APNs keeps anything.
		lp.Expiration = now.Add(channelKeep).Unix()
	}
	r := m.cfg.Sender.SendLive(ctx, lp)
	in.LastPushAt = time.Unix(stamp, 0)
	if lp.Channel != "" && m.channelLost(in, r) {
		if in.UpdateToken != "" {
			// Try the token at once.
			return m.sendEnd(ctx, a, dev, in, dismissal)
		}
		delete(m.endRetry, in.ID)
		return false
	}
	if r.Sent() || r.Gone() {
		delete(m.endRetry, in.ID)
		return r.Sent()
	}
	er := m.endRetry[in.ID]
	if er.delay >= maxBackoff {
		delete(m.endRetry, in.ID)
		m.cfg.Logger.Warn("live: end not sent; giving up", "device", dev.ID, "key", a.Key, "error", r.Error())
		return false
	}
	m.endRetry[in.ID] = endRetry{backoff: er.backoff.grow(now), dismissal: dismissal}
	m.cfg.Logger.Warn("live: end not sent", "device", dev.ID, "key", a.Key, "error", r.Error())
	return false
}

// retryEnds sends again the end pushes that are due.
func (m *Manager) retryEnds(ctx context.Context) {
	now := m.cfg.Now()
	for id, er := range m.endRetry {
		if now.Before(er.next) {
			continue
		}
		in, err := m.cfg.Store.GetInstance(ctx, id)
		if err != nil || (!in.OnChannel() && in.UpdateToken == "") {
			delete(m.endRetry, id)
			continue
		}
		dev, err := m.cfg.Store.GetDevice(ctx, in.DeviceID)
		if err != nil {
			delete(m.endRetry, id)
			continue
		}
		a, err := m.cfg.Store.GetActivity(ctx, in.Key)
		if err != nil {
			delete(m.endRetry, id)
			continue
		}
		m.sendEnd(ctx, a, dev, &in, er.dismissal)
		m.save(ctx, in)
	}
}

// abandon ends a, silent for AbandonAfter, and its instances.
func (m *Manager) abandon(ctx context.Context, a store.Activity) {
	ok, err := m.cfg.Store.EndActivity(ctx, a.Key, a.ID, store.EndAbandoned)
	if err != nil {
		m.cfg.Logger.Error("live: end abandoned", "key", a.Key, "err", err)
		return
	}
	if ok {
		a.State, a.EndedAt, a.EndReason = "ended", m.cfg.Now(), store.EndAbandoned
		m.finish(ctx, a)
	}
}

// nextStamp is the timestamp for a push at now: now in Unix seconds, or one
// second past the last push to the same instance, whichever is later.
func nextStamp(now, last time.Time) int64 {
	s := now.Unix()
	if !last.IsZero() && s <= last.Unix() {
		s = last.Unix() + 1
	}
	return s
}

// endOthers ends the open instances on a's key that belong to an earlier
// activity: a start replaced it.
func (m *Manager) endOthers(ctx context.Context, a store.Activity) {
	open, err := m.cfg.Store.OpenInstances(ctx)
	if err != nil {
		m.cfg.Logger.Error("live: open instances", "err", err)
		return
	}
	for _, in := range open {
		if in.Key != a.Key || in.ActivityID == a.ID {
			continue
		}
		dev, err := m.cfg.Store.GetDevice(ctx, in.DeviceID)
		if err != nil {
			continue
		}
		m.end(ctx, a, dev, in, m.cfg.Now(), store.InstanceEnded, store.EndRestarted)
	}
}

// finish ends every open instance of the ended activity a and writes its
// history event.
func (m *Manager) finish(ctx context.Context, a store.Activity) {
	insts, err := m.cfg.Store.Instances(ctx, a.ID)
	if err != nil {
		m.cfg.Logger.Error("live: instances", "err", err)
	}
	content := contentMap(a.Content)
	var dismissal time.Time
	if k, ok := content["keep_for"].(float64); ok {
		dismissal = m.cfg.Now().Add(time.Duration(k) * time.Second)
	}
	for _, in := range insts {
		if !in.Open() {
			continue
		}
		dev, err := m.cfg.Store.GetDevice(ctx, in.DeviceID)
		if err != nil {
			continue
		}
		m.end(ctx, a, dev, in, dismissal, store.InstanceEnded, a.EndReason)
	}
	delete(m.lastPush, a.ID)
	delete(m.pending, a.ID)
	for k := range m.startBackoff {
		if strings.HasPrefix(k, a.ID+"/") {
			delete(m.startBackoff, k)
		}
	}
	if m.cfg.Emit == nil {
		return
	}
	if err := m.cfg.Emit(ctx, historyEvent(a, insts)); err != nil {
		m.cfg.Logger.Error("live: history event", "key", a.Key, "err", err)
	}
}

// staleDate is when the content counts as out of date: stale_after from
// now, else twice the sender's update interval.
func (m *Manager) staleDate(a store.Activity, content map[string]any) time.Time {
	now := m.cfg.Now()
	if s, ok := content["stale_after"].(float64); ok && s > 0 {
		return now.Add(time.Duration(s) * time.Second)
	}
	iv := a.Interval
	if iv <= 0 {
		iv = defaultInterval
	}
	iv = min(max(iv, m.cfg.Throttle), maxInterval)
	return now.Add(2 * iv)
}

func (m *Manager) save(ctx context.Context, in store.Instance) {
	if err := m.cfg.Store.SaveInstance(context.WithoutCancel(ctx), in); err != nil {
		m.cfg.Logger.Error("live: save instance", "instance", in.ID, "err", err)
	}
}

// ---- channels, with m.mu held -------------------------------------------

// target is where a push to in goes: its channel while it has one, else
// its update token.
func (m *Manager) target(dev store.Device, in store.Instance) push.LivePush {
	lp := push.LivePush{Environment: dev.Environment, Topic: dev.Topic}
	if in.OnChannel() {
		lp.Channel = in.Channel
	} else {
		lp.Token = in.UpdateToken
	}
	return lp
}

// newChannel makes a channel for one new instance on dev. It returns "" when
// channels are off, held back, or the create failed: the start then asks
// for an update token instead.
func (m *Manager) newChannel(ctx context.Context, dev store.Device) string {
	if m.cfg.Channels == nil {
		return ""
	}
	now := m.cfg.Now()
	k := dev.Environment + "/" + dev.Topic
	if now.Before(m.channelOff[k]) {
		return ""
	}
	// Store the latest push: a phone offline for a while gets the newest
	// content, and the end, when it is back.
	r := m.cfg.Channels.CreateChannel(ctx, dev.Environment, dev.Topic, push.ChannelStoreLatest)
	if r.ChannelCreated() {
		delete(m.channelOff, k)
		return r.ID
	}
	wait := channelRetryIn
	if r.FeatureOff() || r.Reason == "CannotCreateChannelConfig" {
		wait = channelOffFor
	}
	m.channelOff[k] = now.Add(wait)
	m.cfg.Logger.Warn("live: channel not created; starting with an update token", "environment", dev.Environment,
		"error", r.Error(), "retry_in", wait)
	return ""
}

// dropChannel deletes a channel no instance kept, best effort.
func (m *Manager) dropChannel(ctx context.Context, dev store.Device, id string) {
	if id == "" || m.cfg.Channels == nil {
		return
	}
	if r := m.cfg.Channels.DeleteChannel(ctx, dev.Environment, dev.Topic, id); !r.ChannelDeleted() {
		m.cfg.Logger.Warn("live: unused channel not deleted", "environment", dev.Environment, "error", r.Error())
	}
}

// channelLost reports whether r says in's channel is unusable for good: APNs
// does not know it, or the App ID lost the broadcast capability, which
// deletes every channel. It then marks the channel gone, so pushes fall
// back to the update token.
func (m *Manager) channelLost(in *store.Instance, r push.LiveResult) bool {
	if !r.ChannelGone() && !r.FeatureOff() {
		return false
	}
	in.ChannelDeletedAt = m.cfg.Now()
	m.cfg.Logger.Warn("live: channel refused", "instance", in.ID, "error", r.Error(), "fallback_token", in.UpdateToken != "")
	return true
}

// reapChannels deletes the channels of closed instances whose end is not
// waiting for a retry. APNs still delivers an end it stored after the
// delete.
func (m *Manager) reapChannels(ctx context.Context) {
	if m.cfg.Channels == nil {
		return
	}
	insts, err := m.cfg.Store.StaleChannels(ctx)
	if err != nil {
		m.cfg.Logger.Error("live: stale channels", "err", err)
		return
	}
	now := m.cfg.Now()
	for _, in := range insts {
		if _, ok := m.endRetry[in.ID]; ok {
			continue
		}
		if b, ok := m.deleteBackoff[in.ID]; ok && now.Before(b.next) {
			continue
		}
		dev, err := m.cfg.Store.GetDevice(ctx, in.DeviceID)
		if err != nil {
			m.deleteBackoff[in.ID] = m.deleteBackoff[in.ID].grow(now)
			continue
		}
		r := m.cfg.Channels.DeleteChannel(ctx, dev.Environment, dev.Topic, in.Channel)
		if !r.ChannelDeleted() {
			m.deleteBackoff[in.ID] = m.deleteBackoff[in.ID].grow(now)
			m.cfg.Logger.Warn("live: channel not deleted", "instance", in.ID, "error", r.Error(),
				"retry_in", m.deleteBackoff[in.ID].delay)
			continue
		}
		delete(m.deleteBackoff, in.ID)
		in.ChannelDeletedAt = now
		m.save(ctx, in)
	}
}

// ---- helpers ------------------------------------------------------------

func contentMap(raw json.RawMessage) map[string]any {
	out := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

func byDevice(insts []store.Instance, device string) []store.Instance {
	var out []store.Instance
	for _, in := range insts {
		if in.DeviceID == device {
			out = append(out, in)
		}
	}
	return out
}

// lastOf returns the newest instance: the highest chain, then the latest
// request.
func lastOf(insts []store.Instance) *store.Instance {
	var best *store.Instance
	for i := range insts {
		in := &insts[i]
		if best == nil || in.Chain > best.Chain || (in.Chain == best.Chain && !in.RequestedAt.Before(best.RequestedAt)) {
			best = in
		}
	}
	return best
}

func anyReason(insts []store.Instance, reason string) bool {
	for _, in := range insts {
		if in.EndReason == reason {
			return true
		}
	}
	return false
}

func count(insts []store.Instance, state string) int {
	n := 0
	for _, in := range insts {
		if in.State == state {
			n++
		}
	}
	return n
}

// historyEvent is the one inbox event an activity leaves: a passive message
// on the activity's key, with the last title and status and how long it ran.
func historyEvent(a store.Activity, insts []store.Instance) map[string]any {
	content := contentMap(a.Content)
	title, _ := content["title"].(string)
	if title == "" {
		title = a.Key
	}
	ended := a.EndedAt
	if ended.IsZero() {
		ended = a.UpdatedAt
	}
	ran := ended.Sub(a.StartedAt).Round(time.Minute)
	var parts []string
	if s, _ := content["status"].(string); s != "" {
		parts = append(parts, s)
	} else if a.EndReason == store.EndAbandoned {
		parts = append(parts, "No update for a while; ended")
	} else {
		parts = append(parts, "Ended")
	}
	if ran > 0 {
		parts = append(parts, "ran "+humanDuration(ran))
	}
	summary := strings.Join(parts, " · ")
	chains := 0
	for _, in := range insts {
		chains = max(chains, in.Chain+1)
	}
	src := a.Source
	if src == "" {
		src = "heraldo"
	}
	return map[string]any{
		"source": src, "kind": "message", "key": a.Key, "level": "passive",
		"title":   cut(title, 256),
		"summary": cut(summary, 178),
		"details": map[string]any{"heraldo": map[string]any{"activity": map[string]any{
			"id": a.ID, "started_at": a.StartedAt.UTC().Format(time.RFC3339), "ended_at": ended.UTC().Format(time.RFC3339),
			"end_reason": a.EndReason, "links": chains, "content": content,
		}}},
	}
}

func humanDuration(d time.Duration) string {
	h, mnt := int(d.Hours()), int(d.Minutes())%60
	switch {
	case h == 0:
		return fmt.Sprintf("%dm", mnt)
	case mnt == 0:
		return fmt.Sprintf("%dh", h)
	default:
		return fmt.Sprintf("%dh %dm", h, mnt)
	}
}

func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

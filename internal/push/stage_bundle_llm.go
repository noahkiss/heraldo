package push

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/decide"
	"github.com/noahkiss/heraldo/internal/store"
)

// The bundle text hook: the LLM's title and summary for a bundle
// notification. It never delays a push. The bundle stage's notification
// goes out first, as it always has; once it is delivered, the hook runs in
// the background with the window's events. An answer that arrives while
// the window is still open, and while no newer push replaced the bundle,
// goes out at level passive with the same apns-collapse-id, so it replaces
// the notification quietly. Any other answer is only recorded. One call
// per view is in flight; a push meanwhile asks again when it returns.

// BundleText is the bundle text hook: decide.Decider.Bundle.
type BundleText = decide.BundleText

// hooks holds the bundle text hook and Run's context.
type hooks struct {
	mu   sync.Mutex
	text BundleText
	ctx  context.Context
}

func (h *hooks) setCtx(ctx context.Context) {
	h.mu.Lock()
	h.ctx = ctx
	h.mu.Unlock()
}

func (h *hooks) get() (BundleText, context.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ctx := h.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return h.text, ctx
}

// SetBundleText sets the bundle text hook; nil removes it.
func (p *Pusher) SetBundleText(f BundleText) {
	p.hooks.mu.Lock()
	p.hooks.text = f
	p.hooks.mu.Unlock()
}

// bundlePush is the last bundle notification a window delivered.
type bundlePush struct {
	n        int
	eventID  string
	event    api.Event
	decision Decision
	devices  []store.Device
	collapse string
}

// bundleLLM is one window's hook state.
type bundleLLM struct {
	last bundlePush
	// asking is set while a call is in flight; dirty when a newer push
	// was delivered meanwhile.
	asking bool
	dirty  bool
}

// bundleSnap is what one call asks about.
type bundleSnap struct {
	push    bundlePush
	members []decide.Member
	ends    time.Time
}

func (w *bundleWindow) snap() bundleSnap {
	return bundleSnap{push: w.llm.last, members: append([]decide.Member{}, w.members...), ends: w.ends}
}

// afterBundle asks the hook, after delivery, about the bundle push r is.
func (p *Pusher) afterBundle(r *Run, view, name string, opened time.Time, n int) {
	if f, _ := p.hooks.get(); f == nil {
		return
	}
	r.after = append(r.after, func() {
		last := bundlePush{n: n, eventID: r.Event.Id, event: *r.Event, decision: r.Decision,
			devices: append([]store.Device{}, r.Devices...), collapse: r.CollapseID}
		b := &p.bundles
		b.mu.Lock()
		w := b.windows[view]
		if w == nil || !w.opened.Equal(opened) {
			b.mu.Unlock()
			return
		}
		w.llm.last = last
		if w.llm.asking {
			w.llm.dirty = true
			b.mu.Unlock()
			return
		}
		w.llm.asking = true
		s := w.snap()
		b.mu.Unlock()
		p.pending.Add(1)
		go func() {
			defer p.pending.Done()
			p.askBundle(view, name, opened, s)
		}()
	})
}

// askBundle calls the hook until no newer push is waiting for an answer.
func (p *Pusher) askBundle(view, name string, opened time.Time, s bundleSnap) {
	f, base := p.hooks.get()
	log := p.cfg.Logger.With("view", view)
	for {
		left := s.ends.Sub(p.cfg.Now())
		if left <= 0 || f == nil {
			p.endAsk(view, opened)
			return
		}
		ctx, cancel := context.WithTimeout(base, left)
		ans := f(ctx, decide.BundleAsk{View: view, ViewName: name, Count: s.push.n, Members: s.members})
		cancel()

		b := &p.bundles
		b.mu.Lock()
		w := b.windows[view]
		now := p.cfg.Now()
		same := w != nil && w.opened.Equal(opened)
		open := same && now.Before(w.ends)
		current := open && w.llm.last.n == s.push.n
		again := false
		var next bundleSnap
		if same && w.llm.dirty && open {
			w.llm.dirty, again, next = false, true, w.snap()
		} else if same {
			w.llm.asking, w.llm.dirty = false, false
		}
		b.mu.Unlock()

		t := ans.Trace
		if t.Reason != "" {
			det := map[string]any{}
			for k, v := range t.Detail {
				det[k] = v
			}
			switch {
			case !ans.Apply:
			case !open:
				t.Reason += "; not sent: the window closed first"
				det["applied"] = false
			case !current:
				t.Reason += "; not sent: a newer push replaced the bundle first"
				det["applied"] = false
			default:
				sent := p.sendBundleText(base, s.push, name, ans)
				det["sent"] = sent
				det["collapse_id"] = s.push.collapse
				if sent > 0 {
					t.Reason += "; replaced the bundle through its collapse id"
				} else {
					t.Reason += "; the replace reached no device"
				}
			}
			t.Detail = det
			if err := p.cfg.Store.AppendDecision(context.WithoutCancel(base), s.push.eventID, t.Row(now)); err != nil {
				log.Error("push: record the bundle text", "err", err)
			}
		}
		if !again {
			return
		}
		s = next
	}
}

func (p *Pusher) endAsk(view string, opened time.Time) {
	b := &p.bundles
	b.mu.Lock()
	if w := b.windows[view]; w != nil && w.opened.Equal(opened) {
		w.llm.asking, w.llm.dirty = false, false
	}
	b.mu.Unlock()
}

// sendBundleText replaces the bundle notification with the hook's text, at
// level passive, and returns how many devices took it. It records no
// delivery: the event's delivery stays the bundle push's.
func (p *Pusher) sendBundleText(ctx context.Context, last bundlePush, name string, ans decide.BundleAnswer) int {
	ev := last.event
	title := ans.Title
	summary := fmt.Sprintf("%d new in %s: %s", last.n, name, ans.Summary)
	ev.Title, ev.Summary = &title, &summary
	d := last.decision
	d.Level = api.LevelPassive
	x := Extra{Icon: p.icon(ctx, &ev)}
	body, h, err := Build(&ev, d, x)
	if err != nil {
		p.cfg.Logger.Error("push: bundle text payload", "err", err)
		return 0
	}
	x.ContentAvailable = true
	mac, _, err := Build(&ev, d, x)
	if err != nil {
		p.cfg.Logger.Error("push: bundle text payload", "err", err)
		return 0
	}
	h.CollapseID = last.collapse
	sent := 0
	for _, dev := range last.devices {
		b := body
		if ContentAvailableFor(dev) {
			b = mac
		}
		if out := p.deliver(ctx, dev, b, h); out.Outcome == "sent" {
			sent++
		}
	}
	return sent
}

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

// bundleStage folds a burst of a saved view's pushes into one notification.
// The first saved view, by position, whose filter matches the event and that
// has a bundle decides. In the view's window, which opens at the first push
// it matches, the first max_visible pushes go out as usual. Each later one
// goes out as one collapsed notification, the same apns-collapse-id per
// view, "N new in <view>: <latest title>"; the first of them alerts, and the
// rest replace it quietly, at level passive.
//
// Only pushes at level active bundle: time-sensitive and critical always
// push on their own, and a passive replace keeps its own collapse id. A
// re-alert never bundles. The window lives in memory: after a restart the
// next push opens a new one.
//
// With a bundle text hook (SetBundleText, the LLM's decisions role), the
// bundle notification still goes out first, unchanged; the hook then runs
// after delivery, and an answer that arrives while the window is open
// replaces the notification quietly through the same collapse id
// (stage_bundle_llm.go). Nothing waits for it.
func (p *Pusher) bundleStage(ctx context.Context, r *Run) Verdict {
	if r.OpenFor > 0 {
		return Pass("re-alert: not bundled")
	}
	if r.Decision.Level != api.LevelActive {
		return Pass("level " + string(r.Decision.Level) + ": never bundled")
	}
	views, err := p.cfg.Store.Views(ctx)
	if err != nil {
		r.Log.Error("push: views", "err", err)
		return Pass("view lookup failed; pushed alone")
	}
	var view *store.View
	for i := range views {
		v := &views[i]
		if v.Bundle == nil {
			continue
		}
		ok, err := p.cfg.Store.Matches(ctx, v.Filter.Filter(p.cfg.Rules().Expand), r.Rec.Seq)
		if err != nil {
			r.Log.Error("push: view match", "view", v.ID, "err", err)
			continue
		}
		if ok {
			view = v
			break
		}
	}
	if view == nil {
		return Pass("no bundling view")
	}
	b := *view.Bundle
	visible := max(b.MaxVisible, 1)
	n, opened := p.bundles.add(view.ID, r.Now, b.Window, decide.MemberOf(r.Event))
	detail := map[string]any{"view": view.ID, "view_name": view.Name, "count": n,
		"window_s": int(b.Window.Seconds()), "max_visible": visible, "window_opened": opened.UTC().Format(time.RFC3339)}
	if n <= visible {
		return Pass(fmt.Sprintf("view %s: %d of %d on its own in %s", view.Name, n, visible, Duration(b.Window))).With(detail)
	}

	// Push the event as the view's bundle notification.
	ev := *r.Event
	title := ev.Source
	if ev.Title != nil && *ev.Title != "" {
		title = *ev.Title
	}
	name, thread := view.Name, "view:"+view.ID
	summary := fmt.Sprintf("%d new in %s: %s", n, view.Name, title)
	ev.Title, ev.Summary, ev.Body, ev.Thread = &name, &summary, nil, &thread
	r.Event = &ev
	r.CollapseID = CollapseID(thread)
	detail["collapse_id"] = r.CollapseID
	why := fmt.Sprintf("bundled into view %s (%d in %s)", view.Name, n, Duration(r.Now.Sub(opened)))
	p.afterBundle(r, view.ID, view.Name, opened, n)
	if n == visible+1 {
		return Pass(why).With(detail)
	}
	return Downgrade(api.LevelPassive, why+"; replaces the bundle quietly").With(detail)
}

// bundler keeps each bundling view's open window.
type bundler struct {
	mu      sync.Mutex
	windows map[string]*bundleWindow
}

type bundleWindow struct {
	opened time.Time
	ends   time.Time
	count  int
	// members are the window's events, for the bundle text hook; the
	// newest decide.MaxBundleMembers.
	members []decide.Member
	// llm is the bundle text hook's state (stage_bundle_llm.go).
	llm bundleLLM
}

// add counts one push of view at now and returns the count in the view's
// window and when the window opened. A push after the window ends opens a
// new one.
func (b *bundler) add(view string, now time.Time, window time.Duration, m decide.Member) (int, time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.windows == nil {
		b.windows = map[string]*bundleWindow{}
	}
	for id, w := range b.windows {
		if !now.Before(w.ends) {
			delete(b.windows, id)
		}
	}
	w, ok := b.windows[view]
	if !ok {
		w = &bundleWindow{opened: now, ends: now.Add(window)}
		b.windows[view] = w
	}
	w.count++
	w.members = append(w.members, m)
	if len(w.members) > decide.MaxBundleMembers {
		w.members = w.members[len(w.members)-decide.MaxBundleMembers:]
	}
	return w.count, w.opened
}

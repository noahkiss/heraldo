package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/decide"
	"github.com/noahkiss/heraldo/internal/llm"
	"github.com/noahkiss/heraldo/internal/store"
)

// The LLM at the hold (docs/api.md, "LLM", "Decisions").
//
// The deadline forward never waits for the LLM. When entries reach their
// deadline with triage still silent, the tick forwards them at once, as it
// always has, and starts one call in the background about them and the
// entries still held beside them. A triage release or suppress never goes
// to the LLM, and the dead-man's forwards stay deterministic.
//
// When the answer comes back:
//   - shadow records what it would do;
//   - advise replaces each forwarded notification, quietly and through its
//     collapse id, with the group's summary added; a member still held
//     carries the summary when its own deadline forwards it;
//   - enforce replaces the group's first forwarded notification with the
//     group's title and summary, and folds the forwards of the members
//     still held into that one notification (same collapse id) at their
//     own deadlines. A suppression (llm.hold_may_suppress, BLIP or FALSE,
//     below time-sensitive) acts only on members not pushed yet: at their
//     deadline they become one passive summary. On members already pushed
//     it is recorded, not applied.
//
// The plans live in memory: after a restart, the members forward as usual.

// replacer is a pusher that can replace a delivered notification
// (push.Pusher.Replace).
type replacer interface {
	Replace(ctx context.Context, id, title, summary string) (int, error)
}

// Plan kinds for a member still held.
const (
	planText     = "text"
	planFold     = "fold"
	planSuppress = "suppress"
)

// memberPlan is what a held member's deadline does, from an answer.
type memberPlan struct {
	kind  string
	group *decide.GroupPlan
	// collapse is the group's anchor, for a fold; anchor names the group
	// for a suppression.
	collapse string
	anchor   string
	// reason is the row its forward (or the summary) gets.
	reason  string
	applied any
}

// holdLLM is the hold's LLM state.
type holdLLM struct {
	mu sync.Mutex
	// plans are the members still held that an answer covers.
	plans map[string]*memberPlan
	// forwarded maps a forwarded entry to its forward's event id ("" while
	// the forward is being stored).
	forwarded map[string]string
	// asking are the entries a call in flight covers.
	asking map[string]bool
	wg     sync.WaitGroup
}

func (h *holdLLM) init() {
	if h.plans == nil {
		h.plans, h.forwarded, h.asking = map[string]*memberPlan{}, map[string]string{}, map[string]bool{}
	}
}

// claim takes the plan of an entry about to forward and marks it
// forwarding.
func (h *holdLLM) claim(id string) *memberPlan {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.init()
	p := h.plans[id]
	delete(h.plans, id)
	h.forwarded[id] = ""
	return p
}

// unclaim undoes claim after a failed forward; the plan is dropped, so the
// next tick forwards as usual.
func (h *holdLLM) unclaim(id string) {
	h.mu.Lock()
	delete(h.forwarded, id)
	h.mu.Unlock()
}

// prune forgets the entries the hold no longer lists.
func (h *holdLLM) prune(entries []store.HoldEntry) {
	keep := make(map[string]bool, len(entries))
	for _, e := range entries {
		keep[e.ID] = true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range []map[string]string{h.forwarded} {
		for id := range m {
			if !keep[id] {
				delete(m, id)
			}
		}
	}
	for id := range h.plans {
		if !keep[id] {
			delete(h.plans, id)
		}
	}
}

func (h *holdLLM) done(id, eventID string) {
	h.mu.Lock()
	h.forwarded[id] = eventID
	h.mu.Unlock()
}

// WaitHold waits for the hold's LLM calls in flight. Tests use it.
func (srv *Server) WaitHold() { srv.s.hl.wg.Wait() }

// rungFor is the ladder tag that stores an event at level l.
func rungFor(l api.Level) string {
	switch l {
	case api.LevelPassive:
		return "quiet"
	case api.LevelActive:
		return "general"
	}
	return "alert"
}

// holdMember reads an entry as the LLM sees it: its held event when there
// is one, else the entry itself.
func (s *server) holdMember(ctx context.Context, e store.HoldEntry) decide.Member {
	if e.EventID != "" {
		if rec, err := s.Store.Get(ctx, e.EventID); err == nil {
			var ev api.Event
			if json.Unmarshal(rec.Event, &ev) == nil {
				m := decide.MemberOf(&ev)
				m.HoldID = e.ID
				return m
			}
		}
	}
	lvl := api.LevelActive
	if l, ok := RungLevels[strings.ToLower(e.Tag)]; ok {
		lvl = l
	}
	return decide.Member{HoldID: e.ID, EventID: e.EventID, Title: e.Title, Body: e.Body, Source: e.Source,
		Kind: api.KindMessage, Level: lvl}
}

// foldText is the summary of a group's one notification.
func foldText(g *decide.GroupPlan) string {
	if len(g.Members) > 1 {
		return fmt.Sprintf("%s (%d alerts)", g.Text, len(g.Members))
	}
	return g.Text
}

// forwardHold forwards a due entry, as plan p says when there is one. It
// returns the forward's event id.
func (s *server) forwardHold(ctx context.Context, e store.HoldEntry, p *memberPlan) (string, bool) {
	meta := map[string]any{"collapse": "hold:" + e.ID}
	var over map[string]any
	if p != nil {
		switch p.kind {
		case planText:
			over = map[string]any{"summary": cut(p.group.Text, 256)}
		case planFold:
			meta["collapse"], meta["fold"] = p.collapse, true
			over = map[string]any{"title": cut(p.group.Title, 256), "summary": cut(foldText(p.group), 256)}
		}
	}
	return s.forwardWith(ctx, e, s.deadlinePrefix(), "deadline", over, meta)
}

// suppressed is the members of one group a tick folds into a passive
// summary.
type suppressed struct {
	plan    *memberPlan
	entries []store.HoldEntry
}

// releaseSummary stores one passive summary for a suppressed group's
// members still held, and closes them. It reports whether it did.
func (s *server) releaseSummary(ctx context.Context, b suppressed) bool {
	g := b.plan.group
	ids := make([]string, len(b.entries))
	var body strings.Builder
	body.WriteString(g.Summary)
	body.WriteString("\n\nHeld alerts:")
	for i, e := range b.entries {
		ids[i] = e.ID
		t := e.Title
		if t == "" {
			t = e.Source
		}
		body.WriteString("\n- " + t)
	}
	msg := map[string]any{"title": g.Title, "body": body.String(), "tag": rungFor(api.LevelPassive) + ",llm", "source": holdSource}
	over := map[string]any{}
	if g.Text != "" {
		over["summary"] = cut(g.Text, 256)
	}
	rec, err := s.ingestNotifyWith(ctx, msg, nil,
		map[string]any{"state": "forward", "reason": "llm", "ids": ids, "verdict": g.Verdict}, "", over)
	if err != nil {
		s.Logger.Error("hold: the LLM's passive summary; forwarding as usual next tick", "err", err)
		return false
	}
	if _, err := s.Store.CloseHolds(ctx, ids, store.HoldReleased, g.Verdict, "llm", s.Now()); err != nil {
		s.Logger.Error("hold: close after the LLM's summary", "err", err)
	}
	t := s.planTrace(b.plan)
	s.appendTrace(ctx, rec.ID, t)
	for _, e := range b.entries {
		s.appendTrace(ctx, e.EventID, t)
	}
	return true
}

func (s *server) planTrace(p *memberPlan) decide.Trace {
	det := maps.Clone(p.group.Detail)
	det["applied"] = p.applied
	t := decide.Trace{Reason: p.reason, Detail: det}
	if p.kind != planText {
		t.Outcome = "merge"
	}
	return t
}

// askHold starts the background call about the entries that just
// forwarded (due) and the ones still held beside them.
func (s *server) askHold(due []store.HoldEntry, entries []store.HoldEntry) {
	if len(due) == 0 || s.decide.Mode() == llm.ModeOff {
		return
	}
	h := &s.hl
	h.mu.Lock()
	h.init()
	batch := append([]store.HoldEntry{}, due...)
	in := map[string]bool{}
	for _, e := range due {
		in[e.ID] = true
	}
	for _, e := range entries {
		if in[e.ID] || !e.Open() || e.Forwarded || h.plans[e.ID] != nil || h.asking[e.ID] {
			continue
		}
		if _, ok := h.forwarded[e.ID]; ok {
			continue
		}
		batch = append(batch, e)
	}
	for _, e := range batch {
		h.asking[e.ID] = true
	}
	h.wg.Add(1)
	h.mu.Unlock()
	go func() {
		defer h.wg.Done()
		s.applyHold(context.Background(), batch)
		h.mu.Lock()
		for _, e := range batch {
			delete(h.asking, e.ID)
		}
		h.mu.Unlock()
	}()
}

// applyHold asks and applies one answer.
func (s *server) applyHold(ctx context.Context, batch []store.HoldEntry) {
	ms := make([]decide.Member, len(batch))
	for i, e := range batch {
		ms[i] = s.holdMember(ctx, e)
	}
	res := s.decide.Hold(ctx, ms)
	h := &s.hl
	fwdOf := func(id string) (string, bool) {
		h.mu.Lock()
		defer h.mu.Unlock()
		ev, ok := h.forwarded[id]
		return ev, ok
	}
	for _, a := range res.Alone {
		s.appendTrace(ctx, a.Member.EventID, a.Trace)
		if ev, ok := fwdOf(a.Member.HoldID); ok {
			s.appendTrace(ctx, ev, a.Trace)
		}
	}
	for i := range res.Groups {
		s.applyGroup(ctx, &res.Groups[i])
	}
}

// applyGroup applies one group to its members: those already forwarded,
// those still held, and those triage closed meanwhile.
func (s *server) applyGroup(ctx context.Context, g *decide.GroupPlan) {
	h := &s.hl
	ids := make([]string, len(g.Members))
	for i, m := range g.Members {
		ids[i] = m.HoldID
	}
	open, _, err := s.Store.SplitHolds(ctx, ids)
	if err != nil {
		s.Logger.Error("hold: look up the LLM's group", "err", err)
		return
	}
	isOpen := map[string]bool{}
	for _, id := range open {
		isOpen[id] = true
	}
	type pushed struct {
		m     decide.Member
		event string
	}
	var sent []pushed
	var held, closed []decide.Member
	h.mu.Lock()
	for _, m := range g.Members {
		if ev, ok := h.forwarded[m.HoldID]; ok {
			sent = append(sent, pushed{m, ev})
		} else if isOpen[m.HoldID] {
			held = append(held, m)
		} else {
			closed = append(closed, m)
		}
	}
	// The anchor is the first member already forwarded, else the first
	// still held; a keyed member's notification collapses by its key.
	var anchor decide.Member
	if len(sent) > 0 {
		anchor = sent[0].m
	} else if len(held) > 0 {
		anchor = held[0]
	}
	collapse := "hold:" + anchor.HoldID
	if anchor.Key != "" {
		collapse = anchor.Key
	}
	suppress := g.Suppress()
	plans := map[string]*memberPlan{}
	for _, m := range held {
		var p *memberPlan
		switch {
		case g.Apply == decide.ApplyText:
			p = &memberPlan{kind: planText, reason: g.Head + "; its forward carries the summary", applied: "text"}
		case g.Apply == decide.ApplyGroup && suppress && !decide.Loud(m.Level):
			p = &memberPlan{kind: planSuppress, anchor: held[0].HoldID,
				reason: g.Head + " → released as 1 passive summary", applied: true}
		case g.Apply == decide.ApplyGroup:
			p = &memberPlan{kind: planFold, collapse: collapse,
				reason: g.Head + " → folded into the group's notification" + g.Note(), applied: true}
		}
		if p != nil {
			p.group = g
			h.plans[m.HoldID] = p
			plans[m.HoldID] = p
		}
	}
	h.mu.Unlock()

	row := func(reason string, applied any, whyNot string) decide.Trace {
		det := maps.Clone(g.Detail)
		det["applied"] = applied
		if whyNot != "" {
			det["why_not"] = whyNot
		}
		out := decide.Trace{Reason: reason, Detail: det}
		if g.Apply == decide.ApplyGroup && applied == true {
			out.Outcome = "merge"
		}
		return out
	}
	if g.Apply == decide.ApplyNone {
		would := "fold into 1 notification"
		if g.Level == api.LevelPassive && decide.Rank(g.Max) > 0 {
			would = "fold into 1 passive summary"
		}
		t := row(g.Head+" → would "+would+g.Note()+"; nothing changed (shadow)", false, "shadow")
		for _, m := range g.Members {
			s.appendTrace(ctx, m.EventID, t)
		}
		for _, p := range sent {
			s.appendTrace(ctx, p.event, t)
		}
		return
	}
	for _, m := range closed {
		s.appendTrace(ctx, m.EventID, row(g.Head+"; triage closed it first", false, "closed by triage"))
	}
	for _, m := range held {
		p := plans[m.HoldID]
		at := "at its deadline"
		switch p.kind {
		case planText:
			s.appendTrace(ctx, m.EventID, row(g.Head+"; its forward will carry the summary "+at, "text", ""))
		case planSuppress:
			s.appendTrace(ctx, m.EventID, row(g.Head+" → folds into 1 passive summary "+at, true, ""))
		case planFold:
			s.appendTrace(ctx, m.EventID, row(g.Head+" → folds into the group's notification "+at+g.Note(), true, ""))
		}
	}
	for i, p := range sent {
		var reason string
		applied := any(false)
		whyNot := ""
		switch {
		case g.Apply == decide.ApplyText:
			reason, applied, whyNot = s.replaceNote(ctx, p.event, "", g.Text, g.Head+"; added its summary to the notification")
		case i == 0:
			reason, applied, whyNot = s.replaceNote(ctx, p.event, g.Title, foldText(g), g.Head+" → replaced its notification with the group's")
			if suppress {
				reason += "; already pushed: suppression recorded, not applied"
			}
		case suppress:
			reason, whyNot = g.Head+"; already pushed: suppression recorded, not applied", "already pushed"
		default:
			reason, whyNot = g.Head+"; already pushed: left as it was", "already pushed"
		}
		t := row(reason+g.Note(), applied, whyNot)
		s.appendTrace(ctx, p.m.EventID, t)
		s.appendTrace(ctx, p.event, t)
	}
}

// replaceWait is how long a replace waits for the forward's push to be
// decided.
const replaceWait = 15 * time.Second

// replaceNote replaces a forwarded notification and returns the reason,
// what was applied and why not.
func (s *server) replaceNote(ctx context.Context, eventID, title, summary, head string) (string, any, string) {
	r, ok := s.Pusher.(replacer)
	if !ok || eventID == "" {
		return head + "; not sent: push is off", false, "push off"
	}
	end := time.Now().Add(replaceWait)
	for {
		n, err := r.Replace(ctx, eventID, title, summary)
		switch {
		case err == nil && n > 0:
			return head, true, ""
		case err == nil:
			return head + "; not sent: no device took it", false, "no device"
		case errors.Is(err, decide.ErrNotDecided) && time.Now().Before(end):
			time.Sleep(25 * time.Millisecond)
			continue
		}
		return head + "; not sent: " + err.Error(), false, err.Error()
	}
}

// appendTrace adds an LLM row to an event's trace, once per event, call
// and reason: a retry adds nothing. An empty reason or id adds nothing.
func (s *server) appendTrace(ctx context.Context, eventID string, t decide.Trace) {
	if eventID == "" || t.Reason == "" {
		return
	}
	call, _ := t.Detail["call_id"].(string)
	if rows, err := s.Store.Decisions(ctx, eventID); err == nil {
		for _, r := range rows {
			if r.Stage != store.LLMStage || r.Reason != t.Reason {
				continue
			}
			var det map[string]any
			_ = json.Unmarshal(r.Detail, &det)
			if c, _ := det["call_id"].(string); c == call {
				return
			}
		}
	}
	if err := s.Store.AppendDecision(context.WithoutCancel(ctx), eventID, t.Row(s.Now())); err != nil {
		s.Logger.Error("llm: record the decision", "event", eventID, "err", err)
	}
}

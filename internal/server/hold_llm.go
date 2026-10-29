package server

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/decide"
	"github.com/noahkiss/heraldo/internal/llm"
	"github.com/noahkiss/heraldo/internal/store"
)

// The LLM at the hold (docs/api.md, "LLM"). It decides at the deadline
// only: the tick asks the decisions model about the entries whose deadline
// passed with triage still silent, in one call, and applies the answer
// before it forwards them. A triage release or suppress never goes to the
// LLM, and the dead-man's forwards stay deterministic: they run when
// something is already broken.

// holdTriage is what the LLM decided for the due entries.
type holdTriage struct {
	// done are the entries a release closed: the tick leaves them alone.
	done map[string]bool
	// text is the summary a forward carries, by hold id.
	text map[string]string
	// trace is the row a forward's event gets, by hold id.
	trace map[string]decide.Trace
}

// rungFor is the ladder tag that stores a release at level l.
func rungFor(l api.Level) string {
	switch l {
	case api.LevelPassive:
		return "quiet"
	case api.LevelActive:
		return "general"
	}
	return "alert"
}

// holdMember reads a due entry as the LLM sees it: its held event when
// there is one, else the entry itself.
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

// triageDue asks the LLM about due entries and applies its releases. With
// the effective mode off it does nothing.
func (s *server) triageDue(ctx context.Context, due []store.HoldEntry) holdTriage {
	out := holdTriage{done: map[string]bool{}, text: map[string]string{}, trace: map[string]decide.Trace{}}
	if len(due) == 0 || s.decide.Mode() == llm.ModeOff {
		return out
	}
	ms := make([]decide.Member, len(due))
	for i, e := range due {
		ms[i] = s.holdMember(ctx, e)
	}
	for _, p := range s.decide.Hold(ctx, ms) {
		if p.Action == decide.ActRelease {
			if id, ok := s.releaseLLM(ctx, p); ok {
				for _, m := range p.Members {
					out.done[m.HoldID] = true
					s.appendTrace(ctx, m.EventID, p.Trace)
				}
				s.appendTrace(ctx, id, p.Trace)
				continue
			}
			p.Trace = decide.Trace{Reason: "llm: could not store the release; forwarded as usual",
				Detail: map[string]any{"applied": false}}
			p.Text = ""
		}
		for _, m := range p.Members {
			out.text[m.HoldID] = p.Text
			out.trace[m.HoldID] = p.Trace
			s.appendTrace(ctx, m.EventID, p.Trace)
		}
	}
	return out
}

// releaseLLM closes a group with one event, as a triage release does, and
// returns the event's id.
func (s *server) releaseLLM(ctx context.Context, p decide.HoldPlan) (string, bool) {
	ids := make([]string, len(p.Members))
	var b strings.Builder
	b.WriteString(p.Summary)
	b.WriteString("\n\nHeld alerts:")
	for i, m := range p.Members {
		ids[i] = m.HoldID
		t := m.Title
		if t == "" {
			t = m.Source
		}
		b.WriteString("\n- " + t)
	}
	msg := map[string]any{"title": p.Title, "body": b.String(), "tag": rungFor(p.Level) + ",llm", "source": holdSource}
	set := map[string]any{}
	if line := firstLine(p.Summary); line != "" {
		set["summary"] = cut(line, 256)
	}
	rec, err := s.ingestNotifyWith(ctx, msg, nil,
		map[string]any{"state": "forward", "reason": "llm", "ids": ids, "verdict": p.Verdict}, "", set)
	if err != nil {
		s.Logger.Error("hold: LLM release; forwarding as usual", "err", err)
		return "", false
	}
	if _, err := s.Store.CloseHolds(ctx, ids, store.HoldReleased, p.Verdict, "llm", s.Now()); err != nil {
		s.Logger.Error("hold: close after the LLM release", "err", err)
	}
	return rec.ID, true
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// appendTrace adds an LLM row to an event's trace. An empty reason or id
// adds nothing.
func (s *server) appendTrace(ctx context.Context, eventID string, t decide.Trace) {
	if eventID == "" || t.Reason == "" {
		return
	}
	if err := s.Store.AppendDecision(context.WithoutCancel(ctx), eventID, t.Row(s.Now())); err != nil {
		s.Logger.Error("llm: record the decision", "event", eventID, "err", err)
	}
}

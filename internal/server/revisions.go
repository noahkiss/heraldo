package server

import (
	"context"
	"crypto/sha256"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/ids"
	"github.com/noahkiss/heraldo/internal/store"
)

// ---- revision vector ---------------------------------------------------

// revisions is the in-memory revision vector: one counter per domain, and
// the boot id. Nothing persists it: a restart makes a new boot id, and a
// client that sees a new boot refreshes everything once.
type revisions struct {
	boot string
	n    map[store.Domain]*atomic.Int64 // fixed keys; never written after newRevisions
	hub  *hub
}

func newRevisions(now time.Time, h *hub) *revisions {
	r := &revisions{boot: ids.New(now), n: map[store.Domain]*atomic.Int64{}, hub: h}
	for _, d := range store.Domains {
		r.n[d] = &atomic.Int64{}
	}
	return r
}

// bump moves the counter of each domain in ds, then tells the streams once.
// It never blocks.
func (r *revisions) bump(ds ...store.Domain) {
	moved := false
	for _, d := range ds {
		if c := r.n[d]; c != nil {
			c.Add(1)
			moved = true
		}
	}
	if moved {
		r.hub.signal()
	}
}

func (r *revisions) get(d store.Domain) int64 { return r.n[d].Load() }

// snapshot is the vector as GET /v1/revisions answers it.
func (r *revisions) snapshot() api.Revisions {
	g := r.get
	return api.Revisions{Boot: r.boot, Rev: api.RevisionVector{
		Events: g(store.DomainEvents), Cursor: g(store.DomainCursor), State: g(store.DomainState),
		Checks: g(store.DomainChecks), Projects: g(store.DomainProjects), Views: g(store.DomainViews),
		Held: g(store.DomainHeld), Activities: g(store.DomainActivities), Llm: g(store.DomainLLM),
		Mutes: g(store.DomainMutes), Settings: g(store.DomainSettings),
	}}
}

// getRevisions is GET /v1/revisions.
func (s *server) getRevisions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.revs.snapshot())
}

// ---- changes that need no write ----------------------------------------

// Some read answers change with time alone: a flapping incident settles,
// an incident turns stale, a mute runs out, a closed hold entry leaves the
// list. No write marks them, so a clock looks for them.

// timeWatch is the clock's memory: each domain's fingerprint, and the
// domain's revision when it was taken.
type timeWatch struct {
	mu   sync.Mutex
	last map[store.Domain]watchMark
}

type watchMark struct {
	sum [32]byte
	rev int64
}

// RunTimeChanges looks every interval, until ctx ends, for read answers
// that time alone changed, and bumps their domains.
func (srv *Server) RunTimeChanges(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		srv.s.tickTime(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// tickTime fingerprints the parts of the state, the mutes and the held
// list that time changes. A domain is bumped only when its fingerprint
// moved while its revision did not: a write already told the clients.
func (s *server) tickTime(ctx context.Context) {
	now := s.Now()
	// The revisions are read before the data: a write in between moves
	// both, and the next tick sees no stale fingerprint to bump for.
	revs := map[store.Domain]int64{}
	for _, d := range []store.Domain{store.DomainState, store.DomainMutes, store.DomainHeld} {
		revs[d] = s.revs.get(d)
	}
	sums := map[store.Domain][32]byte{}
	if st, err := s.Store.State(ctx, nil); err == nil {
		s.staleState(&st, false)
		h := sha256.New()
		for _, raw := range st.Incidents {
			h.Write(raw)
			h.Write([]byte{0})
		}
		sums[store.DomainState] = [32]byte(h.Sum(nil))
	} else {
		s.Logger.Error("revisions: state", "err", err)
	}
	if ms, err := s.Store.Mutes(ctx, now); err == nil {
		h := sha256.New()
		for _, m := range ms {
			h.Write([]byte(m.ID))
			h.Write([]byte{0})
		}
		sums[store.DomainMutes] = [32]byte(h.Sum(nil))
	} else {
		s.Logger.Error("revisions: mutes", "err", err)
	}
	if es, err := s.Store.Holds(ctx, now.Add(-closedWindow)); err == nil {
		h := sha256.New()
		for _, e := range es {
			h.Write([]byte(e.ID))
			h.Write([]byte{0})
		}
		sums[store.DomainHeld] = [32]byte(h.Sum(nil))
	} else {
		s.Logger.Error("revisions: held", "err", err)
	}
	s.tw.mu.Lock()
	defer s.tw.mu.Unlock()
	if s.tw.last == nil {
		s.tw.last = map[store.Domain]watchMark{}
	}
	for d, sum := range sums {
		rev := revs[d]
		prev, seen := s.tw.last[d]
		if seen && prev.sum != sum && prev.rev == rev {
			// A mute that ran out changes what the apps show in the state.
			if d == store.DomainMutes {
				s.revs.bump(d, store.DomainState)
			} else {
				s.revs.bump(d)
			}
			rev = s.revs.get(d)
		}
		s.tw.last[d] = watchMark{sum: sum, rev: rev}
	}
}

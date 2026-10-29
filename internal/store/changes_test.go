package store

import (
	"context"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/api"
)

type changeLog struct {
	mu sync.Mutex
	ds []Domain
}

func (c *changeLog) add(ds ...Domain) { c.mu.Lock(); c.ds = append(c.ds, ds...); c.mu.Unlock() }
func (c *changeLog) take() []Domain {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.ds
	c.ds = nil
	return out
}

// A write that changes nothing tells no one: a cursor that did not move, a
// mute already ended, a hold entry already closed, a duplicate event.
func TestOnChangeOnlyOnChange(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	log := &changeLog{}
	s.OnChange(log.add)
	kind, dedup := api.KindMessage, "d-1"
	ev := func(id string) NewEvent {
		return NewEvent{ID: id, ReceivedAt: time.Now(), Fields: map[string]any{"id": id, "source": "s", "kind": "message"},
			Typed: api.EventInput{Source: "s", Kind: &kind, Dedup: &dedup}}
	}
	if _, created, err := s.Insert(ctx, ev("01A")); err != nil || !created {
		t.Fatal(created, err)
	}
	if got := log.take(); !slices.Contains(got, DomainEvents) || !slices.Contains(got, DomainCursor) {
		t.Fatalf("insert: %v", got)
	}
	if _, created, err := s.Insert(ctx, ev("01B")); err != nil || created {
		t.Fatal(created, err)
	}
	if got := log.take(); len(got) != 0 {
		t.Fatalf("duplicate: %v", got)
	}
	if _, err := s.SetCursor(ctx, "inbox", 1, "t"); err != nil {
		t.Fatal(err)
	}
	if got := log.take(); !slices.Contains(got, DomainCursor) {
		t.Fatalf("cursor moved: %v", got)
	}
	if _, err := s.SetCursor(ctx, "inbox", 1, "t"); err != nil {
		t.Fatal(err)
	}
	if got := log.take(); len(got) != 0 {
		t.Fatalf("cursor stayed: %v", got)
	}
	if _, err := s.CloseHolds(ctx, []string{"h-none"}, HoldReleased, "", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := log.take(); len(got) != 0 {
		t.Fatalf("close nothing: %v", got)
	}
	s.OnChange(nil)
	if _, err := s.SetCursor(ctx, "other", 1, "t"); err != nil {
		t.Fatal(err)
	}
	if got := log.take(); len(got) != 0 {
		t.Fatalf("after OnChange(nil): %v", got)
	}
}

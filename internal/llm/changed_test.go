package llm

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// Changed fires when the in-memory part of Status moves: a failure, and
// the success that clears it. A success with nothing to clear is quiet.
func TestChangedHook(t *testing.T) {
	f := &Fake{Err: errors.New("boom")}
	var n atomic.Int32
	clk := &clock{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	c := New(Config{Settings: manager(t, on(nil)), NewProvider: f.Factory, Now: clk.now,
		Changed: func() { n.Add(1) }})
	if _, err := c.Call(context.Background(), simple("a")); err == nil {
		t.Fatal("want an error")
	}
	if n.Load() == 0 {
		t.Fatal("a failure did not call Changed")
	}
	f.Err = nil
	f.Replies = []Response{{Content: "x"}, {Content: "y"}}
	before := n.Load()
	if _, err := c.Call(context.Background(), simple("b")); err != nil {
		t.Fatal(err)
	}
	if n.Load() == before {
		t.Fatal("the success that cleared the failure did not call Changed")
	}
	before = n.Load()
	if _, err := c.Call(context.Background(), simple("c")); err != nil {
		t.Fatal(err)
	}
	if n.Load() != before {
		t.Fatal("a plain success called Changed")
	}
}

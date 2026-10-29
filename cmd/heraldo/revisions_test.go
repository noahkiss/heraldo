package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/internal/client"
)

// The client reads the revision vector, and tells an older server by its
// 404.
func TestClientRevisions(t *testing.T) {
	f := newFixture(t)
	c := &client.Client{BaseURL: f.srv.URL, Token: readTok}
	ctx := context.Background()
	rv, err := c.Revisions(ctx)
	if err != nil || rv.Boot == "" {
		t.Fatalf("revisions: %+v %v", rv, err)
	}
	f.post(writeTok, `{"source":"s","kind":"message","title":"x"}`)
	rv2, err := c.Revisions(ctx)
	if err != nil || rv2.Rev.Events == rv.Rev.Events || rv2.Boot != rv.Boot {
		t.Fatalf("after a post: %+v (was %+v) %v", rv2, rv, err)
	}
	old := httptest.NewServer(http.NotFoundHandler())
	defer old.Close()
	if _, err := (&client.Client{BaseURL: old.URL, Token: readTok}).Revisions(ctx); !errors.Is(err, client.ErrNoRevisions) {
		t.Fatalf("old server: %v", err)
	}
}

// Stream skips the revisions event and passes each poke's cursor, also
// for a poke that stored no event.
func TestClientStreamSkipsRevisions(t *testing.T) {
	f := newFixture(t)
	f.post(writeTok, `{"source":"s","kind":"message","title":"x"}`)
	c := &client.Client{BaseURL: f.srv.URL, Token: readTok}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got []string
	err := c.Stream(ctx, func(cursor string) error {
		got = append(got, cursor)
		switch len(got) {
		case 1:
			if _, err := c.SetCursor(ctx, "inbox", cursor); err != nil {
				return err
			}
		case 2:
			return errStop
		}
		return nil
	})
	if !errors.Is(err, errStop) || len(got) != 2 || got[0] != "s1" || got[1] != "s1" {
		t.Fatalf("pokes %v, err %v", got, err)
	}
}

var errStop = errors.New("stop")

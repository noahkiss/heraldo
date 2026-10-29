package llm

import (
	"context"
	"errors"
	"sync"
)

// Fake is a Provider for tests: it answers from Reply, or from Replies in
// order, and records every request. It never makes a network call.
type Fake struct {
	mu sync.Mutex
	// Reply answers each request when set.
	Reply func(req Request) (Response, error)
	// Replies answer requests in order when Reply is nil; past the end,
	// the last one repeats.
	Replies []Response
	// Err, when set and Reply is nil, fails every request.
	Err  error
	reqs []Request
	// Built is the API kind and base URL of each provider Factory built.
	Built []string
}

// Complete records req and answers it.
func (f *Fake) Complete(ctx context.Context, req Request) (Response, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	n := len(f.reqs)
	reply, replies, ferr := f.Reply, f.Replies, f.Err
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	switch {
	case reply != nil:
		return reply(req)
	case ferr != nil:
		return Response{}, ferr
	case len(replies) > 0:
		return replies[min(n, len(replies))-1], nil
	}
	return Response{}, errors.New("fake: no reply")
}

// Requests returns the requests so far.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request{}, f.reqs...)
}

// Factory is a Config.NewProvider that returns f and records what it was
// built with.
func (f *Fake) Factory(apiKind, baseURL, token string) Provider {
	f.mu.Lock()
	f.Built = append(f.Built, apiKind+" "+baseURL)
	f.mu.Unlock()
	return f
}

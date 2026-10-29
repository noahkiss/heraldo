// Package mcphttp serves the MCP tools (internal/mcptools) on the server at
// POST /v1/mcp, over streamable HTTP, when HERALDO_MCP_HTTP is on.
//
// The server checks the bearer token before a request reaches this handler.
// The tools then call the REST API in the same process, with the token as an
// auth grant (auth.WithGrant): no network hop, and each call is checked as
// the REST API checks it. A read token gets the read tools. A write or admin
// token gets the write tools as well, and its grant may also read.
//
// The transport is stateless: every POST carries its own bearer token, and
// the server keeps no MCP session.
package mcphttp

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/noahkiss/heraldo/internal/auth"
	"github.com/noahkiss/heraldo/internal/client"
	"github.com/noahkiss/heraldo/internal/mcptools"
)

// MaxRequestBytes caps one MCP request body, as the REST API caps a POST.
const MaxRequestBytes = 1 << 20

// inProcessURL is the base URL the tools' client uses. No request leaves the
// process; the host only fills the request line.
const inProcessURL = "http://heraldo.in-process"

// Options configure the endpoint.
type Options struct {
	Version string
	Logger  *slog.Logger
}

// New returns the factory server.Options.MCP takes.
func New(o Options) func(api http.Handler, tokenOf func(*http.Request) auth.Token) http.Handler {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	// A stateless server connects a session per request, and the SDK logs
	// each one at info. The server's request log already has a line per
	// call, so keep warnings and errors only.
	o.Logger = slog.New(warnOnly{o.Logger.Handler()})
	return func(api http.Handler, tokenOf func(*http.Request) auth.Token) http.Handler {
		e := &endpoint{Options: o, api: api, servers: map[string]*mcp.Server{}}
		return mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
			return e.server(tokenOf(r), r.Host)
		}, &mcp.StreamableHTTPOptions{
			Stateless:           true,
			JSONResponse:        true,
			Logger:              o.Logger,
			MaxRequestBodyBytes: MaxRequestBytes,
			// Every request carries a bearer token the server has checked,
			// so a DNS-rebound page gains nothing. The check would refuse a
			// reverse proxy on the same host that forwards the public name.
			DisableLocalhostProtection: true,
		})
	}
}

type endpoint struct {
	Options
	api     http.Handler
	mu      sync.Mutex
	servers map[string]*mcp.Server
}

// server returns the tools server for token t, made on first use. Tokens
// are a fixed set, so the cache stays small.
func (e *endpoint) server(t auth.Token, host string) *mcp.Server {
	e.mu.Lock()
	defer e.mu.Unlock()
	key := t.Name + "\x00" + string(t.Scope)
	if s, ok := e.servers[key]; ok {
		return s
	}
	c := &client.Client{
		BaseURL: inProcessURL,
		// The grant carries the credential. This placeholder is sent so the
		// handlers that branch on an Authorization header take the bearer
		// path; it matches no token.
		Token:     "in-process",
		HTTP:      &http.Client{Transport: &inProcess{api: e.api, grant: t}},
		UserAgent: "heraldo-mcp-http/" + e.Version,
	}
	o := mcptools.Options{
		API: c, URLHost: host, Version: e.Version, Logger: e.Logger,
		Source:     "agent:" + t.Name,
		ConfigHint: "the server's token list sets each token's scope",
	}
	switch t.Scope {
	case auth.ScopeWrite:
		o.Write = true
	case auth.ScopeAdmin:
		o.Write, o.Admin = true, true
	default:
		o.WriteOff = "a read token"
	}
	s := mcptools.New(o)
	e.servers[key] = s
	return s
}

// inProcess is an http.RoundTripper that serves each request with the API
// handler in this process, under an auth grant.
type inProcess struct {
	api   http.Handler
	grant auth.Token
}

func (p *inProcess) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(auth.WithGrant(req.Context(), p.grant))
	r.RequestURI = r.URL.RequestURI()
	r.RemoteAddr = "in-process"
	if r.Body == nil {
		r.Body = http.NoBody
	}
	w := &recorder{header: http.Header{}, status: http.StatusOK}
	p.api.ServeHTTP(w, r)
	return &http.Response{
		Status: strconv.Itoa(w.status) + " " + http.StatusText(w.status), StatusCode: w.status,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: w.header, Body: io.NopCloser(bytes.NewReader(w.body.Bytes())),
		ContentLength: int64(w.body.Len()), Request: req,
	}, nil
}

// warnOnly passes records at warn and above.
type warnOnly struct{ slog.Handler }

func (h warnOnly) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelWarn && h.Handler.Enabled(ctx, l)
}

func (h warnOnly) WithAttrs(a []slog.Attr) slog.Handler { return warnOnly{h.Handler.WithAttrs(a)} }
func (h warnOnly) WithGroup(n string) slog.Handler      { return warnOnly{h.Handler.WithGroup(n)} }

// recorder is the response of one in-process call.
type recorder struct {
	header http.Header
	status int
	wrote  bool
	body   bytes.Buffer
}

func (w *recorder) Header() http.Header { return w.header }

func (w *recorder) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
}

func (w *recorder) Write(b []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.body.Write(b)
}

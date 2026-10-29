package auth

import "context"

// An in-process grant is how the server's own MCP endpoint calls the API.
// The MCP endpoint checks the caller's bearer token once, then calls the
// API handlers in the same process with the token attached to the request
// context. Only Go code in this process can attach it: no header, query or
// body sets it.
//
// A grant of a write token also allows read calls. An HTTP MCP client sends
// one bearer token, and a write tool such as incident_act reads before it
// writes. A write token outside MCP still cannot read.

type grantKey struct{}

// WithGrant returns ctx carrying t as an in-process grant.
func WithGrant(ctx context.Context, t Token) context.Context {
	return context.WithValue(ctx, grantKey{}, t)
}

// GrantFrom returns the in-process grant ctx carries.
func GrantFrom(ctx context.Context) (Token, bool) {
	t, ok := ctx.Value(grantKey{}).(Token)
	return t, ok
}

// GrantAllows reports whether a grant of t may act with need: as its scope
// allows, and a plain write grant may also read. A write token with a
// capability (notify-key, trusted-ingest, standby) is a sender's credential
// and never reads. critical does not count: it changes what a write may
// send, not who holds the token.
func GrantAllows(t Token, need Scope) bool {
	plain := true
	for _, c := range t.Caps {
		if c != CapCritical {
			plain = false
		}
	}
	return t.Scope.Allows(need) || (t.Scope == ScopeWrite && need == ScopeRead && plain)
}

// SenderOnly reports whether t is a sender's credential that the MCP
// endpoint refuses: a token with the notify-key or trusted-ingest
// capability. A notify-key token may sit in a URL path and so in access
// logs; a trusted-ingest token is a bridge's.
func SenderOnly(t Token) bool { return t.Has(CapNotifyKey) || t.Has(CapTrustedIngest) }

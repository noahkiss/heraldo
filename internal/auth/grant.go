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
// allows, and a write grant may also read.
func GrantAllows(t Token, need Scope) bool {
	return t.Scope.Allows(need) || (t.Scope == ScopeWrite && need == ScopeRead)
}

// Package auth checks bearer tokens. The server keeps only each token's
// SHA-256, never the token itself.
//
// A token list has one entry per line (a file) or per comma (an env var):
//
//	<name> <scope> <sha256-hex> [capability ...]
//
// scope is write, read or admin. admin covers write and read. A capability
// grants one extra power: trusted-ingest (see CapTrustedIngest),
// notify-key (see CapNotifyKey), standby (see CapStandby) or critical (see
// CapCritical). Blank lines and
// lines starting with # are ignored. `heraldo-server hash-token` prints the
// hash of a token read from stdin.
package auth

import (
	"bufio"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

// Scope is what a token may do.
type Scope string

// Scopes.
const (
	ScopeWrite Scope = "write"
	ScopeRead  Scope = "read"
	ScopeAdmin Scope = "admin"
)

// CapTrustedIngest lets a write token pass server-owned triage state for a
// bridged sender, such as an apprise-tee hold, and dedup against every
// earlier event instead of the last 24 hours.
const CapTrustedIngest = "trusted-ingest"

// CapNotifyKey lets a token stand in the path of the Apprise compatibility
// ingest, POST /notify/{key}, as an Apprise config key does, for senders
// that cannot set a header. The path ends up in access logs, so give such a
// token write scope and nothing more.
const CapNotifyKey = "notify-key"

// CapStandby lets a token read every device's full push tokens and import
// the events a standby server stored: the standby's token on the primary.
const CapStandby = "standby"

// CapCritical lets a token send level critical: an emergency that breaks
// through Focus and the silent switch, and escalates when escalation is on.
// A critical event from a token without it is pushed as time-sensitive. A
// level an ingest rule sets needs no capability: the owner writes the rules.
const CapCritical = "critical"

// Allows reports whether a token with scope s may act with need.
func (s Scope) Allows(need Scope) bool { return s == ScopeAdmin || s == need }

// Token is one configured token.
type Token struct {
	Name  string
	Scope Scope
	Caps  []string
	hash  [sha256.Size]byte
}

// Has reports whether the token carries capability c.
func (t Token) Has(c string) bool {
	for _, x := range t.Caps {
		if x == c {
			return true
		}
	}
	return false
}

// Set is the configured tokens.
type Set struct{ tokens []Token }

// Len returns the number of tokens.
func (s *Set) Len() int { return len(s.tokens) }

// Hash returns the hex SHA-256 of a token, as the token list stores it.
func Hash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// Parse reads a token list. sep splits entries: '\n' for a file, ',' for an
// env var.
func Parse(r io.Reader, sep byte) (*Set, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	set := &Set{}
	names := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Split(splitOn(sep))
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 3 {
			return nil, fmt.Errorf("token entry %d: want <name> <scope> <sha256-hex> [capability ...]", n)
		}
		t := Token{Name: f[0], Scope: Scope(f[1])}
		for _, c := range f[3:] {
			if c != CapTrustedIngest && c != CapNotifyKey && c != CapStandby && c != CapCritical {
				return nil, fmt.Errorf("token %q: unknown capability %q", f[0], c)
			}
			t.Caps = append(t.Caps, c)
		}
		switch t.Scope {
		case ScopeWrite, ScopeRead, ScopeAdmin:
		default:
			return nil, fmt.Errorf("token %q: scope %q is not write, read or admin", t.Name, f[1])
		}
		b, err := hex.DecodeString(strings.ToLower(f[2]))
		if err != nil || len(b) != sha256.Size {
			return nil, fmt.Errorf("token %q: the hash must be 64 hex characters (SHA-256)", t.Name)
		}
		copy(t.hash[:], b)
		if names[t.Name] {
			return nil, fmt.Errorf("token %q: listed twice", t.Name)
		}
		names[t.Name] = true
		set.tokens = append(set.tokens, t)
	}
	return set, sc.Err()
}

func splitOn(sep byte) bufio.SplitFunc {
	return func(data []byte, atEOF bool) (int, []byte, error) {
		for i, b := range data {
			if b == sep || (sep == '\n' && b == '\r') {
				return i + 1, data[:i], nil
			}
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	}
}

// Lookup returns the token that presented hashes to, comparing every entry
// in constant time.
func (s *Set) Lookup(presented string) (Token, bool) {
	if presented == "" {
		return Token{}, false
	}
	h := sha256.Sum256([]byte(presented))
	var found Token
	ok := 0
	for _, t := range s.tokens {
		if subtle.ConstantTimeCompare(h[:], t.hash[:]) == 1 {
			found = t
			ok = 1
		}
	}
	return found, ok == 1
}

// Package auth checks bearer tokens. The server keeps only each token's
// SHA-256, never the token itself.
//
// A token list has one entry per line (a file) or per comma (an env var):
//
//	<name> <scope> <sha256-hex>
//
// scope is write, read or admin. admin covers write and read. Blank lines and
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

// Allows reports whether a token with scope s may act with need.
func (s Scope) Allows(need Scope) bool { return s == ScopeAdmin || s == need }

// Token is one configured token.
type Token struct {
	Name  string
	Scope Scope
	hash  [sha256.Size]byte
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
		if len(f) != 3 {
			return nil, fmt.Errorf("token entry %d: want <name> <scope> <sha256-hex>", n)
		}
		t := Token{Name: f[0], Scope: Scope(f[1])}
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

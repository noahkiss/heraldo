// Package config reads the server configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/noahkiss/heraldo/internal/auth"
)

// DefaultAddr is the listen address when HERALDO_ADDR is unset.
const DefaultAddr = ":7842"

// DefaultDB is the database path when HERALDO_DB is unset.
const DefaultDB = "heraldo.db"

// Config is the server configuration.
type Config struct {
	// Addr is the TCP listen address, host:port. HERALDO_ADDR.
	Addr string
	// DB is the SQLite database path. HERALDO_DB.
	DB string
	// TokensFile is a token list file, one entry per line. HERALDO_TOKENS_FILE.
	TokensFile string
	// Tokens is a token list, entries separated by commas. HERALDO_TOKENS.
	Tokens string
}

// FromEnv builds a Config from getenv, normally os.Getenv.
func FromEnv(getenv func(string) string) (Config, error) {
	c := Config{
		Addr:       DefaultAddr,
		DB:         DefaultDB,
		TokensFile: getenv("HERALDO_TOKENS_FILE"),
		Tokens:     getenv("HERALDO_TOKENS"),
	}
	if v := getenv("HERALDO_ADDR"); v != "" {
		c.Addr = v
	}
	if v := getenv("HERALDO_DB"); v != "" {
		c.DB = v
	}
	if _, _, err := net.SplitHostPort(c.Addr); err != nil {
		return Config{}, fmt.Errorf("HERALDO_ADDR %q: %w", c.Addr, err)
	}
	if c.TokensFile != "" && c.Tokens != "" {
		return Config{}, errors.New("set HERALDO_TOKENS_FILE or HERALDO_TOKENS, not both")
	}
	return c, nil
}

// LoadTokens parses the configured token list. No list gives an empty set,
// which refuses every authenticated call.
func (c Config) LoadTokens() (*auth.Set, error) {
	if c.TokensFile != "" {
		f, err := os.Open(c.TokensFile)
		if err != nil {
			return nil, fmt.Errorf("HERALDO_TOKENS_FILE: %w", err)
		}
		defer f.Close()
		s, err := auth.Parse(f, '\n')
		if err != nil {
			return nil, fmt.Errorf("HERALDO_TOKENS_FILE: %w", err)
		}
		return s, nil
	}
	s, err := auth.Parse(strings.NewReader(c.Tokens), ',')
	if err != nil {
		return nil, fmt.Errorf("HERALDO_TOKENS: %w", err)
	}
	return s, nil
}

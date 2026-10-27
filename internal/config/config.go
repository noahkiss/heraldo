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
	// RulesFile is the ingest rules file; empty means no rules.
	// HERALDO_RULES.
	RulesFile string
	// Push turns APNs pushes on. Off by default, so a server with a key
	// still sends nothing until it is set. HERALDO_PUSH.
	Push bool
	// APNsKeyFile is the APNs auth key, a .p8 file. HERALDO_APNS_KEY_FILE.
	APNsKeyFile string
	// APNsKeyID is the key's 10-character id. HERALDO_APNS_KEY_ID.
	APNsKeyID string
	// APNsTeamID is the Apple developer team id. HERALDO_APNS_TEAM_ID.
	APNsTeamID string
}

// FromEnv builds a Config from getenv, normally os.Getenv.
func FromEnv(getenv func(string) string) (Config, error) {
	c := Config{
		Addr:       DefaultAddr,
		DB:         DefaultDB,
		TokensFile: getenv("HERALDO_TOKENS_FILE"),
		Tokens:     getenv("HERALDO_TOKENS"),
		RulesFile:  getenv("HERALDO_RULES"),

		APNsKeyFile: getenv("HERALDO_APNS_KEY_FILE"),
		APNsKeyID:   getenv("HERALDO_APNS_KEY_ID"),
		APNsTeamID:  getenv("HERALDO_APNS_TEAM_ID"),
	}
	switch strings.ToLower(getenv("HERALDO_PUSH")) {
	case "", "off", "false", "0":
	case "on", "true", "1":
		c.Push = true
	default:
		return Config{}, fmt.Errorf("HERALDO_PUSH %q: want on or off", getenv("HERALDO_PUSH"))
	}
	if n := countSet(c.APNsKeyFile, c.APNsKeyID, c.APNsTeamID); n != 0 && n != 3 {
		return Config{}, errors.New("set all of HERALDO_APNS_KEY_FILE, HERALDO_APNS_KEY_ID and HERALDO_APNS_TEAM_ID, or none")
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

func countSet(vals ...string) int {
	n := 0
	for _, v := range vals {
		if v != "" {
			n++
		}
	}
	return n
}

// HasAPNsKey reports whether an APNs key is configured.
func (c Config) HasAPNsKey() bool { return c.APNsKeyFile != "" }

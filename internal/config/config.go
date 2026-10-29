// Package config reads the server configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/noahkiss/heraldo/internal/auth"
	"github.com/noahkiss/heraldo/internal/hold"
)

// DefaultAddr is the listen address when HERALDO_ADDR is unset.
const DefaultAddr = ":7842"

// DefaultDB is the database path when HERALDO_DB is unset.
const DefaultDB = "heraldo.db"

// Hold timers when HERALDO_HOLD_DEADLINE and HERALDO_HOLD_DEADMAN are unset.
const (
	DefaultHoldDeadline = 30 * time.Minute
	DefaultHoldDeadman  = 20 * time.Minute
)

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
	// APNsKey is the APNs auth key inline, the .p8 file's PEM text.
	// HERALDO_APNS_KEY. Set this or APNsKeyFile, not both.
	APNsKey string
	// APNsKeyID is the key's 10-character id. HERALDO_APNS_KEY_ID.
	APNsKeyID string
	// APNsTeamID is the Apple developer team id. HERALDO_APNS_TEAM_ID.
	APNsTeamID string
	// NotifyTrustedNets are the networks whose senders may post to the
	// Apprise compatibility paths with no credential. Empty means none.
	// HERALDO_NOTIFY_TRUSTED_NETS, CIDRs separated by commas.
	NotifyTrustedNets []netip.Prefix
	// HoldMode is the alert hold's mode. HERALDO_HOLD, default off.
	HoldMode hold.Mode
	// HoldBypassFile is the hold's bypass list. HERALDO_HOLD_BYPASS_FILE.
	HoldBypassFile string
	// HoldDeadline is how long an alert waits for triage before it is
	// forwarded as it is. HERALDO_HOLD_DEADLINE.
	HoldDeadline time.Duration
	// HoldDeadman is how long the hold waits for a triage poll before it
	// stops holding. HERALDO_HOLD_DEADMAN.
	HoldDeadman time.Duration

	// Standby runs the server as the standby of PrimaryURL.
	// HERALDO_ROLE=standby; the default role is primary.
	Standby bool
	// PrimaryURL is the primary's root. HERALDO_PRIMARY_URL.
	PrimaryURL string
	// PrimaryToken is the standby's token on the primary, a read token with
	// the standby capability. HERALDO_PRIMARY_TOKEN.
	PrimaryToken string
	// StandbyInterval is the gap between the standby's rounds.
	// HERALDO_STANDBY_INTERVAL, a Go duration; default 1m.
	StandbyInterval time.Duration
	// PrimaryDownAfter is how long the primary may go unanswered before the
	// standby raises its alert. HERALDO_PRIMARY_DOWN_AFTER; default 10m.
	PrimaryDownAfter time.Duration

	// SecretKeyFile holds the key that encrypts secrets written through
	// the settings API. Empty refuses those writes. HERALDO_SECRET_KEY_FILE.
	SecretKeyFile string
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
		APNsKey:     getenv("HERALDO_APNS_KEY"),
		APNsKeyID:   getenv("HERALDO_APNS_KEY_ID"),
		APNsTeamID:  getenv("HERALDO_APNS_TEAM_ID"),

		HoldBypassFile: getenv("HERALDO_HOLD_BYPASS_FILE"),
		SecretKeyFile:  getenv("HERALDO_SECRET_KEY_FILE"),
		HoldDeadline:   DefaultHoldDeadline,
		HoldDeadman:    DefaultHoldDeadman,
	}
	var err error
	if c.HoldMode, err = hold.ParseMode(getenv("HERALDO_HOLD")); err != nil {
		return Config{}, fmt.Errorf("HERALDO_HOLD: %w", err)
	}
	for _, d := range []struct {
		name string
		dst  *time.Duration
	}{{"HERALDO_HOLD_DEADLINE", &c.HoldDeadline}, {"HERALDO_HOLD_DEADMAN", &c.HoldDeadman}} {
		v := getenv(d.name)
		if v == "" {
			continue
		}
		if *d.dst, err = time.ParseDuration(v); err != nil || *d.dst < time.Minute {
			return Config{}, fmt.Errorf("%s %q: want a Go duration of a minute or more, such as 30m", d.name, v)
		}
	}
	for _, f := range strings.Split(getenv("HERALDO_NOTIFY_TRUSTED_NETS"), ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		p, err := netip.ParsePrefix(f)
		if err != nil {
			return Config{}, fmt.Errorf("HERALDO_NOTIFY_TRUSTED_NETS: %q is not a CIDR such as 192.0.2.0/24", f)
		}
		c.NotifyTrustedNets = append(c.NotifyTrustedNets, p.Masked())
	}
	switch strings.ToLower(getenv("HERALDO_PUSH")) {
	case "", "off", "false", "0":
	case "on", "true", "1":
		c.Push = true
	default:
		return Config{}, fmt.Errorf("HERALDO_PUSH %q: want on or off", getenv("HERALDO_PUSH"))
	}
	if c.APNsKeyFile != "" && c.APNsKey != "" {
		return Config{}, errors.New("set HERALDO_APNS_KEY_FILE or HERALDO_APNS_KEY, not both")
	}
	if n := countSet(c.APNsKeyFile+c.APNsKey, c.APNsKeyID, c.APNsTeamID); n != 0 && n != 3 {
		return Config{}, errors.New("set an APNs key (HERALDO_APNS_KEY_FILE or HERALDO_APNS_KEY), HERALDO_APNS_KEY_ID and HERALDO_APNS_TEAM_ID, or none")
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
	if err := c.standby(getenv); err != nil {
		return Config{}, err
	}
	return c, nil
}

// standby reads the standby settings.
func (c *Config) standby(getenv func(string) string) error {
	switch strings.ToLower(getenv("HERALDO_ROLE")) {
	case "", "primary":
	case "standby":
		c.Standby = true
	default:
		return fmt.Errorf("HERALDO_ROLE %q: want primary or standby", getenv("HERALDO_ROLE"))
	}
	c.PrimaryURL, c.PrimaryToken = getenv("HERALDO_PRIMARY_URL"), getenv("HERALDO_PRIMARY_TOKEN")
	if !c.Standby {
		return nil
	}
	if c.PrimaryURL == "" || c.PrimaryToken == "" {
		return errors.New("HERALDO_ROLE=standby needs HERALDO_PRIMARY_URL and HERALDO_PRIMARY_TOKEN")
	}
	for _, d := range []struct {
		name string
		to   *time.Duration
	}{{"HERALDO_STANDBY_INTERVAL", &c.StandbyInterval}, {"HERALDO_PRIMARY_DOWN_AFTER", &c.PrimaryDownAfter}} {
		v := getenv(d.name)
		if v == "" {
			continue
		}
		n, err := time.ParseDuration(v)
		if err != nil || n <= 0 {
			return fmt.Errorf("%s %q: want a duration such as 10m", d.name, v)
		}
		*d.to = n
	}
	return nil
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
func (c Config) HasAPNsKey() bool { return c.APNsKeyFile != "" || c.APNsKey != "" }

// LoadAPNsKey returns the APNs key's PEM text, read from APNsKeyFile or
// taken from APNsKey. An inline key with no line breaks but literal "\n"
// sequences, as a single-line secret store gives it, has them turned back
// into line breaks.
func (c Config) LoadAPNsKey() ([]byte, error) {
	if c.APNsKeyFile != "" {
		b, err := os.ReadFile(c.APNsKeyFile)
		if err != nil {
			return nil, fmt.Errorf("HERALDO_APNS_KEY_FILE: %w", err)
		}
		return b, nil
	}
	k := c.APNsKey
	if !strings.Contains(k, "\n") {
		k = strings.ReplaceAll(k, `\n`, "\n")
	}
	return []byte(k), nil
}

// Package config reads the server configuration from the environment.
package config

import (
	"fmt"
	"net"
)

// DefaultAddr is the listen address when HERALDO_ADDR is unset.
const DefaultAddr = ":7842"

// Config is the server configuration.
type Config struct {
	// Addr is the TCP listen address, host:port. HERALDO_ADDR.
	Addr string
}

// FromEnv builds a Config from getenv, normally os.Getenv.
func FromEnv(getenv func(string) string) (Config, error) {
	c := Config{Addr: DefaultAddr}
	if v := getenv("HERALDO_ADDR"); v != "" {
		c.Addr = v
	}
	if _, _, err := net.SplitHostPort(c.Addr); err != nil {
		return Config{}, fmt.Errorf("HERALDO_ADDR %q: %w", c.Addr, err)
	}
	return c, nil
}

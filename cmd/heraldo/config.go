package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The CLI's settings. Each comes from the environment first, then from the
// env file. Nothing has a built-in default that names a server.
const (
	envURL        = "HERALDO_URL"         // server root
	envToken      = "HERALDO_TOKEN"       // read token
	envWriteToken = "HERALDO_WRITE_TOKEN" // write token, for ack and resolve
	envAdminToken = "HERALDO_ADMIN_TOKEN" // admin token, for settings writes; default the write token
	envCursor     = "HERALDO_CURSOR"      // read cursor name; default: inbox, shared
	envSource     = "HERALDO_SOURCE"      // source of ack and resolve events
	envProject    = "HERALDO_PROJECT"     // send and mcp: the project, when --project is not given
	envSendURL    = "HERALDO_SEND_URL"    // send: the notify URL; falls back to APPRISE_URL
	envAppriseURL = "APPRISE_URL"
	envExtraJSON  = "NOTIFY_EXTRA_JSON"
	envFallback   = "HERALDO_FALLBACK_URL"   // send: a heraldo server to post to when the notify URL fails
	envFallbackTk = "HERALDO_FALLBACK_TOKEN" // send: a write token for the fallback
	envEnvFile    = "HERALDO_ENV_FILE"
)

// configDir is $XDG_CONFIG_HOME/heraldo, or ~/.config/heraldo.
func (a *app) configDir() string {
	if d := a.getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "heraldo")
	}
	if h := a.getenv("HOME"); h != "" {
		return filepath.Join(h, ".config", "heraldo")
	}
	return ""
}

// loadEnvFile reads KEY=VALUE lines from HERALDO_ENV_FILE, or the env file in
// the config directory. A missing default file is fine; a missing named file
// is an error.
func (a *app) loadEnvFile() error {
	path := a.getenv(envEnvFile)
	named := path != ""
	if !named {
		if d := a.configDir(); d != "" {
			path = filepath.Join(d, "env")
		}
	}
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) && !named {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	vals, err := parseEnvFile(f.Name(), bufio.NewScanner(f))
	if err != nil {
		return err
	}
	a.file = vals
	return nil
}

func parseEnvFile(name string, sc *bufio.Scanner) (map[string]string, error) {
	vals := map[string]string{}
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("%s:%d: want KEY=VALUE", name, n)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		vals[k] = v
	}
	return vals, sc.Err()
}

// setting returns a setting from the environment, else from the env file.
func (a *app) setting(key string) string {
	if v := a.getenv(key); v != "" {
		return v
	}
	return a.file[key]
}

// defaultCursor is the shared read cursor: one unread state for every
// machine and app that reads it.
const defaultCursor = "inbox"

// cursorName is the name of the read cursor on the server.
func (a *app) cursorName() string {
	if name := a.setting(envCursor); name != "" {
		return name
	}
	return defaultCursor
}

// execAllowFile lists the commands tail --exec may run, one absolute path a
// line.
func (a *app) execAllowFile() string {
	if d := a.configDir(); d != "" {
		return filepath.Join(d, "exec-allow")
	}
	return ""
}

// allowedCommand resolves cmd and reports whether the allowlist names it.
// Both sides are compared after symlinks are resolved.
func (a *app) allowedCommand(cmd string) (string, error) {
	path := a.execAllowFile()
	if path == "" {
		return "", fmt.Errorf("--exec needs an allowlist, and there is no config directory")
	}
	resolved, err := resolveCommand(a.lookPath, cmd)
	if err != nil {
		return "", fmt.Errorf("--exec %s: %w", cmd, err)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("--exec needs an allowlist in %s: %w", path, err)
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil {
		return "", err
	} else if fi.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf("--exec: %s is writable by group or others; chmod go-w it", path)
	}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "~/") {
			line = filepath.Join(a.getenv("HOME"), line[2:])
		}
		if !filepath.IsAbs(line) {
			continue
		}
		if r, err := filepath.EvalSymlinks(line); err == nil && r == resolved {
			return resolved, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("--exec %s: not in the allowlist %s", cmd, path)
}

func resolveCommand(lookPath func(string) (string, error), cmd string) (string, error) {
	p := cmd
	if !strings.Contains(cmd, string(filepath.Separator)) {
		var err error
		if p, err = lookPath(cmd); err != nil {
			return "", err
		}
	}
	p, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

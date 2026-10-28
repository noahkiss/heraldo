// Package hold holds the alert hold's configuration: its mode and the
// bypass list. The hold itself, its state and its endpoints are in the
// server (docs/api.md, "Hold").
package hold

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Mode is what the hold does with an alert that no bypass rule matches.
type Mode string

// Modes.
const (
	// Off holds nothing and records nothing.
	Off Mode = "off"
	// Shadow pushes the alert as usual and records it in the hold.
	Shadow Mode = "shadow"
	// On stores the alert held, so it does not push, until triage closes it
	// or its deadline passes.
	On Mode = "on"
)

// ParseMode reads a mode; empty is Off.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.ToLower(strings.TrimSpace(s))); m {
	case "":
		return Off, nil
	case Off, Shadow, On:
		return m, nil
	}
	return "", fmt.Errorf("hold mode %q: want off, shadow or on", s)
}

// Rule is one bypass rule. It matches when every pattern it sets matches.
type Rule struct {
	Tag   string
	Title *regexp.Regexp
	Body  *regexp.Regexp
}

// ruleSpec is a rule as the file writes it: a JSON object with tag,
// title_regex and body_regex, and why as a comment.
type ruleSpec struct {
	Tag   *string `json:"tag"`
	Title *string `json:"title_regex"`
	Body  *string `json:"body_regex"`
	Why   string  `json:"why"`
}

// Parse reads a bypass file: a JSON list of rules. A rule with no tag and
// no pattern is skipped.
func Parse(raw []byte) ([]Rule, error) {
	var specs []ruleSpec
	if err := json.Unmarshal(raw, &specs); err != nil {
		return nil, err
	}
	var out []Rule
	for i, sp := range specs {
		var r Rule
		if sp.Tag != nil {
			r.Tag = strings.ToLower(strings.TrimSpace(*sp.Tag))
		}
		for _, p := range []struct {
			src *string
			dst **regexp.Regexp
		}{{sp.Title, &r.Title}, {sp.Body, &r.Body}} {
			if p.src == nil {
				continue
			}
			re, err := regexp.Compile(*p.src)
			if err != nil {
				return nil, fmt.Errorf("rule %d: %w", i+1, err)
			}
			*p.dst = re
		}
		if r.Tag != "" || r.Title != nil || r.Body != nil {
			out = append(out, r)
		}
	}
	return out, nil
}

// Matches reports whether the rule lets a post with this tag, title and
// body bypass the hold.
func (r Rule) Matches(tag, title, body string) bool {
	if r.Tag != "" && r.Tag != strings.ToLower(tag) {
		return false
	}
	if r.Title != nil && !r.Title.MatchString(title) {
		return false
	}
	if r.Body != nil && !r.Body.MatchString(body) {
		return false
	}
	return true
}

// Bypass is the bypass list from one file, reloaded when the file's
// modification time changes. A file that does not parse keeps the rules in
// force, so a typo cannot start holding what the list exists to let
// through. A missing file means no rules. It is safe for concurrent use.
type Bypass struct {
	path  string
	log   *slog.Logger
	mu    sync.Mutex
	mtime time.Time
	gone  bool
	rules []Rule
}

// NewBypass loads path. An empty path means no rules. A file that exists
// but does not parse is an error, so a server never starts on one.
func NewBypass(path string, log *slog.Logger) (*Bypass, error) {
	if log == nil {
		log = slog.Default()
	}
	b := &Bypass{path: path, log: log}
	if path == "" {
		return b, nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		log.Warn("hold: the bypass file is missing; no alert bypasses the hold", "file", path)
		b.gone = true
		return b, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if b.rules, err = Parse(raw); err != nil {
		return nil, err
	}
	b.mtime = fi.ModTime()
	log.Info("hold: loaded bypass rules", "rules", len(b.rules))
	return b, nil
}

// Rules returns the rules in force, reloading the file first if it changed.
func (b *Bypass) Rules() []Rule {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.path == "" {
		return b.rules
	}
	fi, err := os.Stat(b.path)
	if err != nil {
		if !b.gone {
			b.log.Warn("hold: the bypass file is missing; no alert bypasses the hold", "file", b.path)
		}
		b.gone, b.rules, b.mtime = true, nil, time.Time{}
		return nil
	}
	if !b.gone && fi.ModTime().Equal(b.mtime) {
		return b.rules
	}
	b.gone, b.mtime = false, fi.ModTime()
	raw, err := os.ReadFile(b.path)
	if err == nil {
		var rules []Rule
		if rules, err = Parse(raw); err == nil {
			b.rules = rules
			b.log.Info("hold: loaded bypass rules", "rules", len(rules))
			return b.rules
		}
	}
	b.log.Error("hold: the bypass file does not load; keeping the rules in force", "err", err)
	return b.rules
}

// Bypassed reports whether any rule lets the post through.
func (b *Bypass) Bypassed(tag, title, body string) bool {
	for _, r := range b.Rules() {
		if r.Matches(tag, title, body) {
			return true
		}
	}
	return false
}

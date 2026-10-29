// Package settings is the server's settings store: a typed registry of
// known keys, each with a built-in default, an env alias and a
// heraldo-server flag alias, and values the operator writes through the API
// into the settings table.
//
// One precedence order, highest first: launch flag, env var, settings
// table, built-in default. A key set by a flag or env var is locked: a write
// through the API answers 409 locked_by_env and names the variable.
//
// A secret key (llm.token) has no flag. Its env forms are HERALDO_LLM_TOKEN
// and HERALDO_LLM_TOKEN_FILE. Written through the API, it is stored
// encrypted with AES-256-GCM under the key in HERALDO_SECRET_KEY_FILE;
// without that file the write is refused. Reads report only whether it is
// set.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Type is a key's value type.
type Type string

// Types. In Go, Bool is bool, Int is int, Duration is time.Duration, List
// is []string, and every other type is string.
const (
	Bool     Type = "bool"
	String   Type = "string"
	URL      Type = "url"      // http or https, or empty
	Int      Type = "int"      // 0 or more
	Duration Type = "duration" // a Go duration above zero, such as 72h
	Clock    Type = "clock"    // a time of day, HH:MM
	TZ       Type = "tz"       // an IANA time zone, such as Europe/Paris
	Enum     Type = "enum"     // one of Key.Enum
	List     Type = "list"     // strings; a comma-separated list in env and flags
	Secret   Type = "secret"   // a string that is never read back
)

// Key is one known setting.
type Key struct {
	Name string
	Type Type
	// Default is the built-in value, in the Go type of Type.
	Default any
	// Enum lists the values of an Enum key.
	Enum []string
	// Regex marks a List whose items are regular expressions.
	Regex bool
	// Desc says what the key does, in one line.
	Desc string
}

// IsSecret reports whether the key holds a secret.
func (k Key) IsSecret() bool { return k.Type == Secret }

// Env is the key's env alias: llm.base_url is HERALDO_LLM_BASE_URL.
func (k Key) Env() string {
	return "HERALDO_" + strings.ToUpper(strings.NewReplacer(".", "_", "-", "_").Replace(k.Name))
}

// EnvFile is the env alias that names a file holding a secret's value, such
// as HERALDO_LLM_TOKEN_FILE. Empty for a key that is not secret.
func (k Key) EnvFile() string {
	if !k.IsSecret() {
		return ""
	}
	return k.Env() + "_FILE"
}

// Flag is the key's heraldo-server flag name without dashes:
// llm.base_url is llm-base-url. A secret key has no flag: empty.
func (k Key) Flag() string {
	if k.IsSecret() {
		return ""
	}
	return flagName(k.Name)
}

func flagName(name string) string {
	return strings.NewReplacer(".", "-", "_", "-").Replace(name)
}

var levels = []string{"passive", "active", "time-sensitive", "critical"}

// DefaultRedact catches bearer-shaped strings, op:// references and long
// hex before text leaves the server.
var DefaultRedact = []string{
	`(?i)bearer\s+[A-Za-z0-9._~+/=-]{8,}`,
	`op://[^\s"']+`,
	`\b[0-9a-fA-F]{32,}\b`,
}

// registry is every known key. Every feature is off by default.
var registry = []Key{
	// The LLM. It runs on the primary only and never on a direct push.
	{Name: "llm.mode", Type: Enum, Default: "off", Enum: []string{"off", "shadow", "advise", "enforce"},
		Desc: "LLM decisions: off, shadow (record only), advise (show), or enforce (apply within the floors)."},
	{Name: "llm.chat", Type: Bool, Default: false, Desc: "Allow questions to the chat model."},
	{Name: "llm.base_url", Type: URL, Default: "", Desc: "The OpenAI-compatible endpoint's root, such as https://llm.example.net/v1."},
	{Name: "llm.api", Type: Enum, Default: "openai-chat", Enum: []string{"openai-chat", "openai-responses"},
		Desc: "The endpoint's API: chat completions or responses."},
	{Name: "llm.token", Type: Secret, Default: "", Desc: "The endpoint's API token. Write-only."},
	{Name: "llm.chat_model", Type: String, Default: "", Desc: "The model for chat and summaries."},
	{Name: "llm.decisions_model", Type: String, Default: "", Desc: "The model for triage decisions."},
	{Name: "llm.chat_timeout", Type: Duration, Default: 60 * time.Second, Desc: "How long a chat call may take."},
	{Name: "llm.decisions_timeout", Type: Duration, Default: 20 * time.Second, Desc: "How long a decisions call may take."},
	{Name: "llm.tokens_per_day", Type: Int, Default: 200000, Desc: "Daily token budget; 0 stops every call."},
	{Name: "llm.calls_per_minute", Type: Int, Default: 10, Desc: "Call cap per minute; 0 stops every call."},
	{Name: "llm.send_body_chars", Type: Int, Default: 0, Desc: "How many characters of an event's body go to the LLM; 0 sends none."},
	{Name: "llm.redact", Type: List, Default: DefaultRedact, Regex: true,
		Desc: "Regular expressions whose matches are removed before text goes to the LLM."},
	{Name: "llm.exclude_projects", Type: List, Default: []string{}, Desc: "Projects whose events never go to the LLM."},
	{Name: "llm.hold_may_suppress", Type: Bool, Default: false,
		Desc: "Let the LLM fold held blips into one passive summary."},

	// Quiet hours: below the floor, pushes go passive.
	{Name: "quiet.enabled", Type: Bool, Default: false, Desc: "Quiet hours on."},
	{Name: "quiet.start", Type: Clock, Default: "22:00", Desc: "When quiet hours start, HH:MM."},
	{Name: "quiet.end", Type: Clock, Default: "07:00", Desc: "When quiet hours end, HH:MM."},
	{Name: "quiet.tz", Type: TZ, Default: "UTC", Desc: "The time zone of quiet.start and quiet.end."},
	{Name: "quiet.floor", Type: Enum, Default: "time-sensitive", Enum: levels,
		Desc: "The lowest level that still interrupts during quiet hours."},

	// The morning digest.
	{Name: "digest.enabled", Type: Bool, Default: false, Desc: "Send a daily digest."},
	{Name: "digest.at", Type: Clock, Default: "07:30", Desc: "When the digest is sent, HH:MM."},
	{Name: "digest.tz", Type: TZ, Default: "UTC", Desc: "The time zone of digest.at."},

	// The stale sweep.
	{Name: "stale.enabled", Type: Bool, Default: false, Desc: "Flag incidents open too long."},
	{Name: "stale.after", Type: Duration, Default: 72 * time.Hour, Desc: "How long an incident stays open before it is stale."},

	// Flap grouping.
	{Name: "flap.enabled", Type: Bool, Default: false, Desc: "Group an incident that opens again and again."},
	{Name: "flap.opens", Type: Int, Default: 3, Desc: "Opens within flap.window that mark an incident flapping."},
	{Name: "flap.window", Type: Duration, Default: time.Hour, Desc: "The window flap.opens counts over."},
	{Name: "flap.settle", Type: Duration, Default: 30 * time.Minute, Desc: "How long a flapping incident stays quiet before it is not flapping."},
}

var byName = func() map[string]Key {
	m := make(map[string]Key, len(registry))
	for _, k := range registry {
		if _, dup := m[k.Name]; dup {
			panic("settings: key listed twice: " + k.Name)
		}
		if _, err := k.check(k.Default); err != nil {
			panic(fmt.Sprintf("settings: %s: bad default: %v", k.Name, err))
		}
		m[k.Name] = k
	}
	return m
}()

// Keys returns every known key, sorted by name.
func Keys() []Key {
	out := slices.Clone(registry)
	slices.SortFunc(out, func(a, b Key) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Lookup returns the key with name.
func Lookup(name string) (Key, bool) {
	k, ok := byName[name]
	return k, ok
}

// ErrUnknown is a key the registry does not list.
var ErrUnknown = errors.New("unknown setting")

const (
	maxString = 1024
	maxItems  = 64
	maxSecret = 8192
)

// ParseText reads a value as an env var or a flag gives it.
func (k Key) ParseText(s string) (any, error) {
	var v any
	switch k.Type {
	case Bool:
		b, err := strconv.ParseBool(strings.TrimSpace(s))
		if err != nil {
			return nil, errors.New("want true or false")
		}
		v = b
	case Int:
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			return nil, errors.New("want a whole number")
		}
		v = n
	case Duration:
		d, err := time.ParseDuration(strings.TrimSpace(s))
		if err != nil {
			return nil, errors.New("want a duration such as 30m or 72h")
		}
		v = d
	case List:
		items := []string{}
		for _, f := range strings.Split(s, ",") {
			if f = strings.TrimSpace(f); f != "" {
				items = append(items, f)
			}
		}
		v = items
	default:
		v = strings.TrimSpace(s)
	}
	return k.check(v)
}

// ParseJSON reads a value as the API and the settings table give it.
func (k Key) ParseJSON(raw json.RawMessage) (any, error) {
	var v any
	var err error
	switch k.Type {
	case Bool:
		var b bool
		if err = json.Unmarshal(raw, &b); err != nil {
			err = errors.New("want true or false")
		}
		v = b
	case Int:
		var n int
		if err = json.Unmarshal(raw, &n); err != nil {
			err = errors.New("want a whole number")
		}
		v = n
	case Duration:
		var s string
		if err = json.Unmarshal(raw, &s); err != nil {
			return nil, errors.New("want a duration string such as \"72h\"")
		}
		d, perr := time.ParseDuration(s)
		if perr != nil {
			return nil, errors.New("want a duration string such as \"72h\"")
		}
		v = d
	case List:
		var l []string
		if err = json.Unmarshal(raw, &l); err != nil || l == nil {
			return nil, errors.New("want an array of strings")
		}
		v = l
	default:
		var s string
		if err = json.Unmarshal(raw, &s); err != nil {
			err = errors.New("want a string")
		}
		v = s
	}
	if err != nil {
		return nil, err
	}
	return k.check(v)
}

// check validates v, in the key's Go type, and returns it.
func (k Key) check(v any) (any, error) {
	switch k.Type {
	case Bool:
		if _, ok := v.(bool); !ok {
			return nil, errors.New("want a bool")
		}
	case Int:
		n, ok := v.(int)
		if !ok {
			return nil, errors.New("want a whole number")
		}
		if n < 0 {
			return nil, errors.New("want 0 or more")
		}
	case Duration:
		d, ok := v.(time.Duration)
		if !ok || d <= 0 {
			return nil, errors.New("want a duration above zero, such as 30m")
		}
	case List:
		l, ok := v.([]string)
		if !ok {
			return nil, errors.New("want a list of strings")
		}
		if len(l) > maxItems {
			return nil, fmt.Errorf("at most %d items", maxItems)
		}
		for _, s := range l {
			if s == "" || len(s) > 256 {
				return nil, errors.New("each item is 1 to 256 bytes")
			}
			if k.Regex {
				if _, err := regexp.Compile(s); err != nil {
					return nil, fmt.Errorf("%q is not a regular expression", s)
				}
			}
		}
	default:
		s, ok := v.(string)
		if !ok {
			return nil, errors.New("want a string")
		}
		if max := maxString; len(s) > max && k.Type != Secret {
			return nil, fmt.Errorf("at most %d bytes", max)
		}
		switch k.Type {
		case Secret:
			if len(s) > maxSecret {
				return nil, fmt.Errorf("at most %d bytes", maxSecret)
			}
		case URL:
			if s == "" {
				break
			}
			u, err := url.Parse(s)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return nil, errors.New("want an http or https URL")
			}
			if u.User != nil {
				return nil, errors.New("the URL may not carry a user or password; use the token setting")
			}
		case Clock:
			if _, err := time.Parse("15:04", s); err != nil || len(s) != 5 {
				return nil, errors.New("want a time of day, HH:MM")
			}
		case TZ:
			if s == "" || s == "Local" {
				return nil, errors.New("want an IANA time zone such as UTC or Europe/Paris")
			}
			if _, err := time.LoadLocation(s); err != nil {
				return nil, errors.New("want an IANA time zone such as UTC or Europe/Paris")
			}
		case Enum:
			if !slices.Contains(k.Enum, s) {
				return nil, fmt.Errorf("want one of %s", strings.Join(k.Enum, ", "))
			}
		}
	}
	return v, nil
}

// JSONValue is v as the API shows it: a duration as a string such as "72h".
func (k Key) JSONValue(v any) any {
	if d, ok := v.(time.Duration); ok {
		return fmtDuration(d)
	}
	return v
}

// fmtDuration is d without zero tails: 72h, not 72h0m0s.
func fmtDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

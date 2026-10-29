package llm

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Redacted replaces each match of an llm.redact pattern.
const Redacted = "[redacted]"

// Policy says what of an event may leave the server: llm.send_body_chars,
// llm.redact and llm.exclude_projects. The zero Policy sends no body,
// redacts nothing and excludes nothing.
type Policy struct {
	// BodyChars is how many characters of a body go; 0 sends none.
	BodyChars int
	Redact    []*regexp.Regexp
	// Exclude lists projects, folded to lower case, whose events never go.
	Exclude map[string]bool
}

// NewPolicy builds a Policy. A pattern that does not compile is skipped:
// the settings store checks them on write.
func NewPolicy(bodyChars int, redact, exclude []string) Policy {
	p := Policy{BodyChars: max(bodyChars, 0), Exclude: map[string]bool{}}
	for _, r := range redact {
		if re, err := regexp.Compile(r); err == nil {
			p.Redact = append(p.Redact, re)
		}
	}
	for _, e := range exclude {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			p.Exclude[e] = true
		}
	}
	return p
}

// Excluded reports whether events of project never go to the LLM.
func (p Policy) Excluded(project string) bool {
	return project != "" && p.Exclude[strings.ToLower(project)]
}

// RedactText removes every match of the redact patterns from s.
func (p Policy) RedactText(s string) string {
	for _, re := range p.Redact {
		s = re.ReplaceAllString(s, Redacted)
	}
	return s
}

// Body is body cut to BodyChars characters and redacted; empty when
// BodyChars is 0.
func (p Policy) Body(body string) string {
	if p.BodyChars <= 0 || body == "" {
		return ""
	}
	if utf8.RuneCountInString(body) > p.BodyChars {
		body = string([]rune(body)[:p.BodyChars]) + "…"
	}
	return p.RedactText(body)
}

// dropKeys never leave the server: an event's details, its image, icon and
// link URLs, attachments, devices and hold client ids.
var dropKeys = map[string]bool{
	"details": true, "image": true, "icon": true, "url": true, "attachments": true, "devices": true,
	"client_id": true, "token": true,
}

// ulid matches an event id.
var ulid = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

// SanitizeJSON applies p to a tool's JSON result before it goes to the
// model: an object whose project is excluded is dropped from a list, or
// replaced by {"excluded": true}; body is cut to BodyChars; details, URLs,
// attachments and devices are removed; every string is redacted. It returns
// the result and the event ids it kept, which are the ids an answer may
// cite.
func (p Policy) SanitizeJSON(raw []byte) ([]byte, []string, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, nil, err
	}
	var ids []string
	out, _ := p.walk(v, &ids)
	b, err := json.Marshal(out)
	return b, ids, err
}

func (p Policy) walk(v any, ids *[]string) (any, bool) {
	switch t := v.(type) {
	case map[string]any:
		if proj, ok := t["project"].(string); ok && p.Excluded(proj) {
			return map[string]any{"excluded": true}, false
		}
		out := make(map[string]any, len(t))
		fromBody, _ := t["summary_from_body"].(bool)
		for k, val := range t {
			if dropKeys[k] || k == "summary_from_body" {
				continue
			}
			if k == "summary" && fromBody {
				// The summary is the body's first line: it follows the
				// body's policy.
				if s, ok := val.(string); ok {
					if b := p.Body(s); b != "" {
						out[k] = b
					}
				}
				continue
			}
			if k == "body" {
				if s, ok := val.(string); ok {
					if b := p.Body(s); b != "" {
						out[k] = b
					}
					continue
				}
			}
			if k == "id" {
				if s, ok := val.(string); ok && ulid.MatchString(s) {
					*ids = append(*ids, s)
				}
			}
			w, _ := p.walk(val, ids)
			out[k] = w
		}
		return out, true
	case []any:
		out := make([]any, 0, len(t))
		for _, val := range t {
			if w, keep := p.walk(val, ids); keep {
				out = append(out, w)
			}
		}
		return out, true
	case string:
		return p.RedactText(t), true
	default:
		return v, true
	}
}

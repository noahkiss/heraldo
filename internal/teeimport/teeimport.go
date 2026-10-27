// Package teeimport backfills events from an apprise-tee catalogue: a
// directory of monthly JSON Lines files, notifications-YYYY-MM.jsonl, one
// notification per line.
//
// Each entry is an untyped event: the ingest rules, when given, type it as
// they would a live post (package rules), and an entry no rule matches is
// stored as kind message.
//
//	tee field              event field
//	id                     details.tee.id and dedup; the event id is derived from ts and id.
//	                       A line without one gets a legacy id (see LegacyID).
//	ts (epoch seconds)     ts and received_at
//	source                 source ("apprise-tee" when the entry has none)
//	title, body            title, body (cut to the envelope limits)
//	tag                    tags (split on commas and spaces)
//	url, image             url, image (when they are valid URIs)
//	level                  level when it is an envelope level, else details.tee.level
//	hold, hold_id, ...     hold {state, id, reason, ids, store_only: true}
//	client, path, status,  details.tee
//	delivered, ms, backends
package teeimport

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/ids"
	"github.com/noahkiss/heraldo/internal/rules"
	"github.com/noahkiss/heraldo/internal/store"
	"github.com/noahkiss/heraldo/internal/validate"
)

// Origin names this import in the store's imports table.
const Origin = "tee"

// DefaultSource is the source of an entry that has none: entries written
// before the tee learned sources.
const DefaultSource = "apprise-tee"

// LegacyPrefix starts every legacy id. The tee's own ids start with "n-",
// so the two never collide.
const LegacyPrefix = "legacy-"

// maxLine is the longest catalogue line read. The tee caps request bodies at
// 1 MiB; JSON escaping can grow that.
const maxLine = 8 << 20

// Stats counts what an import did.
type Stats struct {
	Files     int
	Lines     int
	Imported  int
	Skipped   int // imported by an earlier run
	Bad       int // not JSON, no ts, or not a valid envelope
	LegacyIDs int // mapped lines that had no id and got a legacy id
	Typed     int // mapped lines a rule classified
}

// LegacyID is the tee id of a catalogue line that has none: the tee wrote
// lines without ids before it learned them. It is LegacyPrefix and the first
// 20 hex characters of the SHA-256 of the line, without its line ending, so a
// rerun over the same files makes the same id.
func LegacyID(line []byte) string {
	h := sha256.Sum256(bytes.TrimRight(line, "\r\n"))
	return LegacyPrefix + hex.EncodeToString(h[:])[:20]
}

// Run imports every catalogue file in dir, classifying each entry with rs
// (nil: no rules).
func Run(ctx context.Context, dir string, st *store.Store, v *validate.Validator, rs *rules.Set, log *slog.Logger) (Stats, error) {
	var s Stats
	files, err := Each(dir, func(file string, n int, line []byte) error {
		s.Lines++
		m, err := Classify(line, v, rs)
		if err != nil {
			s.Bad++
			log.Warn("tee import: skipped a line", "file", file, "line", n, "err", err)
			return nil
		}
		if m.RuleErr != nil {
			log.Warn("tee import: a rule matched but its event is not valid; stored as a message",
				"file", file, "line", n, "err", m.RuleErr)
		}
		if m.Rule != "" {
			s.Typed++
		}
		if strings.HasPrefix(m.TeeID, LegacyPrefix) {
			s.LegacyIDs++
		}
		created, err := st.Import(ctx, Origin, m.TeeID, m.Event)
		if err != nil {
			return fmt.Errorf("line %d: %w", n, err)
		}
		if created {
			s.Imported++
		} else {
			s.Skipped++
		}
		return nil
	})
	s.Files = files
	return s, err
}

// Each calls fn for every non-blank line of every catalogue file in dir,
// files in name order (oldest month first). file is the base name and n the
// line number. An error from fn stops the walk. It returns the number of
// files read.
func Each(dir string, fn func(file string, n int, line []byte) error) (int, error) {
	files, err := filepath.Glob(filepath.Join(dir, "notifications-*.jsonl"))
	if err != nil {
		return 0, err
	}
	if len(files) == 0 {
		return 0, fmt.Errorf("no notifications-*.jsonl files in %s", dir)
	}
	sort.Strings(files)
	for i, path := range files {
		if err := eachLine(path, fn); err != nil {
			return i + 1, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
	}
	return len(files), nil
}

func eachLine(path string, fn func(file string, n int, line []byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), maxLine)
	n := 0
	for sc.Scan() {
		n++
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if err := fn(filepath.Base(path), n, line); err != nil {
			return err
		}
	}
	return sc.Err()
}

// entry is a catalogue line. Unknown fields are ignored.
type entry struct {
	ID         string          `json:"id"`
	Ts         *float64        `json:"ts"`
	Time       string          `json:"time"`
	Client     string          `json:"client"`
	Path       string          `json:"path"`
	Tag        json.RawMessage `json:"tag"`
	Title      *string         `json:"title"`
	Body       *string         `json:"body"`
	Status     any             `json:"status"`
	Delivered  *bool           `json:"delivered"`
	Ms         *int64          `json:"ms"`
	Source     *string         `json:"source"`
	URL        *string         `json:"url"`
	Image      *string         `json:"image"`
	Level      *string         `json:"level"`
	Backends   any             `json:"backends"`
	Hold       *string         `json:"hold"`
	HoldID     *string         `json:"hold_id"`
	HoldReason *string         `json:"hold_reason"`
	HoldIDs    any             `json:"hold_ids"`
}

// Map turns one catalogue line into an event ready to store, and returns the
// tee id it came from: the line's id, or its LegacyID when it has none. It
// applies no rules, so the event is kind message.
func Map(line []byte, v *validate.Validator) (store.NewEvent, string, error) {
	m, err := Classify(line, v, nil)
	return m.Event, m.TeeID, err
}

// Mapped is one catalogue line as an event.
type Mapped struct {
	Event store.NewEvent
	// TeeID is the line's id, or its LegacyID when it has none.
	TeeID string
	// Rule names the rule that classified the event, or is empty.
	Rule string
	// RuleErr is set when a rule matched but the event it made was not a
	// valid envelope; the event is then the plain message.
	RuleErr error
}

// Classify maps one catalogue line as Map does, then classifies it with rs
// (nil: no rules).
func Classify(line []byte, v *validate.Validator, rs *rules.Set) (Mapped, error) {
	var e entry
	if err := json.Unmarshal(line, &e); err != nil {
		return Mapped{}, fmt.Errorf("not a JSON object: %w", err)
	}
	if e.ID == "" {
		e.ID = LegacyID(line)
	}
	if e.Ts == nil || math.IsNaN(*e.Ts) || *e.Ts <= 0 {
		return Mapped{}, fmt.Errorf("%s: no ts", e.ID)
	}
	sec, frac := math.Modf(*e.Ts)
	at := time.Unix(int64(sec), int64(math.Round(frac*1000))*int64(time.Millisecond)).UTC()

	in := map[string]any{
		"source": DefaultSource,
		"ts":     at.Format(time.RFC3339Nano),
		// The tee forwards live events with dedup = its catalogue id, so
		// a record stored live and then backfilled is stored once.
		"dedup": cut(e.ID, 128),
	}
	if e.Source != nil && *e.Source != "" {
		in["source"] = cut(*e.Source, 128)
	}
	if e.Title != nil && *e.Title != "" {
		in["title"] = cut(*e.Title, 256)
	}
	if e.Body != nil && *e.Body != "" {
		in["body"] = cutBytes(*e.Body, validate.MaxBodyBytes)
	}
	if tags := splitTags(e.Tag); len(tags) > 0 {
		in["tags"] = tags
	}
	if e.URL != nil && isURI(*e.URL) {
		in["url"] = *e.URL
	}
	if e.Image != nil && isURI(*e.Image) {
		in["image"] = *e.Image
	}
	tee := map[string]any{"id": e.ID}
	if e.Level != nil && *e.Level != "" {
		if api.Level(*e.Level).Valid() {
			in["level"] = *e.Level
		} else {
			tee["level"] = *e.Level
		}
	}
	for k, x := range map[string]any{
		"time": e.Time, "client": e.Client, "path": e.Path, "status": e.Status,
		"delivered": e.Delivered, "ms": e.Ms, "backends": e.Backends,
	} {
		if !empty(x) {
			tee[k] = x
		}
	}
	in["details"] = map[string]any{"tee": tee}

	raw, err := json.Marshal(in)
	if err != nil {
		return Mapped{}, err
	}
	checked, err := v.Input(raw)
	if err != nil {
		return Mapped{}, fmt.Errorf("%s: %w", e.ID, err)
	}
	m := Mapped{TeeID: e.ID}
	typed, rule, ruleErr := rules.Type(rs, checked, v)
	if typed == nil {
		return Mapped{}, fmt.Errorf("%s: %w", e.ID, ruleErr)
	}
	checked, m.Rule, m.RuleErr = typed, rule, ruleErr
	id := e.ID
	if !ids.Valid(id) {
		id = ids.Derived(at, Origin, e.ID)
	}
	fields := checked.Fields
	fields["id"] = id
	fields["received_at"] = at.Format(time.RFC3339Nano)
	if e.Hold != nil && *e.Hold != "" && *e.Hold != "none" {
		// Imported events are history: never pushed, like a live held one.
		hold := map[string]any{"state": *e.Hold, "store_only": true}
		if e.HoldID != nil {
			hold["id"] = *e.HoldID
		}
		if e.HoldReason != nil {
			hold["reason"] = *e.HoldReason
		}
		if !empty(e.HoldIDs) {
			hold["ids"] = e.HoldIDs
		}
		fields["hold"] = hold
	}
	m.Event = store.NewEvent{ID: id, ReceivedAt: at, Fields: fields, Typed: checked.Typed}
	return m, nil
}

func empty(x any) bool {
	switch t := x.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case *bool:
		return t == nil
	case *int64:
		return t == nil
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

// splitTags reads the tee's tag: a string of Apprise tags separated by
// commas or spaces, a list, or null.
func splitTags(raw json.RawMessage) []any {
	var parts []string
	var one string
	var many []string
	switch {
	case json.Unmarshal(raw, &one) == nil:
		parts = strings.FieldsFunc(one, func(r rune) bool { return r == ',' || r == ' ' })
	case json.Unmarshal(raw, &many) == nil:
		parts = many
	}
	seen := map[string]bool{}
	var out []any
	for _, p := range parts {
		p = cut(strings.TrimSpace(p), 64)
		if p == "" || seen[p] || len(out) == 16 {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

func isURI(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme != ""
}

// cut shortens s to n characters.
func cut(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// cutBytes shortens s to at most n bytes without splitting a character.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

package push

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/noahkiss/heraldo/api"
)

// MaxPayload is the largest alert payload APNs takes, in bytes.
const MaxPayload = 4096

// Decision is whether an event pushes an alert, and how loud.
type Decision struct {
	Push bool
	// Reason says why an event does not push.
	Reason string
	// Level is the interruption level the push carries.
	Level api.Level
	// Downgraded is set when the event asked for critical and the push
	// carries time-sensitive: the app has no critical-alerts entitlement.
	Downgraded bool
	// Replace is set on a passive push. It goes out only to replace an
	// earlier visible push of the event's key, by collapse id; the pusher
	// checks that. It never counts toward the badge.
	Replace bool
}

// Decide applies the push table to a stored event at time now.
//
//	kind      action                  push   level
//	message   -                       yes    the event's (default active)
//	digest    -                       yes    passive
//	incident  open, update, none      yes    the event's
//	incident  resolve                 yes    passive
//	incident  ack                     no
//	checkin   fail, or exit_code != 0 yes    the event's
//	checkin   start, success, log     no
//	progress  any                     no     (Live Activities, stage 7)
//	status    any                     no     (widgets)
//
// Before the table: a store-only or held event, and one past expires_at,
// never pushes. After it: a passive event is inbox-only, like Pushover's
// quiet priority. It pushes only when it has a key, as a Replace decision:
// the pusher sends it when an earlier push of that key was visible, so the
// open alert on the lock screen gives way to "Resolved after 12 min".
// critical goes out as time-sensitive. store.pushedWhere is the same table
// in SQL, for the badge; keep them together.
//
// The pusher runs the same steps as the stages hold, expiry and table
// (stages.go); Decide composes them for callers that want one answer.
func Decide(ev *api.Event, now time.Time) Decision {
	if why := holdStops(ev); why != "" {
		return Decision{Reason: why}
	}
	if expired(ev, now) {
		return Decision{Reason: "expired"}
	}
	return decideTable(ev)
}

// holdStops returns store-only or held when the hold keeps ev from
// pushing, else "".
func holdStops(ev *api.Event) string {
	if ev.Hold != nil {
		h := *ev.Hold
		if so, _ := h["store_only"].(bool); so {
			return "store-only"
		}
		// A held alert the hold forwarded at once (forwarded: true) pushes.
		fwd, _ := h["forwarded"].(bool)
		if st, _ := h["state"].(string); st == "held" && !fwd {
			return "held"
		}
	}
	return ""
}

// expired reports whether ev is past its expires_at at now.
func expired(ev *api.Event, now time.Time) bool {
	return ev.ExpiresAt != nil && !ev.ExpiresAt.After(now)
}

// decideTable applies the push table alone: Decide without the hold and
// expiry checks.
func decideTable(ev *api.Event) Decision {
	action := ""
	if ev.Action != nil {
		action = string(*ev.Action)
	}
	level := api.LevelActive
	if ev.Level != nil {
		level = *ev.Level
	}
	switch ev.Kind {
	case api.KindMessage:
	case api.KindDigest:
		level = api.LevelPassive
	case api.KindIncident:
		switch action {
		case "ack":
			return Decision{Reason: "incident ack"}
		case "resolve":
			level = api.LevelPassive
		}
	case api.KindCheckin:
		failed := action == "fail" ||
			(action != "log" && ev.Checkin != nil && ev.Checkin.ExitCode != nil && *ev.Checkin.ExitCode != 0)
		if !failed {
			return Decision{Reason: "checkin " + orNone(action)}
		}
	default:
		return Decision{Reason: string(ev.Kind) + " sends no alert"}
	}
	if level == api.LevelPassive {
		if ev.Key == nil || *ev.Key == "" {
			return Decision{Reason: "passive"}
		}
		return Decision{Push: true, Level: level, Replace: true}
	}
	d := Decision{Push: true, Level: level}
	if level == api.LevelCritical {
		d.Level, d.Downgraded = api.LevelTimeSensitive, true
	}
	return d
}

func orNone(s string) string {
	if s == "" {
		return "without action"
	}
	return s
}

// Payload is an APNs alert payload.
type Payload struct {
	APS     APS     `json:"aps"`
	Heraldo Heraldo `json:"heraldo"`
}

// APS is the aps dictionary.
type APS struct {
	Alert             Alert    `json:"alert"`
	Badge             *int     `json:"badge,omitempty"`
	Sound             string   `json:"sound,omitempty"`
	ThreadID          string   `json:"thread-id,omitempty"`
	Category          string   `json:"category,omitempty"`
	MutableContent    int      `json:"mutable-content"`
	InterruptionLevel string   `json:"interruption-level"`
	RelevanceScore    *float32 `json:"relevance-score,omitempty"`
}

// Alert is the visible text.
type Alert struct {
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
}

// Heraldo is what the app and its notification service extension need to
// fetch the full event.
type Heraldo struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Source string `json:"source"`
	Key    string `json:"key,omitempty"`
	Action string `json:"action,omitempty"`
	// Project is the event's project, so the extension can file it.
	Project string `json:"project,omitempty"`
	URL     string `json:"url,omitempty"`
	Image   string `json:"image,omitempty"`
	// Icon is the name of the source's icon in the server's registry, for
	// GET /v1/icons/{name}. IconURL is an icon the event links to instead.
	Icon    string `json:"icon,omitempty"`
	IconURL string `json:"icon_url,omitempty"`
	// Truncated is set when the alert does not carry the full text: the
	// body was cut to fit, or the alert shows the summary of a longer body.
	// The extension fetches the event by id for the rest.
	Truncated bool `json:"truncated,omitempty"`
}

// Categories the app registers: incident has Ack, Mute 1 h and Open; event
// has Mute 1 h and Open.
const (
	CategoryIncident = "incident"
	CategoryEvent    = "event"
)

// Headers are the APNs request headers that depend on the event.
type Headers struct {
	CollapseID string
	Priority   int
	Expiration int64
	// PushType is apns-push-type; empty means alert.
	PushType string
	// Topic replaces the device's topic as apns-topic when set.
	Topic string
}

// CollapseID returns the apns-collapse-id for a key: the first 32 hex
// characters of its SHA-256. APNs allows 64 bytes; a key may be 255.
func CollapseID(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:16])
}

// Extra is what a push carries beyond the event.
type Extra struct {
	// Badge nil leaves the badge alone.
	Badge *int
	// Icon is the registry icon to show, by name.
	Icon string
	// OpenFor, on a re-alert, is how long the incident has been open.
	OpenFor time.Duration
	// Flap, on a flapping incident, leads the text with its opens.
	Flap *FlapNote
}

// Build makes the payload and headers for ev with decision d. The payload
// fits MaxPayload: the alert body is cut at a character boundary, with an
// ellipsis, and Truncated is set.
//
// Some kinds get their own text. A resolve says how long the incident was
// open ("Resolved after 12 min") and, sent with the same collapse id,
// replaces the incident's notification. A re-alert starts with how long the
// incident has been open. A digest shows its summary, else the first line of
// its body.
func Build(ev *api.Event, d Decision, x Extra) ([]byte, Headers, error) {
	p := Payload{
		APS: APS{
			Badge:             x.Badge,
			ThreadID:          ev.Source,
			Category:          CategoryEvent,
			MutableContent:    1,
			InterruptionLevel: string(d.Level),
			RelevanceScore:    ev.Relevance,
		},
		Heraldo: Heraldo{ID: ev.Id, Kind: string(ev.Kind), Source: ev.Source},
	}
	if ev.Thread != nil && *ev.Thread != "" {
		p.APS.ThreadID = *ev.Thread
	}
	if ev.Kind == api.KindIncident && (ev.Action == nil || *ev.Action == api.EventActionOpen || *ev.Action == api.EventActionUpdate) {
		p.APS.Category = CategoryIncident
	}
	if ev.Key != nil {
		p.Heraldo.Key = *ev.Key
	}
	if ev.Project != nil {
		p.Heraldo.Project = *ev.Project
	}
	if ev.Action != nil {
		p.Heraldo.Action = string(*ev.Action)
	}
	if ev.Url != nil {
		p.Heraldo.URL = *ev.Url
	}
	if ev.Image != nil {
		p.Heraldo.Image = *ev.Image
	}
	p.Heraldo.Icon = x.Icon
	if ev.Icon != nil && strings.HasPrefix(*ev.Icon, "https://") {
		p.Heraldo.IconURL = *ev.Icon
	}
	p.APS.Alert.Title = ev.Source
	if ev.Title != nil && *ev.Title != "" {
		p.APS.Alert.Title = *ev.Title
	}
	text := ""
	switch {
	case ev.Summary != nil && *ev.Summary != "":
		text = *ev.Summary
		p.Heraldo.Truncated = ev.Body != nil && *ev.Body != ""
	case ev.Body != nil:
		text = *ev.Body
	}
	switch {
	case ev.Kind == api.KindIncident && ev.Action != nil && *ev.Action == api.EventActionResolve:
		text = ResolvedText(ev)
		p.Heraldo.Truncated = false
	case ev.Kind == api.KindDigest && (ev.Summary == nil || *ev.Summary == "") && ev.Body != nil:
		text = FirstLine(*ev.Body)
		p.Heraldo.Truncated = text != strings.TrimSpace(*ev.Body)
	}
	if x.OpenFor > 0 {
		lead := "Still open after " + Duration(x.OpenFor)
		if text == "" {
			text = lead
		} else {
			text = lead + ". " + text
		}
	}
	if x.Flap != nil && x.Flap.Count > 0 {
		lead := fmt.Sprintf("Flapping: %d opens", x.Flap.Count)
		if x.Flap.For > 0 {
			lead += " in " + Duration(x.Flap.For)
		}
		if text == "" {
			text = lead
		} else {
			text = lead + ". " + text
		}
	}
	if d.Level != api.LevelPassive {
		p.APS.Sound = "default"
		if ev.Sound != nil {
			p.APS.Sound = *ev.Sound
		}
		if p.APS.Sound == "none" {
			p.APS.Sound = ""
		}
	}

	h := Headers{Priority: 10}
	if d.Level == api.LevelPassive {
		h.Priority = 5
	}
	if ev.Key != nil && *ev.Key != "" {
		h.CollapseID = CollapseID(*ev.Key)
	}
	if ev.ExpiresAt != nil {
		h.Expiration = ev.ExpiresAt.Unix()
	}

	p.APS.Alert.Body = text
	b, err := json.Marshal(p)
	if err != nil || len(b) <= MaxPayload {
		return b, h, err
	}
	// Find the longest prefix of text that fits, by bytes, on a character
	// boundary. JSON escaping makes the size not linear in the cut.
	p.Heraldo.Truncated = true
	lo, hi := 0, len(text)
	var best []byte
	for lo <= hi {
		mid := (lo + hi) / 2
		cut := mid
		for cut > 0 && cut < len(text) && !utf8.RuneStart(text[cut]) {
			cut--
		}
		p.APS.Alert.Body = text[:cut] + "…"
		b, err := json.Marshal(p)
		if err != nil {
			return nil, h, err
		}
		if len(b) <= MaxPayload {
			best = b
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	if best == nil {
		p.APS.Alert.Body = ""
		if best, err = json.Marshal(p); err != nil {
			return nil, h, err
		}
	}
	return best, h, nil
}

// ResolvedText is the alert text of an incident resolve: "Resolved after
// 12 min", from the incident snapshot the server stores on the event. A
// resolve with no open before it says "Resolved".
func ResolvedText(ev *api.Event) string {
	if inc := ev.Incident; inc != nil && inc.ResolvedAt != nil && inc.ResolvedAt.After(inc.OpenedAt) {
		return "Resolved after " + Duration(inc.ResolvedAt.Sub(inc.OpenedAt))
	}
	return "Resolved"
}

// Duration says d for a person, to the minute: "under a minute", "12 min",
// "2 h 5 min", "3 d 4 h".
func Duration(d time.Duration) string {
	m := int(d / time.Minute)
	switch {
	case m < 1:
		return "under a minute"
	case m < 60:
		return fmt.Sprintf("%d min", m)
	case m < 24*60:
		if m%60 == 0 {
			return fmt.Sprintf("%d h", m/60)
		}
		return fmt.Sprintf("%d h %d min", m/60, m%60)
	default:
		h := m / 60
		if h%24 == 0 {
			return fmt.Sprintf("%d d", h/24)
		}
		return fmt.Sprintf("%d d %d h", h/24, h%24)
	}
}

// FirstLine is the first line of markdown text with words in it, without
// its heading, list or quote marker, cut to the summary limit.
func FirstLine(md string) string {
	for _, line := range strings.Split(md, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
		for _, mark := range []string{"> ", "- ", "* ", "+ "} {
			line = strings.TrimSpace(strings.TrimPrefix(line, mark))
		}
		if line == "" {
			continue
		}
		if r := []rune(line); len(r) > 178 {
			return string(r[:177]) + "…"
		}
		return line
	}
	return ""
}

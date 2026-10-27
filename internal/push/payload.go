package push

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
// never pushes. critical goes out as time-sensitive. store.pushedWhere is
// the same table in SQL; keep them together.
func Decide(ev *api.Event, now time.Time) Decision {
	if ev.Hold != nil {
		h := *ev.Hold
		if so, _ := h["store_only"].(bool); so {
			return Decision{Reason: "store-only"}
		}
		if st, _ := h["state"].(string); st == "held" {
			return Decision{Reason: "held"}
		}
	}
	if ev.ExpiresAt != nil && !ev.ExpiresAt.After(now) {
		return Decision{Reason: "expired"}
	}
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
	URL    string `json:"url,omitempty"`
	Image  string `json:"image,omitempty"`
	// Truncated is set when the alert does not carry the full text: the
	// body was cut to fit, or the alert shows the summary of a longer body.
	// The extension fetches the event by id for the rest.
	Truncated bool `json:"truncated,omitempty"`
}

// Categories the app registers: incident has Ack, Mute and Open; event has
// Mute and Open.
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

// Build makes the payload and headers for ev with decision d. badge nil
// leaves the badge alone. The payload fits MaxPayload: the alert body is cut
// at a character boundary, with an ellipsis, and Truncated is set.
func Build(ev *api.Event, d Decision, badge *int) ([]byte, Headers, error) {
	p := Payload{
		APS: APS{
			Badge:             badge,
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
	if ev.Action != nil {
		p.Heraldo.Action = string(*ev.Action)
	}
	if ev.Url != nil {
		p.Heraldo.URL = *ev.Url
	}
	if ev.Image != nil {
		p.Heraldo.Image = *ev.Image
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

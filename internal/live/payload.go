package live

import (
	"encoding/json"
	"time"
)

// AttributesType is the Swift type name of the app's ActivityAttributes,
// sent as attributes-type on a push-to-start. HeraldoKit's
// ProgressAttributes must keep this name.
const AttributesType = "ProgressAttributes"

// Attributes are an activity's fixed attributes. The field names match
// ProgressAttributes in HeraldoKit.
type Attributes struct {
	Key      string `json:"key"`
	Instance string `json:"instance"`
	Source   string `json:"source"`
}

// apsLive is the aps dictionary of a liveactivity push.
type apsLive struct {
	Timestamp      int64          `json:"timestamp"`
	Event          string         `json:"event"`
	ContentState   map[string]any `json:"content-state"`
	StaleDate      int64          `json:"stale-date,omitempty"`
	DismissalDate  int64          `json:"dismissal-date,omitempty"`
	AttributesType string         `json:"attributes-type,omitempty"`
	Attributes     *Attributes    `json:"attributes,omitempty"`
	Alert          *liveAlert     `json:"alert,omitempty"`
	InputPushToken int            `json:"input-push-token,omitempty"`
	// InputPushChannel subscribes the started activity to a broadcast
	// channel (iOS 18) in place of asking for an update token.
	InputPushChannel string `json:"input-push-channel,omitempty"`
}

type liveAlert struct {
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	Sound string `json:"sound,omitempty"`
}

// payload is one liveactivity push body.
type payload struct {
	event string // start, update or end
	now   time.Time
	// stamp replaces now as the timestamp when set.
	stamp     int64
	content   map[string]any
	stale     time.Time
	dismissal time.Time
	attrs     *Attributes
	alert     *liveAlert
	// channel, on a start, is the channel the activity listens on; empty
	// asks for an update token.
	channel string
}

func (p payload) marshal() ([]byte, error) {
	a := apsLive{
		Timestamp:    p.now.Unix(),
		Event:        p.event,
		ContentState: ContentState(p.content),
		Alert:        p.alert,
	}
	if p.stamp != 0 {
		a.Timestamp = p.stamp
	}
	if !p.stale.IsZero() {
		a.StaleDate = p.stale.Unix()
	}
	if !p.dismissal.IsZero() {
		// 0 would mean "keep it for the system's default"; a dismissal
		// date at or before now removes it at once.
		a.DismissalDate = max(p.dismissal.Unix(), 1)
	}
	if p.event == "start" {
		a.AttributesType = AttributesType
		a.Attributes = p.attrs
		if p.channel != "" {
			// iOS 18: listen for updates on the channel.
			a.InputPushChannel = p.channel
		} else {
			// iOS 18: ask the device to send this activity's update token.
			a.InputPushToken = 1
		}
	}
	return json.Marshal(map[string]any{"aps": a})
}

// stateFields are the LiveContent fields the content state carries. stale_after
// and keep_for steer the server and stay out.
var stateFields = []string{"title", "symbol", "tint", "body", "progress", "steps", "trailing", "status", "metrics", "button"}

// ContentState turns merged LiveContent into the content-state the app
// decodes: the same field names, with ends_at as Unix seconds instead of
// RFC 3339 text, because ActivityKit decodes the state without the app's
// date strategy.
func ContentState(content map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range stateFields {
		if v, ok := content[k]; ok && v != nil {
			out[k] = v
		}
	}
	if s, ok := content["ends_at"].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			out["ends_at"] = t.Unix()
		}
	}
	return out
}

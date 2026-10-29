package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/auth"
	"github.com/noahkiss/heraldo/internal/ids"
	"github.com/noahkiss/heraldo/internal/store"
)

// DeviceScope is the token scope the device calls need: read, as for the
// read cursors. The app's read token registers its device.
var DeviceScope = auth.ScopeRead

var apnsToken = regexp.MustCompile(`^[0-9A-Fa-f]{32,200}$`)

// permissionSource is the source of the incidents that permission health
// raises.
const permissionSource = "heraldo"

// permissions, in the order incidents are raised, with what each one is
// called in an incident's title.
var permissions = []struct {
	name, label string
	get         func(*store.Device) *bool
}{
	{"alerts", "notifications", func(d *store.Device) *bool { return d.Alerts }},
	{"time_sensitive", "time-sensitive notifications", func(d *store.Device) *bool { return d.TimeSensitive }},
	{"live_activities", "Live Activities", func(d *store.Device) *bool { return d.LiveActivities }},
	{"critical_alerts", "critical alerts", func(d *store.Device) *bool { return d.CriticalAlerts }},
}

func deviceJSON(d store.Device) api.Device {
	out := api.Device{
		Id:           d.ID,
		Environment:  api.DeviceEnvironment(d.Environment),
		Topic:        d.Topic,
		TokenSuffix:  d.Token[max(0, len(d.Token)-8):],
		Active:       d.Active(),
		RegisteredAt: d.RegisteredAt,
		UpdatedAt:    d.UpdatedAt,
		Permissions: api.Permissions{
			Alerts: d.Alerts, TimeSensitive: d.TimeSensitive, LiveActivities: d.LiveActivities,
			CriticalAlerts: d.CriticalAlerts,
		},
	}
	str := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	out.Name, out.Platform, out.AppVersion = str(d.Name), str(d.Platform), str(d.AppVersion)
	if t := d.LiveActivityToken; t != "" {
		out.LiveActivityTokenSuffix = str(t[max(0, len(t)-8):])
	}
	out.FocusFilter = focusJSON(d.FocusFilter)
	if !d.Active() {
		t := d.RetiredAt
		out.RetiredAt, out.RetiredReason = &t, str(d.RetiredReason)
	}
	return out
}

// readDevice decodes and checks a DeviceRegistration.
func (s *server) readDevice(w http.ResponseWriter, r *http.Request) (store.Device, bool) {
	var in api.DeviceRegistration
	dec := json.NewDecoder(io.LimitReader(r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "want a DeviceRegistration: %v", err)
		return store.Device{}, false
	}
	var problems []string
	if !apnsToken.MatchString(in.Token) {
		problems = append(problems, "token: want 32 to 200 hex characters")
	}
	switch in.Environment {
	case api.DeviceRegistrationEnvironmentSandbox, api.DeviceRegistrationEnvironmentProduction:
	default:
		problems = append(problems, "environment: want sandbox or production")
	}
	if in.Topic == "" || len(in.Topic) > 255 || strings.ContainsAny(in.Topic, " /\t\r\n") {
		problems = append(problems, "topic: want the app's bundle id, 1 to 255 characters")
	}
	opt := func(name string, v *string, limit int) string {
		if v == nil {
			return ""
		}
		if len(*v) > limit {
			problems = append(problems, fmt.Sprintf("%s: at most %d bytes", name, limit))
		}
		return *v
	}
	d := store.Device{
		Token:       strings.ToLower(in.Token),
		Environment: string(in.Environment),
		Topic:       in.Topic,
		Name:        opt("name", in.Name, 128),
		Platform:    opt("platform", in.Platform, 32),
		AppVersion:  opt("app_version", in.AppVersion, 64),
		UpdatedBy:   tokenFrom(r).Name,
	}
	if in.LiveActivityToken != nil {
		if liveToken.MatchString(*in.LiveActivityToken) {
			d.LiveActivityToken = strings.ToLower(*in.LiveActivityToken)
		} else {
			problems = append(problems, "live_activity_token: want 32 to 400 hex characters")
		}
	}
	if in.Permissions != nil {
		d.Alerts, d.TimeSensitive, d.LiveActivities = in.Permissions.Alerts, in.Permissions.TimeSensitive, in.Permissions.LiveActivities
		d.CriticalAlerts = in.Permissions.CriticalAlerts
	}
	if in.FocusFilter != nil {
		f, bad := focusFrom(in.FocusFilter)
		d.FocusFilter, d.SetFocus = f, true
		problems = append(problems, bad...)
	}
	if len(problems) > 0 {
		writeError(w, http.StatusBadRequest, "%s", strings.Join(problems, "; "))
		return store.Device{}, false
	}
	return d, true
}

func (s *server) listDevices(w http.ResponseWriter, r *http.Request) {
	ds, err := s.Store.Devices(r.Context(), false)
	if err != nil {
		s.Logger.Error("devices", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := api.DeviceList{Devices: make([]api.Device, 0, len(ds))}
	for _, d := range ds {
		out.Devices = append(out.Devices, deviceJSON(d))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) getDevice(w http.ResponseWriter, r *http.Request) {
	d, err := s.Store.GetDevice(r.Context(), r.PathValue("id"))
	if s.deviceErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, deviceJSON(d))
}

// registerDevice is POST /v1/devices: create by token, or update the device
// that holds it.
func (s *server) registerDevice(w http.ResponseWriter, r *http.Request) {
	d, ok := s.readDevice(w, r)
	if !ok {
		return
	}
	got, old, created, err := s.Store.RegisterDevice(r.Context(), d, "dev_"+ids.New(s.Now()))
	if s.deviceErr(w, err) {
		return
	}
	s.permissionHealth(r.Context(), old, got)
	s.liveTokenChanged(old, got)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, deviceJSON(got))
}

func (s *server) updateDevice(w http.ResponseWriter, r *http.Request) {
	d, ok := s.readDevice(w, r)
	if !ok {
		return
	}
	d.ID = r.PathValue("id")
	got, old, err := s.Store.UpdateDevice(r.Context(), d)
	if s.deviceErr(w, err) {
		return
	}
	s.permissionHealth(r.Context(), &old, got)
	s.liveTokenChanged(&old, got)
	writeJSON(w, http.StatusOK, deviceJSON(got))
}

// liveTokenChanged lets running activities start on a device that reported
// a new push-to-start token.
func (s *server) liveTokenChanged(old *store.Device, cur store.Device) {
	if s.Live == nil || cur.LiveActivityToken == "" || (old != nil && old.Active() && old.LiveActivityToken == cur.LiveActivityToken) {
		return
	}
	s.Live.Enqueue(s.Live.Resync)
}

// removeDevice retires the device. Its open permission incidents resolve:
// nobody can turn the permission back on for a device that is gone.
func (s *server) removeDevice(w http.ResponseWriter, r *http.Request) {
	old, err := s.Store.GetDevice(r.Context(), r.PathValue("id"))
	if s.deviceErr(w, err) {
		return
	}
	got, err := s.Store.RetireDevice(r.Context(), old.ID, "removed")
	if s.deviceErr(w, err) {
		return
	}
	if old.Active() {
		gone := got
		gone.Alerts, gone.TimeSensitive, gone.LiveActivities, gone.CriticalAlerts = nil, nil, nil, nil
		s.permissionHealth(r.Context(), &old, gone)
	}
	writeJSON(w, http.StatusOK, deviceJSON(got))
}

func (s *server) deviceErr(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "no device with that id")
	case errors.Is(err, store.ErrTokenTaken):
		writeError(w, http.StatusConflict, "another device holds that token; register it with POST /v1/devices")
	default:
		s.Logger.Error("device", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
	return true
}

// permissionHealth raises an incident for each permission that turned off
// and resolves the incident of each one that was off and is no longer. old
// is nil for a new device. A retired device's earlier state does not count
// as on or off: re-registering one raises what is off now.
func (s *server) permissionHealth(ctx context.Context, old *store.Device, cur store.Device) {
	for _, p := range permissions {
		was := (*bool)(nil)
		if old != nil && old.Active() {
			was = p.get(old)
		}
		now := p.get(&cur)
		wasOff := was != nil && !*was
		nowOff := now != nil && !*now
		name := cur.Name
		if name == "" {
			name = cur.ID
		}
		key := "device:" + cur.ID + ":" + p.name
		var f map[string]any
		switch {
		case nowOff && !wasOff:
			f = map[string]any{
				"source": permissionSource, "kind": "incident", "action": "open", "key": key, "level": "active",
				"title":   fmt.Sprintf("%s: %s are off", name, p.label),
				"summary": cut(fmt.Sprintf("Heraldo cannot show %s on %s until they are turned on in Settings.", p.label, name), 178),
				"details": map[string]any{"heraldo": map[string]any{"device": cur.ID, "permission": p.name}},
			}
		case wasOff && !nowOff:
			f = map[string]any{
				"source": permissionSource, "kind": "incident", "action": "resolve", "key": key, "level": "passive",
				"title":   fmt.Sprintf("%s: %s are back on", name, p.label),
				"details": map[string]any{"heraldo": map[string]any{"device": cur.ID, "permission": p.name}},
			}
			if !cur.Active() {
				f["title"] = fmt.Sprintf("%s: device removed", name)
			}
		default:
			continue
		}
		in, err := s.Validator.Fields(f)
		if err != nil {
			s.Logger.Error("permission health: event", "err", err)
			continue
		}
		_, _, _ = s.ingest(ctx, in, false)
	}
}

// cut shortens s to at most n characters.
func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

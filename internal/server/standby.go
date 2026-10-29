package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/auth"
	"github.com/noahkiss/heraldo/internal/store"
)

// StandbyOrigin names standby imports in the store's imports table.
const StandbyOrigin = "standby"

// MaxImportBytes caps a POST /v1/standby/import body.
const MaxImportBytes = 64 << 20

// maxImportEvents is the most events one import takes.
const maxImportEvents = 500

func (s *server) standbyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/standby/devices", s.auth(auth.ScopeRead, false, standbyOnly(s.exportDevices)))
	mux.HandleFunc("POST /v1/standby/import", s.auth(auth.ScopeRead, false, standbyOnly(s.importStandby)))
}

// standbyOnly admits an admin token or a token with the standby capability.
// A plain read token never sees a full push token.
func standbyOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t := tokenFrom(r)
		if t.Scope != auth.ScopeAdmin && !t.Has(auth.CapStandby) {
			writeError(w, http.StatusForbidden, "token %q needs scope admin or the %s capability", t.Name, auth.CapStandby)
			return
		}
		h(w, r)
	}
}

// Emit stores and pushes an event the server makes itself, as a sender's
// event would be. A standby raises its primary-down alert with it.
func (s *Server) Emit(ctx context.Context, fields map[string]any) error { return s.s.emit(ctx, fields) }

func (s *server) exportDevices(w http.ResponseWriter, r *http.Request) {
	ds, err := s.Store.Devices(r.Context(), false)
	if err != nil {
		s.Logger.Error("standby devices", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := api.StandbyDeviceList{Devices: make([]api.StandbyDevice, 0, len(ds))}
	for _, d := range ds {
		out.Devices = append(out.Devices, StandbyDeviceJSON(d))
	}
	writeJSON(w, http.StatusOK, out)
}

// StandbyDeviceJSON is d as GET /v1/standby/devices shows it.
func StandbyDeviceJSON(d store.Device) api.StandbyDevice {
	str := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	out := api.StandbyDevice{
		Id: d.ID, Token: d.Token, Environment: api.StandbyDeviceEnvironment(d.Environment), Topic: d.Topic,
		Name: str(d.Name), Platform: str(d.Platform), AppVersion: str(d.AppVersion),
		Permissions: api.Permissions{Alerts: d.Alerts, TimeSensitive: d.TimeSensitive, LiveActivities: d.LiveActivities,
			CriticalAlerts: d.CriticalAlerts},
		RegisteredAt: d.RegisteredAt, UpdatedAt: d.UpdatedAt,
		RetiredReason: str(d.RetiredReason), LiveActivityToken: str(d.LiveActivityToken),
		FocusFilter: focusJSON(d.FocusFilter),
	}
	if !d.Active() {
		t := d.RetiredAt
		out.RetiredAt = &t
	}
	return out
}

// StandbyDevice is a device from GET /v1/standby/devices as the store keeps
// it. updatedBy names who wrote the copy.
func StandbyDevice(d api.StandbyDevice, updatedBy string) store.Device {
	val := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	out := store.Device{
		ID: d.Id, Token: d.Token, Environment: string(d.Environment), Topic: d.Topic,
		Name: val(d.Name), Platform: val(d.Platform), AppVersion: val(d.AppVersion),
		Alerts: d.Permissions.Alerts, TimeSensitive: d.Permissions.TimeSensitive, LiveActivities: d.Permissions.LiveActivities,
		CriticalAlerts: d.Permissions.CriticalAlerts,
		RegisteredAt:   d.RegisteredAt, UpdatedAt: d.UpdatedAt, UpdatedBy: updatedBy,
		RetiredReason: val(d.RetiredReason), LiveActivityToken: val(d.LiveActivityToken),
		FocusFilter: focusCopy(d.FocusFilter),
	}
	if d.RetiredAt != nil {
		out.RetiredAt = *d.RetiredAt
	}
	return out
}

// importStandby is POST /v1/standby/import: store what a standby stored,
// by its own ids, and push none of it.
func (s *server) importStandby(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Events []json.RawMessage `json:"events"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxImportBytes))
	if err := dec.Decode(&in); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body over %d bytes", MaxImportBytes)
			return
		}
		writeError(w, http.StatusBadRequest, "want a StandbyImport: %v", err)
		return
	}
	if len(in.Events) > maxImportEvents {
		writeError(w, http.StatusBadRequest, "events: at most %d per import", maxImportEvents)
		return
	}
	var res api.StandbyImportResult
	for i, raw := range in.Events {
		ev, err := s.importable(raw)
		if err != nil {
			s.Logger.Warn("standby import: event rejected", "index", i, "err", err)
			res.Rejected++
			continue
		}
		created, err := s.Store.Import(r.Context(), StandbyOrigin, ev.ID, ev)
		if errors.Is(err, store.ErrUnknownAttachment) {
			// A standby takes no uploads, so this is an event from another
			// server that names bytes this one never got.
			s.Logger.Warn("standby import: event rejected", "index", i, "err", err)
			res.Rejected++
			continue
		}
		if err != nil {
			s.Logger.Error("standby import", "err", err)
			writeError(w, http.StatusInternalServerError, "could not store the events")
			return
		}
		if created {
			res.Imported++
		} else {
			res.Skipped++
		}
	}
	// Store.Import's OnChange hook has bumped the revisions and woken the
	// streams.
	s.Logger.Info("standby import", "imported", res.Imported, "skipped", res.Skipped, "rejected", res.Rejected)
	writeJSON(w, http.StatusOK, res)
}

// importable turns a stored event back into one the store takes: the
// sender's fields checked again, and the standby's id, received_at and hold
// kept. The store derives incident again.
func (s *server) importable(raw json.RawMessage) (store.NewEvent, error) {
	var f map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&f); err != nil {
		return store.NewEvent{}, err
	}
	id, _ := f["id"].(string)
	if id == "" {
		return store.NewEvent{}, errors.New("no id")
	}
	at, _ := f["received_at"].(string)
	received, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return store.NewEvent{}, errors.New("received_at: not a time")
	}
	hold := f["hold"]
	for _, k := range []string{"id", "received_at", "cursor", "hold", "delivery", "why", "attachment_info", "incident"} {
		delete(f, k)
	}
	in, err := s.Validator.Fields(f)
	if err != nil {
		return store.NewEvent{}, err
	}
	if in.Typed.Kind == nil {
		return store.NewEvent{}, errors.New("no kind")
	}
	in.Fields["id"] = id
	in.Fields["received_at"] = at
	if hold != nil {
		in.Fields["hold"] = hold
	}
	return store.NewEvent{ID: id, ReceivedAt: received, Fields: in.Fields, Typed: in.Typed}, nil
}

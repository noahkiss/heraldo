package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/auth"
	"github.com/noahkiss/heraldo/internal/settings"
)

func (s *server) settingsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/settings", s.auth(auth.ScopeRead, false, s.listSettings))
	mux.HandleFunc("GET /v1/settings/{key}", s.auth(auth.ScopeRead, false, s.getSetting))
	mux.HandleFunc("PUT /v1/settings/{key}", s.auth(auth.ScopeAdmin, false, s.putSetting))
	mux.HandleFunc("DELETE /v1/settings/{key}", s.auth(auth.ScopeAdmin, false, s.deleteSetting))
}

// settingJSON is v as the API shows it. A secret never carries its value.
func settingJSON(v settings.View) api.Setting {
	k := v.Key
	out := api.Setting{
		Key: k.Name, Type: api.SettingType(k.Type), Source: api.SettingSource(v.Source),
		Secret: k.IsSecret(), Description: k.Desc, Env: k.Env(),
	}
	if k.IsSecret() {
		out.Set = ptr(v.Set)
		out.EnvFile = ptr(k.EnvFile())
	} else {
		out.Value = v.Value
		out.Default = k.JSONValue(k.Default)
		out.Flag = ptr("--" + k.Flag())
	}
	if len(k.Enum) > 0 {
		out.Enum = ptr(append([]string(nil), k.Enum...))
	}
	if v.LockedBy != "" {
		out.LockedBy = ptr(v.LockedBy)
	}
	if !v.UpdatedAt.IsZero() {
		out.UpdatedAt, out.UpdatedBy = ptr(v.UpdatedAt), ptr(v.UpdatedBy)
	}
	if v.Error != "" {
		out.Error = ptr(v.Error)
	}
	return out
}

func ptr[T any](v T) *T { return &v }

func (s *server) listSettings(w http.ResponseWriter, r *http.Request) {
	all := s.Settings.All()
	out := api.SettingList{Settings: make([]api.Setting, 0, len(all))}
	for _, v := range all {
		out.Settings = append(out.Settings, settingJSON(v))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) getSetting(w http.ResponseWriter, r *http.Request) {
	v, err := s.Settings.View(r.PathValue("key"))
	if err != nil {
		s.settingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, settingJSON(v))
}

// putSetting is PUT /v1/settings/{key}. It never logs the value: the key
// may be a secret.
func (s *server) putSetting(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if _, ok := settings.Lookup(key); !ok {
		s.settingError(w, settings.ErrUnknown)
		return
	}
	var in struct {
		Value json.RawMessage `json:"value"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 32<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "want a SettingInput, {\"value\": ...}")
		return
	}
	if len(bytes.TrimSpace(in.Value)) == 0 || bytes.Equal(bytes.TrimSpace(in.Value), []byte("null")) {
		writeError(w, http.StatusBadRequest, "give value; DELETE the key to go back to its default")
		return
	}
	who := tokenFrom(r).Name
	v, err := s.Settings.Set(r.Context(), key, in.Value, who)
	if err != nil {
		s.settingError(w, err)
		return
	}
	s.Logger.Info("setting", "key", key, "op", "set", "token", who)
	writeJSON(w, http.StatusOK, settingJSON(v))
}

func (s *server) deleteSetting(w http.ResponseWriter, r *http.Request) {
	key, who := r.PathValue("key"), tokenFrom(r).Name
	v, err := s.Settings.Unset(r.Context(), key, who)
	if err != nil {
		s.settingError(w, err)
		return
	}
	s.Logger.Info("setting", "key", key, "op", "unset", "token", who)
	writeJSON(w, http.StatusOK, settingJSON(v))
}

func (s *server) settingError(w http.ResponseWriter, err error) {
	var le *settings.LockedError
	var ie *settings.InvalidError
	switch {
	case errors.Is(err, settings.ErrUnknown):
		writeError(w, http.StatusNotFound, "unknown setting; GET /v1/settings lists the keys")
	case errors.As(err, &le):
		writeJSON(w, http.StatusConflict, api.SettingError{Code: api.SettingErrorCodeLockedByEnv, Error: le.Error(), Variable: ptr(le.By)})
	case errors.Is(err, settings.ErrNoSecretKey):
		writeJSON(w, http.StatusConflict, api.SettingError{Code: api.SettingErrorCodeNoSecretKey, Error: err.Error()})
	case errors.As(err, &ie):
		writeError(w, http.StatusBadRequest, "%s", ie.Error())
	default:
		s.Logger.Error("setting", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

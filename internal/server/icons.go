package server

import (
	"errors"
	"io"
	"net/http"
	"regexp"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/store"
)

// MaxIconBytes caps one icon image.
const MaxIconBytes = 256 << 10

var iconName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func iconJSON(ic store.Icon) api.IconInfo {
	return api.IconInfo{Name: ic.Name, ContentType: api.IconInfoContentType(ic.ContentType), Bytes: ic.Size,
		Sha256: ic.SHA256, UpdatedAt: ic.UpdatedAt}
}

func (s *server) iconName(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if !iconName.MatchString(name) {
		writeError(w, http.StatusBadRequest, "name: 1 to 128 of A-Z, a-z, 0-9, dot, underscore and dash, starting with a letter or digit")
		return "", false
	}
	return name, true
}

func (s *server) listIcons(w http.ResponseWriter, r *http.Request) {
	ics, err := s.Store.Icons(r.Context())
	if err != nil {
		s.Logger.Error("icons", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := api.IconInfoList{Icons: make([]api.IconInfo, 0, len(ics))}
	for _, ic := range ics {
		out.Icons = append(out.Icons, iconJSON(ic))
	}
	writeJSON(w, http.StatusOK, out)
}

// getIcon is GET /v1/icons/{name}: the image, with its SHA-256 as the ETag.
func (s *server) getIcon(w http.ResponseWriter, r *http.Request) {
	name, ok := s.iconName(w, r)
	if !ok {
		return
	}
	ic, err := s.Store.GetIcon(r.Context(), name)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no icon called %q", name)
		return
	}
	if err != nil {
		s.Logger.Error("icon", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	etag := `"` + ic.SHA256 + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", ic.ContentType)
	_, _ = w.Write(ic.Data)
}

// putIcon is PUT /v1/icons/{name} with the image as the body. The bytes
// decide the type: PNG or JPEG only.
func (s *server) putIcon(w http.ResponseWriter, r *http.Request) {
	name, ok := s.iconName(w, r)
	if !ok {
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxIconBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "an icon is at most %d bytes", MaxIconBytes)
			return
		}
		writeError(w, http.StatusBadRequest, "read body: %v", err)
		return
	}
	if len(data) == 0 {
		writeError(w, http.StatusBadRequest, "the body is the image; it is empty")
		return
	}
	ct := http.DetectContentType(data)
	if ct != "image/png" && ct != "image/jpeg" {
		writeError(w, http.StatusUnsupportedMediaType, "want a PNG or JPEG image, got %s", ct)
		return
	}
	ic := store.Icon{Name: name, ContentType: ct, Data: data, UpdatedAt: s.Now(), UpdatedBy: tokenFrom(r).Name}
	created, err := s.Store.PutIcon(r.Context(), &ic)
	if err != nil {
		s.Logger.Error("put icon", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, iconJSON(ic))
}

func (s *server) deleteIcon(w http.ResponseWriter, r *http.Request) {
	name, ok := s.iconName(w, r)
	if !ok {
		return
	}
	err := s.Store.DeleteIcon(r.Context(), name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "no icon called %q", name)
	case err != nil:
		s.Logger.Error("delete icon", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

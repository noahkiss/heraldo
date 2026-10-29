package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/ids"
	"github.com/noahkiss/heraldo/internal/store"
)

// MaxAttachmentBytes caps one attachment.
const MaxAttachmentBytes = 5 << 20

// multipartSlack is what a multipart upload may add around the file: the
// boundaries and part headers.
const multipartSlack = 64 << 10

// AttachmentScheme is the prefix of an attachment's url: attachment:<id>.
const AttachmentScheme = "attachment:"

var attachmentID = regexp.MustCompile(`^att_[0-9A-HJKMNP-TV-Z]{26}$`)

// errStandbyAttachments is why a standby refuses an upload. An event it
// stores is handed back to the primary without bytes, so it takes none.
const errStandbyAttachments = "this server is a standby; attachments are stored on the primary only. Send the event without attachments, or retry when the primary is back"

// AllowedAttachment reports whether an attachment may have media type ct:
// any image, plain text, PDF or JSON.
func AllowedAttachment(ct string) bool {
	return strings.HasPrefix(ct, "image/") || ct == "text/plain" || ct == "application/pdf" || ct == "application/json"
}

func attachmentJSON(a store.Attachment) api.AttachmentCreated {
	return api.AttachmentCreated{Id: a.ID, Url: AttachmentScheme + a.ID, ContentType: a.ContentType,
		Bytes: a.Size, Sha256: a.SHA256}
}

// createAttachment is POST /v1/attachments: the raw body with its
// Content-Type, or multipart/form-data with the file in the part "file".
func (s *server) createAttachment(w http.ResponseWriter, r *http.Request) {
	if s.Standby {
		writeError(w, http.StatusServiceUnavailable, errStandbyAttachments)
		return
	}
	body := http.MaxBytesReader(w, r.Body, MaxAttachmentBytes+multipartSlack)
	declared := r.Header.Get("Content-Type")
	var data []byte
	var err error
	if mt, _, _ := mime.ParseMediaType(declared); mt == "multipart/form-data" {
		data, declared, err = filePart(r, body)
	} else {
		data, err = io.ReadAll(io.LimitReader(body, MaxAttachmentBytes+1))
	}
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "an attachment is at most %d bytes", MaxAttachmentBytes)
			return
		}
		writeError(w, http.StatusBadRequest, "read body: %v", err)
		return
	}
	if len(data) > MaxAttachmentBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "an attachment is at most %d bytes", MaxAttachmentBytes)
		return
	}
	if len(data) == 0 {
		writeError(w, http.StatusBadRequest, "the body is the file; it is empty")
		return
	}
	ct, err := attachmentType(declared, data)
	if err != nil {
		writeError(w, http.StatusUnsupportedMediaType, "%v", err)
		return
	}
	now := s.Now()
	a := store.Attachment{ID: "att_" + ids.New(now), ContentType: ct, Body: data, CreatedAt: now,
		CreatedBy: tokenFrom(r).Name}
	created, err := s.Store.PutAttachment(r.Context(), &a)
	if err != nil {
		s.Logger.Error("put attachment", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, attachmentJSON(a))
}

// filePart reads the part called file of a multipart body, and its
// Content-Type.
func filePart(r *http.Request, body io.ReadCloser) ([]byte, string, error) {
	r.Body = body
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, "", err
	}
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, "", errors.New("multipart: no part called file")
		}
		if err != nil {
			return nil, "", err
		}
		if p.FormName() != "file" {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(p, MaxAttachmentBytes+1))
		return data, p.Header.Get("Content-Type"), err
	}
}

// attachmentType is the media type to store for data, sent as declared.
// No type, or application/octet-stream, means the bytes decide. An image
// whose bytes read as text, such as SVG, is refused: a browser may run it.
func attachmentType(declared string, data []byte) (string, error) {
	sniffed, _, _ := mime.ParseMediaType(http.DetectContentType(data))
	ct, _, err := mime.ParseMediaType(declared)
	if declared == "" || err != nil || ct == "application/octet-stream" {
		ct = sniffed
	}
	ct = strings.ToLower(ct)
	if !AllowedAttachment(ct) {
		return "", fmt.Errorf("type %s is not allowed; want an image, text/plain, application/pdf or application/json", ct)
	}
	if strings.HasPrefix(ct, "image/") && strings.HasPrefix(sniffed, "text/") {
		return "", fmt.Errorf("type %s: the bytes are text, not an image", ct)
	}
	return ct, nil
}

// getAttachment is GET /v1/attachments/{id}: the bytes, with headers that
// keep a browser from running them.
func (s *server) getAttachment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !attachmentID.MatchString(id) {
		writeError(w, http.StatusBadRequest, "id: want att_ and a ULID")
		return
	}
	a, err := s.Store.GetAttachment(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no attachment with that id")
		return
	}
	if err != nil {
		s.Logger.Error("attachment", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	etag := `"` + a.SHA256 + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	ct := a.ContentType
	if strings.HasPrefix(ct, "text/") || ct == "application/json" {
		ct += "; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	_, _ = w.Write(a.Body)
}

// AttachmentSweepEvery is how often RunAttachmentSweep runs.
const AttachmentSweepEvery = time.Hour

// RunAttachmentSweep deletes detached attachments older than
// store.AttachmentDetachedFor, once at the start and then each interval,
// until ctx ends.
func (srv *Server) RunAttachmentSweep(ctx context.Context, every time.Duration) {
	s := srv.s
	sweep := func() {
		n, err := s.Store.SweepAttachments(ctx, s.Now().Add(-store.AttachmentDetachedFor))
		if err != nil {
			if ctx.Err() == nil {
				s.Logger.Error("attachments: sweep", "err", err)
			}
			return
		}
		if n > 0 {
			s.Logger.Info("attachments: swept detached", "deleted", n)
		}
	}
	sweep()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweep()
		}
	}
}

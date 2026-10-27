package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/api"
)

func TestMutesEndpoints(t *testing.T) {
	e := newEnv(t)
	// Every mute call needs write.
	if rec := e.do(t, http.MethodPost, "/v1/mutes", readTok, []byte(`{"source":"s","for":"1h"}`)); rec.Code != http.StatusForbidden {
		t.Fatalf("read token: %d", rec.Code)
	}
	if rec := e.do(t, http.MethodGet, "/v1/mutes", readTok, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("read token lists: %d", rec.Code)
	}
	for _, bad := range []string{
		`{}`,
		`{"source":"s"}`,
		`{"source":"s","for":"soon"}`,
		`{"source":"s","for":"-1h"}`,
		`{"source":"s","for":"9000h"}`,
		`{"source":"s","for":"1h","until":"2026-10-27T13:00:00Z"}`,
		`{"source":"s","until":"2026-10-27T11:00:00Z"}`,
		`{"source":"s","for":"1h","why":"x"}`,
	} {
		if rec := e.do(t, http.MethodPost, "/v1/mutes", writeTok, []byte(bad)); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", bad, rec.Code, rec.Body)
		}
	}
	rec := e.do(t, http.MethodPost, "/v1/mutes", writeTok, []byte(`{"source":"backup","for":"1h"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	m := decode[api.Mute](t, rec)
	if !m.Active || m.Source == nil || *m.Source != "backup" || m.Key != nil || !m.Until.Equal(e.clock.now().Add(time.Hour)) {
		t.Fatalf("mute %+v", m)
	}
	rec = e.do(t, http.MethodPost, "/v1/mutes", adminTok, []byte(`{"key":"unit:a:b","until":"2026-10-27T12:30:00Z"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create by key: %d %s", rec.Code, rec.Body)
	}
	list := decode[api.MuteList](t, e.do(t, http.MethodGet, "/v1/mutes", writeTok, nil))
	if len(list.Mutes) != 2 || list.Mutes[0].Key == nil {
		t.Fatalf("list %+v", list)
	}
	// An expired mute leaves the list.
	e.clock.add(45 * time.Minute)
	list = decode[api.MuteList](t, e.do(t, http.MethodGet, "/v1/mutes", writeTok, nil))
	if len(list.Mutes) != 1 || list.Mutes[0].Id != m.Id {
		t.Fatalf("list after expiry %+v", list)
	}
	rec = e.do(t, http.MethodDelete, "/v1/mutes/"+m.Id, writeTok, nil)
	ended := decode[api.Mute](t, rec)
	if rec.Code != http.StatusOK || ended.Active || ended.EndedAt == nil {
		t.Fatalf("end: %d %+v", rec.Code, ended)
	}
	if rec := e.do(t, http.MethodDelete, "/v1/mutes/mute_none", writeTok, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("end unknown: %d", rec.Code)
	}
	if list := decode[api.MuteList](t, e.do(t, http.MethodGet, "/v1/mutes", writeTok, nil)); len(list.Mutes) != 0 {
		t.Fatalf("list after end %+v", list)
	}
}

// Hand-made image headers: enough bytes for content sniffing.
var (
	pngBytes  = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 24)...)
	jpegBytes = append([]byte("\xff\xd8\xff\xe0"), bytes.Repeat([]byte{0}, 24)...)
)

func TestIconEndpoints(t *testing.T) {
	e := newEnv(t)
	if rec := e.do(t, http.MethodPut, "/v1/icons/backup", writeTok, pngBytes); rec.Code != http.StatusForbidden {
		t.Fatalf("write token puts: %d", rec.Code)
	}
	if rec := e.do(t, http.MethodPut, "/v1/icons/backup", adminTok, []byte("<svg></svg>")); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("svg: %d", rec.Code)
	}
	if rec := e.do(t, http.MethodPut, "/v1/icons/-bad", adminTok, pngBytes); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad name: %d", rec.Code)
	}
	if rec := e.do(t, http.MethodPut, "/v1/icons/big", adminTok, make([]byte, MaxIconBytes+1)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("too big: %d", rec.Code)
	}
	rec := e.do(t, http.MethodPut, "/v1/icons/backup", adminTok, pngBytes)
	ic := decode[api.IconInfo](t, rec)
	if rec.Code != http.StatusCreated || ic.ContentType != "image/png" || ic.Bytes != len(pngBytes) || len(ic.Sha256) != 64 {
		t.Fatalf("put: %d %+v", rec.Code, ic)
	}
	rec = e.do(t, http.MethodPut, "/v1/icons/backup", adminTok, jpegBytes)
	if rec.Code != http.StatusOK || decode[api.IconInfo](t, rec).ContentType != "image/jpeg" {
		t.Fatalf("replace: %d %s", rec.Code, rec.Body)
	}
	rec = e.do(t, http.MethodGet, "/v1/icons/backup", readTok, nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/jpeg" || !bytes.Equal(rec.Body.Bytes(), jpegBytes) {
		t.Fatalf("get: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	req, _ := http.NewRequest(http.MethodGet, "/v1/icons/backup", nil)
	req.Header.Set("Authorization", "Bearer "+readTok)
	req.Header.Set("If-None-Match", rec.Header().Get("ETag"))
	if rec := serve(e, req); rec.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match: %d", rec.Code)
	}
	list := decode[api.IconInfoList](t, e.do(t, http.MethodGet, "/v1/icons", readTok, nil))
	if len(list.Icons) != 1 || list.Icons[0].Name != "backup" {
		t.Fatalf("list %+v", list)
	}
	if rec := e.do(t, http.MethodDelete, "/v1/icons/backup", adminTok, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec := e.do(t, http.MethodGet, "/v1/icons/backup", readTok, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete: %d", rec.Code)
	}
}

func TestStateCarriesIncident(t *testing.T) {
	e := newEnv(t)
	e.post(t, `{"source":"s","kind":"incident","key":"web","action":"open","title":"Web is down"}`)
	e.clock.add(5 * time.Minute)
	e.post(t, `{"source":"s","kind":"incident","key":"web","action":"ack"}`)
	e.clock.add(5 * time.Minute)
	e.post(t, `{"source":"s","kind":"incident","key":"web","action":"update","summary":"still down"}`)
	e.clock.add(2 * time.Minute)
	rec := e.post(t, `{"source":"s","kind":"incident","key":"db","action":"resolve"}`)
	checkStored(t, storedSchema(t), rec.Body.Bytes())
	st := decode[api.State](t, e.do(t, http.MethodGet, "/v1/state", readTok, nil))
	if len(st.Incidents) != 1 {
		t.Fatalf("incidents %+v", st.Incidents)
	}
	inc := st.Incidents[0].Incident
	if inc == nil || inc.State != api.EventIncidentStateAcked || !inc.OpenedAt.Equal(time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("incident %+v", inc)
	}
	// A sender may not set it.
	if rec := e.post(t, `{"source":"s","kind":"incident","key":"k","incident":{"state":"open","opened_at":"2026-10-27T12:00:00Z"}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("input with incident: %d", rec.Code)
	}
}

func serve(e *env, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

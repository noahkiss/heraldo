package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/store"
)

const devToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func devBody(token, extra string) []byte {
	return []byte(`{"token":"` + token + `","environment":"production","topic":"org.example.heraldo"` + extra + `}`)
}

func TestDeviceRegister(t *testing.T) {
	e := newEnv(t)
	rec := e.do(t, http.MethodPost, "/v1/devices", readTok, devBody(strings.ToUpper(devToken),
		`,"name":"Test phone","platform":"iOS","app_version":"1.0 (1)","permissions":{"alerts":true}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	d := decode[api.Device](t, rec)
	if !strings.HasPrefix(d.Id, "dev_") || !d.Active || d.TokenSuffix != devToken[56:] || *d.Name != "Test phone" ||
		*d.Permissions.Alerts != true || d.Permissions.TimeSensitive != nil {
		t.Fatalf("device %+v", d)
	}
	if strings.Contains(rec.Body.String(), devToken) {
		t.Fatal("the full token is in the answer")
	}

	// The same token again updates the device; a field left out is cleared.
	rec = e.do(t, http.MethodPost, "/v1/devices", readTok, devBody(devToken, `,"app_version":"1.1 (2)"`))
	again := decode[api.Device](t, rec)
	if rec.Code != http.StatusOK || again.Id != d.Id || *again.AppVersion != "1.1 (2)" || again.Name != nil {
		t.Fatalf("%d %+v", rec.Code, again)
	}

	// PUT moves the device to a new token.
	newTok := strings.Repeat("c", 64)
	rec = e.do(t, http.MethodPut, "/v1/devices/"+d.Id, readTok, devBody(newTok, ""))
	if rec.Code != http.StatusOK || decode[api.Device](t, rec).TokenSuffix != newTok[56:] {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	// A token another device holds is a conflict.
	other := decode[api.Device](t, e.do(t, http.MethodPost, "/v1/devices", readTok, devBody(devToken, "")))
	if rec := e.do(t, http.MethodPut, "/v1/devices/"+other.Id, readTok, devBody(newTok, "")); rec.Code != http.StatusConflict {
		t.Fatalf("conflict: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do(t, http.MethodPut, "/v1/devices/dev_nope", readTok, devBody(newTok, "")); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id: %d", rec.Code)
	}

	// DELETE retires it; the list keeps it.
	rec = e.do(t, http.MethodDelete, "/v1/devices/"+d.Id, readTok, nil)
	gone := decode[api.Device](t, rec)
	if rec.Code != http.StatusOK || gone.Active || *gone.RetiredReason != "removed" || gone.RetiredAt == nil {
		t.Fatalf("delete: %d %+v", rec.Code, gone)
	}
	list := decode[api.DeviceList](t, e.do(t, http.MethodGet, "/v1/devices", readTok, nil))
	if len(list.Devices) != 2 || list.Devices[0].Id != d.Id || list.Devices[0].Active {
		t.Fatalf("list %+v", list)
	}
	if rec := e.do(t, http.MethodGet, "/v1/devices/"+other.Id, readTok, nil); rec.Code != http.StatusOK {
		t.Fatalf("get: %d", rec.Code)
	}
	if rec := e.do(t, http.MethodDelete, "/v1/devices/dev_nope", readTok, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("delete unknown: %d", rec.Code)
	}
}

func TestDeviceValidation(t *testing.T) {
	e := newEnv(t)
	for _, body := range []string{
		`{"token":"xyz","environment":"production","topic":"org.example.heraldo"}`,
		`{"token":"` + devToken + `","environment":"staging","topic":"org.example.heraldo"}`,
		`{"token":"` + devToken + `","environment":"production"}`,
		`{"token":"` + devToken + `","environment":"production","topic":"org.example.heraldo","colour":"red"}`,
		`{"token":"` + devToken + `","environment":"production","topic":"org.example.heraldo","name":"` + strings.Repeat("n", 129) + `"}`,
		`not json`,
	} {
		if rec := e.do(t, http.MethodPost, "/v1/devices", readTok, []byte(body)); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", body, rec.Code)
		}
	}
	if rec := e.do(t, http.MethodPost, "/v1/devices", writeTok, devBody(devToken, "")); rec.Code != http.StatusForbidden {
		t.Errorf("write token: %d", rec.Code)
	}
	if rec := e.do(t, http.MethodGet, "/v1/devices", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", rec.Code)
	}
}

// incident returns the incident row for key, or nil.
func incident(t *testing.T, e *env, key string) map[string]any {
	t.Helper()
	row, err := e.st.Row(context.Background(), "incidents", key)
	if err == store.ErrNotFound {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func TestPermissionHealth(t *testing.T) {
	e := newEnv(t)
	perms := func(p string) []byte { return devBody(devToken, `,"name":"Test phone","permissions":`+p) }
	d := decode[api.Device](t, e.do(t, http.MethodPost, "/v1/devices", readTok,
		perms(`{"alerts":true,"time_sensitive":false}`)))
	alerts, ts, la := "device:"+d.Id+":alerts", "device:"+d.Id+":time_sensitive", "device:"+d.Id+":live_activities"
	if incident(t, e, alerts) != nil || incident(t, e, la) != nil {
		t.Fatal("incident for a permission that is on or unknown")
	}
	row := incident(t, e, ts)
	if row == nil || row["state"] != "open" {
		t.Fatalf("time_sensitive: %v", row)
	}
	ev, _ := e.st.Get(context.Background(), row["last_event"].(string))
	for _, want := range []string{`"source":"heraldo"`, `"title":"Test phone: time-sensitive notifications are off"`, `"level":"active"`} {
		if !strings.Contains(string(ev.Event), want) {
			t.Errorf("event lacks %s: %s", want, ev.Event)
		}
	}
	n, _ := e.st.Count(context.Background())

	// Reporting the same again raises nothing new.
	e.do(t, http.MethodPost, "/v1/devices", readTok, perms(`{"alerts":true,"time_sensitive":false}`))
	if m, _ := e.st.Count(context.Background()); m != n {
		t.Fatalf("%d events after a repeat report, want %d", m, n)
	}

	// Alerts off, time-sensitive back on.
	e.do(t, http.MethodPut, "/v1/devices/"+d.Id, readTok, perms(`{"alerts":false,"time_sensitive":true}`))
	if row := incident(t, e, ts); row["state"] != "resolved" {
		t.Fatalf("time_sensitive after on: %v", row)
	}
	if row := incident(t, e, alerts); row == nil || row["state"] != "open" {
		t.Fatalf("alerts after off: %v", row)
	}

	// Removing the device resolves what was off.
	e.do(t, http.MethodDelete, "/v1/devices/"+d.Id, readTok, nil)
	if row := incident(t, e, alerts); row["state"] != "resolved" {
		t.Fatalf("alerts after remove: %v", row)
	}
	// Registering it again with alerts still off raises it again.
	e.do(t, http.MethodPost, "/v1/devices", readTok, perms(`{"alerts":false}`))
	if row := incident(t, e, alerts); row["state"] != "open" {
		t.Fatalf("alerts after re-register: %v", row)
	}
}

func TestHealthPushAndUnread(t *testing.T) {
	e := newEnv(t)
	h := decode[api.Health](t, e.do(t, http.MethodGet, "/healthz", "", nil))
	if h.Push == nil || *h.Push != api.HealthPushOff {
		t.Fatalf("push = %v, want off", h.Push)
	}
	e.post(t, `{"source":"s","title":"one"}`)
	e.post(t, `{"source":"s","kind":"progress","key":"p","action":"start"}`)
	e.post(t, `{"source":"s","title":"two"}`)
	first := decode[api.EventPage](t, e.do(t, http.MethodGet, "/v1/events?limit=1", readTok, nil))
	c := decode[api.Cursor](t, e.do(t, http.MethodPut, "/v1/cursors/inbox", readTok, []byte(`{"cursor":"`+*first.NextCursor+`"}`)))
	// Two events after the cursor; progress does not count.
	if c.Unread == nil || *c.Unread != 1 {
		t.Fatalf("unread = %v, want 1", c.Unread)
	}
	if c := decode[api.Cursor](t, e.do(t, http.MethodGet, "/v1/cursors/inbox", readTok, nil)); *c.Unread != 1 {
		t.Fatalf("get unread = %d", *c.Unread)
	}
}

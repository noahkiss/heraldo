package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/noahkiss/heraldo/api"
)

func TestViewEndpoints(t *testing.T) {
	e := newEnv(t)
	if rec := e.do(t, http.MethodPost, "/v1/views", readTok, []byte(`{"name":"x"}`)); rec.Code != http.StatusForbidden {
		t.Fatalf("read token saves: %d", rec.Code)
	}
	for _, bad := range []string{
		`{}`,
		`{"name":""}`,
		`{"name":"view_x"}`,
		`{"name":"x","filter":{"kind":"nope"}}`,
		`{"name":"x","filter":{"level":["loud"]}}`,
		`{"name":"x","filter":{"routine":"both"}}`,
		`{"name":"x","filter":{"project":["Not A Name!"]}}`,
		`{"name":"x","filter":{"colour":"red"}}`,
		`{"name":"x","bundle":{"window":"soon"}}`,
		`{"name":"x","bundle":{"window":"10s"}}`,
		`{"name":"x","bundle":{"window":"10m","max_visible":0}}`,
		`{"name":"x","position":-1}`,
	} {
		if rec := e.do(t, http.MethodPost, "/v1/views", writeTok, []byte(bad)); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", bad, rec.Code, rec.Body)
		}
	}
	rec := e.do(t, http.MethodPost, "/v1/views", writeTok,
		[]byte(`{"name":"Backups","filter":{"source":"backup","project":["Home"],"level":["active","time-sensitive"]},"bundle":{"window":"15m"},"icon":"externaldrive","position":2}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	v := decode[api.View](t, rec)
	if v.Name != "Backups" || v.Filter.Source == nil || *v.Filter.Source != "backup" || v.Filter.Project == nil ||
		(*v.Filter.Project)[0] != "home" || v.Bundle == nil || v.Bundle.Window != "15m" || *v.Bundle.MaxVisible != 1 {
		t.Fatalf("view %+v", v)
	}
	if rec := e.do(t, http.MethodPost, "/v1/views", writeTok, []byte(`{"name":"backups"}`)); rec.Code != http.StatusConflict {
		t.Fatalf("same name: %d %s", rec.Code, rec.Body)
	}
	e.do(t, http.MethodPost, "/v1/views", writeTok, []byte(`{"name":"Agents","filter":{"source":"agent"}}`))
	list := decode[api.ViewList](t, e.do(t, http.MethodGet, "/v1/views", readTok, nil))
	if len(list.Views) != 2 || list.Views[0].Name != "Agents" {
		t.Fatalf("list %+v", list)
	}
	// By id, or by name without regard to case.
	for _, ref := range []string{v.Id, "BACKUPS"} {
		if got := decode[api.View](t, e.do(t, http.MethodGet, "/v1/views/"+ref, readTok, nil)); got.Id != v.Id {
			t.Fatalf("get %s: %+v", ref, got)
		}
	}
	rec = e.do(t, http.MethodPut, "/v1/views/"+v.Id, writeTok, []byte(`{"name":"Backup jobs","filter":{"source":"backup"}}`))
	put := decode[api.View](t, rec)
	if rec.Code != http.StatusOK || put.Name != "Backup jobs" || put.Bundle != nil || !put.CreatedAt.Equal(v.CreatedAt) {
		t.Fatalf("put: %d %+v", rec.Code, put)
	}
	if rec := e.do(t, http.MethodPut, "/v1/views/"+v.Id, writeTok, []byte(`{"name":"agents"}`)); rec.Code != http.StatusConflict {
		t.Fatalf("put to a taken name: %d", rec.Code)
	}
	if rec := e.do(t, http.MethodDelete, "/v1/views/backup%20jobs", writeTok, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do(t, http.MethodGet, "/v1/views/"+v.Id, readTok, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("get deleted: %d", rec.Code)
	}
	if rec := e.do(t, http.MethodDelete, "/v1/views/"+v.Id, writeTok, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("delete again: %d", rec.Code)
	}
}

func TestEventsByView(t *testing.T) {
	e := newEnv(t)
	for _, body := range []string{
		`{"source":"backup","title":"nas failed","project":"home"}`,
		`{"source":"backup","title":"vm ok","level":"passive","project":"home"}`,
		`{"source":"backup","title":"pi failed","project":"lab"}`,
		`{"source":"web","title":"hello","project":"home"}`,
		`{"source":"backup","kind":"checkin","key":"nightly","action":"success","project":"home"}`,
	} {
		if rec := e.do(t, http.MethodPost, "/v1/events", writeTok, []byte(body)); rec.Code != http.StatusCreated {
			t.Fatalf("post %s: %d %s", body, rec.Code, rec.Body)
		}
	}
	v := decode[api.View](t, e.do(t, http.MethodPost, "/v1/views", writeTok,
		[]byte(`{"name":"Home backups","filter":{"source":"backup","project":["home"],"level":["active"],"routine":"exclude"}}`)))
	titles := func(target string) ([]string, string) {
		t.Helper()
		rec := e.do(t, http.MethodGet, target, readTok, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", target, rec.Code, rec.Body)
		}
		var p struct{ Events []api.Event }
		_ = json.Unmarshal(rec.Body.Bytes(), &p)
		var out []string
		for _, ev := range p.Events {
			out = append(out, *ev.Title)
		}
		return out, rec.Header().Get("Heraldo-View")
	}
	got, hdr := titles("/v1/events?view=" + v.Id)
	if len(got) != 1 || got[0] != "nas failed" || hdr != v.Id {
		t.Fatalf("view: %v %q", got, hdr)
	}
	// A parameter replaces the view's field of the same name.
	if got, _ := titles("/v1/events?view=home%20backups&project=lab"); len(got) != 1 || got[0] != "pi failed" {
		t.Fatalf("view with project: %v", got)
	}
	if got, _ := titles("/v1/events?view=" + v.Id + "&level=passive,active"); len(got) != 2 {
		t.Fatalf("view with level: %v", got)
	}
	if got, _ := titles("/v1/events?level=passive"); len(got) != 1 || got[0] != "vm ok" {
		t.Fatalf("level alone: %v", got)
	}
	if rec := e.do(t, http.MethodGet, "/v1/events?view=nope", readTok, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown view: %d", rec.Code)
	}
	if rec := e.do(t, http.MethodGet, "/v1/events?level=loud", readTok, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad level: %d", rec.Code)
	}
}

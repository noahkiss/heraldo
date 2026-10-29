package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestListRoutine(t *testing.T) {
	e := newEnv(t)
	for _, b := range []string{
		`{"source":"s","kind":"message"}`,
		`{"source":"s","kind":"checkin","key":"job","action":"start"}`,
		`{"source":"s","kind":"checkin","key":"job","action":"success"}`,
		`{"source":"s","kind":"checkin","key":"job","action":"fail"}`,
		`{"source":"s","kind":"incident","key":"i","action":"open","title":"t"}`,
	} {
		if rec := e.post(t, b); rec.Code != http.StatusCreated {
			t.Fatalf("post %s = %d %s", b, rec.Code, rec.Body)
		}
	}
	count := func(q string) (int, string) {
		rec := e.do(t, http.MethodGet, "/v1/events?"+q, readTok, nil)
		var p struct{ Events []json.RawMessage }
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatalf("%s = %d %s", q, rec.Code, rec.Body)
		}
		return len(p.Events), rec.Header().Get("Heraldo-Routine")
	}
	for _, c := range []struct {
		q   string
		n   int
		hdr string
	}{
		{"", 5, ""},
		{"routine=exclude", 3, "exclude"},
		{"routine=only", 2, "only"},
		{"routine=exclude&key=job", 1, "exclude"},
		{"routine=exclude&order=desc&limit=1", 1, "exclude"},
	} {
		if n, hdr := count(c.q); n != c.n || hdr != c.hdr {
			t.Errorf("%q: %d events, header %q; want %d, %q", c.q, n, hdr, c.n, c.hdr)
		}
	}
	if rec := e.do(t, http.MethodGet, "/v1/events?routine=yes", readTok, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad routine = %d", rec.Code)
	}
}

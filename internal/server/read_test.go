package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/internal/auth"
)

func TestReadCallsNeedReadScope(t *testing.T) {
	e := newEnv(t)
	e.post(t, `{"source":"s","kind":"message"}`)
	for _, c := range []struct{ method, target, body string }{
		{http.MethodGet, "/v1/state", ""},
		{http.MethodPut, "/v1/cursors/laptop", `{"cursor":"s1"}`},
		{http.MethodGet, "/v1/cursors/laptop", ""},
	} {
		if rec := e.do(t, c.method, c.target, "", []byte(c.body)); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without a token = %d", c.method, c.target, rec.Code)
		}
		if rec := e.do(t, c.method, c.target, writeTok, []byte(c.body)); rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s with a write token = %d", c.method, c.target, rec.Code)
		}
		if rec := e.do(t, c.method, c.target, readTok, []byte(c.body)); rec.Code != http.StatusOK {
			t.Fatalf("%s %s with a read token = %d: %s", c.method, c.target, rec.Code, rec.Body)
		}
	}
}

func TestStateEndpoint(t *testing.T) {
	e := newEnv(t)
	for _, b := range []string{
		`{"source":"s","kind":"incident","key":"disk:a","action":"open","title":"disk a"}`,
		`{"source":"s","kind":"incident","key":"disk:b","action":"open"}`,
		`{"source":"s","kind":"incident","key":"disk:b","action":"resolve"}`,
		`{"source":"s","kind":"checkin","key":"nightly","action":"success"}`,
	} {
		if rec := e.post(t, b); rec.Code != http.StatusCreated {
			t.Fatalf("post: %d %s", rec.Code, rec.Body)
		}
	}
	rec := e.do(t, http.MethodGet, "/v1/state", readTok, nil)
	var st struct {
		Incidents, Progress, Checkins, Statuses []struct{ Key string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Incidents) != 1 || st.Incidents[0].Key != "disk:a" || len(st.Checkins) != 1 {
		t.Fatalf("state = %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"statuses":[]`) {
		t.Fatalf("an empty list must be [], not null: %s", rec.Body)
	}
}

func TestCursorEndpoints(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 3; i++ {
		e.post(t, `{"source":"s","kind":"message"}`)
	}
	if rec := e.do(t, http.MethodGet, "/v1/cursors/laptop", readTok, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unset cursor = %d", rec.Code)
	}
	put := func(tok, c string) *httptest.ResponseRecorder {
		return e.do(t, http.MethodPut, "/v1/cursors/laptop", tok, []byte(`{"cursor":"`+c+`"}`))
	}
	if rec := put(readTok, "s2"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"cursor":"s2"`) {
		t.Fatalf("put s2 = %d %s", rec.Code, rec.Body)
	}
	// Backward is ignored; the answer is the stored cursor.
	if rec := put(readTok, "s1"); !strings.Contains(rec.Body.String(), `"cursor":"s2"`) {
		t.Fatalf("put s1 = %s", rec.Body)
	}
	if rec := put(readTok, "s9"); rec.Code != http.StatusBadRequest {
		t.Fatalf("past head = %d", rec.Code)
	}
	if rec := put(readTok, "nope"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad cursor = %d", rec.Code)
	}
	if rec := e.do(t, http.MethodGet, "/v1/cursors/laptop", readTok, nil); !strings.Contains(rec.Body.String(), `"cursor":"s2"`) {
		t.Fatalf("get = %s", rec.Body)
	}
	// Cursors are shared: another token sees and moves the same one.
	if rec := e.do(t, http.MethodGet, "/v1/cursors/laptop", adminTok, nil); !strings.Contains(rec.Body.String(), `"cursor":"s2"`) {
		t.Fatalf("other token = %d %s", rec.Code, rec.Body)
	}
	if rec := put(adminTok, "s3"); !strings.Contains(rec.Body.String(), `"cursor":"s3"`) {
		t.Fatalf("other token put = %s", rec.Body)
	}
	if rec := e.do(t, http.MethodGet, "/v1/cursors/laptop", readTok, nil); !strings.Contains(rec.Body.String(), `"cursor":"s3"`) {
		t.Fatalf("after the other token = %s", rec.Body)
	}
	if rec := e.do(t, http.MethodGet, "/v1/cursors/a%20b", readTok, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad name = %d", rec.Code)
	}
}

func TestListOrderBeforeHold(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 3; i++ {
		e.post(t, `{"source":"s","kind":"message"}`)
	}
	rec := e.do(t, http.MethodGet, "/v1/events?order=desc&limit=2", readTok, nil)
	var p struct {
		Events     []json.RawMessage
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || len(p.Events) != 2 || p.NextCursor != "s2" {
		t.Fatalf("desc = %s", rec.Body)
	}
	rec = e.do(t, http.MethodGet, "/v1/events?order=desc&before=s2", readTok, nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || len(p.Events) != 1 || p.NextCursor != "s1" {
		t.Fatalf("before = %s", rec.Body)
	}
	if rec := e.do(t, http.MethodGet, "/v1/events?order=sideways", readTok, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad order = %d", rec.Code)
	}
	if rec := e.do(t, http.MethodGet, "/v1/events?before=x", readTok, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad before = %d", rec.Code)
	}
	rec = e.do(t, http.MethodGet, "/v1/events?hold=held", readTok, nil)
	if !strings.Contains(rec.Body.String(), `"events":[]`) {
		t.Fatalf("hold = %s", rec.Body)
	}
}

func TestStreamPokes(t *testing.T) {
	e := newEnv(t)
	e.post(t, `{"source":"s","kind":"message"}`)
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/stream", nil)
	req.Header.Set("Authorization", "Bearer "+readTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	next := func() string {
		for sc.Scan() {
			if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				return d
			}
		}
		t.Fatalf("stream ended: %v", sc.Err())
		return ""
	}
	if d := next(); d != `{"cursor":"s1"}` {
		t.Fatalf("first poke %q", d)
	}
	e.post(t, `{"source":"s","kind":"message"}`)
	if d := next(); d != `{"cursor":"s2"}` {
		t.Fatalf("second poke %q", d)
	}
}

func TestStreamEndsOnDone(t *testing.T) {
	e := newEnv(t)
	done := make(chan struct{})
	e.h = New(Options{Store: e.st, Tokens: mustTokens(t), Done: done, Heartbeat: 10 * time.Millisecond})
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/stream", nil)
	req.Header.Set("Authorization", "Bearer "+readTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sawKeepAlive := false
	go func() { time.Sleep(100 * time.Millisecond); close(done) }()
	for sc.Scan() {
		if sc.Text() == ": keep-alive" {
			sawKeepAlive = true
		}
	}
	if !sawKeepAlive {
		t.Fatal("no keep-alive before the stream ended")
	}
}

func mustTokens(t *testing.T) *auth.Set {
	t.Helper()
	set, err := auth.Parse(strings.NewReader("phone read "+auth.Hash(readTok)), '\n')
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// Each listed event carries its own list cursor: the next_cursor of a page
// that ends on it, so one PUT marks it read.
func TestListedEventsCarryCursor(t *testing.T) {
	e := newEnv(t)
	for _, b := range []string{`{"source":"s","title":"a"}`, `{"source":"s","title":"b"}`, `{"source":"s","title":"c"}`} {
		e.post(t, b)
	}
	rec := e.do(t, http.MethodGet, "/v1/events?order=desc", readTok, nil)
	var p struct {
		Events []struct {
			ID, Title, Cursor string
		}
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Events) != 3 || p.Events[2].Cursor != p.NextCursor || p.Events[0].Cursor == "" {
		t.Fatalf("page %s", rec.Body)
	}
	b := p.Events[1]
	if got := e.do(t, http.MethodGet, "/v1/events/"+b.ID, readTok, nil); !strings.Contains(got.Body.String(), `"cursor":"`+b.Cursor+`"`) {
		t.Fatalf("get by id: %s", got.Body)
	}
	put := e.do(t, http.MethodPut, "/v1/cursors/inbox", readTok, []byte(`{"cursor":"`+b.Cursor+`"}`))
	if !strings.Contains(put.Body.String(), `"unread":1`) {
		t.Fatalf("mark b read: %s", put.Body)
	}
	// A sender may not set it.
	if rec := e.post(t, `{"source":"s","cursor":"s1"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("post with cursor: %d", rec.Code)
	}
}

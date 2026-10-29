package server

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/store"
)

// attPNG is the start of a PNG file: enough for the type sniffer.
var attPNG = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 32)...)

func (e *env) upload(t *testing.T, token, ct string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/attachments", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func TestAttachmentUploadAndRead(t *testing.T) {
	e := newEnv(t)
	rec := e.upload(t, writeTok, "image/png", attPNG)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	a := decode[api.AttachmentCreated](t, rec)
	if !attachmentID.MatchString(a.Id) || a.Url != "attachment:"+a.Id || a.ContentType != "image/png" || a.Bytes != len(attPNG) {
		t.Fatalf("created: %+v", a)
	}
	// The same bytes again: the first attachment, 200.
	again := e.upload(t, writeTok, "image/png", attPNG)
	if again.Code != http.StatusOK || decode[api.AttachmentCreated](t, again).Id != a.Id {
		t.Fatalf("dedup: %d %s", again.Code, again.Body)
	}
	got := e.do(t, http.MethodGet, "/v1/attachments/"+a.Id, readTok, nil)
	if got.Code != http.StatusOK || !bytes.Equal(got.Body.Bytes(), attPNG) {
		t.Fatalf("get: %d", got.Code)
	}
	for k, want := range map[string]string{
		"Content-Type": "image/png", "X-Content-Type-Options": "nosniff", "ETag": `"` + a.Sha256 + `"`,
	} {
		if v := got.Header().Get(k); v != want {
			t.Fatalf("%s = %q, want %q", k, v, want)
		}
	}
	if !strings.Contains(got.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("no sandbox CSP")
	}
	if rec := e.do(t, http.MethodGet, "/v1/attachments/att_01JBA6Z0000000000000000099", readTok, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id: %d", rec.Code)
	}
	if rec := e.do(t, http.MethodGet, "/v1/attachments/nope", readTok, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id: %d", rec.Code)
	}
}

func TestAttachmentUploadRules(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		name, token, ct string
		body            []byte
		want            int
	}{
		{"read token", readTok, "text/plain", []byte("hi"), http.StatusForbidden},
		{"empty", writeTok, "text/plain", nil, http.StatusBadRequest},
		{"too big", writeTok, "text/plain", bytes.Repeat([]byte("a"), MaxAttachmentBytes+1), http.StatusRequestEntityTooLarge},
		{"html", writeTok, "text/html", []byte("<p>hi</p>"), http.StatusUnsupportedMediaType},
		{"svg", writeTok, "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>x</script></svg>`), http.StatusUnsupportedMediaType},
		{"zip sniffed", writeTok, "", []byte("PK\x03\x04rest"), http.StatusUnsupportedMediaType},
		{"exactly the cap", writeTok, "text/plain", bytes.Repeat([]byte("b"), MaxAttachmentBytes), http.StatusCreated},
		{"json", writeTok, "application/json", []byte(`{"a":1}`), http.StatusCreated},
		{"pdf", writeTok, "application/pdf", []byte("%PDF-1.7\n"), http.StatusCreated},
		{"text with charset", writeTok, "text/plain; charset=utf-8", []byte("log line"), http.StatusCreated},
	} {
		if rec := e.upload(t, c.token, c.ct, c.body); rec.Code != c.want {
			t.Fatalf("%s: %d, want %d: %s", c.name, rec.Code, c.want, rec.Body)
		}
	}
	// No type: the bytes decide.
	rec := e.upload(t, writeTok, "application/octet-stream", attPNG)
	if rec.Code != http.StatusCreated || decode[api.AttachmentCreated](t, rec).ContentType != "image/png" {
		t.Fatalf("sniffed: %d %s", rec.Code, rec.Body)
	}
}

func TestAttachmentMultipart(t *testing.T) {
	e := newEnv(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("note", "ignored")
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{`form-data; name="file"; filename="log.txt"`}
	h["Content-Type"] = []string{"text/plain"}
	pw, _ := mw.CreatePart(h)
	_, _ = pw.Write([]byte("disk full on /data"))
	_ = mw.Close()
	rec := e.upload(t, writeTok, mw.FormDataContentType(), buf.Bytes())
	if rec.Code != http.StatusCreated {
		t.Fatalf("multipart: %d %s", rec.Code, rec.Body)
	}
	a := decode[api.AttachmentCreated](t, rec)
	if a.ContentType != "text/plain" || a.Bytes != len("disk full on /data") {
		t.Fatalf("multipart: %+v", a)
	}
	got := e.do(t, http.MethodGet, "/v1/attachments/"+a.Id, readTok, nil)
	if got.Body.String() != "disk full on /data" || got.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("get: %q %q", got.Body, got.Header().Get("Content-Type"))
	}
	// A multipart body without a file part.
	buf.Reset()
	mw = multipart.NewWriter(&buf)
	_ = mw.WriteField("note", "no file")
	_ = mw.Close()
	if rec := e.upload(t, writeTok, mw.FormDataContentType(), buf.Bytes()); rec.Code != http.StatusBadRequest {
		t.Fatalf("no file part: %d", rec.Code)
	}
}

func TestEventWithAttachments(t *testing.T) {
	e := newEnv(t)
	img := decode[api.AttachmentCreated](t, e.upload(t, writeTok, "image/png", attPNG))
	txt := decode[api.AttachmentCreated](t, e.upload(t, writeTok, "text/plain", []byte("a log")))
	// An id the server does not hold is refused, and nothing is stored.
	rec := e.post(t, `{"source":"s","kind":"message","attachments":["att_01JBA6Z0000000000000000099"]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "att_01JBA6Z0000000000000000099") {
		t.Fatalf("unknown attachment: %d %s", rec.Code, rec.Body)
	}
	if n, _ := e.st.Count(context.Background()); n != 0 {
		t.Fatalf("stored %d events", n)
	}
	rec = e.post(t, `{"source":"s","kind":"message","title":"look","attachments":["`+txt.Id+`","`+img.Id+`"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("post: %d %s", rec.Code, rec.Body)
	}
	checkStored(t, storedSchema(t), rec.Body.Bytes())
	ev := decode[api.Event](t, rec)
	want := []api.AttachmentInfo{
		{Id: txt.Id, ContentType: "text/plain", Bytes: 5},
		{Id: img.Id, ContentType: "image/png", Bytes: len(attPNG)},
	}
	check := func(where string, ev api.Event) {
		t.Helper()
		if ev.AttachmentInfo == nil || len(*ev.AttachmentInfo) != 2 || (*ev.AttachmentInfo)[0] != want[0] || (*ev.AttachmentInfo)[1] != want[1] {
			t.Fatalf("%s: attachment_info = %+v", where, ev.AttachmentInfo)
		}
	}
	check("post", ev)
	check("get", decode[api.Event](t, e.do(t, http.MethodGet, "/v1/events/"+ev.Id, readTok, nil)))
	var page api.EventPage
	if err := json.Unmarshal(e.do(t, http.MethodGet, "/v1/events", readTok, nil).Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	check("list", page.Events[0])
	// A second event may name the same attachment.
	if rec := e.post(t, `{"source":"s","kind":"message","attachments":["`+img.Id+`"]}`); rec.Code != http.StatusCreated {
		t.Fatalf("second event: %d %s", rec.Code, rec.Body)
	}
	a, err := e.st.GetAttachment(context.Background(), img.Id)
	if err != nil || a.EventID != ev.Id {
		t.Fatalf("event_id = %q, %v; want the first event %s", a.EventID, err, ev.Id)
	}
}

func TestAttachmentSweep(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	kept := decode[api.AttachmentCreated](t, e.upload(t, writeTok, "text/plain", []byte("named")))
	gone := decode[api.AttachmentCreated](t, e.upload(t, writeTok, "text/plain", []byte("never named")))
	if rec := e.post(t, `{"source":"s","kind":"message","attachments":["`+kept.Id+`"]}`); rec.Code != http.StatusCreated {
		t.Fatalf("post: %d", rec.Code)
	}
	e.clock.add(23 * time.Hour)
	srv := e.h.(*Server)
	if n, err := e.st.SweepAttachments(ctx, srv.s.Now().Add(-store.AttachmentDetachedFor)); err != nil || n != 0 {
		t.Fatalf("sweep at 23 h: %d, %v", n, err)
	}
	e.clock.add(2 * time.Hour)
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { srv.RunAttachmentSweep(sctx, time.Hour); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := e.st.GetAttachment(ctx, gone.Id); err == store.ErrNotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the sweep kept a detached attachment past 24 h")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if _, err := e.st.GetAttachment(ctx, kept.Id); err != nil {
		t.Fatalf("the sweep deleted a named attachment: %v", err)
	}
}

func TestStandbyRefusesAttachments(t *testing.T) {
	e := newEnvWith(t, func(o *Options) { o.Standby = true })
	rec := e.upload(t, writeTok, "text/plain", []byte("hi"))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "primary") {
		t.Fatalf("standby upload: %d %s", rec.Code, rec.Body)
	}
}

// An import that names an attachment the primary never got rejects that
// event and keeps the rest of the batch.
func TestStandbyImportUnknownAttachment(t *testing.T) {
	e := newEnv(t)
	body := `{"events":[
		{"id":"01JBA6Z0000000000000000020","received_at":"2026-10-27T11:00:00Z","ts":"2026-10-27T11:00:00Z","source":"s","kind":"message","attachments":["att_01JBA6Z0000000000000000099"]},
		{"id":"01JBA6Z0000000000000000021","received_at":"2026-10-27T11:00:01Z","ts":"2026-10-27T11:00:01Z","source":"s","kind":"message"}]}`
	rec := e.do(t, http.MethodPost, "/v1/standby/import", adminTok, []byte(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("import: %d %s", rec.Code, rec.Body)
	}
	res := decode[api.StandbyImportResult](t, rec)
	if res.Imported != 1 || res.Rejected != 1 {
		t.Fatalf("import: %+v", res)
	}
}

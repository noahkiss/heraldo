package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestParseKey(t *testing.T) {
	k := testKey(t)
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(k) {
		t.Fatal("parsed key differs")
	}
	if _, err := parseKey([]byte("not a key")); err == nil {
		t.Fatal("want an error for a file with no PEM block")
	}
	k384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	der, _ = x509.MarshalPKCS8PrivateKey(k384)
	if _, err := parseKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); err == nil {
		t.Fatal("want an error for a P-384 key")
	}
}

func TestSignToken(t *testing.T) {
	k := testKey(t)
	now := time.Unix(1_800_000_000, 0)
	tok, err := signToken(k, "KEY123", "issuer-uuid", now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts", len(parts))
	}
	dec := func(s string) []byte {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	var h map[string]string
	if err := json.Unmarshal(dec(parts[0]), &h); err != nil {
		t.Fatal(err)
	}
	if h["alg"] != "ES256" || h["kid"] != "KEY123" || h["typ"] != "JWT" {
		t.Fatalf("header %v", h)
	}
	var c map[string]any
	if err := json.Unmarshal(dec(parts[1]), &c); err != nil {
		t.Fatal(err)
	}
	if c["iss"] != "issuer-uuid" || c["aud"] != "appstoreconnect-v1" ||
		c["iat"] != float64(now.Unix()) || c["exp"] != float64(now.Add(tokenLifetime).Unix()) {
		t.Fatalf("claims %v", c)
	}
	if exp := c["exp"].(float64) - c["iat"].(float64); exp > 20*60 {
		t.Fatalf("lifetime %v s is over Apple's 20 minute cap", exp)
	}
	sig := dec(parts[2])
	if len(sig) != 64 {
		t.Fatalf("signature is %d bytes, want raw r||s of 64", len(sig))
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(&k.PublicKey, sum[:], r, s) {
		t.Fatal("signature does not verify")
	}
}

// Handwritten fixtures in the shape of the App Store Connect API.
const appsJSON = `{"data":[
 {"type":"apps","id":"111","attributes":{"bundleId":"com.example.app.beta"}},
 {"type":"apps","id":"222","attributes":{"bundleId":"com.example.app"}}]}`

const notListedJSON = `{"data":[]}`

const processingJSON = `{"data":[{"type":"buildUploads","id":"u1","attributes":{
 "cfBundleShortVersionString":"1.2.0","cfBundleVersion":"202601010000","platform":"IOS",
 "createdDate":"2026-01-01T00:00:00Z","uploadedDate":"2026-01-01T00:01:00Z",
 "state":{"state":"PROCESSING","errors":[],"warnings":[],"infos":[]}},
 "relationships":{"build":{"data":null}}}]}`

// The upload passed, but its build is still processing: not done yet.
const completeBuildProcessingJSON = `{"data":[{"type":"buildUploads","id":"u1","attributes":{
 "cfBundleShortVersionString":"1.2.0","cfBundleVersion":"202601010000","platform":"IOS",
 "uploadedDate":"2026-01-01T00:01:00Z",
 "state":{"state":"COMPLETE","errors":[],"warnings":[],"infos":[]}},
 "relationships":{"build":{"data":{"type":"builds","id":"b1"}}}}],
 "included":[{"type":"builds","id":"b1","attributes":{"processingState":"PROCESSING"}}]}`

const completeJSON = `{"data":[{"type":"buildUploads","id":"u1","attributes":{
 "cfBundleShortVersionString":"1.2.0","cfBundleVersion":"202601010000","platform":"IOS",
 "uploadedDate":"2026-01-01T00:01:00Z",
 "state":{"state":"COMPLETE","errors":[],"warnings":[{"code":"90000","description":"A warning."}],"infos":[]}},
 "relationships":{"build":{"data":{"type":"builds","id":"b1"}}}}],
 "included":[{"type":"builds","id":"b1","attributes":{"processingState":"VALID"}}]}`

// Two uploads with one build number: the newer one failed.
const failedJSON = `{"data":[
 {"type":"buildUploads","id":"u0","attributes":{
  "cfBundleShortVersionString":"1.2.0","cfBundleVersion":"202601010000","platform":"IOS",
  "uploadedDate":"2026-01-01T00:01:00Z",
  "state":{"state":"COMPLETE","errors":[],"warnings":[],"infos":[]}},
  "relationships":{"build":{"data":{"type":"builds","id":"b0"}}}},
 {"type":"buildUploads","id":"u1","attributes":{
  "cfBundleShortVersionString":"1.2.0","cfBundleVersion":"202601010000","platform":"IOS",
  "uploadedDate":"2026-01-01T00:05:00Z",
  "state":{"state":"FAILED","errors":[{"code":"90626","description":"Invalid Siri Support. An example reason."}],"warnings":[],"infos":[]}},
  "relationships":{"build":{"data":null}}}],
 "included":[{"type":"builds","id":"b0","attributes":{"processingState":"VALID"}}]}`

// fakeASC answers /v1/apps with appsJSON and each buildUploads call with the
// next response in turn, repeating the last.
type fakeASC struct {
	t         *testing.T
	uploads   []string
	status    int // answer every call with this status when non-zero
	calls     int
	lastQuery string
}

func (f *fakeASC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		f.t.Errorf("no bearer token on %s", r.URL.Path)
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
		w.Write([]byte(`{"errors":[{"status":"401","code":"NOT_AUTHORIZED"}]}`))
		return
	}
	switch r.URL.Path {
	case "/v1/apps":
		if got := r.URL.Query().Get("filter[bundleId]"); got != "com.example.app" {
			f.t.Errorf("filter[bundleId] = %q", got)
		}
		w.Write([]byte(appsJSON))
	case "/v1/apps/222/buildUploads":
		f.lastQuery = r.URL.RawQuery
		i := min(f.calls, len(f.uploads)-1)
		f.calls++
		w.Write([]byte(f.uploads[i]))
	default:
		f.t.Errorf("unexpected path %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func newWaiter(t *testing.T, url string, timeout time.Duration) (*waiter, *bytes.Buffer) {
	clock := time.Unix(1_800_000_000, 0)
	now := func() time.Time { return clock }
	var log bytes.Buffer
	return &waiter{
		api:      &client{base: url, http: http.DefaultClient, key: testKey(t), keyID: "K", issuer: "I", now: now},
		bundleID: "com.example.app", build: "202601010000", platform: "IOS",
		timeout: timeout, interval: time.Minute,
		now: now, sleep: func(d time.Duration) { clock = clock.Add(d) }, log: &log,
	}, &log
}

func TestWaitComplete(t *testing.T) {
	f := &fakeASC{t: t, uploads: []string{notListedJSON, processingJSON, completeBuildProcessingJSON, completeJSON}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	w, log := newWaiter(t, srv.URL, time.Hour)
	var out bytes.Buffer
	if code := w.run(context.Background(), &out); code != exitComplete {
		t.Fatalf("exit %d; log:\n%s", code, log)
	}
	if f.calls != 4 {
		t.Fatalf("%d buildUploads calls, want 4", f.calls)
	}
	for _, want := range []string{"filter%5BcfBundleVersion%5D=202601010000", "filter%5Bplatform%5D=IOS", "include=build"} {
		if !strings.Contains(f.lastQuery, want) {
			t.Errorf("query %q lacks %s", f.lastQuery, want)
		}
	}
	if !strings.Contains(out.String(), "processed and valid") || !strings.Contains(out.String(), "warning 90000") {
		t.Fatalf("out:\n%s", out.String())
	}
	if !strings.Contains(log.String(), "upload COMPLETE, build PROCESSING") {
		t.Fatalf("log:\n%s", log)
	}
}

func TestWaitFailed(t *testing.T) {
	f := &fakeASC{t: t, uploads: []string{processingJSON, failedJSON}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	w, log := newWaiter(t, srv.URL, time.Hour)
	if code := w.run(context.Background(), &bytes.Buffer{}); code != exitFailed {
		t.Fatalf("exit %d; log:\n%s", code, log)
	}
	if !strings.Contains(log.String(), "error 90626: Invalid Siri Support. An example reason.") {
		t.Fatalf("log:\n%s", log)
	}
}

func TestWaitBuildInvalid(t *testing.T) {
	invalid := strings.Replace(completeJSON, `"processingState":"VALID"`, `"processingState":"INVALID"`, 1)
	f := &fakeASC{t: t, uploads: []string{invalid}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	w, log := newWaiter(t, srv.URL, time.Hour)
	if code := w.run(context.Background(), &bytes.Buffer{}); code != exitFailed {
		t.Fatalf("exit %d; log:\n%s", code, log)
	}
	if !strings.Contains(log.String(), "upload COMPLETE, build INVALID") {
		t.Fatalf("log:\n%s", log)
	}
}

func TestWaitTimeout(t *testing.T) {
	f := &fakeASC{t: t, uploads: []string{processingJSON}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	w, log := newWaiter(t, srv.URL, 3*time.Minute)
	if code := w.run(context.Background(), &bytes.Buffer{}); code != exitUnknown {
		t.Fatalf("exit %d; log:\n%s", code, log)
	}
	// Checks at 0, 1, 2 and 3 minutes: the last one is at the deadline.
	if f.calls != 4 {
		t.Fatalf("%d calls, want 4", f.calls)
	}
	if !strings.Contains(log.String(), "state unknown") {
		t.Fatalf("log:\n%s", log)
	}
}

func TestWaitUnauthorized(t *testing.T) {
	f := &fakeASC{t: t, status: http.StatusUnauthorized}
	srv := httptest.NewServer(f)
	defer srv.Close()
	w, log := newWaiter(t, srv.URL, time.Hour)
	if code := w.run(context.Background(), &bytes.Buffer{}); code != exitUnknown {
		t.Fatalf("exit %d; log:\n%s", code, log)
	}
	if !strings.Contains(log.String(), "401") {
		t.Fatalf("log:\n%s", log)
	}
}

func TestWaitRetriesServerError(t *testing.T) {
	fails := 2
	inner := &fakeASC{t: t, uploads: []string{completeJSON}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fails > 0 {
			fails--
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer srv.Close()
	w, log := newWaiter(t, srv.URL, time.Hour)
	if code := w.run(context.Background(), &bytes.Buffer{}); code != exitComplete {
		t.Fatalf("exit %d; log:\n%s", code, log)
	}
}

func TestAppIDNoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"type":"apps","id":"111","attributes":{"bundleId":"com.example.app.beta"}}]}`))
	}))
	defer srv.Close()
	w, log := newWaiter(t, srv.URL, time.Hour)
	if code := w.run(context.Background(), &bytes.Buffer{}); code != exitUnknown {
		t.Fatalf("exit %d; log:\n%s", code, log)
	}
	if !strings.Contains(log.String(), "no app with this bundle ID") {
		t.Fatalf("log:\n%s", log)
	}
}

func TestPrintNewest(t *testing.T) {
	var query string
	builds := `{"data":[{"type":"builds","id":"b1","attributes":{"version":"202601010000",
 "uploadedDate":"2026-01-01T00:01:00Z","expirationDate":"2026-04-01T00:01:00Z"}}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/apps":
			w.Write([]byte(appsJSON))
		case "/v1/builds":
			query = r.URL.RawQuery
			w.Write([]byte(builds))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	now := func() time.Time { return time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC) }
	c := &client{base: srv.URL, http: http.DefaultClient, key: testKey(t), keyID: "K", issuer: "I", now: now}
	var out, log bytes.Buffer
	if code := printNewest(context.Background(), c, "com.example.app", "IOS", &out, &log); code != exitComplete {
		t.Fatalf("exit %d, log %q", code, log.String())
	}
	if got, want := out.String(), "202601010000 2026-01-01T00:01:00Z 2026-04-01T00:01:00Z 60\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	for _, want := range []string{"filter%5Bapp%5D=222", "filter%5BprocessingState%5D=VALID", "sort=-uploadedDate", "limit=1"} {
		if !strings.Contains(query, want) {
			t.Errorf("query %q lacks %s", query, want)
		}
	}

	builds = `{"data":[]}`
	out.Reset()
	if code := printNewest(context.Background(), c, "com.example.app", "IOS", &out, &log); code != exitComplete || out.String() != "none\n" {
		t.Fatalf("no builds: exit %d, out %q", code, out.String())
	}
}

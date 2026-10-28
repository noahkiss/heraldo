package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/internal/auth"
	"github.com/noahkiss/heraldo/internal/server"
	"github.com/noahkiss/heraldo/internal/store"
	"github.com/noahkiss/heraldo/internal/validate"
)

const (
	readTok   = "cli-read-token-for-tests"
	readTok2  = "phone-read-token-for-tests"
	writeTok  = "cli-write-token-for-tests"
	bridgeTok = "cli-bridge-token-for-tests"
)

// syncBuf is a bytes.Buffer that tail's goroutine and the test can share.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type fixture struct {
	t   *testing.T
	srv *httptest.Server
	env map[string]string
}

// newFixture runs a real heraldo server on a temporary database.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	list := fmt.Sprintf("cli read %s\nphone read %s\nsender write %s\nbridge write %s trusted-ingest\n",
		auth.Hash(readTok), auth.Hash(readTok2), auth.Hash(writeTok), auth.Hash(bridgeTok))
	tokens, err := auth.Parse(strings.NewReader(list), '\n')
	if err != nil {
		t.Fatal(err)
	}
	v, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	srv := httptest.NewServer(server.New(server.Options{
		Store: st, Tokens: tokens, Validator: v, Done: done,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}))
	t.Cleanup(func() { close(done); srv.Close() })
	return &fixture{t: t, srv: srv, env: map[string]string{
		"HERALDO_URL": srv.URL, "HERALDO_TOKEN": readTok,
		"XDG_CONFIG_HOME": t.TempDir(), "HOME": t.TempDir(),
	}}
}

func (f *fixture) post(tok, body string) string {
	f.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/v1/events", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	var e struct{ ID string }
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated || json.Unmarshal(b, &e) != nil {
		f.t.Fatalf("post %s: %d %s", body, resp.StatusCode, b)
	}
	return e.ID
}

type result struct {
	code           int
	stdout, stderr string
}

func (f *fixture) app(stdout, stderr io.Writer, stdin string, tty bool) *app {
	return &app{
		stdout: stdout, stderr: stderr, stdin: strings.NewReader(stdin),
		stdinIsTTY: func() bool { return tty },
		getenv:     func(k string) string { return f.env[k] },
		lookPath:   exec.LookPath,
		pollEvery:  20 * time.Millisecond,
		retryMax:   50 * time.Millisecond,
	}
}

func (f *fixture) run(args ...string) result {
	f.t.Helper()
	var out, errb bytes.Buffer
	code := f.app(&out, &errb, "", true).run(context.Background(), args)
	return result{code, out.String(), errb.String()}
}

func (f *fixture) runIn(stdin string, args ...string) result {
	f.t.Helper()
	var out, errb bytes.Buffer
	code := f.app(&out, &errb, stdin, false).run(context.Background(), args)
	return result{code, out.String(), errb.String()}
}

func TestNeedsURLAndToken(t *testing.T) {
	f := newFixture(t)
	delete(f.env, "HERALDO_URL")
	if r := f.run("ls"); r.code != 1 || !strings.Contains(r.stderr, "HERALDO_URL is not set") {
		t.Fatalf("%+v", r)
	}
}

func TestEnvFile(t *testing.T) {
	f := newFixture(t)
	f.post(writeTok, `{"source":"s","kind":"message","title":"from the file"}`)
	path := filepath.Join(f.env["XDG_CONFIG_HOME"], "heraldo", "env")
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte("# settings\nexport HERALDO_URL=\""+f.srv.URL+"\"\nHERALDO_TOKEN='"+readTok+"'\n"), 0o600)
	delete(f.env, "HERALDO_URL")
	delete(f.env, "HERALDO_TOKEN")
	if r := f.run("ls"); r.code != 0 || !strings.Contains(r.stdout, "from the file") {
		t.Fatalf("%+v", r)
	}
	// The environment wins over the file.
	f.env["HERALDO_TOKEN"] = "wrong"
	if r := f.run("ls"); r.code != 1 || !strings.Contains(r.stderr, "401") {
		t.Fatalf("%+v", r)
	}
}

func TestLsShowAndFilters(t *testing.T) {
	f := newFixture(t)
	id := f.post(writeTok, `{"source":"backup","kind":"message","title":"first","body":"line one\nline two"}`)
	f.post(writeTok, `{"source":"watch","kind":"incident","key":"disk:a","action":"open","title":"disk a full","host":"box1"}`)
	f.post(writeTok, `{"source":"watch","kind":"incident","key":"disk:b","action":"open","title":"disk b full"}`)
	f.post(writeTok, `{"source":"watch","kind":"incident","key":"disk:b","action":"resolve","title":"disk b ok"}`)

	r := f.run("ls")
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if r.code != 0 || len(lines) != 4 || !strings.Contains(lines[0], "first") || !strings.Contains(lines[3], "disk b ok") {
		t.Fatalf("ls, oldest first: %+v", r)
	}
	if r := f.run("ls", "--limit", "1"); !strings.Contains(r.stdout, "disk b ok") || strings.Count(r.stdout, "\n") != 1 {
		t.Fatalf("ls --limit 1 keeps the newest: %+v", r)
	}
	if r := f.run("ls", "--source", "backup"); strings.Count(r.stdout, "\n") != 1 {
		t.Fatalf("--source: %+v", r)
	}
	if r := f.run("ls", "--kind", "incident"); strings.Count(r.stdout, "\n") != 3 {
		t.Fatalf("--kind: %+v", r)
	}
	r = f.run("ls", "--open")
	if r.code != 0 || !strings.Contains(r.stdout, "[incident open disk:a]") || strings.Contains(r.stdout, "disk:b") {
		t.Fatalf("--open: %+v", r)
	}
	f.post(writeTok, `{"source":"watch","kind":"incident","key":"disk:0","action":"open","title":"disk 0 full"}`)
	f.post(writeTok, `{"source":"watch","kind":"incident","key":"disk:c","action":"open","title":"disk c full"}`)
	r = f.run("ls", "--open", "--limit", "2")
	if lines := strings.Split(strings.TrimSpace(r.stdout), "\n"); len(lines) != 2 ||
		!strings.Contains(lines[0], "disk:0") || !strings.Contains(lines[1], "disk:c") {
		t.Fatalf("--open --limit 2 keeps the two most recent, oldest first: %+v", r)
	}
	r = f.run("ls", "--json", "--key", "disk:b", "--limit", "1")
	var e map[string]any
	if json.Unmarshal([]byte(r.stdout), &e) != nil || e["key"] != "disk:b" {
		t.Fatalf("--json: %+v", r)
	}
	r = f.run("show", id)
	if r.code != 0 || !strings.Contains(r.stdout, "source:   backup") || !strings.Contains(r.stdout, "line one\nline two") {
		t.Fatalf("show: %+v", r)
	}
	if r := f.run("show", "--json", id); !strings.HasPrefix(r.stdout, "{") {
		t.Fatalf("show --json: %+v", r)
	}
	if r := f.run("show", "nope"); r.code != 1 || !strings.Contains(r.stderr, "404") {
		t.Fatalf("show unknown: %+v", r)
	}
	if r := f.run("show"); r.code != 2 {
		t.Fatalf("show without id: %+v", r)
	}
}

func TestShowStripsControlCharacters(t *testing.T) {
	f := newFixture(t)
	id := f.post(writeTok, `{"source":"s","kind":"message","title":"a\u001b[2Jb"}`)
	if r := f.run("show", id); strings.Contains(r.stdout, "\x1b") {
		t.Fatalf("escape reached the terminal: %q", r.stdout)
	}
	if r := f.run("ls"); strings.Contains(r.stdout, "\x1b") {
		t.Fatalf("escape reached the terminal: %q", r.stdout)
	}
}

func TestPeekAndMark(t *testing.T) {
	f := newFixture(t)
	f.post(writeTok, `{"source":"s","kind":"message","title":"old one"}`)
	f.post(writeTok, `{"source":"s","kind":"message","title":"old two"}`)
	r := f.run("peek")
	if r.code != 0 || !strings.Contains(r.stderr, `no read cursor "inbox" yet`) || strings.Count(r.stdout, "\n") != 2 {
		t.Fatalf("first peek: %+v", r)
	}
	if r := f.run("peek", "--mark"); !strings.Contains(r.stderr, "read cursor") {
		t.Fatalf("peek --mark: %+v", r)
	}
	if r := f.run("peek"); r.stdout != "" || !strings.Contains(r.stderr, "nothing unread") {
		t.Fatalf("after mark: %+v", r)
	}
	f.post(writeTok, `{"source":"s","kind":"message","title":"new one"}`)
	f.post(writeTok, `{"source":"s","kind":"message","title":"new two"}`)
	r = f.run("peek", "--limit", "1", "--mark")
	if !strings.Contains(r.stdout, "new one") || !strings.Contains(r.stderr, "more unread") {
		t.Fatalf("peek --limit 1: %+v", r)
	}
	if r := f.run("peek"); !strings.Contains(r.stdout, "new two") || strings.Contains(r.stdout, "new one") {
		t.Fatalf("peek after a partial mark: %+v", r)
	}
	// Another token reads the same inbox.
	f.env["HERALDO_TOKEN"] = writeTok
	if r := f.run("peek"); r.code != 1 || !strings.Contains(r.stderr, "403") {
		t.Fatalf("write token: %+v", r)
	}
	f.env["HERALDO_TOKEN"] = readTok2
	if r := f.run("peek"); !strings.Contains(r.stdout, "new two") || strings.Contains(r.stdout, "new one") {
		t.Fatalf("second read token, shared cursor: %+v", r)
	}
	// HERALDO_CURSOR names a separate cursor.
	f.env["HERALDO_CURSOR"] = "scratch"
	if r := f.run("peek"); !strings.Contains(r.stderr, `no read cursor "scratch"`) {
		t.Fatalf("scratch cursor: %+v", r)
	}
	f.env["HERALDO_CURSOR"] = "bad name"
	if r := f.run("peek"); r.code != 1 || !strings.Contains(r.stderr, "400") {
		t.Fatalf("bad cursor name: %+v", r)
	}
}

func TestHeld(t *testing.T) {
	f := newFixture(t)
	f.post(bridgeTok, `{"source":"s","kind":"message","title":"held one","details":{"tee":{"hold":{"state":"held","id":"h-1"}}}}`)
	f.post(writeTok, `{"source":"s","kind":"message","title":"not held"}`)
	r := f.run("held")
	if r.code != 0 || !strings.Contains(r.stdout, "(held) held one") || strings.Contains(r.stdout, "not held") {
		t.Fatalf("%+v", r)
	}
}

func TestCheckins(t *testing.T) {
	f := newFixture(t)
	if r := f.run("checkins"); r.code != 0 || !strings.Contains(r.stderr, "no check-ins") {
		t.Fatalf("%+v", r)
	}
	f.post(writeTok, `{"source":"cron","kind":"checkin","key":"nightly","action":"success"}`)
	f.post(writeTok, `{"source":"cron","kind":"checkin","key":"weekly","action":"fail","checkin":{"exit_code":3}}`)
	r := f.run("checkins")
	if !strings.Contains(r.stdout, "nightly") || !strings.Contains(r.stdout, "up") || !strings.Contains(r.stdout, "down (failed)") {
		t.Fatalf("%+v", r)
	}

	if r := f.run("checkins", "create", "backup"); r.code != 1 || !strings.Contains(r.stderr, "HERALDO_WRITE_TOKEN is not set") {
		t.Fatalf("create without a write token: %+v", r)
	}
	f.env["HERALDO_WRITE_TOKEN"] = writeTok
	r = f.run("checkins", "create", "backup", "--period", "24h", "--grace", "2h", "--reminder", "0s", "--host", "box-1")
	if r.code != 0 || !strings.Contains(r.stdout, "every 24h0m0s, grace 2h0m0s") || !strings.Contains(r.stdout, "state:     new") {
		t.Fatalf("create: %+v", r)
	}
	f.ping("/ping/backup/start?rid=1", "")
	f.ping("/ping/backup?rid=1", "wrote 12 GB\nall good\n")
	r = f.run("checkins")
	if !regexpMatch(`backup +up +\S+ \S+ +\d+m?s +wrote 12 GB\n`, r.stdout) {
		t.Fatalf("list after a run: %q", r.stdout)
	}
	r = f.run("checkins", "show", "backup")
	if r.code != 0 || !strings.Contains(r.stdout, "wrote 12 GB\nall good") || !strings.Contains(r.stdout, "host:      box-1") {
		t.Fatalf("show: %+v", r)
	}
	if r := f.run("checkins", "pause", "backup"); r.code != 0 || r.stdout != "backup: paused\n" {
		t.Fatalf("pause: %+v", r)
	}
	if r := f.run("checkins", "resume", "backup"); r.code != 0 || r.stdout != "backup: new\n" {
		t.Fatalf("resume: %+v", r)
	}
	r = f.run("checkins", "key", "backup")
	pingURL := strings.TrimSpace(r.stdout)
	if r.code != 0 || !strings.HasPrefix(pingURL, f.srv.URL+"/p/") || len(pingURL) != len(f.srv.URL)+3+32 {
		t.Fatalf("key: %+v", r)
	}
	resp, err := http.Get(pingURL + "/log")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("ping by key: %v %v", resp, err)
	}
	resp.Body.Close()
	if r := f.run("checkins", "show", "backup"); !strings.Contains(r.stdout, "ping key:  minted") {
		t.Fatalf("show after key: %+v", r)
	}
	if r := f.run("checkins", "key", "--remove", "backup"); r.code != 0 || r.stdout != "backup: ping key removed\n" {
		t.Fatalf("key --remove: %+v", r)
	}
	if r := f.run("checkins", "pause", "nope"); r.code != 1 || !strings.Contains(r.stderr, "no check") {
		t.Fatalf("pause unknown: %+v", r)
	}
	if r := f.run("checkins", "--json"); r.code != 0 || strings.Count(r.stdout, "\n") != 3 || !strings.Contains(r.stdout, `"key":"backup"`) {
		t.Fatalf("json: %+v", r)
	}
}

func (f *fixture) ping(path, body string) {
	f.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+writeTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		f.t.Fatalf("%s: %d", path, resp.StatusCode)
	}
}

func regexpMatch(pattern, s string) bool { return regexp.MustCompile(pattern).MatchString(s) }

func TestAckResolve(t *testing.T) {
	f := newFixture(t)
	f.post(writeTok, `{"source":"watch","kind":"incident","key":"disk:a","action":"open","title":"disk a full","host":"box1"}`)
	if r := f.run("ack", "disk:a"); r.code != 1 || !strings.Contains(r.stderr, "HERALDO_WRITE_TOKEN is not set") {
		t.Fatalf("without a write token: %+v", r)
	}
	f.env["HERALDO_WRITE_TOKEN"] = writeTok
	if r := f.run("ack", "disk:zz"); r.code != 1 || !strings.Contains(r.stderr, "no open incident") {
		t.Fatalf("unknown key: %+v", r)
	}
	if r := f.run("ack", "disk:a", "-m", "looking"); r.code != 0 || !strings.Contains(r.stdout, "acknowledged disk:a") {
		t.Fatalf("ack: %+v", r)
	}
	if r := f.run("ack", "disk:a"); !strings.Contains(r.stderr, "already acknowledged") {
		t.Fatalf("second ack: %+v", r)
	}
	r := f.run("ls", "--open")
	if !strings.Contains(r.stdout, "[incident ack disk:a]") || !strings.Contains(r.stdout, "Acknowledged: disk a full") {
		t.Fatalf("ls --open after ack: %+v", r)
	}
	if r := f.run("resolve", "disk:a"); r.code != 0 {
		t.Fatalf("resolve: %+v", r)
	}
	if r := f.run("ls", "--open"); r.stdout != "" {
		t.Fatalf("ls --open after resolve: %+v", r)
	}
	// The ack event keeps the incident's host.
	r = f.run("ls", "--json", "--key", "disk:a", "--limit", "3")
	if strings.Count(r.stdout, `"host":"box1"`) != 3 || !strings.Contains(r.stdout, `"source":"heraldo-cli"`) {
		t.Fatalf("events: %s", r.stdout)
	}
}

// tailUntil runs tail until stdout contains want, or fails after 2 s.
func (f *fixture) tailUntil(t *testing.T, args []string, start func(), want ...string) (string, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, errb syncBuf
	done := make(chan int)
	go func() { done <- f.app(&out, &errb, "", true).run(ctx, append([]string{"tail"}, args...)) }()
	// Give tail time to take the head before the new event arrives.
	time.Sleep(150 * time.Millisecond)
	start()
	deadline := time.Now().Add(2 * time.Second)
	for {
		ok := true
		for _, w := range want {
			ok = ok && strings.Contains(out.String(), w)
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("tail never printed %q; stdout %q stderr %q", want, out.String(), errb.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("tail exit %d: %s", code, errb.String())
	}
	return out.String(), errb.String()
}

func TestTailStream(t *testing.T) {
	f := newFixture(t)
	f.post(writeTok, `{"source":"s","kind":"message","title":"before tail"}`)
	start := time.Now()
	out, _ := f.tailUntil(t, nil, func() {
		f.post(writeTok, `{"source":"s","kind":"message","title":"during tail"}`)
	}, "during tail")
	if strings.Contains(out, "before tail") {
		t.Fatalf("tail showed history: %q", out)
	}
	if d := time.Since(start); d > time.Second+150*time.Millisecond {
		t.Fatalf("tail took %s", d)
	}
}

func TestTailFiltersAndMark(t *testing.T) {
	f := newFixture(t)
	if r := f.run("tail", "--mark", "--source", "x"); r.code != 1 || !strings.Contains(r.stderr, "--mark with filters") {
		t.Fatalf("%+v", r)
	}
	out, _ := f.tailUntil(t, []string{"--source", "b"}, func() {
		f.post(writeTok, `{"source":"a","kind":"message","title":"from a"}`)
		f.post(writeTok, `{"source":"b","kind":"message","title":"from b"}`)
	}, "from b")
	if strings.Contains(out, "from a") {
		t.Fatalf("filter let a through: %q", out)
	}
	f.tailUntil(t, []string{"--mark"}, func() {
		f.post(writeTok, `{"source":"a","kind":"message","title":"marked"}`)
	}, "marked")
	if r := f.run("peek"); !strings.Contains(r.stderr, "nothing unread") {
		t.Fatalf("tail --mark did not move the cursor: %+v", r)
	}
}

// oldServer stands in for a server before stage 3: no stream, and a list
// call that ignores order, before and hold.
func (f *fixture) oldServer(t *testing.T) {
	real := f.srv.Config.Handler
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/stream" || r.URL.Path == "/v1/state" || strings.HasPrefix(r.URL.Path, "/v1/checkins") {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		q.Del("order")
		q.Del("before")
		q.Del("hold")
		r.URL.RawQuery = q.Encode()
		rw := &stripOrder{ResponseWriter: w}
		real.ServeHTTP(rw, r)
	}))
	t.Cleanup(old.Close)
	f.env["HERALDO_URL"] = old.URL
}

type stripOrder struct{ http.ResponseWriter }

func (s *stripOrder) WriteHeader(code int) {
	s.Header().Del("Heraldo-Order")
	s.ResponseWriter.WriteHeader(code)
}

func TestOldServer(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 3; i++ {
		f.post(writeTok, fmt.Sprintf(`{"source":"s","kind":"message","title":"n%d"}`, i))
	}
	f.post(bridgeTok, `{"source":"s","kind":"message","title":"held one","details":{"tee":{"hold":{"state":"held","id":"h-1"}}}}`)
	f.oldServer(t)
	if r := f.run("ls", "--limit", "2"); r.code != 0 || !strings.Contains(r.stdout, "n2") || !strings.Contains(r.stdout, "held one") || strings.Contains(r.stdout, "n1") {
		t.Fatalf("ls: %+v", r)
	}
	if r := f.run("held"); r.code != 0 || strings.Count(r.stdout, "\n") != 1 {
		t.Fatalf("held: %+v", r)
	}
	if r := f.run("checkins"); r.code != 1 || !strings.Contains(r.stderr, "no /v1/state") {
		t.Fatalf("checkins: %+v", r)
	}
}

func TestTailPollsAServerWithoutStream(t *testing.T) {
	f := newFixture(t)
	f.post(writeTok, `{"source":"s","kind":"message","title":"history"}`)
	f.oldServer(t)
	out, errOut := f.tailUntil(t, nil, func() {
		f.post(writeTok, `{"source":"s","kind":"message","title":"polled"}`)
	}, "polled")
	if strings.Contains(out, "history") {
		t.Fatalf("tail showed history: %q", out)
	}
	if !strings.Contains(errOut, "polling") {
		t.Fatalf("stderr %q", errOut)
	}
}

func TestTailExec(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	outFile := filepath.Join(dir, "seen")
	script := filepath.Join(dir, "hook.sh")
	os.WriteFile(script, []byte("#!/bin/sh\n{ printf '%s|%s|' \"$HERALDO_EVENT_SOURCE\" \"$HERALDO_EVENT_TITLE\"; printf 'tok=%s|' \"$HERALDO_TOKEN\"; cat; echo; } >> "+outFile+"\necho hook-done\n"), 0o755)
	allow := filepath.Join(f.env["XDG_CONFIG_HOME"], "heraldo", "exec-allow")
	os.MkdirAll(filepath.Dir(allow), 0o700)

	other := filepath.Join(dir, "other.sh")
	os.WriteFile(other, []byte("#!/bin/sh\n"), 0o755)
	if r := f.run("tail", "--exec", script); r.code != 1 || !strings.Contains(r.stderr, "needs an allowlist") {
		t.Fatalf("no allowlist: %+v", r)
	}
	os.WriteFile(allow, []byte("# hooks\n"+other+"\n"), 0o600)
	if r := f.run("tail", "--exec", script); r.code != 1 || !strings.Contains(r.stderr, "not in the allowlist") {
		t.Fatalf("not listed: %+v", r)
	}
	os.WriteFile(allow, []byte(script+"\n"), 0o666)
	os.Chmod(allow, 0o666)
	if r := f.run("tail", "--exec", script); r.code != 1 || !strings.Contains(r.stderr, "writable by group or others") {
		t.Fatalf("world-writable allowlist: %+v", r)
	}
	os.Chmod(allow, 0o600)

	// Text that a shell would expand stays text.
	title := `$(touch ` + filepath.Join(dir, "pwned") + `); "quoted"`
	body, _ := json.Marshal(map[string]any{"source": "s", "kind": "message", "title": title})
	f.tailUntil(t, []string{"--exec", script}, func() { f.post(writeTok, string(body)) }, "hook-done")
	seen, _ := os.ReadFile(outFile)
	got := string(seen)
	if !strings.HasPrefix(got, "s|"+title+"|tok=|{") || !strings.Contains(got, `"kind":"message"`) {
		t.Fatalf("hook saw %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "pwned")); err == nil {
		t.Fatal("event text ran as a command")
	}
}

// tee fakes an Apprise-compatible notify URL.
func tee(t *testing.T, status int, answer string, got *map[string]any) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got != nil {
			json.NewDecoder(r.Body).Decode(got)
		}
		w.WriteHeader(status)
		io.WriteString(w, answer)
	}))
	t.Cleanup(s.Close)
	return s.URL + "/notify/apprise"
}

func TestSendContract(t *testing.T) {
	f := newFixture(t)
	// No URL: a silent no-op, as notify.
	if r := f.run("send", "-t", "x", "-m", "y"); r.code != 0 || r.stdout != "" || r.stderr != "" {
		t.Fatalf("no URL: %+v", r)
	}
	var got map[string]any
	f.env["APPRISE_URL"] = tee(t, 200, "Sent Pushover notification.", &got)
	r := f.run("send", "-t", "Title", "-m", "Body", "--tag", "quiet", "--source", "test")
	if r.code != 0 || r.stdout != sentLine+"\n" {
		t.Fatalf("sent: %+v", r)
	}
	if got["title"] != "Title" || got["body"] != "Body" || got["tag"] != "quiet" || got["source"] != "test" {
		t.Fatalf("payload %v", got)
	}
	// Defaults, stdin, long flags and extra fields.
	f.env["NOTIFY_EXTRA_JSON"] = `"_nonce": "abc"`
	got = nil
	r = f.runIn("line one\nline two\n", "send", "--title", "T")
	if r.code != 0 || got["body"] != "line one\nline two" || got["_nonce"] != "abc" || got["title"] != "T" {
		t.Fatalf("stdin: %+v %v", r, got)
	}
	delete(f.env, "NOTIFY_EXTRA_JSON")
	got = nil
	f.runIn("b", "send")
	if got["title"] != "System Notification" {
		t.Fatalf("default title: %v", got)
	}
	// A terminal on stdin and no -m.
	if r := f.run("send"); r.code != 1 || !strings.HasPrefix(r.stdout, "Error: Message is required.") {
		t.Fatalf("no message: %+v", r)
	}
	if r := f.run("send", "-m", "x", "stray"); r.code != 1 || !strings.Contains(r.stdout, "Unknown parameter: stray") {
		t.Fatalf("stray: %+v", r)
	}
	// Failures.
	f.env["APPRISE_URL"] = tee(t, 424, "One or more notification could not be sent.", nil)
	if r := f.run("send", "-m", "x"); r.code != 1 || r.stdout != "❌ Failed to send notification. HTTP Status: 424\n" {
		t.Fatalf("424: %+v", r)
	}
	f.env["APPRISE_URL"] = tee(t, 200, "", nil)
	if r := f.run("send", "-m", "x"); r.code != 1 || !strings.Contains(r.stdout, "HTTP Status: 200") {
		t.Fatalf("200 without Sent: %+v", r)
	}
	f.env["APPRISE_URL"] = "http://127.0.0.1:1/notify/apprise?token=secret"
	if r := f.run("send", "-m", "x"); r.code != 1 || !strings.Contains(r.stdout, "HTTP Status: 000") || strings.Contains(r.stderr, "secret") {
		t.Fatalf("no answer: %+v", r)
	}
	// HERALDO_SEND_URL wins over APPRISE_URL.
	f.env["HERALDO_SEND_URL"] = tee(t, 200, "Sent", nil)
	if r := f.run("send", "-m", "x"); r.code != 0 {
		t.Fatalf("HERALDO_SEND_URL: %+v", r)
	}
}

func TestSendFallback(t *testing.T) {
	f := newFixture(t)
	f.env["HERALDO_WRITE_TOKEN"] = writeTok
	// The notify URL's host is down: nothing listens there.
	f.env["APPRISE_URL"] = "http://127.0.0.1:1/notify/apprise?token=secret"
	// No fallback: the old contract.
	if r := f.run("send", "-m", "x"); r.code != 1 || r.stdout != "❌ Failed to send notification. HTTP Status: 000\n" {
		t.Fatalf("no fallback: %+v", r)
	}
	f.env["HERALDO_FALLBACK_URL"] = f.srv.URL
	if r := f.run("send", "-m", "x"); r.code != 1 || !strings.Contains(r.stderr, "but HERALDO_FALLBACK_TOKEN is not") {
		t.Fatalf("no fallback token: %+v", r)
	}
	f.env["HERALDO_FALLBACK_TOKEN"] = writeTok
	r := f.run("send", "-t", "Disk", "-m", "pool degraded", "--tag", "ops", "--level", "urgent",
		"--source", "zpool-health", "--url", "https://example.net/pool")
	if r.code != 0 || r.stdout != sentLine+"\n" || !strings.Contains(r.stderr, "the fallback 127.0.0.1") ||
		strings.Contains(r.stderr, "secret") {
		t.Fatalf("fallback: %+v", r)
	}
	ls := f.run("ls", "--json")
	if !strings.Contains(ls.stdout, `"source":"zpool-health"`) || !strings.Contains(ls.stdout, `"tags":["urgent","ops"]`) ||
		!strings.Contains(ls.stdout, `"body":"pool degraded"`) {
		t.Fatalf("stored: %+v", ls)
	}
	// An envelope level passes as level.
	f.run("send", "-m", "y", "--level", "time-sensitive")
	if ls := f.run("ls", "--json"); !strings.Contains(ls.stdout, `"level":"time-sensitive"`) {
		t.Fatalf("level: %s", ls.stdout)
	}
	// A notify URL that answers 424 falls back too.
	f.env["APPRISE_URL"] = tee(t, 424, "One or more notification could not be sent.", nil)
	if r := f.run("send", "-m", "z"); r.code != 0 || !strings.Contains(r.stderr, "HTTP 424") {
		t.Fatalf("424: %+v", r)
	}
	// The primary works: the fallback is not used.
	f.env["APPRISE_URL"] = tee(t, 200, "Sent", nil)
	if r := f.run("send", "-m", "w"); r.code != 0 || r.stderr != "" {
		t.Fatalf("primary ok: %+v", r)
	}
	// Both fail: the failed line carries the notify URL's status.
	f.env["APPRISE_URL"] = tee(t, 424, "", nil)
	f.env["HERALDO_FALLBACK_TOKEN"] = "wrong-token"
	if r := f.run("send", "-m", "v"); r.code != 1 || r.stdout != "❌ Failed to send notification. HTTP Status: 424\n" ||
		!strings.Contains(r.stderr, "fallback: HTTP 401") {
		t.Fatalf("both fail: %+v", r)
	}
}

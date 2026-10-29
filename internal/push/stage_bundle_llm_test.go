package push

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/internal/decide"
	"github.com/noahkiss/heraldo/internal/llm"
	"github.com/noahkiss/heraldo/internal/store"
)

// withBundleLLM gives the rig's pusher a bundle text hook over f in mode.
func (r *rig) withBundleLLM(t *testing.T, mode string, f *llm.Fake) {
	t.Helper()
	r.set(t, "llm.mode", `"`+mode+`"`)
	r.set(t, "llm.base_url", `"https://llm.example.net/v1"`)
	r.set(t, "llm.decisions_model", `"dm"`)
	c := llm.New(llm.Config{Settings: r.prefs, Log: r.st, NewProvider: f.Factory, Now: r.clock.now})
	d := decide.New(decide.Config{LLM: c, Guidance: r.st, Settings: r.prefs})
	r.p.SetBundleText(d.Bundle)
}

// postNoWait stores an event and does not wait for its push or the hook.
func (r *rig) postNoWait(t *testing.T, body string) string {
	t.Helper()
	rec := r.do(t, http.MethodPost, "/v1/events", writeTok, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("post: %d %s", rec.Code, rec.Body)
	}
	return strings.Split(strings.Split(rec.Body.String(), `"id":"`)[1], `"`)[0]
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func llmRow(t *testing.T, st *store.Store, id string) string {
	t.Helper()
	rows, err := st.Decisions(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		if r.Stage == store.LLMStage {
			out = append(out, r.Reason)
		}
	}
	return strings.Join(out, " | ")
}

// gated is a fake that answers only once gate closes.
func gated(gate chan struct{}) *llm.Fake {
	return &llm.Fake{Reply: func(llm.Request) (llm.Response, error) {
		<-gate
		return llm.Response{Content: `{"title":"Backups failing","summary":"nas, vm and pi backups failed"}`}, nil
	}}
}

func TestBundleLLMNeverDelays(t *testing.T) {
	r := newRig(t, 0)
	r.register(t, prodToken, "production")
	view := r.view(t, `{"name":"Backups","filter":{"source":"backup"},"bundle":{"window":"10m"}}`)
	gate := make(chan struct{})
	f := gated(gate)
	r.withBundleLLM(t, "advise", f)

	r.post(t, writeTok, `{"source":"backup","title":"nas failed"}`)
	if len(f.Requests()) != 0 {
		t.Fatal("a push on its own asked the LLM")
	}
	r.clock.add(time.Minute)
	second := r.postNoWait(t, `{"source":"backup","title":"vm failed"}`)
	// The bundle push goes out while the LLM still has not answered.
	waitFor(t, "the bundle push", func() bool { return len(r.prod.requests()) == 2 })
	req, a := lastAlert(t, r.prod)
	if a.APS.Alert.Body != "2 new in Backups: vm failed" || a.APS.InterruptionLevel != "active" {
		t.Fatalf("bundle push %+v", a)
	}
	waitFor(t, "the LLM call", func() bool { return len(f.Requests()) == 1 })
	close(gate)
	r.p.Wait()

	reqs := r.prod.requests()
	if len(reqs) != 3 {
		t.Fatalf("want the LLM's replace, got %d pushes", len(reqs))
	}
	up, b := lastAlert(t, r.prod)
	if up.CollapseID != req.CollapseID || up.CollapseID != CollapseID("view:"+view) || b.APS.Alert.Title != "Backups failing" ||
		b.APS.Alert.Body != "2 new in Backups: nas, vm and pi backups failed" || b.APS.InterruptionLevel != "passive" || b.APS.Sound != "" {
		t.Fatalf("replace %+v %+v", up, b)
	}
	if got := llmRow(t, r.st, second); !strings.Contains(got, `LLM advise (dm, `) ||
		!strings.Contains(got, `titled the bundle "Backups failing"; replaced the bundle through its collapse id`) {
		t.Fatalf("trace %q", got)
	}
	// The event's delivery stays the bundle push's.
	if d := r.delivery(t, second); len(d) != 1 || d[0].Level == nil || string(*d[0].Level) != "active" {
		t.Fatalf("delivery %+v", d)
	}
}

func TestBundleLLMLateAnswer(t *testing.T) {
	r := newRig(t, 0)
	r.register(t, prodToken, "production")
	r.view(t, `{"name":"Backups","filter":{"source":"backup"},"bundle":{"window":"10m"}}`)
	gate := make(chan struct{})
	f := gated(gate)
	r.withBundleLLM(t, "enforce", f)
	r.post(t, writeTok, `{"source":"backup","title":"nas failed"}`)
	second := r.postNoWait(t, `{"source":"backup","title":"vm failed"}`)
	waitFor(t, "the LLM call", func() bool { return len(f.Requests()) == 1 })
	r.clock.add(11 * time.Minute)
	close(gate)
	r.p.Wait()
	if n := len(r.prod.requests()); n != 2 {
		t.Fatalf("a late answer pushed: %d pushes", n)
	}
	if got := llmRow(t, r.st, second); !strings.Contains(got, "not sent: the window closed first") {
		t.Fatalf("trace %q", got)
	}
}

func TestBundleLLMNewerPushAsksAgain(t *testing.T) {
	r := newRig(t, 0)
	r.register(t, prodToken, "production")
	r.view(t, `{"name":"Backups","filter":{"source":"backup"},"bundle":{"window":"10m"}}`)
	gate := make(chan struct{})
	f := gated(gate)
	r.withBundleLLM(t, "enforce", f)
	r.post(t, writeTok, `{"source":"backup","title":"nas failed"}`)
	second := r.postNoWait(t, `{"source":"backup","title":"vm failed"}`)
	waitFor(t, "the LLM call", func() bool { return len(f.Requests()) == 1 })
	third := r.postNoWait(t, `{"source":"backup","title":"pi failed"}`)
	waitFor(t, "the third push", func() bool { return len(r.prod.requests()) == 3 })
	close(gate)
	r.p.Wait()
	if n := len(f.Requests()); n != 2 {
		t.Fatalf("calls %d, want 2", n)
	}
	if got := llmRow(t, r.st, second); !strings.Contains(got, "not sent: a newer push replaced the bundle first") {
		t.Fatalf("second trace %q", got)
	}
	_, a := lastAlert(t, r.prod)
	if a.APS.Alert.Body != "3 new in Backups: nas, vm and pi backups failed" {
		t.Fatalf("replace %+v", a)
	}
	if got := llmRow(t, r.st, third); !strings.Contains(got, "replaced the bundle through its collapse id") {
		t.Fatalf("third trace %q", got)
	}
}

func TestBundleLLMShadowAndFailure(t *testing.T) {
	for _, c := range []struct {
		mode, reply, want string
	}{
		{"shadow", `{"title":"Backups failing","summary":"three failed"}`, `would title the bundle "Backups failing"; nothing changed (shadow)`},
		{"advise", `{"title":"","summary":"x"}`, "llm: invalid output (title must be one line of 1 to 80 characters); the bundle keeps its own text"},
	} {
		t.Run(c.mode, func(t *testing.T) {
			r := newRig(t, 0)
			r.register(t, prodToken, "production")
			r.view(t, `{"name":"Backups","filter":{"source":"backup"},"bundle":{"window":"10m"}}`)
			r.withBundleLLM(t, c.mode, &llm.Fake{Replies: []llm.Response{{Content: c.reply}}})
			r.post(t, writeTok, `{"source":"backup","title":"nas failed"}`)
			second := r.post(t, writeTok, `{"source":"backup","title":"vm failed"}`)
			if n := len(r.prod.requests()); n != 2 {
				t.Fatalf("pushes %d, want 2", n)
			}
			if got := llmRow(t, r.st, second); !strings.Contains(got, c.want) {
				t.Fatalf("trace %q", got)
			}
		})
	}
}

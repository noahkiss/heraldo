package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/noahkiss/heraldo/internal/llm"
	"github.com/noahkiss/heraldo/internal/store"
)

const proseReply = `{"paragraph":"Overnight one disk filled on lab and is still open. Everything else was quiet."}`

func TestDigestProse(t *testing.T) {
	cases := []struct {
		name  string
		vars  map[string]string
		reply string
		prose bool
		trace string
	}{
		{"prose off", map[string]string{"HERALDO_LLM_MODE": "enforce"}, proseReply, false, ""},
		{"advise", map[string]string{"HERALDO_LLM_MODE": "advise", "HERALDO_DIGEST_PROSE": "true"}, proseReply, true,
			"wrote the digest's paragraph ("},
		{"shadow", map[string]string{"HERALDO_LLM_MODE": "shadow", "HERALDO_DIGEST_PROSE": "true"}, proseReply, false,
			"wrote the digest's paragraph; not sent (shadow)"},
		{"invalid", map[string]string{"HERALDO_LLM_MODE": "enforce", "HERALDO_DIGEST_PROSE": "true"}, `{"paragraph":""}`, false,
			"llm: invalid output (paragraph must be one paragraph of 1 to 700 characters); the digest is the plain list"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vars := map[string]string{"HERALDO_LLM_BASE_URL": "https://llm.example.net/v1", "HERALDO_LLM_DECISIONS_MODEL": "dm"}
			for k, v := range c.vars {
				vars[k] = v
			}
			f := &llm.Fake{Replies: []llm.Response{{Content: c.reply}}}
			e := newLLMEnv(t, f, vars, nil)
			mustPost(t, e, `{"source":"mon","kind":"incident","key":"disk:a","action":"open","title":"Disk full","project":"lab"}`)
			rec := e.do(t, http.MethodPost, "/v1/digest/send", writeTok, nil)
			if rec.Code != http.StatusCreated {
				t.Fatalf("send %d %s", rec.Code, rec.Body)
			}
			evs := digestEvents(t, e)
			if len(evs) != 1 {
				t.Fatalf("digests %d", len(evs))
			}
			body := deref(evs[0].Body)
			if got := strings.Contains(body, "Overnight one disk filled"); got != c.prose {
				t.Fatalf("prose in body %v, want %v:\n%s", got, c.prose, body)
			}
			if c.trace == "" {
				if len(f.Requests()) != 0 {
					t.Fatal("digest.prose off asked the LLM")
				}
				return
			}
			if u := f.Requests()[0].Messages[1].Content; !strings.Contains(u, `"task":"digest"`) || !strings.Contains(u, "Disk full") {
				t.Fatalf("user message %s", u)
			}
			rows, _ := e.st.Decisions(context.Background(), evs[0].Id)
			found := false
			for _, r := range rows {
				found = found || (r.Stage == store.LLMStage && strings.Contains(r.Reason, c.trace))
			}
			if !found {
				t.Fatalf("trace %+v", rows)
			}
		})
	}
}

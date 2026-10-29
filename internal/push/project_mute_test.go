package push

import (
	"net/http"
	"strings"
	"testing"

	"github.com/noahkiss/heraldo/api"
)

// A project mute holds back every event filed under the project, or under
// a name the aliases map to it, and says so in the delivery.
func TestProjectMuteHoldsBackPush(t *testing.T) {
	r := newRigRules(t, 0, "aliases:\n  old-web: web\n")
	r.register(t, prodToken, "production")
	rec := r.do(t, http.MethodPost, "/v1/mutes", writeTok, `{"project":"Web","for":"1h"}`)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"project":"web"`) {
		t.Fatalf("mute: %d %s", rec.Code, rec.Body)
	}
	muted := r.post(t, writeTok, `{"source":"deploy","project":"web","title":"deployed"}`)
	aliased := r.post(t, writeTok, `{"source":"deploy","project":"old-web","title":"deployed again"}`)
	r.post(t, writeTok, `{"source":"deploy","project":"fleet","title":"not muted"}`)
	r.post(t, writeTok, `{"source":"deploy","title":"no project"}`)
	if n := len(r.prod.requests()); n != 2 {
		t.Fatalf("%d pushes, want 2", n)
	}
	for _, id := range []string{muted, aliased} {
		ds := r.delivery(t, id)
		if len(ds) != 1 || ds[0].Outcome != api.DeliveryOutcomeSkipped || ds[0].Error == nil ||
			!strings.HasPrefix(*ds[0].Error, "muted by mute_") || !strings.HasSuffix(*ds[0].Error, " (project)") {
			t.Fatalf("delivery of %s: %+v", id, ds)
		}
	}
	if rec := r.do(t, http.MethodPost, "/v1/mutes", writeTok, `{"project":"not a name","for":"1h"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad project: %d %s", rec.Code, rec.Body)
	}
}

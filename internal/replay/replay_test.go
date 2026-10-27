package replay

import (
	"bytes"
	"strings"
	"testing"

	"github.com/noahkiss/heraldo/internal/rules"
	"github.com/noahkiss/heraldo/internal/validate"
)

func TestReplay(t *testing.T) {
	v, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	rs, err := rules.Load("../rules/testdata/rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	r, err := Run("testdata/catalogue", rs, v)
	if err != nil {
		t.Fatal(err)
	}
	if r.Files != 2 || r.Lines != 11 || r.Bad != 1 || len(r.Events) != 10 {
		t.Fatalf("files %d lines %d bad %d events %d", r.Files, r.Lines, r.Bad, len(r.Events))
	}
	if r.Classified() != 8 {
		t.Fatalf("classified %d, want 8", r.Classified())
	}
	// alpha pairs. beta's recovery names the second unit, the failure keys
	// on the first. gamma recovers before the catalogue saw it fail.
	if r.Recoveries != 3 || r.Paired != 1 || r.StillOpen != 1 {
		t.Fatalf("recoveries %d paired %d still open %d", r.Recoveries, r.Paired, r.StillOpen)
	}
	if len(r.Unpaired) != 2 || r.Unpaired[0].Key != "unit:gamma:user/late.service" ||
		r.Unpaired[1].Key != "unit:beta:system/two.service" {
		t.Fatalf("unpaired = %+v", r.Unpaired)
	}
	if len(r.Unclassified) != 2 {
		t.Fatalf("unclassified = %+v", r.Unclassified)
	}
	for _, g := range r.Unclassified {
		if g.Source != "apprise-tee" || g.Count != 1 || (g.Tag != "general" && g.Tag != "") {
			t.Errorf("group %+v", g)
		}
	}
	hits := map[string]int{}
	for _, h := range r.Hits {
		hits[h.Rule] = h.Count
	}
	if hits["unit-recovered"] != 3 || hits["job-ok"] != 1 || hits["nightly-digest"] != 1 || hits["quiet-message"] != 1 {
		t.Fatalf("hits = %v", hits)
	}

	var out bytes.Buffer
	r.Write(&out)
	for _, want := range []string{
		"recoveries paired: 1 / 3 (33.3%)",
		"unit:beta:system/two.service",
		"unclassified source/tag groups: 2",
		"apprise-tee / (no tag)",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
}

func TestReplayWithoutRules(t *testing.T) {
	v, _ := validate.New()
	r, err := Run("testdata/catalogue", nil, v)
	if err != nil {
		t.Fatal(err)
	}
	if r.Classified() != 0 || r.Recoveries != 0 || len(r.Hits) != 0 {
		t.Fatalf("classified %d recoveries %d", r.Classified(), r.Recoveries)
	}
}

func TestReplayNoCatalogue(t *testing.T) {
	v, _ := validate.New()
	if _, err := Run(t.TempDir(), nil, v); err == nil {
		t.Fatal("empty directory: want an error")
	}
}

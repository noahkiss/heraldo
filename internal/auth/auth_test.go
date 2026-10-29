package auth

import (
	"strings"
	"testing"
)

func TestParseAndLookup(t *testing.T) {
	list := "# senders\nsender write " + Hash("w") + "\n\nphone read " + strings.ToUpper(Hash("r")) + "\r\nops admin " + Hash("a") + "\n"
	s, err := Parse(strings.NewReader(list), '\n')
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 3 {
		t.Fatalf("len = %d", s.Len())
	}
	for tok, want := range map[string]Scope{"w": ScopeWrite, "r": ScopeRead, "a": ScopeAdmin} {
		got, ok := s.Lookup(tok)
		if !ok || got.Scope != want {
			t.Errorf("%s: %v %v", tok, got, ok)
		}
	}
	for _, bad := range []string{"", "x", Hash("w")} {
		if _, ok := s.Lookup(bad); ok {
			t.Errorf("%q matched", bad)
		}
	}
}

func TestParseEnvList(t *testing.T) {
	s, err := Parse(strings.NewReader("a write "+Hash("1")+", b read "+Hash("2")), ',')
	if err != nil || s.Len() != 2 {
		t.Fatalf("%v %v", s, err)
	}
	if empty, err := Parse(strings.NewReader(""), ','); err != nil || empty.Len() != 0 {
		t.Fatalf("empty list: %v %v", empty, err)
	}
}

func TestParseErrors(t *testing.T) {
	for _, bad := range []string{
		"a write",
		"a delete " + Hash("1"),
		"a write abc",
		"a write " + Hash("1") + "\na read " + Hash("2"),
		"a write " + Hash("1") + " extra",
		"a write " + Hash("1") + " trusted-ingest bogus",
	} {
		if _, err := Parse(strings.NewReader(bad), '\n'); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestScopes(t *testing.T) {
	if !ScopeAdmin.Allows(ScopeWrite) || !ScopeAdmin.Allows(ScopeRead) {
		t.Fatal("admin must cover write and read")
	}
	if ScopeWrite.Allows(ScopeRead) || ScopeRead.Allows(ScopeWrite) {
		t.Fatal("write and read must stay apart")
	}
}

func TestCapabilities(t *testing.T) {
	s, err := Parse(strings.NewReader("bridge write "+Hash("b")+" trusted-ingest, plain write "+Hash("p")), ',')
	if err != nil {
		t.Fatal(err)
	}
	b, _ := s.Lookup("b")
	p, _ := s.Lookup("p")
	if !b.Has(CapTrustedIngest) || p.Has(CapTrustedIngest) {
		t.Fatalf("caps: bridge %v, plain %v", b.Caps, p.Caps)
	}
	k, err := Parse(strings.NewReader("keyed write "+Hash("k")+" notify-key"), ',')
	if err != nil {
		t.Fatal(err)
	}
	if kt, _ := k.Lookup("k"); !kt.Has(CapNotifyKey) || kt.Has(CapTrustedIngest) {
		t.Fatalf("caps: keyed %v", kt.Caps)
	}
}

func TestGrantAllows(t *testing.T) {
	w := Token{Name: "w", Scope: ScopeWrite}
	k := Token{Name: "k", Scope: ScopeWrite, Caps: []string{CapNotifyKey}}
	r := Token{Name: "r", Scope: ScopeRead}
	if !GrantAllows(w, ScopeRead) || !GrantAllows(w, ScopeWrite) {
		t.Fatal("a plain write grant reads and writes")
	}
	if GrantAllows(k, ScopeRead) || !SenderOnly(k) || SenderOnly(w) {
		t.Fatal("a notify-key grant must not read")
	}
	if GrantAllows(r, ScopeWrite) || GrantAllows(w, ScopeAdmin) {
		t.Fatal("a grant never raises a scope")
	}
}

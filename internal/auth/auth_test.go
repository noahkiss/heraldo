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

package validate

import (
	"strings"
	"testing"
)

func TestMessages(t *testing.T) {
	v, err := New()
	if err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{
		`{"source":"s","kind":"incident"}`:               "key",
		`{"source":"s","kind":"message","level":"loud"}`: "/level",
		`{"source":"s","kind":"message","nope":1}`:       "nope",
		`{"source":"s","kind":"message","id":"x"}`:       "set by the server",
	} {
		_, err := v.Input([]byte(in))
		if err == nil || !IsValidation(err) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want it to mention %q", in, err, want)
		}
		t.Log(err)
	}
}

func TestValid(t *testing.T) {
	v, err := New()
	if err != nil {
		t.Fatal(err)
	}
	in, err := v.Input([]byte(`{"source":"s","kind":"progress","key":"k","action":"update","content":{"steps":{"total":3,"current":3}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if in.Typed.Source != "s" || in.Fields["key"] != "k" {
		t.Fatalf("decoded %+v", in)
	}
}

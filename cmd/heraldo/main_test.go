package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"version"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.HasPrefix(out.String(), "heraldo ") {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestNoArgs(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(nil, &out, &errb); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}

func TestUnknown(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"frob"}, &out, &errb); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}

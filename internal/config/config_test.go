package config

import "testing"

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestFromEnvDefault(t *testing.T) {
	c, err := FromEnv(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != DefaultAddr {
		t.Fatalf("Addr = %q, want %q", c.Addr, DefaultAddr)
	}
}

func TestFromEnvAddr(t *testing.T) {
	c, err := FromEnv(env(map[string]string{"HERALDO_ADDR": "127.0.0.1:9000"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != "127.0.0.1:9000" {
		t.Fatalf("Addr = %q", c.Addr)
	}
}

func TestFromEnvBadAddr(t *testing.T) {
	if _, err := FromEnv(env(map[string]string{"HERALDO_ADDR": "nope"})); err == nil {
		t.Fatal("want an error for an address without a port")
	}
}

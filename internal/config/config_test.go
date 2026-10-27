package config

import (
	"os"
	"path/filepath"
	"testing"
)

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

func TestFromEnvDBAndTokens(t *testing.T) {
	c, err := FromEnv(env(map[string]string{
		"HERALDO_DB":     "/data/heraldo.db",
		"HERALDO_TOKENS": "a write 2c624232cdd221771294dfbb310aca000a0df6ac8b66b696d90ef06fdefb64a3",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.DB != "/data/heraldo.db" {
		t.Fatalf("DB = %q", c.DB)
	}
	s, err := c.LoadTokens()
	if err != nil || s.Len() != 1 {
		t.Fatalf("tokens: %v %v", s, err)
	}
}

func TestFromEnvTokensBoth(t *testing.T) {
	if _, err := FromEnv(env(map[string]string{"HERALDO_TOKENS": "x", "HERALDO_TOKENS_FILE": "y"})); err == nil {
		t.Fatal("want an error when both token settings are set")
	}
}

func TestLoadTokensFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(path, []byte("# c\nr read 2c624232cdd221771294dfbb310aca000a0df6ac8b66b696d90ef06fdefb64a3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Config{TokensFile: path}.LoadTokens()
	if err != nil || s.Len() != 1 {
		t.Fatalf("%v %v", s, err)
	}
	if _, err := (Config{TokensFile: path + ".missing"}).LoadTokens(); err == nil {
		t.Fatal("want an error for a missing file")
	}
}

func TestFromEnvRules(t *testing.T) {
	c, err := FromEnv(env(map[string]string{"HERALDO_RULES": "/config/rules.yaml"}))
	if err != nil || c.RulesFile != "/config/rules.yaml" {
		t.Fatalf("RulesFile = %q, %v", c.RulesFile, err)
	}
	if c, _ := FromEnv(env(nil)); c.RulesFile != "" {
		t.Fatalf("default RulesFile = %q, want none", c.RulesFile)
	}
}

func TestFromEnvPush(t *testing.T) {
	c, err := FromEnv(env(nil))
	if err != nil || c.Push || c.HasAPNsKey() {
		t.Fatalf("default: push %v key %v err %v", c.Push, c.HasAPNsKey(), err)
	}
	c, err = FromEnv(env(map[string]string{
		"HERALDO_PUSH": "on", "HERALDO_APNS_KEY_FILE": "/keys/apns.p8",
		"HERALDO_APNS_KEY_ID": "KEYID00001", "HERALDO_APNS_TEAM_ID": "TEAMID0001",
	}))
	if err != nil || !c.Push || !c.HasAPNsKey() || c.APNsKeyID != "KEYID00001" || c.APNsTeamID != "TEAMID0001" {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := FromEnv(env(map[string]string{"HERALDO_PUSH": "maybe"})); err == nil {
		t.Fatal("accepted HERALDO_PUSH=maybe")
	}
	if _, err := FromEnv(env(map[string]string{"HERALDO_APNS_KEY_FILE": "/keys/apns.p8"})); err == nil {
		t.Fatal("accepted a key file without key and team ids")
	}
}

package config

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestFromEnvMCPHTTP(t *testing.T) {
	if c, err := FromEnv(env(nil)); err != nil || c.MCPHTTP {
		t.Fatalf("default: %v %v", c.MCPHTTP, err)
	}
	if c, err := FromEnv(env(map[string]string{"HERALDO_MCP_HTTP": "on"})); err != nil || !c.MCPHTTP {
		t.Fatalf("on: %v %v", c.MCPHTTP, err)
	}
	if _, err := FromEnv(env(map[string]string{"HERALDO_MCP_HTTP": "maybe"})); err == nil {
		t.Fatal("accepted HERALDO_MCP_HTTP=maybe")
	}
}

func TestFromEnvAPNsKeyInline(t *testing.T) {
	ids := map[string]string{"HERALDO_APNS_KEY_ID": "KEYID00001", "HERALDO_APNS_TEAM_ID": "TEAMID0001"}
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range ids {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	c, err := FromEnv(env(with("HERALDO_APNS_KEY", "pem")))
	if err != nil || !c.HasAPNsKey() || c.APNsKey != "pem" {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := FromEnv(env(with("HERALDO_APNS_KEY", "pem", "HERALDO_APNS_KEY_FILE", "/keys/apns.p8"))); err == nil {
		t.Fatal("accepted both HERALDO_APNS_KEY and HERALDO_APNS_KEY_FILE")
	}
	if _, err := FromEnv(env(map[string]string{"HERALDO_APNS_KEY": "pem"})); err == nil {
		t.Fatal("accepted an inline key without key and team ids")
	}
}

func TestLoadAPNsKey(t *testing.T) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	p8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	path := filepath.Join(t.TempDir(), "apns.p8")
	if err := os.WriteFile(path, p8, 0o600); err != nil {
		t.Fatal(err)
	}
	flat := strings.ReplaceAll(string(p8), "\n", `\n`)
	for name, c := range map[string]Config{
		"file":   {APNsKeyFile: path},
		"inline": {APNsKey: string(p8)},
		"flat":   {APNsKey: flat},
	} {
		got, err := c.LoadAPNsKey()
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !bytes.Equal(got, p8) {
			t.Errorf("%s: got %q, want the PEM text", name, got)
		}
	}
	if _, err := (Config{APNsKeyFile: path + ".missing"}).LoadAPNsKey(); err == nil {
		t.Fatal("want an error for a missing key file")
	}
}

func TestFromEnvHoldAndTrustedNets(t *testing.T) {
	c, err := FromEnv(env(nil))
	if err != nil || c.HoldMode != "off" || c.HoldDeadline != DefaultHoldDeadline || c.HoldDeadman != DefaultHoldDeadman || len(c.NotifyTrustedNets) != 0 {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	c, err = FromEnv(env(map[string]string{
		"HERALDO_HOLD": "on", "HERALDO_HOLD_DEADLINE": "45m", "HERALDO_HOLD_DEADMAN": "1h",
		"HERALDO_NOTIFY_TRUSTED_NETS": "192.0.2.7/24, 2001:db8::/32",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.HoldMode != "on" || c.HoldDeadline.Minutes() != 45 || c.HoldDeadman.Hours() != 1 {
		t.Fatalf("hold: %+v", c)
	}
	if len(c.NotifyTrustedNets) != 2 || c.NotifyTrustedNets[0].String() != "192.0.2.0/24" {
		t.Fatalf("nets = %v", c.NotifyTrustedNets)
	}
	for k, v := range map[string]string{
		"HERALDO_HOLD": "sometimes", "HERALDO_HOLD_DEADLINE": "30s", "HERALDO_HOLD_DEADMAN": "soon",
		"HERALDO_NOTIFY_TRUSTED_NETS": "192.0.2.7",
	} {
		if _, err := FromEnv(env(map[string]string{k: v})); err == nil {
			t.Errorf("%s=%s accepted", k, v)
		}
	}
}

func TestFromEnvStandby(t *testing.T) {
	c, err := FromEnv(env(nil))
	if err != nil || c.Standby {
		t.Fatalf("default role: %+v %v", c, err)
	}
	c, err = FromEnv(env(map[string]string{
		"HERALDO_ROLE": "standby", "HERALDO_PRIMARY_URL": "https://primary.example.net",
		"HERALDO_PRIMARY_TOKEN": "t", "HERALDO_PRIMARY_DOWN_AFTER": "15m",
	}))
	if err != nil || !c.Standby || c.PrimaryURL != "https://primary.example.net" || c.PrimaryDownAfter != 15*time.Minute || c.StandbyInterval != 0 {
		t.Fatalf("standby: %+v %v", c, err)
	}
	for _, bad := range []map[string]string{
		{"HERALDO_ROLE": "replica"},
		{"HERALDO_ROLE": "standby", "HERALDO_PRIMARY_URL": "https://primary.example.net"},
		{"HERALDO_ROLE": "standby", "HERALDO_PRIMARY_URL": "u", "HERALDO_PRIMARY_TOKEN": "t", "HERALDO_STANDBY_INTERVAL": "soon"},
	} {
		if _, err := FromEnv(env(bad)); err == nil {
			t.Fatalf("%v: no error", bad)
		}
	}
}

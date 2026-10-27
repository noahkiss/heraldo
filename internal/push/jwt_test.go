package push

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

// newKey makes a throwaway P-256 key and its .p8 PEM.
func newKey(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return k, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// verifyES256 checks a provider token's signature and returns its header
// and claims.
func verifyES256(pub *ecdsa.PublicKey, token string) (map[string]any, map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, nil, errors.New("jwt: want three parts")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return nil, nil, errors.New("jwt: bad signature encoding")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return nil, nil, errors.New("jwt: signature does not verify")
	}
	var h, c map[string]any
	for i, dst := range []*map[string]any{&h, &c} {
		raw, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil {
			return nil, nil, err
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			return nil, nil, err
		}
	}
	return h, c, nil
}

func TestParseKey(t *testing.T) {
	_, p8 := newKey(t)
	if _, err := ParseKey(p8); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "not pem", "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"} {
		if _, err := ParseKey([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestSignerCachesAndRefreshes(t *testing.T) {
	k, _ := newKey(t)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	s := &signer{key: k, keyID: "KEYID00001", teamID: "TEAMID0001", now: func() time.Time { return now }}
	tok, _, err := s.get(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	h, c, err := verifyES256(&k.PublicKey, tok)
	if err != nil {
		t.Fatal(err)
	}
	if h["alg"] != "ES256" || h["kid"] != "KEYID00001" || c["iss"] != "TEAMID0001" || c["iat"] != float64(now.Unix()) {
		t.Fatalf("header %v claims %v", h, c)
	}
	now = now.Add(TokenLifetime - time.Second)
	if again, _, _ := s.get(time.Time{}); again != tok {
		t.Fatal("token re-signed before its lifetime ended")
	}
	now = now.Add(time.Second)
	fresh, issued, _ := s.get(time.Time{})
	if fresh == tok || !issued.Equal(now) {
		t.Fatal("token not re-signed after its lifetime")
	}
	// Apple called the token stale: a token issued at or before then is
	// replaced.
	now = now.Add(time.Minute)
	if again, _, _ := s.get(now); again == fresh {
		t.Fatal("stale token kept")
	}
}

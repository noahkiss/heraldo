package push

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"sync"
	"time"
)

// TokenLifetime is how long the server uses one provider token. Apple
// refuses a token older than an hour and asks for a new one no more often
// than every 20 minutes.
const TokenLifetime = 50 * time.Minute

// ParseKey reads an APNs auth key: the .p8 file Apple hands out, a PEM
// PKCS #8 P-256 private key.
func ParseKey(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("APNs key: no PEM block")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("APNs key: %w", err)
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok || ec.Curve.Params().BitSize != 256 {
		return nil, errors.New("APNs key: not a P-256 key")
	}
	return ec, nil
}

// signer makes and caches the provider token, an ES256 JWT.
type signer struct {
	key    *ecdsa.PrivateKey
	keyID  string
	teamID string
	now    func() time.Time

	mu     sync.Mutex
	token  string
	issued time.Time
}

// get returns the cached token, or a new one when the cached one is older
// than TokenLifetime or was issued at or before stale.
func (s *signer) get(stale time.Time) (string, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if s.token != "" && now.Sub(s.issued) < TokenLifetime && s.issued.After(stale) {
		return s.token, s.issued, nil
	}
	tok, err := s.sign(now)
	if err != nil {
		return "", time.Time{}, err
	}
	s.token, s.issued = tok, now
	return tok, now, nil
}

func (s *signer) sign(now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	h, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": s.keyID})
	c, _ := json.Marshal(map[string]any{"iss": s.teamID, "iat": now.Unix()})
	unsigned := enc.EncodeToString(h) + "." + enc.EncodeToString(c)
	sum := sha256.Sum256([]byte(unsigned))
	r, sig, err := ecdsa.Sign(rand.Reader, s.key, sum[:])
	if err != nil {
		return "", err
	}
	// JWS ES256 is r and s, 32 bytes each, big-endian.
	out := make([]byte, 64)
	r.FillBytes(out[:32])
	sig.FillBytes(out[32:])
	return unsigned + "." + enc.EncodeToString(out), nil
}

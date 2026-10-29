package settings

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// SecretKeyEnv names the file that holds the key secrets are encrypted
// with at rest.
const SecretKeyEnv = "HERALDO_SECRET_KEY_FILE"

// LoadSecretKey reads a 32-byte AES-256 key from path. The file holds 64
// hex characters (openssl rand -hex 32), base64 of 32 bytes, or the 32 raw
// bytes.
func LoadSecretKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", SecretKeyEnv, err)
	}
	if len(b) == 32 {
		return b, nil
	}
	t := bytes.TrimSpace(b)
	if k, err := hex.DecodeString(string(t)); err == nil && len(k) == 32 {
		return k, nil
	}
	if k, err := base64.StdEncoding.DecodeString(string(t)); err == nil && len(k) == 32 {
		return k, nil
	}
	return nil, fmt.Errorf("%s: want 32 bytes: 64 hex characters, base64, or raw", SecretKeyEnv)
}

// sealed is a secret as the settings table stores it.
type sealed struct {
	Enc string `json:"enc"`
}

// aad binds a ciphertext to its key name, so one key's row cannot be moved
// to another.
func aad(name string) []byte { return []byte("heraldo-setting:" + name) }

func gcm(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// seal encrypts plain for the key name.
func seal(key []byte, name, plain string) (json.RawMessage, error) {
	g, err := gcm(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	ct := g.Seal(nonce, nonce, []byte(plain), aad(name))
	return json.Marshal(sealed{Enc: base64.StdEncoding.EncodeToString(ct)})
}

// errSealed is a stored secret this server cannot decrypt.
var errSealed = errors.New("the stored value does not decrypt with the key in " + SecretKeyEnv)

// unseal decrypts a stored secret. A nil key cannot.
func unseal(key []byte, name string, raw json.RawMessage) (string, error) {
	var s sealed
	if err := json.Unmarshal(raw, &s); err != nil || s.Enc == "" {
		return "", errors.New("the stored value is not an encrypted secret")
	}
	if key == nil {
		return "", errors.New("a secret is stored, but " + SecretKeyEnv + " is not set")
	}
	ct, err := base64.StdEncoding.DecodeString(s.Enc)
	if err != nil {
		return "", errSealed
	}
	g, err := gcm(key)
	if err != nil {
		return "", err
	}
	if len(ct) < g.NonceSize() {
		return "", errSealed
	}
	plain, err := g.Open(nil, ct[:g.NonceSize()], ct[g.NonceSize():], aad(name))
	if err != nil {
		return "", errSealed
	}
	return string(plain), nil
}

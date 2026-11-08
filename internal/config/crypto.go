package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	secretFileName   = ".secret.key"
	envelopePrefixV1 = "enc:v1:"
)

// KeyRing holds domain-separated cryptographic subkeys derived from VOLO_SECRET_KEY
// via RFC 5869 HKDF-SHA256.
type KeyRing struct {
	dbSecretsKey     [32]byte // AES-256-GCM at-rest encryption for DB credentials (S3, SMTP, TOTP, Webhook)
	guestPassportKey [32]byte // HMAC-SHA256 signing key for persistent self-healing GuestPassport
	envFPPepperKey   [32]byte // Server-side HMAC pepper for hardware/environment fingerprint hashing
	shareTokenKey    [32]byte // Default HMAC seed for stateless timed ShareTokens
	sessionCSRFKey   [32]byte // HMAC key for CSRF & OAuth/Passkey state binding
}

// LoadOrInitKeyRing resolves VOLO_SECRET_KEY if provided (>= 16 chars required), or
// atomically loads/creates ${dataDir}/.secret.key with strict 0600 permissions.
func LoadOrInitKeyRing(explicitSecret string, dataDir string) (*KeyRing, string, error) {
	var master []byte
	var source string

	trimmed := strings.TrimSpace(explicitSecret)
	if trimmed != "" {
		if len(trimmed) < 16 {
			return nil, "", errors.New("config: VOLO_SECRET_KEY must be at least 16 characters long")
		}
		dk, err := pbkdf2.Key(sha256.New, trimmed, []byte("volo-master-explicit:v1"), 10_000, 32)
		if err != nil {
			return nil, "", fmt.Errorf("config: derive master key: %w", err)
		}
		master = dk
		source = "env"
	} else {
		if err := os.MkdirAll(dataDir, 0o750); err != nil {
			return nil, "", fmt.Errorf("config: create data dir %q: %w", dataDir, err)
		}
		keyPath := filepath.Join(dataDir, secretFileName)
		loaded, created, err := readOrCreateSecretFile(keyPath)
		if err != nil {
			return nil, "", err
		}
		master = loaded
		if created {
			source = "generated:" + keyPath
		} else {
			source = "file:" + keyPath
		}
	}

	kr := &KeyRing{
		dbSecretsKey:     hkdfSHA256(master, "volo-go:v1:db-secrets-aes-gcm"),
		guestPassportKey: hkdfSHA256(master, "volo-go:v1:guest-passport-hmac"),
		envFPPepperKey:   hkdfSHA256(master, "volo-go:v1:env-fp-pepper-hmac"),
		shareTokenKey:    hkdfSHA256(master, "volo-go:v1:share-token-hmac"),
		sessionCSRFKey:   hkdfSHA256(master, "volo-go:v1:session-csrf-hmac"),
	}
	return kr, source, nil
}

func readOrCreateSecretFile(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, false, fmt.Errorf("config: refusing symlinked secret key file %q", path)
		}
		if !info.Mode().IsRegular() {
			return nil, false, fmt.Errorf("config: secret key path %q is not a regular file", path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, false, fmt.Errorf("config: read secret key file %q: %w", path, err)
		}
		decoded, err := hex.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil || len(decoded) < 32 {
			return nil, false, fmt.Errorf("config: invalid secret key file %q (expected >= 64 hex chars)", path)
		}
		// Enforce 0600 on Unix if permissions drifted wider (Windows NTFS uses ACLs and reports 0666).
		if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
			_ = os.Chmod(path, 0o600)
		}
		return decoded, false, nil
	}
	if !os.IsNotExist(err) {
		return nil, false, fmt.Errorf("config: stat secret key file %q: %w", path, err)
	}

	buf := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return nil, false, fmt.Errorf("config: generate random master key: %w", err)
	}
	encoded := hex.EncodeToString(buf) + "\n"
	tmpPath := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("config: create temp secret key file: %w", err)
	}
	if _, err := f.WriteString(encoded); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return nil, false, fmt.Errorf("config: write secret key file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return nil, false, fmt.Errorf("config: sync secret key file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return nil, false, fmt.Errorf("config: close secret key file: %w", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(tmpPath, 0o600); err != nil {
			_ = os.Remove(tmpPath)
			return nil, false, fmt.Errorf("config: chmod 0600 secret key file: %w", err)
		}
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return nil, false, fmt.Errorf("config: rename secret key file: %w", err)
	}
	return buf, true, nil
}

// hkdfSHA256 implements RFC 5869 HKDF-Extract + HKDF-Expand for a 32-byte output key
// using stdlib crypto/hmac + crypto/sha256.
func hkdfSHA256(ikm []byte, info string) [32]byte {
	salt := []byte("volo-go-hkdf-salt-v1")
	extractor := hmac.New(sha256.New, salt)
	extractor.Write(ikm)
	prk := extractor.Sum(nil)

	expander := hmac.New(sha256.New, prk)
	expander.Write([]byte(info))
	expander.Write([]byte{0x01})
	var out [32]byte
	copy(out[:], expander.Sum(nil))
	return out
}

// SealString encrypts plaintext with AES-256-GCM bound to aad (e.g. table+column+rowID)
// and returns "enc:v1:<base64url(nonce||ciphertext||tag)>". Empty plaintext returns "".
func (kr *KeyRing) SealString(plaintext string, aad string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	block, err := aes.NewCipher(kr.dbSecretsKey[:])
	if err != nil {
		return "", fmt.Errorf("config: aes cipher init: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("config: gcm init: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("config: gcm nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), []byte(aad))
	return envelopePrefixV1 + base64.RawURLEncoding.EncodeToString(sealed), nil
}

// OpenString decrypts an "enc:v1:..." envelope authenticated against aad.
// Empty ciphertext returns "". Tampered ciphertext or mismatched aad returns an error.
func (kr *KeyRing) OpenString(envelope string, aad string) (string, error) {
	if envelope == "" {
		return "", nil
	}
	if !strings.HasPrefix(envelope, envelopePrefixV1) {
		return "", errors.New("config: unrecognized encrypted secret envelope version")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(envelope, envelopePrefixV1))
	if err != nil {
		return "", fmt.Errorf("config: decode secret envelope: %w", err)
	}
	block, err := aes.NewCipher(kr.dbSecretsKey[:])
	if err != nil {
		return "", fmt.Errorf("config: aes cipher init: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("config: gcm init: %w", err)
	}
	if len(raw) < gcm.NonceSize()+gcm.Overhead() {
		return "", errors.New("config: truncated secret envelope")
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, []byte(aad))
	if err != nil {
		return "", errors.New("config: secret authentication failed (tampered data or wrong VOLO_SECRET_KEY)")
	}
	return string(pt), nil
}

// ArticlePasswordAAD returns the canonical AES-256-GCM AAD for an article view password.
func ArticlePasswordAAD(id string) string {
	return "articles.password:" + id
}

// WebhookSecretAAD returns the canonical AES-256-GCM AAD for an outbound webhook signing secret.
func WebhookSecretAAD(id string) string {
	return "webhook:" + id
}

// CommentNotifyEmailAAD returns the canonical AES-256-GCM AAD for a comment reply notification email.
func CommentNotifyEmailAAD(id string) string {
	return "comments.notify_email:" + id
}

// OpenStringFallback decrypts an "enc:v1:..." envelope trying primaryAAD first,
// then each fallbackAAD in order to seamlessly support legacy AAD bindings.
func (kr *KeyRing) OpenStringFallback(ciphertext string, primaryAAD string, fallbackAADs ...string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}
	if kr == nil {
		return "", errors.New("config: keyring is nil")
	}
	pt, primaryErr := kr.OpenString(ciphertext, primaryAAD)
	if primaryErr == nil {
		return pt, nil
	}
	for _, fb := range fallbackAADs {
		if fb == primaryAAD {
			continue
		}
		if pt, err := kr.OpenString(ciphertext, fb); err == nil {
			return pt, nil
		}
	}
	return "", primaryErr
}

// GuestPassportKey returns a copy of the 32-byte HMAC key for GuestPassport signing.
func (kr *KeyRing) GuestPassportKey() [32]byte { return kr.guestPassportKey }

// EnvFPPepperKey returns a copy of the 32-byte HMAC pepper for env_fp hashing.
func (kr *KeyRing) EnvFPPepperKey() [32]byte { return kr.envFPPepperKey }

// ShareTokenKey returns a copy of the 32-byte HMAC seed for ShareToken signing.
func (kr *KeyRing) ShareTokenKey() [32]byte { return kr.shareTokenKey }

// SessionCSRFKey returns a copy of the 32-byte HMAC key for CSRF tokens.
func (kr *KeyRing) SessionCSRFKey() [32]byte { return kr.sessionCSRFKey }

// CheckSecretKeyFilePerm verifies the on-disk .secret.key file permissions (used by volo doctor).
func CheckSecretKeyFilePerm(dataDir string) (exists bool, mode os.FileMode, err error) {
	p := filepath.Join(dataDir, secretFileName)
	st, err := os.Lstat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return false, 0, nil
		}
		return false, 0, err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return true, st.Mode(), fmt.Errorf("secret file %q is a symlink", p)
	}
	return true, st.Mode().Perm(), nil
}

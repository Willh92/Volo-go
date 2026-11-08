package auth

import (
	"crypto/md5"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	argon2Time            uint32 = 2
	argon2Memory          uint32 = 19 * 1024 // 19 MiB (OWASP recommended minimum for t=2, p=1)
	argon2Threads         uint8  = 1
	argon2SaltLen                = 16
	argon2KeyLen          uint32 = 32
	maxArgon2VerifyMemory uint32 = 128 * 1024 // Allow existing 64 MiB+ PHC hashes to verify seamlessly

	legacyPBKDF2Prefix  = "pbkdf2$sha256$"
	legacyBoloMD5Prefix = "bolomd5x2$"
)

var argonSem = make(chan struct{}, 2)

func deriveArgon2Key(password, salt []byte, timeCost, mem uint32, threads uint8, keyLen uint32) []byte {
	argonSem <- struct{}{}
	defer func() { <-argonSem }()
	return argon2.IDKey(password, salt, timeCost, mem, threads, keyLen)
}

const (
	// MinPasswordLength is the minimum allowed password length in runes.
	MinPasswordLength = 6
	// MaxPasswordLength is the maximum allowed password length in runes.
	MaxPasswordLength = 128
	// MaxLoginPasswordBytes caps raw login password length in bytes before KDF hashing.
	MaxLoginPasswordBytes = 256
)

// ErrWeakPassword is returned when a password violates length rules.
var ErrWeakPassword = errors.New("auth: password must be 6-128 characters")

// ValidatePasswordPolicy enforces password length rules (6-128 runes, not purely whitespace).
func ValidatePasswordPolicy(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return ErrWeakPassword
	}
	runes := []rune(raw)
	if len(runes) < MinPasswordLength || len(runes) > MaxPasswordLength {
		return ErrWeakPassword
	}
	return nil
}

// HashPassword hashes a plaintext password with Argon2id in standard PHC string format:
// $argon2id$v=19$m=65536,t=3,p=2$<b64_raw_salt>$<b64_raw_key>
func HashPassword(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("auth: password must not be empty")
	}
	salt := make([]byte, argon2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generate salt: %w", err)
	}
	dk := deriveArgon2Key([]byte(raw), salt, argon2Time, argon2Memory, argon2Threads, argon2KeyLen)
	b64Salt := base64.RawStdEncoding.EncodeToString(salt)
	b64Key := base64.RawStdEncoding.EncodeToString(dk)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argon2Memory, argon2Time, argon2Threads, b64Salt, b64Key), nil
}

// VerifyPassword checks raw against primary Argon2id passwordHash first; if passwordHash is empty,
// it verifies against legacyHash (supporting PBKDF2-HMAC-SHA256 and historical bolo double-MD5)
// and signals needsUpgrade=true so the caller silently upgrades the account to Argon2id on login.
func VerifyPassword(raw string, passwordHash string, legacyHash string) (matched bool, needsUpgrade bool) {
	if raw == "" {
		return false, false
	}
	if len(raw) > MaxLoginPasswordBytes {
		verifyDummyPassword("")
		return false, false
	}
	if strings.TrimSpace(passwordHash) != "" {
		ok, upgrade := verifyArgon2id(raw, passwordHash)
		return ok, upgrade
	}
	lh := strings.TrimSpace(legacyHash)
	if lh == "" {
		return false, false
	}
	if strings.HasPrefix(lh, legacyPBKDF2Prefix) {
		if verifyLegacyPBKDF2(raw, lh) {
			return true, true
		}
		return false, false
	}
	if strings.HasPrefix(lh, legacyBoloMD5Prefix) {
		expected := strings.TrimPrefix(lh, legacyBoloMD5Prefix)
		actual := legacyBoloStringToMD5Twice(raw)
		if subtle.ConstantTimeCompare([]byte(strings.ToLower(actual)), []byte(strings.ToLower(expected))) == 1 {
			return true, true
		}
		return false, false
	}
	return false, false
}

func verifyArgon2id(raw string, encoded string) (bool, bool) {
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=65536,t=3,p=2", "<salt>", "<key>"]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, false
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, false
	}
	var mem, timeCost uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &timeCost, &threads); err != nil {
		return false, false
	}
	if mem < 8*1024 || mem > maxArgon2VerifyMemory || timeCost < 1 || timeCost > 16 || threads < 1 || threads > 4 {
		return false, false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return false, false
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(expected) < 16 {
		return false, false
	}
	actual := deriveArgon2Key([]byte(raw), salt, timeCost, mem, threads, uint32(len(expected)))
	if subtle.ConstantTimeCompare(actual, expected) != 1 {
		return false, false
	}
	needsUpgrade := mem < argon2Memory || timeCost < argon2Time
	return true, needsUpgrade
}

// verifyLegacyPBKDF2 verifies bolo-solo v3.0 PasswordHashes format:
// pbkdf2$sha256$<iterations>$<base64url_raw_salt>$<base64url_raw_key>
func verifyLegacyPBKDF2(raw string, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "pbkdf2" || parts[1] != "sha256" {
		return false
	}
	iters, err := strconv.Atoi(parts[2])
	if err != nil || iters < 10_000 || iters > 1_000_000 {
		return false
	}
	salt, err := decodeBase64URLCompat(parts[3])
	if err != nil || len(salt) < 8 {
		return false
	}
	expected, err := decodeBase64URLCompat(parts[4])
	if err != nil || len(expected) < 16 {
		return false
	}
	actual, err := pbkdf2.Key(sha256.New, raw, salt, iters, len(expected))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func decodeBase64URLCompat(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "=")); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

// legacyBoloStringToMD5Twice reproduces bolo-solo MD5Utils.stringToMD5Twice byte-for-byte,
// including the historical Java BigInteger(1, secretBytes).toString(16) loop quirk:
//
//	for (int i = 0; i < 32 - md5code.length(); i++) { md5code = "0" + md5code; }
func legacyBoloStringToMD5Twice(plain string) string {
	return legacyBoloMD5Once(legacyBoloMD5Once(plain))
}

func legacyBoloMD5Once(plain string) string {
	sum := md5.Sum([]byte(plain))
	fullHex := hex.EncodeToString(sum[:])
	md5code := strings.TrimLeft(fullHex, "0")
	if md5code == "" {
		md5code = "0"
	}
	for i := 0; i < 32-len(md5code); i++ {
		md5code = "0" + md5code
	}
	return md5code
}

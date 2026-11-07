package auth

import (
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

func TestArgon2idHashAndVerify(t *testing.T) {
	if err := ValidatePasswordPolicy("12345"); err == nil {
		t.Fatal("expected <6 character password rejection")
	}
	if err := ValidatePasswordPolicy("      "); err == nil {
		t.Fatal("expected whitespace-only password rejection")
	}
	if err := ValidatePasswordPolicy("123456"); err != nil {
		t.Fatalf("expected 6-character password acceptance, got: %v", err)
	}
	if err := ValidatePasswordPolicy("Sovereign2026!"); err != nil {
		t.Fatalf("unexpected policy error: %v", err)
	}

	hash, err := HashPassword("Sovereign2026!")
	if err != nil {
		t.Fatalf("HashPassword failed: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("unexpected PHC format: %s", hash)
	}

	ok, upgrade := VerifyPassword("Sovereign2026!", hash, "")
	if !ok || upgrade {
		t.Fatalf("expected match without upgrade, got ok=%v upgrade=%v", ok, upgrade)
	}
	okWrong, _ := VerifyPassword("WrongPass2026!", hash, "")
	if okWrong {
		t.Fatal("expected wrong password to fail")
	}

	// Pre-existing 64 MiB (m=65536,t=3,p=2) Argon2id hashes MUST continue to verify seamlessly!
	salt64 := []byte("0123456789abcdef")
	dk64 := deriveArgon2Key([]byte("Existing64MiB!"), salt64, 3, 64*1024, 2, 32)
	hash64 := fmt.Sprintf("$argon2id$v=19$m=65536,t=3,p=2$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt64),
		base64.RawStdEncoding.EncodeToString(dk64),
	)
	if ok64, up64 := VerifyPassword("Existing64MiB!", hash64, ""); !ok64 || up64 {
		t.Fatalf("expected existing 64 MiB hash to verify seamlessly, got ok=%v upgrade=%v", ok64, up64)
	}
}

func TestLegacyPBKDF2AndBoloDoubleMD5SilentUpgradeVerification(t *testing.T) {
	// 1. Legacy bolo-solo v3.0 PBKDF2-HMAC-SHA256 vector
	salt := []byte("0123456789abcdef")
	dk, err := pbkdf2.Key(sha256.New, "LegacyPass99", salt, 120000, 32)
	if err != nil {
		t.Fatal(err)
	}
	legacyPBKDF2 := fmt.Sprintf("pbkdf2$sha256$120000$%s$%s",
		base64.RawURLEncoding.EncodeToString(salt),
		base64.RawURLEncoding.EncodeToString(dk),
	)

	ok, upgrade := VerifyPassword("LegacyPass99", "", legacyPBKDF2)
	if !ok || !upgrade {
		t.Fatalf("expected legacy PBKDF2 match with needsUpgrade=true, got ok=%v upgrade=%v", ok, upgrade)
	}
	if okBad, _ := VerifyPassword("Wrong999", "", legacyPBKDF2); okBad {
		t.Fatal("expected wrong PBKDF2 password to fail")
	}

	// 2. Legacy bolo-solo double-MD5 vector (including leading-zero BigInteger loop quirk)
	// Input "jk8289" has MD5 starting with "00...", exercising the exact (32-len) loop quirk!
	quirkDigest := legacyBoloStringToMD5Twice("jk8289")
	if len(quirkDigest) == 0 {
		t.Fatal("empty digest")
	}
	okMD5, upMD5 := VerifyPassword("jk8289", "", "bolomd5x2$"+quirkDigest)
	if !okMD5 || !upMD5 {
		t.Fatalf("expected legacy bolomd5x2 match with needsUpgrade=true, got ok=%v upgrade=%v", okMD5, upMD5)
	}

	// Once upgraded to Argon2id, legacyHash is ignored even if still present
	newHash, err := HashPassword("UpgradedPass2026")
	if err != nil {
		t.Fatal(err)
	}
	if okOld, _ := VerifyPassword("jk8289", newHash, "bolomd5x2$"+quirkDigest); okOld {
		t.Fatal("expected old legacy password to be rejected once Argon2id password_hash is set")
	}
}

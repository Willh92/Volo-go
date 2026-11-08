package auth

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestPasskeyRequiresUserVerificationFlagAndOptions(t *testing.T) {
	ctx := context.Background()
	svc, store, kr := setupAuthTestService(t)
	now := time.Now().UnixMilli()

	pw, _ := HashPassword("AdminPass2026!")
	_, err := store.ExecContext(ctx,
		`INSERT INTO users (id, username, email, display_name, avatar_url, bio, website_url, role, password_hash, legacy_hash, status, email_verified_at, preferred_locale, created_at, updated_at)
		 VALUES ('u_uv', 'uvadmin', 'uv@example.com', 'UV Admin', '', '', '', 'admin', ?, '', 'active', ?, 'en-US', ?, ?)`,
		pw, now, now, now)
	if err != nil {
		t.Fatal(err)
	}

	beginReg, err := BeginPasskeyCeremony(kr, "u_uv", "register", "https://blog.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if beginReg.UserVerification != "required" || beginReg.AuthenticatorSelection == nil || beginReg.AuthenticatorSelection["userVerification"] != "required" {
		t.Fatalf("expected register ceremony to enforce userVerification=required, got %+v", beginReg)
	}

	beginLogin, err := BeginPasskeyCeremony(kr, "", "login", "https://blog.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if beginLogin.UserVerification != "required" {
		t.Fatalf("expected login ceremony to enforce userVerification=required, got %+v", beginLogin)
	}

	// Attempt registration WITHOUT authFlagUserVerify (0x04) -> MUST fail!
	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credID := []byte("cred_without_uv_test")
	cdjNoUV, attNoUV := buildSyntheticWebAuthnRegistration(t, beginReg.ChallengeB64URL, "https://blog.example.com", "blog.example.com", credID, &privKey.PublicKey, 1)
	// Clear authFlagUserVerify bit inside attestationObject authData:
	// cbor prefix: 0xa1 0x68 "authData" 0x58 <len> <32-byte rpIdHash> <flags byte at offset 12+32=44>
	if idx := bytes.Index(attNoUV, []byte("authData")); idx >= 0 {
		flagsIdx := idx + len("authData") + 2 + 32
		attNoUV[flagsIdx] &^= authFlagUserVerify
	}
	if _, err := svc.RegisterPasskey(ctx, "u_uv", "No-UV Key", beginReg.StateToken, cdjNoUV, attNoUV); err == nil || !strings.Contains(err.Error(), "UV") {
		t.Fatalf("expected RegisterPasskey without UV flag to fail, got %v", err)
	}

	// Register valid UV passkey, then attempt assertion WITHOUT authFlagUserVerify -> MUST fail!
	beginReg2, _ := BeginPasskeyCeremony(kr, "u_uv", "register", "https://blog.example.com")
	cdjOK, attOK := buildSyntheticWebAuthnRegistration(t, beginReg2.ChallengeB64URL, "https://blog.example.com", "blog.example.com", credID, &privKey.PublicKey, 1)
	credRec, err := svc.RegisterPasskey(ctx, "u_uv", "TouchID Key", beginReg2.StateToken, cdjOK, attOK)
	if err != nil {
		t.Fatalf("RegisterPasskey with UV failed: %v", err)
	}

	cdjAssert, _ := json.Marshal(map[string]any{
		"type":      "webauthn.get",
		"challenge": beginLogin.ChallengeB64URL,
		"origin":    "https://blog.example.com",
	})
	rpHash := sha256.Sum256([]byte("blog.example.com"))
	authDataNoUV := make([]byte, 37)
	copy(authDataNoUV[:32], rpHash[:])
	authDataNoUV[32] = authFlagUserPresent // UP=1, UV=0!
	binary.BigEndian.PutUint32(authDataNoUV[33:37], 2)
	cHash := sha256.Sum256(cdjAssert)
	msg := append(append([]byte{}, authDataNoUV...), cHash[:]...)
	digest := sha256.Sum256(msg)
	sigNoUV, _ := ecdsa.SignASN1(rand.Reader, privKey, digest[:])

	if _, err := svc.LoginWithPasskey(ctx, beginLogin.StateToken, credRec.CredentialID, cdjAssert, authDataNoUV, sigNoUV, netip.MustParseAddr("1.1.1.1"), "", "Safari"); err == nil || !strings.Contains(err.Error(), "UV") {
		t.Fatalf("expected LoginWithPasskey without UV flag to fail, got %v", err)
	}
}

func TestReaderVerificationCodeCooldownAndRollingThreeCodeWindow(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := setupAuthTestService(t)

	t0 := time.Now().Add(-3 * time.Minute)
	code1, err := svc.issueReaderVerificationCodeAt("reader@example.com", t0)
	if err != nil {
		t.Fatalf("first code issue failed: %v", err)
	}

	// Resend after 20s (< 60s cooldown) MUST return ErrCodeCooldown!
	if _, err := svc.issueReaderVerificationCodeAt("reader@example.com", t0.Add(20*time.Second)); !errors.Is(err, ErrCodeCooldown) {
		t.Fatalf("expected ErrCodeCooldown within 60s window, got %v", err)
	}

	// Resend after 65s succeeds AND keeps code1 valid in the 3-code rolling window!
	code2, err := svc.issueReaderVerificationCodeAt("reader@example.com", t0.Add(65*time.Second))
	if err != nil {
		t.Fatalf("second code issue after 65s failed: %v", err)
	}
	_, err = svc.issueReaderVerificationCodeAt("reader@example.com", t0.Add(130*time.Second))
	if err != nil {
		t.Fatalf("third code issue after 130s failed: %v", err)
	}
	_ = code2

	// Reader completing registration with the ORIGINAL code1 from their first email MUST still succeed!
	u, err := svc.RegisterReader(ctx, "reader_one", "reader@example.com", "StrongReader2026!", code1, "en-US")
	if err != nil {
		t.Fatalf("expected original code1 in 3-code rolling window to remain valid, got %v", err)
	}
	if u.Username != "reader_one" {
		t.Fatalf("unexpected reader user: %+v", u)
	}
}

func TestLoginOverlongPasswordTimingGuardAndRevokeOtherSessions(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := setupAuthTestService(t)
	now := time.Now().UnixMilli()

	pw, _ := HashPassword("StaffSecret2026!")
	_, err := store.ExecContext(ctx,
		`INSERT INTO users (id, username, email, display_name, avatar_url, bio, website_url, role, password_hash, legacy_hash, status, email_verified_at, preferred_locale, created_at, updated_at)
		 VALUES ('u_staff', 'staffer', 'staff@example.com', 'Staffer', '', '', '', 'editor', ?, '', 'active', ?, 'en-US', ?, ?)`,
		pw, now, now, now)
	if err != nil {
		t.Fatal(err)
	}

	hugePwd := strings.Repeat("A", MaxLoginPasswordBytes+64)
	if _, err := svc.LoginStaff(ctx, "staffer", hugePwd, netip.MustParseAddr("1.1.1.1"), "", "UA", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials for >256B staff password, got %v", err)
	}
	if _, err := svc.LoginReader(ctx, "staff@example.com", hugePwd, netip.MustParseAddr("1.1.1.1"), "", "UA"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials for >256B reader password, got %v", err)
	}

	s1, _, err := svc.CreateSession(ctx, "u_staff", netip.MustParseAddr("1.1.1.1"), "", "UA1", now+3600_000)
	if err != nil {
		t.Fatal(err)
	}
	s2, _, err := svc.CreateSession(ctx, "u_staff", netip.MustParseAddr("1.1.1.2"), "", "UA2", now+3600_000)
	if err != nil {
		t.Fatal(err)
	}
	s3, _, err := svc.CreateSession(ctx, "u_staff", netip.MustParseAddr("1.1.1.3"), "", "UA3", now+3600_000)
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := svc.RevokeOtherSessionsCount(ctx, "u_staff", s1.ID)
	if err != nil || revoked != 2 {
		t.Fatalf("expected 2 other sessions revoked (s2=%s, s3=%s), got count=%d err=%v", s2.ID, s3.ID, revoked, err)
	}
}

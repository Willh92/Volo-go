package auth

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/guest"
	"github.com/Willh92/Volo-go/internal/repo"
)

func setupAuthTestService(t *testing.T) (*Service, *sqlx.Store, *config.KeyRing) {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(tmp, "auth.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatal(err)
	}
	kr, _, err := config.LoadOrInitKeyRing(strings.Repeat("s", 32), tmp)
	if err != nil {
		t.Fatal(err)
	}
	sm := repo.NewSettingsManager(store, kr, "zh-CN")
	svc := NewService(store, kr, sm, "https://blog.example.com")
	return svc, store, kr
}

func TestRBACIsolationSilentUpgradeTOTPRecoverySessionsAndAPITokens(t *testing.T) {
	ctx := context.Background()
	svc, store, kr := setupAuthTestService(t)
	now := time.Now().UnixMilli()

	// Seed Admin (with legacy PBKDF2 hash!) and Editor (with Argon2id hash)
	salt := []byte("legacy16bytesalt")
	dk, _ := pbkdf2.Key(sha256.New, "AdminPass2026!", salt, 120000, 32)
	legacyPBKDF2 := fmt.Sprintf("pbkdf2$sha256$120000$%s$%s",
		base64.RawURLEncoding.EncodeToString(salt),
		base64.RawURLEncoding.EncodeToString(dk))

	_, err := store.ExecContext(ctx,
		`INSERT INTO users (id, username, email, display_name, avatar_url, bio, website_url, role, password_hash, legacy_hash, status, email_verified_at, preferred_locale, created_at, updated_at)
		 VALUES ('u_admin', 'admin', 'admin@example.com', 'Admin', '', '', '', ?, '', ?, 'active', ?, 'zh-CN', ?, ?)`,
		domain.RoleAdmin, legacyPBKDF2, now, now, now)
	if err != nil {
		t.Fatal(err)
	}

	editorHash, _ := HashPassword("EditorPass2026!")
	_, err = store.ExecContext(ctx,
		`INSERT INTO users (id, username, email, display_name, avatar_url, bio, website_url, role, password_hash, legacy_hash, status, email_verified_at, preferred_locale, created_at, updated_at)
		 VALUES ('u_editor', 'editor', 'editor@example.com', 'Editor', '', '', '', ?, ?, '', 'active', ?, 'zh-CN', ?, ?)`,
		domain.RoleEditor, editorHash, now, now, now)
	if err != nil {
		t.Fatal(err)
	}

	clientIP := netip.MustParseAddr("198.51.100.10")

	// 1. Admin logs in -> legacy PBKDF2 hash MUST silently upgrade to Argon2id and wipe legacy_hash!
	loginRes, err := svc.LoginStaff(ctx, "admin", "AdminPass2026!", clientIP, "广东省·深圳市", "Mozilla/5.0", "")
	if err != nil || loginRes.Requires2FA {
		t.Fatalf("LoginStaff failed: %v", err)
	}
	dbAdmin, err := sqlx.Get[domain.User](ctx, store, `SELECT * FROM users WHERE id = 'u_admin'`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dbAdmin.PasswordHash, "$argon2id$") || dbAdmin.LegacyHash != "" {
		t.Fatalf("expected silent upgrade to Argon2id + empty legacy_hash, got hash=%q legacy=%q", dbAdmin.PasswordHash, dbAdmin.LegacyHash)
	}

	// 2. Staff accounts MUST be physically blocked from public /api/v1/reader/login!
	if _, rErr := svc.LoginReader(ctx, "admin", "AdminPass2026!", clientIP, "", "Mozilla/5.0"); !errors.Is(rErr, ErrForbidden) {
		t.Fatalf("expected Admin to be blocked on LoginReader with ErrForbidden, got %v", rErr)
	}
	if _, rErr := svc.LoginReader(ctx, "editor", "EditorPass2026!", clientIP, "", "Mozilla/5.0"); !errors.Is(rErr, ErrForbidden) {
		t.Fatalf("expected Editor to be blocked on LoginReader with ErrForbidden, got %v", rErr)
	}

	// 3. Reader email-verified registration + physical block from staff LoginStaff & RequireStaff!
	code, err := svc.IssueReaderVerificationCode("reader1@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, badErr := svc.RegisterReader(ctx, "reader1", "reader1@example.com", "ReaderPass99!", "000000", "zh-CN"); !errors.Is(badErr, ErrVerificationCode) {
		t.Fatalf("expected wrong verification code error, got %v", badErr)
	}
	readerUser, err := svc.RegisterReader(ctx, "reader1", "reader1@example.com", "ReaderPass99!", code, "zh-CN")
	if err != nil {
		t.Fatalf("RegisterReader failed: %v", err)
	}
	if err := RequireStaff(readerUser); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected RequireStaff to block Reader, got %v", err)
	}
	if _, sErr := svc.LoginStaff(ctx, "reader1", "ReaderPass99!", clientIP, "", "UA", ""); !errors.Is(sErr, ErrForbidden) {
		t.Fatalf("expected LoginStaff to reject Reader with ErrForbidden, got %v", sErr)
	}

	// 4. Guest-to-Reader One-Click Comment Claim: claims ONLY matching guest_id_hash, NEVER env_fp_hash!
	pm := guest.NewPassportManager(kr, "https://blog.example.com")
	recGP := httptest.NewRecorder()
	gpIdent, _ := pm.ResolveOrHeal(recGP, httptest.NewRequest(http.MethodGet, "/", nil), true)
	sharedEnvFP := guest.ComputeEnvFPHash(kr, "shared-office-macbook")

	// c_mine matches GuestIDHash; c_colleague has DIFFERENT GuestIDHash on the SAME EnvFPHash!
	_, err = store.ExecContext(ctx,
		`INSERT INTO comments (id, target_type, target_id, parent_id, reply_to_name, user_id, guest_id_hash, env_fp_hash, author_name, author_url, avatar_url, notify_email_ciphertext, notify_locale, client_ip, ip_masked, geo_region, content_md, status, is_deleted, created_at, updated_at)
		 VALUES
		 ('c_mine', 'article', 'art_admin', '', '', '', ?, ?, 'GuestMe', '', '', 'enc_email', 'zh-CN', '198.51.100.10', '198.51.*.*', '深圳', 'My guest comment', 'approved', 0, ?, ?),
		 ('c_other', 'article', 'art_admin', '', '', '', 'other_guest_hash_xyz', ?, 'Colleague', '', '', '', 'zh-CN', '198.51.100.10', '198.51.*.*', '深圳', 'Not mine', 'approved', 0, ?, ?)`,
		gpIdent.GuestIDHash, sharedEnvFP, now, now,
		sharedEnvFP, now, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := svc.ClaimGuestComments(ctx, readerUser, gpIdent)
	if err != nil || claimed != 1 {
		t.Fatalf("expected exactly 1 comment claimed via GuestIDHash (never env_fp!), got %d (%v)", claimed, err)
	}
	cOther, _ := sqlx.Get[domain.Comment](ctx, store, `SELECT * FROM comments WHERE id = 'c_other'`)
	if cOther.UserID != "" {
		t.Fatal("CRITICAL: ClaimGuestComments leaked across shared env_fp_hash!")
	}

	// 5. Editor Collaborator Isolation & PII Scrubbing!
	editorRes, err := svc.LoginStaff(ctx, "editor", "EditorPass2026!", clientIP, "", "UA", "")
	if err != nil {
		t.Fatal(err)
	}
	editorActor := editorRes.User
	if err := RequireAdmin(editorActor); !errors.Is(err, ErrForbidden) {
		t.Fatal("expected RequireAdmin to block Editor")
	}
	if CanManageArticle(editorActor, "u_admin") {
		t.Fatal("Editor must NOT manage Admin's articles")
	}
	if !CanManageArticle(editorActor, "u_editor") {
		t.Fatal("Editor MUST manage their own articles")
	}
	cMine, _ := sqlx.Get[domain.Comment](ctx, store, `SELECT * FROM comments WHERE id = 'c_mine'`)
	if _, cErr := AuthorizeAndScrubCommentForStaff(editorActor, cMine, "u_admin"); !errors.Is(cErr, ErrForbidden) {
		t.Fatalf("Editor must NOT see comments on Admin's articles, got %v", cErr)
	}
	scrubbed, cErr := AuthorizeAndScrubCommentForStaff(editorActor, cMine, "u_editor")
	if cErr != nil || scrubbed.NotifyEmailCiphertext != "" || scrubbed.ClientIP != "" || scrubbed.IPMasked == "" {
		t.Fatalf("Editor comment view failed PII scrubbing: err=%v scrubbed=%+v", cErr, scrubbed)
	}

	// 6. TOTP Enrollment, 2-Step Login Challenge, Replay Guard & Recovery Code Consumption!
	totpSecret, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	validOTP, _ := ComputeTOTPCode(totpSecret, time.Now())
	recoveryCodes, err := svc.EnrollTOTP(ctx, "u_admin", totpSecret, validOTP)
	if err != nil || len(recoveryCodes) != 8 {
		t.Fatalf("EnrollTOTP failed: %v / len=%d", err, len(recoveryCodes))
	}

	// Replaying the exact same OTP within the same 30s window MUST fail!
	step1, err := svc.LoginStaff(ctx, "admin", "AdminPass2026!", clientIP, "", "Mozilla/5.0", "")
	if err != nil || !step1.Requires2FA || step1.Session != nil {
		t.Fatalf("expected Requires2FA without premature session, got %+v (%v)", step1, err)
	}
	if _, _, repErr := svc.CompleteStaff2FA(ctx, step1.ChallengeToken, validOTP, false, clientIP, "", "Mozilla/5.0"); !errors.Is(repErr, ErrTOTPReplay) {
		t.Fatalf("expected ErrTOTPReplay on reused OTP code, got %v", repErr)
	}
	// Using one of the 8 single-use recovery codes succeeds ONCE, then fails on second attempt!
	step2Res, trustedCookie, err := svc.CompleteStaff2FA(ctx, step1.ChallengeToken, recoveryCodes[0], true, clientIP, "", "Mozilla/5.0")
	if err != nil || step2Res.Session == nil || trustedCookie == "" {
		t.Fatalf("CompleteStaff2FA with recovery code failed: %v", err)
	}
	if _, _, reuseErr := svc.CompleteStaff2FA(ctx, step1.ChallengeToken, recoveryCodes[0], false, clientIP, "", "Mozilla/5.0"); reuseErr == nil {
		t.Fatal("consumed recovery code must not work a second time!")
	}

	// 7. Session Kick-Offline & CSRF Token Verification
	if !svc.VerifyCSRFToken(step2Res.Session.ID, step2Res.CSRFToken) {
		t.Fatal("expected valid CSRF token")
	}
	if err := svc.RevokeSession(ctx, step2Res.User, step2Res.Session.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, authErr := svc.AuthenticateSession(ctx, step2Res.RawToken); !errors.Is(authErr, ErrUnauthorized) {
		t.Fatalf("expected kicked session to return ErrUnauthorized, got %v", authErr)
	}

	// 8. Scoped API Tokens (Editor blocked from backup:read; Admin allowed; scope enforced!)
	if _, _, patErr := svc.CreateAPIToken(ctx, editorActor, "Obsidian", []string{ScopeBackupRead}, 0); !errors.Is(patErr, ErrForbidden) {
		t.Fatalf("Editor must NOT mint backup:read API token, got %v", patErr)
	}
	_, rawPAT, err := svc.CreateAPIToken(ctx, editorActor, "Obsidian", []string{ScopeArticlesWrite}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, scErr := svc.AuthenticateAPIToken(ctx, rawPAT, ScopeArticlesWrite); scErr != nil {
		t.Fatalf("expected valid articles:write scope on PAT, got %v", scErr)
	}
	if _, _, scErr := svc.AuthenticateAPIToken(ctx, rawPAT, ScopeMediaWrite); !errors.Is(scErr, ErrForbidden) {
		t.Fatalf("expected ErrForbidden when using articles:write PAT on media:write endpoint, got %v", scErr)
	}
}

func TestWebAuthnPasskeyES256CeremonyAndCloneDetection(t *testing.T) {
	ctx := context.Background()
	svc, store, kr := setupAuthTestService(t)
	now := time.Now().UnixMilli()

	pw, _ := HashPassword("AdminPass2026!")
	_, err := store.ExecContext(ctx,
		`INSERT INTO users (id, username, email, display_name, avatar_url, bio, website_url, role, password_hash, legacy_hash, status, email_verified_at, preferred_locale, created_at, updated_at)
		 VALUES ('u_pk', 'pkadmin', 'pk@example.com', 'PK Admin', '', '', '', 'admin', ?, '', 'active', ?, 'zh-CN', ?, ?)`,
		pw, now, now, now)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Begin Passkey Registration
	beginReg, err := BeginPasskeyCeremony(kr, "u_pk", "register", "https://blog.example.com")
	if err != nil {
		t.Fatal(err)
	}

	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credID := []byte("0123456789abcdef_cred_id")
	regCDJ, attObj := buildSyntheticWebAuthnRegistration(t, beginReg.ChallengeB64URL, "https://blog.example.com", "blog.example.com", credID, &privKey.PublicKey, 1)

	credRec, err := svc.RegisterPasskey(ctx, "u_pk", "MacBook Touch ID", beginReg.StateToken, regCDJ, attObj)
	if err != nil {
		t.Fatalf("RegisterPasskey failed: %v", err)
	}

	// 2. Begin & Finish Passkey Assertion (signCount advances 1 -> 2)
	beginLogin, err := BeginPasskeyCeremony(kr, "", "login", "https://blog.example.com")
	if err != nil {
		t.Fatal(err)
	}
	loginCDJ, authData2, sig2 := buildSyntheticWebAuthnAssertion(t, beginLogin.ChallengeB64URL, "https://blog.example.com", "blog.example.com", privKey, 2)
	loginRes, err := svc.LoginWithPasskey(ctx, beginLogin.StateToken, credRec.CredentialID, loginCDJ, authData2, sig2, netip.MustParseAddr("1.1.1.1"), "", "Safari")
	if err != nil || loginRes.Session == nil {
		t.Fatalf("LoginWithPasskey failed: %v", err)
	}

	// 3. Simulated cloned authenticator replaying stale signCount=2 MUST trigger ErrPasskeyCloneDetected!
	beginClone, _ := BeginPasskeyCeremony(kr, "", "login", "https://blog.example.com")
	cloneCDJ, authDataStale, sigStale := buildSyntheticWebAuthnAssertion(t, beginClone.ChallengeB64URL, "https://blog.example.com", "blog.example.com", privKey, 2)
	_, cloneErr := svc.LoginWithPasskey(ctx, beginClone.StateToken, credRec.CredentialID, cloneCDJ, authDataStale, sigStale, netip.MustParseAddr("1.1.1.1"), "", "Safari")
	if !errors.Is(cloneErr, ErrPasskeyCloneDetected) {
		t.Fatalf("expected ErrPasskeyCloneDetected on signCount regression/stagnation, got %v", cloneErr)
	}
}

func buildSyntheticWebAuthnRegistration(t *testing.T, challengeB64, origin, rpID string, credID []byte, pub *ecdsa.PublicKey, signCount uint32) ([]byte, []byte) {
	t.Helper()
	cdj, _ := json.Marshal(map[string]any{
		"type":      "webauthn.create",
		"challenge": challengeB64,
		"origin":    origin,
	})

	rpHash := sha256.Sum256([]byte(rpID))
	var authData bytes.Buffer
	authData.Write(rpHash[:])
	authData.WriteByte(authFlagUserPresent | authFlagUserVerify | authFlagAttested)
	var sc [4]byte
	binary.BigEndian.PutUint32(sc[:], signCount)
	authData.Write(sc[:])
	authData.Write(make([]byte, 16)) // aaguid
	var cidLen [2]byte
	binary.BigEndian.PutUint16(cidLen[:], uint16(len(credID)))
	authData.Write(cidLen[:])
	authData.Write(credID)

	// Encode COSE ES256 map (5 entries): {1: 2, 3: -7, -1: 1, -2: x(32), -3: y(32)}
	xBytes := pub.X.FillBytes(make([]byte, 32))
	yBytes := pub.Y.FillBytes(make([]byte, 32))
	var cose bytes.Buffer
	cose.WriteByte(0xa5)           // map(5)
	cose.Write([]byte{0x01, 0x02}) // 1: 2 (EC2)
	cose.Write([]byte{0x03, 0x26}) // 3: -7 (ES256)
	cose.Write([]byte{0x20, 0x01}) // -1: 1 (P-256)
	cose.Write([]byte{0x21, 0x58, 0x20})
	cose.Write(xBytes) // -2: x
	cose.Write([]byte{0x22, 0x58, 0x20})
	cose.Write(yBytes) // -3: y
	authData.Write(cose.Bytes())

	// Encode attestationObject map(1): {"authData": bstr}
	ad := authData.Bytes()
	var att bytes.Buffer
	att.WriteByte(0xa1) // map(1)
	att.WriteByte(0x68) // tstr(8) "authData"
	att.WriteString("authData")
	if len(ad) < 256 {
		att.Write([]byte{0x58, byte(len(ad))})
	} else {
		att.WriteByte(0x59)
		var l2 [2]byte
		binary.BigEndian.PutUint16(l2[:], uint16(len(ad)))
		att.Write(l2[:])
	}
	att.Write(ad)
	return cdj, att.Bytes()
}

func buildSyntheticWebAuthnAssertion(t *testing.T, challengeB64, origin, rpID string, priv *ecdsa.PrivateKey, signCount uint32) ([]byte, []byte, []byte) {
	t.Helper()
	cdj, _ := json.Marshal(map[string]any{
		"type":      "webauthn.get",
		"challenge": challengeB64,
		"origin":    origin,
	})
	rpHash := sha256.Sum256([]byte(rpID))
	authData := make([]byte, 37)
	copy(authData[:32], rpHash[:])
	authData[32] = authFlagUserPresent | authFlagUserVerify
	binary.BigEndian.PutUint32(authData[33:37], signCount)

	cHash := sha256.Sum256(cdj)
	msg := append(append([]byte{}, authData...), cHash[:]...)
	digest := sha256.Sum256(msg)
	sig, err := ecdsa.SignASN1(rand.Reader, priv, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return cdj, authData, sig
}

func TestPasskeySingleUseChallengeAntiReplay(t *testing.T) {
	ctx := context.Background()
	svc, store, kr := setupAuthTestService(t)
	now := time.Now().UnixMilli()

	pw, _ := HashPassword("AdminPass2026!")
	_, err := store.ExecContext(ctx,
		`INSERT INTO users (id, username, email, display_name, avatar_url, bio, website_url, role, password_hash, legacy_hash, status, email_verified_at, preferred_locale, created_at, updated_at)
		 VALUES ('u_pk0', 'pkzero', 'pk0@example.com', 'PK Zero', '', '', '', 'admin', ?, '', 'active', ?, 'zh-CN', ?, ?)`,
		pw, now, now, now)
	if err != nil {
		t.Fatal(err)
	}

	// Register a passkey whose authenticator always emits signCount == 0 (e.g. synced passkey).
	beginReg, err := BeginPasskeyCeremony(kr, "u_pk0", "register", "https://blog.example.com")
	if err != nil {
		t.Fatal(err)
	}
	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credID := []byte("0000_synced_passkey_id")
	regCDJ, attObj := buildSyntheticWebAuthnRegistration(t, beginReg.ChallengeB64URL, "https://blog.example.com", "blog.example.com", credID, &privKey.PublicKey, 0)
	credRec, err := svc.RegisterPasskey(ctx, "u_pk0", "iCloud Keychain", beginReg.StateToken, regCDJ, attObj)
	if err != nil {
		t.Fatalf("RegisterPasskey signCount=0 failed: %v", err)
	}

	// Registration stateToken replay MUST fail!
	if _, err := svc.RegisterPasskey(ctx, "u_pk0", "Replay Reg", beginReg.StateToken, regCDJ, attObj); err == nil {
		t.Fatal("expected registration stateToken replay to be rejected")
	}

	// Login with signCount == 0 succeeds on first use of stateToken...
	beginLogin, err := BeginPasskeyCeremony(kr, "", "login", "https://blog.example.com")
	if err != nil {
		t.Fatal(err)
	}
	loginCDJ, authData0, sig0 := buildSyntheticWebAuthnAssertion(t, beginLogin.ChallengeB64URL, "https://blog.example.com", "blog.example.com", privKey, 0)
	if _, err := svc.LoginWithPasskey(ctx, beginLogin.StateToken, credRec.CredentialID, loginCDJ, authData0, sig0, netip.MustParseAddr("1.1.1.1"), "", "Safari"); err != nil {
		t.Fatalf("first LoginWithPasskey signCount=0 failed: %v", err)
	}

	// ...and MUST be rejected on replay of the same stateToken + assertion!
	if _, err := svc.LoginWithPasskey(ctx, beginLogin.StateToken, credRec.CredentialID, loginCDJ, authData0, sig0, netip.MustParseAddr("1.1.1.1"), "", "Safari"); !errors.Is(err, ErrPasskeyVerifyFailed) {
		t.Fatalf("expected replayed signCount=0 assertion to fail with ErrPasskeyVerifyFailed, got %v", err)
	}
}

func TestTrusted2FACookieInvalidatedOnPasswordAndTOTPRotation(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := setupAuthTestService(t)
	svc.SetPublicURL("https://updated.example.com/")
	if got := svc.PublicURL(); got != "https://updated.example.com" {
		t.Fatalf("PublicURL() = %q", got)
	}
	now := time.Now().UnixMilli()

	pw1, _ := HashPassword("FirstPass2026!")
	_, err := store.ExecContext(ctx,
		`INSERT INTO users (id, username, email, display_name, avatar_url, bio, website_url, role, password_hash, legacy_hash, status, email_verified_at, preferred_locale, created_at, updated_at)
		 VALUES ('u_2fa', 'secadmin', 'sec@example.com', 'Sec Admin', '', '', '', 'admin', ?, '', 'active', ?, 'zh-CN', ?, ?)`,
		pw1, now, now, now)
	if err != nil {
		t.Fatal(err)
	}

	secret1, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	code1, _ := ComputeTOTPCode(secret1, time.Now().Add(-30*time.Second))
	if _, err := svc.EnrollTOTP(ctx, "u_2fa", secret1, code1); err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}

	ip := netip.MustParseAddr("203.0.113.10")
	ua := "Mozilla/5.0 TestBrowser"
	step1, err := svc.LoginStaff(ctx, "secadmin", "FirstPass2026!", ip, "", ua, "")
	if err != nil || !step1.Requires2FA {
		t.Fatalf("expected step1 2FA challenge, got %+v err=%v", step1, err)
	}
	codeNow, _ := ComputeTOTPCode(secret1, time.Now())
	_, trustCookie, err := svc.CompleteStaff2FA(ctx, step1.ChallengeToken, codeNow, true, ip, "", ua)
	if err != nil || trustCookie == "" {
		t.Fatalf("CompleteStaff2FA failed: cookie=%q err=%v", trustCookie, err)
	}

	// Valid trustCookie skips 2FA when password & TOTP credential are unchanged:
	bypassRes, err := svc.LoginStaff(ctx, "secadmin", "FirstPass2026!", ip, "", ua, trustCookie)
	if err != nil || bypassRes.Requires2FA {
		t.Fatalf("expected valid trustCookie to bypass 2FA, got %+v err=%v", bypassRes, err)
	}

	// 1. Rotating TOTP secret MUST immediately invalidate old trustCookie!
	secret2, _ := GenerateTOTPSecret()
	code2, _ := ComputeTOTPCode(secret2, time.Now().Add(30*time.Second))
	recCodes2, err := svc.EnrollTOTP(ctx, "u_2fa", secret2, code2)
	if err != nil || len(recCodes2) == 0 {
		t.Fatalf("re-enroll TOTP: %v", err)
	}
	afterTOTPRotate, err := svc.LoginStaff(ctx, "secadmin", "FirstPass2026!", ip, "", ua, trustCookie)
	if err != nil || !afterTOTPRotate.Requires2FA {
		t.Fatalf("CRITICAL: old trustCookie was NOT invalidated after TOTP rotation!")
	}

	// Mint a new trust cookie for secret2, then change password -> MUST invalidate!
	_, trustCookie2, err := svc.CompleteStaff2FA(ctx, afterTOTPRotate.ChallengeToken, recCodes2[0], true, ip, "", ua)
	if err != nil || trustCookie2 == "" {
		t.Fatalf("CompleteStaff2FA #2 failed: %v", err)
	}
	pw2, _ := HashPassword("SecondPass2026!")
	if _, err := store.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = 'u_2fa'`, pw2); err != nil {
		t.Fatal(err)
	}
	afterPWChange, err := svc.LoginStaff(ctx, "secadmin", "SecondPass2026!", ip, "", ua, trustCookie2)
	if err != nil || !afterPWChange.Requires2FA {
		t.Fatalf("CRITICAL: trustCookie2 was NOT invalidated after password change!")
	}
}

func TestRenderQRCodeSVGAndMatrixInvariants(t *testing.T) {
	secret, err := GenerateTOTPSecret()
	if err != nil || secret == "" {
		t.Fatalf("GenerateTOTPSecret: %v", err)
	}
	shortURI := BuildOTPAuthURI("Volo", "admin", secret)
	longURI := BuildOTPAuthURI("Volo Sovereign Blog", "admin@blog.example.com", secret)

	for _, uri := range []string{shortURI, longURI} {
		svg := RenderQRCodeSVG(uri, 240)
		if !strings.HasPrefix(svg, `<svg xmlns="http://www.w3.org/2000/svg"`) || !strings.HasSuffix(svg, `</svg>`) {
			t.Fatalf("invalid SVG wrapper: %s", svg)
		}
		if !strings.Contains(svg, `width="240" height="240"`) || !strings.Contains(svg, `<path fill="#000000" d="M`) {
			t.Fatalf("missing expected SVG dimensions or path data: %s", svg)
		}

		mat, ok := encodeQRMatrix([]byte(uri))
		if !ok {
			t.Fatalf("encodeQRMatrix returned false for otpauth URI (%d bytes)", len(uri))
		}
		n := len(mat)
		if n < 21 || (n-17)%4 != 0 {
			t.Fatalf("invalid QR module dimension %d", n)
		}

		// Verify 7x7 finder corners + white separator ring + dark module + timing alternating modules
		for _, corner := range [][2]int{{0, 0}, {0, n - 7}, {n - 7, 0}} {
			r0, c0 := corner[0], corner[1]
			if !mat[r0][c0] || !mat[r0+6][c0+6] || !mat[r0+3][c0+3] {
				t.Fatalf("finder pattern outer/center dark modules missing at corner %v", corner)
			}
			if mat[r0+1][c0+1] || mat[r0+5][c0+5] {
				t.Fatalf("finder pattern inner white ring corrupted at corner %v", corner)
			}
		}
		// Always-dark module at (4*V + 9, 8) == (n - 8, 8)
		if !mat[n-8][8] {
			t.Fatalf("expected ISO always-dark module at (%d, 8)", n-8)
		}
		// Timing pattern alternating along row 6 between cols 8..10
		if !mat[6][8] || mat[6][9] || !mat[6][10] {
			t.Fatalf("timing pattern alternating invariant violated at row 6")
		}
	}
}

func TestCompleteStaffPrimaryLoginAndRevokeUserAPITokens(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := setupAuthTestService(t)
	now := time.Now().UnixMilli()

	pw, _ := HashPassword("StaffOIDC2026!")
	_, err := store.ExecContext(ctx,
		`INSERT INTO users (id, username, email, display_name, avatar_url, bio, website_url, role, password_hash, legacy_hash, status, email_verified_at, preferred_locale, created_at, updated_at)
		 VALUES
		 ('u_staff', 'staffsso', 'staff@example.com', 'Staff SSO', '', '', '', 'admin', ?, '', 'active', ?, 'en-US', ?, ?),
		 ('u_reader', 'readersso', 'reader@example.com', 'Reader SSO', '', '', '', 'reader', ?, '', 'active', ?, 'en-US', ?, ?),
		 ('u_disabled', 'disadmin', 'dis@example.com', 'Disabled Admin', '', '', '', 'admin', ?, '', 'disabled', ?, 'en-US', ?, ?)`,
		pw, now, now, now,
		pw, now, now, now,
		pw, now, now, now,
	)
	if err != nil {
		t.Fatal(err)
	}

	ip := netip.MustParseAddr("203.0.113.42")
	ua := "VoloTestBrowser/1.0"

	// Non-staff or disabled accounts must be rejected by CompleteStaffPrimaryLogin
	readerRow, _ := sqlx.Get[domain.User](ctx, store, `SELECT * FROM users WHERE id = 'u_reader'`)
	if _, err := svc.CompleteStaffPrimaryLogin(ctx, readerRow, ip, "", ua, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden for reader in CompleteStaffPrimaryLogin, got %v", err)
	}
	disabledRow, _ := sqlx.Get[domain.User](ctx, store, `SELECT * FROM users WHERE id = 'u_disabled'`)
	if _, err := svc.CompleteStaffPrimaryLogin(ctx, disabledRow, ip, "", ua, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden for disabled staff in CompleteStaffPrimaryLogin, got %v", err)
	}

	// Without TOTP -> mints session directly
	staffRow, _ := sqlx.Get[domain.User](ctx, store, `SELECT * FROM users WHERE id = 'u_staff'`)
	resNo2FA, err := svc.CompleteStaffPrimaryLogin(ctx, staffRow, ip, "US", ua, "")
	if err != nil || resNo2FA.Requires2FA || resNo2FA.Session == nil || resNo2FA.RawToken == "" {
		t.Fatalf("expected immediate session when TOTP is not enrolled, got %+v (%v)", resNo2FA, err)
	}

	// Enroll TOTP -> CompleteStaffPrimaryLogin MUST issue ChallengeToken unless valid trusted2FACookie is supplied
	secret, _ := GenerateTOTPSecret()
	otp, _ := ComputeTOTPCode(secret, time.Now())
	recCodes, err := svc.EnrollTOTP(ctx, "u_staff", secret, otp)
	if err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	resWith2FA, err := svc.CompleteStaffPrimaryLogin(ctx, staffRow, ip, "US", ua, "")
	if err != nil || !resWith2FA.Requires2FA || resWith2FA.ChallengeToken == "" || resWith2FA.Session != nil {
		t.Fatalf("expected ChallengeToken when TOTP is enabled and no trusted cookie is present, got %+v (%v)", resWith2FA, err)
	}
	_, trustCookie, err := svc.CompleteStaff2FA(ctx, resWith2FA.ChallengeToken, recCodes[0], true, ip, "US", ua)
	if err != nil || trustCookie == "" {
		t.Fatalf("CompleteStaff2FA failed: %v", err)
	}
	resTrusted, err := svc.CompleteStaffPrimaryLogin(ctx, staffRow, ip, "US", ua, trustCookie)
	if err != nil || resTrusted.Requires2FA || resTrusted.Session == nil {
		t.Fatalf("expected valid trusted2FACookie to bypass 2FA challenge, got %+v (%v)", resTrusted, err)
	}

	// Mint API tokens and verify RevokeUserAPITokens revokes all tokens for the user
	_, pat1, err := svc.CreateAPIToken(ctx, &staffRow, "CLI-1", []string{ScopeArticlesRead}, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, pat2, err := svc.CreateAPIToken(ctx, &staffRow, "CLI-2", []string{ScopeMediaWrite}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RevokeUserAPITokens(ctx, "u_staff"); err != nil {
		t.Fatalf("RevokeUserAPITokens: %v", err)
	}
	if _, _, err := svc.AuthenticateAPIToken(ctx, pat1, ScopeArticlesRead); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected pat1 revoked (ErrUnauthorized), got %v", err)
	}
	if _, _, err := svc.AuthenticateAPIToken(ctx, pat2, ScopeMediaWrite); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected pat2 revoked (ErrUnauthorized), got %v", err)
	}
}

package auth_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/auth"
	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/repo"
)

func TestCompleteStaff2FA_RecoveryCodeSurvivesSecretKeyChange(t *testing.T) {
	ctx := context.Background()
	store, err := sqlx.Open(ctx, sqlx.Options{Driver: "sqlite", DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer store.Close()
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	oldKeyRing, _, err := config.LoadOrInitKeyRing("old-volo-secret-key-for-totp-enroll-2026", t.TempDir())
	if err != nil {
		t.Fatalf("old keyring: %v", err)
	}
	settingsMgr := repo.NewSettingsManager(store, oldKeyRing, "en-US")
	_, _ = settingsMgr.Reload(ctx)

	svcOld := auth.NewService(store, oldKeyRing, settingsMgr, "https://example.com")

	pwHash, err := auth.HashPassword("StrongAdminPass#2026")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	now := time.Now().UnixMilli()
	adminID := "admin-bkp3"
	_, err = store.ExecContext(ctx,
		`INSERT INTO users (id, username, email, display_name, avatar_url, bio, website_url, role, password_hash, legacy_hash, status, email_verified_at, preferred_locale, created_at, updated_at)
		 VALUES (?, 'admin', 'admin@example.com', 'Admin', '', '', '', ?, ?, '', ?, ?, 'en-US', ?, ?)`,
		adminID, domain.RoleAdmin, pwHash, domain.UserStatusActive, now, now, now)
	if err != nil {
		t.Fatalf("insert admin: %v", err)
	}

	secretBase32, err := auth.GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("GenerateTOTPSecret: %v", err)
	}
	initCode, err := auth.ComputeTOTPCode(secretBase32, time.Now())
	if err != nil {
		t.Fatalf("ComputeTOTPCode: %v", err)
	}
	recoveryCodes, err := svcOld.EnrollTOTP(ctx, adminID, secretBase32, initCode)
	if err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	if len(recoveryCodes) == 0 {
		t.Fatal("expected non-empty recovery codes")
	}

	// Simulate restoring DB onto a new host with a completely different VOLO_SECRET_KEY (no old key in keyring).
	newKeyRing, _, err := config.LoadOrInitKeyRing("brand-new-unrelated-volo-secret-key-2027", t.TempDir())
	if err != nil {
		t.Fatalf("new keyring: %v", err)
	}
	svcNew := auth.NewService(store, newKeyRing, nil, "https://example.com")

	clientIP := netip.MustParseAddr("203.0.113.10")
	ua := "VoloTestBrowser/1.0"
	step1, err := svcNew.LoginStaff(ctx, "admin", "StrongAdminPass#2026", clientIP, "US", ua, "")
	if err != nil {
		t.Fatalf("LoginStaff with new keyring: %v", err)
	}
	if !step1.Requires2FA || step1.ChallengeToken == "" {
		t.Fatalf("expected 2FA challenge, got %+v", step1)
	}

	// Invalid code must still fail closed with ErrTOTPInvalid.
	if _, _, err := svcNew.CompleteStaff2FA(ctx, step1.ChallengeToken, "000000", false, clientIP, "US", ua); !errors.Is(err, auth.ErrTOTPInvalid) {
		t.Fatalf("expected ErrTOTPInvalid on wrong code, got %v", err)
	}

	// Valid recovery code MUST succeed even though totp.secret_ciphertext cannot be decrypted by newKeyRing!
	step2, _, err := svcNew.CompleteStaff2FA(ctx, step1.ChallengeToken, recoveryCodes[0], false, clientIP, "US", ua)
	if err != nil {
		t.Fatalf("CompleteStaff2FA with valid recovery code after VOLO_SECRET_KEY change failed: %v", err)
	}
	if step2 == nil || step2.Session == nil || step2.RawToken == "" {
		t.Fatalf("expected authenticated session, got %+v", step2)
	}

	// Reusing the same consumed single-use recovery code must fail.
	step1Again, err := svcNew.LoginStaff(ctx, "admin", "StrongAdminPass#2026", clientIP, "US", ua, "")
	if err != nil {
		t.Fatalf("second LoginStaff: %v", err)
	}
	if _, _, err := svcNew.CompleteStaff2FA(ctx, step1Again.ChallengeToken, recoveryCodes[0], false, clientIP, "US", ua); !errors.Is(err, auth.ErrTOTPInvalid) {
		t.Fatalf("expected consumed recovery code to be rejected, got %v", err)
	}
}

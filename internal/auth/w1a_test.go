package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/domain"
)

func TestAuthenticateAPIToken_EnforcesAdminRoleForBackupAndAdminScopes(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := setupAuthTestService(t)
	now := time.Now().UnixMilli()

	adminHash, _ := HashPassword("AdminPass2026!")
	editorHash, _ := HashPassword("EditorPass2026!")
	_, err := store.ExecContext(ctx,
		`INSERT INTO users (id, username, email, display_name, avatar_url, bio, website_url, role, password_hash, legacy_hash, status, email_verified_at, preferred_locale, created_at, updated_at)
		 VALUES ('u_adm', 'adm', 'adm@example.com', 'Adm', '', '', '', ?, ?, '', 'active', ?, 'zh-CN', ?, ?),
		        ('u_ed', 'ed', 'ed@example.com', 'Ed', '', '', '', ?, ?, '', 'active', ?, 'zh-CN', ?, ?)`,
		domain.RoleAdmin, adminHash, now, now, now,
		domain.RoleEditor, editorHash, now, now, now,
	)
	if err != nil {
		t.Fatal(err)
	}

	// Mint backup:read token as Admin, then demote user to Editor -> AuthenticateAPIToken MUST reject with ErrForbidden!
	admActor := &domain.User{ID: "u_adm", Role: domain.RoleAdmin, Status: domain.UserStatusActive}
	_, rawBackupTok, err := svc.CreateAPIToken(ctx, admActor, "backup-tok", []string{ScopeBackupRead, ScopeArticlesRead}, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken failed: %v", err)
	}
	if _, _, err := svc.AuthenticateAPIToken(ctx, rawBackupTok, ScopeArticlesRead); err != nil {
		t.Fatalf("expected Admin with backup:read token to authenticate: %v", err)
	}

	// Demote u_adm -> editor and verify backup:read scope on token triggers RequireAdmin even when endpoint asks for articles:read!
	if _, err := store.ExecContext(ctx, `UPDATE users SET role = ? WHERE id = 'u_adm'`, domain.RoleEditor); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.AuthenticateAPIToken(ctx, rawBackupTok, ScopeArticlesRead); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected demoted editor carrying backup:read token to fail RequireAdmin with ErrForbidden, got: %v", err)
	}

	// Editor with normal articles:read token must still succeed for articles:read, but fail if requiredScope is backup:read or admin.
	edActor := &domain.User{ID: "u_ed", Role: domain.RoleEditor, Status: domain.UserStatusActive}
	_, rawEditorTok, err := svc.CreateAPIToken(ctx, edActor, "ed-tok", []string{ScopeArticlesRead}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.AuthenticateAPIToken(ctx, rawEditorTok, ScopeArticlesRead); err != nil {
		t.Fatalf("expected Editor with articles:read token to succeed: %v", err)
	}
	if _, _, err := svc.AuthenticateAPIToken(ctx, rawEditorTok, ScopeBackupRead); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected Editor requesting backup:read to fail with ErrForbidden, got: %v", err)
	}
}

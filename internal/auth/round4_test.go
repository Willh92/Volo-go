package auth

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/pkg/id"
)

func TestUsernameFormatValidationEmailCollisionAndIdentDisambiguation(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := setupAuthTestService(t)

	for _, bad := range []string{"a", "user@domain.com", "bad/slash", `bad\slash`, "has space", "ctrl\x07char"} {
		if err := ValidateUsername(bad); err == nil {
			t.Fatalf("expected ValidateUsername(%q) to fail", bad)
		}
		if _, err := svc.RegisterReader(ctx, bad, "new_"+id.Next()+"@example.com", "Str0ng!Pass#2026", "000000", "en-US"); err == nil {
			t.Fatalf("expected RegisterReader(%q) to fail", bad)
		}
	}

	// Register victim reader with email victim@example.com
	code, err := svc.IssueReaderVerificationCode("victim@example.com")
	if err != nil {
		t.Fatalf("IssueReaderVerificationCode: %v", err)
	}
	victim, err := svc.RegisterReader(ctx, "victim_user", "victim@example.com", "Vict1m!Pass#2026", code, "en-US")
	if err != nil {
		t.Fatalf("RegisterReader victim: %v", err)
	}

	// EnsureUsernameNotMatchingEmail blocks exact/case-insensitive match with another user's email
	if err := svc.EnsureUsernameNotMatchingEmail(ctx, "VICTIM@example.com", ""); err == nil || !strings.Contains(err.Error(), "conflicts with an existing email") {
		t.Fatalf("expected email collision error when username matches existing user email, got %v", err)
	}

	// Simulate pre-existing legacy row whose username was migrated as "victim@example.com" with a different password:
	// LoginReader("victim@example.com", ...) MUST prioritize email match (victim_user) over username match!
	spoofHash, err := HashPassword("Spoof!Pass#2026")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExecContext(ctx,
		`INSERT INTO users (id, username, email, display_name, avatar_url, bio, website_url, role, password_hash, legacy_hash, status, email_verified_at, preferred_locale, created_at, updated_at)
		 VALUES (?, ?, ?, ?, '', '', '', ?, ?, '', ?, 1700000000000, 'en-US', 1700000000000, 1700000000000)`,
		"usr_legacy_spoof", "victim@example.com", "spoof-real@example.com", "Spoof", domain.RoleReader, spoofHash, domain.UserStatusActive); err != nil {
		t.Fatal(err)
	}

	res, err := svc.LoginReader(ctx, "victim@example.com", "Vict1m!Pass#2026", netip.MustParseAddr("127.0.0.1"), "", "GoTest")
	if err != nil || res == nil || res.User == nil {
		t.Fatalf("expected LoginReader by victim email to resolve victim_user, got err=%v res=%+v", err, res)
	}
	if res.User.ID != victim.ID {
		t.Fatalf("expected victim ID %s when logging in with victim@example.com, got %s", victim.ID, res.User.ID)
	}
}

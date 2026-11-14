package notify

import (
	"context"
	"encoding/json"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/repo"
)

func TestDispatcher_PublicURLFallbackAndSetPublicURL(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(tmp, "notify.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatal(err)
	}
	kr, _, err := config.LoadOrInitKeyRing(strings.Repeat("n", 32), tmp)
	if err != nil {
		t.Fatal(err)
	}
	sm := repo.NewSettingsManager(store, kr, "zh-CN")
	if _, err := sm.Reload(ctx); err != nil {
		t.Fatal(err)
	}

	patchSite, _ := json.Marshal(map[string]any{
		"custom_vars": map[string]string{
			"public_url": "https://custom.example.com/",
		},
	})
	if _, _, err := sm.PatchDomain(ctx, domain.SettingsDomainSite, patchSite, 0); err != nil {
		t.Fatal(err)
	}

	disp := NewDispatcher(DispatcherOptions{
		Store:    store,
		Keys:     kr,
		Settings: sm,
	})
	defer func() { _ = disp.Close(ctx) }()

	if got := disp.getPublicURL(); got != "https://custom.example.com" {
		t.Fatalf("expected fallback to Site.CustomVars[public_url], got %q", got)
	}

	disp.SetPublicURL("https://override.example.com/")
	if got := disp.getPublicURL(); got != "https://override.example.com" {
		t.Fatalf("expected SetPublicURL override without trailing slash, got %q", got)
	}
}

func TestMailer_SMTPTargetIPSSRFValidation(t *testing.T) {
	m := NewMailer()

	unconditionallyBlocked := []string{
		"127.0.0.1",
		"::1",
		"169.254.169.254",
		"fe80::1",
		"100.64.0.10",
		"224.0.0.1",
		"0.0.0.0",
		"::",
	}
	for _, raw := range unconditionallyBlocked {
		ip := netip.MustParseAddr(raw)
		for _, mode := range []string{"tls", "starttls", "plain_internal"} {
			if err := m.validateSMTPTargetIP(ip, mode); err == nil {
				t.Fatalf("expected IP %s to be unconditionally blocked in mode %q", raw, mode)
			}
		}
	}

	rfc1918 := []string{"10.0.0.5", "172.16.5.4", "192.168.1.20", "fd00::1234"}
	for _, raw := range rfc1918 {
		ip := netip.MustParseAddr(raw)
		if err := m.validateSMTPTargetIP(ip, "tls"); err == nil {
			t.Fatalf("expected RFC1918/ULA IP %s to be blocked in tls mode", raw)
		}
		if err := m.validateSMTPTargetIP(ip, "starttls"); err == nil {
			t.Fatalf("expected RFC1918/ULA IP %s to be blocked in starttls mode", raw)
		}
		if err := m.validateSMTPTargetIP(ip, "plain_internal"); err != nil {
			t.Fatalf("expected RFC1918/ULA IP %s to be allowed in plain_internal mode, got: %v", raw, err)
		}
	}

	pubIP := netip.MustParseAddr("93.184.216.34")
	if err := m.validateSMTPTargetIP(pubIP, "tls"); err != nil {
		t.Fatalf("expected public IP to be allowed in tls mode: %v", err)
	}
}

func TestDispatcher_PostPermalinkStyleClosure(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(tmp, "notify-permalink.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatal(err)
	}
	kr, _, err := config.LoadOrInitKeyRing(strings.Repeat("p", 32), tmp)
	if err != nil {
		t.Fatal(err)
	}
	sm := repo.NewSettingsManager(store, kr, "zh-CN")
	if _, err := sm.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	// Seed article with published_at = 2026-05-15T12:00:00Z (1778846400000 ms)
	const pubMS int64 = 1778846400000
	if _, err := store.ExecContext(ctx,
		`INSERT INTO articles (id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md, cover_url, status, published_at, scheduled_at, is_pinned, allow_comment, password_ciphertext, has_content_lock, audio_url, audio_duration_sec, audio_size_bytes, audio_mime, lang, translation_group, license_override, word_count, reading_minutes, view_count, comment_count, created_at, updated_at)
		 VALUES ('art-1', 'u1', '', 'Permalink Post', 'hello-style', '', '', 'Body', '', 'published', ?, 0, ?, ?, '', ?, '', 0, 0, '', 'zh-CN', '', '', 1, 1, 0, 0, ?, ?)`,
		pubMS, false, true, false, pubMS, pubMS); err != nil {
		t.Fatal(err)
	}

	disp := NewDispatcher(DispatcherOptions{
		Store:     store,
		Keys:      kr,
		Settings:  sm,
		PublicURL: "https://blog.example.com",
	})
	defer func() { _ = disp.Close(ctx) }()

	for _, tc := range []struct {
		style    string
		wantPath string
	}{
		{domain.PermalinkStyleShortP, "/p/hello-style"},
		{domain.PermalinkStyleArchives, "/archives/hello-style"},
		{domain.PermalinkStyleYearMonth, "/2026/05/hello-style"},
		{domain.PermalinkStylePosts, "/posts/hello-style"},
	} {
		patchSEO, _ := json.Marshal(map[string]any{"post_permalink_style": tc.style})
		if _, _, err := sm.PatchDomain(ctx, domain.SettingsDomainSEO, patchSEO, 0); err != nil {
			t.Fatal(err)
		}
		_, gotPath := disp.resolveTargetTitleAndPath(ctx, domain.TargetTypeArticle, "art-1")
		if gotPath != tc.wantPath {
			t.Fatalf("resolveTargetTitleAndPath(style=%s) = %q, want %q", tc.style, gotPath, tc.wantPath)
		}
	}
}

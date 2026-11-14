package repo

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
)

func TestSettingsManagerAtomicPatchSecretMaskingAndHotReload(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	kr, _, err := config.LoadOrInitKeyRing("unit-test-master-secret-key-2026", tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(tmpDir, "settings.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatal(err)
	}

	mgr := NewSettingsManager(store, kr, "zh-CN")

	// 1. Partial PATCH on "site": updates title without overwriting subtitle or license_template!
	_, v1, err := mgr.PatchDomain(ctx, "site", []byte(`{"title":"Studio Notes","social_links":[{"id":"gh","label":"GitHub","icon":"github","url":"https://github.com/octocat"}]}`), 0)
	if err != nil || v1 != 1 {
		t.Fatalf("PatchDomain(site) failed: v=%d err=%v", v1, err)
	}
	snap := mgr.Snapshot()
	if snap.Site.Title != "Studio Notes" || snap.Site.Subtitle != "记录 · 思考 · 分享" || len(snap.Site.SocialLinks) != 1 {
		t.Fatalf("unexpected site snapshot after partial PATCH: %+v", snap.Site)
	}

	// 2. Partial PATCH on nested "feed.podcast" without wiping feed.item_count
	_, _, err = mgr.PatchDomain(ctx, "feed", []byte(`{"podcast":{"title":"Volo Cast","author":"Alex"}}`), 0)
	if err != nil {
		t.Fatalf("PatchDomain(feed) failed: %v", err)
	}
	snap = mgr.Snapshot()
	if snap.Feed.Podcast.Title != "Volo Cast" || snap.Feed.ItemCount != 20 || !snap.Feed.EnableLLMSTxt {
		t.Fatalf("nested feed PATCH clobbered sibling fields: %+v", snap.Feed)
	}

	// 3. Storage secret encryption at rest + *Set masking + preserve on blank update + explicit clear
	val, ver, err := mgr.PatchDomain(ctx, "storage", []byte(`{"active_driver":"s3","s3_bucket":"blog-assets","s3_secret_key":"MY_RAW_S3_SECRET"}`), 0)
	if err != nil || ver != 1 {
		t.Fatalf("PatchDomain(storage) failed: ver=%d err=%v", ver, err)
	}
	stView := val.(domain.StorageSettings)
	if !stView.S3SecretKeySet || stView.S3Bucket != "blog-assets" {
		t.Fatalf("expected S3SecretKeySet=true, got %+v", stView)
	}

	// Verify DB never stores plaintext MY_RAW_S3_SECRET!
	rawRec, err := sqlx.Get[domain.SettingRecord](ctx, store, `SELECT * FROM settings WHERE domain = 'storage'`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rawRec.ValueJSON, "MY_RAW_S3_SECRET") || strings.Contains(rawRec.SecretCiphertext, "MY_RAW_S3_SECRET") {
		t.Fatalf("CRITICAL: plaintext S3 secret leaked in DB record: %+v", rawRec)
	}
	if !strings.HasPrefix(rawRec.SecretCiphertext, "enc:v1:") {
		t.Fatalf("expected AES-GCM enc:v1: ciphertext, got %q", rawRec.SecretCiphertext)
	}

	// Blank s3_secret_key in subsequent PATCH must preserve stored secret
	_, ver2, err := mgr.PatchDomain(ctx, "storage", []byte(`{"max_upload_mb":80,"s3_secret_key":""}`), ver)
	if err != nil || ver2 != 2 {
		t.Fatalf("second storage PATCH failed: ver=%d err=%v", ver2, err)
	}
	sec, err := mgr.GetStorageSecrets(ctx)
	if err != nil || sec.S3SecretKey != "MY_RAW_S3_SECRET" || mgr.Snapshot().Storage.MaxUploadMB != 80 {
		t.Fatalf("secret was not preserved on blank PATCH: sec=%+v snap=%+v err=%v", sec, mgr.Snapshot().Storage, err)
	}

	// Optimistic concurrency check: stale version must return ErrSettingsConflict
	if _, _, err := mgr.PatchDomain(ctx, "storage", []byte(`{"max_upload_mb":90}`), 1); !errors.Is(err, ErrSettingsConflict) {
		t.Fatalf("expected ErrSettingsConflict on stale version, got %v", err)
	}

	// Explicit clear_secrets removes stored secret cleanly
	_, _, err = mgr.PatchDomain(ctx, "storage", []byte(`{"active_driver":"local","clear_secrets":["s3_secret_key"]}`), ver2)
	if err != nil {
		t.Fatal(err)
	}
	if mgr.Snapshot().Storage.S3SecretKeySet {
		t.Fatal("expected S3SecretKeySet=false after clear_secrets")
	}

	// 4. EnablePJAX defaults to true even for legacy DB rows missing "enable_pjax", and respects explicit false
	if !mgr.Snapshot().Reading.EnablePJAX {
		t.Fatal("expected EnablePJAX=true by default")
	}
	if _, err := store.ExecContext(ctx, `INSERT INTO settings (domain, value_json, secret_ciphertext, version, updated_at) VALUES ('reading', '{"posts_per_page":20,"list_style":"card"}', '', 1, 1000)`); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if !mgr.Snapshot().Reading.EnablePJAX || mgr.Snapshot().Reading.PostsPerPage != 20 {
		t.Fatalf("legacy reading JSON missing enable_pjax must default EnablePJAX=true, got %+v", mgr.Snapshot().Reading)
	}
	if _, _, err := mgr.PatchDomain(ctx, "reading", []byte(`{"enable_pjax":false}`), 1); err != nil {
		t.Fatalf("PatchDomain(reading enable_pjax=false): %v", err)
	}
	if mgr.Snapshot().Reading.EnablePJAX {
		t.Fatal("expected EnablePJAX=false after explicit patch")
	}

	// 5. UTF-8 case-folding (U+023A Ⱥ, U+212A K) + </style> breakout + nested javascript: in CustomCSS
	trickyCSS := "/* \u023A\u212A\u023A */ body { background: url(javaJaVaScRiPt:script:alert(1)); } </style><script>alert(1)</script>"
	if _, _, err := mgr.PatchDomain(ctx, "appearance", []byte(`{"custom_css":`+`"`+strings.ReplaceAll(trickyCSS, `"`, `\"`)+`"}`), 0); err != nil {
		t.Fatalf("PatchDomain(appearance): %v", err)
	}
	gotCSS := mgr.Snapshot().Appearance.CustomCSS
	if strings.Contains(strings.ToLower(gotCSS), "</style") || strings.Contains(strings.ToLower(gotCSS), "javascript:") {
		t.Fatalf("CustomCSS failed to neutralize breakout/javascript: %q", gotCSS)
	}

	// 6. CustomHeadMeta & FooterText preserve Admin verification tags and third-party ad/analytics scripts while stripping NUL bytes
	rawHead := "<meta name=\"google-site-verification\" content=\"abc123\">\x00\r\n<script async src=\"https://pagead2.googlesyndication.com/pagead/js/adsbygoogle.js?client=ca-pub-123\" crossorigin=\"anonymous\"></script>"
	cleanHead := SanitizeCustomHeadMeta(rawHead)
	if strings.Contains(cleanHead, "\x00") || strings.Contains(cleanHead, "\r") {
		t.Fatalf("SanitizeCustomHeadMeta failed to strip NUL/CR bytes: %q", cleanHead)
	}
	if !strings.Contains(cleanHead, `google-site-verification`) || !strings.Contains(cleanHead, `googlesyndication.com`) {
		t.Fatalf("SanitizeCustomHeadMeta dropped valid Admin head tags/scripts: %q", cleanHead)
	}

	// 7. Reject javascript:/control-char/protocol-relative SocialLinks & FaviconURL on write
	if _, _, err := mgr.PatchDomain(ctx, "site", []byte(`{"favicon_url":"java\u0009script:alert(1)"}`), 1); err == nil {
		t.Fatal("expected error for control-obfuscated javascript: favicon_url")
	}
	if _, _, err := mgr.PatchDomain(ctx, "site", []byte(`{"favicon_url":"//evil.example/fav.ico"}`), 1); err == nil {
		t.Fatal("expected error for protocol-relative // favicon_url")
	}
}

func TestSecurityRiskControlAndRetentionSettingsNormalization(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	kr, _, err := config.LoadOrInitKeyRing("unit-test-master-secret-key-2026", tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(tmpDir, "sec_settings.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatal(err)
	}

	mgr := NewSettingsManager(store, kr, "zh-CN")
	defSec := mgr.Snapshot().Security
	if defSec.RiskWindowSeconds != 600 || defSec.RiskStrikeThreshold != 5 || defSec.RiskTier1Minutes != 15 || defSec.RiskTier2Minutes != 60 || defSec.RiskTier3Minutes != 1440 {
		t.Fatalf("unexpected default risk control settings: %+v", defSec)
	}
	if defSec.IPBanRetentionDays != 30 || defSec.AuditLogRetentionDays != 90 || defSec.PluginLogRetentionDays != 30 {
		t.Fatalf("unexpected default retention settings: %+v", defSec)
	}

	// Patch with canonicalized CIDRs, permanent tier3 (0), permanent retention (0)
	_, ver, err := mgr.PatchDomain(ctx, "security", []byte(`{
		"risk_window_seconds": 300,
		"risk_strike_threshold": 3,
		"risk_tier1_minutes": 10,
		"risk_tier2_minutes": 120,
		"risk_tier3_minutes": 0,
		"risk_whitelist_cidrs": ["198.51.100.7", "198.51.100.7/32", "203.0.113.42/24", "2001:db8::1"],
		"ip_ban_retention_days": 0,
		"audit_log_retention_days": 0,
		"plugin_log_retention_days": 14
	}`), 0)
	if err != nil {
		t.Fatalf("PatchDomain(security) failed: %v", err)
	}
	got := mgr.Snapshot().Security
	if got.RiskTier3Minutes != 0 || got.IPBanRetentionDays != 0 || got.AuditLogRetentionDays != 0 || got.PluginLogRetentionDays != 14 {
		t.Fatalf("expected 0 permanent tier3/retention to be preserved, got %+v", got)
	}
	wantCIDRs := []string{"198.51.100.7/32", "203.0.113.0/24", "2001:db8::1/128"}
	if strings.Join(got.RiskWhitelistCIDRs, ",") != strings.Join(wantCIDRs, ",") {
		t.Fatalf("unexpected canonicalized whitelist CIDRs: got=%v want=%v", got.RiskWhitelistCIDRs, wantCIDRs)
	}

	// Reject malformed CIDR/IP in risk_whitelist_cidrs
	if _, _, err := mgr.PatchDomain(ctx, "security", []byte(`{"risk_whitelist_cidrs":["not-an-ip"]}`), ver); err == nil {
		t.Fatal("expected error on invalid risk_whitelist_cidrs entry")
	}
}


package config

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFourLayerPrecedenceAndZeroConfig(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Zero-config startup (no .env file, no env vars, only dataDir override)
	cfg, err := LoadWithEnviron(CLIOverrides{
		EnvFile: filepath.Join(tmpDir, "nonexistent.env"),
		DataDir: &tmpDir,
	}, nil)
	if err != nil {
		t.Fatalf("zero-config Load failed: %v", err)
	}
	if cfg.ListenAddr != DefaultListenAddr {
		t.Fatalf("expected default listen addr %q, got %q", DefaultListenAddr, cfg.ListenAddr)
	}
	if cfg.DBDriver != "sqlite" || cfg.DBDSN != filepath.Join(tmpDir, "volo.db") {
		t.Fatalf("unexpected default DB: driver=%q dsn=%q", cfg.DBDriver, cfg.DBDSN)
	}
	if !cfg.GeoAccessEnabled {
		t.Fatal("expected GeoAccessEnabled=true by default")
	}
	exists, mode, err := CheckSecretKeyFilePerm(tmpDir)
	if err != nil || !exists || mode != 0o600 {
		t.Fatalf("expected auto-generated 0600 .secret.key, exists=%v mode=%o err=%v", exists, mode, err)
	}

	// 2. Write .env file (Layer 3)
	envFile := filepath.Join(tmpDir, ".env")
	dotEnvBody := `
# Sample .env
VOLO_LISTEN_ADDR="127.0.0.1:9001" # inline comment
VOLO_PUBLIC_URL='https://envfile.example.com/'
VOLO_ADMIN_PATH=/secret-desk
VOLO_GEO_ACCESS_ENABLED=false
VOLO_DEFAULT_LOCALE=en-US
`
	if err := os.WriteFile(envFile, []byte(dotEnvBody), 0o600); err != nil {
		t.Fatal(err)
	}

	// 3. Pass OS Env (Layer 2: overrides .env for VOLO_LISTEN_ADDR and VOLO_DEFAULT_LOCALE)
	osEnv := []string{
		"VOLO_LISTEN_ADDR=127.0.0.1:9002",
		"VOLO_DEFAULT_LOCALE=zh-TW",
	}

	// 4. Pass CLI Flag (Layer 1: overrides OS Env for ListenAddr)
	cliAddr := "127.0.0.1:9003"
	cfg2, err := LoadWithEnviron(CLIOverrides{
		EnvFile:    envFile,
		DataDir:    &tmpDir,
		ListenAddr: &cliAddr,
	}, osEnv)
	if err != nil {
		t.Fatalf("4-layer Load failed: %v", err)
	}
	if cfg2.ListenAddr != "127.0.0.1:9003" {
		t.Fatalf("CLI flag failed to override ENV: got %q", cfg2.ListenAddr)
	}
	if cfg2.DefaultLocale != "zh-TW" {
		t.Fatalf("OS ENV failed to override .env: got %q", cfg2.DefaultLocale)
	}
	if cfg2.PublicURL != "https://envfile.example.com" || cfg2.PublicOrigin != "https://envfile.example.com" {
		t.Fatalf(".env PublicURL normalization failed: url=%q origin=%q", cfg2.PublicURL, cfg2.PublicOrigin)
	}
	if cfg2.AdminPath != "/secret-desk" || !cfg2.IsCustomAdminPath {
		t.Fatalf("expected custom admin path /secret-desk, got %q (custom=%v)", cfg2.AdminPath, cfg2.IsCustomAdminPath)
	}
	if cfg2.GeoAccessEnabled {
		t.Fatal("expected GeoAccessEnabled=false from .env")
	}
}

func TestKeyRingAESGCMAndSymlinkRejection(t *testing.T) {
	tmpDir := t.TempDir()
	kr, _, err := LoadOrInitKeyRing("my-super-strong-volo-secret-key-2026", tmpDir)
	if err != nil {
		t.Fatalf("LoadOrInitKeyRing failed: %v", err)
	}

	sealed, err := kr.SealString("s3-secret-token-xyz", "settings:storage")
	if err != nil || !strings.HasPrefix(sealed, "enc:v1:") {
		t.Fatalf("SealString failed: %q, err=%v", sealed, err)
	}
	opened, err := kr.OpenString(sealed, "settings:storage")
	if err != nil || opened != "s3-secret-token-xyz" {
		t.Fatalf("OpenString failed: got %q, err=%v", opened, err)
	}
	if _, err := kr.OpenString(sealed, "settings:notifications"); err == nil {
		t.Fatal("expected AAD mismatch to fail authentication")
	}
	tampered := sealed[:len(sealed)-2] + "AA"
	if _, err := kr.OpenString(tampered, "settings:storage"); err == nil {
		t.Fatal("expected tampered ciphertext to fail authentication")
	}

	// Symlinked .secret.key must be rejected
	symDir := t.TempDir()
	realFile := filepath.Join(tmpDir, "dummy.key")
	_ = os.WriteFile(realFile, []byte(strings.Repeat("a", 64)), 0o600)
	_ = os.Symlink(realFile, filepath.Join(symDir, ".secret.key"))
	if _, _, err := LoadOrInitKeyRing("", symDir); err == nil {
		t.Fatal("expected symlinked .secret.key to be rejected")
	}
}

func TestSocketAdminPathAndTrustedProxiesValidation(t *testing.T) {
	s1, err := ParseListenSocketSpec("/run/volo/volo.sock")
	if err != nil || s1.Perm != 0o660 {
		t.Fatalf("expected default 0660 perm, got %+v err=%v", s1, err)
	}
	s2, err := ParseListenSocketSpec("/run/volo/volo.sock,0600")
	if err != nil || s2.Perm != 0o600 {
		t.Fatalf("expected 0600 perm, got %+v err=%v", s2, err)
	}
	for _, bad := range []string{"", ",0660", "/run/a.sock,0999", "/run/a.sock,0660,extra", "/" + strings.Repeat("a", 105)} {
		if _, err := ParseListenSocketSpec(bad); err == nil {
			t.Fatalf("expected invalid socket spec %q to fail", bad)
		}
	}

	for _, badPath := range []string{"/posts", "/api/panel", "/login", "/admin", "/bad$chars"} {
		if _, _, err := ValidateAdminPath(badPath); err == nil {
			t.Fatalf("expected reserved/invalid admin path %q to fail", badPath)
		}
	}
	norm, custom, err := ValidateAdminPath("/private-panel/v2/")
	if err != nil || !custom || norm != "/private-panel/v2" {
		t.Fatalf("unexpected ValidateAdminPath result: norm=%q custom=%v err=%v", norm, custom, err)
	}

	tp, err := ParseTrustedProxies("unix, 127.0.0.1, 10.0.0.0/8, 2001:db8::/32")
	if err != nil || !tp.TrustUnix || len(tp.Prefixes) != 3 {
		t.Fatalf("unexpected TrustedProxiesPolicy: %+v err=%v", tp, err)
	}
	if !tp.Prefixes[1].Contains(netip.MustParseAddr("10.24.1.9")) {
		t.Fatal("expected 10.24.1.9 to match 10.0.0.0/8")
	}
}

func TestAADHelpersOpenStringFallbackAndThreadSafeConfig(t *testing.T) {
	if got := ArticlePasswordAAD("a1"); got != "articles.password:a1" {
		t.Fatalf("ArticlePasswordAAD = %q", got)
	}
	if got := WebhookSecretAAD("w1"); got != "webhook:w1" {
		t.Fatalf("WebhookSecretAAD = %q", got)
	}
	if got := CommentNotifyEmailAAD("c1"); got != "comments.notify_email:c1" {
		t.Fatalf("CommentNotifyEmailAAD = %q", got)
	}

	tmpDir := t.TempDir()
	kr, _, err := LoadOrInitKeyRing("my-super-strong-volo-secret-key-2026", tmpDir)
	if err != nil {
		t.Fatalf("LoadOrInitKeyRing: %v", err)
	}

	// Primary AAD match
	ctPrimary, err := kr.SealString("art-pass", ArticlePasswordAAD("42"))
	if err != nil {
		t.Fatal(err)
	}
	pt, err := kr.OpenStringFallback(ctPrimary, ArticlePasswordAAD("42"), "articles:password:42")
	if err != nil || pt != "art-pass" {
		t.Fatalf("primary fallback open failed: pt=%q err=%v", pt, err)
	}

	// Legacy fallback AAD match
	ctLegacyComment, err := kr.SealString("reader@example.com", "comment:email")
	if err != nil {
		t.Fatal(err)
	}
	pt2, err := kr.OpenStringFallback(ctLegacyComment, CommentNotifyEmailAAD("c99"), "comments:email:c99", "comment:email")
	if err != nil || pt2 != "reader@example.com" {
		t.Fatalf("legacy fallback open failed: pt=%q err=%v", pt2, err)
	}

	// Wrong AAD must still fail closed
	if _, err := kr.OpenStringFallback(ctPrimary, ArticlePasswordAAD("999"), "articles:password:999"); err == nil {
		t.Fatal("expected wrong AAD across all fallbacks to fail")
	}
	if empty, err := kr.OpenStringFallback("", ArticlePasswordAAD("1")); err != nil || empty != "" {
		t.Fatalf("expected empty ciphertext to return empty nil, got %q %v", empty, err)
	}

	cfg, err := LoadWithEnviron(CLIOverrides{
		EnvFile: filepath.Join(tmpDir, "missing.env"),
		DataDir: &tmpDir,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			cfg.SetPublicURL("https://blog.example.com/")
			cfg.SetDBSpec("postgresql", "postgres://u:p@127.0.0.1:5432/volo", true)
		}
	}()
	for i := 0; i < 100; i++ {
		_ = cfg.GetPublicURL()
		_, _, _ = cfg.GetDBSpec()
	}
	<-done

	if got := cfg.GetPublicURL(); got != "https://blog.example.com" {
		t.Fatalf("GetPublicURL = %q, want https://blog.example.com", got)
	}
	drv, dsn, simple := cfg.GetDBSpec()
	if drv != "postgres" || dsn != "postgres://u:p@127.0.0.1:5432/volo" || !simple {
		t.Fatalf("unexpected GetDBSpec: drv=%q dsn=%q simple=%v", drv, dsn, simple)
	}
}

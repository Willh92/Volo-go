package security

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/repo"
)

func TestWAFWhitelistCrossWindowEscalationAndPermanentTier3(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	kr, _, err := config.LoadOrInitKeyRing("waf-risk-test-secret-2026", tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(tmpDir, "waf.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatal(err)
	}

	mgr := repo.NewSettingsManager(store, kr, "zh-CN")
	_, _, err = mgr.PatchDomain(ctx, "security", []byte(`{
		"risk_control_enabled": true,
		"waf_enabled": true,
		"waf_window_seconds": 60,
		"waf_max_requests": 10,
		"risk_window_seconds": 120,
		"risk_strike_threshold": 2,
		"risk_tier1_minutes": 10,
		"risk_tier2_minutes": 60,
		"risk_tier3_minutes": 0,
		"risk_whitelist_cidrs": ["198.51.100.0/24"]
	}`), 0)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{AdminPath: "/console"}
	waf := NewWAFEngine(cfg, store, mgr, nil)
	defer waf.Close()
	if err := waf.ReloadBans(ctx); err != nil {
		t.Fatal(err)
	}

	// 1. Whitelisted IP bypasses strikes AND global WAF rate shield
	wlAddr := netip.MustParseAddr("198.51.100.42")
	for i := 0; i < 10; i++ {
		waf.RecordStrike(ctx, wlAddr, ReasonScannerTrap, 5)
	}
	if banned, _ := waf.IsBanned(wlAddr); banned {
		t.Fatal("whitelisted IP must never be auto-banned by RecordStrike")
	}
	handler := waf.WrapHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for i := 0; i < 20; i++ {
		req := httptest.NewRequest(http.MethodGet, "http://example.com/posts", nil)
		req.RemoteAddr = "198.51.100.42:12345"
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("whitelisted IP hit WAF rate limit on req %d: status=%d", i, rr.Code)
		}
	}

	// 2. Non-whitelisted IP escalates across windows: Tier 1 (10m) -> expire -> Tier 2 (60m) -> expire -> Tier 3 (0 = permanent)
	attacker := netip.MustParseAddr("203.0.113.9")
	waf.RecordStrike(ctx, attacker, ReasonLoginFail, 2) // meets threshold=2 -> Occurrences=1 -> Tier 1 (10m)
	rec1, err := sqlx.Get[domain.SecurityIPBan](ctx, store, `SELECT * FROM security_ip_bans WHERE cidr = '203.0.113.9/32'`)
	if err != nil {
		t.Fatalf("expected tier-1 ban row: %v", err)
	}
	if rec1.Occurrences != 1 || rec1.ExpiresAt <= 0 {
		t.Fatalf("expected occurrences=1 with finite tier-1 expiry, got %+v", rec1)
	}

	// Simulate ban expiration + server restart (new WAFEngine with empty in-memory maps, relying on DB history!)
	pastMS := time.Now().Add(-2 * time.Hour).UnixMilli()
	if _, err := store.ExecContext(ctx, `UPDATE security_ip_bans SET expires_at = ? WHERE id = ?`, pastMS, rec1.ID); err != nil {
		t.Fatal(err)
	}
	waf2 := NewWAFEngine(cfg, store, mgr, nil)
	defer waf2.Close()
	if err := waf2.ReloadBans(ctx); err != nil {
		t.Fatal(err)
	}
	waf2.RecordStrike(ctx, attacker, ReasonLoginFail, 2) // 2nd historical offense -> Tier 2 (60m)
	rec2, err := sqlx.Get[domain.SecurityIPBan](ctx, store, `SELECT * FROM security_ip_bans WHERE cidr = '203.0.113.9/32'`)
	if err != nil {
		t.Fatal(err)
	}
	minTier2 := time.Now().Add(50 * time.Minute).UnixMilli()
	if rec2.Occurrences != 2 || rec2.ExpiresAt < minTier2 {
		t.Fatalf("expected occurrences=2 with ~60m tier-2 expiry, got %+v", rec2)
	}

	// Simulate 2nd expiration + strike -> Tier 3 configured as 0 => permanent ban (expires_at == 0)!
	if _, err := store.ExecContext(ctx, `UPDATE security_ip_bans SET expires_at = ? WHERE id = ?`, pastMS, rec2.ID); err != nil {
		t.Fatal(err)
	}
	if err := waf2.ReloadBans(ctx); err != nil {
		t.Fatal(err)
	}
	waf2.strikes = make(map[string]*strikeBucket)
	waf2.RecordStrike(ctx, attacker, ReasonScannerTrap, 2)
	rec3, err := sqlx.Get[domain.SecurityIPBan](ctx, store, `SELECT * FROM security_ip_bans WHERE cidr = '203.0.113.9/32'`)
	if err != nil {
		t.Fatal(err)
	}
	if rec3.Occurrences != 3 || rec3.ExpiresAt != 0 {
		t.Fatalf("expected occurrences=3 and permanent expires_at=0 on Tier 3, got %+v", rec3)
	}
	if banned, _ := waf2.IsBanned(attacker); !banned {
		t.Fatal("expected attacker to be permanently banned in RAM")
	}

	// 3. Active permanent manual ban is never downgraded by automated strikes
	manualIP := netip.MustParseAddr("192.0.2.77")
	if _, err := waf2.AddManualBan(ctx, "192.0.2.77", "manual perm", 0, netip.MustParseAddr("127.0.0.1")); err != nil {
		t.Fatal(err)
	}
	waf2.RecordStrike(ctx, manualIP, ReasonLoginFail, 10)
	manRec, err := sqlx.Get[domain.SecurityIPBan](ctx, store, `SELECT * FROM security_ip_bans WHERE cidr = '192.0.2.77/32'`)
	if err != nil {
		t.Fatal(err)
	}
	if manRec.ExpiresAt != 0 || manRec.Source != "manual" {
		t.Fatalf("manual permanent ban was unexpectedly modified: %+v", manRec)
	}
}

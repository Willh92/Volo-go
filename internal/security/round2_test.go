package security

import (
	"context"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/repo"
)

func TestWAFRecordStrikeNeverDowngradesPermanentManualBan(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	secret := strings.Repeat("w", 32)
	cfg, err := config.Load(config.CLIOverrides{
		SecretKey: &secret,
		DataDir:   &tmp,
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(tmp, "waf-r2.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatal(err)
	}
	settings := repo.NewSettingsManager(store, cfg.KeyRing, "en-US")
	_, _ = settings.Reload(ctx)

	now := time.Now().UnixMilli()
	// Insert a permanent manual ban (expires_at = 0, source = 'manual') for 203.0.113.55/32.
	_, err = store.ExecContext(ctx,
		`INSERT INTO security_ip_bans (id, cidr, source, reason_code, reason, note, occurrences, expires_at, created_at, updated_at)
		 VALUES ('ban_perm_1', '203.0.113.55/32', 'manual', 'admin_block', 'Manual permanent block', 'repeat attacker', 1, 0, ?, ?)`,
		now, now)
	if err != nil {
		t.Fatal(err)
	}

	waf := NewWAFEngine(cfg, store, settings, nil)
	defer waf.Close()
	if err := waf.ReloadBans(ctx); err != nil {
		t.Fatal(err)
	}

	attacker := netip.MustParseAddr("203.0.113.55")
	// Trigger automated temporary strikes (weight=10 exceeds threshold).
	triggered := waf.RecordStrike(ctx, attacker, "login_fail", 10)
	if !triggered {
		t.Fatal("expected strike threshold to trigger")
	}

	// 1. In-memory activeBans MUST still have expiresAt == 0 (permanent)!
	pfx := netip.MustParsePrefix("203.0.113.55/32")
	waf.bansMu.RLock()
	memExp := waf.activeBans[pfx]
	waf.bansMu.RUnlock()
	if memExp != 0 {
		t.Fatalf("CRITICAL: automated strike downgraded permanent in-memory ban to temporary expires_at=%d!", memExp)
	}

	// 2. Database row MUST preserve source='manual' and expires_at=0!
	rec, err := sqlx.Get[domain.SecurityIPBan](ctx, store, `SELECT * FROM security_ip_bans WHERE cidr = '203.0.113.55/32'`)
	if err != nil {
		t.Fatal(err)
	}
	if rec.ExpiresAt != 0 || rec.Source != "manual" || rec.ReasonCode != "admin_block" {
		t.Fatalf("CRITICAL: automated strike overwrote permanent manual DB ban: %+v", rec)
	}
}

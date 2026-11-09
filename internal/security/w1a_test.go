package security

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestGeoEngine_GeoUnknownActionAndAdminLockoutValidation(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(tmp, "sec.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatal(err)
	}
	kr, _, err := config.LoadOrInitKeyRing(strings.Repeat("k", 32), tmp)
	if err != nil {
		t.Fatal(err)
	}
	sm := repo.NewSettingsManager(store, kr, "zh-CN")
	if _, err := sm.Reload(ctx); err != nil {
		t.Fatal(err)
	}

	geo := NewGeoEngine("", sm)
	knownIP := netip.MustParseAddr("8.8.8.8")
	unknownIP := netip.MustParseAddr("198.51.100.77")
	geo.SetLookupOverride(func(addr netip.Addr) (GeoLocation, bool) {
		if addr == knownIP {
			return GeoLocation{CountryISO: "US"}, true
		}
		return GeoLocation{}, false
	})

	// Enable GeoPolicy with default=allow, unknown=deny
	patchDenyUnknown, _ := json.Marshal(map[string]any{
		"geo_policy_active":  true,
		"geo_default_action": "allow",
		"geo_unknown_action": "deny",
	})
	if _, _, err := sm.PatchDomain(ctx, domain.SettingsDomainSecurity, patchDenyUnknown, 0); err != nil {
		t.Fatal(err)
	}

	if !geo.Allows(knownIP) {
		t.Fatalf("expected knownIP to be allowed under default=allow")
	}
	if geo.Allows(unknownIP) {
		t.Fatalf("expected unknownIP to be denied when geo_unknown_action=deny")
	}

	// Admin lockout validation must reject unknown admin IP when unknownPolicy="deny"
	if err := geo.ValidateRulesWithUnknownAgainstAdminIP(nil, "allow", "deny", unknownIP); err == nil {
		t.Fatalf("expected ValidateRulesWithUnknownAgainstAdminIP to block lockout of unknown admin IP")
	}
	if err := geo.ValidateRulesAgainstAdminIP(nil, "allow", unknownIP); err == nil {
		t.Fatalf("expected backwards-compatible ValidateRulesAgainstAdminIP to honor active geo_unknown_action=deny")
	}
	if err := geo.ValidateRulesWithUnknownAgainstAdminIP(nil, "allow", "allow", unknownIP); err != nil {
		t.Fatalf("expected unknownPolicy=allow to permit unknown admin IP, got: %v", err)
	}

	// Flip geo_unknown_action back to allow and verify Allows(unknownIP) == true
	patchAllowUnknown, _ := json.Marshal(map[string]any{
		"geo_unknown_action": "allow",
	})
	if _, _, err := sm.PatchDomain(ctx, domain.SettingsDomainSecurity, patchAllowUnknown, 0); err != nil {
		t.Fatal(err)
	}
	if !geo.Allows(unknownIP) {
		t.Fatalf("expected unknownIP to be allowed when geo_unknown_action=allow")
	}
}

func TestWAFEngine_AuthRateBucketIsolation(t *testing.T) {
	waf := NewWAFEngine(nil, nil, nil, nil)
	defer waf.Close()

	attackerIP := netip.MustParseAddr("203.0.113.42")
	authScopes := []string{"login", "admin:totp", "2fa:verify", "reader_login", "reader_code"}

	// Exhaust each auth rate bucket (limit=2) for attackerIP.
	for _, sc := range authScopes {
		if !waf.AllowRate(sc, attackerIP, 2, 10*time.Minute) {
			t.Fatalf("expected attempt 1 for %s to pass", sc)
		}
		if !waf.AllowRate(sc, attackerIP, 2, 10*time.Minute) {
			t.Fatalf("expected attempt 2 for %s to pass", sc)
		}
		if waf.AllowRate(sc, attackerIP, 2, 10*time.Minute) {
			t.Fatalf("expected attempt 3 for %s to be rate-limited", sc)
		}
	}

	// Flood public non-auth buckets well beyond maxWAFRates (8192) to force eviction in w.rates.
	for i := 0; i < maxWAFRates+2500; i++ {
		pubScope := fmt.Sprintf("unlock:article-%d", i)
		waf.AllowRate(pubScope, attackerIP, 5, 10*time.Minute)
	}

	// Verify all auth scopes remain strictly blocked and were NOT evicted by public bucket churn!
	for _, sc := range authScopes {
		if waf.AllowRate(sc, attackerIP, 2, 10*time.Minute) {
			t.Fatalf("CRITICAL: auth rate bucket %q was evicted by high-cardinality public scopes!", sc)
		}
	}
}

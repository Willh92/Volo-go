package security

import (
	"context"
	"errors"
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
	"github.com/Willh92/Volo-go/internal/repo"
)

func TestResolveClientIPSpoofingDefenseAndIPv6Bucket(t *testing.T) {
	policy := config.TrustedProxiesPolicy{
		TrustUnix: true,
		Prefixes:  []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
	}

	// 1. Direct untrusted peer attempting to spoof X-Real-IP / X-Forwarded-For -> MUST be ignored!
	rSpoof := httptest.NewRequest(http.MethodGet, "/", nil)
	rSpoof.RemoteAddr = "198.51.100.77:54321"
	rSpoof.Header.Set("X-Real-IP", "1.1.1.1")
	rSpoof.Header.Set("X-Forwarded-For", "1.1.1.1, 8.8.8.8")
	gotSpoof := ResolveClientIP(rSpoof, policy)
	if gotSpoof.String() != "198.51.100.77" {
		t.Fatalf("untrusted peer spoofed client IP: got %s, want 198.51.100.77", gotSpoof)
	}

	// 2. Trusted reverse proxy (10.0.0.2) forwarding X-Forwarded-For with spoofed left hop -> picks right-most non-trusted hop!
	rProxy := httptest.NewRequest(http.MethodGet, "/", nil)
	rProxy.RemoteAddr = "10.0.0.2:44300"
	rProxy.Header.Set("X-Forwarded-For", "1.2.3.4, 198.51.100.88, 10.0.0.9")
	gotProxy := ResolveClientIP(rProxy, policy)
	if gotProxy.String() != "198.51.100.88" {
		t.Fatalf("expected right-most untrusted hop 198.51.100.88, got %s", gotProxy)
	}

	// 3. Trusted Unix Domain Socket peer forwarding X-Real-IP
	rUnix := httptest.NewRequest(http.MethodGet, "/", nil)
	rUnix.RemoteAddr = "@"
	rUnix.Header.Set("X-Real-IP", "2001:db8:abcd:1234::99")
	gotUnix := ResolveClientIP(rUnix, policy)
	if gotUnix.String() != "2001:db8:abcd:1234::99" {
		t.Fatalf("expected unix X-Real-IP, got %s", gotUnix)
	}

	// 4. Two distinct SLAAC addresses inside the same IPv6 /64 MUST share one rate-limit bucket!
	ip6A := netip.MustParseAddr("2408:8207:1234:5678:1111:2222:3333:4444")
	ip6B := netip.MustParseAddr("2408:8207:1234:5678:aaaa:bbbb:cccc:dddd")
	if RateLimitBucketKey(ip6A) != RateLimitBucketKey(ip6B) {
		t.Fatalf("expected identical /64 bucket for SLAAC siblings: %s vs %s", RateLimitBucketKey(ip6A), RateLimitBucketKey(ip6B))
	}
}

func TestOutboundSafeDialerBlocksSSRFDNSRebindingAndAllowlist(t *testing.T) {
	ctx := context.Background()

	// 1. Block cloud metadata (169.254.169.254), loopback, RFC1918, IPv4-mapped IPv6, non-HTTP schemes
	d := &OutboundSafeDialer{
		AllowedHosts: []string{"api.github.com", "*.trusted.example"},
	}

	badURLs := []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://127.0.0.1:8080/admin",
		"http://[::1]:8080/",
		"http://[::ffff:127.0.0.1]/",
		"http://10.0.0.5/internal",
		"http://192.168.1.1/",
		"file:///etc/passwd",
		"gopher://example.com/",
		"https://evil.example.org/payload", // not in AllowedHosts
		"https://user:pass@api.github.com/repos",
	}
	for _, u := range badURLs {
		if _, err := d.ValidateOutboundURL(u); err == nil {
			t.Fatalf("expected SSRF firewall to block %q, but it was allowed", u)
		}
	}
	if _, err := d.ValidateOutboundURL("https://sub.trusted.example/webhook"); err != nil {
		t.Fatalf("expected wildcard allowed host to pass URL check, got %v", err)
	}

	// 2. Simulate DNS Rebinding / Dual-A-Record attack where public hostname resolves to [8.8.8.8, 169.254.169.254]
	rebindDialer := &OutboundSafeDialer{
		Resolver: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return []netip.Addr{
				netip.MustParseAddr("8.8.8.8"),
				netip.MustParseAddr("169.254.169.254"),
			}, nil
		},
	}
	_, err := rebindDialer.DialContext(ctx, "tcp", "rebind.attacker.example:80")
	if !errors.Is(err, ErrSSRFBlocked) {
		t.Fatalf("expected ErrSSRFBlocked on multi-A-record metadata rebinding, got: %v", err)
	}
}

func TestWAFStealthAdmin404AutoBanGeoIPAndCircuitBreaker(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "data")
	adminPath := "/vault-ops"
	trusted := "10.0.0.0/8"

	cfg, err := config.LoadWithEnviron(config.CLIOverrides{
		DataDir:        &dataDir,
		AdminPath:      &adminPath,
		TrustedProxies: &trusted,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	store, err := sqlx.Open(ctx, sqlx.Options{Driver: "sqlite", DSN: cfg.DBDSN})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatal(err)
	}

	sm := repo.NewSettingsManager(store, cfg.KeyRing, "zh-CN")
	// Enable GeoIP blocking and risk control in settings
	if _, _, err := sm.PatchDomain(ctx, "security", []byte(`{"geo_policy_active":true,"risk_control_enabled":true}`), 0); err != nil {
		t.Fatal(err)
	}

	geo := NewGeoEngine("", sm)
	geo.SetLookupOverride(func(addr netip.Addr) (GeoLocation, bool) {
		if addr.String() == "93.184.216.34" {
			return GeoLocation{CountryISO: "US", Region: "California"}, true
		}
		return GeoLocation{CountryISO: "CN", Region: "广东省·深圳市"}, true
	})
	geo.SetRules([]domain.SecurityGeoRule{
		{ID: "r1", Name: "Block US", IsEnabled: true, Action: "deny", OnMiss: "next", Countries: "US"},
	})

	// Admin self-lockout guard MUST reject rules that block the admin's own IP!
	if lockErr := geo.ValidateRulesAgainstAdminIP(geo.rules, "allow", netip.MustParseAddr("93.184.216.34")); lockErr == nil {
		t.Fatal("expected ValidateRulesAgainstAdminIP to reject self-lockout rule")
	}

	waf := NewWAFEngine(cfg, store, sm, geo)
	handler := waf.WrapHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok:" + r.URL.Path))
	}))

	// 1. Stealth 404 cloaking on default /console when VOLO_ADMIN_PATH=/vault-ops
	reqStealth := httptest.NewRequest(http.MethodGet, "/console/articles", nil)
	reqStealth.RemoteAddr = "10.0.0.1:1234"
	reqStealth.Header.Set("X-Real-IP", "1.2.3.4")
	recStealth := httptest.NewRecorder()
	handler.ServeHTTP(recStealth, reqStealth)
	if recStealth.Code != http.StatusNotFound {
		t.Fatalf("expected stealth 404 on /console/articles, got %d", recStealth.Code)
	}

	// Real custom admin path /vault-ops reaches inner handler
	reqVault := httptest.NewRequest(http.MethodGet, "/vault-ops", nil)
	reqVault.RemoteAddr = "10.0.0.1:1234"
	reqVault.Header.Set("X-Real-IP", "1.2.3.4")
	recVault := httptest.NewRecorder()
	handler.ServeHTTP(recVault, reqVault)
	if recVault.Code != http.StatusOK {
		t.Fatalf("expected 200 on custom /vault-ops, got %d", recVault.Code)
	}

	// 2. Scanner traps trigger escalating auto-ban on IPv6 /64 subnet!
	for i := 0; i < 2; i++ {
		reqScan := httptest.NewRequest(http.MethodGet, "/.env", nil)
		reqScan.RemoteAddr = "10.0.0.1:1234"
		// Use two different host suffixes inside the SAME /64!
		if i == 0 {
			reqScan.Header.Set("X-Real-IP", "240e:123:4567:89ab::1")
		} else {
			reqScan.Header.Set("X-Real-IP", "240e:123:4567:89ab::ffff")
		}
		recScan := httptest.NewRecorder()
		handler.ServeHTTP(recScan, reqScan)
	}
	// Now a third SLAAC address in that /64 visiting "/" MUST be blocked with 403!
	reqBlocked := httptest.NewRequest(http.MethodGet, "/", nil)
	reqBlocked.RemoteAddr = "10.0.0.1:1234"
	reqBlocked.Header.Set("X-Real-IP", "240e:123:4567:89ab::9999")
	recBlocked := httptest.NewRecorder()
	handler.ServeHTTP(recBlocked, reqBlocked)
	if recBlocked.Code != http.StatusForbidden {
		t.Fatalf("expected 403 auto-ban across IPv6 /64 subnet, got %d", recBlocked.Code)
	}

	// 3. Manual self-ban guard refuses banning admin's own IP
	if _, selfErr := waf.AddManualBan(ctx, "1.2.3.0/24", "oops", 0, netip.MustParseAddr("1.2.3.4")); selfErr == nil || !strings.Contains(selfErr.Error(), "own active IP") {
		t.Fatalf("expected self-ban guard error, got: %v", selfErr)
	}

	// 4. GeoIP blocks US IP (93.184.216.34) with 403, UNTIL VOLO_GEO_ACCESS_ENABLED=false circuit breaker flips!
	reqGeo := httptest.NewRequest(http.MethodGet, "/", nil)
	reqGeo.RemoteAddr = "10.0.0.1:1234"
	reqGeo.Header.Set("X-Real-IP", "93.184.216.34")
	recGeo := httptest.NewRecorder()
	handler.ServeHTTP(recGeo, reqGeo)
	if recGeo.Code != http.StatusForbidden {
		t.Fatalf("expected 403 GeoIP block on 93.184.216.34, got %d", recGeo.Code)
	}

	// Flip emergency circuit breaker VOLO_GEO_ACCESS_ENABLED=false -> immediately passes 200 OK!
	cfg.GeoAccessEnabled = false
	recBypass := httptest.NewRecorder()
	handler.ServeHTTP(recBypass, reqGeo)
	if recBypass.Code != http.StatusOK {
		t.Fatalf("expected 200 OK when VOLO_GEO_ACCESS_ENABLED=false circuit breaker active, got %d", recBypass.Code)
	}

	// 5. Rate limiter test
	ipRate := netip.MustParseAddr("8.8.4.4")
	if !waf.AllowRate("comment", ipRate, 2, time.Minute) || !waf.AllowRate("comment", ipRate, 2, time.Minute) {
		t.Fatal("expected first 2 requests within rate limit")
	}
	if waf.AllowRate("comment", ipRate, 2, time.Minute) {
		t.Fatal("expected 3rd request to be rate limited")
	}
}

package security

import (
	"fmt"
	"net/netip"
	"testing"
	"time"
)

func TestWAF_AuthRateScopesSurvivePublicRateChurn(t *testing.T) {
	w := &WAFEngine{}
	ip := netip.MustParseAddr("203.0.113.42")

	authScopes := []string{
		"stepup:u_admin_1",
		"setup:testdb",
		"oidc_start:github",
		"reader_register:",
		"unlock_ip:",
	}
	for _, sc := range authScopes {
		if !isAuthRateScope(sc) {
			t.Fatalf("expected scope %q to be classified as auth rate scope", sc)
		}
		if !w.AllowRate(sc, ip, 2, 5*time.Minute) {
			t.Fatalf("expected attempt 1 for %q to be allowed", sc)
		}
		if !w.AllowRate(sc, ip, 2, 5*time.Minute) {
			t.Fatalf("expected attempt 2 for %q to be allowed", sc)
		}
		if w.AllowRate(sc, ip, 2, 5*time.Minute) {
			t.Fatalf("expected attempt 3 for %q to be blocked", sc)
		}
	}

	// Churn public w.rates well past maxWAFRates (8192) and verify auth scopes stay rate-limited!
	for i := 0; i < maxWAFRates+1500; i++ {
		pubScope := fmt.Sprintf("unlock:art_%d", i)
		if isAuthRateScope(pubScope) {
			t.Fatalf("did not expect public scope %q to be classified as auth scope", pubScope)
		}
		_ = w.AllowRate(pubScope, ip, 10, 5*time.Minute)
	}

	for _, sc := range authScopes {
		if w.AllowRate(sc, ip, 2, 5*time.Minute) {
			t.Fatalf("auth rate limit for %q was evicted by public rate churn!", sc)
		}
	}
}

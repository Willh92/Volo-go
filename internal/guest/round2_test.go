package guest

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
)

func TestPoWVerifierEvictsEarliestExpiryDeterministically(t *testing.T) {
	kr, _, err := config.LoadOrInitKeyRing(strings.Repeat("p", 32), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pv := NewPoWVerifier(kr)
	pv.maxUsed = 3

	now := time.Now().Unix()
	pv.used["nonce_late_1"] = now + 500
	pv.used["nonce_earliest"] = now + 10
	pv.used["nonce_late_2"] = now + 600

	ch, err := pv.IssueChallenge(8)
	if err != nil {
		t.Fatal(err)
	}
	sol := SolveForTest(ch.Challenge, ch.DifficultyBits)
	if err := pv.Verify(ch.Challenge, sol, ""); err != nil {
		t.Fatalf("Verify valid PoW failed: %v", err)
	}

	if _, exists := pv.used["nonce_earliest"]; exists {
		t.Fatal("expected nonce_earliest (smallest expiration) to be evicted first at capacity")
	}
	if _, exists := pv.used["nonce_late_1"]; !exists {
		t.Fatal("expected fresher nonce_late_1 to remain protected against replay")
	}
	if _, exists := pv.used["nonce_late_2"]; !exists {
		t.Fatal("expected fresher nonce_late_2 to remain protected against replay")
	}
	if err := pv.Verify(ch.Challenge, sol, ""); !errors.Is(err, ErrPoWReplayed) {
		t.Fatalf("expected immediate replay of newly verified challenge to fail with ErrPoWReplayed, got %v", err)
	}
}

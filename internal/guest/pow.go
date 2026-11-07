package guest

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"math/bits"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/pkg/id"
)

const powChallengeTTL = 5 * time.Minute

var (
	ErrHoneypotTriggered = errors.New("guest: anti-spam honeypot triggered")
	ErrPoWInvalid        = errors.New("guest: invalid or expired proof-of-work solution")
	ErrPoWReplayed       = errors.New("guest: proof-of-work challenge already consumed")
)

// PoWVerifier issues and validates zero-CAPTCHA, privacy-preserving SHA-256 Proof-of-Work challenges
// paired with invisible honeypot detection and single-use replay prevention.
type PoWVerifier struct {
	keyRing *config.KeyRing
	mu      sync.Mutex
	used    map[string]int64 // nonce -> expiresAt unix seconds
}

func NewPoWVerifier(keyRing *config.KeyRing) *PoWVerifier {
	return &PoWVerifier{
		keyRing: keyRing,
		used:    make(map[string]int64),
	}
}

// Challenge Payload returned by GET /api/v1/pow/challenge.
type PoWChallenge struct {
	Challenge      string `json:"challenge"`
	DifficultyBits int    `json:"difficulty_bits"`
	ExpiresAt      int64  `json:"expires_at"`
}

// IssueChallenge creates a stateless HMAC-signed PoW challenge for the given difficulty (8..24 bits).
func (pv *PoWVerifier) IssueChallenge(difficultyBits int) (*PoWChallenge, error) {
	if difficultyBits < 8 {
		difficultyBits = 8
	}
	if difficultyBits > 24 {
		difficultyBits = 24
	}
	nonce, err := id.RandomHex(12)
	if err != nil {
		return nil, err
	}
	exp := time.Now().Add(powChallengeTTL).Unix()
	body := fmt.Sprintf("%d.%d.%s", exp, difficultyBits, nonce)
	sig := pv.sign(body)
	return &PoWChallenge{
		Challenge:      body + "." + sig,
		DifficultyBits: difficultyBits,
		ExpiresAt:      exp,
	}, nil
}

// Verify checks:
// 1. Invisible honeypot field MUST be empty.
// 2. Challenge HMAC signature & 5-minute TTL MUST be valid.
// 3. SHA-256(challenge + ":" + solution) MUST have >= difficultyBits leading zero bits.
// 4. Challenge nonce MUST NOT have been consumed before (prevents replay spam).
func (pv *PoWVerifier) Verify(challenge string, solution string, honeypot string) error {
	if strings.TrimSpace(honeypot) != "" {
		return ErrHoneypotTriggered
	}
	parts := strings.Split(strings.TrimSpace(challenge), ".")
	if len(parts) != 4 {
		return ErrPoWInvalid
	}
	expSec, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() > expSec {
		return ErrPoWInvalid
	}
	diffBits, err := strconv.Atoi(parts[1])
	if err != nil || diffBits < 8 || diffBits > 24 {
		return ErrPoWInvalid
	}
	nonce := parts[2]
	body := parts[0] + "." + parts[1] + "." + parts[2]
	expectedSig := pv.sign(body)
	if subtle.ConstantTimeCompare([]byte(parts[3]), []byte(expectedSig)) != 1 {
		return ErrPoWInvalid
	}

	sol := strings.TrimSpace(solution)
	if sol == "" || len(sol) > 64 {
		return ErrPoWInvalid
	}
	digest := sha256.Sum256([]byte(challenge + ":" + sol))
	if countLeadingZeroBits(digest[:]) < diffBits {
		return ErrPoWInvalid
	}

	const maxPoWUsed = 8192
	nowSec := time.Now().Unix()
	pv.mu.Lock()
	defer pv.mu.Unlock()
	if exp, already := pv.used[nonce]; already && nowSec <= exp {
		return ErrPoWReplayed
	}
	if len(pv.used) >= maxPoWUsed {
		for k, exp := range pv.used {
			if nowSec > exp {
				delete(pv.used, k)
			}
		}
		for k := range pv.used {
			if len(pv.used) < maxPoWUsed {
				break
			}
			delete(pv.used, k)
		}
	}
	pv.used[nonce] = expSec + 60
	return nil
}

// SolveForTest solves a PoWChallenge in-process (used by unit and smoke tests).
func SolveForTest(challenge string, difficultyBits int) string {
	for i := uint64(0); ; i++ {
		cand := strconv.FormatUint(i, 10)
		sum := sha256.Sum256([]byte(challenge + ":" + cand))
		if countLeadingZeroBits(sum[:]) >= difficultyBits {
			return cand
		}
	}
}

func countLeadingZeroBits(b []byte) int {
	total := 0
	for _, v := range b {
		if v == 0 {
			total += 8
			continue
		}
		total += bits.LeadingZeros8(v)
		break
	}
	return total
}

func (pv *PoWVerifier) sign(body string) string {
	key := pv.keyRing.SessionCSRFKey()
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte("volo-pow:v1:"))
	mac.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
}

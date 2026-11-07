package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
)

const (
	totpPeriodSec       = 30
	totpDigits          = 6
	recoveryCodeCount   = 8
	recoveryAlphabet    = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"
	twoFactorTicketTTL  = 5 * time.Minute
)

var (
	ErrTOTPInvalid       = errors.New("auth: invalid or expired TOTP verification code")
	ErrTOTPReplay        = errors.New("auth: TOTP code already used in current time window")
	ErrTwoFactorExpired  = errors.New("auth: two-factor login challenge expired or invalid")
)

// TOTPReplayGuard prevents a valid 6-digit TOTP code from being replayed within the same 30s counter window.
type TOTPReplayGuard struct {
	mu   sync.Mutex
	seen map[string]int64 // userID:step -> expiresAtUnix
}

func NewTOTPReplayGuard() *TOTPReplayGuard {
	return &TOTPReplayGuard{
		seen: make(map[string]int64),
	}
}

// GenerateTOTPSecret creates a 20-byte (160-bit) CSPRNG secret encoded in unpadded RFC 4648 Base32.
func GenerateTOTPSecret() (string, error) {
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("auth: generate totp secret: %w", err)
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw), nil
}

// BuildOTPAuthURI constructs a standard otpauth://totp/... URI for authenticator QR apps.
func BuildOTPAuthURI(issuer string, accountName string, secretBase32 string) string {
	if strings.TrimSpace(issuer) == "" {
		issuer = "Volo-go"
	}
	label := url.PathEscape(issuer + ":" + accountName)
	q := url.Values{}
	q.Set("secret", strings.ToUpper(strings.TrimSpace(secretBase32)))
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", "6")
	q.Set("period", "30")
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// ComputeTOTPCode computes the 6-digit RFC 6238 TOTP code for secretBase32 at timestamp t.
func ComputeTOTPCode(secretBase32 string, t time.Time) (string, error) {
	key, err := decodeBase32Secret(secretBase32)
	if err != nil {
		return "", err
	}
	step := uint64(t.Unix() / totpPeriodSec)
	return hotpCode(key, step), nil
}

// VerifyTOTPCode validates a 6-digit TOTP code across [-windowSteps, +windowSteps] (default 1)
// and atomically records the matched step in replayGuard if non-nil.
func VerifyTOTPCode(userID string, secretBase32 string, code string, now time.Time, windowSteps int, replayGuard *TOTPReplayGuard) error {
	cleaned := strings.TrimSpace(strings.ReplaceAll(code, " ", ""))
	if len(cleaned) != totpDigits {
		return ErrTOTPInvalid
	}
	key, err := decodeBase32Secret(secretBase32)
	if err != nil {
		return ErrTOTPInvalid
	}
	if windowSteps < 0 || windowSteps > 3 {
		windowSteps = 1
	}
	curStep := now.Unix() / totpPeriodSec
	matchedStep := int64(-1)
	for delta := -int64(windowSteps); delta <= int64(windowSteps); delta++ {
		step := curStep + delta
		if step < 0 {
			continue
		}
		cand := hotpCode(key, uint64(step))
		if subtle.ConstantTimeCompare([]byte(cand), []byte(cleaned)) == 1 {
			matchedStep = step
			break
		}
	}
	if matchedStep < 0 {
		return ErrTOTPInvalid
	}

	if replayGuard != nil && userID != "" {
		const maxTOTPSeen = 4096
		rk := fmt.Sprintf("%s:%d", userID, matchedStep)
		nowUnix := now.Unix()
		replayGuard.mu.Lock()
		defer replayGuard.mu.Unlock()
		if exp, dup := replayGuard.seen[rk]; dup && nowUnix <= exp {
			return ErrTOTPReplay
		}
		if len(replayGuard.seen) >= maxTOTPSeen {
			for k, exp := range replayGuard.seen {
				if nowUnix > exp {
					delete(replayGuard.seen, k)
				}
			}
			for k := range replayGuard.seen {
				if len(replayGuard.seen) < maxTOTPSeen {
					break
				}
				delete(replayGuard.seen, k)
			}
		}
		replayGuard.seen[rk] = nowUnix + int64((windowSteps+2)*totpPeriodSec)
	}
	return nil
}

func decodeBase32Secret(s string) ([]byte, error) {
	clean := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
	clean = strings.TrimRight(clean, "=")
	return base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(clean)
}

func hotpCode(key []byte, counter uint64) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	binCode := (uint32(sum[offset]&0x7f) << 24) |
		(uint32(sum[offset+1]) << 16) |
		(uint32(sum[offset+2]) << 8) |
		uint32(sum[offset+3])
	otp := binCode % 1_000_000
	return fmt.Sprintf("%06d", otp)
}

// GenerateRecoveryCodes generates 8 single-use backup codes formatted as "XXXX-XXXX",
// returning both the plaintext list (shown once to the user) and their SHA-256 hex digests for storage.
func GenerateRecoveryCodes() (plaintext []string, hashes []string, err error) {
	plaintext = make([]string, recoveryCodeCount)
	hashes = make([]string, recoveryCodeCount)
	buf := make([]byte, 8)
	for i := 0; i < recoveryCodeCount; i++ {
		if _, err := rand.Read(buf); err != nil {
			return nil, nil, err
		}
		var b strings.Builder
		for j := 0; j < 8; j++ {
			if j == 4 {
				b.WriteByte('-')
			}
			b.WriteByte(recoveryAlphabet[int(buf[j])%len(recoveryAlphabet)])
		}
		code := b.String()
		plaintext[i] = code
		hashes[i] = HashRecoveryCode(code)
	}
	return plaintext, hashes, nil
}

// HashRecoveryCode normalizes a recovery code (uppercase, dashes/spaces stripped) and hashes with SHA-256.
func HashRecoveryCode(raw string) string {
	norm := strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(raw)))
	sum := sha256.Sum256([]byte("volo-recovery-code:v1:" + norm))
	return hex.EncodeToString(sum[:])
}

// ConsumeRecoveryCodeHash checks candidateRaw against storedHashes in constant time and returns
// the remaining unused hashes if matched.
func ConsumeRecoveryCodeHash(candidateRaw string, storedHashes []string) (remaining []string, matched bool) {
	candHash := HashRecoveryCode(candidateRaw)
	remaining = make([]string, 0, len(storedHashes))
	for _, h := range storedHashes {
		if !matched && subtle.ConstantTimeCompare([]byte(h), []byte(candHash)) == 1 {
			matched = true
			continue
		}
		remaining = append(remaining, h)
	}
	return remaining, matched
}

// TwoFactorChallengeClaims is sealed inside an AES-256-GCM envelope after Step-1 password verification
// when a user has TOTP enabled, binding the pending login to the client subnet & User-Agent without
// creating a premature session row!
type TwoFactorChallengeClaims struct {
	UserID    string `json:"uid"`
	IPBucket  string `json:"ipb"`
	UAHash    string `json:"uah"`
	ExpiresAt int64  `json:"exp"`
}

func SealTwoFactorChallenge(kr *config.KeyRing, userID string, ipBucket string, userAgent string) (string, error) {
	uaSum := sha256.Sum256([]byte(strings.TrimSpace(userAgent)))
	claims := TwoFactorChallengeClaims{
		UserID:    userID,
		IPBucket:  ipBucket,
		UAHash:    hex.EncodeToString(uaSum[:8]),
		ExpiresAt: time.Now().Add(twoFactorTicketTTL).UnixMilli(),
	}
	raw, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	return kr.SealString(string(raw), "auth:2fa-challenge:"+ipBucket)
}

func OpenTwoFactorChallenge(kr *config.KeyRing, token string, ipBucket string, userAgent string) (*TwoFactorChallengeClaims, error) {
	opened, err := kr.OpenString(strings.TrimSpace(token), "auth:2fa-challenge:"+ipBucket)
	if err != nil || opened == "" {
		return nil, ErrTwoFactorExpired
	}
	var claims TwoFactorChallengeClaims
	if err := json.Unmarshal([]byte(opened), &claims); err != nil {
		return nil, ErrTwoFactorExpired
	}
	if time.Now().UnixMilli() > claims.ExpiresAt || claims.UserID == "" {
		return nil, ErrTwoFactorExpired
	}
	uaSum := sha256.Sum256([]byte(strings.TrimSpace(userAgent)))
	if subtle.ConstantTimeCompare([]byte(claims.UAHash), []byte(hex.EncodeToString(uaSum[:8]))) != 1 {
		return nil, ErrTwoFactorExpired
	}
	return &claims, nil
}

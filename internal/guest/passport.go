package guest

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/pkg/id"
)

const (
	PassportCookieName = "__volo_gp"
	PassportHeaderName = "X-Volo-Guest-Passport"
	UnlockCookieName   = "__volo_unlock"
	PassportMaxAgeSec  = 400 * 24 * 3600 // 400 days (maximum modern browser cookie cap)
	UnlockMaxAgeSec    = 30 * 24 * 3600  // 30 days persistent unlock state
	maxUnlockedEntries = 64
)

// Identity represents a verified, cryptographically signed persistent guest passport.
type Identity struct {
	GuestID     string `json:"guest_id"`
	GuestIDHash string `json:"guest_id_hash"`
	IssuedAtMS  int64  `json:"issued_at"`
	SignedToken string `json:"passport_token"`
	Source      string `json:"source"` // "cookie" | "header_healed" | "minted"
}

// GuestPassport aliases Identity for HTTP cookie issuance callers.
type GuestPassport = Identity

// PassportManager issues, verifies, and self-heals persistent guest passports across
// HttpOnly Cookie + localStorage/IndexedDB (X-Volo-Guest-Passport header) and manages
// password-bound article/lock unlock state.
type PassportManager struct {
	keyRing         *config.KeyRing
	secureCookie    atomic.Bool
	secureRequestFn atomic.Pointer[func(*http.Request) bool]
}

func NewPassportManager(keyRing *config.KeyRing, publicURL string) *PassportManager {
	pm := &PassportManager{
		keyRing: keyRing,
	}
	pm.SetPublicURL(publicURL)
	return pm
}

// SetPublicURL dynamically updates the HTTPS Secure cookie policy when PublicURL changes at runtime.
func (pm *PassportManager) SetPublicURL(publicURL string) {
	if pm == nil {
		return
	}
	pm.secureCookie.Store(strings.HasPrefix(strings.ToLower(strings.TrimSpace(publicURL)), "https://"))
}

// SetSecureRequestFunc wires a trusted-proxy-aware HTTPS request detector for dynamic Cookie Secure flags.
func (pm *PassportManager) SetSecureRequestFunc(fn func(*http.Request) bool) {
	if pm == nil {
		return
	}
	if fn == nil {
		pm.secureRequestFn.Store(nil)
		return
	}
	pm.secureRequestFn.Store(&fn)
}

func (pm *PassportManager) isSecureForRequest(r *http.Request) bool {
	tlsActive := r != nil && r.TLS != nil
	if pm == nil {
		return tlsActive
	}
	if pm.secureCookie.Load() || tlsActive {
		return true
	}
	ptr := pm.secureRequestFn.Load()
	return ptr != nil && *ptr != nil && r != nil && (*ptr)(r)
}

// HashGuestID computes the irreversible SHA-256 storage identifier for comments.guest_id_hash.
func HashGuestID(guestID string) string {
	sum := sha256.Sum256([]byte("volo-guest-id:v1:" + strings.TrimSpace(guestID)))
	return hex.EncodeToString(sum[:])
}

// SignPassport creates a stateless HMAC-SHA256 signed GuestPassport token:
// <guest_id>.<issued_ms>.<base64url_hmac>
func (pm *PassportManager) SignPassport(guestID string, issuedAtMS int64) string {
	payload := fmt.Sprintf("%s.%d", guestID, issuedAtMS)
	sig := pm.computePassportMAC(payload)
	return payload + "." + sig
}

// VerifyPassport validates a signed GuestPassport token in constant time without any DB lookup.
func (pm *PassportManager) VerifyPassport(token string) (guestID string, issuedAtMS int64, ok bool) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return "", 0, false
	}
	gid := parts[0]
	if len(gid) < 12 || len(gid) > 64 || !isSafeTokenIdent(gid) {
		return "", 0, false
	}
	ts, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || ts <= 0 {
		return "", 0, false
	}
	expectedSig := pm.computePassportMAC(parts[0] + "." + parts[1])
	if subtle.ConstantTimeCompare([]byte(parts[2]), []byte(expectedSig)) != 1 {
		return "", 0, false
	}
	return gid, ts, true
}

// ResolveOrHeal inspects HttpOnly cookie first, then falls back to X-Volo-Guest-Passport header
// (mirrored in client localStorage + IndexedDB).
//   - If Cookie is missing/invalidated (e.g., Safari ITP) but Header carries a valid HMAC token,
//     ResolveOrHeal self-heals by re-writing the HttpOnly cookie!
//   - If neither exists and mintIfMissing==true, it mints a fresh cryptographic GuestPassport and sets the cookie.
func (pm *PassportManager) ResolveOrHeal(w http.ResponseWriter, r *http.Request, mintIfMissing bool) (*Identity, bool) {
	if c, err := r.Cookie(PassportCookieName); err == nil && c.Value != "" {
		if gid, ts, ok := pm.VerifyPassport(c.Value); ok {
			return &Identity{
				GuestID:     gid,
				GuestIDHash: HashGuestID(gid),
				IssuedAtMS:  ts,
				SignedToken: c.Value,
				Source:      "cookie",
			}, true
		}
	}

	if hdr := strings.TrimSpace(r.Header.Get(PassportHeaderName)); hdr != "" {
		if gid, ts, ok := pm.VerifyPassport(hdr); ok {
			if w != nil {
				pm.IssueCookieWithSecure(w, GuestPassport{SignedToken: hdr}, pm.isSecureForRequest(r))
			}
			return &Identity{
				GuestID:     gid,
				GuestIDHash: HashGuestID(gid),
				IssuedAtMS:  ts,
				SignedToken: hdr,
				Source:      "header_healed",
			}, true
		}
	}

	if !mintIfMissing {
		return nil, false
	}

	gid := id.NextPrefixed("gp")
	now := time.Now().UnixMilli()
	tok := pm.SignPassport(gid, now)
	if w != nil {
		pm.IssueCookieWithSecure(w, GuestPassport{SignedToken: tok}, pm.isSecureForRequest(r))
	}
	return &Identity{
		GuestID:     gid,
		GuestIDHash: HashGuestID(gid),
		IssuedAtMS:  now,
		SignedToken: tok,
		Source:      "minted",
	}, true
}

// IssueCookie writes the HttpOnly GuestPassport cookie using the manager's configured Secure flag.
func (pm *PassportManager) IssueCookie(w http.ResponseWriter, gp GuestPassport) {
	pm.IssueCookieWithSecure(w, gp, pm != nil && pm.secureCookie.Load())
}

// IssueCookieWithSecure writes the HttpOnly GuestPassport cookie with an explicit per-request Secure flag.
func (pm *PassportManager) IssueCookieWithSecure(w http.ResponseWriter, gp GuestPassport, secure bool) {
	if w == nil {
		return
	}
	tok := strings.TrimSpace(gp.SignedToken)
	if tok == "" && pm != nil && gp.GuestID != "" {
		ts := gp.IssuedAtMS
		if ts <= 0 {
			ts = time.Now().UnixMilli()
		}
		tok = pm.SignPassport(gp.GuestID, ts)
	}
	if tok == "" {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     PassportCookieName,
		Value:    tok,
		Path:     "/",
		MaxAge:   PassportMaxAgeSec,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (pm *PassportManager) computePassportMAC(payload string) string {
	key := pm.keyRing.GuestPassportKey()
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte("volo-gp-v1:"))
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// --- Password-Bound Persistent Article / :::lock Unlock State ---

type unlockBundle struct {
	GuestID string            `json:"g"`
	Entries map[string]string `json:"e"` // articleID -> exp_sec.mac
}

// WriteUnlockCookie writes the HttpOnly persistent unlock cookie using the manager's configured Secure flag.
func (pm *PassportManager) WriteUnlockCookie(w http.ResponseWriter, rawValue string) {
	pm.WriteUnlockCookieWithSecure(w, rawValue, pm != nil && pm.secureCookie.Load())
}

// WriteUnlockCookieWithSecure writes the HttpOnly persistent unlock cookie with an explicit per-request Secure flag.
func (pm *PassportManager) WriteUnlockCookieWithSecure(w http.ResponseWriter, rawValue string, secure bool) {
	if w == nil || strings.TrimSpace(rawValue) == "" {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     UnlockCookieName,
		Value:    strings.TrimSpace(rawValue),
		Path:     "/",
		MaxAge:   UnlockMaxAgeSec,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// GrantArticleUnlock binds an unlocked article state to BOTH the visitor's GuestID AND the
// SHA-256 digest of the article's current plaintext password. If the author later changes
// the article password, all existing unlock cookies for that article automatically become invalid!
func (pm *PassportManager) GrantArticleUnlock(w http.ResponseWriter, r *http.Request, guestID string, articleID string, currentPasswordPlain string) (string, error) {
	if guestID == "" || articleID == "" || currentPasswordPlain == "" {
		return "", errors.New("guest: missing unlock parameters")
	}
	bundle := pm.readUnlockBundle(r, guestID)
	expSec := time.Now().Add(time.Duration(UnlockMaxAgeSec) * time.Second).Unix()
	mac := pm.computeUnlockMAC(guestID, articleID, currentPasswordPlain, expSec)
	bundle.Entries[articleID] = fmt.Sprintf("%d.%s", expSec, mac)

	// Cap map size so cookie never exceeds 4KB browser limit
	if len(bundle.Entries) > maxUnlockedEntries {
		for k := range bundle.Entries {
			if k != articleID {
				delete(bundle.Entries, k)
				break
			}
		}
	}

	rawJSON, err := json.Marshal(bundle)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(rawJSON)
	sig := pm.computePassportMAC("unlock-bundle:" + encoded)
	cookieVal := encoded + "." + sig
	if w != nil {
		pm.WriteUnlockCookieWithSecure(w, cookieVal, pm.isSecureForRequest(r))
	}
	return cookieVal, nil
}

// HasArticleUnlock checks whether r carries a valid, non-expired unlock proof bound to
// BOTH guestID and the article's current password!
func (pm *PassportManager) HasArticleUnlock(r *http.Request, guestID string, articleID string, currentPasswordPlain string) bool {
	if r == nil || guestID == "" || articleID == "" || currentPasswordPlain == "" {
		return false
	}
	bundle := pm.readUnlockBundle(r, guestID)
	entry, exists := bundle.Entries[articleID]
	if !exists {
		return false
	}
	parts := strings.Split(entry, ".")
	if len(parts) != 2 {
		return false
	}
	expSec, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() >= expSec {
		return false
	}
	expected := pm.computeUnlockMAC(guestID, articleID, currentPasswordPlain, expSec)
	return subtle.ConstantTimeCompare([]byte(parts[1]), []byte(expected)) == 1
}

func (pm *PassportManager) readUnlockBundle(r *http.Request, expectedGuestID string) unlockBundle {
	out := unlockBundle{
		GuestID: expectedGuestID,
		Entries: make(map[string]string),
	}
	if r == nil {
		return out
	}
	rawVal := ""
	if c, err := r.Cookie(UnlockCookieName); err == nil && c.Value != "" {
		rawVal = c.Value
	} else if hdr := strings.TrimSpace(r.Header.Get("X-Volo-Unlock-State")); hdr != "" {
		rawVal = hdr
	}
	if rawVal == "" {
		return out
	}
	parts := strings.Split(rawVal, ".")
	if len(parts) != 2 {
		return out
	}
	expectedSig := pm.computePassportMAC("unlock-bundle:" + parts[0])
	if subtle.ConstantTimeCompare([]byte(parts[1]), []byte(expectedSig)) != 1 {
		return out
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return out
	}
	var parsed unlockBundle
	if json.Unmarshal(decoded, &parsed) != nil || parsed.GuestID != expectedGuestID || parsed.Entries == nil {
		return out
	}
	return parsed
}

func (pm *PassportManager) computeUnlockMAC(guestID string, articleID string, pwdPlain string, expSec int64) string {
	key := pm.keyRing.GuestPassportKey()
	salt := make([]byte, 0, len(key)+len(guestID)+len(articleID)+48)
	salt = append(salt, key[:]...)
	salt = fmt.Appendf(salt, ":volo-unlock:v2:%d:%s:%d:%s:%d", len(guestID), guestID, len(articleID), articleID, expSec)
	dk, _ := pbkdf2.Key(sha256.New, pwdPlain, salt, 4096, 16)
	return base64.RawURLEncoding.EncodeToString(dk)
}

func isSafeTokenIdent(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

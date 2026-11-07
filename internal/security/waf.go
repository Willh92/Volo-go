package security

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/pkg/id"
)

// StrikeReason classifies hostile probes for escalating auto-ban defense.
const (
	ReasonScannerTrap = "scanner_trap"
	ReasonLoginFail   = "login_fail"
	Reason2FAFail     = "two_factor_fail"
	ReasonUnlockFail  = "unlock_fail"
	ReasonRateAbuse   = "rate_abuse"
)

var commonScannerPrefixes = []string{
	"/wp-admin",
	"/wp-login.php",
	"/xmlrpc.php",
	"/.env",
	"/.git",
	"/phpmyadmin",
	"/pma",
	"/actuator",
	"/vendor/phpunit",
	"/cgi-bin/",
}

type strikeBucket struct {
	score       int
	occurrences int
	windowStart time.Time
}

type rateWindow struct {
	count       int
	windowReset time.Time
}

// WAFEngine enforces:
// 1. Manual & escalating automatic IP/CIDR bans (persisted in security_ip_bans + cached in RAM).
// 2. Stealth 404 cloaking on default /console when VOLO_ADMIN_PATH is customized.
// 3. Automated scanner trap detection (/.env, /wp-admin, /.git, etc.).
// 4. Sliding-window rate limiting keyed by /32 IPv4 and /64 IPv6 subnet buckets.
type WAFEngine struct {
	cfg         *config.Config
	store       *sqlx.Store
	settingsMgr *repo.SettingsManager
	geo         *GeoEngine

	mu         sync.RWMutex
	publicURL  string
	activeBans []compiledBan
	strikes    map[string]*strikeBucket
	rates      map[string]*rateWindow
}

type compiledBan struct {
	record domain.SecurityIPBan
	prefix netip.Prefix
}

const (
	maxWAFStrikes = 4096
	maxWAFRates   = 8192
)

func NewWAFEngine(cfg *config.Config, store *sqlx.Store, settingsMgr *repo.SettingsManager, geo *GeoEngine) *WAFEngine {
	pubURL := ""
	if cfg != nil {
		pubURL = strings.TrimSpace(cfg.PublicURL)
	}
	return &WAFEngine{
		cfg:         cfg,
		store:       store,
		settingsMgr: settingsMgr,
		geo:         geo,
		publicURL:   pubURL,
		strikes:     make(map[string]*strikeBucket),
		rates:       make(map[string]*rateWindow),
	}
}

// SetPublicURL updates the canonical public URL used for conditional HTTPS security headers (HSTS).
func (w *WAFEngine) SetPublicURL(publicURL string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.publicURL = strings.TrimSpace(publicURL)
	w.mu.Unlock()
}

// ReloadBans refreshes active IP/CIDR bans from the database into memory.
func (w *WAFEngine) ReloadBans(ctx context.Context) error {
	if w.store == nil {
		return nil
	}
	now := time.Now().UnixMilli()
	rows, err := sqlx.Select[domain.SecurityIPBan](ctx, w.store,
		`SELECT * FROM security_ip_bans WHERE expires_at = 0 OR expires_at > ? ORDER BY created_at DESC`, now)
	if err != nil {
		return err
	}
	compiled := make([]compiledBan, 0, len(rows))
	for _, r := range rows {
		if pfx, ok := parseBanPrefix(r.CIDR); ok {
			compiled = append(compiled, compiledBan{record: r, prefix: pfx})
		}
	}
	w.mu.Lock()
	w.activeBans = compiled
	w.mu.Unlock()
	return nil
}

func parseBanPrefix(raw string) (netip.Prefix, bool) {
	s := strings.TrimSpace(raw)
	if pfx, err := netip.ParsePrefix(s); err == nil {
		return pfx.Masked(), true
	}
	if addr, err := netip.ParseAddr(s); err == nil {
		u := addr.Unmap()
		if u.Is4() {
			return netip.PrefixFrom(u, 32), true
		}
		return netip.PrefixFrom(u, 128), true
	}
	return netip.Prefix{}, false
}

// IsBanned checks if clientAddr is covered by any active manual or automatic IP/CIDR ban.
func (w *WAFEngine) IsBanned(clientAddr netip.Addr) (bool, string) {
	if !clientAddr.IsValid() {
		return false, ""
	}
	u := clientAddr.Unmap()
	now := time.Now().UnixMilli()

	w.mu.RLock()
	defer w.mu.RUnlock()
	for _, b := range w.activeBans {
		if b.record.ExpiresAt > 0 && b.record.ExpiresAt <= now {
			continue
		}
		if b.prefix.Contains(u) {
			return true, b.record.ReasonCode
		}
	}
	return false, ""
}

// AddManualBan persists a manual IP or CIDR ban and updates the in-memory firewall immediately.
// Refuses banning the administrator's own current IP to prevent accidental self-lockout!
func (w *WAFEngine) AddManualBan(ctx context.Context, cidrOrIP string, note string, expiresAt int64, actorAddr netip.Addr) (*domain.SecurityIPBan, error) {
	pfx, ok := parseBanPrefix(cidrOrIP)
	if !ok {
		return nil, fmt.Errorf("security: invalid IP or CIDR %q", cidrOrIP)
	}
	if actorAddr.IsValid() && pfx.Contains(actorAddr.Unmap()) {
		return nil, fmt.Errorf("security: refusing to ban your own active IP %s", actorAddr.String())
	}

	now := time.Now().UnixMilli()
	rec := domain.SecurityIPBan{
		ID:          id.Next(),
		CIDR:        pfx.String(),
		Source:      "manual",
		ReasonCode:  "manual_ban",
		Reason:      "Manual administrator block",
		Note:        strings.TrimSpace(note),
		Occurrences: 1,
		ExpiresAt:   expiresAt,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if w.store != nil {
		upsert := w.store.Dialect().UpsertSQL(
			"security_ip_bans",
			[]string{"id", "cidr", "source", "reason_code", "reason", "note", "occurrences", "expires_at", "created_at", "updated_at"},
			[]string{"cidr"},
			[]string{"source", "reason_code", "reason", "note", "expires_at", "updated_at"},
		)
		if _, err := w.store.ExecContext(ctx, upsert,
			rec.ID, rec.CIDR, rec.Source, rec.ReasonCode, rec.Reason, rec.Note, rec.Occurrences, rec.ExpiresAt, rec.CreatedAt, rec.UpdatedAt,
		); err != nil {
			return nil, err
		}
		_ = w.ReloadBans(ctx)
	} else {
		w.mu.Lock()
		w.activeBans = append(w.activeBans, compiledBan{record: rec, prefix: pfx})
		w.mu.Unlock()
	}
	return &rec, nil
}

// RecordStrike increments behavioral risk score for the client's subnet bucket (/32 IPv4 or /64 IPv6)
// and triggers an escalating automatic ban when threshold is crossed.
func (w *WAFEngine) RecordStrike(ctx context.Context, clientAddr netip.Addr, reasonCode string, weight int) (autoBanned bool) {
	if !clientAddr.IsValid() || clientAddr.IsLoopback() {
		// Still track non-loopback in production; allow unit tests to pass public test IPs (e.g., 203.0.113.9).
		if clientAddr.IsLoopback() {
			return false
		}
	}
	if weight <= 0 {
		weight = 1
	}

	secSnap := domain.DefaultSettingsSnapshot("zh-CN").Security
	if w.settingsMgr != nil {
		secSnap = w.settingsMgr.Snapshot().Security
	}
	if !secSnap.RiskControlEnabled {
		return false
	}

	bucketKey := RateLimitBucketKey(clientAddr)
	now := time.Now()

	w.mu.Lock()
	sb, exists := w.strikes[bucketKey]
	if !exists || now.Sub(sb.windowStart) > 10*time.Minute {
		if !exists && len(w.strikes) >= maxWAFStrikes {
			for k, v := range w.strikes {
				if now.Sub(v.windowStart) > 10*time.Minute {
					delete(w.strikes, k)
				}
			}
			for k := range w.strikes {
				if len(w.strikes) < maxWAFStrikes {
					break
				}
				delete(w.strikes, k)
			}
		}
		sb = &strikeBucket{score: 0, occurrences: 0, windowStart: now}
		w.strikes[bucketKey] = sb
	}
	sb.score += weight
	sb.occurrences++

	const threshold = 5
	if sb.score < threshold {
		w.mu.Unlock()
		return false
	}

	// Escalating ban ladder: 1st ban = 15m, 2nd = 1h, 3rd+ = 24h
	sb.score = 0
	occ := sb.occurrences
	const baseMins = 15
	dur := time.Duration(baseMins) * time.Minute
	if occ >= threshold*3 {
		dur = 24 * time.Hour
	} else if occ >= threshold*2 {
		dur = time.Duration(baseMins*4) * time.Minute
	}
	expiresAt := now.Add(dur).UnixMilli()

	pfx, _ := parseBanPrefix(bucketKey)
	rec := domain.SecurityIPBan{
		ID:          id.Next(),
		CIDR:        pfx.String(),
		Source:      "auto",
		ReasonCode:  reasonCode,
		Reason:      fmt.Sprintf("Automated defense triggered (%s)", reasonCode),
		Occurrences: occ,
		ExpiresAt:   expiresAt,
		CreatedAt:   now.UnixMilli(),
		UpdatedAt:   now.UnixMilli(),
	}
	w.activeBans = append(w.activeBans, compiledBan{record: rec, prefix: pfx})
	w.mu.Unlock()

	if w.store != nil {
		upsert := w.store.Dialect().UpsertSQL(
			"security_ip_bans",
			[]string{"id", "cidr", "source", "reason_code", "reason", "note", "occurrences", "expires_at", "created_at", "updated_at"},
			[]string{"cidr"},
			[]string{"source", "reason_code", "reason", "occurrences", "expires_at", "updated_at"},
		)
		_, _ = w.store.ExecContext(ctx, upsert,
			rec.ID, rec.CIDR, rec.Source, rec.ReasonCode, rec.Reason, "", rec.Occurrences, rec.ExpiresAt, rec.CreatedAt, rec.UpdatedAt)
	}
	return true
}

// AllowRate enforces a fixed-window rate limit keyed by (scope + /32 IPv4 or /64 IPv6 bucket).
func (w *WAFEngine) AllowRate(scope string, clientAddr netip.Addr, limit int, window time.Duration) bool {
	if limit <= 0 {
		return true
	}
	key := scope + "|" + RateLimitBucketKey(clientAddr)
	now := time.Now()

	w.mu.Lock()
	defer w.mu.Unlock()
	rw, exists := w.rates[key]
	if !exists || now.After(rw.windowReset) {
		if !exists && len(w.rates) >= maxWAFRates {
			// Bounded GC sweep of expired windows + hard cap eviction so RAM stays tiny under DDoS
			for k, v := range w.rates {
				if now.After(v.windowReset) {
					delete(w.rates, k)
				}
			}
			for k := range w.rates {
				if len(w.rates) < maxWAFRates {
					break
				}
				delete(w.rates, k)
			}
		}
		w.rates[key] = &rateWindow{count: 1, windowReset: now.Add(window)}
		return true
	}
	if rw.count >= limit {
		return false
	}
	rw.count++
	return true
}

// WrapHTTP wraps next with:
// 1. /healthz bypass (always fast)
// 2. IP/CIDR ban enforcement (403)
// 3. Offline GeoIP access control (with emergency circuit-breaker bypass)
// 4. Custom VOLO_ADMIN_PATH stealth 404 cloaking on default /console
// 5. Automated scanner trap 404 + strike recording
func (w *WAFEngine) WrapHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		// Baseline hardened security headers on every response
		rw.Header().Set("X-Content-Type-Options", "nosniff")
		rw.Header().Set("X-Frame-Options", "SAMEORIGIN")
		rw.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		rw.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")

		w.mu.RLock()
		pubURL := w.publicURL
		w.mu.RUnlock()
		if r.TLS != nil || strings.HasPrefix(strings.ToLower(pubURL), "https://") {
			rw.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}

		reqPath := r.URL.Path
		if reqPath == "/healthz" {
			next.ServeHTTP(rw, r)
			return
		}

		var tp config.TrustedProxiesPolicy
		if w.cfg != nil {
			tp = w.cfg.TrustedProxies
		}
		clientIP := ResolveClientIP(r, tp)

		// 1. Active IP / /64 CIDR Ban Check
		if banned, _ := w.IsBanned(clientIP); banned {
			http.Error(rw, "403 Forbidden", http.StatusForbidden)
			return
		}

		// 2. Offline GeoIP Access Control (with VOLO_GEO_ACCESS_ENABLED circuit breaker)
		if w.geo != nil && (w.cfg == nil || w.cfg.GeoAccessEnabled) {
			if allowed := w.geo.Allows(clientIP); !allowed {
				http.Error(rw, "403 Forbidden", http.StatusForbidden)
				return
			}
		}

		// 3. Custom VOLO_ADMIN_PATH Stealth 404 Cloaking!
		// When VOLO_ADMIN_PATH is customized (e.g. /secret-panel), /console and /console/*
		// MUST return an indistinguishable 404 Not Found with zero redirect or auth hint!
		if w.cfg != nil && w.cfg.IsCustomAdminPath {
			if reqPath == config.DefaultAdminPath || strings.HasPrefix(reqPath, config.DefaultAdminPath+"/") {
				w.RecordStrike(r.Context(), clientIP, ReasonScannerTrap, 2)
				http.NotFound(rw, r)
				return
			}
		}

		// 4. Scanner Trap Paths (/.env, /.git, /wp-admin, etc.)
		lowPath := strings.ToLower(reqPath)
		for _, trap := range commonScannerPrefixes {
			if lowPath == trap || strings.HasPrefix(lowPath, trap+"/") {
				w.RecordStrike(r.Context(), clientIP, ReasonScannerTrap, 3)
				http.NotFound(rw, r)
				return
			}
		}

		next.ServeHTTP(rw, r)
	})
}

package security

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
	ReasonScannerTrap  = "scanner_trap"
	ReasonNotFoundScan = "not_found_scan"
	ReasonLoginFail    = "login_fail"
	Reason2FAFail      = "two_factor_fail"
	ReasonUnlockFail   = "unlock_fail"
	ReasonRateAbuse    = "rate_abuse"
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
// 3. Automated scanner trap & 404 path enumeration detection.
// 4. Sliding-window rate limiting keyed by /32 IPv4 and /64 IPv6 subnet buckets.
type WAFEngine struct {
	cfg         *config.Config
	store       *sqlx.Store
	settingsMgr *repo.SettingsManager
	geo         *GeoEngine

	bansMu           sync.RWMutex
	publicURL        string
	activeBans       map[netip.Prefix]int64
	banReasons       map[netip.Prefix]string
	banPrefixes      []netip.Prefix
	errorHandler     func(w http.ResponseWriter, r *http.Request, status int, msgKey string)
	staffBypassCheck func(r *http.Request) bool

	rateMu    sync.Mutex
	strikes   map[string]*strikeBucket
	rates     map[string]*rateWindow
	authRates map[string]*rateWindow

	stopOnce sync.Once
	stopCh   chan struct{}
}

const (
	maxWAFStrikes    = 4096
	maxWAFRates      = 8192
	maxWAFAuthRates  = 4096
	maxWAFActiveBans = 16384
	wafSweepInterval = time.Minute
)

func NewWAFEngine(cfg *config.Config, store *sqlx.Store, settingsMgr *repo.SettingsManager, geo *GeoEngine) *WAFEngine {
	pubURL := ""
	if cfg != nil {
		pubURL = strings.TrimSpace(cfg.GetPublicURL())
	}
	w := &WAFEngine{
		cfg:         cfg,
		store:       store,
		settingsMgr: settingsMgr,
		geo:         geo,
		publicURL:   pubURL,
		activeBans:  make(map[netip.Prefix]int64),
		banReasons:  make(map[netip.Prefix]string),
		strikes:     make(map[string]*strikeBucket),
		rates:       make(map[string]*rateWindow),
		authRates:   make(map[string]*rateWindow),
		stopCh:      make(chan struct{}),
	}
	go w.sweepLoop()
	return w
}

// SetErrorHandler wires custom themed HTML / localized JSON rendering for WAF 403/404/429 responses.
func (w *WAFEngine) SetErrorHandler(fn func(rw http.ResponseWriter, r *http.Request, status int, msgKey string)) {
	if w == nil {
		return
	}
	w.bansMu.Lock()
	w.errorHandler = fn
	w.bansMu.Unlock()
}

// SetStaffBypassCheck wires an optional check so authenticated staff sessions are never self-banned or rate-throttled.
func (w *WAFEngine) SetStaffBypassCheck(fn func(r *http.Request) bool) {
	if w == nil {
		return
	}
	w.bansMu.Lock()
	w.staffBypassCheck = fn
	w.bansMu.Unlock()
}

// Close stops the background sweep loop cleanly.
func (w *WAFEngine) Close() {
	if w == nil {
		return
	}
	w.stopOnce.Do(func() {
		close(w.stopCh)
	})
}

func (w *WAFEngine) sweepLoop() {
	ticker := time.NewTicker(wafSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-w.stopCh:
			return
		case now := <-ticker.C:
			w.sweep(now)
		}
	}
}

func (w *WAFEngine) sweep(now time.Time) {
	nowMS := now.UnixMilli()

	w.bansMu.Lock()
	w.pruneExpiredBansLocked(nowMS)
	w.bansMu.Unlock()

	var maxWAFReset time.Time
	if w.settingsMgr != nil {
		winSec := w.settingsMgr.Snapshot().Security.WAFWindowSeconds
		if winSec <= 0 {
			winSec = 180
		}
		maxWAFReset = now.Add(time.Duration(winSec) * time.Second)
	}

	w.rateMu.Lock()
	for k, v := range w.strikes {
		if now.Sub(v.windowStart) > 10*time.Minute {
			delete(w.strikes, k)
		}
	}
	for k, v := range w.rates {
		if now.After(v.windowReset) {
			delete(w.rates, k)
			continue
		}
		if !maxWAFReset.IsZero() && strings.HasPrefix(k, "waf_shield|") && v.windowReset.After(maxWAFReset) {
			v.windowReset = maxWAFReset
		}
	}
	for k, v := range w.authRates {
		if now.After(v.windowReset) {
			delete(w.authRates, k)
		}
	}
	w.rateMu.Unlock()
}

// UpdateConfig clamps active global WAF rate-limit buckets immediately when WAFWindowSeconds is reduced via hot-reload.
func (w *WAFEngine) UpdateConfig(wafWindowSeconds int) {
	if w == nil {
		return
	}
	if wafWindowSeconds <= 0 {
		wafWindowSeconds = 180
	}
	now := time.Now()
	maxReset := now.Add(time.Duration(wafWindowSeconds) * time.Second)

	w.rateMu.Lock()
	for k, v := range w.rates {
		if now.After(v.windowReset) {
			delete(w.rates, k)
			continue
		}
		if strings.HasPrefix(k, "waf_shield|") && v.windowReset.After(maxReset) {
			v.windowReset = maxReset
		}
	}
	w.rateMu.Unlock()
}

func (w *WAFEngine) pruneExpiredBansLocked(nowMS int64) {
	if len(w.banPrefixes) == 0 {
		return
	}
	kept := w.banPrefixes[:0]
	for _, pfx := range w.banPrefixes {
		exp, ok := w.activeBans[pfx]
		if !ok {
			continue
		}
		if exp > 0 && exp <= nowMS {
			delete(w.activeBans, pfx)
			delete(w.banReasons, pfx)
			continue
		}
		kept = append(kept, pfx)
	}
	clear(w.banPrefixes[len(kept):])
	w.banPrefixes = kept
}

func (w *WAFEngine) upsertBanLocked(pfx netip.Prefix, expiresAt int64, reasonCode string, nowMS int64) {
	if prevExp, exists := w.activeBans[pfx]; exists {
		if prevExp == 0 && expiresAt > 0 {
			// Never downgrade an active permanent ban into a temporary auto-ban.
			return
		}
		if expiresAt > 0 && prevExp > expiresAt {
			expiresAt = prevExp
		}
	} else {
		if len(w.banPrefixes) >= maxWAFActiveBans {
			w.pruneExpiredBansLocked(nowMS)
		}
		if len(w.banPrefixes) >= maxWAFActiveBans {
			// Evict oldest prefix if still at hard capacity under extreme churn.
			oldest := w.banPrefixes[0]
			delete(w.activeBans, oldest)
			delete(w.banReasons, oldest)
			copy(w.banPrefixes, w.banPrefixes[1:])
			w.banPrefixes[len(w.banPrefixes)-1] = netip.Prefix{}
			w.banPrefixes = w.banPrefixes[:len(w.banPrefixes)-1]
		}
		w.banPrefixes = append(w.banPrefixes, pfx)
	}
	w.activeBans[pfx] = expiresAt
	w.banReasons[pfx] = reasonCode
}

// SetPublicURL updates the canonical public URL used for conditional HTTPS security headers (HSTS).
func (w *WAFEngine) SetPublicURL(publicURL string) {
	if w == nil {
		return
	}
	w.bansMu.Lock()
	w.publicURL = strings.TrimSpace(publicURL)
	w.bansMu.Unlock()
}

// ReloadBans refreshes active IP/CIDR bans from the database into memory.
func (w *WAFEngine) ReloadBans(ctx context.Context) error {
	if w.store == nil {
		return nil
	}
	now := time.Now().UnixMilli()
	var rows []domain.SecurityIPBan
	err := sqlx.SelectInto(ctx, w.store, &rows,
		`SELECT * FROM security_ip_bans WHERE expires_at = 0 OR expires_at > ? ORDER BY created_at DESC`, now)
	if err != nil {
		return err
	}
	newBans := make(map[netip.Prefix]int64, len(rows))
	newReasons := make(map[netip.Prefix]string, len(rows))
	newPrefixes := make([]netip.Prefix, 0, len(rows))
	for _, r := range rows {
		pfx, ok := parseBanPrefix(r.CIDR)
		if !ok {
			continue
		}
		if r.ExpiresAt > 0 && r.ExpiresAt <= now {
			continue
		}
		if prevExp, seen := newBans[pfx]; seen {
			// Keep permanent ban (0) or whichever expiration extends further.
			if prevExp != 0 && (r.ExpiresAt == 0 || r.ExpiresAt > prevExp) {
				newBans[pfx] = r.ExpiresAt
				newReasons[pfx] = r.ReasonCode
			}
			continue
		}
		newBans[pfx] = r.ExpiresAt
		newReasons[pfx] = r.ReasonCode
		newPrefixes = append(newPrefixes, pfx)
	}
	w.bansMu.Lock()
	w.activeBans = newBans
	w.banReasons = newReasons
	w.banPrefixes = newPrefixes
	w.bansMu.Unlock()
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

	w.bansMu.RLock()
	defer w.bansMu.RUnlock()
	if len(w.activeBans) == 0 {
		return false, ""
	}

	// Fast-path O(1) lookup for exact host (/32 or /128) and IPv6 /64 auto-ban buckets.
	if u.Is4() {
		exact := netip.PrefixFrom(u, 32)
		if exp, ok := w.activeBans[exact]; ok && (exp == 0 || exp > now) {
			return true, w.banReasons[exact]
		}
	} else {
		exact := netip.PrefixFrom(u, 128)
		if exp, ok := w.activeBans[exact]; ok && (exp == 0 || exp > now) {
			return true, w.banReasons[exact]
		}
		if pfx64, err := u.Prefix(64); err == nil {
			if exp, ok := w.activeBans[pfx64]; ok && (exp == 0 || exp > now) {
				return true, w.banReasons[pfx64]
			}
		}
	}

	for _, pfx := range w.banPrefixes {
		if !pfx.Contains(u) {
			continue
		}
		exp, ok := w.activeBans[pfx]
		if !ok || (exp > 0 && exp <= now) {
			continue
		}
		return true, w.banReasons[pfx]
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
		w.bansMu.Lock()
		w.upsertBanLocked(pfx, rec.ExpiresAt, rec.ReasonCode, now)
		w.bansMu.Unlock()
	}
	return &rec, nil
}

func (w *WAFEngine) recordSecurityAudit(ctx context.Context, clientAddr netip.Addr, action string, resourceType string, resourceID string, detail map[string]any) {
	if w == nil || w.store == nil {
		return
	}
	detJSON := "{}"
	if detail != nil {
		if b, err := json.Marshal(detail); err == nil {
			detJSON = string(b)
		}
	}
	_, _ = w.store.ExecBgContext(ctx,
		`INSERT INTO audit_logs (id, actor_id, actor_name, action, resource_type, resource_id, client_ip, detail_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id.Next(), "", "WAF", action, resourceType, resourceID, MaskIP(clientAddr), detJSON, time.Now().UnixMilli())
}

// RecordStrike increments behavioral risk score for the client's subnet bucket (/32 IPv4 or /64 IPv6)
// and triggers an escalating automatic ban when threshold is crossed.
func (w *WAFEngine) RecordStrike(ctx context.Context, clientAddr netip.Addr, reasonCode string, weight int) (autoBanned bool) {
	return w.recordStrikeInternal(ctx, clientAddr, reasonCode, weight, "", false, false)
}

// RecordProbe404 records a 404 risk-control signal (scanner trap, stealth admin probe, or 404 enumeration).
// High-suspicion traps (weight >= 2) or repeated 404 scans (>= 3 within window) emit a rate-bounded
// security audit log entry (`security.waf_404_probe`) even on local/loopback testing, while only
// non-loopback, non-staff callers are subject to escalating automatic IP/CIDR bans (`security_ip_bans`).
func (w *WAFEngine) RecordProbe404(ctx context.Context, clientAddr netip.Addr, reasonCode string, weight int, reqPath string, skipBan bool) (autoBanned bool) {
	return w.recordStrikeInternal(ctx, clientAddr, reasonCode, weight, reqPath, skipBan, true)
}

func (w *WAFEngine) recordStrikeInternal(ctx context.Context, clientAddr netip.Addr, reasonCode string, weight int, reqPath string, skipBan bool, is404Probe bool) (autoBanned bool) {
	if w == nil {
		return false
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

	canBan := clientAddr.IsValid() && !clientAddr.IsLoopback() && !skipBan
	if !canBan && !is404Probe {
		return false
	}

	bucketKey := RateLimitBucketKey(clientAddr)
	if bucketKey == "invalid" && is404Probe {
		bucketKey = "local"
	}
	now := time.Now()

	w.rateMu.Lock()
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
	curScore := sb.score
	curOcc := sb.occurrences

	const threshold = 5
	shouldBan := canBan && curScore >= threshold
	if shouldBan {
		sb.score = 0
	} else if !canBan && curScore >= threshold {
		sb.score = 0
	}
	w.rateMu.Unlock()

	if is404Probe && (weight >= 2 || reasonCode == ReasonScannerTrap || curOcc >= 3) {
		if w.AllowRate("audit404:"+reasonCode, clientAddr, 1, time.Minute) {
			cleanPath := reqPath
			if len(cleanPath) > 180 {
				cleanPath = cleanPath[:180]
			}
			slog.Warn("waf 404 risk probe recorded",
				"reason", reasonCode,
				"path", cleanPath,
				"ip", MaskIP(clientAddr),
				"occurrences", curOcc,
			)
			w.recordSecurityAudit(ctx, clientAddr, "security.waf_404_probe", "ip_ban", cleanPath, map[string]any{
				"reason_code": reasonCode,
				"path":        cleanPath,
				"occurrences": curOcc,
			})
		}
	}

	if !shouldBan {
		return false
	}

	// Escalating ban ladder: 1st ban = 15m, 2nd = 1h, 3rd+ = 24h
	occ := curOcc
	const baseMins = 15
	dur := time.Duration(baseMins) * time.Minute
	if occ >= threshold*3 {
		dur = 24 * time.Hour
	} else if occ >= threshold*2 {
		dur = time.Duration(baseMins*4) * time.Minute
	}
	nowMS := now.UnixMilli()
	expiresAt := now.Add(dur).UnixMilli()

	pfx, ok := parseBanPrefix(bucketKey)
	if !ok {
		return false
	}
	rec := domain.SecurityIPBan{
		ID:          id.Next(),
		CIDR:        pfx.String(),
		Source:      "auto",
		ReasonCode:  reasonCode,
		Reason:      fmt.Sprintf("Automated defense triggered (%s)", reasonCode),
		Occurrences: occ,
		ExpiresAt:   expiresAt,
		CreatedAt:   nowMS,
		UpdatedAt:   nowMS,
	}
	w.bansMu.Lock()
	w.upsertBanLocked(pfx, rec.ExpiresAt, rec.ReasonCode, nowMS)
	w.bansMu.Unlock()

	if w.store != nil {
		var upsert string
		if w.store.Dialect().Name() == "mysql" {
			upsert = `INSERT INTO security_ip_bans (id, cidr, source, reason_code, reason, note, occurrences, expires_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE
  source = IF(expires_at = 0 OR source = 'manual', source, VALUES(source)),
  reason_code = IF(expires_at = 0 OR source = 'manual', reason_code, VALUES(reason_code)),
  reason = IF(expires_at = 0 OR source = 'manual', reason, VALUES(reason)),
  occurrences = GREATEST(occurrences + 1, VALUES(occurrences)),
  expires_at = IF(expires_at = 0, 0, GREATEST(expires_at, VALUES(expires_at))),
  updated_at = VALUES(updated_at)`
		} else {
			upsert = `INSERT INTO security_ip_bans (id, cidr, source, reason_code, reason, note, occurrences, expires_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (cidr) DO UPDATE SET
  source = CASE WHEN security_ip_bans.expires_at = 0 OR security_ip_bans.source = 'manual' THEN security_ip_bans.source ELSE EXCLUDED.source END,
  reason_code = CASE WHEN security_ip_bans.expires_at = 0 OR security_ip_bans.source = 'manual' THEN security_ip_bans.reason_code ELSE EXCLUDED.reason_code END,
  reason = CASE WHEN security_ip_bans.expires_at = 0 OR security_ip_bans.source = 'manual' THEN security_ip_bans.reason ELSE EXCLUDED.reason END,
  occurrences = CASE WHEN security_ip_bans.occurrences >= EXCLUDED.occurrences THEN security_ip_bans.occurrences + 1 ELSE EXCLUDED.occurrences END,
  expires_at = CASE WHEN security_ip_bans.expires_at = 0 THEN 0 WHEN security_ip_bans.expires_at > EXCLUDED.expires_at THEN security_ip_bans.expires_at ELSE EXCLUDED.expires_at END,
  updated_at = EXCLUDED.updated_at`
		}
		_, _ = w.store.ExecBgContext(ctx, upsert,
			rec.ID, rec.CIDR, rec.Source, rec.ReasonCode, rec.Reason, "", rec.Occurrences, rec.ExpiresAt, rec.CreatedAt, rec.UpdatedAt)

		detail := map[string]any{
			"cidr":        rec.CIDR,
			"reason_code": rec.ReasonCode,
			"occurrences": rec.Occurrences,
			"expires_at":  rec.ExpiresAt,
		}
		if reqPath != "" {
			cleanPath := reqPath
			if len(cleanPath) > 180 {
				cleanPath = cleanPath[:180]
			}
			detail["path"] = cleanPath
		}
		slog.Warn("waf automatic ip ban triggered",
			"cidr", rec.CIDR,
			"reason", rec.ReasonCode,
			"occurrences", rec.Occurrences,
		)
		w.recordSecurityAudit(ctx, clientAddr, "security.waf_auto_ban", "ip_ban", rec.CIDR, detail)
	}
	return true
}

func (w *WAFEngine) serveBlocked(rw http.ResponseWriter, r *http.Request, status int, msgKey string) {
	w.bansMu.RLock()
	errFn := w.errorHandler
	w.bansMu.RUnlock()
	if errFn != nil {
		errFn(rw, r, status, msgKey)
		return
	}
	if status == http.StatusNotFound {
		http.NotFound(rw, r)
		return
	}
	http.Error(rw, fmt.Sprintf("%d %s", status, http.StatusText(status)), status)
}

func (w *WAFEngine) isStaffSession(r *http.Request) bool {
	w.bansMu.RLock()
	fn := w.staffBypassCheck
	w.bansMu.RUnlock()
	if fn == nil {
		return false
	}
	return fn(r)
}

func isStaticWAFExemptionPath(p string) bool {
	return strings.HasPrefix(p, "/themes/") ||
		strings.HasPrefix(p, "/uploads/") ||
		strings.HasPrefix(p, "/upload/") ||
		strings.HasPrefix(p, "/image/") ||
		strings.HasPrefix(p, "/images/") ||
		strings.HasPrefix(p, "/posts/image/") ||
		strings.HasPrefix(p, "/posts/images/") ||
		strings.HasPrefix(p, "/posts/upload/") ||
		strings.HasPrefix(p, "/avatars/") ||
		strings.HasPrefix(p, "/plugins/") ||
		strings.HasPrefix(p, "/assets/") ||
		p == "/favicon.ico" ||
		p == "/favicon.svg" ||
		p == "/assets/favicon.svg"
}

func (w *WAFEngine) effectiveAdminPath() (string, bool) {
	base := config.DefaultAdminPath
	if w.cfg != nil {
		if p := strings.Trim(strings.TrimSpace(w.cfg.AdminPath), "/"); p != "" {
			base = "/" + p
		}
	}
	if w.settingsMgr != nil {
		if dyn := strings.Trim(strings.TrimSpace(w.settingsMgr.Snapshot().Security.DynamicLoginPath), "/"); dyn != "" && !strings.ContainsAny(dyn, "/ \t\r\n") {
			base = "/" + dyn
		}
	}
	return base, base != config.DefaultAdminPath
}

func isAuthRateScope(scope string) bool {
	s := strings.ToLower(strings.TrimSpace(scope))
	return strings.HasPrefix(s, "admin:") ||
		strings.HasPrefix(s, "login") ||
		strings.HasPrefix(s, "2fa") ||
		strings.HasPrefix(s, "reader_login") ||
		strings.HasPrefix(s, "reader_code")
}

// AllowRate enforces a fixed-window rate limit keyed by (scope + /32 IPv4 or /64 IPv6 bucket),
// isolating authentication scopes from high-cardinality public scopes so scanner/unlock churn
// can never evict brute-force counters.
func (w *WAFEngine) AllowRate(scope string, clientAddr netip.Addr, limit int, window time.Duration) bool {
	if limit <= 0 {
		return true
	}
	key := scope + "|" + RateLimitBucketKey(clientAddr)
	now := time.Now()
	authScope := isAuthRateScope(scope)

	w.rateMu.Lock()
	defer w.rateMu.Unlock()

	var buckets map[string]*rateWindow
	maxBuckets := maxWAFRates
	if authScope {
		if w.authRates == nil {
			w.authRates = make(map[string]*rateWindow)
		}
		buckets = w.authRates
		maxBuckets = maxWAFAuthRates
	} else {
		if w.rates == nil {
			w.rates = make(map[string]*rateWindow)
		}
		buckets = w.rates
	}

	rw, exists := buckets[key]
	if !exists || now.After(rw.windowReset) {
		if !exists && len(buckets) >= maxBuckets {
			// Bounded GC sweep of expired windows + hard cap eviction so RAM stays tiny under DDoS
			for k, v := range buckets {
				if now.After(v.windowReset) {
					delete(buckets, k)
				}
			}
			for k := range buckets {
				if len(buckets) < maxBuckets {
					break
				}
				delete(buckets, k)
			}
		}
		buckets[key] = &rateWindow{count: 1, windowReset: now.Add(window)}
		return true
	}
	if window > 0 {
		if maxReset := now.Add(window); rw.windowReset.After(maxReset) {
			rw.windowReset = maxReset
		}
	}
	if rw.count >= limit {
		return false
	}
	rw.count++
	return true
}

// WrapHTTP wraps next with:
// 1. /healthz bypass (always fast)
// 2. IP/CIDR ban enforcement (403, bypassed by authenticated staff sessions)
// 3. Offline GeoIP access control (with emergency circuit-breaker + authenticated staff bypass)
// 4. Global WAF sliding-window rate shield (429 when WAFEnabled)
// 5. Custom AdminPath stealth 404 cloaking on default /console
// 6. Automated scanner trap 404 + strike & audit recording
func (w *WAFEngine) WrapHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		// Baseline hardened security headers on every response
		rw.Header().Set("X-Content-Type-Options", "nosniff")
		rw.Header().Set("X-Frame-Options", "SAMEORIGIN")
		rw.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		rw.Header().Set("Permissions-Policy", "camera=(), microphone=(self), geolocation=(), payment=(), usb=()")

		w.bansMu.RLock()
		pubURL := w.publicURL
		w.bansMu.RUnlock()
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

		// 1. Active IP / /64 CIDR Ban Check (authenticated staff sessions bypass IP lockouts so admins can always access /console)
		if banned, _ := w.IsBanned(clientIP); banned && !w.isStaffSession(r) {
			w.serveBlocked(rw, r, http.StatusForbidden, "error.forbidden")
			return
		}

		// 2. Offline GeoIP Access Control (with VOLO_GEO_ACCESS_ENABLED circuit breaker + authenticated staff bypass)
		if w.geo != nil && (w.cfg == nil || w.cfg.GeoAccessEnabled) {
			if allowed := w.geo.Allows(clientIP); !allowed && !w.isStaffSession(r) {
				w.serveBlocked(rw, r, http.StatusForbidden, "error.forbidden")
				return
			}
		}

		// 3. Global WAF Rate Shield (configurable via SecuritySettings)
		if w.settingsMgr != nil && clientIP.IsValid() && !clientIP.IsLoopback() &&
			!isStaticWAFExemptionPath(reqPath) && !strings.HasPrefix(reqPath, "/api/admin/") {
			sec := w.settingsMgr.Snapshot().Security
			if sec.WAFEnabled && sec.WAFMaxRequests > 0 {
				winSec := sec.WAFWindowSeconds
				if winSec <= 0 {
					winSec = 180
				}
				if !w.AllowRate("waf_shield", clientIP, sec.WAFMaxRequests, time.Duration(winSec)*time.Second) {
					if !w.isStaffSession(r) {
						w.recordStrikeInternal(r.Context(), clientIP, ReasonRateAbuse, 2, reqPath, false, false)
						w.serveBlocked(rw, r, http.StatusTooManyRequests, "error.rate_limited")
						return
					}
				}
			}
		}

		// 4. Custom Admin Path Stealth 404 Cloaking!
		// When effective admin path is customized (via VOLO_ADMIN_PATH or DynamicLoginPath),
		// /console and /console/* MUST return an indistinguishable custom 404 Not Found!
		if _, isCustomAdmin := w.effectiveAdminPath(); isCustomAdmin {
			if reqPath == config.DefaultAdminPath || strings.HasPrefix(reqPath, config.DefaultAdminPath+"/") {
				w.RecordProbe404(r.Context(), clientIP, ReasonScannerTrap, 2, reqPath, w.isStaffSession(r))
				w.serveBlocked(rw, r, http.StatusNotFound, "error.not_found")
				return
			}
		}

		// 5. Scanner Trap Paths (/.env, /.git, /wp-admin, etc.)
		lowPath := strings.ToLower(reqPath)
		for _, trap := range commonScannerPrefixes {
			if lowPath == trap || strings.HasPrefix(lowPath, trap+"/") {
				w.RecordProbe404(r.Context(), clientIP, ReasonScannerTrap, 3, reqPath, w.isStaffSession(r))
				w.serveBlocked(rw, r, http.StatusNotFound, "error.not_found")
				return
			}
		}

		next.ServeHTTP(rw, r)
	})
}

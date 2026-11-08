package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/pkg/id"
)

// ErrSettingsConflict indicates an optimistic concurrency ETag/version mismatch on PATCH.
var ErrSettingsConflict = errors.New("settings: version conflict")

// SettingsManager manages atomic per-domain settings with:
// - Lock-free in-memory hot-reload snapshot (Snapshot())
// - Partial JSON Merge PATCH per domain without clobbering sibling fields
// - AES-256-GCM at-rest encryption for sensitive credentials & automatic *Set masking on read
type SettingsManager struct {
	store         *sqlx.Store
	keyRing       *config.KeyRing
	defaultLocale string
	writeMu       sync.Mutex
	snap          atomic.Pointer[domain.SettingsSnapshot]
	rev           atomic.Uint64
}

func NewSettingsManager(store *sqlx.Store, keyRing *config.KeyRing, defaultLocale string) *SettingsManager {
	m := &SettingsManager{
		store:         store,
		keyRing:       keyRing,
		defaultLocale: defaultLocale,
	}
	def := domain.DefaultSettingsSnapshot(defaultLocale)
	m.snap.Store(&def)
	m.rev.Store(1)
	return m
}

// Snapshot returns the current immutable hot-reloaded settings snapshot with zero DB queries.
func (m *SettingsManager) Snapshot() domain.SettingsSnapshot {
	return *m.snap.Load()
}

// Revision returns a monotonically increasing sequence incremented whenever settings reload or patch.
func (m *SettingsManager) Revision() uint64 {
	if m == nil {
		return 0
	}
	return m.rev.Load()
}

// Reload loads all persisted setting domains from the database and swaps the in-memory snapshot atomically.
func (m *SettingsManager) Reload(ctx context.Context) (domain.SettingsSnapshot, error) {
	rows, err := sqlx.Select[domain.SettingRecord](ctx, m.store,
		`SELECT domain, value_json, secret_ciphertext, version, updated_at FROM settings`)
	if err != nil {
		return m.Snapshot(), fmt.Errorf("settings: reload query: %w", err)
	}

	next := domain.DefaultSettingsSnapshot(m.defaultLocale)
	for _, rec := range rows {
		if err := m.applyRecordToSnapshot(&next, rec); err != nil {
			return m.Snapshot(), err
		}
	}
	m.snap.Store(&next)
	m.rev.Add(1)
	return next, nil
}

func (m *SettingsManager) applyRecordToSnapshot(s *domain.SettingsSnapshot, rec domain.SettingRecord) error {
	switch rec.Domain {
	case domain.SettingsDomainSite:
		if err := json.Unmarshal([]byte(rec.ValueJSON), &s.Site); err != nil {
			return err
		}
		if s.Site.LicenseTemplate == "本文由 {author} 原创发布于 {title}，转载请注明原文链接：{url}" {
			s.Site.LicenseTemplate = "本文由 {author} 原创发布于 {site}，转载请注明原文链接：{url}"
		}
		s.Site.NoticeBoard = NormalizeSiteInjectionHTML(s.Site.NoticeBoard)
		s.Site.CustomHeadMeta = NormalizeSiteInjectionHTML(s.Site.CustomHeadMeta)
		s.Site.FooterText = NormalizeSiteInjectionHTML(s.Site.FooterText)
		if s.Site.FaviconURL != "" {
			if cleanFav, ok := SanitizeAllowedURL(s.Site.FaviconURL, false); ok {
				s.Site.FaviconURL = cleanFav
			} else {
				s.Site.FaviconURL = "/favicon.svg"
			}
		}
		if s.Site.LogoURL != "" {
			if cleanLogo, ok := SanitizeAllowedURL(s.Site.LogoURL, false); ok {
				s.Site.LogoURL = cleanLogo
			} else {
				s.Site.LogoURL = ""
			}
		}
		for i := range s.Site.SocialLinks {
			if s.Site.SocialLinks[i].URL != "" {
				if cleanU, ok := SanitizeAllowedURL(s.Site.SocialLinks[i].URL, true); ok {
					s.Site.SocialLinks[i].URL = cleanU
				} else {
					s.Site.SocialLinks[i].URL = "#"
				}
			}
		}
		return nil
	case domain.SettingsDomainReading:
		s.Reading.EnablePJAX = true
		s.Reading.VendorCDNPrefix = domain.DefaultVendorCDNPrefix
		if err := json.Unmarshal([]byte(rec.ValueJSON), &s.Reading); err != nil {
			return err
		}
		if strings.TrimSpace(s.Reading.VendorCDNPrefix) == "" {
			s.Reading.VendorCDNPrefix = domain.DefaultVendorCDNPrefix
		}
		return nil
	case domain.SettingsDomainFeed:
		return json.Unmarshal([]byte(rec.ValueJSON), &s.Feed)
	case domain.SettingsDomainSEO:
		return json.Unmarshal([]byte(rec.ValueJSON), &s.SEO)
	case domain.SettingsDomainStorage:
		if err := json.Unmarshal([]byte(rec.ValueJSON), &s.Storage); err != nil {
			return err
		}
		if sec, err := m.openStorageSecrets(rec.SecretCiphertext); err == nil {
			s.Storage.S3SecretKeySet = sec.S3SecretKey != ""
		} else {
			s.Storage.S3SecretKeySet = false
		}
		return nil
	case domain.SettingsDomainNotifications:
		if err := json.Unmarshal([]byte(rec.ValueJSON), &s.Notifications); err != nil {
			return err
		}
		if sec, err := m.openNotificationsSecrets(rec.SecretCiphertext); err == nil {
			s.Notifications.SMTPPasswordSet = sec.SMTPPassword != ""
		} else {
			s.Notifications.SMTPPasswordSet = false
		}
		return nil
	case domain.SettingsDomainSecurity:
		return json.Unmarshal([]byte(rec.ValueJSON), &s.Security)
	case domain.SettingsDomainAppearance:
		if err := json.Unmarshal([]byte(rec.ValueJSON), &s.Appearance); err != nil {
			return err
		}
		s.Appearance.CustomCSS = sanitizeCustomCSS(s.Appearance.CustomCSS)
		return nil
	case domain.SettingsDomainBackup:
		if err := json.Unmarshal([]byte(rec.ValueJSON), &s.Backup); err != nil {
			return err
		}
		if sec, err := m.openBackupSecrets(rec.SecretCiphertext); err == nil {
			s.Backup.WebDAVPasswordSet = sec.WebDAVPassword != ""
			s.Backup.S3SecretKeySet = sec.S3SecretKey != ""
		} else {
			s.Backup.WebDAVPasswordSet = false
			s.Backup.S3SecretKeySet = false
		}
		return nil
	default:
		return nil
	}
}

// GetDomain returns the masked JSON representation and current version of a single settings domain.
func (m *SettingsManager) GetDomain(ctx context.Context, domainName string) (any, int64, error) {
	domainName = strings.ToLower(strings.TrimSpace(domainName))
	if !isKnownSettingsDomain(domainName) {
		return nil, 0, fmt.Errorf("settings: unknown domain %q", domainName)
	}
	var ver int64 = 1
	rec, err := sqlx.Get[domain.SettingRecord](ctx, m.store,
		`SELECT domain, value_json, secret_ciphertext, version, updated_at FROM settings WHERE domain = ?`, domainName)
	if err == nil {
		ver = rec.Version
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, 0, err
	}

	snap := m.Snapshot()
	return extractDomainFromSnapshot(snap, domainName), ver, nil
}

// PatchDomain atomically merges partial JSON fields into a single settings domain, encrypts any
// non-empty secret inputs via AES-256-GCM, persists to DB, and hot-updates the in-memory snapshot.
// If expectedVersion > 0 and does not match the stored version, ErrSettingsConflict is returned.
func (m *SettingsManager) PatchDomain(ctx context.Context, domainName string, patchJSON []byte, expectedVersion int64) (any, int64, error) {
	domainName = strings.ToLower(strings.TrimSpace(domainName))
	if !isKnownSettingsDomain(domainName) {
		return nil, 0, fmt.Errorf("settings: unknown domain %q", domainName)
	}

	var patchMap map[string]any
	if err := json.Unmarshal(patchJSON, &patchMap); err != nil {
		return nil, 0, fmt.Errorf("settings: invalid JSON patch payload: %w", err)
	}

	m.writeMu.Lock()
	defer m.writeMu.Unlock()

	var newVersion int64
	err := m.store.WithTx(ctx, func(tx *sqlx.Tx) error {
		existing, err := sqlx.Get[domain.SettingRecord](ctx, tx,
			`SELECT domain, value_json, secret_ciphertext, version, updated_at FROM settings WHERE domain = ?`, domainName)
		hasExisting := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if expectedVersion > 0 {
			curVer := int64(1)
			if hasExisting {
				curVer = existing.Version
			}
			if curVer != expectedVersion {
				return ErrSettingsConflict
			}
		}

		curSnap := m.Snapshot()
		if hasExisting {
			if err := m.applyRecordToSnapshot(&curSnap, existing); err != nil {
				return err
			}
		}

		currentDomainVal := extractDomainFromSnapshot(curSnap, domainName)
		baseBytes, err := json.Marshal(currentDomainVal)
		if err != nil {
			return err
		}
		var mergedMap map[string]any
		if err := json.Unmarshal(baseBytes, &mergedMap); err != nil {
			return err
		}

		secretCipher := existing.SecretCiphertext
		secretCipher, err = m.mergeDomainSecrets(domainName, patchMap, secretCipher)
		if err != nil {
			return err
		}

		// Strip write-only secret keys & read-only *Set flags before merging public JSON value.
		delete(patchMap, "s3_secret_key")
		delete(patchMap, "s3_secret_key_set")
		delete(patchMap, "smtp_password")
		delete(patchMap, "smtp_password_set")
		delete(patchMap, "webdav_password")
		delete(patchMap, "webdav_password_set")
		delete(patchMap, "clear_secrets")

		deepMergeMaps(mergedMap, patchMap)
		mergedJSON, err := json.Marshal(mergedMap)
		if err != nil {
			return err
		}

		// Validate round-trip into typed struct + normalize
		normalizedJSON, err := validateAndNormalizeDomainJSON(domainName, mergedJSON)
		if err != nil {
			return err
		}

		if hasExisting {
			newVersion = existing.Version + 1
		} else {
			newVersion = 1
		}
		now := time.Now().UnixMilli()
		upsert := tx.Dialect().UpsertSQL(
			"settings",
			[]string{"domain", "value_json", "secret_ciphertext", "version", "updated_at"},
			[]string{"domain"},
			[]string{"value_json", "secret_ciphertext", "version", "updated_at"},
		)
		_, err = tx.ExecContext(ctx, upsert, domainName, string(normalizedJSON), secretCipher, newVersion, now)
		return err
	})
	if err != nil {
		return nil, 0, err
	}

	snap, err := m.Reload(ctx)
	if err != nil {
		return nil, 0, err
	}
	return extractDomainFromSnapshot(snap, domainName), newVersion, nil
}

func (m *SettingsManager) mergeDomainSecrets(domainName string, patchMap map[string]any, currentCipher string) (string, error) {
	clearSet := parseClearSecretsList(patchMap["clear_secrets"])

	switch domainName {
	case domain.SettingsDomainStorage:
		sec, _ := m.openStorageSecrets(currentCipher)
		if clearSet["s3_secret_key"] {
			sec.S3SecretKey = ""
		} else if raw, ok := patchMap["s3_secret_key"].(string); ok && strings.TrimSpace(raw) != "" {
			sec.S3SecretKey = strings.TrimSpace(raw)
		}
		if sec.S3SecretKey == "" {
			return "", nil
		}
		b, _ := json.Marshal(sec)
		return m.keyRing.SealString(string(b), "settings:storage")

	case domain.SettingsDomainNotifications:
		sec, _ := m.openNotificationsSecrets(currentCipher)
		if clearSet["smtp_password"] {
			sec.SMTPPassword = ""
		} else if raw, ok := patchMap["smtp_password"].(string); ok && raw != "" {
			sec.SMTPPassword = raw
		}
		if sec.SMTPPassword == "" {
			return "", nil
		}
		b, _ := json.Marshal(sec)
		return m.keyRing.SealString(string(b), "settings:notifications")

	case domain.SettingsDomainBackup:
		sec, _ := m.openBackupSecrets(currentCipher)
		if clearSet["webdav_password"] {
			sec.WebDAVPassword = ""
		} else if raw, ok := patchMap["webdav_password"].(string); ok && raw != "" {
			sec.WebDAVPassword = raw
		}
		if clearSet["s3_secret_key"] {
			sec.S3SecretKey = ""
		} else if raw, ok := patchMap["s3_secret_key"].(string); ok && strings.TrimSpace(raw) != "" {
			sec.S3SecretKey = strings.TrimSpace(raw)
		}
		if sec.WebDAVPassword == "" && sec.S3SecretKey == "" {
			return "", nil
		}
		b, _ := json.Marshal(sec)
		return m.keyRing.SealString(string(b), "settings:backup")

	default:
		return currentCipher, nil
	}
}

// GetStorageSecrets decrypts storage credentials for internal runtime use (never exposed via API).
func (m *SettingsManager) GetStorageSecrets(ctx context.Context) (domain.StorageSecrets, error) {
	rec, err := sqlx.Get[domain.SettingRecord](ctx, m.store,
		`SELECT secret_ciphertext FROM settings WHERE domain = ?`, domain.SettingsDomainStorage)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.StorageSecrets{}, nil
		}
		return domain.StorageSecrets{}, err
	}
	return m.openStorageSecrets(rec.SecretCiphertext)
}

// GetNotificationsSecrets decrypts SMTP credentials for internal mailer use.
func (m *SettingsManager) GetNotificationsSecrets(ctx context.Context) (domain.NotificationsSecrets, error) {
	rec, err := sqlx.Get[domain.SettingRecord](ctx, m.store,
		`SELECT secret_ciphertext FROM settings WHERE domain = ?`, domain.SettingsDomainNotifications)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.NotificationsSecrets{}, nil
		}
		return domain.NotificationsSecrets{}, err
	}
	return m.openNotificationsSecrets(rec.SecretCiphertext)
}

// GetBackupSecrets decrypts remote cloud backup credentials for internal sync use.
func (m *SettingsManager) GetBackupSecrets(ctx context.Context) (domain.BackupSecrets, error) {
	rec, err := sqlx.Get[domain.SettingRecord](ctx, m.store,
		`SELECT secret_ciphertext FROM settings WHERE domain = ?`, domain.SettingsDomainBackup)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.BackupSecrets{}, nil
		}
		return domain.BackupSecrets{}, err
	}
	return m.openBackupSecrets(rec.SecretCiphertext)
}

// PeekShareTokenSecret reads the current ShareToken HMAC secret from ReadDB with zero write side-effects.
func (m *SettingsManager) PeekShareTokenSecret(ctx context.Context) string {
	rec, err := sqlx.Get[domain.SettingRecord](ctx, m.store,
		`SELECT secret_ciphertext FROM settings WHERE domain = ?`, domain.SettingsDomainSecurity)
	if err != nil || rec.SecretCiphertext == "" {
		return ""
	}
	opened, openErr := m.keyRing.OpenString(rec.SecretCiphertext, "settings:security")
	if openErr != nil {
		return ""
	}
	var sec domain.SecuritySecrets
	if json.Unmarshal([]byte(opened), &sec) == nil {
		return sec.ShareTokenSecret
	}
	return ""
}

// GetOrInitShareTokenSecret returns (or initializes & encrypts) the global ShareToken HMAC secret,
// supporting one-click rotation via RotateShareTokenSecret.
func (m *SettingsManager) GetOrInitShareTokenSecret(ctx context.Context) (string, error) {
	if cur := m.PeekShareTokenSecret(ctx); len(cur) >= 32 {
		return cur, nil
	}
	return m.RotateShareTokenSecret(ctx)
}

// RotateShareTokenSecret generates a fresh 32-byte random secret, instantly revoking all previously issued ShareTokens.
func (m *SettingsManager) RotateShareTokenSecret(ctx context.Context) (string, error) {
	m.writeMu.Lock()
	defer m.writeMu.Unlock()

	fresh, err := id.RandomHex(32)
	if err != nil {
		return "", err
	}
	secBytes, _ := json.Marshal(domain.SecuritySecrets{ShareTokenSecret: fresh})
	sealed, err := m.keyRing.SealString(string(secBytes), "settings:security")
	if err != nil {
		return "", err
	}

	err = m.store.WithTx(ctx, func(tx *sqlx.Tx) error {
		existing, qErr := sqlx.Get[domain.SettingRecord](ctx, tx,
			`SELECT domain, value_json, secret_ciphertext, version, updated_at FROM settings WHERE domain = ?`, domain.SettingsDomainSecurity)
		valJSON := ""
		ver := int64(1)
		if qErr == nil {
			valJSON = existing.ValueJSON
			ver = existing.Version + 1
		} else if errors.Is(qErr, sql.ErrNoRows) {
			b, _ := json.Marshal(m.Snapshot().Security)
			valJSON = string(b)
		} else {
			return qErr
		}
		upsert := tx.Dialect().UpsertSQL(
			"settings",
			[]string{"domain", "value_json", "secret_ciphertext", "version", "updated_at"},
			[]string{"domain"},
			[]string{"value_json", "secret_ciphertext", "version", "updated_at"},
		)
		_, execErr := tx.ExecContext(ctx, upsert, domain.SettingsDomainSecurity, valJSON, sealed, ver, time.Now().UnixMilli())
		return execErr
	})
	return fresh, err
}

func (m *SettingsManager) openStorageSecrets(cipher string) (domain.StorageSecrets, error) {
	if cipher == "" {
		return domain.StorageSecrets{}, nil
	}
	pt, err := m.keyRing.OpenString(cipher, "settings:storage")
	if err != nil {
		return domain.StorageSecrets{}, err
	}
	var out domain.StorageSecrets
	return out, json.Unmarshal([]byte(pt), &out)
}

func (m *SettingsManager) openNotificationsSecrets(cipher string) (domain.NotificationsSecrets, error) {
	if cipher == "" {
		return domain.NotificationsSecrets{}, nil
	}
	pt, err := m.keyRing.OpenString(cipher, "settings:notifications")
	if err != nil {
		return domain.NotificationsSecrets{}, err
	}
	var out domain.NotificationsSecrets
	return out, json.Unmarshal([]byte(pt), &out)
}

func (m *SettingsManager) openBackupSecrets(cipher string) (domain.BackupSecrets, error) {
	if cipher == "" {
		return domain.BackupSecrets{}, nil
	}
	pt, err := m.keyRing.OpenString(cipher, "settings:backup")
	if err != nil {
		return domain.BackupSecrets{}, err
	}
	var out domain.BackupSecrets
	return out, json.Unmarshal([]byte(pt), &out)
}

func isKnownSettingsDomain(d string) bool {
	switch d {
	case domain.SettingsDomainSite,
		domain.SettingsDomainReading,
		domain.SettingsDomainFeed,
		domain.SettingsDomainSEO,
		domain.SettingsDomainStorage,
		domain.SettingsDomainNotifications,
		domain.SettingsDomainSecurity,
		domain.SettingsDomainAppearance,
		domain.SettingsDomainBackup:
		return true
	default:
		return false
	}
}

func extractDomainFromSnapshot(s domain.SettingsSnapshot, d string) any {
	switch d {
	case domain.SettingsDomainSite:
		return s.Site
	case domain.SettingsDomainReading:
		return s.Reading
	case domain.SettingsDomainFeed:
		return s.Feed
	case domain.SettingsDomainSEO:
		return s.SEO
	case domain.SettingsDomainStorage:
		return s.Storage
	case domain.SettingsDomainNotifications:
		return s.Notifications
	case domain.SettingsDomainSecurity:
		return s.Security
	case domain.SettingsDomainAppearance:
		return s.Appearance
	case domain.SettingsDomainBackup:
		return s.Backup
	default:
		return nil
	}
}

func validateAndNormalizeDomainJSON(d string, raw []byte) ([]byte, error) {
	def := domain.DefaultSettingsSnapshot("zh-CN")
	switch d {
	case domain.SettingsDomainSite:
		v := def.Site
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		if strings.TrimSpace(v.Title) == "" {
			return nil, errors.New("settings: site.title cannot be empty")
		}
		if v.LicenseTemplate == "本文由 {author} 原创发布于 {title}，转载请注明原文链接：{url}" {
			v.LicenseTemplate = "本文由 {author} 原创发布于 {site}，转载请注明原文链接：{url}"
		}
		v.FaviconURL = strings.TrimSpace(StripASCIIControls(v.FaviconURL))
		if v.FaviconURL != "" {
			cleanFav, ok := SanitizeAllowedURL(v.FaviconURL, false)
			if !ok {
				return nil, fmt.Errorf("settings: unsafe or invalid site.favicon_url %q", v.FaviconURL)
			}
			v.FaviconURL = cleanFav
		}
		v.LogoURL = strings.TrimSpace(StripASCIIControls(v.LogoURL))
		if v.LogoURL != "" {
			cleanLogo, ok := SanitizeAllowedURL(v.LogoURL, false)
			if !ok {
				return nil, fmt.Errorf("settings: unsafe or invalid site.logo_url %q", v.LogoURL)
			}
			v.LogoURL = cleanLogo
		}
		if v.SocialLinks == nil {
			v.SocialLinks = []domain.SocialLink{}
		}
		for i := range v.SocialLinks {
			rawU := strings.TrimSpace(StripASCIIControls(v.SocialLinks[i].URL))
			if rawU != "" {
				cleanU, ok := SanitizeAllowedURL(rawU, true)
				if !ok {
					return nil, fmt.Errorf("settings: unsafe or invalid site.social_links[%d].url %q", i, v.SocialLinks[i].URL)
				}
				v.SocialLinks[i].URL = cleanU
			} else {
				v.SocialLinks[i].URL = ""
			}
		}
		v.CustomHeadMeta = NormalizeSiteInjectionHTML(v.CustomHeadMeta)
		v.FooterText = NormalizeSiteInjectionHTML(v.FooterText)
		v.NoticeBoard = NormalizeSiteInjectionHTML(v.NoticeBoard)
		if v.CustomVars == nil {
			v.CustomVars = map[string]string{}
		}
		return json.Marshal(v)
	case domain.SettingsDomainReading:
		v := def.Reading
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		if v.PostsPerPage < 1 || v.PostsPerPage > 100 {
			v.PostsPerPage = 15
		}
		if v.PoWDifficultyBits < 8 || v.PoWDifficultyBits > 24 {
			v.PoWDifficultyBits = 16
		}
		cleanAssetPrefix, okAsset := sanitizeCDNPrefix(v.AssetCDNPrefix)
		if !okAsset {
			return nil, fmt.Errorf("settings: invalid reading.asset_cdn_prefix %q", v.AssetCDNPrefix)
		}
		v.AssetCDNPrefix = cleanAssetPrefix
		cleanVendorPrefix, okVendor := sanitizeCDNPrefix(v.VendorCDNPrefix)
		if !okVendor {
			return nil, fmt.Errorf("settings: invalid reading.vendor_cdn_prefix %q", v.VendorCDNPrefix)
		}
		if cleanVendorPrefix == "" {
			cleanVendorPrefix = domain.DefaultVendorCDNPrefix
		}
		v.VendorCDNPrefix = cleanVendorPrefix
		return json.Marshal(v)
	case domain.SettingsDomainFeed:
		var v domain.FeedSettings
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		if v.ItemCount < 1 || v.ItemCount > 100 {
			v.ItemCount = 20
		}
		return json.Marshal(v)
	case domain.SettingsDomainSEO:
		var v domain.SEOSettings
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		if v.GitHubRepos == nil {
			v.GitHubRepos = []domain.GitHubRepoItem{}
		}
		return json.Marshal(v)
	case domain.SettingsDomainStorage:
		var v domain.StorageSettings
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		if !isValidStorageDriverID(v.ActiveDriver) {
			return nil, fmt.Errorf("settings: invalid storage.active_driver %q", v.ActiveDriver)
		}
		v.LocalDir = strings.TrimSpace(StripASCIIControls(v.LocalDir))
		if strings.Contains(v.LocalDir, "..") {
			return nil, fmt.Errorf("settings: invalid storage.local_dir %q", v.LocalDir)
		}
		cleanKeyPrefix := strings.Trim(strings.ReplaceAll(strings.TrimSpace(StripASCIIControls(v.LocalKeyPrefix)), "\\", "/"), "/")
		if strings.Contains(cleanKeyPrefix, "..") || len(cleanKeyPrefix) > 120 {
			return nil, fmt.Errorf("settings: invalid storage.local_key_prefix %q", v.LocalKeyPrefix)
		}
		for _, r := range cleanKeyPrefix {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '/' || r == '-' || r == '_' || r == '.') {
				return nil, fmt.Errorf("settings: invalid character in storage.local_key_prefix %q", v.LocalKeyPrefix)
			}
		}
		v.LocalKeyPrefix = cleanKeyPrefix
		v.S3SecretKeySet = false
		return json.Marshal(v)
	case domain.SettingsDomainNotifications:
		var v domain.NotificationsSettings
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		v.SMTPPasswordSet = false
		return json.Marshal(v)
	case domain.SettingsDomainSecurity:
		var v domain.SecuritySettings
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		normDyn, err := normalizeDynamicLoginPath(v.DynamicLoginPath)
		if err != nil {
			return nil, err
		}
		v.DynamicLoginPath = normDyn
		v.GeoIPDir = strings.TrimSpace(StripASCIIControls(v.GeoIPDir))
		return json.Marshal(v)
	case domain.SettingsDomainAppearance:
		v := def.Appearance
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		switch v.Accent {
		case "forest", "ocean", "teal", "amber", "plum", "rose":
		default:
			return nil, fmt.Errorf("settings: invalid appearance.accent %q", v.Accent)
		}
		v.MobileTheme = strings.TrimSpace(v.MobileTheme)
		if v.MobileTheme == "same" {
			v.MobileTheme = ""
		}
		v.CustomCSS = sanitizeCustomCSS(v.CustomCSS)
		if v.ThemeConfigs == nil {
			v.ThemeConfigs = map[string]map[string]any{}
		}
		return json.Marshal(v)
	case domain.SettingsDomainBackup:
		var v domain.BackupSettings
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		if v.LocalKeepCount < 1 {
			v.LocalKeepCount = 7
		}
		v.WebDAVPasswordSet = false
		v.S3SecretKeySet = false
		return json.Marshal(v)
	default:
		return raw, nil
	}
}

func deepMergeMaps(dst map[string]any, src map[string]any) {
	for k, sv := range src {
		if sv == nil {
			delete(dst, k)
			continue
		}
		if srcSub, ok := sv.(map[string]any); ok {
			if dstSub, okDst := dst[k].(map[string]any); okDst {
				deepMergeMaps(dstSub, srcSub)
				continue
			}
		}
		dst[k] = sv
	}
}

func parseClearSecretsList(v any) map[string]bool {
	out := make(map[string]bool)
	arr, ok := v.([]any)
	if !ok {
		return out
	}
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out[strings.TrimSpace(s)] = true
		}
	}
	return out
}

// StripASCIIControls strips all ASCII control characters (<= 0x1F and 0x7F) anywhere in s.
func StripASCIIControls(s string) string {
	hasCtrl := false
	for i := 0; i < len(s); i++ {
		if s[i] <= 0x1F || s[i] == 0x7F {
			hasCtrl = true
			break
		}
	}
	if !hasCtrl {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= 0x1F || c == 0x7F {
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// SanitizeAllowedURL strips ASCII control characters first, then enforces a strict scheme allowlist:
//   - http://, https://, or single-slash site-relative /... NOT starting with // or /\
//   - When allowMailTelAnchor is true, also allows mailto:, tel:, and #...
func SanitizeAllowedURL(raw string, allowMailTelAnchor bool) (string, bool) {
	clean := strings.TrimSpace(StripASCIIControls(raw))
	if clean == "" {
		return "", false
	}
	if strings.HasPrefix(clean, "/") {
		if strings.HasPrefix(clean, "//") || strings.HasPrefix(clean, "/\\") {
			return "", false
		}
		return clean, true
	}
	if allowMailTelAnchor && strings.HasPrefix(clean, "#") {
		return clean, true
	}
	if hasPrefixFoldASCII(clean, "http://") && len(clean) > len("http://") {
		return clean, true
	}
	if hasPrefixFoldASCII(clean, "https://") && len(clean) > len("https://") {
		return clean, true
	}
	if allowMailTelAnchor {
		if hasPrefixFoldASCII(clean, "mailto:") && len(clean) > len("mailto:") {
			return clean, true
		}
		if hasPrefixFoldASCII(clean, "tel:") && len(clean) > len("tel:") {
			return clean, true
		}
	}
	return "", false
}

func asciiLowerByte(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

func hasPrefixFoldASCII(s, lowerASCIIPrefix string) bool {
	if len(s) < len(lowerASCIIPrefix) {
		return false
	}
	for i := 0; i < len(lowerASCIIPrefix); i++ {
		if asciiLowerByte(s[i]) != lowerASCIIPrefix[i] {
			return false
		}
	}
	return true
}

func indexFoldASCII(s, lowerASCIINeedle string) int {
	n := len(lowerASCIINeedle)
	if n == 0 || len(s) < n {
		return -1
	}
	first := lowerASCIINeedle[0]
	limit := len(s) - n
	for i := 0; i <= limit; i++ {
		if asciiLowerByte(s[i]) != first {
			continue
		}
		match := true
		for j := 1; j < n; j++ {
			if asciiLowerByte(s[i+j]) != lowerASCIINeedle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// sanitizeCustomCSS strips NUL bytes, removes ASCII case-insensitive `javascript:` / `expression(`,
// and escapes every `</` sequence to `<\/` in a fixed-point loop without ever using strings.ToLower indices.
func sanitizeCustomCSS(css string) string {
	s := strings.TrimSpace(css)
	if s == "" {
		return ""
	}
	for {
		prev := s
		s = strings.ReplaceAll(s, "\x00", "")
		for _, bad := range []string{"javascript:", "expression("} {
			for {
				idx := indexFoldASCII(s, bad)
				if idx < 0 {
					break
				}
				s = s[:idx] + s[idx+len(bad):]
			}
		}
		s = strings.ReplaceAll(s, "</", `<\/`)
		if s == prev {
			break
		}
	}
	return s
}

const maxSiteInjectionBytes = 65536

// NormalizeSiteInjectionHTML cleans NUL bytes and CRLF line endings while preserving
// Admin-configured <head> and footer HTML/script snippets (e.g. verification tags,
// Google AdSense, Analytics, ICP links). Write access is strictly RoleAdmin-only.
func NormalizeSiteInjectionHTML(raw string) string {
	cleaned := strings.ReplaceAll(raw, "\x00", "")
	cleaned = strings.ReplaceAll(cleaned, "\r\n", "\n")
	cleaned = strings.TrimSpace(cleaned)
	if len(cleaned) > maxSiteInjectionBytes {
		cleaned = strings.TrimSpace(cleaned[:maxSiteInjectionBytes])
	}
	return cleaned
}

// SanitizeCustomHeadMeta is retained as an alias for NormalizeSiteInjectionHTML.
func SanitizeCustomHeadMeta(raw string) string {
	return NormalizeSiteInjectionHTML(raw)
}

func isValidStorageDriverID(id string) bool {
	if id == "local" || id == "s3" {
		return true
	}
	if len(id) < 2 || len(id) > 63 {
		return false
	}
	first := id[0]
	if !((first >= 'a' && first <= 'z') || (first >= '0' && first <= '9')) {
		return false
	}
	for i := 1; i < len(id); i++ {
		c := id[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func sanitizeCDNPrefix(raw string) (string, bool) {
	s := strings.TrimSpace(StripASCIIControls(raw))
	if s == "" || s == "/" {
		return "", true
	}
	clean := strings.TrimRight(s, "/")
	if clean == "" {
		return "", true
	}
	for i := 0; i < len(clean); i++ {
		switch clean[i] {
		case ' ', '\t', '\n', '\r', '<', '>', '"', '\'', '`', '\\', '?', '#':
			return "", false
		}
	}
	if strings.Contains(clean, "..") {
		return "", false
	}
	low := strings.ToLower(clean)
	if strings.HasPrefix(low, "https://") {
		return clean, len(clean) > len("https://") && clean[len("https://")] != '/'
	}
	if strings.HasPrefix(low, "http://") {
		return clean, len(clean) > len("http://") && clean[len("http://")] != '/'
	}
	if strings.HasPrefix(low, "//") {
		return clean, len(clean) > 2 && clean[2] != '/'
	}
	if clean[0] == '/' {
		return clean, len(clean) > 1 && clean[1] != '/'
	}
	return "", false
}

var reservedDynamicLoginPrefixes = []string{
	"/api", "/setup", "/posts", "/articles", "/categories", "/category", "/tags",
	"/archives", "/archive", "/archivedate", "/series", "/books", "/links", "/follows",
	"/dynamic", "/uploads", "/images", "/themes", "/plugins", "/avatars", "/assets",
	"/static", "/reader", "/logout", "/healthz", "/github", "/my-github-repos",
	"/github-stats", "/rss.xml", "/atom.xml", "/feed.json", "/podcast.xml",
	"/sitemap.xml", "/robots.txt", "/llms.txt", "/llms-full.txt", "/favicon.svg", "/favicon.ico",
}

func normalizeDynamicLoginPath(raw string) (string, error) {
	s := strings.TrimSpace(StripASCIIControls(raw))
	if s == "" || s == "/" {
		return "", nil
	}
	if !strings.HasPrefix(s, "/") {
		s = "/" + s
	}
	s = strings.TrimRight(s, "/")
	if s == "" {
		return "", nil
	}
	if len(s) < 2 || len(s) > 64 || strings.Contains(s, "..") || strings.Contains(s, "//") {
		return "", fmt.Errorf("settings: invalid dynamic_login_path")
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '/') {
			return "", fmt.Errorf("settings: invalid dynamic_login_path")
		}
	}
	low := strings.ToLower(s)
	for _, pfx := range reservedDynamicLoginPrefixes {
		if low == pfx || strings.HasPrefix(low, pfx+"/") {
			return "", fmt.Errorf("settings: invalid dynamic_login_path")
		}
	}
	return s, nil
}

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
		return json.Unmarshal([]byte(rec.ValueJSON), &s.Site)
	case domain.SettingsDomainReading:
		return json.Unmarshal([]byte(rec.ValueJSON), &s.Reading)
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
		return json.Unmarshal([]byte(rec.ValueJSON), &s.Appearance)
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
	switch d {
	case domain.SettingsDomainSite:
		var v domain.SiteSettings
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		if strings.TrimSpace(v.Title) == "" {
			return nil, errors.New("settings: site.title cannot be empty")
		}
		if v.SocialLinks == nil {
			v.SocialLinks = []domain.SocialLink{}
		}
		if v.CustomVars == nil {
			v.CustomVars = map[string]string{}
		}
		return json.Marshal(v)
	case domain.SettingsDomainReading:
		var v domain.ReadingSettings
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		if v.PostsPerPage < 1 || v.PostsPerPage > 100 {
			v.PostsPerPage = 15
		}
		if v.PoWDifficultyBits < 8 || v.PoWDifficultyBits > 24 {
			v.PoWDifficultyBits = 16
		}
		v.AssetCDNPrefix = strings.TrimRight(strings.TrimSpace(v.AssetCDNPrefix), "/")
		if v.AssetCDNPrefix != "" {
			low := strings.ToLower(v.AssetCDNPrefix)
			if !strings.HasPrefix(low, "https://") && !strings.HasPrefix(low, "http://") && !strings.HasPrefix(low, "//") {
				return nil, fmt.Errorf("settings: invalid reading.asset_cdn_prefix %q", v.AssetCDNPrefix)
			}
		}
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
		return json.Marshal(v)
	case domain.SettingsDomainAppearance:
		var v domain.AppearanceSettings
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		switch v.Accent {
		case "forest", "ocean", "teal", "amber", "plum", "rose":
		default:
			return nil, fmt.Errorf("settings: invalid appearance.accent %q", v.Accent)
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

func sanitizeCustomCSS(css string) string {
	s := strings.TrimSpace(css)
	if s == "" {
		return ""
	}
	// Strip any case variation of `</style` or `<script` so inline `<style>` injection can never break out into HTML/JS.
	for {
		lower := strings.ToLower(s)
		idx := strings.Index(lower, "</style")
		if idx < 0 {
			idx = strings.Index(lower, "<script")
		}
		if idx < 0 {
			break
		}
		s = s[:idx] + s[idx+7:]
	}
	return s
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

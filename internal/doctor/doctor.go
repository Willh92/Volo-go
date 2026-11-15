package doctor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/security"
)

const (
	StatusOK   = "ok"
	StatusWarn = "warn"
	StatusFail = "fail"
)

// CheckResult describes a single diagnostic probe executed by volo doctor.
type CheckResult struct {
	Name    string            `json:"name"`
	Status  string            `json:"status"` // ok | warn | fail
	Code    string            `json:"code,omitempty"`
	Params  map[string]string `json:"params,omitempty"`
	Summary string            `json:"summary"`
	Detail  string            `json:"detail,omitempty"`
}

// Report aggregates all diagnostic checks and overall health status.
type Report struct {
	Healthy   bool          `json:"healthy"`
	CheckedAt int64         `json:"checked_at"`
	Checks    []CheckResult `json:"checks"`
}

func cleanDisplayPath(p string) string {
	trimmed := strings.TrimSpace(p)
	if trimmed == "" {
		return ""
	}
	return filepath.Clean(trimmed)
}

func cleanSecretSource(src string) string {
	for _, pfx := range []string{"file:", "generated:"} {
		if strings.HasPrefix(src, pfx) {
			return pfx + cleanDisplayPath(strings.TrimPrefix(src, pfx))
		}
	}
	return src
}

// isSecretKeyModeValid enforces regular non-symlink files on all OSes and strict 0600 on Unix.
// On Windows NTFS, Go's os.Lstat synthesizes 0666 for writable regular files while ACLs govern access.
func isSecretKeyModeValid(goos string, mode os.FileMode) bool {
	if mode&os.ModeSymlink != 0 || !mode.IsRegular() {
		return false
	}
	if goos == "windows" {
		return true
	}
	return mode.Perm() == 0o600
}

// Run executes non-destructive environment, cryptographic, database, and filesystem checks.
func Run(ctx context.Context, cfg *config.Config, store *sqlx.Store) *Report {
	rep := &Report{
		Healthy:   true,
		CheckedAt: time.Now().UnixMilli(),
	}
	add := func(res CheckResult) {
		if res.Status == StatusFail {
			rep.Healthy = false
		}
		rep.Checks = append(rep.Checks, res)
	}

	// 1. Runtime & Pure-Go CGO_ENABLED=0 check
	add(checkRuntime())

	// 2. Master Secret Key & DB Secret Ciphertext Decryptability
	add(checkCryptoAndSecrets(ctx, cfg, store))

	// 3. Database Connectivity, Dialect, Schema Version & WAL/FK Invariants
	add(checkDatabase(ctx, cfg, store))

	// 4. Writable Directories Probe (DataDir, UploadDir, BackupDir)
	add(checkWritableDirs(cfg))

	// 5. Optional Offline GeoIP Databases
	add(checkGeoIPAssets(ctx, cfg, store))

	// 5b. Native HTTPS certificate (only when VOLO_TLS_CERT_FILE/VOLO_TLS_KEY_FILE are configured)
	if cfg.TLSEnabled() {
		add(checkTLSCertificate(cfg, time.Now()))
	}

	// 6. Admin Console Path Cloaking Posture
	adminPath := "/console"
	isCustomAdmin := false
	if cfg != nil {
		if strings.TrimSpace(cfg.AdminPath) != "" {
			adminPath = cfg.AdminPath
		}
		isCustomAdmin = cfg.IsCustomAdminPath
	}
	if store != nil {
		var secJSON string
		if err := store.QueryRowContext(ctx, store.Dialect().Rebind(`SELECT value_json FROM settings WHERE domain = ?`), "security").Scan(&secJSON); err == nil && secJSON != "" {
			var sec struct {
				DynamicLoginPath string `json:"dynamic_login_path"`
			}
			if json.Unmarshal([]byte(secJSON), &sec) == nil {
				if dyn := "/" + strings.Trim(strings.TrimSpace(sec.DynamicLoginPath), "/"); dyn != "/" {
					adminPath = dyn
					isCustomAdmin = dyn != "/console" || isCustomAdmin
				}
			}
		}
	}
	if isCustomAdmin {
		add(CheckResult{
			Name:    "admin_stealth_path",
			Status:  StatusOK,
			Code:    "admin_path_custom_ok",
			Params:  map[string]string{"path": adminPath},
			Summary: fmt.Sprintf("custom admin path active (%s); default /console cloaked with 404", adminPath),
		})
	} else {
		add(CheckResult{
			Name:    "admin_stealth_path",
			Status:  StatusOK,
			Code:    "admin_path_default_ok",
			Params:  map[string]string{"path": adminPath},
			Summary: fmt.Sprintf("default admin path %s active (optional: set VOLO_ADMIN_PATH or --admin-path for 404 stealth cloaking)", adminPath),
		})
	}

	return rep
}

func checkRuntime() CheckResult {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	heapStr := fmt.Sprintf("%.2f", float64(ms.HeapAlloc)/(1024*1024))
	sysStr := fmt.Sprintf("%.2f", float64(ms.Sys)/(1024*1024))

	cgoSetting := "0"
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "CGO_ENABLED" {
				cgoSetting = s.Value
			}
		}
	}

	return CheckResult{
		Name:   "runtime",
		Status: StatusOK,
		Code:   "runtime_ok",
		Params: map[string]string{
			"version": runtime.Version(),
			"os":      runtime.GOOS,
			"arch":    runtime.GOARCH,
			"cgo":     cgoSetting,
			"heap":    heapStr,
			"sys":     sysStr,
		},
		Summary: fmt.Sprintf("%s %s/%s (CGO_ENABLED=%s, heap=%s MiB, sys=%s MiB)",
			runtime.Version(), runtime.GOOS, runtime.GOARCH, cgoSetting, heapStr, sysStr),
	}
}

func verifyAppearanceThemeConfigSecrets(kr *config.KeyRing, rawJSON string) (string, string, error) {
	if kr == nil || strings.TrimSpace(rawJSON) == "" {
		return "", "", nil
	}
	var payload struct {
		ThemeConfigs map[string]map[string]any `json:"theme_configs"`
	}
	if err := json.Unmarshal([]byte(rawJSON), &payload); err != nil || len(payload.ThemeConfigs) == 0 {
		return "", "", nil
	}
	var walk func(v any, aad string) error
	walk = func(v any, aad string) error {
		switch tv := v.(type) {
		case string:
			if strings.HasPrefix(tv, "enc:v1:") {
				if _, err := kr.OpenString(tv, aad); err != nil {
					return err
				}
			}
		case []any:
			for _, elem := range tv {
				if err := walk(elem, aad); err != nil {
					return err
				}
			}
		case map[string]any:
			for _, elem := range tv {
				if err := walk(elem, aad); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for themeID, cfgMap := range payload.ThemeConfigs {
		cleanTheme := strings.TrimSpace(themeID)
		for key, val := range cfgMap {
			cleanKey := strings.TrimSpace(key)
			aad := "theme.config:" + cleanTheme + ":" + cleanKey
			if err := walk(val, aad); err != nil {
				return cleanTheme, cleanKey, err
			}
		}
	}
	return "", "", nil
}

func checkCryptoAndSecrets(ctx context.Context, cfg *config.Config, store *sqlx.Store) CheckResult {
	if cfg == nil {
		return CheckResult{
			Name:    "secret_key",
			Status:  StatusFail,
			Code:    "secret_uninit",
			Summary: "KeyRing is uninitialized",
		}
	}
	if strings.HasPrefix(cfg.SecretKeySource, "missing:") {
		missingPath := cleanDisplayPath(strings.TrimPrefix(cfg.SecretKeySource, "missing:"))
		return CheckResult{
			Name:    "secret_key",
			Status:  StatusWarn,
			Code:    "secret_missing_warn",
			Params:  map[string]string{"path": missingPath},
			Summary: fmt.Sprintf("secret key file %s not initialized yet and VOLO_SECRET_KEY unset (will be auto-generated on first volo serve)", missingPath),
		}
	}
	if cfg.KeyRing == nil {
		return CheckResult{
			Name:    "secret_key",
			Status:  StatusFail,
			Code:    "secret_uninit",
			Summary: "KeyRing is uninitialized",
		}
	}

	// Verify file integrity & permissions if file-backed
	for _, pfx := range []string{"file:", "generated:"} {
		if strings.HasPrefix(cfg.SecretKeySource, pfx) {
			keyPath := strings.TrimPrefix(cfg.SecretKeySource, pfx)
			dispPath := cleanDisplayPath(keyPath)
			info, err := os.Lstat(keyPath)
			if err != nil {
				return CheckResult{
					Name:    "secret_key",
					Status:  StatusFail,
					Code:    "secret_stat_err",
					Params:  map[string]string{"path": dispPath, "err": err.Error()},
					Summary: fmt.Sprintf("cannot stat secret key file %s: %v", dispPath, err),
				}
			}
			if !isSecretKeyModeValid(runtime.GOOS, info.Mode()) {
				modeStr := fmt.Sprintf("%04o", info.Mode().Perm())
				return CheckResult{
					Name:    "secret_key",
					Status:  StatusFail,
					Code:    "secret_perm_err",
					Params:  map[string]string{"path": dispPath, "mode": modeStr},
					Summary: fmt.Sprintf("secret key file %s must be regular mode 0600 (got %s)", dispPath, modeStr),
				}
			}
		}
	}

	sealed, err := cfg.KeyRing.SealString("volo-doctor-probe", "doctor:probe")
	if err != nil {
		return CheckResult{
			Name:    "secret_key",
			Status:  StatusFail,
			Code:    "secret_seal_err",
			Params:  map[string]string{"err": err.Error()},
			Summary: fmt.Sprintf("AES-GCM seal failed: %v", err),
		}
	}
	opened, err := cfg.KeyRing.OpenString(sealed, "doctor:probe")
	if err != nil || opened != "volo-doctor-probe" {
		return CheckResult{
			Name:    "secret_key",
			Status:  StatusFail,
			Code:    "secret_roundtrip_err",
			Summary: "AES-GCM round-trip verification failed",
		}
	}

	// 1/8: Verify data/db.json if present on disk
	if strings.TrimSpace(cfg.DataDir) != "" {
		if _, wErr := config.LoadWizardDBConfig(cfg.DataDir, cfg.KeyRing); wErr != nil {
			return CheckResult{
				Name:    "secret_key",
				Status:  StatusWarn,
				Code:    "secret_mismatch_warn",
				Params:  map[string]string{"domain": "db.json", "store": "db.json"},
				Summary: fmt.Sprintf("data/db.json dsn_ciphertext cannot be decrypted with current VOLO_SECRET_KEY: %v", wErr),
			}
		}
	}

	// Verify existing encrypted secrets across all 7 database secret stores
	if store != nil {
		// 2/8: settings.secret_ciphertext
		var settingRows []domain.SettingRecord
		if qErr := sqlx.SelectInto(ctx, store, &settingRows,
			`SELECT domain, secret_ciphertext FROM settings WHERE secret_ciphertext <> ''`); qErr == nil {
			for _, r := range settingRows {
				if _, oErr := cfg.KeyRing.OpenString(r.SecretCiphertext, "settings:"+r.Domain); oErr != nil {
					return CheckResult{
						Name:    "secret_key",
						Status:  StatusWarn,
						Code:    "secret_mismatch_warn",
						Params:  map[string]string{"domain": r.Domain, "store": "settings"},
						Summary: fmt.Sprintf("settings.%s secret_ciphertext cannot be decrypted with current VOLO_SECRET_KEY (re-enter secret after cross-machine restore)", r.Domain),
					}
				}
			}
		}

		// 3/8: user_credentials.secret_ciphertext (kind='totp')
		var totpRows []struct {
			UserID           string `db:"user_id"`
			SecretCiphertext string `db:"secret_ciphertext"`
		}
		if qErr := sqlx.SelectInto(ctx, store, &totpRows,
			`SELECT user_id, secret_ciphertext FROM user_credentials WHERE kind = 'totp' AND secret_ciphertext <> ''`); qErr == nil {
			for _, r := range totpRows {
				if _, oErr := cfg.KeyRing.OpenString(r.SecretCiphertext, "cred:totp:"+r.UserID); oErr != nil {
					return CheckResult{
						Name:    "secret_key",
						Status:  StatusWarn,
						Code:    "secret_mismatch_warn",
						Params:  map[string]string{"domain": "user_credentials.totp", "store": "user_credentials", "user_id": r.UserID},
						Summary: fmt.Sprintf("user_credentials(totp) for user %s cannot be decrypted with current VOLO_SECRET_KEY", r.UserID),
					}
				}
			}
		}

		// 4/8: plugins.secret_ciphertext
		var pluginRows []struct {
			ID               string `db:"id"`
			SecretCiphertext string `db:"secret_ciphertext"`
		}
		if qErr := sqlx.SelectInto(ctx, store, &pluginRows,
			`SELECT id, secret_ciphertext FROM plugins WHERE secret_ciphertext <> ''`); qErr == nil {
			sb := cfg.KeyRing.SecretBox()
			for _, r := range pluginRows {
				if _, oErr := sb.Open(r.SecretCiphertext); oErr != nil {
					return CheckResult{
						Name:    "secret_key",
						Status:  StatusWarn,
						Code:    "secret_mismatch_warn",
						Params:  map[string]string{"domain": "plugins." + r.ID, "store": "plugins", "plugin_id": r.ID},
						Summary: fmt.Sprintf("plugins.%s secret_ciphertext cannot be decrypted with current VOLO_SECRET_KEY", r.ID),
					}
				}
			}
		}

		// 5/8: webhooks.secret_ciphertext
		var webhookRows []struct {
			ID               string `db:"id"`
			SecretCiphertext string `db:"secret_ciphertext"`
		}
		if qErr := sqlx.SelectInto(ctx, store, &webhookRows,
			`SELECT id, secret_ciphertext FROM webhooks WHERE secret_ciphertext <> ''`); qErr == nil {
			for _, r := range webhookRows {
				if _, oErr := cfg.KeyRing.OpenStringFallback(r.SecretCiphertext, config.WebhookSecretAAD(r.ID), "webhooks:secret:"+r.ID); oErr != nil {
					return CheckResult{
						Name:    "secret_key",
						Status:  StatusWarn,
						Code:    "secret_mismatch_warn",
						Params:  map[string]string{"domain": "webhooks." + r.ID, "store": "webhooks", "webhook_id": r.ID},
						Summary: fmt.Sprintf("webhooks.%s secret_ciphertext cannot be decrypted with current VOLO_SECRET_KEY", r.ID),
					}
				}
			}
		}

		// 6/8: articles.password_ciphertext
		var articleRows []struct {
			ID                 string `db:"id"`
			PasswordCiphertext string `db:"password_ciphertext"`
		}
		if qErr := sqlx.SelectInto(ctx, store, &articleRows,
			`SELECT id, password_ciphertext FROM articles WHERE password_ciphertext <> ''`); qErr == nil {
			for _, r := range articleRows {
				if _, oErr := cfg.KeyRing.OpenStringFallback(r.PasswordCiphertext, config.ArticlePasswordAAD(r.ID), "articles:password:"+r.ID); oErr != nil {
					return CheckResult{
						Name:    "secret_key",
						Status:  StatusWarn,
						Code:    "secret_mismatch_warn",
						Params:  map[string]string{"domain": "articles." + r.ID, "store": "articles", "article_id": r.ID},
						Summary: fmt.Sprintf("articles.%s password_ciphertext cannot be decrypted with current VOLO_SECRET_KEY", r.ID),
					}
				}
			}
		}

		// 7/8: comments.notify_email_ciphertext
		var commentRows []struct {
			ID                    string `db:"id"`
			NotifyEmailCiphertext string `db:"notify_email_ciphertext"`
		}
		if qErr := sqlx.SelectInto(ctx, store, &commentRows,
			`SELECT id, notify_email_ciphertext FROM comments WHERE notify_email_ciphertext <> ''`); qErr == nil {
			for _, r := range commentRows {
				if _, oErr := cfg.KeyRing.OpenStringFallback(
					r.NotifyEmailCiphertext,
					config.CommentNotifyEmailAAD(r.ID),
					"comments:notify_email:"+r.ID,
					"comments:email:"+r.ID,
					"comment:email",
				); oErr != nil {
					return CheckResult{
						Name:    "secret_key",
						Status:  StatusWarn,
						Code:    "secret_mismatch_warn",
						Params:  map[string]string{"domain": "comments." + r.ID, "store": "comments", "comment_id": r.ID},
						Summary: fmt.Sprintf("comments.%s notify_email_ciphertext cannot be decrypted with current VOLO_SECRET_KEY", r.ID),
					}
				}
			}
		}

		// 8/8: settings(appearance).value_json -> theme_configs[themeID][key] ("enc:v1:...")
		var appJSON string
		if qErr := store.QueryRowContext(ctx, store.Dialect().Rebind(`SELECT value_json FROM settings WHERE domain = ?`), "appearance").Scan(&appJSON); qErr == nil && appJSON != "" {
			if badTheme, badKey, tErr := verifyAppearanceThemeConfigSecrets(cfg.KeyRing, appJSON); tErr != nil {
				return CheckResult{
					Name:    "secret_key",
					Status:  StatusWarn,
					Code:    "secret_mismatch_warn",
					Params:  map[string]string{"domain": "appearance.theme_configs." + badTheme + "." + badKey, "store": "appearance.theme_configs", "theme": badTheme, "key": badKey},
					Summary: fmt.Sprintf("appearance.theme_configs.%s.%s secret cannot be decrypted with current VOLO_SECRET_KEY", badTheme, badKey),
				}
			}
		}
	}

	cleanSrc := cleanSecretSource(cfg.SecretKeySource)
	return CheckResult{
		Name:    "secret_key",
		Status:  StatusOK,
		Code:    "secret_ok",
		Params:  map[string]string{"source": cleanSrc},
		Summary: fmt.Sprintf("AES-256-GCM & HKDF subkeys verified (source=%s)", cleanSrc),
	}
}

func checkDatabase(ctx context.Context, cfg *config.Config, store *sqlx.Store) CheckResult {
	if store == nil {
		if cfg != nil && cfg.DBDriver == "sqlite" && cfg.DBDSN != "" && cfg.DBDSN != ":memory:" {
			if _, statErr := os.Stat(cfg.DBDSN); os.IsNotExist(statErr) {
				dispDSN := cleanDisplayPath(cfg.DBDSN)
				return CheckResult{
					Name:    "database",
					Status:  StatusWarn,
					Code:    "db_uninitialized",
					Params:  map[string]string{"dialect": "sqlite", "dsn": dispDSN},
					Summary: fmt.Sprintf("sqlite database file %s does not exist yet (will be created on first volo serve)", dispDSN),
				}
			}
		}
		return CheckResult{
			Name:    "database",
			Status:  StatusFail,
			Code:    "db_nil",
			Summary: "database store is nil",
		}
	}
	start := time.Now()
	if err := store.Ping(ctx); err != nil {
		return CheckResult{
			Name:    "database",
			Status:  StatusFail,
			Code:    "db_ping_err",
			Params:  map[string]string{"err": err.Error()},
			Summary: fmt.Sprintf("ping failed: %v", err),
		}
	}
	latency := time.Since(start)
	dName := store.Dialect().Name()

	var verRows []struct {
		Version int `db:"version"`
	}
	err := sqlx.SelectInto(ctx, store, &verRows, `SELECT version FROM schema_migrations ORDER BY version DESC`)
	if err != nil || len(verRows) == 0 {
		latStr := latency.Round(100 * time.Microsecond).String()
		return CheckResult{
			Name:    "database",
			Status:  StatusFail,
			Code:    "db_migrations_missing",
			Params:  map[string]string{"dialect": dName, "latency": latStr},
			Summary: fmt.Sprintf("dialect=%s reachable (%s), but schema_migrations missing or empty", dName, latStr),
		}
	}
	latestVer := verRows[0].Version
	if latestVer < migrate.CurrentSchemaVersion {
		return CheckResult{
			Name:   "database",
			Status: StatusWarn,
			Code:   "db_schema_behind",
			Params: map[string]string{
				"dialect": dName,
				"current": strconv.Itoa(latestVer),
				"target":  strconv.Itoa(migrate.CurrentSchemaVersion),
			},
			Summary: fmt.Sprintf("dialect=%s schema v%d behind binary v%d", dName, latestVer, migrate.CurrentSchemaVersion),
		}
	}

	pingStr := latency.Round(10 * time.Microsecond).String()
	extra := ""
	if dName == "sqlite" {
		var jm string
		_ = store.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&jm)
		var fk int
		_ = store.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk)
		jmLower := strings.ToLower(strings.TrimSpace(jm))
		extra = fmt.Sprintf(", journal_mode=%s, foreign_keys=%d", jmLower, fk)
		if jmLower != "wal" {
			return CheckResult{
				Name:   "database",
				Status: StatusWarn,
				Code:   "db_sqlite_not_wal",
				Params: map[string]string{
					"dialect":      dName,
					"version":      strconv.Itoa(latestVer),
					"journal_mode": jmLower,
					"ping":         pingStr,
				},
				Summary: fmt.Sprintf("dialect=%s, schema=v%d, ping=%s%s (expected journal_mode=wal)", dName, latestVer, pingStr, extra),
			}
		}
	}

	return CheckResult{
		Name:   "database",
		Status: StatusOK,
		Code:   "db_ok",
		Params: map[string]string{
			"dialect": dName,
			"version": strconv.Itoa(latestVer),
			"ping":    pingStr,
			"extra":   extra,
		},
		Summary: fmt.Sprintf("dialect=%s, schema=v%d, ping=%s%s", dName, latestVer, pingStr, extra),
	}
}

func probeDirWritableReadOnly(rawDir string) error {
	trimmed := strings.TrimSpace(rawDir)
	if trimmed == "" {
		return fmt.Errorf("directory path is empty")
	}
	absDir, err := filepath.Abs(trimmed)
	if err != nil {
		return err
	}
	st, statErr := os.Stat(absDir)
	if statErr == nil {
		if !st.IsDir() {
			return fmt.Errorf("path exists and is not a directory")
		}
		f, wErr := os.CreateTemp(absDir, ".volo-doctor-*")
		if wErr != nil {
			return wErr
		}
		name := f.Name()
		_ = f.Close()
		_ = os.Remove(name)
		return nil
	}
	if !os.IsNotExist(statErr) {
		return statErr
	}
	cur := filepath.Dir(absDir)
	for {
		pst, pErr := os.Stat(cur)
		if pErr == nil {
			if !pst.IsDir() {
				return fmt.Errorf("ancestor %s is not a directory", cur)
			}
			f, wErr := os.CreateTemp(cur, ".volo-doctor-*")
			if wErr != nil {
				return wErr
			}
			name := f.Name()
			_ = f.Close()
			_ = os.Remove(name)
			return nil
		}
		if !os.IsNotExist(pErr) {
			return pErr
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return pErr
		}
		cur = parent
	}
}

func checkWritableDirs(cfg *config.Config) CheckResult {
	if cfg == nil {
		return CheckResult{
			Name:    "directories",
			Status:  StatusFail,
			Code:    "dirs_nil",
			Summary: "nil config",
		}
	}
	uploadDir := cfg.GetUploadDir()
	targets := []struct {
		label string
		dir   string
	}{
		{"data", cfg.DataDir},
		{"uploads", uploadDir},
		{"backups", cfg.BackupDir},
	}
	for _, t := range targets {
		dispDir := cleanDisplayPath(t.dir)
		if err := probeDirWritableReadOnly(t.dir); err != nil {
			return CheckResult{
				Name:    "directories",
				Status:  StatusFail,
				Code:    "dirs_write_err",
				Params:  map[string]string{"label": t.label, "dir": dispDir, "err": err.Error()},
				Summary: fmt.Sprintf("%s dir %s is not writable: %v", t.label, dispDir, err),
			}
		}
	}
	dData := cleanDisplayPath(cfg.DataDir)
	dUploads := cleanDisplayPath(uploadDir)
	dBackups := cleanDisplayPath(cfg.BackupDir)
	return CheckResult{
		Name:   "directories",
		Status: StatusOK,
		Code:   "dirs_ok",
		Params: map[string]string{
			"data":    dData,
			"uploads": dUploads,
			"backups": dBackups,
		},
		Summary: fmt.Sprintf("data (%s), uploads (%s), and backups (%s) writable", dData, dUploads, dBackups),
	}
}

// tlsExpiryWarnWindow is the remaining-validity threshold under which the TLS certificate check warns.
const tlsExpiryWarnWindow = 14 * 24 * time.Hour

// checkTLSCertificate verifies the native HTTPS key pair is readable and matching, and grades the
// leaf certificate's remaining validity: expired -> fail, < 14 days -> warn, otherwise ok.
func checkTLSCertificate(cfg *config.Config, now time.Time) CheckResult {
	certPath := cleanDisplayPath(cfg.TLSCertFile)
	keyPath := cleanDisplayPath(cfg.TLSKeyFile)
	for _, f := range []struct{ label, path string }{{"cert", cfg.TLSCertFile}, {"key", cfg.TLSKeyFile}} {
		fh, err := os.Open(f.path)
		if err != nil {
			return CheckResult{
				Name:    "tls_certificate",
				Status:  StatusFail,
				Code:    "tls_unreadable",
				Params:  map[string]string{"label": f.label, "path": cleanDisplayPath(f.path), "err": err.Error()},
				Summary: fmt.Sprintf("tls %s file %s is not readable: %v", f.label, cleanDisplayPath(f.path), err),
			}
		}
		_ = fh.Close()
	}
	pair, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return CheckResult{
			Name:    "tls_certificate",
			Status:  StatusFail,
			Code:    "tls_keypair_invalid",
			Params:  map[string]string{"cert": certPath, "key": keyPath, "err": err.Error()},
			Summary: fmt.Sprintf("tls key pair invalid or mismatched (cert=%s key=%s): %v", certPath, keyPath, err),
		}
	}
	leaf := pair.Leaf
	if leaf == nil && len(pair.Certificate) > 0 {
		leaf, err = x509.ParseCertificate(pair.Certificate[0])
	}
	if leaf == nil || err != nil {
		return CheckResult{
			Name:    "tls_certificate",
			Status:  StatusFail,
			Code:    "tls_keypair_invalid",
			Params:  map[string]string{"cert": certPath, "key": keyPath, "err": fmt.Sprint(err)},
			Summary: fmt.Sprintf("tls leaf certificate unparsable (cert=%s): %v", certPath, err),
		}
	}
	expires := leaf.NotAfter.UTC().Format("2006-01-02")
	daysLeft := int(leaf.NotAfter.Sub(now).Hours() / 24)
	subject := leaf.Subject.CommonName
	if len(leaf.DNSNames) > 0 {
		subject = strings.Join(leaf.DNSNames, ",")
	}
	params := map[string]string{"cert": certPath, "subject": subject, "expires": expires, "days": strconv.Itoa(daysLeft)}
	switch {
	case !now.Before(leaf.NotAfter):
		return CheckResult{
			Name:    "tls_certificate",
			Status:  StatusFail,
			Code:    "tls_cert_expired",
			Params:  params,
			Summary: fmt.Sprintf("tls certificate %s (%s) expired on %s", certPath, subject, expires),
		}
	case leaf.NotAfter.Sub(now) < tlsExpiryWarnWindow:
		return CheckResult{
			Name:    "tls_certificate",
			Status:  StatusWarn,
			Code:    "tls_cert_expiring",
			Params:  params,
			Summary: fmt.Sprintf("tls certificate %s (%s) expires in %d days on %s; renew soon (hot-reloaded on file change)", certPath, subject, daysLeft, expires),
		}
	}
	return CheckResult{
		Name:    "tls_certificate",
		Status:  StatusOK,
		Code:    "tls_cert_ok",
		Params:  params,
		Summary: fmt.Sprintf("tls certificate %s (%s) valid until %s (%d days left)", certPath, subject, expires, daysLeft),
	}
}

func checkGeoIPAssets(ctx context.Context, cfg *config.Config, store *sqlx.Store) CheckResult {
	if cfg == nil || !cfg.GeoAccessEnabled {
		return CheckResult{
			Name:    "geoip",
			Status:  StatusOK,
			Code:    "geoip_disabled_ok",
			Summary: "GeoIP regional access control disabled (no offline database required)",
		}
	}

	activeGeoRules := 0
	effectivePath := cfg.GetGeoIPDir()
	if store != nil {
		_ = store.QueryRowContext(ctx,
			store.Dialect().Rebind(`SELECT COUNT(*) FROM security_geo_rules WHERE is_enabled = ?`), true,
		).Scan(&activeGeoRules)

		if effectivePath == cfg.GetDefaultGeoIPDir() {
			var secJSON string
			if err := store.QueryRowContext(ctx, store.Dialect().Rebind(`SELECT value_json FROM settings WHERE domain = ?`), "security").Scan(&secJSON); err == nil && secJSON != "" {
				var sec struct {
					GeoIPDir string `json:"geoip_dir"`
				}
				if json.Unmarshal([]byte(secJSON), &sec) == nil && strings.TrimSpace(sec.GeoIPDir) != "" {
					if validPath, vErr := cfg.ValidateCustomGeoIPPath(sec.GeoIPDir); vErr == nil && validPath != "" {
						effectivePath = validPath
					}
				}
			}
		}
	}

	dispDir := cleanDisplayPath(effectivePath)
	if dispDir == "" {
		if activeGeoRules > 0 {
			return CheckResult{
				Name:    "geoip",
				Status:  StatusWarn,
				Code:    "geoip_missing_warn",
				Params:  map[string]string{"dir": "(unset)", "rules": strconv.Itoa(activeGeoRules)},
				Summary: fmt.Sprintf("%d active GeoIP rule(s), but VOLO_GEOIP_DIR is not configured", activeGeoRules),
			}
		}
		return CheckResult{
			Name:    "geoip",
			Status:  StatusOK,
			Code:    "geoip_disabled_ok",
			Summary: "GeoIP regional rules not configured (offline database optional)",
		}
	}

	st, _ := security.InspectGeoIPPath(effectivePath)
	if len(st.ActiveFiles) > 0 {
		filesStr := strings.Join(st.ActiveFiles, ", ")
		if len(st.SkippedFiles) > 0 {
			filesStr = fmt.Sprintf("%s (ignored: %s)", filesStr, strings.Join(st.SkippedFiles, ", "))
		}
		return CheckResult{
			Name:    "geoip",
			Status:  StatusOK,
			Code:    "geoip_ready_ok",
			Params:  map[string]string{"files": filesStr},
			Summary: fmt.Sprintf("offline GeoIP databases ready: %s", filesStr),
		}
	}

	if len(st.SkippedFiles) > 0 {
		skippedStr := strings.Join(st.SkippedFiles, ", ")
		return CheckResult{
			Name:    "geoip",
			Status:  StatusWarn,
			Code:    "geoip_unusable_warn",
			Params:  map[string]string{"dir": dispDir, "skipped": skippedStr},
			Summary: fmt.Sprintf("no usable regional GeoIP database in %s (skipped: %s)", dispDir, skippedStr),
		}
	}

	if activeGeoRules > 0 {
		return CheckResult{
			Name:    "geoip",
			Status:  StatusWarn,
			Code:    "geoip_missing_warn",
			Params:  map[string]string{"dir": dispDir, "rules": strconv.Itoa(activeGeoRules)},
			Summary: fmt.Sprintf("%d active GeoIP rule(s), but no .mmdb or .xdb files in %s (using fallback policy)", activeGeoRules, dispDir),
		}
	}
	return CheckResult{
		Name:    "geoip",
		Status:  StatusOK,
		Code:    "geoip_optional_ok",
		Params:  map[string]string{"dir": dispDir},
		Summary: fmt.Sprintf("optional offline GeoIP dir %s not populated (not needed until regional rules are added)", dispDir),
	}
}

// FormatHuman writes a clean, aligned diagnostic summary table to w.
func (r *Report) FormatHuman(w io.Writer) {
	fmt.Fprintln(w, "Volo-go System Diagnostics (volo doctor)")
	fmt.Fprintln(w, strings.Repeat("-", 72))
	for _, c := range r.Checks {
		badge := "[ OK ]"
		switch c.Status {
		case StatusWarn:
			badge = "[WARN]"
		case StatusFail:
			badge = "[FAIL]"
		}
		fmt.Fprintf(w, "%s %-20s %s\n", badge, c.Name, c.Summary)
		if c.Detail != "" {
			fmt.Fprintf(w, "       %s\n", c.Detail)
		}
	}
	fmt.Fprintln(w, strings.Repeat("-", 72))
	if r.Healthy {
		fmt.Fprintln(w, "Overall Status: HEALTHY")
	} else {
		fmt.Fprintln(w, "Overall Status: UNHEALTHY (see [FAIL] items above)")
	}
}

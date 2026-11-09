package doctor

import (
	"context"
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
	add(checkDatabase(ctx, store))

	// 4. Writable Directories Probe (DataDir, UploadDir, BackupDir)
	add(checkWritableDirs(cfg))

	// 5. Optional Offline GeoIP Databases
	add(checkGeoIPAssets(ctx, cfg, store))

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

func checkCryptoAndSecrets(ctx context.Context, cfg *config.Config, store *sqlx.Store) CheckResult {
	if cfg == nil || cfg.KeyRing == nil {
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

	// Verify existing encrypted secrets in settings table can be opened with active KeyRing
	if store != nil {
		var rows []domain.SettingRecord
		qErr := sqlx.SelectInto(ctx, store, &rows,
			`SELECT domain, secret_ciphertext FROM settings WHERE secret_ciphertext <> ''`)
		if qErr == nil {
			for _, r := range rows {
				if _, oErr := cfg.KeyRing.OpenString(r.SecretCiphertext, "settings:"+r.Domain); oErr != nil {
					return CheckResult{
						Name:    "secret_key",
						Status:  StatusWarn,
						Code:    "secret_mismatch_warn",
						Params:  map[string]string{"domain": r.Domain},
						Summary: fmt.Sprintf("settings.%s secret_ciphertext cannot be decrypted with current VOLO_SECRET_KEY (re-enter secret after cross-machine restore)", r.Domain),
					}
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

func checkDatabase(ctx context.Context, store *sqlx.Store) CheckResult {
	if store == nil {
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

	extra := ""
	if dName == "sqlite" {
		var jm string
		_ = store.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&jm)
		var fk int
		_ = store.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk)
		extra = fmt.Sprintf(", journal_mode=%s, foreign_keys=%d", strings.ToLower(jm), fk)
	}
	pingStr := latency.Round(10 * time.Microsecond).String()

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
		if err := os.MkdirAll(t.dir, 0o750); err != nil {
			return CheckResult{
				Name:    "directories",
				Status:  StatusFail,
				Code:    "dirs_mkdir_err",
				Params:  map[string]string{"label": t.label, "dir": dispDir, "err": err.Error()},
				Summary: fmt.Sprintf("cannot create %s dir %s: %v", t.label, dispDir, err),
			}
		}
		probePath := filepath.Join(t.dir, fmt.Sprintf(".volo-doctor-%d.tmp", os.Getpid()))
		if err := os.WriteFile(probePath, []byte("ok"), 0o600); err != nil {
			return CheckResult{
				Name:    "directories",
				Status:  StatusFail,
				Code:    "dirs_write_err",
				Params:  map[string]string{"label": t.label, "dir": dispDir, "err": err.Error()},
				Summary: fmt.Sprintf("%s dir %s is not writable: %v", t.label, dispDir, err),
			}
		}
		_ = os.Remove(probePath)
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

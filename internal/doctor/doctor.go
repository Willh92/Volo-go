package doctor

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
)

const (
	StatusOK   = "ok"
	StatusWarn = "warn"
	StatusFail = "fail"
)

// CheckResult describes a single diagnostic probe executed by volo doctor.
type CheckResult struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // ok | warn | fail
	Summary string `json:"summary"`
	Detail  string `json:"detail,omitempty"`
}

// Report aggregates all diagnostic checks and overall health status.
type Report struct {
	Healthy   bool          `json:"healthy"`
	CheckedAt int64         `json:"checked_at"`
	Checks    []CheckResult `json:"checks"`
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
	add(checkGeoIPAssets(cfg))

	// 6. Admin Console Path Cloaking Posture
	if cfg != nil && cfg.IsCustomAdminPath {
		add(CheckResult{
			Name:    "admin_stealth_path",
			Status:  StatusOK,
			Summary: fmt.Sprintf("custom admin path active (%s); default /console cloaked with 404", cfg.AdminPath),
		})
	} else {
		add(CheckResult{
			Name:    "admin_stealth_path",
			Status:  StatusWarn,
			Summary: "using default VOLO_ADMIN_PATH=/console (set custom path in .env for stealth 404 cloaking)",
		})
	}

	return rep
}

func checkRuntime() CheckResult {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	heapMiB := float64(ms.HeapAlloc) / (1024 * 1024)
	sysMiB := float64(ms.Sys) / (1024 * 1024)

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
		Summary: fmt.Sprintf("%s %s/%s (CGO_ENABLED=%s, heap=%.2f MiB, sys=%.2f MiB)",
			runtime.Version(), runtime.GOOS, runtime.GOARCH, cgoSetting, heapMiB, sysMiB),
	}
}

func checkCryptoAndSecrets(ctx context.Context, cfg *config.Config, store *sqlx.Store) CheckResult {
	if cfg == nil || cfg.KeyRing == nil {
		return CheckResult{
			Name:    "secret_key",
			Status:  StatusFail,
			Summary: "KeyRing is uninitialized",
		}
	}

	// Verify file permissions if file-backed
	for _, pfx := range []string{"file:", "generated:"} {
		if strings.HasPrefix(cfg.SecretKeySource, pfx) {
			keyPath := strings.TrimPrefix(cfg.SecretKeySource, pfx)
			info, err := os.Lstat(keyPath)
			if err != nil {
				return CheckResult{
					Name:    "secret_key",
					Status:  StatusFail,
					Summary: fmt.Sprintf("cannot stat secret key file %q: %v", keyPath, err),
				}
			}
			if info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
				return CheckResult{
					Name:    "secret_key",
					Status:  StatusFail,
					Summary: fmt.Sprintf("secret key file %q must be regular mode 0600 (got %04o)", keyPath, info.Mode().Perm()),
				}
			}
		}
	}

	sealed, err := cfg.KeyRing.SealString("volo-doctor-probe", "doctor:probe")
	if err != nil {
		return CheckResult{Name: "secret_key", Status: StatusFail, Summary: fmt.Sprintf("AES-GCM seal failed: %v", err)}
	}
	opened, err := cfg.KeyRing.OpenString(sealed, "doctor:probe")
	if err != nil || opened != "volo-doctor-probe" {
		return CheckResult{Name: "secret_key", Status: StatusFail, Summary: "AES-GCM round-trip verification failed"}
	}

	// Verify existing encrypted secrets in settings table can be opened with active KeyRing
	if store != nil {
		rows, qErr := sqlx.Select[domain.SettingRecord](ctx, store,
			`SELECT domain, secret_ciphertext FROM settings WHERE secret_ciphertext <> ''`)
		if qErr == nil {
			for _, r := range rows {
				if _, oErr := cfg.KeyRing.OpenString(r.SecretCiphertext, "settings:"+r.Domain); oErr != nil {
					return CheckResult{
						Name:    "secret_key",
						Status:  StatusWarn,
						Summary: fmt.Sprintf("settings.%s secret_ciphertext cannot be decrypted with current VOLO_SECRET_KEY (re-enter secret after cross-machine restore)", r.Domain),
					}
				}
			}
		}
	}

	return CheckResult{
		Name:    "secret_key",
		Status:  StatusOK,
		Summary: fmt.Sprintf("AES-256-GCM & HKDF subkeys verified (source=%s)", cfg.SecretKeySource),
	}
}

func checkDatabase(ctx context.Context, store *sqlx.Store) CheckResult {
	if store == nil {
		return CheckResult{Name: "database", Status: StatusFail, Summary: "database store is nil"}
	}
	start := time.Now()
	if err := store.Ping(ctx); err != nil {
		return CheckResult{Name: "database", Status: StatusFail, Summary: fmt.Sprintf("ping failed: %v", err)}
	}
	latency := time.Since(start)
	dName := store.Dialect().Name()

	verRows, err := sqlx.Select[struct {
		Version int `db:"version"`
	}](ctx, store, `SELECT version FROM schema_migrations ORDER BY version DESC`)
	if err != nil || len(verRows) == 0 {
		return CheckResult{
			Name:    "database",
			Status:  StatusFail,
			Summary: fmt.Sprintf("dialect=%s reachable (%s), but schema_migrations missing or empty", dName, latency.Round(100*time.Microsecond)),
		}
	}
	latestVer := verRows[0].Version
	if latestVer < migrate.CurrentSchemaVersion {
		return CheckResult{
			Name:    "database",
			Status:  StatusWarn,
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

	return CheckResult{
		Name:    "database",
		Status:  StatusOK,
		Summary: fmt.Sprintf("dialect=%s, schema=v%d, ping=%s%s", dName, latestVer, latency.Round(10*time.Microsecond), extra),
	}
}

func checkWritableDirs(cfg *config.Config) CheckResult {
	if cfg == nil {
		return CheckResult{Name: "directories", Status: StatusFail, Summary: "nil config"}
	}
	targets := []struct {
		label string
		dir   string
	}{
		{"data", cfg.DataDir},
		{"uploads", cfg.UploadDir},
		{"backups", cfg.BackupDir},
	}
	for _, t := range targets {
		if err := os.MkdirAll(t.dir, 0o750); err != nil {
			return CheckResult{
				Name:    "directories",
				Status:  StatusFail,
				Summary: fmt.Sprintf("cannot create %s dir %q: %v", t.label, t.dir, err),
			}
		}
		probePath := filepath.Join(t.dir, fmt.Sprintf(".volo-doctor-%d.tmp", os.Getpid()))
		if err := os.WriteFile(probePath, []byte("ok"), 0o600); err != nil {
			return CheckResult{
				Name:    "directories",
				Status:  StatusFail,
				Summary: fmt.Sprintf("%s dir %q is not writable: %v", t.label, t.dir, err),
			}
		}
		_ = os.Remove(probePath)
	}
	return CheckResult{
		Name:    "directories",
		Status:  StatusOK,
		Summary: fmt.Sprintf("data (%s), uploads (%s), and backups (%s) writable", cfg.DataDir, cfg.UploadDir, cfg.BackupDir),
	}
}

func checkGeoIPAssets(cfg *config.Config) CheckResult {
	if cfg == nil || strings.TrimSpace(cfg.GeoIPDir) == "" {
		return CheckResult{Name: "geoip", Status: StatusWarn, Summary: "VOLO_GEOIP_DIR not configured"}
	}
	entries, err := os.ReadDir(cfg.GeoIPDir)
	if err != nil {
		return CheckResult{
			Name:    "geoip",
			Status:  StatusWarn,
			Summary: fmt.Sprintf("optional GeoIP dir %q not populated yet (.mmdb / .xdb optional)", cfg.GeoIPDir),
		}
	}
	var found []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		low := strings.ToLower(e.Name())
		if strings.HasSuffix(low, ".mmdb") || strings.HasSuffix(low, ".xdb") {
			found = append(found, e.Name())
		}
	}
	if len(found) == 0 {
		return CheckResult{
			Name:    "geoip",
			Status:  StatusWarn,
			Summary: fmt.Sprintf("no .mmdb or .xdb files in %q (GeoIP rules will use fallback policy)", cfg.GeoIPDir),
		}
	}
	return CheckResult{
		Name:    "geoip",
		Status:  StatusOK,
		Summary: fmt.Sprintf("offline GeoIP databases ready: %s", strings.Join(found, ", ")),
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

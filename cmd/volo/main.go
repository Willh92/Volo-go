package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/Willh92/Volo-go/internal/auth"
	"github.com/Willh92/Volo-go/internal/backup"
	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/doctor"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/etl"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/internal/server"
	"github.com/Willh92/Volo-go/pkg/id"
	"github.com/Willh92/Volo-go/pkg/plugin"
)

// Version can be overridden at link time via -ldflags="-X main.Version=v1.0.0".
var Version = "v0.1.0-dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(runCLI(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func runCLI(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
			printRootUsage(stdout)
			return 0
		}
		return cmdServe(ctx, args, stdout, stderr)
	}

	switch args[0] {
	case "serve":
		return cmdServe(ctx, args[1:], stdout, stderr)
	case "admin":
		return cmdAdmin(ctx, args[1:], stdout, stderr)
	case "backup":
		return cmdBackup(ctx, args[1:], stdout, stderr)
	case "import-legacy":
		return cmdImportLegacy(ctx, args[1:], stdout, stderr)
	case "doctor":
		return cmdDoctor(ctx, args[1:], stdout, stderr)
	case "version":
		fmt.Fprintf(stdout, "volo %s (%s %s/%s, pure-go static)\n",
			Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return 0
	case "help", "-h", "--help":
		printRootUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "volo: unknown subcommand %q\n\n", args[0])
		printRootUsage(stderr)
		return 2
	}
}

func printRootUsage(w io.Writer) {
	fmt.Fprint(w, `Volo-go — Pure-Go Sovereign Blog Engine

Usage:
  volo [serve] [flags]                          Start HTTP / Unix Socket server
  volo admin reset-password --user <u|email>    Reset user password via Argon2id & revoke sessions
  volo admin disable-2fa --user <u|email>       Emergency disable TOTP & recovery codes for user
  volo backup create [--output file]            Create streaming database (+ optional media) backup
  volo backup restore <archive.zip> [--dry-run] Verify SHA-256 & restore cross-DB native backup
  volo import-legacy <bolo.zip|json> [--dry-run] Stream & normalize legacy bolo-solo 15-table backup
  volo doctor [--json]                          Run environment, crypto, DB WAL & FS diagnostics
  volo version                                  Print binary version & runtime info
`)
}

type boundCommonFlags struct {
	envFile          string
	listenAddr       string
	listenSocket     string
	publicURL        string
	trustedProxies   string
	adminPath        string
	secretKey        string
	geoAccessEnabled bool
	defaultLocale    string
	dataDir          string
	uploadDir        string
	backupDir        string
	geoIPDir         string
	themesDir        string
	pluginsDir       string
	localesDir       string
	dbDriver         string
	dbDSN            string
	dbSimpleProtocol bool
	shutdownSec      int
}

func registerCommonFlags(fs *flag.FlagSet, b *boundCommonFlags) {
	fs.StringVar(&b.envFile, "env-file", "", "Path to .env file (default .env)")
	fs.StringVar(&b.listenAddr, "listen-addr", "", "TCP listen address (e.g. 0.0.0.0:8080)")
	fs.StringVar(&b.listenSocket, "listen-socket", "", "Unix domain socket path[,octal_mode]")
	fs.StringVar(&b.publicURL, "public-url", "", "Canonical public URL")
	fs.StringVar(&b.trustedProxies, "trusted-proxies", "", "Comma-separated trusted proxy CIDRs or 'unix'")
	fs.StringVar(&b.adminPath, "admin-path", "", "Custom stealth admin path prefix (e.g. /secret-panel)")
	fs.StringVar(&b.secretKey, "secret-key", "", "Master secret key (>=16 chars)")
	fs.BoolVar(&b.geoAccessEnabled, "geo-access-enabled", true, "Enable or emergency-bypass GeoIP access rules")
	fs.StringVar(&b.defaultLocale, "default-locale", "", "Default locale (zh-CN or en-US)")
	fs.StringVar(&b.dataDir, "data-dir", "", "Data directory")
	fs.StringVar(&b.uploadDir, "upload-dir", "", "Uploads directory")
	fs.StringVar(&b.backupDir, "backup-dir", "", "Backups directory")
	fs.StringVar(&b.geoIPDir, "geoip-dir", "", "GeoIP database directory")
	fs.StringVar(&b.themesDir, "themes-dir", "", "External themes directory")
	fs.StringVar(&b.pluginsDir, "plugins-dir", "", "External plugins directory")
	fs.StringVar(&b.localesDir, "locales-dir", "", "External locale overrides directory")
	fs.StringVar(&b.dbDriver, "db-driver", "", "Database driver (sqlite | postgres | mysql)")
	fs.StringVar(&b.dbDSN, "db-dsn", "", "Database DSN")
	fs.BoolVar(&b.dbSimpleProtocol, "db-simple-protocol", false, "Enable pgx simple protocol for transaction poolers")
	fs.IntVar(&b.shutdownSec, "shutdown-timeout", 15, "Graceful shutdown timeout in seconds")
}

func buildCLIOverrides(fs *flag.FlagSet, b *boundCommonFlags) config.CLIOverrides {
	setFlags := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		setFlags[f.Name] = true
	})
	var o config.CLIOverrides
	o.EnvFile = b.envFile
	if setFlags["listen-addr"] {
		v := b.listenAddr
		o.ListenAddr = &v
	}
	if setFlags["listen-socket"] {
		v := b.listenSocket
		o.ListenSocket = &v
	}
	if setFlags["public-url"] {
		v := b.publicURL
		o.PublicURL = &v
	}
	if setFlags["trusted-proxies"] {
		v := b.trustedProxies
		o.TrustedProxies = &v
	}
	if setFlags["admin-path"] {
		v := b.adminPath
		o.AdminPath = &v
	}
	if setFlags["secret-key"] {
		v := b.secretKey
		o.SecretKey = &v
	}
	if setFlags["geo-access-enabled"] {
		v := b.geoAccessEnabled
		o.GeoAccessEnabled = &v
	}
	if setFlags["default-locale"] {
		v := b.defaultLocale
		o.DefaultLocale = &v
	}
	if setFlags["data-dir"] {
		v := b.dataDir
		o.DataDir = &v
	}
	if setFlags["upload-dir"] {
		v := b.uploadDir
		o.UploadDir = &v
	}
	if setFlags["backup-dir"] {
		v := b.backupDir
		o.BackupDir = &v
	}
	if setFlags["geoip-dir"] {
		v := b.geoIPDir
		o.GeoIPDir = &v
	}
	if setFlags["themes-dir"] {
		v := b.themesDir
		o.ThemesDir = &v
	}
	if setFlags["plugins-dir"] {
		v := b.pluginsDir
		o.PluginsDir = &v
	}
	if setFlags["locales-dir"] {
		v := b.localesDir
		o.LocalesDir = &v
	}
	if setFlags["db-driver"] {
		v := b.dbDriver
		o.DBDriver = &v
	}
	if setFlags["db-dsn"] {
		v := b.dbDSN
		o.DBDSN = &v
	}
	if setFlags["db-simple-protocol"] {
		v := b.dbSimpleProtocol
		o.DBSimpleProtocol = &v
	}
	if setFlags["shutdown-timeout"] {
		v := b.shutdownSec
		o.ShutdownTimeoutSec = &v
	}
	return o
}

func openMigratedStore(ctx context.Context, cfg *config.Config) (*sqlx.Store, error) {
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver:         cfg.DBDriver,
		DSN:            cfg.DBDSN,
		SimpleProtocol: cfg.DBSimpleProtocol,
		MaxOpenConns:   cfg.DBMaxOpenConns,
		MaxIdleConns:   cfg.DBMaxIdleConns,
		ConnLifetime:   cfg.DBConnLifetime,
	})
	if err != nil {
		return nil, err
	}
	if err := migrate.Up(ctx, store); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

func cmdServe(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	fs := flag.NewFlagSet("volo serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var common boundCommonFlags
	registerCommonFlags(fs, &common)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(buildCLIOverrides(fs, &common))
	if err != nil {
		fmt.Fprintf(stderr, "volo serve: config error: %v\n", err)
		return 1
	}

	for _, dir := range []string{cfg.DataDir, cfg.UploadDir, cfg.BackupDir} {
		if mkErr := os.MkdirAll(dir, 0o750); mkErr != nil {
			fmt.Fprintf(stderr, "volo serve: directory init failed (%s): %v\n", dir, mkErr)
			return 1
		}
	}

	store, err := openMigratedStore(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "volo serve: database init failed: %v\n", err)
		return 1
	}
	defer store.Close()

	catalog, err := i18n.NewCatalog(cfg.DefaultLocale, cfg.LocalesDir)
	if err != nil {
		fmt.Fprintf(stderr, "volo serve: i18n init failed: %v\n", err)
		return 1
	}

	settingsMgr := repo.NewSettingsManager(store, cfg.KeyRing, cfg.DefaultLocale)
	if _, err := settingsMgr.Reload(ctx); err != nil {
		fmt.Fprintf(stderr, "volo serve: settings load failed: %v\n", err)
		return 1
	}

	pluginMgr := plugin.NewManager(plugin.ManagerOptions{
		Store:      store,
		Keys:       cfg.KeyRing,
		PluginsDir: cfg.PluginsDir,
		DataDir:    cfg.DataDir,
		Catalog:    catalog,
	})
	_ = pluginMgr.DiscoverAndStart(ctx)
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = pluginMgr.Close(shutCtx)
	}()

	startedAt := time.Now()
	appSrv, err := server.NewAppServer(server.AppDeps{
		Config:    cfg,
		Store:     store,
		Settings:  settingsMgr,
		Catalog:   catalog,
		Plugins:   pluginMgr,
		StartedAt: startedAt,
	})
	if err != nil {
		fmt.Fprintf(stderr, "volo serve: server init failed: %v\n", err)
		return 1
	}

	// Background scheduled-article publisher (checks every 30s)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = appSrv.Deps().Content.PublishDueScheduled(ctx)
			}
		}
	}()

	handle, err := server.OpenListener(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "volo serve: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Volo-go %s listening on %s://%s (dialect=%s, admin=%s)\n",
		Version, handle.Network, handle.Address, store.Dialect().Name(), cfg.AdminPath)

	if err := server.ServeWithGracefulShutdown(ctx, handle, appSrv.Handler(), cfg.ShutdownTimeout); err != nil {
		fmt.Fprintf(stderr, "volo serve: shutdown error: %v\n", err)
		return 1
	}
	return 0
}

func cmdAdmin(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: volo admin <reset-password | disable-2fa> [flags]")
		return 2
	}
	switch args[0] {
	case "reset-password":
		fs := flag.NewFlagSet("volo admin reset-password", flag.ContinueOnError)
		fs.SetOutput(stderr)
		var common boundCommonFlags
		registerCommonFlags(fs, &common)
		userIdent := fs.String("user", "", "Target username or email (required)")
		newPass := fs.String("password", "", "New password (if omitted, generates a strong random password)")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		ident := strings.TrimSpace(*userIdent)
		if ident == "" {
			fmt.Fprintln(stderr, "volo admin reset-password: --user <username|email> is required")
			return 2
		}

		rawPass := *newPass
		generated := false
		if rawPass == "" {
			rnd, rErr := id.RandomHex(10)
			if rErr != nil {
				fmt.Fprintf(stderr, "volo admin reset-password: %v\n", rErr)
				return 1
			}
			rawPass = "Volo-" + rnd + "9a"
			generated = true
		}
		if err := auth.ValidatePasswordPolicy(rawPass); err != nil {
			fmt.Fprintf(stderr, "volo admin reset-password: %v\n", err)
			return 1
		}
		hashed, err := auth.HashPassword(rawPass)
		if err != nil {
			fmt.Fprintf(stderr, "volo admin reset-password: %v\n", err)
			return 1
		}

		cfg, err := config.Load(buildCLIOverrides(fs, &common))
		if err != nil {
			fmt.Fprintf(stderr, "volo admin reset-password: %v\n", err)
			return 1
		}
		store, err := openMigratedStore(ctx, cfg)
		if err != nil {
			fmt.Fprintf(stderr, "volo admin reset-password: %v\n", err)
			return 1
		}
		defer store.Close()

		var targetUser domain.User
		err = store.WithTx(ctx, func(tx *sqlx.Tx) error {
			u, qErr := sqlx.Get[domain.User](ctx, tx,
				`SELECT * FROM users WHERE LOWER(username) = LOWER(?) OR LOWER(email) = LOWER(?)`,
				ident, ident)
			if qErr != nil {
				if errors.Is(qErr, sql.ErrNoRows) {
					return fmt.Errorf("user %q not found", ident)
				}
				return qErr
			}
			targetUser = u
			now := time.Now().UnixMilli()
			if _, uErr := tx.ExecContext(ctx,
				`UPDATE users SET password_hash = ?, legacy_hash = '', updated_at = ? WHERE id = ?`,
				hashed, now, u.ID); uErr != nil {
				return uErr
			}
			if _, sErr := tx.ExecContext(ctx, `DELETE FROM user_sessions WHERE user_id = ?`, u.ID); sErr != nil {
				return sErr
			}
			_, aErr := tx.ExecContext(ctx,
				`INSERT INTO audit_logs (id, actor_id, actor_name, action, resource_type, resource_id, client_ip, detail_json, created_at)
				 VALUES (?, ?, 'cli', 'admin.reset_password', 'user', ?, 'local-cli', '{}', ?)`,
				id.Next(), u.ID, u.ID, now)
			return aErr
		})
		if err != nil {
			fmt.Fprintf(stderr, "volo admin reset-password: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "Password reset complete for user %q (id=%s); all active sessions revoked.\n", targetUser.Username, targetUser.ID)
		if generated {
			fmt.Fprintf(stdout, "Generated temporary password: %s\n", rawPass)
		}
		return 0

	case "disable-2fa":
		fs := flag.NewFlagSet("volo admin disable-2fa", flag.ContinueOnError)
		fs.SetOutput(stderr)
		var common boundCommonFlags
		registerCommonFlags(fs, &common)
		userIdent := fs.String("user", "", "Target username or email (required)")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		ident := strings.TrimSpace(*userIdent)
		if ident == "" {
			fmt.Fprintln(stderr, "volo admin disable-2fa: --user <username|email> is required")
			return 2
		}

		cfg, err := config.Load(buildCLIOverrides(fs, &common))
		if err != nil {
			fmt.Fprintf(stderr, "volo admin disable-2fa: %v\n", err)
			return 1
		}
		store, err := openMigratedStore(ctx, cfg)
		if err != nil {
			fmt.Fprintf(stderr, "volo admin disable-2fa: %v\n", err)
			return 1
		}
		defer store.Close()

		var removedCount int64
		var targetUser domain.User
		err = store.WithTx(ctx, func(tx *sqlx.Tx) error {
			u, qErr := sqlx.Get[domain.User](ctx, tx,
				`SELECT * FROM users WHERE LOWER(username) = LOWER(?) OR LOWER(email) = LOWER(?)`,
				ident, ident)
			if qErr != nil {
				if errors.Is(qErr, sql.ErrNoRows) {
					return fmt.Errorf("user %q not found", ident)
				}
				return qErr
			}
			targetUser = u
			res, dErr := tx.ExecContext(ctx,
				`DELETE FROM user_credentials WHERE user_id = ? AND kind IN ('totp', 'recovery_codes')`, u.ID)
			if dErr != nil {
				return dErr
			}
			removedCount, _ = res.RowsAffected()
			now := time.Now().UnixMilli()
			_, aErr := tx.ExecContext(ctx,
				`INSERT INTO audit_logs (id, actor_id, actor_name, action, resource_type, resource_id, client_ip, detail_json, created_at)
				 VALUES (?, ?, 'cli', 'admin.disable_2fa', 'user', ?, 'local-cli', '{}', ?)`,
				id.Next(), u.ID, u.ID, now)
			return aErr
		})
		if err != nil {
			fmt.Fprintf(stderr, "volo admin disable-2fa: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "2FA emergency reset complete for user %q (id=%s, removed_credentials=%d).\n",
			targetUser.Username, targetUser.ID, removedCount)
		return 0

	default:
		fmt.Fprintf(stderr, "volo admin: unknown command %q (expected reset-password or disable-2fa)\n", args[0])
		return 2
	}
}

func cmdBackup(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: volo backup <create | restore> [flags]")
		return 2
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("volo backup create", flag.ContinueOnError)
		fs.SetOutput(stderr)
		var common boundCommonFlags
		registerCommonFlags(fs, &common)
		outPath := fs.String("output", "", "Output backup .zip path")
		withMedia := fs.Bool("with-media", true, "Include uploads media directory in archive")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}

		cfg, err := config.Load(buildCLIOverrides(fs, &common))
		if err != nil {
			fmt.Fprintf(stderr, "volo backup create: %v\n", err)
			return 1
		}
		store, err := openMigratedStore(ctx, cfg)
		if err != nil {
			fmt.Fprintf(stderr, "volo backup create: %v\n", err)
			return 1
		}
		defer store.Close()

		dest := strings.TrimSpace(*outPath)
		if dest == "" {
			dest = filepath.Join(cfg.BackupDir, fmt.Sprintf("volo-backup-%s.zip", time.Now().UTC().Format("20060102-150405")))
		}
		manifest, err := backup.CreateToFile(ctx, store, dest, backup.CreateOptions{
			IncludeMedia: *withMedia,
			UploadDir:    cfg.UploadDir,
		})
		if err != nil {
			fmt.Fprintf(stderr, "volo backup create: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "Backup created: %s (dialect=%s, streams=%d, media=%v)\n",
			dest, manifest.SourceDialect, len(manifest.Files), manifest.IncludesMedia)
		return 0

	case "restore":
		fs := flag.NewFlagSet("volo backup restore", flag.ContinueOnError)
		fs.SetOutput(stderr)
		var common boundCommonFlags
		registerCommonFlags(fs, &common)
		dryRun := fs.Bool("dry-run", false, "Verify archive checksums and row counts without modifying DB")
		// Allow positional <archive.zip> either before or after flags
		posPath, flagArgs := extractFirstPositional(args[1:])
		if err := fs.Parse(flagArgs); err != nil {
			return 2
		}
		if posPath == "" && fs.NArg() > 0 {
			posPath = fs.Arg(0)
		}
		if strings.TrimSpace(posPath) == "" {
			fmt.Fprintln(stderr, "usage: volo backup restore <archive.zip> [--dry-run]")
			return 2
		}

		cfg, err := config.Load(buildCLIOverrides(fs, &common))
		if err != nil {
			fmt.Fprintf(stderr, "volo backup restore: %v\n", err)
			return 1
		}
		store, err := openMigratedStore(ctx, cfg)
		if err != nil {
			fmt.Fprintf(stderr, "volo backup restore: %v\n", err)
			return 1
		}
		defer store.Close()

		sum, err := backup.RestoreFromFile(ctx, store, posPath, backup.RestoreOptions{
			DryRun:    *dryRun,
			UploadDir: cfg.UploadDir,
		})
		if err != nil {
			fmt.Fprintf(stderr, "volo backup restore: %v\n", err)
			return 1
		}
		b, _ := json.MarshalIndent(sum, "", "  ")
		fmt.Fprintln(stdout, string(b))
		return 0

	default:
		fmt.Fprintf(stderr, "volo backup: unknown command %q (expected create or restore)\n", args[0])
		return 2
	}
}

func cmdImportLegacy(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	fs := flag.NewFlagSet("volo import-legacy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var common boundCommonFlags
	registerCommonFlags(fs, &common)
	dryRun := fs.Bool("dry-run", false, "Validate and summarize legacy bolo-solo archive without writing")
	posPath, flagArgs := extractFirstPositional(args)
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	if posPath == "" && fs.NArg() > 0 {
		posPath = fs.Arg(0)
	}
	if strings.TrimSpace(posPath) == "" {
		fmt.Fprintln(stderr, "usage: volo import-legacy <bolo-solo-backup.zip|data.json> [--dry-run]")
		return 2
	}

	cfg, err := config.Load(buildCLIOverrides(fs, &common))
	if err != nil {
		fmt.Fprintf(stderr, "volo import-legacy: %v\n", err)
		return 1
	}
	store, err := openMigratedStore(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "volo import-legacy: %v\n", err)
		return 1
	}
	defer store.Close()

	settingsMgr := repo.NewSettingsManager(store, cfg.KeyRing, cfg.DefaultLocale)
	_, _ = settingsMgr.Reload(ctx)

	rep, err := etl.ImportLegacyArchive(ctx, store, settingsMgr, cfg.KeyRing, posPath, etl.ImportOptions{
		DryRun:          *dryRun,
		TargetUploadDir: cfg.UploadDir,
	})
	if err != nil {
		fmt.Fprintf(stderr, "volo import-legacy: %v\n", err)
		return 1
	}
	b, _ := json.MarshalIndent(rep, "", "  ")
	fmt.Fprintln(stdout, string(b))
	return 0
}

func cmdDoctor(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	fs := flag.NewFlagSet("volo doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var common boundCommonFlags
	registerCommonFlags(fs, &common)
	asJSON := fs.Bool("json", false, "Emit machine-readable JSON diagnostics")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(buildCLIOverrides(fs, &common))
	if err != nil {
		fmt.Fprintf(stderr, "volo doctor: config load failed: %v\n", err)
		return 1
	}
	store, err := openMigratedStore(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "volo doctor: database open failed: %v\n", err)
		return 1
	}
	defer store.Close()

	rep := doctor.Run(ctx, cfg, store)
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
	} else {
		rep.FormatHuman(stdout)
	}
	if !rep.Healthy {
		return 1
	}
	return 0
}

// extractFirstPositional allows callers to write either `volo backup restore foo.zip --dry-run`
// or `volo backup restore --dry-run foo.zip` seamlessly with Go's standard flag package.
func extractFirstPositional(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

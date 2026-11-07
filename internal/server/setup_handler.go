package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Willh92/Volo-go/internal/adminui"
	"github.com/Willh92/Volo-go/internal/auth"
	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/etl"
	"github.com/Willh92/Volo-go/internal/security"
	"github.com/Willh92/Volo-go/pkg/id"
)

// isSameOriginRequest enforces browser Same-Origin verification on unauthenticated / public / setup
// state-changing endpoints while allowing non-browser CLI/API clients that omit Origin/Referer/Sec-Fetch-Site.
func (s *AppServer) isSameOriginRequest(r *http.Request) bool {
	sfs := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")))
	if sfs == "cross-site" {
		return false
	}
	reqHost := strings.ToLower(strings.TrimSpace(r.Host))
	pubHost := ""
	if s.deps.Config != nil && strings.TrimSpace(s.deps.Config.PublicURL) != "" {
		if u, err := url.Parse(strings.TrimSpace(s.deps.Config.PublicURL)); err == nil {
			pubHost = strings.ToLower(strings.TrimSpace(u.Host))
		}
	}
	matchesHost := func(raw string) bool {
		if strings.EqualFold(raw, "null") {
			return false
		}
		u, err := url.Parse(raw)
		if err != nil || strings.TrimSpace(u.Host) == "" {
			return false
		}
		h := strings.ToLower(strings.TrimSpace(u.Host))
		return (reqHost != "" && h == reqHost) || (pubHost != "" && h == pubHost)
	}
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		return matchesHost(origin)
	}
	if ref := strings.TrimSpace(r.Header.Get("Referer")); ref != "" {
		return matchesHost(ref)
	}
	return true
}

func (s *AppServer) requireSameOrigin(w http.ResponseWriter, r *http.Request) bool {
	if !s.isSameOriginRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request blocked"})
		return false
	}
	return true
}

var blockedSetupDBPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

func isForbiddenSetupDBPort(port int) bool {
	if port <= 0 || port > 65535 || port < 1024 {
		return true
	}
	switch port {
	case 2375, 2376, 2379, 6379, 8500, 9200, 10250, 11211:
		return true
	default:
		return false
	}
}

func validateSetupDBEndpoint(ctx context.Context, host string, port int) error {
	clean := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(strings.Trim(host, "[]")), "."))
	if clean == "" || clean == "metadata" || strings.HasSuffix(clean, ".internal") {
		return fmt.Errorf("forbidden database hostname %q", host)
	}
	if isForbiddenSetupDBPort(port) {
		return fmt.Errorf("forbidden database port %d", port)
	}
	checkIP := func(ip netip.Addr) error {
		u := ip.Unmap()
		if !u.IsValid() || u.IsUnspecified() || u.IsLinkLocalUnicast() || u.IsLinkLocalMulticast() || u.IsMulticast() {
			return fmt.Errorf("forbidden database target address %s", u.String())
		}
		for _, pfx := range blockedSetupDBPrefixes {
			if pfx.Contains(u) {
				return fmt.Errorf("forbidden database target address %s", u.String())
			}
		}
		return nil
	}
	if ip, err := netip.ParseAddr(clean); err == nil {
		return checkIP(ip)
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", clean)
	if err != nil || len(addrs) == 0 {
		return fmt.Errorf("cannot resolve database host %q", host)
	}
	for _, ip := range addrs {
		if err := checkIP(ip); err != nil {
			return err
		}
	}
	return nil
}

func extractSetupSQLTargets(driver, dsn string) ([]struct {
	host string
	port int
}, error) {
	lower := strings.ToLower(dsn)
	unescapedLower := lower
	if u, uErr := url.QueryUnescape(dsn); uErr == nil {
		unescapedLower = strings.ToLower(u)
	}
	for _, bad := range []string{
		"allowallfiles", "allowurlinlocalinfile", "allowlocalinfile", "localinfile", "infile",
		"unix(", "host=/", "passfile", "service=", "servicefile",
		"sslrootcert", "sslcert", "sslkey", "sslpassword", "serverpubkey",
	} {
		if strings.Contains(lower, bad) || strings.Contains(unescapedLower, bad) {
			return nil, fmt.Errorf("forbidden database DSN parameter %q", strings.Trim(bad, "=(/"))
		}
	}

	var out []struct {
		host string
		port int
	}
	addHostPort := func(raw string, defaultPort int) error {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return fmt.Errorf("missing database host")
		}
		h := raw
		p := defaultPort
		if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
			h = strings.Trim(raw, "[]")
		} else if sh, sp, err := net.SplitHostPort(raw); err == nil {
			h = sh
			parsedPort, pErr := strconv.Atoi(sp)
			if pErr != nil {
				return fmt.Errorf("invalid database port %q", sp)
			}
			p = parsedPort
		} else if strings.Count(raw, ":") == 1 {
			return fmt.Errorf("invalid database host:port %q", raw)
		}
		h = strings.Trim(strings.TrimSpace(h), "[]")
		if h == "" {
			return fmt.Errorf("missing database host")
		}
		if strings.HasPrefix(h, "/") {
			return fmt.Errorf("unix socket database paths are not permitted")
		}
		out = append(out, struct {
			host string
			port int
		}{host: h, port: p})
		return nil
	}

	switch driver {
	case "postgres":
		pgCfg, err := pgconn.ParseConfig(dsn)
		if err != nil {
			return nil, fmt.Errorf("invalid postgres DSN: %w", err)
		}
		primaryHost := strings.TrimSpace(pgCfg.Host)
		if primaryHost == "" || strings.HasPrefix(primaryHost, "/") {
			return nil, fmt.Errorf("postgres DSN requires an explicit TCP host (unix sockets are forbidden)")
		}
		primaryPort := int(pgCfg.Port)
		if primaryPort <= 0 {
			primaryPort = 5432
		}
		out = append(out, struct {
			host string
			port int
		}{host: primaryHost, port: primaryPort})
		for _, fb := range pgCfg.Fallbacks {
			fbHost := strings.TrimSpace(fb.Host)
			if fbHost == "" || strings.HasPrefix(fbHost, "/") {
				return nil, fmt.Errorf("postgres fallback unix socket database paths are not permitted")
			}
			fbPort := int(fb.Port)
			if fbPort <= 0 {
				fbPort = primaryPort
			}
			out = append(out, struct {
				host string
				port int
			}{host: fbHost, port: fbPort})
		}
		return out, nil

	case "mysql":
		myCfg, err := mysql.ParseDSN(dsn)
		if err != nil {
			return nil, fmt.Errorf("invalid mysql DSN: %w", err)
		}
		if myCfg.AllowAllFiles {
			return nil, fmt.Errorf("forbidden mysql DSN parameter AllowAllFiles")
		}
		if myCfg.AllowOldPasswords {
			return nil, fmt.Errorf("forbidden mysql DSN parameter AllowOldPasswords")
		}
		if strings.TrimSpace(myCfg.ServerPubKey) != "" {
			return nil, fmt.Errorf("forbidden mysql DSN parameter ServerPubKey")
		}
		if strings.ToLower(strings.TrimSpace(myCfg.Net)) != "tcp" {
			return nil, fmt.Errorf("mysql DSN must use tcp(host:port)")
		}
		switch strings.ToLower(strings.TrimSpace(myCfg.TLSConfig)) {
		case "", "true", "false", "skip-verify", "preferred":
		default:
			return nil, fmt.Errorf("unsupported custom mysql tls profile %q", myCfg.TLSConfig)
		}
		for k := range myCfg.Params {
			kl := strings.ToLower(strings.TrimSpace(k))
			if strings.Contains(kl, "infile") || strings.Contains(kl, "allow") {
				return nil, fmt.Errorf("forbidden mysql DSN query parameter %q", k)
			}
		}
		if err := addHostPort(myCfg.Addr, 3306); err != nil {
			return nil, err
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported database driver %q", driver)
	}
}

func (s *AppServer) validateAndNormalizeSetupDB(ctx context.Context, rawDriver, rawDSN string) (string, string, error) {
	driver := strings.ToLower(strings.TrimSpace(rawDriver))
	switch driver {
	case "", "sqlite", "sqlite3":
		driver = "sqlite"
	case "postgres", "postgresql", "pgx":
		driver = "postgres"
	case "mysql", "mariadb":
		driver = "mysql"
	default:
		return "", "", fmt.Errorf("unsupported database driver %q", rawDriver)
	}

	dsn := strings.TrimSpace(rawDSN)
	if strings.ContainsRune(dsn, 0) {
		return "", "", fmt.Errorf("invalid database DSN")
	}

	if driver == "sqlite" {
		if dsn == "" {
			dsn = filepath.Join(s.deps.Config.DataDir, "volo.db")
		}
		if dsn == ":memory:" || strings.HasPrefix(dsn, "file::memory:") {
			if s.deps.OutboundDialer != nil && !s.deps.OutboundDialer.AllowLoopbackForTests {
				return "", "", fmt.Errorf("in-memory sqlite is disabled in production setup")
			}
			return "sqlite", dsn, nil
		}
		if strings.ContainsAny(dsn, "?#") || strings.HasPrefix(strings.ToLower(dsn), "file:") || strings.Contains(dsn, "..") {
			return "", "", fmt.Errorf("sqlite path must be a plain .db/.sqlite/.sqlite3 file inside data directory")
		}
		dataAbs, err := filepath.Abs(s.deps.Config.DataDir)
		if err != nil {
			return "", "", err
		}
		var cand string
		if filepath.IsAbs(dsn) {
			cand, err = filepath.Abs(dsn)
			if err != nil {
				return "", "", err
			}
		} else {
			cand = filepath.Join(dataAbs, dsn)
		}
		cand = filepath.Clean(cand)
		if !strings.HasPrefix(cand, dataAbs+string(filepath.Separator)) {
			return "", "", fmt.Errorf("sqlite database path must reside inside data directory")
		}
		switch strings.ToLower(filepath.Ext(cand)) {
		case ".db", ".sqlite", ".sqlite3":
		default:
			return "", "", fmt.Errorf("sqlite database file must end with .db, .sqlite, or .sqlite3")
		}
		parentDir := filepath.Dir(cand)
		if err := os.MkdirAll(parentDir, 0o750); err != nil {
			return "", "", err
		}
		if realParent, err := filepath.EvalSymlinks(parentDir); err == nil {
			realData, dErr := filepath.EvalSymlinks(dataAbs)
			if dErr == nil && realParent != realData && !strings.HasPrefix(realParent, realData+string(filepath.Separator)) {
				return "", "", fmt.Errorf("sqlite database path escapes data directory via symlink")
			}
		}
		return "sqlite", cand, nil
	}

	if dsn == "" {
		return "", "", fmt.Errorf("database DSN is required for %s", driver)
	}
	targets, err := extractSetupSQLTargets(driver, dsn)
	if err != nil {
		return "", "", err
	}
	for _, t := range targets {
		if err := validateSetupDBEndpoint(ctx, t.host, t.port); err != nil {
			return "", "", err
		}
	}
	return driver, dsn, nil
}

func (s *AppServer) isSameCurrentDB(driver, dsn string, simpleProto bool) bool {
	curDriver := strings.ToLower(strings.TrimSpace(s.deps.Config.DBDriver))
	if curDriver == "" {
		curDriver = s.deps.Store.Dialect().Name()
	}
	if driver != curDriver || simpleProto != s.deps.Config.DBSimpleProtocol {
		return false
	}
	if driver == "sqlite" {
		curAbs, err1 := filepath.Abs(strings.TrimSpace(s.deps.Config.DBDSN))
		reqAbs, err2 := filepath.Abs(strings.TrimSpace(dsn))
		if err1 == nil && err2 == nil && filepath.Clean(curAbs) == filepath.Clean(reqAbs) {
			return true
		}
	}
	return strings.TrimSpace(dsn) == strings.TrimSpace(s.deps.Config.DBDSN)
}

func (s *AppServer) probeSetupDB(ctx context.Context, driver, dsn string, simpleProto bool) (string, int64, error) {
	testCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	existedBefore := true
	if driver == "sqlite" && dsn != ":memory:" && !strings.HasPrefix(dsn, "file::memory:") {
		if _, err := os.Stat(dsn); os.IsNotExist(err) {
			existedBefore = false
		}
	}
	cleanupSQLiteProbe := func() {
		if !existedBefore {
			_ = os.Remove(dsn)
			_ = os.Remove(dsn + "-wal")
			_ = os.Remove(dsn + "-shm")
		}
	}

	start := time.Now()
	probeStore, err := sqlx.Open(testCtx, sqlx.Options{
		Driver:         driver,
		DSN:            dsn,
		SimpleProtocol: simpleProto,
		MaxOpenConns:   2,
		MaxIdleConns:   1,
	})
	if err != nil {
		cleanupSQLiteProbe()
		return "", 0, err
	}

	var one int
	qErr := probeStore.QueryRowContext(testCtx, "SELECT 1").Scan(&one)
	dialectName := probeStore.Dialect().Name()
	_ = probeStore.Close()
	cleanupSQLiteProbe()
	if qErr != nil {
		return "", 0, qErr
	}
	return dialectName, time.Since(start).Milliseconds(), nil
}

// applySetupDatabaseSelection atomically opens, migrates, persists, and swaps the runtime DB pool
// when the Setup Wizard selects a database connection different from the boot configuration.
func (s *AppServer) applySetupDatabaseSelection(ctx context.Context, rawDriver, rawDSN string, simpleProto bool) error {
	if strings.TrimSpace(rawDriver) == "" && strings.TrimSpace(rawDSN) == "" {
		return nil
	}
	if strings.TrimSpace(rawDriver) == "" {
		rawDriver = s.deps.Config.DBDriver
	}
	driver, dsn, err := s.validateAndNormalizeSetupDB(ctx, rawDriver, rawDSN)
	if err != nil {
		return err
	}
	if s.isSameCurrentDB(driver, dsn, simpleProto) {
		return nil
	}

	newStore, err := sqlx.Open(ctx, sqlx.Options{
		Driver:         driver,
		DSN:            dsn,
		SimpleProtocol: simpleProto,
		MaxOpenConns:   s.deps.Config.DBMaxOpenConns,
		MaxIdleConns:   s.deps.Config.DBMaxIdleConns,
	})
	if err != nil {
		return err
	}
	if err := migrate.Up(ctx, newStore); err != nil {
		_ = newStore.Close()
		return err
	}
	if err := config.SaveWizardDBConfig(s.deps.Config.DataDir, s.deps.Config.KeyRing, config.WizardDBSelection{
		Driver:         driver,
		DSN:            dsn,
		SimpleProtocol: simpleProto,
	}); err != nil {
		_ = newStore.Close()
		return err
	}

	s.deps.Config.DBDriver = driver
	s.deps.Config.DBDSN = dsn
	s.deps.Config.DBSimpleProtocol = simpleProto
	s.deps.Store.Swap(newStore)
	if s.deps.Content != nil {
		s.deps.Content.ResetAnalyticsBuffer()
	}
	if _, err := s.deps.Settings.Reload(ctx); err != nil {
		return err
	}
	s.reloadRuntimeAfterDBRestore(ctx)
	return nil
}

// reloadRuntimeAfterDBRestore resets buffered analytics and reloads settings, WAF bans, GeoIP rules,
// themes, external plugins, media dimensions, and public content/search caches after any DB swap or restore.
func (s *AppServer) reloadRuntimeAfterDBRestore(ctx context.Context) {
	if s == nil {
		return
	}
	if s.deps.Content != nil {
		s.deps.Content.ResetAnalyticsBuffer()
	}
	if s.deps.Settings != nil {
		_, _ = s.deps.Settings.Reload(ctx)
	}
	if s.deps.WAF != nil {
		_ = s.deps.WAF.ReloadBans(ctx)
	}
	if s.deps.Geo != nil {
		_ = s.deps.Geo.ReloadRules(ctx, s.deps.Store)
	}
	if s.deps.Themes != nil {
		_ = s.deps.Themes.Reload()
	}
	if s.deps.Plugins != nil {
		_ = s.deps.Plugins.DiscoverAndStart(ctx)
	}
	if s.deps.Content != nil {
		_ = s.deps.Content.RefreshMediaDimsCache(ctx)
		_ = s.deps.Content.WarmCaches(ctx)
	}
}

// isSetupUnlocked reports whether the first-boot Setup Wizard is still unlocked.
// Once `Site.Initialized == true` OR any `admin` user exists in `users`, `/setup` and `/api/setup/*`
// permanently return 404 Not Found so the wizard can NEVER be re-entered or probed!
func (s *AppServer) isSetupUnlocked(ctx context.Context) bool {
	if s.deps.Settings.Snapshot().Site.Initialized {
		return false
	}
	var adminCount int
	if err := s.deps.Store.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE role = ?`, domain.RoleAdmin).Scan(&adminCount); err == nil && adminCount > 0 {
		return false
	}
	return true
}

func (s *AppServer) registerSetupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/setup", s.handleSetupSPA)
	mux.HandleFunc("/setup/", s.handleSetupSPA)
	mux.HandleFunc("/api/setup/status", s.handleSetupStatus)
	mux.HandleFunc("/api/setup/test-db", s.handleSetupTestDB)
	mux.HandleFunc("/api/setup/import-legacy", s.handleSetupImportLegacy)
	mux.HandleFunc("/api/setup/complete", s.handleSetupComplete)
}

func (s *AppServer) handleSetupSPA(w http.ResponseWriter, r *http.Request) {
	if !s.isSetupUnlocked(r.Context()) {
		http.NotFound(w, r)
		return
	}
	snap := s.deps.Settings.Snapshot()
	adminui.Default().ServeMount(w, r, "/setup", adminui.BootConfig{
		Mode:          "setup",
		AdminPath:     s.deps.Config.AdminPath,
		PublicURL:     s.publicBaseURL(r),
		DefaultLocale: snap.Site.DefaultLocale,
		Accent:        snap.Appearance.Accent,
		DefaultTheme:  snap.Appearance.DefaultMode,
		SiteTitle:     snap.Site.Title,
		Locales:       s.deps.Catalog.Locales(),
	})
}

func (s *AppServer) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	if !s.isSetupUnlocked(r.Context()) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	snap := s.deps.Settings.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"initialized":       false,
		"current_driver":    s.deps.Store.Dialect().Name(),
		"default_locale":    snap.Site.DefaultLocale,
		"supported_locales": s.deps.Catalog.Locales(),
		"admin_path":        s.deps.Config.AdminPath,
		"timezone":          snap.Site.TimeZone,
		"accent":            snap.Appearance.Accent,
	})
}

func (s *AppServer) handleSetupTestDB(w http.ResponseWriter, r *http.Request) {
	if !s.isSetupUnlocked(r.Context()) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.requireSameOrigin(w, r) {
		return
	}
	clientIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
	if !s.deps.WAF.AllowRate("setup:testdb", clientIP, 15, time.Minute) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limited"})
		return
	}

	var req struct {
		Driver         string `json:"driver"`
		DSN            string `json:"dsn"`
		SimpleProtocol bool   `json:"simple_protocol"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	rawDriver := strings.TrimSpace(req.Driver)
	rawDSN := strings.TrimSpace(req.DSN)
	if rawDriver == "" {
		rawDriver = s.deps.Config.DBDriver
	}
	if rawDSN == "" && strings.EqualFold(rawDriver, s.deps.Config.DBDriver) {
		rawDSN = s.deps.Config.DBDSN
	}

	driver, dsn, err := s.validateAndNormalizeSetupDB(r.Context(), rawDriver, rawDSN)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}

	dialectName, latencyMS, err := s.probeSetupDB(r.Context(), driver, dsn, req.SimpleProtocol)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"dialect":    dialectName,
		"latency_ms": latencyMS,
	})
}

func (s *AppServer) handleSetupImportLegacy(w http.ResponseWriter, r *http.Request) {
	if !s.isSetupUnlocked(r.Context()) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.requireSameOrigin(w, r) {
		return
	}
	clientIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
	if !s.deps.WAF.AllowRate("setup:import", clientIP, 15, time.Minute) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limited"})
		return
	}

	rep, dryRun, newSiteURL, err := s.streamLegacyImportUpload(w, r, true)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if dryRun {
		targetDialect := s.deps.Store.Dialect().Name()
		if rep != nil && rep.TargetDialect != "" {
			targetDialect = rep.TargetDialect
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":             true,
			"dry_run":        true,
			"report":         rep,
			"admin_path":     s.deps.Config.AdminPath,
			"target_dialect": targetDialect,
		})
		return
	}

	if newSiteURL != "" {
		s.deps.Config.PublicURL = strings.TrimRight(newSiteURL, "/")
		if s.deps.WAF != nil {
			s.deps.WAF.SetPublicURL(s.deps.Config.PublicURL)
		}
	}
	_, _ = s.deps.Settings.Reload(r.Context())
	_, _, _ = s.deps.Settings.PatchDomain(r.Context(), domain.SettingsDomainSite, []byte(`{"initialized":true}`), 0)
	s.reloadRuntimeAfterDBRestore(r.Context())

	resp := map[string]any{
		"ok":             true,
		"report":         rep,
		"admin_path":     s.deps.Config.AdminPath,
		"target_dialect": s.deps.Store.Dialect().Name(),
	}
	// Automatically sign in the primary imported administrator so entering /console works seamlessly!
	if adminUser, qErr := sqlx.Get[domain.User](r.Context(), s.deps.Store,
		`SELECT * FROM users WHERE role = ? AND status = ? ORDER BY created_at ASC LIMIT 1`,
		domain.RoleAdmin, domain.UserStatusActive); qErr == nil && adminUser.ID != "" {
		geo := ""
		if s.deps.Geo != nil {
			geo = s.deps.Geo.LookupDisplay(clientIP)
		}
		if sess, rawTok, sErr := s.deps.Auth.CreateSession(r.Context(), adminUser.ID, clientIP, geo, r.UserAgent(), 0); sErr == nil {
			s.setSessionCookie(w, rawTok)
			adminUser.PasswordHash = ""
			adminUser.LegacyHash = ""
			resp["user"] = adminUser
			resp["csrf_token"] = s.deps.Auth.ComputeCSRFToken(sess.ID)
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *AppServer) handleSetupComplete(w http.ResponseWriter, r *http.Request) {
	if !s.isSetupUnlocked(r.Context()) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.requireSameOrigin(w, r) {
		return
	}
	clientIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
	if !s.deps.WAF.AllowRate("setup:complete", clientIP, 10, time.Minute) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limited"})
		return
	}

	var req struct {
		Driver           string `json:"driver"`
		DBDriver         string `json:"db_driver"`
		DSN              string `json:"dsn"`
		DBDSN            string `json:"db_dsn"`
		SimpleProtocol   *bool  `json:"simple_protocol"`
		DBSimpleProtocol *bool  `json:"db_simple_protocol"`
		SiteTitle        string `json:"site_title"`
		SiteSubtitle     string `json:"site_subtitle"`
		SiteDescription  string `json:"site_description"`
		DefaultLocale    string `json:"default_locale"`
		TimeZone         string `json:"timezone"`
		Accent           string `json:"accent"`
		AdminUsername    string `json:"admin_username"`
		AdminEmail       string `json:"admin_email"`
		AdminDisplayName string `json:"admin_display_name"`
		AdminPassword    string `json:"admin_password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32768)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}

	uname := strings.TrimSpace(req.AdminUsername)
	email := strings.ToLower(strings.TrimSpace(req.AdminEmail))
	if len(uname) < 2 || len(uname) > 48 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "admin_username must be 2-48 characters"})
		return
	}
	if !strings.Contains(email, "@") || len(email) > 190 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "valid admin_email is required"})
		return
	}
	if err := auth.ValidatePasswordPolicy(req.AdminPassword); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	pwHash, err := auth.HashPassword(req.AdminPassword)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "password hashing failed"})
		return
	}

	chosenDriver := strings.TrimSpace(req.Driver)
	if chosenDriver == "" {
		chosenDriver = strings.TrimSpace(req.DBDriver)
	}
	chosenDSN := strings.TrimSpace(req.DSN)
	if chosenDSN == "" {
		chosenDSN = strings.TrimSpace(req.DBDSN)
	}
	chosenSimpleProto := s.deps.Config.DBSimpleProtocol
	if req.SimpleProtocol != nil {
		chosenSimpleProto = *req.SimpleProtocol
	} else if req.DBSimpleProtocol != nil {
		chosenSimpleProto = *req.DBSimpleProtocol
	}
	if err := s.applySetupDatabaseSelection(r.Context(), chosenDriver, chosenDSN, chosenSimpleProto); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	locale := s.deps.Catalog.Match(req.DefaultLocale, "")
	isZH := strings.HasPrefix(strings.ToLower(locale), "zh")
	displayName := strings.TrimSpace(req.AdminDisplayName)
	if displayName == "" {
		displayName = uname
	}
	siteTitle := strings.TrimSpace(req.SiteTitle)
	if siteTitle == "" {
		if isZH {
			siteTitle = "Volo 随笔"
		} else {
			siteTitle = "Volo Blog"
		}
	}
	siteSubtitle := strings.TrimSpace(req.SiteSubtitle)
	if siteSubtitle == "" {
		if isZH {
			siteSubtitle = "记录 · 思考 · 分享"
		} else {
			siteSubtitle = "Independent thoughts & craft"
		}
	}
	tz := strings.TrimSpace(req.TimeZone)
	if tz == "" {
		tz = "Asia/Shanghai"
	}
	accent := strings.ToLower(strings.TrimSpace(req.Accent))
	switch accent {
	case "forest", "ocean", "teal", "amber", "plum", "rose":
	default:
		accent = "forest"
	}

	now := time.Now().UnixMilli()
	adminUser := domain.User{
		ID:              id.Next(),
		Username:        uname,
		Email:           email,
		DisplayName:     displayName,
		AvatarURL:       "/avatars/" + uname + ".svg",
		Role:            domain.RoleAdmin,
		PasswordHash:    pwHash,
		Status:          domain.UserStatusActive,
		EmailVerifiedAt: now,
		PreferredLocale: locale,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	// Atomically ensure zero race condition on initial admin creation + starter About page!
	err = s.deps.Store.WithTx(r.Context(), func(tx *sqlx.Tx) error {
		var cnt int
		if qErr := tx.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM users WHERE role = ?`, domain.RoleAdmin).Scan(&cnt); qErr != nil {
			return qErr
		}
		if cnt > 0 {
			return fmt.Errorf("setup already completed")
		}
		if _, iErr := tx.ExecContext(r.Context(),
			`INSERT INTO users (id, username, email, display_name, avatar_url, bio, website_url, role, password_hash, legacy_hash, status, email_verified_at, preferred_locale, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, '', '', ?, ?, '', ?, ?, ?, ?, ?)`,
			adminUser.ID, adminUser.Username, adminUser.Email, adminUser.DisplayName, adminUser.AvatarURL,
			adminUser.Role, adminUser.PasswordHash, adminUser.Status, adminUser.EmailVerifiedAt, adminUser.PreferredLocale,
			adminUser.CreatedAt, adminUser.UpdatedAt,
		); iErr != nil {
			return iErr
		}

		// Seed default starter category & "/about" standalone page if empty
		var catCnt int
		_ = tx.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM categories`).Scan(&catCnt)
		if catCnt == 0 {
			catID := id.Next()
			catName := "Notes"
			catDesc := "Essays & engineering notes"
			if isZH {
				catName = "随笔"
				catDesc = "独立思考与工程手记"
			}
			_, _ = tx.ExecContext(r.Context(),
				`INSERT INTO categories (id, name, slug, description, icon, sort_order, created_at, updated_at)
				 VALUES (?, ?, 'notes', ?, '', 1, ?, ?)`,
				catID, catName, catDesc, now, now)
		}
		var pageCnt int
		_ = tx.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM pages`).Scan(&pageCnt)
		if pageCnt == 0 {
			pageID := id.Next()
			aboutTitle := "About"
			aboutBody := "# About\n\nWelcome to **" + siteTitle + "**, powered by Volo-go."
			if isZH {
				aboutTitle = "关于"
				aboutBody = "# 关于\n\n欢迎来到 **" + siteTitle + "**，本站由纯 Go 单文件独立博客引擎 Volo-go 驱动。"
			}
			_, _ = tx.ExecContext(r.Context(),
				`INSERT INTO pages (id, author_id, title, slug, content_md, is_published, allow_comment, show_toc, lang, translation_group, view_count, comment_count, created_at, updated_at)
				 VALUES (?, ?, ?, 'about', ?, ?, ?, ?, ?, '', 0, 0, ?, ?)`,
				pageID, adminUser.ID, aboutTitle, aboutBody, true, true, false, locale, now, now)
			_, _ = tx.ExecContext(r.Context(),
				`INSERT INTO nav_items (id, parent_id, title, i18n_titles_json, url, open_in_new_tab, icon, sort_order, is_enabled, created_at, updated_at)
				 VALUES (?, '', ?, '{"zh-CN":"关于","en-US":"About"}', '/about', ?, '', 10, ?, ?, ?)`,
				id.Next(), aboutTitle, false, true, now, now)
		}
		return nil
	})
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}

	sitePatch, _ := json.Marshal(map[string]any{
		"initialized":    true,
		"title":          siteTitle,
		"subtitle":       siteSubtitle,
		"description":    strings.TrimSpace(req.SiteDescription),
		"default_locale": locale,
		"timezone":       tz,
	})
	_, _, _ = s.deps.Settings.PatchDomain(r.Context(), domain.SettingsDomainSite, sitePatch, 0)

	appPatch, _ := json.Marshal(map[string]any{"accent": accent})
	_, _, _ = s.deps.Settings.PatchDomain(r.Context(), domain.SettingsDomainAppearance, appPatch, 0)
	_ = s.deps.Content.WarmCaches(r.Context())

	geo := ""
	if s.deps.Geo != nil {
		geo = s.deps.Geo.LookupDisplay(clientIP)
	}
	sess, rawTok, err := s.deps.Auth.CreateSession(r.Context(), adminUser.ID, clientIP, geo, r.UserAgent(), 0)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.setSessionCookie(w, rawTok)
	adminUser.PasswordHash = ""

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"admin_path":     s.deps.Config.AdminPath,
		"target_dialect": s.deps.Store.Dialect().Name(),
		"user":           adminUser,
		"csrf_token":     s.deps.Auth.ComputeCSRFToken(sess.ID),
	})
}

func splitSetupURLPrefixes(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func parseSetupBoolField(raw string, fallback bool) bool {
	v := strings.ToLower(strings.TrimSpace(raw))
	switch v {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	default:
		return fallback
	}
}

func (s *AppServer) streamLegacyImportUpload(w http.ResponseWriter, r *http.Request, allowDBSwap bool) (*etl.ImportReport, bool, string, error) {
	const maxLegacyBytes = 256 << 20 // 256 MiB streaming cap
	r.Body = http.MaxBytesReader(w, r.Body, maxLegacyBytes)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		return nil, false, "", fmt.Errorf("upload parse error: %w", err)
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		return nil, false, "", fmt.Errorf("missing multipart 'file' field")
	}
	defer file.Close()

	dryRaw := strings.TrimSpace(r.URL.Query().Get("dry_run"))
	if dryRaw == "" {
		dryRaw = strings.TrimSpace(r.FormValue("dry_run"))
	}
	dryRun := strings.EqualFold(dryRaw, "true") || dryRaw == "1"

	rawDriver := strings.TrimSpace(r.FormValue("driver"))
	if rawDriver == "" {
		rawDriver = strings.TrimSpace(r.FormValue("db_driver"))
	}
	rawDSN := strings.TrimSpace(r.FormValue("dsn"))
	if rawDSN == "" {
		rawDSN = strings.TrimSpace(r.FormValue("db_dsn"))
	}
	simpleRaw := strings.TrimSpace(r.FormValue("simple_protocol"))
	if simpleRaw == "" {
		simpleRaw = strings.TrimSpace(r.FormValue("db_simple_protocol"))
	}
	simpleProto := parseSetupBoolField(simpleRaw, s.deps.Config.DBSimpleProtocol)

	newSiteURL := strings.TrimSpace(r.FormValue("new_site_url"))
	if newSiteURL != "" {
		parsedURL, uErr := url.Parse(newSiteURL)
		if uErr != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || strings.TrimSpace(parsedURL.Host) == "" {
			return nil, dryRun, "", fmt.Errorf("new_site_url must be a valid http(s):// URL")
		}
		newSiteURL = strings.TrimRight(newSiteURL, "/")
	}

	adminUsername := strings.TrimSpace(r.FormValue("admin_username"))
	if adminUsername != "" && (len(adminUsername) < 2 || len(adminUsername) > 48) {
		return nil, dryRun, "", fmt.Errorf("admin_username must be 2-48 characters")
	}
	adminEmail := strings.ToLower(strings.TrimSpace(r.FormValue("admin_email")))
	if adminEmail != "" && (!strings.Contains(adminEmail, "@") || len(adminEmail) > 190) {
		return nil, dryRun, "", fmt.Errorf("valid admin_email is required")
	}
	adminDisplayName := strings.TrimSpace(r.FormValue("admin_display_name"))
	adminPassword := r.FormValue("admin_password")
	adminPasswordHash := ""
	if adminPassword != "" {
		if err := auth.ValidatePasswordPolicy(adminPassword); err != nil {
			return nil, dryRun, "", err
		}
		h, hErr := auth.HashPassword(adminPassword)
		if hErr != nil {
			return nil, dryRun, "", fmt.Errorf("password hashing failed")
		}
		adminPasswordHash = h
	}

	var prevalidatedDialect string
	if allowDBSwap && dryRun {
		if rawDriver != "" || rawDSN != "" {
			probeDriver := rawDriver
			if probeDriver == "" {
				probeDriver = s.deps.Config.DBDriver
			}
			normDriver, normDSN, vErr := s.validateAndNormalizeSetupDB(r.Context(), probeDriver, rawDSN)
			if vErr != nil {
				return nil, true, "", vErr
			}
			prevalidatedDialect = normDriver
			if !s.isSameCurrentDB(normDriver, normDSN, simpleProto) {
				if dName, _, pErr := s.probeSetupDB(r.Context(), normDriver, normDSN, simpleProto); pErr != nil {
					return nil, true, "", pErr
				} else if dName != "" {
					prevalidatedDialect = dName
				}
			}
		}
	}

	ext := strings.ToLower(filepath.Ext(hdr.Filename))
	if ext != ".zip" && ext != ".json" {
		ext = ".zip"
	}
	tmpFile, err := os.CreateTemp(s.ensureTempDir(), "volo-legacy-*"+ext)
	if err != nil {
		return nil, dryRun, "", err
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmpFile, io.LimitReader(file, maxLegacyBytes)); err != nil {
		_ = tmpFile.Close()
		return nil, dryRun, "", err
	}
	_ = tmpFile.Close()

	if allowDBSwap && !dryRun {
		if err := s.applySetupDatabaseSelection(r.Context(), rawDriver, rawDSN, simpleProto); err != nil {
			return nil, false, "", err
		}
	}

	storageMode := strings.ToLower(strings.TrimSpace(r.FormValue("storage_mode")))
	opts := etl.ImportOptions{
		DryRun:            dryRun,
		TargetUploadDir:   s.deps.Config.UploadDir,
		AdminPath:         s.deps.Config.AdminPath,
		OldURLPrefixes:    splitSetupURLPrefixes(r.FormValue("old_site_url")),
		NewSiteURL:        newSiteURL,
		SiteTitle:         strings.TrimSpace(r.FormValue("site_title")),
		SiteSubtitle:      strings.TrimSpace(r.FormValue("site_subtitle")),
		DefaultLocale:     strings.TrimSpace(r.FormValue("default_locale")),
		TimeZone:          strings.TrimSpace(r.FormValue("timezone")),
		Accent:            strings.TrimSpace(r.FormValue("accent")),
		ForceLocalStorage: storageMode == "local",
		AdminUsername:     adminUsername,
		AdminEmail:        adminEmail,
		AdminDisplayName:  adminDisplayName,
		AdminPasswordHash: adminPasswordHash,
	}

	rep, err := etl.ImportLegacyArchive(r.Context(), s.deps.Store, s.deps.Settings, s.deps.Config.KeyRing, tmpPath, opts)
	if err != nil {
		return nil, dryRun, "", err
	}
	if dryRun && prevalidatedDialect != "" && rep != nil {
		rep.TargetDialect = prevalidatedDialect
	}
	return rep, dryRun, newSiteURL, nil
}

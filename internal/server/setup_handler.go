package server

import (
	"context"
	"encoding/json"
	"fmt"
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

	"github.com/Willh92/Volo-go/internal/adminui"
	"github.com/Willh92/Volo-go/internal/auth"
	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/etl"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/pkg/id"
)

func (s *AppServer) isTrustedProxyPeer(r *http.Request) bool {
	if r == nil || s.deps.Config == nil {
		return false
	}
	policy := s.deps.Config.TrustedProxies
	remote := strings.TrimSpace(r.RemoteAddr)
	isUnixPeer := remote == "" || remote == "@" || remote == "unix" || strings.HasPrefix(remote, "/")
	if isUnixPeer {
		return policy.TrustUnix
	}
	hostStr := remote
	if h, _, err := net.SplitHostPort(remote); err == nil {
		hostStr = h
	}
	if idx := strings.IndexByte(hostStr, '%'); idx > 0 {
		hostStr = hostStr[:idx]
	}
	peerAddr, err := netip.ParseAddr(strings.Trim(hostStr, "[]"))
	if err != nil {
		return false
	}
	peerAddr = peerAddr.Unmap()
	for _, pfx := range policy.Prefixes {
		if pfx.Contains(peerAddr) {
			return true
		}
	}
	return false
}

func (s *AppServer) effectiveRequestScheme(r *http.Request) string {
	if r != nil && r.TLS != nil {
		return "https"
	}
	if r != nil && s.isTrustedProxyPeer(r) {
		if xfp := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); xfp != "" {
			switch strings.ToLower(strings.TrimSpace(strings.Split(xfp, ",")[0])) {
			case "https":
				return "https"
			case "http":
				return "http"
			}
		}
	}
	if s != nil && s.deps.Config != nil {
		if pubRaw := strings.TrimSpace(s.deps.Config.GetPublicURL()); pubRaw != "" {
			if u, err := url.Parse(pubRaw); err == nil {
				if r == nil || strings.TrimSpace(r.Host) == "" || strings.EqualFold(strings.TrimSpace(r.Host), strings.TrimSpace(u.Host)) {
					switch strings.ToLower(u.Scheme) {
					case "https":
						return "https"
					case "http":
						return "http"
					}
				}
			}
		}
	}
	return "http"
}

// isSameOriginRequest enforces browser Same-Origin verification (both host AND scheme!) on
// state-changing endpoints while allowing non-browser CLI/API clients that omit Origin/Referer/Sec-Fetch-Site.
func (s *AppServer) isSameOriginRequest(r *http.Request) bool {
	sfs := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")))
	if sfs == "cross-site" {
		return false
	}
	reqHost := strings.ToLower(strings.TrimSpace(r.Host))
	reqScheme := s.effectiveRequestScheme(r)

	pubHost := ""
	pubScheme := ""
	if s.deps.Config != nil {
		if pubRaw := strings.TrimSpace(s.deps.Config.GetPublicURL()); pubRaw != "" {
			if u, err := url.Parse(pubRaw); err == nil && (u.Scheme == "http" || u.Scheme == "https") && strings.TrimSpace(u.Host) != "" {
				pubHost = strings.ToLower(strings.TrimSpace(u.Host))
				pubScheme = strings.ToLower(strings.TrimSpace(u.Scheme))
			}
		}
	}
	matchesOrigin := func(raw string) bool {
		if strings.EqualFold(raw, "null") {
			return false
		}
		u, err := url.Parse(raw)
		if err != nil || strings.TrimSpace(u.Host) == "" {
			return false
		}
		h := strings.ToLower(strings.TrimSpace(u.Host))
		sch := strings.ToLower(strings.TrimSpace(u.Scheme))
		if sch != "http" && sch != "https" {
			return false
		}
		if reqHost != "" && h == reqHost && sch == reqScheme {
			return true
		}
		if pubHost != "" && h == pubHost && sch == pubScheme {
			return true
		}
		return false
	}
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		return matchesOrigin(origin)
	}
	if ref := strings.TrimSpace(r.Header.Get("Referer")); ref != "" {
		return matchesOrigin(ref)
	}
	return true
}

func (s *AppServer) requireSameOrigin(w http.ResponseWriter, r *http.Request) bool {
	if !s.isSameOriginRequest(r) {
		writeErrJSON(w, http.StatusForbidden, "cross-origin request blocked")
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

func validateSetupDBEndpoint(ctx context.Context, host string, port int) (netip.Addr, error) {
	clean := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(strings.Trim(host, "[]")), "."))
	if clean == "" || clean == "metadata" || strings.HasSuffix(clean, ".internal") {
		return netip.Addr{}, fmt.Errorf("forbidden database hostname %q", host)
	}
	if isForbiddenSetupDBPort(port) {
		return netip.Addr{}, fmt.Errorf("forbidden database port %d", port)
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
		if err := checkIP(ip); err != nil {
			return netip.Addr{}, err
		}
		return ip.Unmap(), nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", clean)
	if err != nil || len(addrs) == 0 {
		return netip.Addr{}, fmt.Errorf("cannot resolve database host %q", host)
	}
	for _, ip := range addrs {
		if err := checkIP(ip); err != nil {
			return netip.Addr{}, err
		}
	}
	return addrs[0].Unmap(), nil
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
		pgCfg, err := sqlx.ParsePostgresConfig(dsn)
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

func confineSQLitePathInDataDir(dataDir string, rawPath string, createParent bool) (string, error) {
	if strings.ContainsRune(rawPath, 0) || strings.ContainsAny(rawPath, "?#") ||
		strings.HasPrefix(strings.ToLower(rawPath), "file:") || strings.Contains(rawPath, "..") {
		return "", fmt.Errorf("sqlite path must be a plain .db/.sqlite/.sqlite3 file inside data directory")
	}
	dataAbs, err := filepath.Abs(dataDir)
	if err != nil {
		return "", err
	}
	dataAbs = filepath.Clean(dataAbs)
	var cand string
	if filepath.IsAbs(rawPath) {
		cand, err = filepath.Abs(rawPath)
		if err != nil {
			return "", err
		}
	} else {
		cand = filepath.Join(dataAbs, rawPath)
	}
	cand = filepath.Clean(cand)
	if strings.Contains(cand, "..") || !strings.HasPrefix(cand, dataAbs+string(filepath.Separator)) {
		return "", fmt.Errorf("sqlite database path must reside inside data directory")
	}
	switch strings.ToLower(filepath.Ext(cand)) {
	case ".db", ".sqlite", ".sqlite3":
	default:
		return "", fmt.Errorf("sqlite database file must end with .db, .sqlite, or .sqlite3")
	}
	parentDir := filepath.Dir(cand)
	if createParent {
		if err := os.MkdirAll(parentDir, 0o750); err != nil {
			return "", err
		}
	}
	if realData, dErr := filepath.EvalSymlinks(dataAbs); dErr == nil {
		if realParent, err := filepath.EvalSymlinks(parentDir); err == nil {
			if realParent != realData && !strings.HasPrefix(realParent, realData+string(filepath.Separator)) {
				return "", fmt.Errorf("sqlite database path escapes data directory via symlink")
			}
		}
		if realCand, err := filepath.EvalSymlinks(cand); err == nil {
			if !strings.HasPrefix(realCand, realData+string(filepath.Separator)) {
				return "", fmt.Errorf("sqlite database file escapes data directory via symlink")
			}
			cand = realCand
		}
	}
	return cand, nil
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
			dsn = "volo.db"
		}
		if dsn == ":memory:" {
			if s.deps.OutboundDialer != nil && !s.deps.OutboundDialer.AllowLoopbackForTests {
				return "", "", fmt.Errorf("in-memory sqlite is disabled in production setup")
			}
			return "sqlite", ":memory:", nil
		}
		// Allow existing data/ subfolder shorthand like "./data/volo.db" when DataDir already ends in "/data".
		if normRel := trimRedundantDataPrefix(s.deps.Config.DataDir, dsn); normRel != "" {
			dsn = normRel
		}
		cand, err := confineSQLitePathInDataDir(s.deps.Config.DataDir, dsn, true)
		if err != nil {
			return "", "", err
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
		if _, err := validateSetupDBEndpoint(ctx, t.host, t.port); err != nil {
			return "", "", err
		}
	}
	return driver, dsn, nil
}

func trimRedundantDataPrefix(dataDir, rawPath string) string {
	cleanRaw := filepath.ToSlash(strings.TrimSpace(rawPath))
	cleanRaw = strings.TrimPrefix(cleanRaw, "./")
	if strings.HasPrefix(cleanRaw, "data/") && strings.EqualFold(filepath.Base(filepath.Clean(dataDir)), "data") {
		return strings.TrimPrefix(cleanRaw, "data/")
	}
	return ""
}

func pinSetupDSNToValidatedIP(driver, dsn string, validIP netip.Addr, port int) string {
	if !validIP.IsValid() || port <= 0 {
		return dsn
	}
	ipStr := validIP.String()
	switch driver {
	case "mysql":
		if myCfg, err := mysql.ParseDSN(dsn); err == nil {
			myCfg.Addr = net.JoinHostPort(ipStr, strconv.Itoa(port))
			return myCfg.FormatDSN()
		}
	case "postgres":
		lower := strings.ToLower(strings.TrimSpace(dsn))
		if strings.HasPrefix(lower, "postgres://") || strings.HasPrefix(lower, "postgresql://") {
			if u, err := url.Parse(dsn); err == nil {
				q := u.Query()
				q.Set("hostaddr", ipStr)
				u.RawQuery = q.Encode()
				return u.String()
			}
		}
		if !strings.Contains(lower, "hostaddr=") {
			return strings.TrimSpace(dsn) + " hostaddr=" + ipStr
		}
	}
	return dsn
}

func (s *AppServer) isSameCurrentDB(driver, dsn string, simpleProto bool) bool {
	cfgDriver, cfgDSN, cfgSimple := s.deps.Config.GetDBSpec()
	curDriver := strings.ToLower(strings.TrimSpace(cfgDriver))
	if curDriver == "" {
		curDriver = s.deps.Store.Dialect().Name()
	}
	if driver != curDriver || simpleProto != cfgSimple {
		return false
	}
	if driver == "sqlite" {
		curAbs, err1 := filepath.Abs(strings.TrimSpace(cfgDSN))
		reqAbs, err2 := filepath.Abs(strings.TrimSpace(dsn))
		if err1 == nil && err2 == nil && filepath.Clean(curAbs) == filepath.Clean(reqAbs) {
			return true
		}
	}
	return strings.TrimSpace(dsn) == strings.TrimSpace(cfgDSN)
}

func (s *AppServer) probeSetupDB(ctx context.Context, driver, dsn string, simpleProto bool) (string, int64, error) {
	testCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	dialDSN := dsn
	if driver == "postgres" || driver == "mysql" {
		targets, err := extractSetupSQLTargets(driver, dsn)
		if err != nil {
			return "", 0, err
		}
		var primaryIP netip.Addr
		var primaryPort int
		for i, t := range targets {
			validIP, vErr := validateSetupDBEndpoint(testCtx, t.host, t.port)
			if vErr != nil {
				return "", 0, vErr
			}
			if i == 0 {
				primaryIP = validIP
				primaryPort = t.port
			}
		}
		if primaryIP.IsValid() && primaryPort > 0 {
			pinnedAddr := net.JoinHostPort(primaryIP.String(), strconv.Itoa(primaryPort))
			var d net.Dialer
			conn, dErr := d.DialContext(testCtx, "tcp", pinnedAddr)
			if dErr != nil {
				return "", 0, dErr
			}
			_ = conn.Close()
			dialDSN = pinSetupDSNToValidatedIP(driver, dsn, primaryIP, primaryPort)
		}
	}

	existedBefore := true
	var confinedSQLite string
	if driver == "sqlite" && dsn != ":memory:" {
		cPath, cErr := confineSQLitePathInDataDir(s.deps.Config.DataDir, dsn, true)
		if cErr != nil {
			return "", 0, cErr
		}
		confinedSQLite = cPath
		dialDSN = cPath
		if _, err := os.Stat(confinedSQLite); os.IsNotExist(err) {
			existedBefore = false
		}
	}
	cleanupSQLiteProbe := func() {
		if !existedBefore && confinedSQLite != "" {
			_ = os.Remove(confinedSQLite)
			_ = os.Remove(confinedSQLite + "-wal")
			_ = os.Remove(confinedSQLite + "-shm")
		}
	}

	start := time.Now()
	probeStore, err := sqlx.Open(testCtx, sqlx.Options{
		Driver:         driver,
		DSN:            dialDSN,
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
	cfgDriver, _, _ := s.deps.Config.GetDBSpec()
	if strings.TrimSpace(rawDriver) == "" {
		rawDriver = cfgDriver
	}
	driver, dsn, err := s.validateAndNormalizeSetupDB(ctx, rawDriver, rawDSN)
	if err != nil {
		return err
	}
	if s.isSameCurrentDB(driver, dsn, simpleProto) {
		return nil
	}

	openDSN := dsn
	if driver == "postgres" || driver == "mysql" {
		if targets, tErr := extractSetupSQLTargets(driver, dsn); tErr == nil && len(targets) > 0 {
			if validIP, vErr := validateSetupDBEndpoint(ctx, targets[0].host, targets[0].port); vErr == nil {
				openDSN = pinSetupDSNToValidatedIP(driver, dsn, validIP, targets[0].port)
			}
		}
	}

	newStore, err := sqlx.Open(ctx, sqlx.Options{
		Driver:         driver,
		DSN:            openDSN,
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

	s.deps.Config.SetDBSpec(driver, dsn, simpleProto)
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
	s.syncConfiguredUploadDir()
	_ = s.syncConfiguredGeoIPDir()
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
		_ = s.deps.Plugins.ReloadAll(ctx)
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
	mux.HandleFunc("/api/setup/configure-db", s.handleSetupConfigureDB)
	mux.HandleFunc("/api/setup/test-legacy-db", s.handleSetupTestLegacyDB)
	mux.HandleFunc("/api/setup/import-legacy", s.handleSetupImportLegacy)
	mux.HandleFunc("/api/setup/migrate-db", s.handleSetupMigrateDB)
	mux.HandleFunc("/api/setup/complete", s.handleSetupComplete)
}

func (s *AppServer) handleSetupSPA(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	if !s.requireSetupUnlocked(w, r) {
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
	if !s.requireSetupUnlocked(w, r) {
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
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
	if !s.requireSetupUnlocked(w, r) {
		return
	}
	if !s.requireSameOriginPost(w, r) {
		return
	}
	clientIP := s.clientIP(r)
	if !s.deps.WAF.AllowRate("setup:testdb", clientIP, 15, time.Minute) {
		writeErrJSON(w, http.StatusTooManyRequests, "rate limited")
		return
	}

	var req struct {
		Driver         string `json:"driver"`
		DSN            string `json:"dsn"`
		SimpleProtocol bool   `json:"simple_protocol"`
	}
	if !decodeJSONBody(w, r, 16384, &req) {
		return
	}

	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if !s.requireSetupUnlocked(w, r) {
		return
	}

	cfgDriver, cfgDSN, _ := s.deps.Config.GetDBSpec()
	rawDriver := strings.TrimSpace(req.Driver)
	rawDSN := strings.TrimSpace(req.DSN)
	if rawDriver == "" {
		rawDriver = cfgDriver
	}
	if rawDSN == "" && strings.EqualFold(rawDriver, cfgDriver) {
		rawDSN = cfgDSN
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

func (s *AppServer) handleSetupConfigureDB(w http.ResponseWriter, r *http.Request) {
	if !s.requireSetupUnlocked(w, r) {
		return
	}
	if !s.requireSameOriginPost(w, r) {
		return
	}
	clientIP := s.clientIP(r)
	if !s.deps.WAF.AllowRate("setup:configdb", clientIP, 10, time.Minute) {
		writeErrJSON(w, http.StatusTooManyRequests, "rate limited")
		return
	}

	var req struct {
		Driver         string `json:"driver"`
		DSN            string `json:"dsn"`
		SimpleProtocol *bool  `json:"simple_protocol"`
	}
	if !decodeJSONBody(w, r, 16384, &req) {
		return
	}

	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if !s.requireSetupUnlocked(w, r) {
		return
	}

	_, _, cfgSimple := s.deps.Config.GetDBSpec()
	simpleProto := cfgSimple
	if req.SimpleProtocol != nil {
		simpleProto = *req.SimpleProtocol
	}
	if err := s.applySetupDatabaseSelection(r.Context(), req.Driver, req.DSN, simpleProto); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"dialect": s.deps.Store.Dialect().Name(),
	})
}

func (s *AppServer) handleSetupImportLegacy(w http.ResponseWriter, r *http.Request) {
	if !s.requireSetupUnlocked(w, r) {
		return
	}
	if !s.requireSameOriginPost(w, r) {
		return
	}
	clientIP := s.clientIP(r)
	if !s.deps.WAF.AllowRate("setup:import", clientIP, 15, time.Minute) {
		writeErrJSON(w, http.StatusTooManyRequests, "rate limited")
		return
	}

	// Spool the multipart upload BEFORE acquiring s.setupMu so slow clients cannot block concurrent wizard calls.
	prep, err := s.spoolLegacyImportUpload(w, r)
	if writeBadErr(w, err) {
		return
	}
	defer os.Remove(prep.tmpPath)

	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if !s.requireSetupUnlocked(w, r) {
		return
	}

	rep, err := s.executeLegacyImport(r.Context(), prep, true)
	if writeBadErr(w, err) {
		return
	}
	if prep.dryRun {
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

	if prep.newSiteURL != "" {
		cleanURL := strings.TrimRight(prep.newSiteURL, "/")
		s.deps.Config.SetPublicURL(cleanURL)
		if s.deps.WAF != nil {
			s.deps.WAF.SetPublicURL(cleanURL)
		}
	}
	_, _ = s.deps.Settings.Reload(r.Context())
	setupSitePatch := map[string]any{"initialized": true}
	if prep.newSiteURL != "" {
		cleanURL := normalizeTrustedBaseURL(prep.newSiteURL)
		if cleanURL != "" {
			cv := make(map[string]string)
			for k, v := range s.deps.Settings.Snapshot().Site.CustomVars {
				cv[k] = v
			}
			if strings.TrimSpace(cv["public_url"]) == "" {
				cv["public_url"] = cleanURL
				setupSitePatch["custom_vars"] = cv
			}
		}
	}
	if patchBytes, mErr := json.Marshal(setupSitePatch); mErr == nil {
		_, _, _ = s.deps.Settings.PatchDomain(r.Context(), domain.SettingsDomainSite, patchBytes, 0)
	}
	s.reloadRuntimeAfterDBRestore(r.Context())

	resp := map[string]any{
		"ok":             true,
		"report":         rep,
		"admin_path":     s.deps.Config.AdminPath,
		"target_dialect": s.deps.Store.Dialect().Name(),
	}
	// Automatically sign in the primary imported administrator so entering /console works seamlessly!
	var adminUser domain.User
	if qErr := sqlx.GetInto(r.Context(), s.deps.Store, &adminUser,
		`SELECT * FROM users WHERE role = ? AND status = ? ORDER BY created_at ASC LIMIT 1`,
		domain.RoleAdmin, domain.UserStatusActive); qErr == nil && adminUser.ID != "" {
		geo := ""
		if s.deps.Geo != nil {
			geo = s.deps.Geo.LookupDisplay(clientIP)
		}
		if sess, rawTok, sErr := s.deps.Auth.CreateSession(r.Context(), adminUser.ID, clientIP, geo, r.UserAgent(), 0); sErr == nil {
			s.setSessionCookieForRequest(w, r, rawTok)
			adminUser.PasswordHash = ""
			adminUser.LegacyHash = ""
			resp["user"] = adminUser
			resp["csrf_token"] = s.deps.Auth.ComputeCSRFToken(sess.ID)
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *AppServer) handleSetupComplete(w http.ResponseWriter, r *http.Request) {
	if !s.requireSetupUnlocked(w, r) {
		return
	}
	if !s.requireSameOriginPost(w, r) {
		return
	}
	clientIP := s.clientIP(r)
	if !s.deps.WAF.AllowRate("setup:complete", clientIP, 10, time.Minute) {
		writeErrJSON(w, http.StatusTooManyRequests, "rate limited")
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
		PublicURL        string `json:"public_url"`
		SiteURL          string `json:"site_url"`
		DefaultLocale    string `json:"default_locale"`
		TimeZone         string `json:"timezone"`
		Accent           string `json:"accent"`
		AdminUsername    string `json:"admin_username"`
		AdminEmail       string `json:"admin_email"`
		AdminDisplayName string `json:"admin_display_name"`
		AdminPassword    string `json:"admin_password"`
	}
	if !decodeJSONBody(w, r, 32768, &req) {
		return
	}

	uname := strings.TrimSpace(req.AdminUsername)
	email := strings.ToLower(strings.TrimSpace(req.AdminEmail))
	if writeBadErr(w, auth.ValidateUsername(uname)) {
		return
	}
	if !strings.Contains(email, "@") || len(email) > 190 {
		writeErrJSON(w, http.StatusBadRequest, "valid admin_email is required")
		return
	}
	if writeBadErr(w, auth.ValidatePasswordPolicy(req.AdminPassword)) {
		return
	}
	rawPubURL := strings.TrimSpace(req.PublicURL)
	if rawPubURL == "" {
		rawPubURL = strings.TrimSpace(req.SiteURL)
	}
	normPublicURL := ""
	if rawPubURL != "" {
		normPublicURL = normalizeTrustedBaseURL(rawPubURL)
		if normPublicURL == "" {
			writeErrJSON(w, http.StatusBadRequest, "public_url must be a valid http(s):// URL")
			return
		}
	}
	pwHash, err := auth.HashPassword(req.AdminPassword)
	if err != nil {
		writeErrJSON(w, http.StatusInternalServerError, "password hashing failed")
		return
	}

	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if !s.requireSetupUnlocked(w, r) {
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
	_, _, cfgSimpleProto := s.deps.Config.GetDBSpec()
	chosenSimpleProto := cfgSimpleProto
	if req.SimpleProtocol != nil {
		chosenSimpleProto = *req.SimpleProtocol
	} else if req.DBSimpleProtocol != nil {
		chosenSimpleProto = *req.DBSimpleProtocol
	}
	if writeBadErr(w, s.applySetupDatabaseSelection(r.Context(), chosenDriver, chosenDSN, chosenSimpleProto)) {
		return
	}

	locale := i18n.NormalizeLocale(req.DefaultLocale)
	if strings.EqualFold(locale, "ja") {
		locale = "ja-JP"
	}
	if locale == "" {
		locale = s.deps.Catalog.Match("", "")
	} else if matched := s.deps.Catalog.Match(locale, ""); matched != "" &&
		strings.EqualFold(strings.Split(matched, "-")[0], strings.Split(locale, "-")[0]) &&
		!strings.EqualFold(locale, "zh-TW") && !strings.EqualFold(locale, "zh-HK") && !strings.HasPrefix(strings.ToLower(locale), "zh-hant") {
		locale = matched
	}
	locLower := strings.ToLower(strings.TrimSpace(locale))
	isTW := strings.Contains(locLower, "tw") || strings.Contains(locLower, "hk") || strings.Contains(locLower, "hant")
	isJA := strings.HasPrefix(locLower, "ja")
	isZH := strings.HasPrefix(locLower, "zh") && !isTW
	displayName := strings.TrimSpace(req.AdminDisplayName)
	if displayName == "" {
		displayName = uname
	}
	siteTitle := strings.TrimSpace(req.SiteTitle)
	if siteTitle == "" {
		switch {
		case isTW:
			siteTitle = "Volo 隨筆"
		case isZH:
			siteTitle = "Volo 随笔"
		case isJA:
			siteTitle = "Volo ノート"
		default:
			siteTitle = "Volo Blog"
		}
	}
	siteSubtitle := strings.TrimSpace(req.SiteSubtitle)
	if siteSubtitle == "" {
		switch {
		case isTW:
			siteSubtitle = "記錄 · 思考 · 分享"
		case isZH:
			siteSubtitle = "记录 · 思考 · 分享"
		case isJA:
			siteSubtitle = "記録 · 思索 · 共有"
		default:
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
			switch {
			case isTW:
				catName = "隨筆"
				catDesc = "獨立思考與工程手記"
			case isZH:
				catName = "随笔"
				catDesc = "独立思考与工程手记"
			case isJA:
				catName = "ノート"
				catDesc = "思考とエンジニアリングの記録"
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
			switch {
			case isTW:
				aboutTitle = "關於"
				aboutBody = "# 關於\n\n歡迎來到 **" + siteTitle + "**，本站由純 Go 單執行檔獨立部落格引擎 Volo-go 驅動。"
			case isZH:
				aboutTitle = "关于"
				aboutBody = "# 关于\n\n欢迎来到 **" + siteTitle + "**，本站由纯 Go 单文件独立博客引擎 Volo-go 驱动。"
			case isJA:
				aboutTitle = "概要"
				aboutBody = "# 概要\n\n**" + siteTitle + "** へようこそ。このサイトはシングルバイナリ Go ブログエンジン Volo-go で構築されています。"
			}
			_, _ = tx.ExecContext(r.Context(),
				`INSERT INTO pages (id, author_id, title, slug, content_md, is_published, allow_comment, show_toc, lang, translation_group, view_count, comment_count, created_at, updated_at)
				 VALUES (?, ?, ?, 'about', ?, ?, ?, ?, ?, '', 0, 0, ?, ?)`,
				pageID, adminUser.ID, aboutTitle, aboutBody, true, true, false, locale, now, now)
			_, _ = tx.ExecContext(r.Context(),
				`INSERT INTO nav_items (id, parent_id, title, i18n_titles_json, url, open_in_new_tab, icon, sort_order, is_enabled, created_at, updated_at)
				 VALUES (?, '', ?, '{"zh-CN":"关于","zh-TW":"關於","en-US":"About","ja-JP":"概要"}', '/about', ?, '', 10, ?, ?, ?)`,
				id.Next(), aboutTitle, false, true, now, now)
		}
		return nil
	})
	if err != nil {
		writeErrJSON(w, http.StatusConflict, err.Error())
		return
	}

	sitePatchMap := map[string]any{
		"initialized":    true,
		"title":          siteTitle,
		"subtitle":       siteSubtitle,
		"description":    strings.TrimSpace(req.SiteDescription),
		"default_locale": locale,
		"timezone":       tz,
	}
	if normPublicURL != "" {
		sitePatchMap["custom_vars"] = map[string]string{"public_url": normPublicURL}
		s.deps.Config.SetPublicURL(normPublicURL)
		if s.deps.WAF != nil {
			s.deps.WAF.SetPublicURL(normPublicURL)
		}
	}
	sitePatch, _ := json.Marshal(sitePatchMap)
	_, _, _ = s.deps.Settings.PatchDomain(r.Context(), domain.SettingsDomainSite, sitePatch, 0)

	appPatch, _ := json.Marshal(map[string]any{"accent": accent})
	_, _, _ = s.deps.Settings.PatchDomain(r.Context(), domain.SettingsDomainAppearance, appPatch, 0)
	_ = s.deps.Content.WarmCaches(r.Context())

	geo := ""
	if s.deps.Geo != nil {
		geo = s.deps.Geo.LookupDisplay(clientIP)
	}
	sess, rawTok, err := s.deps.Auth.CreateSession(r.Context(), adminUser.ID, clientIP, geo, r.UserAgent(), 0)
	if writeServerErr(w, err) {
		return
	}
	s.setSessionCookieForRequest(w, r, rawTok)
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

type preparedLegacyImport struct {
	tmpPath           string
	dryRun            bool
	rawDriver         string
	rawDSN            string
	simpleProto       bool
	newSiteURL        string
	oldURLPrefixes    []string
	siteTitle         string
	siteSubtitle      string
	defaultLocale     string
	timeZone          string
	accent            string
	forceLocalStorage bool
	adminUsername     string
	adminEmail        string
	adminDisplayName  string
	adminPasswordHash string
	currentPassword   string
}

func (s *AppServer) spoolLegacyImportUpload(w http.ResponseWriter, r *http.Request) (*preparedLegacyImport, error) {
	const maxLegacyBytes = 256 << 20 // 256 MiB streaming cap
	spooled, err := streamMultipartSingleFile(w, r, "file", maxLegacyBytes, s.ensureTempDir(), "volo-legacy-", ".zip", ".zip", ".json")
	if err != nil {
		return nil, err
	}
	cleanupOnErr := true
	defer func() {
		if cleanupOnErr {
			_ = os.Remove(spooled.TempPath)
		}
	}()

	formVal := func(key string) string {
		if spooled.FormFields == nil {
			return ""
		}
		return spooled.FormFields[key]
	}

	dryRaw := strings.TrimSpace(r.URL.Query().Get("dry_run"))
	if dryRaw == "" {
		dryRaw = strings.TrimSpace(formVal("dry_run"))
	}
	dryRun := strings.EqualFold(dryRaw, "true") || dryRaw == "1"

	rawDriver := strings.TrimSpace(formVal("driver"))
	if rawDriver == "" {
		rawDriver = strings.TrimSpace(formVal("db_driver"))
	}
	rawDSN := strings.TrimSpace(formVal("dsn"))
	if rawDSN == "" {
		rawDSN = strings.TrimSpace(formVal("db_dsn"))
	}
	simpleRaw := strings.TrimSpace(formVal("simple_protocol"))
	if simpleRaw == "" {
		simpleRaw = strings.TrimSpace(formVal("db_simple_protocol"))
	}
	_, _, cfgSimple := s.deps.Config.GetDBSpec()
	simpleProto := parseSetupBoolField(simpleRaw, cfgSimple)

	newSiteURL := strings.TrimSpace(formVal("new_site_url"))
	if newSiteURL != "" {
		parsedURL, uErr := url.Parse(newSiteURL)
		if uErr != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || strings.TrimSpace(parsedURL.Host) == "" {
			return nil, fmt.Errorf("new_site_url must be a valid http(s):// URL")
		}
		newSiteURL = strings.TrimRight(newSiteURL, "/")
	}

	adminUsername := strings.TrimSpace(formVal("admin_username"))
	if adminUsername != "" {
		if err := auth.ValidateUsername(adminUsername); err != nil {
			return nil, err
		}
	}
	adminEmail := strings.ToLower(strings.TrimSpace(formVal("admin_email")))
	if adminEmail != "" && (!strings.Contains(adminEmail, "@") || len(adminEmail) > 190) {
		return nil, fmt.Errorf("valid admin_email is required")
	}
	adminDisplayName := strings.TrimSpace(formVal("admin_display_name"))
	adminPassword := formVal("admin_password")
	adminPasswordHash := ""
	if adminPassword != "" {
		if err := auth.ValidatePasswordPolicy(adminPassword); err != nil {
			return nil, err
		}
		h, hErr := auth.HashPassword(adminPassword)
		if hErr != nil {
			return nil, fmt.Errorf("password hashing failed")
		}
		adminPasswordHash = h
	}
	currentPassword := formVal("current_password")
	if currentPassword == "" {
		currentPassword = r.Header.Get("X-Volo-Current-Password")
	}
	if currentPassword == "" {
		currentPassword = r.URL.Query().Get("current_password")
	}

	storageMode := strings.ToLower(strings.TrimSpace(formVal("storage_mode")))
	cleanupOnErr = false
	return &preparedLegacyImport{
		tmpPath:           spooled.TempPath,
		dryRun:            dryRun,
		rawDriver:         rawDriver,
		rawDSN:            rawDSN,
		simpleProto:       simpleProto,
		newSiteURL:        newSiteURL,
		oldURLPrefixes:    splitSetupURLPrefixes(formVal("old_site_url")),
		siteTitle:         strings.TrimSpace(formVal("site_title")),
		siteSubtitle:      strings.TrimSpace(formVal("site_subtitle")),
		defaultLocale:     strings.TrimSpace(formVal("default_locale")),
		timeZone:          strings.TrimSpace(formVal("timezone")),
		accent:            strings.TrimSpace(formVal("accent")),
		forceLocalStorage: storageMode == "local",
		adminUsername:     adminUsername,
		adminEmail:        adminEmail,
		adminDisplayName:  adminDisplayName,
		adminPasswordHash: adminPasswordHash,
		currentPassword:   currentPassword,
	}, nil
}

func (s *AppServer) executeLegacyImport(ctx context.Context, prep *preparedLegacyImport, allowDBSwap bool) (*etl.ImportReport, error) {
	var prevalidatedDialect string
	if allowDBSwap && prep.dryRun {
		if prep.rawDriver != "" || prep.rawDSN != "" {
			cfgDriver, _, _ := s.deps.Config.GetDBSpec()
			probeDriver := prep.rawDriver
			if probeDriver == "" {
				probeDriver = cfgDriver
			}
			normDriver, normDSN, vErr := s.validateAndNormalizeSetupDB(ctx, probeDriver, prep.rawDSN)
			if vErr != nil {
				return nil, vErr
			}
			prevalidatedDialect = normDriver
			if !s.isSameCurrentDB(normDriver, normDSN, prep.simpleProto) {
				if dName, _, pErr := s.probeSetupDB(ctx, normDriver, normDSN, prep.simpleProto); pErr != nil {
					return nil, pErr
				} else if dName != "" {
					prevalidatedDialect = dName
				}
			}
		}
	}

	if allowDBSwap && !prep.dryRun {
		if err := s.applySetupDatabaseSelection(ctx, prep.rawDriver, prep.rawDSN, prep.simpleProto); err != nil {
			return nil, err
		}
	}

	opts := etl.ImportOptions{
		DryRun:            prep.dryRun,
		TargetUploadDir:   s.deps.Config.GetUploadDir(),
		AdminPath:         s.deps.Config.AdminPath,
		OldURLPrefixes:    prep.oldURLPrefixes,
		NewSiteURL:        prep.newSiteURL,
		SiteTitle:         prep.siteTitle,
		SiteSubtitle:      prep.siteSubtitle,
		DefaultLocale:     prep.defaultLocale,
		TimeZone:          prep.timeZone,
		Accent:            prep.accent,
		ForceLocalStorage: prep.forceLocalStorage,
		AdminUsername:          prep.adminUsername,
		AdminEmail:             prep.adminEmail,
		AdminDisplayName:       prep.adminDisplayName,
		AdminPasswordHash:      prep.adminPasswordHash,
		PreserveExistingAdmins: !allowDBSwap,
	}

	rep, err := etl.ImportLegacyArchive(ctx, s.deps.Store, s.deps.Settings, s.deps.Config.KeyRing, prep.tmpPath, opts)
	if err != nil {
		return nil, err
	}
	if prep.dryRun && prevalidatedDialect != "" && rep != nil {
		rep.TargetDialect = prevalidatedDialect
	}
	return rep, nil
}

func (s *AppServer) streamLegacyImportUpload(w http.ResponseWriter, r *http.Request, allowDBSwap bool) (*etl.ImportReport, bool, string, error) {
	prep, err := s.spoolLegacyImportUpload(w, r)
	if err != nil {
		return nil, false, "", err
	}
	defer os.Remove(prep.tmpPath)
	if !allowDBSwap {
		prep.rawDriver = ""
		prep.rawDSN = ""
		prep.adminUsername = ""
		prep.adminEmail = ""
		prep.adminDisplayName = ""
		prep.adminPasswordHash = ""
		prep.newSiteURL = ""
	}
	rep, err := s.executeLegacyImport(r.Context(), prep, allowDBSwap)
	if err != nil {
		return nil, prep.dryRun, "", err
	}
	return rep, prep.dryRun, prep.newSiteURL, nil
}

func validateSourceTablePrefix(raw string) (string, error) {
	pfx := strings.TrimSpace(raw)
	if pfx == "" {
		return "", nil
	}
	if len(pfx) > 64 {
		return "", fmt.Errorf("source_table_prefix must be at most 64 characters")
	}
	for _, ch := range pfx {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_') {
			return "", fmt.Errorf("source_table_prefix may only contain letters, digits, and underscores")
		}
	}
	return pfx, nil
}

func (s *AppServer) validateAndOpenSourceDBReadOnly(ctx context.Context, rawDriver, rawDSN string, simpleProto bool) (*sqlx.Store, string, string, error) {
	driver := strings.ToLower(strings.TrimSpace(rawDriver))
	switch driver {
	case "", "sqlite", "sqlite3":
		driver = "sqlite"
	case "postgres", "postgresql", "pgx":
		driver = "postgres"
	case "mysql", "mariadb":
		driver = "mysql"
	default:
		return nil, "", "", fmt.Errorf("unsupported source database driver %q", rawDriver)
	}

	dsn := strings.TrimSpace(rawDSN)
	if dsn == "" || strings.ContainsRune(dsn, 0) {
		return nil, "", "", fmt.Errorf("source database DSN is required")
	}

	if driver == "sqlite" {
		if dsn == ":memory:" {
			if s.deps.OutboundDialer != nil && !s.deps.OutboundDialer.AllowLoopbackForTests {
				return nil, "", "", fmt.Errorf("in-memory sqlite is disabled in production setup")
			}
			st, err := sqlx.OpenReadOnly(ctx, sqlx.Options{
				Driver:       "sqlite",
				DSN:          ":memory:",
				MaxOpenConns: 2,
				MaxIdleConns: 1,
			})
			if err != nil {
				return nil, "", "", err
			}
			return st, "sqlite", ":memory:", nil
		}
		if normRel := trimRedundantDataPrefix(s.deps.Config.DataDir, dsn); normRel != "" {
			dsn = normRel
		}
		confined, err := confineSQLitePathInDataDir(s.deps.Config.DataDir, dsn, false)
		if err != nil {
			return nil, "", "", err
		}
		st, err := sqlx.OpenReadOnly(ctx, sqlx.Options{
			Driver:       "sqlite",
			DSN:          confined,
			MaxOpenConns: 2,
			MaxIdleConns: 1,
		})
		if err != nil {
			return nil, "", "", err
		}
		return st, "sqlite", confined, nil
	}

	targets, err := extractSetupSQLTargets(driver, dsn)
	if err != nil {
		return nil, "", "", err
	}
	dnsCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var primaryIP netip.Addr
	var primaryPort int
	for i, t := range targets {
		validIP, vErr := validateSetupDBEndpoint(dnsCtx, t.host, t.port)
		if vErr != nil {
			return nil, "", "", vErr
		}
		if i == 0 {
			primaryIP = validIP
			primaryPort = t.port
		}
	}

	openDSN := pinSetupDSNToValidatedIP(driver, dsn, primaryIP, primaryPort)
	st, err := sqlx.OpenReadOnly(ctx, sqlx.Options{
		Driver:         driver,
		DSN:            openDSN,
		SimpleProtocol: simpleProto,
		MaxOpenConns:   2,
		MaxIdleConns:   1,
	})
	if err != nil {
		return nil, "", "", err
	}
	return st, driver, dsn, nil
}

func normalizeHostPortKey(addr string, defaultPort int) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	h := addr
	p := defaultPort
	if sh, sp, err := net.SplitHostPort(addr); err == nil {
		h = sh
		if parsed, pErr := strconv.Atoi(sp); pErr == nil && parsed > 0 {
			p = parsed
		}
	}
	h = strings.ToLower(strings.TrimSuffix(strings.Trim(strings.TrimSpace(h), "[]"), "."))
	return net.JoinHostPort(h, strconv.Itoa(p))
}

func isSameSetupAndSourceDB(targetDriver, targetDSN, sourceDriver, sourceDSN string) bool {
	if targetDriver != sourceDriver {
		return false
	}
	switch targetDriver {
	case "sqlite":
		if targetDSN == ":memory:" || sourceDSN == ":memory:" {
			return false
		}
		tClean := filepath.Clean(strings.TrimSpace(targetDSN))
		sClean := filepath.Clean(strings.TrimSpace(sourceDSN))
		return tClean == sClean

	case "postgres":
		tCfg, err1 := sqlx.ParsePostgresConfig(targetDSN)
		sCfg, err2 := sqlx.ParsePostgresConfig(sourceDSN)
		if err1 == nil && err2 == nil && tCfg != nil && sCfg != nil {
			tPort := int(tCfg.Port)
			if tPort <= 0 {
				tPort = 5432
			}
			sPort := int(sCfg.Port)
			if sPort <= 0 {
				sPort = 5432
			}
			tHost := strings.ToLower(strings.TrimSuffix(strings.Trim(strings.TrimSpace(tCfg.Host), "[]"), "."))
			sHost := strings.ToLower(strings.TrimSuffix(strings.Trim(strings.TrimSpace(sCfg.Host), "[]"), "."))
			tDB := strings.TrimSpace(tCfg.Database)
			if tDB == "" {
				tDB = strings.TrimSpace(tCfg.User)
			}
			sDB := strings.TrimSpace(sCfg.Database)
			if sDB == "" {
				sDB = strings.TrimSpace(sCfg.User)
			}
			if tHost == sHost && tPort == sPort && tDB == sDB {
				return true
			}
		}

	case "mysql":
		tCfg, err1 := mysql.ParseDSN(targetDSN)
		sCfg, err2 := mysql.ParseDSN(sourceDSN)
		if err1 == nil && err2 == nil {
			if normalizeHostPortKey(tCfg.Addr, 3306) == normalizeHostPortKey(sCfg.Addr, 3306) &&
				strings.EqualFold(strings.TrimSpace(tCfg.DBName), strings.TrimSpace(sCfg.DBName)) {
				return true
			}
		}
	}
	return strings.TrimSpace(targetDSN) == strings.TrimSpace(sourceDSN)
}

func (s *AppServer) handleSetupTestLegacyDB(w http.ResponseWriter, r *http.Request) {
	if !s.requireSetupUnlocked(w, r) {
		return
	}
	if !s.requireSameOriginPost(w, r) {
		return
	}
	clientIP := s.clientIP(r)
	if !s.deps.WAF.AllowRate("setup:testlegacydb", clientIP, 15, time.Minute) {
		writeErrJSON(w, http.StatusTooManyRequests, "rate limited")
		return
	}

	var req struct {
		SourceDriver         string `json:"source_driver"`
		SourceDSN            string `json:"source_dsn"`
		SourceSimpleProtocol bool   `json:"source_simple_protocol"`
		SourceTablePrefix    string `json:"source_table_prefix"`
	}
	if !decodeJSONBody(w, r, 16384, &req) {
		return
	}
	pfx, err := validateSourceTablePrefix(req.SourceTablePrefix)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if !s.requireSetupUnlocked(w, r) {
		return
	}

	testCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	start := time.Now()
	srcStore, _, _, err := s.validateAndOpenSourceDBReadOnly(testCtx, req.SourceDriver, req.SourceDSN, req.SourceSimpleProtocol)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer srcStore.Close()

	insp, err := etl.InspectSourceDatabase(testCtx, srcStore, pfx)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"dialect":      insp.Dialect,
		"schema_kind":  insp.SchemaKind,
		"table_prefix": insp.TablePrefix,
		"articles":     insp.Articles,
		"comments":     insp.Comments,
		"users":        insp.Users,
		"categories":   insp.Categories,
		"tags":         insp.Tags,
		"latency_ms":   time.Since(start).Milliseconds(),
	})
}

func (s *AppServer) handleSetupMigrateDB(w http.ResponseWriter, r *http.Request) {
	if !s.requireSetupUnlocked(w, r) {
		return
	}
	if !s.requireSameOriginPost(w, r) {
		return
	}
	clientIP := s.clientIP(r)
	if !s.deps.WAF.AllowRate("setup:migratedb", clientIP, 10, time.Minute) {
		writeErrJSON(w, http.StatusTooManyRequests, "rate limited")
		return
	}

	var req struct {
		Driver               string `json:"driver"`
		DBDriver             string `json:"db_driver"`
		DSN                  string `json:"dsn"`
		DBDSN                string `json:"db_dsn"`
		SimpleProtocol       *bool  `json:"simple_protocol"`
		DBSimpleProtocol     *bool  `json:"db_simple_protocol"`
		SourceDriver         string `json:"source_driver"`
		SourceDSN            string `json:"source_dsn"`
		SourceSimpleProtocol bool   `json:"source_simple_protocol"`
		SourceTablePrefix    string `json:"source_table_prefix"`
		OldSiteURL           string `json:"old_site_url"`
		NewSiteURL           string `json:"new_site_url"`
		SiteTitle            string `json:"site_title"`
		SiteSubtitle         string `json:"site_subtitle"`
		DefaultLocale        string `json:"default_locale"`
		TimeZone             string `json:"timezone"`
		Accent               string `json:"accent"`
		StorageMode          string `json:"storage_mode"`
		AdminUsername        string `json:"admin_username"`
		AdminEmail           string `json:"admin_email"`
		AdminDisplayName     string `json:"admin_display_name"`
		AdminPassword        string `json:"admin_password"`
		DryRun               bool   `json:"dry_run"`
	}
	if !decodeJSONBody(w, r, 32768, &req) {
		return
	}

	dryRaw := strings.TrimSpace(r.URL.Query().Get("dry_run"))
	dryRun := req.DryRun || strings.EqualFold(dryRaw, "true") || dryRaw == "1"

	pfx, err := validateSourceTablePrefix(req.SourceTablePrefix)
	if writeBadErr(w, err) {
		return
	}

	newSiteURL := strings.TrimSpace(req.NewSiteURL)
	if newSiteURL != "" {
		parsedURL, uErr := url.Parse(newSiteURL)
		if uErr != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || strings.TrimSpace(parsedURL.Host) == "" {
			writeErrJSON(w, http.StatusBadRequest, "new_site_url must be a valid http(s):// URL")
			return
		}
		newSiteURL = strings.TrimRight(newSiteURL, "/")
	}

	adminUsername := strings.TrimSpace(req.AdminUsername)
	if adminUsername != "" && writeBadErr(w, auth.ValidateUsername(adminUsername)) {
		return
	}
	adminEmail := strings.ToLower(strings.TrimSpace(req.AdminEmail))
	if adminEmail != "" && (!strings.Contains(adminEmail, "@") || len(adminEmail) > 190) {
		writeErrJSON(w, http.StatusBadRequest, "valid admin_email is required")
		return
	}
	adminDisplayName := strings.TrimSpace(req.AdminDisplayName)
	adminPasswordHash := ""
	if req.AdminPassword != "" {
		if writeBadErr(w, auth.ValidatePasswordPolicy(req.AdminPassword)) {
			return
		}
		h, hErr := auth.HashPassword(req.AdminPassword)
		if hErr != nil {
			writeErrJSON(w, http.StatusInternalServerError, "password hashing failed")
			return
		}
		adminPasswordHash = h
	}

	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if !s.requireSetupUnlocked(w, r) {
		return
	}

	cfgDriver, cfgDSN, cfgSimple := s.deps.Config.GetDBSpec()
	chosenDriver := strings.TrimSpace(req.Driver)
	if chosenDriver == "" {
		chosenDriver = strings.TrimSpace(req.DBDriver)
	}
	chosenDSN := strings.TrimSpace(req.DSN)
	if chosenDSN == "" {
		chosenDSN = strings.TrimSpace(req.DBDSN)
	}
	targetSimple := cfgSimple
	if req.SimpleProtocol != nil {
		targetSimple = *req.SimpleProtocol
	} else if req.DBSimpleProtocol != nil {
		targetSimple = *req.DBSimpleProtocol
	}

	effRawDriver := chosenDriver
	if effRawDriver == "" {
		effRawDriver = cfgDriver
	}
	effRawDSN := chosenDSN
	if effRawDSN == "" && strings.EqualFold(effRawDriver, cfgDriver) {
		effRawDSN = cfgDSN
	}
	normTargetDriver, normTargetDSN, err := s.validateAndNormalizeSetupDB(r.Context(), effRawDriver, effRawDSN)
	if writeBadErr(w, err) {
		return
	}

	srcStore, normSrcDriver, normSrcDSN, err := s.validateAndOpenSourceDBReadOnly(r.Context(), req.SourceDriver, req.SourceDSN, req.SourceSimpleProtocol)
	if writeBadErr(w, err) {
		return
	}
	defer srcStore.Close()

	if isSameSetupAndSourceDB(normTargetDriver, normTargetDSN, normSrcDriver, normSrcDSN) {
		writeErrJSON(w, http.StatusBadRequest, "source database and target database cannot be the same")
		return
	}

	// Pre-flight verify source schema BEFORE swapping or touching the target database!
	if _, err := etl.InspectSourceDatabase(r.Context(), srcStore, pfx); err != nil {
		writeErrJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	opts := etl.ImportOptions{
		DryRun:            dryRun,
		TargetUploadDir:   s.deps.Config.GetUploadDir(),
		AdminPath:         s.deps.Config.AdminPath,
		OldURLPrefixes:    splitSetupURLPrefixes(req.OldSiteURL),
		NewSiteURL:        newSiteURL,
		SiteTitle:         strings.TrimSpace(req.SiteTitle),
		SiteSubtitle:      strings.TrimSpace(req.SiteSubtitle),
		DefaultLocale:     strings.TrimSpace(req.DefaultLocale),
		TimeZone:          strings.TrimSpace(req.TimeZone),
		Accent:            strings.TrimSpace(req.Accent),
		ForceLocalStorage: strings.EqualFold(strings.TrimSpace(req.StorageMode), "local"),
		AdminUsername:     adminUsername,
		AdminEmail:        adminEmail,
		AdminDisplayName:  adminDisplayName,
		AdminPasswordHash: adminPasswordHash,
	}

	if dryRun {
		prevalidatedDialect := normTargetDriver
		if !s.isSameCurrentDB(normTargetDriver, normTargetDSN, targetSimple) {
			dName, _, pErr := s.probeSetupDB(r.Context(), normTargetDriver, normTargetDSN, targetSimple)
			if pErr != nil {
				writeErrJSON(w, http.StatusBadRequest, pErr.Error())
				return
			}
			if dName != "" {
				prevalidatedDialect = dName
			}
		}
		rep, insp, err := etl.ImportSourceDatabase(r.Context(), srcStore, s.deps.Store, s.deps.Settings, s.deps.Config.KeyRing, pfx, opts)
		if writeBadErr(w, err) {
			return
		}
		if rep != nil {
			rep.TargetDialect = prevalidatedDialect
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":             true,
			"dry_run":        true,
			"report":         rep,
			"source":         insp,
			"admin_path":     s.deps.Config.AdminPath,
			"target_dialect": prevalidatedDialect,
		})
		return
	}

	if writeBadErr(w, s.applySetupDatabaseSelection(r.Context(), chosenDriver, chosenDSN, targetSimple)) {
		return
	}

	rep, insp, err := etl.ImportSourceDatabase(r.Context(), srcStore, s.deps.Store, s.deps.Settings, s.deps.Config.KeyRing, pfx, opts)
	if writeBadErr(w, err) {
		return
	}

	if newSiteURL != "" {
		s.deps.Config.SetPublicURL(newSiteURL)
		if s.deps.WAF != nil {
			s.deps.WAF.SetPublicURL(newSiteURL)
		}
	}
	_, _ = s.deps.Settings.Reload(r.Context())
	_, _, _ = s.deps.Settings.PatchDomain(r.Context(), domain.SettingsDomainSite, []byte(`{"initialized":true}`), 0)
	s.reloadRuntimeAfterDBRestore(r.Context())

	resp := map[string]any{
		"ok":             true,
		"report":         rep,
		"source":         insp,
		"admin_path":     s.deps.Config.AdminPath,
		"target_dialect": s.deps.Store.Dialect().Name(),
	}
	if adminUser, qErr := sqlx.Get[domain.User](r.Context(), s.deps.Store,
		`SELECT * FROM users WHERE role = ? AND status = ? ORDER BY created_at ASC LIMIT 1`,
		domain.RoleAdmin, domain.UserStatusActive); qErr == nil && adminUser.ID != "" {
		geo := ""
		if s.deps.Geo != nil {
			geo = s.deps.Geo.LookupDisplay(clientIP)
		}
		if sess, rawTok, sErr := s.deps.Auth.CreateSession(r.Context(), adminUser.ID, clientIP, geo, r.UserAgent(), 0); sErr == nil {
			s.setSessionCookieForRequest(w, r, rawTok)
			adminUser.PasswordHash = ""
			adminUser.LegacyHash = ""
			resp["user"] = adminUser
			resp["csrf_token"] = s.deps.Auth.ComputeCSRFToken(sess.ID)
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

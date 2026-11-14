package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultListenAddr     = "0.0.0.0:8080"
	DefaultPublicURL      = "http://localhost:8080"
	DefaultAdminPath      = "/console"
	DefaultLocale         = "zh-CN"
	DefaultDataDir        = "./data"
	DefaultDBDriver       = "sqlite"
	DefaultMaxOpenConns   = 25
	DefaultMaxIdleConns   = 5
	DefaultConnLifetimeS  = 1800
	DefaultShutdownSec    = 15
	DefaultSocketMode     = "0660"
	DefaultUnixSocketPerm = os.FileMode(0o660)
	DefaultTLSMinVersion  = "1.2"
)

var (
	adminPathRegex = regexp.MustCompile(`^/[A-Za-z0-9][A-Za-z0-9_-]*(/[A-Za-z0-9][A-Za-z0-9_-]*)*$`)

	// Reserved top-level route segments that VOLO_ADMIN_PATH cannot claim when customized.
	reservedFirstSegments = map[string]struct{}{
		"api": {}, "apis": {}, "posts": {}, "articles": {}, "article": {},
		"pages": {}, "page": {}, "categories": {}, "category": {}, "tags": {},
		"archives": {}, "search": {}, "links": {}, "follows": {}, "follow": {},
		"series": {}, "books": {}, "uploads": {}, "upload": {}, "image": {},
		"images": {}, "assets": {}, "static": {}, "themes": {}, "skins": {},
		"plugins": {}, "avatars": {}, "github-stats": {}, "healthz": {},
		"rss.xml": {}, "atom.xml": {}, "podcast.xml": {}, "sitemap.xml": {},
		"robots.txt": {}, "llms.txt": {}, "llms-full.txt": {}, "favicon.ico": {},
		"setup": {}, "user": {}, "reader": {}, "logout": {},
	}
)

// CLIOverrides captures explicit CLI flags passed on the command line (Layer 1: highest precedence).
// Pointer fields distinguish "explicitly passed flag" (even if empty string or false) from "omitted".
type CLIOverrides struct {
	EnvFile            string
	SkipKeyRing        bool
	ListenAddr         *string
	ListenSocket       *string
	SocketGroup        *string
	SocketMode         *string
	PublicURL          *string
	TrustedProxies     *string
	AdminPath          *string
	SecretKey          *string
	GeoAccessEnabled   *bool
	DefaultLocale      *string
	DataDir            *string
	UploadDir          *string
	BackupDir          *string
	GeoIPDir           *string
	ThemesDir          *string
	PluginsDir         *string
	LocalesDir         *string
	DBDriver           *string
	DBDSN              *string
	DBSimpleProtocol   *bool
	ShutdownTimeoutSec *int
	// Native HTTPS/TLS serving (optional; typical reverse-proxy deployments leave these unset).
	TLSCertFile         *string
	TLSKeyFile          *string
	TLSMinVersion       *string
	TLSHTTPRedirectAddr *string
}

// ListenSocketSpec represents a parsed VOLO_LISTEN_SOCKET ("/path/to/volo.sock,0660").
type ListenSocketSpec struct {
	Path string
	Perm os.FileMode
}

// TrustedProxiesPolicy defines which direct peers are allowed to supply X-Real-IP.
type TrustedProxiesPolicy struct {
	TrustUnix bool
	Prefixes  []netip.Prefix
}

// Config holds the runtime bootstrap configuration resolved from the 4-layer hierarchy.
// Dynamic runtime updates (Setup Wizard, Legacy Import) synchronize via mu.
type Config struct {
	mu sync.RWMutex

	ListenAddr        string
	ListenSocket      *ListenSocketSpec
	SocketGroup       string
	SocketMode        string
	PublicURL         string
	PublicOrigin      string // scheme://authority
	TrustedProxiesRaw string
	TrustedProxies    TrustedProxiesPolicy
	AdminPath         string
	IsCustomAdminPath bool
	SecretKeySource   string
	KeyRing           *KeyRing
	GeoAccessEnabled  bool
	ShutdownTimeout   time.Duration
	DefaultLocale     string

	// Native HTTPS/TLS serving. TLS is enabled iff TLSCertFile and TLSKeyFile are both set.
	TLSCertFile         string
	TLSKeyFile          string
	TLSMinVersion       string // "1.2" (default) | "1.3"
	TLSHTTPRedirectAddr string // optional plain-HTTP redirect listener, e.g. ":80"

	DataDir          string
	UploadDir        string
	DefaultUploadDir string
	UploadDirPinned  bool
	BackupDir        string
	GeoIPDir         string
	DefaultGeoIPDir  string
	GeoIPDirPinned   bool
	ThemesDir        string
	PluginsDir       string
	LocalesDir       string

	DBDriver         string // sqlite | postgres | mysql
	DBDSN            string
	DBSimpleProtocol bool
	DBMaxOpenConns   int
	DBMaxIdleConns   int
	DBConnLifetime   time.Duration
}

// TLSEnabled reports whether native HTTPS serving is configured (both cert and key files set).
func (c *Config) TLSEnabled() bool {
	if c == nil {
		return false
	}
	return c.TLSCertFile != "" && c.TLSKeyFile != ""
}

// GetUploadDir returns the active local media upload directory in a thread-safe manner.
func (c *Config) GetUploadDir() string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if strings.TrimSpace(c.UploadDir) != "" {
		return c.UploadDir
	}
	return c.DefaultUploadDir
}

// GetDefaultUploadDir returns the bootstrap default local upload directory resolved at startup.
func (c *Config) GetDefaultUploadDir() string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if strings.TrimSpace(c.DefaultUploadDir) != "" {
		return c.DefaultUploadDir
	}
	return c.UploadDir
}

// IsUploadDirPinned reports whether UploadDir was explicitly pinned via CLI flag (--upload-dir).
// Directory precedence: CLI Flags (--upload-dir) > Admin Console DB Settings (storage.local_dir) > OS ENV / .env > Built-in Defaults.
func (c *Config) IsUploadDirPinned() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.UploadDirPinned
}

// SetUploadDir updates the active local upload directory (or resets to DefaultUploadDir when empty) under a write lock.
func (c *Config) SetUploadDir(dir string) {
	if c == nil {
		return
	}
	clean := strings.TrimSpace(dir)
	c.mu.Lock()
	if c.DefaultUploadDir == "" && c.UploadDir != "" {
		c.DefaultUploadDir = c.UploadDir
	}
	if clean == "" {
		c.UploadDir = c.DefaultUploadDir
	} else {
		c.UploadDir = filepath.Clean(clean)
	}
	c.mu.Unlock()
}

// GetGeoIPDir returns the active offline GeoIP directory or database file path in a thread-safe manner.
func (c *Config) GetGeoIPDir() string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if strings.TrimSpace(c.GeoIPDir) != "" {
		return c.GeoIPDir
	}
	return c.DefaultGeoIPDir
}

// GetDefaultGeoIPDir returns the bootstrap default GeoIP directory resolved at startup.
func (c *Config) GetDefaultGeoIPDir() string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if strings.TrimSpace(c.DefaultGeoIPDir) != "" {
		return c.DefaultGeoIPDir
	}
	return c.GeoIPDir
}

// IsGeoIPDirPinned reports whether GeoIPDir was explicitly pinned via CLI flag (--geoip-dir).
// Directory precedence: CLI Flags (--geoip-dir) > Admin Console DB Settings (security.geoip_dir) > OS ENV / .env > Built-in Defaults.
func (c *Config) IsGeoIPDirPinned() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.GeoIPDirPinned
}

// SetGeoIPDir updates the active GeoIP directory/path (or resets to DefaultGeoIPDir when empty) under a write lock.
func (c *Config) SetGeoIPDir(dir string) {
	if c == nil {
		return
	}
	clean := strings.TrimSpace(dir)
	c.mu.Lock()
	if c.DefaultGeoIPDir == "" && c.GeoIPDir != "" {
		c.DefaultGeoIPDir = c.GeoIPDir
	}
	if clean == "" {
		c.GeoIPDir = c.DefaultGeoIPDir
	} else {
		c.GeoIPDir = filepath.Clean(clean)
	}
	c.mu.Unlock()
}

func hasWindowsReservedDeviceName(normSlash string) bool {
	parts := strings.Split(normSlash, "/")
	for i, seg := range parts {
		if seg == "" {
			continue
		}
		if i == 0 && len(seg) == 2 && seg[1] == ':' && ((seg[0] >= 'a' && seg[0] <= 'z') || (seg[0] >= 'A' && seg[0] <= 'Z')) {
			continue
		}
		base := strings.TrimRight(seg, " .")
		if idx := strings.IndexAny(base, ".:"); idx >= 0 {
			base = base[:idx]
		}
		switch strings.ToUpper(strings.TrimSpace(base)) {
		case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$",
			"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
			"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
			return true
		}
	}
	return false
}

func checkCustomPathSystemRoots(absTarget string, fieldName string, allowUsrShareGeoIP bool) error {
	vol := filepath.VolumeName(absTarget)
	noVol := strings.ToLower(filepath.ToSlash(absTarget[len(vol):]))
	allowedSysGeoShare := allowUsrShareGeoIP && (noVol == "/usr/share/geoip" || strings.HasPrefix(noVol, "/usr/share/geoip/"))
	if !allowedSysGeoShare {
		for _, sysRoot := range []string{
			"/etc", "/proc", "/sys", "/dev", "/boot", "/root", "/bin", "/sbin", "/usr", "/lib", "/lib64", "/run", "/var/run",
		} {
			if noVol == sysRoot || strings.HasPrefix(noVol, sysRoot+"/") {
				return fmt.Errorf("config: %s cannot target system directory %q", fieldName, sysRoot)
			}
		}
	}

	lowTarget := strings.ToLower(filepath.ToSlash(filepath.Clean(absTarget)))
	winRoots := []string{
		"c:/windows", "c:/program files", "c:/program files (x86)", "c:/programdata",
	}
	for _, envKey := range []string{"SystemRoot", "windir", "ProgramFiles", "ProgramFiles(x86)", "ProgramData"} {
		if ev := strings.TrimSpace(os.Getenv(envKey)); ev != "" {
			norm := strings.TrimRight(strings.ToLower(filepath.ToSlash(filepath.Clean(ev))), "/")
			if norm != "" && norm != "." && norm != "/" {
				winRoots = append(winRoots, norm)
			}
		}
	}
	for _, winRoot := range winRoots {
		if lowTarget == winRoot || strings.HasPrefix(lowTarget, winRoot+"/") {
			return fmt.Errorf("config: %s cannot target system directory %q", fieldName, winRoot)
		}
	}
	return nil
}

// ValidateCustomGeoIPPath ensures a custom GeoIP directory or .mmdb/.xdb file path does not traverse "..",
// target sensitive OS roots, or overlap with public upload/protected workspace directories.
func (c *Config) ValidateCustomGeoIPPath(rawPath string) (string, error) {
	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" {
		return "", nil
	}
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] <= 0x1F || trimmed[i] == 0x7F {
			return "", errors.New("config: geoip_dir contains control characters")
		}
	}
	if strings.HasPrefix(trimmed, `\\`) {
		return "", errors.New("config: geoip_dir cannot use UNC network paths")
	}
	normSlash := strings.ReplaceAll(trimmed, `\`, "/")
	for _, seg := range strings.Split(normSlash, "/") {
		if seg == ".." {
			return "", errors.New("config: geoip_dir must not contain '..' segments")
		}
	}
	if hasWindowsReservedDeviceName(normSlash) {
		return "", errors.New("config: geoip_dir contains a reserved device name")
	}
	cleaned := filepath.Clean(trimmed)
	if strings.HasPrefix(cleaned, `\\`) || strings.HasPrefix(filepath.VolumeName(cleaned), `\\`) {
		return "", errors.New("config: geoip_dir cannot use UNC network paths")
	}
	if cleaned == "." || cleaned == ".." || cleaned == "/" || cleaned == `\` {
		return "", errors.New("config: geoip_dir must point to a dedicated GeoIP directory or .mmdb/.xdb file")
	}
	if !filepath.IsAbs(cleaned) {
		return "", errors.New("config: geoip_dir must be an explicit absolute path")
	}
	absTarget, err := filepath.Abs(cleaned)
	if err != nil {
		return "", fmt.Errorf("config: invalid geoip_dir: %w", err)
	}
	if filepath.Dir(absTarget) == absTarget {
		return "", errors.New("config: geoip_dir cannot be filesystem root")
	}
	if err := checkCustomPathSystemRoots(absTarget, "geoip_dir", true); err != nil {
		return "", err
	}

	// Pure lexical extension check (zero filesystem syscalls on user-supplied path strings;
	// symlink and regular-file verification is enforced at load time in GeoEngine.LoadDatabases via os.Lstat).
	lowBase := strings.ToLower(filepath.Base(cleaned))
	hasDBExt := strings.HasSuffix(lowBase, ".mmdb") || strings.HasSuffix(lowBase, ".xdb")
	if ext := strings.ToLower(filepath.Ext(lowBase)); ext != "" && !hasDBExt {
		switch ext {
		case ".db", ".sqlite", ".sqlite3", ".env", ".key", ".pem", ".crt", ".pub",
			".json", ".yaml", ".yml", ".toml", ".ini", ".conf", ".cfg",
			".zip", ".tar", ".gz", ".sql", ".log", ".txt", ".md", ".csv",
			".sh", ".bat", ".ps1", ".exe", ".html", ".js", ".css":
			return "", errors.New("config: geoip_dir file target must end with .mmdb or .xdb")
		}
	}

	var protected []string
	var uploadDirs []string
	if wd, wdErr := os.Getwd(); wdErr == nil && wd != "" {
		protected = append(protected, wd)
	}
	if c != nil {
		c.mu.RLock()
		protected = append(protected, c.DataDir, c.BackupDir, c.ThemesDir, c.PluginsDir, c.LocalesDir)
		uploadDirs = append(uploadDirs, c.UploadDir, c.DefaultUploadDir)
		c.mu.RUnlock()
	}
	targetCmp := filepath.Clean(absTarget)
	if os.PathSeparator == '\\' {
		targetCmp = strings.ToLower(targetCmp)
	}
	sep := string(filepath.Separator)
	for _, p := range protected {
		if strings.TrimSpace(p) == "" {
			continue
		}
		pAbs, aErr := filepath.Abs(p)
		if aErr != nil {
			continue
		}
		pCmp := filepath.Clean(pAbs)
		if os.PathSeparator == '\\' {
			pCmp = strings.ToLower(pCmp)
		}
		if targetCmp == pCmp || strings.HasPrefix(pCmp, targetCmp+sep) {
			return "", fmt.Errorf("config: geoip_dir cannot equal or enclose protected directory %q", p)
		}
	}
	for _, u := range uploadDirs {
		if strings.TrimSpace(u) == "" {
			continue
		}
		uAbs, aErr := filepath.Abs(u)
		if aErr != nil {
			continue
		}
		uCmp := filepath.Clean(uAbs)
		if os.PathSeparator == '\\' {
			uCmp = strings.ToLower(uCmp)
		}
		// Never place GeoIP databases inside or enclosing the public media upload directory!
		if targetCmp == uCmp || strings.HasPrefix(uCmp, targetCmp+sep) || strings.HasPrefix(targetCmp, uCmp+sep) {
			return "", fmt.Errorf("config: geoip_dir cannot overlap with upload directory %q", u)
		}
	}
	return cleaned, nil
}

// ValidateCustomUploadDir ensures a custom local media directory does not traverse "..", point to a
// filesystem/system root, or encompass sensitive workspace directories (working dir, DataDir/volo.db, BackupDir, ThemesDir, PluginsDir).
func (c *Config) ValidateCustomUploadDir(rawDir string) (string, error) {
	trimmed := strings.TrimSpace(rawDir)
	if trimmed == "" {
		return "", nil
	}
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] <= 0x1F || trimmed[i] == 0x7F {
			return "", errors.New("config: local_dir contains control characters")
		}
	}
	if strings.HasPrefix(trimmed, `\\`) {
		return "", errors.New("config: local_dir cannot use UNC network paths")
	}
	normSlash := strings.ReplaceAll(trimmed, `\`, "/")
	for _, seg := range strings.Split(normSlash, "/") {
		if seg == ".." {
			return "", errors.New("config: local_dir must not contain '..' segments")
		}
	}
	if hasWindowsReservedDeviceName(normSlash) {
		return "", errors.New("config: local_dir contains a reserved device name")
	}
	cleaned := filepath.Clean(trimmed)
	if strings.HasPrefix(cleaned, `\\`) || strings.HasPrefix(filepath.VolumeName(cleaned), `\\`) {
		return "", errors.New("config: local_dir cannot use UNC network paths")
	}
	if cleaned == "." || cleaned == ".." || cleaned == "/" || cleaned == `\` {
		return "", errors.New("config: local_dir must point to a dedicated media directory")
	}
	if !filepath.IsAbs(cleaned) {
		return "", errors.New("config: local_dir must be an explicit absolute path")
	}
	absTarget, err := filepath.Abs(cleaned)
	if err != nil {
		return "", fmt.Errorf("config: invalid local_dir: %w", err)
	}
	if filepath.Dir(absTarget) == absTarget {
		return "", errors.New("config: local_dir cannot be filesystem root")
	}
	if err := checkCustomPathSystemRoots(absTarget, "local_dir", false); err != nil {
		return "", err
	}

	var protected []string
	if wd, wdErr := os.Getwd(); wdErr == nil && wd != "" {
		protected = append(protected, wd)
	}
	if c != nil {
		c.mu.RLock()
		protected = append(protected, c.DataDir, c.BackupDir, c.ThemesDir, c.PluginsDir, c.GeoIPDir, c.DefaultGeoIPDir, c.LocalesDir)
		c.mu.RUnlock()
	}
	targetCmp := filepath.Clean(absTarget)
	if os.PathSeparator == '\\' {
		targetCmp = strings.ToLower(targetCmp)
	}
	for _, p := range protected {
		if strings.TrimSpace(p) == "" {
			continue
		}
		pAbs, aErr := filepath.Abs(p)
		if aErr != nil {
			continue
		}
		pCmp := filepath.Clean(pAbs)
		if os.PathSeparator == '\\' {
			pCmp = strings.ToLower(pCmp)
		}
		// Reject if custom upload dir equals a protected dir OR contains a protected dir inside it!
		if targetCmp == pCmp || strings.HasPrefix(pCmp, targetCmp+string(filepath.Separator)) {
			return "", fmt.Errorf("config: local_dir cannot equal or enclose protected directory %q", p)
		}
	}
	return cleaned, nil
}

// GetPublicURL returns the current canonical public URL in a thread-safe manner.
func (c *Config) GetPublicURL() string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.PublicURL
}

// SetPublicURL updates PublicURL (and PublicOrigin when valid) under a write lock.
func (c *Config) SetPublicURL(u string) {
	if c == nil {
		return
	}
	clean, origin, err := normalizePublicURL(u)
	if err != nil {
		clean = strings.TrimRight(strings.TrimSpace(u), "/")
		if parsed, pErr := url.Parse(clean); pErr == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" {
			origin = parsed.Scheme + "://" + parsed.Host
		} else {
			origin = ""
		}
	}
	c.mu.Lock()
	c.PublicURL = clean
	if origin != "" || clean != "" {
		if origin != "" || clean == "" {
			c.PublicOrigin = origin
		}
	}
	c.mu.Unlock()
}

// GetDBSpec returns the active database driver, DSN, and simple-protocol flag under a read lock.
func (c *Config) GetDBSpec() (driver, dsn string, simpleProto bool) {
	if c == nil {
		return "", "", false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.DBDriver, c.DBDSN, c.DBSimpleProtocol
}

// SetDBSpec updates the active database driver, DSN, and simple-protocol flag under a write lock.
func (c *Config) SetDBSpec(driver, dsn string, simpleProto bool) {
	if c == nil {
		return
	}
	normDriver := normalizeDriverName(driver)
	c.mu.Lock()
	c.DBDriver = normDriver
	c.DBDSN = dsn
	c.DBSimpleProtocol = simpleProto
	c.mu.Unlock()
}

// PersistedDBConfig allows the First-Run Setup Wizard to save chosen DB settings encrypted on disk
// when VOLO_DB_DSN was not locked via CLI/ENV/.env.
type PersistedDBConfig struct {
	Driver         string `json:"driver"`
	DSNSealed      string `json:"dsn_sealed"`
	SimpleProtocol bool   `json:"simple_protocol"`
}

// Load resolves configuration strictly in order:
// CLI Flags > OS Environment (VOLO_*) > .env File > Built-in Secure Defaults.
func Load(cli CLIOverrides) (*Config, error) {
	return LoadWithEnviron(cli, os.Environ())
}

// LoadWithEnviron is the testable core of Load accepting an explicit environment slice.
func LoadWithEnviron(cli CLIOverrides, environ []string) (*Config, error) {
	envMap := make(map[string]string, len(environ))
	for _, kv := range environ {
		if eq := strings.IndexByte(kv, '='); eq > 0 {
			envMap[kv[:eq]] = kv[eq+1:]
		}
	}

	envFilePath := cli.EnvFile
	if envFilePath == "" {
		if v, ok := envMap["VOLO_ENV_FILE"]; ok && strings.TrimSpace(v) != "" {
			envFilePath = strings.TrimSpace(v)
		} else {
			envFilePath = ".env"
		}
	}
	dotEnvMap, err := ParseDotEnvFile(envFilePath)
	if err != nil {
		return nil, err
	}

	resolveStr := func(cliVal *string, key string, def string) (string, bool) {
		if cliVal != nil {
			return strings.TrimSpace(*cliVal), true
		}
		if v, ok := envMap[key]; ok {
			return strings.TrimSpace(v), true
		}
		if v, ok := dotEnvMap[key]; ok {
			return strings.TrimSpace(v), true
		}
		return def, false
	}

	resolveBool := func(cliVal *bool, key string, def bool) (bool, error) {
		if cliVal != nil {
			return *cliVal, nil
		}
		if v, ok := envMap[key]; ok && strings.TrimSpace(v) != "" {
			return parseStrictBool(key, v)
		}
		if v, ok := dotEnvMap[key]; ok && strings.TrimSpace(v) != "" {
			return parseStrictBool(key, v)
		}
		return def, nil
	}

	resolveInt := func(cliVal *int, key string, def int, min int, max int) (int, error) {
		var n int
		if cliVal != nil {
			n = *cliVal
		} else if v, ok := envMap[key]; ok && strings.TrimSpace(v) != "" {
			parsed, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return 0, fmt.Errorf("config: %s must be an integer: %w", key, err)
			}
			n = parsed
		} else if v, ok := dotEnvMap[key]; ok && strings.TrimSpace(v) != "" {
			parsed, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return 0, fmt.Errorf("config: %s must be an integer: %w", key, err)
			}
			n = parsed
		} else {
			n = def
		}
		if n < min || n > max {
			return 0, fmt.Errorf("config: %s=%d out of valid range [%d, %d]", key, n, min, max)
		}
		return n, nil
	}

	dataDir, _ := resolveStr(cli.DataDir, "VOLO_DATA_DIR", DefaultDataDir)
	if dataDir == "" {
		dataDir = DefaultDataDir
	}
	uploadDir, _ := resolveStr(cli.UploadDir, "VOLO_UPLOAD_DIR", filepath.Join(dataDir, "uploads"))
	uploadDirPinned := cli.UploadDir != nil && strings.TrimSpace(*cli.UploadDir) != ""
	if uploadDir == "" {
		uploadDir = filepath.Join(dataDir, "uploads")
	}
	backupDir, _ := resolveStr(cli.BackupDir, "VOLO_BACKUP_DIR", filepath.Join(dataDir, "backups"))
	if backupDir == "" {
		backupDir = filepath.Join(dataDir, "backups")
	}
	geoIPDir, _ := resolveStr(cli.GeoIPDir, "VOLO_GEOIP_DIR", filepath.Join(dataDir, "geoip"))
	geoIPDirPinned := cli.GeoIPDir != nil && strings.TrimSpace(*cli.GeoIPDir) != ""
	if geoIPDir == "" {
		geoIPDir = filepath.Join(dataDir, "geoip")
	}
	themesDir, _ := resolveStr(cli.ThemesDir, "VOLO_THEMES_DIR", "./themes")
	pluginsDir, _ := resolveStr(cli.PluginsDir, "VOLO_PLUGINS_DIR", "./plugins")
	localesDir, _ := resolveStr(cli.LocalesDir, "VOLO_LOCALES_DIR", "./locales")

	listenAddr, listenAddrExplicit := resolveStr(cli.ListenAddr, "VOLO_LISTEN_ADDR", DefaultListenAddr)
	socketRaw, _ := resolveStr(cli.ListenSocket, "VOLO_LISTEN_SOCKET", "")
	if cli.ListenAddr != nil && cli.ListenSocket != nil && *cli.ListenAddr != "" && *cli.ListenSocket != "" {
		return nil, errors.New("config: --listen-addr and --listen-socket cannot be specified simultaneously")
	}

	socketGroup, _ := resolveStr(cli.SocketGroup, "VOLO_SOCKET_GROUP", "")
	socketModeRaw, _ := resolveStr(cli.SocketMode, "VOLO_SOCKET_MODE", DefaultSocketMode)
	if socketModeRaw == "" {
		socketModeRaw = DefaultSocketMode
	}
	socketModePerm, err := ParseSocketMode(socketModeRaw)
	if err != nil {
		return nil, err
	}

	socketModeLayer := 4
	if cli.SocketMode != nil && strings.TrimSpace(*cli.SocketMode) != "" {
		socketModeLayer = 1
	} else if v, ok := envMap["VOLO_SOCKET_MODE"]; ok && strings.TrimSpace(v) != "" {
		socketModeLayer = 2
	} else if v, ok := dotEnvMap["VOLO_SOCKET_MODE"]; ok && strings.TrimSpace(v) != "" {
		socketModeLayer = 3
	}

	hasInlineSocketPerm := func(raw string) bool {
		idx := strings.IndexByte(raw, ',')
		return idx >= 0 && strings.TrimSpace(raw[idx+1:]) != ""
	}
	inlinePermLayer := 5
	if cli.ListenSocket != nil && hasInlineSocketPerm(*cli.ListenSocket) {
		inlinePermLayer = 1
	} else if cli.ListenSocket == nil {
		if v, ok := envMap["VOLO_LISTEN_SOCKET"]; ok && hasInlineSocketPerm(v) {
			inlinePermLayer = 2
		} else if _, envSet := envMap["VOLO_LISTEN_SOCKET"]; !envSet {
			if v, ok := dotEnvMap["VOLO_LISTEN_SOCKET"]; ok && hasInlineSocketPerm(v) {
				inlinePermLayer = 3
			}
		}
	}

	var socketSpec *ListenSocketSpec
	socketModeFinal := fmt.Sprintf("%04o", socketModePerm)
	if socketRaw != "" {
		spec, err := ParseListenSocketSpec(socketRaw)
		if err != nil {
			return nil, err
		}
		useExplicitSocketMode := true
		if inlinePermLayer < socketModeLayer {
			useExplicitSocketMode = false
		} else if inlinePermLayer == socketModeLayer && inlinePermLayer > 1 && socketModePerm == DefaultUnixSocketPerm {
			useExplicitSocketMode = false
		}
		if useExplicitSocketMode {
			spec.Perm = socketModePerm
			socketModeFinal = fmt.Sprintf("%04o", socketModePerm)
		} else {
			socketModeFinal = fmt.Sprintf("%04o", spec.Perm)
		}
		socketSpec = spec
		_ = listenAddrExplicit
	} else if listenAddr == "" {
		listenAddr = DefaultListenAddr
	}

	pubURLRaw, _ := resolveStr(cli.PublicURL, "VOLO_PUBLIC_URL", DefaultPublicURL)
	pubURL, pubOrigin, err := normalizePublicURL(pubURLRaw)
	if err != nil {
		return nil, err
	}

	trustedRaw, trustedExplicit := resolveStr(cli.TrustedProxies, "VOLO_TRUSTED_PROXIES", "127.0.0.1,::1")
	trustedPolicy, err := ParseTrustedProxies(trustedRaw)
	if err != nil {
		return nil, err
	}
	if socketSpec != nil && !(trustedExplicit && trustedRaw == "") {
		trustedPolicy.TrustUnix = true
	}

	adminPathRaw, _ := resolveStr(cli.AdminPath, "VOLO_ADMIN_PATH", DefaultAdminPath)
	adminPath, isCustomAdmin, err := ValidateAdminPath(adminPathRaw)
	if err != nil {
		return nil, err
	}

	geoEnabled, err := resolveBool(cli.GeoAccessEnabled, "VOLO_GEO_ACCESS_ENABLED", true)
	if err != nil {
		return nil, err
	}
	shutdownSec, err := resolveInt(cli.ShutdownTimeoutSec, "VOLO_SHUTDOWN_TIMEOUT_SEC", DefaultShutdownSec, 1, 120)
	if err != nil {
		return nil, err
	}

	tlsCert, _ := resolveStr(cli.TLSCertFile, "VOLO_TLS_CERT_FILE", "")
	tlsKey, _ := resolveStr(cli.TLSKeyFile, "VOLO_TLS_KEY_FILE", "")
	tlsMinRaw, _ := resolveStr(cli.TLSMinVersion, "VOLO_TLS_MIN_VERSION", DefaultTLSMinVersion)
	tlsRedirect, _ := resolveStr(cli.TLSHTTPRedirectAddr, "VOLO_TLS_HTTP_REDIRECT_ADDR", "")
	tlsMin, err := normalizeTLSMinVersion(tlsMinRaw)
	if err != nil {
		return nil, err
	}
	if (tlsCert == "") != (tlsKey == "") {
		return nil, errors.New("config: VOLO_TLS_CERT_FILE (--tls-cert) and VOLO_TLS_KEY_FILE (--tls-key) must be set together")
	}
	tlsOn := tlsCert != "" && tlsKey != ""
	if tlsOn && socketSpec != nil {
		return nil, errors.New("config: native TLS (VOLO_TLS_CERT_FILE/VOLO_TLS_KEY_FILE) cannot be combined with a Unix socket listener (VOLO_LISTEN_SOCKET); terminate TLS at the reverse proxy instead")
	}
	if tlsRedirect != "" {
		if !tlsOn {
			return nil, errors.New("config: VOLO_TLS_HTTP_REDIRECT_ADDR (--tls-http-redirect-addr) requires native TLS (VOLO_TLS_CERT_FILE and VOLO_TLS_KEY_FILE)")
		}
		if _, _, err := net.SplitHostPort(tlsRedirect); err != nil {
			return nil, fmt.Errorf("config: invalid VOLO_TLS_HTTP_REDIRECT_ADDR %q (expected host:port such as \":80\"): %w", tlsRedirect, err)
		}
	}

	locale, _ := resolveStr(cli.DefaultLocale, "VOLO_DEFAULT_LOCALE", DefaultLocale)
	if locale == "" {
		locale = DefaultLocale
	}

	var keyRing *KeyRing
	var secretSource string
	if !cli.SkipKeyRing {
		secretExplicit, _ := resolveStr(cli.SecretKey, "VOLO_SECRET_KEY", "")
		keyRing, secretSource, err = LoadOrInitKeyRing(secretExplicit, dataDir)
		if err != nil {
			return nil, err
		}
	}

	dbDriver, driverExplicit := resolveStr(cli.DBDriver, "VOLO_DB_DRIVER", DefaultDBDriver)
	dbDSN, dsnExplicit := resolveStr(cli.DBDSN, "VOLO_DB_DSN", "")
	simpleProto, err := resolveBool(cli.DBSimpleProtocol, "VOLO_DB_SIMPLE_PROTOCOL", false)
	if err != nil {
		return nil, err
	}

	// If neither driver nor DSN was explicitly pinned via CLI/ENV/.env, check Setup Wizard's db.json.
	if !cli.SkipKeyRing && !driverExplicit && !dsnExplicit && keyRing != nil {
		if saved, err := LoadWizardDBConfig(dataDir, keyRing); err == nil && saved != nil {
			dbDriver = saved.Driver
			dbDSN = saved.DSN
			simpleProto = saved.SimpleProtocol
		}
	}

	dbDriver = normalizeDriverName(dbDriver)
	if dbDriver != "sqlite" && dbDriver != "postgres" && dbDriver != "mysql" {
		return nil, fmt.Errorf("config: unsupported VOLO_DB_DRIVER %q (must be sqlite, postgres, or mysql)", dbDriver)
	}
	if dbDriver == "sqlite" && dbDSN == "" {
		dbDSN = filepath.Join(dataDir, "volo.db")
	}

	maxOpen, err := resolveInt(nil, "VOLO_DB_MAX_OPEN_CONNS", DefaultMaxOpenConns, 1, 500)
	if err != nil {
		return nil, err
	}
	maxIdle, err := resolveInt(nil, "VOLO_DB_MAX_IDLE_CONNS", DefaultMaxIdleConns, 0, 500)
	if err != nil {
		return nil, err
	}
	connLifeSec, err := resolveInt(nil, "VOLO_DB_CONN_MAX_LIFETIME_SEC", DefaultConnLifetimeS, 10, 86400)
	if err != nil {
		return nil, err
	}

	return &Config{
		ListenAddr:          listenAddr,
		ListenSocket:        socketSpec,
		SocketGroup:         socketGroup,
		SocketMode:          socketModeFinal,
		PublicURL:           pubURL,
		PublicOrigin:        pubOrigin,
		TrustedProxiesRaw:   trustedRaw,
		TrustedProxies:      trustedPolicy,
		AdminPath:           adminPath,
		IsCustomAdminPath:   isCustomAdmin,
		SecretKeySource:     secretSource,
		KeyRing:             keyRing,
		GeoAccessEnabled:    geoEnabled,
		ShutdownTimeout:     time.Duration(shutdownSec) * time.Second,
		DefaultLocale:       locale,
		TLSCertFile:         tlsCert,
		TLSKeyFile:          tlsKey,
		TLSMinVersion:       tlsMin,
		TLSHTTPRedirectAddr: tlsRedirect,
		DataDir:             dataDir,
		UploadDir:           uploadDir,
		DefaultUploadDir:    uploadDir,
		UploadDirPinned:     uploadDirPinned,
		BackupDir:           backupDir,
		GeoIPDir:            geoIPDir,
		DefaultGeoIPDir:     geoIPDir,
		GeoIPDirPinned:      geoIPDirPinned,
		ThemesDir:           themesDir,
		PluginsDir:          pluginsDir,
		LocalesDir:          localesDir,
		DBDriver:            dbDriver,
		DBDSN:               dbDSN,
		DBSimpleProtocol:    simpleProto,
		DBMaxOpenConns:      maxOpen,
		DBMaxIdleConns:      maxIdle,
		DBConnLifetime:      time.Duration(connLifeSec) * time.Second,
	}, nil
}

// ParseSocketMode parses an octal Unix socket mode string such as "0660" or "660".
// Empty strings default to 0660; values outside (0, 0777] or non-octal characters fail fast.
func ParseSocketMode(modeStr string) (os.FileMode, error) {
	trimmed := strings.TrimSpace(modeStr)
	if trimmed == "" {
		return DefaultUnixSocketPerm, nil
	}
	clean := strings.TrimPrefix(trimmed, "0")
	if len(trimmed) < 3 || len(trimmed) > 4 || len(clean) > 3 {
		return 0, fmt.Errorf("config: invalid socket octal permission %q", modeStr)
	}
	parsed, err := strconv.ParseUint(trimmed, 8, 32)
	if err != nil || parsed == 0 || parsed > 0o777 {
		return 0, fmt.Errorf("config: invalid socket octal permission %q", modeStr)
	}
	return os.FileMode(parsed), nil
}

func unixSocketMaxPathBytes() int {
	switch runtime.GOOS {
	case "darwin", "freebsd", "openbsd", "netbsd":
		return 103
	default:
		return 107
	}
}

// ParseListenSocketSpec parses "/run/volo/volo.sock,0660" (matching bolo-solo's strict socket permission semantics).
// Missing or empty permission after a single comma defaults to 0660; extra commas or non-octal permissions fail fast.
func ParseListenSocketSpec(raw string) (*ListenSocketSpec, error) {
	parts := strings.Split(raw, ",")
	if len(parts) > 2 {
		return nil, fmt.Errorf("config: invalid VOLO_LISTEN_SOCKET %q: at most one comma allowed", raw)
	}
	sockPath := strings.TrimSpace(parts[0])
	if sockPath == "" {
		return nil, errors.New("config: VOLO_LISTEN_SOCKET path must not be empty")
	}
	maxBytes := unixSocketMaxPathBytes()
	if len(sockPath) > maxBytes {
		return nil, fmt.Errorf("config: VOLO_LISTEN_SOCKET path %q (%d bytes) exceeds %d-byte sun_path limit", sockPath, len(sockPath), maxBytes)
	}
	perm := DefaultUnixSocketPerm
	if len(parts) == 2 && strings.TrimSpace(parts[1]) != "" {
		parsedPerm, err := ParseSocketMode(parts[1])
		if err != nil {
			return nil, fmt.Errorf("config: invalid socket octal permission %q in VOLO_LISTEN_SOCKET", strings.TrimSpace(parts[1]))
		}
		perm = parsedPerm
	}
	return &ListenSocketSpec{Path: sockPath, Perm: perm}, nil
}

// ParseTrustedProxies parses comma-separated IPv4/IPv6 literals, CIDRs, or literal "unix".
func ParseTrustedProxies(raw string) (TrustedProxiesPolicy, error) {
	var pol TrustedProxiesPolicy
	if strings.TrimSpace(raw) == "" {
		return pol, nil
	}
	for _, token := range strings.Split(raw, ",") {
		t := strings.TrimSpace(token)
		if t == "" {
			continue
		}
		if strings.EqualFold(t, "unix") {
			pol.TrustUnix = true
			continue
		}
		if strings.Contains(t, "/") {
			pfx, err := netip.ParsePrefix(t)
			if err != nil {
				return pol, fmt.Errorf("config: invalid CIDR %q in VOLO_TRUSTED_PROXIES: %w", t, err)
			}
			pol.Prefixes = append(pol.Prefixes, pfx.Masked())
			continue
		}
		addr, err := netip.ParseAddr(t)
		if err != nil {
			return pol, fmt.Errorf("config: invalid IP %q in VOLO_TRUSTED_PROXIES: %w", t, err)
		}
		addr = addr.Unmap()
		pol.Prefixes = append(pol.Prefixes, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return pol, nil
}

// ValidateAdminPath enforces safe URL prefix formatting and blocks reserved system routes.
func ValidateAdminPath(raw string) (normalized string, isCustom bool, err error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == DefaultAdminPath {
		return DefaultAdminPath, false, nil
	}
	trimmed = strings.TrimRight(trimmed, "/")
	if len(trimmed) > 120 || !adminPathRegex.MatchString(trimmed) {
		return "", false, fmt.Errorf("config: invalid VOLO_ADMIN_PATH %q (must match /segment[/segment] with [A-Za-z0-9_-])", raw)
	}
	if trimmed == DefaultAdminPath {
		return DefaultAdminPath, false, nil
	}
	firstSeg := strings.Split(trimmed[1:], "/")[0]
	if strings.EqualFold(firstSeg, "admin") || strings.EqualFold(firstSeg, "login") || strings.EqualFold(firstSeg, "start") || strings.EqualFold(firstSeg, "root") {
		return "", false, fmt.Errorf("config: VOLO_ADMIN_PATH %q cannot use predictable legacy prefix %q", raw, firstSeg)
	}
	if _, bad := reservedFirstSegments[strings.ToLower(firstSeg)]; bad {
		return "", false, fmt.Errorf("config: VOLO_ADMIN_PATH %q collides with reserved system route /%s", raw, firstSeg)
	}
	return trimmed, true, nil
}

func normalizePublicURL(raw string) (cleanURL string, origin string, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", "", fmt.Errorf("config: invalid VOLO_PUBLIC_URL %q (must be absolute http:// or https:// URL)", raw)
	}
	u.RawQuery = ""
	u.Fragment = ""
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String(), u.Scheme + "://" + u.Host, nil
}

func normalizeDriverName(d string) string {
	switch strings.ToLower(strings.TrimSpace(d)) {
	case "", "sqlite", "sqlite3":
		return "sqlite"
	case "postgres", "postgresql", "pg", "pgx":
		return "postgres"
	case "mysql", "mariadb", "tidb":
		return "mysql"
	default:
		return strings.ToLower(strings.TrimSpace(d))
	}
}

// normalizeTLSMinVersion validates VOLO_TLS_MIN_VERSION ("1.2" default, or "1.3").
func normalizeTLSMinVersion(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	v = strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(v), "tls"), "v")
	switch v {
	case "":
		return DefaultTLSMinVersion, nil
	case "1.2", "1.3":
		return v, nil
	}
	return "", fmt.Errorf("config: unsupported VOLO_TLS_MIN_VERSION %q (must be 1.2 or 1.3)", raw)
}

func parseStrictBool(key string, raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "yes", "on":
		return true, nil
	case "false", "0", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("config: invalid boolean value %q for %s", raw, key)
	}
}

// WizardDBSelection holds decrypted Setup Wizard database settings.
type WizardDBSelection struct {
	Driver         string
	DSN            string
	SimpleProtocol bool
}

// SaveWizardDBConfig persists the Setup Wizard's chosen DB configuration with DSN encrypted via AES-256-GCM.
func SaveWizardDBConfig(dataDir string, kr *KeyRing, sel WizardDBSelection) error {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return err
	}
	sealed, err := kr.SealString(sel.DSN, "wizard:db.json:dsn")
	if err != nil {
		return err
	}
	doc := PersistedDBConfig{
		Driver:         normalizeDriverName(sel.Driver),
		DSNSealed:      sealed,
		SimpleProtocol: sel.SimpleProtocol,
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	target := filepath.Join(dataDir, "db.json")
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}

// LoadWizardDBConfig loads and decrypts ${dataDir}/db.json if present.
func LoadWizardDBConfig(dataDir string, kr *KeyRing) (*WizardDBSelection, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, "db.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var doc PersistedDBConfig
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	dsn, err := kr.OpenString(doc.DSNSealed, "wizard:db.json:dsn")
	if err != nil {
		return nil, err
	}
	return &WizardDBSelection{
		Driver:         normalizeDriverName(doc.Driver),
		DSN:            dsn,
		SimpleProtocol: doc.SimpleProtocol,
	}, nil
}

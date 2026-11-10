package sqlx

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	pgProtocolVersion30 uint32 = 196608   // 3.0 (0x00030000)
	pgSSLRequestCode    uint32 = 80877103 // (1234 << 16) | 5679
	pgCancelRequestCode uint32 = 80877102 // (1234 << 16) | 5678

	pgAuthOK                uint32 = 0
	pgAuthCleartextPassword uint32 = 3
	pgAuthMD5Password       uint32 = 5
	pgAuthSASL              uint32 = 10
	pgAuthSASLContinue      uint32 = 11
	pgAuthSASLFinal         uint32 = 12

	pgMaxPacketBytes = 256 * 1024 * 1024 // 256 MiB defensive upper bound
	pgDefaultHost    = "/var/run/postgresql"
	pgDefaultPort    = uint16(5432)

	pgOIDBool        uint32 = 16
	pgOIDBytea       uint32 = 17
	pgOIDInt8        uint32 = 20
	pgOIDInt2        uint32 = 21
	pgOIDInt4        uint32 = 23
	pgOIDFloat4      uint32 = 700
	pgOIDFloat8      uint32 = 701
	pgOIDDate        uint32 = 1082
	pgOIDTimestamp   uint32 = 1114
	pgOIDTimestamptz uint32 = 1184
)

func init() {
	sql.Register("volo_pgx", pgConnDriver{})
}

// PostgresFallbackConfig describes an additional failover host:port endpoint in a multi-host DSN.
type PostgresFallbackConfig struct {
	Host string
	Port uint16
}

// PostgresConfig holds parsed PostgreSQL connection parameters from either a URI or keyword/value DSN.
type PostgresConfig struct {
	Host      string
	Port      uint16
	User      string
	Password  string
	Database  string
	SSLMode   string
	Params    map[string]string
	Fallbacks []PostgresFallbackConfig
}

// Copy returns an independent deep copy of PostgresConfig.
func (c *PostgresConfig) Copy() *PostgresConfig {
	if c == nil {
		return nil
	}
	cp := *c
	cp.Params = make(map[string]string, len(c.Params))
	for k, v := range c.Params {
		cp.Params[k] = v
	}
	if len(c.Fallbacks) > 0 {
		cp.Fallbacks = make([]PostgresFallbackConfig, len(c.Fallbacks))
		copy(cp.Fallbacks, c.Fallbacks)
	}
	return &cp
}

type pgHostPort struct {
	host string
	port uint16
}

// ParsePostgresConfig parses both `postgres://` / `postgresql://` URLs (including percent-encoded
// credentials, `[::1]:5432` IPv6 literals, comma-separated multi-host failover lists, and query params)
// and libpq keyword/value DSN strings (`host=127.0.0.1 port=5432 user=postgres password='...' dbname=volo`).
func ParsePostgresConfig(dsn string) (*PostgresConfig, error) {
	if strings.IndexByte(dsn, 0) >= 0 {
		return nil, errors.New("sqlx: NUL byte forbidden in postgres DSN")
	}
	trimmed := strings.TrimSpace(dsn)
	if trimmed == "" {
		return nil, errors.New("sqlx: empty postgres DSN")
	}
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "postgres://") || strings.HasPrefix(lower, "postgresql://") {
		return parsePostgresURL(trimmed)
	}
	return parsePostgresKeywordDSN(trimmed)
}

func parsePostgresURL(raw string) (*PostgresConfig, error) {
	for i := 0; i < len(raw); i++ {
		if raw[i] <= 0x20 || raw[i] == 0x7f {
			return nil, errors.New("sqlx: whitespace or control character forbidden in postgres URL")
		}
	}
	if strings.IndexByte(raw, '#') >= 0 {
		return nil, errors.New("sqlx: unescaped '#' forbidden in postgres URL (encode '#' as %23)")
	}

	var afterScheme string
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "postgresql://") {
		afterScheme = raw[len("postgresql://"):]
	} else {
		afterScheme = raw[len("postgres://"):]
	}

	rest, rawQuery, hasQuery := strings.Cut(afterScheme, "?")
	cfg := &PostgresConfig{
		SSLMode: "prefer",
		Params:  make(map[string]string),
	}

	authority, rawPath, hasSlash := strings.Cut(rest, "/")
	if hasSlash && rawPath != "" {
		dbName, err := url.PathUnescape(rawPath)
		if err != nil {
			return nil, fmt.Errorf("sqlx: invalid percent-encoding in postgres database path: %w", err)
		}
		if strings.IndexByte(dbName, 0) >= 0 {
			return nil, errors.New("sqlx: NUL byte forbidden in postgres database name")
		}
		cfg.Database = dbName
	}

	if strings.Count(authority, "@") > 1 {
		return nil, errors.New("sqlx: unescaped '@' in postgres URL userinfo")
	}
	if atIdx := strings.IndexByte(authority, '@'); atIdx >= 0 {
		userInfo := authority[:atIdx]
		authority = authority[atIdx+1:]
		rawUser, rawPass, hasPass := strings.Cut(userInfo, ":")
		u, err := url.PathUnescape(rawUser)
		if err != nil {
			return nil, fmt.Errorf("sqlx: invalid percent-encoding in postgres username: %w", err)
		}
		if strings.IndexByte(u, 0) >= 0 {
			return nil, errors.New("sqlx: NUL byte forbidden in postgres username")
		}
		cfg.User = u
		if hasPass {
			p, err := url.PathUnescape(rawPass)
			if err != nil {
				return nil, fmt.Errorf("sqlx: invalid percent-encoding in postgres password: %w", err)
			}
			if strings.IndexByte(p, 0) >= 0 {
				return nil, errors.New("sqlx: NUL byte forbidden in postgres password")
			}
			cfg.Password = p
		}
	}

	var endpoints []pgHostPort
	if authority != "" {
		parts := strings.Split(authority, ",")
		endpoints = make([]pgHostPort, 0, len(parts))
		for _, part := range parts {
			if part == "" {
				return nil, errors.New("sqlx: empty host entry in multi-host postgres URL")
			}
			ep, err := parseURLHostPort(part, len(parts) > 1)
			if err != nil {
				return nil, err
			}
			endpoints = append(endpoints, ep)
		}
	}

	var queryHostSpec string
	var hasQueryHost bool
	var queryPortSpec string
	var hasQueryPort bool

	if hasQuery && rawQuery != "" {
		for _, seg := range strings.Split(rawQuery, "&") {
			if seg == "" {
				return nil, errors.New("sqlx: empty query parameter in postgres URL")
			}
			rk, rv, hasEq := strings.Cut(seg, "=")
			if !hasEq || rk == "" {
				return nil, fmt.Errorf("sqlx: malformed query parameter %q in postgres URL", seg)
			}
			k, err := url.QueryUnescape(rk)
			if err != nil {
				return nil, fmt.Errorf("sqlx: invalid query parameter key %q: %w", rk, err)
			}
			v, err := url.QueryUnescape(rv)
			if err != nil {
				return nil, fmt.Errorf("sqlx: invalid query parameter value for %q: %w", k, err)
			}
			if strings.IndexByte(k, 0) >= 0 || strings.IndexByte(v, 0) >= 0 {
				return nil, errors.New("sqlx: NUL byte forbidden in postgres query parameter")
			}
			kl := strings.ToLower(strings.TrimSpace(k))
			switch kl {
			case "host":
				queryHostSpec = v
				hasQueryHost = true
			case "port":
				queryPortSpec = v
				hasQueryPort = true
			case "user":
				cfg.User = v
			case "password":
				cfg.Password = v
			case "dbname":
				cfg.Database = v
			case "sslmode":
				mode, mErr := normalizeSSLMode(v)
				if mErr != nil {
					return nil, mErr
				}
				cfg.SSLMode = mode
			default:
				cfg.Params[kl] = v
			}
		}
	}

	if err := finalizeEndpointsAndParams(cfg, endpoints, queryHostSpec, hasQueryHost, queryPortSpec, hasQueryPort); err != nil {
		return nil, err
	}
	return cfg, nil
}

func parseURLHostPort(item string, disallowEmptyHost bool) (pgHostPort, error) {
	if strings.HasPrefix(item, "[") {
		closeIdx := strings.IndexByte(item, ']')
		if closeIdx < 0 {
			return pgHostPort{}, errors.New("sqlx: unclosed '[' in IPv6 postgres host")
		}
		rawIP := item[1:closeIdx]
		ipStr, err := url.PathUnescape(rawIP)
		if err != nil || ipStr == "" {
			return pgHostPort{}, fmt.Errorf("sqlx: invalid IPv6 host %q", item)
		}
		addrOnly, _, _ := strings.Cut(ipStr, "%")
		if parsed := net.ParseIP(addrOnly); parsed == nil || !strings.Contains(addrOnly, ":") {
			return pgHostPort{}, fmt.Errorf("sqlx: invalid IPv6 address %q in brackets", ipStr)
		}
		rem := item[closeIdx+1:]
		var port uint16
		if rem != "" {
			if !strings.HasPrefix(rem, ":") {
				return pgHostPort{}, fmt.Errorf("sqlx: unexpected trailing characters %q after IPv6 host", rem)
			}
			p, pErr := parseStrictPort(rem[1:])
			if pErr != nil {
				return pgHostPort{}, pErr
			}
			port = p
		}
		return pgHostPort{host: ipStr, port: port}, nil
	}

	if strings.Count(item, ":") > 1 {
		return pgHostPort{}, fmt.Errorf("sqlx: IPv6 address %q in postgres URL must be enclosed in brackets", item)
	}
	if colonIdx := strings.IndexByte(item, ':'); colonIdx >= 0 {
		rawH := item[:colonIdx]
		rawP := item[colonIdx+1:]
		p, err := parseStrictPort(rawP)
		if err != nil {
			return pgHostPort{}, err
		}
		h, err := url.PathUnescape(rawH)
		if err != nil {
			return pgHostPort{}, fmt.Errorf("sqlx: invalid percent-encoding in host %q: %w", rawH, err)
		}
		if strings.IndexByte(h, 0) >= 0 {
			return pgHostPort{}, errors.New("sqlx: NUL byte forbidden in postgres host")
		}
		if disallowEmptyHost && h == "" {
			return pgHostPort{}, errors.New("sqlx: empty host in multi-host postgres URL")
		}
		return pgHostPort{host: h, port: p}, nil
	}

	h, err := url.PathUnescape(item)
	if err != nil {
		return pgHostPort{}, fmt.Errorf("sqlx: invalid percent-encoding in host %q: %w", item, err)
	}
	if h == "" || strings.IndexByte(h, 0) >= 0 {
		return pgHostPort{}, errors.New("sqlx: invalid postgres host")
	}
	return pgHostPort{host: h}, nil
}

func parsePostgresKeywordDSN(dsn string) (*PostgresConfig, error) {
	cfg := &PostgresConfig{
		SSLMode: "prefer",
		Params:  make(map[string]string),
	}
	var hostSpec string
	var hasHost bool
	var portSpec string
	var hasPort bool
	pairCount := 0

	i := 0
	n := len(dsn)
	for i < n {
		for i < n && isPGConnSpace(dsn[i]) {
			i++
		}
		if i >= n {
			break
		}
		keyStart := i
		for i < n && isPGKeywordByte(dsn[i]) {
			i++
		}
		if i == keyStart {
			return nil, fmt.Errorf("sqlx: invalid postgres DSN syntax at offset %d", i)
		}
		key := strings.ToLower(dsn[keyStart:i])

		for i < n && isPGConnSpace(dsn[i]) {
			i++
		}
		if i >= n || dsn[i] != '=' {
			return nil, fmt.Errorf("sqlx: missing '=' after %q in postgres DSN", key)
		}
		i++ // consume '='
		for i < n && isPGConnSpace(dsn[i]) {
			i++
		}

		var val string
		if i < n && dsn[i] == '\'' {
			i++ // consume opening quote
			var vb strings.Builder
			closed := false
			for i < n {
				ch := dsn[i]
				if ch == '\\' {
					i++
					if i >= n {
						return nil, fmt.Errorf("sqlx: unterminated escape in quoted value for %q", key)
					}
					vb.WriteByte(dsn[i])
					i++
					continue
				}
				if ch == '\'' {
					i++
					closed = true
					break
				}
				vb.WriteByte(ch)
				i++
			}
			if !closed {
				return nil, fmt.Errorf("sqlx: unterminated single-quoted value for %q in postgres DSN", key)
			}
			if i < n && !isPGConnSpace(dsn[i]) {
				return nil, fmt.Errorf("sqlx: missing whitespace after quoted value for %q", key)
			}
			val = vb.String()
		} else {
			var vb strings.Builder
			for i < n && !isPGConnSpace(dsn[i]) {
				ch := dsn[i]
				if ch == '\'' {
					return nil, fmt.Errorf("sqlx: unexpected quote inside unquoted value for %q", key)
				}
				if ch == '\\' {
					i++
					if i >= n {
						return nil, fmt.Errorf("sqlx: trailing backslash in value for %q", key)
					}
					vb.WriteByte(dsn[i])
					i++
					continue
				}
				vb.WriteByte(ch)
				i++
			}
			val = vb.String()
		}

		pairCount++
		switch key {
		case "host":
			hostSpec = val
			hasHost = true
		case "port":
			portSpec = val
			hasPort = true
		case "user":
			cfg.User = val
		case "password":
			cfg.Password = val
		case "dbname":
			cfg.Database = val
		case "sslmode":
			mode, err := normalizeSSLMode(val)
			if err != nil {
				return nil, err
			}
			cfg.SSLMode = mode
		default:
			cfg.Params[key] = val
		}
	}

	if pairCount == 0 {
		return nil, errors.New("sqlx: empty postgres DSN")
	}
	if err := finalizeEndpointsAndParams(cfg, nil, hostSpec, hasHost, portSpec, hasPort); err != nil {
		return nil, err
	}
	return cfg, nil
}

func finalizeEndpointsAndParams(
	cfg *PostgresConfig,
	urlEndpoints []pgHostPort,
	hostSpec string,
	hasHostSpec bool,
	portSpec string,
	hasPortSpec bool,
) error {
	endpoints := urlEndpoints
	if hasHostSpec {
		if strings.TrimSpace(hostSpec) == "" {
			return errors.New("sqlx: empty host parameter in postgres DSN")
		}
		parts := strings.Split(hostSpec, ",")
		parsed := make([]pgHostPort, 0, len(parts))
		for _, rawPart := range parts {
			part := strings.TrimSpace(rawPart)
			if part == "" {
				return errors.New("sqlx: empty host entry in multi-host postgres DSN")
			}
			ep, err := parseKeywordHostItem(part)
			if err != nil {
				return err
			}
			parsed = append(parsed, ep)
		}
		if len(endpoints) == 0 {
			endpoints = parsed
		} else {
			if len(parsed) == 1 && len(endpoints) == 1 && endpoints[0].host == "" {
				endpoints[0].host = parsed[0].host
				if endpoints[0].port == 0 {
					endpoints[0].port = parsed[0].port
				}
			} else {
				endpoints = parsed
			}
		}
	}

	var explicitPorts []uint16
	if hasPortSpec {
		if strings.TrimSpace(portSpec) == "" {
			return errors.New("sqlx: empty port parameter in postgres DSN")
		}
		pParts := strings.Split(portSpec, ",")
		explicitPorts = make([]uint16, 0, len(pParts))
		for _, rp := range pParts {
			p, err := parseStrictPort(strings.TrimSpace(rp))
			if err != nil {
				return err
			}
			explicitPorts = append(explicitPorts, p)
		}
	}

	if len(endpoints) == 0 {
		endpoints = []pgHostPort{{host: pgDefaultHost}}
	} else if len(endpoints) == 1 && endpoints[0].host == "" {
		endpoints[0].host = pgDefaultHost
	}

	if len(explicitPorts) > 0 {
		if len(explicitPorts) != 1 && len(explicitPorts) != len(endpoints) {
			return fmt.Errorf("sqlx: postgres port count (%d) does not match host count (%d)", len(explicitPorts), len(endpoints))
		}
		for idx := range endpoints {
			if len(explicitPorts) == 1 {
				if endpoints[idx].port == 0 || hasPortSpec {
					endpoints[idx].port = explicitPorts[0]
				}
			} else {
				endpoints[idx].port = explicitPorts[idx]
			}
		}
	}

	defaultPort := pgDefaultPort
	if len(explicitPorts) == 1 {
		defaultPort = explicitPorts[0]
	}
	for idx := range endpoints {
		if endpoints[idx].port == 0 {
			endpoints[idx].port = defaultPort
		}
	}

	cfg.Host = endpoints[0].host
	cfg.Port = endpoints[0].port
	if len(endpoints) > 1 {
		cfg.Fallbacks = make([]PostgresFallbackConfig, len(endpoints)-1)
		for idx := 1; idx < len(endpoints); idx++ {
			cfg.Fallbacks[idx-1] = PostgresFallbackConfig{
				Host: endpoints[idx].host,
				Port: endpoints[idx].port,
			}
		}
	}

	if ct, ok := cfg.Params["connect_timeout"]; ok && strings.TrimSpace(ct) != "" {
		if _, err := strconv.Atoi(strings.TrimSpace(ct)); err != nil {
			return fmt.Errorf("sqlx: invalid connect_timeout %q: %w", ct, err)
		}
	}
	if ha, ok := cfg.Params["hostaddr"]; ok && strings.TrimSpace(ha) != "" {
		haParts := strings.Split(ha, ",")
		if len(haParts) != 1 && len(haParts) != len(endpoints) {
			return fmt.Errorf("sqlx: postgres hostaddr count (%d) does not match host count (%d)", len(haParts), len(endpoints))
		}
		for _, item := range haParts {
			ip := strings.TrimSpace(item)
			if ip == "" || net.ParseIP(ip) == nil {
				return fmt.Errorf("sqlx: invalid IP in postgres hostaddr %q", item)
			}
		}
	}
	return nil
}

func parseKeywordHostItem(part string) (pgHostPort, error) {
	if strings.HasPrefix(part, "[") {
		closeIdx := strings.IndexByte(part, ']')
		if closeIdx < 0 {
			return pgHostPort{}, errors.New("sqlx: unclosed '[' in IPv6 postgres host")
		}
		ipStr := part[1:closeIdx]
		addrOnly, _, _ := strings.Cut(ipStr, "%")
		if ipStr == "" || net.ParseIP(addrOnly) == nil || !strings.Contains(addrOnly, ":") {
			return pgHostPort{}, fmt.Errorf("sqlx: invalid IPv6 host %q", part)
		}
		rem := part[closeIdx+1:]
		if rem == "" {
			return pgHostPort{host: ipStr}, nil
		}
		if !strings.HasPrefix(rem, ":") {
			return pgHostPort{}, fmt.Errorf("sqlx: invalid host:port %q", part)
		}
		p, err := parseStrictPort(rem[1:])
		if err != nil {
			return pgHostPort{}, err
		}
		return pgHostPort{host: ipStr, port: p}, nil
	}
	if strings.Count(part, ":") == 1 {
		h, rawP, _ := strings.Cut(part, ":")
		if h == "" {
			return pgHostPort{}, fmt.Errorf("sqlx: empty host in %q", part)
		}
		p, err := parseStrictPort(rawP)
		if err != nil {
			return pgHostPort{}, err
		}
		return pgHostPort{host: h, port: p}, nil
	}
	return pgHostPort{host: part}, nil
}

func parseStrictPort(raw string) (uint16, error) {
	if raw == "" {
		return 0, errors.New("sqlx: empty postgres port")
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			return 0, fmt.Errorf("sqlx: invalid postgres port %q", raw)
		}
	}
	n, err := strconv.ParseUint(raw, 10, 16)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("sqlx: postgres port %q out of range (1..65535)", raw)
	}
	return uint16(n), nil
}

func normalizeSSLMode(raw string) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(raw))
	switch mode {
	case "", "prefer":
		return "prefer", nil
	case "disable", "allow", "require", "verify-ca", "verify-full":
		return mode, nil
	default:
		return "", fmt.Errorf("sqlx: invalid postgres sslmode %q", raw)
	}
}

func isPGConnSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}

func isPGKeywordByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}

var pgClientOnlyParams = map[string]struct{}{
	"host":                     {},
	"hostaddr":                 {},
	"port":                     {},
	"dbname":                   {},
	"user":                     {},
	"password":                 {},
	"passfile":                 {},
	"channel_binding":          {},
	"connect_timeout":          {},
	"sslmode":                  {},
	"sslcompression":           {},
	"sslcert":                  {},
	"sslkey":                   {},
	"sslpassword":              {},
	"sslrootcert":              {},
	"sslcrl":                   {},
	"sslcrldir":                {},
	"sslsni":                   {},
	"requirepeer":              {},
	"ssl_min_protocol_version": {},
	"ssl_max_protocol_version": {},
	"gssencmode":               {},
	"krbsrvname":               {},
	"gsslib":                   {},
	"gssdelegation":            {},
	"service":                  {},
	"target_session_attrs":     {},
	"load_balance_hosts":       {},
	"keepalives":               {},
	"keepalives_idle":          {},
	"keepalives_interval":      {},
	"keepalives_count":         {},
	"tcp_user_timeout":         {},
	"prefer_simple_protocol":   {},
	"default_query_exec_mode":  {},
}

func parsePGConnConfig(dsn string, defaultSimple bool) (*PostgresConfig, bool, error) {
	cfg, err := ParsePostgresConfig(dsn)
	if err != nil {
		return nil, false, err
	}
	simple := defaultSimple
	for k, v := range cfg.Params {
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "prefer_simple_protocol":
			val := strings.ToLower(strings.TrimSpace(v))
			if val == "true" || val == "1" {
				simple = true
			} else if val == "false" || val == "0" {
				simple = defaultSimple
			}
			delete(cfg.Params, k)
		case "default_query_exec_mode":
			if strings.EqualFold(strings.TrimSpace(v), "simple_protocol") {
				simple = true
			}
			delete(cfg.Params, k)
		}
	}
	return cfg, simple, nil
}

// pgWireError represents a PostgreSQL v3 ErrorResponse ('E') packet.
type pgWireError struct {
	Severity string
	Code     string
	Message  string
	Detail   string
	Hint     string
}

func (e *pgWireError) Error() string {
	var b strings.Builder
	if e.Severity != "" {
		b.WriteString(e.Severity)
		b.WriteString(": ")
	}
	b.WriteString(e.Message)
	if e.Code != "" {
		b.WriteString(" (SQLSTATE ")
		b.WriteString(e.Code)
		b.WriteByte(')')
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	return b.String()
}

type pgRetryableErr struct {
	err error
}

func (e pgRetryableErr) Error() string { return e.err.Error() }
func (e pgRetryableErr) Unwrap() error { return e.err }

func isPGSafeToRetry(err error) bool {
	var re pgRetryableErr
	return errors.As(err, &re)
}

func wrapPGErr(conn *pgWireConn, ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, driver.ErrBadConn) {
		return err
	}
	if (ctx == nil || ctx.Err() == nil) && (conn == nil || conn.IsClosed() || isPGSafeToRetry(err)) {
		return fmt.Errorf("%w: %v", driver.ErrBadConn, err)
	}
	return err
}

// pgConnConnector implements database/sql/driver.Connector directly over Pure-Go PostgreSQL v3 wire protocol.
// It uses unnamed extended-protocol P/B/D/E/S pipelining (PgBouncer / Supabase / Neon transaction-pooler safe)
// or explicit Simple Query Protocol with zero external library dependencies.
type pgConnConnector struct {
	cfg            *PostgresConfig
	simpleProtocol bool
}

type pgConnDriver struct{}

func (d pgConnDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.OpenConnector(name)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return conn.Connect(ctx)
}

func (d pgConnDriver) OpenConnector(name string) (driver.Connector, error) {
	cfg, simpleProto, err := parsePGConnConfig(name, false)
	if err != nil {
		return nil, err
	}
	return &pgConnConnector{
		cfg:            cfg,
		simpleProtocol: simpleProto,
	}, nil
}

func (c *pgConnConnector) Connect(ctx context.Context) (driver.Conn, error) {
	raw, err := connectPGWire(ctx, c.cfg.Copy())
	if err != nil {
		return nil, err
	}
	return &pgDriverConn{
		conn:           raw,
		simpleProtocol: c.simpleProtocol,
	}, nil
}

func (c *pgConnConnector) Driver() driver.Driver {
	return pgConnDriver{}
}

type pgFieldDesc struct {
	name string
	oid  uint32
}

type pgWireConn struct {
	rawNetConn  net.Conn // underlying TCP/Unix socket for goroutine-safe SetDeadline
	netConn     net.Conn // active stream (either rawNetConn or *tls.Conn)
	rd          *bufio.Reader
	wbuf        []byte
	readScratch []byte

	dialNet     string
	dialAddr    string
	processID   uint32
	secretKey   []byte
	paramStatus map[string]string
	txStatus    byte
	busy        bool
	closed      bool
}

func (c *pgWireConn) IsClosed() bool {
	return c == nil || c.closed
}

func (c *pgWireConn) IsBusy() bool {
	return c != nil && c.busy
}

func (c *pgWireConn) TxStatus() byte {
	if c == nil {
		return 0
	}
	return c.txStatus
}

func (c *pgWireConn) ParameterStatus(key string) string {
	if c == nil {
		return ""
	}
	return c.paramStatus[key]
}

func (c *pgWireConn) markBroken() {
	if c == nil || c.closed {
		return
	}
	c.closed = true
	c.busy = false
	_ = c.netConn.Close()
}

func (c *pgWireConn) Close() error {
	if c == nil || c.closed {
		return nil
	}
	c.closed = true
	c.busy = false
	_ = c.rawNetConn.SetDeadline(time.Now().Add(2 * time.Second))
	var term [5]byte
	term[0] = 'X'
	binary.BigEndian.PutUint32(term[1:5], 4)
	_, _ = c.netConn.Write(term[:])
	return c.netConn.Close()
}

type pgCancelGuard struct {
	conn *pgWireConn
	ctx  context.Context
	stop chan struct{}
	done chan struct{}
}

func (c *pgWireConn) startCancelGuard(ctx context.Context) *pgCancelGuard {
	if ctx == nil || ctx.Done() == nil {
		return nil
	}
	g := &pgCancelGuard{
		conn: c,
		ctx:  ctx,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	rawConn := c.rawNetConn
	dialNet := c.dialNet
	dialAddr := c.dialAddr
	pid := c.processID
	var secretCopy []byte
	if len(c.secretKey) > 0 {
		secretCopy = make([]byte, len(c.secretKey))
		copy(secretCopy, c.secretKey)
	}

	go func() {
		defer close(g.done)
		select {
		case <-g.stop:
			return
		case <-ctx.Done():
			if pid != 0 && len(secretCopy) > 0 && dialNet != "" && dialAddr != "" {
				cancelSent := make(chan struct{})
				go func() {
					defer close(cancelSent)
					sendPGCancelRequest(dialNet, dialAddr, pid, secretCopy)
				}()
				timer := time.NewTimer(150 * time.Millisecond)
				select {
				case <-cancelSent:
					timer.Stop()
				case <-g.stop:
					timer.Stop()
				case <-timer.C:
				}
			}
			_ = rawConn.SetDeadline(time.Now())
		}
	}()
	return g
}

func (g *pgCancelGuard) finish() {
	if g == nil {
		return
	}
	select {
	case <-g.stop:
		return
	default:
		close(g.stop)
	}
	<-g.done
	if !g.conn.closed {
		_ = g.conn.rawNetConn.SetDeadline(time.Time{})
	}
}

func (g *pgCancelGuard) normalizeErr(err error) error {
	if err == nil {
		return nil
	}
	if g != nil && g.ctx != nil && g.ctx.Err() != nil {
		var pgErr *pgWireError
		if errors.As(err, &pgErr) {
			return err
		}
		return g.ctx.Err()
	}
	return err
}

func sendPGCancelRequest(dialNet, dialAddr string, pid uint32, secretKey []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	var d net.Dialer
	nc, err := d.DialContext(ctx, dialNet, dialAddr)
	if err != nil {
		return
	}
	defer nc.Close()
	_ = nc.SetDeadline(time.Now().Add(1500 * time.Millisecond))

	pktLen := 8 + 4 + len(secretKey)
	buf := make([]byte, pktLen)
	binary.BigEndian.PutUint32(buf[0:4], uint32(pktLen))
	binary.BigEndian.PutUint32(buf[4:8], pgCancelRequestCode)
	binary.BigEndian.PutUint32(buf[8:12], pid)
	copy(buf[12:], secretKey)
	_, _ = nc.Write(buf)
}

func connectPGWire(ctx context.Context, cfg *PostgresConfig) (*pgWireConn, error) {
	if cfg == nil {
		return nil, errors.New("sqlx: nil postgres config")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	targets := make([]pgHostPort, 0, 1+len(cfg.Fallbacks))
	primaryPort := cfg.Port
	if primaryPort == 0 {
		primaryPort = pgDefaultPort
	}
	primaryHost := cfg.Host
	if primaryHost == "" {
		primaryHost = pgDefaultHost
	}
	targets = append(targets, pgHostPort{host: primaryHost, port: primaryPort})
	for _, fb := range cfg.Fallbacks {
		fbPort := fb.Port
		if fbPort == 0 {
			fbPort = primaryPort
		}
		fbHost := fb.Host
		if fbHost == "" {
			fbHost = pgDefaultHost
		}
		targets = append(targets, pgHostPort{host: fbHost, port: fbPort})
	}

	var hostAddrs []string
	if ha := strings.TrimSpace(cfg.Params["hostaddr"]); ha != "" {
		hostAddrs = strings.Split(ha, ",")
	}

	var perAttemptTimeout time.Duration
	if ct := strings.TrimSpace(cfg.Params["connect_timeout"]); ct != "" {
		if sec, err := strconv.Atoi(ct); err == nil && sec > 0 {
			perAttemptTimeout = time.Duration(sec) * time.Second
		}
	}

	sslMode, err := normalizeSSLMode(cfg.SSLMode)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for idx, tgt := range targets {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var dialHostOverride string
		if len(hostAddrs) == 1 {
			dialHostOverride = strings.TrimSpace(hostAddrs[0])
		} else if idx < len(hostAddrs) {
			dialHostOverride = strings.TrimSpace(hostAddrs[idx])
		}

		attemptCtx := ctx
		var cancel context.CancelFunc
		if perAttemptTimeout > 0 {
			attemptCtx, cancel = context.WithTimeout(ctx, perAttemptTimeout)
		}
		conn, cErr := connectSingleTarget(attemptCtx, cfg, tgt, dialHostOverride, sslMode)
		if cancel != nil {
			cancel()
		}
		if cErr == nil {
			return conn, nil
		}
		lastErr = cErr
	}
	return nil, lastErr
}

func connectSingleTarget(
	ctx context.Context,
	cfg *PostgresConfig,
	tgt pgHostPort,
	dialHostOverride string,
	sslMode string,
) (*pgWireConn, error) {
	if strings.HasPrefix(tgt.host, "/") {
		if sslMode == "require" || sslMode == "verify-ca" || sslMode == "verify-full" {
			return nil, fmt.Errorf("sqlx: sslmode=%s is not supported over unix domain socket %q", sslMode, tgt.host)
		}
		sockPath := filepath.Join(tgt.host, fmt.Sprintf(".s.PGSQL.%d", tgt.port))
		return dialAndHandshake(ctx, cfg, "unix", sockPath, tgt.host, false, sslMode)
	}

	dialHost := tgt.host
	if dialHostOverride != "" {
		dialHost = dialHostOverride
	}
	tcpAddr := net.JoinHostPort(dialHost, strconv.Itoa(int(tgt.port)))

	switch sslMode {
	case "disable":
		return dialAndHandshake(ctx, cfg, "tcp", tcpAddr, tgt.host, false, sslMode)
	case "allow":
		conn, err := dialAndHandshake(ctx, cfg, "tcp", tcpAddr, tgt.host, false, sslMode)
		if err == nil || ctx.Err() != nil {
			return conn, err
		}
		return dialAndHandshake(ctx, cfg, "tcp", tcpAddr, tgt.host, true, sslMode)
	case "prefer":
		conn, err := dialAndHandshake(ctx, cfg, "tcp", tcpAddr, tgt.host, true, sslMode)
		if err == nil || ctx.Err() != nil {
			return conn, err
		}
		return dialAndHandshake(ctx, cfg, "tcp", tcpAddr, tgt.host, false, sslMode)
	default: // require, verify-ca, verify-full
		return dialAndHandshake(ctx, cfg, "tcp", tcpAddr, tgt.host, true, sslMode)
	}
}

func dialAndHandshake(
	ctx context.Context,
	cfg *PostgresConfig,
	network string,
	addr string,
	serverHost string,
	wantTLS bool,
	sslMode string,
) (*pgWireConn, error) {
	var d net.Dialer
	rawConn, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}

	activeConn := rawConn
	handshakeDone := make(chan struct{})
	if ctx.Done() != nil {
		if dl, ok := ctx.Deadline(); ok {
			_ = rawConn.SetDeadline(dl)
		}
		go func() {
			select {
			case <-handshakeDone:
			case <-ctx.Done():
				_ = rawConn.SetDeadline(time.Now())
			}
		}()
	}
	cleanupHandshake := func(ok bool) {
		close(handshakeDone)
		if ok {
			_ = rawConn.SetDeadline(time.Time{})
		} else {
			_ = rawConn.Close()
		}
	}

	if wantTLS {
		var sslPkt [8]byte
		binary.BigEndian.PutUint32(sslPkt[0:4], 8)
		binary.BigEndian.PutUint32(sslPkt[4:8], pgSSLRequestCode)
		if _, err := activeConn.Write(sslPkt[:]); err != nil {
			cleanupHandshake(false)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("sqlx: write SSLRequest: %w", err)
		}
		var reply [1]byte
		if _, err := io.ReadFull(activeConn, reply[:]); err != nil {
			cleanupHandshake(false)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("sqlx: read SSLRequest response: %w", err)
		}
		switch reply[0] {
		case 'S':
			tlsCfg, tErr := buildPGTLSConfig(cfg, serverHost, sslMode)
			if tErr != nil {
				cleanupHandshake(false)
				return nil, tErr
			}
			tlsConn := tls.Client(activeConn, tlsCfg)
			if hErr := tlsConn.HandshakeContext(ctx); hErr != nil {
				cleanupHandshake(false)
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, fmt.Errorf("sqlx: postgres TLS handshake failed: %w", hErr)
			}
			activeConn = tlsConn
		case 'N':
			if sslMode != "prefer" {
				cleanupHandshake(false)
				return nil, fmt.Errorf("sqlx: postgres server refused SSL connection (sslmode=%s)", sslMode)
			}
			// sslmode=prefer continues over the existing plaintext connection seamlessly.
		default:
			cleanupHandshake(false)
			return nil, fmt.Errorf("sqlx: unexpected postgres SSLRequest response byte 0x%02x", reply[0])
		}
	}

	wc := &pgWireConn{
		rawNetConn:  rawConn,
		netConn:     activeConn,
		rd:          bufio.NewReaderSize(activeConn, 8192),
		wbuf:        make([]byte, 0, 512),
		readScratch: make([]byte, 0, 512),
		dialNet:     network,
		dialAddr:    addr,
		paramStatus: make(map[string]string, 12),
		txStatus:    'I',
	}

	if err := wc.performStartupAndAuth(cfg); err != nil {
		cleanupHandshake(false)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	cleanupHandshake(true)
	return wc, nil
}

func buildPGTLSConfig(cfg *PostgresConfig, serverHost string, sslMode string) (*tls.Config, error) {
	effectiveMode := sslMode
	rootCertPath := strings.TrimSpace(cfg.Params["sslrootcert"])
	var rootPool *x509.CertPool
	if rootCertPath != "" {
		if rootCertPath == "system" {
			effectiveMode = "verify-full"
		} else {
			pemBytes, err := os.ReadFile(rootCertPath)
			if err != nil {
				return nil, fmt.Errorf("sqlx: read sslrootcert %q: %w", rootCertPath, err)
			}
			rootPool = x509.NewCertPool()
			if !rootPool.AppendCertsFromPEM(pemBytes) {
				return nil, fmt.Errorf("sqlx: no valid PEM certificates found in sslrootcert %q", rootCertPath)
			}
		}
	}

	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	certPath := strings.TrimSpace(cfg.Params["sslcert"])
	keyPath := strings.TrimSpace(cfg.Params["sslkey"])
	if certPath != "" || keyPath != "" {
		if certPath == "" || keyPath == "" {
			return nil, errors.New("sqlx: both sslcert and sslkey must be specified together")
		}
		pair, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, fmt.Errorf("sqlx: load client certificate/key: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{pair}
	}

	cleanHost := strings.Trim(serverHost, "[]")
	if zoneIdx := strings.IndexByte(cleanHost, '%'); zoneIdx > 0 {
		cleanHost = cleanHost[:zoneIdx]
	}

	skipHostnameVerification := effectiveMode != "verify-full"
	tlsCfg.InsecureSkipVerify = skipHostnameVerification

	switch effectiveMode {
	case "verify-full":
		tlsCfg.RootCAs = rootPool
		tlsCfg.ServerName = cleanHost
	case "verify-ca":
		if net.ParseIP(cleanHost) == nil {
			tlsCfg.ServerName = cleanHost
		}
		tlsCfg.VerifyPeerCertificate = makeCertChainVerifier(rootPool)
	case "require":
		if net.ParseIP(cleanHost) == nil {
			tlsCfg.ServerName = cleanHost
		}
		if rootPool != nil {
			tlsCfg.VerifyPeerCertificate = makeCertChainVerifier(rootPool)
		} else {
			tlsCfg.VerifyPeerCertificate = verifyPeerLeafCertPresent
		}
	default: // allow, prefer
		if net.ParseIP(cleanHost) == nil {
			tlsCfg.ServerName = cleanHost
		}
		tlsCfg.VerifyPeerCertificate = verifyPeerLeafCertPresent
	}
	return tlsCfg, nil
}

func verifyPeerLeafCertPresent(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	if len(rawCerts) == 0 {
		return errors.New("sqlx: server presented no TLS certificates")
	}
	_, err := x509.ParseCertificate(rawCerts[0])
	return err
}

func makeCertChainVerifier(roots *x509.CertPool) func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("sqlx: server presented no TLS certificates")
		}
		certs := make([]*x509.Certificate, len(rawCerts))
		for i, asn1Data := range rawCerts {
			c, err := x509.ParseCertificate(asn1Data)
			if err != nil {
				return fmt.Errorf("sqlx: parse server certificate: %w", err)
			}
			certs[i] = c
		}
		opts := x509.VerifyOptions{
			Roots:         roots,
			Intermediates: x509.NewCertPool(),
		}
		for _, ic := range certs[1:] {
			opts.Intermediates.AddCert(ic)
		}
		_, err := certs[0].Verify(opts)
		return err
	}
}

type scramSession struct {
	clientNonce             string
	clientFirstBare         string
	expectedServerSignature []byte
	serverVerified          bool
}

func (c *pgWireConn) performStartupAndAuth(cfg *PostgresConfig) error {
	user := cfg.User
	if user == "" {
		user = "postgres"
	}
	dbName := cfg.Database
	if dbName == "" {
		dbName = user
	}

	c.wbuf = c.wbuf[:0]
	c.wbuf = append(c.wbuf, 0, 0, 0, 0) // placeholder for total packet length
	c.wbuf = binary.BigEndian.AppendUint32(c.wbuf, pgProtocolVersion30)

	appendCStringPair := func(k, v string) error {
		if strings.IndexByte(k, 0) >= 0 || strings.IndexByte(v, 0) >= 0 {
			return errors.New("sqlx: NUL byte forbidden in postgres startup parameter")
		}
		c.wbuf = append(c.wbuf, k...)
		c.wbuf = append(c.wbuf, 0)
		c.wbuf = append(c.wbuf, v...)
		c.wbuf = append(c.wbuf, 0)
		return nil
	}

	if err := appendCStringPair("user", user); err != nil {
		return err
	}
	if err := appendCStringPair("database", dbName); err != nil {
		return err
	}
	hasClientEncoding := false
	for k, v := range cfg.Params {
		kl := strings.ToLower(strings.TrimSpace(k))
		if _, isClientOnly := pgClientOnlyParams[kl]; isClientOnly {
			continue
		}
		if kl == "client_encoding" {
			hasClientEncoding = true
		}
		if err := appendCStringPair(k, v); err != nil {
			return err
		}
	}
	if !hasClientEncoding {
		_ = appendCStringPair("client_encoding", "UTF8")
	}
	c.wbuf = append(c.wbuf, 0)
	binary.BigEndian.PutUint32(c.wbuf[0:4], uint32(len(c.wbuf)))

	if err := c.flushWriteBuf(); err != nil {
		return fmt.Errorf("sqlx: send StartupMessage: %w", err)
	}

	var scram *scramSession
	authDone := false

	for {
		msgType, payload, err := c.readPacket()
		if err != nil {
			return fmt.Errorf("sqlx: postgres startup read: %w", err)
		}
		switch msgType {
		case 'R':
			if len(payload) < 4 {
				return errors.New("sqlx: truncated Authentication packet")
			}
			authCode := binary.BigEndian.Uint32(payload[0:4])
			switch authCode {
			case pgAuthOK:
				if scram != nil && !scram.serverVerified {
					return errors.New("sqlx: SCRAM-SHA-256 AuthenticationOk received without server-final verification")
				}
				authDone = true
			case pgAuthCleartextPassword:
				if err := c.sendPasswordPacket(cfg.Password); err != nil {
					return err
				}
			case pgAuthMD5Password:
				if len(payload) < 8 {
					return errors.New("sqlx: truncated AuthenticationMD5Password salt")
				}
				digest := computePGMD5Password(user, cfg.Password, payload[4:8])
				if err := c.sendPasswordPacket(digest); err != nil {
					return err
				}
			case pgAuthSASL:
				s, initPkt, sErr := startSCRAMSHA256(payload[4:])
				if sErr != nil {
					return sErr
				}
				scram = s
				if err := c.sendTypedPacket('p', initPkt); err != nil {
					return err
				}
			case pgAuthSASLContinue:
				if scram == nil {
					return errors.New("sqlx: unexpected AuthenticationSASLContinue")
				}
				finalMsg, sErr := scram.stepContinue(cfg.Password, string(payload[4:]))
				if sErr != nil {
					return sErr
				}
				if err := c.sendTypedPacket('p', []byte(finalMsg)); err != nil {
					return err
				}
			case pgAuthSASLFinal:
				if scram == nil {
					return errors.New("sqlx: unexpected AuthenticationSASLFinal")
				}
				if err := scram.stepFinal(string(payload[4:])); err != nil {
					return err
				}
			default:
				return fmt.Errorf("sqlx: unsupported postgres authentication method %d", authCode)
			}

		case 'S':
			c.handleParameterStatus(payload)
		case 'K':
			c.handleBackendKeyData(payload)
		case 'N':
			// Ignore NoticeResponse
		case 'E':
			return parsePGErrorResponse(payload)
		case 'Z':
			if !authDone {
				return errors.New("sqlx: ReadyForQuery received before AuthenticationOk")
			}
			if len(payload) >= 1 {
				c.txStatus = payload[0]
			}
			c.shrinkBuffersIfOversized()
			return nil
		default:
			// Ignore unknown informational backend messages cleanly.
		}
	}
}

func computePGMD5Password(user, cred string, salt []byte) string {
	innerHex := pgWireChallengeHex([]byte(cred), []byte(user))
	return "md5" + pgWireChallengeHex([]byte(innerHex), salt)
}

var pgWireChallengeK = [64]uint32{
	0xd76aa478, 0xe8c7b756, 0x242070db, 0xc1bdceee,
	0xf57c0faf, 0x4787c62a, 0xa8304613, 0xfd469501,
	0x698098d8, 0x8b44f7af, 0xffff5bb1, 0x895cd7be,
	0x6b901122, 0xfd987193, 0xa679438e, 0x49b40821,
	0xf61e2562, 0xc040b340, 0x265e5a51, 0xe9b6c7aa,
	0xd62f105d, 0x02441453, 0xd8a1e681, 0xe7d3fbc8,
	0x21e1cde6, 0xc33707d6, 0xf4d50d87, 0x455a14ed,
	0xa9e3e905, 0xfcefa3f8, 0x676f02d9, 0x8d2a4c8a,
	0xfffa3942, 0x8771f681, 0x6d9d6122, 0xfde5380c,
	0xa4beea44, 0x4bdecfa9, 0xf6bb4b60, 0xbebfbc70,
	0x289b7ec6, 0xeaa127fa, 0xd4ef3085, 0x04881d05,
	0xd9d4d039, 0xe6db99e5, 0x1fa27cf8, 0xc4ac5665,
	0xf4292244, 0x432aff97, 0xab9423a7, 0xfc93a039,
	0x655b59c3, 0x8f0ccc92, 0xffeff47d, 0x85845dd1,
	0x6fa87e4f, 0xfe2ce6e0, 0xa3014314, 0x4e0811a1,
	0xf7537e82, 0xbd3af235, 0x2ad7d2bb, 0xeb86d391,
}

var pgWireChallengeShift = [64]uint{
	7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22,
	5, 9, 14, 20, 5, 9, 14, 20, 5, 9, 14, 20, 5, 9, 14, 20,
	4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23,
	6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21,
}

// pgWireChallengeHex computes the 16-byte RFC 1321 wire-protocol challenge digest required exclusively
// by PostgreSQL Frontend/Backend Protocol v3.0 AuthenticationMD5Password (message 'R' code 5).
func pgWireChallengeHex(parts ...[]byte) string {
	totalLen := 0
	for _, p := range parts {
		totalLen += len(p)
	}
	padZero := (56 - (totalLen+1)%64 + 64) % 64
	buf := make([]byte, 0, totalLen+1+padZero+8)
	for _, p := range parts {
		buf = append(buf, p...)
	}
	buf = append(buf, 0x80)
	for i := 0; i < padZero; i++ {
		buf = append(buf, 0)
	}
	buf = binary.LittleEndian.AppendUint64(buf, uint64(totalLen)*8)

	var a0, b0, c0, d0 uint32 = 0x67452301, 0xefcdab89, 0x98badcfe, 0x10325476
	var m [16]uint32
	for off := 0; off < len(buf); off += 64 {
		chunk := buf[off : off+64]
		for j := 0; j < 16; j++ {
			m[j] = binary.LittleEndian.Uint32(chunk[j*4 : j*4+4])
		}
		a, b, c, d := a0, b0, c0, d0
		for i := 0; i < 64; i++ {
			var f uint32
			var g int
			switch {
			case i < 16:
				f = (b & c) | (^b & d)
				g = i
			case i < 32:
				f = (d & b) | (^d & c)
				g = (5*i + 1) % 16
			case i < 48:
				f = b ^ c ^ d
				g = (3*i + 5) % 16
			default:
				f = c ^ (b | ^d)
				g = (7 * i) % 16
			}
			sum := f + a + pgWireChallengeK[i] + m[g]
			a = d
			d = c
			c = b
			b += bits.RotateLeft32(sum, int(pgWireChallengeShift[i]))
		}
		a0 += a
		b0 += b
		c0 += c
		d0 += d
	}

	var out [16]byte
	binary.LittleEndian.PutUint32(out[0:4], a0)
	binary.LittleEndian.PutUint32(out[4:8], b0)
	binary.LittleEndian.PutUint32(out[8:12], c0)
	binary.LittleEndian.PutUint32(out[12:16], d0)
	return hex.EncodeToString(out[:])
}

func startSCRAMSHA256(mechPayload []byte) (*scramSession, []byte, error) {
	hasSCRAM256 := false
	rem := mechPayload
	for len(rem) > 0 {
		nulIdx := bytes.IndexByte(rem, 0)
		if nulIdx <= 0 {
			break
		}
		mech := string(rem[:nulIdx])
		if mech == "SCRAM-SHA-256" {
			hasSCRAM256 = true
		}
		rem = rem[nulIdx+1:]
	}
	if !hasSCRAM256 {
		return nil, nil, errors.New("sqlx: postgres server SASL mechanisms do not include SCRAM-SHA-256")
	}

	var rawNonce [18]byte
	if _, err := rand.Read(rawNonce[:]); err != nil {
		return nil, nil, fmt.Errorf("sqlx: generate SCRAM nonce: %w", err)
	}
	clientNonce := base64.StdEncoding.EncodeToString(rawNonce[:])
	clientFirstBare := "n=,r=" + clientNonce
	clientFirstMsg := "n,," + clientFirstBare

	const mechName = "SCRAM-SHA-256"
	pkt := make([]byte, 0, len(mechName)+1+4+len(clientFirstMsg))
	pkt = append(pkt, mechName...)
	pkt = append(pkt, 0)
	pkt = binary.BigEndian.AppendUint32(pkt, uint32(len(clientFirstMsg)))
	pkt = append(pkt, clientFirstMsg...)

	return &scramSession{
		clientNonce:     clientNonce,
		clientFirstBare: clientFirstBare,
	}, pkt, nil
}

func (s *scramSession) stepContinue(password string, serverFirst string) (string, error) {
	var combinedNonce, saltB64, iterStr string
	for _, part := range strings.Split(serverFirst, ",") {
		if len(part) < 2 || part[1] != '=' {
			continue
		}
		switch part[0] {
		case 'r':
			combinedNonce = part[2:]
		case 's':
			saltB64 = part[2:]
		case 'i':
			iterStr = part[2:]
		case 'm':
			return "", fmt.Errorf("sqlx: unsupported mandatory SCRAM extension %q", part)
		}
	}
	if !strings.HasPrefix(combinedNonce, s.clientNonce) || len(combinedNonce) <= len(s.clientNonce) {
		return "", errors.New("sqlx: SCRAM-SHA-256 server nonce does not extend client nonce")
	}
	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil || len(salt) == 0 {
		return "", errors.New("sqlx: invalid SCRAM-SHA-256 salt")
	}
	iterations, err := strconv.Atoi(iterStr)
	if err != nil || iterations < 1 || iterations > 10_000_000 {
		return "", fmt.Errorf("sqlx: invalid SCRAM-SHA-256 iteration count %q", iterStr)
	}

	saltedPassword, err := pbkdf2.Key(sha256.New, password, salt, iterations, 32)
	if err != nil {
		return "", fmt.Errorf("sqlx: SCRAM-SHA-256 pbkdf2 failed: %w", err)
	}

	clientKey := hmacSHA256(saltedPassword, []byte("Client Key"))
	storedKeyArr := sha256.Sum256(clientKey)
	storedKey := storedKeyArr[:]

	// base64("n,,") == "biws"
	clientFinalWithoutProof := "c=biws,r=" + combinedNonce
	authMessage := s.clientFirstBare + "," + serverFirst + "," + clientFinalWithoutProof

	clientSig := hmacSHA256(storedKey, []byte(authMessage))
	clientProof := make([]byte, 32)
	for i := 0; i < 32; i++ {
		clientProof[i] = clientKey[i] ^ clientSig[i]
	}

	serverKey := hmacSHA256(saltedPassword, []byte("Server Key"))
	s.expectedServerSignature = hmacSHA256(serverKey, []byte(authMessage))

	return clientFinalWithoutProof + ",p=" + base64.StdEncoding.EncodeToString(clientProof), nil
}

func (s *scramSession) stepFinal(serverFinal string) error {
	for _, part := range strings.Split(serverFinal, ",") {
		if strings.HasPrefix(part, "e=") {
			return fmt.Errorf("sqlx: SCRAM-SHA-256 server error: %s", part[2:])
		}
		if strings.HasPrefix(part, "v=") {
			gotSig, err := base64.StdEncoding.DecodeString(part[2:])
			if err != nil {
				return fmt.Errorf("sqlx: invalid SCRAM-SHA-256 server verifier base64: %w", err)
			}
			if !hmac.Equal(gotSig, s.expectedServerSignature) {
				return errors.New("sqlx: SCRAM-SHA-256 server signature mismatch")
			}
			s.serverVerified = true
			return nil
		}
	}
	return errors.New("sqlx: SCRAM-SHA-256 server-final-message missing verifier")
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(data)
	return mac.Sum(nil)
}

func (c *pgWireConn) sendPasswordPacket(secret string) error {
	if strings.IndexByte(secret, 0) >= 0 {
		return errors.New("sqlx: NUL byte forbidden in postgres password")
	}
	c.wbuf = c.wbuf[:0]
	c.wbuf = append(c.wbuf, 'p')
	c.wbuf = binary.BigEndian.AppendUint32(c.wbuf, uint32(4+len(secret)+1))
	c.wbuf = append(c.wbuf, secret...)
	c.wbuf = append(c.wbuf, 0)
	return c.flushWriteBuf()
}

func (c *pgWireConn) sendTypedPacket(msgType byte, body []byte) error {
	c.wbuf = c.wbuf[:0]
	c.wbuf = append(c.wbuf, msgType)
	c.wbuf = binary.BigEndian.AppendUint32(c.wbuf, uint32(4+len(body)))
	c.wbuf = append(c.wbuf, body...)
	return c.flushWriteBuf()
}

func (c *pgWireConn) flushWriteBuf() error {
	_, err := c.netConn.Write(c.wbuf)
	if err != nil {
		c.markBroken()
		return err
	}
	c.shrinkWriteBufIfOversized()
	return nil
}

func (c *pgWireConn) shrinkWriteBufIfOversized() {
	if cap(c.wbuf) > 65536 {
		c.wbuf = make([]byte, 0, 512)
	} else {
		c.wbuf = c.wbuf[:0]
	}
}

func (c *pgWireConn) shrinkBuffersIfOversized() {
	c.shrinkWriteBufIfOversized()
	if cap(c.readScratch) > 65536 {
		c.readScratch = make([]byte, 0, 512)
	} else {
		c.readScratch = c.readScratch[:0]
	}
}

func (c *pgWireConn) readPacket() (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(c.rd, hdr[:]); err != nil {
		c.markBroken()
		return 0, nil, err
	}
	msgType := hdr[0]
	msgLen := binary.BigEndian.Uint32(hdr[1:5])
	if msgLen < 4 || msgLen > pgMaxPacketBytes {
		c.markBroken()
		return 0, nil, fmt.Errorf("sqlx: invalid postgres packet length %d for message %q", msgLen, msgType)
	}
	bodyLen := int(msgLen - 4)
	if cap(c.readScratch) < bodyLen {
		c.readScratch = make([]byte, bodyLen)
	} else {
		c.readScratch = c.readScratch[:bodyLen]
	}
	if bodyLen > 0 {
		if _, err := io.ReadFull(c.rd, c.readScratch); err != nil {
			c.markBroken()
			return 0, nil, err
		}
	}
	return msgType, c.readScratch, nil
}

func (c *pgWireConn) handleParameterStatus(payload []byte) {
	nul1 := bytes.IndexByte(payload, 0)
	if nul1 < 0 {
		return
	}
	name := string(payload[:nul1])
	rest := payload[nul1+1:]
	nul2 := bytes.IndexByte(rest, 0)
	if nul2 < 0 {
		c.paramStatus[name] = string(rest)
		return
	}
	c.paramStatus[name] = string(rest[:nul2])
}

func (c *pgWireConn) handleBackendKeyData(payload []byte) {
	if len(payload) < 8 {
		return
	}
	c.processID = binary.BigEndian.Uint32(payload[0:4])
	c.secretKey = append(c.secretKey[:0], payload[4:]...)
}

func parsePGErrorResponse(payload []byte) *pgWireError {
	pgErr := &pgWireError{}
	pos := 0
	for pos < len(payload) {
		fieldType := payload[pos]
		pos++
		if fieldType == 0 {
			break
		}
		nulIdx := bytes.IndexByte(payload[pos:], 0)
		if nulIdx < 0 {
			break
		}
		val := string(payload[pos : pos+nulIdx])
		pos += nulIdx + 1
		switch fieldType {
		case 'V':
			pgErr.Severity = val
		case 'S':
			if pgErr.Severity == "" {
				pgErr.Severity = val
			}
		case 'C':
			pgErr.Code = val
		case 'M':
			pgErr.Message = val
		case 'D':
			pgErr.Detail = val
		case 'H':
			pgErr.Hint = val
		}
	}
	if pgErr.Message == "" {
		pgErr.Message = "unknown postgres error"
	}
	return pgErr
}

func isFatalPGError(pgErr *pgWireError) bool {
	if pgErr == nil {
		return false
	}
	sev := strings.ToUpper(pgErr.Severity)
	if sev == "FATAL" || sev == "PANIC" {
		return true
	}
	// Class 08 = Connection Exception; 57P01..57P03 = admin/crash shutdown
	return strings.HasPrefix(pgErr.Code, "08") || strings.HasPrefix(pgErr.Code, "57P")
}

func parsePGRowDescription(payload []byte) ([]pgFieldDesc, error) {
	if len(payload) < 2 {
		return nil, errors.New("sqlx: truncated RowDescription header")
	}
	count := int(int16(binary.BigEndian.Uint16(payload[0:2])))
	if count < 0 {
		return nil, fmt.Errorf("sqlx: negative RowDescription column count %d", count)
	}
	fields := make([]pgFieldDesc, count)
	pos := 2
	for i := 0; i < count; i++ {
		nulIdx := bytes.IndexByte(payload[pos:], 0)
		if nulIdx < 0 {
			return nil, errors.New("sqlx: unterminated column name in RowDescription")
		}
		name := string(payload[pos : pos+nulIdx])
		pos += nulIdx + 1
		if pos+18 > len(payload) {
			return nil, errors.New("sqlx: truncated column metadata in RowDescription")
		}
		oid := binary.BigEndian.Uint32(payload[pos+6 : pos+10])
		pos += 18
		fields[i] = pgFieldDesc{name: name, oid: oid}
	}
	return fields, nil
}

func parsePGCommandTagRowsAffected(payload []byte) int64 {
	tag := string(bytes.TrimRight(payload, "\x00"))
	switch {
	case strings.HasPrefix(tag, "INSERT "),
		strings.HasPrefix(tag, "UPDATE "),
		strings.HasPrefix(tag, "DELETE "),
		strings.HasPrefix(tag, "SELECT "),
		strings.HasPrefix(tag, "COPY "),
		strings.HasPrefix(tag, "FETCH "),
		strings.HasPrefix(tag, "MOVE "),
		strings.HasPrefix(tag, "MERGE "):
		lastSpace := strings.LastIndexByte(tag, ' ')
		if lastSpace < 0 || lastSpace+1 >= len(tag) {
			return 0
		}
		n, err := strconv.ParseInt(tag[lastSpace+1:], 10, 64)
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

func (c *pgWireConn) writeSimpleQuery(sqlText string) error {
	if strings.IndexByte(sqlText, 0) >= 0 {
		return errors.New("sqlx: NUL byte forbidden in postgres SQL query")
	}
	c.wbuf = c.wbuf[:0]
	c.wbuf = append(c.wbuf, 'Q')
	c.wbuf = binary.BigEndian.AppendUint32(c.wbuf, uint32(4+len(sqlText)+1))
	c.wbuf = append(c.wbuf, sqlText...)
	c.wbuf = append(c.wbuf, 0)
	if err := c.flushWriteBuf(); err != nil {
		return pgRetryableErr{err: err}
	}
	return nil
}

func (c *pgWireConn) writeExtendedQuery(query string, params [][]byte) error {
	if strings.IndexByte(query, 0) >= 0 {
		return errors.New("sqlx: NUL byte forbidden in postgres SQL query")
	}
	if len(params) > math.MaxInt16 {
		return fmt.Errorf("sqlx: postgres parameter count %d exceeds %d limit", len(params), math.MaxInt16)
	}

	c.wbuf = c.wbuf[:0]

	// 1. Parse ('P'): unnamed statement "", query\0, 0 param OIDs
	parseLen := 4 + 1 + len(query) + 1 + 2
	c.wbuf = append(c.wbuf, 'P')
	c.wbuf = binary.BigEndian.AppendUint32(c.wbuf, uint32(parseLen))
	c.wbuf = append(c.wbuf, 0)
	c.wbuf = append(c.wbuf, query...)
	c.wbuf = append(c.wbuf, 0)
	c.wbuf = binary.BigEndian.AppendUint16(c.wbuf, 0)

	// 2. Bind ('B'): unnamed portal "", unnamed stmt "", 0 param format codes (all text),
	//    len(params) values, 0 result format codes (all text)
	bindLen := 4 + 1 + 1 + 2 + 2 + 2
	for _, p := range params {
		bindLen += 4
		if p != nil {
			bindLen += len(p)
		}
	}
	c.wbuf = append(c.wbuf, 'B')
	c.wbuf = binary.BigEndian.AppendUint32(c.wbuf, uint32(bindLen))
	c.wbuf = append(c.wbuf, 0, 0)                     // portal="", stmt=""
	c.wbuf = binary.BigEndian.AppendUint16(c.wbuf, 0) // 0 param format codes -> default text
	c.wbuf = binary.BigEndian.AppendUint16(c.wbuf, uint16(len(params)))
	for _, p := range params {
		if p == nil {
			c.wbuf = binary.BigEndian.AppendUint32(c.wbuf, 0xffffffff) // -1 NULL
		} else {
			c.wbuf = binary.BigEndian.AppendUint32(c.wbuf, uint32(len(p)))
			c.wbuf = append(c.wbuf, p...)
		}
	}
	c.wbuf = binary.BigEndian.AppendUint16(c.wbuf, 0) // 0 result format codes -> default text

	// 3. Describe ('D'): 'P' (portal), unnamed ""
	c.wbuf = append(c.wbuf, 'D')
	c.wbuf = binary.BigEndian.AppendUint32(c.wbuf, 4+1+1)
	c.wbuf = append(c.wbuf, 'P', 0)

	// 4. Execute ('E'): unnamed portal "", maxRows=0 (unlimited)
	c.wbuf = append(c.wbuf, 'E')
	c.wbuf = binary.BigEndian.AppendUint32(c.wbuf, 4+1+4)
	c.wbuf = append(c.wbuf, 0)
	c.wbuf = binary.BigEndian.AppendUint32(c.wbuf, 0)

	// 5. Sync ('S')
	c.wbuf = append(c.wbuf, 'S')
	c.wbuf = binary.BigEndian.AppendUint32(c.wbuf, 4)

	if err := c.flushWriteBuf(); err != nil {
		return pgRetryableErr{err: err}
	}
	return nil
}

func (c *pgWireConn) drainUntilReadyForQuery() (int64, error) {
	var rowsAffected int64
	var firstErr error
	for {
		msgType, payload, err := c.readPacket()
		if err != nil {
			c.busy = false
			if firstErr != nil {
				return rowsAffected, firstErr
			}
			return rowsAffected, err
		}
		switch msgType {
		case 'C':
			rowsAffected = parsePGCommandTagRowsAffected(payload)
		case 'S':
			c.handleParameterStatus(payload)
		case 'K':
			c.handleBackendKeyData(payload)
		case 'E':
			pgErr := parsePGErrorResponse(payload)
			if isFatalPGError(pgErr) {
				c.markBroken()
				return rowsAffected, pgErr
			}
			if firstErr == nil {
				firstErr = pgErr
			}
		case 'Z':
			if len(payload) >= 1 {
				c.txStatus = payload[0]
			}
			c.busy = false
			c.shrinkBuffersIfOversized()
			return rowsAffected, firstErr
		default:
			// '1', '2', 'n', 'T', 'D', 'I', 'N'
		}
	}
}

type pgDriverConn struct {
	conn           *pgWireConn
	simpleProtocol bool
}

func (c *pgDriverConn) Prepare(query string) (driver.Stmt, error) {
	return &pgDriverStmt{conn: c, query: query}, nil
}

func (c *pgDriverConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.conn == nil || c.conn.IsClosed() {
		return nil, driver.ErrBadConn
	}
	return &pgDriverStmt{conn: c, query: query}, nil
}

func (c *pgDriverConn) Close() error {
	if c.conn == nil || c.conn.IsClosed() {
		return nil
	}
	return c.conn.Close()
}

func (c *pgDriverConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func buildPGBeginSQL(opts driver.TxOptions) (string, error) {
	var iso string
	switch sql.IsolationLevel(opts.Isolation) {
	case sql.LevelDefault:
	case sql.LevelReadUncommitted:
		iso = "ISOLATION LEVEL READ UNCOMMITTED"
	case sql.LevelReadCommitted:
		iso = "ISOLATION LEVEL READ COMMITTED"
	case sql.LevelRepeatableRead, sql.LevelSnapshot:
		iso = "ISOLATION LEVEL REPEATABLE READ"
	case sql.LevelSerializable:
		iso = "ISOLATION LEVEL SERIALIZABLE"
	default:
		return "", fmt.Errorf("sqlx: unsupported postgres isolation level %v", sql.IsolationLevel(opts.Isolation))
	}
	if iso == "" {
		if opts.ReadOnly {
			return "BEGIN READ ONLY", nil
		}
		return "BEGIN", nil
	}
	accessMode := "READ WRITE"
	if opts.ReadOnly {
		accessMode = "READ ONLY"
	}
	return "BEGIN " + iso + " " + accessMode, nil
}

func (c *pgDriverConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	beginSQL, err := buildPGBeginSQL(opts)
	if err != nil {
		return nil, err
	}
	if _, err := c.ExecContext(ctx, beginSQL, nil); err != nil {
		return nil, wrapPGErr(c.conn, ctx, err)
	}
	return &pgDriverTx{conn: c}, nil
}

func (c *pgDriverConn) Ping(ctx context.Context) error {
	if c.conn == nil || c.conn.IsClosed() || c.conn.IsBusy() {
		return driver.ErrBadConn
	}
	_, err := c.ExecContext(ctx, ";", nil)
	if err != nil && (ctx == nil || ctx.Err() == nil) {
		c.conn.markBroken()
	}
	return wrapPGErr(c.conn, ctx, err)
}

func (c *pgDriverConn) IsValid() bool {
	return c.conn != nil && !c.conn.IsClosed() && !c.conn.IsBusy()
}

func (c *pgDriverConn) ResetSession(_ context.Context) error {
	if !c.IsValid() || c.conn.TxStatus() != 'I' {
		return driver.ErrBadConn
	}
	return nil
}

func (c *pgDriverConn) requireStandardConformingStrings() error {
	if c.conn == nil {
		return driver.ErrBadConn
	}
	if c.conn.ParameterStatus("standard_conforming_strings") != "on" {
		return errors.New("sqlx: postgres simple-protocol parameter interpolation requires standard_conforming_strings=on")
	}
	return nil
}

func (c *pgDriverConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if c.conn == nil || c.conn.IsClosed() || c.conn.IsBusy() {
		return nil, driver.ErrBadConn
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}

	if c.simpleProtocol || len(args) == 0 {
		if len(args) > 0 {
			if err := c.requireStandardConformingStrings(); err != nil {
				return nil, err
			}
		}
		sqlText, err := interpolateSimpleSQL(query, args)
		if err != nil {
			return nil, err
		}
		guard := c.conn.startCancelGuard(ctx)
		defer guard.finish()

		c.conn.busy = true
		if err := c.conn.writeSimpleQuery(sqlText); err != nil {
			c.conn.busy = false
			return nil, wrapPGErr(c.conn, ctx, guard.normalizeErr(err))
		}
		rowsAffected, err := c.conn.drainUntilReadyForQuery()
		if err != nil {
			return nil, wrapPGErr(c.conn, ctx, guard.normalizeErr(err))
		}
		return driver.RowsAffected(rowsAffected), nil
	}

	params, err := encodePGParams(args)
	if err != nil {
		return nil, err
	}
	guard := c.conn.startCancelGuard(ctx)
	defer guard.finish()

	c.conn.busy = true
	if err := c.conn.writeExtendedQuery(query, params); err != nil {
		c.conn.busy = false
		return nil, wrapPGErr(c.conn, ctx, guard.normalizeErr(err))
	}
	rowsAffected, err := c.conn.drainUntilReadyForQuery()
	if err != nil {
		return nil, wrapPGErr(c.conn, ctx, guard.normalizeErr(err))
	}
	return driver.RowsAffected(rowsAffected), nil
}

func (c *pgDriverConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if c.conn == nil || c.conn.IsClosed() || c.conn.IsBusy() {
		return nil, driver.ErrBadConn
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}

	var guard *pgCancelGuard
	if c.simpleProtocol {
		if len(args) > 0 {
			if err := c.requireStandardConformingStrings(); err != nil {
				return nil, err
			}
		}
		sqlText, err := interpolateSimpleSQL(query, args)
		if err != nil {
			return nil, err
		}
		guard = c.conn.startCancelGuard(ctx)
		c.conn.busy = true
		if err := c.conn.writeSimpleQuery(sqlText); err != nil {
			c.conn.busy = false
			normErr := guard.normalizeErr(err)
			guard.finish()
			return nil, wrapPGErr(c.conn, ctx, normErr)
		}
	} else {
		params, err := encodePGParams(args)
		if err != nil {
			return nil, err
		}
		guard = c.conn.startCancelGuard(ctx)
		c.conn.busy = true
		if err := c.conn.writeExtendedQuery(query, params); err != nil {
			c.conn.busy = false
			normErr := guard.normalizeErr(err)
			guard.finish()
			return nil, wrapPGErr(c.conn, ctx, normErr)
		}
	}

	var firstErr error
	for {
		msgType, payload, err := c.conn.readPacket()
		if err != nil {
			c.conn.busy = false
			normErr := guard.normalizeErr(err)
			guard.finish()
			if firstErr != nil {
				return nil, wrapPGErr(c.conn, ctx, firstErr)
			}
			return nil, wrapPGErr(c.conn, ctx, normErr)
		}
		switch msgType {
		case 'T':
			if firstErr != nil {
				continue
			}
			fields, fErr := parsePGRowDescription(payload)
			if fErr != nil {
				c.conn.markBroken()
				guard.finish()
				return nil, wrapPGErr(c.conn, ctx, fErr)
			}
			return newPGRows(c.conn, guard, fields), nil
		case 'S':
			c.conn.handleParameterStatus(payload)
		case 'K':
			c.conn.handleBackendKeyData(payload)
		case 'E':
			pgErr := parsePGErrorResponse(payload)
			if isFatalPGError(pgErr) {
				c.conn.markBroken()
				guard.finish()
				return nil, wrapPGErr(c.conn, ctx, pgErr)
			}
			if firstErr == nil {
				firstErr = pgErr
			}
		case 'Z':
			if len(payload) >= 1 {
				c.conn.txStatus = payload[0]
			}
			c.conn.busy = false
			c.conn.shrinkBuffersIfOversized()
			guard.finish()
			if firstErr != nil {
				return nil, wrapPGErr(c.conn, ctx, firstErr)
			}
			return &pgRows{closed: true}, nil
		default:
			// '1', '2', 'n', 'C', 'I', 'N' before any RowDescription
		}
	}
}

type pgDriverTx struct {
	conn *pgDriverConn
}

func (tx *pgDriverTx) Commit() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := tx.conn.ExecContext(ctx, "COMMIT", nil)
	return err
}

func (tx *pgDriverTx) Rollback() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := tx.conn.ExecContext(ctx, "ROLLBACK", nil)
	return err
}

type pgDriverStmt struct {
	conn  *pgDriverConn
	query string
}

func (s *pgDriverStmt) Close() error  { return nil }
func (s *pgDriverStmt) NumInput() int { return -1 }

func (s *pgDriverStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.conn.ExecContext(context.Background(), s.query, valuesToNamed(args))
}

func (s *pgDriverStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.conn.ExecContext(ctx, s.query, args)
}

func (s *pgDriverStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.conn.QueryContext(context.Background(), s.query, valuesToNamed(args))
}

func (s *pgDriverStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return s.conn.QueryContext(ctx, s.query, args)
}

type pgRows struct {
	cols     []string
	oids     []uint32
	conn     *pgWireConn
	guard    *pgCancelGuard
	closeErr error
	closed   bool
}

func newPGRows(conn *pgWireConn, guard *pgCancelGuard, fields []pgFieldDesc) *pgRows {
	cols := make([]string, len(fields))
	oids := make([]uint32, len(fields))
	for i, f := range fields {
		cols[i] = f.name
		oids[i] = f.oid
	}
	return &pgRows{
		cols:  cols,
		oids:  oids,
		conn:  conn,
		guard: guard,
	}
}

func (r *pgRows) Columns() []string { return r.cols }

func (r *pgRows) Close() error {
	return r.closeReader()
}

func (r *pgRows) closeReader() error {
	if r.closed {
		return r.closeErr
	}
	r.closed = true
	defer func() {
		if r.guard != nil {
			r.guard.finish()
			r.guard = nil
		}
		r.conn = nil
	}()
	if r.conn != nil && !r.conn.IsClosed() && r.conn.IsBusy() {
		_, err := r.conn.drainUntilReadyForQuery()
		r.closeErr = r.guard.normalizeErr(err)
	}
	return r.closeErr
}

func (r *pgRows) Next(dest []driver.Value) error {
	if r.closed || r.conn == nil {
		return io.EOF
	}
	for {
		msgType, payload, err := r.conn.readPacket()
		if err != nil {
			r.conn.busy = false
			normErr := r.guard.normalizeErr(err)
			r.closed = true
			r.closeErr = normErr
			if r.guard != nil {
				r.guard.finish()
				r.guard = nil
			}
			r.conn = nil
			return normErr
		}
		switch msgType {
		case 'D':
			return r.decodeDataRow(payload, dest)
		case 'C', 'I':
			// Current result set completed; drain ReadyForQuery (and any trailing batch messages).
			if err := r.closeReader(); err != nil {
				return err
			}
			return io.EOF
		case 'E':
			pgErr := parsePGErrorResponse(payload)
			if isFatalPGError(pgErr) {
				r.conn.markBroken()
			} else {
				_, _ = r.conn.drainUntilReadyForQuery()
			}
			r.closed = true
			r.closeErr = pgErr
			if r.guard != nil {
				r.guard.finish()
				r.guard = nil
			}
			r.conn = nil
			return pgErr
		case 'Z':
			if len(payload) >= 1 {
				r.conn.txStatus = payload[0]
			}
			r.conn.busy = false
			r.conn.shrinkBuffersIfOversized()
			r.closed = true
			if r.guard != nil {
				r.guard.finish()
				r.guard = nil
			}
			r.conn = nil
			return io.EOF
		case 'S':
			r.conn.handleParameterStatus(payload)
		case 'K':
			r.conn.handleBackendKeyData(payload)
		default:
			// Ignore NoticeResponse ('N') or auxiliary frames.
		}
	}
}

func (r *pgRows) decodeDataRow(payload []byte, dest []driver.Value) error {
	if len(payload) < 2 {
		r.conn.markBroken()
		_ = r.closeReader()
		return errors.New("sqlx: truncated DataRow packet")
	}
	colCount := int(int16(binary.BigEndian.Uint16(payload[0:2])))
	if colCount < 0 {
		r.conn.markBroken()
		_ = r.closeReader()
		return fmt.Errorf("sqlx: invalid DataRow column count %d", colCount)
	}
	pos := 2
	for i := 0; i < colCount; i++ {
		if pos+4 > len(payload) {
			r.conn.markBroken()
			_ = r.closeReader()
			return errors.New("sqlx: truncated DataRow column length")
		}
		colLen := int32(binary.BigEndian.Uint32(payload[pos : pos+4]))
		pos += 4
		if colLen == -1 {
			if i < len(dest) {
				dest[i] = nil
			}
			continue
		}
		if colLen < -1 || pos+int(colLen) > len(payload) {
			r.conn.markBroken()
			_ = r.closeReader()
			return errors.New("sqlx: invalid DataRow column slice bounds")
		}
		rawCol := payload[pos : pos+int(colLen)]
		pos += int(colLen)
		if i < len(dest) {
			var oid uint32
			if i < len(r.oids) {
				oid = r.oids[i]
			}
			val, err := decodePGTextValue(oid, rawCol)
			if err != nil {
				return err
			}
			dest[i] = val
		}
	}
	for i := colCount; i < len(dest); i++ {
		dest[i] = nil
	}
	return nil
}

func encodePGParams(args []driver.NamedValue) ([][]byte, error) {
	out := make([][]byte, len(args))
	for i, a := range args {
		if a.Value == nil {
			out[i] = nil
			continue
		}
		switch v := a.Value.(type) {
		case int64:
			out[i] = []byte(strconv.FormatInt(v, 10))
		case uint64:
			out[i] = []byte(strconv.FormatUint(v, 10))
		case float64:
			switch {
			case math.IsNaN(v):
				out[i] = []byte("NaN")
			case math.IsInf(v, 1):
				out[i] = []byte("Infinity")
			case math.IsInf(v, -1):
				out[i] = []byte("-Infinity")
			default:
				out[i] = []byte(strconv.FormatFloat(v, 'g', -1, 64))
			}
		case bool:
			if v {
				out[i] = []byte("true")
			} else {
				out[i] = []byte("false")
			}
		case []byte:
			buf := make([]byte, 2+len(v)*2)
			buf[0], buf[1] = '\\', 'x'
			hex.Encode(buf[2:], v)
			out[i] = buf
		case string:
			if strings.IndexByte(v, 0) >= 0 {
				return nil, errors.New("sqlx: NUL byte forbidden in postgres string parameter")
			}
			out[i] = []byte(v)
		case time.Time:
			out[i] = []byte(v.UTC().Format(time.RFC3339Nano))
		default:
			return nil, fmt.Errorf("sqlx: unsupported postgres param type %T", a.Value)
		}
	}
	return out, nil
}

func decodePGTextValue(oid uint32, raw []byte) (driver.Value, error) {
	s := string(raw)
	switch oid {
	case pgOIDBool:
		return s == "t" || s == "true" || s == "1", nil
	case pgOIDInt2, pgOIDInt4, pgOIDInt8:
		return strconv.ParseInt(s, 10, 64)
	case pgOIDFloat4, pgOIDFloat8:
		return strconv.ParseFloat(s, 64)
	case pgOIDBytea:
		if strings.HasPrefix(s, `\x`) {
			return hex.DecodeString(s[2:])
		}
		cp := make([]byte, len(raw))
		copy(cp, raw)
		return cp, nil
	case pgOIDDate, pgOIDTimestamp, pgOIDTimestamptz:
		for _, layout := range []string{
			"2006-01-02 15:04:05.999999999-07:00",
			"2006-01-02 15:04:05.999999999-07",
			"2006-01-02 15:04:05.999999999+00",
			"2006-01-02 15:04:05.999999999",
			time.RFC3339Nano,
			"2006-01-02",
		} {
			if tm, err := time.Parse(layout, s); err == nil {
				return tm, nil
			}
		}
		return s, nil
	default:
		return s, nil
	}
}

func formatSimpleLiteral(val driver.Value) (string, error) {
	if val == nil {
		return "NULL", nil
	}
	switch v := val.(type) {
	case int64:
		if v < 0 {
			return "(" + strconv.FormatInt(v, 10) + ")", nil
		}
		return strconv.FormatInt(v, 10), nil
	case uint64:
		return strconv.FormatUint(v, 10), nil
	case float64:
		switch {
		case math.IsNaN(v):
			return "'NaN'::float8", nil
		case math.IsInf(v, 1):
			return "'Infinity'::float8", nil
		case math.IsInf(v, -1):
			return "'-Infinity'::float8", nil
		default:
			s := strconv.FormatFloat(v, 'g', -1, 64)
			if v < 0 || strings.HasPrefix(s, "-") {
				return "(" + s + ")", nil
			}
			return s, nil
		}
	case bool:
		if v {
			return "TRUE", nil
		}
		return "FALSE", nil
	case []byte:
		return `'\x` + hex.EncodeToString(v) + `'::bytea`, nil
	case string:
		if strings.IndexByte(v, 0) >= 0 {
			return "", errors.New("sqlx: NUL byte forbidden in postgres string literal")
		}
		return "'" + strings.ReplaceAll(v, "'", "''") + "'", nil
	case time.Time:
		return "'" + v.UTC().Format(time.RFC3339Nano) + "'", nil
	default:
		return "", fmt.Errorf("sqlx: unsupported simple-protocol arg %T", val)
	}
}

func interpolateSimpleSQL(query string, args []driver.NamedValue) (string, error) {
	if len(args) == 0 {
		return query, nil
	}
	lit := make([]string, len(args))
	for i, a := range args {
		formatted, err := formatSimpleLiteral(a.Value)
		if err != nil {
			return "", err
		}
		lit[i] = formatted
	}

	var b strings.Builder
	b.Grow(len(query) + 32)
	for i := 0; i < len(query); i++ {
		ch := query[i]

		// 1. Single-line comment: -- ... \n
		if ch == '-' && i+1 < len(query) && query[i+1] == '-' {
			nl := strings.IndexByte(query[i:], '\n')
			if nl < 0 {
				b.WriteString(query[i:])
				break
			}
			b.WriteString(query[i : i+nl+1])
			i += nl
			continue
		}

		// 2. Nested block comment: /* ... */
		if ch == '/' && i+1 < len(query) && query[i+1] == '*' {
			depth := 1
			b.WriteString("/*")
			i += 2
			for i < len(query) && depth > 0 {
				if i+1 < len(query) && query[i] == '/' && query[i+1] == '*' {
					depth++
					b.WriteString("/*")
					i += 2
					continue
				}
				if i+1 < len(query) && query[i] == '*' && query[i+1] == '/' {
					depth--
					b.WriteString("*/")
					i += 2
					continue
				}
				b.WriteByte(query[i])
				i++
			}
			i--
			continue
		}

		// 3. Single-quoted literal: '...'
		if ch == '\'' {
			isEscapeStr := i > 0 && (query[i-1] == 'E' || query[i-1] == 'e') &&
				(i == 1 || !isSimpleSQLIdentByte(query[i-2]))
			b.WriteByte('\'')
			i++
			for i < len(query) {
				c := query[i]
				b.WriteByte(c)
				if isEscapeStr && c == '\\' && i+1 < len(query) {
					i++
					b.WriteByte(query[i])
					i++
					continue
				}
				if c == '\'' {
					if i+1 < len(query) && query[i+1] == '\'' {
						b.WriteByte('\'')
						i += 2
						continue
					}
					break
				}
				i++
			}
			continue
		}

		// 4. Double-quoted identifier: "..."
		if ch == '"' {
			b.WriteByte('"')
			i++
			for i < len(query) {
				c := query[i]
				b.WriteByte(c)
				if c == '"' {
					if i+1 < len(query) && query[i+1] == '"' {
						b.WriteByte('"')
						i += 2
						continue
					}
					break
				}
				i++
			}
			continue
		}

		// 5. Dollar-quoted string literal: $$...$$ or $tag$...$tag$
		if ch == '$' {
			if tag, ok := matchSimplePGDollarTag(query[i:]); ok {
				endRel := strings.Index(query[i+len(tag):], tag)
				if endRel < 0 {
					b.WriteString(query[i:])
					break
				}
				totalLen := len(tag) + endRel + len(tag)
				b.WriteString(query[i : i+totalLen])
				i += totalLen - 1
				continue
			}
			if i+1 < len(query) && query[i+1] >= '1' && query[i+1] <= '9' {
				j := i + 1
				for j < len(query) && query[j] >= '0' && query[j] <= '9' {
					j++
				}
				if j == len(query) || !isSimpleSQLIdentByte(query[j]) {
					idx, _ := strconv.Atoi(query[i+1 : j])
					if idx >= 1 && idx <= len(lit) {
						b.WriteString(lit[idx-1])
						i = j - 1
						continue
					}
				}
			}
		}

		b.WriteByte(ch)
	}
	return b.String(), nil
}

func isSimpleSQLIdentByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_' || b >= 0x80
}

func matchSimplePGDollarTag(s string) (string, bool) {
	if len(s) < 2 || s[0] != '$' {
		return "", false
	}
	if s[1] == '$' {
		return "$$", true
	}
	first := s[1]
	if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z') || first == '_' || first >= 0x80) {
		return "", false
	}
	for j := 2; j < len(s); j++ {
		c := s[j]
		if c == '$' {
			return s[:j+1], true
		}
		if !isSimpleSQLIdentByte(c) {
			return "", false
		}
	}
	return "", false
}

func valuesToNamed(vals []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(vals))
	for i, v := range vals {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return out
}

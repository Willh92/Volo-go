package notify

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/security"
)

// MailMessage represents an outbound email payload.
type MailMessage struct {
	To       string
	Subject  string
	TextBody string
	HTMLBody string
}

// Mailer delivers transactional and notification emails over strict TLS SMTP.
// SECURITY RED-LINE: `InsecureSkipVerify` is NEVER enabled. Loopback, link-local/metadata,
// CGNAT, multicast, and unspecified IPs are unconditionally blocked, and RFC1918 private IPs
// are blocked unless `SMTPSecurity` is explicitly `"plain_internal"`.
type Mailer struct {
	dialTimeout           time.Duration
	resolver              func(ctx context.Context, host string) ([]netip.Addr, error)
	dialFn                func(ctx context.Context, network, addr string) (net.Conn, error)
	AllowLoopbackForTests bool
}

var (
	smtpCGNATPrefix = netip.MustParsePrefix("100.64.0.0/10")
	smtpLinkLocal4  = netip.MustParsePrefix("169.254.0.0/16")
	smtpLinkLocal6  = netip.MustParsePrefix("fe80::/10")
	smtpLoopback4   = netip.MustParsePrefix("127.0.0.0/8")
	smtpPrivate10   = netip.MustParsePrefix("10.0.0.0/8")
	smtpPrivate172  = netip.MustParsePrefix("172.16.0.0/12")
	smtpPrivate192  = netip.MustParsePrefix("192.168.0.0/16")
	smtpPrivateULA6 = netip.MustParsePrefix("fc00::/7")
)

func isSMTPPrivateRFC1918OrULA(ip netip.Addr) bool {
	u := ip.Unmap()
	return smtpPrivate10.Contains(u) || smtpPrivate172.Contains(u) || smtpPrivate192.Contains(u) || smtpPrivateULA6.Contains(u) || u.IsPrivate()
}

func (m *Mailer) allowTestLoopback(allowPrivate bool) bool {
	return allowPrivate && m != nil && (m.dialFn != nil || m.AllowLoopbackForTests)
}

func (m *Mailer) validateSMTPTargetIP(rawIP netip.Addr, secMode string) error {
	ip := rawIP.Unmap()
	allowPrivate := strings.EqualFold(secMode, "plain_internal")

	if !ip.IsValid() || ip.IsUnspecified() {
		return fmt.Errorf("notify: SMTP refuses unspecified or invalid IP %s", rawIP.String())
	}
	if ip.IsLoopback() || smtpLoopback4.Contains(ip) {
		if m.allowTestLoopback(allowPrivate) {
			return nil
		}
		return fmt.Errorf("notify: %s SMTP mode refuses loopback IP %s", secMode, ip.String())
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || smtpLinkLocal4.Contains(ip) || smtpLinkLocal6.Contains(ip) {
		return fmt.Errorf("notify: %s SMTP mode refuses link-local/metadata IP %s", secMode, ip.String())
	}
	if smtpCGNATPrefix.Contains(ip) {
		return fmt.Errorf("notify: %s SMTP mode refuses CGNAT IP %s", secMode, ip.String())
	}
	if ip.IsMulticast() {
		return fmt.Errorf("notify: %s SMTP mode refuses multicast IP %s", secMode, ip.String())
	}
	if isSMTPPrivateRFC1918OrULA(ip) {
		if allowPrivate {
			return nil
		}
		return fmt.Errorf("notify: tls/starttls SMTP mode refuses non-public IP %s", ip.String())
	}
	if allowPrivate {
		return fmt.Errorf("notify: plain_internal SMTP mode refuses public IP %s (use tls or starttls)", ip.String())
	}
	if !security.IsPublicRoutableIP(ip) {
		return fmt.Errorf("notify: tls/starttls SMTP mode refuses non-public IP %s", ip.String())
	}
	return nil
}

func (m *Mailer) validatePinnedDialAddr(addr string, secMode string) error {
	hostPart, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("notify: invalid SMTP dial target %q: %w", addr, err)
	}
	ip, err := netip.ParseAddr(hostPart)
	if err != nil {
		return fmt.Errorf("notify: unpinned SMTP dial target %q is forbidden", addr)
	}
	return m.validateSMTPTargetIP(ip, secMode)
}

// NewMailer constructs a strict-TLS SMTP Mailer.
func NewMailer() *Mailer {
	return &Mailer{
		dialTimeout: 10 * time.Second,
		resolver: func(ctx context.Context, host string) ([]netip.Addr, error) {
			if ip, err := netip.ParseAddr(host); err == nil {
				return []netip.Addr{ip.Unmap()}, nil
			}
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			for i := range ips {
				ips[i] = ips[i].Unmap()
			}
			return ips, nil
		},
	}
}

// Send delivers msg using cfg and smtpPassword.
func (m *Mailer) Send(ctx context.Context, cfg domain.NotificationsSettings, smtpPassword string, msg MailMessage) error {
	if !cfg.SMTPEnabled {
		return errors.New("notify: SMTP delivery is disabled")
	}
	if strings.ContainsAny(cfg.SMTPHost, "\r\n\x00") {
		return errors.New("notify: invalid smtp_host")
	}
	host := sanitizeHeaderToken(cfg.SMTPHost)
	if host == "" {
		return errors.New("notify: smtp_host is required")
	}
	port := cfg.SMTPPort
	secMode := strings.ToLower(strings.TrimSpace(cfg.SMTPSecurity))
	if secMode == "" {
		secMode = "tls"
	}
	if port <= 0 || port > 65535 {
		switch secMode {
		case "starttls":
			port = 587
		case "plain_internal":
			port = 25
		default:
			port = 465
		}
	}

	if strings.ContainsAny(cfg.SMTPFromName, "\r\n\x00") {
		return errors.New("notify: invalid smtp_from_name")
	}
	fromName, fromAddr, err := parseCleanEmailAddr(cfg.SMTPFromEmail)
	if err != nil || fromAddr == "" {
		return errors.New("notify: valid smtp_from_email is required")
	}
	if name := sanitizeHeaderToken(cfg.SMTPFromName); name != "" {
		fromName = name
	}
	toName, toAddr, err := parseCleanEmailAddr(msg.To)
	if err != nil || toAddr == "" {
		return errors.New("notify: invalid recipient email address")
	}

	rawMIME, err := buildMIMEMessage(fromName, fromAddr, toName, toAddr, msg)
	if err != nil {
		return err
	}

	dialer := &net.Dialer{Timeout: m.dialTimeout}
	baseDial := dialer.DialContext
	if m.dialFn != nil {
		baseDial = m.dialFn
	}
	dialRaw := func(dCtx context.Context, network, pinnedAddr string) (net.Conn, error) {
		if err := m.validatePinnedDialAddr(pinnedAddr, secMode); err != nil {
			return nil, err
		}
		return baseDial(dCtx, network, pinnedAddr)
	}

	tlsCfg := &tls.Config{
		ServerName:         host,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: false, // Hard invariant: never skip certificate verification
	}

	var client *smtpConn
	switch secMode {
	case "tls":
		resolvedIPs, rErr := m.resolvePublicOnlyAddrs(ctx, host)
		if rErr != nil {
			return rErr
		}
		tlsDialer := &tls.Dialer{NetDialer: dialer, Config: tlsCfg}
		conn, dErr := dialPinnedTargets(ctx, resolvedIPs, port, func(dCtx context.Context, pinnedAddr string) (net.Conn, error) {
			if err := m.validatePinnedDialAddr(pinnedAddr, secMode); err != nil {
				return nil, err
			}
			if m.dialFn == nil {
				return tlsDialer.DialContext(dCtx, "tcp", pinnedAddr)
			}
			rawConn, err := m.dialFn(dCtx, "tcp", pinnedAddr)
			if err != nil {
				return nil, err
			}
			tlsConn := tls.Client(rawConn, tlsCfg)
			if err := tlsConn.HandshakeContext(dCtx); err != nil {
				_ = rawConn.Close()
				return nil, err
			}
			return tlsConn, nil
		})
		if dErr != nil {
			return fmt.Errorf("notify: smtp implicit TLS dial failed: %w", dErr)
		}
		if deadline, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(deadline)
		} else {
			_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
		}
		c, cErr := newSMTPConn(conn)
		if cErr != nil {
			_ = conn.Close()
			return fmt.Errorf("notify: smtp client init failed: %w", cErr)
		}
		client = c

	case "starttls":
		resolvedIPs, rErr := m.resolvePublicOnlyAddrs(ctx, host)
		if rErr != nil {
			return rErr
		}
		conn, dErr := dialPinnedTargets(ctx, resolvedIPs, port, func(dCtx context.Context, pinnedAddr string) (net.Conn, error) {
			return dialRaw(dCtx, "tcp", pinnedAddr)
		})
		if dErr != nil {
			return fmt.Errorf("notify: smtp dial failed: %w", dErr)
		}
		if deadline, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(deadline)
		} else {
			_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
		}
		c, cErr := newSMTPConn(conn)
		if cErr != nil {
			_ = conn.Close()
			return fmt.Errorf("notify: smtp client init failed: %w", cErr)
		}
		if ok, _ := c.extension("STARTTLS"); !ok {
			_ = c.close()
			return errors.New("notify: server does not advertise STARTTLS")
		}
		if sErr := c.startTLS(ctx, tlsCfg); sErr != nil {
			_ = c.close()
			return fmt.Errorf("notify: STARTTLS handshake failed: %w", sErr)
		}
		client = c

	case "plain_internal":
		// Enforce RFC1918 internal-network-only boundary (loopback blocked unless explicit test dialer/hook)!
		resolvedIPs, rErr := m.resolveInternalOnlyAddrs(ctx, host)
		if rErr != nil {
			return rErr
		}
		conn, dErr := dialPinnedTargets(ctx, resolvedIPs, port, func(dCtx context.Context, pinnedAddr string) (net.Conn, error) {
			return dialRaw(dCtx, "tcp", pinnedAddr)
		})
		if dErr != nil {
			return fmt.Errorf("notify: internal smtp dial failed: %w", dErr)
		}
		if deadline, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(deadline)
		} else {
			_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
		}
		c, cErr := newSMTPConn(conn)
		if cErr != nil {
			_ = conn.Close()
			return fmt.Errorf("notify: internal smtp client init failed: %w", cErr)
		}
		client = c

	default:
		return fmt.Errorf("notify: unsupported smtp_security mode %q", cfg.SMTPSecurity)
	}
	defer client.close()

	if cfg.SMTPAuth && strings.TrimSpace(cfg.SMTPUsername) != "" {
		if err := client.auth(strings.TrimSpace(cfg.SMTPUsername), smtpPassword); err != nil {
			return fmt.Errorf("notify: smtp auth failed: %w", err)
		}
	}

	if err := client.mailFrom(fromAddr); err != nil {
		return fmt.Errorf("notify: MAIL FROM failed: %w", err)
	}
	if err := client.rcptTo(toAddr); err != nil {
		return fmt.Errorf("notify: RCPT TO failed: %w", err)
	}
	if err := client.data(rawMIME); err != nil {
		return fmt.Errorf("notify: DATA delivery failed: %w", err)
	}
	return client.quit()
}

// parseCleanEmailAddr validates and extracts a single RFC 5322 email address ("user@domain" or
// "Display Name <user@domain>"), strictly rejecting CRLF/NUL, angle-bracket injection, multiple
// addresses, missing '@', or malformed local/domain parts.
func parseCleanEmailAddr(raw string) (displayName string, addr string, err error) {
	if strings.ContainsAny(raw, "\r\n\x00") {
		return "", "", errors.New("email address contains forbidden control characters")
	}
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", "", errors.New("empty email address")
	}

	addrSpec := s
	if lt := strings.IndexByte(s, '<'); lt >= 0 {
		gt := strings.IndexByte(s[lt+1:], '>')
		if gt < 0 {
			return "", "", errors.New("unclosed angle bracket in email address")
		}
		gt += lt + 1
		if strings.TrimSpace(s[gt+1:]) != "" {
			return "", "", errors.New("unexpected trailing tokens after angle-bracketed email address")
		}
		addrSpec = strings.TrimSpace(s[lt+1 : gt])
		namePart := strings.TrimSpace(s[:lt])
		if strings.ContainsAny(namePart, "<>") {
			return "", "", errors.New("nested angle brackets in display name")
		}
		if len(namePart) >= 2 && namePart[0] == '"' && namePart[len(namePart)-1] == '"' {
			unq := strings.ReplaceAll(namePart[1:len(namePart)-1], `\"`, `"`)
			unq = strings.ReplaceAll(unq, `\\`, `\`)
			namePart = strings.TrimSpace(unq)
		} else if strings.ContainsAny(namePart, ",;") {
			return "", "", errors.New("unquoted list separator in display name")
		}
		displayName = namePart
	} else if strings.IndexByte(s, '>') >= 0 {
		return "", "", errors.New("stray closing angle bracket in email address")
	}

	if addrSpec == "" || len(addrSpec) > 254 {
		return "", "", errors.New("invalid email address length")
	}
	for i := 0; i < len(addrSpec); i++ {
		b := addrSpec[i]
		if b <= 0x20 || b == 0x7F || b == '<' || b == '>' || b == ',' || b == ';' || b == ':' || b == '"' || b == '\\' {
			return "", "", errors.New("forbidden character in email address")
		}
	}

	at := strings.IndexByte(addrSpec, '@')
	if at <= 0 || at != strings.LastIndexByte(addrSpec, '@') || at == len(addrSpec)-1 {
		return "", "", errors.New("email address must contain exactly one '@' separating local and domain parts")
	}
	local := addrSpec[:at]
	domainPart := addrSpec[at+1:]

	if len(local) > 64 || local[0] == '.' || local[len(local)-1] == '.' || strings.Contains(local, "..") {
		return "", "", errors.New("invalid email local-part")
	}
	if domainPart[0] == '.' || domainPart[len(domainPart)-1] == '.' || strings.Contains(domainPart, "..") || strings.IndexByte(domainPart, '.') < 0 {
		return "", "", errors.New("invalid email domain")
	}
	labels := strings.Split(domainPart, ".")
	for _, lbl := range labels {
		if lbl == "" || len(lbl) > 63 || lbl[0] == '-' || lbl[len(lbl)-1] == '-' {
			return "", "", errors.New("invalid email domain label")
		}
		for i := 0; i < len(lbl); i++ {
			c := lbl[i]
			isAlphaNum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
			if !isAlphaNum && c != '-' && c < 0x80 {
				return "", "", errors.New("invalid character in email domain label")
			}
		}
	}
	return displayName, addrSpec, nil
}

type smtpConn struct {
	conn net.Conn
	r    *bufio.Reader
	ext  map[string]string
}

func newSMTPConn(conn net.Conn) (*smtpConn, error) {
	c := &smtpConn{
		conn: conn,
		r:    bufio.NewReaderSize(conn, 4096),
		ext:  make(map[string]string),
	}
	if _, _, err := c.readResponse(220); err != nil {
		return nil, err
	}
	if err := c.ehlo(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *smtpConn) ehlo() error {
	if err := c.writeCmd("EHLO localhost"); err != nil {
		return err
	}
	code, lines, err := c.readResponse(250)
	if err != nil {
		if code == 500 || code == 502 {
			if hErr := c.writeCmd("HELO localhost"); hErr != nil {
				return hErr
			}
			_, _, hErr := c.readResponse(250)
			return hErr
		}
		return err
	}
	c.ext = make(map[string]string, len(lines))
	for i := 1; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, " ")
		c.ext[strings.ToUpper(k)] = strings.TrimSpace(v)
		if !ok {
			c.ext[strings.ToUpper(line)] = ""
		}
	}
	return nil
}

func (c *smtpConn) extension(name string) (bool, string) {
	v, ok := c.ext[strings.ToUpper(name)]
	return ok, v
}

func (c *smtpConn) startTLS(ctx context.Context, tlsCfg *tls.Config) error {
	if err := c.writeCmd("STARTTLS"); err != nil {
		return err
	}
	if _, _, err := c.readResponse(220); err != nil {
		return err
	}
	tlsConn := tls.Client(c.conn, tlsCfg)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return err
	}
	c.conn = tlsConn
	c.r = bufio.NewReaderSize(tlsConn, 4096)
	return c.ehlo()
}

func (c *smtpConn) auth(username, password string) error {
	if strings.ContainsAny(username, "\r\n\x00") || strings.ContainsAny(password, "\r\n\x00") {
		return errors.New("smtp credentials contain forbidden control characters")
	}
	hasAuth, mechsRaw := c.extension("AUTH")
	useLogin := false
	if hasAuth && mechsRaw != "" {
		hasPlain := false
		hasLogin := false
		for _, m := range strings.Fields(mechsRaw) {
			switch strings.ToUpper(m) {
			case "PLAIN":
				hasPlain = true
			case "LOGIN":
				hasLogin = true
			}
		}
		if !hasPlain && hasLogin {
			useLogin = true
		}
	}
	if useLogin {
		if err := c.writeCmd("AUTH LOGIN"); err != nil {
			return err
		}
		if _, _, err := c.readResponse(334); err != nil {
			return err
		}
		if err := c.writeCmd(base64.StdEncoding.EncodeToString([]byte(username))); err != nil {
			return err
		}
		if _, _, err := c.readResponse(334); err != nil {
			return err
		}
		if err := c.writeCmd(base64.StdEncoding.EncodeToString([]byte(password))); err != nil {
			return err
		}
		_, _, err := c.readResponse(235)
		return err
	}

	plainToken := base64.StdEncoding.EncodeToString([]byte("\x00" + username + "\x00" + password))
	if err := c.writeCmd("AUTH PLAIN " + plainToken); err != nil {
		return err
	}
	code, _, err := c.readResponse(235)
	if err != nil && code == 334 {
		if wErr := c.writeCmd(plainToken); wErr != nil {
			return wErr
		}
		_, _, err = c.readResponse(235)
	}
	return err
}

func (c *smtpConn) mailFrom(addr string) error {
	if err := c.writeCmd("MAIL FROM:<" + addr + ">"); err != nil {
		return err
	}
	_, _, err := c.readResponse(250)
	return err
}

func (c *smtpConn) rcptTo(addr string) error {
	if err := c.writeCmd("RCPT TO:<" + addr + ">"); err != nil {
		return err
	}
	code, lines, err := c.readResponse(0)
	if err != nil {
		return err
	}
	if code != 250 && code != 251 {
		return fmt.Errorf("smtp status %d: %s", code, strings.Join(lines, "; "))
	}
	return nil
}

func (c *smtpConn) data(rawMIME []byte) error {
	if err := c.writeCmd("DATA"); err != nil {
		return err
	}
	if _, _, err := c.readResponse(354); err != nil {
		return err
	}
	bw := bufio.NewWriterSize(c.conn, 8192)
	if err := writeDotStuffedSMTPData(bw, rawMIME); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	_, _, err := c.readResponse(250)
	return err
}

func (c *smtpConn) quit() error {
	_ = c.writeCmd("QUIT")
	_, _, _ = c.readResponse(221)
	return c.close()
}

func (c *smtpConn) close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func (c *smtpConn) writeCmd(cmd string) error {
	if strings.ContainsAny(cmd, "\r\n\x00") {
		return errors.New("smtp command contains CRLF/NUL")
	}
	_, err := c.conn.Write([]byte(cmd + "\r\n"))
	return err
}

func (c *smtpConn) readResponse(expectCode int) (int, []string, error) {
	var lines []string
	var code int
	for len(lines) < 64 {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return 0, nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if len(line) < 3 {
			return 0, nil, fmt.Errorf("malformed smtp response line %q", line)
		}
		parsed, pErr := strconv.Atoi(line[:3])
		if pErr != nil {
			return 0, nil, fmt.Errorf("invalid smtp status code in %q", line)
		}
		code = parsed
		msg := ""
		sep := byte(' ')
		if len(line) > 3 {
			sep = line[3]
			msg = line[4:]
		}
		lines = append(lines, msg)
		if sep != '-' {
			break
		}
	}
	if expectCode > 0 && code != expectCode {
		return code, lines, fmt.Errorf("smtp status %d: %s", code, strings.Join(lines, "; "))
	}
	return code, lines, nil
}

// writeDotStuffedSMTPData writes rawMIME normalized to CRLF line endings with RFC 5321
// transparency dot-stuffing (doubling any leading '.' on a line) followed by "\r\n.\r\n".
func writeDotStuffedSMTPData(bw *bufio.Writer, data []byte) error {
	atLineStart := true
	for i := 0; i < len(data); i++ {
		b := data[i]
		if b == '\r' {
			if i+1 < len(data) && data[i+1] == '\n' {
				i++
			}
			if _, err := bw.WriteString("\r\n"); err != nil {
				return err
			}
			atLineStart = true
			continue
		}
		if b == '\n' {
			if _, err := bw.WriteString("\r\n"); err != nil {
				return err
			}
			atLineStart = true
			continue
		}
		if atLineStart && b == '.' {
			if err := bw.WriteByte('.'); err != nil {
				return err
			}
		}
		atLineStart = false
		if err := bw.WriteByte(b); err != nil {
			return err
		}
	}
	if !atLineStart {
		if _, err := bw.WriteString("\r\n"); err != nil {
			return err
		}
	}
	_, err := bw.WriteString(".\r\n")
	return err
}

func orderAddrsIPv4First(ips []netip.Addr) []netip.Addr {
	out := make([]netip.Addr, 0, len(ips))
	seen := make(map[netip.Addr]bool, len(ips))
	for _, raw := range ips {
		ip := raw.Unmap()
		if !ip.IsValid() || seen[ip] {
			continue
		}
		seen[ip] = true
		out = append(out, ip)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Is4() && !out[j].Is4()
	})
	return out
}

func dialPinnedTargets(
	ctx context.Context,
	addrs []netip.Addr,
	port int,
	dialFn func(ctx context.Context, pinnedAddr string) (net.Conn, error),
) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	portStr := strconv.Itoa(port)
	var lastErr error
	for _, ip := range addrs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		conn, err := dialFn(ctx, net.JoinHostPort(ip.String(), portStr))
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, lastErr
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no resolved target addresses")
	}
	return nil, lastErr
}

func (m *Mailer) resolvePublicOnlyAddrs(ctx context.Context, host string) ([]netip.Addr, error) {
	clean := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if clean == "localhost" || strings.HasSuffix(clean, ".localhost") || strings.HasSuffix(clean, ".internal") {
		return nil, fmt.Errorf("notify: %s SMTP mode refuses internal host %q", "tls/starttls", host)
	}
	ips, err := m.resolver(ctx, host)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("notify: cannot resolve SMTP host %q", host)
	}
	ordered := orderAddrsIPv4First(ips)
	if len(ordered) == 0 {
		return nil, fmt.Errorf("notify: cannot resolve SMTP host %q", host)
	}
	for _, ip := range ordered {
		if err := m.validateSMTPTargetIP(ip, "tls"); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}

func (m *Mailer) resolveInternalOnlyAddrs(ctx context.Context, host string) ([]netip.Addr, error) {
	clean := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if (clean == "localhost" || strings.HasSuffix(clean, ".localhost")) && !m.allowTestLoopback(true) {
		return nil, fmt.Errorf("notify: plain_internal SMTP mode refuses loopback host %q", host)
	}
	ips, err := m.resolver(ctx, host)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("notify: cannot resolve internal SMTP host %q", host)
	}
	ordered := orderAddrsIPv4First(ips)
	if len(ordered) == 0 {
		return nil, fmt.Errorf("notify: cannot resolve internal SMTP host %q", host)
	}
	for _, ip := range ordered {
		if err := m.validateSMTPTargetIP(ip, "plain_internal"); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}

func sanitizeHeaderToken(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	s = strings.ReplaceAll(s, "\x00", "")
	return strings.TrimSpace(s)
}

func formatAddressHeader(displayName, addr string) string {
	cleanName := sanitizeHeaderToken(displayName)
	if cleanName == "" {
		return "<" + addr + ">"
	}
	return mime.QEncoding.Encode("utf-8", cleanName) + " <" + addr + ">"
}

func buildMIMEMessage(fromName, fromAddr, toName, toAddr string, msg MailMessage) ([]byte, error) {
	subj := mime.QEncoding.Encode("utf-8", sanitizeHeaderToken(msg.Subject))
	textBody := strings.TrimSpace(msg.TextBody)
	htmlBody := strings.TrimSpace(msg.HTMLBody)
	if htmlBody == "" && textBody != "" {
		escaped := html.EscapeString(textBody)
		escaped = strings.ReplaceAll(escaped, "\n", "<br>\n")
		htmlBody = `<div style="font:14px/1.7 -apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;color:#1b2422;max-width:600px;padding:16px;">` + escaped + `</div>`
	}

	boundary := fmt.Sprintf("volo-mime-%d", time.Now().UnixNano())
	var buf bytes.Buffer
	buf.WriteString("From: " + formatAddressHeader(fromName, fromAddr) + "\r\n")
	buf.WriteString("To: " + formatAddressHeader(toName, toAddr) + "\r\n")
	buf.WriteString("Subject: " + subj + "\r\n")
	buf.WriteString("Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n")
	buf.WriteString("MIME-Version: 1.0\r\n")
	buf.WriteString("Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n\r\n")

	// Part 1: text/plain
	buf.WriteString("--" + boundary + "\r\n")
	buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	buf.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
	qpwText := quotedprintable.NewWriter(&buf)
	_, _ = qpwText.Write([]byte(textBody))
	_ = qpwText.Close()
	buf.WriteString("\r\n")

	// Part 2: text/html
	buf.WriteString("--" + boundary + "\r\n")
	buf.WriteString("Content-Type: text/html; charset=utf-8\r\n")
	buf.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
	qpwHTML := quotedprintable.NewWriter(&buf)
	_, _ = qpwHTML.Write([]byte(htmlBody))
	_ = qpwHTML.Close()
	buf.WriteString("\r\n")

	buf.WriteString("--" + boundary + "--\r\n")
	return buf.Bytes(), nil
}

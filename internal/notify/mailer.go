package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"html"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/netip"
	"net/smtp"
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
// SECURITY RED-LINE: `InsecureSkipVerify` is NEVER enabled. Plaintext SMTP (`plain_internal`)
// is physically restricted to loopback or private RFC1918 network addresses!
type Mailer struct {
	dialTimeout time.Duration
	resolver    func(ctx context.Context, host string) ([]netip.Addr, error)
	dialFn      func(ctx context.Context, network, addr string) (net.Conn, error)
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

	fromAddr, err := mail.ParseAddress(sanitizeHeaderToken(cfg.SMTPFromEmail))
	if err != nil || fromAddr.Address == "" {
		return errors.New("notify: valid smtp_from_email is required")
	}
	if name := sanitizeHeaderToken(cfg.SMTPFromName); name != "" {
		fromAddr.Name = name
	}
	toAddr, err := mail.ParseAddress(sanitizeHeaderToken(msg.To))
	if err != nil || toAddr.Address == "" {
		return errors.New("notify: invalid recipient email address")
	}

	rawMIME, err := buildMIMEMessage(fromAddr, toAddr, msg)
	if err != nil {
		return err
	}

	dialer := &net.Dialer{Timeout: m.dialTimeout}
	dialRaw := dialer.DialContext
	if m.dialFn != nil {
		dialRaw = m.dialFn
	}

	tlsCfg := &tls.Config{
		ServerName:         host,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: false, // Hard invariant: never skip certificate verification
	}

	var client *smtp.Client
	switch secMode {
	case "tls":
		resolvedIPs, rErr := m.resolvePublicOnlyAddrs(ctx, host)
		if rErr != nil {
			return rErr
		}
		tlsDialer := &tls.Dialer{NetDialer: dialer, Config: tlsCfg}
		conn, dErr := dialPinnedTargets(ctx, resolvedIPs, port, func(dCtx context.Context, pinnedAddr string) (net.Conn, error) {
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
		c, cErr := smtp.NewClient(conn, host)
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
		c, cErr := smtp.NewClient(conn, host)
		if cErr != nil {
			_ = conn.Close()
			return fmt.Errorf("notify: smtp client init failed: %w", cErr)
		}
		if ok, _ := c.Extension("STARTTLS"); !ok {
			_ = c.Close()
			return errors.New("notify: server does not advertise STARTTLS")
		}
		if sErr := c.StartTLS(tlsCfg); sErr != nil {
			_ = c.Close()
			return fmt.Errorf("notify: STARTTLS handshake failed: %w", sErr)
		}
		client = c

	case "plain_internal":
		// Enforce loopback / RFC1918 internal-network-only boundary!
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
		c, cErr := smtp.NewClient(conn, host)
		if cErr != nil {
			_ = conn.Close()
			return fmt.Errorf("notify: internal smtp client init failed: %w", cErr)
		}
		client = c

	default:
		return fmt.Errorf("notify: unsupported smtp_security mode %q", cfg.SMTPSecurity)
	}
	defer client.Close()

	if cfg.SMTPAuth && strings.TrimSpace(cfg.SMTPUsername) != "" {
		auth := smtp.PlainAuth("", strings.TrimSpace(cfg.SMTPUsername), smtpPassword, host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("notify: smtp auth failed: %w", err)
		}
	}

	if err := client.Mail(fromAddr.Address); err != nil {
		return fmt.Errorf("notify: MAIL FROM failed: %w", err)
	}
	if err := client.Rcpt(toAddr.Address); err != nil {
		return fmt.Errorf("notify: RCPT TO failed: %w", err)
	}
	wc, err := client.Data()
	if err != nil {
		return fmt.Errorf("notify: DATA command failed: %w", err)
	}
	if _, err := wc.Write(rawMIME); err != nil {
		_ = wc.Close()
		return fmt.Errorf("notify: write body failed: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("notify: close DATA failed: %w", err)
	}
	return client.Quit()
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
		if !security.IsPublicRoutableIP(ip) {
			return nil, fmt.Errorf("notify: tls/starttls SMTP mode refuses non-public IP %s", ip.String())
		}
	}
	return ordered, nil
}

func (m *Mailer) resolveInternalOnlyAddrs(ctx context.Context, host string) ([]netip.Addr, error) {
	ips, err := m.resolver(ctx, host)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("notify: cannot resolve internal SMTP host %q", host)
	}
	ordered := orderAddrsIPv4First(ips)
	if len(ordered) == 0 {
		return nil, fmt.Errorf("notify: cannot resolve internal SMTP host %q", host)
	}
	for _, ip := range ordered {
		if !ip.IsLoopback() && !ip.IsPrivate() {
			return nil, fmt.Errorf("notify: plain_internal SMTP mode refuses public IP %s (use tls or starttls)", ip.String())
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

func buildMIMEMessage(from, to *mail.Address, msg MailMessage) ([]byte, error) {
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
	buf.WriteString("From: " + from.String() + "\r\n")
	buf.WriteString("To: " + to.String() + "\r\n")
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

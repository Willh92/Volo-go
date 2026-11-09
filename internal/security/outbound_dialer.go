package security

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// ErrSSRFBlocked is returned whenever an outbound connection targets a private, loopback,
// link-local, cloud-metadata, multicast, or non-whitelisted address/host.
var ErrSSRFBlocked = errors.New("security: outbound target blocked by SSRF firewall")

var blockedSpecialPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "This" network
	netip.MustParsePrefix("10.0.0.0/8"),      // RFC 1918 private
	netip.MustParsePrefix("100.64.0.0/10"),   // RFC 6598 CGNAT
	netip.MustParsePrefix("127.0.0.0/8"),     // Loopback
	netip.MustParsePrefix("169.254.0.0/16"),  // Link-local & cloud metadata (169.254.169.254)
	netip.MustParsePrefix("172.16.0.0/12"),   // RFC 1918 private
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
	netip.MustParsePrefix("192.168.0.0/16"),  // RFC 1918 private
	netip.MustParsePrefix("198.18.0.0/15"),   // Network benchmark
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
	netip.MustParsePrefix("224.0.0.0/4"),     // Multicast
	netip.MustParsePrefix("240.0.0.0/4"),     // Reserved / broadcast
	netip.MustParsePrefix("::/128"),          // Unspecified IPv6
	netip.MustParsePrefix("::1/128"),         // Loopback IPv6
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64 well-known prefix
	netip.MustParsePrefix("100::/64"),        // Discard-only
	netip.MustParsePrefix("2001:db8::/32"),   // Documentation IPv6
	netip.MustParsePrefix("fc00::/7"),        // Unique local (ULA)
	netip.MustParsePrefix("fe80::/10"),       // Link-local IPv6
	netip.MustParsePrefix("ff00::/8"),        // Multicast IPv6
}

// ResolverFunc abstracts DNS lookup so unit tests can simulate DNS rebinding deterministically.
type ResolverFunc func(ctx context.Context, host string) ([]netip.Addr, error)

// OutboundSafeDialer enforces connection-level SSRF protection with DNS-pinning (zero TOCTOU rebinding)
// and optional host allowlists (for sandboxed external plugins).
type OutboundSafeDialer struct {
	AllowedHosts          []string
	Timeout               time.Duration
	Resolver              ResolverFunc
	AllowLoopbackForTests bool // Strictly false in production; only for local httptest unit tests
}

// IsPublicRoutableIP returns true ONLY if addr is a globally routable unicast public IP.
func IsPublicRoutableIP(addr netip.Addr) bool {
	return !IsForbiddenIP(addr)
}

// CheckHostAllowed verifies host against AllowedHosts (supports exact match or "*.example.com" wildcard).
// When AllowedHosts is empty, any public hostname is allowed (subject to IP validation).
func (d *OutboundSafeDialer) CheckHostAllowed(host string) error {
	clean := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if clean == "" || clean == "localhost" || strings.HasSuffix(clean, ".localhost") || strings.HasSuffix(clean, ".internal") {
		if d.AllowLoopbackForTests && (clean == "127.0.0.1" || clean == "::1" || clean == "localhost") {
			return nil
		}
		return fmt.Errorf("%w: forbidden internal hostname %q", ErrSSRFBlocked, host)
	}
	if len(d.AllowedHosts) == 0 {
		return nil
	}
	for _, pat := range d.AllowedHosts {
		p := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(pat), "."))
		if p == "" {
			continue
		}
		if p == clean {
			return nil
		}
		if strings.HasPrefix(p, "*.") {
			suffix := p[1:] // ".example.com"
			if strings.HasSuffix(clean, suffix) && len(clean) > len(suffix) {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: host %q is not in allowed_hosts", ErrSSRFBlocked, host)
}

// DialContext resolves the target hostname once, rejects ALL resolved IPs if ANY resolved IP is
// non-public (defeating multi-A-record SSRF), and dials the pinned IP directly (defeating DNS rebinding).
func (d *OutboundSafeDialer) DialContext(ctx context.Context, network string, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("%w: unsupported network %q", ErrSSRFBlocked, network)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("security: invalid dial address %q: %w", addr, err)
	}
	if err := d.CheckHostAllowed(host); err != nil {
		return nil, err
	}

	var ips []netip.Addr
	if literal, lErr := netip.ParseAddr(strings.Trim(host, "[]")); lErr == nil {
		ips = []netip.Addr{literal.Unmap()}
	} else {
		resolve := d.Resolver
		if resolve == nil {
			resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
				return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			}
		}
		resolved, rErr := resolve(ctx, host)
		if rErr != nil {
			return nil, fmt.Errorf("security: dns lookup %q: %w", host, rErr)
		}
		if len(resolved) == 0 {
			return nil, fmt.Errorf("%w: %q resolved to zero addresses", ErrSSRFBlocked, host)
		}
		ips = make([]netip.Addr, len(resolved))
		for i, a := range resolved {
			ips[i] = a.Unmap()
		}
	}

	// Strict all-IP check: if ANY DNS record points to private/metadata space, block immediately!
	for _, ip := range ips {
		if d.AllowLoopbackForTests && ip.IsLoopback() {
			continue
		}
		if !IsPublicRoutableIP(ip) {
			return nil, fmt.Errorf("%w: resolved address %s for %q is non-public", ErrSSRFBlocked, ip.String(), host)
		}
	}

	timeout := d.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	baseDialer := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 30 * time.Second,
	}

	// Pin the validated IP directly so the OS never performs a second DNS lookup!
	var lastErr error
	for _, ip := range ips {
		pinnedTarget := net.JoinHostPort(ip.String(), port)
		conn, dialErr := baseDialer.DialContext(ctx, network, pinnedTarget)
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	return nil, lastErr
}

// ValidateOutboundURL ensures rawURL uses http or https without embedded credentials and passes host policy.
func (d *OutboundSafeDialer) ValidateOutboundURL(rawURL string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("%w: invalid URL: %v", ErrSSRFBlocked, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("%w: scheme %q not permitted (only http/https)", ErrSSRFBlocked, u.Scheme)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%w: embedded URL userinfo credentials prohibited", ErrSSRFBlocked)
	}
	host := u.Hostname()
	if err := d.CheckHostAllowed(host); err != nil {
		return nil, err
	}
	if literal, lErr := netip.ParseAddr(host); lErr == nil {
		if !(d.AllowLoopbackForTests && literal.Unmap().IsLoopback()) && !IsPublicRoutableIP(literal) {
			return nil, fmt.Errorf("%w: literal IP %s is non-public", ErrSSRFBlocked, literal.String())
		}
	}
	return u, nil
}

type approvedProxyAddrKey struct{}

func canonicalProxyHostPort(u *url.URL) string {
	if u == nil {
		return ""
	}
	host := u.Hostname()
	if host == "" {
		return ""
	}
	if port := u.Port(); port != "" {
		return net.JoinHostPort(host, port)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return net.JoinHostPort(host, "443")
	case "socks5", "socks5h":
		return net.JoinHostPort(host, "1080")
	default:
		return net.JoinHostPort(host, "80")
	}
}

// NewHTTPClient builds a hardened *http.Client using DNS-pinned SSRF protection, strict TLS>=1.2
// certificate verification (InsecureSkipVerify=false ALWAYS), and max 3 validated redirects.
func (d *OutboundSafeDialer) NewHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	dialTimeout := d.Timeout
	if dialTimeout <= 0 {
		dialTimeout = 10 * time.Second
	}
	proxySocketDialer := &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: 30 * time.Second,
	}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if approvedProxy, ok := ctx.Value(approvedProxyAddrKey{}).(string); ok && approvedProxy != "" && addr == approvedProxy {
				return proxySocketDialer.DialContext(ctx, network, addr)
			}
			return d.DialContext(ctx, network, addr)
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   8 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: timeout,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: false,
		},
	}
	// Note: when HTTP_PROXY is NOT used or when dialing directly, DialContext enforces IP pinning;
	// additionally CheckRedirect + RoundTripper pre-validate every target URL & DNS resolution!
	return &http.Client{
		Timeout:   timeout,
		Transport: &ssrfValidatingRoundTripper{dialer: d, next: transport},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("security: stopped after 3 redirects")
			}
			_, err := d.ValidateOutboundURL(req.URL.String())
			return err
		},
	}
}

type ssrfValidatingRoundTripper struct {
	dialer *OutboundSafeDialer
	next   http.RoundTripper
}

func (rt *ssrfValidatingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := rt.dialer.ValidateOutboundURL(req.URL.String())
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	if err := rt.dialer.CheckHostAllowed(host); err != nil {
		return nil, err
	}
	// Always resolve and validate non-literal target hostnames against private/loopback/link-local/metadata
	// IP ranges, even when an outbound HTTP_PROXY environment variable is configured.
	if _, lErr := netip.ParseAddr(host); lErr != nil {
		resolve := rt.dialer.Resolver
		if resolve == nil {
			resolve = func(ctx context.Context, h string) ([]netip.Addr, error) {
				return net.DefaultResolver.LookupNetIP(ctx, "ip", h)
			}
		}
		ips, rErr := resolve(req.Context(), host)
		if rErr != nil {
			return nil, fmt.Errorf("security: dns lookup %q: %w", host, rErr)
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("%w: %q resolved to zero addresses", ErrSSRFBlocked, host)
		}
		for _, ip := range ips {
			if !(rt.dialer.AllowLoopbackForTests && ip.Unmap().IsLoopback()) && !IsPublicRoutableIP(ip) {
				return nil, fmt.Errorf("%w: resolved address %s for %q is non-public", ErrSSRFBlocked, ip.String(), host)
			}
		}
	}

	// Only after the target req.URL.Hostname() has passed host & IP policy, allow the underlying
	// transport to connect to a local/corporate HTTP proxy socket if configured in environment.
	if proxyURL, pErr := http.ProxyFromEnvironment(req); pErr == nil && proxyURL != nil {
		if proxyAddr := canonicalProxyHostPort(proxyURL); proxyAddr != "" {
			req = req.WithContext(context.WithValue(req.Context(), approvedProxyAddrKey{}, proxyAddr))
		}
	}
	return rt.next.RoundTrip(req)
}

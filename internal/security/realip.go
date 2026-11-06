package security

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/Willh92/Volo-go/internal/config"
)

// ResolveClientIP extracts the trustworthy client IP address from r.
// Forwarded headers (X-Real-IP / X-Forwarded-For) are ONLY honored when the direct peer
// matches policy (either Unix Domain Socket with policy.TrustUnix, or an IP inside policy.Prefixes).
// Untrusted peers attempting to spoof X-Real-IP or X-Forwarded-For are ignored.
func ResolveClientIP(r *http.Request, policy config.TrustedProxiesPolicy) netip.Addr {
	if r == nil {
		return netip.IPv4Unspecified()
	}

	remote := strings.TrimSpace(r.RemoteAddr)
	isUnixPeer := remote == "" || remote == "@" || remote == "unix" || strings.HasPrefix(remote, "/")

	var peerAddr netip.Addr
	if !isUnixPeer {
		hostStr := remote
		if h, _, err := net.SplitHostPort(remote); err == nil {
			hostStr = h
		}
		// Strip IPv6 zone if present (e.g., fe80::1%eth0)
		if idx := strings.IndexByte(hostStr, '%'); idx > 0 {
			hostStr = hostStr[:idx]
		}
		if parsed, err := netip.ParseAddr(strings.Trim(hostStr, "[]")); err == nil {
			peerAddr = parsed.Unmap()
		}
	}

	trustedPeer := false
	if isUnixPeer {
		trustedPeer = policy.TrustUnix
	} else if peerAddr.IsValid() {
		for _, pfx := range policy.Prefixes {
			if pfx.Contains(peerAddr) {
				trustedPeer = true
				break
			}
		}
	}

	if trustedPeer {
		// Walk X-Forwarded-For right-to-left to pick the right-most non-trusted hop first,
		// falling back to X-Real-IP if XFF is absent.
		if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
			parts := strings.Split(xff, ",")
			var firstValid netip.Addr
			for i := len(parts) - 1; i >= 0; i-- {
				cand, ok := parseCleanAddr(parts[i])
				if !ok {
					continue
				}
				if !firstValid.IsValid() {
					firstValid = cand
				}
				if !isTrustedAddr(cand, policy.Prefixes) {
					return cand
				}
			}
			if firstValid.IsValid() {
				return firstValid
			}
		}
		if xri, ok := parseCleanAddr(r.Header.Get("X-Real-IP")); ok {
			return xri
		}
	}

	if peerAddr.IsValid() {
		return peerAddr
	}
	return netip.MustParseAddr("127.0.0.1")
}

func parseCleanAddr(raw string) (netip.Addr, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return netip.Addr{}, false
	}
	if h, _, err := net.SplitHostPort(s); err == nil {
		s = h
	}
	if idx := strings.IndexByte(s, '%'); idx > 0 {
		s = s[:idx]
	}
	a, err := netip.ParseAddr(strings.Trim(s, "[]"))
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

func isTrustedAddr(addr netip.Addr, prefixes []netip.Prefix) bool {
	for _, pfx := range prefixes {
		if pfx.Contains(addr) {
			return true
		}
	}
	return false
}

// MaskIP anonymizes an IP address for privacy-safe display (`203.0.*.*` or `2001:db8:1::/48`).
func MaskIP(addr netip.Addr) string {
	if !addr.IsValid() {
		return ""
	}
	u := addr.Unmap()
	if u.Is4() {
		b := u.As4()
		return fmt.Sprintf("%d.%d.*.*", b[0], b[1])
	}
	pfx, err := u.Prefix(48)
	if err != nil {
		return u.String()
	}
	return pfx.Masked().String()
}

// RateLimitBucketKey normalizes IPv4 to /32 and IPv6 to /64 subnet prefix so an attacker
// rotating SLAAC IPv6 addresses within a single /64 allocation shares one rate-limit & ban budget.
func RateLimitBucketKey(addr netip.Addr) string {
	if !addr.IsValid() {
		return "0.0.0.0/32"
	}
	u := addr.Unmap()
	if u.Is4() {
		return u.String() + "/32"
	}
	pfx, err := u.Prefix(64)
	if err != nil {
		return u.String()
	}
	return pfx.Masked().String()
}

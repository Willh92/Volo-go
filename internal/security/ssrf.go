package security

import "net/netip"

// IsForbiddenIP reports whether ip is non-routable or reserved (invalid, unspecified, loopback,
// RFC1918 private, link-local / cloud metadata 169.254.0.0/16 & fe80::/10, RFC6598 CGNAT 100.64.0.0/10,
// benchmark 198.18.0.0/15, multicast, documentation/TEST-NET, ULA fc00::/7, NAT64, or IPv4-mapped IPv6).
func IsForbiddenIP(ip netip.Addr) bool {
	if !ip.IsValid() {
		return true
	}
	u := ip.Unmap()
	if u.IsLoopback() || u.IsPrivate() || u.IsLinkLocalUnicast() || u.IsLinkLocalMulticast() ||
		u.IsMulticast() || u.IsUnspecified() {
		return true
	}
	for _, pfx := range blockedSpecialPrefixes {
		if pfx.Contains(u) {
			return true
		}
	}
	return false
}

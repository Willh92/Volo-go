package security

import (
	"net/netip"
	"testing"
)

func TestIsForbiddenIP_ZoneIDAndSpecialRanges(t *testing.T) {
	forbidden := []string{
		"127.0.0.1",
		"10.0.0.1",
		"169.254.169.254",
		"100.64.0.1",
		"192.168.1.1",
		"172.16.0.1",
		"::1",
		"fe80::1",
		"fc00::1",
		"64:ff9b::a9fe:a9fe",
		"64:ff9b::a9fe:a9fe%eth0",
		"fe80::1%lo0",
		"2001:db8::1%eth0",
		"2606:4700:4700::1111%eth0", // Even globally routable IPv6 with zone ID must be rejected!
		"::ffff:169.254.169.254",
	}
	for _, s := range forbidden {
		ip, err := netip.ParseAddr(s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		if !IsForbiddenIP(ip) {
			t.Fatalf("expected IsForbiddenIP(%q) == true", s)
		}
	}

	if !IsForbiddenIP(netip.Addr{}) {
		t.Fatal("expected invalid zero netip.Addr{} to be forbidden")
	}

	allowed := []string{
		"1.1.1.1",
		"8.8.8.8",
		"2606:4700:4700::1111",
	}
	for _, s := range allowed {
		ip := netip.MustParseAddr(s)
		if IsForbiddenIP(ip) {
			t.Fatalf("expected IsForbiddenIP(%q) == false", s)
		}
	}
}

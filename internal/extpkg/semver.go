package extpkg

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/Willh92/Volo-go/internal/security"
)

var publicHTTPURLShapeRe = regexp.MustCompile(`^(?i)https?://[A-Za-z0-9._:\[\]-]+(?:[/?#][^\s\x00-\x1f\x7f]*)?$`)

type parsedSemVer struct {
	major uint64
	minor uint64
	patch uint64
	pre   string
}

// ParseSemVer parses a SemVer 2.0.0 version string (allowing optional leading 'v'/'V'
// and 2-segment "major.minor" shorthand treated as "major.minor.0").
func parseSemVer(raw string) (parsedSemVer, bool) {
	s := strings.TrimSpace(raw)
	if len(s) > 0 && (s[0] == 'v' || s[0] == 'V') {
		s = s[1:]
	}
	if s == "" || len(s) > 128 {
		return parsedSemVer{}, false
	}
	// Strip +build metadata (ignored for version precedence per SemVer 2.0.0 §10).
	if plus := strings.IndexByte(s, '+'); plus >= 0 {
		if plus == len(s)-1 {
			return parsedSemVer{}, false
		}
		s = s[:plus]
	}

	var pre string
	if dash := strings.IndexByte(s, '-'); dash >= 0 {
		pre = s[dash+1:]
		if pre == "" {
			return parsedSemVer{}, false
		}
		s = s[:dash]
	}

	parts := strings.Split(s, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return parsedSemVer{}, false
	}
	maj, ok1 := parseSemVerUint(parts[0])
	min, ok2 := parseSemVerUint(parts[1])
	if !ok1 || !ok2 {
		return parsedSemVer{}, false
	}
	var pat uint64
	if len(parts) == 3 {
		var ok3 bool
		pat, ok3 = parseSemVerUint(parts[2])
		if !ok3 {
			return parsedSemVer{}, false
		}
	}

	if pre != "" {
		start := 0
		for i := 0; i <= len(pre); i++ {
			if i == len(pre) || pre[i] == '.' {
				seg := pre[start:i]
				if seg == "" {
					return parsedSemVer{}, false
				}
				for j := 0; j < len(seg); j++ {
					c := seg[j]
					if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '-') {
						return parsedSemVer{}, false
					}
				}
				start = i + 1
			}
		}
	}
	return parsedSemVer{major: maj, minor: min, patch: pat, pre: pre}, true
}

func parseSemVerUint(s string) (uint64, bool) {
	if s == "" || len(s) > 18 {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// CompareSemVer compares two SemVer strings according to SemVer 2.0.0 precedence rules.
// Returns (-1, true) if a < b, (0, true) if a == b, (1, true) if a > b, or (0, false) if either is invalid.
func CompareSemVer(a, b string) (int, bool) {
	va, okA := parseSemVer(a)
	vb, okB := parseSemVer(b)
	if !okA || !okB {
		return 0, false
	}
	if va.major != vb.major {
		if va.major < vb.major {
			return -1, true
		}
		return 1, true
	}
	if va.minor != vb.minor {
		if va.minor < vb.minor {
			return -1, true
		}
		return 1, true
	}
	if va.patch != vb.patch {
		if va.patch < vb.patch {
			return -1, true
		}
		return 1, true
	}
	if va.pre == vb.pre {
		return 0, true
	}
	// Normal release has higher precedence than pre-release (1.0.0 > 1.0.0-rc.1).
	if va.pre == "" {
		return 1, true
	}
	if vb.pre == "" {
		return -1, true
	}
	return comparePreRelease(va.pre, vb.pre), true
}

func comparePreRelease(a, b string) int {
	pa := strings.Split(a, ".")
	pb := strings.Split(b, ".")
	limit := len(pa)
	if len(pb) < limit {
		limit = len(pb)
	}
	for i := 0; i < limit; i++ {
		sa, sb := pa[i], pb[i]
		if sa == sb {
			continue
		}
		na, okA := parseSemVerUint(sa)
		nb, okB := parseSemVerUint(sb)
		switch {
		case okA && okB:
			if na < nb {
				return -1
			}
			return 1
		case okA && !okB:
			// Numeric identifiers always have lower precedence than non-numeric identifiers.
			return -1
		case !okA && okB:
			return 1
		default:
			if sa < sb {
				return -1
			}
			return 1
		}
	}
	if len(pa) < len(pb) {
		return -1
	}
	if len(pa) > len(pb) {
		return 1
	}
	return 0
}

// IsNewerVersion reports whether latest is strictly newer than current using SemVer 2.0.0.
func IsNewerVersion(current, latest string) bool {
	cmp, ok := CompareSemVer(latest, current)
	return ok && cmp > 0
}

// NormalizeVersion strips a leading 'v' or 'V' while keeping valid SemVer core + prerelease intact.
func NormalizeVersion(raw string) string {
	s := strings.TrimSpace(raw)
	if len(s) > 1 && (s[0] == 'v' || s[0] == 'V') && s[1] >= '0' && s[1] <= '9' {
		return s[1:]
	}
	return s
}

// IsHostCompatible checks whether hostVersion satisfies minHostVersion.
// Unversioned development builds ("" or "dev*") always pass so local testing is unimpeded.
func IsHostCompatible(hostVersion, minHostVersion string) bool {
	minHost := strings.TrimSpace(minHostVersion)
	if minHost == "" {
		return true
	}
	if _, ok := parseSemVer(minHost); !ok {
		return false
	}
	host := strings.TrimSpace(hostVersion)
	lowHost := strings.ToLower(host)
	if host == "" || lowHost == "dev" || strings.HasPrefix(lowHost, "dev-") {
		return true
	}
	cmp, ok := CompareSemVer(host, minHost)
	return ok && cmp >= 0
}

// ValidatePublicHTTPURL validates that rawURL is a well-formed http/https URL without embedded credentials
// and whose hostname is not a private/loopback/internal target (unless allowLoopback is true for tests).
func ValidatePublicHTTPURL(rawURL string, allowLoopback bool) (*url.URL, error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" || len(trimmed) > 2048 {
		return nil, fmt.Errorf("url must be between 1 and 2048 characters")
	}
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] < 0x20 || trimmed[i] == 0x7f {
			return nil, fmt.Errorf("url must not contain control characters")
		}
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return nil, fmt.Errorf("url scheme %q must be http or https", u.Scheme)
	}
	if u.User != nil {
		return nil, fmt.Errorf("url must not embed credentials")
	}
	if strings.Contains(u.Host, "%") || strings.Contains(u.Hostname(), "%") {
		return nil, fmt.Errorf("%w: IPv6 zone identifier in host %q is forbidden", security.ErrSSRFBlocked, u.Host)
	}
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(u.Hostname()), "."))
	if host == "" {
		return nil, fmt.Errorf("url missing hostname")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		host == "local" || strings.HasSuffix(host, ".local") ||
		host == "internal" || strings.HasSuffix(host, ".internal") {
		if !(allowLoopback && host == "localhost") {
			return nil, fmt.Errorf("%w: internal hostname %q is forbidden", security.ErrSSRFBlocked, host)
		}
	} else if ip, ipErr := netip.ParseAddr(host); ipErr == nil {
		if !(allowLoopback && ip.Zone() == "" && ip.Unmap().IsLoopback()) && !security.IsPublicRoutableIP(ip) {
			return nil, fmt.Errorf("%w: non-public IP %q is forbidden", security.ErrSSRFBlocked, host)
		}
	}
	cleanURL := u.String()
	if !publicHTTPURLShapeRe.MatchString(cleanURL) {
		return nil, fmt.Errorf("invalid public http/https url format")
	}
	return url.Parse(cleanURL)
}

// SanitizeHTTPURL trims raw, rejects empty strings, ASCII control characters, non-http(s) schemes,
// embedded credentials, IPv6 zone identifiers, or missing hostnames, and returns the trimmed URL (or "" if unsafe).
func SanitizeHTTPURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" || len(s) > 2048 {
		return ""
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return ""
		}
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "http" && scheme != "https") || u.Hostname() == "" || u.User != nil || strings.Contains(u.Host, "%") {
		return ""
	}
	return s
}

// SanitizeZipPath validates a ZIP archive entry path against Zip-Slip traversal, backslashes, leading slashes,
// ASCII control characters, Windows illegal path characters, trailing dots/spaces, and Windows reserved device names.
func SanitizeZipPath(name string) (string, error) {
	if name == "" {
		return "", errors.New("extpkg: empty zip entry path")
	}
	if strings.HasPrefix(name, "/") || strings.ContainsRune(name, '\\') {
		return "", fmt.Errorf("extpkg: invalid zip entry path %q", name)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c < 0x20 || c == 0x7f {
			return "", fmt.Errorf("extpkg: control character in zip entry path %q", name)
		}
	}
	if strings.ContainsAny(name, ":<>\"|?*") {
		return "", fmt.Errorf("extpkg: illegal character in zip entry path %q", name)
	}

	trimmed := strings.TrimSuffix(name, "/")
	if trimmed == "" || strings.HasSuffix(trimmed, "/") {
		return "", fmt.Errorf("extpkg: empty segment in zip entry path %q", name)
	}

	parts := strings.Split(trimmed, "/")
	for _, seg := range parts {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("extpkg: forbidden traversal segment in zip entry path %q", name)
		}
		if strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ") {
			return "", fmt.Errorf("extpkg: segment has trailing dot or space in zip entry path %q", name)
		}
		if isWindowsReservedDeviceSegment(seg) {
			return "", fmt.Errorf("extpkg: reserved device name in zip entry path %q", name)
		}
	}
	return strings.Join(parts, "/"), nil
}

func isWindowsReservedDeviceSegment(seg string) bool {
	stem := seg
	if dot := strings.IndexByte(stem, '.'); dot >= 0 {
		stem = stem[:dot]
	}
	stem = strings.ToUpper(strings.TrimSpace(stem))
	switch stem {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) {
		d := stem[3]
		if d >= '1' && d <= '9' {
			return true
		}
	}
	return false
}

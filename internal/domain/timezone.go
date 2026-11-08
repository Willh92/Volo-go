package domain

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

var siteLocCache sync.Map // map[string]*time.Location

// ianaFallbackOffsets provides standard UTC offsets (in seconds) for common IANA timezones
// when running a standalone static binary on Windows hosts without $GOROOT/lib/time/zoneinfo.zip.
var ianaFallbackOffsets = map[string]int{
	"asia/shanghai":                  8 * 3600,
	"asia/chongqing":                 8 * 3600,
	"asia/harbin":                    8 * 3600,
	"asia/urumqi":                    8 * 3600,
	"asia/hong_kong":                 8 * 3600,
	"asia/macau":                     8 * 3600,
	"asia/taipei":                    8 * 3600,
	"asia/singapore":                 8 * 3600,
	"asia/kuala_lumpur":              8 * 3600,
	"asia/manila":                    8 * 3600,
	"australia/perth":                8 * 3600,
	"prc":                            8 * 3600,
	"rok":                            9 * 3600,
	"asia/tokyo":                     9 * 3600,
	"asia/seoul":                     9 * 3600,
	"japan":                          9 * 3600,
	"asia/bangkok":                   7 * 3600,
	"asia/ho_chi_minh":               7 * 3600,
	"asia/saigon":                    7 * 3600,
	"asia/jakarta":                   7 * 3600,
	"asia/kolkata":                   5*3600 + 1800,
	"asia/calcutta":                  5*3600 + 1800,
	"asia/colombo":                   5*3600 + 1800,
	"asia/dubai":                     4 * 3600,
	"europe/moscow":                  3 * 3600,
	"europe/istanbul":                3 * 3600,
	"europe/athens":                  2 * 3600,
	"africa/cairo":                   2 * 3600,
	"europe/berlin":                  1 * 3600,
	"europe/paris":                   1 * 3600,
	"europe/rome":                    1 * 3600,
	"europe/madrid":                  1 * 3600,
	"europe/amsterdam":               1 * 3600,
	"europe/zurich":                  1 * 3600,
	"europe/london":                  0,
	"europe/dublin":                  0,
	"europe/lisbon":                  0,
	"america/sao_paulo":              -3 * 3600,
	"america/argentina/buenos_aires": -3 * 3600,
	"america/new_york":               -5 * 3600,
	"america/toronto":                -5 * 3600,
	"america/chicago":                -6 * 3600,
	"america/mexico_city":            -6 * 3600,
	"america/denver":                 -7 * 3600,
	"america/los_angeles":            -8 * 3600,
	"america/vancouver":              -8 * 3600,
	"australia/sydney":               10 * 3600,
	"australia/melbourne":            10 * 3600,
	"pacific/auckland":               12 * 3600,
}

// ResolveSiteLocation resolves an IANA timezone or fixed UTC/GMT offset with caching.
// When OS zoneinfo files are unavailable (e.g. portable Windows .exe), common IANA zones
// and UTC/GMT±HH:MM expressions resolve deterministically instead of diverging across layers.
// Unrecognized timezone identifiers fall back safely to time.UTC.
func ResolveSiteLocation(tz string) *time.Location {
	trimmed := strings.TrimSpace(tz)
	if trimmed == "" || strings.EqualFold(trimmed, "UTC") || strings.EqualFold(trimmed, "Etc/UTC") ||
		strings.EqualFold(trimmed, "GMT") || strings.EqualFold(trimmed, "Z") {
		return time.UTC
	}
	if cached, ok := siteLocCache.Load(trimmed); ok {
		return cached.(*time.Location)
	}
	if loc, err := time.LoadLocation(trimmed); err == nil && loc != nil {
		siteLocCache.Store(trimmed, loc)
		return loc
	}
	if loc := parseFallbackLocation(trimmed); loc != nil {
		siteLocCache.Store(trimmed, loc)
		return loc
	}
	return time.UTC
}

func parseFallbackLocation(tz string) *time.Location {
	low := strings.ToLower(strings.TrimSpace(tz))
	if off, ok := ianaFallbackOffsets[low]; ok {
		return time.FixedZone(tz, off)
	}
	switch low {
	case "cst", "hkt", "sgt", "cct":
		return time.FixedZone(tz, 8*3600)
	case "jst", "kst":
		return time.FixedZone(tz, 9*3600)
	}
	rest := low
	for _, pfx := range []string{"etc/gmt", "utc", "gmt"} {
		if strings.HasPrefix(rest, pfx) {
			rest = strings.TrimSpace(rest[len(pfx):])
			if rest == "" {
				return time.UTC
			}
			// Note: POSIX Etc/GMT+8 means UTC-8, whereas user-typed GMT+8 / UTC+8 means +08:00;
			// invert sign only for explicit "etc/gmt" prefix.
			if off, ok := parseSignedOffsetSeconds(rest); ok {
				if pfx == "etc/gmt" {
					off = -off
				}
				return time.FixedZone(tz, off)
			}
			return nil
		}
	}
	if off, ok := parseSignedOffsetSeconds(rest); ok {
		return time.FixedZone(tz, off)
	}
	return nil
}

func parseSignedOffsetSeconds(s string) (int, bool) {
	if len(s) < 2 || (s[0] != '+' && s[0] != '-') {
		return 0, false
	}
	sign := 1
	if s[0] == '-' {
		sign = -1
	}
	body := s[1:]
	var hh, mm int
	var err error
	switch {
	case strings.Contains(body, ":"):
		parts := strings.SplitN(body, ":", 2)
		if len(parts) != 2 {
			return 0, false
		}
		hh, err = strconv.Atoi(parts[0])
		if err != nil {
			return 0, false
		}
		mm, err = strconv.Atoi(parts[1])
		if err != nil {
			return 0, false
		}
	case len(body) == 4:
		hh, err = strconv.Atoi(body[:2])
		if err != nil {
			return 0, false
		}
		mm, err = strconv.Atoi(body[2:4])
		if err != nil {
			return 0, false
		}
	case len(body) == 1 || len(body) == 2:
		hh, err = strconv.Atoi(body)
		if err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	if hh < 0 || hh > 14 || mm < 0 || mm >= 60 {
		return 0, false
	}
	return sign * (hh*3600 + mm*60), true
}

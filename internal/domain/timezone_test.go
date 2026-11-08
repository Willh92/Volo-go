package domain

import (
	"testing"
	"time"
)

func TestResolveSiteLocationAndPureGoZoneinfoFallback(t *testing.T) {
	cases := []struct {
		tz         string
		wantOffset int
	}{
		{"Asia/Shanghai", 8 * 3600},
		{"Asia/Taipei", 8 * 3600},
		{"Asia/Tokyo", 9 * 3600},
		{"ROK", 9 * 3600},
		{"Asia/Kolkata", 5*3600 + 1800},
		{"UTC+8", 8 * 3600},
		{"UTC+08:00", 8 * 3600},
		{"GMT-5", -5 * 3600},
		{"+05:30", 5*3600 + 1800},
		{"-0800", -8 * 3600},
		{"Etc/GMT-8", 8 * 3600},
		{"Etc/GMT+5", -5 * 3600},
		{"CST", 8 * 3600},
	}

	sample := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	for _, tc := range cases {
		fb := parseFallbackLocation(tc.tz)
		if fb == nil {
			t.Fatalf("parseFallbackLocation(%q) returned nil", tc.tz)
		}
		if _, got := sample.In(fb).Zone(); got != tc.wantOffset {
			t.Fatalf("parseFallbackLocation(%q) offset = %d, want %d", tc.tz, got, tc.wantOffset)
		}

		resolved := ResolveSiteLocation(tc.tz)
		if resolved == nil {
			t.Fatalf("ResolveSiteLocation(%q) returned nil", tc.tz)
		}
		if _, got := sample.In(resolved).Zone(); got != tc.wantOffset {
			t.Fatalf("ResolveSiteLocation(%q) offset = %d, want %d", tc.tz, got, tc.wantOffset)
		}
	}

	for _, utcAlias := range []string{"", "UTC", "Etc/UTC", "GMT", "Z", "Unknown/NonExistent_Zone"} {
		loc := ResolveSiteLocation(utcAlias)
		if _, got := sample.In(loc).Zone(); got != 0 {
			t.Fatalf("ResolveSiteLocation(%q) offset = %d, want 0 (UTC)", utcAlias, got)
		}
	}
}

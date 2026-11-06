package guest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
)

type avatarPalette struct {
	bg      string
	primary string
	accent  string
}

// Harmonious volo-studio botanical & mineral palettes (zero external requests!).
var avatarPalettes = []avatarPalette{
	{bg: "#edf3ef", primary: "#246b51", accent: "#78b198"}, // forest
	{bg: "#edf3f7", primary: "#2d6583", accent: "#82b1cc"}, // ocean
	{bg: "#ebf4f4", primary: "#286c70", accent: "#7ab8ba"}, // teal
	{bg: "#f6f1e8", primary: "#855c27", accent: "#cba574"}, // amber
	{bg: "#f3eef6", primary: "#70517d", accent: "#b498c4"}, // plum
	{bg: "#f7eef0", primary: "#87525f", accent: "#cb96a4"}, // rose
}

// RenderAvatarSVG deterministically renders a clean 5x5 symmetric geometric SVG identicon (< 900 bytes)
// from seed without calling Gravatar or any third-party avatar CDN.
func RenderAvatarSVG(seed string) []byte {
	clean := strings.TrimSpace(strings.TrimSuffix(seed, ".svg"))
	if clean == "" {
		clean = "anonymous"
	}
	sum := sha256.Sum256([]byte("volo-avatar:v1:" + clean))
	pal := avatarPalettes[int(sum[0])%len(avatarPalettes)]

	var sb strings.Builder
	sb.Grow(896)
	fmt.Fprintf(&sb, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 80 80" width="80" height="80" role="img" aria-label="avatar"><rect width="80" height="80" rx="18" fill="%s"/>`, pal.bg)

	// 5x5 vertically symmetric grid inside a 10px margin (cell size 12x12)
	bitIdx := 8
	for row := 0; row < 5; row++ {
		for col := 0; col < 3; col++ {
			bytePos := (bitIdx / 8) % len(sum)
			bitOff := uint(bitIdx % 8)
			bitIdx++
			on := (sum[bytePos]>>bitOff)&1 == 1
			if !on {
				continue
			}
			fill := pal.primary
			if (sum[(bytePos+5)%len(sum)]>>bitOff)&1 == 1 {
				fill = pal.accent
			}
			y := 10 + row*12
			xLeft := 10 + col*12
			fmt.Fprintf(&sb, `<rect x="%d" y="%d" width="12" height="12" rx="3" fill="%s"/>`, xLeft, y, fill)
			if col < 2 {
				xRight := 10 + (4-col)*12
				fmt.Fprintf(&sb, `<rect x="%d" y="%d" width="12" height="12" rx="3" fill="%s"/>`, xRight, y, fill)
			}
		}
	}
	sb.WriteString(`</svg>`)
	return []byte(sb.String())
}

// ServeAvatarHTTP serves /avatars/{seed}.svg with strict CSP, ETag, and 1-year immutable caching.
func ServeAvatarHTTP(w http.ResponseWriter, r *http.Request, seed string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	svg := RenderAvatarSVG(seed)
	sum := sha256.Sum256(svg)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`

	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("ETag", etag)

	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(svg)
}

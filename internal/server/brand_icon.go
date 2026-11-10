package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/http"
	"strings"
	"sync"

	"github.com/Willh92/Volo-go/internal/repo"
)

// voloFaviconSVG is the authentic Volo Studio `V.` emblem (#246b51 rounded squircle + `#eef5ee` bold V + period dot).
const voloFaviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64">
  <rect width="64" height="64" rx="18" fill="#246b51"/>
  <path d="m12.5 18 11.5 29h8.5l12.5-29H34.5L28.5 35.5 22.8 18Z" fill="#eef5ee"/>
  <circle cx="48.5" cy="43" r="4.2" fill="#eef5ee"/>
</svg>
`

var (
	faviconOnce    sync.Once
	faviconSVGRaw  []byte
	faviconSVGETag string
	faviconICORaw  []byte
	faviconICOETag string
)

func initFaviconAssets() {
	faviconOnce.Do(func() {
		faviconSVGRaw = []byte(voloFaviconSVG)
		svgSum := sha256.Sum256(faviconSVGRaw)
		faviconSVGETag = fmt.Sprintf(`"%x"`, svgSum[:10])

		faviconICORaw = buildFaviconICO32()
		icoSum := sha256.Sum256(faviconICORaw)
		faviconICOETag = fmt.Sprintf(`"%x"`, icoSum[:10])
	})
}

func (s *AppServer) resolveCustomFaviconRedirect() string {
	if s == nil || s.deps.Settings == nil {
		return ""
	}
	fav, ok := repo.SanitizeAllowedURL(s.deps.Settings.Snapshot().Site.FaviconURL, false)
	if !ok {
		return ""
	}
	switch fav {
	case "", "/favicon.svg", "/favicon.ico", "/assets/favicon.svg":
		return ""
	}
	lower := strings.ToLower(fav)
	if (strings.HasPrefix(fav, "/") && !strings.HasPrefix(fav, "//") && !strings.HasPrefix(fav, `/\`)) ||
		strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return fav
	}
	return ""
}

func (s *AppServer) handleServeFaviconSVG(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if custom := s.resolveCustomFaviconRedirect(); custom != "" {
		w.Header().Set("Cache-Control", "no-cache")
		http.Redirect(w, r, custom, http.StatusFound)
		return
	}
	initFaviconAssets()
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("ETag", faviconSVGETag)
	if strings.TrimSpace(r.Header.Get("If-None-Match")) == faviconSVGETag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(faviconSVGRaw)
}

func (s *AppServer) handleServeFaviconICO(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// Browsers, feed readers and crawlers request /favicon.ico directly; follow any custom site icon
	// (PNG/JPG/SVG/ICO all work through a redirect) instead of always serving the built-in mark.
	if custom := s.resolveCustomFaviconRedirect(); custom != "" {
		w.Header().Set("Cache-Control", "no-cache")
		http.Redirect(w, r, custom, http.StatusFound)
		return
	}
	initFaviconAssets()
	w.Header().Set("Content-Type", "image/x-icon")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("ETag", faviconICOETag)
	if strings.TrimSpace(r.Header.Get("If-None-Match")) == faviconICOETag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(faviconICORaw)
}

func buildFaviconICO32() []byte {
	const size = 32
	const ss = 4
	img := image.NewNRGBA(image.Rect(0, 0, size, size))

	bgCol := [3]float64{0x24, 0x6b, 0x51} // #246b51 Studio Forest
	fgCol := [3]float64{0xee, 0xf5, 0xee} // #eef5ee Studio Mark Light

	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var sumR, sumG, sumB, sumA float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := (float64(px) + (float64(sx)+0.5)/float64(ss)) * 2.0
					y := (float64(py) + (float64(sy)+0.5)/float64(ss)) * 2.0
					if !inRoundedRect(x, y, 0, 0, 64, 64, 18) {
						continue
					}
					col := bgCol
					if inVoloMarkForeground(x, y) {
						col = fgCol
					}
					sumR += col[0]
					sumG += col[1]
					sumB += col[2]
					sumA += 255.0
				}
			}
			samples := float64(ss * ss)
			alpha := sumA / samples
			if alpha <= 0.5 {
				continue
			}
			cov := sumA / 255.0
			img.SetNRGBA(px, py, color.NRGBA{
				R: uint8(math.Round(sumR / cov)),
				G: uint8(math.Round(sumG / cov)),
				B: uint8(math.Round(sumB / cov)),
				A: uint8(math.Round(alpha)),
			})
		}
	}

	var pngBuf bytes.Buffer
	_ = png.Encode(&pngBuf, img)
	pngBytes := pngBuf.Bytes()

	out := make([]byte, 22+len(pngBytes))
	binary.LittleEndian.PutUint16(out[0:2], 0)
	binary.LittleEndian.PutUint16(out[2:4], 1)
	binary.LittleEndian.PutUint16(out[4:6], 1)
	out[6] = size
	out[7] = size
	out[8] = 0
	out[9] = 0
	binary.LittleEndian.PutUint16(out[10:12], 1)
	binary.LittleEndian.PutUint16(out[12:14], 32)
	binary.LittleEndian.PutUint32(out[14:18], uint32(len(pngBytes)))
	binary.LittleEndian.PutUint32(out[18:22], 22)
	copy(out[22:], pngBytes)
	return out
}

func inRoundedRect(x, y, rx, ry, rw, rh, radius float64) bool {
	if x < rx || x > rx+rw || y < ry || y > ry+rh {
		return false
	}
	cx := math.Max(rx+radius, math.Min(x, rx+rw-radius))
	cy := math.Max(ry+radius, math.Min(y, ry+rh-radius))
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= radius*radius
}

func inVoloMarkForeground(x, y float64) bool {
	// Period dot at (48.5, 43), r=4.2
	if (x-48.5)*(x-48.5)+(y-43.0)*(y-43.0) <= 4.2*4.2 {
		return true
	}
	// Bold 'V' centered at x=28.5, y=18..47
	if y < 18.0 || y > 47.0 {
		return false
	}
	t := (y - 18.0) / 29.0
	outerHalf := 16.2*(1-t) + 4.3*t
	dx := math.Abs(x - 28.6)
	if dx > outerHalf {
		return false
	}
	if y < 35.5 {
		innerHalf := 5.9 * (1 - (y-18.0)/(35.5-18.0))
		if dx < innerHalf {
			return false
		}
	}
	return true
}

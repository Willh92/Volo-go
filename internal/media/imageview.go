package media

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HugoSmits86/nativewebp"
)

const (
	defaultImageViewCacheBytes int64 = 8 << 20 // 8 MiB default transformed-bytes LRU cache
	maxImageViewDim                  = 4096
	defaultImageViewQuality          = 85
)

// ImageViewSpec defines a normalized non-destructive real-time image rendering specification.
type ImageViewSpec struct {
	Mode         int      // 0..5 (default 0 when w/h set without mode; -1 when no resize)
	W            int      // 0 or 1..4096
	H            int      // 0 or 1..4096
	Format       string   // "", "jpg", "png", "webp", "avif"
	Interlace    int      // 0 or 1
	Quality      int      // 0 (default 85) or 1..100
	Colors       int      // 0 or 2..256
	Strip        bool     // true when strip requested
	MetaKeepList []string // canonical sorted subset of ["exif", "icc", "xmp"]
	IgnoreError  bool     // true when ignore-error=1
}

// CanonicalKey returns a deterministic string key representing all rendering parameters of s.
// Equivalent Qiniu path (`?imageView2/...`) and KV (`?mode=...&w=...`) queries produce identical keys.
func (s ImageViewSpec) CanonicalKey() string {
	n, _ := normalizeAndValidateSpec(s, true)
	effMode := n.Mode
	if n.W == 0 && n.H == 0 {
		effMode = -1
	} else if effMode < 0 {
		effMode = 0
	}
	effQ := n.Quality
	if effQ == 0 {
		effQ = defaultImageViewQuality
	}
	return fmt.Sprintf("m=%d|w=%d|h=%d|f=%s|il=%d|q=%d|c=%d|st=%t|mk=%s",
		effMode, n.W, n.H, n.Format, n.Interlace, effQ, n.Colors, n.Strip, strings.Join(n.MetaKeepList, ","))
}

// IsTransformableImageExt reports whether ext (with or without leading '.') is a supported image extension
// eligible for real-time ImageView rendering.
func IsTransformableImageExt(ext string) bool {
	clean := strings.ToLower(strings.TrimSpace(ext))
	if clean != "" && !strings.HasPrefix(clean, ".") {
		clean = "." + clean
	}
	switch clean {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif", ".avif":
		return true
	default:
		return false
	}
}

func isKnownImageViewPathKey(tok string) bool {
	switch strings.ToLower(strings.TrimSpace(tok)) {
	case "w", "h", "format", "q", "quality", "interlace", "colors", "strip", "meta-keep-list", "ignore-error":
		return true
	default:
		return false
	}
}

func preExtractIgnoreError(rawQuery string) bool {
	q := strings.TrimPrefix(strings.TrimSpace(rawQuery), "?")
	if q == "" {
		return false
	}
	ignoreErr := false
	for _, seg := range strings.Split(q, "&") {
		if seg == "" {
			continue
		}
		kRaw, vRaw, hasEq := strings.Cut(seg, "=")
		kDec, err1 := url.QueryUnescape(kRaw)
		if err1 != nil {
			kDec = kRaw
		}
		vDec, err2 := url.QueryUnescape(vRaw)
		if err2 != nil {
			vDec = vRaw
		}
		trimmedKey := strings.TrimSpace(kDec)
		lowerKey := strings.ToLower(trimmedKey)

		if lowerKey == "ignore-error" && hasEq {
			v := strings.ToLower(strings.TrimSpace(vDec))
			if v == "1" || v == "true" {
				ignoreErr = true
			} else if v == "0" || v == "false" {
				ignoreErr = false
			}
			continue
		}
		if strings.HasPrefix(lowerKey, "imageview2/") || lowerKey == "imageview2" {
			pathStr := trimmedKey
			if hasEq && vDec != "" && lowerKey == "imageview2" {
				pathStr = "imageView2/" + strings.TrimPrefix(vDec, "/")
			}
			parts := strings.Split(pathStr, "/")
			for i := 2; i+1 < len(parts); i++ {
				if strings.EqualFold(parts[i], "ignore-error") {
					v := strings.ToLower(strings.TrimSpace(parts[i+1]))
					if v == "1" || v == "true" {
						ignoreErr = true
					} else if v == "0" || v == "false" {
						ignoreErr = false
					}
					i++
				}
			}
		}
	}
	return ignoreErr
}

// ParseImageViewSpec parses Qiniu path style (`?imageView2/<mode>/w/...`), standard KV query style
// (`?mode=1&w=960&format=webp`), or a hybrid combination of both.
// If rawQuery contains no `imageView2` directive and none of the recognized ImageView keys,
// it returns `hasTransform = false, err = nil`.
func ParseImageViewSpec(rawQuery string) (spec ImageViewSpec, hasTransform bool, err error) {
	spec.Mode = -1
	q := strings.TrimPrefix(strings.TrimSpace(rawQuery), "?")
	if q == "" {
		return spec, false, nil
	}

	// Extract ignore-error BEFORE validating any other parameter so callers always know whether
	// to fall back to the original image on malformed parameters.
	spec.IgnoreError = preExtractIgnoreError(q)

	var (
		sawMode      bool
		sawTransform bool
		keepSet      map[string]bool
	)

	applyParam := func(key, val string, hasVal bool) error {
		switch key {
		case "mode":
			if !hasVal || val == "" {
				return errors.New("imageview: missing mode value")
			}
			m, pErr := strconv.Atoi(val)
			if pErr != nil || m < 0 || m > 5 {
				return fmt.Errorf("imageview: mode %q out of range [0..5]", val)
			}
			spec.Mode = m
			sawMode = true
		case "w":
			if !hasVal || val == "" {
				return errors.New("imageview: missing width value")
			}
			w, pErr := strconv.Atoi(val)
			if pErr != nil || w <= 0 {
				return fmt.Errorf("imageview: width %q must be a positive integer", val)
			}
			if w > maxImageViewDim {
				w = maxImageViewDim
			}
			spec.W = w
		case "h":
			if !hasVal || val == "" {
				return errors.New("imageview: missing height value")
			}
			h, pErr := strconv.Atoi(val)
			if pErr != nil || h <= 0 {
				return fmt.Errorf("imageview: height %q must be a positive integer", val)
			}
			if h > maxImageViewDim {
				h = maxImageViewDim
			}
			spec.H = h
		case "format":
			if !hasVal || strings.TrimSpace(val) == "" {
				return errors.New("imageview: missing format value")
			}
			f := strings.ToLower(strings.TrimSpace(val))
			switch f {
			case "jpeg", "jpg":
				spec.Format = "jpg"
			case "png", "webp", "avif":
				spec.Format = f
			default:
				return fmt.Errorf("imageview: unsupported format %q", val)
			}
		case "interlace":
			if !hasVal || val == "" {
				return errors.New("imageview: missing interlace value")
			}
			il, pErr := strconv.Atoi(val)
			if pErr != nil || (il != 0 && il != 1) {
				return fmt.Errorf("imageview: interlace %q must be 0 or 1", val)
			}
			spec.Interlace = il
		case "q", "quality":
			if !hasVal || val == "" {
				return errors.New("imageview: missing quality value")
			}
			qv, pErr := strconv.Atoi(val)
			if pErr != nil || qv < 1 || qv > 100 {
				return fmt.Errorf("imageview: quality %q out of range [1..100]", val)
			}
			spec.Quality = qv
		case "colors":
			if !hasVal || val == "" {
				return errors.New("imageview: missing colors value")
			}
			cv, pErr := strconv.Atoi(val)
			if pErr != nil || cv < 2 || cv > 256 {
				return fmt.Errorf("imageview: colors %q out of range [2..256]", val)
			}
			spec.Colors = cv
		case "strip":
			if !hasVal || val == "" {
				spec.Strip = true
				return nil
			}
			switch strings.ToLower(strings.TrimSpace(val)) {
			case "1", "true":
				spec.Strip = true
			case "0", "false":
				spec.Strip = false
			default:
				return fmt.Errorf("imageview: invalid strip value %q", val)
			}
		case "meta-keep-list":
			if !hasVal || strings.TrimSpace(val) == "" {
				return errors.New("imageview: empty meta-keep-list")
			}
			if keepSet == nil {
				keepSet = make(map[string]bool, 3)
			}
			for _, tok := range strings.Split(val, ",") {
				t := strings.ToLower(strings.TrimSpace(tok))
				switch t {
				case "exif", "icc", "xmp":
					keepSet[t] = true
				default:
					return fmt.Errorf("imageview: unsupported meta-keep-list token %q", tok)
				}
			}
		case "ignore-error":
			if !hasVal || val == "" {
				return errors.New("imageview: missing ignore-error value")
			}
			switch strings.ToLower(strings.TrimSpace(val)) {
			case "1", "true":
				spec.IgnoreError = true
			case "0", "false":
				spec.IgnoreError = false
			default:
				return fmt.Errorf("imageview: invalid ignore-error value %q", val)
			}
		}
		return nil
	}

	segments := strings.Split(q, "&")
	for _, seg := range segments {
		if seg == "" {
			continue
		}
		kRaw, vRaw, hasEq := strings.Cut(seg, "=")
		kDec, uErr1 := url.QueryUnescape(kRaw)
		if uErr1 != nil {
			return spec, true, fmt.Errorf("imageview: malformed query key %q", kRaw)
		}
		vDec, uErr2 := url.QueryUnescape(vRaw)
		if uErr2 != nil {
			return spec, true, fmt.Errorf("imageview: malformed query value %q", vRaw)
		}
		trimmedKey := strings.TrimSpace(kDec)
		lowerKey := strings.ToLower(trimmedKey)

		// 1. Qiniu path style: `imageView2/<mode>/...` (or `imageView2=/<mode>/...`)
		if lowerKey == "imageview2" || strings.HasPrefix(lowerKey, "imageview2/") {
			sawTransform = true
			pathStr := trimmedKey
			if lowerKey == "imageview2" {
				if !hasEq || strings.TrimSpace(vDec) == "" {
					return spec, true, errors.New("imageview: missing mode in imageView2 directive")
				}
				pathStr = "imageView2/" + strings.TrimPrefix(strings.TrimSpace(vDec), "/")
			} else if hasEq && vDec != "" {
				return spec, true, errors.New("imageview: unexpected '=' inside imageView2 path directive")
			}
			pathStr = strings.TrimSuffix(pathStr, "/")
			parts := strings.Split(pathStr, "/")
			if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
				return spec, true, errors.New("imageview: missing mode in imageView2 path")
			}
			if err := applyParam("mode", strings.TrimSpace(parts[1]), true); err != nil {
				return spec, true, err
			}
			idx := 2
			for idx < len(parts) {
				pKey := strings.ToLower(strings.TrimSpace(parts[idx]))
				if pKey == "" {
					return spec, true, errors.New("imageview: empty segment in imageView2 path")
				}
				if !isKnownImageViewPathKey(pKey) {
					return spec, true, fmt.Errorf("imageview: unknown parameter %q in imageView2 path", parts[idx])
				}
				if pKey == "strip" {
					if idx+1 >= len(parts) {
						spec.Strip = true
						idx++
						continue
					}
					nextTok := strings.TrimSpace(parts[idx+1])
					lowerNext := strings.ToLower(nextTok)
					if lowerNext == "0" || lowerNext == "false" {
						spec.Strip = false
						idx += 2
						continue
					}
					if lowerNext == "1" || lowerNext == "true" {
						spec.Strip = true
						idx += 2
						continue
					}
					if isKnownImageViewPathKey(lowerNext) {
						spec.Strip = true
						idx++
						continue
					}
					return spec, true, fmt.Errorf("imageview: invalid strip value %q in imageView2 path", nextTok)
				}
				if idx+1 >= len(parts) {
					return spec, true, fmt.Errorf("imageview: missing value for %q in imageView2 path", pKey)
				}
				if err := applyParam(pKey, strings.TrimSpace(parts[idx+1]), true); err != nil {
					return spec, true, err
				}
				idx += 2
			}
			continue
		}

		// 2. Standard KV query style: `?mode=1&w=960&h=540&format=webp&strip...`
		switch lowerKey {
		case "mode", "w", "h", "format", "interlace", "q", "quality", "colors", "strip", "meta-keep-list":
			sawTransform = true
			if err := applyParam(lowerKey, strings.TrimSpace(vDec), hasEq); err != nil {
				return spec, true, err
			}
		case "ignore-error":
			if err := applyParam("ignore-error", strings.TrimSpace(vDec), hasEq); err != nil {
				return spec, sawTransform, err
			}
		}
	}

	if !sawTransform {
		return ImageViewSpec{Mode: -1}, false, nil
	}

	if !sawMode && (spec.W > 0 || spec.H > 0) {
		spec.Mode = 0
	}
	if len(keepSet) > 0 {
		keepList := make([]string, 0, len(keepSet))
		for k := range keepSet {
			keepList = append(keepList, k)
		}
		sort.Strings(keepList)
		spec.MetaKeepList = keepList
	}
	return spec, true, nil
}

func normalizeAndValidateSpec(s ImageViewSpec, lenient bool) (ImageViewSpec, error) {
	out := s
	if out.W > maxImageViewDim {
		out.W = maxImageViewDim
	}
	if out.H > maxImageViewDim {
		out.H = maxImageViewDim
	}
	if !lenient && (out.W < 0 || out.H < 0) {
		return out, errors.New("imageview: negative dimensions are invalid")
	}
	if out.Mode == 0 && out.W == 0 && out.H == 0 {
		// Keep Mode as-is or treat as no-resize during geometry calculation
	} else if out.Mode < 0 && (out.W > 0 || out.H > 0) {
		out.Mode = 0
	}
	if !lenient && (out.Mode < -1 || out.Mode > 5) {
		return out, fmt.Errorf("imageview: invalid mode %d", out.Mode)
	}
	if out.Format != "" {
		f := strings.ToLower(strings.TrimSpace(out.Format))
		if f == "jpeg" {
			f = "jpg"
		}
		switch f {
		case "jpg", "png", "webp", "avif":
			out.Format = f
		default:
			if !lenient {
				return out, fmt.Errorf("imageview: unsupported format %q", out.Format)
			}
		}
	}
	if !lenient && (out.Interlace < 0 || out.Interlace > 1) {
		return out, fmt.Errorf("imageview: invalid interlace %d", out.Interlace)
	}
	if !lenient && (out.Quality < 0 || out.Quality > 100) {
		return out, fmt.Errorf("imageview: invalid quality %d", out.Quality)
	}
	if !lenient && out.Colors != 0 && (out.Colors < 2 || out.Colors > 256) {
		return out, fmt.Errorf("imageview: invalid colors %d", out.Colors)
	}
	if len(out.MetaKeepList) > 0 {
		seen := make(map[string]bool, len(out.MetaKeepList))
		clean := make([]string, 0, len(out.MetaKeepList))
		for _, item := range out.MetaKeepList {
			k := strings.ToLower(strings.TrimSpace(item))
			switch k {
			case "exif", "icc", "xmp":
				if !seen[k] {
					seen[k] = true
					clean = append(clean, k)
				}
			default:
				if !lenient {
					return out, fmt.Errorf("imageview: invalid meta-keep-list item %q", item)
				}
			}
		}
		sort.Strings(clean)
		out.MetaKeepList = clean
	}
	return out, nil
}

type modeGeometry struct {
	scaledW int
	scaledH int
	cropX   int
	cropY   int
	finalW  int
	finalH  int
}

// computeModeGeometry implements Qiniu `imageView2` modes 0..5 with strict no-upscale (`scale <= 1.0`).
// Shared by both `PredictTransformedDims` and `ImageViewEngine.Transform` for 100% zero-CLS parity.
func computeModeGeometry(srcW, srcH, mode, w, h int) modeGeometry {
	if srcW <= 0 || srcH <= 0 {
		return modeGeometry{scaledW: srcW, scaledH: srcH, finalW: srcW, finalH: srcH}
	}
	if w > maxImageViewDim {
		w = maxImageViewDim
	}
	if h > maxImageViewDim {
		h = maxImageViewDim
	}
	if mode < 0 || (w <= 0 && h <= 0) {
		return modeGeometry{scaledW: srcW, scaledH: srcH, finalW: srcW, finalH: srcH}
	}

	roundDim := func(orig int, scale float64) int {
		v := int(math.Round(float64(orig) * scale))
		if v < 1 {
			v = 1
		}
		return v
	}

	switch mode {
	case 0:
		// Mode 0: Fit long edge <= W, short edge <= H (aspect ratio preserved, no crop, no upscale).
		var maxW, maxH int
		if srcW >= srcH {
			maxW, maxH = w, h
		} else {
			maxH, maxW = w, h
		}
		scale := 1.0
		if maxW > 0 {
			if r := float64(maxW) / float64(srcW); r < scale {
				scale = r
			}
		}
		if maxH > 0 {
			if r := float64(maxH) / float64(srcH); r < scale {
				scale = r
			}
		}
		sw, sh := roundDim(srcW, scale), roundDim(srcH, scale)
		return modeGeometry{scaledW: sw, scaledH: sh, finalW: sw, finalH: sh}

	case 1:
		// Mode 1: Scale to cover W x H, then center-crop to min(scaledW, W) x min(scaledH, H) (no upscale).
		targetW, targetH := w, h
		if targetW <= 0 {
			targetW = targetH
		}
		if targetH <= 0 {
			targetH = targetW
		}
		rw := float64(targetW) / float64(srcW)
		rh := float64(targetH) / float64(srcH)
		scale := math.Max(rw, rh)
		if scale > 1.0 {
			scale = 1.0
		}
		sw, sh := roundDim(srcW, scale), roundDim(srcH, scale)
		if srcW >= targetW && sw < targetW && scale >= rw {
			sw = targetW
		}
		if srcH >= targetH && sh < targetH && scale >= rh {
			sh = targetH
		}
		cw := min(sw, targetW)
		ch := min(sh, targetH)
		return modeGeometry{
			scaledW: sw,
			scaledH: sh,
			cropX:   (sw - cw) / 2,
			cropY:   (sh - ch) / 2,
			finalW:  cw,
			finalH:  ch,
		}

	case 2:
		// Mode 2: Fit inside W x H bounding box (aspect ratio preserved, no crop, no upscale).
		scale := 1.0
		if w > 0 {
			if r := float64(w) / float64(srcW); r < scale {
				scale = r
			}
		}
		if h > 0 {
			if r := float64(h) / float64(srcH); r < scale {
				scale = r
			}
		}
		sw, sh := roundDim(srcW, scale), roundDim(srcH, scale)
		return modeGeometry{scaledW: sw, scaledH: sh, finalW: sw, finalH: sh}

	case 3:
		// Mode 3: Scale to cover at least W x H (no crop, aspect ratio preserved, no upscale).
		var rw, rh float64
		if w > 0 {
			rw = float64(w) / float64(srcW)
		}
		if h > 0 {
			rh = float64(h) / float64(srcH)
		}
		scale := math.Max(rw, rh)
		if scale <= 0 || scale > 1.0 {
			scale = 1.0
		}
		sw, sh := roundDim(srcW, scale), roundDim(srcH, scale)
		if w > 0 && srcW >= w && sw < w && scale >= rw {
			sw = w
		}
		if h > 0 && srcH >= h && sh < h && scale >= rh {
			sh = h
		}
		return modeGeometry{scaledW: sw, scaledH: sh, finalW: sw, finalH: sh}

	case 4:
		// Mode 4: Scale so long edge >= W and short edge >= H (no crop, aspect ratio preserved, no upscale).
		var minW, minH int
		if srcW >= srcH {
			minW, minH = w, h
		} else {
			minH, minW = w, h
		}
		var rw, rh float64
		if minW > 0 {
			rw = float64(minW) / float64(srcW)
		}
		if minH > 0 {
			rh = float64(minH) / float64(srcH)
		}
		scale := math.Max(rw, rh)
		if scale <= 0 || scale > 1.0 {
			scale = 1.0
		}
		sw, sh := roundDim(srcW, scale), roundDim(srcH, scale)
		if minW > 0 && srcW >= minW && sw < minW && scale >= rw {
			sw = minW
		}
		if minH > 0 && srcH >= minH && sh < minH && scale >= rh {
			sh = minH
		}
		return modeGeometry{scaledW: sw, scaledH: sh, finalW: sw, finalH: sh}

	case 5:
		// Mode 5: Scale so long edge covers W and short edge covers H, then center-crop to target box (no upscale).
		longTarget, shortTarget := w, h
		if longTarget <= 0 {
			longTarget = shortTarget
		}
		if shortTarget <= 0 {
			shortTarget = longTarget
		}
		var targetW, targetH int
		if srcW >= srcH {
			targetW, targetH = longTarget, shortTarget
		} else {
			targetW, targetH = shortTarget, longTarget
		}
		rw := float64(targetW) / float64(srcW)
		rh := float64(targetH) / float64(srcH)
		scale := math.Max(rw, rh)
		if scale > 1.0 {
			scale = 1.0
		}
		sw, sh := roundDim(srcW, scale), roundDim(srcH, scale)
		if srcW >= targetW && sw < targetW && scale >= rw {
			sw = targetW
		}
		if srcH >= targetH && sh < targetH && scale >= rh {
			sh = targetH
		}
		cw := min(sw, targetW)
		ch := min(sh, targetH)
		return modeGeometry{
			scaledW: sw,
			scaledH: sh,
			cropX:   (sw - cw) / 2,
			cropY:   (sh - ch) / 2,
			finalW:  cw,
			finalH:  ch,
		}

	default:
		return modeGeometry{scaledW: srcW, scaledH: srcH, finalW: srcW, finalH: srcH}
	}
}

// PredictTransformedDims predicts the exact post-transformation `(width, height)` for an image of
// upright dimensions `(origW, origH)` rendered with `rawQuery`. Returns `(origW, origH)` if `rawQuery`
// has no resize directive or is invalid.
func PredictTransformedDims(origW, origH int, rawQuery string) (int, int) {
	if origW <= 0 || origH <= 0 {
		return origW, origH
	}
	spec, hasTransform, err := ParseImageViewSpec(rawQuery)
	if err != nil || !hasTransform || spec.Mode < 0 {
		return origW, origH
	}
	g := computeModeGeometry(origW, origH, spec.Mode, spec.W, spec.H)
	return g.finalW, g.finalH
}

// ImageViewSource describes an immutable source image object on local disk or S3.
type ImageViewSource struct {
	Key         string
	ModTimeNano int64
	Size        int64
	Ext         string
	Load        func(ctx context.Context) ([]byte, error)
}

// ImageViewResult contains the rendered bytes, MIME Content-Type, deterministic strong ETag,
// and optional fallback diagnostics.
type ImageViewResult struct {
	Data                []byte
	ContentType         string
	ETag                string
	FormatFallback      string // e.g. "avif->webp" when format=avif transcode falls back under CGO_ENABLED=0
	PassthroughOriginal bool   // true when ignore-error=1 recovered by returning untouched raw source bytes
}

type ivCacheNode struct {
	key  string
	res  ImageViewResult
	prev *ivCacheNode
	next *ivCacheNode
}

type ivFlightCall struct {
	done chan struct{}
	res  *ImageViewResult
	err  error
}

// ImageViewEngine executes concurrency-bounded, singleflight-deduplicated, LRU-cached real-time image transforms.
type ImageViewEngine struct {
	maxCacheBytes int64
	maxEntryBytes int64

	cacheMu   sync.Mutex
	usedBytes int64
	entries   map[string]*ivCacheNode
	head      *ivCacheNode
	tail      *ivCacheNode

	flightMu sync.Mutex
	flights  map[string]*ivFlightCall
}

// NewImageViewEngine creates a bounded in-memory ImageView rendering engine (defaulting to 8 MiB if maxCacheBytes <= 0).
func NewImageViewEngine(maxCacheBytes int64) *ImageViewEngine {
	if maxCacheBytes <= 0 {
		maxCacheBytes = defaultImageViewCacheBytes
	}
	maxEntry := maxCacheBytes / 8
	if maxEntry < 1 {
		maxEntry = maxCacheBytes
	}
	return &ImageViewEngine{
		maxCacheBytes: maxCacheBytes,
		maxEntryBytes: maxEntry,
		entries:       make(map[string]*ivCacheNode),
		flights:       make(map[string]*ivFlightCall),
	}
}

// ComputeETag returns a strong HTTP ETag uniquely determined by source identity/version and spec.CanonicalKey().
func (e *ImageViewEngine) ComputeETag(srcKey string, modTimeNano int64, size int64, spec ImageViewSpec) string {
	h := sha256.New()
	_, _ = h.Write([]byte("volo-iv2:v1|"))
	_, _ = h.Write([]byte(srcKey))
	_, _ = h.Write([]byte("|"))
	_, _ = h.Write([]byte(strconv.FormatInt(modTimeNano, 10)))
	_, _ = h.Write([]byte("|"))
	_, _ = h.Write([]byte(strconv.FormatInt(size, 10)))
	_, _ = h.Write([]byte("|"))
	_, _ = h.Write([]byte(spec.CanonicalKey()))
	sum := h.Sum(nil)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

// Purge evicts all cached transformed image representations from memory.
func (e *ImageViewEngine) Purge() {
	if e == nil {
		return
	}
	e.cacheMu.Lock()
	e.entries = make(map[string]*ivCacheNode)
	e.head = nil
	e.tail = nil
	e.usedBytes = 0
	e.cacheMu.Unlock()
}

func (e *ImageViewEngine) getCached(cacheKey string) (*ImageViewResult, bool) {
	if e == nil {
		return nil, false
	}
	e.cacheMu.Lock()
	defer e.cacheMu.Unlock()
	node, ok := e.entries[cacheKey]
	if !ok {
		return nil, false
	}
	e.moveToFrontLocked(node)
	cp := node.res
	return &cp, true
}

func (e *ImageViewEngine) putCached(cacheKey string, res ImageViewResult) {
	if e == nil || res.PassthroughOriginal {
		return
	}
	entrySize := int64(len(res.Data))
	if entrySize <= 0 || entrySize > e.maxEntryBytes || entrySize > e.maxCacheBytes {
		return
	}
	e.cacheMu.Lock()
	defer e.cacheMu.Unlock()

	if existing, ok := e.entries[cacheKey]; ok {
		e.usedBytes -= int64(len(existing.res.Data))
		existing.res = res
		e.usedBytes += entrySize
		e.moveToFrontLocked(existing)
	} else {
		node := &ivCacheNode{key: cacheKey, res: res}
		e.entries[cacheKey] = node
		e.pushFrontLocked(node)
		e.usedBytes += entrySize
	}

	for e.usedBytes > e.maxCacheBytes && e.tail != nil {
		evict := e.tail
		e.removeNodeLocked(evict)
		delete(e.entries, evict.key)
		e.usedBytes -= int64(len(evict.res.Data))
	}
}

func (e *ImageViewEngine) pushFrontLocked(n *ivCacheNode) {
	n.prev = nil
	n.next = e.head
	if e.head != nil {
		e.head.prev = n
	}
	e.head = n
	if e.tail == nil {
		e.tail = n
	}
}

func (e *ImageViewEngine) removeNodeLocked(n *ivCacheNode) {
	if n.prev != nil {
		n.prev.next = n.next
	} else {
		e.head = n.next
	}
	if n.next != nil {
		n.next.prev = n.prev
	} else {
		e.tail = n.prev
	}
	n.prev = nil
	n.next = nil
}

func (e *ImageViewEngine) moveToFrontLocked(n *ivCacheNode) {
	if e.head == n {
		return
	}
	e.removeNodeLocked(n)
	e.pushFrontLocked(n)
}

func buildCacheLookupKey(src ImageViewSource, canonicalSpec string) string {
	return src.Key + "|" + strconv.FormatInt(src.ModTimeNano, 10) + "|" + strconv.FormatInt(src.Size, 10) + "|" + canonicalSpec
}

// Transform loads (on cache miss) and non-destructively renders `src` according to `spec`.
func (e *ImageViewEngine) Transform(ctx context.Context, src ImageViewSource, spec ImageViewSpec) (*ImageViewResult, error) {
	if src.Load == nil {
		return nil, errors.New("imageview: nil source loader")
	}
	normSpec, valErr := normalizeAndValidateSpec(spec, false)
	if valErr != nil {
		if spec.IgnoreError {
			return e.recoverPassthrough(ctx, src, spec)
		}
		return nil, valErr
	}

	etag := e.ComputeETag(src.Key, src.ModTimeNano, src.Size, normSpec)
	cacheKey := buildCacheLookupKey(src, normSpec.CanonicalKey())

	if cached, ok := e.getCached(cacheKey); ok {
		return cached, nil
	}

	if e == nil {
		return executeImageViewTransform(ctx, src, normSpec, etag)
	}

	e.flightMu.Lock()
	if cached, ok := e.getCached(cacheKey); ok {
		e.flightMu.Unlock()
		return cached, nil
	}
	if call, inFlight := e.flights[cacheKey]; inFlight {
		e.flightMu.Unlock()
		select {
		case <-call.done:
			if call.err != nil {
				if normSpec.IgnoreError {
					return e.recoverPassthrough(ctx, src, normSpec)
				}
				return nil, call.err
			}
			cp := *call.res
			return &cp, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &ivFlightCall{done: make(chan struct{})}
	e.flights[cacheKey] = call
	e.flightMu.Unlock()

	go func() {
		leaderCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		res, err := executeImageViewTransform(leaderCtx, src, normSpec, etag)
		if err == nil && res != nil && !res.PassthroughOriginal {
			e.putCached(cacheKey, *res)
		}
		call.res = res
		call.err = err
		close(call.done)

		e.flightMu.Lock()
		delete(e.flights, cacheKey)
		e.flightMu.Unlock()
	}()

	select {
	case <-call.done:
		if call.err != nil {
			return nil, call.err
		}
		cp := *call.res
		return &cp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (e *ImageViewEngine) recoverPassthrough(ctx context.Context, src ImageViewSource, spec ImageViewSpec) (*ImageViewResult, error) {
	raw, err := src.Load(ctx)
	if err != nil {
		return nil, err
	}
	ct, safe := sniffSafeOriginalImageMIME(src.Ext, raw)
	if !safe {
		return nil, errors.New("imageview: unsafe source payload cannot be passed through")
	}
	return &ImageViewResult{
		Data:                raw,
		ContentType:         ct,
		ETag:                e.ComputeETag(src.Key, src.ModTimeNano, src.Size, spec),
		PassthroughOriginal: true,
	}, nil
}

func sniffSafeOriginalImageMIME(ext string, raw []byte) (string, bool) {
	switch {
	case len(raw) >= 3 && raw[0] == 0xFF && raw[1] == 0xD8 && raw[2] == 0xFF:
		return "image/jpeg", true
	case bytes.HasPrefix(raw, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}):
		return "image/png", true
	case isWebPRIFF(raw):
		return "image/webp", true
	case bytes.HasPrefix(raw, []byte("GIF87a")) || bytes.HasPrefix(raw, []byte("GIF89a")):
		return "image/gif", true
	case hasAVIFMagic(raw):
		return "image/avif", true
	}
	cleanExt := strings.ToLower(strings.TrimSpace(ext))
	if cleanExt != "" && !strings.HasPrefix(cleanExt, ".") {
		cleanExt = "." + cleanExt
	}
	if isForbiddenUploadExt(cleanExt) {
		return "", false
	}
	// Never return active markup MIME types even on fallback
	head := raw
	if len(head) > 512 {
		head = head[:512]
	}
	lowHead := bytes.ToLower(bytes.TrimSpace(head))
	if bytes.HasPrefix(lowHead, []byte("<svg")) || bytes.HasPrefix(lowHead, []byte("<!doctype html")) || bytes.HasPrefix(lowHead, []byte("<html")) || bytes.HasPrefix(lowHead, []byte("<?xml")) {
		return "", false
	}
	switch cleanExt {
	case ".jpg", ".jpeg":
		return "image/jpeg", true
	case ".png":
		return "image/png", true
	case ".webp":
		return "image/webp", true
	case ".gif":
		return "image/gif", true
	case ".avif":
		return "image/avif", true
	default:
		return "application/octet-stream", true
	}
}

func resolveMetadataKeepSet(spec ImageViewSpec) map[string]bool {
	keep := make(map[string]bool, 3)
	if len(spec.MetaKeepList) > 0 {
		for _, k := range spec.MetaKeepList {
			keep[k] = true
		}
		return keep
	}
	if spec.Strip {
		return keep
	}
	// Default privacy-first policy: strip EXIF/GPS and XMP, preserve ICC color profile!
	keep["icc"] = true
	return keep
}

func executeImageViewTransform(ctx context.Context, src ImageViewSource, spec ImageViewSpec, etag string) (*ImageViewResult, error) {
	raw, err := src.Load(ctx)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, errors.New("imageview: empty source image")
	}

	failOrRecover := func(tErr error) (*ImageViewResult, error) {
		if spec.IgnoreError {
			if ct, safe := sniffSafeOriginalImageMIME(src.Ext, raw); safe {
				return &ImageViewResult{
					Data:                raw,
					ContentType:         ct,
					ETag:                etag,
					PassthroughOriginal: true,
				}, nil
			}
		}
		return nil, tErr
	}

	// 1. Handle .avif source files (pure-Go zero-CGO build cannot decode AV1 bitstreams).
	extLower := strings.ToLower(strings.TrimSpace(src.Ext))
	if extLower == "" && src.Key != "" {
		extLower = strings.ToLower(filepath.Ext(src.Key))
	}
	if extLower != "" && !strings.HasPrefix(extLower, ".") {
		extLower = "." + extLower
	}
	if hasAVIFMagic(raw) || extLower == ".avif" {
		if _, _, aErr := parseAVIFDimensions(raw); aErr != nil {
			return failOrRecover(aErr)
		}
		isPureAVIFPassthrough := (spec.Format == "" || spec.Format == "avif") &&
			spec.W == 0 && spec.H == 0 &&
			spec.Quality == 0 && spec.Colors == 0 && spec.Interlace == 0 &&
			!spec.Strip && len(spec.MetaKeepList) == 0
		if isPureAVIFPassthrough {
			return &ImageViewResult{
				Data:        raw,
				ContentType: "image/avif",
				ETag:        etag,
			}, nil
		}
		return failOrRecover(errors.New("imageview: AVIF pixel decoding is unsupported in zero-CGO pure-Go builds"))
	}

	// Acquire shared transcoding concurrency semaphore to bound peak heap usage.
	select {
	case transcodeSem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-transcodeSem }()

	cfg, srcFormat, cfgErr := decodeImageConfigSafe(raw)
	if cfgErr != nil {
		return failOrRecover(fmt.Errorf("imageview: invalid image header: %w", cfgErr))
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxImageDimension || cfg.Height > maxImageDimension ||
		int64(cfg.Width)*int64(cfg.Height) > maxImagePixels {
		return failOrRecover(fmt.Errorf("imageview: source dimensions %dx%d exceed 24MP safety limit", cfg.Width, cfg.Height))
	}

	// Determine effective output format & fallback diagnostic.
	targetFormat := spec.Format
	var formatFallback string
	if targetFormat == "avif" {
		targetFormat = "webp"
		formatFallback = "avif->webp"
	} else if targetFormat == "" {
		switch srcFormat {
		case "jpeg":
			targetFormat = "jpg"
		case "png":
			targetFormat = "png"
		case "webp":
			targetFormat = "webp"
		case "gif":
			targetFormat = "gif"
		default:
			targetFormat = "jpg"
		}
	}

	// 2. GIF -> GIF (preserves multi-frame animation with Catmull-Rom resize + Median-Cut quantization)
	if srcFormat == "gif" && targetFormat == "gif" {
		if gErr := inspectGIFStreamLimits(raw); gErr != nil {
			return failOrRecover(gErr)
		}
		g, decErr := gif.DecodeAll(bytes.NewReader(raw))
		if decErr != nil || len(g.Image) == 0 {
			return failOrRecover(fmt.Errorf("imageview: gif decode failed: %w", decErr))
		}
		outBytes, gEncErr := transformAnimatedGIF(g, spec)
		if gEncErr != nil {
			return failOrRecover(gEncErr)
		}
		return &ImageViewResult{
			Data:           outBytes,
			ContentType:    "image/gif",
			ETag:           etag,
			FormatFallback: formatFallback,
		}, nil
	}

	if srcFormat == "gif" {
		if gErr := inspectGIFStreamLimits(raw); gErr != nil {
			return failOrRecover(gErr)
		}
	}

	// 3. Extract container metadata (`exif`, `icc`, `xmp`) prior to pixel decode.
	meta := extractImageMetadata(raw)
	orient := 1
	if srcFormat != "gif" {
		orient = ReadEXIFOrientation(raw)
	}

	decoded, _, decErr := decodeRasterImageSafe(raw)
	if decErr != nil {
		return failOrRecover(fmt.Errorf("imageview: decode failed: %w", decErr))
	}
	upright := toNRGBA(ApplyEXIFOrientation(decoded, orient))
	decoded = nil

	upBounds := upright.Bounds()
	geom := computeModeGeometry(upBounds.Dx(), upBounds.Dy(), spec.Mode, spec.W, spec.H)
	rendered := resampleAndCropCatmullRom(upright, geom)
	upright = nil

	quality := spec.Quality
	if quality < 1 || quality > 100 {
		quality = defaultImageViewQuality
	}

	var (
		encoded     []byte
		contentType string
		encErr      error
	)

	switch targetFormat {
	case "jpg":
		// Composite translucent pixels onto white before JPEG encoding so alpha=0 doesn't turn black.
		opaque := compositeOverWhiteIfTranslucent(rendered)
		var buf bytes.Buffer
		encErr = jpeg.Encode(&buf, opaque, &jpeg.Options{Quality: quality})
		encoded = buf.Bytes()
		contentType = "image/jpeg"

	case "png":
		contentType = "image/png"
		if spec.Colors >= 2 {
			palImg := quantizeMedianCut(rendered, spec.Colors)
			if spec.Interlace == 1 {
				encoded, encErr = encodePNGInterlacedAdam7Paletted(palImg)
			} else {
				var buf bytes.Buffer
				enc := png.Encoder{CompressionLevel: png.DefaultCompression}
				encErr = enc.Encode(&buf, palImg)
				encoded = buf.Bytes()
			}
		} else if spec.Interlace == 1 {
			encoded, encErr = encodePNGInterlacedAdam7NRGBA(rendered)
		} else {
			var buf bytes.Buffer
			enc := png.Encoder{CompressionLevel: png.DefaultCompression}
			encErr = enc.Encode(&buf, rendered)
			encoded = buf.Bytes()
		}

	case "webp":
		contentType = "image/webp"
		var imgToEncode image.Image = rendered
		if spec.Colors >= 2 {
			imgToEncode = quantizeMedianCut(rendered, spec.Colors)
		}
		var buf bytes.Buffer
		encErr = nativewebp.Encode(&buf, imgToEncode, nil)
		encoded = buf.Bytes()

	default:
		return failOrRecover(fmt.Errorf("imageview: unsupported target format %q", targetFormat))
	}

	if encErr != nil {
		return failOrRecover(fmt.Errorf("imageview: encode %s failed: %w", targetFormat, encErr))
	}

	keep := resolveMetadataKeepSet(spec)
	finalData, mErr := injectImageMetadata(encoded, targetFormat, keep, meta, orient > 1)
	if mErr != nil {
		finalData = encoded
	}

	return &ImageViewResult{
		Data:           finalData,
		ContentType:    contentType,
		ETag:           etag,
		FormatFallback: formatFallback,
	}, nil
}

func compositeOverWhiteIfTranslucent(src *image.NRGBA) image.Image {
	pix := src.Pix
	hasTranslucent := false
	for i := 3; i < len(pix); i += 4 {
		if pix[i] < 255 {
			hasTranslucent = true
			break
		}
	}
	if !hasTranslucent {
		return src
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(dst.Pix); i++ {
		dst.Pix[i] = 255
	}
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Over)
	return dst
}

func transformAnimatedGIF(g *gif.GIF, spec ImageViewSpec) ([]byte, error) {
	canvasW, canvasH := g.Config.Width, g.Config.Height
	if canvasW <= 0 || canvasH <= 0 {
		b := g.Image[0].Bounds()
		canvasW, canvasH = b.Max.X, b.Max.Y
	}
	geom := computeModeGeometry(canvasW, canvasH, spec.Mode, spec.W, spec.H)
	maxColors := spec.Colors
	if maxColors < 2 || maxColors > 256 {
		maxColors = 256
	}

	canvas := image.NewNRGBA(image.Rect(0, 0, canvasW, canvasH))
	var prevBackup []byte

	out := &gif.GIF{
		Image:     make([]*image.Paletted, len(g.Image)),
		Delay:     append([]int(nil), g.Delay...),
		Disposal:  make([]byte, len(g.Image)),
		LoopCount: g.LoopCount,
		Config: image.Config{
			Width:  geom.finalW,
			Height: geom.finalH,
		},
	}

	for i, frame := range g.Image {
		var disp byte
		if i < len(g.Disposal) {
			disp = g.Disposal[i]
		}
		if disp == gif.DisposalPrevious {
			if len(prevBackup) != len(canvas.Pix) {
				prevBackup = make([]byte, len(canvas.Pix))
			}
			copy(prevBackup, canvas.Pix)
		}

		fb := frame.Bounds()
		draw.Draw(canvas, fb, frame, fb.Min, draw.Over)

		resized := resampleAndCropCatmullRom(canvas, geom)
		palFrame := quantizeMedianCut(resized, maxColors)
		out.Image[i] = palFrame
		out.Disposal[i] = gif.DisposalBackground
		if i == 0 {
			out.Config.ColorModel = palFrame.Palette
		}

		switch disp {
		case gif.DisposalBackground:
			clearRect := fb.Intersect(canvas.Bounds())
			for y := clearRect.Min.Y; y < clearRect.Max.Y; y++ {
				rowOff := y*canvas.Stride + clearRect.Min.X*4
				clear(canvas.Pix[rowOff : rowOff+clearRect.Dx()*4])
			}
		case gif.DisposalPrevious:
			copy(canvas.Pix, prevBackup)
		}
	}

	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ============================================================================
// Pure-Go Separable 2-Pass Catmull-Rom Resampler (Zero golang.org/x/image/draw)
// ============================================================================

const catmullFixedOne = 1 << 14

type contribTap struct {
	idx    int32
	weight int32 // fixed-point scaled by catmullFixedOne (16384)
}

type contribList struct {
	spans [][]contribTap
}

func catmullRomKernel(x float64) float64 {
	if x < 0 {
		x = -x
	}
	if x < 1.0 {
		return 1.5*x*x*x - 2.5*x*x + 1.0
	}
	if x < 2.0 {
		return -0.5*x*x*x + 2.5*x*x - 4.0*x + 2.0
	}
	return 0.0
}

func buildCatmullContributors(srcLen, scaledLen, cropOffset, outLen int) contribList {
	if srcLen <= 0 || srcLen > 65535 || scaledLen <= 0 || outLen <= 0 {
		return contribList{}
	}
	scale := float64(srcLen) / float64(scaledLen)
	filterScale := scale
	if filterScale < 1.0 {
		filterScale = 1.0
	}
	support := 2.0 * filterScale
	invFilterScale := 1.0 / filterScale

	spans := make([][]contribTap, outLen)
	for dstIdx := 0; dstIdx < outLen; dstIdx++ {
		virtualIdx := cropOffset + dstIdx
		center := (float64(virtualIdx)+0.5)*scale - 0.5
		left := int(math.Ceil(center - support))
		right := int(math.Floor(center + support))
		if right < left {
			left = int(math.Round(center))
			right = left
		}

		rawWeights := make([]float64, 0, right-left+1)
		rawIdxs := make([]int32, 0, right-left+1)
		var sum float64
		for s := left; s <= right; s++ {
			w := catmullRomKernel((float64(s) - center) * invFilterScale)
			if w == 0 {
				continue
			}
			clamped := s
			if clamped < 0 {
				clamped = 0
			} else if clamped >= srcLen {
				clamped = srcLen - 1
			}
			// Merge adjacent clamped edge duplicates
			if len(rawIdxs) > 0 && rawIdxs[len(rawIdxs)-1] == int32(clamped) {
				rawWeights[len(rawWeights)-1] += w
			} else {
				rawIdxs = append(rawIdxs, int32(clamped))
				rawWeights = append(rawWeights, w)
			}
			sum += w
		}

		if len(rawIdxs) == 0 || sum == 0 {
			c := int(math.Round(center))
			if c < 0 {
				c = 0
			} else if c >= srcLen {
				c = srcLen - 1
			}
			spans[dstIdx] = []contribTap{{idx: int32(c), weight: catmullFixedOne}}
			continue
		}

		taps := make([]contribTap, len(rawIdxs))
		var fixedSum int32
		maxPos := 0
		maxAbs := -1.0
		for i, w := range rawWeights {
			norm := w / sum
			fw := int32(math.Round(norm * catmullFixedOne))
			taps[i] = contribTap{idx: rawIdxs[i], weight: fw}
			fixedSum += fw
			if math.Abs(norm) > maxAbs {
				maxAbs = math.Abs(norm)
				maxPos = i
			}
		}
		taps[maxPos].weight += catmullFixedOne - fixedSum
		spans[dstIdx] = taps
	}
	return contribList{spans: spans}
}

func resampleAndCropCatmullRom(src *image.NRGBA, g modeGeometry) *image.NRGBA {
	b := src.Bounds()
	srcW, srcH := b.Dx(), b.Dy()
	if srcW <= 0 || srcH <= 0 || g.finalW <= 0 || g.finalH <= 0 {
		return src
	}
	if g.scaledW == srcW && g.scaledH == srcH {
		if g.cropX == 0 && g.cropY == 0 && g.finalW == srcW && g.finalH == srcH {
			out := image.NewNRGBA(image.Rect(0, 0, srcW, srcH))
			draw.Draw(out, out.Bounds(), src, b.Min, draw.Src)
			return out
		}
		out := image.NewNRGBA(image.Rect(0, 0, g.finalW, g.finalH))
		draw.Draw(out, out.Bounds(), src, image.Pt(b.Min.X+g.cropX, b.Min.Y+g.cropY), draw.Src)
		return out
	}

	hContrib := buildCatmullContributors(srcW, g.scaledW, g.cropX, g.finalW)
	vContrib := buildCatmullContributors(srcH, g.scaledH, g.cropY, g.finalH)

	// Pass 1: Horizontal Catmull-Rom in premultiplied RGBA space (srcW x srcH -> finalW x srcH)
	tmpStride := g.finalW * 4
	tmpPix := make([]uint8, tmpStride*srcH)
	srcPix := src.Pix
	srcStride := src.Stride

	for y := 0; y < srcH; y++ {
		srcRow := srcPix[y*srcStride : y*srcStride+srcW*4]
		dstRow := tmpPix[y*tmpStride : (y+1)*tmpStride]
		for x, taps := range hContrib.spans {
			var accR, accG, accB, accA int64
			for _, t := range taps {
				off := int(t.idx) * 4
				a := int64(srcRow[off+3])
				w := int64(t.weight)
				if a == 255 {
					accR += int64(srcRow[off]) * 255 * w
					accG += int64(srcRow[off+1]) * 255 * w
					accB += int64(srcRow[off+2]) * 255 * w
					accA += 255 * 255 * w
				} else if a > 0 {
					accR += int64(srcRow[off]) * a * w
					accG += int64(srcRow[off+1]) * a * w
					accB += int64(srcRow[off+2]) * a * w
					accA += a * 255 * w
				}
			}
			const denom = 255 * catmullFixedOne
			const half = denom / 2
			pa := int((accA + half) / denom)
			if pa <= 0 {
				dOff := x * 4
				dstRow[dOff], dstRow[dOff+1], dstRow[dOff+2], dstRow[dOff+3] = 0, 0, 0, 0
				continue
			}
			if pa > 255 {
				pa = 255
			}
			pr := clampPremulByte(int((accR+half)/denom), pa)
			pg := clampPremulByte(int((accG+half)/denom), pa)
			pb := clampPremulByte(int((accB+half)/denom), pa)
			dOff := x * 4
			dstRow[dOff] = pr
			dstRow[dOff+1] = pg
			dstRow[dOff+2] = pb
			dstRow[dOff+3] = uint8(pa)
		}
	}

	// Pass 2: Vertical Catmull-Rom + un-premultiply back to NRGBA (finalW x srcH -> finalW x finalH)
	dst := image.NewNRGBA(image.Rect(0, 0, g.finalW, g.finalH))
	dstPix := dst.Pix
	dstStride := dst.Stride

	for y, taps := range vContrib.spans {
		dstRow := dstPix[y*dstStride : y*dstStride+g.finalW*4]
		for x := 0; x < g.finalW; x++ {
			colOff := x * 4
			var accR, accG, accB, accA int32
			for _, t := range taps {
				off := int(t.idx)*tmpStride + colOff
				w := t.weight
				accR += int32(tmpPix[off]) * w
				accG += int32(tmpPix[off+1]) * w
				accB += int32(tmpPix[off+2]) * w
				accA += int32(tmpPix[off+3]) * w
			}
			const half = catmullFixedOne / 2
			pa := int((accA + half) >> 14)
			dOff := colOff
			if pa <= 0 {
				dstRow[dOff], dstRow[dOff+1], dstRow[dOff+2], dstRow[dOff+3] = 0, 0, 0, 0
				continue
			}
			if pa >= 255 {
				dstRow[dOff] = clampPremulByte(int((accR+half)>>14), 255)
				dstRow[dOff+1] = clampPremulByte(int((accG+half)>>14), 255)
				dstRow[dOff+2] = clampPremulByte(int((accB+half)>>14), 255)
				dstRow[dOff+3] = 255
				continue
			}
			pr := int(clampPremulByte(int((accR+half)>>14), pa))
			pg := int(clampPremulByte(int((accG+half)>>14), pa))
			pb := int(clampPremulByte(int((accB+half)>>14), pa))
			dstRow[dOff] = uint8((pr*255 + pa/2) / pa)
			dstRow[dOff+1] = uint8((pg*255 + pa/2) / pa)
			dstRow[dOff+2] = uint8((pb*255 + pa/2) / pa)
			dstRow[dOff+3] = uint8(pa)
		}
	}
	return dst
}

func clampPremulByte(v, maxA int) uint8 {
	if v <= 0 {
		return 0
	}
	if v > maxA {
		return uint8(maxA)
	}
	return uint8(v)
}

// ============================================================================
// Pure-Go Median-Cut Palette Quantizer (`colors` 2..256)
// ============================================================================

type histColorBin struct {
	r, g, b uint8
	count   uint32
}

type medianCutBox struct {
	bins               []histColorBin
	totalCount         uint64
	rMin, rMax         uint8
	gMin, gMax         uint8
	bMin, bMax         uint8
	maxRange           int
	dominantChannelIdx int // 0=R, 1=G, 2=B
}

func newMedianCutBox(bins []histColorBin) medianCutBox {
	box := medianCutBox{
		bins: bins,
		rMin: 255, gMin: 255, bMin: 255,
	}
	for _, c := range bins {
		box.totalCount += uint64(c.count)
		if c.r < box.rMin {
			box.rMin = c.r
		}
		if c.r > box.rMax {
			box.rMax = c.r
		}
		if c.g < box.gMin {
			box.gMin = c.g
		}
		if c.g > box.gMax {
			box.gMax = c.g
		}
		if c.b < box.bMin {
			box.bMin = c.b
		}
		if c.b > box.bMax {
			box.bMax = c.b
		}
	}
	rSpan := int(box.rMax) - int(box.rMin)
	gSpan := int(box.gMax) - int(box.gMin)
	bSpan := int(box.bMax) - int(box.bMin)
	box.maxRange = rSpan
	box.dominantChannelIdx = 0
	if gSpan >= rSpan && gSpan >= bSpan {
		box.maxRange = gSpan
		box.dominantChannelIdx = 1
	} else if bSpan >= rSpan && bSpan >= gSpan {
		box.maxRange = bSpan
		box.dominantChannelIdx = 2
	}
	return box
}

func quantizeMedianCut(src *image.NRGBA, maxColors int) *image.Paletted {
	if maxColors < 2 {
		maxColors = 2
	} else if maxColors > 256 {
		maxColors = 256
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	pix := src.Pix
	stride := src.Stride

	hasTransparent := false
	for y := 0; y < h && !hasTransparent; y++ {
		row := pix[y*stride : y*stride+w*4]
		for x := 3; x < len(row); x += 4 {
			if row[x] < 128 {
				hasTransparent = true
				break
			}
		}
	}

	targetOpaque := maxColors
	if hasTransparent {
		targetOpaque = maxColors - 1
	}
	if targetOpaque < 1 {
		targetOpaque = 1
	}

	// Fast exact-color check for images with <= targetOpaque distinct RGB888 colors,
	// paired with a 32768-bin RGB555 histogram for general photographic quantization.
	var hist [32768]uint32
	exactColors := make(map[uint32]uint8, min(targetOpaque+1, 256))
	exactOverflow := false

	for y := 0; y < h; y++ {
		row := pix[y*stride : y*stride+w*4]
		for off := 0; off < len(row); off += 4 {
			if row[off+3] < 128 {
				continue
			}
			r, g, bl := row[off], row[off+1], row[off+2]
			idx555 := (uint16(r>>3) << 10) | (uint16(g>>3) << 5) | uint16(bl>>3)
			hist[idx555]++
			if !exactOverflow {
				rgb24 := uint32(r)<<16 | uint32(g)<<8 | uint32(bl)
				if _, ok := exactColors[rgb24]; !ok {
					if len(exactColors) >= targetOpaque {
						exactOverflow = true
					} else {
						exactColors[rgb24] = 0
					}
				}
			}
		}
	}

	palette := make(color.Palette, 0, maxColors)
	if hasTransparent {
		palette = append(palette, color.NRGBA{R: 0, G: 0, B: 0, A: 0})
	}

	if !exactOverflow && len(exactColors) > 0 {
		keys := make([]uint32, 0, len(exactColors))
		for k := range exactColors {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		for _, k := range keys {
			pIdx := uint8(len(palette))
			exactColors[k] = pIdx
			palette = append(palette, color.NRGBA{
				R: uint8(k >> 16),
				G: uint8(k >> 8),
				B: uint8(k),
				A: 255,
			})
		}
		dst := image.NewPaletted(image.Rect(0, 0, w, h), palette)
		for y := 0; y < h; y++ {
			srcRow := pix[y*stride : y*stride+w*4]
			dstRow := dst.Pix[y*dst.Stride : y*dst.Stride+w]
			for x := 0; x < w; x++ {
				off := x * 4
				if srcRow[off+3] < 128 {
					dstRow[x] = 0
					continue
				}
				rgb24 := uint32(srcRow[off])<<16 | uint32(srcRow[off+1])<<8 | uint32(srcRow[off+2])
				dstRow[x] = exactColors[rgb24]
			}
		}
		return dst
	}

	bins := make([]histColorBin, 0, 1024)
	for i, cnt := range hist {
		if cnt == 0 {
			continue
		}
		r5 := uint8((i >> 10) & 0x1F)
		g5 := uint8((i >> 5) & 0x1F)
		b5 := uint8(i & 0x1F)
		bins = append(bins, histColorBin{
			r:     (r5 << 3) | (r5 >> 2),
			g:     (g5 << 3) | (g5 >> 2),
			b:     (b5 << 3) | (b5 >> 2),
			count: cnt,
		})
	}

	if len(bins) == 0 {
		if len(palette) == 0 {
			palette = append(palette, color.NRGBA{R: 0, G: 0, B: 0, A: 255})
		}
		for len(palette) < 2 {
			palette = append(palette, color.NRGBA{R: 0, G: 0, B: 0, A: 255})
		}
		return image.NewPaletted(image.Rect(0, 0, w, h), palette)
	}

	boxes := make([]medianCutBox, 0, targetOpaque)
	boxes = append(boxes, newMedianCutBox(bins))

	for len(boxes) < targetOpaque {
		bestIdx := -1
		var bestScore uint64
		for i := range boxes {
			if len(boxes[i].bins) < 2 || boxes[i].maxRange == 0 {
				continue
			}
			score := uint64(boxes[i].maxRange) * (1 + uint64(math.Sqrt(float64(boxes[i].totalCount))))
			if bestIdx < 0 || score > bestScore {
				bestIdx = i
				bestScore = score
			}
		}
		if bestIdx < 0 {
			break
		}
		target := boxes[bestIdx]
		ch := target.dominantChannelIdx
		sort.Slice(target.bins, func(i, j int) bool {
			switch ch {
			case 0:
				return target.bins[i].r < target.bins[j].r
			case 1:
				return target.bins[i].g < target.bins[j].g
			default:
				return target.bins[i].b < target.bins[j].b
			}
		})
		halfCount := target.totalCount / 2
		var acc uint64
		splitAt := 1
		for i := 0; i < len(target.bins)-1; i++ {
			acc += uint64(target.bins[i].count)
			if acc >= halfCount {
				splitAt = i + 1
				break
			}
		}
		boxes[bestIdx] = newMedianCutBox(target.bins[:splitAt])
		boxes = append(boxes, newMedianCutBox(target.bins[splitAt:]))
	}

	type palRGB struct{ r, g, b int }
	opaqueStart := len(palette)
	palEntries := make([]palRGB, 0, len(boxes))
	for _, bx := range boxes {
		var sr, sg, sb, sc uint64
		for _, c := range bx.bins {
			cnt := uint64(c.count)
			sr += uint64(c.r) * cnt
			sg += uint64(c.g) * cnt
			sb += uint64(c.b) * cnt
			sc += cnt
		}
		if sc == 0 {
			sc = 1
		}
		pr := uint8((sr + sc/2) / sc)
		pg := uint8((sg + sc/2) / sc)
		pb := uint8((sb + sc/2) / sc)
		palette = append(palette, color.NRGBA{R: pr, G: pg, B: pb, A: 255})
		palEntries = append(palEntries, palRGB{r: int(pr), g: int(pg), b: int(pb)})
	}
	for len(palette) < 2 {
		palette = append(palette, palette[0])
	}

	var lut [32768]int16
	for i := range lut {
		lut[i] = -1
	}

	dst := image.NewPaletted(image.Rect(0, 0, w, h), palette)
	for y := 0; y < h; y++ {
		srcRow := pix[y*stride : y*stride+w*4]
		dstRow := dst.Pix[y*dst.Stride : y*dst.Stride+w]
		for x := 0; x < w; x++ {
			off := x * 4
			if hasTransparent && srcRow[off+3] < 128 {
				dstRow[x] = 0
				continue
			}
			r, g, bl := srcRow[off], srcRow[off+1], srcRow[off+2]
			idx555 := (uint16(r>>3) << 10) | (uint16(g>>3) << 5) | uint16(bl>>3)
			if cached := lut[idx555]; cached >= 0 {
				dstRow[x] = uint8(cached)
				continue
			}
			r5 := int((r>>3)<<3 | (r >> 5))
			g5 := int((g>>3)<<3 | (g >> 5))
			b5 := int((bl>>3)<<3 | (bl >> 5))
			bestIdx := 0
			bestDist := int(^uint(0) >> 1)
			for i, pe := range palEntries {
				dr := r5 - pe.r
				dg := g5 - pe.g
				db := b5 - pe.b
				dist := 2*dr*dr + 4*dg*dg + 3*db*db
				if dist < bestDist {
					bestDist = dist
					bestIdx = i
					if dist == 0 {
						break
					}
				}
			}
			chosen := uint8(opaqueStart + bestIdx)
			lut[idx555] = int16(chosen)
			dstRow[x] = chosen
		}
	}
	return dst
}

// ============================================================================
// Pure-Go Adam7 Interlaced PNG Encoder (`interlace=1`)
// ============================================================================

var (
	adam7X0 = [7]int{0, 4, 0, 2, 0, 1, 0}
	adam7DX = [7]int{8, 8, 4, 4, 2, 2, 1}
	adam7Y0 = [7]int{0, 0, 4, 0, 2, 0, 1}
	adam7DY = [7]int{8, 8, 8, 4, 4, 2, 2}
)

func paethPredictor(a, b, c byte) byte {
	p := int(a) + int(b) - int(c)
	pa := p - int(a)
	if pa < 0 {
		pa = -pa
	}
	pb := p - int(b)
	if pb < 0 {
		pb = -pb
	}
	pc := p - int(c)
	if pc < 0 {
		pc = -pc
	}
	if pa <= pb && pa <= pc {
		return a
	}
	if pb <= pc {
		return b
	}
	return c
}

func writeFilteredPNGScanline(zw *zlib.Writer, row, prevRow []byte, bpp int, scratch *[5][]byte) error {
	n := len(row)
	for f := 0; f < 5; f++ {
		if cap(scratch[f]) < n+1 {
			scratch[f] = make([]byte, n+1)
		} else {
			scratch[f] = scratch[f][:n+1]
		}
		scratch[f][0] = byte(f)
	}
	copy(scratch[0][1:], row)

	for i := 0; i < n; i++ {
		var a, b, c byte
		if i >= bpp {
			a = row[i-bpp]
			c = prevRow[i-bpp]
		}
		b = prevRow[i]
		x := row[i]
		scratch[1][i+1] = x - a
		scratch[2][i+1] = x - b
		scratch[3][i+1] = x - byte((int(a)+int(b))/2)
		scratch[4][i+1] = x - paethPredictor(a, b, c)
	}

	bestFilter := 0
	bestSum := int(^uint(0) >> 1)
	for f := 0; f < 5; f++ {
		sum := 0
		buf := scratch[f][1:]
		for _, v := range buf {
			sv := int(int8(v))
			if sv < 0 {
				sum -= sv
			} else {
				sum += sv
			}
		}
		if sum < bestSum {
			bestSum = sum
			bestFilter = f
		}
	}
	_, err := zw.Write(scratch[bestFilter])
	return err
}

func encodePNGInterlacedAdam7NRGBA(src *image.NRGBA) ([]byte, error) {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 || w > 65535 || h > 65535 {
		return nil, errors.New("imageview: empty image bounds for Adam7 PNG")
	}
	hasAlpha := false
	for y := 0; y < h && !hasAlpha; y++ {
		row := src.Pix[y*src.Stride : y*src.Stride+w*4]
		for off := 3; off < len(row); off += 4 {
			if row[off] < 255 {
				hasAlpha = true
				break
			}
		}
	}

	colorType := byte(2) // RGB truecolor
	bpp := 3
	if hasAlpha {
		colorType = 6 // RGBA straight-alpha truecolor
		bpp = 4
	}

	var idatRaw bytes.Buffer
	zw := zlib.NewWriter(&idatRaw)
	var scratch [5][]byte

	for pass := 0; pass < 7; pass++ {
		x0, dx := adam7X0[pass], adam7DX[pass]
		y0, dy := adam7Y0[pass], adam7DY[pass]
		if w <= x0 || h <= y0 {
			continue
		}
		passW := (w - x0 + dx - 1) / dx
		passH := (h - y0 + dy - 1) / dy
		if passW <= 0 || passH <= 0 {
			continue
		}
		rowBytes := passW * bpp
		curRow := make([]byte, rowBytes)
		prevRow := make([]byte, rowBytes)

		for py := 0; py < passH; py++ {
			sy := y0 + py*dy
			srcLine := src.Pix[sy*src.Stride:]
			for px := 0; px < passW; px++ {
				sx := x0 + px*dx
				sOff := sx * 4
				dOff := px * bpp
				curRow[dOff] = srcLine[sOff]
				curRow[dOff+1] = srcLine[sOff+1]
				curRow[dOff+2] = srcLine[sOff+2]
				if hasAlpha {
					curRow[dOff+3] = srcLine[sOff+3]
				}
			}
			if err := writeFilteredPNGScanline(zw, curRow, prevRow, bpp, &scratch); err != nil {
				_ = zw.Close()
				return nil, err
			}
			curRow, prevRow = prevRow, curRow
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}

	var out bytes.Buffer
	out.Grow(64 + idatRaw.Len())
	out.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	var ihdr [13]byte
	binary.BigEndian.PutUint32(ihdr[0:4], uint32(w))
	binary.BigEndian.PutUint32(ihdr[4:8], uint32(h))
	ihdr[8] = 8         // 8 bits per sample
	ihdr[9] = colorType // 2 (RGB) or 6 (RGBA)
	ihdr[10] = 0        // deflate compression
	ihdr[11] = 0        // adaptive filter
	ihdr[12] = 1        // Adam7 interlace!
	writePNGChunk(&out, "IHDR", ihdr[:])
	writePNGChunk(&out, "IDAT", idatRaw.Bytes())
	writePNGChunk(&out, "IEND", nil)
	return out.Bytes(), nil
}

func encodePNGInterlacedAdam7Paletted(src *image.Paletted) ([]byte, error) {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 || w > 65535 || h > 65535 || len(src.Palette) == 0 || len(src.Palette) > 256 {
		return nil, errors.New("imageview: invalid paletted bounds for Adam7 PNG")
	}

	plte := make([]byte, len(src.Palette)*3)
	var trns []byte
	lastAlphaIdx := -1
	alphas := make([]byte, len(src.Palette))
	for i, c := range src.Palette {
		nc := color.NRGBAModel.Convert(c).(color.NRGBA)
		plte[i*3] = nc.R
		plte[i*3+1] = nc.G
		plte[i*3+2] = nc.B
		alphas[i] = nc.A
		if nc.A < 255 {
			lastAlphaIdx = i
		}
	}
	if lastAlphaIdx >= 0 {
		trns = alphas[:lastAlphaIdx+1]
	}

	var idatRaw bytes.Buffer
	zw := zlib.NewWriter(&idatRaw)
	var scratch [5][]byte

	for pass := 0; pass < 7; pass++ {
		x0, dx := adam7X0[pass], adam7DX[pass]
		y0, dy := adam7Y0[pass], adam7DY[pass]
		if w <= x0 || h <= y0 {
			continue
		}
		passW := (w - x0 + dx - 1) / dx
		passH := (h - y0 + dy - 1) / dy
		if passW <= 0 || passH <= 0 {
			continue
		}
		curRow := make([]byte, passW)
		prevRow := make([]byte, passW)

		for py := 0; py < passH; py++ {
			sy := y0 + py*dy
			srcLine := src.Pix[sy*src.Stride:]
			for px := 0; px < passW; px++ {
				curRow[px] = srcLine[x0+px*dx]
			}
			if err := writeFilteredPNGScanline(zw, curRow, prevRow, 1, &scratch); err != nil {
				_ = zw.Close()
				return nil, err
			}
			curRow, prevRow = prevRow, curRow
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}

	var out bytes.Buffer
	out.Grow(64 + len(plte) + len(trns) + idatRaw.Len())
	out.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	var ihdr [13]byte
	binary.BigEndian.PutUint32(ihdr[0:4], uint32(w))
	binary.BigEndian.PutUint32(ihdr[4:8], uint32(h))
	ihdr[8] = 8  // 8-bit palette index
	ihdr[9] = 3  // Indexed-color
	ihdr[10] = 0 // deflate compression
	ihdr[11] = 0 // adaptive filter
	ihdr[12] = 1 // Adam7 interlace!
	writePNGChunk(&out, "IHDR", ihdr[:])
	writePNGChunk(&out, "PLTE", plte)
	if len(trns) > 0 {
		writePNGChunk(&out, "tRNS", trns)
	}
	writePNGChunk(&out, "IDAT", idatRaw.Bytes())
	writePNGChunk(&out, "IEND", nil)
	return out.Bytes(), nil
}

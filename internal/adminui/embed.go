package adminui

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"sync"
	"github.com/Willh92/Volo-go/internal/i18n"
)

//go:embed all:dist
var distFS embed.FS

// BootConfig is injected as JSON inside `<script id="volo-boot-config" type="application/json">`
// so the Admin Console & Setup Wizard SPA immediately knows the custom VOLO_ADMIN_PATH, locale, and Studio accent.
type BootConfig struct {
	Mode          string            `json:"mode"` // "console" | "setup"
	AdminPath     string            `json:"admin_path"`
	PublicURL     string            `json:"public_url"`
	DefaultLocale string            `json:"default_locale"`
	Accent        string            `json:"accent"`
	DefaultTheme  string            `json:"default_theme"`
	SiteTitle     string            `json:"site_title"`
	Locales       []i18n.LocaleInfo `json:"locales,omitempty"`
}

type cachedAsset struct {
	raw         []byte
	gz          []byte
	contentType string
	etagRaw     string
	etagGZ      string
	immutable   bool
}

// Handler serves the embedded Admin Console & Setup Wizard SPA with pre-compressed Gzip negotiation.
type Handler struct {
	indexHTML string
	assets    map[string]cachedAsset
}

var (
	defaultOnce    sync.Once
	defaultHandler *Handler
)

// Default returns the singleton embedded Admin UI handler.
func Default() *Handler {
	defaultOnce.Do(func() {
		defaultHandler = newHandler()
	})
	return defaultHandler
}

// TotalGzipBytes returns the total pre-compressed Gzip byte size of all embedded Admin SPA assets (for < 300 KB budget audit).
func (h *Handler) TotalGzipBytes() int {
	total := 0
	for rel, item := range h.assets {
		if rel == "index.html" {
			total += len(item.gz)
			continue
		}
		if len(item.gz) > 0 {
			total += len(item.gz)
		} else {
			total += len(item.raw)
		}
	}
	return total
}

func newHandler() *Handler {
	h := &Handler{
		assets: make(map[string]cachedAsset),
	}
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return h
	}
	var gzBuf bytes.Buffer
	gzw, _ := gzip.NewWriterLevel(&gzBuf, gzip.BestCompression)
	_ = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, wErr error) error {
		if wErr != nil || d.IsDir() || strings.HasSuffix(p, ".map") {
			return nil
		}
		if strings.HasSuffix(p, ".gz") {
			logical := strings.TrimSuffix(p, ".gz")
			gzBytes, rErr := fs.ReadFile(sub, p)
			if rErr != nil {
				return nil
			}
			ct := detectContentType(logical)
			sumGZ := sha256.Sum256(gzBytes)
			h.assets[logical] = cachedAsset{
				gz:          gzBytes,
				contentType: ct,
				etagRaw:     fmt.Sprintf(`"%x"`, sumGZ[:12]),
				etagGZ:      fmt.Sprintf(`"%x-gz"`, sumGZ[:12]),
				immutable:   strings.HasPrefix(logical, "assets/"),
			}
			return nil
		}
		if _, exists := h.assets[p]; exists {
			return nil
		}
		raw, rErr := fs.ReadFile(sub, p)
		if rErr != nil {
			return nil
		}
		ct := detectContentType(p)
		sumRaw := sha256.Sum256(raw)
		etagRaw := fmt.Sprintf(`"%x"`, sumRaw[:12])

		var gzBytes []byte
		var etagGZ string
		if isCompressible(p, ct) && gzw != nil {
			gzBuf.Reset()
			gzw.Reset(&gzBuf)
			_, _ = gzw.Write(raw)
			_ = gzw.Close()
			if gzBuf.Len() < len(raw) {
				gzBytes = append([]byte(nil), gzBuf.Bytes()...)
				sumGZ := sha256.Sum256(gzBytes)
				etagGZ = fmt.Sprintf(`"%x-gz"`, sumGZ[:12])
			}
		}
		h.assets[p] = cachedAsset{
			raw:         raw,
			gz:          gzBytes,
			contentType: ct,
			etagRaw:     etagRaw,
			etagGZ:      etagGZ,
			immutable:   strings.HasPrefix(p, "assets/"),
		}
		if p == "index.html" {
			h.indexHTML = string(raw)
		}
		return nil
	})
	return h
}

// ServeMount serves either a static asset under mountPrefix (e.g. `/console/assets/app.js`)
// or the SPA HTML shell with `<base href="{mountPrefix}/">` and JSON boot config injected!
func (h *Handler) ServeMount(w http.ResponseWriter, r *http.Request, mountPrefix string, boot BootConfig) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	cleanMount := "/" + strings.Trim(strings.TrimSpace(mountPrefix), "/")
	rel := strings.TrimPrefix(r.URL.Path, cleanMount)
	rel = strings.TrimPrefix(rel, "/")

	// Serve static bundle files (`assets/*`, `favicon.svg`, etc.)
	if rel != "" && (strings.HasPrefix(rel, "assets/") || strings.Contains(path.Base(rel), ".")) {
		cleanRel := path.Clean(rel)
		item, ok := h.assets[cleanRel]
		if !ok {
			http.NotFound(w, r)
			return
		}
		serveCachedAsset(w, r, item)
		return
	}

	// Otherwise serve SPA index.html shell with no-store cache control & strict CSP
	rendered := h.renderIndexHTML(cleanMount+"/", boot)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Vary", "Accept-Encoding")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob: https: http:; media-src 'self' data: blob: https: http:; connect-src 'self' https:; font-src 'self' data:; object-src 'none'; base-uri 'self'; frame-ancestors 'none'")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(rendered)
}

func (h *Handler) renderIndexHTML(baseHref string, boot BootConfig) []byte {
	var cfgBuf bytes.Buffer
	enc := json.NewEncoder(&cfgBuf)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(boot)

	lang := strings.TrimSpace(boot.DefaultLocale)
	if lang == "" {
		lang = "zh-CN"
	}
	isZH := strings.HasPrefix(strings.ToLower(lang), "zh")
	pageTitle := "Volo Studio Console"
	if boot.Mode == "setup" {
		if isZH {
			pageTitle = "初始化 Volo-go 独立博客"
		} else {
			pageTitle = "Initialize Volo-go"
		}
	} else {
		siteTitle := strings.TrimSpace(boot.SiteTitle)
		if isZH {
			if siteTitle != "" {
				pageTitle = siteTitle + " · Volo 管理工作台"
			} else {
				pageTitle = "Volo 管理工作台"
			}
		} else if siteTitle != "" {
			pageTitle = siteTitle + " · Volo Studio Console"
		}
	}

	inject := fmt.Sprintf(
		`<base href="%s">`+"\n"+
			`<link rel="icon" type="image/svg+xml" href="/favicon.svg">`+"\n"+
			`<link rel="alternate icon" sizes="32x32" href="/favicon.ico">`+"\n"+
			`<script id="volo-boot-config" type="application/json">%s</script>`,
		html.EscapeString(baseHref),
		strings.TrimSpace(cfgBuf.String()),
	)
	tpl := h.indexHTML
	if tpl == "" {
		tpl = "<!doctype html><html lang=\"zh-CN\"><head><meta charset=\"utf-8\"><title>Volo Studio Console</title><!--VOLO_BOOT--></head><body><div id=\"app\"></div></body></html>"
	}
	tpl = strings.Replace(tpl, `<html lang="zh-CN">`, `<html lang="`+html.EscapeString(lang)+`">`, 1)
	tpl = strings.Replace(tpl, `<title>Volo Studio Console</title>`, `<title>`+html.EscapeString(pageTitle)+`</title>`, 1)
	out := strings.Replace(tpl, "<!--VOLO_BOOT-->", inject, 1)
	if out == tpl {
		out = strings.Replace(tpl, "<head>", "<head>\n"+inject, 1)
	}
	sum := sha256.Sum256([]byte(out))
	_ = hex.EncodeToString(sum[:4])
	return []byte(out)
}

func serveCachedAsset(w http.ResponseWriter, r *http.Request, item cachedAsset) {
	w.Header().Set("Content-Type", item.contentType)
	w.Header().Set("Vary", "Accept-Encoding")
	if item.immutable {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=300")
	}

	useGZ := len(item.gz) > 0 && acceptsGzip(r.Header.Get("Accept-Encoding"))
	etag := item.etagRaw
	if useGZ {
		etag = item.etagGZ
		w.Header().Set("Content-Encoding", "gzip")
	}
	w.Header().Set("ETag", etag)

	if inm := strings.TrimSpace(r.Header.Get("If-None-Match")); inm != "" && inm == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if useGZ {
		_, _ = w.Write(item.gz)
		return
	}
	if len(item.raw) > 0 {
		_, _ = w.Write(item.raw)
		return
	}
	if len(item.gz) > 0 {
		if gr, err := gzip.NewReader(bytes.NewReader(item.gz)); err == nil {
			defer gr.Close()
			var out bytes.Buffer
			_, _ = out.ReadFrom(gr)
			_, _ = w.Write(out.Bytes())
		}
	}
}

func acceptsGzip(hdr string) bool {
	for _, part := range strings.Split(strings.ToLower(hdr), ",") {
		seg := strings.TrimSpace(part)
		if seg == "gzip" || strings.HasPrefix(seg, "gzip;") {
			return !strings.Contains(seg, "q=0")
		}
	}
	return false
}

func detectContentType(p string) string {
	ext := strings.ToLower(path.Ext(p))
	switch ext {
	case ".js", ".mjs":
		return "application/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".html":
		return "text/html; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".json":
		return "application/json; charset=utf-8"
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

func isCompressible(p, ct string) bool {
	return strings.HasPrefix(ct, "text/") ||
		strings.Contains(ct, "javascript") ||
		strings.Contains(ct, "json") ||
		strings.Contains(ct, "svg") ||
		strings.HasSuffix(p, ".css")
}

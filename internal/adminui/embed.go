package adminui

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
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
	Mode            string            `json:"mode"` // "console" | "setup"
	AdminPath       string            `json:"admin_path"`
	BaseAdminPath   string            `json:"base_admin_path,omitempty"`
	PublicURL       string            `json:"public_url"`
	DefaultLocale   string            `json:"default_locale"`
	Accent          string            `json:"accent"`
	DefaultTheme    string            `json:"default_theme"`
	SiteTitle       string            `json:"site_title"`
	VendorCDNPrefix string            `json:"vendor_cdn_prefix,omitempty"`
	Locales         []i18n.LocaleInfo `json:"locales,omitempty"`
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
	indexHTML   string
	cspHeader   string
	assets      map[string]cachedAsset
	bootLocales map[string]string
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

func gunzipBytes(gz []byte) ([]byte, error) {
	gr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, err
	}
	defer gr.Close()
	return io.ReadAll(gr)
}

func sanitizeJSONForScript(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.ReplaceAll(s, "</", `<\/`)
	s = strings.ReplaceAll(s, "<!--", `\u003c!--`)
	return s
}

func newHandler() *Handler {
	h := &Handler{
		assets:      make(map[string]cachedAsset),
		bootLocales: make(map[string]string),
	}
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		h.cspHeader = buildIndexCSP("")
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
			if logical == "index.html" {
				if rawHTML, uErr := gunzipBytes(gzBytes); uErr == nil {
					h.indexHTML = string(rawHTML)
				}
			} else if strings.HasPrefix(logical, "i18n/") && strings.HasSuffix(logical, ".json") {
				code := strings.TrimSuffix(strings.TrimPrefix(logical, "i18n/"), ".json")
				if rawJSON, uErr := gunzipBytes(gzBytes); uErr == nil && code != "" {
					h.bootLocales[code] = sanitizeJSONForScript(string(rawJSON))
				}
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
		if _, statErr := fs.Stat(sub, p+".gz"); statErr == nil {
			return nil
		}
		raw, rErr := fs.ReadFile(sub, p)
		if rErr != nil {
			return nil
		}
		if p == "index.html" {
			h.indexHTML = string(raw)
		} else if strings.HasPrefix(p, "i18n/") && strings.HasSuffix(p, ".json") {
			code := strings.TrimSuffix(strings.TrimPrefix(p, "i18n/"), ".json")
			if code != "" {
				h.bootLocales[code] = sanitizeJSONForScript(string(raw))
			}
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
		var retainedRaw []byte
		if len(gzBytes) == 0 {
			retainedRaw = raw
		}
		h.assets[p] = cachedAsset{
			raw:         retainedRaw,
			gz:          gzBytes,
			contentType: ct,
			etagRaw:     etagRaw,
			etagGZ:      etagGZ,
			immutable:   strings.HasPrefix(p, "assets/"),
		}
		return nil
	})
	h.cspHeader = buildIndexCSP(h.indexHTML)
	return h
}

func buildIndexCSP(indexHTML string) string {
	var hashes []string
	rem := indexHTML
	for {
		low := strings.ToLower(rem)
		start := strings.Index(low, "<script")
		if start < 0 {
			break
		}
		rem = rem[start:]
		tagEnd := strings.IndexByte(rem, '>')
		if tagEnd < 0 {
			break
		}
		openTag := strings.ToLower(rem[:tagEnd+1])
		afterOpen := rem[tagEnd+1:]
		closeIdx := strings.Index(strings.ToLower(afterOpen), "</script>")
		if closeIdx < 0 {
			break
		}
		scriptBody := afterOpen[:closeIdx]
		rem = afterOpen[closeIdx+len("</script>"):]
		if strings.Contains(openTag, "src=") || strings.Contains(openTag, "application/json") {
			continue
		}
		if scriptBody == "" {
			continue
		}
		sum := sha256.Sum256([]byte(scriptBody))
		hashes = append(hashes, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
	}
	scriptSrc := "script-src 'self'"
	if len(hashes) > 0 {
		scriptSrc += " " + strings.Join(hashes, " ")
	}
	const defaultVendorOrigins = "https://cdn.jsdelivr.net https://fastly.jsdelivr.net https://unpkg.com"
	return "default-src 'self'; " + scriptSrc + " " + defaultVendorOrigins + "; style-src 'self' 'unsafe-inline' " + defaultVendorOrigins + "; img-src 'self' data: blob: https: http:; media-src 'self' data: blob: https: http:; frame-src 'self' https://player.bilibili.com https://www.youtube-nocookie.com; connect-src 'self' https:; font-src 'self' data: " + defaultVendorOrigins + "; object-src 'none'; base-uri 'self'; frame-ancestors 'none'"
}

func extractVendorOrigin(prefix string) string {
	s := strings.TrimSpace(prefix)
	var rest string
	if strings.HasPrefix(s, "https://") {
		rest = s[len("https://"):]
		s = "https://"
	} else if strings.HasPrefix(s, "http://") {
		rest = s[len("http://"):]
		s = "http://"
	} else {
		return ""
	}
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		rest = rest[:slash]
	}
	if rest == "" || strings.ContainsAny(rest, " \t\r\n;'\"<>\\@?") {
		return ""
	}
	return s + rest
}

// ServeMount serves either a static asset under mountPrefix (e.g. `/console/assets/app.js`, `/console/i18n/en-US.json`)
// or the SPA HTML shell with `<base href="{mountPrefix}/">` and JSON boot config + boot locale injected!
func (h *Handler) ServeMount(w http.ResponseWriter, r *http.Request, mountPrefix string, boot BootConfig) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	cleanMount := "/" + strings.Trim(strings.TrimSpace(mountPrefix), "/")
	rel := strings.TrimPrefix(r.URL.Path, cleanMount)
	rel = strings.TrimPrefix(rel, "/")

	// Serve static bundle files (`assets/*`, `i18n/*`, `favicon.svg`, etc.)
	if rel != "" && (strings.HasPrefix(rel, "assets/") || strings.HasPrefix(rel, "i18n/") || strings.Contains(path.Base(rel), ".")) {
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
	csp := h.cspHeader
	if csp == "" {
		csp = buildIndexCSP(h.indexHTML)
	}
	if extraOrigin := extractVendorOrigin(boot.VendorCDNPrefix); extraOrigin != "" && !strings.Contains(csp, extraOrigin) {
		csp = strings.ReplaceAll(csp, "https://unpkg.com", "https://unpkg.com "+extraOrigin)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Vary", "Accept-Encoding")
	w.Header().Set("Content-Security-Policy", csp)
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

	bootLocaleCode := "en-US"
	if isZH {
		bootLocaleCode = "zh-CN"
	}
	bootLocaleJSON := h.bootLocales[bootLocaleCode]
	if bootLocaleJSON == "" && bootLocaleCode != "zh-CN" {
		bootLocaleCode = "zh-CN"
		bootLocaleJSON = h.bootLocales[bootLocaleCode]
	}

	inject := fmt.Sprintf(
		`<base href="%s">`+"\n"+
			`<link rel="icon" type="image/svg+xml" href="/favicon.svg">`+"\n"+
			`<link rel="alternate icon" sizes="32x32" href="/favicon.ico">`+"\n"+
			`<script id="volo-boot-config" type="application/json">%s</script>`,
		html.EscapeString(baseHref),
		strings.TrimSpace(cfgBuf.String()),
	)
	if bootLocaleJSON != "" {
		inject += fmt.Sprintf(
			"\n"+`<script id="volo-boot-locale" type="application/json" data-locale="%s">%s</script>`,
			html.EscapeString(bootLocaleCode),
			bootLocaleJSON,
		)
	}

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
			_, _ = io.Copy(w, gr)
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

package server

import (
	"compress/gzip"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"

	"github.com/Willh92/Volo-go/internal/auth"
	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/content"
	"github.com/Willh92/Volo-go/internal/guest"
)

const minDynamicGzipBytes = 1024

var dynamicGzipChan = make(chan *gzip.Writer, min(max(8, runtime.GOMAXPROCS(0)*2), 16))

func acquireDynamicGzipWriter(w io.Writer) *gzip.Writer {
	select {
	case zw := <-dynamicGzipChan:
		zw.Reset(w)
		return zw
	default:
		zw, _ := gzip.NewWriterLevel(w, gzip.BestSpeed)
		return zw
	}
}

func releaseDynamicGzipWriter(zw *gzip.Writer) {
	if zw == nil {
		return
	}
	zw.Reset(io.Discard)
	select {
	case dynamicGzipChan <- zw:
	default:
	}
}

// serveCachedDocument serves a revision-coherent cached XML/Markdown document with
// strong ETag (`304 Not Modified`) negotiation and zero-CPU pre-compressed Gzip delivery.
func serveCachedDocument(w http.ResponseWriter, r *http.Request, doc content.CachedDocument) {
	h := w.Header()
	if doc.ContentType != "" {
		h.Set("Content-Type", doc.ContentType)
	}
	addVaryHeader(h, "Accept-Encoding")
	if h.Get("Cache-Control") == "" {
		h.Set("Cache-Control", "public, max-age=60, stale-while-revalidate=300")
	}
	if doc.ETag != "" {
		h.Set("ETag", doc.ETag)
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && ifNoneMatchMatches(r.Header.Get("If-None-Match"), doc.ETag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	useGzip := len(doc.Gzip) > 0 && acceptsGzipEncoding(r.Header.Get("Accept-Encoding"))
	if useGzip {
		h.Set("Content-Encoding", "gzip")
		h.Set("Content-Length", strconv.Itoa(len(doc.Gzip)))
	} else {
		h.Set("Content-Length", strconv.Itoa(len(doc.Raw)))
	}

	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if useGzip {
		_, _ = w.Write(doc.Gzip)
	} else {
		_, _ = w.Write(doc.Raw)
	}
}

func ifNoneMatchMatches(headerVal string, etag string) bool {
	headerVal = strings.TrimSpace(headerVal)
	if headerVal == "" || etag == "" {
		return false
	}
	if headerVal == "*" {
		return true
	}
	cleanTarget := strings.TrimPrefix(etag, "W/")
	for _, part := range strings.Split(headerVal, ",") {
		cand := strings.TrimSpace(part)
		cand = strings.TrimPrefix(cand, "W/")
		if cand == cleanTarget {
			return true
		}
	}
	return false
}

func acceptsGzipEncoding(acceptEncoding string) bool {
	if acceptEncoding == "" {
		return false
	}
	for _, part := range strings.Split(acceptEncoding, ",") {
		token := strings.TrimSpace(part)
		semi := strings.IndexByte(token, ';')
		params := ""
		if semi >= 0 {
			params = strings.TrimSpace(token[semi+1:])
			token = strings.TrimSpace(token[:semi])
		}
		if !strings.EqualFold(token, "gzip") {
			continue
		}
		if params != "" && (strings.Contains(params, "q=0") && !strings.Contains(params, "q=0.")) {
			// Exact q=0, q=0.0, q=0.00, q=0.000 disables gzip.
			qIdx := strings.Index(strings.ToLower(params), "q=")
			if qIdx >= 0 {
				qVal := strings.TrimSpace(params[qIdx+2:])
				if semi2 := strings.IndexByte(qVal, ';'); semi2 >= 0 {
					qVal = qVal[:semi2]
				}
				if f, err := strconv.ParseFloat(strings.TrimSpace(qVal), 64); err == nil && f <= 0 {
					return false
				}
			}
		} else if params != "" {
			qIdx := strings.Index(strings.ToLower(params), "q=")
			if qIdx >= 0 {
				qVal := strings.TrimSpace(params[qIdx+2:])
				if semi2 := strings.IndexByte(qVal, ';'); semi2 >= 0 {
					qVal = qVal[:semi2]
				}
				if f, err := strconv.ParseFloat(strings.TrimSpace(qVal), 64); err == nil && f <= 0 {
					return false
				}
			}
		}
		return true
	}
	return false
}

func addVaryHeader(h http.Header, tokens ...string) {
	if h == nil || len(tokens) == 0 {
		return
	}
	var merged []string
	seen := make(map[string]struct{}, 8)
	addOne := func(tok string) {
		clean := strings.TrimSpace(tok)
		if clean == "" {
			return
		}
		key := strings.ToLower(clean)
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		merged = append(merged, clean)
	}
	for _, v := range h.Values("Vary") {
		for _, item := range strings.Split(v, ",") {
			addOne(item)
		}
	}
	for _, tok := range tokens {
		for _, item := range strings.Split(tok, ",") {
			addOne(item)
		}
	}
	if len(merged) > 0 {
		h.Set("Vary", strings.Join(merged, ", "))
	}
}

// wrapPublicDynamicGzip applies pooled BestSpeed Gzip compression strictly to anonymous, public,
// cache-safe GET responses >= 1 KiB while enforcing 100% BREACH/CRIME immunity:
// any request carrying session/unlock credentials or preview tokens, or any response setting
// cookies or private/no-store Cache-Control headers, bypasses compression completely.
func wrapPublicDynamicGzip(next http.Handler, adminPathFn func() string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cleanAdmin := config.DefaultAdminPath
		if adminPathFn != nil {
			if dyn := strings.TrimSpace(adminPathFn()); dyn != "" {
				cleanAdmin = dyn
			}
		}
		if !eligibleForPublicDynamicGzip(r, cleanAdmin) {
			next.ServeHTTP(w, r)
			return
		}
		gzw := &dynamicGzipResponseWriter{
			ResponseWriter: w,
			statusCode:     http.StatusOK,
		}
		defer gzw.finish()
		next.ServeHTTP(gzw, r)
	})
}

func eligibleForPublicDynamicGzip(r *http.Request, adminPath string) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if !acceptsGzipEncoding(r.Header.Get("Accept-Encoding")) {
		return false
	}
	if r.Header.Get("Range") != "" || r.Header.Get("Upgrade") != "" || r.Header.Get("Authorization") != "" {
		return false
	}
	// BREACH defense: never compress responses for authenticated sessions, unlocked content cookies/headers, or ShareToken previews.
	if ck := r.Header.Get("Cookie"); ck != "" {
		if strings.Contains(ck, auth.SessionCookieName) || strings.Contains(ck, guest.UnlockCookieName) {
			return false
		}
	}
	if r.Header.Get("X-Volo-Unlock-State") != "" || r.Header.Get("X-Volo-Share-Token") != "" {
		return false
	}
	if q := r.URL.Query(); q.Get("preview") != "" || q.Get("st") != "" || q.Get("device") != "" || q.Get("skin") != "" || q.Get("theme_preview") != "" {
		return false
	}

	p := r.URL.Path
	// Skip static/Range/pre-compressed endpoints and sensitive/admin/auth namespaces.
	switch p {
	case "/healthz", "/rss.xml", "/atom.xml", "/podcast.xml", "/feed.json", "/sitemap.xml", "/llms.txt", "/llms-full.txt":
		return false
	}
	if strings.HasPrefix(p, "/uploads/") ||
		strings.HasPrefix(p, "/images/") ||
		strings.HasPrefix(p, "/themes/") ||
		strings.HasPrefix(p, "/avatars/") ||
		strings.HasPrefix(p, "/plugins/") ||
		strings.HasPrefix(p, "/api/admin") ||
		strings.HasPrefix(p, "/setup") ||
		strings.HasPrefix(p, "/api/v1/i18n") ||
		strings.HasPrefix(p, "/api/v1/pow/") ||
		strings.HasPrefix(p, "/api/v1/guest/") ||
		strings.HasPrefix(p, "/api/v1/reader/") ||
		strings.HasPrefix(p, "/api/v1/auth/") ||
		p == adminPath || strings.HasPrefix(p, adminPath+"/") {
		return false
	}
	return true
}

type dynamicGzipResponseWriter struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
	decided     bool
	passthrough bool
	gz          *gzip.Writer
	buf         []byte
}

func (g *dynamicGzipResponseWriter) WriteHeader(code int) {
	if g.wroteHeader {
		return
	}
	g.statusCode = code
	g.wroteHeader = true
	if code < 200 || code == http.StatusNoContent || code == http.StatusNotModified || code >= 300 && code < 400 {
		g.decided = true
		g.passthrough = true
		g.ResponseWriter.WriteHeader(code)
	}
}

func (g *dynamicGzipResponseWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if !g.wroteHeader {
		g.WriteHeader(http.StatusOK)
	}
	if !g.decided {
		if len(g.buf)+len(p) < minDynamicGzipBytes {
			g.buf = append(g.buf, p...)
			return len(p), nil
		}
		g.decide(len(g.buf) + len(p))
	}
	if g.passthrough {
		return g.ResponseWriter.Write(p)
	}
	return g.gz.Write(p)
}

func (g *dynamicGzipResponseWriter) decide(totalPending int) {
	g.decided = true
	h := g.ResponseWriter.Header()

	// Response-side BREACH & double-encoding guard:
	if totalPending < minDynamicGzipBytes ||
		h.Get("Content-Encoding") != "" ||
		len(h.Values("Set-Cookie")) > 0 ||
		isPrivateOrNoStoreCacheControl(h.Get("Cache-Control")) ||
		!isCompressibleContentType(h.Get("Content-Type"), g.buf) {
		g.passthrough = true
		g.ResponseWriter.WriteHeader(g.statusCode)
		if len(g.buf) > 0 {
			_, _ = g.ResponseWriter.Write(g.buf)
			g.buf = nil
		}
		return
	}

	addVaryHeader(h, "Accept-Encoding")
	h.Del("Content-Length")
	h.Set("Content-Encoding", "gzip")
	g.ResponseWriter.WriteHeader(g.statusCode)

	g.gz = acquireDynamicGzipWriter(g.ResponseWriter)
	if len(g.buf) > 0 {
		_, _ = g.gz.Write(g.buf)
		g.buf = nil
	}
}

func (g *dynamicGzipResponseWriter) Flush() {
	if !g.decided && len(g.buf) > 0 {
		g.decide(len(g.buf))
	}
	if g.gz != nil {
		_ = g.gz.Flush()
	}
	if fl, ok := g.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

func (g *dynamicGzipResponseWriter) finish() {
	if !g.decided {
		if !g.wroteHeader && len(g.buf) == 0 {
			return
		}
		g.decided = true
		g.passthrough = true
		h := g.ResponseWriter.Header()
		if !isPrivateOrNoStoreCacheControl(h.Get("Cache-Control")) && len(h.Values("Set-Cookie")) == 0 && isCompressibleContentType(h.Get("Content-Type"), g.buf) {
			addVaryHeader(h, "Accept-Encoding")
		}
		g.ResponseWriter.WriteHeader(g.statusCode)
		if len(g.buf) > 0 {
			_, _ = g.ResponseWriter.Write(g.buf)
			g.buf = nil
		}
		return
	}
	if g.gz != nil {
		_ = g.gz.Close()
		releaseDynamicGzipWriter(g.gz)
		g.gz = nil
	}
}

func isPrivateOrNoStoreCacheControl(cc string) bool {
	if cc == "" {
		return false
	}
	lower := strings.ToLower(cc)
	return strings.Contains(lower, "private") || strings.Contains(lower, "no-store")
}

func isCompressibleContentType(ct string, sample []byte) bool {
	if ct == "" && len(sample) > 0 {
		ct = http.DetectContentType(sample)
	}
	lower := strings.ToLower(ct)
	return strings.HasPrefix(lower, "text/") ||
		strings.Contains(lower, "application/json") ||
		strings.Contains(lower, "application/xml") ||
		strings.Contains(lower, "+xml") ||
		strings.Contains(lower, "+json") ||
		strings.Contains(lower, "application/javascript")
}

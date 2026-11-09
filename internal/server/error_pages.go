package server

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/internal/security"
	"github.com/Willh92/Volo-go/internal/theme"
)

type errorProbeCtxKey struct{}

type errorProbeTracker struct {
	recorded atomic.Bool
}

func withErrorProbeTracker(r *http.Request) (*http.Request, *errorProbeTracker) {
	if r == nil {
		return nil, &errorProbeTracker{}
	}
	if existing, ok := r.Context().Value(errorProbeCtxKey{}).(*errorProbeTracker); ok && existing != nil {
		return r, existing
	}
	tr := &errorProbeTracker{}
	return r.WithContext(context.WithValue(r.Context(), errorProbeCtxKey{}, tr)), tr
}

func markErrorProbeRecorded(r *http.Request) bool {
	if r == nil {
		return true
	}
	if tr, ok := r.Context().Value(errorProbeCtxKey{}).(*errorProbeTracker); ok && tr != nil {
		return !tr.recorded.Swap(true)
	}
	return true
}

func isActiveStaffUser(u *domain.User) bool {
	return u != nil && u.Status == domain.UserStatusActive &&
		(u.Role == domain.RoleAdmin || u.Role == domain.RoleEditor)
}

func (s *AppServer) isStaffRequest(r *http.Request) bool {
	u, _ := s.resolveViewer(r)
	return isActiveStaffUser(u) && s.isStaffTOTPCompliant(r.Context(), u)
}

func defaultErrorKeyForStatus(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusMethodNotAllowed, http.StatusRequestEntityTooLarge:
		return "error.bad_request"
	case http.StatusUnauthorized:
		return "error.unauthorized"
	case http.StatusForbidden:
		return "error.forbidden"
	case http.StatusNotFound, http.StatusGone:
		return "error.not_found"
	case http.StatusTooManyRequests:
		return "error.rate_limited"
	case http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusGatewayTimeout:
		return "error.server_busy"
	default:
		return "error.default_title"
	}
}

func isJSONAPIRequest(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	p := r.URL.Path
	if strings.HasPrefix(p, "/api/v1/auth/oidc/") && p != "/api/v1/auth/oidc/providers" {
		// OIDC /start and /callback are top-level browser redirects/navigations.
		return false
	}
	if p == "/api" || strings.HasPrefix(p, "/api/") {
		return true
	}
	if strings.HasPrefix(p, "/plugins/") {
		parts := strings.SplitN(strings.TrimPrefix(p, "/plugins/"), "/", 3)
		if len(parts) >= 2 && parts[1] == "api" {
			return true
		}
	}
	accept := strings.ToLower(r.Header.Get("Accept"))
	if strings.Contains(accept, "application/json") && !strings.Contains(accept, "text/html") {
		return true
	}
	return false
}

func isSubresourceAssetRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	sfd := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Dest")))
	switch sfd {
	case "image", "style", "script", "font", "audio", "video", "worker", "manifest":
		return true
	case "document", "iframe":
		return false
	}
	accept := strings.ToLower(strings.TrimSpace(r.Header.Get("Accept")))
	if accept != "" && !strings.Contains(accept, "text/html") && !strings.Contains(accept, "*/*") {
		if strings.Contains(accept, "image/") ||
			strings.Contains(accept, "text/css") ||
			strings.Contains(accept, "javascript") ||
			strings.Contains(accept, "font/") ||
			strings.Contains(accept, "audio/") ||
			strings.Contains(accept, "video/") {
			return true
		}
	}
	return false
}

var (
	benign404ExactPaths = map[string]struct{}{
		"/favicon.ico":                      {},
		"/favicon.svg":                      {},
		"/assets/favicon.svg":               {},
		"/apple-touch-icon.png":             {},
		"/apple-touch-icon-precomposed.png": {},
		"/browserconfig.xml":                {},
		"/manifest.json":                    {},
		"/site.webmanifest":                 {},
		"/robots.txt":                       {},
		"/sitemap.xml":                      {},
		"/.well-known/traffic-advice":       {},
		"/.well-known/assetlinks.json":      {},
	}

	highRisk404Suffixes = []string{
		".php", ".asp", ".aspx", ".jsp", ".cgi",
		".env", ".bak", ".sql", ".ini", ".conf", ".sh",
	}

	highRisk404Prefixes = []string{
		"/.svn", "/.hg", "/.ds_store", "/wp-content", "/wp-includes",
		"/administrator", "/admin.php", "/config.php", "/backup",
		"/server-status", "/debug", "/setup", "/api/setup",
	}
)

func classify404Risk(r *http.Request) (reasonCode string, weight int, track bool) {
	if r == nil || r.URL == nil {
		return "", 0, false
	}
	rawPath := r.URL.Path
	lowPath := strings.ToLower(rawPath)

	if strings.Contains(rawPath, "..") || strings.Contains(strings.ToLower(r.URL.RawPath), "%2e%2e") {
		return security.ReasonScannerTrap, 3, true
	}
	for _, pfx := range highRisk404Prefixes {
		if lowPath == pfx || strings.HasPrefix(lowPath, pfx+"/") {
			if pfx == "/setup" || pfx == "/api/setup" {
				return security.ReasonScannerTrap, 2, true
			}
			return security.ReasonScannerTrap, 3, true
		}
	}
	for _, sfx := range highRisk404Suffixes {
		if strings.HasSuffix(lowPath, sfx) {
			return security.ReasonScannerTrap, 3, true
		}
	}

	if _, ok := benign404ExactPaths[lowPath]; ok {
		return "", 0, false
	}
	if strings.HasPrefix(lowPath, "/apple-touch-icon") ||
		strings.HasPrefix(lowPath, "/.well-known/appspecific/") ||
		strings.HasSuffix(lowPath, ".map") {
		return "", 0, false
	}
	if isSubresourceAssetRequest(r) {
		return "", 0, false
	}
	return security.ReasonNotFoundScan, 1, true
}

func (s *AppServer) record404ProbeOnce(r *http.Request, viewer *domain.User, forceReason string, forceWeight int) {
	if s == nil || s.deps.WAF == nil || !markErrorProbeRecorded(r) {
		return
	}
	reason := forceReason
	weight := forceWeight
	if reason == "" || weight <= 0 {
		var track bool
		reason, weight, track = classify404Risk(r)
		if !track {
			return
		}
	}
	var tp config.TrustedProxiesPolicy
	if s.deps.Config != nil {
		tp = s.deps.Config.TrustedProxies
	}
	clientIP := security.ResolveClientIP(r, tp)
	skipBan := isActiveStaffUser(viewer) && s.isStaffTOTPCompliant(r.Context(), viewer)
	if !skipBan && viewer == nil {
		skipBan = s.isStaffRequest(r)
	}
	reqPath := "/"
	if r != nil && r.URL != nil && r.URL.Path != "" {
		reqPath = r.URL.Path
	}
	s.deps.WAF.RecordProbe404(r.Context(), clientIP, reason, weight, reqPath, skipBan)
}

// handleWAFError renders themed HTML error pages for browser requests and localized JSON for /api/* requests
// whenever WAF blocks an IP (403), GeoIP denies access (403), rate shield throttles (429), or traps fire (404).
func (s *AppServer) handleWAFError(w http.ResponseWriter, r *http.Request, status int, msgKey string) {
	if msgKey == "" {
		msgKey = defaultErrorKeyForStatus(status)
	}
	r, tr := withErrorProbeTracker(r)
	// WAFEngine already recorded its strike/probe before calling handleWAFError.
	tr.recorded.Store(true)

	if isJSONAPIRequest(r) {
		locale := s.resolveLocale(r)
		writeErrJSON(w, status, s.deps.Catalog.T(locale, msgKey))
		return
	}
	if isSubresourceAssetRequest(r) {
		w.WriteHeader(status)
		return
	}
	s.renderLightweightErrorPage(w, r, status, msgKey)
}

// renderLightweightErrorPage renders the active theme's standalone frameless error.html purely from in-memory snapshots
// with zero SQL queries or plugin VM execution, protecting the database under scanner/banned-IP floods.
func (s *AppServer) renderLightweightErrorPage(w http.ResponseWriter, r *http.Request, status int, msgKey string) {
	s.renderLightweightErrorPageWithViewer(w, r, nil, status, msgKey)
}

func (s *AppServer) renderLightweightErrorPageWithViewer(w http.ResponseWriter, r *http.Request, viewer *domain.User, status int, msgKey string) {
	snap := s.deps.Settings.Snapshot()
	locale := s.deps.Catalog.ResolveRequest(r, snap.Site.DefaultLocale, snap.Site.AllowLocaleSwitch)
	activeTheme := theme.DefaultThemeID
	var resolvedThemeCfg map[string]any
	if s.deps.Themes != nil {
		activeTheme = s.deps.Themes.ResolveThemeForRequest(r, viewer, snap.Appearance)
		resolvedThemeCfg = s.deps.Themes.ResolveThemeConfig(activeTheme, snap.Appearance.ThemeConfigs)
	}
	siteCfg := snap.Site
	siteCfg.CustomHeadMeta = repo.NormalizeSiteInjectionHTML(siteCfg.CustomHeadMeta)
	siteCfg.NoticeBoard = repo.NormalizeSiteInjectionHTML(siteCfg.NoticeBoard)
	if !siteCfg.EnableFooterHTML {
		siteCfg.FooterText = ""
	} else if siteCfg.FooterText != "" {
		siteCfg.FooterText = repo.NormalizeSiteInjectionHTML(siteCfg.FooterText)
	}
	localizeUnmodifiedSiteDefaults(&siteCfg, locale)
	adminPath, _ := s.effectiveAdminPrefix()

	pctx := theme.PageContext{
		Locale:          locale,
		TextDir:         s.deps.Catalog.Dir(locale),
		Locales:         s.deps.Catalog.Locales(),
		Year:            time.Now().In(s.siteTimezoneLocation(snap.Site.TimeZone)).Year(),
		CurrentPath:     r.URL.Path,
		RawQuery:        r.URL.RawQuery,
		BasePath:        r.URL.Path,
		CanonicalURL:    s.publicBaseURL(r) + r.URL.Path,
		PageTitle:       siteCfg.Title,
		MetaDescription: siteCfg.Description,
		AdminPath:       adminPath,
		ThemeID:         activeTheme,
		ThemeConfig:     resolvedThemeCfg,
		Site:            siteCfg,
		Reading:         snap.Reading,
		Feed:            snap.Feed,
		SEO:             snap.SEO,
		Appearance:      snap.Appearance,
		Viewer:          viewer,
		StatusCode:      status,
		ErrorMessage:    s.deps.Catalog.T(locale, msgKey),
		NoIndex:         true,
	}
	addVaryHeader(w.Header(), "Accept-Encoding", "Accept-Language")
	w.Header().Set("Cache-Control", "no-store")
	if err := s.deps.Themes.RenderWithFallback(w, status, activeTheme, []string{"error.html"}, pctx); err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		msg := html.EscapeString(pctx.ErrorMessage)
		if msg == "" {
			msg = html.EscapeString(http.StatusText(status))
		}
		_, _ = fmt.Fprintf(w, "<!doctype html><html><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width,initial-scale=1\"><title>%d</title></head><body style=\"font-family:system-ui,sans-serif;padding:48px 24px;max-width:560px;margin:0 auto;color:#222\"><h1 style=\"margin:0 0 8px\">%d</h1><p style=\"margin:0 0 16px;color:#666\">%s</p><a href=\"/\">&larr; Home</a></body></html>", status, status, msg)
	}
}

type customErrorInterceptWriter struct {
	http.ResponseWriter
	status      int
	intercepted bool
	wroteHeader bool
	buf         []byte
}

func (cw *customErrorInterceptWriter) Unwrap() http.ResponseWriter {
	return cw.ResponseWriter
}

func (cw *customErrorInterceptWriter) Flush() {
	if cw.intercepted {
		return
	}
	if f, ok := cw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (cw *customErrorInterceptWriter) shouldInterceptStatus(code int) bool {
	if code < 400 {
		return false
	}
	ct := strings.ToLower(strings.TrimSpace(cw.Header().Get("Content-Type")))
	if ct == "" || strings.HasPrefix(ct, "text/plain") {
		return true
	}
	return false
}

func (cw *customErrorInterceptWriter) WriteHeader(code int) {
	if cw.wroteHeader {
		return
	}
	cw.wroteHeader = true
	cw.status = code
	if cw.shouldInterceptStatus(code) {
		cw.intercepted = true
		return
	}
	cw.ResponseWriter.WriteHeader(code)
}

func (cw *customErrorInterceptWriter) Write(p []byte) (int, error) {
	if !cw.wroteHeader {
		cw.WriteHeader(http.StatusOK)
	}
	if !cw.intercepted {
		return cw.ResponseWriter.Write(p)
	}
	if len(cw.buf) < 512 {
		rem := 512 - len(cw.buf)
		if len(p) <= rem {
			cw.buf = append(cw.buf, p...)
		} else {
			cw.buf = append(cw.buf, p[:rem]...)
		}
	}
	return len(p), nil
}

// wrapCustomErrorPages ensures that ANY plain-text or empty 4xx/5xx error emitted anywhere in the mux
// (including http.NotFound, http.Error, method-not-allowed, disabled feeds, setup cloaking, or plugin 404s)
// renders the active theme's custom error.html for browser requests (or structured JSON for /api/*)
// AND records 404 risk-control signals without ever double-counting.
func (s *AppServer) wrapCustomErrorPages(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r, _ = withErrorProbeTracker(r)
		cw := &customErrorInterceptWriter{ResponseWriter: w}
		next.ServeHTTP(cw, r)

		if !cw.intercepted {
			return
		}

		status := cw.status
		if status == http.StatusNotFound {
			s.record404ProbeOnce(r, nil, "", 0)
		}

		msgKey := defaultErrorKeyForStatus(status)
		if isJSONAPIRequest(r) {
			w.Header().Del("Content-Length")
			locale := s.resolveLocale(r)
			writeErrJSON(w, status, s.deps.Catalog.T(locale, msgKey))
			return
		}

		if isSubresourceAssetRequest(r) {
			w.WriteHeader(status)
			if r.Method != http.MethodHead && len(cw.buf) > 0 {
				_, _ = w.Write(cw.buf)
			}
			return
		}

		w.Header().Del("Content-Type")
		w.Header().Del("Content-Length")
		s.renderErrorPage(w, r, status, msgKey)
	})
}

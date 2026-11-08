package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Willh92/Volo-go/internal/auth"
	"github.com/Willh92/Volo-go/internal/backup"
	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/content"
	"github.com/Willh92/Volo-go/internal/db/dialect"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/guest"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/render"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/internal/security"
	"github.com/Willh92/Volo-go/internal/theme"
	"github.com/Willh92/Volo-go/internal/visibility"
	"github.com/Willh92/Volo-go/pkg/plugin"
)

// VerificationMailSender is an optional callback wired in M5 to deliver reader 6-digit email codes via SMTP.
type VerificationMailSender func(ctx context.Context, toEmail string, code string, locale string) error

// CommentEventNotifier is an optional callback wired in M5 to dispatch email/webhook comment notifications.
type CommentEventNotifier func(ctx context.Context, comment content.PublicCommentView, in content.CreateCommentInput)

// AppDeps bundles all runtime dependencies for the unified Volo-go HTTP server.
type AppDeps struct {
	Config          *config.Config
	Store           *sqlx.Store
	Settings        *repo.SettingsManager
	Catalog         *i18n.Catalog
	Auth            *auth.Service
	Passports       *guest.PassportManager
	PoW             *guest.PoWVerifier
	Geo             *security.GeoEngine
	WAF             *security.WAFEngine
	OutboundDialer  *security.OutboundSafeDialer
	Plugins         *plugin.Manager
	Content         *content.Service
	Themes          *theme.Engine
	BackupScheduler *backup.Scheduler
	StartedAt       time.Time
	Version         string

	SendVerificationMail   VerificationMailSender
	NotifyCommentCreated   CommentEventNotifier
	Media                  MediaProcessor
	NotifyArticlePublished func(ctx context.Context, raw domain.RawArticle)
	TestSMTP               func(ctx context.Context, cfg domain.NotificationsSettings, overridePassword string, toEmail string, locale string) error
	TestWebhook            func(ctx context.Context, wh domain.Webhook, locale string) (int, error)
	ExtraMuxSetup          func(mux *http.ServeMux)
}

type backupExportTicket struct {
	UserID    string
	SessionID string
	IPBucket  string
	ExpiresAt time.Time
}

// AppServer serves all public pages, feeds, zero-copy Range media, plugins, and v1 APIs (plus Admin/Setup routes).
type AppServer struct {
	deps    AppDeps
	handler http.Handler

	setupMu sync.Mutex

	backupTicketMu sync.Mutex
	backupTickets  map[string]backupExportTicket

	mediaRefCacheMu      sync.Mutex
	mediaRefCachedRev    uint64
	mediaRefCachedTokens map[string]int
	mediaRefOrphanCounts map[string]int
}

// NewAppServer constructs and wires a complete AppServer instance.
func NewAppServer(deps AppDeps) (*AppServer, error) {
	if deps.StartedAt.IsZero() {
		deps.StartedAt = time.Now()
	}
	if deps.OutboundDialer == nil {
		deps.OutboundDialer = &security.OutboundSafeDialer{Timeout: 10 * time.Second}
	}
	if deps.Catalog == nil {
		cat, err := i18n.NewCatalog(deps.Config.DefaultLocale, deps.Config.LocalesDir)
		if err != nil {
			return nil, err
		}
		deps.Catalog = cat
	}
	if deps.Settings == nil {
		deps.Settings = repo.NewSettingsManager(deps.Store, deps.Config.KeyRing, deps.Config.DefaultLocale)
		_, _ = deps.Settings.Reload(context.Background())
	}
	pubURL := ""
	if deps.Config != nil {
		pubURL = deps.Config.GetPublicURL()
	}
	if deps.Passports == nil {
		deps.Passports = guest.NewPassportManager(deps.Config.KeyRing, pubURL)
	}
	if deps.PoW == nil {
		deps.PoW = guest.NewPoWVerifier(deps.Config.KeyRing)
	}
	if deps.Auth == nil {
		deps.Auth = auth.NewService(deps.Store, deps.Config.KeyRing, deps.Settings, pubURL)
	}
	if deps.Geo == nil {
		deps.Geo = security.NewGeoEngine(deps.Config.GeoIPDir, deps.Settings)
		_ = deps.Geo.ReloadRules(context.Background(), deps.Store)
	}
	if deps.WAF == nil {
		deps.WAF = security.NewWAFEngine(deps.Config, deps.Store, deps.Settings, deps.Geo)
		_ = deps.WAF.ReloadBans(context.Background())
	} else if deps.Config != nil {
		deps.WAF.SetPublicURL(pubURL)
	}
	if deps.Themes == nil {
		th, err := theme.NewEngine(deps.Catalog, deps.Config.ThemesDir)
		if err != nil {
			return nil, err
		}
		deps.Themes = th
	}
	deps.Themes.SetAssetCDNPrefixFunc(func() string {
		return deps.Settings.Snapshot().Reading.AssetCDNPrefix
	})
	deps.Themes.SetVendorCDNPrefixFunc(func() string {
		return deps.Settings.Snapshot().Reading.VendorCDNPrefix
	})
	if deps.Content == nil {
		rnd := render.Default()
		grd := visibility.NewGuard(deps.Config.KeyRing, deps.Settings, deps.Passports, deps.Catalog, rnd)
		srch := visibility.NewSearchIndex(deps.Store, rnd, visibility.DefaultSearchMaxBytes)
		deps.Content = content.NewService(content.ServiceOptions{
			Store:          deps.Store,
			Keys:           deps.Config.KeyRing,
			Settings:       deps.Settings,
			Passports:      deps.Passports,
			PoW:            deps.PoW,
			Guard:          grd,
			Search:         srch,
			Renderer:       rnd,
			Plugins:        deps.Plugins,
			Catalog:        deps.Catalog,
			OutboundDialer: deps.OutboundDialer,
		})
		_ = deps.Content.WarmCaches(context.Background())
	}
	if deps.Content != nil && deps.NotifyArticlePublished != nil {
		deps.Content.SetArticlePublishedCallback(deps.NotifyArticlePublished)
	}

	s := &AppServer{deps: deps}
	s.syncConfiguredUploadDir()
	if deps.Plugins != nil {
		deps.Plugins.SetSiteInfoFunc(func() map[string]any {
			snap := deps.Settings.Snapshot()
			pubURL := ""
			if deps.Config != nil {
				pubURL = deps.Config.GetPublicURL()
			}
			return map[string]any{
				"title":          snap.Site.Title,
				"subtitle":       snap.Site.Subtitle,
				"description":    snap.Site.Description,
				"public_url":     pubURL,
				"default_locale": snap.Site.DefaultLocale,
				"active_theme":   snap.Appearance.ActiveTheme,
				"version":        deps.Version,
			}
		})
		deps.Plugins.SetRequestInspector(func(r *http.Request) (bool, string) {
			viewer, sess := s.resolveViewer(r)
			isAdmin := viewer != nil && viewer.Status == domain.UserStatusActive && viewer.Role == domain.RoleAdmin
			if isAdmin && isStateChangingHTTPMethod(r.Method) {
				csrfOK := sess != nil && s.deps.Auth != nil &&
					s.deps.Auth.VerifyCSRFToken(sess.ID, r.Header.Get("X-CSRF-Token"))
				if !s.isSameOriginRequest(r) || !csrfOK {
					isAdmin = false
				}
			}
			var proxies config.TrustedProxiesPolicy
			if s.deps.Config != nil {
				proxies = s.deps.Config.TrustedProxies
			}
			clientIP := security.ResolveClientIP(r, proxies)
			return isAdmin, security.MaskIP(clientIP)
		})
	}
	mux := http.NewServeMux()
	s.registerRoutes(mux)
	s.registerSetupRoutes(mux)
	s.registerAdminRoutes(mux)
	if deps.ExtraMuxSetup != nil {
		deps.ExtraMuxSetup(mux)
	}

	var root http.Handler = s.withDynamicAdminMount(mux)
	root = s.wrapCustomErrorPages(root)
	root = wrapPublicDynamicGzip(root, func() string {
		p, _ := s.effectiveAdminPrefix()
		return p
	})
	root = s.withRedirectGuardian(root)
	if deps.WAF != nil {
		deps.WAF.SetErrorHandler(s.handleWAFError)
		deps.WAF.SetStaffBypassCheck(s.isStaffRequest)
		root = deps.WAF.WrapHTTP(root)
	}
	s.handler = root
	return s, nil
}

func (s *AppServer) Handler() http.Handler { return s.handler }
func (s *AppServer) Deps() AppDeps         { return s.deps }

// Close flushes buffered view/analytics counters and stops background workers before database shutdown.
func (s *AppServer) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if s.deps.WAF != nil {
		s.deps.WAF.Close()
	}
	if s.deps.Content == nil {
		return nil
	}
	return s.deps.Content.Close(ctx)
}

func isStateChangingHTTPMethod(method string) bool {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func (s *AppServer) handlePluginMount(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.deps.Plugins == nil || next == nil {
			http.NotFound(w, r)
			return
		}
		trimmed := strings.TrimPrefix(r.URL.Path, "/plugins/")
		parts := strings.SplitN(trimmed, "/", 3)
		if len(parts) < 2 || parts[0] == "" {
			http.NotFound(w, r)
			return
		}
		pluginID, namespace := parts[0], parts[1]
		reqToServe := r
		if namespace == "api" {
			clonedHdr := r.Header.Clone()
			modified := false
			if isStateChangingHTTPMethod(r.Method) {
				_, sess := s.resolveViewer(r)
				csrfOK := sess != nil && s.deps.Auth != nil &&
					s.deps.Auth.VerifyCSRFToken(sess.ID, r.Header.Get("X-CSRF-Token"))
				if !s.isSameOriginRequest(r) || !csrfOK {
					// Strip ambient session cookies so unauthenticated third-party webhooks
					// reach ServeAPI with IsAdmin: false without ambient session elevation.
					clonedHdr.Del("Cookie")
					modified = true
				}
			}
			if !s.deps.Plugins.HasPermission(pluginID, "http:raw_headers") && !s.deps.Plugins.HasPermission(pluginID, "raw_headers") {
				for _, sigHdr := range []string{
					"X-Hub-Signature-256",
					"X-Hub-Signature",
					"X-Signature",
					"X-Webhook-Signature",
					"Stripe-Signature",
					"Authorization",
				} {
					if clonedHdr.Get(sigHdr) != "" {
						clonedHdr.Del(sigHdr)
						modified = true
					}
				}
			}
			if modified {
				reqCopy := r.Clone(r.Context())
				reqCopy.Header = clonedHdr
				reqToServe = reqCopy
			}
		}
		next.ServeHTTP(w, reqToServe)
	})
}

func (s *AppServer) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", NewHealthzHandler(s.deps.Store, s.deps.StartedAt))
	mux.HandleFunc("/favicon.svg", s.handleServeFaviconSVG)
	mux.HandleFunc("/assets/favicon.svg", s.handleServeFaviconSVG)
	mux.HandleFunc("/favicon.ico", s.handleServeFaviconICO)
	mux.HandleFunc("/uploads/", s.handleServeUpload)
	mux.HandleFunc("/themes/", s.handleServeThemeAsset)
	mux.HandleFunc("/avatars/", s.handleServeAvatar)
	if s.deps.Plugins != nil {
		mux.Handle("/plugins/", s.handlePluginMount(s.deps.Plugins.Handler()))
	}

	// Machine & AI Distribution Feeds
	mux.HandleFunc("/rss.xml", s.handleRSS)
	mux.HandleFunc("/atom.xml", s.handleAtom)
	mux.HandleFunc("/feed.json", s.handleJSONFeed)
	mux.HandleFunc("/podcast.xml", s.handlePodcast)
	mux.HandleFunc("/sitemap.xml", s.handleSitemap)
	mux.HandleFunc("/robots.txt", s.handleRobotsTxt)
	mux.HandleFunc("/llms.txt", func(w http.ResponseWriter, r *http.Request) { s.handleLLMSTxt(w, r, false) })
	mux.HandleFunc("/llms-full.txt", func(w http.ResponseWriter, r *http.Request) { s.handleLLMSTxt(w, r, true) })
	mux.HandleFunc("/github-stats/", s.handleGitHubStatsSVG)

	// Public JSON v1 APIs
	mux.HandleFunc("/api/v1/i18n", s.handlePublicI18nAPI)
	mux.HandleFunc("/api/v1/i18n/", s.handlePublicI18nAPI)
	mux.HandleFunc("/api/v1/pow/challenge", s.handlePoWChallenge)
	mux.HandleFunc("/api/v1/guest/passport", s.handleGuestPassport)
	mux.HandleFunc("/api/v1/search", s.handleSearchAPI)
	mux.HandleFunc("/api/v1/articles/", s.handleArticleActionAPI)
	mux.HandleFunc("/api/v1/comments", s.handleCommentsRootAPI)
	mux.HandleFunc("/api/v1/comments/", s.handleCommentItemAPI)
	mux.HandleFunc("/api/v1/reactions", s.handleReactionsAPI)
	mux.HandleFunc("/api/v1/reader/send-code", s.handleReaderSendCode)
	mux.HandleFunc("/api/v1/reader/register", s.handleReaderRegister)
	mux.HandleFunc("/api/v1/reader/login", s.handleReaderLogin)
	mux.HandleFunc("/api/v1/reader/logout", s.handleReaderLogout)
	mux.HandleFunc("/api/v1/reader/claim-guest-comments", s.handleReaderClaimComments)
	mux.HandleFunc("/api/v1/auth/oidc", s.handleOIDCAuthRoutes)
	mux.HandleFunc("/api/v1/auth/oidc/", s.handleOIDCAuthRoutes)

	// Legacy Bolo-Solo 301 Canonical Skin Routes & Compatibility Shims
	mux.HandleFunc("/archives.html", s.permanentRedirectTo("/archives"))
	mux.HandleFunc("/archive.html", s.permanentRedirectTo("/archives"))
	mux.HandleFunc("/archivedate.html", s.permanentRedirectTo("/archives"))
	mux.HandleFunc("/archive", s.permanentRedirectTo("/archives"))
	mux.HandleFunc("/archive/", s.handleLegacyArchiveRedirect)
	mux.HandleFunc("/archivedate/", s.handleLegacyArchiveRedirect)
	mux.HandleFunc("/tags.html", s.permanentRedirectTo("/tags"))
	mux.HandleFunc("/categories.html", s.permanentRedirectTo("/categories"))
	mux.HandleFunc("/links.html", s.permanentRedirectTo("/links"))
	mux.HandleFunc("/follows.html", s.permanentRedirectTo("/follows"))
	mux.HandleFunc("/dynamic.html", s.permanentRedirectTo("/dynamic"))
	mux.HandleFunc("/category/", s.handleLegacyCategoryRedirect)
	mux.HandleFunc("/articles/", s.handleLegacyArticleRedirect)
	mux.HandleFunc("/admin-index.do", s.handleLegacyAdminRedirect)
	mux.HandleFunc("/admin/index.do", s.handleLegacyAdminRedirect)
	mux.HandleFunc("/logout", s.handlePublicLogoutRedirect)
	mux.HandleFunc("/github", s.handlePublicGitHubShowcaseRoute)
	mux.HandleFunc("/my-github-repos", s.handlePublicGitHubShowcaseRoute)
	mux.HandleFunc("/images/", s.handleLegacyImageShim)

	// Public Studio HTML Routes
	mux.HandleFunc("/posts/", s.handlePostRoute)
	mux.HandleFunc("/categories", s.handleCategoriesOverview)
	mux.HandleFunc("/categories/", s.handleCategoryPosts)
	mux.HandleFunc("/tags", s.handleTagsOverview)
	mux.HandleFunc("/tags/", s.handleTagPosts)
	mux.HandleFunc("/archives", s.handleArchivesOverview)
	mux.HandleFunc("/archives/", s.handleArchiveMonthPosts)
	mux.HandleFunc("/series", s.handleSeriesOverview)
	mux.HandleFunc("/series/", s.handleSeriesDetail)
	mux.HandleFunc("/books", s.handleBookshelf)
	mux.HandleFunc("/books/", s.handleBookReader)
	mux.HandleFunc("/links", s.handleLinksPage)
	mux.HandleFunc("/follows", s.handleFollowsPage)
	mux.HandleFunc("/dynamic", s.handleDynamicPage)
	mux.HandleFunc("/reader/login", s.handleReaderAuthPage)
	mux.HandleFunc("/reader/register", s.handleReaderAuthPage)
	mux.HandleFunc("/", s.handleRootOrStandalonePage)
}

func (s *AppServer) withRedirectGuardian(next http.Handler) http.Handler {
	staticAdmin := s.staticAdminPrefix()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		adminPrefix, _ := s.effectiveAdminPrefix()
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
			p != "/" &&
			p != "/setup" &&
			!strings.HasPrefix(p, "/setup/") &&
			p != adminPrefix &&
			!strings.HasPrefix(p, adminPrefix+"/") &&
			p != staticAdmin &&
			!strings.HasPrefix(p, staticAdmin+"/") &&
			!strings.HasPrefix(p, "/api/") &&
			!strings.HasPrefix(p, "/themes/") &&
			!strings.HasPrefix(p, "/uploads/") &&
			!strings.HasPrefix(p, "/images/") &&
			!strings.HasPrefix(p, "/plugins/") &&
			!strings.HasPrefix(p, "/avatars/") &&
			p != "/logout" &&
			p != "/favicon.svg" &&
			p != "/favicon.ico" &&
			p != "/assets/favicon.svg" &&
			p != "/healthz" {
			if target, code, ok := s.deps.Content.ResolveRedirect(r.Context(), p); ok {
				dest := target
				if r.URL.RawQuery != "" && !strings.Contains(dest, "?") {
					dest += "?" + r.URL.RawQuery
				}
				http.Redirect(w, r, dest, code)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// --- Zero-Copy Media Range Serving & Theme Assets ---

func isInlineSafeUploadExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".avif", ".mp3", ".m4a", ".ogg", ".wav", ".weba", ".webm", ".mp4":
		return true
	default:
		return false
	}
}

func explicitInlineMediaMIME(ext string) string {
	switch strings.ToLower(ext) {
	case ".weba":
		return "audio/webm"
	case ".m4a":
		return "audio/mp4"
	case ".mp3":
		return "audio/mpeg"
	case ".ogg":
		return "audio/ogg"
	case ".wav":
		return "audio/wav"
	case ".webm":
		return "video/webm"
	case ".mp4":
		return "video/mp4"
	default:
		return ""
	}
}

var contentDispositionSanitizer = strings.NewReplacer(`"`, "_", "\r", "", "\n", "", `\`, "_")

func serveSafeUserStaticFile(w http.ResponseWriter, r *http.Request, baseDir string, cleanRel string) bool {
	if strings.TrimSpace(baseDir) == "" || cleanRel == "" || strings.Contains(cleanRel, "..") {
		return false
	}
	rootAbs, err := filepath.Abs(baseDir)
	if err != nil {
		return false
	}
	if evalRoot, eErr := filepath.EvalSymlinks(rootAbs); eErr == nil {
		rootAbs = evalRoot
	}

	fullPath := filepath.Join(rootAbs, filepath.FromSlash(cleanRel))
	realFile, err := filepath.EvalSymlinks(fullPath)
	if err != nil {
		return false
	}
	if realFile != rootAbs && !strings.HasPrefix(realFile, rootAbs+string(filepath.Separator)) {
		return false
	}

	f, err := os.Open(realFile)
	if err != nil {
		return false
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil || st.IsDir() || !st.Mode().IsRegular() {
		return false
	}

	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; media-src 'self'; style-src 'unsafe-inline'; sandbox")
	w.Header().Set("Cache-Control", "public, max-age=2592000")
	etag := fmt.Sprintf(`"%x-%x"`, st.ModTime().UnixNano(), st.Size())
	w.Header().Set("ETag", etag)

	name := st.Name()
	ext := filepath.Ext(name)
	if !isInlineSafeUploadExt(ext) {
		safeName := contentDispositionSanitizer.Replace(name)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, safeName))
	} else if mimeHdr := explicitInlineMediaMIME(ext); mimeHdr != "" {
		w.Header().Set("Content-Type", mimeHdr)
	}

	// Zero-copy HTTP Range / 206 Partial Content streaming via stdlib http.ServeContent!
	http.ServeContent(w, r, name, st.ModTime(), f)
	return true
}

func (s *AppServer) handleServeUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	rel := strings.TrimPrefix(r.URL.Path, "/uploads/")
	cleanRel := strings.TrimPrefix(path.Clean("/"+rel), "/")
	if cleanRel == "" || strings.Contains(cleanRel, "..") {
		http.NotFound(w, r)
		return
	}
	activeDir := s.deps.Config.GetUploadDir()
	if serveSafeUserStaticFile(w, r, activeDir, cleanRel) {
		return
	}
	if defDir := s.deps.Config.GetDefaultUploadDir(); defDir != "" && defDir != activeDir {
		if serveSafeUserStaticFile(w, r, defDir, cleanRel) {
			return
		}
	}
	http.NotFound(w, r)
}

func (s *AppServer) handleServeThemeAsset(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/themes/")
	if strings.HasSuffix(rest, "/preview") {
		themeID := strings.TrimSuffix(rest, "/preview")
		if themeID == "" || strings.Contains(themeID, "/") {
			http.NotFound(w, r)
			return
		}
		s.deps.Themes.ServePreview(w, r, themeID)
		return
	}
	parts := strings.SplitN(rest, "/assets/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		http.NotFound(w, r)
		return
	}
	s.deps.Themes.ServeAsset(w, r, parts[0], parts[1])
}

func (s *AppServer) permanentRedirectTo(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dest := target
		if r.URL.RawQuery != "" {
			dest += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, dest, http.StatusMovedPermanently)
	}
}

func (s *AppServer) handleLegacyCategoryRedirect(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/category/"), ".html")
	if slug == "" {
		http.Redirect(w, r, "/categories", http.StatusMovedPermanently)
		return
	}
	dest := "/categories/" + slug
	if r.URL.RawQuery != "" {
		dest += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, dest, http.StatusMovedPermanently)
}

func (s *AppServer) handleLegacyArticleRedirect(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/articles/"), "/")
	if trimmed == "" {
		http.Redirect(w, r, "/archives", http.StatusMovedPermanently)
		return
	}
	parts := strings.Split(trimmed, "/")
	slug := strings.TrimSuffix(parts[len(parts)-1], ".html")
	if slug == "" || strings.Contains(slug, "..") {
		http.NotFound(w, r)
		return
	}
	dest := "/posts/" + slug
	if r.URL.RawQuery != "" {
		dest += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, dest, http.StatusMovedPermanently)
}

func (s *AppServer) handleServeAvatar(w http.ResponseWriter, r *http.Request) {
	seed := strings.TrimPrefix(r.URL.Path, "/avatars/")
	guest.ServeAvatarHTTP(w, r, seed)
}

// --- Feeds, Sitemap, Robots, LLMs & Raw Markdown ---

func normalizeTrustedBaseURL(raw string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if trimmed == "" || strings.ContainsAny(trimmed, "\r\n\t ") {
		return ""
	}
	u, err := url.Parse(trimmed)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return ""
	}
	if strings.ContainsAny(u.Host, `/\@`) {
		return ""
	}
	cleanPath := strings.TrimRight(u.Path, "/")
	return strings.ToLower(u.Scheme) + "://" + u.Host + cleanPath
}

func sanitizePublicRequestHost(rawHost string) string {
	h := strings.TrimSpace(rawHost)
	if h == "" || len(h) > 255 {
		return "localhost:8080"
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if c <= 0x20 || c >= 0x7f || c == '/' || c == '\\' || c == '@' || c == '?' || c == '#' {
			return "localhost:8080"
		}
	}
	return h
}

func (s *AppServer) publicBaseURL(r *http.Request) string {
	if s.deps.Config != nil {
		if base := normalizeTrustedBaseURL(s.deps.Config.GetPublicURL()); base != "" {
			return base
		}
	}
	if s.deps.Settings != nil {
		snap := s.deps.Settings.Snapshot()
		if snap.Site.CustomVars != nil {
			for _, key := range []string{"site_url", "public_url", "url"} {
				if base := normalizeTrustedBaseURL(snap.Site.CustomVars[key]); base != "" {
					return base
				}
			}
		}
	}
	scheme := s.effectiveRequestScheme(r)
	host := "localhost:8080"
	if r != nil {
		host = sanitizePublicRequestHost(r.Host)
	}
	return scheme + "://" + host
}

func (s *AppServer) resolveLocale(r *http.Request) string {
	snap := s.deps.Settings.Snapshot()
	return s.deps.Catalog.ResolveRequest(r, snap.Site.DefaultLocale, snap.Site.AllowLocaleSwitch)
}

func (s *AppServer) resolveViewer(r *http.Request) (*domain.User, *domain.UserSession) {
	if r == nil || s.deps.Auth == nil {
		return nil, nil
	}
	ck, err := r.Cookie(auth.SessionCookieName)
	if err != nil || strings.TrimSpace(ck.Value) == "" {
		return nil, nil
	}
	u, sess, err := s.deps.Auth.AuthenticateSession(r.Context(), ck.Value)
	if err != nil {
		return nil, nil
	}
	return u, sess
}

func (s *AppServer) handleRSS(w http.ResponseWriter, r *http.Request) {
	doc, err := s.deps.Content.GetCachedRSS(r.Context(), s.publicBaseURL(r), s.resolveLocale(r))
	if err != nil {
		http.Error(w, "feed error", http.StatusInternalServerError)
		return
	}
	addVaryHeader(w.Header(), "Accept-Language", "Cookie")
	serveCachedDocument(w, r, doc)
}

func (s *AppServer) handleAtom(w http.ResponseWriter, r *http.Request) {
	doc, err := s.deps.Content.GetCachedAtom(r.Context(), s.publicBaseURL(r), s.resolveLocale(r))
	if err != nil {
		http.Error(w, "feed error", http.StatusInternalServerError)
		return
	}
	addVaryHeader(w.Header(), "Accept-Language", "Cookie")
	serveCachedDocument(w, r, doc)
}

func (s *AppServer) handleJSONFeed(w http.ResponseWriter, r *http.Request) {
	doc, err := s.deps.Content.GetCachedJSONFeed(r.Context(), s.publicBaseURL(r), s.resolveLocale(r))
	if err != nil {
		http.Error(w, "feed error", http.StatusInternalServerError)
		return
	}
	addVaryHeader(w.Header(), "Accept-Language", "Cookie")
	serveCachedDocument(w, r, doc)
}

func (s *AppServer) handlePodcast(w http.ResponseWriter, r *http.Request) {
	if !s.deps.Settings.Snapshot().Feed.Podcast.Enabled {
		http.NotFound(w, r)
		return
	}
	doc, err := s.deps.Content.GetCachedPodcast(r.Context(), s.publicBaseURL(r), s.resolveLocale(r))
	if err != nil {
		http.Error(w, "podcast feed error", http.StatusInternalServerError)
		return
	}
	addVaryHeader(w.Header(), "Accept-Language", "Cookie")
	serveCachedDocument(w, r, doc)
}

func (s *AppServer) handleSitemap(w http.ResponseWriter, r *http.Request) {
	doc, err := s.deps.Content.GetCachedSitemap(r.Context(), s.publicBaseURL(r))
	if err != nil {
		http.Error(w, "sitemap error", http.StatusInternalServerError)
		return
	}
	serveCachedDocument(w, r, doc)
}

func (s *AppServer) handleRobotsTxt(w http.ResponseWriter, r *http.Request) {
	adminPath, isCustom := s.effectiveAdminPrefix()
	var sb strings.Builder
	sb.WriteString("User-agent: *\nAllow: /\nDisallow: /api/\n")
	if !isCustom {
		sb.WriteString("Disallow: " + adminPath + "/\n")
	}
	sb.WriteString("Sitemap: " + s.publicBaseURL(r) + "/sitemap.xml\n")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(sb.String()))
}

func (s *AppServer) handleLLMSTxt(w http.ResponseWriter, r *http.Request, full bool) {
	if !s.deps.Settings.Snapshot().Feed.EnableLLMSTxt {
		http.NotFound(w, r)
		return
	}
	doc, err := s.deps.Content.GetCachedLLMSTxt(r.Context(), s.publicBaseURL(r), full)
	if err != nil {
		http.Error(w, "llms.txt error", http.StatusInternalServerError)
		return
	}
	serveCachedDocument(w, r, doc)
}

func (s *AppServer) handleGitHubStatsSVG(w http.ResponseWriter, r *http.Request) {
	user := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/github-stats/"), ".svg")
	if strings.TrimSpace(user) == "" {
		http.NotFound(w, r)
		return
	}
	svg := s.deps.Content.BuildGitHubStatsSVGWithTheme(user, r.URL.Query().Get("theme"))
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	addVaryHeader(w.Header(), "Accept-Encoding")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(svg)
}

// --- Public JSON v1 APIs ---

type jsonErrPayload struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeErrJSON(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, jsonErrPayload{Error: msg})
}

func writeOKJSON(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("{\"ok\":true}\n"))
}

func (s *AppServer) handlePublicI18nAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	addVaryHeader(w.Header(), "Accept-Language", "Cookie")
	snap := s.deps.Settings.Snapshot()
	requested := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/i18n"), "/")
	requested = strings.TrimSuffix(requested, ".json")
	if requested == "" {
		requested = strings.TrimSpace(r.URL.Query().Get("locale"))
	}
	if requested == "" {
		requested = s.resolveLocale(r)
	}
	if cached, ok := s.deps.Catalog.CachedAPIResponse(requested, snap.Site.DefaultLocale); ok && len(cached.JSON) > 0 {
		h := w.Header()
		h.Set("Content-Type", "application/json; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
		if cached.ETag != "" {
			h.Set("ETag", cached.ETag)
			if ifNoneMatchMatches(r.Header.Get("If-None-Match"), cached.ETag) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		h.Set("Content-Length", strconv.Itoa(len(cached.JSON)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(cached.JSON)
		}
		return
	}
	resolved := s.deps.Catalog.Match(requested, snap.Site.DefaultLocale)
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, map[string]any{
		"locale":          resolved,
		"dir":             s.deps.Catalog.Dir(resolved),
		"locales":         s.deps.Catalog.Locales(),
		"messages":        s.deps.Catalog.Map(resolved),
		"custom_messages": s.deps.Catalog.CustomMap(resolved),
	})
}

func (s *AppServer) handlePoWChallenge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	bits := s.deps.Settings.Snapshot().Reading.PoWDifficultyBits
	if bits <= 0 {
		bits = 14
	}
	chal, err := s.deps.PoW.IssueChallenge(bits)
	if err != nil {
		writeErrJSON(w, http.StatusInternalServerError, s.localizePublicError(s.resolveLocale(r), err))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, chal)
}

func (s *AppServer) clearGuestPassportCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     guest.PassportCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.isCookieSecureForRequest(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *AppServer) handleGuestPassport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.Method == http.MethodPost && !s.requireSameOrigin(w, r) {
		return
	}
	if viewer, _ := s.resolveViewer(r); viewer != nil && viewer.Role != domain.RoleReader {
		s.clearGuestPassportCookie(w, r)
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{
			"guest_id":       "",
			"passport_token": "",
			"source":         "staff",
		})
		return
	}
	gp, _ := s.deps.Passports.ResolveOrHeal(w, r, true)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"guest_id":       gp.GuestID,
		"passport_token": gp.SignedToken,
		"source":         gp.Source,
	})
}

func (s *AppServer) handleSearchAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.deps.Settings.Snapshot().Reading.EnableInstantSearch {
		http.NotFound(w, r)
		return
	}
	locale := s.resolveLocale(r)
	clientIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
	if !s.deps.WAF.AllowRate("public_search", clientIP, 60, time.Minute) {
		writeErrJSON(w, http.StatusTooManyRequests, s.deps.Catalog.T(locale, "error.rate_limited"))
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	hits, err := s.deps.Content.SearchIndex().Search(r.Context(), q, 20)
	if err != nil {
		writeErrJSON(w, http.StatusInternalServerError, s.deps.Catalog.T(locale, "error.server_busy"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hits": hits})
}

func verifyPageInlinePassword(contentMD string, candidate string) (string, bool) {
	c := strings.TrimSpace(candidate)
	if c == "" {
		return "", false
	}
	var matched string
	for _, inlinePwd := range render.ExtractAllLockPasswords(contentMD) {
		if visibility.EqualLockPassword(inlinePwd, c, "page") && matched == "" {
			matched = inlinePwd
		}
	}
	return matched, matched != ""
}

func (s *AppServer) handleArticleActionAPI(w http.ResponseWriter, r *http.Request) {
	// Matches POST /api/v1/articles/{id}/unlock (supports published articles AND published standalone pages!)
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/articles/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[1] != "unlock" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if !s.requireSameOrigin(w, r) {
		return
	}
	locale := s.resolveLocale(r)
	targetID := strings.TrimSpace(parts[0])
	clientIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
	if !s.deps.WAF.AllowRate("unlock:"+targetID, clientIP, 10, 5*time.Minute) {
		writeErrJSON(w, http.StatusTooManyRequests, s.deps.Catalog.T(locale, "error.rate_limited"))
		return
	}

	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		writeErrJSON(w, http.StatusBadRequest, s.deps.Catalog.T(locale, "error.bad_request"))
		return
	}

	var entryKey, plainPwd string
	if strings.HasPrefix(targetID, "page:") {
		pageID := strings.TrimSpace(strings.TrimPrefix(targetID, "page:"))
		pg, pErr := sqlx.Get[domain.Page](r.Context(), s.deps.Store, `SELECT * FROM pages WHERE id = ? AND is_published = ?`, pageID, true)
		if pErr != nil {
			http.NotFound(w, r)
			return
		}
		matched, ok := verifyPageInlinePassword(pg.ContentMD, req.Password)
		if !ok {
			s.deps.WAF.RecordStrike(r.Context(), clientIP, security.ReasonUnlockFail, 1)
			writeErrJSON(w, http.StatusForbidden, s.deps.Catalog.T(locale, "article.unlock_wrong_pwd"))
			return
		}
		entryKey = visibility.InlineUnlockKey("page:"+pg.ID, matched)
		plainPwd = matched
	} else {
		raw, err := s.deps.Content.GetRawArticleByID(r.Context(), targetID)
		if err == nil && raw.Status == domain.ArticleStatusPublished {
			matched, ok := s.deps.Content.Guard().VerifyArticlePassword(raw, req.Password)
			if !ok {
				s.deps.WAF.RecordStrike(r.Context(), clientIP, security.ReasonUnlockFail, 1)
				writeErrJSON(w, http.StatusForbidden, s.deps.Catalog.T(locale, "article.unlock_wrong_pwd"))
				return
			}
			entryKey = s.deps.Content.Guard().UnlockEntryKey(raw, matched)
			plainPwd = matched
		} else {
			pg, pErr := sqlx.Get[domain.Page](r.Context(), s.deps.Store, `SELECT * FROM pages WHERE id = ? AND is_published = ?`, targetID, true)
			if pErr != nil {
				http.NotFound(w, r)
				return
			}
			matched, ok := verifyPageInlinePassword(pg.ContentMD, req.Password)
			if !ok {
				s.deps.WAF.RecordStrike(r.Context(), clientIP, security.ReasonUnlockFail, 1)
				writeErrJSON(w, http.StatusForbidden, s.deps.Catalog.T(locale, "article.unlock_wrong_pwd"))
				return
			}
			entryKey = visibility.InlineUnlockKey("page:"+pg.ID, matched)
			plainPwd = matched
		}
	}

	gp, _ := s.deps.Passports.ResolveOrHeal(w, r, true)
	unlockState, err := s.deps.Passports.GrantArticleUnlock(w, r, gp.GuestID, entryKey, plainPwd)
	if err != nil {
		writeErrJSON(w, http.StatusInternalServerError, s.localizePublicError(locale, err))
		return
	}
	visibility.ApplyNoIndexPrivateHeaders(w)
	writeJSON(w, http.StatusOK, map[string]any{
		"unlocked":       true,
		"passport_token": gp.SignedToken,
		"unlock_state":   unlockState,
	})
}

func (s *AppServer) handleCommentsRootAPI(w http.ResponseWriter, r *http.Request) {
	viewer, _ := s.resolveViewer(r)
	switch r.Method {
	case http.MethodGet:
		q := r.URL.Query()
		addVaryHeader(w.Header(), "Cookie", guest.PassportHeaderName, "X-Volo-Unlock-State", "X-Volo-Share-Token")
		ckHdr := r.Header.Get("Cookie")
		hasAuthOrGuestCreds := viewer != nil ||
			q.Get("st") != "" ||
			q.Get("preview") != "" ||
			strings.TrimSpace(r.Header.Get(guest.PassportHeaderName)) != "" ||
			strings.TrimSpace(r.Header.Get("X-Volo-Unlock-State")) != "" ||
			strings.TrimSpace(r.Header.Get("X-Volo-Share-Token")) != "" ||
			strings.Contains(ckHdr, auth.SessionCookieName) ||
			strings.Contains(ckHdr, guest.PassportCookieName) ||
			strings.Contains(ckHdr, guest.UnlockCookieName)
		if hasAuthOrGuestCreds {
			w.Header().Set("Cache-Control", "private, no-store")
		}
		tType := q.Get("target_type")
		tID := q.Get("target_id")
		page := parsePageParam(r)
		perPage, _ := strconv.Atoi(q.Get("per_page"))
		focusComment := q.Get("focus_comment")

		locale := s.resolveLocale(r)
		res, err := s.deps.Content.ListTargetCommentsPage(r.Context(), r, viewer, tType, tID, page, perPage, focusComment)
		if errors.Is(err, visibility.ErrProtectedForbidden) {
			writeErrJSON(w, http.StatusForbidden, s.deps.Catalog.T(locale, "error.forbidden"))
			return
		}
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			writeErrJSON(w, http.StatusBadRequest, s.localizePublicError(locale, err))
			return
		}

		claimableGuestCount := 0
		clearGuestPassport := false
		if viewer != nil {
			if viewer.Role != domain.RoleReader {
				clearGuestPassport = true
				if strings.Contains(ckHdr, guest.PassportCookieName) {
					s.clearGuestPassportCookie(w, r)
				}
			} else if s.deps.Passports != nil && s.deps.Store != nil {
				if gp, ok := s.deps.Passports.ResolveOrHeal(nil, r, false); ok && gp != nil && strings.TrimSpace(gp.GuestIDHash) != "" {
					_ = s.deps.Store.QueryRowContext(
						r.Context(),
						s.deps.Store.Dialect().Rebind(`SELECT COUNT(*) FROM comments WHERE user_id = '' AND guest_id_hash = ? AND is_deleted = ?`),
						gp.GuestIDHash, false,
					).Scan(&claimableGuestCount)
					if claimableGuestCount == 0 {
						clearGuestPassport = true
						if strings.Contains(ckHdr, guest.PassportCookieName) {
							s.clearGuestPassportCookie(w, r)
						}
					}
				}
			}
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"comments":              res.Comments,
			"page":                  res.Page,
			"per_page":              res.PerPage,
			"total_roots":           res.TotalRoots,
			"total_comments":        res.TotalComments,
			"total_pages":           res.TotalPages,
			"has_more":              res.HasMore,
			"claimable_guest_count": claimableGuestCount,
			"clear_guest_passport":  clearGuestPassport,
		})

	case http.MethodPost:
		if !s.requireSameOrigin(w, r) {
			return
		}
		locale := s.resolveLocale(r)
		clientIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
		if !s.deps.WAF.AllowRate("comment_post", clientIP, 12, time.Minute) {
			writeErrJSON(w, http.StatusTooManyRequests, s.deps.Catalog.T(locale, "error.rate_limited"))
			return
		}
		var in content.CreateCommentInput
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32768)).Decode(&in); err != nil {
			writeErrJSON(w, http.StatusBadRequest, s.deps.Catalog.T(locale, "error.bad_request"))
			return
		}
		if strings.TrimSpace(in.EnvFPSignal) == "" {
			in.EnvFPSignal = r.Header.Get(guest.EnvFPHeaderName)
		}
		in.ClientIP = clientIP.String()
		if loc, ok := s.deps.Geo.Lookup(clientIP); ok {
			in.GeoRegion = loc.Region
		}
		in.Locale = locale

		created, passportTok, err := s.deps.Content.CreateComment(r.Context(), w, r, viewer, in)
		if errors.Is(err, visibility.ErrProtectedForbidden) || errors.Is(err, content.ErrCommentsClosed) || errors.Is(err, content.ErrRegisteredOnly) || errors.Is(err, content.ErrCommentRejected) {
			writeErrJSON(w, http.StatusForbidden, s.localizePublicError(locale, err))
			return
		}
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			writeErrJSON(w, http.StatusBadRequest, s.localizePublicError(locale, err))
			return
		}
		if s.deps.NotifyCommentCreated != nil {
			go s.deps.NotifyCommentCreated(context.Background(), created, in)
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"comment":        created,
			"passport_token": passportTok,
		})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *AppServer) handleCommentItemAPI(w http.ResponseWriter, r *http.Request) {
	commentID := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/api/v1/comments/"))
	if commentID == "" || strings.Contains(commentID, "/") {
		http.NotFound(w, r)
		return
	}
	viewer, _ := s.resolveViewer(r)
	locale := s.resolveLocale(r)

	switch r.Method {
	case http.MethodPatch:
		if !s.requireSameOrigin(w, r) {
			return
		}
		var body struct {
			ContentMD string `json:"content_md"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32768)).Decode(&body); err != nil {
			writeErrJSON(w, http.StatusBadRequest, s.deps.Catalog.T(locale, "error.bad_request"))
			return
		}
		updated, err := s.deps.Content.UpdateOwnComment(r.Context(), r, viewer, commentID, body.ContentMD)
		if errors.Is(err, content.ErrCommentEditForbidden) || errors.Is(err, content.ErrCommentRejected) || errors.Is(err, visibility.ErrProtectedForbidden) {
			writeErrJSON(w, http.StatusForbidden, s.localizePublicError(locale, err))
			return
		}
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			writeErrJSON(w, http.StatusBadRequest, s.localizePublicError(locale, err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"comment": updated})

	case http.MethodDelete:
		if !s.requireSameOrigin(w, r) {
			return
		}
		err := s.deps.Content.DeleteOwnComment(r.Context(), r, viewer, commentID)
		if errors.Is(err, content.ErrCommentEditForbidden) || errors.Is(err, visibility.ErrProtectedForbidden) {
			writeErrJSON(w, http.StatusForbidden, s.localizePublicError(locale, err))
			return
		}
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			writeErrJSON(w, http.StatusBadRequest, s.localizePublicError(locale, err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *AppServer) isPubliclyReactableTarget(ctx context.Context, targetType string, targetID string) (bool, bool) {
	tType := strings.TrimSpace(targetType)
	tID := strings.TrimSpace(targetID)
	if tID == "" {
		return false, false
	}
	switch tType {
	case domain.TargetTypeArticle:
		var status string
		err := s.deps.Store.QueryRowContext(ctx, `SELECT status FROM articles WHERE id = ?`, tID).Scan(&status)
		return true, err == nil && status == domain.ArticleStatusPublished
	case domain.TargetTypePage:
		var isPub bool
		err := s.deps.Store.QueryRowContext(ctx, `SELECT is_published FROM pages WHERE id = ?`, tID).Scan(&isPub)
		return true, err == nil && isPub
	case "comment":
		var status string
		var isDel bool
		err := s.deps.Store.QueryRowContext(ctx, `SELECT status, is_deleted FROM comments WHERE id = ?`, tID).Scan(&status, &isDel)
		return true, err == nil && status == domain.CommentStatusApproved && !isDel
	default:
		return false, false
	}
}

func (s *AppServer) handleReactionsAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.requireSameOrigin(w, r) {
		return
	}
	locale := s.resolveLocale(r)
	clientIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
	if !s.deps.WAF.AllowRate("public_reactions", clientIP, 40, time.Minute) {
		writeErrJSON(w, http.StatusTooManyRequests, s.deps.Catalog.T(locale, "error.rate_limited"))
		return
	}
	var req struct {
		TargetType string `json:"target_type"`
		TargetID   string `json:"target_id"`
		Kind       string `json:"kind"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeErrJSON(w, http.StatusBadRequest, s.deps.Catalog.T(locale, "error.bad_request"))
		return
	}
	validType, visible := s.isPubliclyReactableTarget(r.Context(), req.TargetType, req.TargetID)
	if !validType {
		writeErrJSON(w, http.StatusBadRequest, s.deps.Catalog.T(locale, "error.bad_request"))
		return
	}
	if !visible {
		http.NotFound(w, r)
		return
	}
	viewer, _ := s.resolveViewer(r)
	actorHash := ""
	if viewer != nil {
		actorHash = "user:" + viewer.ID
	} else if gp, ok := s.deps.Passports.ResolveOrHeal(w, r, true); ok && gp != nil {
		actorHash = "gp:" + gp.GuestIDHash
	} else if fp := guest.ComputeEnvFPHash(s.deps.Config.KeyRing, r.Header.Get(guest.EnvFPHeaderName)); fp != "" {
		actorHash = "fp:" + fp
	}
	counts, added, err := s.deps.Content.ToggleReaction(r.Context(), strings.TrimSpace(req.TargetType), strings.TrimSpace(req.TargetID), actorHash, req.Kind)
	if err != nil {
		writeErrJSON(w, http.StatusBadRequest, s.localizePublicError(locale, err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"counts": counts, "added": added})
}

// --- Isolated Reader Auth & One-Click Historical Guest Comment Claim ---

func (s *AppServer) handleReaderSendCode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.requireSameOrigin(w, r) {
		return
	}
	locale := s.resolveLocale(r)
	if !s.deps.Settings.Snapshot().Reading.AllowReaderRegister {
		writeErrJSON(w, http.StatusForbidden, s.deps.Catalog.T(locale, "reader.err_reg_disabled"))
		return
	}
	clientIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
	if !s.deps.WAF.AllowRate("reader_code", clientIP, 5, 10*time.Minute) {
		writeErrJSON(w, http.StatusTooManyRequests, s.deps.Catalog.T(locale, "error.rate_limited"))
		return
	}
	var req struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeErrJSON(w, http.StatusBadRequest, s.deps.Catalog.T(locale, "error.bad_request"))
		return
	}
	code, err := s.deps.Auth.IssueReaderVerificationCode(req.Email)
	if err != nil {
		writeErrJSON(w, http.StatusBadRequest, s.localizePublicError(locale, err))
		return
	}
	if s.deps.SendVerificationMail != nil {
		_ = s.deps.SendVerificationMail(r.Context(), req.Email, code, locale)
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": s.deps.Catalog.T(locale, "reader.code_sent")})
}

func sanitizeReaderNextTarget(raw string) string {
	cand := strings.TrimSpace(raw)
	if !isSafeRelativeRedirect(cand) {
		return ""
	}
	if cand == "/reader/login" || strings.HasPrefix(cand, "/reader/login?") ||
		cand == "/reader/register" || strings.HasPrefix(cand, "/reader/register?") ||
		cand == "/logout" || strings.HasPrefix(cand, "/logout?") {
		return ""
	}
	return cand
}

func (s *AppServer) handleReaderRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.requireSameOrigin(w, r) {
		return
	}
	locale := s.resolveLocale(r)
	if !s.deps.Settings.Snapshot().Reading.AllowReaderRegister {
		writeErrJSON(w, http.StatusForbidden, s.deps.Catalog.T(locale, "reader.err_reg_disabled"))
		return
	}
	clientIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
	if !s.deps.WAF.AllowRate("reader_register", clientIP, 8, 10*time.Minute) {
		writeErrJSON(w, http.StatusTooManyRequests, s.deps.Catalog.T(locale, "error.rate_limited"))
		return
	}
	var req struct {
		Username string `json:"username"`
		Email    string `json:"email"`
		Password string `json:"password"`
		Code     string `json:"code"`
		Next     string `json:"next"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		writeErrJSON(w, http.StatusBadRequest, s.deps.Catalog.T(locale, "error.bad_request"))
		return
	}
	u, err := s.deps.Auth.RegisterReader(r.Context(), req.Username, req.Email, req.Password, req.Code, locale)
	if err != nil {
		writeErrJSON(w, http.StatusBadRequest, s.localizePublicError(locale, err))
		return
	}
	if u.Role == domain.RoleReader && s.deps.Passports != nil {
		if gp, ok := s.deps.Passports.ResolveOrHeal(nil, r, false); ok && gp != nil {
			_, _ = s.deps.Auth.ClaimGuestComments(r.Context(), u, gp)
		}
	}
	var geo string
	if loc, ok := s.deps.Geo.Lookup(clientIP); ok {
		geo = loc.Region
	}
	_, rawTok, err := s.deps.Auth.CreateSession(r.Context(), u.ID, clientIP, geo, r.UserAgent(), 0)
	if err == nil {
		s.setSessionCookieForRequest(w, r, rawTok)
	}
	resp := map[string]any{"user": u}
	if safeNext := sanitizeReaderNextTarget(req.Next); safeNext != "" {
		resp["next"] = safeNext
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *AppServer) handleReaderLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.requireSameOrigin(w, r) {
		return
	}
	locale := s.resolveLocale(r)
	clientIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
	if !s.deps.WAF.AllowRate("reader_login", clientIP, 10, 5*time.Minute) {
		writeErrJSON(w, http.StatusTooManyRequests, s.deps.Catalog.T(locale, "error.rate_limited"))
		return
	}
	var req struct {
		Ident    string `json:"ident"`
		Password string `json:"password"`
		Next     string `json:"next"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		writeErrJSON(w, http.StatusBadRequest, s.deps.Catalog.T(locale, "error.bad_request"))
		return
	}
	var geo string
	if loc, ok := s.deps.Geo.Lookup(clientIP); ok {
		geo = loc.Region
	}
	res, err := s.deps.Auth.LoginReader(r.Context(), req.Ident, req.Password, clientIP, geo, r.UserAgent())
	if err != nil {
		s.deps.WAF.RecordStrike(r.Context(), clientIP, security.ReasonLoginFail, 1)
		if errors.Is(err, auth.ErrForbidden) {
			err = auth.ErrInvalidCredentials
		}
		writeErrJSON(w, http.StatusUnauthorized, s.localizePublicError(locale, err))
		return
	}
	s.setSessionCookieForRequest(w, r, res.RawToken)
	resp := map[string]any{"user": res.User, "csrf_token": res.CSRFToken}
	if safeNext := sanitizeReaderNextTarget(req.Next); safeNext != "" {
		resp["next"] = safeNext
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *AppServer) handleReaderLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.requireSameOrigin(w, r) {
		return
	}
	viewer, sess := s.resolveViewer(r)
	if viewer != nil && sess != nil {
		_ = s.deps.Auth.RevokeSession(r.Context(), viewer, sess.ID)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.isCookieSecureForRequest(r),
		SameSite: http.SameSiteLaxMode,
	})
	writeOKJSON(w)
}

func (s *AppServer) handleReaderClaimComments(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.requireSameOrigin(w, r) {
		return
	}
	locale := s.resolveLocale(r)
	viewer, _ := s.resolveViewer(r)
	if viewer == nil {
		writeErrJSON(w, http.StatusUnauthorized, s.deps.Catalog.T(locale, "error.unauthorized"))
		return
	}
	if viewer.Role != domain.RoleReader {
		s.clearGuestPassportCookie(w, r)
		writeErrJSON(w, http.StatusForbidden, s.deps.Catalog.T(locale, "error.forbidden"))
		return
	}
	gp, ok := s.deps.Passports.ResolveOrHeal(nil, r, false)
	if !ok || gp == nil {
		writeErrJSON(w, http.StatusBadRequest, s.deps.Catalog.T(locale, "comment.err_no_guest_passport"))
		return
	}
	claimed, err := s.deps.Auth.ClaimGuestComments(r.Context(), viewer, gp)
	if err != nil {
		writeErrJSON(w, http.StatusBadRequest, s.localizePublicError(locale, err))
		return
	}
	s.clearGuestPassportCookie(w, r)
	msg := s.deps.Catalog.T(locale, "comment.claim_guest_success", map[string]any{"count": claimed})
	writeJSON(w, http.StatusOK, map[string]any{"claimed": claimed, "message": msg})
}

func (s *AppServer) setSessionCookie(w http.ResponseWriter, rawToken string) {
	s.setSessionCookieForRequest(w, nil, rawToken)
}

func (s *AppServer) isCookieSecureForRequest(r *http.Request) bool {
	pubURL := ""
	if s.deps.Config != nil {
		pubURL = s.deps.Config.GetPublicURL()
	}
	return s.effectiveRequestScheme(r) == "https" ||
		strings.HasPrefix(strings.ToLower(strings.TrimSpace(pubURL)), "https://")
}

func (s *AppServer) setSessionCookieForRequest(w http.ResponseWriter, r *http.Request, rawToken string) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    rawToken,
		Path:     "/",
		MaxAge:   int(auth.SessionTTL / time.Second),
		HttpOnly: true,
		Secure:   s.isCookieSecureForRequest(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func hasActiveUnlockCookie(r *http.Request) bool {
	if r == nil {
		return false
	}
	if strings.TrimSpace(r.Header.Get("X-Volo-Unlock-State")) != "" {
		return true
	}
	ck, err := r.Cookie(guest.UnlockCookieName)
	return err == nil && strings.TrimSpace(ck.Value) != ""
}

// --- Public Studio HTML Pages ---

func (s *AppServer) basePageContext(r *http.Request, viewer *domain.User) theme.PageContext {
	return s.basePageContextWithSession(r, viewer, nil)
}

func (s *AppServer) basePageContextWithSession(r *http.Request, viewer *domain.User, sess *domain.UserSession) theme.PageContext {
	snap := s.deps.Settings.Snapshot()
	locale := s.resolveLocale(r)
	navItems, _ := s.deps.Content.ListPublicNavItems(r.Context(), locale)
	cats, _ := s.deps.Content.ListCategoriesWithCounts(r.Context())
	tags, _ := s.deps.Content.ListTagsWithCounts(r.Context(), snap.Reading.PopularTagsCount)
	var recentComments []content.RecentCommentItem
	if snap.Reading.RecentCommentsCount > 0 && r.URL.Path != "/dynamic" {
		recentComments, _ = s.deps.Content.ListRecentPublicComments(r.Context(), snap.Reading.RecentCommentsCount)
	}
	extras := s.deps.Content.GetPublicChromeExtras(r.Context(), locale, 6)

	viewerCSRF := ""
	if viewer != nil && s.deps.Auth != nil {
		if sess != nil {
			viewerCSRF = s.deps.Auth.ComputeCSRFToken(sess.ID)
		} else if ck, err := r.Cookie(auth.SessionCookieName); err == nil && strings.TrimSpace(ck.Value) != "" {
			if _, resolvedSess, aErr := s.deps.Auth.AuthenticateSession(r.Context(), ck.Value); aErr == nil && resolvedSess != nil {
				viewerCSRF = s.deps.Auth.ComputeCSRFToken(resolvedSess.ID)
			}
		}
	}

	var headSlots, headerSlots, sidebarSlots, footerSlots []plugin.RenderedSlot
	if s.deps.Plugins != nil {
		slotCtx := plugin.SlotRenderContext{
			Locale:      locale,
			Path:        r.URL.Path,
			ThemeAccent: snap.Appearance.Accent,
		}
		headSlots = s.deps.Plugins.RenderSlots(r.Context(), plugin.SlotPublicHead, slotCtx)
		headerSlots = s.deps.Plugins.RenderSlots(r.Context(), plugin.SlotPublicHeader, slotCtx)
		sidebarSlots = s.deps.Plugins.RenderSlots(r.Context(), plugin.SlotPublicSidebar, slotCtx)
		footerSlots = s.deps.Plugins.RenderSlots(r.Context(), plugin.SlotPublicFooter, slotCtx)
	}

	adminPath, _ := s.effectiveAdminPrefix()

	desc := snap.SEO.MetaDescription
	if desc == "" {
		desc = snap.Site.Description
	}
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
	if snap.SEO.MetaDescription == "" || snap.SEO.MetaDescription == "Personal independent blog" {
		desc = siteCfg.Description
	}

	return theme.PageContext{
		Locale:             locale,
		TextDir:            s.deps.Catalog.Dir(locale),
		Locales:            s.deps.Catalog.Locales(),
		Year:               time.Now().Year(),
		CurrentPath:        r.URL.Path,
		RawQuery:           r.URL.RawQuery,
		BasePath:           r.URL.Path,
		CanonicalURL:       s.publicBaseURL(r) + r.URL.Path,
		PageTitle:          snap.Site.Title,
		MetaDescription:    desc,
		OGImage:            snap.SEO.DefaultOGImageURL,
		AdminPath:          adminPath,
		ThemeID:            activeTheme,
		ThemeConfig:        resolvedThemeCfg,
		Site:               siteCfg,
		Reading:            snap.Reading,
		Feed:               snap.Feed,
		SEO:                snap.SEO,
		Appearance:         snap.Appearance,
		Viewer:             viewer,
		ViewerCSRFToken:    viewerCSRF,
		Owner:              extras.Owner,
		Authors:            extras.Authors,
		Stats:              extras.Stats,
		NavItems:           navItems,
		SidebarCategories:  cats,
		SidebarTags:        tags,
		SidebarArchives:    extras.ArchiveBuckets,
		RecentPosts:        extras.RecentPosts,
		MostViewedPosts:    extras.MostViewedPosts,
		MostCommentedPosts: extras.MostCommentedPosts,
		RecentComments:     recentComments,
		HeadWidgets:        headSlots,
		HeaderWidgets:      headerSlots,
		SidebarWidgets:     sidebarSlots,
		FooterWidgets:      footerSlots,
	}
}

func (s *AppServer) renderStudio(w http.ResponseWriter, r *http.Request, status int, pageName string, pctx theme.PageContext) {
	s.renderStudioFallback(w, r, status, []string{pageName}, pctx)
}

func (s *AppServer) renderStudioFallback(w http.ResponseWriter, r *http.Request, status int, candidates []string, pctx theme.PageContext) {
	activeTheme := s.deps.Themes.ResolveThemeForRequest(r, pctx.Viewer, pctx.Appearance)
	addVaryHeader(w.Header(), "Accept-Encoding", "Cookie", "Accept-Language")
	if mob := strings.TrimSpace(pctx.Appearance.MobileTheme); mob != "" && mob != "same" {
		addVaryHeader(w.Header(), "User-Agent", "Sec-CH-UA-Mobile")
	}
	hasQueryPrivate := false
	if r != nil && r.URL != nil {
		q := r.URL.Query()
		hasQueryPrivate = q.Get("st") != "" || q.Get("preview") != "" || q.Get("skin") != "" || q.Get("theme_preview") != "" || q.Get("device") != "" || strings.TrimSpace(r.Header.Get("X-Volo-Share-Token")) != ""
	}
	if pctx.Viewer != nil || hasQueryPrivate || hasActiveUnlockCookie(r) || pctx.BookLocked ||
		(pctx.Detail.ID != "" && (pctx.Detail.UnlockedViaShare || pctx.Detail.IsPasswordProtected || pctx.Detail.RequiresPassword)) ||
		(pctx.StandalonePage.ID != "" && pctx.StandalonePage.HasLockedSections) {
		w.Header().Set("Cache-Control", "private, no-store")
	}
	if pctx.ThemeID != activeTheme || pctx.ThemeConfig == nil {
		pctx.ThemeID = activeTheme
		pctx.ThemeConfig = s.deps.Themes.ResolveThemeConfig(activeTheme, pctx.Appearance.ThemeConfigs)
	}
	if err := s.deps.Themes.RenderWithFallback(w, status, activeTheme, candidates, pctx); err != nil {
		http.Error(w, "render error: "+err.Error(), http.StatusInternalServerError)
	}
}

func appendVaryHeader(h http.Header, keys ...string) {
	addVaryHeader(h, keys...)
}

func (s *AppServer) renderErrorPage(w http.ResponseWriter, r *http.Request, status int, msgKey string) {
	viewer, sess := s.resolveViewer(r)
	if status == http.StatusNotFound {
		s.record404ProbeOnce(r, viewer, "", 0)
	}
	pctx := s.basePageContextWithSession(r, viewer, sess)
	pctx.StatusCode = status
	pctx.ErrorMessage = s.deps.Catalog.T(pctx.Locale, msgKey)
	pctx.NoIndex = true
	s.renderStudio(w, r, status, "error.html", pctx)
}

func parsePageParam(r *http.Request) int {
	p, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if p < 1 {
		return 1
	}
	return p
}

func computeTotalPages(total int, pageSize int) int {
	if pageSize <= 0 {
		pageSize = 15
	}
	if total <= 0 {
		return 1
	}
	return int(math.Ceil(float64(total) / float64(pageSize)))
}

func clampPageBounds(page int, total int, perPage int) (clampedPage int, totalPages int, start int, end int) {
	if perPage <= 0 {
		perPage = 15
	}
	totalPages = computeTotalPages(total, perPage)
	clampedPage = page
	if clampedPage < 1 {
		clampedPage = 1
	}
	if clampedPage > totalPages {
		clampedPage = totalPages
	}
	if total == 0 {
		return clampedPage, totalPages, 0, 0
	}
	start = (clampedPage - 1) * perPage
	end = start + perPage
	if end > total {
		end = total
	}
	return clampedPage, totalPages, start, end
}

func (s *AppServer) siteTimezoneLocation(tz string) *time.Location {
	trimmed := strings.TrimSpace(tz)
	if trimmed == "" {
		trimmed = "Asia/Shanghai"
	}
	return domain.ResolveSiteLocation(trimmed)
}

func (s *AppServer) handleRootOrStandalonePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		if s.isSetupUnlocked(r.Context()) {
			http.Redirect(w, r, "/setup", http.StatusFound)
			return
		}
		viewer, sess := s.resolveViewer(r)
		pctx := s.basePageContextWithSession(r, viewer, sess)
		pctx.IsHome = true
		page := parsePageParam(r)
		cards, total, err := s.deps.Content.ListPublicCards(r.Context(), content.ArticleListFilter{
			Page:     page,
			PageSize: pctx.Reading.PostsPerPage,
			Locale:   pctx.Locale,
		})
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		pctx.HeroTitle = pctx.Site.Title
		pctx.HeroSubtitle = pctx.Site.Subtitle
		pctx.ArticleCards = cards
		pctx.Page = page
		pctx.TotalPages = computeTotalPages(total, pctx.Reading.PostsPerPage)
		pctx.Pagination = theme.BuildPagination(page, pctx.TotalPages, pctx.Reading.PostsPerPage, total, 5)
		s.renderStudio(w, r, http.StatusOK, "index.html", pctx)
		return
	}

	// Check IndexNow `{key}.txt` if enabled
	seo := s.deps.Settings.Snapshot().SEO
	if seo.IndexNowEnabled && seo.IndexNowKey != "" && r.URL.Path == "/"+seo.IndexNowKey+".txt" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(seo.IndexNowKey))
		return
	}

	// Lookup Standalone Markdown Page (`/{slug}`)
	slug := strings.TrimPrefix(r.URL.Path, "/")
	if slug == "" || strings.Contains(slug, "/") {
		s.renderErrorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	viewer, sess := s.resolveViewer(r)
	locale := s.resolveLocale(r)
	pgView, err := s.deps.Content.GetPublicPageBySlugForViewer(r.Context(), r, slug, locale, viewer)
	if err != nil {
		s.renderErrorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	if pgView.HasLockedSections {
		w.Header().Set("Cache-Control", "private, no-store")
	}
	pctx := s.basePageContextWithSession(r, viewer, sess)
	pctx.IsStandalonePage = true
	pctx.PageTitle = pgView.Title + " · " + pctx.Site.Title
	if pgView.PlainSummary != "" {
		pctx.MetaDescription = pgView.PlainSummary
	}
	pctx.StandalonePage = pgView
	pctx.Alternates = pgView.Translations
	pctx.Reactions, _ = s.deps.Content.GetReactionCounts(r.Context(), domain.TargetTypePage, pgView.ID)
	if s.deps.Plugins != nil {
		slotCtx := plugin.SlotRenderContext{
			Locale:      pctx.Locale,
			Path:        r.URL.Path,
			ArticleID:   pgView.ID,
			ArticleSlug: pgView.Slug,
			ThemeAccent: pctx.Appearance.Accent,
		}
		pctx.ArticleBeforeWidgets = s.deps.Plugins.RenderSlots(r.Context(), plugin.SlotArticleBeforeContent, slotCtx)
		pctx.ArticleAfterWidgets = s.deps.Plugins.RenderSlots(r.Context(), plugin.SlotArticleAfterContent, slotCtx)
		pctx.CommentBeforeWidgets = s.deps.Plugins.RenderSlots(r.Context(), plugin.SlotCommentBefore, slotCtx)
		pctx.CommentAfterWidgets = s.deps.Plugins.RenderSlots(r.Context(), plugin.SlotCommentAfter, slotCtx)
	}
	s.recordPageView(r, domain.TargetTypePage, pgView.ID)

	tmplName := strings.TrimSpace(pgView.Template)
	if tmplName == "" {
		tmplName = "default"
	}
	candidates := []string{
		"page-" + pgView.Slug + ".html",
		"page-" + tmplName + ".html",
		"page.html",
	}
	s.renderStudioFallback(w, r, http.StatusOK, candidates, pctx)
}

func (s *AppServer) handlePostRoute(w http.ResponseWriter, r *http.Request) {
	rawSeg := strings.TrimPrefix(r.URL.Path, "/posts/")
	if rawSeg == "" || strings.Contains(rawSeg, "/") {
		s.renderErrorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	slugDecoded, _ := url.PathUnescape(rawSeg)

	// Surface 9: Raw Markdown endpoint (`GET /posts/{slug}.md`)
	if strings.HasSuffix(slugDecoded, ".md") {
		if !s.deps.Settings.Snapshot().Feed.EnableRawMarkdown {
			http.NotFound(w, r)
			return
		}
		baseSlug := strings.TrimSuffix(slugDecoded, ".md")
		raw, err := s.deps.Content.GetPublishedRawArticleBySlug(r.Context(), baseSlug)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		safeMD, ok := s.deps.Content.Guard().ToSafeRawMarkdown(raw)
		if !ok {
			http.Error(w, "403 Forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(safeMD))
		return
	}

	viewer, sess := s.resolveViewer(r)
	raw, err := s.deps.Content.GetPublishedRawArticleBySlug(r.Context(), slugDecoded)
	if err != nil {
		// Allow draft/scheduled preview ONLY when caller presents a valid timed ShareToken or is the Staff owner;
		// all other callers receive an indistinguishable 404 Not Found.
		cand, cErr := s.deps.Content.GetRawArticleBySlugAnyStatus(r.Context(), slugDecoded)
		if cErr != nil {
			s.renderErrorPage(w, r, http.StatusNotFound, "error.not_found")
			return
		}
		shareTok := strings.TrimSpace(r.URL.Query().Get("preview"))
		if shareTok == "" {
			shareTok = strings.TrimSpace(r.URL.Query().Get("st"))
		}
		if shareTok == "" {
			shareTok = strings.TrimSpace(r.Header.Get("X-Volo-Share-Token"))
		}
		staffOwner := viewer != nil && viewer.Status == domain.UserStatusActive &&
			(viewer.Role == domain.RoleAdmin || (viewer.Role == domain.RoleEditor && viewer.ID == cand.AuthorID))
		if !staffOwner && (shareTok == "" || !s.deps.Content.Guard().VerifyShareToken(r.Context(), cand, shareTok)) {
			s.renderErrorPage(w, r, http.StatusNotFound, "error.not_found")
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		raw = cand
	}
	if raw.HasContentLock || strings.TrimSpace(raw.PasswordCiphertext) != "" || render.ContainsLockDirective(raw.ContentMD) || render.ContainsLockDirective(raw.SummaryMD) {
		w.Header().Set("Cache-Control", "private, no-store")
	}
	pctx := s.basePageContextWithSession(r, viewer, sess)
	detail, prev, next, backlinks, err := s.deps.Content.RenderArticleDetail(
		r.Context(), w, r, raw, viewer, pctx.Locale, s.publicBaseURL(r),
	)
	if err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}

	pctx.IsArticle = true
	pctx.PageTitle = detail.Title + " · " + pctx.Site.Title
	pctx.MetaDescription = detail.Summary
	pctx.OGImage = detail.OGImageURL
	pctx.NoIndex = raw.Status != domain.ArticleStatusPublished || detail.RequiresPassword || detail.UnlockedViaShare || detail.IsPasswordProtected
	pctx.Detail = detail
	pctx.PrevPost = prev
	pctx.NextPost = next
	pctx.Backlinks = backlinks
	pctx.RelatedPosts = s.deps.Content.ListRelatedArticles(r.Context(), raw, pctx.Locale, 4)
	pctx.RandomPosts = s.deps.Content.ListRandomArticles(r.Context(), raw.ID, pctx.Locale, 4)
	pctx.Alternates = detail.Translations
	pctx.Reactions, _ = s.deps.Content.GetReactionCounts(r.Context(), domain.TargetTypeArticle, raw.ID)

	if s.deps.Plugins != nil {
		slotCtx := plugin.SlotRenderContext{
			Locale:      pctx.Locale,
			Path:        r.URL.Path,
			ArticleID:   raw.ID,
			ArticleSlug: raw.Slug,
			ThemeAccent: pctx.Appearance.Accent,
		}
		pctx.ArticleBeforeWidgets = s.deps.Plugins.RenderSlots(r.Context(), plugin.SlotArticleBeforeContent, slotCtx)
		pctx.ArticleAfterWidgets = s.deps.Plugins.RenderSlots(r.Context(), plugin.SlotArticleAfterContent, slotCtx)
		pctx.CommentBeforeWidgets = s.deps.Plugins.RenderSlots(r.Context(), plugin.SlotCommentBefore, slotCtx)
		pctx.CommentAfterWidgets = s.deps.Plugins.RenderSlots(r.Context(), plugin.SlotCommentAfter, slotCtx)
	}

	if pctx.Reading.EnableOutdatedNotice && pctx.Reading.OutdatedNoticeDays > 0 && raw.UpdatedAt > 0 {
		days := int(time.Since(time.UnixMilli(raw.UpdatedAt)).Hours() / 24)
		if days >= pctx.Reading.OutdatedNoticeDays {
			pctx.OutdatedDays = days
		}
	}
	if sc := s.lookupArticleSeriesContext(r.Context(), raw.ID); sc != nil {
		pctx.SeriesContext = sc
	}
	if !pctx.NoIndex {
		ld, _ := json.Marshal(map[string]any{
			"@context":      "https://schema.org",
			"@type":         "BlogPosting",
			"headline":      detail.Title,
			"description":   detail.Summary,
			"datePublished": time.UnixMilli(detail.PublishedAt).UTC().Format(time.RFC3339),
			"dateModified":  time.UnixMilli(detail.UpdatedAt).UTC().Format(time.RFC3339),
			"url":           pctx.CanonicalURL,
		})
		pctx.JSONLD = string(ld)
	}

	s.recordPageView(r, domain.TargetTypeArticle, raw.ID)
	s.renderStudio(w, r, http.StatusOK, "article.html", pctx)
}

func (s *AppServer) lookupArticleSeriesContext(ctx context.Context, articleID string) *theme.ArticleSeriesContext {
	sa, err := sqlx.Get[domain.SeriesArticle](ctx, s.deps.Store, `SELECT * FROM series_articles WHERE article_id = ?`, articleID)
	if err != nil {
		return nil
	}
	ser, err := sqlx.Get[domain.Series](ctx, s.deps.Store, `SELECT * FROM series WHERE id = ?`, sa.SeriesID)
	if err != nil {
		return nil
	}
	rows, err := sqlx.Select[domain.RawArticle](
		ctx, s.deps.Store,
		`SELECT a.id, a.title, a.slug, a.published_at, a.password_ciphertext
		 FROM series_articles sa
		 INNER JOIN articles a ON a.id = sa.article_id
		 WHERE sa.series_id = ? AND a.status = ?
		 ORDER BY sa.sort_order ASC, a.published_at ASC`,
		ser.ID, domain.ArticleStatusPublished,
	)
	if err != nil || len(rows) == 0 {
		return nil
	}
	curIdx := -1
	episodes := make([]theme.SeriesEpisodeItem, 0, len(rows))
	for i, item := range rows {
		isCur := item.ID == articleID
		if isCur {
			curIdx = i
		}
		episodes = append(episodes, theme.SeriesEpisodeItem{
			ID:        item.ID,
			Slug:      item.Slug,
			Title:     item.Title,
			Order:     i + 1,
			IsCurrent: isCur,
		})
	}
	if curIdx < 0 {
		return nil
	}
	var prevInSeries, nextInSeries *visibility.PublicNeighbor
	if curIdx > 0 {
		n := s.deps.Content.Guard().ToNeighbor(rows[curIdx-1])
		prevInSeries = &n
	}
	if curIdx+1 < len(rows) {
		n := s.deps.Content.Guard().ToNeighbor(rows[curIdx+1])
		nextInSeries = &n
	}
	return &theme.ArticleSeriesContext{
		Series:       ser,
		Index:        curIdx + 1,
		Total:        len(rows),
		Episodes:     episodes,
		PrevInSeries: prevInSeries,
		NextInSeries: nextInSeries,
	}
}

func (s *AppServer) recordPageView(r *http.Request, targetType string, targetID string) {
	if r.Method != http.MethodGet {
		return
	}
	actorFP := ""
	if gp, ok := s.deps.Passports.ResolveOrHeal(nil, r, false); ok && gp != nil {
		actorFP = gp.GuestIDHash
	} else {
		clientIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
		actorFP = guest.ComputeEnvFPHash(s.deps.Config.KeyRing, clientIP.String()+"|"+r.UserAgent())
	}
	refHost := ""
	if ref := strings.TrimSpace(r.Referer()); ref != "" {
		if u, err := url.Parse(ref); err == nil {
			refHost = u.Hostname()
		}
	}
	s.deps.Content.RecordView(r.Context(), targetType, targetID, actorFP, refHost)
}

func (s *AppServer) handleCategoriesOverview(w http.ResponseWriter, r *http.Request) {
	viewer, sess := s.resolveViewer(r)
	pctx := s.basePageContextWithSession(r, viewer, sess)
	pctx.TaxonomyKind = "categories"
	pctx.HeroTitle = s.deps.Catalog.T(pctx.Locale, "nav.categories")
	pctx.PageTitle = pctx.HeroTitle + " · " + pctx.Site.Title
	pctx.Categories = pctx.SidebarCategories
	s.renderStudioFallback(w, r, http.StatusOK, []string{"categories.html", "taxonomies.html"}, pctx)
}

func (s *AppServer) handleCategoryPosts(w http.ResponseWriter, r *http.Request) {
	rawSlug := strings.TrimPrefix(r.URL.Path, "/categories/")
	if rawSlug == "" || strings.Contains(rawSlug, "/") {
		s.renderErrorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	slug, _ := url.PathUnescape(rawSlug)
	catRow, err := sqlx.Get[domain.Category](r.Context(), s.deps.Store, `SELECT * FROM categories WHERE slug = ?`, slug)
	if err != nil {
		s.renderErrorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	viewer, sess := s.resolveViewer(r)
	pctx := s.basePageContextWithSession(r, viewer, sess)
	page := parsePageParam(r)
	cards, total, _ := s.deps.Content.ListPublicCards(r.Context(), content.ArticleListFilter{
		Page:         page,
		PageSize:     pctx.Reading.PostsPerPage,
		CategorySlug: slug,
		Locale:       pctx.Locale,
	})
	pctx.HeroTitle = catRow.Name
	pctx.HeroSubtitle = catRow.Description
	pctx.HeroBackURL = "/categories"
	pctx.HeroBackLabel = s.deps.Catalog.T(pctx.Locale, "taxonomy.back_categories")
	pctx.PageTitle = pctx.HeroTitle + " · " + pctx.Site.Title
	pctx.ArticleCards = cards
	pctx.Page = page
	pctx.TotalPages = computeTotalPages(total, pctx.Reading.PostsPerPage)
	pctx.Pagination = theme.BuildPagination(page, pctx.TotalPages, pctx.Reading.PostsPerPage, total, 5)
	s.renderStudioFallback(w, r, http.StatusOK, []string{"category_posts.html", "index.html"}, pctx)
}

func (s *AppServer) handleTagsOverview(w http.ResponseWriter, r *http.Request) {
	viewer, sess := s.resolveViewer(r)
	pctx := s.basePageContextWithSession(r, viewer, sess)
	pctx.TaxonomyKind = "tags"
	pctx.HeroTitle = s.deps.Catalog.T(pctx.Locale, "nav.tags")
	pctx.PageTitle = pctx.HeroTitle + " · " + pctx.Site.Title

	allTags, _ := s.deps.Content.ListTagsWithCounts(r.Context(), 0)
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	sortBy := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sort")))
	if sortBy != "name" {
		sortBy = "popular"
	}

	filtered := allTags
	if q != "" {
		needle := strings.ToLower(q)
		filtered = make([]content.TagWithCount, 0, len(allTags))
		for _, t := range allTags {
			if strings.Contains(strings.ToLower(t.Name), needle) || strings.Contains(strings.ToLower(t.Slug), needle) {
				filtered = append(filtered, t)
			}
		}
	}
	if sortBy == "name" {
		sorted := make([]content.TagWithCount, len(filtered))
		copy(sorted, filtered)
		sort.Slice(sorted, func(i, j int) bool {
			li, lj := strings.ToLower(sorted[i].Name), strings.ToLower(sorted[j].Name)
			if li != lj {
				return li < lj
			}
			return sorted[i].Slug < sorted[j].Slug
		})
		filtered = sorted
	}

	const perPage = 120
	page, totalPages, start, end := clampPageBounds(parsePageParam(r), len(filtered), perPage)
	var paged []content.TagWithCount
	if len(filtered) > 0 {
		paged = filtered[start:end]
	}

	pctx.TagQuery = q
	pctx.TagSort = sortBy
	pctx.Tags = paged
	pctx.Page = page
	pctx.TotalPages = totalPages
	pctx.Pagination = theme.BuildPagination(page, totalPages, perPage, len(filtered), 5)
	s.renderStudioFallback(w, r, http.StatusOK, []string{"tags.html", "taxonomies.html"}, pctx)
}

func (s *AppServer) handleTagPosts(w http.ResponseWriter, r *http.Request) {
	rawSlug := strings.TrimPrefix(r.URL.Path, "/tags/")
	if rawSlug == "" || strings.Contains(rawSlug, "/") {
		s.renderErrorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	slug, _ := url.PathUnescape(rawSlug)
	tagRow, err := sqlx.Get[domain.Tag](r.Context(), s.deps.Store, `SELECT * FROM tags WHERE slug = ?`, slug)
	if err != nil {
		s.renderErrorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	viewer, sess := s.resolveViewer(r)
	pctx := s.basePageContextWithSession(r, viewer, sess)
	page := parsePageParam(r)
	cards, total, _ := s.deps.Content.ListPublicCards(r.Context(), content.ArticleListFilter{
		Page:     page,
		PageSize: pctx.Reading.PostsPerPage,
		TagSlug:  slug,
		Locale:   pctx.Locale,
	})
	pctx.HeroTitle = "#" + tagRow.Name
	pctx.HeroBackURL = "/tags"
	pctx.HeroBackLabel = s.deps.Catalog.T(pctx.Locale, "taxonomy.back_tags")
	pctx.PageTitle = pctx.HeroTitle + " · " + pctx.Site.Title
	pctx.ArticleCards = cards
	pctx.Page = page
	pctx.TotalPages = computeTotalPages(total, pctx.Reading.PostsPerPage)
	pctx.Pagination = theme.BuildPagination(page, pctx.TotalPages, pctx.Reading.PostsPerPage, total, 5)
	s.renderStudioFallback(w, r, http.StatusOK, []string{"tag_posts.html", "index.html"}, pctx)
}

func (s *AppServer) handleArchivesOverview(w http.ResponseWriter, r *http.Request) {
	viewer, sess := s.resolveViewer(r)
	pctx := s.basePageContextWithSession(r, viewer, sess)
	pctx.TaxonomyKind = "archives"
	pctx.HeroTitle = s.deps.Catalog.T(pctx.Locale, "nav.archives")
	pctx.PageTitle = pctx.HeroTitle + " · " + pctx.Site.Title
	buckets, _ := s.deps.Content.ListArchiveMonths(r.Context())
	pctx.ArchiveBuckets = buckets

	yearCounts := make(map[string]int)
	var yearOrder []string
	for _, b := range buckets {
		if len(b.YearMonth) >= 4 {
			yr := b.YearMonth[:4]
			if yearCounts[yr] == 0 {
				yearOrder = append(yearOrder, yr)
			}
			yearCounts[yr] += b.Count
		}
	}
	archiveYears := make([]theme.ArchiveYearStat, 0, len(yearOrder))
	for _, yr := range yearOrder {
		archiveYears = append(archiveYears, theme.ArchiveYearStat{Year: yr, Count: yearCounts[yr]})
	}
	pctx.ArchiveYears = archiveYears

	loc := s.siteTimezoneLocation(pctx.Site.TimeZone)
	selectedYear := strings.TrimSpace(r.URL.Query().Get("year"))
	where := []string{"a.status = ?"}
	args := []any{domain.ArticleStatusPublished}
	if selectedYear != "" {
		if yrNum, err := strconv.Atoi(selectedYear); err == nil && len(selectedYear) == 4 && yrNum >= 1900 && yrNum <= 9999 {
			startT := time.Date(yrNum, 1, 1, 0, 0, 0, 0, loc)
			endT := startT.AddDate(1, 0, 0)
			where = append(where, "(CASE WHEN a.published_at > 0 THEN a.published_at ELSE a.created_at END) >= ? AND (CASE WHEN a.published_at > 0 THEN a.published_at ELSE a.created_at END) < ?")
			args = append(args, startT.UnixMilli(), endT.UnixMilli())
			pctx.SelectedArchiveYear = selectedYear
		} else {
			selectedYear = ""
		}
	}

	const perPage = 50
	whereSQL := strings.Join(where, " AND ")
	var total int
	_ = s.deps.Store.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM articles a WHERE "+whereSQL, args...).Scan(&total)

	page := parsePageParam(r)
	totalPages := computeTotalPages(total, perPage)
	if page > totalPages {
		page = totalPages
	}
	limit, offset := dialect.NormalizePage(page, perPage, perPage, 100)

	type archiveRow struct {
		Slug         string `db:"slug"`
		Title        string `db:"title"`
		PublishedAt  int64  `db:"published_at"`
		CategoryName string `db:"category_name"`
		CategorySlug string `db:"category_slug"`
	}
	var groups []theme.ArchiveYearGroup
	if total > 0 {
		baseSQL := `
			SELECT a.slug, a.title,
			       CASE WHEN a.published_at > 0 THEN a.published_at ELSE a.created_at END AS published_at,
			       COALESCE(c.name, '') AS category_name,
			       COALESCE(c.slug, '') AS category_slug
			FROM articles a
			LEFT JOIN categories c ON c.id = a.category_id
			WHERE ` + whereSQL + `
			ORDER BY published_at DESC, a.id DESC`
		qSQL, pageArgs := s.deps.Store.Dialect().PaginateSQL(baseSQL, limit, offset)
		rows, _ := sqlx.Select[archiveRow](r.Context(), s.deps.Store, qSQL, append(append([]any{}, args...), pageArgs...)...)
		yearIdx := make(map[string]int)
		for _, row := range rows {
			tm := time.UnixMilli(row.PublishedAt).In(loc)
			yr := tm.Format("2006")
			ym := tm.Format("2006-01")
			mo := tm.Format("01")
			md := tm.Format("01-02")

			yi, exists := yearIdx[yr]
			if !exists {
				yi = len(groups)
				yearIdx[yr] = yi
				groups = append(groups, theme.ArchiveYearGroup{
					Year:  yr,
					Count: yearCounts[yr],
				})
			}
			mLen := len(groups[yi].Months)
			if mLen == 0 || groups[yi].Months[mLen-1].YearMonth != ym {
				groups[yi].Months = append(groups[yi].Months, theme.ArchiveMonthGroup{
					YearMonth: ym,
					Month:     mo,
				})
				mLen++
			}
			groups[yi].Months[mLen-1].Posts = append(groups[yi].Months[mLen-1].Posts, theme.ArchivePostEntry{
				Slug:         row.Slug,
				Title:        row.Title,
				CategoryName: row.CategoryName,
				CategorySlug: row.CategorySlug,
				PublishedAt:  row.PublishedAt,
				MonthDay:     md,
				YearMonth:    ym,
			})
			groups[yi].Months[mLen-1].Count = len(groups[yi].Months[mLen-1].Posts)
		}
	}

	pctx.ArchiveGroups = groups
	pctx.Page = page
	pctx.TotalPages = totalPages
	pctx.Pagination = theme.BuildPagination(page, totalPages, perPage, total, 5)
	s.renderStudioFallback(w, r, http.StatusOK, []string{"archives.html", "taxonomies.html"}, pctx)
}

func isValidArchiveYearMonth(ym string) bool {
	if len(ym) != 7 || ym[4] != '-' {
		return false
	}
	yr, err1 := strconv.Atoi(ym[:4])
	mo, err2 := strconv.Atoi(ym[5:7])
	return err1 == nil && err2 == nil && yr >= 1900 && yr <= 9999 && mo >= 1 && mo <= 12
}

func canonicalizeArchiveTail(rawTail string) (dest string, isExactYM bool) {
	hadTrailingSlash := strings.HasSuffix(rawTail, "/")
	hadHTMLExt := strings.HasSuffix(strings.TrimRight(rawTail, "/"), ".html")
	clean := strings.Trim(rawTail, "/")
	clean = strings.TrimSuffix(clean, ".html")
	clean = strings.Trim(clean, "/")
	if clean == "" {
		return "/archives", false
	}
	if len(clean) == 4 {
		if yr, err := strconv.Atoi(clean); err == nil && yr >= 1900 && yr <= 9999 {
			return "/archives?year=" + clean, false
		}
	}
	if isValidArchiveYearMonth(clean) {
		if hadTrailingSlash || hadHTMLExt {
			return "/archives/" + clean, false
		}
		return clean, true
	}
	// Support YYYY-M, YYYY/MM, YYYY/M
	var yrPart, moPart string
	if idx := strings.IndexByte(clean, '-'); idx > 0 && !strings.Contains(clean, "/") {
		yrPart, moPart = clean[:idx], clean[idx+1:]
	} else {
		parts := strings.Split(clean, "/")
		if len(parts) == 2 {
			yrPart, moPart = parts[0], parts[1]
		} else if len(parts) >= 3 {
			slug := strings.TrimSuffix(parts[len(parts)-1], ".html")
			if slug != "" && !strings.Contains(slug, "..") {
				return "/posts/" + slug, false
			}
		}
	}
	if len(yrPart) == 4 {
		yr, err1 := strconv.Atoi(yrPart)
		mo, err2 := strconv.Atoi(moPart)
		if err1 == nil && err2 == nil && yr >= 1900 && yr <= 9999 && mo >= 1 && mo <= 12 {
			return fmt.Sprintf("/archives/%04d-%02d", yr, mo), false
		}
	}
	return "", false
}

func (s *AppServer) handleLegacyArchiveRedirect(w http.ResponseWriter, r *http.Request) {
	tail := r.URL.Path
	for _, pfx := range []string{"/archivedate/", "/archive/"} {
		if strings.HasPrefix(tail, pfx) {
			tail = strings.TrimPrefix(tail, pfx)
			break
		}
	}
	dest, isExactYM := canonicalizeArchiveTail(tail)
	if isExactYM {
		dest = "/archives/" + dest
	}
	if dest == "" {
		s.renderErrorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	if r.URL.RawQuery != "" {
		if strings.Contains(dest, "?") {
			dest += "&" + r.URL.RawQuery
		} else {
			dest += "?" + r.URL.RawQuery
		}
	}
	http.Redirect(w, r, dest, http.StatusMovedPermanently)
}

func (s *AppServer) handleArchiveMonthPosts(w http.ResponseWriter, r *http.Request) {
	rawTail := strings.TrimPrefix(r.URL.Path, "/archives/")
	ym, isExactYM := canonicalizeArchiveTail(rawTail)
	if !isExactYM {
		if ym != "" {
			dest := ym
			if r.URL.RawQuery != "" {
				if strings.Contains(dest, "?") {
					dest += "&" + r.URL.RawQuery
				} else {
					dest += "?" + r.URL.RawQuery
				}
			}
			http.Redirect(w, r, dest, http.StatusMovedPermanently)
			return
		}
		s.renderErrorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	viewer, sess := s.resolveViewer(r)
	pctx := s.basePageContextWithSession(r, viewer, sess)
	perPage := pctx.Reading.PostsPerPage
	if perPage <= 0 {
		perPage = 15
	}
	page := parsePageParam(r)
	cards, total, _ := s.deps.Content.ListPublicCards(r.Context(), content.ArticleListFilter{
		Page:      page,
		PageSize:  perPage,
		YearMonth: ym,
		Locale:    pctx.Locale,
	})
	if total == 0 {
		s.renderErrorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	pctx.HeroTitle = ym
	pctx.HeroBackURL = "/archives"
	pctx.HeroBackLabel = s.deps.Catalog.T(pctx.Locale, "taxonomy.back_archives")
	pctx.PageTitle = ym + " · " + pctx.Site.Title
	pctx.ArticleCards = cards
	pctx.Page = page
	pctx.TotalPages = computeTotalPages(total, perPage)
	pctx.Pagination = theme.BuildPagination(page, pctx.TotalPages, perPage, total, 5)
	s.renderStudioFallback(w, r, http.StatusOK, []string{"archive_posts.html", "index.html"}, pctx)
}

func (s *AppServer) handleSeriesOverview(w http.ResponseWriter, r *http.Request) {
	viewer, sess := s.resolveViewer(r)
	pctx := s.basePageContextWithSession(r, viewer, sess)
	pctx.TaxonomyKind = "series"
	pctx.HeroTitle = s.deps.Catalog.T(pctx.Locale, "nav.series")
	pctx.PageTitle = pctx.HeroTitle + " · " + pctx.Site.Title
	pctx.SeriesList, _ = s.deps.Content.ListSeries(r.Context())
	s.renderStudioFallback(w, r, http.StatusOK, []string{"series.html", "taxonomies.html"}, pctx)
}

func (s *AppServer) handleSeriesDetail(w http.ResponseWriter, r *http.Request) {
	slug, _ := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/series/"))
	viewer, sess := s.resolveViewer(r)
	pctx := s.basePageContextWithSession(r, viewer, sess)
	sv, err := s.deps.Content.GetSeriesBySlug(r.Context(), slug, pctx.Locale)
	if err != nil {
		s.renderErrorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	perPage := pctx.Reading.PostsPerPage
	if perPage <= 0 {
		perPage = 15
	}
	page, totalPages, start, end := clampPageBounds(parsePageParam(r), len(sv.Articles), perPage)
	var slice []visibility.PublicArticleCard
	if len(sv.Articles) > 0 {
		slice = sv.Articles[start:end]
	}

	pctx.HeroTitle = sv.Title
	pctx.HeroSubtitle = sv.Description
	pctx.HeroBackURL = "/series"
	pctx.HeroBackLabel = s.deps.Catalog.T(pctx.Locale, "taxonomy.back_series")
	pctx.PageTitle = sv.Title + " · " + pctx.Site.Title
	pctx.ArticleCards = slice
	pctx.Page = page
	pctx.TotalPages = totalPages
	pctx.Pagination = theme.BuildPagination(page, totalPages, perPage, len(sv.Articles), 5)
	s.renderStudioFallback(w, r, http.StatusOK, []string{"series_detail.html", "index.html"}, pctx)
}

func (s *AppServer) handleBookshelf(w http.ResponseWriter, r *http.Request) {
	viewer, sess := s.resolveViewer(r)
	pctx := s.basePageContextWithSession(r, viewer, sess)
	pctx.PageTitle = s.deps.Catalog.T(pctx.Locale, "bookshelf.title") + " · " + pctx.Site.Title
	allBooks, _ := s.deps.Content.ListBooks(r.Context())

	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		needle := strings.ToLower(q)
		filtered := make([]domain.Book, 0, len(allBooks))
		for _, bk := range allBooks {
			if strings.Contains(strings.ToLower(bk.Title), needle) ||
				strings.Contains(strings.ToLower(bk.Slug), needle) ||
				strings.Contains(strings.ToLower(bk.Description), needle) {
				filtered = append(filtered, bk)
			}
		}
		allBooks = filtered
	}

	const perPage = 24
	page, totalPages, start, end := clampPageBounds(parsePageParam(r), len(allBooks), perPage)
	var paged []domain.Book
	if len(allBooks) > 0 {
		paged = allBooks[start:end]
	}

	pctx.Books = paged
	pctx.Page = page
	pctx.TotalPages = totalPages
	pctx.Pagination = theme.BuildPagination(page, totalPages, perPage, len(allBooks), 5)
	s.renderStudio(w, r, http.StatusOK, "bookshelf.html", pctx)
}

func (s *AppServer) extractLocalPostSlugFromChapterURL(r *http.Request, rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return ""
	}
	pathPart := ""
	if strings.HasPrefix(rawURL, "/") && !strings.HasPrefix(rawURL, "//") {
		pathPart = rawURL
	} else if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		pubBase := s.publicBaseURL(r)
		pubParsed, pErr := url.Parse(pubBase)
		sameHost := strings.EqualFold(u.Host, r.Host) || (pErr == nil && strings.EqualFold(u.Host, pubParsed.Host))
		if sameHost {
			pathPart = u.Path
		}
	}
	if idx := strings.IndexAny(pathPart, "?#"); idx >= 0 {
		pathPart = pathPart[:idx]
	}
	if !strings.HasPrefix(pathPart, "/posts/") {
		return ""
	}
	seg := strings.Trim(strings.TrimPrefix(pathPart, "/posts/"), "/")
	if seg == "" || strings.Contains(seg, "/") || strings.HasSuffix(seg, ".md") {
		return ""
	}
	decoded, err := url.PathUnescape(seg)
	if err != nil || decoded == "" {
		return seg
	}
	return decoded
}

func (s *AppServer) handleBookReader(w http.ResponseWriter, r *http.Request) {
	slug, _ := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/books/"))
	viewer, sess := s.resolveViewer(r)
	bk, err := s.deps.Content.GetBookBySlug(r.Context(), slug)
	if err != nil {
		s.renderErrorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	pctx := s.basePageContextWithSession(r, viewer, sess)
	pctx.PageTitle = bk.Title + " · " + pctx.Site.Title
	pctx.BookDetail = bk
	activeIdx := -1
	if chID := strings.TrimSpace(r.URL.Query().Get("ch")); chID != "" {
		for i := range bk.FlatChapters {
			if bk.FlatChapters[i].ID == chID {
				pctx.ActiveChapter = &bk.FlatChapters[i]
				activeIdx = i
				break
			}
		}
		if pctx.ActiveChapter == nil {
			s.renderErrorPage(w, r, http.StatusNotFound, "error.not_found")
			return
		}
	} else if bk.EmbedMode == "iframe" || bk.EmbedMode == "inline" {
		for i := range bk.FlatChapters {
			if strings.TrimSpace(bk.FlatChapters[i].URL) != "" {
				pctx.ActiveChapter = &bk.FlatChapters[i]
				activeIdx = i
				break
			}
		}
	}
	if activeIdx > 0 {
		prevCh := bk.FlatChapters[activeIdx-1]
		pctx.PrevChapter = &prevCh
	}
	if activeIdx >= 0 && activeIdx+1 < len(bk.FlatChapters) {
		nextCh := bk.FlatChapters[activeIdx+1]
		pctx.NextChapter = &nextCh
	}
	if pctx.ActiveChapter != nil {
		if postSlug := s.extractLocalPostSlugFromChapterURL(r, pctx.ActiveChapter.URL); postSlug != "" {
			if raw, gErr := s.deps.Content.GetPublishedRawArticleBySlug(r.Context(), postSlug); gErr == nil {
				if r.Method == http.MethodPost {
					if !s.isSameOriginRequest(r) {
						http.Error(w, "403 Forbidden", http.StatusForbidden)
						return
					}
					clientIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
					if !s.deps.WAF.AllowRate("article_unlock", clientIP, 12, 5*time.Minute) {
						pctx.BookUnlockError = s.deps.Catalog.T(pctx.Locale, "error.rate_limited")
					} else {
						r.Body = http.MaxBytesReader(w, r.Body, 8192)
						_ = r.ParseForm()
						pwd := r.FormValue("password")
						if matched, ok := s.deps.Content.Guard().VerifyArticlePassword(raw, pwd); ok {
							gp, _ := s.deps.Passports.ResolveOrHeal(w, r, true)
							entryKey := s.deps.Content.Guard().UnlockEntryKey(raw, matched)
							_, _ = s.deps.Passports.GrantArticleUnlock(w, r, gp.GuestID, entryKey, matched)
							visibility.ApplyNoIndexPrivateHeaders(w)
							selfTarget := "/books"
							if reqURI := r.URL.RequestURI(); isLocalUrl(reqURI) {
								selfTarget = reqURI
							}
							http.Redirect(w, r, selfTarget, http.StatusSeeOther)
							return
						}
						s.deps.WAF.RecordStrike(r.Context(), clientIP, security.ReasonUnlockFail, 1)
						pctx.BookUnlockError = s.deps.Catalog.T(pctx.Locale, "article.unlock_wrong_pwd")
					}
					w.Header().Set("Cache-Control", "private, no-store")
				}
				if raw.HasContentLock || strings.TrimSpace(raw.PasswordCiphertext) != "" || render.ContainsLockDirective(raw.ContentMD) || render.ContainsLockDirective(raw.SummaryMD) {
					w.Header().Set("Cache-Control", "private, no-store")
				}
				if detail, _, _, _, rErr := s.deps.Content.RenderArticleDetail(
					r.Context(), w, r, raw, viewer, pctx.Locale, s.publicBaseURL(r),
				); rErr == nil {
					if detail.IsPasswordProtected || detail.RequiresPassword || detail.UnlockedViaShare {
						pctx.NoIndex = true
						w.Header().Set("Cache-Control", "private, no-store")
					}
					if detail.RequiresPassword {
						pctx.BookLocked = true
						pctx.BookArticleID = raw.ID
						pctx.BookArticleSlug = raw.Slug
					} else {
						pctx.ActiveChapterHTML = detail.HTML
						pctx.ActiveChapterNeedsMath = detail.NeedsMath
						pctx.ActiveChapterNeedsMermaid = detail.NeedsMermaid
					}
				}
			}
		} else if (bk.EmbedMode == "iframe" || bk.EmbedMode == "inline") &&
			(strings.HasPrefix(pctx.ActiveChapter.URL, "https://") || strings.HasPrefix(pctx.ActiveChapter.URL, "http://")) {
			if htmlBody, needsMath, needsMermaid, ok := s.deps.Content.ExtractRemoteBookChapter(r.Context(), bk, *pctx.ActiveChapter); ok {
				pctx.ActiveChapterHTML = htmlBody
				pctx.ActiveChapterNeedsMath = needsMath
				pctx.ActiveChapterNeedsMermaid = needsMermaid
			}
		}
	} else if (bk.EmbedMode == "iframe" || bk.EmbedMode == "inline") &&
		(strings.HasPrefix(strings.TrimSpace(bk.SourceURL), "https://") || strings.HasPrefix(strings.TrimSpace(bk.SourceURL), "http://")) {
		syntheticRoot := domain.BookChapter{
			ID:     "root",
			BookID: bk.ID,
			Title:  bk.Title,
			URL:    strings.TrimSpace(bk.SourceURL),
		}
		if htmlBody, needsMath, needsMermaid, ok := s.deps.Content.ExtractRemoteBookChapter(r.Context(), bk, syntheticRoot); ok {
			pctx.ActiveChapterHTML = htmlBody
			pctx.ActiveChapterNeedsMath = needsMath
			pctx.ActiveChapterNeedsMermaid = needsMermaid
		}
	}
	if pctx.ActiveChapterHTML == "" && bk.EmbedMode == "iframe" {
		frameTarget := bk.SourceURL
		if pctx.ActiveChapter != nil && strings.TrimSpace(pctx.ActiveChapter.URL) != "" {
			frameTarget = pctx.ActiveChapter.URL
		}
		pctx.BookIframeAllowSameOrigin = s.isStrictlyCrossOriginURL(r, frameTarget)
	}
	s.renderStudio(w, r, http.StatusOK, "book_reader.html", pctx)
}

func (s *AppServer) isStrictlyCrossOriginURL(r *http.Request, rawURL string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return false
	}
	if pubParsed, pErr := url.Parse(s.publicBaseURL(r)); pErr == nil && pubParsed.Host != "" && strings.EqualFold(u.Host, pubParsed.Host) {
		return false
	}
	return true
}

func (s *AppServer) handleLinksPage(w http.ResponseWriter, r *http.Request) {
	viewer, sess := s.resolveViewer(r)
	pctx := s.basePageContextWithSession(r, viewer, sess)
	pctx.PageTitle = s.deps.Catalog.T(pctx.Locale, "nav.links") + " · " + pctx.Site.Title
	links, _ := s.deps.Content.ListLinks(r.Context())
	grouped := make(map[string][]domain.Link)
	var catOrder []string
	for _, lk := range links {
		cat := strings.TrimSpace(lk.CategoryName)
		if cat == "" {
			cat = s.deps.Catalog.T(pctx.Locale, "nav.links")
		}
		if len(grouped[cat]) == 0 {
			catOrder = append(catOrder, cat)
		}
		grouped[cat] = append(grouped[cat], lk)
	}
	linkGroups := make([]theme.LinkCategoryGroup, 0, len(catOrder))
	for _, cat := range catOrder {
		linkGroups = append(linkGroups, theme.LinkCategoryGroup{
			Category: cat,
			Items:    grouped[cat],
		})
	}
	pctx.LinksByCategory = grouped
	pctx.LinkGroups = linkGroups
	s.renderStudio(w, r, http.StatusOK, "links.html", pctx)
}

func (s *AppServer) handleFollowsPage(w http.ResponseWriter, r *http.Request) {
	viewer, sess := s.resolveViewer(r)
	pctx := s.basePageContextWithSession(r, viewer, sess)
	pctx.PageTitle = s.deps.Catalog.T(pctx.Locale, "nav.follows") + " · " + pctx.Site.Title

	const perPage = 18
	page := parsePageParam(r)
	groups, total, _ := s.deps.Content.ListFollowsWithItemsPage(r.Context(), page, perPage, 6)
	totalPages := computeTotalPages(total, perPage)
	if page > totalPages {
		page = totalPages
		groups, total, _ = s.deps.Content.ListFollowsWithItemsPage(r.Context(), page, perPage, 6)
	}

	pctx.FollowGroups = groups
	pctx.Page = page
	pctx.TotalPages = totalPages
	pctx.Pagination = theme.BuildPagination(page, totalPages, perPage, total, 5)
	s.renderStudio(w, r, http.StatusOK, "follows.html", pctx)
}

func (s *AppServer) handleDynamicPage(w http.ResponseWriter, r *http.Request) {
	viewer, sess := s.resolveViewer(r)
	pctx := s.basePageContextWithSession(r, viewer, sess)
	pctx.TaxonomyKind = "dynamic"
	pctx.HeroTitle = s.deps.Catalog.T(pctx.Locale, "nav.dynamic")
	pctx.PageTitle = pctx.HeroTitle + " · " + pctx.Site.Title

	const perPage = 20
	page := parsePageParam(r)
	recent, total, _ := s.deps.Content.ListRecentPublicCommentsPage(r.Context(), page, perPage)
	totalPages := computeTotalPages(total, perPage)
	if page > totalPages {
		page = totalPages
		recent, total, _ = s.deps.Content.ListRecentPublicCommentsPage(r.Context(), page, perPage)
	}

	pctx.RecentComments = recent
	pctx.Page = page
	pctx.TotalPages = totalPages
	pctx.Pagination = theme.BuildPagination(page, totalPages, perPage, total, 5)
	s.renderStudioFallback(w, r, http.StatusOK, []string{"dynamic.html", "taxonomies.html"}, pctx)
}

func localizeUnmodifiedSiteDefaults(siteCfg *domain.SiteSettings, locale string) {
	if siteCfg == nil {
		return
	}
	loc := strings.ToLower(strings.TrimSpace(locale))
	isTW := strings.Contains(loc, "tw") || strings.Contains(loc, "hk") || strings.Contains(loc, "hant")
	isEN := strings.HasPrefix(loc, "en")

	switch strings.TrimSpace(siteCfg.Subtitle) {
	case "记录 · 思考 · 分享", "Record · Reflect · Share", "Independent thoughts & craft", "記錄 · 思考 · 分享":
		if isEN {
			siteCfg.Subtitle = "Record · Reflect · Share"
		} else if isTW {
			siteCfg.Subtitle = "記錄 · 思考 · 分享"
		} else {
			siteCfg.Subtitle = "记录 · 思考 · 分享"
		}
	}
	switch strings.TrimSpace(siteCfg.Description) {
	case "由 Volo-go 驱动的独立个人博客。", "A fast, sovereign personal blog powered by Volo-go.", "由 Volo-go 驅動的獨立個人部落格。":
		if isEN {
			siteCfg.Description = "A fast, sovereign personal blog powered by Volo-go."
		} else if isTW {
			siteCfg.Description = "由 Volo-go 驅動的獨立個人部落格。"
		} else {
			siteCfg.Description = "由 Volo-go 驱动的独立个人博客。"
		}
	}
	switch strings.TrimSpace(siteCfg.LicenseTemplate) {
	case "本文由 {author} 原创发布于 {site}，转载请注明原文链接：{url}",
		"Originally published by {author} on {site}. Please credit the source: {url}",
		"本文由 {author} 原創發布於 {site}，轉載請註明原文連結：{url}":
		if isEN {
			siteCfg.LicenseTemplate = "Originally published by {author} on {site}. Please credit the source: {url}"
		} else if isTW {
			siteCfg.LicenseTemplate = "本文由 {author} 原創發布於 {site}，轉載請註明原文連結：{url}"
		} else {
			siteCfg.LicenseTemplate = "本文由 {author} 原创发布于 {site}，转载请注明原文链接：{url}"
		}
	}
}

func (s *AppServer) localizeReaderAuthError(locale string, code string) string {
	clean := strings.ToLower(strings.TrimSpace(code))
	if clean == "" {
		return ""
	}
	loc := strings.ToLower(strings.TrimSpace(locale))
	isTW := strings.Contains(loc, "tw") || strings.Contains(loc, "hk") || strings.Contains(loc, "hant")
	isZH := strings.HasPrefix(loc, "zh")
	switch clean {
	case "state":
		if isTW {
			return "登入狀態校驗失敗或已過期，請重試。"
		}
		if isZH {
			return "登录状态校验失败或已过期，请重试。"
		}
		return "Sign-in state expired or invalid. Please try again."
	case "disabled":
		if isTW {
			return "目前帳號已被停用或暫未開放註冊。"
		}
		if isZH {
			return "当前账号已被禁用或暂未开放注册。"
		}
		return "This account is disabled or registration is closed."
	case "unverified_email":
		if isTW {
			return "第三方帳號信箱尚未驗證，無法自動登入。"
		}
		if isZH {
			return "第三方账号邮箱尚未验证，无法自动登录。"
		}
		return "Third-party account email is not verified."
	case "forbidden":
		if isTW {
			return "管理員或編輯帳號請透過管理後台登入，不可使用讀者通道。"
		}
		if isZH {
			return "管理员或编辑账号请通过管理后台登录，不可使用读者通道。"
		}
		return "Staff accounts must sign in via the admin console."
	default:
		if isTW {
			return "登入失敗，請檢查憑據或稍後重試。"
		}
		if isZH {
			return "登录失败，请检查凭据或稍后重试。"
		}
		return "Sign-in failed. Please verify your credentials and try again."
	}
}

func (s *AppServer) handleReaderAuthPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	safeNext := sanitizeReaderNextTarget(r.URL.Query().Get("next"))
	viewer, _ := s.resolveViewer(r)
	if viewer != nil {
		dest := "/"
		if safeNext != "" {
			dest = safeNext
		}
		http.Redirect(w, r, dest, http.StatusFound)
		return
	}

	status := http.StatusOK
	authErrMsg := ""

	if r.Method == http.MethodPost {
		if !s.isSameOriginRequest(r) {
			http.Error(w, "403 Forbidden", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8192)
		_ = r.ParseForm()
		if formNext := sanitizeReaderNextTarget(r.FormValue("next")); formNext != "" {
			safeNext = formNext
		}
		clientIP := security.ResolveClientIP(r, s.deps.Config.TrustedProxies)
		var geo string
		if loc, ok := s.deps.Geo.Lookup(clientIP); ok {
			geo = loc.Region
		}
		locale := s.resolveLocale(r)

		if r.URL.Path == "/reader/register" {
			if !s.deps.Settings.Snapshot().Reading.AllowReaderRegister {
				status = http.StatusForbidden
				authErrMsg = s.localizeReaderAuthError(locale, "disabled")
			} else if !s.deps.WAF.AllowRate("reader_register", clientIP, 8, 10*time.Minute) {
				status = http.StatusTooManyRequests
				authErrMsg = s.deps.Catalog.T(locale, "error.rate_limited")
			} else {
				u, regErr := s.deps.Auth.RegisterReader(r.Context(), r.FormValue("username"), r.FormValue("email"), r.FormValue("password"), r.FormValue("code"), locale)
				if regErr != nil {
					status = http.StatusBadRequest
					authErrMsg = s.localizePublicError(locale, regErr)
				} else {
					if u.Role == domain.RoleReader && s.deps.Passports != nil {
						if gp, ok := s.deps.Passports.ResolveOrHeal(nil, r, false); ok && gp != nil {
							_, _ = s.deps.Auth.ClaimGuestComments(r.Context(), u, gp)
						}
					}
					if _, rawTok, sErr := s.deps.Auth.CreateSession(r.Context(), u.ID, clientIP, geo, r.UserAgent(), 0); sErr == nil {
						s.setSessionCookieForRequest(w, r, rawTok)
					}
					dest := "/"
					if safeNext != "" {
						dest = safeNext
					}
					http.Redirect(w, r, dest, http.StatusSeeOther)
					return
				}
			}
		} else {
			if !s.deps.WAF.AllowRate("reader_login", clientIP, 10, 5*time.Minute) {
				status = http.StatusTooManyRequests
				authErrMsg = s.deps.Catalog.T(locale, "error.rate_limited")
			} else {
				res, lErr := s.deps.Auth.LoginReader(r.Context(), r.FormValue("ident"), r.FormValue("password"), clientIP, geo, r.UserAgent())
				if lErr != nil {
					s.deps.WAF.RecordStrike(r.Context(), clientIP, security.ReasonLoginFail, 1)
					status = http.StatusUnauthorized
					if errors.Is(lErr, auth.ErrForbidden) {
						lErr = auth.ErrInvalidCredentials
					}
					authErrMsg = s.localizePublicError(locale, lErr)
				} else {
					s.setSessionCookieForRequest(w, r, res.RawToken)
					dest := "/"
					if safeNext != "" {
						dest = safeNext
					}
					http.Redirect(w, r, dest, http.StatusSeeOther)
					return
				}
			}
		}
	}

	pctx := s.basePageContext(r, nil)
	pctx.PageTitle = s.deps.Catalog.T(pctx.Locale, "reader.login_title") + " · " + pctx.Site.Title
	pctx.NoIndex = true
	pctx.ReaderNextURL = safeNext
	if authErrMsg != "" {
		pctx.ReaderAuthError = authErrMsg
	} else if qErr := r.URL.Query().Get("error"); qErr != "" {
		pctx.ReaderAuthError = s.localizeReaderAuthError(pctx.Locale, qErr)
	}
	pctx.OIDCProviders = s.listOIDCProviderViews(pctx.Locale, "reader", safeNext)
	w.Header().Set("Cache-Control", "private, no-store")
	s.renderStudio(w, r, status, "reader_auth.html", pctx)
}

func (s *AppServer) localizePublicError(locale string, err error) string {
	if err == nil {
		return ""
	}
	cat := s.deps.Catalog
	switch {
	case errors.Is(err, auth.ErrUnauthorized):
		return cat.T(locale, "error.unauthorized")
	case errors.Is(err, auth.ErrForbidden), errors.Is(err, visibility.ErrProtectedForbidden):
		return cat.T(locale, "error.forbidden")
	case errors.Is(err, auth.ErrInvalidCredentials):
		return cat.T(locale, "reader.err_invalid_credentials")
	case errors.Is(err, auth.ErrVerificationCode):
		return cat.T(locale, "reader.err_verify_code")
	case errors.Is(err, auth.ErrCodeCooldown):
		return cat.T(locale, "reader.err_code_cooldown")
	case errors.Is(err, content.ErrCommentsClosed):
		return cat.T(locale, "comment.closed_notice")
	case errors.Is(err, content.ErrRegisteredOnly):
		return cat.T(locale, "comment.registered_only")
	case errors.Is(err, content.ErrCommentEditForbidden):
		return cat.T(locale, "comment.err_edit_forbidden")
	case errors.Is(err, guest.ErrPoWInvalid), errors.Is(err, guest.ErrPoWReplayed):
		return cat.T(locale, "comment.err_pow")
	case errors.Is(err, content.ErrCommentRejected), errors.Is(err, guest.ErrHoneypotTriggered):
		return cat.T(locale, "comment.err_antispam")
	}

	low := strings.ToLower(strings.TrimSpace(err.Error()))
	switch {
	case strings.Contains(low, "guestpassport") || strings.Contains(low, "guest passport"):
		return cat.T(locale, "comment.err_no_guest_passport")
	case strings.Contains(low, "content length must be between"):
		return cat.T(locale, "comment.err_length")
	case strings.Contains(low, "author_name is required"):
		return cat.T(locale, "comment.err_nickname")
	case strings.Contains(low, "parent comment not found"):
		return cat.T(locale, "comment.err_parent_missing")
	case strings.Contains(low, "invalid email"):
		return cat.T(locale, "reader.err_invalid_email")
	case strings.Contains(low, "username must be"):
		return cat.T(locale, "reader.err_username_len")
	case strings.Contains(low, "password "):
		return cat.T(locale, "reader.err_password_policy")
	case strings.Contains(low, "unique") || strings.Contains(low, "duplicate"):
		return cat.T(locale, "reader.err_account_exists")
	case strings.Contains(low, "target_type") || strings.Contains(low, "reaction kind") || strings.Contains(low, "missing"):
		return cat.T(locale, "error.bad_request")
	default:
		return cat.T(locale, "error.hint_default")
	}
}

func (s *AppServer) handleLegacyAdminRedirect(w http.ResponseWriter, r *http.Request) {
	adminPrefix, isCustom := s.effectiveAdminPrefix()
	if isCustom {
		s.renderErrorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	http.Redirect(w, r, adminPrefix, http.StatusFound)
}

func (s *AppServer) handlePublicLogoutRedirect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.isSameOriginRequest(r) {
		http.Error(w, "403 Forbidden", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodGet {
		// Reject subresource GET CSRF (e.g. <img src="/logout">, <script src="/logout">, cross-site iframe).
		sfs := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")))
		sfm := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Mode")))
		sfd := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Dest")))
		if (sfs != "" && sfs != "same-origin" && sfs != "none") ||
			(sfm != "" && sfm != "navigate") ||
			(sfd != "" && sfd != "document") {
			http.Error(w, "403 Forbidden", http.StatusForbidden)
			return
		}
	}

	viewer, sess := s.resolveViewer(r)
	if viewer != nil && sess != nil {
		_ = s.deps.Auth.RevokeSession(r.Context(), viewer, sess.ID)
	}
	secure := s.isCookieSecureForRequest(r)
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     "__volo_2fa_trust",
		Value:    "",
		Path:     "/api",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     "__volo_2fa_trust",
		Value:    "",
		Path:     "/api/admin/auth",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
	adminPrefix, _ := s.effectiveAdminPrefix()
	staticAdmin := s.staticAdminPrefix()
	isSafeTarget := func(cand string) bool {
		if !isSafeRelativeRedirect(cand) {
			return false
		}
		if cand == "/logout" || strings.HasPrefix(cand, "/logout?") ||
			cand == adminPrefix || strings.HasPrefix(cand, adminPrefix+"/") ||
			cand == staticAdmin || strings.HasPrefix(cand, staticAdmin+"/") {
			return false
		}
		return true
	}
	dest := "/"
	if qRedir := strings.TrimSpace(r.URL.Query().Get("redirect")); isSafeTarget(qRedir) {
		dest = qRedir
	} else if ref := strings.TrimSpace(r.Referer()); ref != "" {
		if u, err := url.Parse(ref); err == nil && (u.Host == "" || strings.EqualFold(u.Host, r.Host)) {
			cand := u.RequestURI()
			if isSafeTarget(cand) {
				dest = cand
			}
		}
	}
	if isLocalUrl(dest) {
		http.Redirect(w, r, dest, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *AppServer) handlePublicGitHubShowcaseRoute(w http.ResponseWriter, r *http.Request) {
	var slug string
	err := s.deps.Store.QueryRowContext(r.Context(),
		`SELECT slug FROM articles WHERE status = ? AND (slug IN ('github', 'my-github-repos') OR legacy_permalink IN ('/my-github-repos', '/github')) ORDER BY updated_at DESC LIMIT 1`,
		domain.ArticleStatusPublished).Scan(&slug)
	if err == nil && slug != "" {
		http.Redirect(w, r, visibility.ArticlePath(slug), http.StatusFound)
		return
	}
	s.renderErrorPage(w, r, http.StatusNotFound, "error.not_found")
}

func (s *AppServer) handleLegacyImageShim(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	rel := strings.TrimPrefix(r.URL.Path, "/images/")
	clean := strings.TrimPrefix(path.Clean("/"+rel), "/")
	if clean == "" || strings.Contains(clean, "..") {
		http.NotFound(w, r)
		return
	}
	// Serve user-provided static file if placed in uploads/images/ or data/images/ with strict CSP + attachment safety
	for _, baseDir := range []string{s.deps.Config.GetUploadDir(), s.deps.Config.GetDefaultUploadDir(), s.deps.Config.DataDir} {
		if strings.TrimSpace(baseDir) == "" {
			continue
		}
		if serveSafeUserStaticFile(w, r, filepath.Join(baseDir, "images"), clean) {
			return
		}
	}

	var svg string
	switch strings.ToLower(clean) {
	case "github-icon-light.svg":
		svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" width="16" height="16"><path fill="#f0f6f2" d="M8 1.2a6.8 6.8 0 0 0-2.15 13.25c.34.06.46-.15.46-.33v-1.15c-1.89.41-2.29-.91-2.29-.91-.31-.78-.75-.99-.75-.99-.62-.42.05-.41.05-.41.68.05 1.04.7 1.04.7.61 1.04 1.59.74 1.98.57.06-.44.24-.74.43-.91-1.51-.17-3.1-.76-3.1-3.36 0-.74.26-1.35.7-1.82-.07-.17-.3-.86.07-1.8 0 0 .57-.18 1.87.7a6.5 6.5 0 0 1 3.4 0c1.3-.88 1.87-.7 1.87-.7.37.94.14 1.63.07 1.8.44.47.7 1.08.7 1.82 0 2.61-1.59 3.19-3.11 3.36.25.21.46.63.46 1.27v1.88c0 .18.12.39.47.33A6.8 6.8 0 0 0 8 1.2Z"/></svg>`
	case "github-icon.png", "github-icon.svg":
		svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" width="16" height="16"><style>path{fill:#24292f}@media (prefers-color-scheme:dark){path{fill:#f0f6f2}}</style><path fill="#24292f" d="M8 1.2a6.8 6.8 0 0 0-2.15 13.25c.34.06.46-.15.46-.33v-1.15c-1.89.41-2.29-.91-2.29-.91-.31-.78-.75-.99-.75-.99-.62-.42.05-.41.05-.41.68.05 1.04.7 1.04.7.61 1.04 1.59.74 1.98.57.06-.44.24-.74.43-.91-1.51-.17-3.1-.76-3.1-3.36 0-.74.26-1.35.7-1.82-.07-.17-.3-.86.07-1.8 0 0 .57-.18 1.87.7a6.5 6.5 0 0 1 3.4 0c1.3-.88 1.87-.7 1.87-.7.37.94.14 1.63.07 1.8.44.47.7 1.08.7 1.82 0 2.61-1.59 3.19-3.11 3.36.25.21.46.63.46 1.27v1.88c0 .18.12.39.47.33A6.8 6.8 0 0 0 8 1.2Z"/></svg>`
	case "admin-panel.svg":
		svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" width="16" height="16" fill="none" stroke="#24292f" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"><style>svg{stroke:#24292f}@media (prefers-color-scheme:dark){svg{stroke:#f0f6f2}}</style><rect x="2.2" y="2.2" width="4.8" height="4.8" rx="1"/><rect x="9" y="2.2" width="4.8" height="4.8" rx="1"/><rect x="2.2" y="9" width="4.8" height="4.8" rx="1"/><rect x="9" y="9" width="4.8" height="4.8" rx="1"/></svg>`
	case "admin-logout.svg":
		svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" width="16" height="16" fill="none" stroke="#24292f" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"><style>svg{stroke:#24292f}@media (prefers-color-scheme:dark){svg{stroke:#f0f6f2}}</style><path d="M6.2 2.5H3.6A1.4 1.4 0 0 0 2.2 3.9v8.2a1.4 1.4 0 0 0 1.4 1.4h2.6"/><path d="M10.5 11.2 13.8 8l-3.3-3.2"/><path d="M13.8 8H6.2"/></svg>`
	case "github_repo.jpg", "github_repo.png", "github_repo.svg":
		svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 800 450" width="800" height="450"><defs><linearGradient id="bg" x1="0" y1="0" x2="1" y2="1"><stop offset="0%" stop-color="#0d1513"/><stop offset="55%" stop-color="#14231e"/><stop offset="100%" stop-color="#1d352d"/></linearGradient><pattern id="grid" width="32" height="32" patternUnits="userSpaceOnUse"><path d="M 32 0 L 0 0 0 32" fill="none" stroke="#5fb399" stroke-opacity="0.08" stroke-width="1"/></pattern></defs><rect width="800" height="450" fill="url(#bg)"/><rect width="800" height="450" fill="url(#grid)"/><g fill="none" stroke="#64c3a6" stroke-opacity="0.22" stroke-width="1.5"><rect x="44" y="38" width="712" height="374" rx="18"/><circle cx="78" cy="68" r="5" fill="#e06c75" stroke="none"/><circle cx="96" cy="68" r="5" fill="#e5c07b" stroke="none"/><circle cx="114" cy="68" r="5" fill="#98c379" stroke="none"/><line x1="44" y1="94" x2="756" y2="94"/></g><g stroke-linecap="round"><line x1="84" y1="142" x2="220" y2="142" stroke="#64c3a6" stroke-opacity="0.65" stroke-width="6"/><line x1="238" y1="142" x2="394" y2="142" stroke="#8ab4f8" stroke-opacity="0.35" stroke-width="6"/><line x1="84" y1="176" x2="462" y2="176" stroke="#d8e6e0" stroke-opacity="0.22" stroke-width="5"/><line x1="84" y1="208" x2="348" y2="208" stroke="#64c3a6" stroke-opacity="0.38" stroke-width="5"/><line x1="366" y1="208" x2="498" y2="208" stroke="#e5c07b" stroke-opacity="0.32" stroke-width="5"/><line x1="84" y1="240" x2="412" y2="240" stroke="#d8e6e0" stroke-opacity="0.16" stroke-width="5"/></g><g transform="translate(556,142) scale(8.8)"><path fill="#e6f2ed" fill-opacity="0.9" d="M8 1.2a6.8 6.8 0 0 0-2.15 13.25c.34.06.46-.15.46-.33v-1.15c-1.89.41-2.29-.91-2.29-.91-.31-.78-.75-.99-.75-.99-.62-.42.05-.41.05-.41.68.05 1.04.7 1.04.7.61 1.04 1.59.74 1.98.57.06-.44.24-.74.43-.91-1.51-.17-3.1-.76-3.1-3.36 0-.74.26-1.35.7-1.82-.07-.17-.3-.86.07-1.8 0 0 .57-.18 1.87.7a6.5 6.5 0 0 1 3.4 0c1.3-.88 1.87-.7 1.87-.7.37.94.14 1.63.07 1.8.44.47.7 1.08.7 1.82 0 2.61-1.59 3.19-3.11 3.36.25.21.46.63.46 1.27v1.88c0 .18.12.39.47.33A6.8 6.8 0 0 0 8 1.2Z"/></g><text x="84" y="330" font-family="system-ui,-apple-system,sans-serif" font-size="30" font-weight="700" fill="#eef6f2" letter-spacing="0.4">OPEN SOURCE MATRIX</text><text x="84" y="364" font-family="ui-monospace,SFMono-Regular,Menlo,monospace" font-size="15" fill="#8fb5a7">git://github.com · public repositories &amp; craft</text></svg>`
	default:
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(svg))
	}
}

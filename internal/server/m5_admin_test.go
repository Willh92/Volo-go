package server_test

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/adminui"
	"github.com/Willh92/Volo-go/internal/auth"
	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/content"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/guest"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/notify"
	"github.com/Willh92/Volo-go/internal/render"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/internal/server"
	"github.com/Willh92/Volo-go/internal/visibility"
	"github.com/Willh92/Volo-go/pkg/id"
	"github.com/Willh92/Volo-go/pkg/plugin"
)

type m5Env struct {
	ctx      context.Context
	cfg      *config.Config
	store    *sqlx.Store
	settings *repo.SettingsManager
	catalog  *i18n.Catalog
	authSvc  *auth.Service
	content  *content.Service
	app      *server.AppServer
	ts       *httptest.Server
}

func setupM5TestEnv(t *testing.T, customAdminPath string) *m5Env {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()

	dataDir := filepath.Join(tmp, "data")
	uploadDir := filepath.Join(tmp, "uploads")
	pluginsDir := filepath.Join(tmp, "plugins")
	themesDir := filepath.Join(tmp, "themes")
	for _, d := range []string{dataDir, uploadDir, pluginsDir, themesDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}

	secret := "0123456789abcdef0123456789abcdef-m5-master-secret"
	pubURL := "https://studio.example.com"
	overrides := config.CLIOverrides{
		SecretKey:  &secret,
		PublicURL:  &pubURL,
		DataDir:    &dataDir,
		UploadDir:  &uploadDir,
		PluginsDir: &pluginsDir,
		ThemesDir:  &themesDir,
	}
	if customAdminPath != "" {
		overrides.AdminPath = &customAdminPath
	}
	cfg, err := config.Load(overrides)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(dataDir, "volo-m5.db"),
	})
	if err != nil {
		t.Fatalf("sqlx.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatalf("migrate.Up: %v", err)
	}

	cat, err := i18n.NewCatalog("zh-CN", "")
	if err != nil {
		t.Fatal(err)
	}
	settingsMgr := repo.NewSettingsManager(store, cfg.KeyRing, "zh-CN")
	if _, err := settingsMgr.Reload(ctx); err != nil {
		t.Fatal(err)
	}

	pluginMgr := plugin.NewManager(plugin.ManagerOptions{
		Store:      store,
		Keys:       cfg.KeyRing,
		PluginsDir: pluginsDir,
		DataDir:    dataDir,
		Catalog:    cat,
	})
	if err := pluginMgr.DiscoverAndStart(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pluginMgr.Close(ctx) })

	passports := guest.NewPassportManager(cfg.KeyRing, cfg.PublicURL)
	pow := guest.NewPoWVerifier(cfg.KeyRing)
	rnd := render.Default()
	grd := visibility.NewGuard(cfg.KeyRing, settingsMgr, passports, cat, rnd)
	srch := visibility.NewSearchIndex(store, rnd, 2<<20)
	cnt := content.NewService(content.ServiceOptions{
		Store:     store,
		Keys:      cfg.KeyRing,
		Settings:  settingsMgr,
		Passports: passports,
		PoW:       pow,
		Guard:     grd,
		Search:    srch,
		Renderer:  rnd,
		Plugins:   pluginMgr,
		Catalog:   cat,
	})
	authSvc := auth.NewService(store, cfg.KeyRing, settingsMgr, cfg.PublicURL)
	disp := notify.NewDispatcher(notify.DispatcherOptions{
		Store:     store,
		Keys:      cfg.KeyRing,
		Settings:  settingsMgr,
		Catalog:   cat,
		Plugins:   pluginMgr,
		PublicURL: cfg.PublicURL,
	})

	t.Cleanup(func() { _ = disp.Close(ctx) })

	appSrv, err := server.NewAppServer(server.AppDeps{
		Config:                cfg,
		Store:                 store,
		Settings:              settingsMgr,
		Catalog:               cat,
		Auth:                  authSvc,
		Passports:             passports,
		PoW:                   pow,
		Plugins:               pluginMgr,
		Content:               cnt,
		SendVerificationMail:  disp.SendVerificationCode,
		NotifyCommentCreated:  disp.NotifyCommentCreated,
		NotifyCommentApproved: disp.NotifyCommentReplyApproved,
		TestSMTP:              disp.SendTestSMTP,
		TestWebhook:           disp.TriggerTestWebhook,
	})
	if err != nil {
		t.Fatalf("NewAppServer: %v", err)
	}
	ts := httptest.NewServer(appSrv.Handler())
	t.Cleanup(ts.Close)

	return &m5Env{
		ctx:      ctx,
		cfg:      cfg,
		store:    store,
		settings: settingsMgr,
		catalog:  cat,
		authSvc:  authSvc,
		content:  cnt,
		app:      appSrv,
		ts:       ts,
	}
}

func doJSONReq(t *testing.T, method, urlStr string, body any, cookies []*http.Cookie, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		switch v := body.(type) {
		case string:
			rdr = strings.NewReader(v)
		case []byte:
			rdr = bytes.NewReader(v)
		default:
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			rdr = bytes.NewReader(b)
		}
	}
	req, err := http.NewRequest(method, urlStr, rdr)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s failed: %v", method, urlStr, err)
	}
	defer resp.Body.Close()
	var raw []byte
	if resp.Header.Get("Content-Encoding") == "gzip" && resp.StatusCode != http.StatusNotModified && resp.StatusCode != http.StatusNoContent {
		gz, gErr := gzip.NewReader(resp.Body)
		if gErr != nil {
			t.Fatalf("gzip.NewReader: %v", gErr)
		}
		raw, _ = io.ReadAll(gz)
		_ = gz.Close()
	} else {
		raw, _ = io.ReadAll(resp.Body)
	}
	return resp, raw
}

func TestM5_AdminBundleBudgetAndStealthMount(t *testing.T) {
	gzBytes := adminui.Default().TotalGzipBytes()
	if gzBytes <= 50*1024 {
		t.Fatalf("expected embedded Admin SPA + CodeMirror 6 bundle > 50KB gzip, got %d bytes", gzBytes)
	}
	if gzBytes >= 300*1024 {
		t.Fatalf("embedded Admin SPA bundle exceeded 300KB gzip budget: got %d bytes (%.2f KB)", gzBytes, float64(gzBytes)/1024.0)
	}

	env := setupM5TestEnv(t, "/secret-studio-99")

	// Default /console and /admin paths MUST stealthily return 404 Not Found
	for _, probe := range []string{"/console", "/console/", "/console/assets/index.js", "/admin", "/admin/"} {
		resp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+probe, nil, nil, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("stealth VOLO_ADMIN_PATH leaked on %s: expected 404, got %d", probe, resp.StatusCode)
		}
	}

	// Custom /secret-studio-99 MUST serve HTML with <base href="/secret-studio-99/">, zero external CDNs,
	// and pre-compressed Gzip + strong ETag + 304 Not Modified on /secret-studio-99/assets/*.js!
	resp, htmlBytes := doJSONReq(t, http.MethodGet, env.ts.URL+"/secret-studio-99", nil, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /secret-studio-99 status = %d, want 200", resp.StatusCode)
	}
	html := string(htmlBytes)
	if !strings.Contains(html, `<base href="/secret-studio-99/">`) {
		t.Fatalf("expected <base href=\"/secret-studio-99/\"> in SPA HTML, got:\n%s", html)
	}
	if !strings.Contains(html, `"admin_path":"/secret-studio-99"`) {
		t.Fatalf("expected boot config admin_path in SPA HTML, got:\n%s", html)
	}
	for _, cdn := range []string{"unpkg.com", "jsdelivr.net", "cdnjs.cloudflare.com", "fonts.googleapis.com"} {
		if strings.Contains(strings.ToLower(html), cdn) {
			t.Fatalf("SPA HTML contains external CDN reference %q", cdn)
		}
	}
	idx := strings.LastIndex(html, "assets/")
	if idx < 0 {
		t.Fatalf("expected hashed assets/ reference in SPA HTML: %s", html)
	}
	endIdx := strings.IndexAny(html[idx:], `"' >`)
	assetRel := html[idx : idx+endIdx]
	assetResp, assetBody := doJSONReq(t, http.MethodGet, env.ts.URL+"/secret-studio-99/"+assetRel, nil, nil, map[string]string{
		"Accept-Encoding": "gzip",
	})
	if assetResp.StatusCode != http.StatusOK || assetResp.Header.Get("Content-Encoding") != "gzip" || len(assetBody) == 0 {
		t.Fatalf("expected pre-compressed gzip asset on /secret-studio-99/%s, got status=%d encoding=%q",
			assetRel, assetResp.StatusCode, assetResp.Header.Get("Content-Encoding"))
	}
	etag := assetResp.Header.Get("ETag")
	if etag == "" {
		t.Fatalf("expected strong ETag on SPA asset")
	}
	notModResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/secret-studio-99/"+assetRel, nil, nil, map[string]string{
		"If-None-Match": etag,
	})
	if notModResp.StatusCode != http.StatusNotModified {
		t.Fatalf("expected 304 Not Modified with If-None-Match=%s, got %d", etag, notModResp.StatusCode)
	}
}

func TestM5_SetupWizardLifecycleAndPermanentLockout(t *testing.T) {
	env := setupM5TestEnv(t, "/console")

	// 1. Fresh uninitialized GET / redirects to /setup
	resp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/", nil, nil, nil)
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/setup" {
		t.Fatalf("uninitialized GET / expected 302 -> /setup, got %d (Location=%q)", resp.StatusCode, resp.Header.Get("Location"))
	}

	// 2. GET /setup serves wizard SPA mode
	resp, setupHTML := doJSONReq(t, http.MethodGet, env.ts.URL+"/setup", nil, nil, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(setupHTML), `"mode":"setup"`) {
		t.Fatalf("GET /setup failed: status=%d html=%s", resp.StatusCode, string(setupHTML))
	}

	// 3. GET /api/setup/status & POST /api/setup/test-db succeed while unlocked
	resp, statusRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/setup/status", nil, nil, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(statusRaw), `"initialized":false`) {
		t.Fatalf("GET /api/setup/status = %d %s", resp.StatusCode, string(statusRaw))
	}
	resp, dbRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/setup/test-db", map[string]any{
		"driver": "sqlite",
		"dsn":    env.cfg.DBDSN,
	}, nil, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(dbRaw), `"dialect":"sqlite"`) {
		t.Fatalf("POST /api/setup/test-db = %d %s", resp.StatusCode, string(dbRaw))
	}

	// 4. POST /api/setup/complete initializes site, creates Sovereign admin & seeds /about page
	resp, compRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/setup/complete", map[string]any{
		"site_title":         "Sovereign Studio",
		"site_subtitle":      "Craft & Signal",
		"site_description":   "Clean-slate blog",
		"default_locale":     "en-US",
		"timezone":           "UTC",
		"accent":             "teal",
		"admin_username":     "captain",
		"admin_email":        "captain@example.com",
		"admin_display_name": "Captain Volo",
		"admin_password":     "Str0ng!Pass#2026",
	}, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/setup/complete = %d %s", resp.StatusCode, string(compRaw))
	}
	var compRes struct {
		CSRFToken string `json:"csrf_token"`
	}
	_ = json.Unmarshal(compRaw, &compRes)
	if compRes.CSRFToken == "" || len(resp.Cookies()) == 0 {
		t.Fatalf("expected session cookie and csrf_token from setup complete")
	}

	// 5. Public home & /about standalone page now render 200 OK
	resp, homeHTML := doJSONReq(t, http.MethodGet, env.ts.URL+"/", nil, nil, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(homeHTML), "Sovereign Studio") {
		t.Fatalf("initialized GET / expected 200 with site title, got %d", resp.StatusCode)
	}
	resp, aboutHTML := doJSONReq(t, http.MethodGet, env.ts.URL+"/about", nil, nil, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(aboutHTML), "About") {
		t.Fatalf("GET /about expected 200 standalone page, got %d", resp.StatusCode)
	}

	// 6. Permanent lockout: /setup and ALL /api/setup/* routes MUST return 404 Not Found!
	for _, locked := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/setup"},
		{http.MethodGet, "/setup/"},
		{http.MethodGet, "/api/setup/status"},
		{http.MethodPost, "/api/setup/test-db"},
		{http.MethodPost, "/api/setup/complete"},
		{http.MethodPost, "/api/setup/import-legacy"},
	} {
		rLocked, _ := doJSONReq(t, locked.method, env.ts.URL+locked.path, "{}", nil, nil)
		if rLocked.StatusCode != http.StatusNotFound {
			t.Fatalf("expected permanent 404 on %s %s after setup, got %d", locked.method, locked.path, rLocked.StatusCode)
		}
	}
}

func TestM5_AtomicSettingsPatchSecretMaskingRBACAndRevisions(t *testing.T) {
	env := setupM5TestEnv(t, "/console")

	// Complete setup to obtain Sovereign Admin session + CSRF token
	setupResp, setupRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/setup/complete", map[string]any{
		"site_title":     "Studio Alpha",
		"site_subtitle":  "Initial Subtitle",
		"default_locale": "zh-CN",
		"accent":         "forest",
		"admin_username": "rootadmin",
		"admin_email":    "root@example.com",
		"admin_password": "Admin!Password#2026",
	}, nil, nil)
	if setupResp.StatusCode != http.StatusOK {
		t.Fatalf("setup complete failed: %d %s", setupResp.StatusCode, string(setupRaw))
	}
	adminCookies := setupResp.Cookies()
	var setupData struct {
		CSRFToken string      `json:"csrf_token"`
		User      domain.User `json:"user"`
	}
	_ = json.Unmarshal(setupRaw, &setupData)
	adminCSRF := map[string]string{auth.CSRFHeaderName: setupData.CSRFToken}

	// 1. Missing CSRF header MUST be rejected with 403 Forbidden
	noCSRFResp, _ := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/site", map[string]any{
		"title": "Tampered",
	}, adminCookies, nil)
	if noCSRFResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 on cookie PATCH without X-CSRF-Token, got %d", noCSRFResp.StatusCode)
	}

	// 2. Partial PATCH across site, reading, feed (podcast), seo, storage, notifications without clobbering siblings!
	pSiteResp, pSiteRaw := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/site", map[string]any{
		"title": "Studio Omega",
	}, adminCookies, adminCSRF)
	if pSiteResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH settings/site = %d %s", pSiteResp.StatusCode, string(pSiteRaw))
	}
	var sitePatchOut struct {
		Version int64               `json:"version"`
		Value   domain.SiteSettings `json:"value"`
	}
	_ = json.Unmarshal(pSiteRaw, &sitePatchOut)
	if sitePatchOut.Value.Title != "Studio Omega" || sitePatchOut.Value.Subtitle != "Initial Subtitle" {
		t.Fatalf("PATCH settings/site clobbered sibling Subtitle: %+v", sitePatchOut.Value)
	}

	// Optimistic concurrency: stale expected_version MUST return 409 Conflict!
	staleVer := sitePatchOut.Version - 1
	if staleVer < 1 {
		// Bump once more so staleVer >= 1
		r2, raw2 := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/site", map[string]any{
			"expected_version": sitePatchOut.Version,
			"footer_motto":     "Stay Sovereign",
		}, adminCookies, adminCSRF)
		if r2.StatusCode != http.StatusOK {
			t.Fatalf("second PATCH settings/site failed: %d %s", r2.StatusCode, string(raw2))
		}
		staleVer = sitePatchOut.Version
	}
	conflictResp, _ := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/site", map[string]any{
		"expected_version": staleVer,
		"title":            "Stale Overwrite",
	}, adminCookies, adminCSRF)
	if conflictResp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict on stale expected_version=%d, got %d", staleVer, conflictResp.StatusCode)
	}

	// Patch reading, feed (Podcast), seo, storage (with S3 secret), notifications (with SMTP password)
	for _, tc := range []struct {
		domain string
		patch  map[string]any
	}{
		{"reading", map[string]any{"posts_per_page": 15}},
		{"feed", map[string]any{"podcast": map[string]any{"enabled": true, "title": "Omega Radio", "category": "Technology"}}},
		{"seo", map[string]any{"github_username": "volo-dev", "indexnow_key": "idx-key-777"}},
		{"storage", map[string]any{"active_driver": "local", "s3_bucket": "omega-media", "s3_secret_key": "SUPER_SECRET_S3_KEY_999"}},
		{"notifications", map[string]any{"smtp_enabled": true, "smtp_host": "smtp.example.com", "smtp_password": "SUPER_SECRET_SMTP_PASS_888"}},
	} {
		rPatch, rawPatch := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/"+tc.domain, tc.patch, adminCookies, adminCSRF)
		if rPatch.StatusCode != http.StatusOK {
			t.Fatalf("PATCH /api/admin/settings/%s = %d %s", tc.domain, rPatch.StatusCode, string(rawPatch))
		}
	}

	// Verify instant hot-reload in Settings.Snapshot() + zero plaintext secret leakage in GET /api/admin/settings AND raw SQLite!
	snap := env.settings.Snapshot()
	if snap.Site.Title != "Studio Omega" || snap.Reading.PostsPerPage != 15 || snap.Feed.Podcast.Title != "Omega Radio" || snap.SEO.GitHubUsername != "volo-dev" {
		t.Fatalf("Settings.Snapshot() did not hot-reload patched domains: %+v", snap)
	}
	allSetResp, allSetRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/settings", nil, adminCookies, nil)
	if allSetResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/admin/settings = %d", allSetResp.StatusCode)
	}
	allSetStr := string(allSetRaw)
	if strings.Contains(allSetStr, "SUPER_SECRET_S3_KEY_999") || strings.Contains(allSetStr, "SUPER_SECRET_SMTP_PASS_888") {
		t.Fatalf("CRITICAL SECRET LEAK in GET /api/admin/settings: %s", allSetStr)
	}
	if !strings.Contains(allSetStr, `"s3_secret_key_set":true`) || !strings.Contains(allSetStr, `"smtp_password_set":true`) {
		t.Fatalf("expected masked *_set:true flags in GET /api/admin/settings, got: %s", allSetStr)
	}
	var rawStorageDB, rawNotifyDB string
	_ = env.store.QueryRowContext(env.ctx, `SELECT value_json FROM settings WHERE domain = 'storage'`).Scan(&rawStorageDB)
	_ = env.store.QueryRowContext(env.ctx, `SELECT value_json FROM settings WHERE domain = 'notifications'`).Scan(&rawNotifyDB)
	if strings.Contains(rawStorageDB, "SUPER_SECRET_S3_KEY_999") || strings.Contains(rawNotifyDB, "SUPER_SECRET_SMTP_PASS_888") {
		t.Fatalf("CRITICAL: plaintext credential written unencrypted to settings table!")
	}

	// 3. RBAC: Reader physical 403 + Editor author-scoped isolation
	// Create an active Reader via POST /api/admin/users and assert 403 on all /api/admin/* endpoints
	rdCreateResp, rdCreateRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/users", map[string]any{
		"username":     "pure_reader",
		"email":        "reader@example.com",
		"display_name": "Reader",
		"role":         "reader",
		"password":     "Reader!Pass#2026",
	}, adminCookies, adminCSRF)
	if rdCreateResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/admin/users (reader) = %d %s", rdCreateResp.StatusCode, string(rdCreateRaw))
	}
	var rdOut struct {
		User domain.User `json:"user"`
	}
	_ = json.Unmarshal(rdCreateRaw, &rdOut)
	readerID := rdOut.User.ID
	_, readerRawTok, err := env.authSvc.CreateSession(env.ctx, readerID, netip.MustParseAddr("203.0.113.9"), "US", "ReaderUA", 86400)
	if err != nil {
		t.Fatalf("CreateSession reader: %v", err)
	}
	readerCookies := []*http.Cookie{{Name: auth.SessionCookieName, Value: readerRawTok}}
	for _, p := range []string{"/api/admin/me", "/api/admin/articles", "/api/admin/settings"} {
		rr, _ := doJSONReq(t, http.MethodGet, env.ts.URL+p, nil, readerCookies, nil)
		if rr.StatusCode != http.StatusForbidden {
			t.Fatalf("Reader accessed %s: got %d, want 403 Forbidden", p, rr.StatusCode)
		}
	}

	// Admin creates an Editor collaborator via POST /api/admin/users
	edCreateResp, edCreateRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/users", map[string]any{
		"username":         "collab_editor",
		"email":            "editor@example.com",
		"display_name":     "Collab Editor",
		"role":             "editor",
		"password":         "Editor!Pass#2026",
		"current_password": "Admin!Password#2026",
	}, adminCookies, adminCSRF)
	if edCreateResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/admin/users (editor) = %d %s", edCreateResp.StatusCode, string(edCreateRaw))
	}

	// Admin creates a category and publishes an Admin-owned article
	catResp, catRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/categories", map[string]any{
		"name": "Architecture",
		"slug": "architecture",
	}, adminCookies, adminCSRF)
	if catResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/admin/categories = %d %s", catResp.StatusCode, string(catRaw))
	}
	var catOut struct {
		Category domain.Category `json:"category"`
	}
	_ = json.Unmarshal(catRaw, &catOut)

	admArtResp, admArtRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"category_id":   catOut.Category.ID,
		"title":         "Admin Sovereign Manifesto",
		"slug":          "admin-manifesto",
		"content_md":    "Admin secret architecture notes.",
		"status":        "published",
		"allow_comment": true,
		"tags":          []string{"go", "sovereign"},
	}, adminCookies, adminCSRF)
	if admArtResp.StatusCode != http.StatusCreated {
		t.Fatalf("admin POST /api/admin/articles = %d %s", admArtResp.StatusCode, string(admArtRaw))
	}
	var admArtOut struct {
		Article domain.RawArticle `json:"article"`
	}
	_ = json.Unmarshal(admArtRaw, &admArtOut)

	// Editor logs in via POST /api/admin/auth/login
	edLoginResp, edLoginRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/auth/login", map[string]any{
		"ident":    "collab_editor",
		"password": "Editor!Pass#2026",
	}, nil, nil)
	if edLoginResp.StatusCode != http.StatusOK {
		t.Fatalf("editor login failed: %d %s", edLoginResp.StatusCode, string(edLoginRaw))
	}
	edCookies := edLoginResp.Cookies()
	var edLoginData struct {
		CSRFToken string      `json:"csrf_token"`
		User      domain.User `json:"user"`
	}
	_ = json.Unmarshal(edLoginRaw, &edLoginData)
	edCSRF := map[string]string{auth.CSRFHeaderName: edLoginData.CSRFToken}

	// Editor creates their own article
	edArtResp, edArtRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"category_id":   catOut.Category.ID,
		"title":         "Editor Field Notes",
		"slug":          "editor-field-notes",
		"content_md":    "Initial v1 body",
		"status":        "published",
		"allow_comment": true,
	}, edCookies, edCSRF)
	if edArtResp.StatusCode != http.StatusCreated {
		t.Fatalf("editor POST /api/admin/articles = %d %s", edArtResp.StatusCode, string(edArtRaw))
	}
	var edArtOut struct {
		Article domain.RawArticle `json:"article"`
	}
	_ = json.Unmarshal(edArtRaw, &edArtOut)

	// Seed 1 comment on Admin's post and 1 comment on Editor's post (with raw IP & encrypted notify email)
	admCmtID := id.Next()
	edCmtID := id.Next()
	encMail, _ := env.cfg.KeyRing.SealString("guest@private.example", "comment_email")
	nowMS := time.Now().UnixMilli()
	for _, c := range []struct {
		id     string
		postID string
		body   string
	}{
		{admCmtID, admArtOut.Article.ID, "Comment on Admin Post"},
		{edCmtID, edArtOut.Article.ID, "Comment on Editor Post"},
	} {
		_, err := env.store.ExecContext(env.ctx,
			`INSERT INTO comments (id, target_type, target_id, parent_id, reply_to_name, user_id, guest_id_hash, env_fp_hash,
			        author_name, author_url, avatar_url, notify_email_ciphertext, notify_locale, client_ip, ip_masked, geo_region,
			        content_md, status, is_deleted, created_at, updated_at)
			 VALUES (?, 'article', ?, '', '', '', 'ghash', 'fphash', 'GuestReader', '', '/avatars/gseed.svg', ?, 'en-US',
			        '198.51.100.42', '198.51.100.*', 'SG', ?, 'pending', 0, ?, ?)`,
			c.id, c.postID, encMail, c.body, nowMS, nowMS)
		if err != nil {
			t.Fatalf("seed comment: %v", err)
		}
	}

	// Editor lists articles & comments -> MUST ONLY see their own, and comment ClientIP + NotifyEmailCiphertext MUST be scrubbed!
	edListArtResp, edListArtRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/articles", nil, edCookies, nil)
	if edListArtResp.StatusCode != http.StatusOK || strings.Contains(string(edListArtRaw), "admin-manifesto") {
		t.Fatalf("editor article list leaked admin's article: %s", string(edListArtRaw))
	}
	edListCmtResp, edListCmtRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/comments", nil, edCookies, nil)
	if edListCmtResp.StatusCode != http.StatusOK {
		t.Fatalf("editor GET /api/admin/comments = %d", edListCmtResp.StatusCode)
	}
	edCmtStr := string(edListCmtRaw)
	if strings.Contains(edCmtStr, "Comment on Admin Post") {
		t.Fatalf("CRITICAL RBAC LEAK: editor comment list exposed admin's article comment: %s", edCmtStr)
	}
	if !strings.Contains(edCmtStr, "Comment on Editor Post") {
		t.Fatalf("editor did not see comment on own article: %s", edCmtStr)
	}
	if strings.Contains(edCmtStr, "198.51.100.42") || strings.Contains(edCmtStr, encMail) || strings.Contains(edCmtStr, "guest@private.example") {
		t.Fatalf("editor comment payload leaked raw ClientIP or NotifyEmail: %s", edCmtStr)
	}

	// Editor MUST get 403 Forbidden when probing Admin's article, Admin's comment, or Admin-only system routes!
	for _, forbiddenReq := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/api/admin/articles/" + admArtOut.Article.ID, nil},
		{http.MethodDelete, "/api/admin/articles/" + admArtOut.Article.ID, nil},
		{http.MethodPatch, "/api/admin/comments/" + admCmtID, map[string]string{"status": "approved"}},
		{http.MethodDelete, "/api/admin/comments/" + admCmtID, nil},
		{http.MethodGet, "/api/admin/settings", nil},
		{http.MethodGet, "/api/admin/plugins", nil},
		{http.MethodGet, "/api/admin/users", nil},
		{http.MethodGet, "/api/admin/backup/export", nil},
	} {
		fResp, _ := doJSONReq(t, forbiddenReq.method, env.ts.URL+forbiddenReq.path, forbiddenReq.body, edCookies, edCSRF)
		if fResp.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden for editor on %s %s, got %d", forbiddenReq.method, forbiddenReq.path, fResp.StatusCode)
		}
	}

	// Editor CAN approve comment on their own article
	edApproveResp, _ := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/comments/"+edCmtID, map[string]string{
		"status": "approved",
	}, edCookies, edCSRF)
	if edApproveResp.StatusCode != http.StatusOK {
		t.Fatalf("editor failed to approve comment on own article: %d", edApproveResp.StatusCode)
	}

	// 4. Scoped API Token (`volo_pat_...`): stateless Bearer auth without CSRF, strict scope enforcement
	patResp, patRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/security/tokens", map[string]any{
		"name":             "Obsidian Reader",
		"scopes":           []string{"articles:read"},
		"expires_in_days":  30,
		"current_password": "Admin!Password#2026",
	}, adminCookies, adminCSRF)
	if patResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/admin/security/tokens = %d %s", patResp.StatusCode, string(patRaw))
	}
	var patOut struct {
		PlaintextToken string `json:"plaintext_token"`
	}
	_ = json.Unmarshal(patRaw, &patOut)
	if !strings.HasPrefix(patOut.PlaintextToken, "volo_pat_") {
		t.Fatalf("expected volo_pat_ token prefix, got %q", patOut.PlaintextToken)
	}
	bearerHdr := map[string]string{"Authorization": "Bearer " + patOut.PlaintextToken}

	patReadResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/articles", nil, nil, bearerHdr)
	if patReadResp.StatusCode != http.StatusOK {
		t.Fatalf("PAT with articles:read failed GET /api/admin/articles: %d", patReadResp.StatusCode)
	}
	patWriteResp, _ := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"title":      "Unauthorized PAT Write",
		"content_md": "blocked",
	}, nil, bearerHdr)
	if patWriteResp.StatusCode != http.StatusForbidden {
		t.Fatalf("PAT with only articles:read MUST get 403 on POST /api/admin/articles, got %d", patWriteResp.StatusCode)
	}

	// 5. 20-Revision Cap, 1-Click Rollback, ShareToken Preview & Safe Batch .zip Markdown Import
	for i := 2; i <= 24; i++ {
		uResp, uRaw := doJSONReq(t, http.MethodPut, env.ts.URL+"/api/admin/articles/"+edArtOut.Article.ID, map[string]any{
			"category_id":   catOut.Category.ID,
			"title":         "Editor Field Notes",
			"slug":          "editor-field-notes",
			"content_md":    fmt.Sprintf("Updated body revision #%d", i),
			"status":        "published",
			"allow_comment": true,
			"revision_note": fmt.Sprintf("rev-%d", i),
		}, edCookies, edCSRF)
		if uResp.StatusCode != http.StatusOK {
			t.Fatalf("PUT article iteration %d = %d %s", i, uResp.StatusCode, string(uRaw))
		}
	}
	revResp, revRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/articles/"+edArtOut.Article.ID+"/revisions", nil, edCookies, nil)
	if revResp.StatusCode != http.StatusOK {
		t.Fatalf("GET revisions = %d %s", revResp.StatusCode, string(revRaw))
	}
	var revOut struct {
		Items []domain.ArticleRevision `json:"items"`
	}
	_ = json.Unmarshal(revRaw, &revOut)
	if len(revOut.Items) != 20 {
		t.Fatalf("expected revision history capped at 20 rows, got %d", len(revOut.Items))
	}
	oldestKept := revOut.Items[len(revOut.Items)-1]
	restResp, restRaw := doJSONReq(t, http.MethodPost,
		env.ts.URL+"/api/admin/articles/"+edArtOut.Article.ID+"/revisions/"+oldestKept.ID+"/restore",
		"{}", edCookies, edCSRF)
	if restResp.StatusCode != http.StatusOK || !strings.Contains(string(restRaw), oldestKept.ContentMD) {
		t.Fatalf("POST revision restore failed: %d %s", restResp.StatusCode, string(restRaw))
	}

	// ShareToken timed preview unlocks a draft article on public route
	pw := "vault-123"
	draftResp, draftRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"title":      "Locked Draft Preview",
		"slug":       "locked-draft-preview",
		"content_md": "PREVIEW_CANARY_BODY_7788",
		"status":     "draft",
		"password":   &pw,
	}, adminCookies, adminCSRF)
	if draftResp.StatusCode != http.StatusCreated {
		t.Fatalf("create locked draft = %d %s", draftResp.StatusCode, string(draftRaw))
	}
	var draftOut struct {
		Article domain.RawArticle `json:"article"`
	}
	_ = json.Unmarshal(draftRaw, &draftOut)
	shResp, shRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles/"+draftOut.Article.ID+"/share-token", map[string]any{
		"ttl_hours": 2,
	}, adminCookies, adminCSRF)
	if shResp.StatusCode != http.StatusOK {
		t.Fatalf("issue share-token = %d %s", shResp.StatusCode, string(shRaw))
	}
	var shOut struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(shRaw, &shOut)
	pubDraftNoTok, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/posts/locked-draft-preview", nil, nil, nil)
	if pubDraftNoTok.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 on draft without preview token, got %d", pubDraftNoTok.StatusCode)
	}
	pubDraftWithTok, pubDraftHTML := doJSONReq(t, http.MethodGet, env.ts.URL+"/posts/locked-draft-preview?preview="+shOut.Token, nil, nil, nil)
	if pubDraftWithTok.StatusCode != http.StatusOK || !strings.Contains(string(pubDraftHTML), "PREVIEW_CANARY_BODY_7788") {
		t.Fatalf("ShareToken preview failed to unlock draft: status=%d", pubDraftWithTok.StatusCode)
	}

	// Batch .zip Markdown Import: reject path-traversal zip, accept clean frontmatter zip
	evilZip := buildMemoryZip(t, map[string]string{
		"../../etc/evil.md": "# Evil Escape",
	})
	evilStatus, _ := uploadBatchZip(t, env.ts.URL+"/api/admin/articles/import", evilZip, adminCookies, setupData.CSRFToken)
	if evilStatus != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request on path-traversal .zip, got %d", evilStatus)
	}

	goodZip := buildMemoryZip(t, map[string]string{
		"notes/clean-go-craft.md": "---\ntitle: Clean Go Craft\nslug: clean-go-craft\ncategory: Engineering\ntags: [golang, craft]\n---\nPure Go static binary craft.",
		"notes/zero-baggage.md":   "---\ntitle: Zero Historical Baggage\nslug: zero-historical-baggage\ncategory: Engineering\n---\nFresh relational schema.",
	})
	goodStatus, goodBody := uploadBatchZip(t, env.ts.URL+"/api/admin/articles/import", goodZip, adminCookies, setupData.CSRFToken)
	if goodStatus != http.StatusOK || !strings.Contains(string(goodBody), `"imported_count":2`) {
		t.Fatalf("expected 2 imported markdown articles, got %d %s", goodStatus, string(goodBody))
	}
	pubImpResp, pubImpHTML := doJSONReq(t, http.MethodGet, env.ts.URL+"/posts/clean-go-craft", nil, nil, nil)
	if pubImpResp.StatusCode != http.StatusOK || !strings.Contains(string(pubImpHTML), "Pure Go static binary craft.") {
		t.Fatalf("imported markdown post not served publicly: %d", pubImpResp.StatusCode)
	}
}

func buildMemoryZip(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		hdr := &zip.FileHeader{Name: name, Method: zip.Deflate}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func uploadBatchZip(t *testing.T, urlStr string, zipBytes []byte, cookies []*http.Cookie, csrf string) (int, []byte) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "posts.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(zipBytes); err != nil {
		t.Fatal(err)
	}
	_ = mw.Close()

	req, err := http.NewRequest(http.MethodPost, urlStr, &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set(auth.CSRFHeaderName, csrf)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

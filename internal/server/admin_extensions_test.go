package server_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/auth"
	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/content"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/extpkg"
	"github.com/Willh92/Volo-go/internal/guest"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/render"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/internal/security"
	"github.com/Willh92/Volo-go/internal/server"
	"github.com/Willh92/Volo-go/internal/visibility"
	"github.com/Willh92/Volo-go/pkg/plugin"
)

func buildTestExtZip(t *testing.T, files map[string]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		hdr := &zip.FileHeader{
			Name:   name,
			Method: zip.Deflate,
		}
		hdr.SetMode(0o644)
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := fw.Write([]byte(body)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	raw := buf.Bytes()
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:])
}

func TestAdminExtensions_SSRFBlockedByDefault(t *testing.T) {
	env, adminCookies, adminHdrs, _ := bootstrapAuditEnv(t)
	adminHdrs["Origin"] = env.ts.URL

	// Default OutboundSafeDialer blocks loopback/private URLs on both plugin and theme URL install.
	resp, raw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/plugins/install", map[string]any{
		"url": "http://127.0.0.1:8080/evil-plugin.zip",
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected SSRF loopback plugin URL install to fail with 400, got %d: %s", resp.StatusCode, string(raw))
	}

	resp, raw = doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/themes/install", map[string]any{
		"url": "http://169.254.169.254/latest/meta-data/",
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected SSRF metadata theme URL install to fail with 400, got %d: %s", resp.StatusCode, string(raw))
	}
}

func TestAdminExtensions_URLInstallCatalogCheckUpdatesAndOnlineUpgrade(t *testing.T) {
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

	var plugZipV1, plugZipV2, plugNoUpdZip []byte
	var plugShaV1, plugShaV2 string
	var themeZipV1, themeZipV2 []byte
	var themeShaV1, themeShaV2 string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/plugins/catalog.json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{
				"repository": "https://github.com/Willh92/Volo-go",
				"plugins": [{
					"id": "ext-demo",
					"version": "1.1.0",
					"min_host_version": "1.0.0",
					"runtime": "goja",
					"distribution": "standalone-release",
					"release_tag": "plugin-ext-demo-v1.1.0",
					"download_url": "http://%s/dl/plugin-ext-demo-v1.1.0.zip",
					"sha256": %q,
					"name": {"en-US": "Ext Demo Plugin", "zh-CN": "示例扩展插件"},
					"summary": {"en-US": "Demo plugin from catalog", "zh-CN": "目录示例插件"}
				}]
			}`, r.Host, plugShaV2)
		case "/themes/catalog.json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{
				"repository": "https://github.com/Willh92/Volo-go",
				"themes": [{
					"id": "demo-skin",
					"version": "1.2.0",
					"min_host_version": "1.0.0",
					"distribution": "standalone-release",
					"release_tag": "theme-demo-skin-v1.2.0",
					"download_url": "http://%s/dl/theme-demo-skin-v1.2.0.zip",
					"sha256": %q,
					"name": {"en-US": "Demo Skin", "zh-CN": "示例主题"},
					"summary": {"en-US": "Demo theme from catalog", "zh-CN": "目录示例主题"}
				}]
			}`, r.Host, themeShaV2)
		case "/dl/plugin-ext-demo-v1.0.0.zip":
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(plugZipV1)
		case "/dl/plugin-ext-demo-v1.1.0.zip":
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(plugZipV2)
		case "/dl/plugin-no-upd.zip":
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(plugNoUpdZip)
		case "/dl/theme-demo-skin-v1.0.0.zip":
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(themeZipV1)
		case "/dl/theme-demo-skin-v1.2.0.zip":
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(themeZipV2)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	// Map fake public hostname "cdn.example.test" to 127.0.0.1 for clean manifest validation + test loopback dialer.
	portPart := strings.TrimPrefix(upstream.URL, "http://127.0.0.1:")
	cdnBase := "http://cdn.example.test:" + portPart

	plugZipV1, plugShaV1 = buildTestExtZip(t, map[string]string{
		"manifest.json": fmt.Sprintf(`{
			"id": "ext-demo",
			"version": "1.0.0",
			"min_host_version": "1.0.0",
			"name": {"en-US": "Ext Demo Plugin", "zh-CN": "示例扩展插件"},
			"runtime": "goja",
			"entry": "main.js",
			"permissions": ["http:routes"],
			"update": {
				"strategy": "volo-catalog",
				"url": "%s/plugins/catalog.json"
			}
		}`, cdnBase),
		"main.js": `module.exports = { onRequest: function() { return { status: 200, json: { v: "1.0.0" } }; } };`,
	})
	plugZipV2, plugShaV2 = buildTestExtZip(t, map[string]string{
		"manifest.json": fmt.Sprintf(`{
			"id": "ext-demo",
			"version": "1.1.0",
			"min_host_version": "1.0.0",
			"name": {"en-US": "Ext Demo Plugin", "zh-CN": "示例扩展插件"},
			"runtime": "goja",
			"entry": "main.js",
			"permissions": ["http:routes"],
			"update": {
				"strategy": "volo-catalog",
				"url": "%s/plugins/catalog.json"
			}
		}`, cdnBase),
		"main.js": `module.exports = { onRequest: function() { return { status: 200, json: { v: "1.1.0" } }; } };`,
	})
	plugNoUpdZip, _ = buildTestExtZip(t, map[string]string{
		"manifest.json": `{
			"id": "offline-only",
			"version": "0.5.0",
			"name": {"en-US": "Offline Only", "zh-CN": "纯离线插件"},
			"runtime": "goja",
			"entry": "main.js"
		}`,
		"main.js": `exports.onEnable = function() {};`,
	})

	themeZipV1, themeShaV1 = buildTestExtZip(t, map[string]string{
		"theme.json": fmt.Sprintf(`{
			"id": "demo-skin",
			"version": "1.0.0",
			"min_host_version": "1.0.0",
			"author": "Volo",
			"name": {"en-US": "Demo Skin", "zh-CN": "示例主题"},
			"description": {"en-US": "v1", "zh-CN": "v1"},
			"update": {
				"strategy": "volo-catalog",
				"url": "%s/themes/catalog.json"
			}
		}`, cdnBase),
		"templates/index.html": `<!doctype html><html><body><div id="skin-ver">skin-v1.0.0</div></body></html>`,
	})
	themeZipV2, themeShaV2 = buildTestExtZip(t, map[string]string{
		"theme.json": fmt.Sprintf(`{
			"id": "demo-skin",
			"version": "1.2.0",
			"min_host_version": "1.0.0",
			"author": "Volo",
			"name": {"en-US": "Demo Skin", "zh-CN": "示例主题"},
			"description": {"en-US": "v2", "zh-CN": "v2"},
			"update": {
				"strategy": "volo-catalog",
				"url": "%s/themes/catalog.json"
			}
		}`, cdnBase),
		"templates/index.html": `<!doctype html><html><body><div id="skin-ver">skin-v1.2.0</div></body></html>`,
	})

	secret := "0123456789abcdef0123456789abcdef-ext-api-secret"
	pubURL := "https://studio.example.com"
	cfg, err := config.Load(config.CLIOverrides{
		SecretKey:  &secret,
		PublicURL:  &pubURL,
		DataDir:    &dataDir,
		UploadDir:  &uploadDir,
		PluginsDir: &pluginsDir,
		ThemesDir:  &themesDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(dataDir, "volo-ext.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatal(err)
	}

	cat, err := i18n.NewCatalog("en-US", "")
	if err != nil {
		t.Fatal(err)
	}
	settingsMgr := repo.NewSettingsManager(store, cfg.KeyRing, "en-US")
	_, _ = settingsMgr.Reload(ctx)

	testDialer := &security.OutboundSafeDialer{
		Timeout:               5 * time.Second,
		AllowLoopbackForTests: true,
		Resolver: func(_ context.Context, host string) ([]netip.Addr, error) {
			if host == "cdn.example.test" || host == "127.0.0.1" {
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			}
			return nil, fmt.Errorf("unexpected host %s", host)
		},
	}

	pluginMgr := plugin.NewManager(plugin.ManagerOptions{
		Store:          store,
		Keys:           cfg.KeyRing,
		PluginsDir:     pluginsDir,
		DataDir:        dataDir,
		Catalog:        cat,
		HostVersion:    "1.2.0",
		OutboundDialer: testDialer,
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
		Store:          store,
		Keys:           cfg.KeyRing,
		Settings:       settingsMgr,
		Passports:      passports,
		PoW:            pow,
		Guard:          grd,
		Search:         srch,
		Renderer:       rnd,
		Plugins:        pluginMgr,
		Catalog:        cat,
		OutboundDialer: testDialer,
	})
	authSvc := auth.NewService(store, cfg.KeyRing, settingsMgr, cfg.PublicURL)

	appSrv, err := server.NewAppServer(server.AppDeps{
		Config:         cfg,
		Store:          store,
		Settings:       settingsMgr,
		Catalog:        cat,
		Auth:           authSvc,
		Passports:      passports,
		PoW:            pow,
		OutboundDialer: testDialer,
		Plugins:        pluginMgr,
		Content:        cnt,
		Version:        "1.2.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	appSrv.SetCatalogURLsForTest(cdnBase+"/plugins/catalog.json", cdnBase+"/themes/catalog.json")

	ts := httptest.NewServer(appSrv.Handler())
	defer ts.Close()

	// Complete setup & get admin session + CSRF
	resp, compRaw := doJSONReq(t, http.MethodPost, ts.URL+"/api/setup/complete", map[string]any{
		"site_title":         "Ext Test",
		"default_locale":     "en-US",
		"timezone":           "UTC",
		"accent":             "teal",
		"admin_username":     "admin",
		"admin_email":        "admin@example.com",
		"admin_display_name": "Admin",
		"admin_password":     "Str0ng!Pass#2026",
	}, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setup/complete = %d %s", resp.StatusCode, string(compRaw))
	}
	var comp struct {
		CSRFToken string `json:"csrf_token"`
	}
	_ = json.Unmarshal(compRaw, &comp)
	cookies := resp.Cookies()
	hdrs := map[string]string{
		"Origin":            ts.URL,
		auth.CSRFHeaderName: comp.CSRFToken,
	}

	// 1. Direct URL install of plugin v1.0.0 with auto-enable + offline-only plugin
	resp, raw := doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/plugins/install", map[string]any{
		"url":    cdnBase + "/dl/plugin-ext-demo-v1.0.0.zip",
		"sha256": plugShaV1,
		"enable": true,
	}, cookies, hdrs)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("plugin URL install v1.0.0 = %d %s", resp.StatusCode, string(raw))
	}

	// Verify live route responds v1.0.0
	resp, raw = doJSONReq(t, http.MethodGet, ts.URL+"/plugins/ext-demo/api/ver", nil, nil, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"1.0.0"`) {
		t.Fatalf("expected live plugin route v1.0.0, got %d %s", resp.StatusCode, string(raw))
	}

	// Duplicate install without overwrite -> 409 Conflict
	resp, raw = doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/plugins/install", map[string]any{
		"url":       cdnBase + "/dl/plugin-ext-demo-v1.0.0.zip",
		"overwrite": false,
	}, cookies, hdrs)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when overwrite=false, got %d %s", resp.StatusCode, string(raw))
	}

	// Install offline-only plugin (capability == "none")
	resp, raw = doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/plugins/install-url", map[string]any{
		"url": cdnBase + "/dl/plugin-no-upd.zip",
	}, cookies, hdrs)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("offline-only plugin URL install = %d %s", resp.StatusCode, string(raw))
	}

	// 2. Check official plugin catalog (?refresh=1) -> reports ext-demo installed=true, has_update=true
	resp, raw = doJSONReq(t, http.MethodGet, ts.URL+"/api/admin/plugins/catalog?refresh=1&locale=en-US", nil, cookies, hdrs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/admin/plugins/catalog = %d %s", resp.StatusCode, string(raw))
	}
	var plugCat struct {
		Items []struct {
			ID          string `json:"id"`
			Installed   bool   `json:"installed"`
			HasUpdate   bool   `json:"has_update"`
			DisplayName string `json:"display_name"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &plugCat); err != nil {
		t.Fatal(err)
	}
	if len(plugCat.Items) != 1 || !plugCat.Items[0].Installed || !plugCat.Items[0].HasUpdate {
		t.Fatalf("unexpected plugin catalog response: %s", string(raw))
	}

	// 3. Batch plugin check-updates -> offline-only is unsupported (1), ext-demo has update 1.1.0 (1)
	resp, raw = doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/plugins/check-updates", map[string]any{
		"force": true,
	}, cookies, hdrs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/plugins/check-updates = %d %s", resp.StatusCode, string(raw))
	}
	var plugBatch struct {
		Summary extpkg.BatchCheckSummary `json:"summary"`
	}
	_ = json.Unmarshal(raw, &plugBatch)
	if plugBatch.Summary.Updates != 1 || plugBatch.Summary.Unsupported != 1 || plugBatch.Summary.Errors != 0 {
		t.Fatalf("unexpected plugin batch summary: %+v (%s)", plugBatch.Summary, string(raw))
	}

	// 4. Online upgrade plugin ext-demo -> v1.1.0 live without downtime
	resp, raw = doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/plugins/ext-demo/update", map[string]any{}, cookies, hdrs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/plugins/ext-demo/update = %d %s", resp.StatusCode, string(raw))
	}
	resp, raw = doJSONReq(t, http.MethodGet, ts.URL+"/plugins/ext-demo/api/ver", nil, nil, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"1.1.0"`) {
		t.Fatalf("expected upgraded live plugin route v1.1.0, got %d %s", resp.StatusCode, string(raw))
	}

	// 5. Theme URL direct install v1.0.0 + activate
	resp, raw = doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/themes/install", map[string]any{
		"url":      cdnBase + "/dl/theme-demo-skin-v1.0.0.zip",
		"sha256":   themeShaV1,
		"activate": true,
	}, cookies, hdrs)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("theme URL install v1.0.0 = %d %s", resp.StatusCode, string(raw))
	}

	// Duplicate theme URL install without overwrite -> 409 Conflict
	resp, raw = doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/themes/install", map[string]any{
		"url":       cdnBase + "/dl/theme-demo-skin-v1.0.0.zip",
		"overwrite": false,
	}, cookies, hdrs)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for duplicate theme install, got %d %s", resp.StatusCode, string(raw))
	}

	// Verify public homepage renders demo-skin v1.0.0
	resp, raw = doJSONReq(t, http.MethodGet, ts.URL+"/", nil, nil, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "skin-v1.0.0") {
		t.Fatalf("expected public home to render skin-v1.0.0, got %d %s", resp.StatusCode, string(raw))
	}

	// 6. Theme catalog + batch check-updates (built-in volo-studio unsupported=1, demo-skin available=1)
	resp, raw = doJSONReq(t, http.MethodGet, ts.URL+"/api/admin/themes/catalog?refresh=1&locale=zh-CN", nil, cookies, hdrs)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "示例主题") {
		t.Fatalf("GET /api/admin/themes/catalog = %d %s", resp.StatusCode, string(raw))
	}

	resp, raw = doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/themes/check-updates", map[string]any{
		"force": true,
	}, cookies, hdrs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/themes/check-updates = %d %s", resp.StatusCode, string(raw))
	}
	var thBatch struct {
		Summary extpkg.BatchCheckSummary `json:"summary"`
	}
	_ = json.Unmarshal(raw, &thBatch)
	if thBatch.Summary.Updates != 1 || thBatch.Summary.Unsupported != 1 || thBatch.Summary.Errors != 0 {
		t.Fatalf("unexpected theme batch summary: %+v (%s)", thBatch.Summary, string(raw))
	}

	// 7. Online upgrade theme demo-skin -> v1.2.0 live on public homepage!
	resp, raw = doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/themes/demo-skin/update", map[string]any{}, cookies, hdrs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/themes/demo-skin/update = %d %s", resp.StatusCode, string(raw))
	}
	resp, raw = doJSONReq(t, http.MethodGet, ts.URL+"/", nil, nil, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "skin-v1.2.0") {
		t.Fatalf("expected upgraded public home to render skin-v1.2.0, got %d %s", resp.StatusCode, string(raw))
	}
}

func TestAdminExtensions_ChecksumURLSanitizationLocalesCatalogAndHealthz(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "data")
	uploadDir := filepath.Join(tmp, "uploads")
	pluginsDir := filepath.Join(tmp, "plugins")
	themesDir := filepath.Join(tmp, "themes")
	localesDir := filepath.Join(tmp, "locales")
	for _, d := range []string{dataDir, uploadDir, pluginsDir, themesDir, localesDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}

	var themeZip, tamperedZip []byte
	var themeSha string
	localePackJSON := []byte(`{
		"meta.code": "fr-FR",
		"meta.name": "Français (fr-FR)",
		"meta.version": "1.0.0",
		"nav.home": "Accueil"
	}`)
	locSum := sha256.Sum256(localePackJSON)
	localeSha := hex.EncodeToString(locSum[:])

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/themes/catalog.json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{
				"repository": "javascript:alert(1)",
				"themes": [{
					"id": "cs-skin",
					"version": "1.0.0",
					"download_url": "http://%s/dl/theme-cs-skin-v1.0.0.zip",
					"checksum_url": "http://%s/dl/SHA256SUMS.txt",
					"release_asset": "theme-cs-skin-v1.0.0.zip",
					"release_url": "javascript:alert(1)",
					"homepage": "javascript:evil()",
					"approx_size_bytes": 4096,
					"name": {"en-US": "Checksum Skin"}
				}]
			}`, r.Host, r.Host)
		case "/locales/catalog.json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{
				"kind": "locale",
				"items": [{
					"id": "fr-FR",
					"version": "1.0.0",
					"download_url": "http://%s/dl/fr-FR.json",
					"checksum_url": "http://%s/dl/SHA256SUMS.txt",
					"homepage": "javascript:bad()",
					"approx_size_bytes": 256,
					"name": {"en-US": "French (fr-FR)"}
				}]
			}`, r.Host, r.Host)
		case "/dl/SHA256SUMS.txt":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = fmt.Fprintf(w, "%s *theme-cs-skin-v1.0.0.zip\n%s  fr-FR.json\n", themeSha, localeSha)
		case "/dl/theme-cs-skin-v1.0.0.zip":
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(themeZip)
		case "/dl/tampered-cs-skin.zip":
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(tamperedZip)
		case "/dl/fr-FR.json":
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write(localePackJSON)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	portPart := strings.TrimPrefix(upstream.URL, "http://127.0.0.1:")
	cdnBase := "http://cdn.example.test:" + portPart

	themeZip, themeSha = buildTestExtZip(t, map[string]string{
		"theme.json": `{"id":"cs-skin","name":{"en-US":"Checksum Skin"},"version":"1.0.0"}`,
		"templates/home.html": `<!doctype html><html><body>checksum-skin-ok</body></html>`,
	})
	tamperedZip, _ = buildTestExtZip(t, map[string]string{
		"theme.json": `{"id":"cs-skin","name":{"en-US":"Tampered Skin"},"version":"1.0.0"}`,
		"templates/home.html": `<!doctype html><html><body>tampered</body></html>`,
	})

	secret := "0123456789abcdef0123456789abcdef-ext-cs-secret"
	pubURL := "http://127.0.0.1:8080"
	adminPath := "/console"
	cfg, err := config.Load(config.CLIOverrides{
		SecretKey:  &secret,
		PublicURL:  &pubURL,
		AdminPath:  &adminPath,
		DataDir:    &dataDir,
		UploadDir:  &uploadDir,
		PluginsDir: &pluginsDir,
		ThemesDir:  &themesDir,
		LocalesDir: &localesDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(dataDir, "volo-ext-cs.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatal(err)
	}
	cat, err := i18n.NewCatalog("en-US", localesDir)
	if err != nil {
		t.Fatal(err)
	}
	settingsMgr := repo.NewSettingsManager(store, cfg.KeyRing, "en-US")
	_, _ = settingsMgr.Reload(ctx)

	dialer := &security.OutboundSafeDialer{
		Timeout:               5 * time.Second,
		AllowLoopbackForTests: true,
		Resolver: func(_ context.Context, host string) ([]netip.Addr, error) {
			if strings.EqualFold(host, "cdn.example.test") {
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			}
			return nil, fmt.Errorf("unexpected host lookup: %s", host)
		},
	}
	pluginMgr := plugin.NewManager(plugin.ManagerOptions{
		Store:          store,
		Keys:           cfg.KeyRing,
		PluginsDir:     pluginsDir,
		DataDir:        dataDir,
		Catalog:        cat,
		HostVersion:    "1.5.0",
		OutboundDialer: dialer,
	})
	if err := pluginMgr.DiscoverAndStart(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pluginMgr.Close(ctx) })

	passports := guest.NewPassportManager(cfg.KeyRing, cfg.PublicURL)
	pow := guest.NewPoWVerifier(cfg.KeyRing)
	rnd := render.Default()
	grd := visibility.NewGuard(cfg.KeyRing, settingsMgr, passports, cat, rnd)
	srch := visibility.NewSearchIndex(store, rnd, 1<<20)
	cnt := content.NewService(content.ServiceOptions{
		Store:          store,
		Keys:           cfg.KeyRing,
		Settings:       settingsMgr,
		Passports:      passports,
		PoW:            pow,
		Guard:          grd,
		Search:         srch,
		Renderer:       rnd,
		Plugins:        pluginMgr,
		Catalog:        cat,
		OutboundDialer: dialer,
	})
	authSvc := auth.NewService(store, cfg.KeyRing, settingsMgr, cfg.PublicURL)
	appSrv, err := server.NewAppServer(server.AppDeps{
		Config:         cfg,
		Store:          store,
		Settings:       settingsMgr,
		Catalog:        cat,
		Auth:           authSvc,
		Passports:      passports,
		PoW:            pow,
		OutboundDialer: dialer,
		Plugins:        pluginMgr,
		Content:        cnt,
		Version:        "1.5.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	appSrv.SetCatalogURLsForTest(cdnBase+"/plugins/catalog.json", cdnBase+"/themes/catalog.json", cdnBase+"/locales/catalog.json")

	ts := httptest.NewServer(appSrv.Handler())
	t.Cleanup(ts.Close)

	// 1. /healthz works even BEFORE setup completion (uninitialized) and returns status="ok", ok=true, ready=true
	hzResp, hzRaw := doJSONReq(t, http.MethodGet, ts.URL+"/healthz", nil, nil, nil)
	if hzResp.StatusCode != http.StatusOK || !strings.Contains(string(hzRaw), `"status":"ok"`) || !strings.Contains(string(hzRaw), `"initialized":false`) {
		t.Fatalf("unexpected pre-setup /healthz: %d %s", hzResp.StatusCode, string(hzRaw))
	}

	// Complete setup
	resp, compRaw := doJSONReq(t, http.MethodPost, ts.URL+"/api/setup/complete", map[string]any{
		"site_title":         "CS Lab",
		"site_subtitle":      "Verify",
		"site_description":   "Suite",
		"default_locale":     "en-US",
		"timezone":           "UTC",
		"accent":             "teal",
		"admin_username":     "admin",
		"admin_email":        "admin@example.com",
		"admin_display_name": "Admin",
		"admin_password":     "Str0ng!Pass#2026",
	}, nil, map[string]string{"Origin": ts.URL})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setup/complete = %d %s", resp.StatusCode, string(compRaw))
	}
	var comp struct {
		CSRFToken string `json:"csrf_token"`
	}
	_ = json.Unmarshal(compRaw, &comp)
	cookies := resp.Cookies()
	hdrs := map[string]string{
		"Origin":         ts.URL,
		"X-Volo-Console": "1",
		"X-CSRF-Token":   comp.CSRFToken,
	}

	// 2. /healthz after setup reports initialized=true
	hzResp, hzRaw = doJSONReq(t, http.MethodGet, ts.URL+"/healthz", nil, nil, nil)
	if hzResp.StatusCode != http.StatusOK || !strings.Contains(string(hzRaw), `"initialized":true`) {
		t.Fatalf("unexpected post-setup /healthz: %d %s", hzResp.StatusCode, string(hzRaw))
	}

	// 3. Themes catalog strips javascript: schemes while preserving checksum_url and approx_size_bytes
	resp, raw := doJSONReq(t, http.MethodGet, ts.URL+"/api/admin/themes/catalog?refresh=1&locale=en-US", nil, cookies, hdrs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/admin/themes/catalog = %d %s", resp.StatusCode, string(raw))
	}
	if strings.Contains(strings.ToLower(string(raw)), "javascript:") {
		t.Fatalf("expected javascript: URLs to be sanitized from catalog response, got %s", string(raw))
	}
	if !strings.Contains(string(raw), `"checksum_url"`) || !strings.Contains(string(raw), `"approx_size_bytes":4096`) {
		t.Fatalf("expected checksum_url and approx_size_bytes in catalog response, got %s", string(raw))
	}

	// 4. Tampered archive rejected via checksum_url auto-verification
	resp, raw = doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/themes/install-url", map[string]any{
		"url":          cdnBase + "/dl/tampered-cs-skin.zip",
		"checksum_url": cdnBase + "/dl/SHA256SUMS.txt",
		"asset_name":   "theme-cs-skin-v1.0.0.zip",
	}, cookies, hdrs)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(strings.ToLower(string(raw)), "checksum") {
		t.Fatalf("expected tampered archive to fail checksum_url verification, got %d %s", resp.StatusCode, string(raw))
	}

	// 5. Authentic theme archive verified via checksum_url and returns appearance_version > 0
	resp, raw = doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/themes/install-url", map[string]any{
		"url":          cdnBase + "/dl/theme-cs-skin-v1.0.0.zip",
		"checksum_url": cdnBase + "/dl/SHA256SUMS.txt",
		"asset_name":   "theme-cs-skin-v1.0.0.zip",
		"activate":     true,
	}, cookies, hdrs)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/admin/themes/install-url = %d %s", resp.StatusCode, string(raw))
	}
	var thInstallRes struct {
		AppearanceVersion int64 `json:"appearance_version"`
	}
	_ = json.Unmarshal(raw, &thInstallRes)
	if thInstallRes.AppearanceVersion <= 0 {
		t.Fatalf("expected appearance_version > 0 in theme install-url response, got %s", string(raw))
	}

	// 6. Locales catalog + URL install (with checksum_url + 409 conflict on duplicate + overwrite=true)
	resp, raw = doJSONReq(t, http.MethodGet, ts.URL+"/api/admin/locales/catalog?refresh=1&locale=en-US", nil, cookies, hdrs)
	if resp.StatusCode != http.StatusOK || strings.Contains(strings.ToLower(string(raw)), "javascript:") || !strings.Contains(string(raw), `"fr-FR"`) {
		t.Fatalf("unexpected GET /api/admin/locales/catalog = %d %s", resp.StatusCode, string(raw))
	}

	resp, raw = doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/locales/install-url", map[string]any{
		"url":          cdnBase + "/dl/fr-FR.json",
		"checksum_url": cdnBase + "/dl/SHA256SUMS.txt",
		"overwrite":    false,
	}, cookies, hdrs)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/admin/locales/install-url = %d %s", resp.StatusCode, string(raw))
	}

	// Duplicate without overwrite -> 409 Conflict
	resp, raw = doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/locales/install-url", map[string]any{
		"url":       cdnBase + "/dl/fr-FR.json",
		"overwrite": false,
	}, cookies, hdrs)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict on duplicate locale URL install, got %d %s", resp.StatusCode, string(raw))
	}

	// Duplicate with overwrite=true -> 201 Created
	resp, raw = doJSONReq(t, http.MethodPost, ts.URL+"/api/admin/locales/install-url", map[string]any{
		"url":          cdnBase + "/dl/fr-FR.json",
		"checksum_url": cdnBase + "/dl/SHA256SUMS.txt",
		"overwrite":    true,
	}, cookies, hdrs)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created on overwrite=true locale URL install, got %d %s", resp.StatusCode, string(raw))
	}

	// Verify /api/v1/i18n/fr-FR serves the newly installed pack immediately
	i18nResp, i18nRaw := doJSONReq(t, http.MethodGet, ts.URL+"/api/v1/i18n/fr-FR", nil, nil, nil)
	if i18nResp.StatusCode != http.StatusOK || !strings.Contains(string(i18nRaw), "Accueil") {
		t.Fatalf("expected /api/v1/i18n/fr-FR to contain Accueil, got %d %s", i18nResp.StatusCode, string(i18nRaw))
	}
}

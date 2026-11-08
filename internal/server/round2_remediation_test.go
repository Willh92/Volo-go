package server_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
	"github.com/Willh92/Volo-go/internal/security"
	"github.com/Willh92/Volo-go/internal/server"
	"github.com/Willh92/Volo-go/internal/visibility"
	"github.com/Willh92/Volo-go/pkg/plugin"
)

type round2Env struct {
	ctx       context.Context
	cfg       *config.Config
	store     *sqlx.Store
	settings  *repo.SettingsManager
	pluginMgr *plugin.Manager
	authSvc   *auth.Service
	content   *content.Service
	app       *server.AppServer
	ts        *httptest.Server
	uploadDir string
}

func setupRound2Env(t *testing.T) (*round2Env, []*http.Cookie, map[string]string, string) {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()

	dataDir := filepath.Join(tmp, "data")
	uploadDir := filepath.Join(tmp, "uploads")
	pluginsDir := filepath.Join(tmp, "plugins")
	themesDir := filepath.Join(tmp, "themes")
	backupDir := filepath.Join(tmp, "backups")
	for _, d := range []string{dataDir, uploadDir, pluginsDir, themesDir, backupDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}

	secret := "0123456789abcdef0123456789abcdef-round2-master-secret"
	pubURL := "https://studio.example.com"
	adminPath := "/console"
	cfg, err := config.Load(config.CLIOverrides{
		SecretKey:  &secret,
		PublicURL:  &pubURL,
		AdminPath:  &adminPath,
		DataDir:    &dataDir,
		UploadDir:  &uploadDir,
		PluginsDir: &pluginsDir,
		ThemesDir:  &themesDir,
		BackupDir:  &backupDir,
	})
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(dataDir, "volo-r2.db"),
	})
	if err != nil {
		t.Fatalf("sqlx.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatalf("migrate.Up: %v", err)
	}

	cat, err := i18n.NewCatalog("en", "")
	if err != nil {
		t.Fatal(err)
	}
	settingsMgr := repo.NewSettingsManager(store, cfg.KeyRing, "en")
	if _, err := settingsMgr.Reload(ctx); err != nil {
		t.Fatal(err)
	}

	dialer := &security.OutboundSafeDialer{
		Timeout:               5 * time.Second,
		AllowLoopbackForTests: true,
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

	appSrv, err := server.NewAppServer(server.AppDeps{
		Config:               cfg,
		Store:                store,
		Settings:             settingsMgr,
		Catalog:              cat,
		Auth:                 authSvc,
		Passports:            passports,
		PoW:                  pow,
		OutboundDialer:       dialer,
		Plugins:              pluginMgr,
		Content:              cnt,
		SendVerificationMail: disp.SendVerificationCode,
		NotifyCommentCreated: disp.NotifyCommentCreated,
		TestSMTP:             disp.SendTestSMTP,
		TestWebhook:          disp.TriggerTestWebhook,
	})
	if err != nil {
		t.Fatalf("NewAppServer: %v", err)
	}
	ts := httptest.NewServer(appSrv.Handler())
	t.Cleanup(ts.Close)

	resp, compRaw := doJSONReq(t, http.MethodPost, ts.URL+"/api/setup/complete", map[string]any{
		"site_title":         "Round2 Studio",
		"site_subtitle":      "Hardened",
		"site_description":   "Round 2 Suite",
		"default_locale":     "en",
		"timezone":           "UTC",
		"accent":             "teal",
		"admin_username":     "captain",
		"admin_email":        "captain@example.com",
		"admin_display_name": "Captain Volo",
		"admin_password":     "Str0ng!Pass#2026",
	}, nil, map[string]string{"Origin": ts.URL})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setup/complete = %d %s", resp.StatusCode, string(compRaw))
	}
	var comp struct {
		CSRFToken string      `json:"csrf_token"`
		User      domain.User `json:"user"`
	}
	if err := json.Unmarshal(compRaw, &comp); err != nil {
		t.Fatal(err)
	}
	hdrs := map[string]string{
		auth.CSRFHeaderName: comp.CSRFToken,
		"Origin":            ts.URL,
	}
	return &round2Env{
		ctx:       ctx,
		cfg:       cfg,
		store:     store,
		settings:  settingsMgr,
		pluginMgr: pluginMgr,
		authSvc:   authSvc,
		content:   cnt,
		app:       appSrv,
		ts:        ts,
		uploadDir: uploadDir,
	}, resp.Cookies(), hdrs, comp.User.ID
}

type round2TestPlugin struct {
	plugin.BaseBuiltinPlugin
	id      string
	mu      sync.Mutex
	hooks   []string
	lastReq plugin.APIRequest
	hostKV  *plugin.KVStore
}

func (p *round2TestPlugin) Manifest() plugin.Manifest {
	return plugin.Manifest{
		ID:      p.id,
		Version: "1.0.0",
		Runtime: plugin.RuntimeBuiltin,
		Name:    map[string]string{"en": "Round2 Probe"},
		Permissions: []string{
			"http:routes",
			"http:raw_headers",
			"hooks:*",
			"kv",
			"provider:oidc",
			"ui:slots",
		},
		Hooks: []string{
			plugin.HookArticlePublished,
			plugin.HookArticleUpdated,
			plugin.HookArticleDeleted,
			plugin.HookPagePublished,
			plugin.HookPageUpdated,
			plugin.HookPageDeleted,
			plugin.HookCommentModerated,
		},
		UISlots: []plugin.SlotSpec{
			{Slot: plugin.SlotCommentBefore, Dynamic: true},
			{Slot: plugin.SlotCommentAfter, Dynamic: true},
		},
	}
}

func (p *round2TestPlugin) OnEnable(_ context.Context, host *plugin.HostContext) error {
	p.mu.Lock()
	p.hostKV = host.KV
	p.mu.Unlock()
	return nil
}

func (p *round2TestPlugin) OnHook(_ context.Context, hookName string, payload any) (any, error) {
	p.mu.Lock()
	p.hooks = append(p.hooks, hookName)
	p.mu.Unlock()
	return payload, nil
}

func (p *round2TestPlugin) RenderSlot(_ context.Context, slotName string, _ plugin.SlotRenderContext) (string, error) {
	switch slotName {
	case plugin.SlotCommentBefore:
		return `<div class="probe-comment-before">BeforeSlot</div>`, nil
	case plugin.SlotCommentAfter:
		return `<div class="probe-comment-after">AfterSlot</div>`, nil
	default:
		return "", nil
	}
}

func (p *round2TestPlugin) ServeAPI(_ context.Context, req plugin.APIRequest) (plugin.APIResponse, error) {
	p.mu.Lock()
	p.lastReq = req
	p.mu.Unlock()
	b, _ := json.Marshal(map[string]any{
		"is_admin": req.IsAdmin,
		"headers":  req.Headers,
	})
	return plugin.APIResponse{
		Status:      http.StatusOK,
		ContentType: "application/json",
		Body:        string(b),
	}, nil
}

func (p *round2TestPlugin) snapshotHooks() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.hooks))
	copy(out, p.hooks)
	return out
}

func (p *round2TestPlugin) resetHooks() {
	p.mu.Lock()
	p.hooks = nil
	p.mu.Unlock()
}

type mockOIDCProvider struct {
	name string
}

func (m *mockOIDCProvider) Name() string               { return m.name }
func (m *mockOIDCProvider) DisplayName(_ string) string { return "GitHub SSO" }
func (m *mockOIDCProvider) AuthCodeURL(state string, redirectURI string) (string, error) {
	return "https://idp.example.com/authorize?state=" + url.QueryEscape(state) + "&redirect_uri=" + url.QueryEscape(redirectURI), nil
}
func (m *mockOIDCProvider) Exchange(_ context.Context, code string, _ string) (plugin.OIDCIdentity, error) {
	switch code {
	case "admin-ok":
		return plugin.OIDCIdentity{
			Provider:    m.name,
			Subject:     "sub_admin",
			Email:       "CAPTAIN@Example.com",
			DisplayName: "Captain OIDC",
		}, nil
	case "stranger":
		return plugin.OIDCIdentity{
			Provider:    m.name,
			Subject:     "sub_stranger",
			Email:       "stranger@example.com",
			DisplayName: "Stranger",
		}, nil
	default:
		return plugin.OIDCIdentity{
			Provider:    m.name,
			Subject:     "sub_reader_1",
			Email:       "sso-reader@example.com",
			DisplayName: "SSO Reader",
		}, nil
	}
}

func TestRound2_RBAC_OverviewScoping_And_EditorPageCommentModeration(t *testing.T) {
	env, adminCookies, adminHdrs, adminID := setupRound2Env(t)

	// Create an editor account
	resp, uRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/users", map[string]any{
		"username":     "editor1",
		"email":        "editor1@example.com",
		"display_name": "Editor One",
		"role":         "editor",
		"password":     "Ed1t0r!Pass#2026",
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("create editor = %d %s", resp.StatusCode, string(uRaw))
	}

	// Log in as editor (with valid Origin)
	resp, loginRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/auth/login", map[string]any{
		"ident":    "editor1",
		"password": "Ed1t0r!Pass#2026",
	}, nil, map[string]string{"Origin": env.ts.URL})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("editor login = %d %s", resp.StatusCode, string(loginRaw))
	}
	var edLogin struct {
		CSRFToken string      `json:"csrf_token"`
		User      domain.User `json:"user"`
	}
	_ = json.Unmarshal(loginRaw, &edLogin)
	edCookies := resp.Cookies()
	edHdrs := map[string]string{
		auth.CSRFHeaderName: edLogin.CSRFToken,
		"Origin":            env.ts.URL,
	}

	// 1. RBAC: editor blocked from POST categories, tags, series, media/reconcile
	for _, ep := range []struct {
		path string
		body map[string]any
	}{
		{"/api/admin/categories", map[string]any{"name": "BlockedCat", "slug": "blocked-cat"}},
		{"/api/admin/tags", map[string]any{"name": "BlockedTag", "slug": "blocked-tag"}},
		{"/api/admin/series", map[string]any{"title": "BlockedSeries", "slug": "blocked-series"}},
		{"/api/admin/media/reconcile", map[string]any{}},
	} {
		r, raw := doJSONReq(t, http.MethodPost, env.ts.URL+ep.path, ep.body, edCookies, edHdrs)
		if r.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403 for editor POST %s, got %d: %s", ep.path, r.StatusCode, string(raw))
		}
	}

	// 2. Admin creates an article + standalone page; Editor creates own article + standalone page
	resp, raw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"title": "Admin Post", "slug": "admin-post", "content_md": "admin body", "status": "published", "allow_comment": true,
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("admin create article = %d %s", resp.StatusCode, string(raw))
	}
	resp, adminPageRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/pages", map[string]any{
		"title": "Admin About", "slug": "admin-about", "content_md": "admin page", "is_published": true, "allow_comment": true,
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("admin create page = %d %s", resp.StatusCode, string(adminPageRaw))
	}
	var adminPageWrap struct {
		Page domain.Page `json:"page"`
	}
	_ = json.Unmarshal(adminPageRaw, &adminPageWrap)
	adminPage := adminPageWrap.Page

	resp, raw = doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"title": "Editor Post", "slug": "editor-post", "content_md": "editor body", "status": "published", "allow_comment": true,
	}, edCookies, edHdrs)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("editor create article = %d %s", resp.StatusCode, string(raw))
	}
	resp, edPageRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/pages", map[string]any{
		"title": "Editor Portfolio", "slug": "editor-portfolio", "template": "wide", "content_md": "editor page", "is_published": true, "allow_comment": true,
	}, edCookies, edHdrs)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("editor create page = %d %s", resp.StatusCode, string(edPageRaw))
	}
	var edPageWrap struct {
		Page domain.Page `json:"page"`
	}
	_ = json.Unmarshal(edPageRaw, &edPageWrap)
	edPage := edPageWrap.Page
	if edPage.Template != "wide" {
		t.Fatalf("expected persisted page template 'wide', got %q", edPage.Template)
	}
	_ = adminID

	// 3. Overview scoping: editor sees total_articles == 1 (only own), admin sees 2
	resp, ovRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/overview", nil, edCookies, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("editor overview = %d %s", resp.StatusCode, string(ovRaw))
	}
	var edOv map[string]any
	_ = json.Unmarshal(ovRaw, &edOv)
	statsMap, _ := edOv["stats"].(map[string]any)
	if statsMap == nil {
		statsMap = edOv
	}
	if gotArts, ok := statsMap["total_articles"].(float64); ok && int(gotArts) != 1 {
		t.Fatalf("expected editor scoped total_articles=1, got %v (payload=%s)", gotArts, string(ovRaw))
	}

	// 4. Page comment visibility & moderation for editor: seed 1 comment on adminPage and 1 on edPage
	now := time.Now().UnixMilli()
	_, err := env.store.ExecContext(env.ctx, `INSERT INTO comments (
		id, target_type, target_id, parent_id, reply_to_name, user_id, guest_id_hash, env_fp_hash,
		author_name, author_url, avatar_url, notify_email_ciphertext, notify_locale,
		client_ip, ip_masked, geo_region, content_md, status, is_deleted, created_at, updated_at
	) VALUES
		('cmt_admin_pg', 'page', ?, '', '', '', 'gh1', '', 'GuestA', '', '', '', 'en', '1.2.3.4', '1.2.3.*', 'US', 'On admin page', 'pending', ?, ?, ?),
		('cmt_ed_pg', 'page', ?, '', '', '', 'gh2', '', 'GuestB', '', '', '', 'en', '5.6.7.8', '5.6.7.*', 'US', 'On editor page', 'pending', ?, ?, ?)`,
		adminPage.ID, false, now, now, edPage.ID, false, now, now)
	if err != nil {
		t.Fatalf("seed page comments: %v", err)
	}

	resp, cListRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/comments", nil, edCookies, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("editor GET /api/admin/comments = %d %s", resp.StatusCode, string(cListRaw))
	}
	var cList struct {
		Total int              `json:"total"`
		Items []map[string]any `json:"items"`
	}
	_ = json.Unmarshal(cListRaw, &cList)
	if cList.Total != 1 || len(cList.Items) != 1 || cList.Items[0]["id"] != "cmt_ed_pg" {
		t.Fatalf("expected editor to see exactly 1 own-page comment (total==len(items)==1), got total=%d items=%+v", cList.Total, cList.Items)
	}
	if rawIP, _ := cList.Items[0]["client_ip"].(string); rawIP != "" {
		t.Fatalf("expected raw PII client_ip scrubbed for editor, got %q", rawIP)
	}

	// Editor can moderate comment on own page (200), but gets 403 on admin's page comment
	resp, modRaw := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/comments/cmt_ed_pg", map[string]any{"status": "approved"}, edCookies, edHdrs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected editor to approve own page comment, got %d: %s", resp.StatusCode, string(modRaw))
	}
	resp, _ = doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/comments/cmt_admin_pg", map[string]any{"status": "approved"}, edCookies, edHdrs)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 when editor moderates another author's page comment, got %d", resp.StatusCode)
	}
}

func TestRound2_OriginScheme_PluginCSRF_StepUpAuth_SessionRevoke_And_LogoutRedirect(t *testing.T) {
	env, adminCookies, adminHdrs, _ := setupRound2Env(t)

	// 1. Cross-origin & scheme-mismatch rejection on /api/admin/auth/login
	resp, _ := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/auth/login", map[string]any{
		"identifier": "captain",
		"password":   "Str0ng!Pass#2026",
	}, nil, map[string]string{"Origin": "https://evil.example.org"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected cross-origin login rejected with 403, got %d", resp.StatusCode)
	}

	// Scheme mismatch: httptest.Server is plain http://127.0.0.1:port, so https://127.0.0.1:port has mismatched scheme!
	httpsOrigin := strings.Replace(env.ts.URL, "http://", "https://", 1)
	resp, _ = doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/auth/login", map[string]any{
		"identifier": "captain",
		"password":   "Str0ng!Pass#2026",
	}, nil, map[string]string{"Origin": httpsOrigin})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected scheme-mismatched Origin (%s vs %s) rejected with 403, got %d", httpsOrigin, env.ts.URL, resp.StatusCode)
	}

	// 2. Plugin API CSRF + http:raw_headers webhook signature passthrough
	probe := &round2TestPlugin{id: "r2-probe"}
	if err := env.pluginMgr.RegisterBuiltin(env.ctx, probe); err != nil {
		t.Fatalf("RegisterBuiltin: %v", err)
	}
	if err := env.pluginMgr.Enable(env.ctx, probe.id); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	// Case A: External third-party webhook POST without session/CSRF still reaches ServeAPI with IsAdmin=false & signature header
	resp, whRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/plugins/r2-probe/api/webhook", map[string]any{"event": "ping"}, nil, map[string]string{
		"X-Hub-Signature-256": "sha256=deadbeef",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook POST /plugins/r2-probe/api/webhook = %d %s", resp.StatusCode, string(whRaw))
	}
	var whRes struct {
		IsAdmin bool              `json:"is_admin"`
		Headers map[string]string `json:"headers"`
	}
	_ = json.Unmarshal(whRaw, &whRes)
	if whRes.IsAdmin {
		t.Fatalf("expected external webhook IsAdmin=false")
	}
	if whRes.Headers["x-hub-signature-256"] != "sha256=deadbeef" {
		t.Fatalf("expected http:raw_headers to forward x-hub-signature-256, got %+v", whRes.Headers)
	}

	// Case B: Browser POST carrying admin cookie WITHOUT X-CSRF-Token must NOT receive IsAdmin=true!
	resp, noCSRFRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/plugins/r2-probe/api/webhook", map[string]any{"event": "forged"}, adminCookies, map[string]string{
		"Origin": env.ts.URL,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cookie-only POST /plugins/r2-probe/api/webhook = %d", resp.StatusCode)
	}
	_ = json.Unmarshal(noCSRFRaw, &whRes)
	if whRes.IsAdmin {
		t.Fatalf("CSRF bypass: POST with session cookie but missing X-CSRF-Token received IsAdmin=true!")
	}

	// Case C: Admin POST WITH valid Origin + valid X-CSRF-Token receives IsAdmin=true
	resp, csrfOKRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/plugins/r2-probe/api/webhook", map[string]any{"event": "admin-op"}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated POST /plugins/r2-probe/api/webhook = %d", resp.StatusCode)
	}
	_ = json.Unmarshal(csrfOKRaw, &whRes)
	if !whRes.IsAdmin {
		t.Fatalf("expected valid Origin + X-CSRF-Token admin POST to receive IsAdmin=true")
	}

	// 3. Step-up password verification on TOTP enroll & Passkey finish
	resp, totpRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/security/totp/enroll", map[string]any{
		"code":             "123456",
		"current_password": "WrongPassword!",
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(totpRaw), "current_password") {
		t.Fatalf("expected 403 current_password check on totp/enroll, got %d: %s", resp.StatusCode, string(totpRaw))
	}

	resp, pkRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/security/passkeys/finish", map[string]any{
		"name":             "Key",
		"current_password": "",
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(pkRaw), "current_password") {
		t.Fatalf("expected 403 current_password check on passkeys/finish, got %d: %s", resp.StatusCode, string(pkRaw))
	}

	// 4. DELETE /api/admin/security/sessions revokes other sessions while keeping caller session alive
	resp2, _ := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/auth/login", map[string]any{
		"ident":    "captain",
		"password": "Str0ng!Pass#2026",
	}, nil, map[string]string{"Origin": env.ts.URL})
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second admin login failed: %d", resp2.StatusCode)
	}
	secondCookies := resp2.Cookies()

	resp, revRaw := doJSONReq(t, http.MethodDelete, env.ts.URL+"/api/admin/security/sessions", nil, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /api/admin/security/sessions = %d %s", resp.StatusCode, string(revRaw))
	}
	var revRes struct {
		OK      bool  `json:"ok"`
		Revoked int64 `json:"revoked"`
	}
	_ = json.Unmarshal(revRaw, &revRes)
	if !revRes.OK || revRes.Revoked < 1 {
		t.Fatalf("expected revoked >= 1, got %+v", revRes)
	}
	// Second session must now fail /api/admin/me, while primary session still succeeds
	chk2, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/me", nil, secondCookies, nil)
	if chk2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected revoked second session to get 401, got %d", chk2.StatusCode)
	}
	chk1, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/me", nil, adminCookies, nil)
	if chk1.StatusCode != http.StatusOK {
		t.Fatalf("expected current session to remain valid (200), got %d", chk1.StatusCode)
	}

	// 5. /logout open-redirect hardening against /\evil.com
	logoutResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+`/logout?next=/\evil.example.com`, nil, nil, nil)
	if loc := logoutResp.Header.Get("Location"); loc != "/" {
		t.Fatalf("expected /logout?next=/\\evil.example.com to sanitize Location to '/', got %q", loc)
	}
}

func TestRound2_CacheControl_Vary_Feeds_And_OIDCSSO(t *testing.T) {
	env, adminCookies, adminHdrs, _ := setupRound2Env(t)

	// Configure mobile_theme so Vary includes User-Agent & Sec-CH-UA-Mobile
	resp, raw := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/appearance", map[string]any{
		"active_theme": "volo-studio",
		"mobile_theme": "volo-studio",
		"default_mode": "auto",
		"accent":       "teal",
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH appearance = %d %s", resp.StatusCode, string(raw))
	}

	// 1. Anonymous GET / sets Vary tokens cleanly even under dynamic Gzip
	resp, _ = doJSONReq(t, http.MethodGet, env.ts.URL+"/", nil, nil, map[string]string{"Accept-Encoding": "gzip"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d", resp.StatusCode)
	}
	vary := resp.Header.Get("Vary")
	for _, tok := range []string{"Accept-Encoding", "Cookie", "Accept-Language", "User-Agent", "Sec-CH-UA-Mobile"} {
		if !strings.Contains(vary, tok) {
			t.Fatalf("expected Vary %q to contain %q", vary, tok)
		}
	}

	// 2. Logged-in Viewer OR ?st= forces Cache-Control: private, no-store
	respViewer, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/", nil, adminCookies, nil)
	if cc := respViewer.Header.Get("Cache-Control"); !strings.Contains(cc, "private") || !strings.Contains(cc, "no-store") {
		t.Fatalf("expected authenticated HTML response Cache-Control: private, no-store, got %q", cc)
	}
	respST, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/?st=tok123", nil, nil, nil)
	if cc := respST.Header.Get("Cache-Control"); !strings.Contains(cc, "private") || !strings.Contains(cc, "no-store") {
		t.Fatalf("expected ?st= HTML response Cache-Control: private, no-store, got %q", cc)
	}

	// 3. RSS, Atom, and JSON feeds include Vary: Accept-Encoding
	for _, feedPath := range []string{"/rss.xml", "/atom.xml", "/feed.json"} {
		fResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+feedPath, nil, nil, map[string]string{"Accept-Encoding": "gzip"})
		if fResp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d", feedPath, fResp.StatusCode)
		}
		if !strings.Contains(fResp.Header.Get("Vary"), "Accept-Encoding") {
			t.Fatalf("expected Vary: Accept-Encoding on %s, got %q", feedPath, fResp.Header.Get("Vary"))
		}
	}

	// 4. OIDC Public & Admin SSO Routes
	probe := &round2TestPlugin{id: "oidc-plugin"}
	if err := env.pluginMgr.RegisterBuiltin(env.ctx, probe); err != nil {
		t.Fatalf("RegisterBuiltin: %v", err)
	}
	if err := env.pluginMgr.Enable(env.ctx, probe.id); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	env.pluginMgr.Providers().RegisterOIDC(probe.id, &mockOIDCProvider{name: "github"})

	// List providers
	resp, provRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/auth/oidc/providers?target=reader&next=/archives", nil, nil, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(provRaw), "GitHub SSO") {
		t.Fatalf("GET /api/v1/auth/oidc/providers = %d %s", resp.StatusCode, string(provRaw))
	}

	// Verify /reader/login page populates OIDCProviders
	resp, loginHTML := doJSONReq(t, http.MethodGet, env.ts.URL+"/reader/login", nil, nil, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(loginHTML), "/api/v1/auth/oidc/github/start") {
		t.Fatalf("expected /reader/login to render OIDC button, status=%d", resp.StatusCode)
	}

	// Reject /\ open redirect in OIDC start
	badStart, _ := doJSONReq(t, http.MethodGet, env.ts.URL+`/api/v1/auth/oidc/github/start?target=reader&next=/\evil.com`, nil, nil, nil)
	if badStart.StatusCode == http.StatusFound {
		loc := badStart.Header.Get("Location")
		u, _ := url.Parse(loc)
		stateTok := u.Query().Get("state")
		cbResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/auth/oidc/github/callback?code=reader-ok&state="+url.QueryEscape(stateTok), nil, badStart.Cookies(), nil)
		if cbLoc := cbResp.Header.Get("Location"); strings.HasPrefix(cbLoc, `/\`) || strings.Contains(cbLoc, "evil.com") {
			t.Fatalf("OIDC open redirect leaked unsafe next=%q", cbLoc)
		}
	}

	// Reader SSO flow: start -> callback -> auto-provisions active reader & redirects to /archives
	startResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/auth/oidc/github/start?target=reader&next=/archives", nil, nil, nil)
	if startResp.StatusCode != http.StatusFound {
		t.Fatalf("expected 302 from OIDC start, got %d", startResp.StatusCode)
	}
	idpURL, err := url.Parse(startResp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse idp redirect: %v", err)
	}
	nonce := idpURL.Query().Get("state")
	cbResp, cbBody := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/auth/oidc/github/callback?code=reader-ok&state="+url.QueryEscape(nonce), nil, startResp.Cookies(), nil)
	if cbResp.StatusCode != http.StatusFound || cbResp.Header.Get("Location") != "/archives" {
		t.Fatalf("OIDC reader callback = %d loc=%q body=%s", cbResp.StatusCode, cbResp.Header.Get("Location"), string(cbBody))
	}
	var readerUser domain.User
	readerUser, err = sqlx.Get[domain.User](env.ctx, env.store, `SELECT * FROM users WHERE email = 'sso-reader@example.com'`)
	if err != nil || readerUser.Role != domain.RoleReader || readerUser.EmailVerifiedAt <= 0 {
		t.Fatalf("expected auto-provisioned verified reader user, got err=%v user=%+v", err, readerUser)
	}

	// Admin SSO flow: unknown email -> 403; matching active staff email -> 302 with session cookie
	admStart1, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/auth/oidc/github/start?target=admin&next=/console", nil, nil, nil)
	admURL1, _ := url.Parse(admStart1.Header.Get("Location"))
	admCB1, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/auth/oidc/github/callback?code=stranger&state="+url.QueryEscape(admURL1.Query().Get("state")), nil, admStart1.Cookies(), nil)
	if admCB1.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for unmatched admin OIDC email, got %d", admCB1.StatusCode)
	}

	admStart2, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/auth/oidc/github/start?target=admin&next=/console", nil, nil, nil)
	admURL2, _ := url.Parse(admStart2.Header.Get("Location"))
	admCB2, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/auth/oidc/github/callback?code=admin-ok&state="+url.QueryEscape(admURL2.Query().Get("state")), nil, admStart2.Cookies(), nil)
	if admCB2.StatusCode != http.StatusFound || admCB2.Header.Get("Location") != "/console" {
		t.Fatalf("expected 302 to /console for matched admin OIDC email, got %d loc=%q", admCB2.StatusCode, admCB2.Header.Get("Location"))
	}
	meResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/me", nil, admCB2.Cookies(), nil)
	if meResp.StatusCode != http.StatusOK {
		t.Fatalf("expected OIDC admin session cookie to authenticate /api/admin/me, got %d", meResp.StatusCode)
	}
}

func TestRound2_LifecycleHooks_AdminOps_MediaEXIF_WebhooksReplay_And_BackupTicket(t *testing.T) {
	env, adminCookies, adminHdrs, _ := setupRound2Env(t)

	probe := &round2TestPlugin{id: "ops-probe"}
	if err := env.pluginMgr.RegisterBuiltin(env.ctx, probe); err != nil {
		t.Fatalf("RegisterBuiltin: %v", err)
	}
	if err := env.pluginMgr.Enable(env.ctx, probe.id); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	// 1. Accurate Article & Page Lifecycle Hooks + SlotCommentBefore/SlotCommentAfter
	// Saving & updating an unpublished draft MUST NOT fire article.published or article.updated!
	resp, artRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"title": "Draft Idea", "slug": "draft-idea", "content_md": "wip", "status": "draft",
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create draft = %d %s", resp.StatusCode, string(artRaw))
	}
	var draftArtWrap struct {
		Article domain.RawArticle `json:"article"`
	}
	_ = json.Unmarshal(artRaw, &draftArtWrap)
	draftArt := draftArtWrap.Article

	resp, _ = doJSONReq(t, http.MethodPut, env.ts.URL+"/api/admin/articles/"+draftArt.ID, map[string]any{
		"title": "Draft Idea v2", "slug": "draft-idea", "content_md": "wip v2", "status": "draft",
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update draft = %d", resp.StatusCode)
	}
	if hooks := probe.snapshotHooks(); len(hooks) != 0 {
		t.Fatalf("expected ZERO lifecycle hooks when creating/updating draft, got %v", hooks)
	}

	// Publish -> update published -> delete article
	resp, _ = doJSONReq(t, http.MethodPut, env.ts.URL+"/api/admin/articles/"+draftArt.ID, map[string]any{
		"title": "Published Idea", "slug": "draft-idea", "content_md": "live", "status": "published", "allow_comment": true,
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("publish article = %d", resp.StatusCode)
	}
	// Verify comment.before & comment.after slots render on public article HTML
	pubArtResp, pubArtHTML := doJSONReq(t, http.MethodGet, env.ts.URL+"/posts/draft-idea", nil, nil, nil)
	if pubArtResp.StatusCode != http.StatusOK || !strings.Contains(string(pubArtHTML), "probe-comment-before") || !strings.Contains(string(pubArtHTML), "probe-comment-after") {
		t.Fatalf("expected comment.before and comment.after widgets on /posts/draft-idea, status=%d", pubArtResp.StatusCode)
	}

	_, _ = doJSONReq(t, http.MethodPut, env.ts.URL+"/api/admin/articles/"+draftArt.ID, map[string]any{
		"title": "Published Idea v2", "slug": "draft-idea", "content_md": "live 2", "status": "published", "allow_comment": true,
	}, adminCookies, adminHdrs)
	_, _ = doJSONReq(t, http.MethodDelete, env.ts.URL+"/api/admin/articles/"+draftArt.ID, nil, adminCookies, adminHdrs)

	// Create -> update -> delete published standalone page
	resp, pgRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/pages", map[string]any{
		"title": "Now Page", "slug": "now-page", "template": "minimal", "content_md": "hello page", "is_published": true, "allow_comment": true,
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create page = %d %s", resp.StatusCode, string(pgRaw))
	}
	var pgWrap struct {
		Page domain.Page `json:"page"`
	}
	_ = json.Unmarshal(pgRaw, &pgWrap)
	pg := pgWrap.Page
	// Public standalone page renders cleanly via fallback chain ["page-now-page.html", "page-minimal.html", "page.html"] + comment slots
	pubPgResp, pubPgHTML := doJSONReq(t, http.MethodGet, env.ts.URL+"/now-page", nil, nil, nil)
	if pubPgResp.StatusCode != http.StatusOK || !strings.Contains(string(pubPgHTML), "probe-comment-before") {
		t.Fatalf("expected /now-page 200 with comment slots, got %d", pubPgResp.StatusCode)
	}
	_, _ = doJSONReq(t, http.MethodPut, env.ts.URL+"/api/admin/pages/"+pg.ID, map[string]any{
		"title": "Now Page v2", "slug": "now-page", "template": "minimal", "content_md": "hello v2", "is_published": true, "allow_comment": true,
	}, adminCookies, adminHdrs)
	_, _ = doJSONReq(t, http.MethodDelete, env.ts.URL+"/api/admin/pages/"+pg.ID, nil, adminCookies, adminHdrs)

	gotHooks := strings.Join(probe.snapshotHooks(), ",")
	for _, wantHook := range []string{
		plugin.HookArticlePublished,
		plugin.HookArticleUpdated,
		plugin.HookArticleDeleted,
		plugin.HookPagePublished,
		plugin.HookPageUpdated,
		plugin.HookPageDeleted,
	} {
		if !strings.Contains(gotHooks, wantHook) {
			t.Fatalf("expected hook %q in recorded hooks %q", wantHook, gotHooks)
		}
	}

	// 2. Tags SQL pagination/unused filter + POST /api/admin/tags/merge
	_, t1Raw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/tags", map[string]any{"name": "Golang", "slug": "golang"}, adminCookies, adminHdrs)
	_, t2Raw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/tags", map[string]any{"name": "GoLangDup", "slug": "golang-dup"}, adminCookies, adminHdrs)
	var t1Wrap, t2Wrap struct {
		Tag domain.Tag `json:"tag"`
	}
	_ = json.Unmarshal(t1Raw, &t1Wrap)
	_ = json.Unmarshal(t2Raw, &t2Wrap)
	tagTarget, tagSource := t1Wrap.Tag, t2Wrap.Tag

	_, taggedArtRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"title": "Tagged Post", "slug": "tagged-post", "content_md": "tagged", "status": "published", "tags": []string{tagSource.Name},
	}, adminCookies, adminHdrs)
	var taggedArtWrap struct {
		Article domain.RawArticle `json:"article"`
	}
	_ = json.Unmarshal(taggedArtRaw, &taggedArtWrap)
	taggedArt := taggedArtWrap.Article

	// Unused tags should contain tagTarget (0 articles), not tagSource (1 article)
	_, unusedRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/tags?unused=1&q=Go", nil, adminCookies, nil)
	if !strings.Contains(string(unusedRaw), tagTarget.ID) || strings.Contains(string(unusedRaw), tagSource.ID) {
		t.Fatalf("unexpected unused=1 tags response: %s", string(unusedRaw))
	}

	// Merge tagSource -> tagTarget
	mResp, mRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/tags/merge", map[string]any{
		"source_id": tagSource.ID,
		"target_id": tagTarget.ID,
	}, adminCookies, adminHdrs)
	if mResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/tags/merge = %d %s", mResp.StatusCode, string(mRaw))
	}
	var mergedCnt int
	_ = env.store.QueryRowContext(env.ctx, `SELECT COUNT(*) FROM article_tags WHERE article_id = ? AND tag_id = ?`, taggedArt.ID, tagTarget.ID).Scan(&mergedCnt)
	if mergedCnt != 1 {
		t.Fatalf("expected article_tags rebound to target tag after merge, got %d", mergedCnt)
	}

	// 3. Links single check, Follows OPML import/export/sync, Webhook deliveries & replay via local receiver
	var webhookHits int
	var gotWebhookSig string
	extSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/friend":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("<html>ok</html>"))
		case "/feed.xml":
			w.Header().Set("Content-Type", "application/rss+xml")
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><rss version="2.0"><channel><title>Friend Feed</title><item><title>Remote Post 1</title><link>https://friend.example.com/p1</link><guid>p1</guid></item></channel></rss>`))
		case "/webhook":
			webhookHits++
			gotWebhookSig = r.Header.Get("X-Volo-Signature")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"received":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(extSrv.Close)

	// Friend link create + single check
	_, lkRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/links", map[string]any{
		"title": "Friend Site", "url": extSrv.URL + "/friend", "category_name": "Tech",
	}, adminCookies, adminHdrs)
	var lkWrap struct {
		Link domain.Link `json:"link"`
	}
	_ = json.Unmarshal(lkRaw, &lkWrap)
	lk := lkWrap.Link
	chkResp, chkRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/links/"+lk.ID+"/check", nil, adminCookies, adminHdrs)
	if chkResp.StatusCode != http.StatusOK || !strings.Contains(string(chkRaw), `"ok"`) {
		t.Fatalf("POST /api/admin/links/{id}/check = %d %s", chkResp.StatusCode, string(chkRaw))
	}

	// OPML import -> export -> single sync
	opmlDoc := `<?xml version="1.0" encoding="UTF-8"?><opml version="2.0"><body><outline text="Blogs"><outline type="rss" text="Friend RSS" xmlUrl="` + extSrv.URL + `/feed.xml" htmlUrl="https://friend.example.com"/></outline></body></opml>`
	impResp, impRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/follows/import-opml", map[string]any{"opml": opmlDoc}, adminCookies, adminHdrs)
	if impResp.StatusCode != http.StatusOK {
		t.Fatalf("import-opml = %d %s", impResp.StatusCode, string(impRaw))
	}
	expResp, expRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/follows/export.opml", nil, adminCookies, nil)
	if expResp.StatusCode != http.StatusOK || !strings.Contains(string(expRaw), extSrv.URL+"/feed.xml") {
		t.Fatalf("export.opml = %d %s", expResp.StatusCode, string(expRaw))
	}
	fwRow, err := sqlx.Get[domain.Follow](env.ctx, env.store, `SELECT * FROM follows LIMIT 1`)
	if err != nil {
		t.Fatalf("load imported follow: %v", err)
	}
	syncResp, syncRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/follows/"+fwRow.ID+"/sync", nil, adminCookies, adminHdrs)
	if syncResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/follows/{id}/sync = %d %s", syncResp.StatusCode, string(syncRaw))
	}

	// Redirect self-loop validation + CSV import/export
	for _, badTarget := range []string{"/same-path", "/same-path?utm=1", "https://studio.example.com/same-path"} {
		loopResp, _ := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/redirects", map[string]any{
			"source_path": "/same-path",
			"target_path": badTarget,
			"status_code": 301,
		}, adminCookies, adminHdrs)
		if loopResp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 for redirect self-loop /same-path -> %s, got %d", badTarget, loopResp.StatusCode)
		}
	}
	rImpResp, rImpRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/redirects/import", map[string]any{
		"format": "csv",
		"data":   "source_path,target_path,status_code\n/old-url,/new-url,301\n",
	}, adminCookies, adminHdrs)
	if rImpResp.StatusCode != http.StatusOK {
		t.Fatalf("redirects/import = %d %s", rImpResp.StatusCode, string(rImpRaw))
	}
	rExpResp, rExpRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/redirects/export?format=csv", nil, adminCookies, nil)
	if rExpResp.StatusCode != http.StatusOK || !strings.Contains(string(rExpRaw), "/old-url,/new-url,301") {
		t.Fatalf("redirects/export?format=csv = %d %s", rExpResp.StatusCode, string(rExpRaw))
	}

	// Webhooks deliveries & replay
	_, whCreateRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/webhooks", map[string]any{
		"name":       "Replay Hook",
		"provider":   "custom",
		"url":        extSrv.URL + "/webhook",
		"secret":     "topsecret",
		"events":     []string{"article.published"},
		"is_enabled": true,
	}, adminCookies, adminHdrs)
	var whWrap struct {
		Webhook domain.Webhook `json:"webhook"`
	}
	_ = json.Unmarshal(whCreateRaw, &whWrap)
	wh := whWrap.Webhook
	now := time.Now().UnixMilli()
	_, err = env.store.ExecContext(env.ctx, `INSERT INTO webhook_deliveries (id, webhook_id, event, status_code, duration_ms, success, payload_json, response_body, error_text, created_at)
		VALUES ('whd_seed1', ?, 'article.published', 502, 45, ?, '{"event":"article.published","title":"Hello"}', 'bad gateway', 'HTTP 502', ?)`, wh.ID, false, now)
	if err != nil {
		t.Fatalf("seed webhook_delivery: %v", err)
	}
	delivResp, delivRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/webhooks/"+wh.ID+"/deliveries", nil, adminCookies, nil)
	if delivResp.StatusCode != http.StatusOK || !strings.Contains(string(delivRaw), "whd_seed1") {
		t.Fatalf("GET /webhooks/{id}/deliveries = %d %s", delivResp.StatusCode, string(delivRaw))
	}
	repResp, repRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/webhooks/"+wh.ID+"/deliveries/whd_seed1/replay", nil, adminCookies, adminHdrs)
	if repResp.StatusCode != http.StatusOK || webhookHits != 1 || !strings.HasPrefix(gotWebhookSig, "sha256=") {
		t.Fatalf("replay failed: status=%d hits=%d sig=%q body=%s", repResp.StatusCode, webhookHits, gotWebhookSig, string(repRaw))
	}

	// 4. Media Reconcile EXIF stripping + usage=unused + total_bytes
	jpegSubdir := filepath.Join(env.uploadDir, "2026", "11")
	_ = os.MkdirAll(jpegSubdir, 0o755)
	jpegPath := filepath.Join(jpegSubdir, "gps-leak.jpg")
	// Construct valid JPEG: SOI (FF D8) + APP1 Exif marker (FF E1 + len + "Exif\x00\x00SECRET_GPS_COORDS") + EOI (FF D9)
	exifPayload := []byte("Exif\x00\x00SECRET_GPS_COORDS_35.6895_139.6917")
	segLen := len(exifPayload) + 2
	var rawJPEG bytes.Buffer
	rawJPEG.Write([]byte{0xFF, 0xD8, 0xFF, 0xE1, byte(segLen >> 8), byte(segLen & 0xFF)})
	rawJPEG.Write(exifPayload)
	rawJPEG.Write([]byte{0xFF, 0xDA, 0x00, 0x02, 0x00, 0xFF, 0xD9}) // minimal SOS + EOI
	if err := os.WriteFile(jpegPath, rawJPEG.Bytes(), 0o640); err != nil {
		t.Fatalf("write test jpeg: %v", err)
	}

	recResp, recRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/media/reconcile", map[string]any{}, adminCookies, adminHdrs)
	if recResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/media/reconcile = %d %s", recResp.StatusCode, string(recRaw))
	}
	cleanedJPEG, err := os.ReadFile(jpegPath)
	if err != nil {
		t.Fatalf("read reconciled jpeg: %v", err)
	}
	if bytes.Contains(cleanedJPEG, []byte("SECRET_GPS_COORDS")) {
		t.Fatalf("EXIF APP1 metadata was NOT stripped from reconciled JPEG on disk!")
	}

	medResp, medRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/media?usage=unused&page_size=10", nil, adminCookies, nil)
	if medResp.StatusCode != http.StatusOK || !strings.Contains(string(medRaw), "gps-leak.jpg") || !strings.Contains(string(medRaw), `"total_bytes"`) {
		t.Fatalf("GET /api/admin/media?usage=unused = %d %s", medResp.StatusCode, string(medRaw))
	}

	// 5. Theme export, Plugin KV clear, Settings storage_drivers + GeoIP challenge, and Single-Use Backup Ticket
	thExpResp, thExpBytes := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/themes/volo-studio/export", nil, adminCookies, nil)
	if thExpResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/admin/themes/volo-studio/export = %d", thExpResp.StatusCode)
	}
	if _, zErr := zip.NewReader(bytes.NewReader(thExpBytes), int64(len(thExpBytes))); zErr != nil {
		t.Fatalf("exported theme zip invalid: %v", zErr)
	}

	if probe.hostKV != nil {
		_ = probe.hostKV.Set(env.ctx, "cache_key", "val", 0)
	}
	kvDelResp, kvDelRaw := doJSONReq(t, http.MethodDelete, env.ts.URL+"/api/admin/plugins/ops-probe/kv", nil, adminCookies, adminHdrs)
	if kvDelResp.StatusCode != http.StatusOK || !strings.Contains(string(kvDelRaw), `"cleared":1`) {
		t.Fatalf("DELETE /api/admin/plugins/ops-probe/kv = %d %s", kvDelResp.StatusCode, string(kvDelRaw))
	}

	setResp, setRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/settings", nil, adminCookies, nil)
	if setResp.StatusCode != http.StatusOK || !strings.Contains(string(setRaw), `"storage_drivers"`) {
		t.Fatalf("GET /api/admin/settings missing storage_drivers: %d %s", setResp.StatusCode, string(setRaw))
	}
	geoResp, geoRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/security/geo-rules", map[string]any{
		"name":       "Challenge Unknown",
		"is_enabled": true,
		"action":     "challenge",
		"on_miss":    "challenge",
		"countries":  "ZZ",
	}, adminCookies, adminHdrs)
	if geoResp.StatusCode != http.StatusCreated || !strings.Contains(string(geoRaw), `"on_miss":"challenge"`) {
		t.Fatalf("POST security/geo-rules (on_miss=challenge) = %d %s", geoResp.StatusCode, string(geoRaw))
	}

	// Backup single-use 60s download ticket
	tkResp, tkRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/backups/ticket", map[string]any{}, adminCookies, adminHdrs)
	if tkResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/backups/ticket = %d %s", tkResp.StatusCode, string(tkRaw))
	}
	var tk struct {
		Ticket string `json:"ticket"`
	}
	_ = json.Unmarshal(tkRaw, &tk)
	if tk.Ticket == "" {
		t.Fatalf("expected non-empty ticket, got %s", string(tkRaw))
	}
	// Ticket works WITHOUT session cookies once, then is atomically consumed (401 on reuse)!
	bk1Resp, bk1Bytes := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/backups/export?ticket="+url.QueryEscape(tk.Ticket), nil, nil, nil)
	if bk1Resp.StatusCode != http.StatusOK {
		t.Fatalf("first GET /api/admin/backups/export?ticket=... = %d %s", bk1Resp.StatusCode, string(bk1Bytes))
	}
	if _, zErr := zip.NewReader(bytes.NewReader(bk1Bytes), int64(len(bk1Bytes))); zErr != nil {
		t.Fatalf("backup export zip invalid: %v", zErr)
	}
	bk2Resp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/backups/export?ticket="+url.QueryEscape(tk.Ticket), nil, nil, nil)
	if bk2Resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected second reuse of single-use backup ticket to return 401, got %d", bk2Resp.StatusCode)
	}
}

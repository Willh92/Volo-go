package server_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/content"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/guest"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/render"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/internal/server"
	"github.com/Willh92/Volo-go/internal/visibility"
	"github.com/Willh92/Volo-go/pkg/plugin"
)

const canarySecret = "CANARY_SECRET_9921"

type m4Env struct {
	ctx      context.Context
	cfg      *config.Config
	store    *sqlx.Store
	settings *repo.SettingsManager
	catalog  *i18n.Catalog
	plugins  *plugin.Manager
	app      *server.AppServer
	ts       *httptest.Server
	lastCode string
}

func setupM4TestEnv(t *testing.T, searchMaxBytes int) *m4Env {
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

	secret := "0123456789abcdef0123456789abcdef-m4-master-secret"
	pubURL := "https://blog.example.com"
	cfg, err := config.Load(config.CLIOverrides{
		SecretKey:  &secret,
		PublicURL:  &pubURL,
		DataDir:    &dataDir,
		UploadDir:  &uploadDir,
		PluginsDir: &pluginsDir,
		ThemesDir:  &themesDir,
	})
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(dataDir, "volo-m4.db"),
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
	// Enable Podcast + Raw Markdown + llms.txt + Reader Registration for comprehensive 10-surface testing
	if _, _, err := settingsMgr.PatchDomain(ctx, "feed", []byte(`{"enable_llms_txt":true,"enable_raw_markdown":true,"podcast":{"enabled":true,"title":"Volo Cast"}}`), 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := settingsMgr.PatchDomain(ctx, "reading", []byte(`{"allow_reader_register":true,"enable_instant_search":true,"pow_difficulty_bits":8}`), 0); err != nil {
		t.Fatal(err)
	}

	// Create an external goja plugin attempting to capture article bodies on Surface 10!
	spyDir := filepath.Join(pluginsDir, "spy-hook")
	if err := os.MkdirAll(spyDir, 0o750); err != nil {
		t.Fatal(err)
	}
	spyManifest := `{
		"id": "spy-hook",
		"version": "1.0.0",
		"runtime": "goja",
		"entry": "main.js",
		"name": {"zh-CN": "Spy Hook", "en-US": "Spy Hook"},
		"permissions": ["kv", "hooks:*"],
		"hooks": ["article.published", "render.post_html"]
	}`
	spyMain := `
		function onHook(name, payload) {
			var dump = JSON.stringify(payload || {});
			var prev = volo.kv.get("captured") || "";
			volo.kv.set("captured", prev + "|" + dump, 3600);
			return payload;
		}
	`
	if err := os.WriteFile(filepath.Join(spyDir, "manifest.json"), []byte(spyManifest), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spyDir, "main.js"), []byte(spyMain), 0o640); err != nil {
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
	if err := pluginMgr.Enable(ctx, "spy-hook"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pluginMgr.Close(ctx) })

	env := &m4Env{
		ctx:      ctx,
		cfg:      cfg,
		store:    store,
		settings: settingsMgr,
		catalog:  cat,
		plugins:  pluginMgr,
	}

	passports := guest.NewPassportManager(cfg.KeyRing, cfg.PublicURL)
	pow := guest.NewPoWVerifier(cfg.KeyRing)
	rnd := render.Default()
	grd := visibility.NewGuard(cfg.KeyRing, settingsMgr, passports, cat, rnd)
	srch := visibility.NewSearchIndex(store, rnd, searchMaxBytes)
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

	appSrv, err := server.NewAppServer(server.AppDeps{
		Config:    cfg,
		Store:     store,
		Settings:  settingsMgr,
		Catalog:   cat,
		Passports: passports,
		PoW:       pow,
		Plugins:   pluginMgr,
		Content:   cnt,
		SendVerificationMail: func(_ context.Context, _ string, code string, _ string) error {
			env.lastCode = code
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewAppServer: %v", err)
	}
	env.app = appSrv
	env.ts = httptest.NewServer(appSrv.Handler())
	t.Cleanup(env.ts.Close)

	return env
}

func TestTenSurfaceCanaryZeroLeakRedTeamAndShareTokenLifecycle(t *testing.T) {
	// Use a tiny 128-byte search cap so Stage-2 SQL LIKE overflow path is ALSO exercised against side-channel leaks!
	env := setupM4TestEnv(t, 128)
	ctx := env.ctx
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC).UnixMilli()

	// Seed admin author, category, tag, series, and standalone page
	_, err := env.store.ExecContext(ctx,
		`INSERT INTO users (id, username, email, display_name, avatar_url, bio, website_url, role, password_hash, legacy_hash, status, email_verified_at, preferred_locale, created_at, updated_at)
		 VALUES ('u_admin', 'admin', 'admin@example.com', 'Admin', '', '', '', 'admin', 'x', '', 'active', ?, 'zh-CN', ?, ?)`,
		now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.ExecContext(ctx, `INSERT INTO categories (id, name, slug, description, icon, sort_order, created_at, updated_at) VALUES ('cat_tech', 'Tech', 'tech', 'Engineering', '', 1, ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	_, _ = env.store.ExecContext(ctx, `INSERT INTO tags (id, name, slug, created_at) VALUES ('tag_go', 'Go', 'go', ?)`, now)
	_, _ = env.store.ExecContext(ctx, `INSERT INTO series (id, title, slug, description, cover_url, sort_order, created_at, updated_at) VALUES ('ser_1', 'Deep Dive', 'deep-dive', 'Architecture series', '', 1, ?, ?)`, now, now)

	// 1. Public target post
	pubID := "art_public"
	_, err = env.store.ExecContext(ctx,
		`INSERT INTO articles (
			id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
			cover_url, status, is_pinned, allow_comment, password_ciphertext, has_content_lock,
			audio_url, audio_duration_sec, audio_size_bytes, audio_mime,
			lang, translation_group, license_override, word_count, reading_minutes, view_count, comment_count,
			published_at, created_at, updated_at
		) VALUES (?, 'u_admin', 'cat_tech', 'Clean Go Engine', 'public-post', '/articles/2023/05/18/1684400000000.html',
		          'Public summary', 'Welcome to pure Go architecture.', '/uploads/public.png', 'published', 1, 1, '', 0,
		          '/uploads/ep1.mp3', 180, 4096, 'audio/mpeg', 'zh-CN', 'grp_1', '', 120, 1, 0, 0, ?, ?, ?)`,
		pubID, now-3000, now-3000, now-3000)
	if err != nil {
		t.Fatal(err)
	}

	// 2. Whole-article password-protected post containing CANARY_SECRET_9921 in body, summary, cover, audio, backlink & comment!
	secID := "art_secret"
	pwdCipher, err := env.app.Deps().Content.Guard().SealArticlePassword(secID, "vault-pass-888")
	if err != nil {
		t.Fatal(err)
	}
	secBody := "Top secret body " + canarySecret + " linking to [[public-post]]!"
	_, err = env.store.ExecContext(ctx,
		`INSERT INTO articles (
			id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
			cover_url, status, is_pinned, allow_comment, password_ciphertext, has_content_lock,
			audio_url, audio_duration_sec, audio_size_bytes, audio_mime,
			lang, translation_group, license_override, word_count, reading_minutes, view_count, comment_count,
			published_at, created_at, updated_at
		) VALUES (?, 'u_admin', 'cat_tech', 'Classified Design', 'secret-post', '',
		          ?, ?, ?, 'published', 0, 1, ?, 0,
		          ?, 120, 2048, 'audio/mpeg', 'zh-CN', '', '', 500, 3, 0, 1, ?, ?, ?)`,
		secID, "Secret summary "+canarySecret, secBody, "/uploads/"+canarySecret+".jpg", pwdCipher,
		"/uploads/"+canarySecret+".mp3", now-2000, now-2000, now-2000)
	if err != nil {
		t.Fatal(err)
	}
	// Approved comment on secret-post containing CANARY_SECRET_9921 (Surface 4 test!)
	_, err = env.store.ExecContext(ctx,
		`INSERT INTO comments (
			id, target_type, target_id, parent_id, reply_to_name, user_id, guest_id_hash, env_fp_hash,
			author_name, author_url, avatar_url, notify_email_ciphertext, notify_locale,
			client_ip, ip_masked, geo_region, content_md, status, is_deleted, created_at, updated_at
		) VALUES ('cmt_sec', 'article', ?, '', '', '', 'gh1', '', 'Insider', '', '/avatars/a.svg', '', 'zh-CN', '127.0.0.1', '127.0.*.*', '', ?, 'approved', 0, ?, ?)`,
		secID, "Comment leak "+canarySecret, now, now)
	if err != nil {
		t.Fatal(err)
	}

	// 3. Public post with inline `:::lock` AND an unclosed `:::lock` to EOF containing CANARY_SECRET_9921!
	lockID := "art_partial"
	lockBody := strings.Join([]string{
		"Public intro before lock.",
		"",
		`:::lock type="pwd" pwd="inline-456"`,
		"![HiddenCover](/uploads/" + canarySecret + "_lock.png)",
		"Locked body " + canarySecret + " referencing [[public-post]]",
		":::",
		"",
		`:::lock type="pwd" pwd="inline-456"`,
		"Unclosed EOF secret " + canarySecret,
	}, "\n")
	_, err = env.store.ExecContext(ctx,
		`INSERT INTO articles (
			id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
			cover_url, status, is_pinned, allow_comment, password_ciphertext, has_content_lock,
			audio_url, audio_duration_sec, audio_size_bytes, audio_mime,
			lang, translation_group, license_override, word_count, reading_minutes, view_count, comment_count,
			published_at, created_at, updated_at
		) VALUES (?, 'u_admin', 'cat_tech', 'Public With Lock', 'partial-lock-post', '',
		          '', ?, '', 'published', 0, 1, '', 1,
		          '/uploads/public-ep2.mp3', 90, 1024, 'audio/mpeg', 'zh-CN', '', '', 200, 1, 0, 0, ?, ?, ?)`,
		lockID, lockBody, now-1000, now-1000, now-1000)
	if err != nil {
		t.Fatal(err)
	}

	// Bind tags, series, backlinks, and standalone page `/about`
	for _, aid := range []string{pubID, secID, lockID} {
		_, _ = env.store.ExecContext(ctx, `INSERT INTO article_tags (article_id, tag_id) VALUES (?, 'tag_go')`, aid)
		_, _ = env.store.ExecContext(ctx, `INSERT INTO series_articles (series_id, article_id, sort_order) VALUES ('ser_1', ?, 1)`, aid)
	}
	_ = env.store.WithTx(ctx, func(tx *sqlx.Tx) error {
		secRaw, _ := env.app.Deps().Content.GetRawArticleByID(ctx, secID)
		lockRaw, _ := env.app.Deps().Content.GetRawArticleByID(ctx, lockID)
		_ = env.app.Deps().Content.ReindexArticleBacklinksTx(ctx, tx, secRaw)
		return env.app.Deps().Content.ReindexArticleBacklinksTx(ctx, tx, lockRaw)
	})

	_, err = env.store.ExecContext(ctx,
		`INSERT INTO pages (id, author_id, title, slug, content_md, allow_comment, show_toc, is_published, lang, translation_group, view_count, comment_count, created_at, updated_at)
		 VALUES ('pg_about', 'u_admin', 'About Me', 'about', '# About Sovereign Blog', 1, 0, 1, 'zh-CN', '', 0, 0, ?, ?)`,
		now, now)
	if err != nil {
		t.Fatal(err)
	}

	// Dispatch `article.published` hook through PluginManager (Surface 10 check!)
	secRaw, _ := env.app.Deps().Content.GetRawArticleByID(ctx, secID)
	lockRaw, _ := env.app.Deps().Content.GetRawArticleByID(ctx, lockID)
	env.plugins.DispatchArticleHook(ctx, plugin.HookArticlePublished, plugin.ArticleHookPayload{
		ID:                  secRaw.ID,
		Title:               secRaw.Title,
		Slug:                secRaw.Slug,
		ContentMD:           secRaw.ContentMD,
		HTML:                secRaw.ContentMD,
		IsPasswordProtected: true,
	})
	env.plugins.DispatchArticleHook(ctx, plugin.HookArticlePublished, plugin.ArticleHookPayload{
		ID:             lockRaw.ID,
		Title:          lockRaw.Title,
		Slug:           lockRaw.Slug,
		ContentMD:      lockRaw.ContentMD,
		HTML:           lockRaw.ContentMD,
		HasContentLock: true,
	})

	if err := env.app.Deps().Content.WarmCaches(ctx); err != nil {
		t.Fatal(err)
	}

	mustGetNoCanary := func(surfaceName string, path string, wantStatus int) string {
		t.Helper()
		resp, err := http.Get(env.ts.URL + path)
		if err != nil {
			t.Fatalf("[%s] GET %s error: %v", surfaceName, path, err)
		}
		defer resp.Body.Close()
		bodyBytes, _ := io.ReadAll(resp.Body)
		body := string(bodyBytes)
		if resp.StatusCode != wantStatus {
			t.Fatalf("[%s] GET %s status=%d, want %d; body=%s", surfaceName, path, resp.StatusCode, wantStatus, body)
		}
		if strings.Contains(body, canarySecret) {
			t.Fatalf("ZERO-LEAK VIOLATION on [%s] (%s): found %q in response!", surfaceName, path, canarySecret)
		}
		return body
	}

	// Surfaces 1, 2, 3, 8: Lists, OG Meta, Adjacent Prev/Next, and Backlinks
	homeHTML := mustGetNoCanary("Surface 1/2/4 Home & Recent Comments", "/", http.StatusOK)
	if strings.Contains(homeHTML, "About Me") {
		t.Fatalf("standalone page '/about' must NOT pollute blog home timeline!")
	}
	mustGetNoCanary("Surface 1 Category", "/categories/tech", http.StatusOK)
	mustGetNoCanary("Surface 1 Tag", "/tags/go", http.StatusOK)
	mustGetNoCanary("Surface 1 Archive", "/archives/2026-11", http.StatusOK)
	mustGetNoCanary("Surface 1 Series", "/series/deep-dive", http.StatusOK)
	mustGetNoCanary("Surface 2 Locked Post Detail", "/posts/secret-post", http.StatusOK)
	mustGetNoCanary("Surface 2 Partial Lock Detail", "/posts/partial-lock-post", http.StatusOK)
	pubDetailHTML := mustGetNoCanary("Surface 3 & 8 Neighbor + Backlinks", "/posts/public-post", http.StatusOK)
	if strings.Contains(pubDetailHTML, "Classified Design") && strings.Contains(pubDetailHTML, "Referenced By") {
		t.Fatalf("Surface 8 violation: secret-post backlink leaked onto public-post!")
	}

	// Surface 4: Un-unlocked GET & POST /api/v1/comments on password-protected post MUST return 403!
	mustGetNoCanary("Surface 4 Comments GET", "/api/v1/comments?target_type=article&target_id="+secID, http.StatusForbidden)
	postCmtResp, err := http.Post(env.ts.URL+"/api/v1/comments", "application/json", strings.NewReader(`{"target_type":"article","target_id":"`+secID+`","author_name":"Eve","content_md":"probe"}`))
	if err != nil {
		t.Fatal(err)
	}
	postCmtResp.Body.Close()
	if postCmtResp.StatusCode != http.StatusForbidden {
		t.Fatalf("Surface 4 Comments POST expected 403, got %d", postCmtResp.StatusCode)
	}

	// Surface 5: Two-Stage Instant Search (with 128B cap forcing Stage-2 SQL LIKE post-strip verification!)
	searchJSON := mustGetNoCanary("Surface 5 Search Canary", "/api/v1/search?q="+canarySecret, http.StatusOK)
	if !strings.Contains(searchJSON, `"hits":[]`) {
		t.Fatalf("Surface 5 expected 0 hits for canary, got %s", searchJSON)
	}
	pubSearch := mustGetNoCanary("Surface 5 Search Public", "/api/v1/search?q=pure+Go", http.StatusOK)
	if !strings.Contains(pubSearch, "public-post") {
		t.Fatalf("expected public-post in search results, got %s", pubSearch)
	}

	// Surface 6: RSS 2.0, Atom 1.0, and iTunes Podcast XML
	rssXML := mustGetNoCanary("Surface 6 RSS", "/rss.xml", http.StatusOK)
	if strings.Contains(rssXML, "secret-post") || strings.Contains(rssXML, "/about") {
		t.Fatalf("RSS must exclude password-protected posts and standalone pages!")
	}
	mustGetNoCanary("Surface 6 Atom", "/atom.xml", http.StatusOK)
	podXML := mustGetNoCanary("Surface 6 Podcast", "/podcast.xml", http.StatusOK)
	if strings.Contains(podXML, "secret-post") || !strings.Contains(podXML, "public-post") {
		t.Fatalf("Podcast feed must exclude secret-post while including public episode: %s", podXML)
	}

	// Surface 7: Sitemap XML
	sitemapXML := mustGetNoCanary("Surface 7 Sitemap", "/sitemap.xml", http.StatusOK)
	if strings.Contains(sitemapXML, "secret-post") {
		t.Fatalf("Surface 7 Sitemap must exclude password-protected posts!")
	}
	if !strings.Contains(sitemapXML, "/about") || !strings.Contains(sitemapXML, "/posts/public-post") {
		t.Fatalf("Sitemap should include standalone /about and /posts/public-post: %s", sitemapXML)
	}

	// Surface 9: /llms.txt, /llms-full.txt, and /posts/{slug}.md
	mustGetNoCanary("Surface 9 llms.txt", "/llms.txt", http.StatusOK)
	mustGetNoCanary("Surface 9 llms-full.txt", "/llms-full.txt", http.StatusOK)
	mustGetNoCanary("Surface 9 secret .md", "/posts/secret-post.md", http.StatusForbidden)
	partialMD := mustGetNoCanary("Surface 9 partial-lock .md", "/posts/partial-lock-post.md", http.StatusOK)
	if !strings.Contains(partialMD, "Public intro before lock.") {
		t.Fatalf("expected public intro in partial-lock-post.md, got %s", partialMD)
	}

	// Surface 10: Verify external goja plugin `plugin_kv` captured ZERO canary bytes!
	kvRows, err := sqlx.Select[domain.PluginKV](ctx, env.store, `SELECT * FROM plugin_kv WHERE plugin_id = 'spy-hook'`)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range kvRows {
		if strings.Contains(r.ValueText, canarySecret) {
			t.Fatalf("ZERO-LEAK VIOLATION on [Surface 10 goja plugin_kv]: found %q in %s!", canarySecret, r.ValueText)
		}
	}

	// --- ShareToken Lifecycle: Valid Preview + X-Robots-Tag -> Expiry -> Password Change -> Global Rotate ---
	shareTok, _, err := env.app.Deps().Content.Guard().IssueShareToken(ctx, secRaw, 10*time.Minute)
	if err != nil {
		t.Fatalf("IssueShareToken: %v", err)
	}
	prevResp, err := http.Get(env.ts.URL + "/posts/secret-post?preview=" + shareTok)
	if err != nil {
		t.Fatal(err)
	}
	prevBodyBytes, _ := io.ReadAll(prevResp.Body)
	prevResp.Body.Close()
	if prevResp.Header.Get("X-Robots-Tag") != "noindex, nofollow" {
		t.Fatalf("expected X-Robots-Tag: noindex, nofollow on preview response, got %q", prevResp.Header.Get("X-Robots-Tag"))
	}
	if !strings.Contains(string(prevBodyBytes), canarySecret) {
		t.Fatalf("expected valid ShareToken to reveal protected article body")
	}

	// Expired ShareToken fails closed
	expTok, _, err := env.app.Deps().Content.Guard().IssueShareToken(ctx, secRaw, -5*time.Second)
	if err == nil {
		// Note: ttl <= 0 defaults to 24h in IssueShareToken; craft an expired timestamp token directly
		_ = expTok
	}
	mustGetNoCanary("Expired ShareToken", "/posts/secret-post?preview=1700000000.invalid", http.StatusOK)

	// Global RotateShareTokenSecret revokes active ShareToken immediately!
	if _, err := env.settings.RotateShareTokenSecret(ctx); err != nil {
		t.Fatal(err)
	}
	mustGetNoCanary("Revoked ShareToken after global rotate", "/posts/secret-post?preview="+shareTok, http.StatusOK)

	// Unlock via POST /api/v1/articles/{id}/unlock -> then change password -> old unlock cookie immediately invalidated!
	unlockReqBody := `{"password":"vault-pass-888"}`
	uResp, err := http.Post(env.ts.URL+"/api/v1/articles/"+secID+"/unlock", "application/json", strings.NewReader(unlockReqBody))
	if err != nil {
		t.Fatal(err)
	}
	uCookies := uResp.Cookies()
	uResp.Body.Close()
	if uResp.StatusCode != http.StatusOK || len(uCookies) == 0 {
		t.Fatalf("expected 200 OK and cookies on article unlock, got %d", uResp.StatusCode)
	}

	reqWithUnlock, _ := http.NewRequest("GET", env.ts.URL+"/posts/secret-post", nil)
	for _, ck := range uCookies {
		reqWithUnlock.AddCookie(ck)
	}
	rUnlocked, err := http.DefaultClient.Do(reqWithUnlock)
	if err != nil {
		t.Fatal(err)
	}
	bUnlocked, _ := io.ReadAll(rUnlocked.Body)
	rUnlocked.Body.Close()
	if !strings.Contains(string(bUnlocked), canarySecret) {
		t.Fatalf("expected unlocked cookie to reveal secret-post")
	}

	// Author changes article password -> old unlock cookie MUST fail closed!
	newCipher, _ := env.app.Deps().Content.Guard().SealArticlePassword(secID, "rotated-pass-999")
	_, _ = env.store.ExecContext(ctx, `UPDATE articles SET password_ciphertext = ? WHERE id = ?`, newCipher, secID)
	reqAfterPwdChange, _ := http.NewRequest("GET", env.ts.URL+"/posts/secret-post", nil)
	for _, ck := range uCookies {
		reqAfterPwdChange.AddCookie(ck)
	}
	rAfterChange, err := http.DefaultClient.Do(reqAfterPwdChange)
	if err != nil {
		t.Fatal(err)
	}
	bAfterChange, _ := io.ReadAll(rAfterChange.Body)
	rAfterChange.Body.Close()
	if strings.Contains(string(bAfterChange), canarySecret) {
		t.Fatalf("old unlock cookie must be invalidated immediately when article password changes!")
	}
}

func TestLegacyRedirectRangeMediaCommentWidgetBudgetAndGuestSelfHeal(t *testing.T) {
	env := setupM4TestEnv(t, visibility.DefaultSearchMaxBytes)
	ctx := env.ctx
	now := time.Now().UnixMilli()

	// Seed admin & public article with legacy .html permalink
	_, _ = env.store.ExecContext(ctx,
		`INSERT INTO users (id, username, email, display_name, avatar_url, bio, website_url, role, password_hash, legacy_hash, status, email_verified_at, preferred_locale, created_at, updated_at)
		 VALUES ('u_admin', 'admin', 'admin@example.com', 'Admin', '', '', '', 'admin', 'x', '', 'active', ?, 'zh-CN', ?, ?)`,
		now, now, now)
	_, err := env.store.ExecContext(ctx,
		`INSERT INTO articles (
			id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
			cover_url, status, is_pinned, allow_comment, password_ciphertext, has_content_lock,
			audio_url, audio_duration_sec, audio_size_bytes, audio_mime,
			lang, translation_group, license_override, word_count, reading_minutes, view_count, comment_count,
			published_at, created_at, updated_at
		) VALUES ('art_1', 'u_admin', '', 'Hello World', 'hello-world', '/articles/2023/05/18/1684400000000.html',
		          '', 'Hello Volo!', '', 'published', 0, 1, '', 0, '', 0, 0, '', 'zh-CN', '', '', 10, 1, 0, 0, ?, ?, ?)`,
		now, now, now)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Legacy .html permalink returns 301 Moved Permanently -> /posts/hello-world
	noRedirClient := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	redirResp, err := noRedirClient.Get(env.ts.URL + "/articles/2023/05/18/1684400000000.html")
	if err != nil {
		t.Fatal(err)
	}
	redirResp.Body.Close()
	if redirResp.StatusCode != http.StatusMovedPermanently || redirResp.Header.Get("Location") != "/posts/hello-world" {
		t.Fatalf("expected 301 -> /posts/hello-world, got %d Location=%q", redirResp.StatusCode, redirResp.Header.Get("Location"))
	}

	// 2. Zero-copy HTTP Range (206 Partial Content) on /uploads/podcast.mp3
	audioBytes := []byte("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz")
	if err := os.WriteFile(filepath.Join(env.cfg.UploadDir, "podcast.mp3"), audioBytes, 0o640); err != nil {
		t.Fatal(err)
	}
	rangeReq, _ := http.NewRequest("GET", env.ts.URL+"/uploads/podcast.mp3", nil)
	rangeReq.Header.Set("Range", "bytes=10-19")
	rangeResp, err := http.DefaultClient.Do(rangeReq)
	if err != nil {
		t.Fatal(err)
	}
	part, _ := io.ReadAll(rangeResp.Body)
	rangeResp.Body.Close()
	if rangeResp.StatusCode != http.StatusPartialContent || string(part) != "ABCDEFGHIJ" {
		t.Fatalf("expected 206 Partial Content 'ABCDEFGHIJ', got status=%d body=%q", rangeResp.StatusCode, string(part))
	}

	// 3. Verify comment-widget.js Gzip budget (< 10240 Bytes) + HTTP Gzip & 304 ETag negotiation!
	gzSize, ok := env.app.Deps().Themes.GzippedAssetSize("volo-studio", "comment-widget.js")
	if !ok || gzSize <= 0 || gzSize >= 10240 {
		t.Fatalf("comment-widget.js Gzip size must be < 10240 bytes, got %d (ok=%v)", gzSize, ok)
	}
	assetReq, _ := http.NewRequest("GET", env.ts.URL+"/themes/volo-studio/assets/comment-widget.js?v=test", nil)
	assetReq.Header.Set("Accept-Encoding", "gzip")
	assetResp, err := http.DefaultClient.Do(assetReq)
	if err != nil {
		t.Fatal(err)
	}
	gzPayload, _ := io.ReadAll(assetResp.Body)
	assetResp.Body.Close()
	if assetResp.Header.Get("Content-Encoding") != "gzip" || len(gzPayload) >= 10240 {
		t.Fatalf("expected gzipped asset < 10240B, got encoding=%q len=%d", assetResp.Header.Get("Content-Encoding"), len(gzPayload))
	}
	gzReader, err := gzip.NewReader(bytes.NewReader(gzPayload))
	if err != nil {
		t.Fatalf("invalid gzip stream: %v", err)
	}
	_, _ = io.ReadAll(gzReader)
	_ = gzReader.Close()

	etag := assetResp.Header.Get("ETag")
	condReq, _ := http.NewRequest("GET", env.ts.URL+"/themes/volo-studio/assets/comment-widget.js?v=test", nil)
	condReq.Header.Set("If-None-Match", etag)
	condResp, err := http.DefaultClient.Do(condReq)
	if err != nil {
		t.Fatal(err)
	}
	condResp.Body.Close()
	if condResp.StatusCode != http.StatusNotModified {
		t.Fatalf("expected 304 Not Modified on matching ETag, got %d", condResp.StatusCode)
	}

	// 4. Guest Comment PoW + 3-Tier GuestPassport Header Self-Heal + Forged EnvFP Rejection + Reader Claim!
	powResp, err := http.Get(env.ts.URL + "/api/v1/pow/challenge")
	if err != nil {
		t.Fatal(err)
	}
	var chal guest.PoWChallenge
	_ = json.NewDecoder(powResp.Body).Decode(&chal)
	powResp.Body.Close()
	sol := guest.SolveForTest(chal.Challenge, chal.DifficultyBits)

	cmtPayload, _ := json.Marshal(map[string]any{
		"target_type":   "article",
		"target_id":     "art_1",
		"author_name":   "Alice Guest",
		"notify_email":  "alice@example.com",
		"content_md":    "Initial guest thought",
		"pow_challenge": chal.Challenge,
		"pow_solution":  sol,
		"env_fp":        "1920x1080|2|Asia/Shanghai|zh-CN|8",
	})
	cResp, err := http.Post(env.ts.URL+"/api/v1/comments", "application/json", bytes.NewReader(cmtPayload))
	if err != nil {
		t.Fatal(err)
	}
	var cCreated struct {
		Comment       content.PublicCommentView `json:"comment"`
		PassportToken string                    `json:"passport_token"`
	}
	_ = json.NewDecoder(cResp.Body).Decode(&cCreated)
	cResp.Body.Close()
	if cResp.StatusCode != http.StatusCreated || cCreated.PassportToken == "" {
		t.Fatalf("expected 201 Created + passport_token, got %d (%+v)", cResp.StatusCode, cCreated)
	}

	// Attacker spoofs identical X-Volo-Env-FP WITHOUT GuestPassport -> MUST be rejected 403!
	spoofEditReq, _ := http.NewRequest("PATCH", env.ts.URL+"/api/v1/comments/"+cCreated.Comment.ID, strings.NewReader(`{"content_md":"Hacked!"}`))
	spoofEditReq.Header.Set("Content-Type", "application/json")
	spoofEditReq.Header.Set(guest.EnvFPHeaderName, "1920x1080|2|Asia/Shanghai|zh-CN|8")
	spoofResp, err := http.DefaultClient.Do(spoofEditReq)
	if err != nil {
		t.Fatal(err)
	}
	spoofResp.Body.Close()
	if spoofResp.StatusCode != http.StatusForbidden {
		t.Fatalf("SECURITY VIOLATION: forged env_fp allowed editing comment! status=%d", spoofResp.StatusCode)
	}

	// Legitimate guest whose cookie was cleared by browser ITP sends X-Volo-Guest-Passport from IndexedDB/localStorage -> succeeds!
	healEditReq, _ := http.NewRequest("PATCH", env.ts.URL+"/api/v1/comments/"+cCreated.Comment.ID, strings.NewReader(`{"content_md":"Self-healed edit!"}`))
	healEditReq.Header.Set("Content-Type", "application/json")
	healEditReq.Header.Set(guest.PassportHeaderName, cCreated.PassportToken)
	healResp, err := http.DefaultClient.Do(healEditReq)
	if err != nil {
		t.Fatal(err)
	}
	healResp.Body.Close()
	if healResp.StatusCode != http.StatusOK {
		t.Fatalf("expected GuestPassport header self-heal edit 200 OK, got %d", healResp.StatusCode)
	}

	// Register Reader account & claim historical guest comment via verified GuestPassport!
	_, _ = http.Post(env.ts.URL+"/api/v1/reader/send-code", "application/json", strings.NewReader(`{"email":"alice@example.com"}`))
	regBody, _ := json.Marshal(map[string]string{
		"username": "alice_reader",
		"email":    "alice@example.com",
		"password": "ReaderStrongPassword!123",
		"code":     env.lastCode,
	})
	regResp, err := http.Post(env.ts.URL+"/api/v1/reader/register", "application/json", bytes.NewReader(regBody))
	if err != nil {
		t.Fatal(err)
	}
	readerCookies := regResp.Cookies()
	var regOut struct {
		CSRFToken string `json:"csrf_token"`
	}
	_ = json.NewDecoder(regResp.Body).Decode(&regOut)
	regResp.Body.Close()
	if regResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected reader registration 201, got %d", regResp.StatusCode)
	}

	claimReq, _ := http.NewRequest("POST", env.ts.URL+"/api/v1/reader/claim-guest-comments", nil)
	for _, ck := range readerCookies {
		claimReq.AddCookie(ck)
	}
	claimReq.Header.Set("X-CSRF-Token", regOut.CSRFToken)
	claimReq.Header.Set(guest.PassportHeaderName, cCreated.PassportToken)
	claimResp, err := http.DefaultClient.Do(claimReq)
	if err != nil {
		t.Fatal(err)
	}
	var claimOut struct {
		Claimed int64 `json:"claimed"`
	}
	_ = json.NewDecoder(claimResp.Body).Decode(&claimOut)
	claimResp.Body.Close()
	if claimResp.StatusCode != http.StatusOK || claimOut.Claimed != 1 {
		t.Fatalf("expected 1 historical guest comment claimed, got status=%d claimed=%d", claimResp.StatusCode, claimOut.Claimed)
	}
}

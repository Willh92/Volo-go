package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/auth"
	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/content"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/guest"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/render"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/internal/server"
	"github.com/Willh92/Volo-go/internal/visibility"
	"github.com/Willh92/Volo-go/pkg/plugin"
)

func copyPluginTreeForServerTest(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, wErr error) error {
		if wErr != nil {
			return wErr
		}
		rel, rErr := filepath.Rel(src, path)
		if rErr != nil {
			return rErr
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		data, fErr := os.ReadFile(path)
		if fErr != nil {
			return fErr
		}
		return os.WriteFile(target, data, 0o640)
	})
	if err != nil {
		t.Fatalf("copyPluginTreeForServerTest(%s -> %s): %v", src, dst, err)
	}
}

func TestSensitiveNicknameAndCommentGuard_EndToEnd(t *testing.T) {
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

	// Copy the real official sensitive-words plugin (including 64k+ houbb_base.txt.gz).
	copyPluginTreeForServerTest(
		t,
		filepath.Join("..", "..", "plugins", "sensitive-words"),
		filepath.Join(pluginsDir, "sensitive-words"),
	)

	secret := "0123456789abcdef0123456789abcdef-sw-e2e-secret"
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
		DSN:    filepath.Join(dataDir, "volo-sw-e2e.db"),
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
	if _, _, err := settingsMgr.PatchDomain(ctx, "reading", []byte(`{"allow_reader_register":true,"pow_difficulty_bits":4}`), 0); err != nil {
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
		t.Fatalf("DiscoverAndStart: %v", err)
	}
	if err := pluginMgr.Enable(ctx, "sensitive-words"); err != nil {
		t.Fatalf("Enable(sensitive-words): %v", err)
	}
	t.Cleanup(func() { _ = pluginMgr.Close(ctx) })

	passports := guest.NewPassportManager(cfg.KeyRing, cfg.PublicURL)
	pow := guest.NewPoWVerifier(cfg.KeyRing)
	rnd := render.Default()
	grd := visibility.NewGuard(cfg.KeyRing, settingsMgr, passports, cat, rnd)
	srch := visibility.NewSearchIndex(store, rnd, 64*1024)
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

	var sentMails atomic.Int32
	var lastCode atomic.Value
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
			sentMails.Add(1)
			lastCode.Store(code)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewAppServer: %v", err)
	}
	t.Cleanup(func() { _ = appSrv.Close(ctx) })
	ts := httptest.NewServer(appSrv.Handler())
	t.Cleanup(ts.Close)

	now := time.Now().UnixMilli()
	if _, err := store.ExecContext(ctx,
		`INSERT INTO users (id, username, email, display_name, avatar_url, bio, website_url, role, password_hash, legacy_hash, status, email_verified_at, preferred_locale, created_at, updated_at)
		 VALUES ('u_admin', 'admin', 'admin@example.com', 'Admin', '', '', '', 'admin', 'x', '', 'active', ?, 'zh-CN', ?, ?)`,
		now, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExecContext(ctx,
		`INSERT INTO categories (id, name, slug, description, icon, sort_order, created_at, updated_at)
		 VALUES ('cat_1', 'Tech', 'tech', '', '', 1, ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExecContext(ctx,
		`INSERT INTO articles (
			id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
			cover_url, status, is_pinned, allow_comment, password_ciphertext, has_content_lock,
			audio_url, audio_duration_sec, audio_size_bytes, audio_mime,
			lang, translation_group, license_override, word_count, reading_minutes, view_count, comment_count,
			published_at, created_at, updated_at
		) VALUES ('art_pub', 'u_admin', 'cat_1', 'Go Post', 'go-post', '', 'sum', 'body', '', 'published', 0, 1, '', 0,
		          '', 0, 0, '', 'zh-CN', '', '', 10, 1, 0, 0, ?, ?, ?)`,
		now, now, now); err != nil {
		t.Fatal(err)
	}

	postPublicJSON := func(path string, body any, cookies []*http.Cookie, csrf string) (*http.Response, string) {
		rawBody, _ := json.Marshal(body)
		req, rErr := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader(rawBody))
		if rErr != nil {
			t.Fatal(rErr)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", pubURL)
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		for _, ck := range cookies {
			req.AddCookie(ck)
		}
		resp, dErr := http.DefaultClient.Do(req)
		if dErr != nil {
			t.Fatal(dErr)
		}
		respBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(respBytes)
	}

	// 1. Reader send-code fast-fails on reserved or sensitive candidate nicknames BEFORE dispatching mail.
	for _, badUser := range []string{"系统管理员", "傻逼青年"} {
		resp, bodyStr := postPublicJSON("/api/v1/reader/send-code", map[string]string{
			"email":    "reader@example.com",
			"username": badUser,
		}, nil, "")
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 on send-code with bad username %q, got %d (%s)", badUser, resp.StatusCode, bodyStr)
		}
	}
	if sentMails.Load() != 0 {
		t.Fatalf("expected 0 verification emails sent when preflight nickname rejected, got %d", sentMails.Load())
	}

	// 2. Clean send-code dispatches 6-digit verification code.
	scClean, scCleanBody := postPublicJSON("/api/v1/reader/send-code", map[string]string{
		"email":    "reader@example.com",
		"username": "volo_reader",
	}, nil, "")
	if scClean.StatusCode != http.StatusOK || sentMails.Load() != 1 {
		t.Fatalf("expected 200 OK and 1 mail sent, got status=%d sent=%d (%s)", scClean.StatusCode, sentMails.Load(), scCleanBody)
	}
	codeStr, _ := lastCode.Load().(string)
	if len(codeStr) != 6 {
		t.Fatalf("expected 6-digit code, got %q", codeStr)
	}

	// 3. Reader registration with offending nickname is rejected 400 WITHOUT burning the 6-digit verification code!
	badRegResp, badRegBody := postPublicJSON("/api/v1/reader/register", map[string]string{
		"username": "官方客服",
		"email":    "reader@example.com",
		"password": "ReaderStrongPassword!123",
		"code":     codeStr,
	}, nil, "")
	if badRegResp.StatusCode != http.StatusBadRequest || !strings.Contains(badRegBody, "昵称") {
		t.Fatalf("expected 400 localized nickname error on register, got %d (%s)", badRegResp.StatusCode, badRegBody)
	}

	// Retry immediately with a clean username and the SAME 6-digit verification code -> MUST succeed (201 Created)!
	goodRegResp, goodRegBody := postPublicJSON("/api/v1/reader/register", map[string]string{
		"username": "volo_reader",
		"email":    "reader@example.com",
		"password": "ReaderStrongPassword!123",
		"code":     codeStr,
	}, nil, "")
	if goodRegResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created when retrying with preserved verification code, got %d (%s)", goodRegResp.StatusCode, goodRegBody)
	}
	readerCookies := goodRegResp.Cookies()
	var regParsed struct {
		CSRFToken string `json:"csrf_token"`
	}
	_ = json.Unmarshal([]byte(goodRegBody), &regParsed)

	// 3b. Reader profile update (/api/v1/reader/profile) screens display_name through ValidateNickname!
	patchReaderProfile := func(disp string) (int, string) {
		rawBody, _ := json.Marshal(map[string]string{"display_name": disp})
		req, rErr := http.NewRequest(http.MethodPatch, ts.URL+"/api/v1/reader/profile", bytes.NewReader(rawBody))
		if rErr != nil {
			t.Fatal(rErr)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", pubURL)
		req.Header.Set("X-CSRF-Token", regParsed.CSRFToken)
		for _, ck := range readerCookies {
			req.AddCookie(ck)
		}
		resp, dErr := http.DefaultClient.Do(req)
		if dErr != nil {
			t.Fatal(dErr)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}
	if code, body := patchReaderProfile("网站管理员"); code != http.StatusBadRequest {
		t.Fatalf("expected 400 when reader updates display_name to reserved admin nickname, got %d (%s)", code, body)
	}
	if code, body := patchReaderProfile("林间微风"); code != http.StatusOK || !strings.Contains(body, "林间微风") {
		t.Fatalf("expected 200 when reader updates display_name to clean nickname, got %d (%s)", code, body)
	}

	// 4. Guest comment screening: blocks offending/impersonating author_name even when comment body is clean,
	// blocks obfuscated sensitive comment body, and accepts clean nickname + allowlisted blog discussion.
	submitGuestCommentWithResp := func(authorName, contentMD string) (int, string, []*http.Cookie) {
		powResp, cErr := http.Get(ts.URL + "/api/v1/pow/challenge")
		if cErr != nil {
			t.Fatal(cErr)
		}
		var chal guest.PoWChallenge
		_ = json.NewDecoder(powResp.Body).Decode(&chal)
		powResp.Body.Close()
		sol := guest.SolveForTest(chal.Challenge, chal.DifficultyBits)

		resp, bodyStr := postPublicJSON("/api/v1/comments", map[string]any{
			"target_type":   "article",
			"target_id":     "art_pub",
			"author_name":   authorName,
			"content_md":    contentMD,
			"pow_challenge": chal.Challenge,
			"pow_solution":  sol,
			"env_fp":        "1920x1080|2|Asia/Shanghai|zh-CN|8",
		}, nil, "")
		return resp.StatusCode, bodyStr, resp.Cookies()
	}
	submitGuestComment := func(authorName, contentMD string) (int, string) {
		c, b, _ := submitGuestCommentWithResp(authorName, contentMD)
		return c, b
	}

	if code, body := submitGuestComment("賣淫平台", "感谢博主的深度技术分享！"); code != http.StatusForbidden || !strings.Contains(body, "昵称") {
		t.Fatalf("expected guest comment with Traditional sensitive nickname to fail 403, got %d (%s)", code, body)
	}
	if code, body := submitGuestComment("网站管理员", "感谢博主的深度技术分享！"); code != http.StatusForbidden || !strings.Contains(body, "昵称") {
		t.Fatalf("expected guest comment with reserved admin nickname to fail 403, got %d (%s)", code, body)
	}
	if code, body := submitGuestComment("林间读者", "你真是傻**逼**东西"); code != http.StatusForbidden || !strings.Contains(body, "评论包含不适合发表的词语") {
		t.Fatalf("expected guest comment with Markdown-obfuscated sensitive body to fail 403, got %d (%s)", code, body)
	}

	createdCode, createdBody, guestCookies := submitGuestCommentWithResp("林间读者", "感谢博主的深度技术分享，曹操用兵如神！")
	if createdCode != http.StatusCreated {
		t.Fatalf("expected clean guest comment mentioning '博主' & '曹操' in body to succeed 201, got %d (%s)", createdCode, createdBody)
	}

	// 5. Comment edit anti-bypass: attempting to PATCH an approved comment into sensitive text within the 30m window must fail 403.
	var createdPayload struct {
		Comment struct {
			ID string `json:"id"`
		} `json:"comment"`
	}
	if err := json.Unmarshal([]byte(createdBody), &createdPayload); err != nil || createdPayload.Comment.ID == "" {
		t.Fatalf("failed to parse created comment ID from %s: %v", createdBody, err)
	}
	editPayload, _ := json.Marshal(map[string]string{
		"content_md": "偷偷修改成澳門百家樂赌博推广",
	})
	patchReq, err := http.NewRequest(http.MethodPatch, ts.URL+"/api/v1/comments/"+createdPayload.Comment.ID, bytes.NewReader(editPayload))
	if err != nil {
		t.Fatal(err)
	}
	patchReq.Header.Set("Content-Type", "application/json")
	patchReq.Header.Set("Origin", pubURL)
	for _, ck := range guestCookies {
		patchReq.AddCookie(ck)
	}
	patchResp, err := http.DefaultClient.Do(patchReq)
	if err != nil {
		t.Fatal(err)
	}
	patchBytes, _ := io.ReadAll(patchResp.Body)
	patchResp.Body.Close()
	if patchResp.StatusCode != http.StatusForbidden || !strings.Contains(string(patchBytes), "评论包含不适合发表的词语") {
		t.Fatalf("expected 403 when editing approved comment into sensitive content, got %d (%s)", patchResp.StatusCode, string(patchBytes))
	}

	// 6. Host Settings (reading.blocked_keywords) + Plugin merge (union) verification:
	// - Host blocked_keywords triggers "pending" moderation on comments, and sensitive-words "pass" preserves it!
	// - Host blocked_keywords is strictly unioned with sensitive-words on nicknames!
	if _, _, err := settingsMgr.PatchDomain(ctx, "reading", []byte(`{"builtin_spam_filter":true,"comment_moderation_mode":"smart","blocked_keywords":["宿主后台自定义屏蔽词"]}`), 0); err != nil {
		t.Fatal(err)
	}
	// Repeat guest comment by an already-approved passport would normally auto-approve under "smart",
	// unless Host blocked_keywords triggers "pending" AND plugin "pass" preserves "pending"!
	powResp2, cErr := http.Get(ts.URL + "/api/v1/pow/challenge")
	if cErr != nil {
		t.Fatal(cErr)
	}
	var chal2 guest.PoWChallenge
	_ = json.NewDecoder(powResp2.Body).Decode(&chal2)
	powResp2.Body.Close()
	sol2 := guest.SolveForTest(chal2.Challenge, chal2.DifficultyBits)
	mergedResp, mergedBody := postPublicJSON("/api/v1/comments", map[string]any{
		"target_type":   "article",
		"target_id":     "art_pub",
		"author_name":   "林间读者",
		"content_md":    "这条评论包含 宿主·后台·自定义·屏蔽词 需要进入待审队列",
		"pow_challenge": chal2.Challenge,
		"pow_solution":  sol2,
		"env_fp":        "1920x1080|2|Asia/Shanghai|zh-CN|8",
	}, guestCookies, "")
	if mergedResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 on host-keyword-moderated comment, got %d (%s)", mergedResp.StatusCode, mergedBody)
	}
	var mergedComment struct {
		Comment struct {
			Status string `json:"status"`
		} `json:"comment"`
	}
	_ = json.Unmarshal([]byte(mergedBody), &mergedComment)
	if mergedComment.Comment.Status != "pending" {
		t.Fatalf("expected host blocked_keywords + plugin pass to merge and preserve status=pending, got %q (%s)", mergedComment.Comment.Status, mergedBody)
	}

	// Host blocked_keywords also blocks reader profile nickname updates even though plugin dict doesn't have that phrase!
	if code, body := patchReaderProfile("宿主后台自定义屏蔽词"); code != http.StatusBadRequest {
		t.Fatalf("expected 400 when reader nickname hits host reading.blocked_keywords, got %d (%s)", code, body)
	}
}

func TestAdminPluginPriorityAndChainedInterception_EndToEnd(t *testing.T) {
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

	writeTestPlugin := func(id, manifestJSON, mainJS string) {
		pDir := filepath.Join(pluginsDir, id)
		if err := os.MkdirAll(pDir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pDir, "manifest.json"), []byte(manifestJSON), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pDir, "main.js"), []byte(mainJS), 0o640); err != nil {
			t.Fatal(err)
		}
	}

	writeTestPlugin("chain-alpha", `{
  "id": "chain-alpha",
  "version": "1.0.0",
  "runtime": "goja",
  "entry": "main.js",
  "priority": 50,
  "name": {"zh-CN": "链式插件 Alpha", "en-US": "Chain Alpha"},
  "permissions": ["cron", "hooks:*"],
  "hooks": ["article.before_save", "page.before_save", "media.before_upload"],
  "cron": [{"name": "tick", "interval_seconds": 300, "handler": "onTick"}]
}`, `
function onTick() { return true; }
volo.on("article.before_save", function(p) {
  if (String(p.title || "").indexOf("[BLOCK_ARTICLE]") >= 0) {
    return volo.reject("文章标题触发链式拦截策略", "ARTICLE_POLICY");
  }
  return { content_md: String(p.content_md || "") + "\n<!--alpha-->" };
});
volo.on("page.before_save", function(p) {
  if (String(p.title || "").indexOf("[BLOCK_PAGE]") >= 0) {
    return { allow: false, reason: "独立页面触发拦截策略", code: "PAGE_POLICY" };
  }
  return { content_md: String(p.content_md || "") + "\n<!--page-alpha-->" };
});
volo.on("media.before_upload", function(p) {
  if (/\.exe$/i.test(String(p.filename || ""))) {
    return volo.reject("禁止上传可执行文件", "MEDIA_EXT_BLOCKED");
  }
});
`)

	writeTestPlugin("chain-beta", `{
  "id": "chain-beta",
  "version": "1.0.0",
  "runtime": "goja",
  "entry": "main.js",
  "priority": 80,
  "name": {"zh-CN": "链式插件 Beta", "en-US": "Chain Beta"},
  "permissions": ["hooks:article.*"],
  "hooks": ["article.before_save"]
}`, `
volo.on("article.before_save", function(p) {
  return { content_md: String(p.content_md || "") + "\n<!--beta-->" };
});
`)

	secret := "0123456789abcdef0123456789abcdef-chain-admin"
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
		t.Fatal(err)
	}

	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(dataDir, "volo-chain-admin.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatal(err)
	}

	cat, err := i18n.NewCatalog("zh-CN", "")
	if err != nil {
		t.Fatal(err)
	}
	settingsMgr := repo.NewSettingsManager(store, cfg.KeyRing, "zh-CN")
	_, _ = settingsMgr.Reload(ctx)
	_, _, _ = settingsMgr.PatchDomain(ctx, "site", []byte(`{"initialized":true}`), 0)

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
	if err := pluginMgr.Enable(ctx, "chain-alpha"); err != nil {
		t.Fatal(err)
	}
	if err := pluginMgr.Enable(ctx, "chain-beta"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pluginMgr.Close(ctx) })

	appSrv, err := server.NewAppServer(server.AppDeps{
		Config:   cfg,
		Store:    store,
		Settings: settingsMgr,
		Catalog:  cat,
		Plugins:  pluginMgr,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = appSrv.Close(ctx) })
	ts := httptest.NewServer(appSrv.Handler())
	t.Cleanup(ts.Close)

	now := time.Now().UnixMilli()
	if _, err := store.ExecContext(ctx,
		`INSERT INTO users (id, username, email, display_name, avatar_url, bio, website_url, role, password_hash, legacy_hash, status, email_verified_at, preferred_locale, created_at, updated_at)
		 VALUES ('u_admin', 'admin', 'admin@example.com', 'Admin', '', '', '', 'admin', 'x', '', 'active', ?, 'zh-CN', ?, ?)`,
		now, now, now); err != nil {
		t.Fatal(err)
	}
	sess, rawTok, err := appSrv.Deps().Auth.CreateSession(ctx, "u_admin", netip.MustParseAddr("127.0.0.1"), "", "GoTest", 0)
	if err != nil {
		t.Fatal(err)
	}
	csrf := appSrv.Deps().Auth.ComputeCSRFToken(sess.ID)
	adminCookie := &http.Cookie{Name: auth.SessionCookieName, Value: rawTok}

	doAdminJSON := func(method, path string, body any) (int, string) {
		var rdr io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		}
		req, rErr := http.NewRequest(method, ts.URL+path, rdr)
		if rErr != nil {
			t.Fatal(rErr)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Origin", pubURL)
		req.Header.Set("X-CSRF-Token", csrf)
		req.AddCookie(adminCookie)
		resp, dErr := http.DefaultClient.Do(req)
		if dErr != nil {
			t.Fatal(dErr)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(raw)
	}

	// 1. Default priority order: chain-alpha (50) runs BEFORE chain-beta (80).
	code, artBody := doAdminJSON(http.MethodPost, "/api/admin/articles", map[string]any{
		"title":      "Order Test 1",
		"content_md": "Hello",
		"status":     "draft",
	})
	var artResp1 struct {
		Article struct {
			ContentMD string `json:"content_md"`
		} `json:"article"`
	}
	_ = json.Unmarshal([]byte(artBody), &artResp1)
	if code != http.StatusCreated || artResp1.Article.ContentMD != "Hello\n<!--alpha-->\n<!--beta-->" {
		t.Fatalf("expected alpha -> beta chain order in saved article, got %d content=%q (%s)", code, artResp1.Article.ContentMD, artBody)
	}

	// 2. Reorder via POST /api/admin/plugins/reorder so chain-beta runs BEFORE chain-alpha!
	rCode, rBody := doAdminJSON(http.MethodPost, "/api/admin/plugins/reorder", map[string]any{
		"ordered_ids": []string{"chain-beta", "chain-alpha"},
	})
	if rCode != http.StatusOK {
		t.Fatalf("expected 200 on /api/admin/plugins/reorder, got %d (%s)", rCode, rBody)
	}
	code2, artBody2 := doAdminJSON(http.MethodPost, "/api/admin/articles", map[string]any{
		"title":      "Order Test 2",
		"content_md": "Hello",
		"status":     "draft",
	})
	var artResp2 struct {
		Article struct {
			ContentMD string `json:"content_md"`
		} `json:"article"`
	}
	_ = json.Unmarshal([]byte(artBody2), &artResp2)
	if code2 != http.StatusCreated || artResp2.Article.ContentMD != "Hello\n<!--beta-->\n<!--alpha-->" {
		t.Fatalf("expected beta -> alpha chain order after reorder, got %d content=%q (%s)", code2, artResp2.Article.ContentMD, artBody2)
	}

	// 3. Explicit numeric priority via PATCH /api/admin/plugins/chain-alpha/priority -> priority 5.
	pCode, pBody := doAdminJSON(http.MethodPatch, "/api/admin/plugins/chain-alpha/priority", map[string]any{
		"priority": 5,
	})
	if pCode != http.StatusOK || !strings.Contains(pBody, `"priority":5`) {
		t.Fatalf("expected 200 with priority=5, got %d (%s)", pCode, pBody)
	}

	// 4. Pre-action chained interception aborts article.before_save, page.before_save, and media.before_upload with 400!
	bArtCode, bArtBody := doAdminJSON(http.MethodPost, "/api/admin/articles", map[string]any{
		"title":      "[BLOCK_ARTICLE] Forbidden Draft",
		"content_md": "Body",
		"status":     "draft",
	})
	if bArtCode != http.StatusBadRequest || !strings.Contains(bArtBody, "ARTICLE_POLICY") {
		t.Fatalf("expected 400 ARTICLE_POLICY on intercepted article save, got %d (%s)", bArtCode, bArtBody)
	}

	bPageCode, bPageBody := doAdminJSON(http.MethodPost, "/api/admin/pages", map[string]any{
		"title":      "[BLOCK_PAGE] Bad Page",
		"content_md": "Body",
	})
	if bPageCode != http.StatusBadRequest || !strings.Contains(bPageBody, "PAGE_POLICY") {
		t.Fatalf("expected 400 PAGE_POLICY on intercepted page save, got %d (%s)", bPageCode, bPageBody)
	}

	// Multipart media upload interception before touching disk:
	var mpBuf bytes.Buffer
	boundary := "----VoloTestBoundary7MA4YWxkTrZu0gW"
	mpBuf.WriteString("--" + boundary + "\r\n")
	mpBuf.WriteString(`Content-Disposition: form-data; name="file"; filename="malware.exe"` + "\r\n")
	mpBuf.WriteString("Content-Type: application/octet-stream\r\n\r\nMZ-fake\r\n")
	mpBuf.WriteString("--" + boundary + "--\r\n")
	upReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/admin/media/upload", &mpBuf)
	upReq.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	upReq.Header.Set("Origin", pubURL)
	upReq.Header.Set("X-CSRF-Token", csrf)
	upReq.AddCookie(adminCookie)
	upResp, err := http.DefaultClient.Do(upReq)
	if err != nil {
		t.Fatal(err)
	}
	upBytes, _ := io.ReadAll(upResp.Body)
	upResp.Body.Close()
	if upResp.StatusCode != http.StatusBadRequest || !strings.Contains(string(upBytes), "MEDIA_EXT_BLOCKED") {
		t.Fatalf("expected 400 MEDIA_EXT_BLOCKED on intercepted media upload, got %d (%s)", upResp.StatusCode, string(upBytes))
	}

	// 5. Trigger plugin cron endpoint returns 200 OK with triggered=1.
	cCode, cBody := doAdminJSON(http.MethodPost, "/api/admin/plugins/chain-alpha/trigger-cron", map[string]any{"name": "tick"})
	if cCode != http.StatusOK || !strings.Contains(cBody, `"triggered":1`) {
		t.Fatalf("expected 200 triggered=1 on trigger-cron, got %d (%s)", cCode, cBody)
	}
}

func TestExistingSensitiveCommentsAndNicknames_MaskedOnDisplayWithoutAnomaly(t *testing.T) {
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

	copyPluginTreeForServerTest(
		t,
		filepath.Join("..", "..", "plugins", "sensitive-words"),
		filepath.Join(pluginsDir, "sensitive-words"),
	)

	secret := "0123456789abcdef0123456789abcdef-mask-existing-e2e"
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
		DSN:    filepath.Join(dataDir, "volo-mask-existing.db"),
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
	if _, _, err := settingsMgr.PatchDomain(ctx, "reading", []byte(`{"builtin_spam_filter":false,"pow_difficulty_bits":4}`), 0); err != nil {
		t.Fatal(err)
	}

	pluginMgr := plugin.NewManager(plugin.ManagerOptions{
		Store:      store,
		Keys:       cfg.KeyRing,
		PluginsDir: pluginsDir,
		DataDir:    dataDir,
		Catalog:    cat,
	})
	// Start with sensitive-words DISABLED so historical comments & caches warm with raw text first!
	if err := pluginMgr.DiscoverAndStart(ctx); err != nil {
		t.Fatalf("DiscoverAndStart: %v", err)
	}
	t.Cleanup(func() { _ = pluginMgr.Close(ctx) })

	passports := guest.NewPassportManager(cfg.KeyRing, cfg.PublicURL)
	pow := guest.NewPoWVerifier(cfg.KeyRing)
	rnd := render.Default()
	grd := visibility.NewGuard(cfg.KeyRing, settingsMgr, passports, cat, rnd)
	srch := visibility.NewSearchIndex(store, rnd, 64*1024)
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

	if _, _, err := settingsMgr.PatchDomain(ctx, "reading", []byte(`{"allow_reader_register":true,"pow_difficulty_bits":4}`), 0); err != nil {
		t.Fatal(err)
	}

	var lastCode atomic.Value
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
			lastCode.Store(code)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewAppServer: %v", err)
	}
	t.Cleanup(func() { _ = appSrv.Close(ctx) })
	ts := httptest.NewServer(appSrv.Handler())
	t.Cleanup(ts.Close)

	now := time.Now().UnixMilli()
	// 1. Real admin user whose official DisplayName is "站长"
	if _, err := store.ExecContext(ctx,
		`INSERT INTO users (id, username, email, display_name, avatar_url, bio, website_url, role, password_hash, legacy_hash, status, email_verified_at, preferred_locale, created_at, updated_at)
		 VALUES ('u_admin', 'admin', 'admin@example.com', '站长', '', '', '', 'admin', 'x', '', 'active', ?, 'zh-CN', ?, ?)`,
		now, now, now); err != nil {
		t.Fatal(err)
	}
	// 2. Pre-existing registered reader ("野生博主阿强") created BEFORE sensitive-words is enabled!
	postJSON := func(path string, payload any) (*http.Response, string) {
		rawB, _ := json.Marshal(payload)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader(rawB))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", pubURL)
		resp, dErr := http.DefaultClient.Do(req)
		if dErr != nil {
			t.Fatal(dErr)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}
	if scResp, scBody := postJSON("/api/v1/reader/send-code", map[string]string{
		"email":    "legacy@example.com",
		"username": "野生博主阿强",
	}); scResp.StatusCode != http.StatusOK {
		t.Fatalf("send-code before plugin enable failed: %d (%s)", scResp.StatusCode, scBody)
	}
	verifyCode, _ := lastCode.Load().(string)
	regResp, regBody := postJSON("/api/v1/reader/register", map[string]string{
		"username": "野生博主阿强",
		"email":    "legacy@example.com",
		"password": "LegacyReaderPassword!123",
		"code":     verifyCode,
	})
	if regResp.StatusCode != http.StatusCreated {
		t.Fatalf("register legacy reader before plugin enable failed: %d (%s)", regResp.StatusCode, regBody)
	}
	readerCookies := regResp.Cookies()
	var regInfo struct {
		CSRFToken string `json:"csrf_token"`
	}
	_ = json.Unmarshal([]byte(regBody), &regInfo)

	if _, err := store.ExecContext(ctx,
		`INSERT INTO categories (id, name, slug, description, icon, sort_order, created_at, updated_at)
		 VALUES ('cat_1', 'Tech', 'tech', '', '', 1, ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExecContext(ctx,
		`INSERT INTO articles (
			id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
			cover_url, status, is_pinned, allow_comment, password_ciphertext, has_content_lock,
			audio_url, audio_duration_sec, audio_size_bytes, audio_mime,
			lang, translation_group, license_override, word_count, reading_minutes, view_count, comment_count,
			published_at, created_at, updated_at
		) VALUES ('art_hist', 'u_admin', 'cat_1', 'History Post', 'history-post', '', 'sum', 'body', '', 'published', 0, 1, '', 0,
		          '', 0, 0, '', 'zh-CN', '', '', 10, 1, 0, 2, ?, ?, ?)`,
		now, now, now); err != nil {
		t.Fatal(err)
	}

	// Historical guest comment containing sensitive words, cross-tag split word, safe URL containing keyword, allowlisted word, and guest impersonator handle "站长"
	rawGuestMD := `历史遗留傻逼言论，傻**逼**跨标签，还有[傻瓜相机](https://example.com/傻逼?x=1&y=2)推荐`
	if _, err := store.ExecContext(ctx,
		`INSERT INTO comments (
			id, target_type, target_id, parent_id, reply_to_name, user_id, guest_id_hash, env_fp_hash,
			author_name, author_url, avatar_url, notify_email_ciphertext, notify_locale,
			client_ip, ip_masked, geo_region, content_md, status, is_deleted, created_at, updated_at
		) VALUES ('c_guest_hist', 'article', 'art_hist', '', '', '', 'gh_1', '',
		          '站长', '', '', '', 'zh-CN',
		          '1.2.3.4', '1.2.3.*', '', ?, 'approved', 0, ?, ?)`,
		rawGuestMD, now-2000, now-2000); err != nil {
		t.Fatal(err)
	}
	// Historical genuine admin comment with author_name "站长"
	if _, err := store.ExecContext(ctx,
		`INSERT INTO comments (
			id, target_type, target_id, parent_id, reply_to_name, user_id, guest_id_hash, env_fp_hash,
			author_name, author_url, avatar_url, notify_email_ciphertext, notify_locale,
			client_ip, ip_masked, geo_region, content_md, status, is_deleted, created_at, updated_at
		) VALUES ('c_admin_hist', 'article', 'art_hist', '', '', 'u_admin', '', '',
		          '站长', '', '', '', 'zh-CN',
		          '1.2.3.4', '1.2.3.*', '', '我是真正的站长，感谢大家阅读！', 'approved', 0, ?, ?)`,
		now-1000, now-1000); err != nil {
		t.Fatal(err)
	}

	fetchComments := func() []content.PublicCommentView {
		resp, gErr := http.Get(ts.URL + "/api/v1/comments?target_type=article&target_id=art_hist")
		if gErr != nil {
			t.Fatal(gErr)
		}
		defer resp.Body.Close()
		var out content.PaginatedCommentsResult
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode comments: %v", err)
		}
		return out.Comments
	}

	findComment := func(items []content.PublicCommentView, wantID string) content.PublicCommentView {
		for _, it := range items {
			if it.ID == wantID {
				return it
			}
		}
		t.Fatalf("comment %q not found in %+v", wantID, items)
		return content.PublicCommentView{}
	}

	// Step A: Before enabling sensitive-words, warm both commentHTML cache and RecentComments chrome cache.
	beforeItems := fetchComments()
	beforeGuest := findComment(beforeItems, "c_guest_hist")
	if !strings.Contains(beforeGuest.ContentHTML, "傻逼") || beforeGuest.AuthorName != "站长" {
		t.Fatalf("expected raw unmasked historical comment before enabling plugin, got %+v", beforeGuest)
	}
	recentBefore, err := cnt.ListRecentPublicComments(ctx, 10)
	if err != nil || len(recentBefore) < 2 || !strings.Contains(recentBefore[1].Excerpt, "傻逼") {
		t.Fatalf("expected raw recent excerpt before enabling plugin (err=%v), got %+v", err, recentBefore)
	}

	// Step B: Enable sensitive-words at runtime -> caches must immediately invalidate and mask with * without corrupting HTML or real admin handle!
	if err := pluginMgr.Enable(ctx, "sensitive-words"); err != nil {
		t.Fatalf("Enable(sensitive-words): %v", err)
	}
	afterItems := fetchComments()
	guestView := findComment(afterItems, "c_guest_hist")
	adminView := findComment(afterItems, "c_admin_hist")

	if guestView.AuthorName != "**" {
		t.Fatalf("expected guest impersonator nickname '站长' masked to '**', got %q", guestView.AuthorName)
	}
	if strings.Contains(guestView.ContentHTML, "遗留傻逼") || strings.Contains(guestView.ContentHTML, "傻<strong>逼</strong>") {
		t.Fatalf("expected historical sensitive words & split-tag words masked in HTML, got %q", guestView.ContentHTML)
	}
	if !strings.Contains(guestView.ContentHTML, "遗留**言论") || !strings.Contains(guestView.ContentHTML, "*<strong>*</strong>") {
		t.Fatalf("expected ** and *<strong>*</strong> in masked HTML, got %q", guestView.ContentHTML)
	}
	if !strings.Contains(guestView.ContentHTML, `href="https://example.com/傻逼?x=1&amp;y=2"`) &&
		!strings.Contains(guestView.ContentHTML, `href="https://example.com/%E5%82%BB%E9%80%BC?x=1&amp;y=2"`) {
		t.Fatalf("expected <a href> URL attribute preserved without corruption, got %q", guestView.ContentHTML)
	}
	if !strings.Contains(guestView.ContentHTML, ">傻瓜相机</a>") {
		t.Fatalf("expected allowlisted phrase '傻瓜相机' preserved intact, got %q", guestView.ContentHTML)
	}
	if adminView.AuthorName != "站长" || !adminView.IsAuthor {
		t.Fatalf("expected genuine admin/author display name '站长' NOT masked, got author=%q is_author=%v", adminView.AuthorName, adminView.IsAuthor)
	}

	// Recent comments feed must also reflect masked guest nickname & masked excerpt immediately!
	recentAfter, err := cnt.ListRecentPublicComments(ctx, 10)
	if err != nil {
		t.Fatalf("ListRecentPublicComments after enable: %v", err)
	}
	var guestRecent *content.RecentCommentItem
	for i := range recentAfter {
		if recentAfter[i].ID == "c_guest_hist" {
			guestRecent = &recentAfter[i]
			break
		}
	}
	if guestRecent == nil || guestRecent.AuthorName != "**" || strings.Contains(guestRecent.Excerpt, "傻逼") || !strings.Contains(guestRecent.Excerpt, "遗留**言论") {
		t.Fatalf("expected recent comment feed masked immediately, got %+v", guestRecent)
	}

	// Verify non-destructive storage: SQLite row still holds original raw text.
	var storedMD, storedAuthor string
	if err := store.QueryRowContext(ctx, `SELECT content_md, author_name FROM comments WHERE id = 'c_guest_hist'`).Scan(&storedMD, &storedAuthor); err != nil {
		t.Fatal(err)
	}
	if storedMD != rawGuestMD || storedAuthor != "站长" {
		t.Fatalf("expected database row untouched, got md=%q author=%q", storedMD, storedAuthor)
	}

	// Step C: Pre-existing registered reader ("野生博主阿强") can post a clean comment without lockout, and displayed author_name masks "博主" -> "**"!
	postReaderComment := func(md string) (*http.Response, string) {
		rawBody, _ := json.Marshal(map[string]any{
			"target_type": "article",
			"target_id":   "art_hist",
			"content_md":  md,
		})
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/comments", bytes.NewReader(rawBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", pubURL)
		req.Header.Set("X-CSRF-Token", regInfo.CSRFToken)
		for _, ck := range readerCookies {
			req.AddCookie(ck)
		}
		resp, dErr := http.DefaultClient.Do(req)
		if dErr != nil {
			t.Fatal(dErr)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}

	okResp, okBody := postReaderComment("感谢站长的精彩文章，收获很大！")
	if okResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected pre-existing registered reader clean comment to succeed (201), got %d (%s)", okResp.StatusCode, okBody)
	}
	var createdWrap struct {
		Comment content.PublicCommentView `json:"comment"`
	}
	_ = json.Unmarshal([]byte(okBody), &createdWrap)
	if createdWrap.Comment.AuthorName != "野生**阿强" {
		t.Fatalf("expected pre-existing reader restricted substring masked to '野生**阿强', got %q", createdWrap.Comment.AuthorName)
	}

	badResp, _ := postReaderComment("这里发一句傻逼试试")
	if badResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected pre-existing reader sensitive comment body still rejected (403), got %d", badResp.StatusCode)
	}

	// Step D: Disable plugin & verify built-in host keyword filter also masks existing content when enabled, and restores raw display when both are off.
	if err := pluginMgr.Disable(ctx, "sensitive-words"); err != nil {
		t.Fatalf("Disable(sensitive-words): %v", err)
	}
	restoredGuest := findComment(fetchComments(), "c_guest_hist")
	if !strings.Contains(restoredGuest.ContentHTML, "遗留傻逼言论") || restoredGuest.AuthorName != "站长" {
		t.Fatalf("expected original unmasked display restored when plugin disabled, got %+v", restoredGuest)
	}

	if _, _, err := settingsMgr.PatchDomain(ctx, "reading", []byte(`{"builtin_spam_filter":true,"blocked_keywords":["遗留傻逼"]}`), 0); err != nil {
		t.Fatal(err)
	}
	hostMaskedGuest := findComment(fetchComments(), "c_guest_hist")
	if !strings.Contains(hostMaskedGuest.ContentHTML, "****言论") {
		t.Fatalf("expected host builtin_spam_filter blocked_keywords to mask existing comment HTML, got %q", hostMaskedGuest.ContentHTML)
	}
}


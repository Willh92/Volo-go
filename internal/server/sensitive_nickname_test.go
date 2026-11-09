package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

	// 1. Reader send-code fast-fails on reserved or sensitive candidate nicknames BEFORE dispatching mail.
	for _, badUser := range []string{"系统管理员", "傻逼青年"} {
		scPayload, _ := json.Marshal(map[string]string{
			"email":    "reader@example.com",
			"username": badUser,
		})
		resp, pErr := http.Post(ts.URL+"/api/v1/reader/send-code", "application/json", bytes.NewReader(scPayload))
		if pErr != nil {
			t.Fatal(pErr)
		}
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 on send-code with bad username %q, got %d (%s)", badUser, resp.StatusCode, string(bodyBytes))
		}
	}
	if sentMails.Load() != 0 {
		t.Fatalf("expected 0 verification emails sent when preflight nickname rejected, got %d", sentMails.Load())
	}

	// 2. Clean send-code dispatches 6-digit verification code.
	scClean, pErr := http.Post(ts.URL+"/api/v1/reader/send-code", "application/json", strings.NewReader(`{"email":"reader@example.com","username":"volo_reader"}`))
	if pErr != nil {
		t.Fatal(pErr)
	}
	scClean.Body.Close()
	if scClean.StatusCode != http.StatusOK || sentMails.Load() != 1 {
		t.Fatalf("expected 200 OK and 1 mail sent, got status=%d sent=%d", scClean.StatusCode, sentMails.Load())
	}
	codeStr, _ := lastCode.Load().(string)
	if len(codeStr) != 6 {
		t.Fatalf("expected 6-digit code, got %q", codeStr)
	}

	// 3. Reader registration with offending nickname is rejected 400 WITHOUT burning the 6-digit verification code!
	badRegBody, _ := json.Marshal(map[string]string{
		"username": "官方客服",
		"email":    "reader@example.com",
		"password": "ReaderStrongPassword!123",
		"code":     codeStr,
	})
	badRegResp, pErr := http.Post(ts.URL+"/api/v1/reader/register", "application/json", bytes.NewReader(badRegBody))
	if pErr != nil {
		t.Fatal(pErr)
	}
	badRegBytes, _ := io.ReadAll(badRegResp.Body)
	badRegResp.Body.Close()
	if badRegResp.StatusCode != http.StatusBadRequest || !strings.Contains(string(badRegBytes), "昵称") {
		t.Fatalf("expected 400 localized nickname error on register, got %d (%s)", badRegResp.StatusCode, string(badRegBytes))
	}

	// Retry immediately with a clean username and the SAME 6-digit verification code -> MUST succeed (201 Created)!
	goodRegBody, _ := json.Marshal(map[string]string{
		"username": "volo_reader",
		"email":    "reader@example.com",
		"password": "ReaderStrongPassword!123",
		"code":     codeStr,
	})
	goodRegResp, pErr := http.Post(ts.URL+"/api/v1/reader/register", "application/json", bytes.NewReader(goodRegBody))
	if pErr != nil {
		t.Fatal(pErr)
	}
	goodRegBytes, _ := io.ReadAll(goodRegResp.Body)
	goodRegResp.Body.Close()
	if goodRegResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created when retrying with preserved verification code, got %d (%s)", goodRegResp.StatusCode, string(goodRegBytes))
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

		payload, _ := json.Marshal(map[string]any{
			"target_type":   "article",
			"target_id":     "art_pub",
			"author_name":   authorName,
			"content_md":    contentMD,
			"pow_challenge": chal.Challenge,
			"pow_solution":  sol,
			"env_fp":        "1920x1080|2|Asia/Shanghai|zh-CN|8",
		})
		resp, rErr := http.Post(ts.URL+"/api/v1/comments", "application/json", bytes.NewReader(payload))
		if rErr != nil {
			t.Fatal(rErr)
		}
		cookies := resp.Cookies()
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(raw), cookies
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
}

package server_test

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/media"
	"github.com/Willh92/Volo-go/internal/server"
	"github.com/Willh92/Volo-go/internal/theme"
	"github.com/Willh92/Volo-go/pkg/id"
)

func bootstrapAuthenticatedAdmin(t *testing.T, env *m5Env) ([]*http.Cookie, map[string]string) {
	t.Helper()
	resp, raw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/setup/complete", map[string]any{
		"site_title":         "Volo",
		"site_subtitle":      "记录 · 思考 · 分享",
		"site_description":   "由 Volo-go 驱动的独立个人博客。",
		"default_locale":     "zh-CN",
		"timezone":           "UTC",
		"accent":             "teal",
		"admin_username":     "admin",
		"admin_email":        "admin@example.com",
		"admin_display_name": "Admin",
		"admin_password":     "Str0ng!Pass#2026",
	}, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/setup/complete = %d: %s", resp.StatusCode, string(raw))
	}
	var compRes struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(raw, &compRes); err != nil || compRes.CSRFToken == "" {
		t.Fatalf("missing csrf_token in setup response: %s", string(raw))
	}
	headers := map[string]string{
		"Origin":       "https://studio.example.com",
		"X-CSRF-Token": compRes.CSRFToken,
	}
	return resp.Cookies(), headers
}

func TestFollowUp_CustomLocalUploadDirFallbackAndContainment(t *testing.T) {
	env := setupM5TestEnv(t, "/console")
	env.cfg.UploadDirPinned = false
	// Wire full media.Pipeline as well to exercise dynamic SetUploadDir + LocalKeyPrefix
	pipe := media.NewPipeline(media.PipelineOptions{
		Store:     env.store,
		Settings:  env.settings,
		UploadDir: env.cfg.GetUploadDir(),
	})
	appWithMedia, err := server.NewAppServer(server.AppDeps{
		Config:    env.cfg,
		Store:     env.store,
		Settings:  env.settings,
		Catalog:   env.catalog,
		Auth:      env.authSvc,
		Content:   env.content,
		Media:     pipe,
		StartedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("NewAppServer with media pipeline: %v", err)
	}
	env.ts.Config.Handler = appWithMedia.Handler()

	cookies, headers := bootstrapAuthenticatedAdmin(t, env)

	// Place a legacy image inside the default UploadDir before switching local_dir
	legacyRel := "2024/08/legacy-sample.png"
	legacyAbs := filepath.Join(env.cfg.GetDefaultUploadDir(), filepath.FromSlash(legacyRel))
	if err := os.MkdirAll(filepath.Dir(legacyAbs), 0o750); err != nil {
		t.Fatal(err)
	}
	img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	img.Set(2, 2, color.NRGBA{R: 20, G: 140, B: 110, A: 255})
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyAbs, pngBuf.Bytes(), 0o640); err != nil {
		t.Fatal(err)
	}

	// 1. Reject traversal, system dirs, and DataDir containment violations
	for _, badDir := range []string{"../escape-media", "/etc/volo-media", env.cfg.DataDir} {
		resp, raw := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/storage", map[string]any{
			"local_dir": badDir,
		}, cookies, headers)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 rejecting unsafe local_dir %q, got %d (%s)", badDir, resp.StatusCode, string(raw))
		}
	}

	// 2. Configure dedicated custom local_dir + local_key_prefix
	customDir := filepath.Join(filepath.Dir(env.cfg.DataDir), "custom-media-vault")
	resp, raw := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/storage", map[string]any{
		"active_driver":    "local",
		"local_dir":        customDir,
		"local_key_prefix": "gallery/2026",
	}, cookies, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH /api/admin/settings/storage customDir failed: %d %s", resp.StatusCode, string(raw))
	}
	if got := env.cfg.GetUploadDir(); got != filepath.Clean(customDir) {
		t.Fatalf("expected Config.GetUploadDir() = %q, got %q", customDir, got)
	}

	// 3. Upload a new PNG via multipart /api/admin/media/upload -> stored under customDir/gallery/2026/...
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "tiny-icon.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(pngBuf.Bytes()); err != nil {
		t.Fatal(err)
	}
	_ = mw.Close()

	req, err := http.NewRequest(http.MethodPost, env.ts.URL+"/api/admin/media/upload", &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	for _, c := range cookies {
		req.AddCookie(c)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	upResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer upResp.Body.Close()
	var upPayload struct {
		URL   string            `json:"url"`
		Asset domain.MediaAsset `json:"asset"`
	}
	if err := json.NewDecoder(upResp.Body).Decode(&upPayload); err != nil {
		t.Fatal(err)
	}
	if upResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/admin/media/upload = %d", upResp.StatusCode)
	}
	if !strings.HasPrefix(upPayload.Asset.ObjectKey, "gallery/2026/") {
		t.Fatalf("expected ObjectKey prefixed with gallery/2026/, got %q", upPayload.Asset.ObjectKey)
	}
	expectedDiskPath := filepath.Join(customDir, filepath.FromSlash(upPayload.Asset.ObjectKey))
	if _, err := os.Stat(expectedDiskPath); err != nil {
		t.Fatalf("expected uploaded file inside custom local_dir at %s: %v", expectedDiskPath, err)
	}

	// 4. Both the newly uploaded asset (in customDir) AND the pre-existing asset (in DefaultUploadDir) serve 200 OK
	for _, u := range []string{upPayload.URL, "/uploads/" + legacyRel} {
		gResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+u, nil, nil, nil)
		if gResp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s expected 200 OK, got %d", u, gResp.StatusCode)
		}
	}
}

func TestFollowUp_OverviewHistoricalMonthsNoticeHTMLAndTaxonomyBackNav(t *testing.T) {
	env := setupM5TestEnv(t, "/console")
	cookies, headers := bootstrapAuthenticatedAdmin(t, env)

	// 1. Unrestricted NoticeBoard Markdown + HTML rendering on public sidebar
	noticeSnippet := `**公告速递** <span class="custom-notice-chip" style="color:#0a7d6a">Live</span><iframe src="https://example.com/embed" height="36"></iframe>`
	resp, raw := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/site", map[string]any{
		"notice_board": noticeSnippet,
	}, cookies, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH site.notice_board failed: %d %s", resp.StatusCode, string(raw))
	}

	// 2. Add custom/Solo-migrated nav items with empty i18n_titles_json
	for idx, nav := range []map[string]any{
		{"title": "我的开源", "url": "/opensource", "target": "_self", "sort_order": idxOr(1, 0), "is_enabled": true, "i18n_titles_json": "{}"},
		{"title": "友链", "url": "/links", "target": "_self", "sort_order": idxOr(2, 0), "is_enabled": true, "i18n_titles_json": "{}"},
	} {
		nav["sort_order"] = idx + 10
		resp, raw = doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/nav", nav, cookies, headers)
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			t.Fatalf("POST /api/admin/nav failed: %d %s", resp.StatusCode, string(raw))
		}
	}

	// 3. Create published articles backdated > 12 months ago (2021-07 & 2022-05) with a tag so we test both Overview & /tags/{slug}
	t2021 := time.Date(2021, 7, 15, 10, 0, 0, 0, time.UTC).UnixMilli()
	t2022 := time.Date(2022, 5, 20, 10, 0, 0, 0, time.UTC).UnixMilli()
	for i, ts := range []int64{t2021, t2022} {
		resp, raw = doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
			"title":        "Historical Post " + id.Next()[:6],
			"slug":         "hist-post-" + string(rune('a'+i)),
			"raw_markdown": "Hello world from historical archive.",
			"status":       "published",
			"visibility":   "public",
			"published_at": ts,
			"tags":         []string{"golang"},
		}, cookies, headers)
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			t.Fatalf("POST /api/admin/articles failed: %d %s", resp.StatusCode, string(raw))
		}
	}

	// 4. Verify /api/admin/overview returns monthly_history = [2021-07, 2022-05] and non-empty anchored monthly_articles
	resp, ovRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/overview", nil, cookies, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/admin/overview = %d %s", resp.StatusCode, string(ovRaw))
	}
	var ov struct {
		MonthlyHistory []struct {
			Month string `json:"month"`
			Count int    `json:"count"`
		} `json:"monthly_history"`
		MonthlyArticles []struct {
			Month string `json:"month"`
			Count int    `json:"count"`
		} `json:"monthly_articles"`
	}
	if err := json.Unmarshal(ovRaw, &ov); err != nil {
		t.Fatal(err)
	}
	if len(ov.MonthlyHistory) != 2 || ov.MonthlyHistory[0].Month != "2021-07" || ov.MonthlyHistory[1].Month != "2022-05" {
		t.Fatalf("unexpected monthly_history: %+v", ov.MonthlyHistory)
	}
	anchoredSum := 0
	for _, b := range ov.MonthlyArticles {
		anchoredSum += b.Count
	}
	if anchoredSum == 0 || ov.MonthlyArticles[len(ov.MonthlyArticles)-1].Month != "2022-05" {
		t.Fatalf("expected monthly_articles anchored at 2022-05 with non-zero sum, got %+v", ov.MonthlyArticles)
	}

	// 5. Verify Public Homepage renders unrestricted NoticeBoard HTML + localized nav/subtitle in en-US
	_, homeZH := doJSONReq(t, http.MethodGet, env.ts.URL+"/", nil, nil, nil)
	zhStr := string(homeZH)
	if !strings.Contains(zhStr, `class="custom-notice-chip"`) || !strings.Contains(zhStr, `<iframe src="https://example.com/embed"`) {
		t.Fatalf("expected unrestricted HTML/iframe inside public NoticeBoard, got snippet missing in home HTML")
	}

	_, homeEN := doJSONReq(t, http.MethodGet, env.ts.URL+"/?lang=en-US", nil, nil, nil)
	enStr := string(homeEN)
	for _, want := range []string{"Open Source", "Links", "Record · Reflect · Share"} {
		if !strings.Contains(enStr, want) {
			t.Fatalf("expected localized %q in /?lang=en-US HTML", want)
		}
	}

	// 6. Verify /tags/golang renders localized back navigation pill to /tags
	tagResp, tagHTML := doJSONReq(t, http.MethodGet, env.ts.URL+"/tags/golang", nil, nil, nil)
	if tagResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /tags/golang = %d", tagResp.StatusCode)
	}
	for _, want := range []string{"Historical Post", `href="/tags"`, "返回全部标签"} {
		if !strings.Contains(string(tagHTML), want) {
			t.Fatalf("expected %q on /tags/golang HTML", want)
		}
	}
	_, tagEN := doJSONReq(t, http.MethodGet, env.ts.URL+"/tags/golang?lang=en-US", nil, nil, nil)
	if !strings.Contains(string(tagEN), "All Tags") {
		t.Fatalf("expected localized 'All Tags' back pill on /tags/golang?lang=en-US")
	}
}

func TestFollowUp_ArchiveTimezoneCanonicalAuthI18nCustomNavAndStealthMount(t *testing.T) {
	env := setupM5TestEnv(t, "/console")
	if err := env.catalog.Reload("../../locales"); err != nil {
		t.Fatalf("catalog.Reload(../../locales): %v", err)
	}
	cookies, headers := bootstrapAuthenticatedAdmin(t, env)

	// =========================================================================
	// Part A: Admin Auth Login Error i18n (zh-CN / zh-TW / en-US, zero raw "auth: ...")
	// =========================================================================
	for _, tc := range []struct {
		acceptLang string
		wantSubstr string
	}{
		{"zh-CN", "用户名/邮箱或密码不正确"},
		{"zh-TW", "使用者名稱/信箱或密碼不正確"},
		{"en-US", "Invalid username/email or password."},
	} {
		lResp, lRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/auth/login", map[string]any{
			"identifier": "admin",
			"password":   "WrongPassword#999",
		}, nil, map[string]string{
			"Origin":          "https://studio.example.com",
			"Accept-Language": tc.acceptLang,
		})
		if lResp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("login wrong password (%s) = %d: %s", tc.acceptLang, lResp.StatusCode, string(lRaw))
		}
		bodyStr := string(lRaw)
		if strings.Contains(bodyStr, "auth:") || !strings.Contains(bodyStr, tc.wantSubstr) {
			t.Fatalf("expected localized error %q without raw 'auth:' prefix for %s, got: %s", tc.wantSubstr, tc.acceptLang, bodyStr)
		}
	}

	// =========================================================================
	// Part B: Archive Timezone Consistency (Asia/Shanghai UTC+8 boundary + published_at=0 fallback + 301 canonicalization)
	// =========================================================================
	resp, raw := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/site", map[string]any{
		"timezone": "Asia/Shanghai",
	}, cookies, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH site.timezone=Asia/Shanghai failed: %d %s", resp.StatusCode, string(raw))
	}

	// 2023-05-31 17:15:00 UTC == 2023-06-01 01:15:00 Asia/Shanghai (no May 2023 article exists!)
	shanghaiBoundary := time.Date(2023, 5, 31, 17, 15, 0, 0, time.UTC).UnixMilli()
	resp, raw = doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"title":        "Shanghai Boundary Post",
		"slug":         "shanghai-boundary-post",
		"raw_markdown": "Published at 01:15 CST on June 1st.",
		"status":       "published",
		"visibility":   "public",
		"published_at": shanghaiBoundary,
	}, cookies, headers)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("POST boundary article failed: %d %s", resp.StatusCode, string(raw))
	}

	// Also seed a legacy published row with published_at = 0 and created_at in 2020-11 (Asia/Shanghai)
	legacyCreatedAt := time.Date(2020, 11, 10, 8, 0, 0, 0, time.UTC).UnixMilli()
	resp, raw = doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"title":        "Zero PublishedAt Post",
		"slug":         "zero-published-at",
		"raw_markdown": "Legacy body with zero published_at.",
		"status":       "published",
	}, cookies, headers)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("POST zero-published-at article failed: %d %s", resp.StatusCode, string(raw))
	}
	if _, err := env.store.ExecContext(env.ctx, `UPDATE articles SET published_at = 0, created_at = ? WHERE slug = ?`, legacyCreatedAt, "zero-published-at"); err != nil {
		t.Fatalf("UPDATE published_at=0 article: %v", err)
	}

	archResp, archHTML := doJSONReq(t, http.MethodGet, env.ts.URL+"/archives", nil, nil, nil)
	if archResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /archives = %d", archResp.StatusCode)
	}
	archStr := string(archHTML)
	if !strings.Contains(archStr, `/archives/2023-06`) || strings.Contains(archStr, `/archives/2023-05`) {
		t.Fatalf("expected /archives to bucket boundary post into /archives/2023-06 (not 2023-05)")
	}
	if !strings.Contains(archStr, `/archives/2020-11`) {
		t.Fatalf("expected /archives to include published_at=0 article under /archives/2020-11")
	}

	for _, monthURL := range []string{"/archives/2023-06", "/archives/2020-11"} {
		mResp, mBody := doJSONReq(t, http.MethodGet, env.ts.URL+monthURL, nil, nil, nil)
		if mResp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s expected 200 OK, got %d (%s)", monthURL, mResp.StatusCode, string(mBody))
		}
	}

	// Non-canonical and legacy archive URLs must 301 redirect cleanly instead of returning 404
	for src, wantLoc := range map[string]string{
		"/archives/":          "/archives",
		"/archives/2023":      "/archives?year=2023",
		"/archives/2023/06":   "/archives/2023-06",
		"/archives/2023-6":    "/archives/2023-06",
		"/archive":            "/archives",
		"/archive.html":       "/archives",
		"/archive/2023/06":    "/archives/2023-06",
		"/archivedate/2023/6": "/archives/2023-06",
	} {
		rResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+src, nil, nil, nil)
		if rResp.StatusCode != http.StatusMovedPermanently || rResp.Header.Get("Location") != wantLoc {
			t.Fatalf("GET %s = (%d, Location=%q), want (301, %q)", src, rResp.StatusCode, rResp.Header.Get("Location"), wantLoc)
		}
	}

	// =========================================================================
	// Part C: Split Custom Navigation Items + Free Interleaving + Safe Deletion
	// =========================================================================
	createNav := func(title, urlStr string, order int) string {
		nResp, nRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/nav", map[string]any{
			"title":      title,
			"url":        urlStr,
			"target":     "_self",
			"sort_order": order,
			"is_enabled": true,
		}, cookies, headers)
		if nResp.StatusCode != http.StatusCreated && nResp.StatusCode != http.StatusOK {
			t.Fatalf("POST /api/admin/nav (%s) failed: %d %s", title, nResp.StatusCode, string(nRaw))
		}
		var created struct {
			Item domain.NavItem `json:"item"`
		}
		if err := json.Unmarshal(nRaw, &created); err != nil || created.Item.ID == "" {
			t.Fatalf("decode created nav item: %v / %s", err, string(nRaw))
		}
		return created.Item.ID
	}
	navAboutID := createNav("关于本站", "/about-studio", 1)
	navWorksID := createNav("精选作品", "/works-showcase", 2)

	// GET /api/admin/themes must expose individual "custom:<id>" options in header_nav_items schema
	_, thRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/themes", nil, cookies, nil)
	if !strings.Contains(string(thRaw), `"custom:`+navAboutID+`"`) || !strings.Contains(string(thRaw), `"custom:`+navWorksID+`"`) {
		t.Fatalf("expected GET /api/admin/themes to include individual custom:<id> options, got: %s", string(thRaw))
	}

	// Interleave custom:<navWorksID> BETWEEN home and archives while leaving navAboutID unchecked
	putResp, putRaw := doJSONReq(t, http.MethodPut, env.ts.URL+"/api/admin/themes/volo-studio/config", map[string]any{
		"header_nav_items":         []string{"home", "custom:" + navWorksID, "archives"},
		"header_nav_primary_count": 5,
	}, cookies, headers)
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /api/admin/themes/volo-studio/config failed: %d %s", putResp.StatusCode, string(putRaw))
	}

	_, homeAfterOrder := doJSONReq(t, http.MethodGet, env.ts.URL+"/", nil, nil, nil)
	hStr := string(homeAfterOrder)
	idxHome := strings.Index(hStr, `href="/"`)
	idxWorks := strings.Index(hStr, `href="/works-showcase"`)
	idxArch := strings.Index(hStr, `href="/archives"`)
	if idxHome < 0 || idxWorks < 0 || idxArch < 0 || !(idxHome < idxWorks && idxWorks < idxArch) {
		t.Fatalf("expected public header order home -> /works-showcase -> /archives, got indices (%d, %d, %d)", idxHome, idxWorks, idxArch)
	}
	if strings.Contains(hStr, `href="/about-studio"`) {
		t.Fatalf("unchecked custom nav /about-studio should remain hidden")
	}

	// Delete navWorksID -> must prune from Appearance.ThemeConfigs and vanish cleanly from public header!
	delResp, delRaw := doJSONReq(t, http.MethodDelete, env.ts.URL+"/api/admin/nav/"+navWorksID, nil, cookies, headers)
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /api/admin/nav/%s failed: %d %s", navWorksID, delResp.StatusCode, string(delRaw))
	}
	appCfg := env.settings.Snapshot().Appearance.ThemeConfigs["volo-studio"]
	cfgBytes, _ := json.Marshal(appCfg["header_nav_items"])
	if strings.Contains(string(cfgBytes), navWorksID) {
		t.Fatalf("expected deleted custom nav ID %s pruned from saved header_nav_items, got %s", navWorksID, string(cfgBytes))
	}
	_, homeAfterDel := doJSONReq(t, http.MethodGet, env.ts.URL+"/", nil, nil, nil)
	if strings.Contains(string(homeAfterDel), `href="/works-showcase"`) {
		t.Fatalf("deleted custom nav /works-showcase must not appear on public homepage")
	}

	// Create a brand-new 3rd custom nav item -> must auto-append while unchecked navAboutID stays hidden!
	_ = createNav("常用工具", "/uses-stack", 3)
	_, homeAfterNew := doJSONReq(t, http.MethodGet, env.ts.URL+"/", nil, nil, nil)
	hNew := string(homeAfterNew)
	if !strings.Contains(hNew, `href="/uses-stack"`) || strings.Contains(hNew, `href="/about-studio"`) {
		t.Fatalf("expected newly created /uses-stack auto-appended while unchecked /about-studio stays hidden")
	}

	// =========================================================================
	// Part D: Dynamic Stealth Admin Entry Path (/hal live mount + /console 404 cloaking)
	// =========================================================================
	for _, reserved := range []string{"/api", "/posts", "/archives", "/uploads"} {
		badResp, _ := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/security", map[string]any{
			"dynamic_login_path": reserved,
		}, cookies, headers)
		if badResp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 rejecting reserved dynamic_login_path %q, got %d", reserved, badResp.StatusCode)
		}
	}

	// Save "hal" (without leading slash) -> normalized to "/hal" and live-mounted immediately!
	secResp, secRaw := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/security", map[string]any{
		"dynamic_login_path": "hal",
	}, cookies, headers)
	if secResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH security.dynamic_login_path='hal' failed: %d %s", secResp.StatusCode, string(secRaw))
	}
	if got := env.settings.Snapshot().Security.DynamicLoginPath; got != "/hal" {
		t.Fatalf("expected normalized DynamicLoginPath='/hal', got %q", got)
	}

	for _, okPath := range []string{"/hal", "/hal/"} {
		halResp, halBody := doJSONReq(t, http.MethodGet, env.ts.URL+okPath, nil, nil, nil)
		if halResp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s expected 200 OK, got %d", okPath, halResp.StatusCode)
		}
		halHTML := string(halBody)
		if !strings.Contains(halHTML, `"admin_path":"/hal"`) || !strings.Contains(halHTML, `"base_admin_path":"/console"`) {
			t.Fatalf("expected BootConfig on %s to expose admin_path=/hal and base_admin_path=/console, got: %s", okPath, halHTML)
		}
	}

	// Default /console and /console/ must now be cloaked with 404!
	for _, cloaked := range []string{"/console", "/console/"} {
		cResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+cloaked, nil, nil, nil)
		if cResp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s expected 404 when cloaked behind /hal, got %d", cloaked, cResp.StatusCode)
		}
	}

	// robots.txt must NEVER leak the stealth admin entry path /hal (nor /console)
	_, robotsRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/robots.txt", nil, nil, nil)
	if strings.Contains(string(robotsRaw), "/hal") || strings.Contains(string(robotsRaw), "/console") {
		t.Fatalf("expected /robots.txt NOT to disclose stealth path /hal or /console, got:\n%s", string(robotsRaw))
	}

	// Clearing dynamic_login_path restores /console (200 OK) and returns 404 on /hal
	clrResp, clrRaw := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/security", map[string]any{
		"dynamic_login_path": "",
	}, cookies, headers)
	if clrResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH clear dynamic_login_path failed: %d %s", clrResp.StatusCode, string(clrRaw))
	}
	cRestored, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/console/", nil, nil, nil)
	halOld, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/hal/", nil, nil, nil)
	if cRestored.StatusCode != http.StatusOK || halOld.StatusCode != http.StatusNotFound {
		t.Fatalf("after clearing dynamic_login_path, expected /console/=200 and /hal/=404, got /console/=%d /hal/=%d", cRestored.StatusCode, halOld.StatusCode)
	}
}

func idxOr(a, _ int) int { return a }

func TestCustomErrorPagesAnd404RiskControl(t *testing.T) {
	env := setupM5TestEnv(t, "/console")
	t.Cleanup(func() { _ = env.app.Close(env.ctx) })

	if err := env.catalog.Reload("../../locales"); err != nil {
		t.Fatalf("catalog.Reload(../../locales): %v", err)
	}
	tp, err := config.ParseTrustedProxies("127.0.0.1, ::1")
	if err != nil {
		t.Fatalf("ParseTrustedProxies: %v", err)
	}
	env.cfg.TrustedProxies = tp

	thEng, err := theme.NewEngine(env.catalog, "../../themes")
	if err != nil {
		t.Fatalf("theme.NewEngine(../../themes): %v", err)
	}

	appSrv, err := server.NewAppServer(server.AppDeps{
		Config:    env.cfg,
		Store:     env.store,
		Settings:  env.settings,
		Catalog:   env.catalog,
		Auth:      env.authSvc,
		Content:   env.content,
		Themes:    thEng,
		StartedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("NewAppServer: %v", err)
	}
	t.Cleanup(func() { _ = appSrv.Close(env.ctx) })
	env.ts.Config.Handler = appSrv.Handler()

	cookies, headers := bootstrapAuthenticatedAdmin(t, env)

	// 1. Custom standalone frameless error.html rendered across public 404, scanner trap (/.env), and post-setup /setup
	for _, p := range []string{"/missing-page-xyz", "/.env", "/setup"} {
		resp, raw := doJSONReq(t, http.MethodGet, env.ts.URL+p, nil, nil, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", p, resp.StatusCode)
		}
		bodyStr := string(raw)
		if !strings.Contains(bodyStr, "volo-err-shell") || !strings.Contains(bodyStr, "err-card") || !strings.Contains(bodyStr, ">404<") || strings.Contains(bodyStr, "studio-header") {
			t.Fatalf("GET %s expected standalone frameless error.html with volo-err-shell, err-card, and 404 code, got: %.240s", p, bodyStr)
		}
	}

	// 2. Stealth /console cloaking when DynamicLoginPath is customized must also render custom error.html
	secResp, secRaw := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/security", map[string]any{
		"dynamic_login_path": "vault",
	}, cookies, headers)
	if secResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH dynamic_login_path=vault failed: %d %s", secResp.StatusCode, string(secRaw))
	}
	cloakResp, cloakRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/console", nil, nil, nil)
	if cloakResp.StatusCode != http.StatusNotFound || !strings.Contains(string(cloakRaw), "volo-err-shell") {
		t.Fatalf("GET /console when cloaked expected 404 standalone error.html, got %d: %.200s", cloakResp.StatusCode, string(cloakRaw))
	}

	// 3. External theme (volo-journal) 404 must render standalone custom error page with volo-journal skin
	appResp, appRaw := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/appearance", map[string]any{
		"active_theme": "volo-journal",
	}, cookies, headers)
	if appResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH active_theme=volo-journal failed: %d %s", appResp.StatusCode, string(appRaw))
	}
	jResp, jRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/journal-missing-route", nil, nil, nil)
	jStr := string(jRaw)
	if jResp.StatusCode != http.StatusNotFound || !strings.Contains(jStr, `data-volo-skin="volo-journal"`) || !strings.Contains(jStr, "volo-err-shell") {
		t.Fatalf("GET /journal-missing-route expected 404 volo-journal custom error page, got %d: %.240s", jResp.StatusCode, jStr)
	}

	// Restore volo-studio for subsequent assertions
	_, _ = doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/appearance", map[string]any{
		"active_theme": "volo-studio",
	}, cookies, headers)

	// 4. API endpoints (/api/v1/... and /api/admin/...) MUST preserve application/json errors (never HTML)
	for _, apiCase := range []struct {
		path       string
		wantStatus int
	}{
		{"/api/v1/articles/non-existent-slug-999", http.StatusNotFound},
		{"/api/admin/overview", http.StatusUnauthorized},
	} {
		aResp, aRaw := doJSONReq(t, http.MethodGet, env.ts.URL+apiCase.path, nil, nil, nil)
		if aResp.StatusCode != apiCase.wantStatus {
			t.Fatalf("GET %s = %d, want %d", apiCase.path, aResp.StatusCode, apiCase.wantStatus)
		}
		ct := strings.ToLower(aResp.Header.Get("Content-Type"))
		if !strings.Contains(ct, "application/json") || strings.Contains(string(aRaw), "volo-err-shell") {
			t.Fatalf("GET %s expected JSON error (got Content-Type=%q, body=%.160s)", apiCase.path, ct, string(aRaw))
		}
	}

	// 5. Loopback 127.0.0.1 scanner probe records security.waf_404_probe in audit_logs without banning 127.0.0.1
	probeAudits, err := sqlx.Select[domain.AuditLog](env.ctx, env.store,
		`SELECT * FROM audit_logs WHERE action = ?`, "security.waf_404_probe")
	if err != nil {
		t.Fatalf("query security.waf_404_probe audit logs: %v", err)
	}
	if len(probeAudits) == 0 {
		t.Fatalf("expected loopback scanner probe to record security.waf_404_probe in audit_logs")
	}
	homeLoopResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/", nil, nil, nil)
	if homeLoopResp.StatusCode != http.StatusOK {
		t.Fatalf("loopback 127.0.0.1 must never be auto-banned, got GET / = %d", homeLoopResp.StatusCode)
	}

	// 6. External IP (198.51.100.77) hitting scanner traps / repeated 404s triggers auto-ban (403 custom HTML page)
	extHeaders := map[string]string{"X-Real-IP": "198.51.100.77"}
	for _, trap := range []string{"/.env", "/wp-admin"} {
		tResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+trap, nil, nil, extHeaders)
		if tResp.StatusCode != http.StatusNotFound {
			t.Fatalf("external scanner GET %s = %d, want 404", trap, tResp.StatusCode)
		}
	}
	banPageResp, banPageRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/", nil, nil, extHeaders)
	banPageStr := string(banPageRaw)
	if banPageResp.StatusCode != http.StatusForbidden || !strings.Contains(banPageStr, "volo-err-shell") || !strings.Contains(banPageStr, ">403<") {
		t.Fatalf("banned external IP GET / expected 403 standalone error.html, got %d: %.240s", banPageResp.StatusCode, banPageStr)
	}
	banAPIResp, banAPIRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/site", nil, nil, extHeaders)
	if banAPIResp.StatusCode != http.StatusForbidden || !strings.Contains(strings.ToLower(banAPIResp.Header.Get("Content-Type")), "application/json") || strings.Contains(string(banAPIRaw), "volo-err-shell") {
		t.Fatalf("banned external IP GET /api/v1/site expected 403 JSON, got %d (%s)", banAPIResp.StatusCode, string(banAPIRaw))
	}

	autoBanAudits, err := sqlx.Select[domain.AuditLog](env.ctx, env.store,
		`SELECT * FROM audit_logs WHERE action = ?`, "security.waf_auto_ban")
	if err != nil || len(autoBanAudits) == 0 {
		t.Fatalf("expected security.waf_auto_ban recorded in audit_logs (err=%v, count=%d)", err, len(autoBanAudits))
	}

	// 7. WAF Rate Shield (429) renders custom HTML error.html with 429 badge for non-API pages
	rateCfgResp, rateCfgRaw := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/security", map[string]any{
		"waf_enabled":        true,
		"waf_window_seconds": 60,
		"waf_max_requests":   10,
	}, cookies, headers)
	if rateCfgResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH enable WAF rate shield failed: %d %s", rateCfgResp.StatusCode, string(rateCfgRaw))
	}
	rateExtHeaders := map[string]string{"X-Real-IP": "203.0.113.42"}
	got429 := false
	for i := 0; i < 15; i++ {
		rResp, rRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/", nil, nil, rateExtHeaders)
		if rResp.StatusCode == http.StatusTooManyRequests {
			rStr := string(rRaw)
			if !strings.Contains(rStr, "volo-err-shell") || !strings.Contains(rStr, ">429<") {
				t.Fatalf("WAF 429 page expected volo-err-shell and 429 badge, got: %.240s", rStr)
			}
			got429 = true
			break
		}
	}
	if !got429 {
		t.Fatalf("expected external IP exceeding waf_max_requests to receive 429 custom error page")
	}
}

func TestFollowUp_PermissionsPolicyLegacyImageShimAndIPBanReasonSearch(t *testing.T) {
	env := setupM5TestEnv(t, "/console")
	cookies, _ := bootstrapAuthenticatedAdmin(t, env)

	// 1. Verify Permissions-Policy allows same-origin microphone=(self) for Admin audio recording while keeping camera/geolocation locked
	homeResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/", nil, nil, nil)
	pp := homeResp.Header.Get("Permissions-Policy")
	if !strings.Contains(pp, "microphone=(self)") || !strings.Contains(pp, "camera=()") {
		t.Fatalf("expected Permissions-Policy with microphone=(self) and camera=(), got %q", pp)
	}

	// 2. Verify /image/ and /posts/image/ legacy shims serve migrated images while never exposing DataDir root files
	img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	img.Set(1, 1, color.NRGBA{R: 12, G: 130, B: 96, A: 255})
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatal(err)
	}
	legacyImgDir := filepath.Join(env.cfg.DataDir, "image")
	if err := os.MkdirAll(legacyImgDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyImgDir, "php_20190706210325.png"), pngBuf.Bytes(), 0o640); err != nil {
		t.Fatal(err)
	}
	secretRootFile := filepath.Join(env.cfg.DataDir, "secret-sentinel.txt")
	if err := os.WriteFile(secretRootFile, []byte("never-expose"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, okPath := range []string{"/image/php_20190706210325.png", "/posts/image/php_20190706210325.png"} {
		r, _ := doJSONReq(t, http.MethodGet, env.ts.URL+okPath, nil, nil, nil)
		if r.StatusCode != http.StatusOK {
			t.Fatalf("GET %s expected 200 OK, got %d", okPath, r.StatusCode)
		}
	}
	for _, blockedPath := range []string{"/image/secret-sentinel.txt", "/image/volo.db", "/posts/image/secret-sentinel.txt"} {
		r, _ := doJSONReq(t, http.MethodGet, env.ts.URL+blockedPath, nil, nil, nil)
		if r.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s must never expose DataDir root files, got %d", blockedPath, r.StatusCode)
		}
	}

	// 3. Verify /api/admin/security/ip-bans?q=... searches reason_code for migrated Bolo/Volo records
	now := time.Now().UnixMilli()
	_, err := env.store.ExecContext(env.ctx,
		`INSERT INTO security_ip_bans (id, cidr, source, reason_code, reason, note, occurrences, expires_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id.Next(), "198.51.100.201/32", "auto", "script_scan", "", "", 3, 0, now, now,
	)
	if err != nil {
		t.Fatalf("insert migrated ip ban: %v", err)
	}
	qResp, qRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/security/ip-bans?q=script_scan", nil, cookies, nil)
	if qResp.StatusCode != http.StatusOK || !strings.Contains(string(qRaw), "198.51.100.201/32") {
		t.Fatalf("expected reason_code search to find 198.51.100.201/32, got %d: %s", qResp.StatusCode, string(qRaw))
	}
}

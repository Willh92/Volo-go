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

	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/media"
	"github.com/Willh92/Volo-go/internal/server"
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
		"admin_username":     "will",
		"admin_email":        "will@example.com",
		"admin_display_name": "Will",
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

func idxOr(a, _ int) int { return a }

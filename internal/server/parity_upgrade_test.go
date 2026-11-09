package server_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParityUpgrades_Password6_BrandIcon_Locales_Themes_Analytics(t *testing.T) {
	env := setupM5TestEnv(t, "/console")
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	// Stage shipped `./locales/zh-TW.json` and `./themes/volo-journal` into test env
	localesDir := filepath.Join(env.cfg.DataDir, "locales")
	if err := os.MkdirAll(localesDir, 0o750); err != nil {
		t.Fatal(err)
	}
	twBytes, err := os.ReadFile(filepath.Join(repoRoot, "locales", "zh-TW.json"))
	if err != nil {
		t.Fatalf("read shipped locales/zh-TW.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localesDir, "zh-TW.json"), twBytes, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := env.catalog.Reload(localesDir); err != nil {
		t.Fatalf("catalog.Reload: %v", err)
	}
	copyTreeForM6(t, filepath.Join(repoRoot, "themes", "volo-journal"), filepath.Join(env.cfg.ThemesDir, "volo-journal"))

	// -------------------------------------------------------------------------
	// 1. Brand Emblem Endpoints (`/favicon.svg` & `/favicon.ico`)
	// -------------------------------------------------------------------------
	svgResp, svgBody := doJSONReq(t, http.MethodGet, env.ts.URL+"/favicon.svg", nil, nil, nil)
	if svgResp.StatusCode != http.StatusOK || !strings.Contains(svgResp.Header.Get("Content-Type"), "image/svg+xml") || !bytes.Contains(svgBody, []byte("<svg")) {
		t.Fatalf("GET /favicon.svg failed: status=%d ct=%q", svgResp.StatusCode, svgResp.Header.Get("Content-Type"))
	}
	icoResp, icoBody := doJSONReq(t, http.MethodGet, env.ts.URL+"/favicon.ico", nil, nil, nil)
	if icoResp.StatusCode != http.StatusOK || len(icoBody) < 6 || icoBody[0] != 0 || icoBody[1] != 0 || icoBody[2] != 1 || icoBody[3] != 0 {
		t.Fatalf("GET /favicon.ico invalid ICO header: status=%d len=%d", icoResp.StatusCode, len(icoBody))
	}

	// -------------------------------------------------------------------------
	// 2. 6-Character Minimum Password Policy (`5` rejected, `6` accepted)
	// -------------------------------------------------------------------------
	stResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/setup/status", nil, nil, nil)
	var setupCSRF string
	for _, c := range stResp.Cookies() {
		if strings.Contains(c.Name, "csrf") {
			setupCSRF = c.Value
		}
	}
	csrfHdr := map[string]string{"X-CSRF-Token": setupCSRF}

	shortResp, _ := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/setup/complete", map[string]any{
		"admin_username":     "studioadmin",
		"admin_email":        "admin@studio.example.com",
		"admin_password":     "12345",
		"admin_display_name": "Studio Owner",
		"site_title":         "Volo Studio",
		"default_locale":     "zh-CN",
	}, stResp.Cookies(), csrfHdr)
	if shortResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 5-char password to fail with 400, got %d", shortResp.StatusCode)
	}

	okSetupResp, okSetupBody := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/setup/complete", map[string]any{
		"admin_username":     "studioadmin",
		"admin_email":        "admin@studio.example.com",
		"admin_password":     "123456",
		"admin_display_name": "Studio Owner",
		"site_title":         "Volo Studio",
		"default_locale":     "zh-CN",
	}, stResp.Cookies(), csrfHdr)
	if okSetupResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 6-char password setup to succeed with 200, got %d: %s", okSetupResp.StatusCode, string(okSetupBody))
	}

	adminCookies := okSetupResp.Cookies()
	var setupDone struct {
		CSRFToken string `json:"csrf_token"`
	}
	_ = json.Unmarshal(okSetupBody, &setupDone)
	adminCSRFVal := setupDone.CSRFToken
	adminCSRF := map[string]string{"X-CSRF-Token": adminCSRFVal}

	// -------------------------------------------------------------------------
	// 3. Language Pack Extension APIs (`GET`, `POST install` JSON & ZIP, `export`, `DELETE`)
	// -------------------------------------------------------------------------
	locListResp, locListBody := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/locales", nil, adminCookies, nil)
	if locListResp.StatusCode != http.StatusOK || !bytes.Contains(locListBody, []byte(`"zh-TW"`)) {
		t.Fatalf("GET /api/admin/locales missing zh-TW: status=%d body=%s", locListResp.StatusCode, string(locListBody))
	}

	// Install `ja-JP.json` via multipart upload
	var mpBuf bytes.Buffer
	mw := multipart.NewWriter(&mpBuf)
	part, err := mw.CreateFormFile("file", "ja-JP.json")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte(`{
		"meta.code": "ja-JP",
		"meta.name": "日本語",
		"nav.home": "ホーム",
		"admin.nav.locales": "言語パック拡張"
	}`))
	_ = mw.Close()

	instReq, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/api/admin/locales/install", &mpBuf)
	instReq.Header.Set("Content-Type", mw.FormDataContentType())
	instReq.Header.Set("X-CSRF-Token", adminCSRFVal)
	for _, c := range adminCookies {
		instReq.AddCookie(c)
	}
	instResp, err := http.DefaultClient.Do(instReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = instResp.Body.Close()
	if instResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/admin/locales/install (ja-JP.json) status=%d, want 201", instResp.StatusCode)
	}

	// Verify `/api/v1/i18n/ja-JP` exposes `custom_messages` with `admin.nav.locales`
	pubI18nResp, pubI18nBody := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/i18n/ja-JP", nil, nil, nil)
	if pubI18nResp.StatusCode != http.StatusOK ||
		!bytes.Contains(pubI18nBody, []byte(`"custom_messages"`)) ||
		!bytes.Contains(pubI18nBody, []byte(`"言語パック拡張"`)) {
		t.Fatalf("GET /api/v1/i18n/ja-JP missing custom_messages: status=%d body=%s", pubI18nResp.StatusCode, string(pubI18nBody))
	}

	// Export `ja-JP`
	expResp, expBody := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/locales/ja-JP/export", nil, adminCookies, nil)
	if expResp.StatusCode != http.StatusOK || !bytes.Contains(expBody, []byte(`"ホーム"`)) {
		t.Fatalf("GET /api/admin/locales/ja-JP/export failed: status=%d", expResp.StatusCode)
	}

	// Delete external `ja-JP`
	delLocResp, _ := doJSONReq(t, http.MethodDelete, env.ts.URL+"/api/admin/locales/ja-JP", nil, adminCookies, adminCSRF)
	if delLocResp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /api/admin/locales/ja-JP status=%d, want 200", delLocResp.StatusCode)
	}

	// -------------------------------------------------------------------------
	// 4. Theme Preview, Per-Theme Config, Standalone `.zip` Install/Uninstall, `/dynamic` & Legacy `.html` Redirect
	// -------------------------------------------------------------------------
	prevSvgResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/themes/volo-studio/preview", nil, nil, nil)
	if prevSvgResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /themes/volo-studio/preview = %d", prevSvgResp.StatusCode)
	}

	cfgResp, cfgBody := doJSONReq(t, http.MethodPut, env.ts.URL+"/api/admin/themes/volo-journal/config", map[string]any{
		"journal_motto": "Crafted in Pure Go",
		"show_colophon": true,
	}, adminCookies, adminCSRF)
	if cfgResp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /api/admin/themes/volo-journal/config = %d: %s", cfgResp.StatusCode, string(cfgBody))
	}
	skinPrevResp, skinPrevHTML := doJSONReq(t, http.MethodGet, env.ts.URL+"/?skin=volo-journal", nil, adminCookies, nil)
	if skinPrevResp.StatusCode != http.StatusOK || !bytes.Contains(skinPrevHTML, []byte("Crafted in Pure Go")) {
		t.Fatalf("expected ?skin=volo-journal to render configured journal_motto, status=%d", skinPrevResp.StatusCode)
	}

	// Hot-install standalone full-HTML `.zip` theme (without `{{define "content"}}`)
	var thZip bytes.Buffer
	tzw := zip.NewWriter(&thZip)
	mfWriter, _ := tzw.Create("retro-solo/theme.json")
	_, _ = mfWriter.Write([]byte(`{"id":"retro-solo","name":{"zh-CN":"复古独立皮","en-US":"Retro Solo"},"version":"1.0.0","author":"QA"}`))
	idxWriter, _ := tzw.Create("retro-solo/templates/index.html")
	_, _ = idxWriter.Write([]byte(`<!DOCTYPE html><html data-volo-skin="retro-solo"><head><title>{{.PageTitle}}</title></head><body><h1>Retro Standalone:{{.Site.Title}}</h1></body></html>`))
	_ = tzw.Close()

	var thMp bytes.Buffer
	tmw := multipart.NewWriter(&thMp)
	thPart, _ := tmw.CreateFormFile("file", "retro-solo.zip")
	_, _ = thPart.Write(thZip.Bytes())
	_ = tmw.Close()

	thReq, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/api/admin/themes/install", &thMp)
	thReq.Header.Set("Content-Type", tmw.FormDataContentType())
	thReq.Header.Set("X-CSRF-Token", adminCSRFVal)
	for _, c := range adminCookies {
		thReq.AddCookie(c)
	}
	thInstResp, err := http.DefaultClient.Do(thReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = thInstResp.Body.Close()
	if thInstResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/admin/themes/install = %d, want 201", thInstResp.StatusCode)
	}

	retroResp, retroHTML := doJSONReq(t, http.MethodGet, env.ts.URL+"/?skin=retro-solo", nil, adminCookies, nil)
	if retroResp.StatusCode != http.StatusOK || !bytes.Contains(retroHTML, []byte("Retro Standalone:Volo Studio")) {
		t.Fatalf("standalone full-HTML theme render failed: status=%d html=%s", retroResp.StatusCode, string(retroHTML))
	}

	delThResp, _ := doJSONReq(t, http.MethodDelete, env.ts.URL+"/api/admin/themes/retro-solo", nil, adminCookies, adminCSRF)
	if delThResp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /api/admin/themes/retro-solo = %d", delThResp.StatusCode)
	}

	// `/dynamic` route renders 200 OK
	dynResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/dynamic", nil, nil, nil)
	if dynResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /dynamic = %d, want 200", dynResp.StatusCode)
	}

	// -------------------------------------------------------------------------
	// 5. Category + Protected Article Filtering, Legacy `.html` Redirect, and 30-Day Overview Series
	// -------------------------------------------------------------------------
	catResp, catBody := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/categories", map[string]any{
		"name": "Engineering",
		"slug": "engineering",
	}, adminCookies, adminCSRF)
	if catResp.StatusCode != http.StatusCreated {
		t.Fatalf("create category failed: %d %s", catResp.StatusCode, string(catBody))
	}
	var createdCat struct {
		Category struct {
			ID string `json:"id"`
		} `json:"category"`
	}
	_ = json.Unmarshal(catBody, &createdCat)

	artResp, artBody := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"title":       "Protected Architecture Note",
		"slug":        "protected-arch-note",
		"content_md":  "Confidential design details.",
		"status":      "published",
		"category_id": createdCat.Category.ID,
		"password":    "secret",
	}, adminCookies, adminCSRF)
	if artResp.StatusCode != http.StatusCreated {
		t.Fatalf("create protected article failed: %d %s", artResp.StatusCode, string(artBody))
	}

	// Legacy `.html` path redirects 301 to canonical `/posts/protected-arch-note`
	legResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/articles/2026/05/protected-arch-note.html", nil, nil, nil)
	if legResp.StatusCode != http.StatusMovedPermanently || legResp.Header.Get("Location") != "/posts/protected-arch-note" {
		t.Fatalf("expected 301 redirect to /posts/protected-arch-note, got status=%d loc=%q", legResp.StatusCode, legResp.Header.Get("Location"))
	}

	// `status=protected` & `category_id` filters
	protListResp, protListBody := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/articles?status=protected&category_id="+createdCat.Category.ID, nil, adminCookies, nil)
	if protListResp.StatusCode != http.StatusOK || !bytes.Contains(protListBody, []byte(`"protected-arch-note"`)) {
		t.Fatalf("GET /api/admin/articles?status=protected&category_id=... failed: %d %s", protListResp.StatusCode, string(protListBody))
	}

	// `GET /api/admin/overview?days=30` returns 30-day series
	ovResp, ovBody := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/overview?days=30", nil, adminCookies, nil)
	if ovResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/admin/overview?days=30 = %d", ovResp.StatusCode)
	}
	var ovPayload struct {
		AnalyticsSeries []any `json:"analytics_series"`
		TopArticles     []any `json:"top_articles"`
		TopReferers     []any `json:"top_referers"`
	}
	if err := json.Unmarshal(ovBody, &ovPayload); err != nil {
		t.Fatal(err)
	}
	if len(ovPayload.AnalyticsSeries) != 30 {
		t.Fatalf("expected 30 analytics_series buckets for ?days=30, got %d", len(ovPayload.AnalyticsSeries))
	}
}

func TestUXAndAdminModalCRUDParity(t *testing.T) {
	env := setupM5TestEnv(t, "/console")

	stResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/setup/status", nil, nil, nil)
	var setupCSRF string
	for _, c := range stResp.Cookies() {
		if strings.Contains(c.Name, "csrf") {
			setupCSRF = c.Value
		}
	}
	okSetupResp, okSetupBody := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/setup/complete", map[string]any{
		"admin_username":     "uxadmin",
		"admin_email":        "ux@studio.example.com",
		"admin_password":     "123456",
		"admin_display_name": "UX Admin",
		"site_title":         "Volo UX",
		"default_locale":     "zh-CN",
	}, stResp.Cookies(), map[string]string{"X-CSRF-Token": setupCSRF})
	if okSetupResp.StatusCode != http.StatusOK {
		t.Fatalf("setup complete failed: %d %s", okSetupResp.StatusCode, string(okSetupBody))
	}
	adminCookies := okSetupResp.Cookies()
	var setupDone struct {
		CSRFToken string `json:"csrf_token"`
	}
	_ = json.Unmarshal(okSetupBody, &setupDone)
	adminCSRF := map[string]string{"X-CSRF-Token": setupDone.CSRFToken}

	// 1. `/images/github_repo.jpg` serves Open Source Showcase SVG cover
	ghCoverResp, ghCoverBody := doJSONReq(t, http.MethodGet, env.ts.URL+"/images/github_repo.jpg", nil, nil, nil)
	if ghCoverResp.StatusCode != http.StatusOK || !bytes.Contains(ghCoverBody, []byte("<svg")) {
		t.Fatalf("GET /images/github_repo.jpg failed: status=%d", ghCoverResp.StatusCode)
	}

	// 2. `GET /api/admin/me` returns `storage_driver`
	meResp, meBody := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/me", nil, adminCookies, nil)
	if meResp.StatusCode != http.StatusOK || !bytes.Contains(meBody, []byte(`"storage_driver":"local"`)) {
		t.Fatalf("GET /api/admin/me missing storage_driver: %d %s", meResp.StatusCode, string(meBody))
	}

	// 3. `GET /api/admin/media` reconciles untracked local uploads & `POST /api/admin/media/orphans?delete=1` cleans orphans
	tinyPNG := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
		0x89, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41,
		0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00,
		0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
		0x42, 0x60, 0x82,
	}
	orphanPath := filepath.Join(env.cfg.UploadDir, "dropped-orphan.png")
	if err := os.WriteFile(orphanPath, tinyPNG, 0o640); err != nil {
		t.Fatal(err)
	}
	recResp, recBody := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/media/reconcile", map[string]any{}, adminCookies, adminCSRF)
	if recResp.StatusCode != http.StatusOK || !bytes.Contains(recBody, []byte(`"imported":1`)) {
		t.Fatalf("POST /api/admin/media/reconcile failed: %d %s", recResp.StatusCode, string(recBody))
	}
	mediaResp, mediaBody := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/media", nil, adminCookies, nil)
	if mediaResp.StatusCode != http.StatusOK || !bytes.Contains(mediaBody, []byte(`"dropped-orphan.png"`)) || !bytes.Contains(mediaBody, []byte(`"is_orphan":true`)) {
		t.Fatalf("GET /api/admin/media failed after reconcile: %d %s", mediaResp.StatusCode, string(mediaBody))
	}
	cleanResp, cleanBody := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/media/orphans?delete=1", map[string]any{}, adminCookies, adminCSRF)
	if cleanResp.StatusCode != http.StatusOK || !bytes.Contains(cleanBody, []byte(`"deleted_count":1`)) {
		t.Fatalf("POST /api/admin/media/orphans?delete=1 failed: %d %s", cleanResp.StatusCode, string(cleanBody))
	}

	// 4. Modal `PUT` endpoints on tags, redirects, and geo-rules
	tagCreateResp, tagCreateBody := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/tags", map[string]any{
		"name": "Golang",
		"slug": "golang",
	}, adminCookies, adminCSRF)
	if tagCreateResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/admin/tags = %d %s", tagCreateResp.StatusCode, string(tagCreateBody))
	}
	var createdTag struct {
		Tag struct {
			ID string `json:"id"`
		} `json:"tag"`
	}
	_ = json.Unmarshal(tagCreateBody, &createdTag)
	tagPutResp, _ := doJSONReq(t, http.MethodPut, env.ts.URL+"/api/admin/tags/"+createdTag.Tag.ID, map[string]any{
		"name": "Go Runtime",
		"slug": "go-runtime",
	}, adminCookies, adminCSRF)
	if tagPutResp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /api/admin/tags/{id} = %d", tagPutResp.StatusCode)
	}

	redCreateResp, redCreateBody := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/redirects", map[string]any{
		"source_path": "/old-note",
		"target_path": "/posts/new-note",
		"status_code": 301,
		"is_enabled":  true,
	}, adminCookies, adminCSRF)
	if redCreateResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/admin/redirects = %d %s", redCreateResp.StatusCode, string(redCreateBody))
	}
	var createdRed struct {
		Item struct {
			ID string `json:"id"`
		} `json:"item"`
		Redirect struct {
			ID string `json:"id"`
		} `json:"redirect"`
	}
	_ = json.Unmarshal(redCreateBody, &createdRed)
	redID := createdRed.Item.ID
	if redID == "" {
		redID = createdRed.Redirect.ID
	}
	if redID != "" {
		redPutResp, _ := doJSONReq(t, http.MethodPut, env.ts.URL+"/api/admin/redirects/"+redID, map[string]any{
			"source_path": "/old-note-v2",
			"target_path": "/posts/new-note-v2",
			"status_code": 302,
			"is_enabled":  true,
		}, adminCookies, adminCSRF)
		if redPutResp.StatusCode != http.StatusOK {
			t.Fatalf("PUT /api/admin/redirects/{id} = %d", redPutResp.StatusCode)
		}
	}

	geoCreateResp, geoCreateBody := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/security/geo-rules", map[string]any{
		"name":       "Test Disabled Rule",
		"is_enabled": false,
		"action":     "deny",
		"on_miss":    "next",
		"countries":  "AQ",
		"sort_order": 10,
	}, adminCookies, adminCSRF)
	if geoCreateResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/admin/security/geo-rules = %d %s", geoCreateResp.StatusCode, string(geoCreateBody))
	}
	var createdGeo struct {
		Rule struct {
			ID string `json:"id"`
		} `json:"rule"`
	}
	_ = json.Unmarshal(geoCreateBody, &createdGeo)
	geoPutResp, _ := doJSONReq(t, http.MethodPut, env.ts.URL+"/api/admin/security/geo-rules/"+createdGeo.Rule.ID, map[string]any{
		"name":       "Updated Disabled Rule",
		"is_enabled": false,
		"action":     "deny",
		"on_miss":    "allow",
		"countries":  "AQ",
		"sort_order": 5,
	}, adminCookies, adminCSRF)
	if geoPutResp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /api/admin/security/geo-rules/{id} = %d", geoPutResp.StatusCode)
	}

	// 5. Article + Comment Moderation (`POST /api/admin/comments/{id}/ban-ip`) + Admin-Only Homepage RecentComments
	artCreateResp, artCreateBody := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"title":      "Hello Parity World",
		"slug":       "hello-parity-world",
		"content_md": "## Section One\n\nBody text.",
		"status":     "published",
		"tags":       []string{"Go Runtime"},
	}, adminCookies, adminCSRF)
	if artCreateResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/admin/articles = %d %s", artCreateResp.StatusCode, string(artCreateBody))
	}
	var createdArt struct {
		Article struct {
			ID string `json:"id"`
		} `json:"article"`
	}
	_ = json.Unmarshal(artCreateBody, &createdArt)

	_, err := env.store.ExecContext(env.ctx,
		`INSERT INTO comments (id, target_type, target_id, parent_id, reply_to_name, user_id, guest_id_hash, env_fp_hash, author_name, author_url, avatar_url, notify_email_ciphertext, notify_locale, client_ip, ip_masked, geo_region, content_md, status, is_deleted, created_at, updated_at)
		 VALUES ('cmt-test-1', 'article', ?, '', '', '', '', '', 'Visitor Bob', '', '', '', 'zh-CN', '203.0.113.77', '203.0.113.*', 'US', 'UNIQUE_RECENT_COMMENT_SNIPPET', 'approved', 0, 1700000000000, 1700000000000)`,
		createdArt.Article.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Guest homepage should NOT display `UNIQUE_RECENT_COMMENT_SNIPPET`, whereas logged-in admin homepage DOES display it
	_, guestHomeHTML := doJSONReq(t, http.MethodGet, env.ts.URL+"/", nil, nil, nil)
	if bytes.Contains(guestHomeHTML, []byte("UNIQUE_RECENT_COMMENT_SNIPPET")) {
		t.Fatalf("guest homepage must NOT expose RecentComments")
	}
	_, adminHomeHTML := doJSONReq(t, http.MethodGet, env.ts.URL+"/", nil, adminCookies, nil)
	if !bytes.Contains(adminHomeHTML, []byte("UNIQUE_RECENT_COMMENT_SNIPPET")) {
		t.Fatalf("logged-in admin homepage expected to display RecentComments")
	}

	// Ban commenter IP via `POST /api/admin/comments/cmt-test-1/ban-ip`
	banResp, banBody := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/comments/cmt-test-1/ban-ip", map[string]any{
		"note":           "spammer",
		"expires_hours":  24,
		"reject_comment": true,
	}, adminCookies, adminCSRF)
	if banResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/admin/comments/cmt-test-1/ban-ip = %d %s", banResp.StatusCode, string(banBody))
	}

	// 6. Overview enriched fields (`monthly_articles`, `category_usage`, `top_tags`, `recent_logs`) & `about/project-info`
	ovResp, ovBody := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/overview", nil, adminCookies, nil)
	if ovResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/admin/overview = %d", ovResp.StatusCode)
	}
	var ovData struct {
		MonthlyArticles []any `json:"monthly_articles"`
		TopTags         []any `json:"top_tags"`
		RecentLogs      []any `json:"recent_logs"`
	}
	if err := json.Unmarshal(ovBody, &ovData); err != nil {
		t.Fatal(err)
	}
	if len(ovData.MonthlyArticles) != 12 || len(ovData.RecentLogs) == 0 {
		t.Fatalf("expected 12 monthly_articles and non-empty recent_logs, got months=%d logs=%d", len(ovData.MonthlyArticles), len(ovData.RecentLogs))
	}

	abResp, abBody := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/about/project-info", nil, adminCookies, nil)
	if abResp.StatusCode != http.StatusOK || !bytes.Contains(abBody, []byte(`"go_version"`)) {
		t.Fatalf("GET /api/admin/about/project-info = %d %s", abResp.StatusCode, string(abBody))
	}

	// 7. `GET /logout?redirect=/archives` revokes session & redirects `303 SeeOther`
	logoutResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/logout?redirect=/archives", nil, adminCookies, nil)
	if logoutResp.StatusCode != http.StatusSeeOther || logoutResp.Header.Get("Location") != "/archives" {
		t.Fatalf("GET /logout?redirect=/archives expected 303 -> /archives, got %d -> %q", logoutResp.StatusCode, logoutResp.Header.Get("Location"))
	}
}

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
		"title":        "Protected Architecture Note",
		"slug":         "protected-arch-note",
		"content_md":   "Confidential design details.",
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

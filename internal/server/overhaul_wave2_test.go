package server_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/auth"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/theme"
)

func buildPluginZipBytes(t *testing.T, id, ver, script string) []byte {
	t.Helper()
	manifest := map[string]any{
		"id":      id,
		"version": ver,
		"runtime": "goja",
		"entry":   "main.js",
		"name": map[string]string{
			"en-US": "Test Plugin " + ver,
			"zh-CN": "测试插件 " + ver,
		},
		"description": map[string]string{
			"en-US": "Wave2 test plugin",
			"zh-CN": "Wave2 测试插件",
		},
		"permissions": []string{},
	}
	mfRaw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w1, _ := zw.Create("manifest.json")
	_, _ = w1.Write(mfRaw)
	w2, _ := zw.Create("main.js")
	_, _ = w2.Write([]byte(script))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func buildSecretThemeZipBytes(t *testing.T, id, ver string) []byte {
	t.Helper()
	manifest := map[string]any{
		"id":      id,
		"version": ver,
		"name": map[string]string{
			"en-US": "Secret Skin " + ver,
			"zh-CN": "密钥主题 " + ver,
		},
		"config_schema": []map[string]any{
			{
				"key":     "analytics_token",
				"type":    "secret",
				"default": "",
				"label":   map[string]string{"en-US": "Analytics Token", "zh-CN": "统计令牌"},
			},
			{
				"key":     "banner_text",
				"type":    "text",
				"default": "Hello",
				"label":   map[string]string{"en-US": "Banner", "zh-CN": "横幅"},
			},
		},
	}
	mfRaw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w1, _ := zw.Create("theme.json")
	_, _ = w1.Write(mfRaw)
	w2, _ := zw.Create("templates/layout.html")
	_, _ = w2.Write([]byte(`{{define "skin_name"}}` + id + `{{end}}`))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func postMultipartZip(t *testing.T, urlStr string, zipBytes []byte, filename string, cookies []*http.Cookie, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(zipBytes); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, urlStr, &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

func TestWave2_PublicListStyleFull_SitemapShards_FeedHostPinning_And_PublicSessionCSRF(t *testing.T) {
	env, adminCookies, adminHdrs, _ := setupRound2Env(t)

	// 1. Create unlocked article + password-locked article
	r1, raw1 := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"title":         "Full Stream Essay",
		"slug":          "full-stream-essay",
		"summary_md":    "Short summary only",
		"content_md":    "UniqueUnlockedBodyMarker991 with **rich** prose.",
		"status":        "published",
		"allow_comment": true,
	}, adminCookies, adminHdrs)
	if r1.StatusCode != http.StatusCreated {
		t.Fatalf("create unlocked article = %d %s", r1.StatusCode, string(raw1))
	}

	r2, raw2 := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"title":         "Secret Vault Essay",
		"slug":          "secret-vault-essay",
		"summary_md":    "Vault teaser",
		"content_md":    "SuperSecretVaultMarker777 MUST NOT leak on home stream.",
		"password":      "Vault#Pass99",
		"status":        "published",
		"allow_comment": true,
	}, adminCookies, adminHdrs)
	if r2.StatusCode != http.StatusCreated {
		t.Fatalf("create password article = %d %s", r2.StatusCode, string(raw2))
	}

	// Switch Reading.ListStyle = "full"
	pResp, pRaw := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/reading", map[string]any{
		"list_style": "full",
	}, adminCookies, adminHdrs)
	if pResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH reading.list_style=full = %d %s", pResp.StatusCode, string(pRaw))
	}

	homeResp, homeRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/", nil, nil, nil)
	homeHTML := string(homeRaw)
	if homeResp.StatusCode != http.StatusOK || !strings.Contains(homeHTML, "UniqueUnlockedBodyMarker991") {
		t.Fatalf("expected Reading.ListStyle=full to render unlocked article full body on /, got %d", homeResp.StatusCode)
	}
	if strings.Contains(homeHTML, "SuperSecretVaultMarker777") {
		t.Fatalf("CRITICAL: password-protected body leaked on home stream under list_style=full!")
	}

	// 2. Paginated Sitemap: /sitemap.xml & /sitemap-1.xml succeed, /sitemap-99.xml returns 404
	smIdx, smIdxRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/sitemap.xml", nil, nil, nil)
	if smIdx.StatusCode != http.StatusOK || !strings.Contains(string(smIdxRaw), "full-stream-essay") {
		t.Fatalf("GET /sitemap.xml = %d %.200s", smIdx.StatusCode, string(smIdxRaw))
	}
	sm1, sm1Raw := doJSONReq(t, http.MethodGet, env.ts.URL+"/sitemap-1.xml", nil, nil, nil)
	if sm1.StatusCode != http.StatusOK || !strings.Contains(string(sm1Raw), "full-stream-essay") {
		t.Fatalf("GET /sitemap-1.xml = %d %.200s", sm1.StatusCode, string(sm1Raw))
	}
	sm99, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/sitemap-99.xml", nil, nil, nil)
	if sm99.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /sitemap-99.xml expected 404, got %d", sm99.StatusCode)
	}

	// 3. Feed Host cache-poisoning defense: spoofed Host header must NOT poison RSS links when PublicURL is configured
	rssSpoof, rssSpoofRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/rss.xml", nil, nil, map[string]string{
		"Host": "evil-poison.example.org",
	})
	if rssSpoof.StatusCode != http.StatusOK || strings.Contains(string(rssSpoofRaw), "evil-poison.example.org") || !strings.Contains(string(rssSpoofRaw), "https://studio.example.com") {
		t.Fatalf("expected /rss.xml to pin canonical PublicURL and ignore spoofed Host, got: %.240s", string(rssSpoofRaw))
	}

	// 4. Authenticated /api/v1/* mutation without X-CSRF-Token MUST return 403 Forbidden
	noCSRF, noCSRFRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/v1/reader/logout", nil, adminCookies, map[string]string{
		"Origin": env.ts.URL,
	})
	if noCSRF.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 on authenticated /api/v1/reader/logout without X-CSRF-Token, got %d %s", noCSRF.StatusCode, string(noCSRFRaw))
	}
}

func TestWave2_StepUpAuth_RoleRevocation_BackupTicket_TOTPScope_And_PinnedPaths(t *testing.T) {
	env, adminCookies, adminHdrs, adminID := setupRound2Env(t)

	// 1. CLI-pinned UploadDir must ignore DB storage.local_dir override while reporting upload_dir_pinned=true
	pinnedBefore := env.cfg.GetUploadDir()
	customVault := filepath.Join(filepath.Dir(env.cfg.DataDir), "unpinned-vault-attempt")
	stPatch, stRaw := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/storage", map[string]any{
		"local_dir": customVault,
	}, adminCookies, adminHdrs)
	if stPatch.StatusCode != http.StatusOK || !strings.Contains(string(stRaw), `"upload_dir_pinned":true`) {
		t.Fatalf("PATCH settings/storage expected upload_dir_pinned=true, got %d %s", stPatch.StatusCode, string(stRaw))
	}
	if got := env.cfg.GetUploadDir(); got != pinnedBefore {
		t.Fatalf("expected CLI-pinned UploadDir %q to remain unchanged, got %q", pinnedBefore, got)
	}

	// 2. Step-Up password required for staff creation & sensitive user PATCH; role demotion revokes active sessions & PATs
	noPwStaff, _ := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/users", map[string]any{
		"username":     "staff_nopw",
		"email":        "staff_nopw@example.com",
		"display_name": "NoPW",
		"role":         "editor",
		"password":     "Ed1t0r!Pass#2026",
	}, adminCookies, adminHdrs)
	if noPwStaff.StatusCode != http.StatusBadRequest && noPwStaff.StatusCode != http.StatusForbidden {
		t.Fatalf("expected Step-Up rejection when creating editor without current_password, got %d", noPwStaff.StatusCode)
	}

	okStaff, okStaffRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/users", map[string]any{
		"username":         "staff_ed",
		"email":            "staff_ed@example.com",
		"display_name":     "Staff Editor",
		"role":             "editor",
		"password":         "Ed1t0r!Pass#2026",
		"current_password": "Str0ng!Pass#2026",
	}, adminCookies, adminHdrs)
	if okStaff.StatusCode != http.StatusCreated {
		t.Fatalf("create editor with Step-Up password failed: %d %s", okStaff.StatusCode, string(okStaffRaw))
	}
	var createdEd struct {
		User domain.User `json:"user"`
	}
	_ = json.Unmarshal(okStaffRaw, &createdEd)

	// Log in as staff_ed and mint an editor PAT
	edLogin, edLoginRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/auth/login", map[string]any{
		"ident":    "staff_ed",
		"password": "Ed1t0r!Pass#2026",
	}, nil, map[string]string{"Origin": env.ts.URL})
	if edLogin.StatusCode != http.StatusOK {
		t.Fatalf("staff_ed login = %d %s", edLogin.StatusCode, string(edLoginRaw))
	}
	var edSession struct {
		CSRFToken string `json:"csrf_token"`
	}
	_ = json.Unmarshal(edLoginRaw, &edSession)
	edCookies := edLogin.Cookies()
	edHdrs := map[string]string{
		auth.CSRFHeaderName: edSession.CSRFToken,
		"Origin":            env.ts.URL,
	}

	// Verify editor CANNOT mint backup:read PAT scope
	badPAT, _ := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/security/tokens", map[string]any{
		"name":             "Editor Backup Attempt",
		"scopes":           []string{"backup:read"},
		"current_password": "Ed1t0r!Pass#2026",
	}, edCookies, edHdrs)
	if badPAT.StatusCode != http.StatusForbidden && badPAT.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected editor backup:read PAT creation to fail, got %d", badPAT.StatusCode)
	}

	okPAT, okPATRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/security/tokens", map[string]any{
		"name":             "Editor Content Token",
		"scopes":           []string{"articles:read"},
		"current_password": "Ed1t0r!Pass#2026",
	}, edCookies, edHdrs)
	if okPAT.StatusCode != http.StatusCreated && okPAT.StatusCode != http.StatusOK {
		t.Fatalf("editor articles:read PAT failed: %d %s", okPAT.StatusCode, string(okPATRaw))
	}
	var mintedPAT struct {
		PlaintextToken string `json:"plaintext_token"`
	}
	_ = json.Unmarshal(okPATRaw, &mintedPAT)

	// 3. TOTPRequiredScope: when totp_required_scope="admin", un-enrolled editor still gets staff draft preview
	// on public routes; under "staff", un-enrolled editor is demoted!
	edDraftResp, _ := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"title":      "Editor Private Draft",
		"slug":       "editor-private-draft",
		"content_md": "DraftContentScopeCheck888",
		"status":     "draft",
	}, edCookies, edHdrs)
	if edDraftResp.StatusCode != http.StatusCreated {
		t.Fatalf("editor draft creation failed: %d", edDraftResp.StatusCode)
	}

	// Enroll TOTP on admin so admin remains TOTP-compliant while testing totp_required_scope="admin" vs "staff"
	totpSec, err := auth.GenerateTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	totpCode, err := auth.ComputeTOTPCode(totpSec, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.authSvc.EnrollTOTP(env.ctx, adminID, totpSec, totpCode); err != nil {
		t.Fatalf("EnrollTOTP admin: %v", err)
	}

	scAdm, scAdmRaw := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/security", map[string]any{
		"totp_required_scope": "admin",
	}, adminCookies, adminHdrs)
	if scAdm.StatusCode != http.StatusOK {
		t.Fatalf("PATCH totp_required_scope=admin failed: %d %s", scAdm.StatusCode, string(scAdmRaw))
	}
	adminOnlyView, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/posts/editor-private-draft", nil, edCookies, nil)
	if adminOnlyView.StatusCode != http.StatusOK {
		t.Fatalf("expected un-enrolled editor to view own draft under totp_required_scope=admin, got %d", adminOnlyView.StatusCode)
	}

	scStaff, scStaffRaw := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/security", map[string]any{
		"totp_required_scope": "staff",
	}, adminCookies, adminHdrs)
	if scStaff.StatusCode != http.StatusOK {
		t.Fatalf("PATCH totp_required_scope=staff failed: %d %s", scStaff.StatusCode, string(scStaffRaw))
	}
	allStaffView, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/posts/editor-private-draft", nil, edCookies, nil)
	if allStaffView.StatusCode != http.StatusNotFound {
		t.Fatalf("expected un-enrolled editor to be demoted from draft preview under totp_required_scope=staff, got %d", allStaffView.StatusCode)
	}
	edBlockedConsole, edBlockedRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/articles", nil, edCookies, nil)
	if edBlockedConsole.StatusCode != http.StatusForbidden || !strings.Contains(string(edBlockedRaw), `"requires_totp_init":true`) {
		t.Fatalf("expected un-enrolled editor to receive 403 requires_totp_init under totp_required_scope=staff, got %d %s", edBlockedConsole.StatusCode, string(edBlockedRaw))
	}
	// Reset totp_required_scope="off" for subsequent checks
	_, _ = doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/security", map[string]any{
		"totp_required_scope": "off",
	}, adminCookies, adminHdrs)

	// Demote staff_ed to reader -> requires Step-Up password AND immediately revokes edCookies + mintedPAT!
	demoteNoPw, _ := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/users/"+createdEd.User.ID, map[string]any{
		"role": "reader",
	}, adminCookies, adminHdrs)
	if demoteNoPw.StatusCode != http.StatusBadRequest && demoteNoPw.StatusCode != http.StatusForbidden {
		t.Fatalf("expected Step-Up rejection when changing role without current_password, got %d", demoteNoPw.StatusCode)
	}
	demoteOK, demoteRaw := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/users/"+createdEd.User.ID, map[string]any{
		"role":             "reader",
		"current_password": "Str0ng!Pass#2026",
	}, adminCookies, adminHdrs)
	if demoteOK.StatusCode != http.StatusOK {
		t.Fatalf("role demotion with Step-Up failed: %d %s", demoteOK.StatusCode, string(demoteRaw))
	}

	meAfterDemote, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/me", nil, edCookies, nil)
	if meAfterDemote.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected demoted user's old session to be revoked (401), got %d", meAfterDemote.StatusCode)
	}
	patAfterDemote, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/articles", nil, nil, map[string]string{
		"Authorization": "Bearer " + mintedPAT.PlaintextToken,
	})
	if patAfterDemote.StatusCode != http.StatusUnauthorized && patAfterDemote.StatusCode != http.StatusForbidden {
		t.Fatalf("expected demoted user's old PAT to be revoked, got %d", patAfterDemote.StatusCode)
	}

	// 4. Local Backup Step-Up Download Ticket: cookie GET without ticket is 403; single-use ticket succeeds once then 403
	snapResp, snapRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/backups/local", map[string]any{}, adminCookies, adminHdrs)
	if snapResp.StatusCode != http.StatusOK {
		t.Fatalf("create local backup = %d %s", snapResp.StatusCode, string(snapRaw))
	}
	var snapOut struct {
		File string `json:"file"`
	}
	_ = json.Unmarshal(snapRaw, &snapOut)

	directGet, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/backups/local/"+snapOut.File, nil, adminCookies, nil)
	if directGet.StatusCode != http.StatusForbidden {
		t.Fatalf("expected cookie GET /api/admin/backups/local/{file} without Step-Up ticket to return 403, got %d", directGet.StatusCode)
	}
	ticketNoPw, _ := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/backups/local/"+snapOut.File+"/ticket", map[string]any{}, adminCookies, adminHdrs)
	if ticketNoPw.StatusCode != http.StatusBadRequest && ticketNoPw.StatusCode != http.StatusForbidden {
		t.Fatalf("expected Step-Up rejection when minting backup ticket without password, got %d", ticketNoPw.StatusCode)
	}
	ticketOK, ticketRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/backups/local/"+snapOut.File+"/ticket", map[string]any{
		"current_password": "Str0ng!Pass#2026",
	}, adminCookies, adminHdrs)
	if ticketOK.StatusCode != http.StatusOK {
		t.Fatalf("mint backup ticket = %d %s", ticketOK.StatusCode, string(ticketRaw))
	}
	var ticketRes struct {
		Ticket      string `json:"ticket"`
		DownloadURL string `json:"download_url"`
	}
	_ = json.Unmarshal(ticketRaw, &ticketRes)

	dl1, dl1Raw := doJSONReq(t, http.MethodGet, env.ts.URL+ticketRes.DownloadURL, nil, adminCookies, nil)
	if dl1.StatusCode != http.StatusOK || len(dl1Raw) < 64 {
		t.Fatalf("expected 200 ZIP download with valid Step-Up ticket, got %d (len=%d)", dl1.StatusCode, len(dl1Raw))
	}
	dl2, _ := doJSONReq(t, http.MethodGet, env.ts.URL+ticketRes.DownloadURL, nil, adminCookies, nil)
	if dl2.StatusCode != http.StatusUnauthorized && dl2.StatusCode != http.StatusForbidden {
		t.Fatalf("expected single-use backup ticket replay to return 401/403, got %d", dl2.StatusCode)
	}
}

func TestWave2_ReorderEndpoints_PluginOverwriteAndExport_And_ThemeSecretMasking(t *testing.T) {
	env, adminCookies, adminHdrs, _ := setupRound2Env(t)

	// 1. Categories & Books Reorder endpoints
	var catIDs []string
	for _, slug := range []string{"cat-alpha", "cat-beta"} {
		r, raw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/categories", map[string]any{
			"name": strings.ToUpper(slug),
			"slug": slug,
		}, adminCookies, adminHdrs)
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("create category %s = %d %s", slug, r.StatusCode, string(raw))
		}
		var out struct {
			Category domain.Category `json:"category"`
		}
		_ = json.Unmarshal(raw, &out)
		catIDs = append(catIDs, out.Category.ID)
	}
	reCat, reCatRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/categories/reorder", map[string]any{
		"ids": []string{catIDs[1], catIDs[0]},
	}, adminCookies, adminHdrs)
	if reCat.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/categories/reorder = %d %s", reCat.StatusCode, string(reCatRaw))
	}

	var bookIDs []string
	for _, slug := range []string{"book-one", "book-two"} {
		r, raw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/books", map[string]any{
			"title":  strings.ToUpper(slug),
			"slug":   slug,
			"status": "published",
		}, adminCookies, adminHdrs)
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("create book %s = %d %s", slug, r.StatusCode, string(raw))
		}
		var out struct {
			Book domain.Book `json:"book"`
		}
		_ = json.Unmarshal(raw, &out)
		bookIDs = append(bookIDs, out.Book.ID)
	}
	reBook, reBookRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/books/reorder", map[string]any{
		"ids": []string{bookIDs[1], bookIDs[0]},
	}, adminCookies, adminHdrs)
	if reBook.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/books/reorder = %d %s", reBook.StatusCode, string(reBookRaw))
	}

	// 2. Plugin ZIP install conflict (409), ?overwrite=true upgrade (200), and GET /api/admin/plugins/{id}/export
	v1Zip := buildPluginZipBytes(t, "wave2-ext", "1.0.0", "volo.on('article.before_render', function(p){ return p; });")
	up1, up1Raw := postMultipartZip(t, env.ts.URL+"/api/admin/plugins/install", v1Zip, "wave2-ext-1.0.0.zip", adminCookies, adminHdrs)
	if up1.StatusCode != http.StatusOK && up1.StatusCode != http.StatusCreated {
		t.Fatalf("install plugin v1 = %d %s", up1.StatusCode, string(up1Raw))
	}

	v2Zip := buildPluginZipBytes(t, "wave2-ext", "2.0.0", "volo.on('article.before_render', function(p){ return p; });")
	upConflict, _ := postMultipartZip(t, env.ts.URL+"/api/admin/plugins/install", v2Zip, "wave2-ext-2.0.0.zip", adminCookies, adminHdrs)
	if upConflict.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when reinstalling existing plugin without overwrite=true, got %d", upConflict.StatusCode)
	}
	upOverwrite, upOvRaw := postMultipartZip(t, env.ts.URL+"/api/admin/plugins/install?overwrite=true", v2Zip, "wave2-ext-2.0.0.zip", adminCookies, adminHdrs)
	if upOverwrite.StatusCode != http.StatusOK && upOverwrite.StatusCode != http.StatusCreated {
		t.Fatalf("expected 200 OK when upgrading plugin with ?overwrite=true, got %d %s", upOverwrite.StatusCode, string(upOvRaw))
	}

	expResp, expRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/plugins/wave2-ext/export", nil, adminCookies, nil)
	if expResp.StatusCode != http.StatusOK || !strings.Contains(expResp.Header.Get("Content-Type"), "zip") || len(expRaw) < 64 {
		t.Fatalf("GET /api/admin/plugins/wave2-ext/export = %d ct=%q len=%d", expResp.StatusCode, expResp.Header.Get("Content-Type"), len(expRaw))
	}

	// 3. Theme ZIP install + type:"secret" config_schema masking & round-trip preservation
	if err := os.MkdirAll(env.cfg.ThemesDir, 0o750); err != nil {
		t.Fatal(err)
	}
	thZip := buildSecretThemeZipBytes(t, "secret-skin", "1.0.0")
	thUp, thUpRaw := postMultipartZip(t, env.ts.URL+"/api/admin/themes/install", thZip, "secret-skin.zip", adminCookies, adminHdrs)
	if thUp.StatusCode != http.StatusOK && thUp.StatusCode != http.StatusCreated {
		t.Fatalf("install secret-skin theme = %d %s", thUp.StatusCode, string(thUpRaw))
	}

	// Save secret analytics_token via PATCH /api/admin/settings/appearance
	apPatch1, apRaw1 := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/appearance", map[string]any{
		"theme_configs": map[string]any{
			"secret-skin": map[string]any{
				"analytics_token": "TOP_SECRET_THEME_TOKEN_42",
				"banner_text":     "Initial Banner",
			},
		},
	}, adminCookies, adminHdrs)
	if apPatch1.StatusCode != http.StatusOK {
		t.Fatalf("PATCH appearance theme secret = %d %s", apPatch1.StatusCode, string(apRaw1))
	}
	if strings.Contains(string(apRaw1), "TOP_SECRET_THEME_TOKEN_42") || !strings.Contains(string(apRaw1), theme.SecretMask) {
		t.Fatalf("expected PATCH response to mask theme secret field, got: %s", string(apRaw1))
	}

	// Verify GET /api/admin/settings masks theme secret too, while DB holds ciphertext (never plaintext!)
	allSet, allSetRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/settings", nil, adminCookies, nil)
	if allSet.StatusCode != http.StatusOK || strings.Contains(string(allSetRaw), "TOP_SECRET_THEME_TOKEN_42") || !strings.Contains(string(allSetRaw), theme.SecretMask) {
		t.Fatalf("expected GET /api/admin/settings to mask theme secret, got: %s", string(allSetRaw))
	}
	var rawAppJSON string
	_ = env.store.QueryRowContext(env.ctx, `SELECT value_json FROM settings WHERE domain = 'appearance'`).Scan(&rawAppJSON)
	if strings.Contains(rawAppJSON, "TOP_SECRET_THEME_TOKEN_42") {
		t.Fatalf("CRITICAL: plaintext theme secret stored unencrypted in settings table: %s", rawAppJSON)
	}

	// Sending masked placeholder back when updating banner_text MUST preserve existing encrypted secret!
	apPatch2, apRaw2 := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/appearance", map[string]any{
		"theme_configs": map[string]any{
			"secret-skin": map[string]any{
				"analytics_token": theme.SecretMask,
				"banner_text":     "Updated Banner",
			},
		},
	}, adminCookies, adminHdrs)
	if apPatch2.StatusCode != http.StatusOK {
		t.Fatalf("PATCH appearance with masked placeholder = %d %s", apPatch2.StatusCode, string(apRaw2))
	}
	snapTC := env.settings.Snapshot().Appearance.ThemeConfigs["secret-skin"]
	savedTok, _ := snapTC["analytics_token"].(string)
	if !strings.HasPrefix(savedTok, "enc:") || snapTC["banner_text"] != "Updated Banner" {
		t.Fatalf("expected encrypted analytics_token to be preserved alongside Updated Banner, got: %+v", snapTC)
	}
}

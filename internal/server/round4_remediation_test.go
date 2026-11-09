package server_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func buildRound4LegacyZipBytes(t *testing.T, htmlHead string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	opts := []map[string]any{
		{"oId": "blogTitle", "optionValue": "Imported Legacy Title"},
		{"oId": "htmlHead", "optionValue": htmlHead},
	}
	rawOpts, _ := json.Marshal(map[string]any{"options": opts})
	w, err := zw.Create("data.json")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(rawOpts)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func postAdminMultipartLegacy(t *testing.T, urlStr string, zipBytes []byte, fields map[string]string, cookies []*http.Cookie, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "legacy.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(zipBytes); err != nil {
		t.Fatal(err)
	}
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, urlStr, &body)
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

func TestRound4_BackupExportStepUp_LegacyImportLockdown_And_OIDCCredentialRevocation(t *testing.T) {
	env, adminCookies, adminHdrs, _ := setupRound2Env(t)

	// 1. Live Backup Export: interactive session without ?ticket= -> 403; with single-use ?ticket= -> 200; PAT with backup:read -> 200
	for _, ep := range []string{"/api/admin/backups/export", "/api/admin/backups/download", "/api/admin/backup/export"} {
		noTicketResp, noTicketBody := doJSONReq(t, http.MethodGet, env.ts.URL+ep, nil, adminCookies, nil)
		if noTicketResp.StatusCode != http.StatusForbidden || !strings.Contains(string(noTicketBody), "step-up password ticket required") {
			t.Fatalf("expected 403 step-up required on %s without ticket, got %d body=%s", ep, noTicketResp.StatusCode, string(noTicketBody))
		}
	}

	tkResp, tkRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/backups/ticket", map[string]any{
		"current_password": "Str0ng!Pass#2026",
	}, adminCookies, adminHdrs)
	if tkResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/backups/ticket = %d %s", tkResp.StatusCode, string(tkRaw))
	}
	var tk struct {
		Ticket string `json:"ticket"`
	}
	_ = json.Unmarshal(tkRaw, &tk)
	expResp, expBytes := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/backups/export?ticket="+url.QueryEscape(tk.Ticket), nil, adminCookies, nil)
	if expResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/admin/backups/export?ticket=... = %d %s", expResp.StatusCode, string(expBytes))
	}
	if _, err := zip.NewReader(bytes.NewReader(expBytes), int64(len(expBytes))); err != nil {
		t.Fatalf("exported zip invalid: %v", err)
	}

	// API token (Bearer volo_pat_...) with backup:read continues working without interactive ticket
	patResp, patRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/security/tokens", map[string]any{
		"name":             "Backup Automation Bot",
		"scopes":           []string{auth.ScopeBackupRead},
		"current_password": "Str0ng!Pass#2026",
	}, adminCookies, adminHdrs)
	if patResp.StatusCode != http.StatusCreated {
		t.Fatalf("create backup PAT = %d %s", patResp.StatusCode, string(patRaw))
	}
	var patOut struct {
		Token string `json:"plaintext_token"`
	}
	_ = json.Unmarshal(patRaw, &patOut)
	patExpResp, patExpBytes := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/backups/export", nil, nil, map[string]string{
		"Authorization": "Bearer " + patOut.Token,
	})
	if patExpResp.StatusCode != http.StatusOK {
		t.Fatalf("Bearer PAT GET /api/admin/backups/export = %d %s", patExpResp.StatusCode, string(patExpBytes))
	}
	if _, err := zip.NewReader(bytes.NewReader(patExpBytes), int64(len(patExpBytes))); err != nil {
		t.Fatalf("PAT exported zip invalid: %v", err)
	}

	// 2. Production POST /api/admin/backups/import-legacy Step-Up + setup-only field lockdown + XSS sanitization
	zipPayload := buildRound4LegacyZipBytes(t, `<link rel="stylesheet" href="https://cdn.example.com/ok.css"><script>alert(1)</script><svg onload="alert(2)"><animate attributeName="href" values="javascript:alert(3)"/></svg>`)

	// Dry-run succeeds without current_password and mutates nothing
	dryResp, dryRaw := postAdminMultipartLegacy(t, env.ts.URL+"/api/admin/backups/import-legacy?dry_run=1", zipPayload, map[string]string{
		"dry_run":      "1",
		"new_site_url": "https://attacker-takeover.example",
	}, adminCookies, adminHdrs)
	if dryResp.StatusCode != http.StatusOK {
		t.Fatalf("dry-run import-legacy = %d %s", dryResp.StatusCode, string(dryRaw))
	}
	if env.cfg.GetPublicURL() != "https://studio.example.com" {
		t.Fatalf("dry-run mutated PublicURL to %q", env.cfg.GetPublicURL())
	}

	// Live import WITHOUT current_password MUST return 400, and wrong password MUST return 401!
	noStepUpResp, _ := postAdminMultipartLegacy(t, env.ts.URL+"/api/admin/backups/import-legacy", zipPayload, map[string]string{
		"dry_run": "0",
	}, adminCookies, adminHdrs)
	if noStepUpResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 when current_password omitted on live import-legacy, got %d", noStepUpResp.StatusCode)
	}
	badStepUpResp, _ := postAdminMultipartLegacy(t, env.ts.URL+"/api/admin/backups/import-legacy", zipPayload, map[string]string{
		"dry_run":          "0",
		"current_password": "WrongPassword!999",
	}, adminCookies, adminHdrs)
	if badStepUpResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 when current_password wrong on live import-legacy, got %d", badStepUpResp.StatusCode)
	}

	// Live import WITH valid current_password ignores setup-only overrides (admin_password, admin_username, new_site_url, driver, dsn) and strips XSS
	liveResp, liveRaw := postAdminMultipartLegacy(t, env.ts.URL+"/api/admin/backups/import-legacy", zipPayload, map[string]string{
		"dry_run":          "0",
		"current_password": "Str0ng!Pass#2026",
		"admin_username":   "hacked_admin",
		"admin_email":      "hacked@evil.example",
		"admin_password":   "Hacked!Pass#9999",
		"new_site_url":     "https://evil-hijack.example",
		"driver":           "postgres",
		"dsn":              "postgres://127.0.0.1:5432/evil",
	}, adminCookies, adminHdrs)
	if liveResp.StatusCode != http.StatusOK {
		t.Fatalf("live import-legacy with step-up = %d %s", liveResp.StatusCode, string(liveRaw))
	}
	if env.cfg.GetPublicURL() != "https://studio.example.com" {
		t.Fatalf("live admin import-legacy hijacked canonical PublicURL to %q", env.cfg.GetPublicURL())
	}
	// Original admin login still works, attacker override password rejected
	origLogin, _ := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/auth/login", map[string]any{
		"ident":    "captain",
		"password": "Str0ng!Pass#2026",
	}, nil, map[string]string{"Origin": env.ts.URL})
	if origLogin.StatusCode != http.StatusOK {
		t.Fatalf("expected original captain password to remain intact, got %d", origLogin.StatusCode)
	}
	headHTML := env.settings.Snapshot().Site.CustomHeadMeta
	if strings.Contains(strings.ToLower(headHTML), "<script") || strings.Contains(strings.ToLower(headHTML), "<svg") {
		t.Fatalf("expected legacy htmlHead XSS stripped, got %q", headHTML)
	}

	// 3. OIDC Staff Bind Step-Up + Cross-Session Rejection + Revocation on Demote / Password Reset / Disable
	probe := &round2TestPlugin{id: "r4-oidc-plugin"}
	_ = env.pluginMgr.RegisterBuiltin(env.ctx, probe)
	_ = env.pluginMgr.Enable(env.ctx, probe.id)
	advOIDC := &round3AdvancedOIDCProvider{name: "okta"}
	env.pluginMgr.Providers().RegisterOIDC(probe.id, advOIDC)

	// oidc-links/begin rejects missing password (400), wrong password (401) & cross-origin Origin (403)
	noPwdBegin, _ := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/security/oidc-links/begin", map[string]any{
		"provider": "okta",
	}, adminCookies, adminHdrs)
	if noPwdBegin.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 from oidc-links/begin without current_password, got %d", noPwdBegin.StatusCode)
	}
	badPwdBegin, _ := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/security/oidc-links/begin", map[string]any{
		"provider":         "okta",
		"current_password": "WrongPassword!999",
	}, adminCookies, adminHdrs)
	if badPwdBegin.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 from oidc-links/begin with wrong current_password, got %d", badPwdBegin.StatusCode)
	}
	crossOriginBegin, _ := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/security/oidc-links/begin", map[string]any{
		"provider":         "okta",
		"current_password": "Str0ng!Pass#2026",
	}, adminCookies, map[string]string{
		auth.CSRFHeaderName: adminHdrs[auth.CSRFHeaderName],
		"Origin":            "https://evil.example",
	})
	if crossOriginBegin.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 on cross-origin oidc-links/begin, got %d", crossOriginBegin.StatusCode)
	}

	// Create a staff editor, bind OIDC to that editor, then verify credential revocation on password reset / demote / disable
	createEdResp, createEdRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/users", map[string]any{
		"username":         "staff_sso",
		"email":            "staff_sso@example.com",
		"display_name":     "Staff SSO",
		"role":             "editor",
		"password":         "Staff!Pass#2026",
		"current_password": "Str0ng!Pass#2026",
	}, adminCookies, adminHdrs)
	if createEdResp.StatusCode != http.StatusCreated {
		t.Fatalf("create staff_sso = %d %s", createEdResp.StatusCode, string(createEdRaw))
	}
	var createdEd struct {
		User domain.User `json:"user"`
	}
	_ = json.Unmarshal(createEdRaw, &createdEd)
	edID := createdEd.User.ID

	bindStaffOIDC := func(pwd string) {
		t.Helper()
		edLogResp, edLogRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/auth/login", map[string]any{
			"ident":    "staff_sso",
			"password": pwd,
		}, nil, map[string]string{"Origin": env.ts.URL})
		if edLogResp.StatusCode != http.StatusOK {
			t.Fatalf("staff_sso login = %d %s", edLogResp.StatusCode, string(edLogRaw))
		}
		var edLogOut struct {
			CSRFToken string `json:"csrf_token"`
		}
		_ = json.Unmarshal(edLogRaw, &edLogOut)
		edHdrs := map[string]string{
			auth.CSRFHeaderName: edLogOut.CSRFToken,
			"Origin":            env.ts.URL,
		}
		bgResp, bgRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/security/oidc-links/begin", map[string]any{
			"provider":         "okta",
			"current_password": pwd,
		}, edLogResp.Cookies(), edHdrs)
		if bgResp.StatusCode != http.StatusOK {
			t.Fatalf("staff_sso oidc-links/begin = %d %s", bgResp.StatusCode, string(bgRaw))
		}
		var bgOut struct {
			RedirectURL string `json:"redirect_url"`
		}
		_ = json.Unmarshal(bgRaw, &bgOut)

		// Verify admin session CANNOT hijack staff_sso's bind_ticket!
		wrongSessStart, _ := doJSONReq(t, http.MethodGet, env.ts.URL+bgOut.RedirectURL, nil, adminCookies, nil)
		if wrongSessStart.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403 when different staff session uses bind_ticket, got %d", wrongSessStart.StatusCode)
		}

		// Mint fresh ticket and complete bind for staff_sso
		bgResp2, bgRaw2 := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/security/oidc-links/begin", map[string]any{
			"provider":         "okta",
			"current_password": pwd,
		}, edLogResp.Cookies(), edHdrs)
		if bgResp2.StatusCode != http.StatusOK {
			t.Fatalf("second oidc-links/begin = %d %s", bgResp2.StatusCode, string(bgRaw2))
		}
		_ = json.Unmarshal(bgRaw2, &bgOut)
		bStart, _ := doJSONReq(t, http.MethodGet, env.ts.URL+bgOut.RedirectURL, nil, edLogResp.Cookies(), nil)
		if bStart.StatusCode != http.StatusFound {
			t.Fatalf("expected 302 from valid bind_ticket start, got %d", bStart.StatusCode)
		}
		bURL, _ := url.Parse(bStart.Header.Get("Location"))
		bCookies := append(append([]*http.Cookie{}, edLogResp.Cookies()...), bStart.Cookies()...)
		bCB, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/auth/oidc/okta/callback?code=admin-sub&state="+url.QueryEscape(bURL.Query().Get("state")), nil, bCookies, nil)
		if bCB.StatusCode != http.StatusFound {
			t.Fatalf("expected 302 from bind callback, got %d", bCB.StatusCode)
		}
		var cnt int
		_ = env.store.QueryRowContext(env.ctx, `SELECT COUNT(*) FROM user_credentials WHERE user_id = ? AND kind = 'oidc'`, edID).Scan(&cnt)
		if cnt != 1 {
			t.Fatalf("expected 1 bound OIDC credential on staff_sso, got %d", cnt)
		}
	}

	// Case A: Admin resets staff_sso password -> OIDC credential revoked
	bindStaffOIDC("Staff!Pass#2026")
	rstResp, rstRaw := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/users/"+edID, map[string]any{
		"password":         "Staff!Pass#2027",
		"current_password": "Str0ng!Pass#2026",
	}, adminCookies, adminHdrs)
	if rstResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH reset password = %d %s", rstResp.StatusCode, string(rstRaw))
	}
	var oidcCnt int
	_ = env.store.QueryRowContext(env.ctx, `SELECT COUNT(*) FROM user_credentials WHERE user_id = ? AND kind = 'oidc'`, edID).Scan(&oidcCnt)
	if oidcCnt != 0 {
		t.Fatalf("expected OIDC credentials revoked after admin password reset, got %d", oidcCnt)
	}

	// Case B: Admin disables staff_sso -> OIDC credential revoked
	bindStaffOIDC("Staff!Pass#2027")
	disResp, _ := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/users/"+edID, map[string]any{
		"status":           "disabled",
		"current_password": "Str0ng!Pass#2026",
	}, adminCookies, adminHdrs)
	if disResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH disable user = %d", disResp.StatusCode)
	}
	_ = env.store.QueryRowContext(env.ctx, `SELECT COUNT(*) FROM user_credentials WHERE user_id = ? AND kind = 'oidc'`, edID).Scan(&oidcCnt)
	if oidcCnt != 0 {
		t.Fatalf("expected OIDC credentials revoked after user disable, got %d", oidcCnt)
	}

	// Re-enable, bind again, then demote editor -> reader -> OIDC credential revoked
	_, _ = doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/users/"+edID, map[string]any{
		"status":           "active",
		"current_password": "Str0ng!Pass#2026",
	}, adminCookies, adminHdrs)
	bindStaffOIDC("Staff!Pass#2027")
	demResp, _ := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/users/"+edID, map[string]any{
		"role":             "reader",
		"current_password": "Str0ng!Pass#2026",
	}, adminCookies, adminHdrs)
	if demResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH demote user = %d", demResp.StatusCode)
	}
	_ = env.store.QueryRowContext(env.ctx, `SELECT COUNT(*) FROM user_credentials WHERE user_id = ? AND kind = 'oidc'`, edID).Scan(&oidcCnt)
	if oidcCnt != 0 {
		t.Fatalf("expected OIDC credentials revoked after role demotion, got %d", oidcCnt)
	}

	// 4. POST /api/admin/users username validation & email collision rejection
	for _, badUsername := range []string{"bad@name", "bad/name", "bad name", "CAPTAIN@EXAMPLE.COM"} {
		badResp, _ := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/users", map[string]any{
			"username":         badUsername,
			"email":            "unique_new@example.com",
			"display_name":     "Bad",
			"role":             "reader",
			"password":         "Valid!Pass#2026",
			"current_password": "Str0ng!Pass#2026",
		}, adminCookies, adminHdrs)
		if badResp.StatusCode != http.StatusBadRequest && badResp.StatusCode != http.StatusConflict {
			t.Fatalf("expected 400/409 for invalid/colliding username %q, got %d", badUsername, badResp.StatusCode)
		}
	}
}

func TestRound4_RenderPreview_Projections_BatchTx_And_SetupJapaneseLocale(t *testing.T) {
	env, adminCookies, adminHdrs, _ := setupRound2Env(t)

	// 1. POST /api/admin/render/preview with preserve_directives=true vs false
	prevNormResp, prevNormRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/render/preview", map[string]any{
		"content_md":          ":::callout type=\"info\"\nHello directive\n:::\n",
		"locale":              "en-US",
		"preserve_directives": false,
	}, adminCookies, adminHdrs)
	if prevNormResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/render/preview (normal) = %d %s", prevNormResp.StatusCode, string(prevNormRaw))
	}
	prevPresResp, prevPresRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/render/preview", map[string]any{
		"content_md":          ":::callout type=\"info\"\nHello directive\n:::\n",
		"locale":              "en-US",
		"preserve_directives": true,
	}, adminCookies, adminHdrs)
	if prevPresResp.StatusCode != http.StatusOK || !strings.Contains(string(prevPresRaw), "data-volo-md-b64") {
		t.Fatalf("expected preserve_directives=true to emit data-volo-md-b64 container, status=%d body=%s", prevPresResp.StatusCode, string(prevPresRaw))
	}

	// 2. Pages list omits content_md while single page GET returns full content_md
	pgCreateResp, pgCreateRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/pages", map[string]any{
		"title":         "About Studio",
		"slug":          "about-studio",
		"content_md":    "Full standalone page markdown body for projection verification.",
		"is_published":  true,
		"allow_comment": true,
	}, adminCookies, adminHdrs)
	if pgCreateResp.StatusCode != http.StatusCreated {
		t.Fatalf("create page = %d %s", pgCreateResp.StatusCode, string(pgCreateRaw))
	}
	var pgCreated struct {
		Page domain.Page `json:"page"`
	}
	_ = json.Unmarshal(pgCreateRaw, &pgCreated)

	pgListResp, pgListRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/pages", nil, adminCookies, nil)
	if pgListResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/admin/pages = %d %s", pgListResp.StatusCode, string(pgListRaw))
	}
	var pgList struct {
		Items []domain.Page `json:"items"`
	}
	_ = json.Unmarshal(pgListRaw, &pgList)
	foundPageInList := false
	for _, p := range pgList.Items {
		if p.ID == pgCreated.Page.ID {
			foundPageInList = true
			if p.ContentMD != "" {
				t.Fatalf("expected GET /api/admin/pages list to project empty content_md, got %q", p.ContentMD)
			}
		}
	}
	if !foundPageInList {
		t.Fatal("created page missing from GET /api/admin/pages")
	}
	pgDetailResp, pgDetailRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/pages/"+pgCreated.Page.ID, nil, adminCookies, nil)
	if pgDetailResp.StatusCode != http.StatusOK || !strings.Contains(string(pgDetailRaw), "Full standalone page markdown body") {
		t.Fatalf("expected GET /api/admin/pages/{id} to include full content_md, got %d %s", pgDetailResp.StatusCode, string(pgDetailRaw))
	}

	// 3. Article Revisions ?summary=1 omits content_md while full list and single revision GET return content_md
	artResp, artRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"title":         "Revision Test Post",
		"slug":          "revision-test-post",
		"summary_md":    "Short summary",
		"content_md":    "Version ONE full article markdown body",
		"status":        "draft",
		"allow_comment": true,
	}, adminCookies, adminHdrs)
	if artResp.StatusCode != http.StatusCreated {
		t.Fatalf("create article = %d %s", artResp.StatusCode, string(artRaw))
	}
	var artCreated struct {
		Article domain.RawArticle `json:"article"`
	}
	_ = json.Unmarshal(artRaw, &artCreated)
	artID := artCreated.Article.ID

	// Update article to record a revision snapshot of Version ONE
	updResp, updRaw := doJSONReq(t, http.MethodPut, env.ts.URL+"/api/admin/articles/"+artID, map[string]any{
		"title":         "Revision Test Post v2",
		"slug":          "revision-test-post",
		"summary_md":    "Short summary",
		"content_md":    "Version TWO full article markdown body",
		"status":        "draft",
		"allow_comment": true,
		"revision_note": "second draft",
	}, adminCookies, adminHdrs)
	if updResp.StatusCode != http.StatusOK {
		t.Fatalf("update article = %d %s", updResp.StatusCode, string(updRaw))
	}

	revSumResp, revSumRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/articles/"+artID+"/revisions?summary=1", nil, adminCookies, nil)
	if revSumResp.StatusCode != http.StatusOK {
		t.Fatalf("GET revisions?summary=1 = %d %s", revSumResp.StatusCode, string(revSumRaw))
	}
	var revSum struct {
		Items []domain.ArticleRevision `json:"items"`
	}
	_ = json.Unmarshal(revSumRaw, &revSum)
	if len(revSum.Items) == 0 || revSum.Items[0].ContentMD != "" {
		t.Fatalf("expected revisions?summary=1 to omit content_md, got %+v", revSum.Items)
	}

	revFullResp, revFullRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/articles/"+artID+"/revisions", nil, adminCookies, nil)
	var revFull struct {
		Items []domain.ArticleRevision `json:"items"`
	}
	_ = json.Unmarshal(revFullRaw, &revFull)
	if revFullResp.StatusCode != http.StatusOK || len(revFull.Items) == 0 || revFull.Items[0].ContentMD == "" {
		t.Fatalf("expected full revisions list to include content_md, got %d %+v", revFullResp.StatusCode, revFull.Items)
	}

	// 4. Batch publish via POST /api/admin/articles/batch & batch approve via POST /api/admin/comments/batch
	batchPubResp, batchPubRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles/batch", map[string]any{
		"action": "publish",
		"ids":    []string{artID},
	}, adminCookies, adminHdrs)
	if batchPubResp.StatusCode != http.StatusOK || !strings.Contains(string(batchPubRaw), `"affected":1`) {
		t.Fatalf("POST /api/admin/articles/batch = %d %s", batchPubResp.StatusCode, string(batchPubRaw))
	}

	if _, err := env.store.ExecContext(env.ctx, `INSERT INTO comments (
		id, target_type, target_id, parent_id, reply_to_name, user_id, guest_id_hash, env_fp_hash,
		author_name, author_url, avatar_url, notify_email_ciphertext, notify_locale,
		client_ip, ip_masked, geo_region, content_md, status, is_deleted, created_at, updated_at
	) VALUES
		('cmt_b1','article',?,'','','','','','Reader A','','','','','','','','Nice post','pending',0,1700000000000,1700000000000),
		('cmt_b2','article',?,'','','','','','Reader B','','','','','','','','Great read','pending',0,1700000001000,1700000001000)`, artID, artID); err != nil {
		t.Fatalf("seed comments failed: %v", err)
	}

	batchCmtResp, batchCmtRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/comments/batch", map[string]any{
		"action": "approve",
		"ids":    []string{"cmt_b1", "cmt_b2"},
	}, adminCookies, adminHdrs)
	if batchCmtResp.StatusCode != http.StatusOK || !strings.Contains(string(batchCmtRaw), `"affected":2`) {
		t.Fatalf("POST /api/admin/comments/batch = %d %s", batchCmtResp.StatusCode, string(batchCmtRaw))
	}
	var recounted int
	_ = env.store.QueryRowContext(env.ctx, `SELECT comment_count FROM articles WHERE id = ?`, artID).Scan(&recounted)
	if recounted != 2 {
		t.Fatalf("expected article comment_count=2 after batch approve, got %d", recounted)
	}

	// Overview KPI check
	ovResp, ovRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/overview", nil, adminCookies, nil)
	if ovResp.StatusCode != http.StatusOK || !strings.Contains(string(ovRaw), `"published":`) {
		t.Fatalf("GET /api/admin/overview = %d %s", ovResp.StatusCode, string(ovRaw))
	}

	// 5. Setup Wizard public_url persistence + ja-JP starter content & Japanese defaults
	ctx := context.Background()
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "data")
	uploadDir := filepath.Join(tmp, "uploads")
	pluginsDir := filepath.Join(tmp, "plugins")
	themesDir := filepath.Join(tmp, "themes")
	backupDir := filepath.Join(tmp, "backups")
	for _, d := range []string{dataDir, uploadDir, pluginsDir, themesDir, backupDir} {
		_ = os.MkdirAll(d, 0o750)
	}
	secret := "0123456789abcdef0123456789abcdef-round4-ja-secret"
	cfgJA, err := config.Load(config.CLIOverrides{
		SecretKey:  &secret,
		DataDir:    &dataDir,
		UploadDir:  &uploadDir,
		PluginsDir: &pluginsDir,
		ThemesDir:  &themesDir,
		BackupDir:  &backupDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	storeJA, err := sqlx.Open(ctx, sqlx.Options{Driver: "sqlite", DSN: filepath.Join(dataDir, "ja.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer storeJA.Close()
	if err := migrate.Up(ctx, storeJA); err != nil {
		t.Fatal(err)
	}
	catJA, _ := i18n.NewCatalog("ja-JP", filepath.Join("..", "..", "locales"))
	settingsJA := repo.NewSettingsManager(storeJA, cfgJA.KeyRing, "ja-JP")
	_, _ = settingsJA.Reload(ctx)
	dialJA := &security.OutboundSafeDialer{Timeout: 5 * time.Second, AllowLoopbackForTests: true}
	plugJA := plugin.NewManager(plugin.ManagerOptions{Store: storeJA, Keys: cfgJA.KeyRing, PluginsDir: pluginsDir, DataDir: dataDir, Catalog: catJA})
	_ = plugJA.DiscoverAndStart(ctx)
	defer plugJA.Close(ctx)
	passJA := guest.NewPassportManager(cfgJA.KeyRing, "")
	powJA := guest.NewPoWVerifier(cfgJA.KeyRing)
	rndJA := render.Default()
	grdJA := visibility.NewGuard(cfgJA.KeyRing, settingsJA, passJA, catJA, rndJA)
	srchJA := visibility.NewSearchIndex(storeJA, rndJA, 1<<20)
	cntJA := content.NewService(content.ServiceOptions{
		Store:     storeJA,
		Keys:      cfgJA.KeyRing,
		Settings:  settingsJA,
		Passports: passJA,
		PoW:       powJA,
		Guard:     grdJA,
		Search:    srchJA,
		Renderer:  rndJA,
		Plugins:   plugJA,
		Catalog:   catJA,
	})
	authJA := auth.NewService(storeJA, cfgJA.KeyRing, settingsJA, "")
	dispJA := notify.NewDispatcher(notify.DispatcherOptions{
		Store:    storeJA,
		Keys:     cfgJA.KeyRing,
		Settings: settingsJA,
		Catalog:  catJA,
		Plugins:  plugJA,
	})
	srvJA, err := server.NewAppServer(server.AppDeps{
		Config:         cfgJA,
		Store:          storeJA,
		Settings:       settingsJA,
		Catalog:        catJA,
		Auth:           authJA,
		Passports:      passJA,
		PoW:            powJA,
		OutboundDialer: dialJA,
		Plugins:        plugJA,
		Content:        cntJA,
		TestSMTP:       dispJA.SendTestSMTP,
		TestWebhook:    dispJA.TriggerTestWebhook,
	})
	if err != nil {
		t.Fatal(err)
	}
	tsJA := httptest.NewServer(srvJA.Handler())
	defer tsJA.Close()

	jaSetupResp, jaSetupRaw := doJSONReq(t, http.MethodPost, tsJA.URL+"/api/setup/complete", map[string]any{
		"site_title":         "東京スタジオ",
		"default_locale":     "ja-JP",
		"timezone":           "Asia/Tokyo",
		"public_url":         "https://tokyo.example.jp/",
		"admin_username":     "kenji",
		"admin_email":        "kenji@example.jp",
		"admin_display_name": "Kenji",
		"admin_password":     "Str0ng!Pass#2026",
	}, nil, map[string]string{"Origin": tsJA.URL})
	if jaSetupResp.StatusCode != http.StatusOK {
		t.Fatalf("ja-JP setup/complete = %d %s", jaSetupResp.StatusCode, string(jaSetupRaw))
	}
	if cfgJA.GetPublicURL() != "https://tokyo.example.jp" {
		t.Fatalf("expected setup public_url persisted into runtime Config without trailing slash, got %q", cfgJA.GetPublicURL())
	}
	if settingsJA.Snapshot().Site.CustomVars["public_url"] != "https://tokyo.example.jp" {
		t.Fatalf("expected site.custom_vars[public_url] persisted, got %+v", settingsJA.Snapshot().Site.CustomVars)
	}
	if settingsJA.Snapshot().Site.DefaultLocale != "ja-JP" || settingsJA.Snapshot().Site.Subtitle != "記録 · 思索 · 共有" {
		t.Fatalf("expected ja-JP locale and Japanese subtitle, got locale=%q subtitle=%q",
			settingsJA.Snapshot().Site.DefaultLocale, settingsJA.Snapshot().Site.Subtitle)
	}
	var jaAboutTitle, jaAboutBody, jaCatName string
	_ = storeJA.QueryRowContext(ctx, `SELECT title, content_md FROM pages WHERE slug = 'about'`).Scan(&jaAboutTitle, &jaAboutBody)
	_ = storeJA.QueryRowContext(ctx, `SELECT name FROM categories WHERE slug = 'notes'`).Scan(&jaCatName)
	if jaAboutTitle != "概要" || !strings.Contains(jaAboutBody, "ようこそ") || jaCatName != "ノート" {
		t.Fatalf("expected Japanese starter About page & category, got title=%q cat=%q body=%q", jaAboutTitle, jaCatName, jaAboutBody)
	}
}

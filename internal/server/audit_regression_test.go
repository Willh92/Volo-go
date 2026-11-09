package server_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/auth"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/pkg/id"
)

func bootstrapAuditEnv(t *testing.T) (*m5Env, []*http.Cookie, map[string]string, string) {
	t.Helper()
	env := setupM5TestEnv(t, "/console")

	// Before setup completes, test Setup DB DSN SSRF & MySQL LFI protections!
	resp, raw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/setup/test-db", map[string]any{
		"driver": "mysql",
		"dsn":    "user:pass@tcp(8.8.8.8:3306)/blog?allowAllFiles=true",
	}, nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected MySQL allowAllFiles=true to be rejected with 400, got %d: %s", resp.StatusCode, string(raw))
	}

	resp, raw = doJSONReq(t, http.MethodPost, env.ts.URL+"/api/setup/test-db", map[string]any{
		"driver": "postgres",
		"dsn":    "postgres://user:pass@8.8.8.8,169.254.169.254:5432/blog",
	}, nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected Postgres multi-host SSRF fallback 169.254.169.254 to be rejected with 400, got %d: %s", resp.StatusCode, string(raw))
	}

	// Complete setup
	resp, compRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/setup/complete", map[string]any{
		"site_title":         "Audit Studio",
		"site_subtitle":      "Security & Scale",
		"site_description":   "Audit Regression Suite",
		"default_locale":     "en",
		"timezone":           "UTC",
		"accent":             "teal",
		"admin_username":     "captain",
		"admin_email":        "captain@example.com",
		"admin_display_name": "Captain Volo",
		"admin_password":     "Str0ng!Pass#2026",
	}, nil, nil)
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
	hdrs := map[string]string{auth.CSRFHeaderName: comp.CSRFToken}
	return env, resp.Cookies(), hdrs, comp.User.ID
}

func TestAudit_Sanitization_FeedXSS_And_UnpublishedTargetPrivacy(t *testing.T) {
	env, adminCookies, adminHdrs, _ := bootstrapAuditEnv(t)

	// 1. Custom CSS UTF-8 U+023A / U+212A non-panic & </style> breakout defense + Admin Head & Footer Script/Ad Injection
	resp, raw := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/appearance", map[string]any{
		"active_theme": "studio",
		"default_mode": "system",
		"accent":       "teal",
		"custom_css":   "ȺK body { background: url(javascript:alert(1)); } </style><script>alert(9)</script>",
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH settings/appearance = %d %s", resp.StatusCode, string(raw))
	}

	resp, raw = doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/site", map[string]any{
		"title":              "Audit Studio",
		"default_locale":     "en",
		"timezone":           "UTC",
		"favicon_url":        "/favicon.svg",
		"enable_footer_html": true,
		"custom_head_meta":   `<meta name="google-site-verification" content="ok123"><script async src="https://pagead2.googlesyndication.com/pagead/js/adsbygoogle.js?client=ca-pub-999"></script>`,
		"footer_text":        `<ins class="adsbygoogle" data-ad-client="ca-pub-999"></ins><script>(adsbygoogle = window.adsbygoogle || []).push({});</script>`,
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH settings/site = %d %s", resp.StatusCode, string(raw))
	}

	// Enable PJAX reading setting test & verify home HTML
	resp, homeBytes := doJSONReq(t, http.MethodGet, env.ts.URL+"/", nil, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d", resp.StatusCode)
	}
	homeHTML := string(homeBytes)
	if strings.Contains(strings.ToLower(homeHTML), "</style><script>") {
		t.Fatalf("CustomCSS breakout escaped into rendered home HTML:\n%s", homeHTML)
	}
	if !strings.Contains(homeHTML, `google-site-verification`) ||
		!strings.Contains(homeHTML, `googlesyndication.com/pagead/js/adsbygoogle.js`) ||
		!strings.Contains(homeHTML, `data-ad-client="ca-pub-999"`) {
		t.Fatalf("expected Admin <head> & footer Google script injection in home HTML:\n%s", homeHTML)
	}

	// 2. Feed Stored XSS Fix: article summary containing backticked `<img src=x onerror=alert(1)>` (which stays literal in PlainText) MUST be escaped in RSS!
	resp, artRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"title":         "RSS Summary Escape Post",
		"slug":          "rss-summary-escape",
		"summary_md":    "Summary payload `<img src=x onerror=alert(1)>` & literal",
		"content_md":    "Safe body text.",
		"status":        "published",
		"allow_comment": true,
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/admin/articles = %d %s", resp.StatusCode, string(artRaw))
	}

	// Switch feed output_mode to summary so guard.ToFeedItem summary path is rendered in RSS
	resp, feedSetRaw := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/feed", map[string]any{
		"output_mode":         "summary",
		"item_count":          20,
		"enable_llms_txt":     true,
		"enable_raw_markdown": false,
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH settings/feed = %d %s", resp.StatusCode, string(feedSetRaw))
	}
	env.content.PurgeRuntimeCaches()

	resp, rssBytes := doJSONReq(t, http.MethodGet, env.ts.URL+"/rss.xml", nil, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /rss.xml = %d", resp.StatusCode)
	}
	rssStr := string(rssBytes)
	if strings.Contains(rssStr, "<img src=x onerror=alert(1)>") {
		t.Fatalf("unescaped <img> XSS found in /rss.xml:\n%s", rssStr)
	}
	if !strings.Contains(rssStr, "&lt;img src=x onerror=alert(1)&gt;") && !strings.Contains(rssStr, "&amp;lt;img") {
		t.Fatalf("expected escaped summary in /rss.xml, got:\n%s", rssStr)
	}

	// 3. Unpublished Draft Article: Comments & Reactions MUST return 404 to anonymous/public callers!
	resp, draftRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"title":         "Secret Draft",
		"slug":          "secret-draft",
		"content_md":    "Unpublished secret",
		"status":        "draft",
		"allow_comment": true,
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create draft = %d %s", resp.StatusCode, string(draftRaw))
	}
	var draftRes struct {
		Article domain.RawArticle `json:"article"`
	}
	_ = json.Unmarshal(draftRaw, &draftRes)

	// Seed an approved comment on that article (as if article was previously published then reverted to draft)
	cmtID := id.Next()
	now := time.Now().UnixMilli()
	_, err := env.store.ExecContext(env.ctx,
		`INSERT INTO comments (
			id, target_type, target_id, parent_id, reply_to_name, user_id, guest_id_hash, env_fp_hash,
			author_name, author_url, avatar_url, notify_email_ciphertext, notify_locale,
			client_ip, ip_masked, geo_region, content_md, status, is_deleted, created_at, updated_at
		) VALUES (?, 'article', ?, '', '', '', 'tok', '', 'Alice', '', '', '', 'en', '1.2.3.4', '1.2.*.*', '', 'Private comment on reverted draft', 'approved', 0, ?, ?)`,
		cmtID, draftRes.Article.ID, now, now)
	if err != nil {
		t.Fatal(err)
	}

	// Anonymous GET /api/v1/comments on draft MUST return 404 (not leak 'Private comment on reverted draft'!)
	resp, leakRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/comments?target_type=article&target_id="+draftRes.Article.ID, nil, nil, nil)
	if resp.StatusCode != http.StatusNotFound || strings.Contains(string(leakRaw), "Private comment") {
		t.Fatalf("expected 404 on draft comments list, got %d: %s", resp.StatusCode, string(leakRaw))
	}

	// Anonymous POST /api/v1/reactions on draft MUST return 404!
	resp, _ = doJSONReq(t, http.MethodPost, env.ts.URL+"/api/v1/reactions", map[string]any{
		"target_type": "article",
		"target_id":   draftRes.Article.ID,
		"emoji":       "heart",
	}, nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 on draft reaction toggle, got %d", resp.StatusCode)
	}
}

func TestAudit_MultiLockIsolation_And_StandalonePageLocks(t *testing.T) {
	env, adminCookies, adminHdrs, _ := bootstrapAuditEnv(t)

	// Create article with TWO independent password locks + ONE user-login lock
	multiMD := strings.Join([]string{
		"Public intro.",
		"",
		`:::lock pwd="alpha"`,
		"SECRET_ALPHA_PAYLOAD",
		":::",
		"",
		`:::lock pwd="bravo"`,
		"SECRET_BRAVO_PAYLOAD",
		":::",
		"",
		`:::lock type="user"`,
		"SECRET_READER_PAYLOAD",
		":::",
	}, "\n")

	resp, artRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"title":      "Multi Lock Article",
		"slug":       "multi-lock-article",
		"content_md": multiMD,
		"status":     "published",
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create multi-lock article = %d %s", resp.StatusCode, string(artRaw))
	}
	var createdArt struct {
		Article domain.RawArticle `json:"article"`
	}
	_ = json.Unmarshal(artRaw, &createdArt)

	// Create standalone page with password lock
	pageMD := "About intro.\n\n:::lock pwd=\"pagepass\"\nSTANDALONE_PAGE_SECRET_99\n:::\n"
	resp, pgRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/pages", map[string]any{
		"title":        "Vault Page",
		"slug":         "vault-page",
		"content_md":   pageMD,
		"is_published": true,
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create locked page = %d %s", resp.StatusCode, string(pgRaw))
	}
	var createdPage struct {
		Page domain.Page `json:"page"`
	}
	_ = json.Unmarshal(pgRaw, &createdPage)

	// 1. Anonymous visitor viewing /posts/multi-lock-article sees NO secrets
	resp, htmlBytes := doJSONReq(t, http.MethodGet, env.ts.URL+"/posts/multi-lock-article", nil, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /posts/multi-lock-article = %d", resp.StatusCode)
	}
	body := string(htmlBytes)
	for _, secret := range []string{"SECRET_ALPHA_PAYLOAD", "SECRET_BRAVO_PAYLOAD", "SECRET_READER_PAYLOAD"} {
		if strings.Contains(body, secret) {
			t.Fatalf("locked secret %s leaked to anonymous viewer!", secret)
		}
	}

	// 2. Unlock ONLY `alpha` -> reveals SECRET_ALPHA_PAYLOAD ONLY, NEVER bravo or user lock!
	resp, unlRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/v1/articles/"+createdArt.Article.ID+"/unlock", map[string]any{
		"password": "alpha",
	}, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unlock alpha = %d %s", resp.StatusCode, string(unlRaw))
	}
	guestCookies := resp.Cookies()

	resp, htmlAfterAlpha := doJSONReq(t, http.MethodGet, env.ts.URL+"/posts/multi-lock-article", nil, guestCookies, nil)
	afterAlpha := string(htmlAfterAlpha)
	if !strings.Contains(afterAlpha, "SECRET_ALPHA_PAYLOAD") {
		t.Fatalf("expected SECRET_ALPHA_PAYLOAD after unlocking alpha")
	}
	if strings.Contains(afterAlpha, "SECRET_BRAVO_PAYLOAD") || strings.Contains(afterAlpha, "SECRET_READER_PAYLOAD") {
		t.Fatalf("multi-lock isolation failed! Unlocking alpha leaked bravo or reader lock:\n%s", afterAlpha)
	}

	// 3. Standalone page /vault-page MUST hide STANDALONE_PAGE_SECRET_99 from anonymous visitor AND /llms-full.txt!
	resp, vaultBytes := doJSONReq(t, http.MethodGet, env.ts.URL+"/vault-page", nil, nil, nil)
	if resp.StatusCode != http.StatusOK || strings.Contains(string(vaultBytes), "STANDALONE_PAGE_SECRET_99") {
		t.Fatalf("standalone page :::lock leaked to anonymous visitor! status=%d body=%s", resp.StatusCode, string(vaultBytes))
	}

	env.content.PurgeRuntimeCaches()
	resp, llmsBytes := doJSONReq(t, http.MethodGet, env.ts.URL+"/llms-full.txt", nil, nil, nil)
	if resp.StatusCode != http.StatusOK || strings.Contains(string(llmsBytes), "STANDALONE_PAGE_SECRET_99") {
		t.Fatalf("standalone page :::lock leaked in /llms-full.txt!")
	}

	// Unlock standalone page via /api/v1/articles/{pageID}/unlock with target_type=page
	resp, pgUnlRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/v1/articles/"+createdPage.Page.ID+"/unlock", map[string]any{
		"target_type": "page",
		"password":    "pagepass",
	}, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unlock standalone page = %d %s", resp.StatusCode, string(pgUnlRaw))
	}
	resp, vaultUnlocked := doJSONReq(t, http.MethodGet, env.ts.URL+"/vault-page", nil, resp.Cookies(), nil)
	if !strings.Contains(string(vaultUnlocked), "STANDALONE_PAGE_SECRET_99") {
		t.Fatalf("expected STANDALONE_PAGE_SECRET_99 after unlocking standalone page")
	}
}

func TestAudit_AdminSecurity_Pagination_PasswordPreservation_And_BgAuditLog(t *testing.T) {
	env, adminCookies, adminHdrs, _ := bootstrapAuditEnv(t)

	// 1. TOTP Setup MUST return non-empty qr_svg (<svg ...)
	resp, totpRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/security/totp/setup", map[string]any{}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("totp/setup = %d %s", resp.StatusCode, string(totpRaw))
	}
	var totpSetup struct {
		Secret string `json:"secret"`
		QRSVG  string `json:"qr_svg"`
	}
	_ = json.Unmarshal(totpRaw, &totpSetup)
	if !strings.HasPrefix(totpSetup.QRSVG, "<svg ") || !strings.Contains(totpSetup.QRSVG, "<path ") {
		t.Fatalf("expected valid QR SVG from totp/setup, got: %q", totpSetup.QRSVG)
	}

	// 2. Create password-protected article, then update without password/clear_password -> MUST preserve password!
	pwd := "top-secret-42"
	resp, createRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"title":         "Vault Article",
		"slug":          "vault-article",
		"content_md":    "Vault body",
		"status":        "published",
		"password":      pwd,
		"allow_comment": true,
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create vault article = %d %s", resp.StatusCode, string(createRaw))
	}
	var created struct {
		Article domain.RawArticle `json:"article"`
	}
	_ = json.Unmarshal(createRaw, &created)
	artID := created.Article.ID

	// Update article sending `"password": ""` WITHOUT `"clear_password": true`
	resp, updRaw := doJSONReq(t, http.MethodPut, env.ts.URL+"/api/admin/articles/"+artID, map[string]any{
		"title":          "Vault Article Updated",
		"slug":           "vault-article",
		"content_md":     "Vault body v2",
		"status":         "published",
		"password":       "",
		"clear_password": false,
		"allow_comment":  true,
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update vault article = %d %s", resp.StatusCode, string(updRaw))
	}
	var updRes struct {
		IsPasswordProtected bool `json:"is_password_protected"`
	}
	_ = json.Unmarshal(updRaw, &updRes)
	if !updRes.IsPasswordProtected {
		t.Fatalf("updating article with empty password and clear_password=false wiped existing password!")
	}

	// 3. Issue a read-only PAT (`articles:read` ONLY) -> MUST redact `"password"` on GET /api/admin/articles/{id} AND reject PATCH /api/admin/me!
	patResp, patCreatedRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/security/tokens", map[string]any{
		"name":             "readonly-pat",
		"scopes":           []string{"articles:read"},
		"expires_in_days":  30,
		"current_password": "Str0ng!Pass#2026",
	}, adminCookies, adminHdrs)
	if patResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/admin/security/tokens = %d %s", patResp.StatusCode, string(patCreatedRaw))
	}
	var patOut struct {
		PlaintextToken string `json:"plaintext_token"`
	}
	_ = json.Unmarshal(patCreatedRaw, &patOut)
	patHdrs := map[string]string{"Authorization": "Bearer " + patOut.PlaintextToken}

	resp, getByPATRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/articles/"+artID, nil, nil, patHdrs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET article with read PAT = %d %s", resp.StatusCode, string(getByPATRaw))
	}
	var patArt struct {
		IsPasswordProtected bool   `json:"is_password_protected"`
		Password            string `json:"password"`
	}
	_ = json.Unmarshal(getByPATRaw, &patArt)
	if !patArt.IsPasswordProtected || patArt.Password != "" {
		t.Fatalf("expected read-only PAT to see is_password_protected=true and redacted password=\"\", got %+v", patArt)
	}

	// PATCH /api/admin/me with PAT MUST return 403 Forbidden
	resp, _ = doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/me", map[string]any{
		"display_name": "Hacked Name",
	}, nil, patHdrs)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected PATCH /api/admin/me with PAT to be rejected with 403, got %d", resp.StatusCode)
	}

	// Enforce TOTPRequiredScope = "admin" on PAT when admin has NOT enrolled TOTP!
	sec := env.settings.Snapshot().Security
	sec.TOTPRequiredScope = "admin"
	resp, secRaw := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/security", sec, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH settings/security = %d %s", resp.StatusCode, string(secRaw))
	}
	resp, totpPolRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/articles", nil, nil, patHdrs)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(totpPolRaw), "requires_totp_init") {
		t.Fatalf("expected PAT under TOTPRequiredScope=admin without TOTP to return 403 requires_totp_init, got %d: %s", resp.StatusCode, string(totpPolRaw))
	}
	// Reset TOTPRequiredScope via DB + Reload (since interactive admin session without TOTP is also gated now!)
	_, _ = env.store.ExecContext(env.ctx, `DELETE FROM settings WHERE domain = ?`, domain.SettingsDomainSecurity)
	_, _ = env.settings.Reload(env.ctx)

	// 4. Staff reply to a PENDING comment MUST auto-approve the parent comment!
	// Create a public article for comments
	resp, pubArtRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"title":         "Discussion Post",
		"slug":          "discussion-post",
		"content_md":    "Let's discuss",
		"status":        "published",
		"allow_comment": true,
	}, adminCookies, adminHdrs)
	var pubArt struct {
		Article domain.RawArticle `json:"article"`
	}
	_ = json.Unmarshal(pubArtRaw, &pubArt)

	pendID := id.Next()
	now := time.Now().UnixMilli()
	_, err := env.store.ExecContext(env.ctx,
		`INSERT INTO comments (
			id, target_type, target_id, parent_id, reply_to_name, user_id, guest_id_hash, env_fp_hash,
			author_name, author_url, avatar_url, notify_email_ciphertext, notify_locale,
			client_ip, ip_masked, geo_region, content_md, status, is_deleted, created_at, updated_at
		) VALUES (?, 'article', ?, '', '', '', 'gtok', '', 'Guest Reader', '', '', '', 'en', '2.2.2.2', '2.2.*.*', '', 'Waiting moderation', 'pending', 0, ?, ?)`,
		pendID, pubArt.Article.ID, now, now)
	if err != nil {
		t.Fatal(err)
	}

	resp, replyRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/comments/"+pendID+"/reply", map[string]any{
		"content_md": "Thanks for your comment!",
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("staff reply to pending comment = %d %s", resp.StatusCode, string(replyRaw))
	}
	var parentStatus string
	_ = env.store.QueryRowContext(env.ctx, `SELECT status FROM comments WHERE id = ?`, pendID).Scan(&parentStatus)
	if parentStatus != domain.CommentStatusApproved {
		t.Fatalf("expected pending parent comment to auto-approve upon staff reply, got status=%q", parentStatus)
	}

	// Verify public paginated GET /api/v1/comments & focus_comment jump
	resp, pubCmtsRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/comments?target_type=article&target_id="+pubArt.Article.ID+"&per_page=10&focus_comment="+pendID, nil, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/comments = %d %s", resp.StatusCode, string(pubCmtsRaw))
	}
	var pubCmts struct {
		TotalRoots    int `json:"total_roots"`
		TotalComments int `json:"total_comments"`
		Page          int `json:"page"`
		TotalPages    int `json:"total_pages"`
	}
	_ = json.Unmarshal(pubCmtsRaw, &pubCmts)
	if pubCmts.TotalRoots != 1 || pubCmts.TotalComments != 2 || pubCmts.Page != 1 {
		t.Fatalf("unexpected public comments pagination metadata: %+v", pubCmts)
	}

	// 5. Paginated admin endpoints & non-invalidating background audit log check
	revBefore := env.store.DataRevision()
	resp, clearRaw := doJSONReq(t, http.MethodDelete, env.ts.URL+"/api/admin/runtime-logs", nil, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /api/admin/runtime-logs = %d %s", resp.StatusCode, string(clearRaw))
	}
	revAfter := env.store.DataRevision()
	if revAfter != revBefore {
		t.Fatalf("recordAuditLog bumped Store.DataRevision() from %d to %d (expected ExecBgContext with zero revision bump!)", revBefore, revAfter)
	}

	resp, auditRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/audit-logs?page=1&per_page=5", nil, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/admin/audit-logs = %d %s", resp.StatusCode, string(auditRaw))
	}
	var auditRes struct {
		Total   int      `json:"total"`
		Page    int      `json:"page"`
		PerPage int      `json:"per_page"`
		Actions []string `json:"actions"`
	}
	_ = json.Unmarshal(auditRaw, &auditRes)
	if auditRes.Total < 1 || auditRes.PerPage != 5 || len(auditRes.Actions) == 0 {
		t.Fatalf("unexpected paginated audit-logs response: %+v", auditRes)
	}
}

func TestAudit_GuestClaimRoleIsolationAndLocalizedPublicErrors(t *testing.T) {
	env, adminCookies, adminHdrs, _ := bootstrapAuditEnv(t)

	resp, artRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"title":         "Guest Claim Isolation Post",
		"slug":          "guest-claim-isolation",
		"content_md":    "Testing guest comment claim isolation.",
		"status":        "published",
		"allow_comment": true,
	}, adminCookies, adminHdrs)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/admin/articles = %d %s", resp.StatusCode, string(artRaw))
	}
	var createdArt struct {
		Article domain.RawArticle `json:"article"`
	}
	_ = json.Unmarshal(artRaw, &createdArt)

	// 1. Admin requesting /api/v1/guest/passport must NEVER mint a guest passport
	resp, gpRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/guest/passport", nil, adminCookies, nil)
	if resp.StatusCode != http.StatusOK || strings.Contains(string(gpRaw), `"passport_token":"v1.`) {
		t.Fatalf("expected empty guest passport for staff admin, got %d %s", resp.StatusCode, string(gpRaw))
	}

	// 2. Admin loading GET /api/v1/comments must see claimable_guest_count=0 and clear_guest_passport=true
	resp, cmtListRaw := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/v1/comments?target_type=article&target_id="+createdArt.Article.ID, nil, adminCookies, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/comments = %d %s", resp.StatusCode, string(cmtListRaw))
	}
	var cmtList struct {
		ClaimableGuestCount int  `json:"claimable_guest_count"`
		ClearGuestPassport  bool `json:"clear_guest_passport"`
	}
	_ = json.Unmarshal(cmtListRaw, &cmtList)
	if cmtList.ClaimableGuestCount != 0 || !cmtList.ClearGuestPassport {
		t.Fatalf("expected admin claimable_guest_count=0 and clear_guest_passport=true, got %+v", cmtList)
	}

	// 3. Admin calling POST /api/v1/reader/claim-guest-comments must get 403 localized zh-CN error (not raw English)
	resp, claimAdmRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/v1/reader/claim-guest-comments", nil, adminCookies, map[string]string{
		"Accept-Language":   "zh-CN",
		auth.CSRFHeaderName: adminHdrs[auth.CSRFHeaderName],
	})
	if resp.StatusCode != http.StatusForbidden || strings.Contains(string(claimAdmRaw), "no verified guest passport") || !strings.Contains(string(claimAdmRaw), "无权") {
		t.Fatalf("expected 403 localized zh-CN forbidden for admin claim, got %d %s", resp.StatusCode, string(claimAdmRaw))
	}

	// 4. Invalid comment length under zh-CN returns localized Chinese message (not raw English)
	resp, badCmtRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/v1/comments", map[string]any{
		"target_type": "article",
		"target_id":   createdArt.Article.ID,
		"content_md":  "",
	}, adminCookies, map[string]string{
		"Accept-Language":   "zh-CN",
		auth.CSRFHeaderName: adminHdrs[auth.CSRFHeaderName],
	})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(badCmtRaw), "评论内容长度") {
		t.Fatalf("expected localized zh-CN comment length error, got %d %s", resp.StatusCode, string(badCmtRaw))
	}
}

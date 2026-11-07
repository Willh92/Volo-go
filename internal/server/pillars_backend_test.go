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

	"github.com/Willh92/Volo-go/internal/auth"
	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/server"
	"github.com/Willh92/Volo-go/pkg/id"
)

func postMultipartLegacyImport(
	t *testing.T,
	urlStr string,
	zipBytes []byte,
	fields map[string]string,
	cookies []*http.Cookie,
	headers map[string]string,
) (*http.Response, []byte) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	fw, err := mw.CreateFormFile("file", "legacy-backup.zip")
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

func buildSetupLegacyZipWithMedia(t *testing.T) []byte {
	t.Helper()
	legacyDoc := map[string]any{
		"options": []map[string]any{
			{"oId": "blogTitle", "optionValue": "Legacy Origin Blog"},
			{"oId": "storageType", "optionValue": "s3"},
		},
		"users": []map[string]any{
			{
				"oId":              "default",
				"userName":         "oldadmin",
				"userEmail":        "oldadmin@example.com",
				"userRole":         "adminRole",
				"userPasswordHash": "pbkdf2$sha256$120000$0011223344556677$0123456789abcdef0123456789abcdef",
			},
		},
		"articles": []map[string]any{
			{
				"oId":              "art_swap_1",
				"articleTitle":     "Swapped Database Post",
				"articleAbstract":  "Hero ![hero](https://old.blog.example/image/hero.png)",
				"articleContent":   "Cross link to [Part 2](https://old.blog.example/articles/2024/01/02/part2.html) and ![hero](https://old.blog.example/image/hero.png)",
				"articleAuthorId":  "default",
				"articlePermalink": "/articles/2024/01/01/part1.html",
				"articleStatus":    0,
				"articleCreated":   1704067200000,
				"articleUpdated":   1704067200000,
			},
			{
				"oId":              "art_swap_2",
				"articleTitle":     "Swapped Database Part Two",
				"articleAbstract":  "Part 2",
				"articleContent":   "Body of part two.",
				"articleAuthorId":  "default",
				"articlePermalink": "/articles/2024/01/02/part2.html",
				"articleStatus":    0,
				"articleCreated":   1704153600000,
				"articleUpdated":   1704153600000,
			},
		},
	}
	docBytes, err := json.Marshal(legacyDoc)
	if err != nil {
		t.Fatal(err)
	}
	pngBytes := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x00, 0x00, 0x00, 0x0d}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w1, _ := zw.Create("data.json")
	_, _ = w1.Write(docBytes)
	w2, _ := zw.Create("image/hero.png")
	_, _ = w2.Write(pngBytes)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSetupLegacyImportWithDatabasePoolSwapAndOverrides(t *testing.T) {
	env := setupM5TestEnv(t, "/console")
	zipBytes := buildSetupLegacyZipWithMedia(t)

	stResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/setup/status", nil, nil, nil)
	var setupCSRF string
	for _, c := range stResp.Cookies() {
		if strings.Contains(c.Name, "csrf") {
			setupCSRF = c.Value
		}
	}

	// 1. Cross-Origin CSRF guard on /api/setup/* must block foreign Origin
	evilResp, _ := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/setup/complete", map[string]any{
		"admin_username": "evil",
	}, stResp.Cookies(), map[string]string{
		"X-CSRF-Token": setupCSRF,
		"Origin":       "https://evil.attacker.example",
	})
	if evilResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 on foreign Origin /api/setup/complete, got %d", evilResp.StatusCode)
	}

	// 2. Dry-Run with invalid SQLite path traversal must fail immediately during DB preflight
	badDryResp, _ := postMultipartLegacyImport(t, env.ts.URL+"/api/setup/import-legacy?dry_run=1", zipBytes, map[string]string{
		"driver": "sqlite",
		"dsn":    filepath.Join(env.cfg.DataDir, "..", "outside.db"),
	}, stResp.Cookies(), map[string]string{"X-CSRF-Token": setupCSRF})
	if badDryResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 on outside-dataDir SQLite DSN during dry-run, got %d", badDryResp.StatusCode)
	}

	// 3. Dry-Run with valid new SQLite target succeeds without mutating boot DB
	swappedDBPath := filepath.Join(env.cfg.DataDir, "swapped-live.db")
	dryResp, dryBody := postMultipartLegacyImport(t, env.ts.URL+"/api/setup/import-legacy?dry_run=1", zipBytes, map[string]string{
		"driver":       "sqlite",
		"dsn":          swappedDBPath,
		"old_site_url": "https://old.blog.example",
	}, stResp.Cookies(), map[string]string{"X-CSRF-Token": setupCSRF})
	if dryResp.StatusCode != http.StatusOK || !bytes.Contains(dryBody, []byte(`"dry_run":true`)) {
		t.Fatalf("dry-run import failed: status=%d body=%s", dryResp.StatusCode, string(dryBody))
	}
	bootArts, _ := sqlx.Select[domain.RawArticle](env.ctx, env.store, `SELECT * FROM articles`)
	if len(bootArts) != 0 {
		t.Fatalf("dry-run should not write articles, got %d", len(bootArts))
	}

	// 4. Live Setup Legacy Import with atomic DB swap + URL rewrite + storage/site/admin overrides!
	liveResp, liveBody := postMultipartLegacyImport(t, env.ts.URL+"/api/setup/import-legacy", zipBytes, map[string]string{
		"driver":             "sqlite",
		"dsn":                swappedDBPath,
		"old_site_url":       "https://old.blog.example",
		"new_site_url":       "https://studio.example.com",
		"site_title":         "Swapped Live Studio",
		"site_subtitle":      "Atomic DB Swap",
		"default_locale":     "en-US",
		"storage_mode":       "local",
		"admin_username":     "swapadmin",
		"admin_email":        "swapadmin@example.com",
		"admin_display_name": "Swap Owner",
		"admin_password":     "SwapPass99!",
	}, stResp.Cookies(), map[string]string{"X-CSRF-Token": setupCSRF})
	if liveResp.StatusCode != http.StatusOK {
		t.Fatalf("live setup import-legacy failed: status=%d body=%s", liveResp.StatusCode, string(liveBody))
	}

	// Verify wizard-db.json persisted new DB selection & Store pointer swapped atomically!
	savedSel, err := config.LoadWizardDBConfig(env.cfg.DataDir, env.cfg.KeyRing)
	if err != nil || savedSel == nil || savedSel.DSN != swappedDBPath {
		t.Fatalf("expected wizard-db.json persisted for %s, got err=%v sel=%+v", swappedDBPath, err, savedSel)
	}
	if env.cfg.DBDSN != swappedDBPath {
		t.Fatalf("expected runtime cfg.DBDSN=%q, got %q", swappedDBPath, env.cfg.DBDSN)
	}

	// Querying the original env.store pointer must now read from swapped-live.db!
	arts, err := sqlx.Select[domain.RawArticle](env.ctx, env.store, `SELECT * FROM articles ORDER BY created_at ASC`)
	if err != nil || len(arts) != 2 {
		t.Fatalf("expected 2 articles in swapped DB, got %d (err=%v)", len(arts), err)
	}
	if !strings.Contains(arts[0].ContentMD, "/posts/"+arts[1].Slug) || !strings.Contains(arts[0].ContentMD, "/uploads/hero.png") {
		t.Fatalf("expected rewritten article permalink and media path in swapped DB, got: %s", arts[0].ContentMD)
	}
	snap := env.settings.Snapshot()
	if snap.Site.Title != "Swapped Live Studio" || snap.Storage.ActiveDriver != "local" {
		t.Fatalf("unexpected settings after swapped import: site=%+v storage=%+v", snap.Site, snap.Storage)
	}

	// Verify new admin credentials work via POST /api/admin/auth/login against swapped DB
	loginResp, loginBody := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/auth/login", map[string]any{
		"ident":    "swapadmin",
		"password": "SwapPass99!",
	}, nil, nil)
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("expected overridden admin login to succeed on swapped DB, got %d: %s", loginResp.StatusCode, string(loginBody))
	}

	// Verify static /uploads/* sandbox CSP: safe inline image (.png) vs non-safe HTML (.html -> attachment)
	imgResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/uploads/hero.png", nil, nil, nil)
	if imgResp.StatusCode != http.StatusOK || !strings.Contains(imgResp.Header.Get("Content-Security-Policy"), "sandbox") || imgResp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("expected sandbox CSP + nosniff on /uploads/hero.png, got status=%d hdr=%v", imgResp.StatusCode, imgResp.Header)
	}
	if strings.Contains(imgResp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("safe inline .png should not force attachment disposition: %q", imgResp.Header.Get("Content-Disposition"))
	}

	if err := os.WriteFile(filepath.Join(env.cfg.UploadDir, "stored-xss.html"), []byte("<script>alert(1)</script>"), 0o640); err != nil {
		t.Fatal(err)
	}
	htmlResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/uploads/stored-xss.html", nil, nil, nil)
	if htmlResp.StatusCode != http.StatusOK || !strings.Contains(htmlResp.Header.Get("Content-Disposition"), "attachment") || !strings.Contains(htmlResp.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("expected attachment + sandbox CSP on /uploads/stored-xss.html, got status=%d disp=%q csp=%q",
			htmlResp.StatusCode, htmlResp.Header.Get("Content-Disposition"), htmlResp.Header.Get("Content-Security-Policy"))
	}

	// 5. Verify fresh POST /api/setup/complete also swaps DB cleanly when custom DSN is provided
	env2 := setupM5TestEnv(t, "/console")
	st2, _ := doJSONReq(t, http.MethodGet, env2.ts.URL+"/api/setup/status", nil, nil, nil)
	var csrf2 string
	for _, c := range st2.Cookies() {
		if strings.Contains(c.Name, "csrf") {
			csrf2 = c.Value
		}
	}
	freshCustomDB := filepath.Join(env2.cfg.DataDir, "fresh-complete.db")
	compResp, compBody := doJSONReq(t, http.MethodPost, env2.ts.URL+"/api/setup/complete", map[string]any{
		"driver":             "sqlite",
		"dsn":                freshCustomDB,
		"admin_username":     "freshadmin",
		"admin_email":        "fresh@example.com",
		"admin_password":     "FreshPass99!",
		"admin_display_name": "Fresh Admin",
		"site_title":         "Fresh DB Blog",
		"default_locale":     "zh-CN",
	}, st2.Cookies(), map[string]string{"X-CSRF-Token": csrf2})
	if compResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/setup/complete with custom DB failed: %d %s", compResp.StatusCode, string(compBody))
	}
	if env2.cfg.DBDSN != freshCustomDB {
		t.Fatalf("expected env2.cfg.DBDSN=%q, got %q", freshCustomDB, env2.cfg.DBDSN)
	}
}

func TestPillarsSecurityHeadersAuthHardeningPublic404sBookReaderAndAdminBatchEndpoints(t *testing.T) {
	// 1. Numeric SemVer comparison
	if server.CompareSemVer("v1.10.0", "v1.9.0") != 1 {
		t.Fatal("expected v1.10.0 > v1.9.0")
	}
	if server.CompareSemVer("1.2.0-rc1", "1.2.0") != -1 {
		t.Fatal("expected 1.2.0-rc1 < 1.2.0")
	}
	if server.CompareSemVer("v2.0.0", "2.0.0") != 0 {
		t.Fatal("expected v2.0.0 == 2.0.0")
	}

	env := setupM5TestEnv(t, "/console")
	stResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/setup/status", nil, nil, nil)
	var setupCSRF string
	for _, c := range stResp.Cookies() {
		if strings.Contains(c.Name, "csrf") {
			setupCSRF = c.Value
		}
	}
	compResp, compBody := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/setup/complete", map[string]any{
		"admin_username":     "pillaradmin",
		"admin_email":        "pillar@example.com",
		"admin_password":     "InitialPass99!",
		"admin_display_name": "Pillar Admin",
		"site_title":         "Pillar Studio",
		"default_locale":     "zh-CN",
	}, stResp.Cookies(), map[string]string{"X-CSRF-Token": setupCSRF})
	if compResp.StatusCode != http.StatusOK {
		t.Fatalf("setup complete: %d %s", compResp.StatusCode, string(compBody))
	}
	adminCookies := compResp.Cookies()
	var setupDone struct {
		CSRFToken string `json:"csrf_token"`
	}
	_ = json.Unmarshal(compBody, &setupDone)
	csrfHdr := map[string]string{auth.CSRFHeaderName: setupDone.CSRFToken}

	// 2. Global Security Headers & Admin SPA CSP
	homeResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/", nil, nil, nil)
	if homeResp.Header.Get("Permissions-Policy") == "" || homeResp.Header.Get("Strict-Transport-Security") == "" {
		t.Fatalf("missing Permissions-Policy or HSTS headers: %v", homeResp.Header)
	}
	spaResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/console", nil, nil, nil)
	if !strings.Contains(spaResp.Header.Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatalf("expected Admin SPA CSP on /console, got %q", spaResp.Header.Get("Content-Security-Policy"))
	}

	// 3. CSRF / Method defenses on /logout, /api/admin/auth/logout, and /api/v1/*
	imgLogoutResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/logout", nil, adminCookies, map[string]string{
		"Sec-Fetch-Dest": "image",
	})
	if imgLogoutResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 on <img src=/logout> subresource request, got %d", imgLogoutResp.StatusCode)
	}
	getAdminLogout, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/auth/logout", nil, adminCookies, nil)
	if getAdminLogout.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 on GET /api/admin/auth/logout, got %d", getAdminLogout.StatusCode)
	}
	crossOriginV1, _ := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/v1/comments", map[string]any{
		"content_md": "hi",
	}, nil, map[string]string{"Origin": "https://evil.example"})
	if crossOriginV1.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 on cross-origin POST /api/v1/comments, got %d", crossOriginV1.StatusCode)
	}

	// 4. Public 404s on nonexistent taxonomy/archive/github routes
	for _, route := range []string{"/categories/does-not-exist", "/tags/does-not-exist", "/archives/2099-12", "/github"} {
		r404, _ := doJSONReq(t, http.MethodGet, env.ts.URL+route, nil, nil, nil)
		if r404.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404 on %s, got %d", route, r404.StatusCode)
		}
	}

	// 5. Create Article + Book with local /posts/{slug} chapter -> verify inline ActiveChapterHTML!
	artResp, artBody := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles", map[string]any{
		"title":      "Inline Book Chapter Article",
		"slug":       "inline-book-chapter",
		"summary_md": "Chapter summary",
		"content_md": "UNIQUE_INLINE_CHAPTER_SENTINEL_7788",
		"status":     domain.ArticleStatusPublished,
	}, adminCookies, csrfHdr)
	if artResp.StatusCode != http.StatusCreated && artResp.StatusCode != http.StatusOK {
		t.Fatalf("create article failed: %d %s", artResp.StatusCode, string(artBody))
	}
	var createdArt struct {
		Article domain.RawArticle `json:"article"`
	}
	_ = json.Unmarshal(artBody, &createdArt)
	artID := createdArt.Article.ID

	bookID := id.Next()
	chapID := id.Next()
	_, err := env.store.ExecContext(env.ctx,
		`INSERT INTO books (id, title, slug, description, cover_url, source_type, source_url, series_id, embed_mode, chapter_count, synced_at, sort_order, created_at, updated_at)
		 VALUES (?, ?, ?, '', '', 'manual', '', '', 'link', 1, 0, 1, 1700000000000, 1700000000000)`,
		bookID, "Architecture Manual", "arch-manual")
	if err != nil {
		t.Fatalf("insert book: %v", err)
	}
	_, err = env.store.ExecContext(env.ctx,
		`INSERT INTO book_chapters (id, book_id, parent_id, title, url, depth, sort_order)
		 VALUES (?, ?, '', ?, ?, 0, 1)`,
		chapID, bookID, "Chapter One", "/posts/inline-book-chapter")
	if err != nil {
		t.Fatalf("insert chapter: %v", err)
	}

	bookResp, bookHTML := doJSONReq(t, http.MethodGet, env.ts.URL+"/books/arch-manual?ch="+chapID, nil, nil, nil)
	if bookResp.StatusCode != http.StatusOK || !bytes.Contains(bookHTML, []byte("UNIQUE_INLINE_CHAPTER_SENTINEL_7788")) {
		t.Fatalf("expected inline ActiveChapterHTML on /books/arch-manual?ch=%s, status=%d", chapID, bookResp.StatusCode)
	}

	// 6. Admin Batch Endpoints (articles, tags, redirects, links, comments, media) + Comment Edit/Search + Audit Filter + Reloads
	batchArtResp, _ := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles/batch", map[string]any{
		"ids":    []string{artID},
		"action": "draft",
	}, adminCookies, csrfHdr)
	if batchArtResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/articles/batch draft failed: %d", batchArtResp.StatusCode)
	}
	_, _ = doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/articles/batch", map[string]any{
		"ids":    []string{artID},
		"action": "publish",
	}, adminCookies, csrfHdr)

	// Tag batch delete
	tagID := id.Next()
	_, _ = env.store.ExecContext(env.ctx, `INSERT INTO tags (id, name, slug, created_at) VALUES (?, 'TempTag', 'temp-tag', 1700000000000)`, tagID)
	tagBatchResp, _ := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/tags/batch", map[string]any{
		"ids":    []string{tagID},
		"action": "delete",
	}, adminCookies, csrfHdr)
	if tagBatchResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/tags/batch failed: %d", tagBatchResp.StatusCode)
	}

	// Redirect batch delete
	redirID := id.Next()
	_, _ = env.store.ExecContext(env.ctx, `INSERT INTO redirects (id, source_path, target_path, status_code, is_system_legacy, hit_count, created_at) VALUES (?, '/old-x', '/posts/inline-book-chapter', 301, ?, 0, 1700000000000)`, redirID, false)
	redirBatchResp, _ := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/redirects/batch", map[string]any{
		"ids":    []string{redirID},
		"action": "delete",
	}, adminCookies, csrfHdr)
	if redirBatchResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/redirects/batch failed: %d", redirBatchResp.StatusCode)
	}

	// Links batch category & delete
	linkID := id.Next()
	_, err = env.store.ExecContext(env.ctx,
		`INSERT INTO links (id, title, url, description, icon_url, category_name, sort_order, health_status, http_code, latency_ms, health_error, last_checked_at, created_at, updated_at)
		 VALUES (?, 'Peer', 'https://peer.example.com', '', '', 'Friends', 1, 'unknown', 0, 0, '', 0, 1700000000000, 1700000000000)`,
		linkID)
	if err != nil {
		t.Fatalf("insert link: %v", err)
	}
	linkBatchResp, _ := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/links/batch", map[string]any{
		"ids":           []string{linkID},
		"action":        "category",
		"category_name": "Tech",
	}, adminCookies, csrfHdr)
	if linkBatchResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/links/batch category failed: %d", linkBatchResp.StatusCode)
	}
	linkDelResp, _ := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/links/batch", map[string]any{
		"ids":    []string{linkID},
		"action": "delete",
	}, adminCookies, csrfHdr)
	if linkDelResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/links/batch delete failed: %d", linkDelResp.StatusCode)
	}

	// Comments: edit content_md, ?q= search, and batch approve
	cmtID := id.Next()
	_, err = env.store.ExecContext(env.ctx,
		`INSERT INTO comments (id, target_type, target_id, parent_id, reply_to_name, user_id, guest_id_hash, env_fp_hash, author_name, author_url, avatar_url, notify_email_ciphertext, notify_locale, client_ip, ip_masked, geo_region, content_md, status, is_deleted, created_at, updated_at)
		 VALUES (?, 'article', ?, '', '', '', '', '', 'Alice', '', '', '', 'zh-CN', '', '203.0.*.*', '', 'Initial text', 'pending', ?, 1700000000000, 1700000000000)`,
		cmtID, artID, false)
	if err != nil {
		t.Fatalf("insert comment: %v", err)
	}
	patchCmtResp, patchCmtBody := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/comments/"+cmtID, map[string]any{
		"content_md": "Edited COMMENT_SEARCH_TOKEN_99",
	}, adminCookies, csrfHdr)
	if patchCmtResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH /api/admin/comments/{id} content_md failed: %d %s", patchCmtResp.StatusCode, string(patchCmtBody))
	}
	qCmtResp, qCmtBody := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/comments?q=COMMENT_SEARCH_TOKEN_99", nil, adminCookies, nil)
	if qCmtResp.StatusCode != http.StatusOK || !bytes.Contains(qCmtBody, []byte(cmtID)) {
		t.Fatalf("GET /api/admin/comments?q= failed: %d %s", qCmtResp.StatusCode, string(qCmtBody))
	}
	cmtBatchResp, _ := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/comments/batch", map[string]any{
		"ids":    []string{cmtID},
		"action": "approve",
	}, adminCookies, csrfHdr)
	if cmtBatchResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/comments/batch failed: %d", cmtBatchResp.StatusCode)
	}

	// Media reconcile + PATCH alt_text/original_name + batch delete
	if err := os.WriteFile(filepath.Join(env.cfg.UploadDir, "reconcile-sample.png"), []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, 0o640); err != nil {
		t.Fatal(err)
	}
	recResp, recBody := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/media/reconcile", map[string]any{}, adminCookies, csrfHdr)
	if recResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/media/reconcile failed: %d %s", recResp.StatusCode, string(recBody))
	}
	medAsset, err := sqlx.Get[domain.MediaAsset](env.ctx, env.store, `SELECT * FROM media_assets WHERE object_key = ?`, "reconcile-sample.png")
	if err != nil {
		t.Fatalf("expected reconciled media asset in DB: %v", err)
	}
	patchMedResp, patchMedBody := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/media/"+medAsset.ID, map[string]any{
		"original_name": "diagram.png",
		"alt_text":      "System Architecture Diagram",
	}, adminCookies, csrfHdr)
	if patchMedResp.StatusCode != http.StatusOK || !bytes.Contains(patchMedBody, []byte("System Architecture Diagram")) {
		t.Fatalf("PATCH /api/admin/media/{id} failed: %d %s", patchMedResp.StatusCode, string(patchMedBody))
	}
	medBatchResp, _ := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/media/batch", map[string]any{
		"ids":    []string{medAsset.ID},
		"action": "delete",
	}, adminCookies, csrfHdr)
	if medBatchResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/media/batch delete failed: %d", medBatchResp.StatusCode)
	}

	// Themes reload + Audit logs filter + Local Backup snapshot & restore (dry-run and live)
	thReloadResp, _ := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/themes/reload", map[string]any{}, adminCookies, csrfHdr)
	if thReloadResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/themes/reload failed: %d", thReloadResp.StatusCode)
	}

	snapResp, snapBody := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/backup/local", map[string]any{}, adminCookies, csrfHdr)
	if snapResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/admin/backup/local failed: %d %s", snapResp.StatusCode, string(snapBody))
	}
	var snapRes struct {
		File string `json:"file"`
	}
	_ = json.Unmarshal(snapBody, &snapRes)
	if snapRes.File == "" {
		t.Fatalf("expected snapshot filename, got: %s", string(snapBody))
	}

	dryRestResp, dryRestBody := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/backup/local/"+snapRes.File+"/restore?dry_run=1", map[string]any{}, adminCookies, csrfHdr)
	if dryRestResp.StatusCode != http.StatusOK || !bytes.Contains(dryRestBody, []byte(`"dry_run":true`)) {
		t.Fatalf("dry-run local backup restore failed: %d %s", dryRestResp.StatusCode, string(dryRestBody))
	}
	liveRestResp, liveRestBody := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/backup/local/"+snapRes.File+"/restore", map[string]any{}, adminCookies, csrfHdr)
	if liveRestResp.StatusCode != http.StatusOK {
		t.Fatalf("live local backup restore failed: %d %s", liveRestResp.StatusCode, string(liveRestBody))
	}

	auditResp, auditBody := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/audit-logs?action=backup.local_restore&q="+snapRes.File, nil, adminCookies, nil)
	if auditResp.StatusCode != http.StatusOK || !bytes.Contains(auditBody, []byte("backup.local_restore")) {
		t.Fatalf("GET /api/admin/audit-logs filter failed: %d %s", auditResp.StatusCode, string(auditBody))
	}

	// 7. Auth Hardening: TOTP disable requires current_password, Email change requires current_password,
	//    and Password change revokes other active sessions!
	totpDelNoPw, _ := doJSONReq(t, http.MethodDelete, env.ts.URL+"/api/admin/security/totp", map[string]any{}, adminCookies, csrfHdr)
	if totpDelNoPw.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 when disabling TOTP without current_password, got %d", totpDelNoPw.StatusCode)
	}

	emailNoPw, _ := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/me", map[string]any{
		"email": "hijack@example.com",
	}, adminCookies, csrfHdr)
	if emailNoPw.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 when changing email without current_password, got %d", emailNoPw.StatusCode)
	}

	// Create a second admin session, then change password on session #1 -> session #2 must be revoked!
	login2Resp, _ := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/auth/login", map[string]any{
		"ident":    "pillaradmin",
		"password": "InitialPass99!",
	}, nil, nil)
	if login2Resp.StatusCode != http.StatusOK {
		t.Fatalf("second login failed: %d", login2Resp.StatusCode)
	}
	secondCookies := login2Resp.Cookies()

	pwChangeResp, pwChangeBody := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/me", map[string]any{
		"email":            "updated-pillar@example.com",
		"current_password": "InitialPass99!",
		"new_password":     "RotatedPass99!",
	}, adminCookies, csrfHdr)
	if pwChangeResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH /api/admin/me password/email change failed: %d %s", pwChangeResp.StatusCode, string(pwChangeBody))
	}

	// Session #1 stays valid, Session #2 must now return 401!
	me1Resp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/me", nil, adminCookies, nil)
	if me1Resp.StatusCode != http.StatusOK {
		t.Fatalf("expected primary session to stay valid after own password change, got %d", me1Resp.StatusCode)
	}
	me2Resp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/me", nil, secondCookies, nil)
	if me2Resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected concurrent session #2 to be revoked (401) after password change, got %d", me2Resp.StatusCode)
	}
}

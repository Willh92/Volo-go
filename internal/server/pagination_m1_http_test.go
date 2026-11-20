package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestM1PaginationAndQueryEndpointsHTTP(t *testing.T) {
	env := setupM5TestEnv(t, "/console")
	now := time.Now().UnixMilli()

	// Complete setup
	stResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/setup/status", nil, nil, nil)
	var setupCSRF string
	for _, c := range stResp.Cookies() {
		if strings.Contains(c.Name, "csrf") {
			setupCSRF = c.Value
		}
	}
	okSetup, okSetupBody := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/setup/complete", map[string]any{
		"admin_username":     "pagadmin",
		"admin_email":        "pag@example.com",
		"admin_password":     "pass1234",
		"admin_display_name": "Pag Admin",
		"site_title":         "Pag Test",
		"default_locale":     "en-US",
	}, stResp.Cookies(), map[string]string{"X-CSRF-Token": setupCSRF})
	if okSetup.StatusCode != http.StatusOK {
		t.Fatalf("setup complete: %d", okSetup.StatusCode)
	}
	adminCookies := okSetup.Cookies()
	var setupDone struct {
		CSRFToken string `json:"csrf_token"`
	}
	_ = json.Unmarshal(okSetupBody, &setupDone)
	adminCSRF := map[string]string{"X-CSRF-Token": setupDone.CSRFToken}

	// Seed 45 categories (40 matching "cloud", 5 matching "local") + articles across 8 categories for overview
	for i := 0; i < 45; i++ {
		cid := fmt.Sprintf("cat_%02d", i)
		name := fmt.Sprintf("Cloud Category %02d", i)
		if i >= 40 {
			name = fmt.Sprintf("Local Category %02d", i)
		}
		if _, err := env.store.ExecContext(env.ctx,
			`INSERT INTO categories (id, name, slug, description, sort_order, created_at, updated_at) VALUES (?, ?, ?, '', ?, ?, ?)`,
			cid, name, fmt.Sprintf("cat-%02d", i), i, now, now); err != nil {
			t.Fatalf("seed category: %v", err)
		}
		if i < 8 {
			for a := 0; a < 8-i; a++ {
				aid := fmt.Sprintf("art_%02d_%02d", i, a)
				if _, err := env.store.ExecContext(env.ctx, `INSERT INTO articles (
					id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
					cover_url, status, published_at, allow_comment, password_ciphertext,
					audio_url, audio_mime, lang, translation_group, license_override, word_count, reading_minutes, created_at, updated_at
				) VALUES (?, 'u_adm', ?, ?, ?, '', '', 'body', '', 'published', ?, 1, '', '', '', 'en', '', '', 10, 1, ?, ?)`,
					aid, cid, aid, aid, now+int64(a), now, now); err != nil {
					t.Fatalf("seed article: %v", err)
				}
			}
		}
	}

	// 1. GET /categories?q=cloud&page=2 -> renders filtered page 2 and preserves q=cloud in pagination links.
	catHTMLResp, catHTML := doJSONReq(t, http.MethodGet, env.ts.URL+"/categories?q=cloud&page=2", nil, nil, nil)
	if catHTMLResp.StatusCode != http.StatusOK || !bytes.Contains(catHTML, []byte("Cloud Category 36")) || !bytes.Contains(catHTML, []byte("q=cloud")) {
		t.Fatalf("GET /categories?q=cloud&page=2 failed: status=%d hasCat36=%v hasQ=%v",
			catHTMLResp.StatusCode, bytes.Contains(catHTML, []byte("Cloud Category 36")), bytes.Contains(catHTML, []byte("q=cloud")))
	}

	// 2. GET /api/admin/categories?q=local&page=1&per_page=3
	admCatResp, admCatBody := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/categories?q=local&page=1&per_page=3", nil, adminCookies, nil)
	if admCatResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/admin/categories failed: %d", admCatResp.StatusCode)
	}
	var admCatOut struct {
		Items      []any `json:"items"`
		Categories []any `json:"categories"`
		Total      int   `json:"total"`
		Page       int   `json:"page"`
		PerPage    int   `json:"per_page"`
		PageSize   int   `json:"page_size"`
	}
	_ = json.Unmarshal(admCatBody, &admCatOut)
	if admCatOut.Total != 5 || len(admCatOut.Items) != 3 || len(admCatOut.Categories) != 3 || admCatOut.PerPage != 3 || admCatOut.PageSize != 3 {
		t.Fatalf("unexpected /api/admin/categories response: %+v", admCatOut)
	}

	// 3. GET /api/admin/overview -> category_usage bounded <= 50 real categories (NO synthetic "__others__")
	ovResp, ovBody := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/overview", nil, adminCookies, nil)
	if ovResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/admin/overview failed: %d", ovResp.StatusCode)
	}
	var ovOut struct {
		CategoryUsage []struct {
			ID           string `json:"id"`
			ArticleCount int    `json:"article_count"`
		} `json:"category_usage"`
		CategoryUsageOthers int `json:"category_usage_others"`
	}
	_ = json.Unmarshal(ovBody, &ovOut)
	if len(ovOut.CategoryUsage) != 8 || len(ovOut.CategoryUsage) > 50 || ovOut.CategoryUsageOthers != 0 {
		t.Fatalf("expected 8 real positive categories and others=0, got len=%d others=%d usage=%+v", len(ovOut.CategoryUsage), ovOut.CategoryUsageOthers, ovOut.CategoryUsage)
	}
	for _, item := range ovOut.CategoryUsage {
		if item.ID == "__others__" || item.ArticleCount <= 0 {
			t.Fatalf("unexpected synthetic/zero entry in category_usage: %+v", item)
		}
	}

	// 4. GET /links?q=... & multi-page group preservation
	for i := 0; i < 60; i++ {
		cat := "Group One"
		title := fmt.Sprintf("LinkOne %02d", i)
		sort := 10
		if i >= 30 {
			cat = "Group Two"
			title = fmt.Sprintf("LinkTwo %02d", i)
			sort = 20
		}
		if i == 55 {
			title = "UniqueNeedleLink"
		}
		if _, err := env.store.ExecContext(env.ctx,
			`INSERT INTO links (id, title, url, description, icon_url, category_name, sort_order, health_status, http_code, latency_ms, health_error, last_checked_at, created_at, updated_at)
			 VALUES (?, ?, ?, '', '', ?, ?, 'ok', 200, 10, '', 0, ?, ?)`,
			fmt.Sprintf("lk_%02d", i), title, fmt.Sprintf("https://example.org/%d", i), cat, sort, now+int64(i), now); err != nil {
			t.Fatalf("seed link: %v", err)
		}
	}
	linksP2Resp, linksP2HTML := doJSONReq(t, http.MethodGet, env.ts.URL+"/links?page=2", nil, nil, nil)
	if linksP2Resp.StatusCode != http.StatusOK || !bytes.Contains(linksP2HTML, []byte("Group Two")) || bytes.Contains(linksP2HTML, []byte("Group One")) {
		t.Fatalf("GET /links?page=2 expected only Group Two, status=%d", linksP2Resp.StatusCode)
	}
	linksSearchResp, linksSearchHTML := doJSONReq(t, http.MethodGet, env.ts.URL+"/links?q=UniqueNeedleLink", nil, nil, nil)
	if linksSearchResp.StatusCode != http.StatusOK || !bytes.Contains(linksSearchHTML, []byte("UniqueNeedleLink")) {
		t.Fatalf("GET /links?q=UniqueNeedleLink missing target link, status=%d", linksSearchResp.StatusCode)
	}

	// 5. GET /books/{slug}?ch=...&cpage=... with 220 chapters
	if _, err := env.store.ExecContext(env.ctx,
		`INSERT INTO books (id, title, slug, description, cover_url, source_type, source_url, series_id, embed_mode, reading_status, chapter_count, synced_at, sort_order, created_at, updated_at)
		 VALUES ('bk_http', 'HTTP Big Book', 'http-big-book', 'Desc', '', 'external', 'https://docs.example.com', '', 'iframe', 'reading', 0, ?, 1, ?, ?)`,
		now, now, now); err != nil {
		t.Fatalf("seed book: %v", err)
	}
	for i := 0; i < 220; i++ {
		if _, err := env.store.ExecContext(env.ctx,
			`INSERT INTO book_chapters (id, book_id, parent_id, title, url, depth, sort_order) VALUES (?, 'bk_http', '', ?, ?, 0, ?)`,
			fmt.Sprintf("bch_%03d", i), fmt.Sprintf("Chapter Title %03d", i), fmt.Sprintf("https://docs.example.com/%03d", i), i); err != nil {
			t.Fatalf("seed chapter: %v", err)
		}
	}
	bkResp, bkHTML := doJSONReq(t, http.MethodGet, env.ts.URL+"/books/http-big-book?ch=bch_105", nil, nil, nil)
	if bkResp.StatusCode != http.StatusOK ||
		!bytes.Contains(bkHTML, []byte("Chapter Title 105")) ||
		bytes.Contains(bkHTML, []byte("Chapter Title 001")) ||
		!bytes.Contains(bkHTML, []byte("Chapter Title 104")) ||
		!bytes.Contains(bkHTML, []byte("Chapter Title 106")) {
		t.Fatalf("GET /books/http-big-book?ch=bch_105 unexpected TOC window: status=%d", bkResp.StatusCode)
	}

	// 5b. Nested-volume book: PrevChapter skips empty volume header (nch_142 -> nch_140) and cpage=2 preserves SubVolume 1.2
	if _, err := env.store.ExecContext(env.ctx,
		`INSERT INTO books (id, title, slug, description, cover_url, source_type, source_url, series_id, embed_mode, reading_status, chapter_count, synced_at, sort_order, created_at, updated_at)
		 VALUES ('bk_nested_http', 'Nested HTTP Book', 'nested-http-book', 'Desc', '', 'external', 'https://docs.example.com', '', 'iframe', 'reading', 0, ?, 2, ?, ?)`,
		now, now, now); err != nil {
		t.Fatalf("seed nested book: %v", err)
	}
	for i := 0; i < 260; i++ {
		chID := fmt.Sprintf("nch_%03d", i)
		title := fmt.Sprintf("Chap %03d", i)
		urlVal := fmt.Sprintf("https://docs.example.com/nch-%03d", i)
		depth := 2
		parent := "nch_001"
		switch {
		case i == 0:
			title, urlVal, depth, parent = "Volume 1", "", 0, ""
		case i == 1:
			title, urlVal, depth, parent = "SubVolume 1.1", "", 1, "nch_000"
		case i == 61:
			title, urlVal, depth, parent = "SubVolume 1.2", "", 1, "nch_000"
		case i > 61 && i <= 140:
			parent = "nch_061"
		case i == 141:
			title, urlVal, depth, parent = "Volume 2", "", 0, ""
		case i >= 142:
			depth, parent = 1, "nch_141"
		}
		if _, err := env.store.ExecContext(env.ctx,
			`INSERT INTO book_chapters (id, book_id, parent_id, title, url, depth, sort_order) VALUES (?, 'bk_nested_http', ?, ?, ?, ?, ?)`,
			chID, parent, title, urlVal, depth, i); err != nil {
			t.Fatalf("seed nested chapter %d: %v", i, err)
		}
	}
	nested142Resp, nested142HTML := doJSONReq(t, http.MethodGet, env.ts.URL+"/books/nested-http-book?ch=nch_142", nil, nil, nil)
	if nested142Resp.StatusCode != http.StatusOK ||
		!bytes.Contains(nested142HTML, []byte("ch=nch_140")) ||
		bytes.Contains(nested142HTML, []byte(`href="/books/nested-http-book?ch=nch_141"`)) {
		t.Fatalf("expected nch_142 PrevChapter -> nch_140 (not empty-URL nch_141), status=%d", nested142Resp.StatusCode)
	}
	nestedP2Resp, nestedP2HTML := doJSONReq(t, http.MethodGet, env.ts.URL+"/books/nested-http-book?cpage=2", nil, nil, nil)
	if nestedP2Resp.StatusCode != http.StatusOK ||
		!bytes.Contains(nestedP2HTML, []byte("Volume 1")) ||
		!bytes.Contains(nestedP2HTML, []byte("SubVolume 1.2")) ||
		!bytes.Contains(nestedP2HTML, []byte("Chap 100")) {
		t.Fatalf("expected cpage=2 to preserve Volume 1 and SubVolume 1.2 before Chap 100, status=%d", nestedP2Resp.StatusCode)
	}

	// 6. GET /api/admin/nav pagination + backward compatibility + int64 overflow guard
	for i := 0; i < 5; i++ {
		r, b := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/nav", map[string]any{
			"title":      fmt.Sprintf("NavItem %02d", i),
			"url":        fmt.Sprintf("/nav-%02d", i),
			"sort_order": i + 1,
		}, adminCookies, adminCSRF)
		if r.StatusCode != http.StatusCreated && r.StatusCode != http.StatusOK {
			t.Fatalf("create nav %d failed: %d %s", i, r.StatusCode, string(b))
		}
	}
	navDefResp, navDefBody := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/nav", nil, adminCookies, nil)
	if navDefResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/admin/nav failed: %d", navDefResp.StatusCode)
	}
	var navOut struct {
		Items    []any `json:"items"`
		NavItems []any `json:"nav_items"`
		Total    int   `json:"total"`
		Page     int   `json:"page"`
		PerPage  int   `json:"per_page"`
		PageSize int   `json:"page_size"`
	}
	_ = json.Unmarshal(navDefBody, &navOut)
	if navOut.Total < 5 || len(navOut.Items) != navOut.Total || len(navOut.NavItems) != navOut.Total || navOut.Page != 1 || navOut.PerPage != 100 {
		t.Fatalf("unexpected default /api/admin/nav: %+v", navOut)
	}

	navPgResp, navPgBody := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/nav?page=2&per_page=2", nil, adminCookies, nil)
	if navPgResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/admin/nav paginated failed: %d", navPgResp.StatusCode)
	}
	var navPgOut struct {
		Items    []any `json:"items"`
		NavItems []any `json:"nav_items"`
		Total    int   `json:"total"`
		Page     int   `json:"page"`
		PerPage  int   `json:"per_page"`
		PageSize int   `json:"page_size"`
	}
	_ = json.Unmarshal(navPgBody, &navPgOut)
	if len(navPgOut.Items) != 2 || len(navPgOut.NavItems) != 2 || navPgOut.Page != 2 || navPgOut.PerPage != 2 || navPgOut.PageSize != 2 || navPgOut.Total < 5 {
		t.Fatalf("unexpected paginated /api/admin/nav: %+v", navPgOut)
	}

	navOverResp, navOverBody := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/nav?page=9223372036854775807&per_page=100", nil, adminCookies, nil)
	if navOverResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/admin/nav overflow page failed: %d", navOverResp.StatusCode)
	}
	var navOverOut struct {
		Items []any `json:"items"`
		Total int   `json:"total"`
	}
	_ = json.Unmarshal(navOverBody, &navOverOut)
	if len(navOverOut.Items) != 0 || navOverOut.Total < 5 {
		t.Fatalf("expected empty items on overflow page=9223372036854775807, got %+v", navOverOut)
	}

	sessOverResp, sessOverBody := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/security/sessions?page=9223372036854775807&per_page=20", nil, adminCookies, nil)
	if sessOverResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/admin/security/sessions overflow page failed: %d (%s)", sessOverResp.StatusCode, string(sessOverBody))
	}
	var sessOverOut struct {
		Items []any `json:"items"`
	}
	_ = json.Unmarshal(sessOverBody, &sessOverOut)
	if len(sessOverOut.Items) != 0 {
		t.Fatalf("expected empty sessions on overflow page, got %+v", sessOverOut)
	}
}

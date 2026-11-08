package etl

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/repo"
)

func TestImportLegacyBoloSolo15TableArchiveStreamingAndZeroDeadLinks(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(tmp, "migrated.db"),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer store.Close()
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	kr, _, err := config.LoadOrInitKeyRing(strings.Repeat("a", 64), tmp)
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	settingsMgr := repo.NewSettingsManager(store, kr, "zh-CN")

	legacyZip := filepath.Join(tmp, "bolo-solo-backup.zip")
	buildComprehensiveLegacy15TableZip(t, legacyZip)

	// 1. Verify DryRun parses without writing rows
	dryReport, err := ImportLegacyArchive(ctx, store, settingsMgr, kr, legacyZip, ImportOptions{DryRun: true})
	if err != nil {
		t.Fatalf("DryRun ImportLegacyArchive failed: %v", err)
	}
	if !dryReport.DryRun || dryReport.Articles != 2 || dryReport.Series != 1 || dryReport.Books != 1 {
		t.Fatalf("unexpected dry-run report: %+v", dryReport)
	}
	artCheck, _ := sqlx.Select[domain.RawArticle](ctx, store, `SELECT * FROM articles`)
	if len(artCheck) != 0 {
		t.Fatalf("DryRun wrote %d articles", len(artCheck))
	}

	// 2. Measure heap delta and run full streaming import
	runtime.GC()
	var mBefore, mAfter runtime.MemStats
	runtime.ReadMemStats(&mBefore)

	report, err := ImportLegacyArchive(ctx, store, settingsMgr, kr, legacyZip, ImportOptions{})
	if err != nil {
		t.Fatalf("ImportLegacyArchive failed: %v", err)
	}

	runtime.GC()
	runtime.ReadMemStats(&mAfter)
	var heapDelta int64
	if mAfter.HeapInuse > mBefore.HeapInuse {
		heapDelta = int64(mAfter.HeapInuse - mBefore.HeapInuse)
	}
	if heapDelta > 20*1024*1024 {
		t.Fatalf("heap delta %d bytes exceeded 20 MiB streaming budget", heapDelta)
	}

	// Assert entity counts
	if report.Users != 2 || report.Categories != 2 || report.Tags != 3 || report.Articles != 2 ||
		report.Revisions != 1 || report.NavItems != 1 || report.Comments != 2 || report.Links != 1 ||
		report.Follows != 1 || report.Series != 1 || report.Books != 1 || report.BookChapters != 2 ||
		report.AnalyticsDays != 2 || report.Webhooks != 1 {
		t.Fatalf("unexpected import report counts: %+v", report)
	}
	if len(report.CategoryConflicts) != 1 {
		t.Fatalf("expected 1 multi-category conflict recorded, got: %+v", report.CategoryConflicts)
	}

	// Assert "default" user ID remapped cleanly & retired external avatar scrubbed
	adminUser, err := sqlx.Get[domain.User](ctx, store, `SELECT * FROM users WHERE id = ?`, defaultMappedAdminID)
	if err != nil {
		t.Fatalf("expected remapped default admin user %s: %v", defaultMappedAdminID, err)
	}
	if adminUser.AvatarURL != "" {
		t.Fatalf("expected retired external CDN avatar to be scrubbed, got %q", adminUser.AvatarURL)
	}
	if !strings.HasPrefix(adminUser.LegacyHash, "pbkdf2$sha256$") {
		t.Fatalf("expected legacy pbkdf2$sha256$ hash preserved for silent upgrade, got %q", adminUser.LegacyHash)
	}

	// Assert direct category derivation picked lower categoryOrder (cat_arch order=1 over cat_go order=2)
	art1, err := sqlx.Get[domain.RawArticle](ctx, store, `SELECT * FROM articles WHERE id = ?`, "art_101")
	if err != nil {
		t.Fatalf("load art_101: %v", err)
	}
	if art1.AuthorID != defaultMappedAdminID {
		t.Fatalf("expected art_101 author remapped to %s, got %s", defaultMappedAdminID, art1.AuthorID)
	}
	if art1.CategoryID != "cat_arch" {
		t.Fatalf("expected art_101 primary category 'cat_arch', got %q", art1.CategoryID)
	}
	if art1.LegacyPermalink != "/articles/2023/05/18/1684400000000.html" {
		t.Fatalf("expected legacy .html path preserved, got %q", art1.LegacyPermalink)
	}
	// Verify all free-form tags were still preserved on art_101!
	art1Tags, err := sqlx.Select[domain.ArticleTag](ctx, store, `SELECT * FROM article_tags WHERE article_id = ?`, "art_101")
	if err != nil || len(art1Tags) != 2 {
		t.Fatalf("expected both tags preserved on art_101 despite multi-category conflict, got %d (%v)", len(art1Tags), err)
	}

	// Assert 301 redirect rules created for legacy .html permalinks (zero 404 dead links)
	redir, err := sqlx.Get[domain.RedirectRule](ctx, store, `SELECT * FROM redirects WHERE source_path = ?`, "/articles/2023/05/18/1684400000000.html")
	if err != nil {
		t.Fatalf("expected 301 redirect for legacy .html permalink: %v", err)
	}
	if redir.StatusCode != 301 || redir.TargetPath != "/posts/"+art1.Slug {
		t.Fatalf("unexpected redirect target: %+v", redir)
	}

	// Assert password-protected article migrated with AES-GCM encrypted password & lock block detected on art_102
	art2, err := sqlx.Get[domain.RawArticle](ctx, store, `SELECT * FROM articles WHERE id = ?`, "art_102")
	if err != nil {
		t.Fatalf("load art_102: %v", err)
	}
	if art2.PasswordCiphertext == "" || strings.Contains(art2.PasswordCiphertext, "s3cr3t-pass") || !art2.HasContentLock {
		t.Fatalf("expected encrypted password_ciphertext + has_content_lock on art_102, got %+v", art2)
	}
	if openedPass, err := kr.OpenString(art2.PasswordCiphertext, config.ArticlePasswordAAD("art_102")); err != nil || openedPass != "s3cr3t-pass" {
		t.Fatalf("failed to decrypt art_102 password ciphertext: %v / %q", err, openedPass)
	}

	// Assert out-of-order chunked voloSeries (.0, .1, .10, .2, ..., .11) reassembled without corruption!
	ser, err := sqlx.Get[domain.Series](ctx, store, `SELECT * FROM series WHERE id = ?`, "ser_go_deep")
	if err != nil {
		t.Fatalf("load reassembled chunked series: %v", err)
	}
	if !strings.Contains(ser.Description, "CHUNK_ORDER_SENTINEL_0123456789_10_11") {
		t.Fatalf("out-of-order chunk reassembly corrupted series description: %q", ser.Description)
	}
	serArts, _ := sqlx.Select[domain.SeriesArticle](ctx, store, `SELECT * FROM series_articles WHERE series_id = ?`, "ser_go_deep")
	if len(serArts) != 2 {
		t.Fatalf("expected 2 series_articles rows, got %d", len(serArts))
	}

	// Assert books & hierarchical book_chapters normalized from voloBooks + voloBookToc.<id>
	chapters, err := sqlx.Select[domain.BookChapter](ctx, store, `SELECT * FROM book_chapters WHERE book_id = ? ORDER BY sort_order ASC`, "book_rust")
	if err != nil || len(chapters) != 2 {
		t.Fatalf("expected 2 normalized book_chapters, got %d (%v)", len(chapters), err)
	}

	// Assert webhooks encrypted secret_ciphertext via AES-256-GCM (never plaintext!)
	wh, err := sqlx.Get[domain.Webhook](ctx, store, `SELECT * FROM webhooks WHERE id = ?`, "wh_ops")
	if err != nil {
		t.Fatalf("load webhook: %v", err)
	}
	if strings.Contains(wh.SecretCiphertext, "my-webhook-hmac-secret") {
		t.Fatal("webhook secret was stored in plaintext instead of AES-256-GCM ciphertext!")
	}
	openedSecret, err := kr.OpenString(wh.SecretCiphertext, config.WebhookSecretAAD("wh_ops"))
	if err != nil || openedSecret != "my-webhook-hmac-secret" {
		t.Fatalf("failed to decrypt webhook secret ciphertext: %v / %q", err, openedSecret)
	}

	// Assert mailUserContext backfilled legacy comment email (encrypted at rest) & masked IP
	cmt1, err := sqlx.Get[domain.Comment](ctx, store, `SELECT * FROM comments WHERE id = ?`, "cmt_1")
	if err != nil {
		t.Fatalf("load cmt_1: %v", err)
	}
	openedEmail, err := kr.OpenString(cmt1.NotifyEmailCiphertext, config.CommentNotifyEmailAAD("cmt_1"))
	if err != nil || openedEmail != "reader_backfill@example.com" || cmt1.IPMasked != "203.0.*.*" {
		t.Fatalf("expected backfilled encrypted email and masked IP on cmt_1, got err=%v email=%q ip=%q", err, openedEmail, cmt1.IPMasked)
	}

	// Assert decomposed settings snapshot: legacy skin fell back to volo-studio, social links extracted
	snap := settingsMgr.Snapshot()
	if snap.Appearance.ActiveTheme != "volo-studio" || snap.Appearance.Accent != "teal" {
		t.Fatalf("unexpected appearance settings after legacy skin fallback: %+v", snap.Appearance)
	}
	if snap.Site.Title != "Will's Notes" || len(snap.Site.SocialLinks) != 2 {
		t.Fatalf("unexpected site settings: %+v", snap.Site)
	}
}

func buildComprehensiveLegacy15TableZip(t *testing.T, zipPath string) {
	t.Helper()
	gen := "0123456789abcdef0123456789abcdef"

	// Build a Series JSON string split into 12 chunks (0..11) and deliberately insert chunk .10 BEFORE .2!
	seriesPayload := `[{"id":"ser_go_deep","title":"Go Internals","slug":"go-internals","description":"CHUNK_ORDER_SENTINEL_0123456789_10_11 — complete 12-chunk reassembly test","status":"completed","articleIds":["art_101","art_102"]}]`
	chunks := splitIntoNChunks(seriesPayload, 12)
	options := []map[string]any{
		{"oId": "version", "optionCategory": "system", "optionValue": "3.0.0"},
		{"oId": "blogTitle", "optionCategory": "preference", "optionValue": "Will's Notes"},
		{"oId": "blogSubtitle", "optionCategory": "preference", "optionValue": "Clean Architecture"},
		{"oId": "localeString", "optionCategory": "preference", "optionValue": "zh_CN"},
		{"oId": "skinDirName", "optionCategory": "skin", "optionValue": "Pinghsu"}, // legacy skin -> fallback volo-studio
		{"oId": "appearanceAccent", "optionCategory": "preference", "optionValue": "teal"},
		{"oId": "usite", "optionCategory": "preference", "optionValue": `{"usiteGithub":"https://github.com/Willh92","usiteX":"https://x.com/willh92"}`},
		{"oId": "mailUserContext", "optionCategory": "mail", "optionValue": "cmt_1:Bob:reader_backfill@example.com;"},
		{
			"oId":            "voloSeries",
			"optionCategory": "preference",
			"optionValue":    fmt.Sprintf(`{"storage":"chunks","generation":"%s","count":12}`, gen),
		},
		{
			"oId":            "voloBooks",
			"optionCategory": "preference",
			"optionValue":    `[{"id":"book_rust","title":"The Rust Book","author":"Steve","sourceType":"mdbook","sourceUrl":"https://doc.rust-lang.org/book/","readingStatus":"reading","rating":5}]`,
		},
		{
			"oId":            "voloBookToc.book_rust",
			"optionCategory": "preference",
			"optionValue":    `[{"title":"Ch 1. Getting Started","url":"https://doc.rust-lang.org/book/ch01-00.html","children":[{"title":"1.1 Installation","url":"https://doc.rust-lang.org/book/ch01-01.html"}]}]`,
		},
		{
			"oId":            "voloDailyStats",
			"optionCategory": "preference",
			"optionValue":    `{"updatedAt":1700000000000,"days":{"2026-11-05":{"pv":120,"uv":45,"referers":{"github.com":19}},"2026-11-06":{"pv":88,"uv":31}}}`,
		},
		{
			"oId":            "voloWebhooks",
			"optionCategory": "preference",
			"optionValue":    `[{"id":"wh_ops","name":"Ops Alert","provider":"custom","url":"https://hooks.example.com/volo","secret":"my-webhook-hmac-secret","events":["comment.created"],"enabled":true}]`,
		},
	}

	// Deliberately append chunks sorted lexicographically (.0, .1, .10, .11, .2, .3...) just like legacy SQL ORDER BY oId ASC!
	lexOrder := []int{0, 1, 10, 11, 2, 3, 4, 5, 6, 7, 8, 9}
	for _, idx := range lexOrder {
		options = append(options, map[string]any{
			"oId":            fmt.Sprintf("voloSeries.%s.%d", gen, idx),
			"optionCategory": chunkOptionCategory,
			"optionValue":    chunks[idx],
		})
	}

	root := map[string]any{
		"options": options,
		"users": []map[string]any{
			{
				"oId":              "default",
				"userName":         "will",
				"userEmail":        "will@example.com",
				"userPasswordHash": "pbkdf2$sha256$120000$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNo",
				"userRole":         "adminRole",
				"userAvatar":       "https://static." + "b3" + "log.org/icons/default-user-thumbnail.png",
			},
			{
				"oId":        "u_editor_2",
				"userName":   "collab",
				"userEmail":  "collab@example.com",
				"userB3Key":  "e10adc3949ba59abbe56e057f20f883e",
				"userRole":   "defaultRole",
				"userAvatar": "https://avatars.example.com/collab.png",
			},
		},
		"categories": []map[string]any{
			{"oId": "cat_go", "categoryTitle": "Go", "categoryURI": "go", "categoryOrder": 2},
			{"oId": "cat_arch", "categoryTitle": "Architecture", "categoryURI": "arch", "categoryOrder": 1},
		},
		"tags": []map[string]any{
			{"oId": "tag_go", "tagTitle": "Golang"},
			{"oId": "tag_ddd", "tagTitle": "DDD"},
			{"oId": "tag_sec", "tagTitle": "Security"},
		},
		"category_tags": []map[string]any{
			{"oId": "ct_1", "category_oId": "cat_go", "tag_oId": "tag_go"},
			{"oId": "ct_2", "category_oId": "cat_arch", "tag_oId": "tag_ddd"},
		},
		"tag_articles": []map[string]any{
			// art_101 has BOTH tag_go (cat_go order=2) and tag_ddd (cat_arch order=1) -> should deterministically pick cat_arch!
			{"oId": "ta_1", "article_oId": "art_101", "tag_oId": "tag_go"},
			{"oId": "ta_2", "article_oId": "art_101", "tag_oId": "tag_ddd"},
			{"oId": "ta_3", "article_oId": "art_102", "tag_oId": "tag_sec"},
		},
		"articles": []map[string]any{
			{
				"oId":                "art_101",
				"articleTitle":       "Migrating Without Dead Links",
				"articleAbstract":    "Clean URLs with 301 protection.",
				"articleContent":     "Full Markdown body for article 101.",
				"articleAuthorId":    "default",
				"articlePermalink":   "/articles/2023/05/18/1684400000000.html",
				"articleStatus":      0,
				"articlePutTop":      true,
				"articleCommentable": true,
				"articleViewCount":   512,
				"articleCreated":     1684400000000,
				"articleUpdated":     1684403600000,
			},
			{
				"oId":                "art_102",
				"articleTitle":       "Protected Deep Dive",
				"articleAbstract":    "Should never leak",
				"articleContent":     "Public intro\n\n:::lock hint=\"VIP\"\nTop secret paragraph\n:::\n",
				"articleAuthorId":    "u_editor_2",
				"articlePermalink":   "/posts/protected-deep-dive",
				"articleViewPwd":     "s3cr3t-pass",
				"articleStatus":      0,
				"articleCommentable": true,
				"articleCreated":     1700000000000,
				"articleUpdated":     1700000000000,
			},
		},
		"articleRevisions": []map[string]any{
			{
				"oId":                "rev_1",
				"articleOId":         "art_101",
				"revisionTitle":      "Migrating Draft v1",
				"revisionAbstract":   "Draft",
				"revisionContent":    "Initial draft content",
				"revisionAuthorName": "will",
				"revisionCreated":    1684390000000,
			},
		},
		"comments": []map[string]any{
			{
				"oId":                      "cmt_1",
				"commentOnId":              "art_101",
				"commentName":              "Bob",
				"commentNotifyEmail":       "", // backfilled from mailUserContext!
				"commentURL":               "https://bob.dev",
				"commentContent":           "Great migration!",
				"commentOriginalCommentId": "",
				"commentClientIP":          "203.0.113.42",
				"commentCreated":           1684410000000,
			},
			{
				"oId":                      "cmt_2",
				"commentOnId":              "art_101",
				"commentName":              "will",
				"commentUserId":            "default",
				"commentContent":           "Thanks Bob!",
				"commentOriginalCommentId": "cmt_1",
				"commentCreated":           1684411000000,
			},
		},
		"pages": []map[string]any{
			{
				"oId":            "nav_1",
				"pageTitle":      "GitHub",
				"pagePermalink":  "https://github.com/Willh92",
				"pageOpenTarget": "_blank",
				"pageOrder":      1,
			},
		},
		"links": []map[string]any{
			{
				"oId":             "lnk_1",
				"linkTitle":       "Go Blog",
				"linkAddress":     "https://go.dev/blog",
				"linkDescription": "Official Go blog",
				"linkOrder":       1,
			},
		},
		"follows": []map[string]any{
			{
				"oId":               "fol_1",
				"followTitle":       "Russ Cox",
				"followAddress":     "https://research.swtch.com/feed.atom",
				"followDescription": "Research blog",
				"followOrder":       1,
			},
		},
		"archivedates":         []any{},
		"archivedate_articles": []any{},
		"plugin":               []any{},
	}

	jsonBytes, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	zf, err := zw.Create("solo-20261106.json")
	if err != nil {
		t.Fatal(err)
	}
	// Prepend UTF-8 BOM to verify BOM-tolerant streaming parser!
	_, _ = zf.Write([]byte{0xEF, 0xBB, 0xBF})
	_, _ = zf.Write(jsonBytes)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zipPath, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func splitIntoNChunks(s string, n int) []string {
	out := make([]string, n)
	base := len(s) / n
	rem := len(s) % n
	pos := 0
	for i := 0; i < n; i++ {
		sz := base
		if i < rem {
			sz++
		}
		out[i] = s[pos : pos+sz]
		pos += sz
	}
	return out
}

func TestImportLegacyResilientToMissingChunksAndRealWorldAnomalies(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(tmp, "resilient.db"),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer store.Close()
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	kr, _, err := config.LoadOrInitKeyRing(strings.Repeat("b", 64), tmp)
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	settingsMgr := repo.NewSettingsManager(store, kr, "zh-CN")

	booksJSON := `[{"id":"b7b80c66","title":"Broken TOC Book","sourceType":"gitbook","sourceUrl":"https://docs.example.com","chapterCount":12}]`
	seriesJSON := `[{"id":"ser_real","title":"Go Deep Dive","articleIds":["art_gh","art_deleted_ghost"]}]`
	ghReposJSON := `[{"githubrepoName":"Volo-go","githubrepoFullName":"Willh92/Volo-go","githubrepoDescription":"Pure Go blog","githubrepoHTMLURL":"https://github.com/Willh92/Volo-go","githubrepoLanguage":"Go","githubrepoStargazersCount":128,"githubrepoForksCount":16,"githubrepoFork":false,"githubrepoUpdated":1730900000000},{"githubrepoName":"SecretRepo","githubrepoFullName":"Willh92/SecretRepo","githubrepoFork":true,"githubrepoUpdated":1730800000000}]`
	ghDisplayJSON := `{"includeForks":true,"hidden":["willh92/secretrepo"]}`

	rawDoc := map[string]any{
		"users": []map[string]any{
			{
				"oId":              "550e8400-e29b-41d4-a716-446655440000",
				"userName":         "will",
				"userEmail":        "will@example.com",
				"userRole":         "adminRole",
				"userPasswordHash": "pbkdf2$sha256$120000$00112233445566778899aabbccddeeff$0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			},
			{
				"oId":       "550e8400-e29b-41d4-a716-446655440001",
				"userName":  "guest_uuid_2",
				"userEmail": "guest2@example.com",
				"userRole":  "visitorRole",
			},
		},
		"options": []map[string]any{
			{"oId": "blogTitle", "optionValue": "Will Studio"},
			{"oId": "myGitHubID", "optionValue": "Willh92"},
			{"oId": "voloBooks", "optionValue": booksJSON},
			// Root manifest referencing a missing chunk (exact reproduction of user's backup failure!)
			{"oId": "voloBookToc.b7b80c66", "optionValue": `{"count":2,"generation":"9003c49a46ad4af0baf8916fcd6f8e2c","storage":"chunks"}`},
			{"oId": "voloSeries", "optionValue": seriesJSON},
			{"oId": "githubRepos", "optionValue": ghReposJSON},
			{"oId": "githubRepositoryDisplay", "optionValue": ghDisplayJSON},
		},
		"articles": []map[string]any{
			{
				"oId":              "art_gh",
				"articleTitle":     "我在 GitHub 上的开源项目",
				"articleAbstract":  "开源作品汇总",
				"articleContent":   "![stats](/github-stats/Willh92.svg)",
				"articleAuthorId":  "550e8400-e29b-41d4-a716-446655440000",
				"articlePermalink": "/my-github-repos",
				"articleStatus":    0,
				"articlePutTop":    true,
				"articleCreated":   1730900000000,
				"articleUpdated":   1730900000000,
			},
		},
		"articleRevisions": []map[string]any{
			{
				"oId":             "rev_orphan",
				"articleOId":      "art_deleted_ghost",
				"revisionTitle":   "Ghost",
				"revisionContent": "Orphan revision must not break FK",
			},
		},
		"pages": []map[string]any{
			{
				"oId":           "nav_gh",
				"pageTitle":     "我的开源",
				"pagePermalink": "/my-github-repos",
				"pageIcon":      "/images/github-icon.png",
				"pageOrder":     1,
			},
			{
				"oId":           "nav_admin",
				"pageTitle":     "管理后台",
				"pagePermalink": "/admin-index.do",
				"pageIcon":      "/images/admin-panel.svg",
				"pageOrder":     2,
			},
		},
	}

	jsonPath := filepath.Join(tmp, "legacy-broken-chunk.json")
	rawBytes, _ := json.Marshal(rawDoc)
	if err := os.WriteFile(jsonPath, rawBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	rep, err := ImportLegacyArchive(ctx, store, settingsMgr, kr, jsonPath, ImportOptions{})
	if err != nil {
		t.Fatalf("expected resilient import to succeed despite missing book TOC chunk, got: %v", err)
	}
	if rep.Users != 2 || rep.Books != 1 || rep.Articles != 1 {
		t.Fatalf("unexpected counts: %+v", rep)
	}
	if len(rep.Warnings) == 0 {
		t.Fatalf("expected warning recorded for missing voloBookToc chunk")
	}

	snap := settingsMgr.Snapshot()
	if len(snap.SEO.GitHubRepos) != 2 {
		t.Fatalf("expected 2 GitHub repos parsed (numeric Updated timestamp), got %d", len(snap.SEO.GitHubRepos))
	}
	if snap.SEO.GitHubRepos[0].Hidden || !snap.SEO.GitHubRepos[1].Hidden {
		t.Fatalf("expected case-insensitive hidden repo mapping preserved: %+v", snap.SEO.GitHubRepos)
	}

	navs, _ := sqlx.Select[domain.NavItem](ctx, store, `SELECT * FROM nav_items ORDER BY sort_order ASC`)
	if len(navs) != 2 || navs[0].Icon != "/images/github-icon.png" || navs[1].URL != "/console" {
		t.Fatalf("unexpected migrated nav items: %+v", navs)
	}
}

func TestImportLegacyArchiveURLRewriteMediaExtractAndOverrides(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	uploadDir := filepath.Join(tmp, "uploads")
	if err := os.MkdirAll(uploadDir, 0o750); err != nil {
		t.Fatal(err)
	}

	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(tmp, "rewrite-migrated.db"),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer store.Close()
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	kr, _, err := config.LoadOrInitKeyRing(strings.Repeat("c", 64), tmp)
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	settingsMgr := repo.NewSettingsManager(store, kr, "zh-CN")

	legacyDoc := map[string]any{
		"options": []map[string]any{
			{"oId": "blogTitle", "optionValue": "Old Legacy Title"},
			{"oId": "blogSubtitle", "optionValue": "Old Subtitle"},
			{"oId": "storageType", "optionValue": "s3"},
			{"oId": "voloBooks", "optionValue": `[{"id":"bk_1","title":"Handbook","coverUrl":"https://cdn.old.example.com/image/banner.png","sourceUrl":"https://old.example.com/articles/2023/05/18/111.html"}]`},
			{"oId": "voloBookToc.bk_1", "optionValue": `[{"title":"Intro","url":"https://old.example.com/articles/2023/05/18/111.html"}]`},
		},
		"users": []map[string]any{
			{
				"oId":              "default",
				"userName":         "legacyadmin",
				"userEmail":        "old@example.com",
				"userRole":         "adminRole",
				"userAvatar":       "https://cdn.old.example.com/image/banner.png",
				"userPasswordHash": "pbkdf2$sha256$120000$0011223344556677$0123456789abcdef0123456789abcdef",
			},
		},
		"articles": []map[string]any{
			{
				"oId":              "art_first",
				"articleTitle":     "First Migrated Post",
				"articleAbstract":  "See ![banner](https://cdn.old.example.com/image/banner.png)",
				"articleContent":   "Read [Second Post](https://old.example.com/articles/2023/05/19/222.html) and ![photo](/upload/2023/photo.jpg)",
				"articleAuthorId":  "default",
				"articlePermalink": "/articles/2023/05/18/111.html",
				"articleStatus":    0,
				"articleCreated":   1684400000000,
				"articleUpdated":   1684400000000,
			},
			{
				"oId":              "art_second",
				"articleTitle":     "Second Target Post",
				"articleAbstract":  "Target summary",
				"articleContent":   "Backlink to [First](/articles/2023/05/18/111.html)",
				"articleAuthorId":  "default",
				"articlePermalink": "/articles/2023/05/19/222.html",
				"articleStatus":    0,
				"articleCreated":   1684500000000,
				"articleUpdated":   1684500000000,
			},
		},
		"comments": []map[string]any{
			{
				"oId":            "cmt_rw",
				"commentOnId":    "art_first",
				"commentName":    "Reader",
				"commentURL":     "https://old.example.com",
				"commentContent": "Check https://old.example.com/articles/2023/05/19/222.html",
				"commentCreated": 1684600000000,
			},
		},
		"pages": []map[string]any{
			{
				"oId":           "nav_custom_admin",
				"pageTitle":     "Console",
				"pagePermalink": "/start",
				"pageOrder":     1,
			},
		},
	}

	docBytes, err := json.Marshal(legacyDoc)
	if err != nil {
		t.Fatal(err)
	}
	pngBytes := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x00, 0x00, 0x00, 0x0d}
	jpgBytes := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01}

	zipPath := filepath.Join(tmp, "legacy-with-media.zip")
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	addZipEntry := func(name string, data []byte) {
		w, cErr := zw.Create(name)
		if cErr != nil {
			t.Fatal(cErr)
		}
		if _, wErr := w.Write(data); wErr != nil {
			t.Fatal(wErr)
		}
	}
	addZipEntry("data.json", docBytes)
	addZipEntry("image/banner.png", pngBytes)
	addZipEntry("upload/2023/photo.jpg", jpgBytes)
	addZipEntry("image/xss.svg", []byte("<svg onload=alert(1)></svg>"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zipPath, zbuf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	opts := ImportOptions{
		TargetUploadDir:   uploadDir,
		AdminPath:         "/studio-admin",
		OldURLPrefixes:    []string{"https://old.example.com", "https://cdn.old.example.com"},
		NewSiteURL:        "https://new.example.com",
		SiteTitle:         "Fresh Migrated Studio",
		SiteSubtitle:      "New Era",
		DefaultLocale:     "en-US",
		TimeZone:          "UTC",
		Accent:            "amber",
		ForceLocalStorage: true,
		AdminUsername:     "newowner",
		AdminEmail:        "newowner@example.com",
		AdminDisplayName:  "New Owner",
		AdminPasswordHash: "$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNo",
	}

	dryRep, err := ImportLegacyArchive(ctx, store, settingsMgr, kr, zipPath, ImportOptions{
		DryRun:          true,
		TargetUploadDir: uploadDir,
		OldURLPrefixes:  opts.OldURLPrefixes,
	})
	if err != nil {
		t.Fatalf("dry-run ImportLegacyArchive: %v", err)
	}
	if dryRep.MediaFiles != 2 || dryRep.URLsRewritten == 0 || dryRep.TargetDialect != "sqlite" {
		t.Fatalf("unexpected dry-run report: %+v", dryRep)
	}

	rep, err := ImportLegacyArchive(ctx, store, settingsMgr, kr, zipPath, opts)
	if err != nil {
		t.Fatalf("live ImportLegacyArchive: %v", err)
	}
	if rep.MediaFiles != 2 || rep.URLsRewritten < 4 {
		t.Fatalf("expected 2 safe media files extracted and >=4 URLs rewritten, got: %+v", rep)
	}

	// Unsafe .svg must never be extracted while .png and .jpg exist under uploads/
	if _, err := os.Stat(filepath.Join(uploadDir, "banner.png")); err != nil {
		t.Fatalf("expected extracted banner.png: %v", err)
	}
	if _, err := os.Stat(filepath.Join(uploadDir, "2023", "photo.jpg")); err != nil {
		t.Fatalf("expected extracted 2023/photo.jpg: %v", err)
	}
	if _, err := os.Stat(filepath.Join(uploadDir, "xss.svg")); err == nil {
		t.Fatal("unsafe .svg should not be extracted from legacy archive")
	}

	// Forward cross-article link in art_first (pointing to art_second which appeared AFTER art_first) must be rewritten via Pass 1 map!
	art1, err := sqlx.Get[domain.RawArticle](ctx, store, `SELECT * FROM articles WHERE id = ?`, "art_first")
	if err != nil {
		t.Fatalf("load art_first: %v", err)
	}
	art2, err := sqlx.Get[domain.RawArticle](ctx, store, `SELECT * FROM articles WHERE id = ?`, "art_second")
	if err != nil {
		t.Fatalf("load art_second: %v", err)
	}
	if !strings.Contains(art1.ContentMD, "https://new.example.com/posts/"+art2.Slug) || strings.Contains(art1.ContentMD, "old.example.com") {
		t.Fatalf("expected forward cross-article link rewritten to https://new.example.com/posts/%s, got: %s", art2.Slug, art1.ContentMD)
	}
	if !strings.Contains(art1.SummaryMD, "https://new.example.com/uploads/banner.png") || !strings.Contains(art1.ContentMD, "/uploads/2023/photo.jpg") {
		t.Fatalf("expected bundled media URLs rewritten to /uploads/..., got summary=%q content=%q", art1.SummaryMD, art1.ContentMD)
	}

	// Book chapter URL should also rewrite to https://new.example.com/posts/<art1.Slug>
	chaps, _ := sqlx.Select[domain.BookChapter](ctx, store, `SELECT * FROM book_chapters WHERE book_id = ?`, "bk_1")
	if len(chaps) != 1 || !strings.HasSuffix(chaps[0].URL, "/posts/"+art1.Slug) {
		t.Fatalf("expected book chapter URL rewritten to .../posts/%s, got %+v", art1.Slug, chaps)
	}

	// Nav item `/start` must rewrite to custom AdminPath `/studio-admin`
	navs, _ := sqlx.Select[domain.NavItem](ctx, store, `SELECT * FROM nav_items`)
	if len(navs) != 1 || navs[0].URL != "/studio-admin" {
		t.Fatalf("expected /start nav rewritten to /studio-admin, got %+v", navs)
	}

	// Verify site, locale, appearance, and ForceLocalStorage overrides
	snap := settingsMgr.Snapshot()
	if snap.Site.Title != "Fresh Migrated Studio" || snap.Site.Subtitle != "New Era" || snap.Site.DefaultLocale != "en-US" || snap.Site.TimeZone != "UTC" {
		t.Fatalf("unexpected site overrides: %+v", snap.Site)
	}
	if snap.Appearance.Accent != "amber" || snap.Storage.ActiveDriver != "local" {
		t.Fatalf("expected accent=amber and storage=local override, got appearance=%+v storage=%+v", snap.Appearance, snap.Storage)
	}

	// Verify primary admin credential overrides cleared LegacyHash and applied new Argon2id PasswordHash
	admin, err := sqlx.Get[domain.User](ctx, store, `SELECT * FROM users WHERE id = ?`, defaultMappedAdminID)
	if err != nil {
		t.Fatalf("load primary admin: %v", err)
	}
	if admin.Username != "newowner" || admin.Email != "newowner@example.com" || admin.DisplayName != "New Owner" || admin.PasswordHash != opts.AdminPasswordHash || admin.LegacyHash != "" {
		t.Fatalf("unexpected primary admin overrides: %+v", admin)
	}
	if !strings.HasSuffix(admin.AvatarURL, "/uploads/banner.png") {
		t.Fatalf("expected admin avatar URL rewritten to .../uploads/banner.png, got %q", admin.AvatarURL)
	}
}

func TestETLRound3_DisguisedMarkup_RollbackCleanup_FKSafeClear_AnalyticsMapping_And_NestedLock(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	uploadDir := filepath.Join(tmp, "uploads")
	if err := os.MkdirAll(uploadDir, 0o750); err != nil {
		t.Fatal(err)
	}

	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(tmp, "round3-etl.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatal(err)
	}

	kr, _, err := config.LoadOrInitKeyRing(strings.Repeat("e", 64), tmp)
	if err != nil {
		t.Fatal(err)
	}
	settingsMgr := repo.NewSettingsManager(store, kr, "zh-CN")
	_, _ = settingsMgr.Reload(ctx)

	// 1. Rollback cleanup test: `"links": {"bad":"object"}` passes Pass 1 (skipJSONValue)
	//    and fails inside streamPass2IntoTx AFTER persistBundledMediaTx wrote `rollback-pic.png`!
	failZip := filepath.Join(tmp, "fail-pass2.zip")
	var failBuf bytes.Buffer
	fzw := zip.NewWriter(&failBuf)
	wData, _ := fzw.Create("data.json")
	_, _ = wData.Write([]byte(`{
		"users": [{"oId":"default","userName":"admin","userEmail":"a@example.com","userRole":"adminRole"}],
		"links": {"not":"an-array"}
	}`))
	wImg, _ := fzw.Create("image/sub/rollback-pic.png")
	_, _ = wImg.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 13})
	_ = fzw.Close()
	if err := os.WriteFile(failZip, failBuf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportLegacyArchive(ctx, store, settingsMgr, kr, failZip, ImportOptions{TargetUploadDir: uploadDir}); err == nil {
		t.Fatal("expected Pass 2 malformed links object to fail ImportLegacyArchive")
	}
	if _, statErr := os.Stat(filepath.Join(uploadDir, "sub", "rollback-pic.png")); !os.IsNotExist(statErr) {
		t.Fatalf("expected rollback-pic.png to be unlinked after failed transaction, statErr=%v", statErr)
	}

	// 2. Seed existing FK child rows across webhook_deliveries, follow_items, reactions, and pages
	//    to prove `tablesToClear` deletes them in FK-safe reverse order!
	now := int64(1730900000000)
	_, err = store.ExecContext(ctx,
		`INSERT INTO users (id, username, email, display_name, avatar_url, bio, website_url, role, password_hash, legacy_hash, status, email_verified_at, preferred_locale, created_at, updated_at)
		 VALUES ('u_old', 'old', 'old@example.com', 'Old', '', '', '', 'admin', 'h', '', 'active', 0, 'zh-CN', ?, ?)`, now, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.ExecContext(ctx,
		`INSERT INTO pages (id, author_id, title, slug, template, content_md, is_published, allow_comment, show_toc, lang, translation_group, view_count, comment_count, created_at, updated_at)
		 VALUES ('pg_old', 'u_old', 'About', 'about', 'default', 'hi', 1, 1, 0, 'zh-CN', '', 0, 0, ?, ?)`, now, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.ExecContext(ctx,
		`INSERT INTO webhooks (id, name, provider, url, secret_ciphertext, chat_id, events_json, is_enabled, last_status_code, last_error, last_triggered_at, created_at, updated_at)
		 VALUES ('wh_old', 'Old Hook', 'custom', 'https://example.com/hook', '', '', '[]', 1, 200, '', 0, ?, ?)`, now, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.ExecContext(ctx,
		`INSERT INTO webhook_deliveries (id, webhook_id, event, status_code, duration_ms, success, payload_json, response_body, error_text, created_at)
		 VALUES ('whd_old', 'wh_old', 'comment.created', 200, 10, 1, '{}', 'ok', '', ?)`, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.ExecContext(ctx,
		`INSERT INTO follows (id, title, site_url, feed_url, description, icon_url, sort_order, last_synced_at, sync_error, created_at, updated_at)
		 VALUES ('fl_old', 'Feed', 'https://example.com', 'https://example.com/rss', '', '', 0, 0, '', ?, ?)`, now, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.ExecContext(ctx,
		`INSERT INTO follow_items (id, follow_id, guid_hash, title, url, summary, author_name, published_at)
		 VALUES ('fli_old', 'fl_old', 'g1', 'Item', 'https://example.com/1', '', '', ?)`, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.ExecContext(ctx,
		`INSERT INTO reactions (target_type, target_id, actor_hash, kind, created_at) VALUES ('article', 'a_old', 'fp_1', 'heart', ?)`, now)
	if err != nil {
		t.Fatal(err)
	}

	// 3. Full Round 3 archive:
	//    - UUID > 32-char article oId mapped consistently into analytics_top_items
	//    - Duplicate tag_articles + articleTags binding via UpsertSQL
	//    - Disguised `<svg onload=...>` named `evil-disguised.png` & `<html><script>` named `evil.jpg` rejected pre-disk
	//    - Unsafe `faviconURL: "javascript:alert(1)"` sanitized without failing PatchDomain("site")
	//    - Nested `:::tip` inside `:::lock` excluded from word_count, while `:::lock` inside ```markdown does NOT set has_content_lock!
	const longUUIDArtID = "550e8400-e29b-41d4-a716-999999999999"
	statsJSON := `{"updatedAt":1730900000000,"days":{"2026-11-08":{"pv":42,"uv":10,"articles":{"` + longUUIDArtID + `":19},"referers":{"https://news.ycombinator.com":7}}}}`
	longPermalink := "/archive/2026/11/08/" + strings.Repeat("deep-legacy-segment-", 12) + "post.html"

	doc := map[string]any{
		"options": []map[string]any{
			{"oId": "blogTitle", "optionValue": "Round 3 ETL"},
			{"oId": "faviconURL", "optionValue": "javascript:alert(1)"},
			{"oId": "usite", "optionValue": `{"github":"https://github.com/Willh92","weibo":"javascript:evil()"}`},
			{"oId": "voloDailyStats", "optionValue": statsJSON},
		},
		"users": []map[string]any{
			{"oId": "default", "userName": "will", "userEmail": "will@example.com", "userRole": "adminRole"},
		},
		"tags": []map[string]any{
			{"oId": "tag_go", "tagTitle": "Golang"},
		},
		"tag_articles": []map[string]any{
			{"tag_oId": "tag_go", "article_oId": longUUIDArtID},
			{"tag_oId": "tag_go", "article_oId": longUUIDArtID},
		},
		"articles": []map[string]any{
			{
				"oId":              longUUIDArtID,
				"articleTitle":     "Nested Lock Post",
				"articleAbstract":  "Summary",
				"articleContent":   "alpha beta\n:::lock\nsecretOne\n:::tip\nsecretTwo\n:::\nsecretThree\n:::\ngamma",
				"articleTags":      "Golang,Golang",
				"articleAuthorId":  "default",
				"articlePermalink": longPermalink,
				"articleStatus":    0,
				"articleCreated":   now,
				"articleUpdated":   now,
			},
			{
				"oId":              "art_fenced_lock",
				"articleTitle":     "Code Fence Example",
				"articleAbstract":  "Summary",
				"articleContent":   "```markdown\n:::lock\nnot locked\n:::\n```\n    :::lock\n    indented code\n    :::\nplain words",
				"articleAuthorId":  "default",
				"articlePermalink": "/fenced-example.html",
				"articleStatus":    0,
				"articleCreated":   now,
				"articleUpdated":   now,
			},
		},
	}
	docRaw, _ := json.Marshal(doc)

	okZip := filepath.Join(tmp, "round3-ok.zip")
	var okBuf bytes.Buffer
	ozw := zip.NewWriter(&okBuf)
	addEntry := func(name string, b []byte) {
		w, _ := ozw.Create(name)
		_, _ = w.Write(b)
	}
	addEntry("data.json", docRaw)
	addEntry("image/disguised/evil-disguised.png", []byte("<?xml version=\"1.0\"?><svg onload=\"alert(1)\"></svg>"))
	addEntry("image/disguised2/evil-html.jpg", []byte("<!DOCTYPE html><html><body><script>alert(1)</script></body></html>"))
	addEntry("image/safe/clean.png", []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 13})
	_ = ozw.Close()
	if err := os.WriteFile(okZip, okBuf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	rep, err := ImportLegacyArchive(ctx, store, settingsMgr, kr, okZip, ImportOptions{TargetUploadDir: uploadDir})
	if err != nil {
		t.Fatalf("Round 3 ImportLegacyArchive failed: %v", err)
	}
	if rep.MediaFiles != 1 {
		t.Fatalf("expected only 1 valid PNG extracted (2 disguised markup files skipped), got MediaFiles=%d warnings=%v", rep.MediaFiles, rep.Warnings)
	}
	// Neither disguised file NOR its parent directory should have been created on disk!
	if _, err := os.Stat(filepath.Join(uploadDir, "disguised")); !os.IsNotExist(err) {
		t.Fatalf("expected disguised SVG directory never created on disk, stat=%v", err)
	}
	if _, err := os.Stat(filepath.Join(uploadDir, "disguised2")); !os.IsNotExist(err) {
		t.Fatalf("expected disguised HTML directory never created on disk, stat=%v", err)
	}

	// Verify old pages/reactions/follow_items/webhook_deliveries were cleared!
	for _, tbl := range []string{"pages", "reactions", "follow_items", "webhook_deliveries"} {
		var cnt int64
		cErr := store.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tbl).Scan(&cnt)
		if cErr != nil || cnt != 0 {
			t.Fatalf("expected table %s cleared to 0 rows, got cnt=%d err=%v", tbl, cnt, cErr)
		}
	}

	// Verify >190-rune permalink redirect succeeded (512 width) and analytics_top_items mapped longUUIDArtID -> actual articles.id!
	lockedArt, err := sqlx.Get[domain.RawArticle](ctx, store, `SELECT * FROM articles WHERE slug != 'fenced-example'`)
	if err != nil {
		t.Fatalf("load lockedArt: %v", err)
	}
	if !lockedArt.HasContentLock || lockedArt.WordCount != 3 {
		t.Fatalf("expected HasContentLock=true and WordCount=3 ('alpha beta' + 'gamma', excluding entire nested :::lock), got lock=%v words=%d", lockedArt.HasContentLock, lockedArt.WordCount)
	}
	topHit, err := sqlx.Get[domain.AnalyticsTopItem](ctx, store, `SELECT * FROM analytics_top_items WHERE kind = 'article'`)
	if err != nil {
		t.Fatalf("load analytics_top_items article row: %v", err)
	}
	if topHit.ItemKey != lockedArt.ID || topHit.Hits != 19 {
		t.Fatalf("expected analytics_top_items.item_key mapped to %q (hits=19), got %+v", lockedArt.ID, topHit)
	}
	var redirCount int64
	_ = store.QueryRowContext(ctx, `SELECT COUNT(*) FROM redirects WHERE source_path = ?`, longPermalink).Scan(&redirCount)
	if redirCount != 1 {
		t.Fatalf("expected >190-char historical permalink redirect stored under V6 VARCHAR(512), got %d", redirCount)
	}

	// Verify fenced/4-space indented :::lock did NOT set has_content_lock!
	fencedArt, err := sqlx.Get[domain.RawArticle](ctx, store, `SELECT * FROM articles WHERE id = 'art_fenced_lock'`)
	if err != nil {
		t.Fatalf("load fencedArt: %v", err)
	}
	if fencedArt.HasContentLock {
		t.Fatalf("expected HasContentLock=false when :::lock only appears inside fenced/4-space code blocks!")
	}

	// Verify unsafe faviconURL was cleared to "" while valid GitHub social link survived
	snap := settingsMgr.Snapshot()
	if snap.Site.FaviconURL != "" || len(snap.Site.SocialLinks) != 1 || snap.Site.SocialLinks[0].Icon != "github" {
		t.Fatalf("unexpected sanitized Site settings: favicon=%q social=%+v", snap.Site.FaviconURL, snap.Site.SocialLinks)
	}
}


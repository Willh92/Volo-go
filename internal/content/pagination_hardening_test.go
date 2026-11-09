package content

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/pkg/plugin"
)

func TestTaxonomiesSeriesLinksAndBooksSQLPagination(t *testing.T) {
	ctx, store, _, _, svc := setupContentTestEnv(t)
	now := time.Now().UnixMilli()

	if _, err := store.ExecContext(ctx, `INSERT INTO users (
		id, username, email, display_name, avatar_url, bio, website_url,
		role, password_hash, legacy_hash, status, preferred_locale, created_at, updated_at
	) VALUES ('u_adm', 'adm', 'adm@example.com', 'Admin', '', '', '', 'admin', 'x', '', 'active', 'en', ?, ?)`, now, now); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	// 1. Categories + Articles
	for i := 0; i < 5; i++ {
		if _, err := store.ExecContext(ctx, `INSERT INTO categories (id, name, slug, description, sort_order, created_at, updated_at)
			VALUES (?, ?, ?, '', ?, ?, ?)`,
			fmt.Sprintf("cat_%d", i), fmt.Sprintf("Category %d", i), fmt.Sprintf("cat-%d", i), i, now, now); err != nil {
			t.Fatalf("seed category: %v", err)
		}
	}
	if _, err := store.ExecContext(ctx, `INSERT INTO series (id, title, slug, description, cover_url, sort_order, created_at, updated_at) VALUES
		('ser_go', 'Go Internals', 'go-internals', 'Runtime and scheduler deep dives', '', 1, ?, ?),
		('ser_db', 'SQLite WAL', 'sqlite-wal', 'Storage engine notes', '', 2, ?, ?),
		('ser_ui', 'Design Systems', 'design-systems', 'Typography and layout', '', 3, ?, ?)`,
		now, now, now, now, now, now); err != nil {
		t.Fatalf("seed series: %v", err)
	}

	for i := 0; i < 6; i++ {
		aid := fmt.Sprintf("art_ep_%d", i)
		catID := "cat_0"
		if i%2 == 1 {
			catID = "cat_2"
		}
		pubAt := now + int64(i)*1000
		if _, err := store.ExecContext(ctx, `INSERT INTO articles (
			id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
			cover_url, status, published_at, allow_comment, password_ciphertext,
			audio_url, audio_mime, lang, translation_group, license_override, word_count, reading_minutes, created_at, updated_at
		) VALUES (?, 'u_adm', ?, ?, ?, '', 'Explicit episode summary', ?, '/uploads/cover.webp', 'published', ?, ?, '', '', '', 'en', '', '', 420, 2, ?, ?)`,
			aid, catID, fmt.Sprintf("Episode %d", i), fmt.Sprintf("ep-%d", i), strings.Repeat("heavy body ", 200), pubAt, true, pubAt, pubAt); err != nil {
			t.Fatalf("seed article: %v", err)
		}
		if _, err := store.ExecContext(ctx, `INSERT INTO series_articles (series_id, article_id, sort_order) VALUES ('ser_go', ?, ?)`, aid, i); err != nil {
			t.Fatalf("seed series_articles: %v", err)
		}
	}

	catPage1, catTotal, err := svc.ListCategoriesWithCountsPage(ctx, 1, 2)
	if err != nil || catTotal != 5 || len(catPage1) != 2 || catPage1[0].ID != "cat_0" || catPage1[0].ArticleCount != 3 {
		t.Fatalf("unexpected ListCategoriesWithCountsPage p1: total=%d items=%+v err=%v", catTotal, catPage1, err)
	}
	catPage2, _, err := svc.ListCategoriesWithCountsPage(ctx, 2, 2)
	if err != nil || len(catPage2) != 2 || catPage2[0].ID != "cat_2" || catPage2[0].ArticleCount != 3 {
		t.Fatalf("unexpected ListCategoriesWithCountsPage p2: items=%+v err=%v", catPage2, err)
	}

	// 2. Tags with q filter + SQL pagination
	for i := 0; i < 8; i++ {
		prefix := "golang"
		if i >= 5 {
			prefix = "rust"
		}
		tid := fmt.Sprintf("tg_%d", i)
		if _, err := store.ExecContext(ctx, `INSERT INTO tags (id, name, slug, created_at) VALUES (?, ?, ?, ?)`,
			tid, fmt.Sprintf("%s-topic-%d", prefix, i), fmt.Sprintf("%s-slug-%d", prefix, i), now); err != nil {
			t.Fatalf("seed tag: %v", err)
		}
	}
	_, _ = store.ExecContext(ctx, `INSERT INTO article_tags (article_id, tag_id) VALUES ('art_ep_0', 'tg_3'), ('art_ep_1', 'tg_3')`)

	goTagsP1, goTagTotal, err := svc.ListTagsWithCountsPage(ctx, "golang", 1, 3)
	if err != nil || goTagTotal != 5 || len(goTagsP1) != 3 || goTagsP1[0].ID != "tg_3" || goTagsP1[0].ArticleCount != 2 {
		t.Fatalf("unexpected ListTagsWithCountsPage: total=%d items=%+v err=%v", goTagTotal, goTagsP1, err)
	}
	goTagsP2, _, err := svc.ListTagsWithCountsPage(ctx, "golang", 2, 3)
	if err != nil || len(goTagsP2) != 2 {
		t.Fatalf("unexpected ListTagsWithCountsPage p2: items=%+v err=%v", goTagsP2, err)
	}

	// 3. Series list + Series detail page
	serItems, serTotal, err := svc.ListSeriesPage(ctx, "scheduler", 1, 10)
	if err != nil || serTotal != 1 || len(serItems) != 1 || serItems[0].Slug != "go-internals" {
		t.Fatalf("unexpected ListSeriesPage: total=%d items=%+v err=%v", serTotal, serItems, err)
	}
	serViewP2, err := svc.GetSeriesBySlugPage(ctx, "go-internals", 2, 2, "en")
	if err != nil || serViewP2.TotalArticles != 6 || len(serViewP2.Articles) != 2 || serViewP2.Articles[0].Slug != "ep-2" || serViewP2.Articles[1].Slug != "ep-3" {
		t.Fatalf("unexpected GetSeriesBySlugPage p2: %+v err=%v", serViewP2, err)
	}
	if serViewP2.Articles[0].Summary != "Explicit episode summary" || serViewP2.Articles[0].CoverURL != "/uploads/cover.webp" {
		t.Fatalf("expected projected card summary & cover intact, got %+v", serViewP2.Articles[0])
	}

	// 4. Public Links with category pills & SQL pagination
	linkSeeds := []struct {
		id, title, cat string
		order          int
	}{
		{"lnk_1", "Alpha", "Friends", 1},
		{"lnk_2", "Beta", "Friends", 2},
		{"lnk_3", "Gamma", "Friends", 3},
		{"lnk_4", "Go Dev", "Tools", 10},
	}
	for _, ls := range linkSeeds {
		if _, err := store.ExecContext(ctx, `INSERT INTO links (
			id, title, url, description, icon_url, category_name, sort_order, health_status, http_code, latency_ms, health_error, last_checked_at, created_at, updated_at
		) VALUES (?, ?, 'https://example.com', 'desc', '', ?, ?, 'ok', 200, 10, '', 0, ?, ?)`,
			ls.id, ls.title, ls.cat, ls.order, now, now); err != nil {
			t.Fatalf("seed link: %v", err)
		}
	}
	linksP1, linkCats, linkTotal, err := svc.ListPublicLinksPage(ctx, "Friends", 1, 2)
	if err != nil || linkTotal != 3 || len(linksP1) != 2 || len(linkCats) != 2 || linkCats[0] != "Friends" || linkCats[1] != "Tools" {
		t.Fatalf("unexpected ListPublicLinksPage: total=%d cats=%v items=%+v err=%v", linkTotal, linkCats, linksP1, err)
	}

	// 5. Books with q search & page-scoped chapter counts
	for i := 0; i < 4; i++ {
		bid := fmt.Sprintf("bk_%d", i)
		if _, err := store.ExecContext(ctx, `INSERT INTO books (
			id, title, slug, description, cover_url, source_type, source_url, series_id, embed_mode, reading_status, chapter_count, synced_at, sort_order, created_at, updated_at
		) VALUES (?, ?, ?, ?, '', 'series', '', '', 'redirect', 'reading', 0, 0, ?, ?, ?)`,
			bid, fmt.Sprintf("Systems Book %d", i), fmt.Sprintf("sys-book-%d", i), fmt.Sprintf("Deep topic %d", i), i, now, now); err != nil {
			t.Fatalf("seed book: %v", err)
		}
	}
	if _, err := store.ExecContext(ctx, `INSERT INTO book_chapters (id, book_id, parent_id, title, url, depth, sort_order) VALUES
		('ch_1', 'bk_0', '', 'Ch 1', '/posts/ep-0', 0, 1),
		('ch_2', 'bk_0', '', 'Ch 2', '/posts/ep-1', 0, 2)`); err != nil {
		t.Fatalf("seed book_chapters: %v", err)
	}

	booksP1, booksTotal, err := svc.ListBooksPage(ctx, "systems", 1, 2)
	if err != nil || booksTotal != 4 || len(booksP1) != 2 || booksP1[0].ID != "bk_0" || booksP1[0].ChapterCount != 2 {
		t.Fatalf("unexpected ListBooksPage: total=%d items=%+v err=%v", booksTotal, booksP1, err)
	}
}

func TestArchiveMonthStats_And_BacklinksLimit50(t *testing.T) {
	ctx, store, _, _, svc := setupContentTestEnv(t)
	loc := time.UTC
	t2025Jan := time.Date(2025, 1, 15, 10, 0, 0, 0, loc).UnixMilli()
	t2025Feb := time.Date(2025, 2, 10, 10, 0, 0, 0, loc).UnixMilli()
	t2026Mar := time.Date(2026, 3, 20, 10, 0, 0, 0, loc).UnixMilli()

	if _, err := store.ExecContext(ctx, `INSERT INTO users (
		id, username, email, display_name, avatar_url, bio, website_url,
		role, password_hash, legacy_hash, status, preferred_locale, created_at, updated_at
	) VALUES ('u_adm', 'adm', 'adm@example.com', 'Admin', '', '', '', 'admin', 'x', '', 'active', 'en', ?, ?)`, t2025Jan, t2025Jan); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	// Target article + 65 published source articles across 2025-01, 2025-02, and 2026-03
	if _, err := store.ExecContext(ctx, `INSERT INTO articles (
		id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
		cover_url, status, published_at, allow_comment, password_ciphertext,
		audio_url, audio_mime, lang, translation_group, license_override, created_at, updated_at
	) VALUES ('art_target', 'u_adm', '', 'Target', 'target', '', '', 'body', '', 'published', ?, ?, '', '', '', 'en', '', '', ?, ?)`,
		t2026Mar, true, t2026Mar, t2026Mar); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	err := store.WithTx(ctx, func(tx *sqlx.Tx) error {
		for i := 0; i < 65; i++ {
			ts := t2026Mar - int64(i)*1000
			if i >= 55 && i < 60 {
				ts = t2025Feb + int64(i)*1000
			} else if i >= 60 {
				ts = t2025Jan + int64(i)*1000
			}
			aid := fmt.Sprintf("src_%02d", i)
			if _, e := tx.ExecContext(ctx, `INSERT INTO articles (
				id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
				cover_url, status, published_at, allow_comment, password_ciphertext,
				audio_url, audio_mime, lang, translation_group, license_override, created_at, updated_at
			) VALUES (?, 'u_adm', '', ?, ?, '', '', 'body', '', 'published', ?, ?, '', '', '', 'en', '', '', ?, ?)`,
				aid, aid, aid, ts, true, ts, ts); e != nil {
				return e
			}
			if _, e := tx.ExecContext(ctx, `INSERT INTO article_backlinks (source_article_id, target_article_id, updated_at) VALUES (?, 'art_target', ?)`, aid, ts); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed 65 articles & backlinks: %v", err)
	}

	stats, err := svc.ArchiveMonthStats(ctx, "UTC")
	if err != nil {
		t.Fatalf("ArchiveMonthStats: %v", err)
	}
	// Total = 1 target (2026-03) + 55 (2026-03) + 5 (2025-02) + 5 (2025-01) = 66
	if stats.TotalArticles != 66 || stats.YearCounts["2026"] != 56 || stats.YearCounts["2025"] != 10 {
		t.Fatalf("unexpected year counts: %+v", stats)
	}
	if stats.MonthCounts["2026-03"] != 56 || stats.MonthCounts["2025-02"] != 5 || stats.MonthCounts["2025-01"] != 5 {
		t.Fatalf("unexpected month counts: %+v", stats)
	}

	targetRaw, err := svc.GetPublishedRawArticleBySlug(ctx, "target")
	if err != nil {
		t.Fatalf("GetPublishedRawArticleBySlug: %v", err)
	}
	backlinks := svc.ListInboundBacklinks(ctx, targetRaw)
	if len(backlinks) != 50 {
		t.Fatalf("expected ListInboundBacklinks capped at LIMIT 50, got %d", len(backlinks))
	}
}

func TestRenderCacheSaltFunc_KeepsArticleHTMLCacheActiveWithHooks(t *testing.T) {
	ctx, store, _, pluginMgr, svc := setupContentTestEnv(t)
	now := time.Now().UnixMilli()

	var hookCalls atomic.Int32
	var badge atomic.Value
	badge.Store("V1")

	hp := &testHookPlugin{
		id: "salt-hook-plugin",
		onHook: func(hookName string, payload any) (any, error) {
			if hookName == plugin.HookRenderPostHTML {
				hookCalls.Add(1)
				p := payload.(plugin.ArticleHookPayload)
				p.HTML = p.HTML + "<p>SALT_" + badge.Load().(string) + "</p>"
				return p, nil
			}
			return payload, nil
		},
	}
	if err := pluginMgr.RegisterBuiltin(ctx, hp); err != nil {
		t.Fatalf("RegisterBuiltin: %v", err)
	}
	if err := pluginMgr.Enable(ctx, hp.id); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	if _, err := store.ExecContext(ctx, `INSERT INTO users (
		id, username, email, display_name, avatar_url, bio, website_url,
		role, password_hash, legacy_hash, status, preferred_locale, created_at, updated_at
	) VALUES ('u_adm', 'adm', 'adm@example.com', 'Admin', '', '', '', 'admin', 'x', '', 'active', 'en', ?, ?)`, now, now); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := store.ExecContext(ctx, `INSERT INTO articles (
		id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
		cover_url, status, published_at, allow_comment, password_ciphertext,
		audio_url, audio_mime, lang, translation_group, license_override, created_at, updated_at
	) VALUES ('art_salt', 'u_adm', '', 'Salted Post', 'salted-post', '', 'Sum', 'Hello cache!', '', 'published', ?, ?, '', '', '', 'en', '', '', ?, ?)`,
		now, true, now, now); err != nil {
		t.Fatalf("seed article: %v", err)
	}

	raw, err := svc.GetPublishedRawArticleBySlug(ctx, "salted-post")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/posts/salted-post", nil)

	// Register salt function: articleHTMLCache MUST stay enabled even with active render hooks!
	var currentSalt atomic.Value
	currentSalt.Store("digest-v1")
	svc.SetRenderCacheSaltFunc(func() string {
		return currentSalt.Load().(string)
	})

	hookCalls.Store(0)
	v1, _, _, _, err := svc.RenderArticleDetail(ctx, nil, req, raw, nil, "en", "https://blog.example.com")
	if err != nil || !strings.Contains(v1.HTML, "SALT_V1") {
		t.Fatalf("unexpected v1 HTML: %q (err=%v)", v1.HTML, err)
	}
	if got := hookCalls.Load(); got != 1 {
		t.Fatalf("expected 1 hook call on cold render, got %d", got)
	}

	// Second call with identical salt MUST hit articleHTMLCache (0 new hook calls!)
	v1Hit, _, _, _, err := svc.RenderArticleDetail(ctx, nil, req, raw, nil, "en", "https://blog.example.com")
	if err != nil || !strings.Contains(v1Hit.HTML, "SALT_V1") {
		t.Fatalf("unexpected v1Hit HTML: %q (err=%v)", v1Hit.HTML, err)
	}
	if got := hookCalls.Load(); got != 1 {
		t.Fatalf("expected articleHTMLCache hit with 0 extra hook calls, got %d", got)
	}

	// Updating salt digest MUST invalidate cache key and re-run render hook with new output!
	badge.Store("V2")
	currentSalt.Store("digest-v2")
	v2, _, _, _, err := svc.RenderArticleDetail(ctx, nil, req, raw, nil, "en", "https://blog.example.com")
	if err != nil || !strings.Contains(v2.HTML, "SALT_V2") {
		t.Fatalf("expected SALT_V2 after salt change, got %q (err=%v)", v2.HTML, err)
	}
	if got := hookCalls.Load(); got != 2 {
		t.Fatalf("expected hook call count = 2 after salt rotation, got %d", got)
	}
}

func TestFeedCacheSingleEviction_SitemapSharding_LLMsBounds_And_GitHubSVGVars(t *testing.T) {
	ctx, store, _, _, svc := setupContentTestEnv(t)
	now := time.Now().UnixMilli()

	// 1. Feed cache: inserting 25 entries evicts ONLY 1 entry (keeping 24 cached), and caches ~1.5 MiB doc safely.
	for i := 0; i < 25; i++ {
		key := fmt.Sprintf("test-feed-%02d", i)
		_, err := svc.getOrBuildCachedDoc(ctx, key, "text/plain", func(_ context.Context) ([]byte, error) {
			return []byte(fmt.Sprintf("payload-%02d", i)), nil
		})
		if err != nil {
			t.Fatalf("getOrBuildCachedDoc %d: %v", i, err)
		}
	}
	svc.caches.feedMu.RLock()
	remainingAfter25 := len(svc.caches.feedDocs)
	_, oldestStillPresent := svc.caches.feedDocs["test-feed-00"]
	_, secondStillPresent := svc.caches.feedDocs["test-feed-01"]
	svc.caches.feedMu.RUnlock()
	if remainingAfter25 != 24 || oldestStillPresent || !secondStillPresent {
		t.Fatalf("expected single-entry oldest eviction leaving 24 entries (00 evicted, 01 kept), got len=%d oldest=%v second=%v",
			remainingAfter25, oldestStillPresent, secondStillPresent)
	}

	largeBody := []byte(strings.Repeat("Markdown line for llms-full corpus.\n", 44000)) // ~1.58 MiB
	_, err := svc.getOrBuildCachedDoc(ctx, "llms-full-large", "text/plain", func(_ context.Context) ([]byte, error) {
		return largeBody, nil
	})
	if err != nil {
		t.Fatalf("large llms-full doc: %v", err)
	}
	svc.caches.feedMu.RLock()
	_, largeCached := svc.caches.feedDocs["llms-full-large"]
	svc.caches.feedMu.RUnlock()
	if !largeCached {
		t.Fatalf("expected ~1.58 MiB /llms-full.txt payload to be cached within 8 MiB ceiling")
	}

	// 2. GitHub Stats SVG CSS custom properties + prefers-color-scheme
	defaultSVG := string(svc.BuildGitHubStatsSVG("octocat"))
	if !strings.Contains(defaultSVG, "--gh-bg:") || !strings.Contains(defaultSVG, "var(--gh-bg)") || !strings.Contains(defaultSVG, "@media (prefers-color-scheme: dark)") {
		t.Fatalf("expected CSS custom properties + dark media query in default SVG:\n%s", defaultSVG)
	}
	emptyThemeSVG := string(svc.BuildGitHubStatsSVGWithTheme("octocat", ""))
	if !strings.Contains(emptyThemeSVG, "@media (prefers-color-scheme: dark)") {
		t.Fatalf("expected empty theme to use adaptive prefers-color-scheme:\n%s", emptyThemeSVG)
	}

	// 3. Seed 520 pages and 9,500 published articles (total = 10,020 > SitemapShardSize=10000) in one transaction!
	if _, err := store.ExecContext(ctx, `INSERT INTO users (
		id, username, email, display_name, avatar_url, bio, website_url,
		role, password_hash, legacy_hash, status, preferred_locale, created_at, updated_at
	) VALUES ('u_adm', 'adm', 'adm@example.com', 'Admin', '', '', '', 'admin', 'x', '', 'active', 'en', ?, ?)`, now, now); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	err = store.WithTx(ctx, func(tx *sqlx.Tx) error {
		const artBatch = 50
		for base := 0; base < 9500; base += artBatch {
			var vals []string
			args := make([]any, 0, artBatch*7)
			for j := 0; j < artBatch; j++ {
				i := base + j
				idStr := fmt.Sprintf("sa_%05d", i)
				slug := fmt.Sprintf("post-%05d", i)
				ts := now + int64(i)
				vals = append(vals, "(?, 'u_adm', '', ?, ?, '', '', 'body', '', 'published', ?, ?, '', '', '', 'en', '', '', ?, ?)")
				args = append(args, idStr, idStr, slug, ts, true, ts, ts)
			}
			q := `INSERT INTO articles (
				id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
				cover_url, status, published_at, allow_comment, password_ciphertext,
				audio_url, audio_mime, lang, translation_group, license_override, created_at, updated_at
			) VALUES ` + strings.Join(vals, ",")
			if _, e := tx.ExecContext(ctx, q, args...); e != nil {
				return e
			}
		}

		for i := 0; i < 520; i++ {
			idStr := fmt.Sprintf("sp_%04d", i)
			slug := fmt.Sprintf("page-%04d", i)
			if _, e := tx.ExecContext(ctx, `INSERT INTO pages (
				id, author_id, title, slug, template, content_md, is_published, allow_comment, show_toc, lang, translation_group, created_at, updated_at
			) VALUES (?, 'u_adm', ?, ?, 'default', 'Page markdown', ?, ?, ?, 'en', '', ?, ?)`,
				idStr, idStr, slug, true, true, false, now, now); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("bulk seed for sitemap sharding & llms limits: %v", err)
	}

	// Verify /llms.txt caps pages at 500 and /llms-full.txt caps pages at 100
	llmsIndex, err := svc.BuildLLMSTxt(ctx, "https://blog.example.com", false)
	if err != nil {
		t.Fatalf("BuildLLMSTxt(index): %v", err)
	}
	if got := strings.Count(string(llmsIndex), "https://blog.example.com/page-"); got != 500 {
		t.Fatalf("expected /llms.txt pages capped at LIMIT 500, got %d", got)
	}
	llmsFull, err := svc.BuildLLMSTxt(ctx, "https://blog.example.com", true)
	if err != nil {
		t.Fatalf("BuildLLMSTxt(full): %v", err)
	}
	if got := strings.Count(string(llmsFull), "URL: https://blog.example.com/page-"); got != 100 {
		t.Fatalf("expected /llms-full.txt pages capped at LIMIT 100, got %d", got)
	}

	// Verify Sitemap sharding (> 10,000 items -> <sitemapindex> on shard 0, <urlset> on shards 1 & 2)
	indexXMLBytes, err := svc.BuildSitemapXML(ctx, "https://blog.example.com")
	if err != nil {
		t.Fatalf("BuildSitemapXML: %v", err)
	}
	indexXML := string(indexXMLBytes)
	if !strings.Contains(indexXML, "<sitemapindex") ||
		!strings.Contains(indexXML, "<loc>https://blog.example.com/sitemap-1.xml</loc>") ||
		!strings.Contains(indexXML, "<loc>https://blog.example.com/sitemap-2.xml</loc>") ||
		strings.Contains(indexXML, "sitemap-3.xml") {
		t.Fatalf("expected <sitemapindex> with shards 1..2, got:\n%s", indexXML)
	}

	shard1Bytes, err := svc.BuildSitemapShardXML(ctx, "https://blog.example.com", 1)
	if err != nil {
		t.Fatalf("BuildSitemapShardXML(1): %v", err)
	}
	shard1 := string(shard1Bytes)
	// Shard 1 = 6 structural routes + 9500 articles + 500 pages = 10,006 <url> tags
	if !strings.Contains(shard1, "<urlset") || strings.Count(shard1, "<url>") != 10006 {
		t.Fatalf("expected shard 1 to contain 10006 <url> entries (6 structural + 10000 items), got %d", strings.Count(shard1, "<url>"))
	}

	shard2Bytes, err := svc.BuildSitemapShardXML(ctx, "https://blog.example.com", 2)
	if err != nil {
		t.Fatalf("BuildSitemapShardXML(2): %v", err)
	}
	shard2 := string(shard2Bytes)
	// Shard 2 = remaining 20 pages = 20 <url> tags
	if !strings.Contains(shard2, "<urlset") || strings.Count(shard2, "<url>") != 20 || !strings.Contains(shard2, "/page-0519</loc>") {
		t.Fatalf("expected shard 2 to contain remaining 20 page <url> entries, got %d", strings.Count(shard2, "<url>"))
	}
}

func TestCommentsFocusFound_And_ReplyHydrationCap500(t *testing.T) {
	ctx, store, _, _, svc := setupContentTestEnv(t)
	now := time.Now().UnixMilli()

	if _, err := store.ExecContext(ctx, `INSERT INTO users (
		id, username, email, display_name, avatar_url, bio, website_url,
		role, password_hash, legacy_hash, status, preferred_locale, created_at, updated_at
	) VALUES ('u_adm', 'adm', 'adm@example.com', 'Admin', '', '', '', 'admin', 'x', '', 'active', 'en', ?, ?)`, now, now); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := store.ExecContext(ctx, `INSERT INTO articles (
		id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
		cover_url, status, published_at, allow_comment, password_ciphertext,
		audio_url, audio_mime, lang, translation_group, license_override, created_at, updated_at
	) VALUES
		('art_empty', 'u_adm', '', 'Empty', 'empty', '', '', 'body', '', 'published', ?, ?, '', '', '', 'en', '', '', ?, ?),
		('art_mega', 'u_adm', '', 'Mega', 'mega', '', '', 'body', '', 'published', ?, ?, '', '', '', 'en', '', '', ?, ?)`,
		now, true, now, now, now, true, now, now); err != nil {
		t.Fatalf("seed articles: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/comments", nil)

	// 1. Empty thread with focusCommentID != "" returns FocusFound=false
	emptyRes, err := svc.ListTargetCommentsPage(ctx, req, nil, domain.TargetTypeArticle, "art_empty", 1, 20, "missing_id")
	if err != nil || emptyRes.FocusFound == nil || *emptyRes.FocusFound != false {
		t.Fatalf("expected FocusFound=false on empty target, got %+v err=%v", emptyRes, err)
	}

	// 2. Seed 1 approved root, 1 pending root, and 540 approved replies under root_1
	err = store.WithTx(ctx, func(tx *sqlx.Tx) error {
		if _, e := tx.ExecContext(ctx, `INSERT INTO comments (
			id, target_type, target_id, parent_id, reply_to_name, user_id, guest_id_hash, env_fp_hash,
			author_name, author_url, avatar_url, notify_email_ciphertext, notify_locale, client_ip, ip_masked, geo_region,
			content_md, status, is_deleted, created_at, updated_at
		) VALUES
			('root_1', 'article', 'art_mega', '', '', '', 'gh', 'eh', 'Alice', '', '', '', 'en', '', '', '', 'Root', 'approved', ?, ?, ?),
			('root_pending', 'article', 'art_mega', '', '', '', 'gh_other', 'eh', 'Bob', '', '', '', 'en', '', '', '', 'Pending', 'pending', ?, ?, ?)`,
			false, now, now, false, now-5000000, now-5000000); e != nil {
			return e
		}
		for i := 0; i < 540; i++ {
			ts := now + int64(i+1)
			if _, e := tx.ExecContext(ctx, `INSERT INTO comments (
				id, target_type, target_id, parent_id, reply_to_name, user_id, guest_id_hash, env_fp_hash,
				author_name, author_url, avatar_url, notify_email_ciphertext, notify_locale, client_ip, ip_masked, geo_region,
				content_md, status, is_deleted, created_at, updated_at
			) VALUES (?, 'article', 'art_mega', 'root_1', 'Alice', '', 'gh', 'eh', 'ReplyUser', '', '', '', 'en', '', '', '', 'Reply', 'approved', ?, ?, ?)`,
				fmt.Sprintf("rep_%04d", i), false, ts, ts); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed mega comments: %v", err)
	}

	// No focusCommentID -> FocusFound is nil AND replies capped at 500!
	noFocus, err := svc.ListTargetCommentsPage(ctx, req, nil, domain.TargetTypeArticle, "art_mega", 1, 20, "")
	if err != nil || noFocus.FocusFound != nil {
		t.Fatalf("expected FocusFound=nil when focusCommentID='', got %+v err=%v", noFocus.FocusFound, err)
	}
	if len(noFocus.Comments) != 1 || len(noFocus.Comments[0].Replies) != 500 {
		t.Fatalf("expected hydrateRootThreadsReplies capped at 500 replies, got %d", len(noFocus.Comments[0].Replies))
	}

	// Approved reply -> FocusFound == true
	hitFocus, err := svc.ListTargetCommentsPage(ctx, req, nil, domain.TargetTypeArticle, "art_mega", 1, 20, "rep_0010")
	if err != nil || hitFocus.FocusFound == nil || *hitFocus.FocusFound != true {
		t.Fatalf("expected FocusFound=true for approved reply rep_0010, got %+v err=%v", hitFocus.FocusFound, err)
	}

	// Unapproved pending comment -> FocusFound == false for anonymous visitor
	pendingFocus, err := svc.ListTargetCommentsPage(ctx, req, nil, domain.TargetTypeArticle, "art_mega", 1, 20, "root_pending")
	if err != nil || pendingFocus.FocusFound == nil || *pendingFocus.FocusFound != false {
		t.Fatalf("expected FocusFound=false for unapproved comment, got %+v err=%v", pendingFocus.FocusFound, err)
	}

	// Nonexistent comment -> FocusFound == false
	missingFocus, err := svc.ListTargetCommentsPage(ctx, req, nil, domain.TargetTypeArticle, "art_mega", 1, 20, "does_not_exist")
	if err != nil || missingFocus.FocusFound == nil || *missingFocus.FocusFound != false {
		t.Fatalf("expected FocusFound=false for missing comment, got %+v err=%v", missingFocus.FocusFound, err)
	}
}

func TestAnalyticsRefererGuard_CountryTopItems_And_Prune(t *testing.T) {
	ctx, store, _, _, svc := setupContentTestEnv(t)
	now := time.Now().UnixMilli()

	if _, err := store.ExecContext(ctx, `INSERT INTO users (
		id, username, email, display_name, avatar_url, bio, website_url,
		role, password_hash, legacy_hash, status, preferred_locale, created_at, updated_at
	) VALUES ('u_adm', 'adm', 'adm@example.com', 'Admin', '', '', '', 'admin', 'x', '', 'active', 'en', ?, ?)`, now, now); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := store.ExecContext(ctx, `INSERT INTO articles (
		id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
		cover_url, status, published_at, allow_comment, password_ciphertext,
		audio_url, audio_mime, lang, translation_group, license_override, created_at, updated_at
	) VALUES ('art_v1', 'u_adm', '', 'Post', 'post', '', '', 'body', '', 'published', ?, ?, '', '', '', 'en', '', '', ?, ?)`,
		now, true, now, now); err != nil {
		t.Fatalf("seed article: %v", err)
	}

	// 1. Record malformed & oversized (> 253 chars) referer hosts — all must be discarded without failing FlushAnalytics!
	for _, badRef := range []string{
		strings.Repeat("a", 254) + ".com",
		"https://evil.example.com/path?x=1",
		"-leading-dash.example.com",
		"double..dot.example.com",
		"bad space.example.com",
		strings.Repeat("label", 15) + ".com", // > 63-char label
	} {
		svc.RecordViewWithGeo(ctx, domain.TargetTypeArticle, "art_v1", "fp_1", badRef, "us")
	}

	// 2. Record 560 distinct valid referer hosts in one flush window — must cap at 512 distinct referer keys!
	for i := 0; i < 560; i++ {
		host := fmt.Sprintf("ref-%04d.example.org", i)
		country := "CN"
		if i%2 == 0 {
			country = "US"
		}
		svc.RecordViewWithGeo(ctx, domain.TargetTypeArticle, "art_v1", fmt.Sprintf("fp_%d", i), host, country)
	}

	if err := svc.FlushAnalytics(ctx); err != nil {
		t.Fatalf("FlushAnalytics failed: %v", err)
	}

	var refCount int
	if err := store.QueryRowContext(ctx, `SELECT COUNT(*) FROM analytics_top_items WHERE kind = 'referer'`).Scan(&refCount); err != nil {
		t.Fatalf("count referers: %v", err)
	}
	if refCount != 512 {
		t.Fatalf("expected distinct referer keys capped at 512 per flush window, got %d", refCount)
	}

	type topRow struct {
		ItemKey string `db:"item_key"`
		Hits    int64  `db:"hits"`
	}
	countries, err := sqlx.Select[topRow](ctx, store, `SELECT item_key, hits FROM analytics_top_items WHERE kind = 'country' ORDER BY item_key ASC`)
	if err != nil || len(countries) != 2 || countries[0].ItemKey != "CN" || countries[0].Hits != 280 || countries[1].ItemKey != "US" || countries[1].Hits != 286 {
		t.Fatalf("unexpected country top items: %+v err=%v", countries, err)
	}

	// 3. Seed stale (> 180d) referer, country, and article rows, then run PruneAnalyticsTopItems(ctx, 180).
	staleDate := time.Now().In(svc.siteLocation()).AddDate(0, 0, -200).Format("2006-01-02")
	recentDate := time.Now().In(svc.siteLocation()).AddDate(0, 0, -30).Format("2006-01-02")
	if _, err := store.ExecContext(ctx, `INSERT INTO analytics_top_items (stat_date, kind, item_key, hits) VALUES
		(?, 'referer', 'old.example.com', 5),
		(?, 'country', 'JP', 9),
		(?, 'article', 'art_v1', 12),
		(?, 'country', 'DE', 4)`,
		staleDate, staleDate, staleDate, recentDate); err != nil {
		t.Fatalf("seed stale analytics_top_items: %v", err)
	}

	if err := svc.PruneAnalyticsTopItems(ctx, 0); err != nil {
		t.Fatalf("PruneAnalyticsTopItems: %v", err)
	}

	var staleRefOrCountry, staleArticle, recentCountry int
	_ = store.QueryRowContext(ctx, `SELECT COUNT(*) FROM analytics_top_items WHERE stat_date = ? AND kind IN ('referer', 'country')`, staleDate).Scan(&staleRefOrCountry)
	_ = store.QueryRowContext(ctx, `SELECT COUNT(*) FROM analytics_top_items WHERE stat_date = ? AND kind = 'article'`, staleDate).Scan(&staleArticle)
	_ = store.QueryRowContext(ctx, `SELECT COUNT(*) FROM analytics_top_items WHERE stat_date = ? AND kind = 'country'`, recentDate).Scan(&recentCountry)
	if staleRefOrCountry != 0 || staleArticle != 1 || recentCountry != 1 {
		t.Fatalf("unexpected prune result: staleRefOrCountry=%d staleArticle=%d recentCountry=%d", staleRefOrCountry, staleArticle, recentCountry)
	}
}

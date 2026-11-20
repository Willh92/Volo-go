package content

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/domain"
)

func TestRenderArticleDetail_MetadataCacheHitInvalidationAndLockIsolation(t *testing.T) {
	ctx, store, _, _, svc := setupContentTestEnv(t)
	now := time.Now().UnixMilli()

	if _, err := store.ExecContext(ctx, `INSERT INTO users (
		id, username, email, display_name, avatar_url, bio, website_url,
		role, password_hash, legacy_hash, status, preferred_locale, created_at, updated_at
	) VALUES ('u_adm', 'adm', 'adm@example.com', 'Admin', '', '', '', 'admin', 'x', '', 'active', 'en', ?, ?)`, now, now); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := store.ExecContext(ctx, `INSERT INTO categories (id, name, slug, description, sort_order, created_at, updated_at)
		VALUES ('cat_1', 'Engineering', 'eng', '', 1, ?, ?)`, now, now); err != nil {
		t.Fatalf("seed category: %v", err)
	}
	if _, err := store.ExecContext(ctx, `INSERT INTO tags (id, name, slug, created_at) VALUES ('tag_1', 'Go', 'go', ?), ('tag_2', 'SQLite', 'sqlite', ?)`, now, now); err != nil {
		t.Fatalf("seed tags: %v", err)
	}

	// Prev, main, next, backlink source, translation alternate
	insertArt := func(id, title, slug, group, lang, contentMD, pwd string, hasLock bool, pubAt int64) {
		if _, err := store.ExecContext(ctx, `INSERT INTO articles (
			id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
			cover_url, status, published_at, allow_comment, password_ciphertext, has_content_lock,
			audio_url, audio_mime, lang, translation_group, license_override, word_count, reading_minutes, created_at, updated_at
		) VALUES (?, 'u_adm', 'cat_1', ?, ?, '', 'Summary', ?, '', 'published', ?, 1, ?, ?, '', '', ?, ?, '', 100, 1, ?, ?)`,
			id, title, slug, contentMD, pubAt, pwd, hasLock, lang, group, pubAt, pubAt); err != nil {
			t.Fatalf("seed article %s: %v", id, err)
		}
	}
	insertArt("art_prev", "Previous Post", "prev-post", "", "en", "prev", "", false, now-2000)
	insertArt("art_main", "Main Post", "main-post", "grp-main", "en", "# Hello\nBody", "", false, now)
	insertArt("art_next", "Next Post", "next-post", "", "en", "next", "", false, now+2000)
	insertArt("art_zh", "Main Post ZH", "main-post-zh", "grp-main", "zh-CN", "zh", "", false, now+500)
	insertArt("art_src", "Source Post", "src-post", "", "en", "See [[main-post]]", "", false, now-1000)

	if _, err := store.ExecContext(ctx, `INSERT INTO article_tags (article_id, tag_id) VALUES ('art_main', 'tag_1')`); err != nil {
		t.Fatalf("seed article_tags: %v", err)
	}
	if _, err := store.ExecContext(ctx, `INSERT INTO article_backlinks (source_article_id, target_article_id, updated_at) VALUES ('art_src', 'art_main', ?)`, now); err != nil {
		t.Fatalf("seed backlink: %v", err)
	}

	rawMain, err := svc.GetPublishedRawArticleBySlug(ctx, "main-post")
	if err != nil {
		t.Fatalf("get main: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "https://example.com/posts/main-post", nil)
	rec := httptest.NewRecorder()
	v1, p1, n1, bl1, err := svc.RenderArticleDetail(ctx, rec, req, rawMain, nil, "en", "https://example.com")
	if err != nil {
		t.Fatalf("cold RenderArticleDetail: %v", err)
	}
	if len(v1.Tags) != 1 || v1.Tags[0].Slug != "go" {
		t.Fatalf("expected 1 tag 'go', got %+v", v1.Tags)
	}
	if len(v1.Translations) != 1 || v1.Translations[0].Slug != "main-post-zh" {
		t.Fatalf("expected 1 translation alternate main-post-zh, got %+v", v1.Translations)
	}
	if p1 == nil || p1.Slug != "src-post" {
		t.Fatalf("expected prev neighbor src-post, got %+v", p1)
	}
	if n1 == nil || n1.Slug != "main-post-zh" {
		t.Fatalf("expected next neighbor main-post-zh, got %+v", n1)
	}
	if len(bl1) != 1 || bl1[0].Slug != "src-post" {
		t.Fatalf("expected 1 backlink src-post, got %+v", bl1)
	}

	// Verify entry is present in articleHTML cache with full metadata.
	snap := svc.settings.Snapshot()
	detailKey := svc.buildArticleHTMLCacheKey(rawMain, "en", "https://example.com", "Admin", snap.Reading.ShowCodeLineNumbers, snap.Site.EnableLicenseNotice, svc.renderCacheSalt())
	cachedEntry, ok := svc.caches.getArticleHTML(detailKey)
	if !ok || len(cachedEntry.tags) != 1 || len(cachedEntry.translations) != 1 || cachedEntry.prev == nil || cachedEntry.next == nil || len(cachedEntry.backlinks) != 1 {
		t.Fatalf("expected cachedArticleDetail with metadata, ok=%v entry=%+v", ok, cachedEntry)
	}

	// Warm hit: pending views should still increment, and returned slices must be isolated copies.
	svc.views.RecordView(domain.TargetTypeArticle, "art_main", "2026-11-19", 1, "")
	v2, p2, n2, bl2, err := svc.RenderArticleDetail(ctx, httptest.NewRecorder(), req, rawMain, nil, "en", "https://example.com")
	if err != nil {
		t.Fatalf("warm RenderArticleDetail: %v", err)
	}
	if v2.ViewCount != v1.ViewCount+1 || len(v2.Tags) != 1 || len(v2.Translations) != 1 || p2 == nil || n2 == nil || len(bl2) != 1 {
		t.Fatalf("warm cache hit mismatch: v2=%+v p2=%+v n2=%+v bl2=%+v", v2.ViewCount, p2, n2, bl2)
	}
	// Mutate returned slice to confirm cache isolation.
	v2.Tags[0].Slug = "mutated"
	v3, _, _, _, _ := svc.RenderArticleDetail(ctx, httptest.NewRecorder(), req, rawMain, nil, "en", "https://example.com")
	if v3.Tags[0].Slug != "go" {
		t.Fatalf("cache slice mutation leaked: %+v", v3.Tags)
	}

	// Mutating tags via Store.ExecContext bumps DataRevision -> immediate cache invalidation.
	if _, err := store.ExecContext(ctx, `INSERT INTO article_tags (article_id, tag_id) VALUES ('art_main', 'tag_2')`); err != nil {
		t.Fatalf("insert second tag: %v", err)
	}
	v4, _, _, _, err := svc.RenderArticleDetail(ctx, httptest.NewRecorder(), req, rawMain, nil, "en", "https://example.com")
	if err != nil {
		t.Fatalf("post-invalidation RenderArticleDetail: %v", err)
	}
	if len(v4.Tags) != 2 {
		t.Fatalf("expected 2 tags after DataRevision bump, got %+v", v4.Tags)
	}

	// Lock / password-protected articles must NEVER be cached in articleHTML cache.
	lockedCipher, err := svc.guard.SealArticlePassword("art_lock", "secret123")
	if err != nil {
		t.Fatalf("SealArticlePassword: %v", err)
	}
	insertArt("art_lock", "Locked Post", "locked-post", "", "en", ":::lock\nsecret body\n:::", lockedCipher, true, now+3000)
	rawLocked, err := svc.GetPublishedRawArticleBySlug(ctx, "locked-post")
	if err != nil {
		t.Fatalf("get locked: %v", err)
	}
	usedBefore := svc.caches.articleHTMLUsed
	vLock, _, _, _, err := svc.RenderArticleDetail(ctx, httptest.NewRecorder(), req, rawLocked, &domain.User{ID: "u_adm", Role: domain.RoleAdmin, Status: domain.UserStatusActive}, "en", "https://example.com")
	if err != nil {
		t.Fatalf("render locked: %v", err)
	}
	if !vLock.IsPasswordProtected && !vLock.HasContentLock {
		t.Fatalf("expected locked flags on locked article")
	}
	if svc.caches.articleHTMLUsed != usedBefore {
		t.Fatalf("locked article must not populate articleHTML cache (before=%d after=%d)", usedBefore, svc.caches.articleHTMLUsed)
	}
	_ = context.Background()
}

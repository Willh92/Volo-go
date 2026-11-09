package content

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/guest"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/render"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/internal/visibility"
)

func TestBuiltinSpamFilterToggleRelatedArticlesAndSargableArchiveFilter(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(tmp, "content-r4.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatal(err)
	}

	kr, _, err := config.LoadOrInitKeyRing("unit-test-content-r4-secret-2026", tmp)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := i18n.NewCatalog("en-US", "")
	if err != nil {
		t.Fatal(err)
	}
	settingsMgr := repo.NewSettingsManager(store, kr, "en-US")
	if _, err := settingsMgr.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	passports := guest.NewPassportManager(kr, "https://example.com")
	pow := guest.NewPoWVerifier(kr)
	rnd := render.Default()
	grd := visibility.NewGuard(kr, settingsMgr, passports, cat, rnd)
	srch := visibility.NewSearchIndex(store, rnd, 1<<20)

	svc := NewService(ServiceOptions{
		Store:     store,
		Keys:      kr,
		Settings:  settingsMgr,
		Passports: passports,
		PoW:       nil,
		Guard:     grd,
		Search:    srch,
		Renderer:  rnd,
		Catalog:   cat,
	})
	_ = pow

	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC).UnixMilli()
	prevMonth := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC).UnixMilli()

	// Seed author, category, tags, and 3 published articles (including one with published_at=0 using created_at fallback)
	if _, err := store.ExecContext(ctx, `INSERT INTO users (
		id, username, email, display_name, avatar_url, bio, website_url,
		role, password_hash, legacy_hash, status, preferred_locale, created_at, updated_at
	) VALUES ('u1','author1','a1@example.com','Author One','','','','admin','x','','active','en',?,?)`, now, now); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := store.ExecContext(ctx, `INSERT INTO categories (id, name, slug, description, sort_order, created_at, updated_at) VALUES ('c1','Systems','systems','',1,?,?)`, now, now); err != nil {
		t.Fatalf("seed category: %v", err)
	}
	if _, err := store.ExecContext(ctx, `INSERT INTO tags (id, name, slug, created_at) VALUES ('t1','Go','go',?), ('t2','SQLite','sqlite',?)`, now, now); err != nil {
		t.Fatalf("seed tags: %v", err)
	}

	if _, err := store.ExecContext(ctx, `INSERT INTO articles (
		id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
		cover_url, status, published_at, allow_comment, password_ciphertext,
		audio_url, audio_mime, lang, translation_group, license_override, word_count, reading_minutes, created_at, updated_at
	) VALUES
		('a1','u1','c1','First Post','first-post','','Summary 1','Body 1','','published',?,1,'','','','en','','',10,1,?,?),
		('a2','u1','c1','Second Post','second-post','','Summary 2','Body 2','','published',0,1,'','','','en','','',10,1,?,?),
		('a3','u1','c1','April Post','april-post','','Summary 3','Body 3','','published',?,1,'','','','en','','',10,1,?,?)`,
		now, now, now, now, now, prevMonth, prevMonth, prevMonth); err != nil {
		t.Fatalf("seed articles: %v", err)
	}
	// Share both tags between a1 and a2 to verify EXISTS avoids duplicate rows in ListRelatedArticles!
	if _, err := store.ExecContext(ctx, `INSERT INTO article_tags (article_id, tag_id) VALUES ('a1','t1'),('a1','t2'),('a2','t1'),('a2','t2')`); err != nil {
		t.Fatalf("seed article_tags: %v", err)
	}

	// 1. Sargable YearMonth archive filter matches both published_at > 0 (a1) and published_at == 0 with created_at in month (a2), excluding April (a3)
	cardsMay, totalMay, err := svc.ListPublicCards(ctx, ArticleListFilter{
		Page:      1,
		PageSize:  10,
		YearMonth: "2026-05",
		Locale:    "en-US",
	})
	if err != nil {
		t.Fatalf("ListPublicCards(2026-05) failed: %v", err)
	}
	if totalMay != 2 || len(cardsMay) != 2 {
		t.Fatalf("expected 2 May articles (published_at + created_at fallback), got total=%d len=%d", totalMay, len(cardsMay))
	}

	// 2. ListRelatedArticles deduplicates multi-tag overlap and fills category fallback cleanly
	rel := svc.ListRelatedArticles(ctx, domain.RawArticle{ID: "a1", CategoryID: "c1", Status: domain.ArticleStatusPublished}, "en-US", 5)
	if len(rel) != 2 || rel[0].ID != "a2" || rel[1].ID != "a3" {
		t.Fatalf("expected [a2 (tag match, deduplicated), a3 (category fallback)], got %+v", rel)
	}

	// 3. reading.builtin_spam_filter enforcement on BlockedKeywords
	if _, _, err := settingsMgr.PatchDomain(ctx, "reading", []byte(`{"comment_access_mode":"all","comment_moderation_mode":"smart","builtin_spam_filter":true,"blocked_keywords":["casino-spam"]}`), 0); err != nil {
		t.Fatal(err)
	}
	req1 := httptest.NewRequest("POST", "/api/v1/comments", nil)
	rr1 := httptest.NewRecorder()
	c1, _, err := svc.CreateComment(ctx, rr1, req1, nil, CreateCommentInput{
		TargetType:  domain.TargetTypeArticle,
		TargetID:    "a1",
		AuthorName:  "Visitor One",
		NotifyEmail: "v1@example.com",
		ContentMD:   "Check out this casino-spam link",
		ClientIP:    "203.0.113.10",
	})
	if err != nil {
		t.Fatalf("CreateComment with builtin_spam_filter=true failed: %v", err)
	}
	if c1.Status != domain.CommentStatusPending {
		t.Fatalf("expected comment matching blocked_keywords to be held pending when builtin_spam_filter=true, got %q", c1.Status)
	}

	// Turn builtin_spam_filter=false -> blocked_keywords check must be bypassed!
	if _, _, err := settingsMgr.PatchDomain(ctx, "reading", []byte(`{"builtin_spam_filter":false}`), 0); err != nil {
		t.Fatal(err)
	}
	req2 := httptest.NewRequest("POST", "/api/v1/comments", nil)
	rr2 := httptest.NewRecorder()
	c2, _, err := svc.CreateComment(ctx, rr2, req2, nil, CreateCommentInput{
		TargetType:  domain.TargetTypeArticle,
		TargetID:    "a1",
		AuthorName:  "Visitor Two",
		NotifyEmail: "v2@example.com",
		ContentMD:   "Discussing casino-spam false positives in filter benchmarks",
		ClientIP:    "203.0.113.11",
	})
	if err != nil {
		t.Fatalf("CreateComment with builtin_spam_filter=false failed: %v", err)
	}
	if c2.Status != domain.CommentStatusApproved {
		t.Fatalf("expected comment to be approved when builtin_spam_filter=false and smart moderation active, got %q", c2.Status)
	}

	// 4. Builtin nav item localization for ja-JP and zh-TW
	if got := inferBuiltinNavItemTitle("归档", "/archives", "ja-JP"); got != "アーカイブ" {
		t.Fatalf("expected Japanese builtin nav title 'アーカイブ', got %q", got)
	}
	if got := inferBuiltinNavItemTitle("友链", "/links", "zh-TW"); got != "友鏈" {
		t.Fatalf("expected Traditional Chinese builtin nav title '友鏈', got %q", got)
	}
}

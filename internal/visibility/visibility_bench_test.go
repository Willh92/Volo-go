package visibility_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

func newVisibilityBenchEnv(tb testing.TB) (*sqlx.Store, *visibility.Guard, *visibility.SearchIndex) {
	tb.Helper()
	ctx := context.Background()
	tmp := tb.TempDir()
	secret := "0123456789abcdef0123456789abcdef-vis-bench-secret"
	pubURL := "https://blog.example.com"
	cfg, err := config.Load(config.CLIOverrides{
		SecretKey: &secret,
		PublicURL: &pubURL,
		DataDir:   &tmp,
	})
	if err != nil {
		tb.Fatal(err)
	}
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(tmp, "vis.db"),
	})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = store.Close() })
	if err := migrate.Up(ctx, store); err != nil {
		tb.Fatal(err)
	}
	cat, err := i18n.NewCatalog("zh-CN", "")
	if err != nil {
		tb.Fatal(err)
	}
	settings := repo.NewSettingsManager(store, cfg.KeyRing, "zh-CN")
	_, _ = settings.Reload(ctx)
	passports := guest.NewPassportManager(cfg.KeyRing, pubURL)
	rnd := render.Default()
	grd := visibility.NewGuard(cfg.KeyRing, settings, passports, cat, rnd)
	srch := visibility.NewSearchIndex(store, rnd, visibility.DefaultSearchMaxBytes)
	return store, grd, srch
}

func TestRenderCacheCoherenceAndZeroLeak(t *testing.T) {
	ctx := context.Background()
	_, grd, _ := newVisibilityBenchEnv(t)

	raw := domain.RawArticle{
		ID:          "art-cache-1",
		Title:       "Cache Coherence Post",
		Slug:        "cache-coherence",
		ContentMD:   "First revision paragraph with **bold** text and image ![cover](/uploads/c1.webp).",
		Status:      domain.ArticleStatusPublished,
		PublishedAt: time.Now().UnixMilli(),
		UpdatedAt:   1000,
	}

	c1 := grd.ToCard(raw, "en-US", nil, nil, nil)
	if !strings.Contains(c1.Summary, "First revision") || c1.CoverURL != "/uploads/c1.webp" {
		t.Fatalf("unexpected initial card: %+v", c1)
	}

	// Mutating ContentMD MUST immediately invalidate cache via digest even if UpdatedAt were identical!
	rawUpdated := raw
	rawUpdated.ContentMD = "Second revision completely updated summary ![cover](/uploads/c2.webp)."
	c2 := grd.ToCard(rawUpdated, "en-US", nil, nil, nil)
	if !strings.Contains(c2.Summary, "Second revision") || c2.CoverURL != "/uploads/c2.webp" {
		t.Fatalf("stale render cache returned old card: %+v", c2)
	}

	// Password-protected posts MUST NEVER leak plaintext or cache unlocked outputs for anonymous callers!
	cipherPwd, err := grd.SealArticlePassword("art-secret-1", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	locked := domain.RawArticle{
		ID:                 "art-secret-1",
		Title:              "Top Secret Entry",
		Slug:               "top-secret",
		ContentMD:          "SUPER_SENSITIVE_BODY_9912",
		PasswordCiphertext: cipherPwd,
		Status:             domain.ArticleStatusPublished,
		PublishedAt:        time.Now().UnixMilli(),
		UpdatedAt:          2000,
	}
	shareTok, _, err := grd.IssueShareToken(ctx, locked, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Authorized ShareToken preview sees plaintext
	reqPreview := httptest.NewRequest("GET", "/posts/top-secret?preview="+shareTok, nil)
	recPreview := httptest.NewRecorder()
	vUnlocked := grd.ToDetail(recPreview, reqPreview, locked, nil, visibility.DetailRenderDeps{Locale: "en-US"})
	if !strings.Contains(vUnlocked.HTML, "SUPER_SENSITIVE_BODY_9912") {
		t.Fatalf("expected ShareToken preview to render unlocked body")
	}

	// 2. Subsequent anonymous request MUST NOT hit any cached unlocked HTML!
	reqAnon := httptest.NewRequest("GET", "/posts/top-secret", nil)
	recAnon := httptest.NewRecorder()
	vAnon := grd.ToDetail(recAnon, reqAnon, locked, nil, visibility.DetailRenderDeps{Locale: "en-US"})
	if strings.Contains(vAnon.HTML, "SUPER_SENSITIVE_BODY_9912") {
		t.Fatalf("CRITICAL: render cache leaked unlocked ShareToken HTML to anonymous viewer!")
	}
}

func BenchmarkGuardToCardWarmCache(b *testing.B) {
	_, grd, _ := newVisibilityBenchEnv(b)
	raw := domain.RawArticle{
		ID:          "bench-card-1",
		Title:       "High Throughput Go Blog Architecture",
		Slug:        "high-throughput-go-blog",
		ContentMD:   strings.Repeat("Pure Go single binary with SQLite WAL and bounded caches.\n\n```go\nx := 42\n```\n\n", 8),
		Status:      domain.ArticleStatusPublished,
		PublishedAt: 1700000000000,
		UpdatedAt:   1700000005000,
	}
	// Prime cache once
	_ = grd.ToCard(raw, "en-US", nil, nil, nil)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		card := grd.ToCard(raw, "en-US", nil, nil, nil)
		if card.Summary == "" {
			b.Fatal("empty summary")
		}
	}
}

func BenchmarkGuardToDetailWarmCache(b *testing.B) {
	_, grd, _ := newVisibilityBenchEnv(b)
	raw := domain.RawArticle{
		ID:          "bench-detail-1",
		Title:       "Zero-Copy HTTP & Render Caching",
		Slug:        "zero-copy-http",
		ContentMD:   strings.Repeat("## Section\n\nDetailed benchmark paragraph with `inline code` and lists.\n\n", 10),
		Status:      domain.ArticleStatusPublished,
		PublishedAt: 1700000000000,
		UpdatedAt:   1700000005000,
	}
	req := httptest.NewRequest("GET", "/posts/zero-copy-http", nil)
	deps := visibility.DetailRenderDeps{
		Locale:        "en-US",
		AuthorName:    "Volo",
		SiteTitle:     "Volo Studio",
		PublicBaseURL: "https://blog.example.com",
	}
	_ = grd.ToDetail(nil, req, raw, nil, deps)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		v := grd.ToDetail(nil, req, raw, nil, deps)
		if v.HTML == "" {
			b.Fatal("empty HTML")
		}
	}
}

func BenchmarkSearchIndexQuery(b *testing.B) {
	ctx := context.Background()
	store, _, srch := newVisibilityBenchEnv(b)
	now := time.Now().UnixMilli()
	for i := 0; i < 60; i++ {
		_, err := store.ExecContext(ctx,
			`INSERT INTO articles (
				id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
				cover_url, status, is_pinned, allow_comment, password_ciphertext, has_content_lock,
				audio_url, audio_duration_sec, audio_size_bytes, audio_mime,
				lang, translation_group, license_override, word_count, reading_minutes, view_count, comment_count,
				published_at, created_at, updated_at
			) VALUES (?, 'u1', '', ?, ?, '', '', ?, '', 'published', 0, 1, '', 0, '', 0, 0, '', 'en-US', '', '', 100, 1, 0, 0, ?, ?, ?)`,
			fmt.Sprintf("s-%d", i),
			fmt.Sprintf("Distributed Systems Note %d", i),
			fmt.Sprintf("dist-note-%d", i),
			strings.Repeat("Consensus, Raft log replication, SQLite WAL snapshots, and deterministic latency tail bounds. ", 12),
			now, now, now,
		)
		if err != nil {
			b.Fatal(err)
		}
	}
	if err := srch.Rebuild(ctx); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hits, err := srch.Search(ctx, "raft sqlite latency", 10)
		if err != nil || len(hits) == 0 {
			b.Fatalf("unexpected search result: hits=%d err=%v", len(hits), err)
		}
	}
}

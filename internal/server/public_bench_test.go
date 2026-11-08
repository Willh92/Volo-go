package server_test

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/internal/server"
)

func setupPerfServerEnv(tb testing.TB) (*sqlx.Store, *server.AppServer) {
	tb.Helper()
	ctx := context.Background()
	tmp := tb.TempDir()

	dataDir := filepath.Join(tmp, "data")
	uploadDir := filepath.Join(tmp, "uploads")
	pluginsDir := filepath.Join(tmp, "plugins")
	themesDir := filepath.Join(tmp, "themes")
	for _, d := range []string{dataDir, uploadDir, pluginsDir, themesDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			tb.Fatal(err)
		}
	}

	secret := "0123456789abcdef0123456789abcdef-perf-master-secret"
	pubURL := "https://blog.example.com"
	cfg, err := config.Load(config.CLIOverrides{
		SecretKey:  &secret,
		PublicURL:  &pubURL,
		DataDir:    &dataDir,
		UploadDir:  &uploadDir,
		PluginsDir: &pluginsDir,
		ThemesDir:  &themesDir,
	})
	if err != nil {
		tb.Fatal(err)
	}
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(dataDir, "perf.db"),
	})
	if err != nil {
		tb.Fatal(err)
	}
	if err := migrate.Up(ctx, store); err != nil {
		tb.Fatal(err)
	}

	now := time.Now().UnixMilli()
	_, err = store.ExecContext(ctx,
		`INSERT INTO users (id, username, email, display_name, avatar_url, bio, website_url, role, password_hash, legacy_hash, status, email_verified_at, preferred_locale, created_at, updated_at)
		 VALUES ('u-admin', 'admin', 'admin@example.com', 'Volo Author', '', '', '', 'admin', 'unused', '', 'active', ?, 'en-US', ?, ?)`,
		now, now, now,
	)
	if err != nil {
		tb.Fatal(err)
	}

	settings := repo.NewSettingsManager(store, cfg.KeyRing, "en-US")
	_, _ = settings.Reload(ctx)
	_, _, _ = settings.PatchDomain(ctx, domain.SettingsDomainSite, []byte(`{"initialized":true,"title":"Volo Performance Blog"}`), 0)
	if _, isBench := tb.(*testing.B); isBench {
		_, _, _ = settings.PatchDomain(ctx, domain.SettingsDomainSecurity, []byte(`{"waf_enabled":false}`), 0)
	}

	for i := 1; i <= 12; i++ {
		_, err := store.ExecContext(ctx,
			`INSERT INTO articles (
				id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
				cover_url, status, is_pinned, allow_comment, password_ciphertext, has_content_lock,
				audio_url, audio_duration_sec, audio_size_bytes, audio_mime,
				lang, translation_group, license_override, word_count, reading_minutes, view_count, comment_count,
				published_at, created_at, updated_at
			) VALUES (?, 'u-admin', '', ?, ?, '', '', ?, '', 'published', 0, 1, '', 0, '', 0, 0, '', 'en-US', '', '', 180, 1, 0, 0, ?, ?, ?)`,
			fmt.Sprintf("art-%02d", i),
			fmt.Sprintf("High-Performance Go Publishing Part %d", i),
			fmt.Sprintf("perf-post-%d", i),
			strings.Repeat("Zero-CGO single binary blog architecture with SQLite WAL, ETag negotiation, and write-behind view batching.\n\n", 18),
			now-int64(i*60000), now, now,
		)
		if err != nil {
			tb.Fatal(err)
		}
	}

	app, err := server.NewAppServer(server.AppDeps{
		Config:   cfg,
		Store:    store,
		Settings: settings,
	})
	if err != nil {
		tb.Fatal(err)
	}
	_ = app.Deps().Content.WarmCaches(ctx)
	tb.Cleanup(func() {
		_ = app.Close(ctx)
		_ = store.Close()
	})
	return store, app
}

func TestHTTPPerfBREACHSafeGzipETagAndViewBufferCoherence(t *testing.T) {
	ctx := context.Background()
	store, app := setupPerfServerEnv(t)
	h := app.Handler()

	// 1. Anonymous GET `/` with `Accept-Encoding: gzip` receives compressed HTML + `Vary: Accept-Encoding`
	reqHome := httptest.NewRequest(http.MethodGet, "/", nil)
	reqHome.Header.Set("Accept-Encoding", "gzip")
	recHome := httptest.NewRecorder()
	h.ServeHTTP(recHome, reqHome)
	if recHome.Code != http.StatusOK {
		t.Fatalf("GET / status=%d", recHome.Code)
	}
	if got := recHome.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("expected anonymous GET / to use Content-Encoding=gzip, got %q", got)
	}
	if !strings.Contains(recHome.Header().Get("Vary"), "Accept-Encoding") {
		t.Fatalf("expected Vary: Accept-Encoding on compressed response, got %q", recHome.Header().Get("Vary"))
	}
	zr, err := gzip.NewReader(recHome.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	unzippedHome, err := io.ReadAll(zr)
	_ = zr.Close()
	if err != nil || !strings.Contains(string(unzippedHome), "High-Performance Go Publishing") {
		t.Fatalf("unexpected unzipped home HTML (len=%d, err=%v)", len(unzippedHome), err)
	}

	// 2. BREACH immunity: requests carrying session cookie, unlock cookie, Authorization, or `?preview=` MUST NOT be compressed!
	sensitiveReqs := []*http.Request{
		func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("Accept-Encoding", "gzip")
			r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "some-session"})
			return r
		}(),
		func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/posts/perf-post-1", nil)
			r.Header.Set("Accept-Encoding", "gzip")
			r.AddCookie(&http.Cookie{Name: guest.UnlockCookieName, Value: "some-unlock"})
			return r
		}(),
		func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/posts/perf-post-1?preview=123.sig", nil)
			r.Header.Set("Accept-Encoding", "gzip")
			return r
		}(),
		func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("Accept-Encoding", "gzip")
			r.Header.Set("Authorization", "Bearer volo_pat_example")
			return r
		}(),
	}
	for idx, sr := range sensitiveReqs {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, sr)
		if enc := rec.Header().Get("Content-Encoding"); enc != "" {
			t.Fatalf("CRITICAL BREACH regression on case #%d (%s): expected uncompressed response, got Content-Encoding=%q", idx, sr.URL.String(), enc)
		}
	}

	// 3. Pre-compressed RSS + strong ETag `304 Not Modified` + automatic invalidation on write
	reqRSS := httptest.NewRequest(http.MethodGet, "/rss.xml", nil)
	reqRSS.Header.Set("Accept-Encoding", "gzip")
	recRSS := httptest.NewRecorder()
	h.ServeHTTP(recRSS, reqRSS)
	if recRSS.Code != http.StatusOK || recRSS.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("expected 200 gzipped /rss.xml, got code=%d enc=%q", recRSS.Code, recRSS.Header().Get("Content-Encoding"))
	}
	etag1 := recRSS.Header().Get("ETag")
	if etag1 == "" {
		t.Fatalf("expected strong ETag on /rss.xml")
	}

	reqRSS304 := httptest.NewRequest(http.MethodGet, "/rss.xml", nil)
	reqRSS304.Header.Set("If-None-Match", etag1)
	recRSS304 := httptest.NewRecorder()
	h.ServeHTTP(recRSS304, reqRSS304)
	if recRSS304.Code != http.StatusNotModified || recRSS304.Body.Len() != 0 {
		t.Fatalf("expected 304 Not Modified with empty body on matching If-None-Match, got code=%d bytes=%d", recRSS304.Code, recRSS304.Body.Len())
	}

	// Inserting a new published article MUST automatically invalidate cached RSS & produce a fresh ETag!
	now := time.Now().UnixMilli()
	if _, err := store.ExecContext(ctx,
		`INSERT INTO articles (
			id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
			cover_url, status, is_pinned, allow_comment, password_ciphertext, has_content_lock,
			audio_url, audio_duration_sec, audio_size_bytes, audio_mime,
			lang, translation_group, license_override, word_count, reading_minutes, view_count, comment_count,
			published_at, created_at, updated_at
		) VALUES ('art-fresh', 'u-admin', '', 'Brand New Release Post', 'brand-new-release', '', '', 'Fresh body',
		          '', 'published', 0, 1, '', 0, '', 0, 0, '', 'en-US', '', '', 10, 1, 0, 0, ?, ?, ?)`,
		now+1000, now+1000, now+1000,
	); err != nil {
		t.Fatal(err)
	}
	reqRSSAfter := httptest.NewRequest(http.MethodGet, "/rss.xml", nil)
	reqRSSAfter.Header.Set("If-None-Match", etag1)
	recRSSAfter := httptest.NewRecorder()
	h.ServeHTTP(recRSSAfter, reqRSSAfter)
	if recRSSAfter.Code != http.StatusOK || recRSSAfter.Header().Get("ETag") == etag1 {
		t.Fatalf("expected stale ETag to miss after article write, got code=%d etag=%q", recRSSAfter.Code, recRSSAfter.Header().Get("ETag"))
	}
	if !strings.Contains(recRSSAfter.Body.String(), "Brand New Release Post") {
		t.Fatalf("expected updated RSS XML to contain newly inserted article")
	}

	// 4. Write-behind view buffer: real-time pending read merge + deterministic FlushAnalytics / Close persistence
	for i := 0; i < 7; i++ {
		reqArt := httptest.NewRequest(http.MethodGet, "/posts/perf-post-2", nil)
		reqArt.RemoteAddr = fmt.Sprintf("198.51.100.%d:40000", i+1)
		recArt := httptest.NewRecorder()
		h.ServeHTTP(recArt, reqArt)
		if recArt.Code != http.StatusOK {
			t.Fatalf("GET /posts/perf-post-2 status=%d", recArt.Code)
		}
	}

	// Even BEFORE flushing to SQLite, ListPublicCards must reflect pending views in real time!
	cards, _, err := app.Deps().Content.ListPublicCards(ctx, serverArticleFilterPage1())
	if err != nil {
		t.Fatal(err)
	}
	var foundViews int64
	for _, c := range cards {
		if c.Slug == "perf-post-2" {
			foundViews = c.ViewCount
			break
		}
	}
	if foundViews != 7 {
		t.Fatalf("expected real-time PendingArticleViews=7 before flush, got %d", foundViews)
	}

	// Deterministic FlushAnalytics persists exact deltas to SQLite `articles` and `analytics_daily`!
	if err := app.Deps().Content.FlushAnalytics(ctx); err != nil {
		t.Fatalf("FlushAnalytics: %v", err)
	}
	gotArt, err := sqlx.Get[domain.RawArticle](ctx, store, `SELECT view_count FROM articles WHERE id = 'art-02'`)
	if err != nil || gotArt.ViewCount != 7 {
		t.Fatalf("expected persisted articles.view_count=7 after FlushAnalytics, got %d (err=%v)", gotArt.ViewCount, err)
	}
}

func serverArticleFilterPage1() content.ArticleListFilter {
	return content.ArticleListFilter{
		Page:     1,
		PageSize: 50,
		Locale:   "en-US",
	}
}

func BenchmarkPublicHomeGzip(b *testing.B) {
	_, app := setupPerfServerEnv(b)
	h := app.Handler()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		req.RemoteAddr = "203.0.113.10:12345"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("status=%d", rec.Code)
		}
	}
}

func BenchmarkPublicArticleConcurrentViews(b *testing.B) {
	_, app := setupPerfServerEnv(b)
	h := app.Handler()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req := httptest.NewRequest(http.MethodGet, "/posts/perf-post-1", nil)
			req.Header.Set("Accept-Encoding", "gzip")
			req.RemoteAddr = "203.0.113.25:23456"
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				b.Fatalf("status=%d", rec.Code)
			}
		}
	})
}

func BenchmarkCachedRSSWithETag(b *testing.B) {
	_, app := setupPerfServerEnv(b)
	h := app.Handler()

	warmReq := httptest.NewRequest(http.MethodGet, "/rss.xml", nil)
	warmReq.Header.Set("Accept-Encoding", "gzip")
	warmRec := httptest.NewRecorder()
	h.ServeHTTP(warmRec, warmReq)
	etag := warmRec.Header().Get("ETag")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodGet, "/rss.xml", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		if i%2 == 1 {
			req.Header.Set("If-None-Match", etag)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK && rec.Code != http.StatusNotModified {
			b.Fatalf("status=%d", rec.Code)
		}
	}
}

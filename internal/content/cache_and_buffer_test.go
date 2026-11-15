package content

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/repo"
)

func openContentTestService(t *testing.T) (context.Context, *sqlx.Store, *Service) {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(tmp, "content.db"),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	kr, _, err := config.LoadOrInitKeyRing(tmp, "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY")
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	settingsMgr := repo.NewSettingsManager(store, kr, "en-US")
	_, _ = settingsMgr.Reload(ctx)
	svc := NewService(ServiceOptions{
		Store:    store,
		Keys:     kr,
		Settings: settingsMgr,
	})
	t.Cleanup(func() { _ = svc.Close(context.Background()) })
	return ctx, store, svc
}

func TestViewAnalyticsBuffer_EpochIsolationOnReset(t *testing.T) {
	ctx, store, svc := openContentTestService(t)
	now := time.Now().UnixMilli()
	_, err := store.ExecContext(ctx, `INSERT INTO articles
		(id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md, cover_url, status, published_at, scheduled_at, is_pinned, allow_comment, password_ciphertext, has_content_lock, audio_url, audio_duration_sec, audio_size_bytes, audio_mime, lang, translation_group, license_override, word_count, reading_minutes, view_count, comment_count, created_at, updated_at)
		VALUES ('art_ep', 'u1', '', 'Epoch Post', 'epoch-post', '', '', 'body', '', 'published', ?, 0, 0, 1, '', 0, '', 0, 0, '', 'en-US', '', '', 1, 1, 10, 0, ?, ?)`,
		now, now, now)
	if err != nil {
		t.Fatalf("insert article: %v", err)
	}

	// Simulate pre-restore buffered views followed by Reset() (DB restore) when a concurrent Flush() races
	svc.views.RecordViewWithGeo(domain.TargetTypeArticle, "art_ep", "2026-11-15", 1, "example.com", "US")

	// Hold a SQLite write lock briefly so Flush() is paused waiting to enter WithBgTx while Reset() fires!
	txDone := make(chan struct{})
	txStarted := make(chan struct{})
	go func() {
		_ = store.WithTx(ctx, func(tx *sqlx.Tx) error {
			close(txStarted)
			<-txDone
			return nil
		})
	}()
	<-txStarted

	flushDone := make(chan error, 1)
	go func() {
		flushDone <- svc.views.Flush(ctx)
	}()

	time.Sleep(30 * time.Millisecond)
	svc.views.Reset()
	close(txDone)

	if fErr := <-flushDone; fErr != nil {
		t.Fatalf("Flush after Reset returned unexpected error: %v", fErr)
	}

	var views int64
	if err := store.QueryRowContext(ctx, `SELECT view_count FROM articles WHERE id = 'art_ep'`).Scan(&views); err != nil {
		t.Fatalf("query view_count: %v", err)
	}
	if views != 10 {
		t.Fatalf("pre-restore view delta leaked across Reset()! expected view_count=10, got %d", views)
	}
	if pending := svc.views.PendingArticleViews("art_ep"); pending != 0 {
		t.Fatalf("expected 0 pending views after aborted epoch flush, got %d", pending)
	}
}

func TestCachedDoc_RenderSaltInvalidation_StampedeProtection_AndMediaDimsCap(t *testing.T) {
	ctx, store, svc := openContentTestService(t)

	var currentSalt atomic.Value
	currentSalt.Store("salt-v1")
	svc.SetRenderCacheSaltFunc(func() string {
		return currentSalt.Load().(string)
	})

	var builds atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			doc, err := svc.getOrBuildCachedDoc(ctx, "rss|test", "application/rss+xml", func(context.Context) ([]byte, error) {
				builds.Add(1)
				time.Sleep(25 * time.Millisecond)
				return []byte("<rss>" + currentSalt.Load().(string) + "</rss>"), nil
			})
			if err != nil {
				t.Errorf("getOrBuildCachedDoc error: %v", err)
			}
			if string(doc.Raw) != "<rss>salt-v1</rss>" {
				t.Errorf("unexpected doc body: %q", string(doc.Raw))
			}
		}()
	}
	wg.Wait()
	if got := builds.Load(); got != 1 {
		t.Fatalf("expected stampede mutex to coalesce 16 cold requests into 1 build, got %d", got)
	}

	doc1, _ := svc.getOrBuildCachedDoc(ctx, "rss|test", "application/rss+xml", func(context.Context) ([]byte, error) {
		return []byte("<rss>salt-v1</rss>"), nil
	})

	// Changing renderCacheSalt MUST invalidate cached feed document AND produce a new ETag!
	currentSalt.Store("salt-v2")
	doc2, err := svc.getOrBuildCachedDoc(ctx, "rss|test", "application/rss+xml", func(context.Context) ([]byte, error) {
		builds.Add(1)
		return []byte("<rss>salt-v2</rss>"), nil
	})
	if err != nil {
		t.Fatalf("getOrBuildCachedDoc after salt change: %v", err)
	}
	if string(doc2.Raw) != "<rss>salt-v2</rss>" || doc2.ETag == doc1.ETag {
		t.Fatalf("expected salt change to rebuild doc and rotate ETag: doc1=%q/%s doc2=%q/%s", doc1.Raw, doc1.ETag, doc2.Raw, doc2.ETag)
	}

	// PutMediaDims must cap resident map size at maxCachedMediaDims!
	for i := 0; i < maxCachedMediaDims+250; i++ {
		svc.PutMediaDims(fmt.Sprintf("/uploads/img-%d.webp", i), 800, 600)
	}
	svc.mediaMu.RLock()
	gotLen := len(svc.mediaDims)
	svc.mediaMu.RUnlock()
	if gotLen > maxCachedMediaDims {
		t.Fatalf("s.mediaDims exceeded maxCachedMediaDims (%d): got %d", maxCachedMediaDims, gotLen)
	}

	// Verify LIKE ESCAPE '!' in ListTagsWithCountsPage, ListSeriesPage, and ListBooksPage!
	now := time.Now().UnixMilli()
	_, _ = store.ExecContext(ctx, `INSERT INTO tags (id, name, slug, created_at) VALUES ('t1', '100% Go_1!', 'go-1', ?), ('t2', '1000 GoX1X', 'go-2', ?)`, now, now)
	tags, totalTags, err := svc.ListTagsWithCountsPage(ctx, "100% Go_1!", 1, 20)
	if err != nil || totalTags != 1 || len(tags) != 1 || tags[0].ID != "t1" {
		t.Fatalf("ListTagsWithCountsPage LIKE escape failed: total=%d tags=%+v err=%v", totalTags, tags, err)
	}

	_, _ = store.ExecContext(ctx, `INSERT INTO series (id, title, slug, description, cover_url, sort_order, created_at, updated_at) VALUES ('s1', 'C++_20%!', 'cpp-20', '', '', 1, ?, ?), ('s2', 'C++X20XX', 'cpp-x', '', '', 2, ?, ?)`, now, now, now, now)
	series, totalSer, err := svc.ListSeriesPage(ctx, "C++_20%!", 1, 20)
	if err != nil || totalSer != 1 || len(series) != 1 || series[0].ID != "s1" {
		t.Fatalf("ListSeriesPage LIKE escape failed: total=%d series=%+v err=%v", totalSer, series, err)
	}

	if _, err := store.ExecContext(ctx, `INSERT INTO books (id, title, slug, description, cover_url, source_type, source_url, series_id, embed_mode, reading_status, chapter_count, synced_at, sort_order, created_at, updated_at) VALUES ('b1', 'Rust_100%!', 'rust-100', '', '', 'manual', '', '', 'link', '', 0, 0, 1, ?, ?), ('b2', 'RustX100XX', 'rust-x', '', '', 'manual', '', '', 'link', '', 0, 0, 2, ?, ?)`, now, now, now, now); err != nil {
		t.Fatalf("insert books: %v", err)
	}
	books, totalBooks, err := svc.ListBooksPage(ctx, "Rust_100%!", 1, 20)
	if err != nil || totalBooks != 1 || len(books) != 1 || books[0].ID != "b1" {
		t.Fatalf("ListBooksPage LIKE escape failed: total=%d books=%+v err=%v", totalBooks, books, err)
	}
}

package visibility

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/render"
)

func TestSearchIndex_SQLOverflowEscapesLikeWildcards(t *testing.T) {
	ctx := context.Background()
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(t.TempDir(), "search.db"),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	now := time.Now().UnixMilli()
	// Insert two articles; force SearchIndex RAM cap tiny (1 byte) so Stage 2 SQL overflow executes!
	_, err = store.ExecContext(ctx, `INSERT INTO articles
		(id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md, cover_url, status, published_at, scheduled_at, is_pinned, allow_comment, password_ciphertext, has_content_lock, audio_url, audio_duration_sec, audio_size_bytes, audio_mime, lang, translation_group, license_override, word_count, reading_minutes, view_count, comment_count, created_at, updated_at)
		VALUES
		('art_literal', 'u1', '', '100% Pure_Go!', 'pure-go', '', '', 'Achieved 100% coverage in init_v2! module.', '', 'published', ?, 0, 0, 1, '', 0, '', 0, 0, '', 'en-US', '', '', 10, 1, 0, 0, ?, ?),
		('art_decoy',   'u1', '', '1000 PureXGoX', 'decoy',   '', '', 'Achieved 100X coverage in initXv2X module.', '', 'published', ?, 0, 0, 1, '', 0, '', 0, 0, '', 'en-US', '', '', 10, 1, 0, 0, ?, ?)`,
		now, now, now, now-1000, now-1000, now-1000)
	if err != nil {
		t.Fatalf("insert articles: %v", err)
	}

	idx := NewSearchIndex(store, render.Default(), 1)
	if err := idx.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	_, _, complete := idx.Stats()
	if complete {
		t.Fatal("expected complete=false so Stage 2 SQL fallback is exercised")
	}

	hits, err := idx.Search(ctx, "100% init_v2!", 10)
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "art_literal" {
		t.Fatalf("expected only art_literal to match escaped LIKE query, got %+v", hits)
	}
}

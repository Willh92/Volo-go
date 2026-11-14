package migrate

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Willh92/Volo-go/internal/db/dialect"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
)

func TestAllThreeDialectsDDLRebindUpsertAndPagination(t *testing.T) {
	for _, drv := range []string{"sqlite", "postgres", "mysql"} {
		d, err := dialect.For(drv)
		if err != nil {
			t.Fatalf("dialect.For(%q) failed: %v", drv, err)
		}
		stmts := buildV1Schema(d)
		if len(stmts) < 25 {
			t.Fatalf("expected full DDL statements for %s, got %d", drv, len(stmts))
		}
		for _, s := range stmts {
			if drv == "mysql" && strings.HasPrefix(s, "CREATE TABLE") && !strings.Contains(s, "utf8mb4") {
				t.Fatalf("mysql table missing utf8mb4 suffix: %s", s)
			}
			if drv == "postgres" && strings.Contains(s, "LONGTEXT") {
				t.Fatalf("postgres DDL must not contain LONGTEXT: %s", s)
			}
		}

		// Verify UpsertSQL generation
		up := d.UpsertSQL("settings", []string{"domain", "value_json", "updated_at"}, []string{"domain"}, []string{"value_json", "updated_at"})
		switch drv {
		case "sqlite":
			if !strings.Contains(up, "ON CONFLICT (\"domain\") DO UPDATE SET") || strings.Contains(up, "$1") {
				t.Fatalf("unexpected sqlite upsert: %s", up)
			}
		case "postgres":
			if !strings.Contains(up, "ON CONFLICT (\"domain\") DO UPDATE SET") || !strings.Contains(up, "$1, $2, $3") {
				t.Fatalf("unexpected postgres upsert: %s", up)
			}
		case "mysql":
			if !strings.Contains(up, "ON DUPLICATE KEY UPDATE `value_json` = VALUES(`value_json`)") {
				t.Fatalf("unexpected mysql upsert: %s", up)
			}
		}

		// Verify PaginateSQL & Rebind literal safety
		pq, args := d.PaginateSQL("SELECT id, title FROM articles WHERE status = ? AND title <> 'what?'", 15, 30)
		if len(args) != 2 || args[0] != 15 || args[1] != 30 {
			t.Fatalf("unexpected pagination args: %v", args)
		}
		if drv == "postgres" {
			expected := "SELECT id, title FROM articles WHERE status = $1 AND title <> 'what?' LIMIT $2 OFFSET $3"
			if pq != expected {
				t.Fatalf("postgres PaginateSQL/Rebind mismatch:\ngot:  %s\nwant: %s", pq, expected)
			}
		}
	}

	limit, offset := dialect.NormalizePage(3, 25, 20, 50)
	if limit != 25 || offset != 50 {
		t.Fatalf("unexpected NormalizePage: limit=%d offset=%d", limit, offset)
	}
}

func TestSQLiteWALReadOnlyZeroSideEffectsTxRollbackAndCaseInsensitiveScan(t *testing.T) {
	ctx := context.Background()
	dbFile := filepath.Join(t.TempDir(), "test_volo.db")

	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    dbFile,
	})
	if err != nil {
		t.Fatalf("sqlx.Open failed: %v", err)
	}
	defer store.Close()

	// Idempotent migration: running Up twice must succeed with zero side effects.
	if err := Up(ctx, store); err != nil {
		t.Fatalf("first migrate.Up failed: %v", err)
	}
	if err := Up(ctx, store); err != nil {
		t.Fatalf("second idempotent migrate.Up failed: %v", err)
	}

	// Verify WAL mode is active
	var journalMode string
	if err := store.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if strings.ToLower(journalMode) != "wal" {
		t.Fatalf("expected WAL journal_mode, got %q", journalMode)
	}

	// Verify ReadDB enforces query_only=ON (zero write side effects on read pool!)
	if _, err := store.ReadDB().ExecContext(ctx,
		`INSERT INTO tags (id, name, slug, created_at) VALUES ('t1', 'Go', 'go', 100)`); err == nil {
		t.Fatal("expected ReadDB with query_only=ON to physically block write statements")
	}

	// Verify WithTx rolls back ALL writes when multi-table operation fails mid-flight
	boom := errors.New("simulated failure after first insert")
	err = store.WithTx(ctx, func(tx *sqlx.Tx) error {
		if _, txErr := tx.ExecContext(ctx,
			`INSERT INTO categories (id, name, slug, description, icon, sort_order, created_at, updated_at)
			 VALUES (?, ?, ?, '', '', 1, 1000, 1000)`, "cat_1", "Backend", "backend"); txErr != nil {
			return txErr
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("expected boom error, got %v", err)
	}

	cats, err := sqlx.Select[domain.Category](ctx, store, `SELECT * FROM categories`)
	if err != nil {
		t.Fatalf("select categories after rollback: %v", err)
	}
	if len(cats) != 0 {
		t.Fatalf("expected 0 categories after rollback, got %d", len(cats))
	}

	// Verify successful WithTx + Upsert + Case-insensitive column alias scanning + Pagination
	err = store.WithTx(ctx, func(tx *sqlx.Tx) error {
		upsert := tx.Dialect().UpsertSQL(
			"tags",
			[]string{"id", "name", "slug", "created_at"},
			[]string{"id"},
			[]string{"name", "slug"},
		)
		for i, name := range []string{"Go", "Rust", "SQLite"} {
			id := "tag_" + strings.ToLower(name)
			if _, txErr := tx.ExecContext(ctx, upsert, id, name, strings.ToLower(name), int64(100+i)); txErr != nil {
				return txErr
			}
		}
		// Update "Go" -> "Golang" idempotently via Upsert
		_, txErr := tx.ExecContext(ctx, upsert, "tag_go", "Golang", "golang", int64(999))
		return txErr
	})
	if err != nil {
		t.Fatalf("WithTx upsert failed: %v", err)
	}

	// Deliberately use UPPERCASE and MixedCase aliases to prove case-insensitive column mapping!
	pageSQL, pageArgs := store.Dialect().PaginateSQL(
		`SELECT id AS ID, name AS NaMe, slug AS SLUG, created_at AS Created_At FROM tags ORDER BY created_at ASC`,
		2, 0,
	)
	tags, err := sqlx.Select[domain.Tag](ctx, store, pageSQL, pageArgs...)
	if err != nil {
		t.Fatalf("case-insensitive paginated Select failed: %v", err)
	}
	if len(tags) != 2 || tags[0].Name != "Golang" || tags[1].Name != "Rust" {
		t.Fatalf("unexpected paginated/upserted tags: %+v", tags)
	}
}

func TestQueryIndexesAndNoDuplicateIndexCollision(t *testing.T) {
	// 1. Verify across all 3 dialects that no index name maps to two different column definitions.
	for _, drv := range []string{"sqlite", "postgres", "mysql"} {
		d, err := dialect.For(drv)
		if err != nil {
			t.Fatal(err)
		}
		seenDef := make(map[string]string)
		for _, step := range steps {
			for _, stmt := range step.Statements(d) {
				trimmed := strings.TrimSpace(stmt)
				if !strings.HasPrefix(trimmed, "CREATE INDEX ") && !strings.HasPrefix(trimmed, "CREATE UNIQUE INDEX ") {
					continue
				}
				rest := strings.TrimPrefix(strings.TrimPrefix(trimmed, "CREATE UNIQUE INDEX "), "CREATE INDEX ")
				idxName := strings.Fields(rest)[0]
				if prev, ok := seenDef[idxName]; ok && prev != trimmed {
					t.Fatalf("dialect %s has colliding index name %q with different definitions:\n  %s\n  %s", drv, idxName, prev, trimmed)
				}
				seenDef[idxName] = trimmed
			}
		}
	}

	// 2. Verify all 11 composite indexes exist after migrate.Up AND after upgrading from v3.
	ctx := context.Background()
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(t.TempDir(), "indexes.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := Up(ctx, store); err != nil {
		t.Fatalf("migrate.Up: %v", err)
	}

	expectedIndexes := []string{
		"idx_articles_status_pub_at",
		"idx_articles_status_views_pub",
		"idx_articles_status_comments_pub",
		"idx_articles_pinned_updated",
		"idx_articles_status_pinned_updated",
		"idx_comments_target_del_created",
		"idx_comments_del_status_created",
		"idx_comments_del_created",
		"idx_media_assets_created",
		"idx_redirects_created",
		"idx_series_articles_sort",
		// V6 scale composite indexes
		"idx_comments_target_root_created",
		"idx_books_sort_created",
		"idx_books_status_sort_created",
		"idx_pages_updated",
		"idx_media_mime_created",
		"idx_media_size_created",
		"idx_webhook_deliveries_hook_created_id",
		// V7 media uploader & audit log composite indexes
		"idx_media_uploader_created",
		"idx_audit_actor_created",
		"idx_audit_action_created",
		// V9 comments, follows, and series composite indexes
		"idx_comments_del_updated",
		"idx_follows_sort_created",
		"idx_series_sort_created",
	}

	rows, err := sqlx.Select[struct {
		Name string `db:"name"`
	}](ctx, store, `SELECT name FROM sqlite_master WHERE type = 'index'`)
	if err != nil {
		t.Fatal(err)
	}
	present := make(map[string]bool, len(rows))
	for _, r := range rows {
		present[r.Name] = true
	}
	for _, idx := range expectedIndexes {
		if !present[idx] {
			t.Fatalf("expected index %q missing after migrate.Up", idx)
		}
	}
	if present["idx_media_created"] {
		t.Fatalf("duplicate index idx_media_created should have been dropped in V6")
	}

	// Verify V9 schema version recorded and execIdempotentAddColumn pre-checks existing columns cleanly
	var maxVer int
	if err := store.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&maxVer); err != nil {
		t.Fatal(err)
	}
	if maxVer != 9 || CurrentSchemaVersion != 9 {
		t.Fatalf("expected schema version 9, got maxVer=%d CurrentSchemaVersion=%d", maxVer, CurrentSchemaVersion)
	}
	for _, col := range []string{"ban_until", "muted_until", "ban_reason", "shield_comments", "shield_text"} {
		exists, cErr := columnExists(ctx, store, "users", col)
		if cErr != nil || !exists {
			t.Fatalf("expected users.%s column from V8 migration (exists=%v, err=%v)", col, exists, cErr)
		}
	}
	if err := execIdempotentAddColumn(ctx, store, `ALTER TABLE media_assets ADD COLUMN alt_text TEXT NOT NULL DEFAULT ''`); err != nil {
		t.Fatalf("execIdempotentAddColumn on existing column failed: %v", err)
	}

	// Verify V7 published_at backfill only updates published articles with published_at <= 0
	const insertArticleSQL = `INSERT INTO articles (
		id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md,
		cover_url, status, published_at, scheduled_at, is_pinned, allow_comment,
		password_ciphertext, has_content_lock, audio_url, audio_duration_sec, audio_size_bytes,
		audio_mime, lang, translation_group, license_override, word_count, reading_minutes,
		view_count, comment_count, created_at, updated_at
	) VALUES (?, 'u1', '', ?, ?, '', '', '', '', ?, ?, 0, 0, 1, '', 0, '', 0, 0, '', 'en', '', '', 0, 0, 0, 0, ?, ?)`

	if _, err := store.ExecContext(ctx, insertArticleSQL, "art_pub_zero", "Published Zero", "pub-zero", "published", int64(0), int64(1700000001000), int64(1700000001000)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExecContext(ctx, insertArticleSQL, "art_pub_keep", "Published Keep", "pub-keep", "published", int64(1700000009000), int64(1700000001000), int64(1700000001000)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExecContext(ctx, insertArticleSQL, "art_draft_zero", "Draft Zero", "draft-zero", "draft", int64(0), int64(1700000001000), int64(1700000001000)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = 7`); err != nil {
		t.Fatal(err)
	}
	if err := Up(ctx, store); err != nil {
		t.Fatalf("re-applying V7 migration failed: %v", err)
	}

	var pubZero, pubKeep, draftZero int64
	_ = store.QueryRowContext(ctx, `SELECT published_at FROM articles WHERE id = 'art_pub_zero'`).Scan(&pubZero)
	_ = store.QueryRowContext(ctx, `SELECT published_at FROM articles WHERE id = 'art_pub_keep'`).Scan(&pubKeep)
	_ = store.QueryRowContext(ctx, `SELECT published_at FROM articles WHERE id = 'art_draft_zero'`).Scan(&draftZero)
	if pubZero != 1700000001000 || pubKeep != 1700000009000 || draftZero != 0 {
		t.Fatalf("unexpected V7 published_at backfill: pubZero=%d pubKeep=%d draftZero=%d", pubZero, pubKeep, draftZero)
	}

	// Verify V6 Postgres & MySQL key-widening statements and V1 VARCHAR(512) widths
	pgD, _ := dialect.For("postgres")
	myD, _ := dialect.For("mysql")
	pgV6 := strings.Join(buildV6ScaleIndexesAndKeyWidths(pgD), "\n")
	myV6 := strings.Join(buildV6ScaleIndexesAndKeyWidths(myD), "\n")
	for _, col := range []string{"credential_id", "legacy_permalink", "source_path", "item_key"} {
		if !strings.Contains(pgV6, "ALTER COLUMN "+col+" TYPE VARCHAR(512)") {
			t.Fatalf("postgres V6 missing VARCHAR(512) widening for %s:\n%s", col, pgV6)
		}
		if !strings.Contains(myV6, "MODIFY COLUMN "+col+" VARCHAR(512) NOT NULL DEFAULT ''") {
			t.Fatalf("mysql V6 missing VARCHAR(512) MODIFY COLUMN for %s:\n%s", col, myV6)
		}
	}
	if !strings.Contains(myV6, "DROP INDEX idx_media_created ON media_assets") {
		t.Fatalf("mysql V6 missing DROP INDEX idx_media_created ON media_assets")
	}
}

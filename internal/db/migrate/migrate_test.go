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

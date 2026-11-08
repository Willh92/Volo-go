package dialect_test

import (
	"strings"
	"testing"

	"github.com/Willh92/Volo-go/internal/db/dialect"
)

func TestDialect_ForAndUnsupported(t *testing.T) {
	cases := []struct {
		driver string
		want   string
	}{
		{"sqlite", "sqlite"},
		{"sqlite3", "sqlite"},
		{"postgres", "postgres"},
		{"postgresql", "postgres"},
		{"pgx", "postgres"},
		{"mysql", "mysql"},
		{"mariadb", "mysql"},
		{"tidb", "mysql"},
	}
	for _, tc := range cases {
		d, err := dialect.For(tc.driver)
		if err != nil {
			t.Fatalf("dialect.For(%q) unexpected error: %v", tc.driver, err)
		}
		if d.Name() != tc.want {
			t.Fatalf("dialect.For(%q).Name() = %q, want %q", tc.driver, d.Name(), tc.want)
		}
	}
	if _, err := dialect.For("oracle"); err == nil {
		t.Fatalf("expected error for unsupported driver")
	}
}

func TestDialect_PlaceholderRebindAcrossThreeDialects(t *testing.T) {
	rawSQL := `SELECT id, "col?name" FROM articles WHERE title = 'What? Really? It''s fine?' AND status = ? AND category_id = ? AND view_count >= ?`

	sq, _ := dialect.For("sqlite")
	my, _ := dialect.For("mysql")
	pg, _ := dialect.For("postgres")

	if got := sq.Rebind(rawSQL); got != rawSQL {
		t.Fatalf("sqlite Rebind mutated query: %s", got)
	}
	if got := my.Rebind(rawSQL); got != rawSQL {
		t.Fatalf("mysql Rebind mutated query: %s", got)
	}

	wantPG := `SELECT id, "col?name" FROM articles WHERE title = 'What? Really? It''s fine?' AND status = $1 AND category_id = $2 AND view_count >= $3`
	if got := pg.Rebind(rawSQL); got != wantPG {
		t.Fatalf("postgres Rebind mismatch:\ngot:  %s\nwant: %s", got, wantPG)
	}
}

func TestDialect_DDLTypesAndIdentifierQuoting(t *testing.T) {
	sq, _ := dialect.For("sqlite")
	pg, _ := dialect.For("postgres")
	my, _ := dialect.For("mysql")

	if sq.QuoteIdent(`user"col`) != `"user""col"` {
		t.Fatalf("sqlite QuoteIdent mismatch: %s", sq.QuoteIdent(`user"col`))
	}
	if pg.QuoteIdent(`user"col`) != `"user""col"` {
		t.Fatalf("postgres QuoteIdent mismatch: %s", pg.QuoteIdent(`user"col`))
	}
	if my.QuoteIdent("user`col") != "`user``col`" {
		t.Fatalf("mysql QuoteIdent mismatch: %s", my.QuoteIdent("user`col"))
	}

	if !strings.Contains(sq.Types().Bool, "INTEGER") || sq.Types().BoolTrueLit != "1" {
		t.Fatalf("unexpected sqlite Types: %+v", sq.Types())
	}
	if !strings.Contains(pg.Types().Bool, "BOOLEAN") || !strings.Contains(pg.Types().Blob, "BYTEA") || pg.Types().BoolTrueLit != "TRUE" {
		t.Fatalf("unexpected postgres Types: %+v", pg.Types())
	}
	if !strings.Contains(my.Types().Bool, "TINYINT(1)") || !strings.Contains(my.Types().LongText, "LONGTEXT") || !strings.Contains(my.Types().Blob, "LONGBLOB") {
		t.Fatalf("unexpected mysql Types: %+v", my.Types())
	}
}

func TestDialect_UpsertSQLAcrossThreeDialects(t *testing.T) {
	sq, _ := dialect.For("sqlite")
	pg, _ := dialect.For("postgres")
	my, _ := dialect.For("mysql")

	cols := []string{"plugin_id", "k", "value_text", "updated_at"}
	conflict := []string{"plugin_id", "k"}
	updates := []string{"value_text", "updated_at"}

	sqUpsert := sq.UpsertSQL("plugin_kv", cols, conflict, updates)
	if !strings.Contains(sqUpsert, `ON CONFLICT ("plugin_id", "k") DO UPDATE SET "value_text" = EXCLUDED."value_text"`) || !strings.Contains(sqUpsert, "VALUES (?, ?, ?, ?)") {
		t.Fatalf("unexpected sqlite UpsertSQL: %s", sqUpsert)
	}

	pgUpsert := pg.UpsertSQL("plugin_kv", cols, conflict, updates)
	if !strings.Contains(pgUpsert, `ON CONFLICT ("plugin_id", "k") DO UPDATE SET "value_text" = EXCLUDED."value_text"`) || !strings.Contains(pgUpsert, "VALUES ($1, $2, $3, $4)") {
		t.Fatalf("unexpected postgres UpsertSQL: %s", pgUpsert)
	}

	myUpsert := my.UpsertSQL("plugin_kv", cols, conflict, updates)
	if !strings.Contains(myUpsert, "ON DUPLICATE KEY UPDATE `value_text` = VALUES(`value_text`), `updated_at` = VALUES(`updated_at`)") {
		t.Fatalf("unexpected mysql UpsertSQL: %s", myUpsert)
	}

	// Do-nothing / idempotent conflict insert (empty updateCols)
	sqNoop := sq.UpsertSQL("article_tags", []string{"article_id", "tag_id"}, []string{"article_id", "tag_id"}, nil)
	if !strings.Contains(sqNoop, "DO NOTHING") {
		t.Fatalf("expected sqlite DO NOTHING upsert, got: %s", sqNoop)
	}
	myNoop := my.UpsertSQL("article_tags", []string{"article_id", "tag_id"}, []string{"article_id", "tag_id"}, nil)
	if !strings.Contains(myNoop, "ON DUPLICATE KEY UPDATE `article_id` = `article_id`") {
		t.Fatalf("expected mysql idempotent anchor upsert, got: %s", myNoop)
	}
}

func TestDialect_PaginateSQLAndNormalizePage(t *testing.T) {
	limit, offset := dialect.NormalizePage(-3, 0, 15, 50)
	if limit != 15 || offset != 0 {
		t.Fatalf("NormalizePage(-3, 0) = (%d, %d), want (15, 0)", limit, offset)
	}
	limit, offset = dialect.NormalizePage(3, 999, 15, 50)
	if limit != 50 || offset != 100 {
		t.Fatalf("NormalizePage(3, 999) = (%d, %d), want (50, 100)", limit, offset)
	}

	pg, _ := dialect.For("postgres")
	qPG, argsPG := pg.PaginateSQL("SELECT * FROM articles WHERE status = ? ORDER BY published_at DESC;", 20, 40)
	if qPG != "SELECT * FROM articles WHERE status = $1 ORDER BY published_at DESC LIMIT $2 OFFSET $3" {
		t.Fatalf("postgres PaginateSQL = %q", qPG)
	}
	if len(argsPG) != 2 || argsPG[0] != 20 || argsPG[1] != 40 {
		t.Fatalf("unexpected PaginateSQL args: %+v", argsPG)
	}

	my, _ := dialect.For("mysql")
	qMY, _ := my.PaginateSQL("SELECT * FROM articles WHERE status = ?;", 10, 0)
	if qMY != "SELECT * FROM articles WHERE status = ? LIMIT ? OFFSET ?" {
		t.Fatalf("mysql PaginateSQL = %q", qMY)
	}
}

func TestDialect_UpsertIncrementSQLAcrossThreeDialects(t *testing.T) {
	sq, _ := dialect.For("sqlite")
	pg, _ := dialect.For("postgres")
	my, _ := dialect.For("mysql")

	cols := []string{"stat_date", "pv", "uv", "updated_at"}
	conflict := []string{"stat_date"}
	incCols := []string{"pv", "uv"}
	ovCols := []string{"updated_at"}

	wantSQ := `INSERT INTO "analytics_daily" ("stat_date", "pv", "uv", "updated_at") VALUES (?, ?, ?, ?) ON CONFLICT ("stat_date") DO UPDATE SET "pv" = "analytics_daily"."pv" + EXCLUDED."pv", "uv" = "analytics_daily"."uv" + EXCLUDED."uv", "updated_at" = EXCLUDED."updated_at"`
	if got := sq.UpsertIncrementSQL("analytics_daily", cols, conflict, incCols, ovCols); got != wantSQ {
		t.Fatalf("sqlite UpsertIncrementSQL mismatch:\ngot:  %s\nwant: %s", got, wantSQ)
	}

	wantPG := `INSERT INTO "analytics_daily" ("stat_date", "pv", "uv", "updated_at") VALUES ($1, $2, $3, $4) ON CONFLICT ("stat_date") DO UPDATE SET "pv" = "analytics_daily"."pv" + EXCLUDED."pv", "uv" = "analytics_daily"."uv" + EXCLUDED."uv", "updated_at" = EXCLUDED."updated_at"`
	if got := pg.UpsertIncrementSQL("analytics_daily", cols, conflict, incCols, ovCols); got != wantPG {
		t.Fatalf("postgres UpsertIncrementSQL mismatch:\ngot:  %s\nwant: %s", got, wantPG)
	}

	wantMY := "INSERT INTO `analytics_daily` (`stat_date`, `pv`, `uv`, `updated_at`) VALUES (?, ?, ?, ?) ON DUPLICATE KEY UPDATE `pv` = `pv` + VALUES(`pv`), `uv` = `uv` + VALUES(`uv`), `updated_at` = VALUES(`updated_at`)"
	if got := my.UpsertIncrementSQL("analytics_daily", cols, conflict, incCols, ovCols); got != wantMY {
		t.Fatalf("mysql UpsertIncrementSQL mismatch:\ngot:  %s\nwant: %s", got, wantMY)
	}

	// Increment-only (nil overwriteCols)
	topCols := []string{"stat_date", "kind", "item_key", "hits"}
	topConflict := []string{"stat_date", "kind", "item_key"}
	pgTop := pg.UpsertIncrementSQL("analytics_top_items", topCols, topConflict, []string{"hits"}, nil)
	wantPGTop := `INSERT INTO "analytics_top_items" ("stat_date", "kind", "item_key", "hits") VALUES ($1, $2, $3, $4) ON CONFLICT ("stat_date", "kind", "item_key") DO UPDATE SET "hits" = "analytics_top_items"."hits" + EXCLUDED."hits"`
	if pgTop != wantPGTop {
		t.Fatalf("postgres top items UpsertIncrementSQL mismatch:\ngot:  %s\nwant: %s", pgTop, wantPGTop)
	}

	// No-op fallback when both incCols and overwriteCols are empty
	if got := sq.UpsertIncrementSQL("article_tags", []string{"article_id", "tag_id"}, []string{"article_id", "tag_id"}, nil, nil); !strings.Contains(got, "DO NOTHING") {
		t.Fatalf("expected sqlite DO NOTHING fallback, got: %s", got)
	}
	if got := my.UpsertIncrementSQL("article_tags", []string{"article_id", "tag_id"}, []string{"article_id", "tag_id"}, nil, nil); !strings.Contains(got, "ON DUPLICATE KEY UPDATE `article_id` = `article_id`") {
		t.Fatalf("expected mysql anchor fallback, got: %s", got)
	}
}


package migrate

import (
	"bytes"
	"compress/gzip"
	"context"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/Willh92/Volo-go/internal/db/dialect"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
)

//go:embed v1_schema.sql.gz
var v1SchemaSQLGZ []byte

var (
	v1SchemaOnce sync.Once
	v1SchemaTpl  string
)

func loadV1SchemaTemplate() string {
	v1SchemaOnce.Do(func() {
		gr, err := gzip.NewReader(bytes.NewReader(v1SchemaSQLGZ))
		if err != nil {
			return
		}
		defer gr.Close()
		raw, err := io.ReadAll(gr)
		if err == nil {
			v1SchemaTpl = string(raw)
		}
	})
	return v1SchemaTpl
}

// CurrentSchemaVersion is the canonical normalized schema version of Volo-go.
const CurrentSchemaVersion = 9

type migrationStep struct {
	Version     int
	Description string
	Statements  func(d dialect.Dialect) []string
}

var steps = []migrationStep{
	{
		Version:     1,
		Description: "Initialize normalized Volo-go v1 schema (all first-class domain tables & indexes)",
		Statements:  buildV1Schema,
	},
	{
		Version:     2,
		Description: "Add composite hot-path read indexes for follow_items, pages, articles, and media_assets",
		Statements:  buildV2Indexes,
	},
	{
		Version:     3,
		Description: "Add alt_text accessibility column to media_assets",
		Statements:  buildV3MediaAltText,
	},
	{
		Version:     4,
		Description: "Fix idx_articles_status_pub name collision and add composite sort/filter indexes across articles, comments, media_assets, redirects, and series_articles",
		Statements:  buildV4QueryIndexes,
	},
	{
		Version:     5,
		Description: "Add custom template column to pages and bounded webhook_deliveries history table",
		Statements:  buildV5PageTemplatesAndWebhookDeliveries,
	},
	{
		Version:     6,
		Description: "Drop duplicate media index, add v6 scale composite indexes, and widen long lookup keys to VARCHAR(512)",
		Statements:  buildV6ScaleIndexesAndKeyWidths,
	},
	{
		Version:     7,
		Description: "Backfill zero published_at on published articles and add media uploader and audit log composite indexes",
		Statements:  buildV7ArticlePublishedAtAndAuditMediaIndexes,
	},
	{
		Version:     8,
		Description: "Add timed account ban, mute, reason, and reversible historical comment shielding columns to users",
		Statements:  buildV8UserModerationAndCommentShielding,
	},
	{
		Version:     9,
		Description: "Add composite sort and incremental sync indexes on comments, follows, and series",
		Statements:  buildV9CommentsFollowsSeriesIndexes,
	},
}

func buildV2Indexes(_ dialect.Dialect) []string {
	return []string{
		`CREATE INDEX idx_follow_items_follow_pub ON follow_items (follow_id, published_at)`,
		`CREATE INDEX idx_pages_pub_updated ON pages (is_published, updated_at)`,
		`CREATE INDEX idx_articles_status_pub_at ON articles (status, published_at)`,
		`CREATE INDEX idx_media_assets_dims ON media_assets (width, height)`,
	}
}

func buildV3MediaAltText(d dialect.Dialect) []string {
	t := d.Types()
	return []string{
		fmt.Sprintf(`ALTER TABLE media_assets ADD COLUMN alt_text%s NOT NULL DEFAULT ''`, t.Varchar512),
	}
}

func buildV4QueryIndexes(_ dialect.Dialect) []string {
	return []string{
		`CREATE INDEX idx_articles_status_pub_at ON articles (status, published_at)`,
		`CREATE INDEX idx_articles_status_views_pub ON articles (status, view_count, published_at)`,
		`CREATE INDEX idx_articles_status_comments_pub ON articles (status, comment_count, published_at)`,
		`CREATE INDEX idx_articles_pinned_updated ON articles (is_pinned, updated_at)`,
		`CREATE INDEX idx_articles_status_pinned_updated ON articles (status, is_pinned, updated_at)`,
		`CREATE INDEX idx_comments_target_del_created ON comments (target_type, target_id, is_deleted, created_at, id)`,
		`CREATE INDEX idx_comments_del_status_created ON comments (is_deleted, status, created_at)`,
		`CREATE INDEX idx_comments_del_created ON comments (is_deleted, created_at)`,
		`CREATE INDEX idx_media_created ON media_assets (created_at)`,
		`CREATE INDEX idx_redirects_created ON redirects (created_at)`,
		`CREATE INDEX idx_series_articles_sort ON series_articles (series_id, sort_order)`,
	}
}

func buildV5PageTemplatesAndWebhookDeliveries(d dialect.Dialect) []string {
	t := d.Types()
	sfx := tableSuffix(d)
	return []string{
		fmt.Sprintf(`ALTER TABLE pages ADD COLUMN template%s NOT NULL DEFAULT 'default'`, t.Varchar64),
		fmt.Sprintf(`ALTER TABLE books ADD COLUMN reading_status%s NOT NULL DEFAULT ''`, t.Varchar32),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS webhook_deliveries (
			id%s PRIMARY KEY,
			webhook_id%s NOT NULL,
			event%s NOT NULL,
			status_code%s NOT NULL DEFAULT 0,
			duration_ms%s NOT NULL DEFAULT 0,
			success%s NOT NULL DEFAULT %s,
			payload_json%s NOT NULL,
			response_body%s NOT NULL,
			error_text%s NOT NULL,
			created_at%s NOT NULL
		)%s`, t.Varchar32, t.Varchar32, t.Varchar64, t.Int, t.Int, t.Bool, t.BoolFalseLit,
			t.Text, t.Text, t.Varchar255, t.BigInt, sfx),
		`CREATE INDEX idx_webhook_deliveries_hook_created ON webhook_deliveries (webhook_id, created_at)`,
	}
}

func buildV6ScaleIndexesAndKeyWidths(d dialect.Dialect) []string {
	stmts := make([]string, 0, 12)
	if d.Name() == "mysql" {
		stmts = append(stmts, `DROP INDEX idx_media_created ON media_assets`)
	} else {
		stmts = append(stmts, `DROP INDEX IF EXISTS idx_media_created`)
	}
	stmts = append(stmts,
		`CREATE INDEX idx_comments_target_root_created ON comments (target_type, target_id, is_deleted, parent_id, created_at, id)`,
		`CREATE INDEX idx_books_sort_created ON books (sort_order, created_at)`,
		`CREATE INDEX idx_books_status_sort_created ON books (reading_status, sort_order, created_at)`,
		`CREATE INDEX idx_pages_updated ON pages (updated_at)`,
		`CREATE INDEX idx_media_mime_created ON media_assets (mime_type, created_at)`,
		`CREATE INDEX idx_media_size_created ON media_assets (size_bytes, created_at)`,
		`CREATE INDEX idx_webhook_deliveries_hook_created_id ON webhook_deliveries (webhook_id, created_at, id)`,
	)
	switch d.Name() {
	case "postgres":
		stmts = append(stmts,
			`ALTER TABLE user_credentials ALTER COLUMN credential_id TYPE VARCHAR(512)`,
			`ALTER TABLE articles ALTER COLUMN legacy_permalink TYPE VARCHAR(512)`,
			`ALTER TABLE redirects ALTER COLUMN source_path TYPE VARCHAR(512)`,
			`ALTER TABLE analytics_top_items ALTER COLUMN item_key TYPE VARCHAR(512)`,
		)
	case "mysql":
		stmts = append(stmts,
			`ALTER TABLE user_credentials MODIFY COLUMN credential_id VARCHAR(512) NOT NULL DEFAULT ''`,
			`ALTER TABLE articles MODIFY COLUMN legacy_permalink VARCHAR(512) NOT NULL DEFAULT ''`,
			`ALTER TABLE redirects MODIFY COLUMN source_path VARCHAR(512) NOT NULL DEFAULT ''`,
			`ALTER TABLE analytics_top_items MODIFY COLUMN item_key VARCHAR(512) NOT NULL DEFAULT ''`,
		)
	}
	return stmts
}

func buildV7ArticlePublishedAtAndAuditMediaIndexes(_ dialect.Dialect) []string {
	return []string{
		`UPDATE articles SET published_at = created_at WHERE status = 'published' AND published_at <= 0`,
		`CREATE INDEX idx_media_uploader_created ON media_assets (uploader_id, created_at)`,
		`CREATE INDEX idx_audit_actor_created ON audit_logs (actor_id, created_at)`,
		`CREATE INDEX idx_audit_action_created ON audit_logs (action, created_at)`,
	}
}

func buildV8UserModerationAndCommentShielding(d dialect.Dialect) []string {
	t := d.Types()
	return []string{
		fmt.Sprintf(`ALTER TABLE users ADD COLUMN ban_until%s NOT NULL DEFAULT 0`, t.BigInt),
		fmt.Sprintf(`ALTER TABLE users ADD COLUMN muted_until%s NOT NULL DEFAULT 0`, t.BigInt),
		fmt.Sprintf(`ALTER TABLE users ADD COLUMN ban_reason%s NOT NULL DEFAULT ''`, t.Varchar255),
		fmt.Sprintf(`ALTER TABLE users ADD COLUMN shield_comments%s NOT NULL DEFAULT %s`, t.Bool, t.BoolFalseLit),
		fmt.Sprintf(`ALTER TABLE users ADD COLUMN shield_text%s NOT NULL DEFAULT ''`, t.Varchar255),
	}
}

func buildV9CommentsFollowsSeriesIndexes(_ dialect.Dialect) []string {
	return []string{
		`CREATE INDEX idx_comments_del_updated ON comments (is_deleted, updated_at, id)`,
		`CREATE INDEX idx_follows_sort_created ON follows (sort_order, created_at)`,
		`CREATE INDEX idx_series_sort_created ON series (sort_order, created_at)`,
	}
}

// Up idempotently applies all pending schema migrations across SQLite, PostgreSQL, and MySQL.
func Up(ctx context.Context, store *sqlx.Store) error {
	d := store.Dialect()
	tm := d.Types()

	bootDDL := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version%s PRIMARY KEY,
		description%s NOT NULL,
		applied_at%s NOT NULL
	)%s`, tm.Int, tm.Varchar255, tm.BigInt, tableSuffix(d))

	if _, err := store.ExecContext(ctx, bootDDL); err != nil {
		return fmt.Errorf("migrate: create schema_migrations: %w", err)
	}

	var appliedRows []struct {
		Version int `db:"version"`
	}
	err := sqlx.SelectInto(ctx, store, &appliedRows, "SELECT version FROM schema_migrations ORDER BY version ASC")
	if err != nil {
		return fmt.Errorf("migrate: query applied migrations: %w", err)
	}
	applied := make(map[int]bool, len(appliedRows))
	for _, r := range appliedRows {
		applied[r.Version] = true
	}

	for _, step := range steps {
		if applied[step.Version] {
			continue
		}
		stmts := step.Statements(d)
		for _, stmt := range stmts {
			upper := strings.TrimSpace(strings.ToUpper(stmt))
			if strings.HasPrefix(upper, "CREATE INDEX ") ||
				strings.HasPrefix(upper, "CREATE UNIQUE INDEX ") {
				if err := execIdempotentIndex(ctx, store, stmt); err != nil {
					return fmt.Errorf("migrate v%d index failed (%s): %w", step.Version, stmt, err)
				}
				continue
			}
			if strings.HasPrefix(upper, "DROP INDEX ") {
				if err := execIdempotentDropIndex(ctx, store, stmt); err != nil {
					return fmt.Errorf("migrate v%d drop index failed (%s): %w", step.Version, stmt, err)
				}
				continue
			}
			if strings.HasPrefix(upper, "ALTER TABLE ") && strings.Contains(upper, " ADD COLUMN ") {
				if err := execIdempotentAddColumn(ctx, store, stmt); err != nil {
					return fmt.Errorf("migrate v%d add column failed (%s): %w", step.Version, stmt, err)
				}
				continue
			}
			if _, err := store.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("migrate v%d DDL failed (%s): %w", step.Version, stmt, err)
			}
		}

		upsertVer := d.UpsertSQL(
			"schema_migrations",
			[]string{"version", "description", "applied_at"},
			[]string{"version"},
			[]string{"description", "applied_at"},
		)
		if _, err := store.ExecContext(ctx, upsertVer, step.Version, step.Description, time.Now().UnixMilli()); err != nil {
			return fmt.Errorf("migrate: record version %d: %w", step.Version, err)
		}
	}
	return nil
}

func execIdempotentIndex(ctx context.Context, store *sqlx.Store, stmt string) error {
	if store.Dialect().Name() != "mysql" {
		// SQLite and PostgreSQL support CREATE [UNIQUE] INDEX IF NOT EXISTS natively.
		stmt = strings.Replace(stmt, "CREATE UNIQUE INDEX ", "CREATE UNIQUE INDEX IF NOT EXISTS ", 1)
		stmt = strings.Replace(stmt, "CREATE INDEX ", "CREATE INDEX IF NOT EXISTS ", 1)
		_, err := store.ExecContext(ctx, stmt)
		return err
	}
	// MySQL 8.0 lacks IF NOT EXISTS on CREATE INDEX; ignore duplicate key name (Error 1061).
	_, err := store.ExecContext(ctx, stmt)
	if err != nil && (strings.Contains(err.Error(), "1061") || strings.Contains(strings.ToLower(err.Error()), "duplicate key name")) {
		return nil
	}
	return err
}

func execIdempotentDropIndex(ctx context.Context, store *sqlx.Store, stmt string) error {
	_, err := store.ExecContext(ctx, stmt)
	if err == nil {
		return nil
	}
	low := strings.ToLower(err.Error())
	if strings.Contains(err.Error(), "1091") ||
		strings.Contains(low, "check that column/key exists") ||
		strings.Contains(low, "can't drop") ||
		strings.Contains(low, "does not exist") ||
		strings.Contains(low, "no such index") {
		return nil
	}
	return err
}

func parseAlterAddColumnTarget(stmt string) (table string, col string, ok bool) {
	fields := strings.Fields(stmt)
	if len(fields) < 6 || !strings.EqualFold(fields[0], "ALTER") || !strings.EqualFold(fields[1], "TABLE") {
		return "", "", false
	}
	idx := 2
	if idx+1 < len(fields) && strings.EqualFold(fields[idx], "IF") && strings.EqualFold(fields[idx+1], "EXISTS") {
		idx += 2
	}
	if idx >= len(fields) {
		return "", "", false
	}
	table = strings.Trim(fields[idx], "`\"")
	idx++
	if idx >= len(fields) || !strings.EqualFold(fields[idx], "ADD") {
		return "", "", false
	}
	idx++
	if idx < len(fields) && strings.EqualFold(fields[idx], "COLUMN") {
		idx++
	}
	if idx+2 < len(fields) && strings.EqualFold(fields[idx], "IF") && strings.EqualFold(fields[idx+1], "NOT") && strings.EqualFold(fields[idx+2], "EXISTS") {
		idx += 3
	}
	if idx >= len(fields) {
		return "", "", false
	}
	col = strings.Trim(fields[idx], "`\"")
	return table, col, table != "" && col != ""
}

func columnExists(ctx context.Context, store *sqlx.Store, table string, col string) (bool, error) {
	switch store.Dialect().Name() {
	case "sqlite":
		var rows []struct {
			Name string `db:"name"`
		}
		err := sqlx.SelectInto(ctx, store, &rows, fmt.Sprintf("PRAGMA table_info(%s)", store.Dialect().QuoteIdent(table)))
		if err != nil {
			return false, err
		}
		for _, r := range rows {
			if strings.EqualFold(r.Name, col) {
				return true, nil
			}
		}
		return false, nil
	case "postgres":
		var cnt int
		err := store.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = CURRENT_SCHEMA() AND LOWER(table_name) = LOWER(?) AND LOWER(column_name) = LOWER(?)`,
			table, col,
		).Scan(&cnt)
		return cnt > 0, err
	case "mysql":
		var cnt int
		err := store.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND LOWER(table_name) = LOWER(?) AND LOWER(column_name) = LOWER(?)`,
			table, col,
		).Scan(&cnt)
		return cnt > 0, err
	default:
		return false, nil
	}
}

func execIdempotentAddColumn(ctx context.Context, store *sqlx.Store, stmt string) error {
	if table, col, ok := parseAlterAddColumnTarget(stmt); ok {
		if exists, err := columnExists(ctx, store, table, col); err == nil && exists {
			return nil
		}
	}
	_, err := store.ExecContext(ctx, stmt)
	if err == nil {
		return nil
	}
	low := strings.ToLower(err.Error())
	if strings.Contains(low, "duplicate column") ||
		strings.Contains(low, "already exists") ||
		strings.Contains(err.Error(), "1060") ||
		strings.Contains(err.Error(), "42701") {
		return nil
	}
	return err
}

func tableSuffix(d dialect.Dialect) string {
	if d.Name() == "mysql" {
		return " ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci"
	}
	return ""
}

func varcharKey(d dialect.Dialect) string {
	if d.Name() == "sqlite" {
		return " TEXT"
	}
	// 191 chars * 4 bytes = 764 bytes, safe on every MySQL/MariaDB/TiDB utf8mb4 index engine.
	return " VARCHAR(191)"
}

func varcharLongKey(d dialect.Dialect) string {
	if d.Name() == "sqlite" {
		return " TEXT"
	}
	return " VARCHAR(512)"
}

// buildV1Schema expands the pre-compressed canonical V1 DDL template (`v1_schema.sql.gz`)
// using dialect-specific column types and table options.
func buildV1Schema(d dialect.Dialect) []string {
	t := d.Types()
	r := strings.NewReplacer(
		"{V32}", t.Varchar32,
		"{V64}", t.Varchar64,
		"{V128}", t.Varchar128,
		"{V255}", t.Varchar255,
		"{V512}", t.Varchar512,
		"{VK}", varcharKey(d),
		"{VLK}", varcharLongKey(d),
		"{TXT}", t.Text,
		"{LTXT}", t.LongText,
		"{INT}", t.Int,
		"{BIG}", t.BigInt,
		"{BOOL}", t.Bool,
		"{T}", t.BoolTrueLit,
		"{F}", t.BoolFalseLit,
		"{SFX}", tableSuffix(d),
	)
	expanded := strings.TrimSpace(r.Replace(loadV1SchemaTemplate()))
	if expanded == "" {
		return nil
	}
	return strings.Split(expanded, "\n")
}

// CanonicalTables lists all data tables in foreign-key-safe dependency order for streaming backup/restore.
var CanonicalTables = []string{
	"settings",
	"users",
	"user_credentials",
	"user_sessions",
	"api_tokens",
	"categories",
	"tags",
	"articles",
	"article_tags",
	"article_revisions",
	"article_backlinks",
	"pages",
	"nav_items",
	"series",
	"series_articles",
	"books",
	"book_chapters",
	"comments",
	"reactions",
	"links",
	"follows",
	"follow_items",
	"media_assets",
	"redirects",
	"webhooks",
	"webhook_deliveries",
	"analytics_daily",
	"analytics_top_items",
	"security_ip_bans",
	"security_geo_rules",
	"audit_logs",
	"plugins",
	"plugin_kv",
}

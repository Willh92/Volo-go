package migrate

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Willh92/Volo-go/internal/db/dialect"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
)

// CurrentSchemaVersion is the canonical normalized schema version of Volo-go.
const CurrentSchemaVersion = 2

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
}

func buildV2Indexes(_ dialect.Dialect) []string {
	return []string{
		`CREATE INDEX idx_follow_items_follow_pub ON follow_items (follow_id, published_at)`,
		`CREATE INDEX idx_pages_pub_updated ON pages (is_published, updated_at)`,
		`CREATE INDEX idx_articles_status_pub ON articles (status, published_at)`,
		`CREATE INDEX idx_media_assets_dims ON media_assets (width, height)`,
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

	appliedRows, err := sqlx.Select[struct {
		Version int `db:"version"`
	}](ctx, store, "SELECT version FROM schema_migrations ORDER BY version ASC")
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
			if strings.HasPrefix(strings.TrimSpace(strings.ToUpper(stmt)), "CREATE INDEX ") ||
				strings.HasPrefix(strings.TrimSpace(strings.ToUpper(stmt)), "CREATE UNIQUE INDEX ") {
				if err := execIdempotentIndex(ctx, store, stmt); err != nil {
					return fmt.Errorf("migrate v%d index failed (%s): %w", step.Version, stmt, err)
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

func buildV1Schema(d dialect.Dialect) []string {
	t := d.Types()
	vk := varcharKey(d)
	sfx := tableSuffix(d)

	return []string{
		// 1. Domain-scoped atomic settings
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS settings (
			domain%s PRIMARY KEY,
			value_json%s NOT NULL,
			secret_ciphertext%s NOT NULL,
			version%s NOT NULL DEFAULT 1,
			updated_at%s NOT NULL
		)%s`, t.Varchar64, t.LongText, t.Text, t.BigInt, t.BigInt, sfx),

		// 2. Sovereign Accounts (admin / editor / reader)
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS users (
			id%s PRIMARY KEY,
			username%s NOT NULL UNIQUE,
			email%s NOT NULL UNIQUE,
			display_name%s NOT NULL,
			avatar_url%s NOT NULL,
			bio%s NOT NULL,
			website_url%s NOT NULL,
			role%s NOT NULL,
			password_hash%s NOT NULL,
			legacy_hash%s NOT NULL,
			status%s NOT NULL,
			email_verified_at%s NOT NULL DEFAULT 0,
			preferred_locale%s NOT NULL,
			created_at%s NOT NULL,
			updated_at%s NOT NULL
		)%s`, t.Varchar32, t.Varchar64, vk, t.Varchar128, t.Varchar512, t.Text, t.Varchar512,
			t.Varchar32, t.Varchar255, t.Varchar255, t.Varchar32, t.BigInt, t.Varchar32, t.BigInt, t.BigInt, sfx),
		`CREATE INDEX idx_users_role_status ON users (role, status)`,

		// 3. Multi-factor & Passkey credentials (replaces voloPasskeyAuth + voloTotpAuth chunks)
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS user_credentials (
			id%s PRIMARY KEY,
			user_id%s NOT NULL,
			kind%s NOT NULL,
			name%s NOT NULL,
			credential_id%s NOT NULL,
			public_key_spki%s NOT NULL,
			cose_alg%s NOT NULL DEFAULT 0,
			sign_count%s NOT NULL DEFAULT 0,
			secret_ciphertext%s NOT NULL,
			meta_json%s NOT NULL,
			last_used_at%s NOT NULL DEFAULT 0,
			created_at%s NOT NULL
		)%s`, t.Varchar32, t.Varchar32, t.Varchar32, t.Varchar128, vk, t.Text,
			t.Int, t.BigInt, t.Text, t.Text, t.BigInt, t.BigInt, sfx),
		`CREATE INDEX idx_user_credentials_user_kind ON user_credentials (user_id, kind)`,
		`CREATE INDEX idx_user_credentials_cred_id ON user_credentials (credential_id)`,

		// 4. Active device sessions (kickable sessions & trusted-2FA epoch)
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS user_sessions (
			id%s PRIMARY KEY,
			user_id%s NOT NULL,
			token_hash%s NOT NULL UNIQUE,
			ip_masked%s NOT NULL,
			geo_region%s NOT NULL,
			user_agent%s NOT NULL,
			totp_verified_until%s NOT NULL DEFAULT 0,
			expires_at%s NOT NULL,
			last_seen_at%s NOT NULL,
			created_at%s NOT NULL
		)%s`, t.Varchar32, t.Varchar32, t.Varchar64, t.Varchar64, t.Varchar128, t.Varchar255,
			t.BigInt, t.BigInt, t.BigInt, t.BigInt, sfx),
		`CREATE INDEX idx_user_sessions_user_exp ON user_sessions (user_id, expires_at)`,

		// 5. Fine-grained scoped API tokens (Obsidian / PicGo / CLI)
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS api_tokens (
			id%s PRIMARY KEY,
			user_id%s NOT NULL,
			name%s NOT NULL,
			token_prefix%s NOT NULL,
			token_hash%s NOT NULL UNIQUE,
			scopes_json%s NOT NULL,
			last_used_at%s NOT NULL DEFAULT 0,
			expires_at%s NOT NULL DEFAULT 0,
			created_at%s NOT NULL
		)%s`, t.Varchar32, t.Varchar32, t.Varchar128, t.Varchar32, t.Varchar64, t.Text,
			t.BigInt, t.BigInt, t.BigInt, sfx),
		`CREATE INDEX idx_api_tokens_user ON api_tokens (user_id)`,

		// 6. Direct Primary Categories
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS categories (
			id%s PRIMARY KEY,
			name%s NOT NULL,
			slug%s NOT NULL UNIQUE,
			description%s NOT NULL,
			icon%s NOT NULL,
			sort_order%s NOT NULL DEFAULT 0,
			created_at%s NOT NULL,
			updated_at%s NOT NULL
		)%s`, t.Varchar32, t.Varchar128, t.Varchar128, t.Text, t.Varchar255, t.Int, t.BigInt, t.BigInt, sfx),
		`CREATE INDEX idx_categories_order ON categories (sort_order)`,

		// 7. Free-form Tags
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS tags (
			id%s PRIMARY KEY,
			name%s NOT NULL UNIQUE,
			slug%s NOT NULL UNIQUE,
			created_at%s NOT NULL
		)%s`, t.Varchar32, t.Varchar128, t.Varchar128, t.BigInt, sfx),

		// 8. Unified Articles (Published / Draft / Scheduled, direct category_id, podcast, i18n)
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS articles (
			id%s PRIMARY KEY,
			author_id%s NOT NULL,
			category_id%s NOT NULL,
			title%s NOT NULL,
			slug%s NOT NULL UNIQUE,
			legacy_permalink%s NOT NULL,
			summary_md%s NOT NULL,
			content_md%s NOT NULL,
			cover_url%s NOT NULL,
			status%s NOT NULL,
			published_at%s NOT NULL DEFAULT 0,
			scheduled_at%s NOT NULL DEFAULT 0,
			is_pinned%s NOT NULL DEFAULT %s,
			allow_comment%s NOT NULL DEFAULT %s,
			password_ciphertext%s NOT NULL,
			has_content_lock%s NOT NULL DEFAULT %s,
			audio_url%s NOT NULL,
			audio_duration_sec%s NOT NULL DEFAULT 0,
			audio_size_bytes%s NOT NULL DEFAULT 0,
			audio_mime%s NOT NULL,
			lang%s NOT NULL,
			translation_group%s NOT NULL,
			license_override%s NOT NULL,
			word_count%s NOT NULL DEFAULT 0,
			reading_minutes%s NOT NULL DEFAULT 0,
			view_count%s NOT NULL DEFAULT 0,
			comment_count%s NOT NULL DEFAULT 0,
			created_at%s NOT NULL,
			updated_at%s NOT NULL
		)%s`,
			t.Varchar32, t.Varchar32, t.Varchar32, t.Varchar255, vk, vk,
			t.Text, t.LongText, t.Varchar512, t.Varchar32, t.BigInt, t.BigInt,
			t.Bool, t.BoolFalseLit, t.Bool, t.BoolTrueLit,
			t.Text, t.Bool, t.BoolFalseLit,
			t.Varchar512, t.Int, t.BigInt, t.Varchar64,
			t.Varchar32, t.Varchar128, t.Text,
			t.Int, t.Int, t.BigInt, t.Int, t.BigInt, t.BigInt, sfx),
		`CREATE INDEX idx_articles_status_pub ON articles (status, is_pinned, published_at)`,
		`CREATE INDEX idx_articles_category ON articles (category_id, status, published_at)`,
		`CREATE INDEX idx_articles_author ON articles (author_id, status, updated_at)`,
		`CREATE INDEX idx_articles_legacy_link ON articles (legacy_permalink)`,
		`CREATE INDEX idx_articles_scheduled ON articles (status, scheduled_at)`,
		`CREATE INDEX idx_articles_trans_group ON articles (translation_group, lang)`,

		// 9. Article <-> Tag join table
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS article_tags (
			article_id%s NOT NULL,
			tag_id%s NOT NULL,
			PRIMARY KEY (article_id, tag_id)
		)%s`, t.Varchar32, t.Varchar32, sfx),
		`CREATE INDEX idx_article_tags_tag ON article_tags (tag_id)`,

		// 10. Rolling Article Revisions
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS article_revisions (
			id%s PRIMARY KEY,
			article_id%s NOT NULL,
			title%s NOT NULL,
			summary_md%s NOT NULL,
			content_md%s NOT NULL,
			author_name%s NOT NULL,
			note%s NOT NULL,
			created_at%s NOT NULL
		)%s`, t.Varchar32, t.Varchar32, t.Varchar255, t.Text, t.LongText, t.Varchar128, t.Varchar255, t.BigInt, sfx),
		`CREATE INDEX idx_article_revisions_art ON article_revisions (article_id, created_at)`,

		// 11. Bidirectional Backlinks graph
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS article_backlinks (
			source_article_id%s NOT NULL,
			target_article_id%s NOT NULL,
			updated_at%s NOT NULL,
			PRIMARY KEY (source_article_id, target_article_id)
		)%s`, t.Varchar32, t.Varchar32, t.BigInt, sfx),
		`CREATE INDEX idx_article_backlinks_target ON article_backlinks (target_article_id, updated_at)`,

		// 12. True Standalone Pages (decoupled from blog timeline & header nav)
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS pages (
			id%s PRIMARY KEY,
			author_id%s NOT NULL,
			title%s NOT NULL,
			slug%s NOT NULL UNIQUE,
			content_md%s NOT NULL,
			is_published%s NOT NULL DEFAULT %s,
			allow_comment%s NOT NULL DEFAULT %s,
			show_toc%s NOT NULL DEFAULT %s,
			lang%s NOT NULL,
			translation_group%s NOT NULL,
			view_count%s NOT NULL DEFAULT 0,
			comment_count%s NOT NULL DEFAULT 0,
			created_at%s NOT NULL,
			updated_at%s NOT NULL
		)%s`, t.Varchar32, t.Varchar32, t.Varchar255, vk, t.LongText,
			t.Bool, t.BoolTrueLit, t.Bool, t.BoolTrueLit, t.Bool, t.BoolTrueLit,
			t.Varchar32, t.Varchar128, t.BigInt, t.Int, t.BigInt, t.BigInt, sfx),
		`CREATE INDEX idx_pages_trans_group ON pages (translation_group, lang)`,

		// 13. Header Navigation Menu Items
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS nav_items (
			id%s PRIMARY KEY,
			parent_id%s NOT NULL,
			title%s NOT NULL,
			i18n_titles_json%s NOT NULL,
			url%s NOT NULL,
			open_in_new_tab%s NOT NULL DEFAULT %s,
			icon%s NOT NULL,
			sort_order%s NOT NULL DEFAULT 0,
			is_enabled%s NOT NULL DEFAULT %s,
			created_at%s NOT NULL,
			updated_at%s NOT NULL
		)%s`, t.Varchar32, t.Varchar32, t.Varchar128, t.Text, t.Varchar512,
			t.Bool, t.BoolFalseLit, t.Varchar255, t.Int, t.Bool, t.BoolTrueLit, t.BigInt, t.BigInt, sfx),
		`CREATE INDEX idx_nav_items_order ON nav_items (sort_order)`,

		// 14. Series & Ordered Articles (replaces voloSeries chunks)
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS series (
			id%s PRIMARY KEY,
			title%s NOT NULL,
			slug%s NOT NULL UNIQUE,
			description%s NOT NULL,
			cover_url%s NOT NULL,
			sort_order%s NOT NULL DEFAULT 0,
			created_at%s NOT NULL,
			updated_at%s NOT NULL
		)%s`, t.Varchar32, t.Varchar255, vk, t.Text, t.Varchar512, t.Int, t.BigInt, t.BigInt, sfx),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS series_articles (
			series_id%s NOT NULL,
			article_id%s NOT NULL,
			sort_order%s NOT NULL DEFAULT 0,
			PRIMARY KEY (series_id, article_id)
		)%s`, t.Varchar32, t.Varchar32, t.Int, sfx),
		`CREATE INDEX idx_series_articles_art ON series_articles (article_id)`,

		// 15. Bookshelf & Recursive Chapter Tree (replaces voloBooks + voloBookToc.* chunks)
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS books (
			id%s PRIMARY KEY,
			title%s NOT NULL,
			slug%s NOT NULL UNIQUE,
			description%s NOT NULL,
			cover_url%s NOT NULL,
			source_type%s NOT NULL,
			source_url%s NOT NULL,
			series_id%s NOT NULL,
			embed_mode%s NOT NULL,
			chapter_count%s NOT NULL DEFAULT 0,
			synced_at%s NOT NULL DEFAULT 0,
			sort_order%s NOT NULL DEFAULT 0,
			created_at%s NOT NULL,
			updated_at%s NOT NULL
		)%s`, t.Varchar32, t.Varchar255, vk, t.Text, t.Varchar512, t.Varchar32, t.Varchar512,
			t.Varchar32, t.Varchar32, t.Int, t.BigInt, t.Int, t.BigInt, t.BigInt, sfx),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS book_chapters (
			id%s PRIMARY KEY,
			book_id%s NOT NULL,
			parent_id%s NOT NULL,
			title%s NOT NULL,
			url%s NOT NULL,
			depth%s NOT NULL DEFAULT 0,
			sort_order%s NOT NULL DEFAULT 0
		)%s`, t.Varchar32, t.Varchar32, t.Varchar32, t.Varchar255, t.Varchar512, t.Int, t.Int, sfx),
		`CREATE INDEX idx_book_chapters_book ON book_chapters (book_id, sort_order)`,

		// 16. Polymorphic Comments (article / page, GuestPassport & env_fp support)
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS comments (
			id%s PRIMARY KEY,
			target_type%s NOT NULL,
			target_id%s NOT NULL,
			parent_id%s NOT NULL,
			reply_to_name%s NOT NULL,
			user_id%s NOT NULL,
			guest_id_hash%s NOT NULL,
			env_fp_hash%s NOT NULL,
			author_name%s NOT NULL,
			author_url%s NOT NULL,
			avatar_url%s NOT NULL,
			notify_email_ciphertext%s NOT NULL,
			notify_locale%s NOT NULL,
			client_ip%s NOT NULL,
			ip_masked%s NOT NULL,
			geo_region%s NOT NULL,
			content_md%s NOT NULL,
			status%s NOT NULL,
			is_deleted%s NOT NULL DEFAULT %s,
			created_at%s NOT NULL,
			updated_at%s NOT NULL
		)%s`, t.Varchar32, t.Varchar32, t.Varchar32, t.Varchar32, t.Varchar128,
			t.Varchar32, t.Varchar64, t.Varchar64, t.Varchar128, t.Varchar512, t.Varchar512,
			t.Text, t.Varchar32, t.Varchar64, t.Varchar64, t.Varchar128,
			t.Text, t.Varchar32, t.Bool, t.BoolFalseLit, t.BigInt, t.BigInt, sfx),
		`CREATE INDEX idx_comments_target_status ON comments (target_type, target_id, status, created_at)`,
		`CREATE INDEX idx_comments_status_created ON comments (status, created_at)`,
		`CREATE INDEX idx_comments_guest ON comments (guest_id_hash)`,
		`CREATE INDEX idx_comments_user ON comments (user_id)`,

		// 17. Reactions / Likes (deduplicated via env_fp_hash or user_id)
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS reactions (
			target_type%s NOT NULL,
			target_id%s NOT NULL,
			actor_hash%s NOT NULL,
			kind%s NOT NULL,
			created_at%s NOT NULL,
			PRIMARY KEY (target_type, target_id, actor_hash, kind)
		)%s`, t.Varchar32, t.Varchar32, t.Varchar64, t.Varchar32, t.BigInt, sfx),

		// 18. Friend Links (with integrated health patrol fields) & Follow Subscriptions
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS links (
			id%s PRIMARY KEY,
			title%s NOT NULL,
			url%s NOT NULL,
			description%s NOT NULL,
			icon_url%s NOT NULL,
			category_name%s NOT NULL,
			sort_order%s NOT NULL DEFAULT 0,
			health_status%s NOT NULL,
			http_code%s NOT NULL DEFAULT 0,
			latency_ms%s NOT NULL DEFAULT 0,
			health_error%s NOT NULL,
			last_checked_at%s NOT NULL DEFAULT 0,
			created_at%s NOT NULL,
			updated_at%s NOT NULL
		)%s`, t.Varchar32, t.Varchar255, t.Varchar512, t.Text, t.Varchar512, t.Varchar64,
			t.Int, t.Varchar32, t.Int, t.Int, t.Varchar255, t.BigInt, t.BigInt, t.BigInt, sfx),
		`CREATE INDEX idx_links_order ON links (sort_order)`,

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS follows (
			id%s PRIMARY KEY,
			title%s NOT NULL,
			site_url%s NOT NULL,
			feed_url%s NOT NULL,
			description%s NOT NULL,
			icon_url%s NOT NULL,
			sort_order%s NOT NULL DEFAULT 0,
			last_synced_at%s NOT NULL DEFAULT 0,
			sync_error%s NOT NULL,
			created_at%s NOT NULL,
			updated_at%s NOT NULL
		)%s`, t.Varchar32, t.Varchar255, t.Varchar512, t.Varchar512, t.Text, t.Varchar512,
			t.Int, t.BigInt, t.Varchar255, t.BigInt, t.BigInt, sfx),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS follow_items (
			id%s PRIMARY KEY,
			follow_id%s NOT NULL,
			guid_hash%s NOT NULL,
			title%s NOT NULL,
			url%s NOT NULL,
			summary%s NOT NULL,
			author_name%s NOT NULL,
			published_at%s NOT NULL,
			UNIQUE (follow_id, guid_hash)
		)%s`, t.Varchar32, t.Varchar32, t.Varchar64, t.Varchar255, t.Varchar512, t.Text, t.Varchar128, t.BigInt, sfx),
		`CREATE INDEX idx_follow_items_pub ON follow_items (published_at)`,

		// 19. Privacy-safe Media Assets Library (with width/height for zero CLS)
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS media_assets (
			id%s PRIMARY KEY,
			storage_driver%s NOT NULL,
			object_key%s NOT NULL UNIQUE,
			public_url%s NOT NULL,
			original_name%s NOT NULL,
			mime_type%s NOT NULL,
			size_bytes%s NOT NULL,
			width%s NOT NULL DEFAULT 0,
			height%s NOT NULL DEFAULT 0,
			sha256%s NOT NULL,
			exif_stripped%s NOT NULL DEFAULT %s,
			uploader_id%s NOT NULL,
			created_at%s NOT NULL
		)%s`, t.Varchar32, t.Varchar32, vk, t.Varchar512, t.Varchar255, t.Varchar64,
			t.BigInt, t.Int, t.Int, t.Varchar64, t.Bool, t.BoolTrueLit, t.Varchar32, t.BigInt, sfx),
		`CREATE INDEX idx_media_assets_created ON media_assets (created_at)`,

		// 20. 301/302 Redirects & Legacy Permalink Guardian
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS redirects (
			id%s PRIMARY KEY,
			source_path%s NOT NULL UNIQUE,
			target_path%s NOT NULL,
			status_code%s NOT NULL DEFAULT 301,
			is_system_legacy%s NOT NULL DEFAULT %s,
			hit_count%s NOT NULL DEFAULT 0,
			created_at%s NOT NULL
		)%s`, t.Varchar32, vk, t.Varchar512, t.Int, t.Bool, t.BoolFalseLit, t.BigInt, t.BigInt, sfx),

		// 21. Webhooks, Zero-PII Daily Analytics, Security Rules & Audit Logs
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS webhooks (
			id%s PRIMARY KEY,
			name%s NOT NULL,
			provider%s NOT NULL,
			url%s NOT NULL,
			secret_ciphertext%s NOT NULL,
			chat_id%s NOT NULL,
			events_json%s NOT NULL,
			is_enabled%s NOT NULL DEFAULT %s,
			last_status_code%s NOT NULL DEFAULT 0,
			last_error%s NOT NULL,
			last_triggered_at%s NOT NULL DEFAULT 0,
			created_at%s NOT NULL,
			updated_at%s NOT NULL
		)%s`, t.Varchar32, t.Varchar128, t.Varchar32, t.Varchar512, t.Text, t.Varchar128,
			t.Text, t.Bool, t.BoolTrueLit, t.Int, t.Varchar255, t.BigInt, t.BigInt, t.BigInt, sfx),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS analytics_daily (
			stat_date%s PRIMARY KEY,
			pv%s NOT NULL DEFAULT 0,
			uv%s NOT NULL DEFAULT 0,
			updated_at%s NOT NULL
		)%s`, t.Varchar32, t.BigInt, t.BigInt, t.BigInt, sfx),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS analytics_top_items (
			stat_date%s NOT NULL,
			kind%s NOT NULL,
			item_key%s NOT NULL,
			hits%s NOT NULL DEFAULT 0,
			PRIMARY KEY (stat_date, kind, item_key)
		)%s`, t.Varchar32, t.Varchar32, vk, t.BigInt, sfx),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS security_ip_bans (
			id%s PRIMARY KEY,
			cidr%s NOT NULL UNIQUE,
			source%s NOT NULL,
			reason_code%s NOT NULL,
			reason%s NOT NULL,
			note%s NOT NULL,
			occurrences%s NOT NULL DEFAULT 1,
			expires_at%s NOT NULL DEFAULT 0,
			created_at%s NOT NULL,
			updated_at%s NOT NULL
		)%s`, t.Varchar32, t.Varchar64, t.Varchar32, t.Varchar64, t.Text, t.Text, t.Int, t.BigInt, t.BigInt, t.BigInt, sfx),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS security_geo_rules (
			id%s PRIMARY KEY,
			name%s NOT NULL,
			is_enabled%s NOT NULL DEFAULT %s,
			action%s NOT NULL,
			on_miss%s NOT NULL,
			countries%s NOT NULL,
			regions%s NOT NULL,
			sort_order%s NOT NULL DEFAULT 0,
			created_at%s NOT NULL,
			updated_at%s NOT NULL
		)%s`, t.Varchar32, t.Varchar128, t.Bool, t.BoolTrueLit, t.Varchar32, t.Varchar32,
			t.Text, t.Text, t.Int, t.BigInt, t.BigInt, sfx),
		`CREATE INDEX idx_security_geo_rules_order ON security_geo_rules (sort_order)`,

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS audit_logs (
			id%s PRIMARY KEY,
			actor_id%s NOT NULL,
			actor_name%s NOT NULL,
			action%s NOT NULL,
			resource_type%s NOT NULL,
			resource_id%s NOT NULL,
			client_ip%s NOT NULL,
			detail_json%s NOT NULL,
			created_at%s NOT NULL
		)%s`, t.Varchar32, t.Varchar32, t.Varchar128, t.Varchar64, t.Varchar64, t.Varchar64, t.Varchar64, t.Text, t.BigInt, sfx),
		`CREATE INDEX idx_audit_logs_created ON audit_logs (created_at)`,

		// 22. Plugins & Cross-DB Persistent plugin_kv (with TTL & atomic counters)
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS plugins (
			id%s PRIMARY KEY,
			kind%s NOT NULL,
			version%s NOT NULL,
			is_enabled%s NOT NULL DEFAULT %s,
			config_json%s NOT NULL,
			secret_ciphertext%s NOT NULL,
			circuit_state%s NOT NULL,
			failure_count%s NOT NULL DEFAULT 0,
			last_error%s NOT NULL,
			installed_at%s NOT NULL,
			updated_at%s NOT NULL
		)%s`, t.Varchar64, t.Varchar32, t.Varchar32, t.Bool, t.BoolFalseLit,
			t.Text, t.Text, t.Varchar32, t.Int, t.Text, t.BigInt, t.BigInt, sfx),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS plugin_kv (
			plugin_id%s NOT NULL,
			k%s NOT NULL,
			value_text%s NOT NULL,
			value_int%s NOT NULL DEFAULT 0,
			expires_at%s NOT NULL DEFAULT 0,
			updated_at%s NOT NULL,
			PRIMARY KEY (plugin_id, k)
		)%s`, t.Varchar64, vk, t.LongText, t.BigInt, t.BigInt, t.BigInt, sfx),
		`CREATE INDEX idx_plugin_kv_exp ON plugin_kv (expires_at)`,
	}
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
	"analytics_daily",
	"analytics_top_items",
	"security_ip_bans",
	"security_geo_rules",
	"audit_logs",
	"plugins",
	"plugin_kv",
}

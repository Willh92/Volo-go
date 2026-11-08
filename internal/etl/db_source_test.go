package etl

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/repo"
)

func TestImportSourceDatabaseSoloSchemaAndReadOnlySource(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	// 1. Seed a realistic legacy Solo/Bolo SQLite relational database with b3_solo_* tables
	// using mixed lowercase / snake_case / UPPERCASE column names (mirroring Postgres/MySQL ORM quirks).
	srcPath := filepath.Join(tmp, "legacy_solo.db")
	seedStore, err := sqlx.Open(ctx, sqlx.Options{Driver: "sqlite", DSN: srcPath})
	if err != nil {
		t.Fatalf("open seed sqlite: %v", err)
	}
	ddls := []string{
		`CREATE TABLE b3_solo_option (oid TEXT PRIMARY KEY, option_category TEXT, option_value TEXT)`,
		`CREATE TABLE b3_solo_user (OID TEXT PRIMARY KEY, user_name TEXT, user_email TEXT, user_role TEXT, user_b3_key TEXT, user_url TEXT, user_status INTEGER, user_created TEXT)`,
		`CREATE TABLE b3_solo_category (oid TEXT PRIMARY KEY, categorytitle TEXT, categoryuri TEXT, categorydescription TEXT, categoryorder INTEGER)`,
		`CREATE TABLE b3_solo_tag (oid TEXT PRIMARY KEY, tag_title TEXT)`,
		`CREATE TABLE b3_solo_category_tag (oid TEXT PRIMARY KEY, category_oid TEXT, tag_oid TEXT)`,
		`CREATE TABLE b3_solo_tag_article (oid TEXT PRIMARY KEY, article_oid TEXT, tag_oid TEXT)`,
		`CREATE TABLE b3_solo_article (
			oid TEXT PRIMARY KEY,
			articletitle TEXT,
			articleabstract TEXT,
			articlecontent TEXT,
			articleauthorid TEXT,
			articlepermalink TEXT,
			articlestatus INTEGER,
			articlecommentable TEXT,
			articleputtop BLOB,
			articlecreatedate TEXT,
			articleupdatedate TEXT,
			articleimg1url TEXT
		)`,
		`CREATE TABLE b3_solo_article_revision (
			oid TEXT PRIMARY KEY,
			articleoid TEXT,
			revisiontitle TEXT,
			revisionabstract TEXT,
			revisioncontent TEXT,
			revisionauthorname TEXT,
			revisioncreated INTEGER
		)`,
		`CREATE TABLE b3_solo_comment (
			oid TEXT PRIMARY KEY,
			commentonid TEXT,
			commentname TEXT,
			commentemail TEXT,
			commenturl TEXT,
			commentcontent TEXT,
			commentdate TEXT,
			commentstatus INTEGER
		)`,
		`CREATE TABLE b3_solo_link (oid TEXT PRIMARY KEY, linktitle TEXT, linkaddress TEXT, linkdescription TEXT, linkorder INTEGER)`,
	}
	for _, ddl := range ddls {
		if _, err := seedStore.ExecContext(ctx, ddl); err != nil {
			t.Fatalf("exec ddl %q: %v", ddl, err)
		}
	}
	_, _ = seedStore.ExecContext(ctx, `INSERT INTO b3_solo_option VALUES ('blogTitle', 'preference', 'Old Solo Blog'), ('blogSubtitle', 'preference', 'Direct DB Stream'), ('version', 'preference', '4.4.0')`)
	_, _ = seedStore.ExecContext(ctx, `INSERT INTO b3_solo_user VALUES ('u1', 'solo_owner', 'owner@old.example', 'adminRole', '0123456789abcdef0123456789abcdef', 'https://old.example.com', 0, '2024-03-10 08:15:00')`)
	_, _ = seedStore.ExecContext(ctx, `INSERT INTO b3_solo_category VALUES ('c1', 'Backend', 'backend', 'Go & SQL', 1)`)
	_, _ = seedStore.ExecContext(ctx, `INSERT INTO b3_solo_tag VALUES ('t1', 'Golang')`)
	_, _ = seedStore.ExecContext(ctx, `INSERT INTO b3_solo_category_tag VALUES ('ct1', 'c1', 't1')`)
	_, _ = seedStore.ExecContext(ctx, `INSERT INTO b3_solo_tag_article VALUES ('ta1', 'a1', 't1')`)
	_, _ = seedStore.ExecContext(ctx,
		`INSERT INTO b3_solo_article VALUES ('a1', 'Direct DB Migration Guide', 'Summary here', 'See ![hero](https://old.example.com/image/hero.png)', 'u1', '/articles/2024/03/10/direct-db.html', 0, 'true', ?, '2024-03-10 09:00:00', '2024-03-11 10:30:00', 'https://old.example.com/image/hero.png')`,
		[]byte{1},
	)
	_, _ = seedStore.ExecContext(ctx, `INSERT INTO b3_solo_article_revision VALUES ('r1', 'a1', 'Draft v1', 'Summary', 'Initial draft', 'solo_owner', 1710061200000)`)
	_, _ = seedStore.ExecContext(ctx, `INSERT INTO b3_solo_comment VALUES ('cm1', 'a1', 'Reader One', 'reader@example.com', 'https://reader.example', 'Awesome!', '2024-03-12 12:00:00', 0)`)
	_, _ = seedStore.ExecContext(ctx, `INSERT INTO b3_solo_link VALUES ('l1', 'Friend Blog', 'https://friend.example', 'Peer', 1)`)
	_ = seedStore.Close()

	// 2. Open legacy DB strictly read-only via sqlx.OpenReadOnly
	srcRO, err := sqlx.OpenReadOnly(ctx, sqlx.Options{Driver: "sqlite", DSN: srcPath})
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer srcRO.Close()

	// Verify read-only enforcement rejects writes
	if _, wErr := srcRO.ExecContext(ctx, `DELETE FROM b3_solo_article`); wErr == nil {
		t.Fatalf("expected OpenReadOnly store to reject DELETE write")
	}

	// 3. Inspect source database auto-detection
	insp, err := InspectSourceDatabase(ctx, srcRO, "")
	if err != nil {
		t.Fatalf("InspectSourceDatabase: %v", err)
	}
	if insp.SchemaKind != SchemaKindSolo || insp.TablePrefix != "b3_solo_" || insp.Articles != 1 || insp.Comments != 1 || insp.Users != 1 {
		t.Fatalf("unexpected inspection: %+v", insp)
	}

	// 4. Open fresh target Volo-go database
	targetPath := filepath.Join(tmp, "volo_new.db")
	targetStore, err := sqlx.Open(ctx, sqlx.Options{Driver: "sqlite", DSN: targetPath})
	if err != nil {
		t.Fatalf("open target: %v", err)
	}
	defer targetStore.Close()
	if err := migrate.Up(ctx, targetStore); err != nil {
		t.Fatalf("migrate target: %v", err)
	}

	kr, _, err := config.LoadOrInitKeyRing(strings.Repeat("b", 64), tmp)
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	settingsMgr := repo.NewSettingsManager(targetStore, kr, "zh-CN")

	opts := ImportOptions{
		OldURLPrefixes: []string{"https://old.example.com"},
		NewSiteURL:     "https://new.example.com",
		SiteTitle:      "Migrated Volo Studio",
		AdminUsername:  "will",
	}

	// 5. Dry-Run first: zero target rows written
	opts.DryRun = true
	dryRep, _, err := ImportSourceDatabase(ctx, srcRO, targetStore, settingsMgr, kr, "", opts)
	if err != nil {
		t.Fatalf("DryRun ImportSourceDatabase: %v", err)
	}
	if !dryRep.DryRun || dryRep.Articles != 1 || dryRep.Revisions != 1 || dryRep.Comments != 1 {
		t.Fatalf("unexpected dry report: %+v", dryRep)
	}
	artsBefore, _ := sqlx.Select[domain.RawArticle](ctx, targetStore, `SELECT * FROM articles`)
	if len(artsBefore) != 0 {
		t.Fatalf("DryRun wrote %d articles", len(artsBefore))
	}

	// 6. Full direct DB-to-DB migration
	opts.DryRun = false
	rep, _, err := ImportSourceDatabase(ctx, srcRO, targetStore, settingsMgr, kr, "", opts)
	if err != nil {
		t.Fatalf("ImportSourceDatabase: %v", err)
	}
	if rep.Articles != 1 || rep.Revisions != 1 || rep.Comments != 1 || rep.Links != 1 || rep.RedirectsCreated < 1 {
		t.Fatalf("unexpected migration report: %+v", rep)
	}

	arts, err := sqlx.Select[domain.RawArticle](ctx, targetStore, `SELECT * FROM articles`)
	if err != nil || len(arts) != 1 {
		t.Fatalf("select migrated article: %v (%d)", err, len(arts))
	}
	if arts[0].Title != "Direct DB Migration Guide" || !arts[0].IsPinned || !strings.Contains(arts[0].ContentMD, "https://new.example.com") {
		t.Fatalf("unexpected migrated article fields: %+v", arts[0])
	}
	if arts[0].CreatedAt <= 0 {
		t.Fatalf("expected parsed SQL datetime CreatedAt > 0, got %d", arts[0].CreatedAt)
	}
	if gotTitle := settingsMgr.Snapshot().Site.Title; gotTitle != "Migrated Volo Studio" {
		t.Fatalf("expected overridden site title, got %q", gotTitle)
	}
}

func TestImportSourceDatabaseNativeVoloCrossDB(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	// Seed an initialized Volo-go source DB
	srcPath := filepath.Join(tmp, "volo_old.db")
	srcSeed, err := sqlx.Open(ctx, sqlx.Options{Driver: "sqlite", DSN: srcPath})
	if err != nil {
		t.Fatalf("open source volo db: %v", err)
	}
	if err := migrate.Up(ctx, srcSeed); err != nil {
		t.Fatalf("migrate source volo db: %v", err)
	}
	kr, _, _ := config.LoadOrInitKeyRing(strings.Repeat("c", 64), tmp)
	srcSettings := repo.NewSettingsManager(srcSeed, kr, "zh-CN")
	_, _, _ = srcSettings.PatchDomain(ctx, domain.SettingsDomainSite, []byte(`{"initialized":true,"title":"Source Volo Site","subtitle":"Old Instance","default_locale":"zh-CN","timezone":"Asia/Shanghai"}`), 0)
	now := time.Now().UnixMilli()
	if _, err := srcSeed.ExecContext(ctx,
		`INSERT INTO users (id, username, email, display_name, avatar_url, bio, website_url, role, password_hash, legacy_hash, status, email_verified_at, preferred_locale, created_at, updated_at) VALUES (?, ?, ?, ?, '', '', '', ?, ?, '', ?, 0, 'zh-CN', ?, ?)`,
		"u_admin", "oldadmin", "old@volo.dev", "Old Admin", domain.RoleAdmin, "argon2id$dummy", domain.UserStatusActive, now, now); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := srcSeed.ExecContext(ctx,
		`INSERT INTO articles (id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md, cover_url, status, published_at, scheduled_at, is_pinned, allow_comment, password_ciphertext, has_content_lock, audio_url, audio_duration_sec, audio_size_bytes, audio_mime, lang, translation_group, license_override, word_count, reading_minutes, view_count, comment_count, created_at, updated_at) VALUES (?, ?, '', ?, ?, '', '', ?, '', ?, ?, 0, 0, 1, '', 0, '', 0, 0, '', 'zh-CN', '', '', 10, 1, 0, 0, ?, ?)`,
		"art_1", "u_admin", "Hello Cross DB", "hello-cross-db", "# Hello", domain.ArticleStatusPublished, now, now, now); err != nil {
		t.Fatalf("seed article: %v", err)
	}
	_ = srcSeed.Close()

	srcRO, err := sqlx.OpenReadOnly(ctx, sqlx.Options{Driver: "sqlite", DSN: srcPath})
	if err != nil {
		t.Fatalf("OpenReadOnly source volo: %v", err)
	}
	defer srcRO.Close()

	insp, err := InspectSourceDatabase(ctx, srcRO, "")
	if err != nil {
		t.Fatalf("InspectSourceDatabase volo: %v", err)
	}
	if insp.SchemaKind != SchemaKindVolo || insp.Articles != 1 || insp.Users != 1 {
		t.Fatalf("expected volo schema kind, got %+v", insp)
	}

	targetPath := filepath.Join(tmp, "volo_dest.db")
	targetStore, err := sqlx.Open(ctx, sqlx.Options{Driver: "sqlite", DSN: targetPath})
	if err != nil {
		t.Fatalf("open dest: %v", err)
	}
	defer targetStore.Close()
	if err := migrate.Up(ctx, targetStore); err != nil {
		t.Fatalf("migrate dest: %v", err)
	}
	destSettings := repo.NewSettingsManager(targetStore, kr, "zh-CN")

	rep, _, err := ImportSourceDatabase(ctx, srcRO, targetStore, destSettings, kr, "", ImportOptions{
		SiteTitle:     "New Prod Volo",
		AdminUsername: "newadmin",
	})
	if err != nil {
		t.Fatalf("ImportSourceDatabase volo->volo: %v", err)
	}
	if rep.Articles != 1 || rep.Users != 1 {
		t.Fatalf("unexpected volo->volo report: %+v", rep)
	}
	if destSettings.Snapshot().Site.Title != "New Prod Volo" || !destSettings.Snapshot().Site.Initialized {
		t.Fatalf("expected dest settings initialized with overridden title, got %+v", destSettings.Snapshot().Site)
	}
	users, _ := sqlx.Select[domain.User](ctx, targetStore, `SELECT * FROM users WHERE id = ?`, "u_admin")
	if len(users) != 1 || users[0].Username != "newadmin" {
		t.Fatalf("expected overridden admin username, got %+v", users)
	}
}

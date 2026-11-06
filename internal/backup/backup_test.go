package backup

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
)

func openTestSQLiteStore(t *testing.T, path string) *sqlx.Store {
	t.Helper()
	store, err := sqlx.Open(context.Background(), sqlx.Options{
		Driver: "sqlite",
		DSN:    path,
	})
	if err != nil {
		t.Fatalf("open sqlite store %s: %v", path, err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := migrate.Up(context.Background(), store); err != nil {
		t.Fatalf("migrate sqlite store %s: %v", path, err)
	}
	return store
}

func TestBackupCreateDryRunRestoreAndIntegrityProtection(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	srcDB := openTestSQLiteStore(t, filepath.Join(tmp, "src.db"))
	srcUploads := filepath.Join(tmp, "src-uploads")
	if err := os.MkdirAll(filepath.Join(srcUploads, "2026/11"), 0o750); err != nil {
		t.Fatal(err)
	}
	imgBytes := []byte("fake-webp-binary-payload-1234567890")
	if err := os.WriteFile(filepath.Join(srcUploads, "2026/11/hero.webp"), imgBytes, 0o640); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UnixMilli()
	_, err := srcDB.ExecContext(ctx, `INSERT INTO users
		(id, username, email, display_name, avatar_url, bio, website_url, role, password_hash, legacy_hash, status, email_verified_at, preferred_locale, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"u_admin_1", "alice", "alice@example.com", "Alice", "", "Founder", "https://alice.dev",
		domain.RoleAdmin, "$argon2id$v=19$m=65536,t=3,p=2$salt$hash", "", domain.UserStatusActive, now, "zh-CN", now, now,
	)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}

	_, err = srcDB.ExecContext(ctx, `INSERT INTO categories (id, name, slug, description, icon, sort_order, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"cat_1", "Engineering", "engineering", "Systems & Go", "", 1, now, now,
	)
	if err != nil {
		t.Fatalf("seed category: %v", err)
	}

	_, err = srcDB.ExecContext(ctx, `INSERT INTO articles
		(id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md, cover_url, status, published_at, scheduled_at, is_pinned, allow_comment, password_ciphertext, has_content_lock, audio_url, audio_duration_sec, audio_size_bytes, audio_mime, lang, translation_group, license_override, word_count, reading_minutes, view_count, comment_count, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"art_1", "u_admin_1", "cat_1", "Pure Go Engine", "pure-go-engine", "/articles/2026/11/pure-go-engine.html",
		"Summary", "# Hello Volo-go\n\nStreaming backup test.", "/uploads/2026/11/hero.webp",
		domain.ArticleStatusPublished, now, 0, true, true, "", false, "", 0, 0, "", "zh-CN", "tg_1", "", 120, 1, 42, 0, now, now,
	)
	if err != nil {
		t.Fatalf("seed article: %v", err)
	}

	_, err = srcDB.ExecContext(ctx, `INSERT INTO plugin_kv (plugin_id, k, value_text, value_int, expires_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"sample-widget", "stats", `{"pats":9}`, 9, 0, now,
	)
	if err != nil {
		t.Fatalf("seed plugin_kv: %v", err)
	}

	archivePath := filepath.Join(tmp, "backups", "volo-backup.zip")
	manifest, err := CreateToFile(ctx, srcDB, archivePath, CreateOptions{
		IncludeMedia: true,
		UploadDir:    srcUploads,
	})
	if err != nil {
		t.Fatalf("CreateToFile failed: %v", err)
	}
	if manifest.Format != BackupFormatVersion || !manifest.IncludesMedia {
		t.Fatalf("unexpected manifest metadata: %+v", manifest)
	}

	dstDB := openTestSQLiteStore(t, filepath.Join(tmp, "dst.db"))
	// Pre-populate sentinel row in dstDB to verify rollback if tampered archive fails
	_, err = dstDB.ExecContext(ctx, `INSERT INTO tags (id, name, slug, created_at) VALUES (?, ?, ?, ?)`,
		"sentinel_tag", "Sentinel", "sentinel", now)
	if err != nil {
		t.Fatalf("seed sentinel tag: %v", err)
	}

	// 1. DryRun must validate without mutating dstDB
	drySummary, err := RestoreFromFile(ctx, dstDB, archivePath, RestoreOptions{DryRun: true})
	if err != nil {
		t.Fatalf("DryRun RestoreFromFile failed: %v", err)
	}
	if !drySummary.DryRun || drySummary.Tables["articles"] != 1 || drySummary.MediaFiles != 1 {
		t.Fatalf("unexpected dry-run summary: %+v", drySummary)
	}
	sentinelTags, err := sqlx.Select[domain.Tag](ctx, dstDB, `SELECT * FROM tags WHERE id = ?`, "sentinel_tag")
	if err != nil || len(sentinelTags) != 1 {
		t.Fatalf("DryRun mutated target database: %v / %+v", err, sentinelTags)
	}

	// 2. Tampered archive must fail SHA-256 verification BEFORE touching dstDB
	tamperedPath := filepath.Join(tmp, "tampered.zip")
	createTamperedCopy(t, archivePath, tamperedPath)
	if _, err := RestoreFromFile(ctx, dstDB, tamperedPath, RestoreOptions{}); err == nil {
		t.Fatal("expected SHA-256 integrity error on tampered archive, got nil")
	}
	sentinelTags, _ = sqlx.Select[domain.Tag](ctx, dstDB, `SELECT * FROM tags WHERE id = ?`, "sentinel_tag")
	if len(sentinelTags) != 1 {
		t.Fatal("tampered restore mutated dstDB despite checksum failure")
	}

	// 3. Real restore into dstDB + dstUploads must succeed with 100% parity
	dstUploads := filepath.Join(tmp, "dst-uploads")
	sum, err := RestoreFromFile(ctx, dstDB, archivePath, RestoreOptions{
		UploadDir: dstUploads,
	})
	if err != nil {
		t.Fatalf("RestoreFromFile failed: %v", err)
	}
	if sum.Tables["users"] != 1 || sum.Tables["articles"] != 1 || sum.Tables["plugin_kv"] != 1 || sum.MediaFiles != 1 {
		t.Fatalf("unexpected restore counts: %+v", sum)
	}

	restoredArt, err := sqlx.Get[domain.RawArticle](ctx, dstDB, `SELECT * FROM articles WHERE id = ?`, "art_1")
	if err != nil {
		t.Fatalf("query restored article: %v", err)
	}
	if restoredArt.Slug != "pure-go-engine" || !restoredArt.IsPinned || restoredArt.CategoryID != "cat_1" {
		t.Fatalf("restored article mismatch: %+v", restoredArt)
	}

	restoredMedia, err := os.ReadFile(filepath.Join(dstUploads, "2026/11/hero.webp"))
	if err != nil || !bytes.Equal(restoredMedia, imgBytes) {
		t.Fatalf("restored media file mismatch: err=%v", err)
	}
}

func TestRestoreRejectsZipSlipTraversal(t *testing.T) {
	ctx := context.Background()
	store := openTestSQLiteStore(t, filepath.Join(t.TempDir(), "zipslip.db"))

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create("../../etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte("evil"))
	_ = zw.Close()

	raw := buf.Bytes()
	_, err = Restore(ctx, store, bytes.NewReader(raw), int64(len(raw)), RestoreOptions{})
	if err == nil || !strings.Contains(err.Error(), "path traversal") {
		t.Fatalf("expected path traversal rejection, got: %v", err)
	}
}

func createTamperedCopy(t *testing.T, srcZip, dstZip string) {
	t.Helper()
	raw, err := os.ReadFile(srcZip)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for _, zf := range zr.File {
		rc, err := zf.Open()
		if err != nil {
			t.Fatal(err)
		}
		var body bytes.Buffer
		_, _ = body.ReadFrom(rc)
		_ = rc.Close()

		data := body.Bytes()
		if zf.Name == "tables/articles.ndjson" {
			data = bytes.ReplaceAll(data, []byte("Pure Go Engine"), []byte("Tampered Title"))
		}
		w, err := zw.Create(zf.Name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(data)
	}
	_ = zw.Close()
	if err := os.WriteFile(dstZip, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

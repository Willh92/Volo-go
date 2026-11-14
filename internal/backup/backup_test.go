package backup

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
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

func TestExtensionsPluginsAndThemesBackupRestoreAndSecurityCaps(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	srcDB := openTestSQLiteStore(t, filepath.Join(tmp, "ext-src.db"))
	srcPlugins := filepath.Join(tmp, "src-plugins")
	srcThemes := filepath.Join(tmp, "src-themes")

	// Seed external plugin with JS, manifest, assets, plus symlink/.tmp*/.bak* that MUST be skipped.
	if err := os.MkdirAll(filepath.Join(srcPlugins, "callout-ext", "assets"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcPlugins, "callout-ext", "manifest.json"), []byte(`{"id":"callout-ext","version":"1.0.0"}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcPlugins, "callout-ext", "main.js"), []byte(`exports.onEnable = function(){};`), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcPlugins, "callout-ext", "assets", "icon.svg"), []byte(`<svg></svg>`), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcPlugins, "callout-ext", ".tmp-stage"), []byte(`skip-me`), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcPlugins, "callout-ext", ".bak-old"), []byte(`skip-me`), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcPlugins, "callout-ext", "draft.bak"), []byte(`skip-me`), 0o640); err != nil {
		t.Fatal(err)
	}
	outsideFile := filepath.Join(tmp, "outside-secret.txt")
	_ = os.WriteFile(outsideFile, []byte("SECRET_HOST_FILE"), 0o600)
	_ = os.Symlink(outsideFile, filepath.Join(srcPlugins, "callout-ext", "escape-link.txt"))
	_ = os.MkdirAll(filepath.Join(srcPlugins, ".tmp-staging-plugin"), 0o750)
	_ = os.WriteFile(filepath.Join(srcPlugins, ".tmp-staging-plugin", "main.js"), []byte("skip"), 0o640)

	// Seed external theme with HTML template and CSS.
	if err := os.MkdirAll(filepath.Join(srcThemes, "aurora-pro", "templates"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcThemes, "aurora-pro", "theme.json"), []byte(`{"id":"aurora-pro","version":"2.1.0"}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcThemes, "aurora-pro", "templates", "post.html"), []byte(`<article>{{.Title}}</article>`), 0o640); err != nil {
		t.Fatal(err)
	}

	zipPath := filepath.Join(tmp, "ext-backup.zip")
	mf, err := CreateToFile(ctx, srcDB, zipPath, CreateOptions{
		PluginsDir: srcPlugins,
		ThemesDir:  srcThemes,
	})
	if err != nil {
		t.Fatalf("CreateToFile with extensions failed: %v", err)
	}
	if !mf.IncludesExtensions {
		t.Fatal("expected manifest.IncludesExtensions = true")
	}

	seenPaths := make(map[string]bool)
	for _, fd := range mf.Files {
		seenPaths[fd.Path] = true
		if strings.Contains(fd.Path, ".tmp") || strings.Contains(fd.Path, ".bak") || strings.Contains(fd.Path, "escape-link") {
			t.Fatalf("skipped temp/bak/symlink entry %q leaked into backup manifest", fd.Path)
		}
	}
	for _, want := range []string{
		"extensions/plugins/callout-ext/manifest.json",
		"extensions/plugins/callout-ext/main.js",
		"extensions/plugins/callout-ext/assets/icon.svg",
		"extensions/themes/aurora-pro/theme.json",
		"extensions/themes/aurora-pro/templates/post.html",
	} {
		if !seenPaths[want] {
			t.Fatalf("missing expected archive path %q in manifest", want)
		}
	}

	// DryRun counts 5 extension files without creating destination directories.
	dstDB := openTestSQLiteStore(t, filepath.Join(tmp, "ext-dst.db"))
	dstPlugins := filepath.Join(tmp, "dst-plugins")
	dstThemes := filepath.Join(tmp, "dst-themes")

	drySum, err := RestoreFromFile(ctx, dstDB, zipPath, RestoreOptions{
		DryRun:     true,
		PluginsDir: dstPlugins,
		ThemesDir:  dstThemes,
	})
	if err != nil {
		t.Fatalf("DryRun failed: %v", err)
	}
	if drySum.ExtensionFiles != 5 {
		t.Fatalf("expected 5 ExtensionFiles in DryRun, got %d", drySum.ExtensionFiles)
	}
	if _, statErr := os.Stat(dstPlugins); !os.IsNotExist(statErr) {
		t.Fatal("DryRun must not create dstPlugins directory")
	}

	// Full restore writes both plugins and themes cleanly.
	sum, err := RestoreFromFile(ctx, dstDB, zipPath, RestoreOptions{
		PluginsDir: dstPlugins,
		ThemesDir:  dstThemes,
	})
	if err != nil {
		t.Fatalf("RestoreFromFile with extensions failed: %v", err)
	}
	if sum.ExtensionFiles != 5 {
		t.Fatalf("expected 5 restored ExtensionFiles, got %d", sum.ExtensionFiles)
	}
	gotJS, err := os.ReadFile(filepath.Join(dstPlugins, "callout-ext", "main.js"))
	if err != nil || string(gotJS) != `exports.onEnable = function(){};` {
		t.Fatalf("restored plugin main.js mismatch: err=%v body=%q", err, string(gotJS))
	}
	gotTpl, err := os.ReadFile(filepath.Join(dstThemes, "aurora-pro", "templates", "post.html"))
	if err != nil || string(gotTpl) != `<article>{{.Title}}</article>` {
		t.Fatalf("restored theme post.html mismatch: err=%v body=%q", err, string(gotTpl))
	}

	// Security Check 1: Mid-path '..' traversal (e.g. extensions/plugins/a/../../evil/main.js) is strictly rejected!
	for _, badEntry := range []string{
		"extensions/plugins/a/../../evil/main.js",
		"extensions/themes/..\\evil\\theme.json",
	} {
		var tbuf bytes.Buffer
		tzw := zip.NewWriter(&tbuf)
		w, _ := tzw.Create(badEntry)
		_, _ = w.Write([]byte("evil"))
		_ = tzw.Close()
		if _, rErr := Restore(ctx, dstDB, bytes.NewReader(tbuf.Bytes()), int64(tbuf.Len()), RestoreOptions{PluginsDir: dstPlugins}); rErr == nil {
			t.Fatalf("expected traversal path %q to be rejected", badEntry)
		}
	}

	// Security Check 2: Unsigned extension file injected into valid archive is rejected!
	rawZipBytes, _ := os.ReadFile(zipPath)
	zr, _ := zip.NewReader(bytes.NewReader(rawZipBytes), int64(len(rawZipBytes)))
	var unsignedBuf bytes.Buffer
	uzw := zip.NewWriter(&unsignedBuf)
	for _, zf := range zr.File {
		rc, _ := zf.Open()
		var b bytes.Buffer
		_, _ = b.ReadFrom(rc)
		_ = rc.Close()
		fw, _ := uzw.Create(zf.Name)
		_, _ = fw.Write(b.Bytes())
	}
	rogueW, _ := uzw.Create("extensions/plugins/rogue/main.js")
	_, _ = rogueW.Write([]byte("evil()"))
	_ = uzw.Close()
	if _, uErr := Restore(ctx, dstDB, bytes.NewReader(unsignedBuf.Bytes()), int64(unsignedBuf.Len()), RestoreOptions{PluginsDir: dstPlugins}); uErr == nil || !strings.Contains(uErr.Error(), "unsigned") {
		t.Fatalf("expected unsigned extension entry error, got %v", uErr)
	}

	// Security Check 3: Entry-count and uncompressed byte caps enforced!
	if _, eErr := RestoreFromFile(ctx, dstDB, zipPath, RestoreOptions{MaxEntries: 3}); eErr == nil || !strings.Contains(eErr.Error(), "entries") {
		t.Fatalf("expected MaxEntries rejection, got %v", eErr)
	}
	if _, bErr := RestoreFromFile(ctx, dstDB, zipPath, RestoreOptions{MaxTotalBytes: 20}); bErr == nil || !strings.Contains(bErr.Error(), "limit") {
		t.Fatalf("expected MaxTotalBytes rejection, got %v", bErr)
	}
	if _, sErr := RestoreFromFile(ctx, dstDB, zipPath, RestoreOptions{MaxSingleFileBytes: 10}); sErr == nil || !strings.Contains(sErr.Error(), "limit") {
		t.Fatalf("expected MaxSingleFileBytes rejection, got %v", sErr)
	}
}

func TestBackup_MixedStoreAndDeflateMediaAndExtensionsRoundTrip(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	srcDB := openTestSQLiteStore(t, filepath.Join(tmp, "mix-src.db"))

	srcUpload := filepath.Join(tmp, "uploads")
	srcPlugins := filepath.Join(tmp, "plugins")
	if err := os.MkdirAll(filepath.Join(srcUpload, "2026"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(srcPlugins, "kanbanniang", "assets"), 0o750); err != nil {
		t.Fatal(err)
	}

	webpData := bytes.Repeat([]byte("RIFFWEBP"), 64)
	txtData := bytes.Repeat([]byte("compressible-notes-line\n"), 64)
	pngData := bytes.Repeat([]byte("\x89PNG\r\n\x1a\n"), 64)
	mocData := bytes.Repeat([]byte("moc-model-stream-"), 64)

	_ = os.WriteFile(filepath.Join(srcUpload, "2026", "photo.webp"), webpData, 0o640)
	_ = os.WriteFile(filepath.Join(srcUpload, "2026", "readme.txt"), txtData, 0o640)
	_ = os.WriteFile(filepath.Join(srcPlugins, "kanbanniang", "manifest.json"), []byte(`{"id":"kanbanniang","version":"2.0.0"}`), 0o640)
	_ = os.WriteFile(filepath.Join(srcPlugins, "kanbanniang", "assets", "tex.png"), pngData, 0o640)
	_ = os.WriteFile(filepath.Join(srcPlugins, "kanbanniang", "assets", "avatar.moc"), mocData, 0o640)

	zipPath := filepath.Join(tmp, "mixed-method.zip")
	if _, err := CreateToFile(ctx, srcDB, zipPath, CreateOptions{
		IncludeMedia: true,
		UploadDir:    srcUpload,
		PluginsDir:   srcPlugins,
	}); err != nil {
		t.Fatalf("CreateToFile: %v", err)
	}

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	methods := make(map[string]uint16)
	for _, f := range zr.File {
		methods[f.Name] = f.Method
	}
	_ = zr.Close()

	if methods["uploads/2026/photo.webp"] != zip.Store {
		t.Fatalf("expected uploads/2026/photo.webp to use zip.Store, got %d", methods["uploads/2026/photo.webp"])
	}
	if methods["uploads/2026/readme.txt"] != zip.Deflate {
		t.Fatalf("expected uploads/2026/readme.txt to use zip.Deflate, got %d", methods["uploads/2026/readme.txt"])
	}
	if methods["extensions/plugins/kanbanniang/assets/tex.png"] != zip.Store {
		t.Fatalf("expected extension .png to use zip.Store, got %d", methods["extensions/plugins/kanbanniang/assets/tex.png"])
	}
	if methods["extensions/plugins/kanbanniang/assets/avatar.moc"] != zip.Deflate {
		t.Fatalf("expected extension .moc to use zip.Deflate, got %d", methods["extensions/plugins/kanbanniang/assets/avatar.moc"])
	}

	dstDB := openTestSQLiteStore(t, filepath.Join(tmp, "mix-dst.db"))
	dstUpload := filepath.Join(tmp, "dst-uploads")
	dstPlugins := filepath.Join(tmp, "dst-plugins")

	if _, err := RestoreFromFile(ctx, dstDB, zipPath, RestoreOptions{
		DryRun:     true,
		UploadDir:  dstUpload,
		PluginsDir: dstPlugins,
	}); err != nil {
		t.Fatalf("DryRun Verify failed: %v", err)
	}

	sum, err := RestoreFromFile(ctx, dstDB, zipPath, RestoreOptions{
		UploadDir:  dstUpload,
		PluginsDir: dstPlugins,
	})
	if err != nil {
		t.Fatalf("RestoreFromFile failed: %v", err)
	}
	if sum.MediaFiles != 2 || sum.ExtensionFiles != 3 {
		t.Fatalf("unexpected restore counts: media=%d ext=%d", sum.MediaFiles, sum.ExtensionFiles)
	}
	gotPNG, err := os.ReadFile(filepath.Join(dstPlugins, "kanbanniang", "assets", "tex.png"))
	if err != nil || !bytes.Equal(gotPNG, pngData) {
		t.Fatalf("restored tex.png mismatch: err=%v len=%d", err, len(gotPNG))
	}
	gotWebP, err := os.ReadFile(filepath.Join(dstUpload, "2026", "photo.webp"))
	if err != nil || !bytes.Equal(gotWebP, webpData) {
		t.Fatalf("restored photo.webp mismatch: err=%v len=%d", err, len(gotWebP))
	}
}

func TestNormalizeRestoreValue_UsersShieldCommentsCrossDB(t *testing.T) {
	if !isBooleanColumn("users", "shield_comments") {
		t.Fatal("expected users.shield_comments to be registered in isBooleanColumn")
	}
	// SQLite/MySQL numeric 1/0 -> Postgres native bool true/false
	if got := coerceJSONValForDB("users", "shield_comments", json.Number("1"), "postgres"); got != true {
		t.Fatalf("postgres coerceJSONValForDB(1) = %#v, want true", got)
	}
	if got := coerceJSONValForDB("users", "shield_comments", json.Number("0"), "postgres"); got != false {
		t.Fatalf("postgres coerceJSONValForDB(0) = %#v, want false", got)
	}
	// Postgres bool true/false -> SQLite/MySQL integer 1/0
	if got := coerceJSONValForDB("users", "shield_comments", true, "sqlite"); got != 1 {
		t.Fatalf("sqlite coerceJSONValForDB(true) = %#v, want 1", got)
	}
	if got := coerceJSONValForDB("users", "shield_comments", false, "mysql"); got != 0 {
		t.Fatalf("mysql coerceJSONValForDB(false) = %#v, want 0", got)
	}
}

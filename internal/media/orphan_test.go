package media_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/media"
)

func TestOrphanScanner_PreservesImageViewQueryReferencedAssets(t *testing.T) {
	ctx, store, settings, uploadDir, pipe, _, _ := setupMediaEnv(t)

	// Upload 5 distinct images: 4 referenced via ImageView query strings across Markdown, HTML srcset, and JSON, plus 1 true orphan.
	jpgBytes := buildJPEGWithEXIFAndGPS(t, 1)
	var assets []*domain.MediaAsset
	for i := 0; i < 5; i++ {
		a, err := pipe.Upload(ctx, "admin-1", "photo.jpg", "image/jpeg", bytes.NewReader(jpgBytes))
		if err != nil {
			t.Fatalf("Upload #%d failed: %v", i, err)
		}
		assets = append(assets, a)
	}

	now := time.Now().UnixMilli()

	// 1. Markdown image reference with Qiniu path style `?imageView2/1/w/960/h/540/format/webp`
	mdContent := "Check out this hero:\n\n![Hero](" + assets[0].PublicURL + "?imageView2/1/w/960/h/540/format/webp/q/85)\n"
	_, err := store.ExecContext(ctx,
		`INSERT INTO articles (id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md, cover_url, status, published_at, scheduled_at, is_pinned, allow_comment, password_ciphertext, has_content_lock, audio_url, audio_duration_sec, audio_size_bytes, audio_mime, lang, translation_group, license_override, word_count, reading_minutes, view_count, comment_count, created_at, updated_at)
		 VALUES ('iv-art-1', 'u1', '', 'ImageView Post', 'iv-post', '', '', ?, ?, 'published', ?, 0, 0, 1, '', 0, '', 0, 0, '', 'zh-CN', '', '', 1, 1, 0, 0, ?, ?)`,
		mdContent,
		assets[1].PublicURL+"?mode=1&w=400&h=300&format=webp",
		now, now, now)
	if err != nil {
		t.Fatalf("insert article: %v", err)
	}

	// 2. HTML <img src srcset> inside page content with both KV and Qiniu ImageView queries
	htmlPage := `<figure><img src="` + assets[2].PublicURL + `?mode=2&w=800&format=webp" srcset="` +
		assets[2].PublicURL + `?imageView2/2/w/480/format/webp 480w, ` +
		assets[2].PublicURL + `?imageView2/2/w/960/format/webp 960w" alt="Responsive"></figure>`
	_, err = store.ExecContext(ctx,
		`INSERT INTO pages (id, author_id, title, slug, content_md, is_published, allow_comment, show_toc, lang, translation_group, view_count, comment_count, created_at, updated_at)
		 VALUES ('iv-page-1', 'u1', 'Gallery', 'gallery', ?, 1, 1, 1, 'zh-CN', '', 0, 0, ?, ?)`,
		htmlPage, now, now)
	if err != nil {
		t.Fatalf("insert page: %v", err)
	}

	// 3. JSON settings reference with escaped slashes and `?mode=1&w=400`
	sitePatch, _ := json.Marshal(map[string]any{
		"logo_url": assets[3].PublicURL + "?mode=1&w=400&strip=1",
	})
	if _, _, err := settings.PatchDomain(ctx, domain.SettingsDomainSite, sitePatch, 0); err != nil {
		t.Fatalf("patch site settings: %v", err)
	}

	// Verify CountAssetReferences counts all 4 ImageView-referenced assets as >= 1 and assets[4] as 0.
	deref := make([]domain.MediaAsset, len(assets))
	for i, a := range assets {
		deref[i] = *a
	}
	counts, err := media.CountAssetReferences(ctx, store, deref)
	if err != nil {
		t.Fatalf("CountAssetReferences: %v", err)
	}
	for i := 0; i < 4; i++ {
		if counts[assets[i].ID] < 1 {
			t.Fatalf("expected asset[%d] (%s) referenced via ImageView query to have count >= 1, got %d", i, assets[i].PublicURL, counts[assets[i].ID])
		}
	}
	if counts[assets[4].ID] != 0 {
		t.Fatalf("expected unreferenced asset[4] count=0, got %d", counts[assets[4].ID])
	}

	// Run destructive ScanOrphans(true): ONLY assets[4] may be deleted!
	orphans, err := pipe.ScanOrphans(ctx, true)
	if err != nil {
		t.Fatalf("ScanOrphans(true): %v", err)
	}
	if len(orphans) != 1 || orphans[0].ID != assets[4].ID {
		t.Fatalf("expected only asset[4] (%s) as orphan, got %+v", assets[4].ID, orphans)
	}
	for i := 0; i < 4; i++ {
		p := filepath.Join(uploadDir, filepath.FromSlash(assets[i].ObjectKey))
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("ImageView-referenced asset[%d] was falsely deleted from disk: %v", i, err)
		}
	}
	if _, err := os.Stat(filepath.Join(uploadDir, filepath.FromSlash(assets[4].ObjectKey))); !os.IsNotExist(err) {
		t.Fatalf("expected unreferenced asset[4] to be unlinked")
	}

	// Verify exported SafeUnlinkLocalFile blocks path traversal and symlinks
	outside := filepath.Join(t.TempDir(), "keep.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := media.SafeUnlinkLocalFile(uploadDir, "../keep.txt"); err == nil {
		t.Fatalf("expected SafeUnlinkLocalFile to reject ../ traversal")
	}
	sym := filepath.Join(uploadDir, "sym.txt")
	if err := os.Symlink(outside, sym); err != nil {
		if runtime.GOOS != "windows" {
			t.Fatal(err)
		}
	} else {
		if err := media.SafeUnlinkLocalFile(uploadDir, "sym.txt"); err == nil {
			t.Fatalf("expected SafeUnlinkLocalFile to reject symlink")
		}
		if _, err := os.Stat(outside); err != nil {
			t.Fatalf("outside file must remain untouched: %v", err)
		}
	}
}

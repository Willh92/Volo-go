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

func TestOrphanScanner_PreservesPluginKVAndSpacedFilenameAssets(t *testing.T) {
	ctx, store, _, uploadDir, pipe, _, _ := setupMediaEnv(t)

	now := time.Now().UnixMilli()
	createLocalAsset := func(id, filename, objKey, pubURL string) domain.MediaAsset {
		full := filepath.Join(uploadDir, filepath.FromSlash(objKey))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("media-bytes-"+id), 0o644); err != nil {
			t.Fatal(err)
		}
		a := domain.MediaAsset{
			ID:            id,
			UploaderID:    "u1",
			OriginalName:  filename,
			StorageDriver: "local",
			ObjectKey:     objKey,
			PublicURL:     pubURL,
			MIMEType:      "image/jpeg",
			SizeBytes:     int64(len("media-bytes-" + id)),
			SHA256:        "sha-" + id,
			CreatedAt:     now,
		}
		_, err := store.ExecContext(ctx,
			`INSERT INTO media_assets (id, storage_driver, object_key, public_url, original_name, alt_text, mime_type, size_bytes, width, height, sha256, exif_stripped, uploader_id, created_at)
			 VALUES (?, ?, ?, ?, ?, '', ?, ?, 0, 0, ?, 0, ?, ?)`,
			a.ID, a.StorageDriver, a.ObjectKey, a.PublicURL, a.OriginalName, a.MIMEType, a.SizeBytes, a.SHA256, a.UploaderID, a.CreatedAt)
		if err != nil {
			t.Fatalf("insert media_asset %s: %v", id, err)
		}
		return a
	}

	// 1. Asset referenced only inside plugin_kv.value_text
	kvAsset := createLocalAsset("m-kv", "banner.jpg", "2026/05/plugin-banner.jpg", "/uploads/2026/05/plugin-banner.jpg")
	_, err := store.ExecContext(ctx,
		`INSERT INTO plugin_kv (plugin_id, k, value_text, expires_at, updated_at) VALUES ('hero-plugin', 'slides', ?, 0, ?)`,
		`{"hero":"/uploads/2026/05/plugin-banner.jpg","title":"Slide"}`, now)
	if err != nil {
		t.Fatalf("insert plugin_kv: %v", err)
	}

	// 2. Asset with spaces in filename/object_key referenced with literal spaces
	rawSpaceAsset := createLocalAsset("m-space-raw", "my holiday photo.jpg", "legacy/2026/my holiday photo.jpg", "/uploads/legacy/2026/my holiday photo.jpg")
	// 3. Asset with spaces in filename/object_key referenced via %20 URL-encoding
	encSpaceAsset := createLocalAsset("m-space-enc", "summer trip 01.jpg", "legacy/2026/summer trip 01.jpg", "/uploads/legacy/2026/summer trip 01.jpg")
	// 4. Unreferenced asset with spaces (must still be cleaned up)
	orphanSpaceAsset := createLocalAsset("m-space-orphan", "unused draft pic.jpg", "legacy/2026/unused draft pic.jpg", "/uploads/legacy/2026/unused draft pic.jpg")

	artMD := "Raw space: <img src=\"/uploads/legacy/2026/my holiday photo.jpg\">\n\n" +
		"Encoded space: ![Trip](/uploads/legacy/2026/summer%20trip%2001.jpg?mode=2&w=800)\n"
	_, err = store.ExecContext(ctx,
		`INSERT INTO articles (id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md, cover_url, status, published_at, scheduled_at, is_pinned, allow_comment, password_ciphertext, has_content_lock, audio_url, audio_duration_sec, audio_size_bytes, audio_mime, lang, translation_group, license_override, word_count, reading_minutes, view_count, comment_count, created_at, updated_at)
		 VALUES ('sp-art-1', 'u1', '', 'Spaced Media', 'spaced-media', '', '', ?, '', 'published', ?, 0, 0, 1, '', 0, '', 0, 0, '', 'zh-CN', '', '', 1, 1, 0, 0, ?, ?)`,
		artMD, now, now, now)
	if err != nil {
		t.Fatalf("insert article: %v", err)
	}

	counts, err := media.CountAssetReferences(ctx, store, []domain.MediaAsset{kvAsset, rawSpaceAsset, encSpaceAsset, orphanSpaceAsset})
	if err != nil {
		t.Fatalf("CountAssetReferences: %v", err)
	}
	for _, want := range []domain.MediaAsset{kvAsset, rawSpaceAsset, encSpaceAsset} {
		if counts[want.ID] < 1 {
			t.Fatalf("expected %s (%s) reference count >= 1, got %d", want.ID, want.ObjectKey, counts[want.ID])
		}
	}
	if counts[orphanSpaceAsset.ID] != 0 {
		t.Fatalf("expected %s reference count = 0, got %d", orphanSpaceAsset.ID, counts[orphanSpaceAsset.ID])
	}

	orphans, err := pipe.ScanOrphans(ctx, true)
	if err != nil {
		t.Fatalf("ScanOrphans(true): %v", err)
	}
	if len(orphans) != 1 || orphans[0].ID != orphanSpaceAsset.ID {
		t.Fatalf("expected only %s to be orphan, got %+v", orphanSpaceAsset.ID, orphans)
	}

	for _, keep := range []domain.MediaAsset{kvAsset, rawSpaceAsset, encSpaceAsset} {
		p := filepath.Join(uploadDir, filepath.FromSlash(keep.ObjectKey))
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected referenced asset %s (%s) to survive ScanOrphans(true), stat err: %v", keep.ID, keep.ObjectKey, err)
		}
	}
	if _, err := os.Stat(filepath.Join(uploadDir, filepath.FromSlash(orphanSpaceAsset.ObjectKey))); !os.IsNotExist(err) {
		t.Fatalf("expected unreferenced asset %s to be deleted", orphanSpaceAsset.ID)
	}
}

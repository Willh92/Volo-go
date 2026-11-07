package media_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/media"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/pkg/id"
)

// buildJPEGWithEXIFAndGPS synthesizes a 120x80 JPEG and injects a valid JPEG APP1 (0xFFE1)
// Exif TIFF segment containing IFD0 Orientation, Camera Make/Model, and GPSInfo coordinates.
func buildJPEGWithEXIFAndGPS(t *testing.T, orientation uint16) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 120, 80))
	for y := 0; y < 80; y++ {
		for x := 0; x < 120; x++ {
			if x < 60 && y < 40 {
				img.SetNRGBA(x, y, color.NRGBA{R: 220, G: 40, B: 40, A: 255})
			} else {
				img.SetNRGBA(x, y, color.NRGBA{R: 30, G: 110, B: 200, A: 255})
			}
		}
	}
	var rawJPG bytes.Buffer
	if err := jpeg.Encode(&rawJPG, img, &jpeg.Options{Quality: 92}); err != nil {
		t.Fatal(err)
	}
	jpgBytes := rawJPG.Bytes()

	// Build Little-Endian TIFF payload with 3 IFD0 entries:
	// 1. 0x010F Make (ASCII)
	// 2. 0x0112 Orientation (SHORT = orientation)
	// 3. 0x8825 GPSInfo IFD pointer -> GPS tags + secret canary
	var tiff bytes.Buffer
	tiff.Write([]byte("II\x2a\x00"))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(8)) // IFD0 offset = 8
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(3)) // 3 entries

	makeStr := []byte("PrivateCamCorp-GeoTracker\x00")
	gpsPayload := []byte("GPS_LAT_37.7749N_LON_122.4194W_SECRET_CANARY\x00")
	makeOffset := uint32(8 + 2 + 3*12 + 4)
	gpsOffset := makeOffset + uint32(len(makeStr))

	// Entry 1: Make (0x010F, ASCII=2)
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(0x010F))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(2))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(len(makeStr)))
	_ = binary.Write(&tiff, binary.LittleEndian, makeOffset)

	// Entry 2: Orientation (0x0112, SHORT=3, count=1)
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(0x0112))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(3))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(1))
	_ = binary.Write(&tiff, binary.LittleEndian, orientation)
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(0))

	// Entry 3: GPSInfo (0x8825, LONG=4, count=1 -> gpsOffset)
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(0x8825))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(4))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(1))
	_ = binary.Write(&tiff, binary.LittleEndian, gpsOffset)

	// Next IFD offset = 0
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(0))
	tiff.Write(makeStr)
	tiff.Write(gpsPayload)

	var app1 bytes.Buffer
	app1.Write([]byte("Exif\x00\x00"))
	app1.Write(tiff.Bytes())
	segLen := uint16(app1.Len() + 2)

	// Splice SOI (0xFFD8) + APP1 (0xFFE1) + remainder of JPEG
	var out bytes.Buffer
	out.Write(jpgBytes[:2])
	out.Write([]byte{0xFF, 0xE1})
	_ = binary.Write(&out, binary.BigEndian, segLen)
	out.Write(app1.Bytes())
	out.Write(jpgBytes[2:])
	return out.Bytes()
}

func setupMediaEnv(t *testing.T) (context.Context, *sqlx.Store, *repo.SettingsManager, string, *media.Pipeline, *int, *int) {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	uploadDir := filepath.Join(tmp, "uploads")
	if err := os.MkdirAll(uploadDir, 0o750); err != nil {
		t.Fatal(err)
	}
	secret := "0123456789abcdef0123456789abcdef-m6-media-secret"
	dataDir := filepath.Join(tmp, "data")
	cfg, err := config.Load(config.CLIOverrides{
		SecretKey: &secret,
		DataDir:   &dataDir,
		UploadDir: &uploadDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(dataDir, "media-test.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatal(err)
	}
	settings := repo.NewSettingsManager(store, cfg.KeyRing, "zh-CN")
	if _, err := settings.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	var cbW, cbH int
	pipe := media.NewPipeline(media.PipelineOptions{
		Store:     store,
		Settings:  settings,
		UploadDir: uploadDir,
		OnDimsUpdated: func(_ string, w, h int) {
			cbW, cbH = w, h
		},
	})
	return ctx, store, settings, uploadDir, pipe, &cbW, &cbH
}

func TestMedia_EXIFGPSStripOrientationAndAdaptiveWebP(t *testing.T) {
	ctx, _, _, uploadDir, pipe, cbW, cbH := setupMediaEnv(t)

	// 1. Upload 120x80 JPEG carrying Orientation=6 (Rotate 90 CW) + GPS coordinates + Camera Make
	gpsJPEG := buildJPEGWithEXIFAndGPS(t, 6)
	if !media.ContainsEXIFOrGPS(gpsJPEG) {
		t.Fatalf("expected synthetic input JPEG to contain EXIF/GPS segment")
	}
	if gotOrient := media.ReadEXIFOrientation(gpsJPEG); gotOrient != 6 {
		t.Fatalf("ReadEXIFOrientation = %d, want 6", gotOrient)
	}

	asset, err := pipe.Upload(ctx, "admin-1", "trip-photo.jpg", "image/jpeg", bytes.NewReader(gpsJPEG))
	if err != nil {
		t.Fatalf("Upload GPS JPEG failed: %v", err)
	}
	if !asset.EXIFStripped {
		t.Fatalf("expected EXIFStripped=true")
	}
	// Orientation=6 rotates 120x80 -> 80x120!
	if asset.Width != 80 || asset.Height != 120 || *cbW != 80 || *cbH != 120 {
		t.Fatalf("expected rotated dimensions 80x120, got asset=%dx%d callback=%dx%d", asset.Width, asset.Height, *cbW, *cbH)
	}

	storedPath := filepath.Join(uploadDir, filepath.FromSlash(asset.ObjectKey))
	storedBytes, err := os.ReadFile(storedPath)
	if err != nil {
		t.Fatalf("read stored output: %v", err)
	}
	if media.ContainsEXIFOrGPS(storedBytes) {
		t.Fatalf("CRITICAL PRIVACY LEAK: output image still contains EXIF/GPS segment!")
	}
	for _, canary := range []string{"Exif", "PrivateCamCorp", "GPS_LAT_37.7749N"} {
		if bytes.Contains(storedBytes, []byte(canary)) {
			t.Fatalf("CRITICAL PRIVACY LEAK: output image contains %q!", canary)
		}
	}

	// 2. Upload compressible solid-block PNG -> Adaptive WebP produces valid .webp with exact dimensions
	pngImg := image.NewNRGBA(image.Rect(0, 0, 240, 160))
	for y := 0; y < 160; y++ {
		for x := 0; x < 240; x++ {
			pngImg.SetNRGBA(x, y, color.NRGBA{R: uint8(x % 4 * 60), G: 140, B: 90, A: 255})
		}
	}
	var pngBuf bytes.Buffer
	// Write uncompressed PNG so AdaptiveWebP wins size comparison deterministically
	enc := png.Encoder{CompressionLevel: png.NoCompression}
	if err := enc.Encode(&pngBuf, pngImg); err != nil {
		t.Fatal(err)
	}
	webpAsset, err := pipe.Upload(ctx, "admin-1", "diagram.png", "image/png", bytes.NewReader(pngBuf.Bytes()))
	if err != nil {
		t.Fatalf("Upload PNG failed: %v", err)
	}
	if webpAsset.MIMEType != "image/webp" || !strings.HasSuffix(webpAsset.PublicURL, ".webp") {
		t.Fatalf("expected AdaptiveWebP output (.webp, image/webp), got %s (%s)", webpAsset.PublicURL, webpAsset.MIMEType)
	}
	if webpAsset.Width != 240 || webpAsset.Height != 160 {
		t.Fatalf("expected 240x160 WebP dimensions, got %dx%d", webpAsset.Width, webpAsset.Height)
	}
	webpDisk, err := os.ReadFile(filepath.Join(uploadDir, filepath.FromSlash(webpAsset.ObjectKey)))
	if err != nil {
		t.Fatal(err)
	}
	decCfg, decFmt, err := image.DecodeConfig(bytes.NewReader(webpDisk))
	if err != nil || decFmt != "webp" || decCfg.Width != 240 || decCfg.Height != 160 {
		t.Fatalf("stored WebP decode verification failed: fmt=%q cfg=%+v err=%v", decFmt, decCfg, err)
	}

	// 3. Reject SVG XSS and magic-byte spoofing
	if _, err := pipe.Upload(ctx, "admin-1", "xss.svg", "image/svg+xml", strings.NewReader(`<svg onload="alert(1)"/>`)); err == nil {
		t.Fatalf("expected .svg upload to be rejected")
	}
	if _, err := pipe.Upload(ctx, "admin-1", "spoof.jpg", "image/jpeg", strings.NewReader(`<script>alert(1)</script>`)); err == nil {
		t.Fatalf("expected spoofed .jpg magic bytes to be rejected")
	}
}

func TestMedia_PureGoS3SigV4UploadHealthAndDelete(t *testing.T) {
	ctx, _, settings, _, pipe, _, _ := setupMediaEnv(t)

	var mu sync.Mutex
	objects := make(map[string][]byte)
	var sawSigV4 bool

	s3Srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHdr := r.Header.Get("Authorization")
		if !strings.HasPrefix(authHdr, "AWS4-HMAC-SHA256 Credential=AKIA_TEST/") || r.Header.Get("X-Amz-Content-Sha256") == "" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("missing SigV4 headers"))
			return
		}
		mu.Lock()
		sawSigV4 = true
		mu.Unlock()

		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			objects[r.URL.Path] = body
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			mu.Lock()
			delete(objects, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer s3Srv.Close()

	// Configure S3 storage domain with encrypted secret key
	patch := []byte(`{
		"active_driver": "s3",
		"s3_endpoint": "` + s3Srv.URL + `",
		"s3_region": "us-east-1",
		"s3_bucket": "volo-vault",
		"s3_access_key": "AKIA_TEST",
		"s3_secret_key": "s3-super-secret-signing-key",
		"s3_public_base_url": "https://cdn.example.com",
		"s3_key_prefix": "blog",
		"s3_path_style": true
	}`)
	if _, _, err := settings.PatchDomain(ctx, domain.SettingsDomainStorage, patch, 0); err != nil {
		t.Fatalf("PatchDomain storage: %v", err)
	}

	if err := pipe.TestStorage(ctx, nil, ""); err != nil {
		t.Fatalf("S3 TestStorage failed: %v", err)
	}

	gpsJPEG := buildJPEGWithEXIFAndGPS(t, 3) // Orientation=3 (Rotate 180)
	asset, err := pipe.Upload(ctx, "admin-1", "cloud-gps.jpg", "image/jpeg", bytes.NewReader(gpsJPEG))
	if err != nil {
		t.Fatalf("S3 Upload failed: %v", err)
	}
	if asset.StorageDriver != "s3" || !strings.HasPrefix(asset.PublicURL, "https://cdn.example.com/blog/") {
		t.Fatalf("unexpected S3 asset metadata: %+v", asset)
	}

	mu.Lock()
	if !sawSigV4 || len(objects) != 1 {
		mu.Unlock()
		t.Fatalf("expected 1 SigV4-authenticated object in mock S3, got %d (sawSigV4=%v)", len(objects), sawSigV4)
	}
	for _, objBytes := range objects {
		if media.ContainsEXIFOrGPS(objBytes) || bytes.Contains(objBytes, []byte("GPS_LAT_37.7749N")) {
			mu.Unlock()
			t.Fatalf("S3 uploaded object leaked EXIF/GPS metadata!")
		}
	}
	mu.Unlock()

	if err := pipe.Delete(ctx, asset.ID); err != nil {
		t.Fatalf("S3 Delete failed: %v", err)
	}
	mu.Lock()
	rem := len(objects)
	mu.Unlock()
	if rem != 0 {
		t.Fatalf("expected S3 bucket empty after Delete, got %d objects", rem)
	}
}

func TestMedia_NineScopeOrphanScannerZeroFalsePositives(t *testing.T) {
	ctx, store, settings, uploadDir, pipe, _, _ := setupMediaEnv(t)

	// Upload 10 small valid PDFs (fast, deterministic)
	assets := make([]*domain.MediaAsset, 10)
	for i := 0; i < 10; i++ {
		a, err := pipe.Upload(ctx, "admin-1", "doc.pdf", "application/pdf", strings.NewReader("%PDF-1.7\n%%EOF\n"))
		if err != nil {
			t.Fatalf("upload #%d: %v", i, err)
		}
		assets[i] = a
	}

	now := time.Now().UnixMilli()
	// Bind assets[0..8] across all 9 distinct database reference scopes!
	// Scope 1: articles
	_, err := store.ExecContext(ctx,
		`INSERT INTO articles (id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md, cover_url, status, published_at, scheduled_at, is_pinned, allow_comment, password_ciphertext, has_content_lock, audio_url, audio_duration_sec, audio_size_bytes, audio_mime, lang, translation_group, license_override, word_count, reading_minutes, view_count, comment_count, created_at, updated_at)
		 VALUES ('a1', 'u1', '', 'T', 's1', '', '', ?, '', 'published', ?, 0, 0, 1, '', 0, '', 0, 0, '', 'zh-CN', '', '', 1, 1, 0, 0, ?, ?)`,
		"See "+assets[0].PublicURL, now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	// Scope 2: article_revisions
	_, err = store.ExecContext(ctx,
		`INSERT INTO article_revisions (id, article_id, title, summary_md, content_md, author_name, note, created_at)
		 VALUES ('r1', 'a1', 'T', '', ?, 'adm', 'v1', ?)`,
		"Old rev "+assets[1].PublicURL, now)
	if err != nil {
		t.Fatal(err)
	}
	// Scope 3: pages
	_, err = store.ExecContext(ctx,
		`INSERT INTO pages (id, author_id, title, slug, content_md, is_published, allow_comment, show_toc, lang, translation_group, view_count, comment_count, created_at, updated_at)
		 VALUES ('p1', 'u1', 'About', 'about', ?, 1, 1, 1, 'zh-CN', '', 0, 0, ?, ?)`,
		"Hero "+assets[2].PublicURL, now, now)
	if err != nil {
		t.Fatal(err)
	}
	// Scope 4: series
	_, err = store.ExecContext(ctx,
		`INSERT INTO series (id, title, slug, description, cover_url, sort_order, created_at, updated_at)
		 VALUES ('ser1', 'S', 's', '', ?, 1, ?, ?)`,
		assets[3].PublicURL, now, now)
	if err != nil {
		t.Fatal(err)
	}
	// Scope 5: books
	_, err = store.ExecContext(ctx,
		`INSERT INTO books (id, title, slug, description, cover_url, source_type, source_url, series_id, embed_mode, chapter_count, synced_at, sort_order, created_at, updated_at)
		 VALUES ('bk1', 'B', 'b', '', ?, 'auto', '', '', 'link', 0, 0, 1, ?, ?)`,
		assets[4].PublicURL, now, now)
	if err != nil {
		t.Fatal(err)
	}
	// Scope 6: links
	_, err = store.ExecContext(ctx,
		`INSERT INTO links (id, title, url, description, icon_url, category_name, sort_order, health_status, http_code, latency_ms, health_error, last_checked_at, created_at, updated_at)
		 VALUES ('l1', 'L', 'https://example.com', '', ?, 'Friends', 1, 'ok', 200, 10, '', 0, ?, ?)`,
		assets[5].PublicURL, now, now)
	if err != nil {
		t.Fatal(err)
	}
	// Scope 7: users
	_, err = store.ExecContext(ctx,
		`INSERT INTO users (id, username, email, display_name, password_hash, legacy_hash, role, status, avatar_url, bio, website_url, email_verified_at, preferred_locale, created_at, updated_at)
		 VALUES ('u1', 'adm', 'a@example.com', 'A', 'h', '', 'admin', 'active', ?, '', '', ?, 'zh-CN', ?, ?)`,
		assets[6].PublicURL, now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	// Scope 8: comments
	_, err = store.ExecContext(ctx,
		`INSERT INTO comments (id, target_type, target_id, parent_id, reply_to_name, user_id, guest_id_hash, env_fp_hash, author_name, author_url, avatar_url, notify_email_ciphertext, notify_locale, client_ip, ip_masked, geo_region, content_md, status, is_deleted, created_at, updated_at)
		 VALUES ('c1', 'article', 'a1', '', '', '', 'g', 'f', 'G', '', '', '', 'zh-CN', '', '', '', ?, 'approved', 0, ?, ?)`,
		"Attached "+assets[7].PublicURL, now, now)
	if err != nil {
		t.Fatal(err)
	}
	// Scope 9: settings (site.logo_url)
	if _, _, err := settings.PatchDomain(ctx, domain.SettingsDomainSite, []byte(`{"logo_url":"`+assets[8].PublicURL+`"}`), 0); err != nil {
		t.Fatal(err)
	}

	// Dry-run scan: ONLY assets[9] may be reported as orphan!
	orphans, err := pipe.ScanOrphans(ctx, false)
	if err != nil {
		t.Fatalf("ScanOrphans(false): %v", err)
	}
	if len(orphans) != 1 || orphans[0].ID != assets[9].ID {
		t.Fatalf("expected exactly 1 orphan (%s), got %+v", assets[9].ID, orphans)
	}

	// Clean scan: deletes ONLY assets[9], leaving all 9 referenced files intact on disk!
	deleted, err := pipe.ScanOrphans(ctx, true)
	if err != nil || len(deleted) != 1 {
		t.Fatalf("ScanOrphans(true): len=%d err=%v", len(deleted), err)
	}
	if _, statErr := os.Stat(filepath.Join(uploadDir, filepath.FromSlash(assets[9].ObjectKey))); !os.IsNotExist(statErr) {
		t.Fatalf("expected orphan file unlinked from disk")
	}
	for i := 0; i < 9; i++ {
		if _, statErr := os.Stat(filepath.Join(uploadDir, filepath.FromSlash(assets[i].ObjectKey))); statErr != nil {
			t.Fatalf("referenced asset #%d in scope %d was falsely deleted: %v", i, i+1, statErr)
		}
	}

	// Symlink containment check: tampering object_key to point at a symlink escaping UploadDir MUST refuse to unlink outside target!
	outsideFile := filepath.Join(t.TempDir(), "outside-secret.txt")
	if err := os.WriteFile(outsideFile, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	symRel := "escape-link.pdf"
	if err := os.Symlink(outsideFile, filepath.Join(uploadDir, symRel)); err != nil {
		t.Fatal(err)
	}
	symID := id.Next()
	_, _ = store.ExecContext(ctx,
		`INSERT INTO media_assets (id, storage_driver, object_key, public_url, original_name, mime_type, size_bytes, width, height, sha256, exif_stripped, uploader_id, created_at)
		 VALUES (?, 'local', ?, '/uploads/escape-link.pdf', 'escape.pdf', 'application/pdf', 4, 0, 0, '00', 0, 'u1', ?)`,
		symID, symRel, now)
	_ = pipe.Delete(ctx, symID)
	if _, err := os.Stat(outsideFile); err != nil {
		t.Fatalf("CRITICAL: symlink escape deleted file outside UploadDir!")
	}
}

func TestMedia_StreamingNonImageUploadsAndLinearOrphanTokenScanner(t *testing.T) {
	ctx, store, settings, uploadDir, pipe, _, _ := setupMediaEnv(t)

	// 1. Stream non-image MP3 and MP4 uploads and verify zero leftover .volo-upload-* temp files.
	mp3Payload := append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), bytes.Repeat([]byte("A"), 128*1024)...)
	mp3Asset, err := pipe.Upload(ctx, "admin-1", "episode.mp3", "audio/mpeg", bytes.NewReader(mp3Payload))
	if err != nil {
		t.Fatalf("MP3 streaming upload failed: %v", err)
	}
	if mp3Asset.MIMEType != "audio/mpeg" || mp3Asset.SizeBytes != int64(len(mp3Payload)) {
		t.Fatalf("unexpected MP3 asset: %+v", mp3Asset)
	}

	mp4Header := []byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm', 0, 0, 0, 0}
	mp4Payload := append(mp4Header, bytes.Repeat([]byte("V"), 64*1024)...)
	mp4Asset, err := pipe.Upload(ctx, "admin-1", "clip.mp4", "video/mp4", bytes.NewReader(mp4Payload))
	if err != nil {
		t.Fatalf("MP4 streaming upload failed: %v", err)
	}
	if mp4Asset.MIMEType != "video/mp4" || mp4Asset.SizeBytes != int64(len(mp4Payload)) {
		t.Fatalf("unexpected MP4 asset: %+v", mp4Asset)
	}

	pdfOrphan, err := pipe.Upload(ctx, "admin-1", "unused.pdf", "application/pdf", strings.NewReader("%PDF-1.7\n%%EOF\n"))
	if err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(uploadDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".volo-upload-") {
			t.Fatalf("leaked temporary upload file in UploadDir: %s", e.Name())
		}
	}

	// 2. Reference mp3Asset via Markdown link with query+fragment and trailing period,
	//    and mp4Asset via JSON-escaped slash URL in settings; pdfOrphan remains unreferenced.
	now := time.Now().UnixMilli()
	mdBody := "Listen to [Episode](" + mp3Asset.PublicURL + "?dl=1#t=30)."
	_, err = store.ExecContext(ctx,
		`INSERT INTO articles (id, author_id, category_id, title, slug, legacy_permalink, summary_md, content_md, cover_url, status, published_at, scheduled_at, is_pinned, allow_comment, password_ciphertext, has_content_lock, audio_url, audio_duration_sec, audio_size_bytes, audio_mime, lang, translation_group, license_override, word_count, reading_minutes, view_count, comment_count, created_at, updated_at)
		 VALUES ('a-tok', 'u1', '', 'T', 's-tok', '', '', ?, '', 'published', ?, 0, 0, 1, '', 0, '', 0, 0, '', 'zh-CN', '', '', 1, 1, 0, 0, ?, ?)`,
		mdBody, now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	escapedURL := strings.ReplaceAll(mp4Asset.PublicURL, "/", `\/`)
	if _, _, err := settings.PatchDomain(ctx, domain.SettingsDomainSite, []byte(`{"favicon_url":"`+escapedURL+`"}`), 0); err != nil {
		t.Fatal(err)
	}

	orphans, err := pipe.ScanOrphans(ctx, false)
	if err != nil {
		t.Fatalf("ScanOrphans failed: %v", err)
	}
	if len(orphans) != 1 || orphans[0].ID != pdfOrphan.ID {
		t.Fatalf("expected only %s as orphan, got %+v", pdfOrphan.ID, orphans)
	}
}

package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HugoSmits86/nativewebp"

	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/pkg/plugin"
)

func buildMinimalSyntheticGIF(canvasW, canvasH uint16, frameCount int, subW, subH uint16) []byte {
	var buf bytes.Buffer
	buf.WriteString("GIF89a")
	_ = binary.Write(&buf, binary.LittleEndian, canvasW)
	_ = binary.Write(&buf, binary.LittleEndian, canvasH)
	// Packed: Global Color Table Flag = 1 (0x80), Size = 0 (2 entries -> 6 bytes)
	buf.Write([]byte{0x80, 0x00, 0x00})
	buf.Write([]byte{0x00, 0x00, 0x00, 0xff, 0xff, 0xff})

	for i := 0; i < frameCount; i++ {
		// Graphic Control Extension (0x21, 0xF9)
		buf.Write([]byte{0x21, 0xf9, 0x04, 0x00, 0x01, 0x00, 0x00, 0x00})
		// Image Descriptor (0x2C)
		buf.WriteByte(0x2c)
		_ = binary.Write(&buf, binary.LittleEndian, uint16(0))
		_ = binary.Write(&buf, binary.LittleEndian, uint16(0))
		_ = binary.Write(&buf, binary.LittleEndian, subW)
		_ = binary.Write(&buf, binary.LittleEndian, subH)
		buf.WriteByte(0x00) // no local color table
		// Minimal LZW data block (minCodeSize=2, 2-byte sub-block, terminator 0x00)
		buf.Write([]byte{0x02, 0x02, 0x4c, 0x01, 0x00})
	}
	buf.WriteByte(0x3b) // Trailer
	return buf.Bytes()
}

func TestInspectGIFStreamLimits_RejectsDecompressionBombsZeroAlloc(t *testing.T) {
	// 1. Valid multi-frame standard library GIF passes pre-scan.
	g := &gif.GIF{
		Image: []*image.Paletted{
			image.NewPaletted(image.Rect(0, 0, 32, 32), color.Palette{color.Black, color.White}),
			image.NewPaletted(image.Rect(0, 0, 32, 32), color.Palette{color.Black, color.White}),
		},
		Delay: []int{5, 5},
	}
	var validGIF bytes.Buffer
	if err := gif.EncodeAll(&validGIF, g); err != nil {
		t.Fatalf("encode valid gif: %v", err)
	}
	if err := inspectGIFStreamLimits(validGIF.Bytes()); err != nil {
		t.Fatalf("expected valid GIF to pass inspectGIFStreamLimits, got %v", err)
	}

	// 2. > 400 frames rejected immediately (< 8 KB input).
	tooManyFrames := buildMinimalSyntheticGIF(16, 16, maxGIFFrames+5, 1, 1)
	if err := inspectGIFStreamLimits(tooManyFrames); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected >400 frame GIF bomb to be rejected, got %v", err)
	}

	// 3. Cumulative sub-frame pixels > 32,000,000 rejected immediately.
	subPixelBomb := buildMinimalSyntheticGIF(3000, 3000, 4, 3000, 3000) // 4 * 9M = 36M > 32M
	if err := inspectGIFStreamLimits(subPixelBomb); err == nil || !strings.Contains(err.Error(), "pixel budget") {
		t.Fatalf("expected cumulative sub-frame pixel bomb to be rejected, got %v", err)
	}

	// 4. Canvas * frames > 64,000,000 (even with 1x1 sub-frames!) rejected immediately.
	canvasFrameBomb := buildMinimalSyntheticGIF(4000, 4000, 5, 1, 1) // 5 * 16M = 80M > 64M
	if err := inspectGIFStreamLimits(canvasFrameBomb); err == nil || !strings.Contains(err.Error(), "canvas frame budget") {
		t.Fatalf("expected frames*canvasW*canvasH bomb to be rejected, got %v", err)
	}
}

type stubMediaStorageProvider struct {
	retURL string
	retErr error
}

func (s *stubMediaStorageProvider) Put(_ context.Context, _ string, r io.Reader, _ int64, _ string) (string, error) {
	_, _ = io.Copy(io.Discard, r)
	if s.retErr != nil {
		return "", s.retErr
	}
	return s.retURL, nil
}
func (s *stubMediaStorageProvider) Name() string                         { return "ext-broken-storage" }
func (s *stubMediaStorageProvider) Delete(context.Context, string) error { return nil }
func (s *stubMediaStorageProvider) HealthCheck(context.Context) error    { return s.retErr }

func TestMediaPipeline_Image25MiBCapAndPluginStorageFallback(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	uploadDir := filepath.Join(tmp, "uploads")
	if err := os.MkdirAll(uploadDir, 0o750); err != nil {
		t.Fatal(err)
	}
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(tmp, "media-r2.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatal(err)
	}

	kr, _, err := config.LoadOrInitKeyRing(strings.Repeat("m", 32), tmp)
	if err != nil {
		t.Fatal(err)
	}
	settings := repo.NewSettingsManager(store, kr, "en-US")
	if _, err := settings.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	// Configure global MaxUploadMB = 512 MiB and ActiveDriver = "ext-broken-storage"
	if _, _, err := settings.PatchDomain(ctx, domain.SettingsDomainStorage, []byte(`{"max_upload_mb":512,"active_driver":"ext-broken-storage"}`), 0); err != nil {
		t.Fatal(err)
	}

	cat, err := i18n.NewCatalog("en-US", "")
	if err != nil {
		t.Fatal(err)
	}
	plugMgr := plugin.NewManager(plugin.ManagerOptions{
		Store:      store,
		Keys:       kr,
		PluginsDir: filepath.Join(tmp, "plugins"),
		DataDir:    tmp,
		Catalog:    cat,
	})
	// 1. Plugin storage returning unsafe javascript: URL -> MUST be rejected by SanitizeAllowedURL!
	plugMgr.Providers().RegisterStorage("ext-broken-storage", &stubMediaStorageProvider{
		retURL: "javascript:alert(document.cookie)",
	})

	pipe := NewPipeline(PipelineOptions{
		Store:     store,
		Settings:  settings,
		UploadDir: uploadDir,
		Plugins:   plugMgr,
	})

	img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	img.SetNRGBA(0, 0, color.NRGBA{R: 10, G: 200, B: 90, A: 255})
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatal(err)
	}

	// Upload > 25 MiB padded PNG stream -> MUST be rejected by maxImageUploadBytes (25 MiB) despite MaxUploadMB=512!
	oversized := io.MultiReader(bytes.NewReader(pngBuf.Bytes()), io.LimitReader(zeroReader{}, maxImageUploadBytes+4096))
	if _, err := pipe.Upload(ctx, "u1", "huge.png", "image/png", oversized); err == nil || !strings.Contains(err.Error(), "exceeds maximum") {
		t.Fatalf("expected >25 MiB raster image upload to be rejected, got %v", err)
	}

	// Unsafe javascript: URL from plugin StorageProvider MUST be rejected!
	if _, err := pipe.Upload(ctx, "u1", "avatar.png", "image/png", bytes.NewReader(pngBuf.Bytes())); err == nil || !strings.Contains(err.Error(), "unsafe public URL") {
		t.Fatalf("expected unsafe javascript: public URL from storage plugin to be rejected, got %v", err)
	}

	// 2. Provider returning runtime error -> logs warning and falls back cleanly to local disk!
	plugMgr.Providers().RegisterStorage("ext-broken-storage", &stubMediaStorageProvider{
		retErr: errors.New("remote bucket unreachable"),
	})
	asset2, err := pipe.Upload(ctx, "u1", "avatar2.png", "image/png", bytes.NewReader(pngBuf.Bytes()))
	if err != nil {
		t.Fatalf("expected fallback to local storage on plugin Put error, got %v", err)
	}
	if asset2.StorageDriver != "local" || !strings.HasPrefix(asset2.PublicURL, "/uploads/") {
		t.Fatalf("expected local fallback driver, got driver=%q url=%q", asset2.StorageDriver, asset2.PublicURL)
	}

	// 3. Detached / unregistered plugin storage -> logs warning and falls back cleanly to local disk!
	plugMgr.Providers().DetachPlugin("ext-broken-storage")
	img.SetNRGBA(2, 2, color.NRGBA{R: 44, G: 88, B: 122, A: 255})
	pngBuf.Reset()
	_ = png.Encode(&pngBuf, img)
	asset3, err := pipe.Upload(ctx, "u1", "avatar3.png", "image/png", bytes.NewReader(pngBuf.Bytes()))
	if err != nil || asset3.StorageDriver != "local" {
		t.Fatalf("expected detached plugin driver to fall back to local storage, got asset=%+v err=%v", asset3, err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func buildTIFFOrientationBytes(bigEndian bool, orient uint16, withExifHeader bool) []byte {
	var out bytes.Buffer
	if withExifHeader {
		out.WriteString("Exif\x00\x00")
	}
	var bo binary.ByteOrder = binary.LittleEndian
	if bigEndian {
		out.WriteString("MM\x00\x2a")
		bo = binary.BigEndian
	} else {
		out.WriteString("II\x2a\x00")
	}
	_ = binary.Write(&out, bo, uint32(8))
	_ = binary.Write(&out, bo, uint16(1))
	_ = binary.Write(&out, bo, uint16(0x0112))
	_ = binary.Write(&out, bo, uint16(3))
	_ = binary.Write(&out, bo, uint32(1))
	_ = binary.Write(&out, bo, orient)
	_ = binary.Write(&out, bo, uint16(0))
	_ = binary.Write(&out, bo, uint32(0))
	return out.Bytes()
}

func injectPNGeXIfChunk(t *testing.T, rawPNG []byte, exifPayload []byte) []byte {
	t.Helper()
	// Insert `eXIf` chunk right before `IEND` (last 12 bytes of standard PNG).
	if len(rawPNG) < 20 {
		t.Fatal("png too short")
	}
	iendPos := len(rawPNG) - 12
	var out bytes.Buffer
	out.Write(rawPNG[:iendPos])
	_ = binary.Write(&out, binary.BigEndian, uint32(len(exifPayload)))
	out.WriteString("eXIf")
	out.Write(exifPayload)
	crc := crc32.NewIEEE()
	_, _ = crc.Write([]byte("eXIf"))
	_, _ = crc.Write(exifPayload)
	_ = binary.Write(&out, binary.BigEndian, crc.Sum32())
	out.Write(rawPNG[iendPos:])
	return out.Bytes()
}

func injectWebPMetadataChunks(t *testing.T, rawWebP []byte, exifPayload, xmpPayload, iccpPayload []byte) []byte {
	t.Helper()
	if len(rawWebP) < 12 || string(rawWebP[:4]) != "RIFF" || string(rawWebP[8:12]) != "WEBP" {
		t.Fatal("invalid base WebP")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(rawWebP))
	if err != nil {
		t.Fatalf("decode base WebP config: %v", err)
	}
	var body bytes.Buffer
	// Prepend VP8X extended header chunk (10 bytes payload: flags[4] + width-1[3] + height-1[3])
	var flags byte
	if len(iccpPayload) > 0 {
		flags |= 0x20
	}
	if len(exifPayload) > 0 {
		flags |= 0x08
	}
	if len(xmpPayload) > 0 {
		flags |= 0x04
	}
	vp8x := make([]byte, 10)
	vp8x[0] = flags
	wMinus1 := uint32(cfg.Width - 1)
	hMinus1 := uint32(cfg.Height - 1)
	vp8x[4] = byte(wMinus1)
	vp8x[5] = byte(wMinus1 >> 8)
	vp8x[6] = byte(wMinus1 >> 16)
	vp8x[7] = byte(hMinus1)
	vp8x[8] = byte(hMinus1 >> 8)
	vp8x[9] = byte(hMinus1 >> 16)

	writeChunk := func(fourCC string, payload []byte) {
		body.WriteString(fourCC)
		_ = binary.Write(&body, binary.LittleEndian, uint32(len(payload)))
		body.Write(payload)
		if len(payload)%2 == 1 {
			body.WriteByte(0)
		}
	}
	writeChunk("VP8X", vp8x)
	if len(iccpPayload) > 0 {
		writeChunk("ICCP", iccpPayload)
	}
	// Copy original VP8/VP8L bitstream chunks (ensuring 2-byte RIFF alignment before trailing chunks)
	body.Write(rawWebP[12:])
	if body.Len()%2 == 1 {
		body.WriteByte(0)
	}
	if len(exifPayload) > 0 {
		writeChunk("EXIF", exifPayload)
	}
	if len(xmpPayload) > 0 {
		writeChunk("XMP ", xmpPayload)
	}

	var out bytes.Buffer
	out.WriteString("RIFF")
	_ = binary.Write(&out, binary.LittleEndian, uint32(4+body.Len()))
	out.WriteString("WEBP")
	out.Write(body.Bytes())
	return out.Bytes()
}

func TestMediaRound3_PNGWebPEXIFLosslessStripAndNormalizeLocal(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	uploadDir := filepath.Join(tmp, "uploads")
	if err := os.MkdirAll(uploadDir, 0o750); err != nil {
		t.Fatal(err)
	}
	store, err := sqlx.Open(ctx, sqlx.Options{Driver: "sqlite", DSN: filepath.Join(tmp, "r3.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatal(err)
	}
	pipe := NewPipeline(PipelineOptions{Store: store, UploadDir: uploadDir})

	// Synthesize 40x20 PNG and inject Big-Endian raw TIFF Orientation=6 into `eXIf`
	srcImg := image.NewNRGBA(image.Rect(0, 0, 40, 20))
	srcImg.SetNRGBA(0, 0, color.NRGBA{R: 250, G: 20, B: 20, A: 255})
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, srcImg); err != nil {
		t.Fatal(err)
	}
	orientedPNG := injectPNGeXIfChunk(t, pngBuf.Bytes(), buildTIFFOrientationBytes(true, 6, false))
	if got := ReadEXIFOrientation(orientedPNG); got != 6 {
		t.Fatalf("PNG eXIf ReadEXIFOrientation = %d, want 6", got)
	}
	cleanPNG, mimePNG, wPNG, hPNG, changedPNG, err := NormalizeLocalImageBytes(orientedPNG, ".png")
	if err != nil || !changedPNG || mimePNG != "image/png" || wPNG != 20 || hPNG != 40 || ContainsEXIFOrGPS(cleanPNG) {
		t.Fatalf("NormalizeLocalImageBytes(PNG orient=6) = (%s, %dx%d, changed=%v, err=%v)", mimePNG, wPNG, hPNG, changedPNG, err)
	}

	// Encode a clean base WebP directly, then inject EXIF (orient=1 + canary), XMP, and ICCP chunks
	var webpSeedBuf bytes.Buffer
	if err := nativewebp.Encode(&webpSeedBuf, srcImg, nil); err != nil {
		t.Fatal(err)
	}
	baseWebP := webpSeedBuf.Bytes()

	// Verify WebP ReadEXIFOrientation works for both Exif\x00\x00 header and direct TIFF
	webpOrientWithHdr := injectWebPMetadataChunks(t, baseWebP, buildTIFFOrientationBytes(false, 8, true), nil, nil)
	if got := ReadEXIFOrientation(webpOrientWithHdr); got != 8 {
		t.Fatalf("WebP EXIF (with Exif header) ReadEXIFOrientation = %d, want 8", got)
	}
	webpOrientDirect := injectWebPMetadataChunks(t, baseWebP, buildTIFFOrientationBytes(true, 3, false), nil, nil)
	if got := ReadEXIFOrientation(webpOrientDirect); got != 3 {
		t.Fatalf("WebP EXIF (direct TIFF) ReadEXIFOrientation = %d, want 3", got)
	}

	// Lossless RIFF container strip on orient=1 WebP (must strip EXIF, XMP, ICCP, clear VP8X bits, preserve bitstream)
	webpWithMeta := injectWebPMetadataChunks(
		t,
		baseWebP,
		append(buildTIFFOrientationBytes(false, 1, true), []byte("SECRET_EXIF_GPS")...),
		[]byte("<x:xmpmeta>SECRET_XMP</x:xmpmeta>"),
		[]byte("FAKE_ICC_PROFILE_ODD_LEN!"),
	)
	if !ContainsEXIFOrGPS(webpWithMeta) {
		t.Fatal("expected webpWithMeta to be flagged by ContainsEXIFOrGPS")
	}
	strippedWebP, err := StripWebPMetadataLossless(webpWithMeta)
	if err != nil {
		t.Fatalf("StripWebPMetadataLossless failed: %v", err)
	}
	if ContainsEXIFOrGPS(strippedWebP) || bytes.Contains(strippedWebP, []byte("SECRET_EXIF")) || bytes.Contains(strippedWebP, []byte("SECRET_XMP")) || bytes.Contains(strippedWebP, []byte("FAKE_ICC")) {
		t.Fatal("StripWebPMetadataLossless failed to strip EXIF/XMP/ICCP chunks")
	}
	if string(strippedWebP[12:16]) == "VP8X" && (strippedWebP[20]&0x2C) != 0 {
		t.Fatalf("expected VP8X ICCP/EXIF/XMP flags cleared, got 0x%02x", strippedWebP[20])
	}
	// Upload orient=1 WebP through Pipeline.Upload -> must preserve compact bitstream size rather than inflating
	uploadedWebPAsset, err := pipe.Upload(ctx, "u1", "photo.webp", "image/webp", bytes.NewReader(webpWithMeta))
	if err != nil {
		t.Fatalf("Upload orient=1 WebP failed: %v", err)
	}
	if uploadedWebPAsset.SizeBytes != int64(len(strippedWebP)) {
		t.Fatalf("expected Pipeline.Upload on orient=1 WebP to use lossless container strip (%d bytes), got %d bytes", len(strippedWebP), uploadedWebPAsset.SizeBytes)
	}

	// Reject magic-byte spoofing in NormalizeLocalImageBytes
	if _, _, _, _, _, err := NormalizeLocalImageBytes([]byte("<svg onload=alert(1)>"), ".png"); err == nil {
		t.Fatal("expected spoofed SVG-as-PNG to be rejected by NormalizeLocalImageBytes")
	}
}

func TestMediaRound3_UnifiedOrphanScopesNavChaptersRedirectsComments(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	uploadDir := filepath.Join(tmp, "uploads")
	_ = os.MkdirAll(uploadDir, 0o750)
	store, err := sqlx.Open(ctx, sqlx.Options{Driver: "sqlite", DSN: filepath.Join(tmp, "orphans-r3.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := migrate.Up(ctx, store); err != nil {
		t.Fatal(err)
	}
	pipe := NewPipeline(PipelineOptions{Store: store, UploadDir: uploadDir})

	seedAsset := func(idStr, key string) domain.MediaAsset {
		a := domain.MediaAsset{
			ID:            idStr,
			StorageDriver: "local",
			ObjectKey:     key,
			PublicURL:     "/uploads/" + key,
			OriginalName:  filepath.Base(key),
			MIMEType:      "application/pdf",
			SizeBytes:     128,
			SHA256:        strings.Repeat("a", 64),
			CreatedAt:     1700000000000,
		}
		if _, err := store.ExecContext(ctx,
			`INSERT INTO media_assets (id, storage_driver, object_key, public_url, original_name, alt_text, mime_type, size_bytes, width, height, sha256, exif_stripped, uploader_id, created_at)
			 VALUES (?, ?, ?, ?, ?, '', ?, ?, 0, 0, ?, ?, 'u1', ?)`,
			a.ID, a.StorageDriver, a.ObjectKey, a.PublicURL, a.OriginalName, a.MIMEType, a.SizeBytes, a.SHA256, true, a.CreatedAt); err != nil {
			t.Fatal(err)
		}
		return a
	}

	aNav := seedAsset("m-nav", "2026/11/resume.pdf")
	aChap := seedAsset("m-chap", "2026/11/chapter1.pdf")
	aRedir := seedAsset("m-redir", "2026/11/legacy-whitepaper.pdf")
	aCmtLive := seedAsset("m-cmt-live", "2026/11/live-attachment.webp")
	aCmtDel := seedAsset("m-cmt-del", "2026/11/deleted-attachment.webp")

	_, err = store.ExecContext(ctx,
		`INSERT INTO nav_items (id, parent_id, title, i18n_titles_json, url, open_in_new_tab, icon, sort_order, is_enabled, created_at, updated_at)
		 VALUES ('n1', '', 'Resume', '{}', '/uploads/2026/11/resume.pdf', ?, '', 1, ?, 1, 1)`, false, true)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.ExecContext(ctx,
		`INSERT INTO book_chapters (id, book_id, parent_id, title, url, depth, sort_order)
		 VALUES ('bc1', 'b1', '', 'Ch 1', '/uploads/2026/11/chapter1.pdf', 0, 1)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.ExecContext(ctx,
		`INSERT INTO redirects (id, source_path, target_path, status_code, is_system_legacy, hit_count, created_at)
		 VALUES ('r1', '/old.pdf', '/uploads/2026/11/legacy-whitepaper.pdf', 301, ?, 0, 1)`, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.ExecContext(ctx,
		`INSERT INTO comments (id, target_type, target_id, parent_id, reply_to_name, user_id, guest_id_hash, env_fp_hash, author_name, author_url, avatar_url, notify_email_ciphertext, notify_locale, client_ip, ip_masked, geo_region, content_md, status, is_deleted, created_at, updated_at)
		 VALUES ('c1', 'article', 'a1', '', '', '', 'g1', 'e1', 'Alice', '', '', '', 'en-US', '', '', '', 'See ![img](/uploads/2026/11/live-attachment.webp)', 'approved', ?, 1, 1)`, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.ExecContext(ctx,
		`INSERT INTO comments (id, target_type, target_id, parent_id, reply_to_name, user_id, guest_id_hash, env_fp_hash, author_name, author_url, avatar_url, notify_email_ciphertext, notify_locale, client_ip, ip_masked, geo_region, content_md, status, is_deleted, created_at, updated_at)
		 VALUES ('c2', 'article', 'a1', '', '', '', 'g1', 'e1', 'Bob', '', '', '', 'en-US', '', '', '', 'Deleted ![img](/uploads/2026/11/deleted-attachment.webp)', 'approved', ?, 2, 2)`, true)
	if err != nil {
		t.Fatal(err)
	}

	tokens, err := CollectAllReferencedURLTokens(ctx, store)
	if err != nil {
		t.Fatalf("CollectAllReferencedURLTokens: %v", err)
	}
	for _, mustHave := range []string{"/uploads/2026/11/resume.pdf", "/uploads/2026/11/chapter1.pdf", "/uploads/2026/11/legacy-whitepaper.pdf", "/uploads/2026/11/live-attachment.webp"} {
		if _, ok := tokens[mustHave]; !ok {
			t.Fatalf("expected token %q in CollectAllReferencedURLTokens", mustHave)
		}
	}

	counts, err := CountAssetReferences(ctx, store, []domain.MediaAsset{aNav, aChap, aRedir, aCmtLive, aCmtDel})
	if err != nil {
		t.Fatalf("CountAssetReferences: %v", err)
	}
	if counts[aNav.ID] != 1 || counts[aChap.ID] != 1 || counts[aRedir.ID] != 1 || counts[aCmtLive.ID] != 1 || counts[aCmtDel.ID] != 0 {
		t.Fatalf("unexpected CountAssetReferences map: %+v", counts)
	}

	orphans, err := pipe.ScanOrphans(ctx, false)
	if err != nil {
		t.Fatalf("ScanOrphans: %v", err)
	}
	if len(orphans) != 1 || orphans[0].ID != aCmtDel.ID {
		t.Fatalf("expected only soft-deleted comment asset %q to be orphaned, got %+v", aCmtDel.ID, orphans)
	}
}


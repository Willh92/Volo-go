package media_test

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/media"
)

func TestMedia_StripEXIFFalsePreservesOriginalBytesAndMaxUploadMBEnforcement(t *testing.T) {
	ctx, _, settings, uploadDir, pipe, _, _ := setupMediaEnv(t)

	// 1. Configure StripEXIF=false, AdaptiveWebP=false, ImageQuality=100, MaxUploadMB=60 -> verbatim passthrough
	patchBytes, _ := json.Marshal(map[string]any{
		"strip_exif":    false,
		"adaptive_webp": false,
		"image_quality": 100,
		"max_upload_mb": 60,
	})
	if _, _, err := settings.PatchDomain(ctx, domain.SettingsDomainStorage, patchBytes, 0); err != nil {
		t.Fatalf("PatchDomain storage failed: %v", err)
	}

	gpsJPEG := buildJPEGWithEXIFAndGPS(t, 1)
	if !media.ContainsEXIFOrGPS(gpsJPEG) {
		t.Fatalf("expected input JPEG to carry EXIF/GPS")
	}

	asset, err := pipe.Upload(ctx, "admin-1", "keep-exif.jpg", "image/jpeg", bytes.NewReader(gpsJPEG))
	if err != nil {
		t.Fatalf("Upload with StripEXIF=false failed: %v", err)
	}
	if asset.EXIFStripped {
		t.Fatalf("expected asset.EXIFStripped=false when StripEXIF=false")
	}
	if asset.Width != 120 || asset.Height != 80 {
		t.Fatalf("expected 120x80 dimensions, got %dx%d", asset.Width, asset.Height)
	}

	storedBytes, err := os.ReadFile(filepath.Join(uploadDir, filepath.FromSlash(asset.ObjectKey)))
	if err != nil {
		t.Fatalf("read stored JPEG: %v", err)
	}
	if !bytes.Equal(storedBytes, gpsJPEG) {
		t.Fatalf("expected original JPEG bytes to be preserved verbatim when StripEXIF=false && AdaptiveWebP=false && ImageQuality=100 && orient=1")
	}
	if !media.ContainsEXIFOrGPS(storedBytes) {
		t.Fatalf("expected preserved JPEG to retain EXIF metadata")
	}

	// 1b. Configure StripEXIF=false, AdaptiveWebP=false, ImageQuality=45 -> JPEG MUST re-encode at target quality while keeping EXIF!
	patchQ45, _ := json.Marshal(map[string]any{
		"strip_exif":    false,
		"adaptive_webp": false,
		"image_quality": 45,
	})
	if _, _, err := settings.PatchDomain(ctx, domain.SettingsDomainStorage, patchQ45, 0); err != nil {
		t.Fatal(err)
	}
	hiImg := image.NewNRGBA(image.Rect(0, 0, 200, 150))
	for y := 0; y < 150; y++ {
		for x := 0; x < 200; x++ {
			hiImg.SetNRGBA(x, y, color.NRGBA{R: uint8((x*17 + y*31) & 0xff), G: uint8((x*29 + y*13) & 0xff), B: uint8((x ^ y) & 0xff), A: 255})
		}
	}
	var q100Buf bytes.Buffer
	if err := jpeg.Encode(&q100Buf, hiImg, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	q45Asset, err := pipe.Upload(ctx, "admin-1", "reencode-q45.jpg", "image/jpeg", bytes.NewReader(q100Buf.Bytes()))
	if err != nil {
		t.Fatalf("Upload JPEG at quality 45 failed: %v", err)
	}
	if q45Asset.SizeBytes >= int64(q100Buf.Len()) {
		t.Fatalf("expected ImageQuality=45 to re-encode JPEG smaller than quality-100 source (%d bytes), got %d bytes", q100Buf.Len(), q45Asset.SizeBytes)
	}
	q45ExifAsset, err := pipe.Upload(ctx, "admin-1", "reencode-exif-q45.jpg", "image/jpeg", bytes.NewReader(gpsJPEG))
	if err != nil {
		t.Fatalf("Upload EXIF JPEG at quality 45 failed: %v", err)
	}
	q45ExifDisk, err := os.ReadFile(filepath.Join(uploadDir, filepath.FromSlash(q45ExifAsset.ObjectKey)))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(q45ExifDisk, gpsJPEG) {
		t.Fatalf("expected ImageQuality=45 JPEG to re-encode rather than return verbatim bytes")
	}
	if !media.ContainsEXIFOrGPS(q45ExifDisk) {
		t.Fatalf("expected StripEXIF=false JPEG re-encode to retain EXIF metadata")
	}

	// 1c. AdaptiveWebP=true on JPEG input must NOT convert JPEG to lossless VP8L .webp!
	patchAdaptiveJPG, _ := json.Marshal(map[string]any{
		"strip_exif":    true,
		"adaptive_webp": true,
		"image_quality": 85,
	})
	if _, _, err := settings.PatchDomain(ctx, domain.SettingsDomainStorage, patchAdaptiveJPG, 0); err != nil {
		t.Fatal(err)
	}
	solidImg := image.NewNRGBA(image.Rect(0, 0, 160, 160))
	var solidJPGBuf bytes.Buffer
	if err := jpeg.Encode(&solidJPGBuf, solidImg, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	jpgAdaptiveAsset, err := pipe.Upload(ctx, "admin-1", "solid.jpg", "image/jpeg", bytes.NewReader(solidJPGBuf.Bytes()))
	if err != nil {
		t.Fatalf("Upload solid JPEG with AdaptiveWebP=true failed: %v", err)
	}
	if jpgAdaptiveAsset.MIMEType != "image/jpeg" || !strings.HasSuffix(jpgAdaptiveAsset.PublicURL, ".jpg") {
		t.Fatalf("expected JPEG input to skip lossless VP8L AdaptiveWebP conversion, got %s (%s)", jpgAdaptiveAsset.PublicURL, jpgAdaptiveAsset.MIMEType)
	}

	// Restore ImageQuality=100 & StripEXIF=false for 10 MiB passthrough check
	if _, _, err := settings.PatchDomain(ctx, domain.SettingsDomainStorage, patchBytes, 0); err != nil {
		t.Fatal(err)
	}

	// 2. Verify a 10 MiB camera JPEG succeeds when MaxUploadMB=60, while >25 MiB raster cap is enforced,
	// AND lowering MaxUploadMB=5 strictly rejects the 10 MiB image!
	tenMiBPad := 10 << 20
	var tenMiBJPG bytes.Buffer
	tenMiBJPG.Grow(len(gpsJPEG) + tenMiBPad)
	tenMiBJPG.Write(gpsJPEG)
	tenMiBJPG.Write(bytes.Repeat([]byte{0x00}, tenMiBPad))

	largeAsset, err := pipe.Upload(ctx, "admin-1", "camera-10mb.jpg", "image/jpeg", bytes.NewReader(tenMiBJPG.Bytes()))
	if err != nil {
		t.Fatalf("expected 10 MiB JPEG upload to succeed when MaxUploadMB=60, got: %v", err)
	}
	if largeAsset.SizeBytes < int64(tenMiBPad) {
		t.Fatalf("expected preserved >10 MiB size, got %d", largeAsset.SizeBytes)
	}

	// Lower MaxUploadMB to 5 MiB -> 10 MiB raster upload MUST be rejected!
	patch5MB, _ := json.Marshal(map[string]any{
		"max_upload_mb": 5,
	})
	if _, _, err := settings.PatchDomain(ctx, domain.SettingsDomainStorage, patch5MB, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := pipe.Upload(ctx, "admin-1", "exceeds-5mb.jpg", "image/jpeg", bytes.NewReader(tenMiBJPG.Bytes())); err == nil {
		t.Fatalf("expected 10 MiB JPEG upload to be rejected when MaxUploadMB=5")
	}

	// 3. Re-enable StripEXIF=true and verify strict stripping is enforced again
	patchStripTrue, _ := json.Marshal(map[string]any{
		"strip_exif":    true,
		"max_upload_mb": 50,
	})
	if _, _, err := settings.PatchDomain(ctx, domain.SettingsDomainStorage, patchStripTrue, 0); err != nil {
		t.Fatal(err)
	}
	strippedAsset, err := pipe.Upload(ctx, "admin-1", "stripped.jpg", "image/jpeg", bytes.NewReader(gpsJPEG))
	if err != nil {
		t.Fatalf("Upload with StripEXIF=true failed: %v", err)
	}
	if !strippedAsset.EXIFStripped {
		t.Fatalf("expected EXIFStripped=true")
	}
	strippedDisk, err := os.ReadFile(filepath.Join(uploadDir, filepath.FromSlash(strippedAsset.ObjectKey)))
	if err != nil {
		t.Fatal(err)
	}
	if media.ContainsEXIFOrGPS(strippedDisk) {
		t.Fatalf("expected EXIF/GPS to be stripped when StripEXIF=true")
	}
}

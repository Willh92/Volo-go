package media_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/media"
)

func TestMedia_StripEXIFFalsePreservesOriginalBytesAndMaxUploadMBEnforcement(t *testing.T) {
	ctx, _, settings, uploadDir, pipe, _, _ := setupMediaEnv(t)

	// 1. Configure StripEXIF=false, AdaptiveWebP=false, MaxUploadMB=60
	patchBytes, _ := json.Marshal(map[string]any{
		"strip_exif":    false,
		"adaptive_webp": false,
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
		t.Fatalf("expected original JPEG bytes to be preserved verbatim when StripEXIF=false && AdaptiveWebP=false && orient=1")
	}
	if !media.ContainsEXIFOrGPS(storedBytes) {
		t.Fatalf("expected preserved JPEG to retain EXIF metadata")
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

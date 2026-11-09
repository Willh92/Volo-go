package media_test

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/media"
)

func TestMedia_StripEXIFFalsePreservesOriginalBytesAndLargeImageLimit(t *testing.T) {
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

	// 2. Verify raster image > 25 MiB succeeds when MaxUploadMB=60 (while staying well within maxImagePixels)
	img := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	img.SetNRGBA(0, 0, color.NRGBA{R: 10, G: 20, B: 30, A: 255})
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatal(err)
	}
	// Inject a valid ancillary PNG tEXt chunk (~26 MiB) before IEND so the PNG is >25 MiB with tiny pixel count.
	rawPNG := pngBuf.Bytes()
	iendOffset := len(rawPNG) - 12
	chunkPayloadLen := 26 << 20
	var largePNG bytes.Buffer
	largePNG.Grow(len(rawPNG) + chunkPayloadLen + 12)
	largePNG.Write(rawPNG[:iendOffset])
	// PNG decoders ignore trailing bytes after IEND or valid chunks; easier & valid: append ancillary chunk with valid CRC,
	// or test JPEG trailing padding after EOI? Wait: Go's jpeg.Decode stops at EOI and ignores trailing bytes!
	var jpgBuf bytes.Buffer
	jpgBuf.Grow(len(gpsJPEG) + chunkPayloadLen)
	jpgBuf.Write(gpsJPEG)
	jpgBuf.Write(bytes.Repeat([]byte{0x00}, chunkPayloadLen))

	largeAsset, err := pipe.Upload(ctx, "admin-1", "large-camera.jpg", "image/jpeg", bytes.NewReader(jpgBuf.Bytes()))
	if err != nil {
		t.Fatalf("expected 26 MiB JPEG upload to succeed when MaxUploadMB=60, got: %v", err)
	}
	if largeAsset.SizeBytes < int64(chunkPayloadLen) {
		t.Fatalf("expected preserved >26 MiB size, got %d", largeAsset.SizeBytes)
	}

	// 3. Re-enable StripEXIF=true and verify strict stripping is enforced again
	patchStripTrue, _ := json.Marshal(map[string]any{
		"strip_exif": true,
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

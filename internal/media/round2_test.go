package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

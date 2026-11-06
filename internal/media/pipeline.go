package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/HugoSmits86/nativewebp"
	_ "golang.org/x/image/webp"

	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/repo"
	"github.com/Willh92/Volo-go/internal/security"
	"github.com/Willh92/Volo-go/pkg/id"
	"github.com/Willh92/Volo-go/pkg/plugin"
)

const (
	maxImagePixels    = 64_000_000 // 64 Megapixel decompression-bomb ceiling
	maxImageDimension = 16384
)

// PipelineOptions configures the privacy-first media upload, EXIF-strip, WebP, S3, and orphan scanner pipeline.
type PipelineOptions struct {
	Store          *sqlx.Store
	Settings       *repo.SettingsManager
	UploadDir      string
	OutboundDialer *security.OutboundSafeDialer
	Plugins        *plugin.Manager
	OnDimsUpdated  func(publicURL string, width, height int)
}

// Pipeline implements both `server.MediaProcessor` and `plugin.StorageProvider`.
type Pipeline struct {
	store          *sqlx.Store
	settings       *repo.SettingsManager
	uploadDir      string
	outboundDialer *security.OutboundSafeDialer
	plugins        *plugin.Manager
	onDimsUpdated  func(publicURL string, width, height int)
}

// NewPipeline creates a privacy-first media pipeline.
func NewPipeline(opts PipelineOptions) *Pipeline {
	return &Pipeline{
		store:          opts.Store,
		settings:       opts.Settings,
		uploadDir:      opts.UploadDir,
		outboundDialer: opts.OutboundDialer,
		plugins:        opts.Plugins,
		onDimsUpdated:  opts.OnDimsUpdated,
	}
}

// Name satisfies `plugin.StorageProvider`.
func (p *Pipeline) Name() string { return "builtin-media" }

// Put satisfies `plugin.StorageProvider` for raw binary storage according to the active storage driver.
func (p *Pipeline) Put(ctx context.Context, objectKey string, r io.Reader, size int64, contentType string) (string, error) {
	maxBytes := p.maxUploadBytes()
	if size > maxBytes {
		return "", fmt.Errorf("payload size %d exceeds maximum %d bytes", size, maxBytes)
	}
	raw, err := readBounded(r, maxBytes)
	if err != nil {
		return "", err
	}
	_, pubURL, err := p.storeRawObject(ctx, objectKey, raw, contentType)
	return pubURL, err
}

// HealthCheck satisfies `plugin.StorageProvider`.
func (p *Pipeline) HealthCheck(ctx context.Context) error {
	return p.TestStorage(ctx, nil, "")
}

// Upload validates magic bytes, blocks decompression bombs (`<= 64 MP`), normalizes EXIF orientation (`1..8`),
// strips 100% of EXIF/GPS/camera metadata via clean pixel re-encoding, adaptively encodes pure-Go WebP,
// extracts intrinsic `width`/`height` for zero-CLS rendering, and persists to Local/S3/Plugin storage.
func (p *Pipeline) Upload(ctx context.Context, uploaderID string, origFilename string, _ string, r io.Reader) (*domain.MediaAsset, error) {
	maxBytes := p.maxUploadBytes()
	raw, err := readBounded(r, maxBytes)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, errors.New("empty file upload")
	}

	origBase := filepath.Base(strings.TrimSpace(origFilename))
	ext := strings.ToLower(filepath.Ext(origBase))
	if ext == ".svg" || ext == ".html" || ext == ".htm" || ext == ".xml" || ext == ".xhtml" {
		return nil, errors.New("active markup uploads (.svg/.html/.xml) are forbidden for stored-XSS safety")
	}

	kind, detectedMIME, err := classifyAndVerifyMagic(ext, raw)
	if err != nil {
		return nil, err
	}

	stCfg := p.storageSettings()
	finalBytes := raw
	finalExt := ext
	finalMIME := detectedMIME
	var width, height int
	exifStripped := false

	if kind == mediaKindImage {
		// Step 1: Decompression-bomb pre-check BEFORE allocating full pixel buffers!
		cfg, formatName, cfgErr := image.DecodeConfig(bytes.NewReader(raw))
		if cfgErr != nil {
			return nil, fmt.Errorf("invalid or corrupt image header: %w", cfgErr)
		}
		if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxImageDimension || cfg.Height > maxImageDimension ||
			int64(cfg.Width)*int64(cfg.Height) > maxImagePixels {
			return nil, fmt.Errorf("image dimensions %dx%d exceed 64MP decompression safety limit", cfg.Width, cfg.Height)
		}

		// Multi-frame animated GIFs: preserve animation if clean (strip if EXIF/GPS present or single-frame)
		isAnimatedGIF := false
		if formatName == "gif" {
			if g, gErr := gif.DecodeAll(bytes.NewReader(raw)); gErr == nil && len(g.Image) > 1 {
				isAnimatedGIF = true
				width, height = g.Config.Width, g.Config.Height
				// Re-encode clean GIF frames without comment/application metadata extensions
				var cleanGIF bytes.Buffer
				if encErr := gif.EncodeAll(&cleanGIF, g); encErr == nil {
					finalBytes = cleanGIF.Bytes()
					exifStripped = true
				}
			}
		}

		if !isAnimatedGIF {
			orient := ReadEXIFOrientation(raw)
			img, _, decErr := image.Decode(bytes.NewReader(raw))
			if decErr != nil {
				return nil, fmt.Errorf("image decode failed: %w", decErr)
			}
			upright := ApplyEXIFOrientation(img, orient)
			b := upright.Bounds()
			width, height = b.Dx(), b.Dy()

			quality := stCfg.ImageQuality
			if quality < 40 || quality > 100 {
				quality = 85
			}

			// Always re-encode fresh pixel buffers so 100% of EXIF GPS, camera serials, thumbnails, and XMP are stripped!
			var baselineBuf bytes.Buffer
			baselineExt := ext
			baselineMIME := detectedMIME
			switch formatName {
			case "png", "gif":
				enc := png.Encoder{CompressionLevel: png.DefaultCompression}
				if err := enc.Encode(&baselineBuf, upright); err != nil {
					return nil, fmt.Errorf("png re-encode failed: %w", err)
				}
				baselineExt = ".png"
				baselineMIME = "image/png"
			case "webp":
				if err := nativewebp.Encode(&baselineBuf, upright, nil); err != nil {
					return nil, fmt.Errorf("webp re-encode failed: %w", err)
				}
				baselineExt = ".webp"
				baselineMIME = "image/webp"
			default: // jpeg
				if err := jpeg.Encode(&baselineBuf, upright, &jpeg.Options{Quality: quality}); err != nil {
					return nil, fmt.Errorf("jpeg re-encode failed: %w", err)
				}
				baselineExt = ".jpg"
				baselineMIME = "image/jpeg"
			}

			finalBytes = baselineBuf.Bytes()
			finalExt = baselineExt
			finalMIME = baselineMIME

			// Pure-Go Adaptive WebP: encode lossless WebP and adopt it whenever smaller than baseline (or when already WebP)
			if stCfg.AdaptiveWebP && formatName != "webp" {
				var webpBuf bytes.Buffer
				if wErr := nativewebp.Encode(&webpBuf, upright, nil); wErr == nil && webpBuf.Len() > 0 && webpBuf.Len() < len(finalBytes) {
					finalBytes = webpBuf.Bytes()
					finalExt = ".webp"
					finalMIME = "image/webp"
				}
			}

			if ContainsEXIFOrGPS(finalBytes) {
				return nil, errors.New("privacy invariant violation: residual metadata detected after pixel re-encode")
			}
			exifStripped = true
		}
	}

	now := time.Now().UTC()
	objectKey := fmt.Sprintf("%s/%s%s", now.Format("2006/01"), strings.ToLower(id.Next()), finalExt)
	driverUsed, publicURL, err := p.storeRawObject(ctx, objectKey, finalBytes, finalMIME)
	if err != nil {
		return nil, err
	}

	sum := sha256.Sum256(finalBytes)
	asset := domain.MediaAsset{
		ID:            id.Next(),
		StorageDriver: driverUsed,
		ObjectKey:     objectKey,
		PublicURL:     publicURL,
		OriginalName:  origBase,
		MIMEType:      finalMIME,
		SizeBytes:     int64(len(finalBytes)),
		Width:         width,
		Height:        height,
		SHA256:        hex.EncodeToString(sum[:]),
		EXIFStripped:  exifStripped,
		UploaderID:    uploaderID,
		CreatedAt:     now.UnixMilli(),
	}

	if _, err := p.store.ExecContext(ctx,
		`INSERT INTO media_assets (id, storage_driver, object_key, public_url, original_name, mime_type, size_bytes, width, height, sha256, exif_stripped, uploader_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		asset.ID, asset.StorageDriver, asset.ObjectKey, asset.PublicURL, asset.OriginalName,
		asset.MIMEType, asset.SizeBytes, asset.Width, asset.Height, asset.SHA256,
		asset.EXIFStripped, asset.UploaderID, asset.CreatedAt,
	); err != nil {
		return nil, err
	}

	if width > 0 && height > 0 && p.onDimsUpdated != nil {
		p.onDimsUpdated(asset.PublicURL, width, height)
	}
	return &asset, nil
}

// Delete removes a MediaAsset by ID from both backing storage and `media_assets`.
func (p *Pipeline) Delete(ctx context.Context, assetID string) error {
	asset, err := sqlx.Get[domain.MediaAsset](ctx, p.store, `SELECT * FROM media_assets WHERE id = ?`, strings.TrimSpace(assetID))
	if err != nil {
		return err
	}
	return p.deleteAssetRecord(ctx, asset)
}

func (p *Pipeline) deleteAssetRecord(ctx context.Context, asset domain.MediaAsset) error {
	switch asset.StorageDriver {
	case "s3":
		if cli, err := p.buildS3Client(ctx, nil, ""); err == nil {
			_ = cli.DeleteObject(ctx, asset.ObjectKey)
		}
	case "local", "":
		if asset.ObjectKey != "" {
			if err := safeDeleteLocalUpload(p.uploadDir, asset.ObjectKey); err != nil {
				return err
			}
		}
	default:
		if p.plugins != nil {
			if sp := p.plugins.Providers().ActiveStorage(); sp != nil && sp.Name() == asset.StorageDriver {
				_ = sp.Delete(ctx, asset.ObjectKey)
			}
		}
	}
	_, err := p.store.ExecContext(ctx, `DELETE FROM media_assets WHERE id = ?`, asset.ID)
	return err
}

// TestStorage validates either local directory write/delete health or live S3 SigV4 bucket connectivity.
func (p *Pipeline) TestStorage(ctx context.Context, override *domain.StorageSettings, overrideSecret string) error {
	cfg := p.storageSettings()
	if override != nil {
		cfg = *override
	}
	driver := strings.ToLower(strings.TrimSpace(cfg.ActiveDriver))
	switch driver {
	case "", "local":
		if err := os.MkdirAll(p.uploadDir, 0o750); err != nil {
			return err
		}
		tmp, err := os.CreateTemp(p.uploadDir, ".volo-probe-*")
		if err != nil {
			return err
		}
		name := tmp.Name()
		_, wErr := tmp.WriteString("ok")
		_ = tmp.Close()
		_ = os.Remove(name)
		return wErr
	case "s3":
		cli, err := p.buildS3Client(ctx, &cfg, overrideSecret)
		if err != nil {
			return err
		}
		return cli.HealthCheck(ctx)
	default:
		if p.plugins != nil {
			if sp := p.plugins.Providers().ActiveStorage(); sp != nil && sp.Name() == driver {
				return sp.HealthCheck(ctx)
			}
		}
		return fmt.Errorf("unknown storage driver %q", driver)
	}
}

func (p *Pipeline) storeRawObject(ctx context.Context, objectKey string, data []byte, contentType string) (string, string, error) {
	cfg := p.storageSettings()
	driver := strings.ToLower(strings.TrimSpace(cfg.ActiveDriver))

	// Check if an external plugin StorageProvider is explicitly selected
	if driver != "" && driver != "local" && driver != "s3" && p.plugins != nil {
		if sp := p.plugins.Providers().ActiveStorage(); sp != nil && sp.Name() == driver {
			u, err := sp.Put(ctx, objectKey, bytes.NewReader(data), int64(len(data)), contentType)
			return driver, u, err
		}
	}

	if driver == "s3" {
		cli, err := p.buildS3Client(ctx, &cfg, "")
		if err != nil {
			return "", "", err
		}
		pubURL, err := cli.PutBytes(ctx, objectKey, data, contentType)
		return "s3", pubURL, err
	}

	// Default: Local UploadDir storage
	cleanRel := filepath.Clean(filepath.FromSlash(strings.TrimLeft(objectKey, "/")))
	if cleanRel == "." || cleanRel == ".." || strings.HasPrefix(cleanRel, ".."+string(os.PathSeparator)) {
		return "", "", errors.New("invalid local object key")
	}
	dest := filepath.Join(p.uploadDir, cleanRel)
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(dest, data, 0o640); err != nil {
		return "", "", err
	}
	return "local", "/uploads/" + strings.TrimLeft(filepath.ToSlash(cleanRel), "/"), nil
}

func (p *Pipeline) buildS3Client(ctx context.Context, override *domain.StorageSettings, overrideSecret string) (*S3Client, error) {
	cfg := p.storageSettings()
	if override != nil {
		cfg = *override
	}
	secKey := strings.TrimSpace(overrideSecret)
	if secKey == "" && p.settings != nil {
		sec, err := p.settings.GetStorageSecrets(ctx)
		if err != nil {
			return nil, err
		}
		secKey = sec.S3SecretKey
	}
	return NewS3Client(S3Config{
		Endpoint:      cfg.S3Endpoint,
		Region:        cfg.S3Region,
		Bucket:        cfg.S3Bucket,
		AccessKey:     cfg.S3AccessKey,
		SecretKey:     secKey,
		PublicBaseURL: cfg.S3PublicBaseURL,
		KeyPrefix:     cfg.S3KeyPrefix,
		PathStyle:     cfg.S3PathStyle,
	}, p.outboundDialer)
}

func (p *Pipeline) storageSettings() domain.StorageSettings {
	if p.settings != nil {
		return p.settings.Snapshot().Storage
	}
	return domain.StorageSettings{
		ActiveDriver: "local",
		MaxUploadMB:  25,
		StripEXIF:    true,
		AdaptiveWebP: true,
		ImageQuality: 85,
	}
}

func (p *Pipeline) maxUploadBytes() int64 {
	mb := p.storageSettings().MaxUploadMB
	if mb <= 0 {
		mb = 25
	}
	if mb > 512 {
		mb = 512
	}
	return int64(mb) << 20
}

type mediaKind int

const (
	mediaKindImage mediaKind = iota + 1
	mediaKindAudio
	mediaKindDocument
)

func classifyAndVerifyMagic(ext string, raw []byte) (mediaKind, string, error) {
	head := raw
	if len(head) > 512 {
		head = head[:512]
	}
	sniff := http.DetectContentType(head)

	switch ext {
	case ".jpg", ".jpeg":
		if len(raw) < 3 || raw[0] != 0xFF || raw[1] != 0xD8 || raw[2] != 0xFF {
			return 0, "", errors.New("file magic bytes do not match JPEG signature")
		}
		return mediaKindImage, "image/jpeg", nil
	case ".png":
		if !bytes.HasPrefix(raw, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) {
			return 0, "", errors.New("file magic bytes do not match PNG signature")
		}
		return mediaKindImage, "image/png", nil
	case ".gif":
		if !bytes.HasPrefix(raw, []byte("GIF87a")) && !bytes.HasPrefix(raw, []byte("GIF89a")) {
			return 0, "", errors.New("file magic bytes do not match GIF signature")
		}
		return mediaKindImage, "image/gif", nil
	case ".webp":
		if len(raw) < 12 || string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WEBP" {
			return 0, "", errors.New("file magic bytes do not match WebP RIFF signature")
		}
		return mediaKindImage, "image/webp", nil
	case ".mp3":
		if len(raw) < 3 || (!bytes.HasPrefix(raw, []byte("ID3")) && (raw[0] != 0xFF || (raw[1]&0xE0) != 0xE0)) {
			return 0, "", errors.New("file magic bytes do not match MP3 signature")
		}
		return mediaKindAudio, "audio/mpeg", nil
	case ".m4a":
		if len(raw) < 12 || string(raw[4:8]) != "ftyp" {
			return 0, "", errors.New("file magic bytes do not match M4A/MP4 ftyp signature")
		}
		return mediaKindAudio, "audio/mp4", nil
	case ".ogg":
		if !bytes.HasPrefix(raw, []byte("OggS")) {
			return 0, "", errors.New("file magic bytes do not match OggS signature")
		}
		return mediaKindAudio, "audio/ogg", nil
	case ".wav":
		if len(raw) < 12 || string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
			return 0, "", errors.New("file magic bytes do not match WAV RIFF signature")
		}
		return mediaKindAudio, "audio/wav", nil
	case ".pdf":
		if !bytes.HasPrefix(raw, []byte("%PDF-")) {
			return 0, "", errors.New("file magic bytes do not match PDF signature")
		}
		return mediaKindDocument, "application/pdf", nil
	case ".zip":
		if !bytes.HasPrefix(raw, []byte("PK\x03\x04")) && !bytes.HasPrefix(raw, []byte("PK\x05\x06")) {
			return 0, "", errors.New("file magic bytes do not match ZIP signature")
		}
		return mediaKindDocument, "application/zip", nil
	default:
		return 0, sniff, fmt.Errorf("unsupported upload extension %q", ext)
	}
}

func readBounded(r io.Reader, maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("upload exceeds maximum size (%d bytes)", maxBytes)
	}
	return data, nil
}

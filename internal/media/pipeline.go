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
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	maxImagePixels      = 24_000_000 // 24 Megapixel decompression-bomb ceiling
	maxGIFTotalPixels   = 32_000_000 // 32 Megapixel animated GIF multi-frame ceiling
	maxGIFFrames        = 400        // Maximum frames permitted in an uploaded GIF
	maxImageDimension   = 16384
	maxImageUploadBytes = 25 << 20 // 25 MiB in-memory cap for raster image uploads
)

var transcodeSem = make(chan struct{}, 2)

func isForbiddenUploadExt(ext string) bool {
	switch strings.ToLower(strings.TrimSpace(ext)) {
	case ".svg", ".html", ".htm", ".xml", ".xhtml", ".js", ".mjs":
		return true
	default:
		return false
	}
}

func isForbiddenUploadMIME(ct string) bool {
	lower := strings.ToLower(strings.TrimSpace(ct))
	return strings.Contains(lower, "svg") ||
		strings.Contains(lower, "html") ||
		strings.Contains(lower, "xml") ||
		strings.Contains(lower, "javascript") ||
		strings.Contains(lower, "ecmascript")
}

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

	dimsMu        sync.RWMutex
	onDimsUpdated func(publicURL string, width, height int)
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

// SetOnDimsUpdated registers or updates the callback invoked when an uploaded image's intrinsic dimensions are recorded.
func (p *Pipeline) SetOnDimsUpdated(fn func(publicURL string, width, height int)) {
	if p == nil {
		return
	}
	p.dimsMu.Lock()
	p.onDimsUpdated = fn
	p.dimsMu.Unlock()
}

// Name satisfies `plugin.StorageProvider`.
func (p *Pipeline) Name() string { return "builtin-media" }

// Put satisfies `plugin.StorageProvider` for raw binary storage according to the active storage driver.
func (p *Pipeline) Put(ctx context.Context, objectKey string, r io.Reader, size int64, contentType string) (string, error) {
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(objectKey)))
	if isForbiddenUploadExt(ext) || isForbiddenUploadMIME(contentType) {
		return "", errors.New("active markup or script uploads (.svg/.html/.xml/.js) are forbidden")
	}
	maxBytes := p.maxUploadBytes()
	if size > maxBytes {
		return "", fmt.Errorf("payload size %d exceeds maximum %d bytes", size, maxBytes)
	}
	tmpFile, tmpPath, written, _, err := p.spoolToTempFile(r, maxBytes)
	if err != nil {
		return "", err
	}
	defer func() {
		if tmpFile != nil {
			_ = tmpFile.Close()
		}
		if tmpPath != "" {
			_ = os.Remove(tmpPath)
		}
	}()
	_, pubURL, err := p.storeStreamObject(ctx, objectKey, &tmpFile, &tmpPath, written, contentType)
	return pubURL, err
}

// HealthCheck satisfies `plugin.StorageProvider`.
func (p *Pipeline) HealthCheck(ctx context.Context) error {
	return p.TestStorage(ctx, nil, "")
}

// Upload validates magic bytes, blocks decompression bombs (`<= 24 MP`), normalizes EXIF orientation (`1..8`),
// strips 100% of EXIF/GPS/camera metadata via clean pixel re-encoding, adaptively encodes pure-Go WebP,
// extracts intrinsic `width`/`height` for zero-CLS rendering, and persists to Local/S3/Plugin storage.
func (p *Pipeline) Upload(ctx context.Context, uploaderID string, origFilename string, _ string, r io.Reader) (*domain.MediaAsset, error) {
	origBase := filepath.Base(strings.TrimSpace(origFilename))
	ext := strings.ToLower(filepath.Ext(origBase))
	if isForbiddenUploadExt(ext) {
		return nil, errors.New("active markup or script uploads (.svg/.html/.xml/.js) are forbidden for stored-XSS safety")
	}

	var headBuf [512]byte
	nHead, hErr := io.ReadFull(r, headBuf[:])
	if hErr != nil && !errors.Is(hErr, io.EOF) && !errors.Is(hErr, io.ErrUnexpectedEOF) {
		return nil, hErr
	}
	if nHead == 0 {
		return nil, errors.New("empty file upload")
	}
	head := headBuf[:nHead]

	kind, detectedMIME, err := classifyAndVerifyMagic(ext, head)
	if err != nil {
		return nil, err
	}

	maxBytes := p.maxUploadBytes()
	fullReader := io.MultiReader(bytes.NewReader(head), r)

	stCfg := p.storageSettings()
	finalExt := ext
	finalMIME := detectedMIME
	var (
		width, height int
		exifStripped  bool
		driverUsed    string
		publicURL     string
		sizeBytes     int64
		sha256Hex     string
	)

	now := time.Now().UTC()

	if kind == mediaKindImage {
		imgMaxBytes := maxBytes
		if imgMaxBytes > maxImageUploadBytes {
			imgMaxBytes = maxImageUploadBytes
		}
		raw, rErr := readBounded(fullReader, imgMaxBytes)
		if rErr != nil {
			return nil, rErr
		}
		var finalBytes []byte

		select {
		case transcodeSem <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		err = func() error {
			defer func() { <-transcodeSem }()

			// Step 1: Decompression-bomb pre-check BEFORE allocating full pixel buffers!
			cfg, formatName, cfgErr := image.DecodeConfig(bytes.NewReader(raw))
			if cfgErr != nil {
				return fmt.Errorf("invalid or corrupt image header: %w", cfgErr)
			}
			if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxImageDimension || cfg.Height > maxImageDimension ||
				int64(cfg.Width)*int64(cfg.Height) > maxImagePixels {
				return fmt.Errorf("image dimensions %dx%d exceed 24MP decompression safety limit", cfg.Width, cfg.Height)
			}

			// Multi-frame animated GIFs: pre-scan stream frame/pixel bounds in O(len(raw)) with zero image allocations BEFORE gif.DecodeAll!
			isAnimatedGIF := false
			if formatName == "gif" {
				if err := inspectGIFStreamLimits(raw); err != nil {
					return err
				}
				if g, gErr := gif.DecodeAll(bytes.NewReader(raw)); gErr == nil && len(g.Image) > 1 {
					totalGIFPixels := int64(cfg.Width) * int64(cfg.Height) * int64(len(g.Image))
					if totalGIFPixels > maxGIFTotalPixels*2 {
						return fmt.Errorf("animated GIF frame budget (%d pixels across %d frames) exceeds safety limit", totalGIFPixels, len(g.Image))
					}
					isAnimatedGIF = true
					raw = nil
					width, height = g.Config.Width, g.Config.Height
					var cleanGIF bytes.Buffer
					if encErr := gif.EncodeAll(&cleanGIF, g); encErr != nil {
						return fmt.Errorf("gif re-encode failed: %w", encErr)
					}
					finalBytes = cleanGIF.Bytes()
					exifStripped = true
				}
			}

			if !isAnimatedGIF {
				orient := ReadEXIFOrientation(raw)
				if detectedMIME == "image/webp" && orient == 1 {
					stripped, sErr := StripWebPMetadataLossless(raw)
					raw = nil
					if sErr != nil {
						return fmt.Errorf("webp metadata strip failed: %w", sErr)
					}
					if ContainsEXIFOrGPS(stripped) {
						return errors.New("privacy invariant violation: residual metadata detected after webp container strip")
					}
					width, height = cfg.Width, cfg.Height
					finalBytes = stripped
					finalExt = ".webp"
					finalMIME = "image/webp"
					exifStripped = true
					return nil
				}

				img, _, decErr := image.Decode(bytes.NewReader(raw))
				raw = nil // Release compressed upload buffer before pixel transforms & WebP encoding.
				if decErr != nil {
					return fmt.Errorf("image decode failed: %w", decErr)
				}
				upright := ApplyEXIFOrientation(img, orient)
				img = nil // Release pre-orientation pixel buffer promptly before encoding.
				b := upright.Bounds()
				width, height = b.Dx(), b.Dy()

				quality := stCfg.ImageQuality
				if quality < 40 || quality > 100 {
					quality = 85
				}

				// Always re-encode fresh pixel buffers so 100% of EXIF GPS, camera serials, thumbnails, and XMP are stripped!
				var baselineBuf bytes.Buffer
				var baselineExt string
				var baselineMIME string
				switch formatName {
				case "png", "gif":
					enc := png.Encoder{CompressionLevel: png.DefaultCompression}
					if err := enc.Encode(&baselineBuf, upright); err != nil {
						return fmt.Errorf("png re-encode failed: %w", err)
					}
					baselineExt = ".png"
					baselineMIME = "image/png"
				case "webp":
					if err := nativewebp.Encode(&baselineBuf, upright, nil); err != nil {
						return fmt.Errorf("webp re-encode failed: %w", err)
					}
					baselineExt = ".webp"
					baselineMIME = "image/webp"
				default: // jpeg
					if err := jpeg.Encode(&baselineBuf, upright, &jpeg.Options{Quality: quality}); err != nil {
						return fmt.Errorf("jpeg re-encode failed: %w", err)
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
				upright = nil

				if ContainsEXIFOrGPS(finalBytes) {
					return errors.New("privacy invariant violation: residual metadata detected after pixel re-encode")
				}
				exifStripped = true
			}
			return nil
		}()
		if err != nil {
			return nil, err
		}

		objectKey := fmt.Sprintf("%s/%s%s", now.Format("2006/01"), strings.ToLower(id.Next()), finalExt)
		driverUsed, publicURL, err = p.storeRawObject(ctx, objectKey, finalBytes, finalMIME)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(finalBytes)
		sizeBytes = int64(len(finalBytes))
		sha256Hex = hex.EncodeToString(sum[:])

		asset := domain.MediaAsset{
			ID:            id.Next(),
			StorageDriver: driverUsed,
			ObjectKey:     objectKey,
			PublicURL:     publicURL,
			OriginalName:  origBase,
			AltText:       "",
			MIMEType:      finalMIME,
			SizeBytes:     sizeBytes,
			Width:         width,
			Height:        height,
			SHA256:        sha256Hex,
			EXIFStripped:  exifStripped,
			UploaderID:    uploaderID,
			CreatedAt:     now.UnixMilli(),
		}
		if err := p.insertMediaAsset(ctx, asset); err != nil {
			return nil, err
		}
		p.dimsMu.RLock()
		cb := p.onDimsUpdated
		p.dimsMu.RUnlock()
		if width > 0 && height > 0 && cb != nil {
			cb(asset.PublicURL, width, height)
		}
		return &asset, nil
	}

	// Non-image uploads (audio/*, video/*, application/pdf, application/zip, etc.):
	// stream via temporary file on disk instead of holding up to 512 MiB in heap.
	tmpFile, tmpPath, written, sumHex, err := p.spoolToTempFile(fullReader, maxBytes)
	if err != nil {
		return nil, err
	}
	defer func() {
		if tmpFile != nil {
			_ = tmpFile.Close()
		}
		if tmpPath != "" {
			_ = os.Remove(tmpPath)
		}
	}()

	objectKey := fmt.Sprintf("%s/%s%s", now.Format("2006/01"), strings.ToLower(id.Next()), finalExt)
	driverUsed, publicURL, err = p.storeStreamObject(ctx, objectKey, &tmpFile, &tmpPath, written, finalMIME)
	if err != nil {
		return nil, err
	}

	asset := domain.MediaAsset{
		ID:            id.Next(),
		StorageDriver: driverUsed,
		ObjectKey:     objectKey,
		PublicURL:     publicURL,
		OriginalName:  origBase,
		AltText:       "",
		MIMEType:      finalMIME,
		SizeBytes:     written,
		Width:         0,
		Height:        0,
		SHA256:        sumHex,
		EXIFStripped:  false,
		UploaderID:    uploaderID,
		CreatedAt:     now.UnixMilli(),
	}
	if err := p.insertMediaAsset(ctx, asset); err != nil {
		return nil, err
	}
	return &asset, nil
}

func (p *Pipeline) insertMediaAsset(ctx context.Context, asset domain.MediaAsset) error {
	_, err := p.store.ExecContext(ctx,
		`INSERT INTO media_assets (id, storage_driver, object_key, public_url, original_name, alt_text, mime_type, size_bytes, width, height, sha256, exif_stripped, uploader_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		asset.ID, asset.StorageDriver, asset.ObjectKey, asset.PublicURL, asset.OriginalName, asset.AltText,
		asset.MIMEType, asset.SizeBytes, asset.Width, asset.Height, asset.SHA256,
		asset.EXIFStripped, asset.UploaderID, asset.CreatedAt,
	)
	return err
}

func (p *Pipeline) resolvePluginStorage(driver string) plugin.StorageProvider {
	if p.plugins == nil || driver == "" {
		return nil
	}
	prov := p.plugins.Providers()
	if sp, ok := prov.StorageByID(driver); ok && sp != nil {
		return sp
	}
	if sp := prov.ActiveStorage(); sp != nil && strings.EqualFold(sp.Name(), driver) {
		return sp
	}
	return nil
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
		if sp := p.resolvePluginStorage(asset.StorageDriver); sp != nil {
			_ = sp.Delete(ctx, asset.ObjectKey)
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
		if sp := p.resolvePluginStorage(driver); sp != nil {
			return sp.HealthCheck(ctx)
		}
		return fmt.Errorf("unknown storage driver %q", driver)
	}
}

// inspectGIFStreamLimits parses GIF87a/GIF89a headers, extensions, and image descriptors in O(len(raw))
// with zero image allocations to reject decompression bombs before gif.DecodeAll allocates frame buffers.
func inspectGIFStreamLimits(raw []byte) error {
	if len(raw) < 13 {
		return errors.New("corrupt GIF stream: truncated header")
	}
	hdr := string(raw[:6])
	if hdr != "GIF87a" && hdr != "GIF89a" {
		return errors.New("corrupt GIF stream: invalid signature")
	}
	canvasW := int(raw[6]) | int(raw[7])<<8
	canvasH := int(raw[8]) | int(raw[9])<<8
	if canvasW <= 0 || canvasH <= 0 || canvasW > maxImageDimension || canvasH > maxImageDimension ||
		int64(canvasW)*int64(canvasH) > maxImagePixels {
		return fmt.Errorf("GIF canvas dimensions %dx%d exceed decompression safety limit", canvasW, canvasH)
	}
	packed := raw[10]
	pos := 13
	if packed&0x80 != 0 {
		gctBytes := 3 * (1 << ((packed & 0x07) + 1))
		if pos+gctBytes > len(raw) {
			return errors.New("corrupt GIF stream: truncated global color table")
		}
		pos += gctBytes
	}

	var (
		frames         int
		totalSubPixels int64
	)
	canvasPixels := int64(canvasW) * int64(canvasH)

	for pos < len(raw) {
		block := raw[pos]
		pos++
		if block == 0x3B { // Trailer
			break
		}
		switch block {
		case 0x21: // Extension block (0x21 + label + sub-blocks terminated by 0x00)
			if pos >= len(raw) {
				return errors.New("corrupt GIF stream: truncated extension header")
			}
			pos++ // skip extension label
			for {
				if pos >= len(raw) {
					return errors.New("corrupt GIF stream: truncated extension sub-block")
				}
				subLen := int(raw[pos])
				pos++
				if subLen == 0 {
					break
				}
				if pos+subLen > len(raw) {
					return errors.New("corrupt GIF stream: truncated extension payload")
				}
				pos += subLen
			}
		case 0x2C: // Image Descriptor (9 bytes + optional LCT + LZW min code size + data sub-blocks)
			if pos+9 > len(raw) {
				return errors.New("corrupt GIF stream: truncated image descriptor")
			}
			subW := int(raw[pos+4]) | int(raw[pos+5])<<8
			subH := int(raw[pos+6]) | int(raw[pos+7])<<8
			imgPacked := raw[pos+8]
			pos += 9

			if subW <= 0 || subH <= 0 || subW > maxImageDimension || subH > maxImageDimension {
				return fmt.Errorf("GIF frame dimensions %dx%d exceed safety limit", subW, subH)
			}
			frames++
			if frames > maxGIFFrames {
				return fmt.Errorf("GIF frame count exceeds %d frame limit", maxGIFFrames)
			}
			totalSubPixels += int64(subW) * int64(subH)
			if totalSubPixels > maxGIFTotalPixels {
				return fmt.Errorf("GIF cumulative sub-image pixel budget (%d) exceeds %d limit", totalSubPixels, maxGIFTotalPixels)
			}
			if int64(frames)*canvasPixels > maxGIFTotalPixels*2 {
				return fmt.Errorf("GIF canvas frame budget (%d pixels across %d frames) exceeds %d limit", int64(frames)*canvasPixels, frames, maxGIFTotalPixels*2)
			}

			if imgPacked&0x80 != 0 {
				lctBytes := 3 * (1 << ((imgPacked & 0x07) + 1))
				if pos+lctBytes > len(raw) {
					return errors.New("corrupt GIF stream: truncated local color table")
				}
				pos += lctBytes
			}
			if pos >= len(raw) {
				return errors.New("corrupt GIF stream: missing LZW minimum code size")
			}
			pos++ // skip LZW minimum code size
			for {
				if pos >= len(raw) {
					return errors.New("corrupt GIF stream: truncated image data sub-block")
				}
				subLen := int(raw[pos])
				pos++
				if subLen == 0 {
					break
				}
				if pos+subLen > len(raw) {
					return errors.New("corrupt GIF stream: truncated image data payload")
				}
				pos += subLen
			}
		default:
			return fmt.Errorf("corrupt GIF stream: unexpected block introducer 0x%02x", block)
		}
	}
	if frames == 0 {
		return errors.New("corrupt GIF stream: zero frames")
	}
	return nil
}

func sanitizeLogToken(s string) string {
	cleaned := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '/' || r == '.' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, s)
	if len(cleaned) > 128 {
		return cleaned[:128]
	}
	return cleaned
}

func classifyPluginStorageErr(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "plugin_put_failed"
	}
}

func (p *Pipeline) storeRawObject(ctx context.Context, objectKey string, data []byte, contentType string) (string, string, error) {
	cfg := p.storageSettings()
	driver := strings.ToLower(strings.TrimSpace(cfg.ActiveDriver))

	// Check if an external plugin StorageProvider is explicitly selected
	if driver != "" && driver != "local" && driver != "s3" {
		if sp := p.resolvePluginStorage(driver); sp != nil {
			u, err := sp.Put(ctx, objectKey, bytes.NewReader(data), int64(len(data)), contentType)
			if err == nil {
				cleanURL, ok := repo.SanitizeAllowedURL(u, true)
				if !ok || strings.HasPrefix(strings.ToLower(cleanURL), "mailto:") || strings.HasPrefix(strings.ToLower(cleanURL), "tel:") || strings.HasPrefix(cleanURL, "#") {
					_ = sp.Delete(ctx, objectKey)
					return "", "", fmt.Errorf("storage plugin %q returned invalid or unsafe public URL", driver)
				}
				return driver, cleanURL, nil
			}
			slog.Warn("storage plugin Put failed; falling back to local disk storage", "driver", sanitizeLogToken(driver), "object_key", sanitizeLogToken(objectKey), "reason", classifyPluginStorageErr(err))
		} else {
			slog.Warn("storage plugin unavailable or detached; falling back to local disk storage", "driver", sanitizeLogToken(driver), "object_key", sanitizeLogToken(objectKey))
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

func (p *Pipeline) spoolToTempFile(r io.Reader, maxBytes int64) (*os.File, string, int64, string, error) {
	tempDir := ""
	if p.uploadDir != "" && os.MkdirAll(p.uploadDir, 0o750) == nil {
		tempDir = p.uploadDir
	}
	tmp, err := os.CreateTemp(tempDir, ".volo-upload-*")
	if err != nil && tempDir != "" {
		tmp, err = os.CreateTemp("", "volo-upload-*")
	}
	if err != nil {
		return nil, "", 0, "", err
	}
	tmpPath := tmp.Name()
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(tmp, hasher), io.LimitReader(r, maxBytes+1))
	if copyErr != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return nil, "", 0, "", copyErr
	}
	if written > maxBytes {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return nil, "", 0, "", fmt.Errorf("upload exceeds maximum size (%d bytes)", maxBytes)
	}
	if _, sErr := tmp.Seek(0, io.SeekStart); sErr != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return nil, "", 0, "", sErr
	}
	return tmp, tmpPath, written, hex.EncodeToString(hasher.Sum(nil)), nil
}

func (p *Pipeline) storeStreamObject(ctx context.Context, objectKey string, tmpFile **os.File, tmpPath *string, size int64, contentType string) (string, string, error) {
	cfg := p.storageSettings()
	driver := strings.ToLower(strings.TrimSpace(cfg.ActiveDriver))

	if driver != "" && driver != "local" && driver != "s3" {
		if sp := p.resolvePluginStorage(driver); sp != nil {
			if _, err := (*tmpFile).Seek(0, io.SeekStart); err != nil {
				return "", "", err
			}
			u, err := sp.Put(ctx, objectKey, *tmpFile, size, contentType)
			if err == nil {
				cleanURL, ok := repo.SanitizeAllowedURL(u, true)
				if !ok || strings.HasPrefix(strings.ToLower(cleanURL), "mailto:") || strings.HasPrefix(strings.ToLower(cleanURL), "tel:") || strings.HasPrefix(cleanURL, "#") {
					_ = sp.Delete(ctx, objectKey)
					return "", "", fmt.Errorf("storage plugin %q returned invalid or unsafe public URL", driver)
				}
				return driver, cleanURL, nil
			}
			slog.Warn("storage plugin Put failed; falling back to local disk storage", "driver", sanitizeLogToken(driver), "object_key", sanitizeLogToken(objectKey), "reason", classifyPluginStorageErr(err))
		} else {
			slog.Warn("storage plugin unavailable or detached; falling back to local disk storage", "driver", sanitizeLogToken(driver), "object_key", sanitizeLogToken(objectKey))
		}
	}

	if driver == "s3" {
		cli, err := p.buildS3Client(ctx, &cfg, "")
		if err != nil {
			return "", "", err
		}
		if _, err := (*tmpFile).Seek(0, io.SeekStart); err != nil {
			return "", "", err
		}
		pubURL, err := cli.PutStream(ctx, objectKey, *tmpFile, size, contentType)
		return "s3", pubURL, err
	}

	// Default: Local UploadDir storage (atomic rename when on same filesystem, streaming copy fallback)
	cleanRel := filepath.Clean(filepath.FromSlash(strings.TrimLeft(objectKey, "/")))
	if cleanRel == "." || cleanRel == ".." || strings.HasPrefix(cleanRel, ".."+string(os.PathSeparator)) {
		return "", "", errors.New("invalid local object key")
	}
	dest := filepath.Join(p.uploadDir, cleanRel)
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return "", "", err
	}

	srcPath := *tmpPath
	if *tmpFile != nil {
		_ = (*tmpFile).Close()
		*tmpFile = nil
	}
	_ = os.Chmod(srcPath, 0o640)
	if err := os.Rename(srcPath, dest); err == nil {
		*tmpPath = ""
		return "local", "/uploads/" + strings.TrimLeft(filepath.ToSlash(cleanRel), "/"), nil
	}

	src, err := os.Open(srcPath)
	if err != nil {
		return "", "", err
	}
	defer src.Close()

	dst, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return "", "", err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		_ = os.Remove(dest)
		return "", "", err
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(dest)
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
	mediaKindVideo
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
	case ".mp4":
		if len(raw) < 12 || string(raw[4:8]) != "ftyp" {
			return 0, "", errors.New("file magic bytes do not match MP4 ftyp signature")
		}
		return mediaKindVideo, "video/mp4", nil
	case ".webm":
		if !bytes.HasPrefix(raw, []byte{0x1A, 0x45, 0xDF, 0xA3}) {
			return 0, "", errors.New("file magic bytes do not match WebM EBML signature")
		}
		return mediaKindVideo, "video/webm", nil
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

package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"

	"github.com/HugoSmits86/nativewebp"
)

const exifOrientationTag uint16 = 0x0112

// ReadEXIFOrientation inspects raw JPEG, PNG (`eXIf`), or WebP (`EXIF`) bytes for an EXIF IFD0 Orientation tag (1..8).
// Returns 1 (upright identity) if absent, malformed, or out of range.
func ReadEXIFOrientation(raw []byte) int {
	tiffPayload := extractTIFFPayload(raw)
	if len(tiffPayload) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(tiffPayload[:4]) {
	case "II\x2a\x00":
		bo = binary.LittleEndian
	case "MM\x00\x2a":
		bo = binary.BigEndian
	default:
		return 1
	}
	ifdOffset := int(bo.Uint32(tiffPayload[4:8]))
	if ifdOffset < 8 || ifdOffset+2 > len(tiffPayload) {
		return 1
	}
	numEntries := int(bo.Uint16(tiffPayload[ifdOffset : ifdOffset+2]))
	pos := ifdOffset + 2
	for i := 0; i < numEntries && pos+12 <= len(tiffPayload); i++ {
		tag := bo.Uint16(tiffPayload[pos : pos+2])
		typ := bo.Uint16(tiffPayload[pos+2 : pos+4])
		cnt := bo.Uint32(tiffPayload[pos+4 : pos+8])
		if tag == exifOrientationTag && typ == 3 && cnt >= 1 {
			val := int(bo.Uint16(tiffPayload[pos+8 : pos+10]))
			if val >= 1 && val <= 8 {
				return val
			}
			return 1
		}
		pos += 12
	}
	return 1
}

func unwrapTIFFHeader(payload []byte) []byte {
	if len(payload) > 6 && bytes.Equal(payload[:6], []byte("Exif\x00\x00")) {
		payload = payload[6:]
	}
	if len(payload) >= 8 && (string(payload[:4]) == "II\x2a\x00" || string(payload[:4]) == "MM\x00\x2a") {
		return payload
	}
	return nil
}

func extractTIFFPayload(raw []byte) []byte {
	// 1. JPEG SOI (0xFF 0xD8) -> scan markers for APP1 (0xFF 0xE1) starting with "Exif\x00\x00"
	if len(raw) >= 4 && raw[0] == 0xFF && raw[1] == 0xD8 {
		i := 2
		for i+4 <= len(raw) {
			if raw[i] != 0xFF {
				break
			}
			for i < len(raw) && raw[i] == 0xFF {
				i++
			}
			if i >= len(raw) {
				break
			}
			marker := raw[i]
			i++
			if marker == 0xDA || marker == 0xD9 { // SOS or EOI
				break
			}
			if marker == 0x00 || (marker >= 0xD0 && marker <= 0xD7) {
				continue
			}
			if i+2 > len(raw) {
				break
			}
			segLen := int(binary.BigEndian.Uint16(raw[i : i+2]))
			if segLen < 2 || i+segLen > len(raw) {
				break
			}
			segData := raw[i+2 : i+segLen]
			if marker == 0xE1 {
				if tiff := unwrapTIFFHeader(segData); tiff != nil {
					return tiff
				}
			}
			i += segLen
		}
		return nil
	}

	// 2. PNG signature -> scan chunks for `eXIf` (raw TIFF or optional "Exif\x00\x00" prefix)
	if bytes.HasPrefix(raw, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) {
		i := 8
		for i+12 <= len(raw) {
			chunkLen := int(binary.BigEndian.Uint32(raw[i : i+4]))
			if chunkLen < 0 || i+12+chunkLen > len(raw) {
				break
			}
			chunkType := string(raw[i+4 : i+8])
			if chunkType == "eXIf" {
				if tiff := unwrapTIFFHeader(raw[i+8 : i+8+chunkLen]); tiff != nil {
					return tiff
				}
			}
			if chunkType == "IEND" {
				break
			}
			i += 12 + chunkLen
		}
		return nil
	}

	// 3. WebP RIFF container -> scan chunks for `EXIF` (optional "Exif\x00\x00" prefix or direct TIFF)
	if len(raw) >= 12 && string(raw[0:4]) == "RIFF" && string(raw[8:12]) == "WEBP" {
		i := 12
		for i+8 <= len(raw) {
			fourCC := string(raw[i : i+4])
			chunkLen := int(binary.LittleEndian.Uint32(raw[i+4 : i+8]))
			if chunkLen < 0 || i+8+chunkLen > len(raw) {
				break
			}
			if fourCC == "EXIF" {
				if tiff := unwrapTIFFHeader(raw[i+8 : i+8+chunkLen]); tiff != nil {
					return tiff
				}
			}
			padded := chunkLen
			if padded%2 == 1 {
				padded++
			}
			i += 8 + padded
		}
		return nil
	}

	return nil
}

// StripWebPMetadataLossless removes `EXIF`, `XMP `, and `ICCP` chunks from a WebP RIFF container
// without decoding or re-encoding VP8/VP8L pixel bitstreams, clearing corresponding VP8X flags
// and updating the RIFF container byte length.
func StripWebPMetadataLossless(raw []byte) ([]byte, error) {
	if len(raw) < 12 || string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WEBP" {
		return nil, errors.New("invalid WebP RIFF header")
	}
	out := make([]byte, 12, len(raw))
	copy(out, raw[:12])

	end := len(raw)
	if riffSize := int(binary.LittleEndian.Uint32(raw[4:8])); riffSize >= 4 && 8+riffSize <= len(raw) {
		end = 8 + riffSize
	}

	pos := 12
	sawImageChunk := false
	vp8xFlagsPos := -1

	for pos < end {
		if pos+8 > len(raw) {
			return nil, errors.New("corrupt WebP container: truncated chunk header")
		}
		fourCC := string(raw[pos : pos+4])
		cSize := int(binary.LittleEndian.Uint32(raw[pos+4 : pos+8]))
		if cSize < 0 || pos+8+cSize > len(raw) {
			return nil, errors.New("corrupt WebP container: invalid chunk size")
		}
		padded := cSize
		if padded%2 == 1 && pos+8+padded+1 <= end {
			padded++
		}

		switch fourCC {
		case "EXIF", "XMP ", "ICCP":
			// Drop container metadata chunk losslessly!
		default:
			if fourCC == "VP8 " || fourCC == "VP8L" || fourCC == "ANMF" {
				sawImageChunk = true
			}
			chunkStart := len(out)
			out = append(out, raw[pos:pos+8+cSize]...)
			if cSize%2 == 1 {
				out = append(out, 0x00)
			}
			if fourCC == "VP8X" && cSize >= 4 && vp8xFlagsPos < 0 {
				vp8xFlagsPos = chunkStart + 8
			}
		}
		pos += 8 + padded
	}

	if !sawImageChunk {
		return nil, errors.New("corrupt WebP container: missing image bitstream chunk")
	}
	if vp8xFlagsPos >= 0 && vp8xFlagsPos < len(out) {
		// Clear ICCP (0x20), EXIF (0x08), and XMP (0x04) feature flags in VP8X header.
		out[vp8xFlagsPos] &^= (0x20 | 0x08 | 0x04)
	}
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(out)-8))
	return out, nil
}

func stripJPEGMetadataLossless(raw []byte) ([]byte, bool, error) {
	if len(raw) < 4 || raw[0] != 0xFF || raw[1] != 0xD8 {
		return nil, false, errors.New("invalid JPEG SOI header")
	}
	out := make([]byte, 0, len(raw))
	out = append(out, 0xFF, 0xD8)
	pos := 2
	removed := false
	for pos < len(raw) {
		if raw[pos] != 0xFF {
			return nil, false, errors.New("corrupt JPEG marker stream")
		}
		for pos < len(raw) && raw[pos] == 0xFF {
			pos++
		}
		if pos >= len(raw) {
			break
		}
		marker := raw[pos]
		pos++
		if marker == 0x00 {
			return nil, false, errors.New("corrupt JPEG stuffed byte outside scan")
		}
		if marker == 0xD9 { // EOI
			out = append(out, 0xFF, 0xD9)
			return out, removed, nil
		}
		if marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			out = append(out, 0xFF, marker)
			continue
		}
		if pos+2 > len(raw) {
			return nil, false, errors.New("truncated JPEG segment length")
		}
		segLen := int(binary.BigEndian.Uint16(raw[pos : pos+2]))
		if segLen < 2 || pos+segLen > len(raw) {
			return nil, false, errors.New("invalid JPEG segment length")
		}
		if marker == 0xDA { // SOS: copy scan header + entropy-coded bitstream to EOF
			out = append(out, 0xFF, marker)
			out = append(out, raw[pos:]...)
			return out, removed, nil
		}
		if marker == 0xFE || (marker >= 0xE1 && marker <= 0xEF) {
			removed = true
			pos += segLen
			continue
		}
		out = append(out, 0xFF, marker)
		out = append(out, raw[pos:pos+segLen]...)
		pos += segLen
	}
	return out, removed, nil
}

func stripPNGMetadataLossless(raw []byte) ([]byte, bool, error) {
	sig := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	if !bytes.HasPrefix(raw, sig) {
		return nil, false, errors.New("invalid PNG signature")
	}
	out := make([]byte, 0, len(raw))
	out = append(out, sig...)
	pos := 8
	removed := false
	sawIEND := false
	for pos+12 <= len(raw) {
		cLen := int(binary.BigEndian.Uint32(raw[pos : pos+4]))
		if cLen < 0 || pos+12+cLen > len(raw) {
			return nil, false, errors.New("corrupt PNG chunk length")
		}
		cType := string(raw[pos+4 : pos+8])
		totalChunk := 12 + cLen
		switch cType {
		case "eXIf", "tEXt", "zTXt", "iTXt":
			removed = true
		default:
			out = append(out, raw[pos:pos+totalChunk]...)
		}
		pos += totalChunk
		if cType == "IEND" {
			sawIEND = true
			break
		}
	}
	if !sawIEND {
		return nil, false, errors.New("corrupt PNG stream: missing IEND")
	}
	return out, removed, nil
}

// NormalizeLocalImageBytes validates magic bytes & decompression-bomb limits for a local image file,
// rotates pixels and updates intrinsic dimensions when `ReadEXIFOrientation(raw) > 1`, or strips
// container metadata losslessly when `orient == 1` (suitable for `reconcileLocalUploads`).
func NormalizeLocalImageBytes(raw []byte, extLower string) (clean []byte, mimeType string, width, height int, changed bool, err error) {
	ext := strings.ToLower(strings.TrimSpace(extLower))
	if ext != "" && !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	if ext == "" {
		switch {
		case len(raw) >= 3 && raw[0] == 0xFF && raw[1] == 0xD8 && raw[2] == 0xFF:
			ext = ".jpg"
		case bytes.HasPrefix(raw, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}):
			ext = ".png"
		case len(raw) >= 12 && string(raw[0:4]) == "RIFF" && string(raw[8:12]) == "WEBP":
			ext = ".webp"
		case bytes.HasPrefix(raw, []byte("GIF87a")) || bytes.HasPrefix(raw, []byte("GIF89a")):
			ext = ".gif"
		}
	}

	kind, detectedMIME, err := classifyAndVerifyMagic(ext, raw)
	if err != nil {
		return nil, "", 0, 0, false, err
	}
	if kind != mediaKindImage {
		return nil, "", 0, 0, false, fmt.Errorf("extension %q is not a supported raster image", ext)
	}

	cfg, formatName, cfgErr := image.DecodeConfig(bytes.NewReader(raw))
	if cfgErr != nil {
		return nil, "", 0, 0, false, fmt.Errorf("invalid image header: %w", cfgErr)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxImageDimension || cfg.Height > maxImageDimension ||
		int64(cfg.Width)*int64(cfg.Height) > maxImagePixels {
		return nil, "", 0, 0, false, fmt.Errorf("image dimensions %dx%d exceed 24MP decompression safety limit", cfg.Width, cfg.Height)
	}
	if formatName == "gif" {
		if err := inspectGIFStreamLimits(raw); err != nil {
			return nil, "", 0, 0, false, err
		}
	}

	orient := ReadEXIFOrientation(raw)
	if orient > 1 && formatName != "gif" {
		img, _, decErr := image.Decode(bytes.NewReader(raw))
		if decErr != nil {
			return nil, "", 0, 0, false, fmt.Errorf("image decode failed: %w", decErr)
		}
		upright := ApplyEXIFOrientation(img, orient)
		b := upright.Bounds()
		width, height = b.Dx(), b.Dy()
		var buf bytes.Buffer
		switch formatName {
		case "png":
			enc := png.Encoder{CompressionLevel: png.DefaultCompression}
			if err := enc.Encode(&buf, upright); err != nil {
				return nil, "", 0, 0, false, err
			}
			detectedMIME = "image/png"
		case "webp":
			if err := nativewebp.Encode(&buf, upright, nil); err != nil {
				return nil, "", 0, 0, false, err
			}
			detectedMIME = "image/webp"
		default:
			if err := jpeg.Encode(&buf, upright, &jpeg.Options{Quality: 90}); err != nil {
				return nil, "", 0, 0, false, err
			}
			detectedMIME = "image/jpeg"
		}
		outBytes := buf.Bytes()
		if ContainsEXIFOrGPS(outBytes) {
			return nil, "", 0, 0, false, errors.New("residual metadata detected after orientation normalization")
		}
		return outBytes, detectedMIME, width, height, true, nil
	}

	width, height = cfg.Width, cfg.Height
	switch detectedMIME {
	case "image/webp":
		stripped, sErr := StripWebPMetadataLossless(raw)
		if sErr != nil {
			return nil, "", 0, 0, false, sErr
		}
		if ContainsEXIFOrGPS(stripped) {
			return nil, "", 0, 0, false, errors.New("residual WebP metadata detected after strip")
		}
		if bytes.Equal(stripped, raw) {
			return raw, detectedMIME, width, height, false, nil
		}
		return stripped, detectedMIME, width, height, true, nil
	case "image/jpeg":
		stripped, mod, sErr := stripJPEGMetadataLossless(raw)
		if sErr != nil {
			return nil, "", 0, 0, false, sErr
		}
		if ContainsEXIFOrGPS(stripped) {
			return nil, "", 0, 0, false, errors.New("residual JPEG metadata detected after strip")
		}
		if !mod {
			return raw, detectedMIME, width, height, false, nil
		}
		return stripped, detectedMIME, width, height, true, nil
	case "image/png":
		stripped, mod, sErr := stripPNGMetadataLossless(raw)
		if sErr != nil {
			return nil, "", 0, 0, false, sErr
		}
		if ContainsEXIFOrGPS(stripped) {
			return nil, "", 0, 0, false, errors.New("residual PNG metadata detected after strip")
		}
		if !mod {
			return raw, detectedMIME, width, height, false, nil
		}
		return stripped, detectedMIME, width, height, true, nil
	case "image/gif":
		g, gErr := gif.DecodeAll(bytes.NewReader(raw))
		if gErr != nil {
			return raw, detectedMIME, width, height, false, nil
		}
		var cleanGIF bytes.Buffer
		if encErr := gif.EncodeAll(&cleanGIF, g); encErr != nil {
			return raw, detectedMIME, width, height, false, nil
		}
		if bytes.Equal(cleanGIF.Bytes(), raw) {
			return raw, detectedMIME, width, height, false, nil
		}
		return cleanGIF.Bytes(), detectedMIME, width, height, true, nil
	default:
		return raw, detectedMIME, width, height, false, nil
	}
}

// ContainsEXIFOrGPS performs a binary segment/chunk inspection on JPEG, PNG, and WebP data
// to verify that zero EXIF, XMP, or GPS metadata blocks remain in processed output.
func ContainsEXIFOrGPS(raw []byte) bool {
	if len(raw) < 8 {
		return false
	}
	// JPEG: scan for APP1 (0xFFE1 Exif/XMP) or APP13 (0xFFED IPTC)
	if raw[0] == 0xFF && raw[1] == 0xD8 {
		i := 2
		for i+4 <= len(raw) {
			if raw[i] != 0xFF {
				break
			}
			for i < len(raw) && raw[i] == 0xFF {
				i++
			}
			if i >= len(raw) {
				break
			}
			marker := raw[i]
			i++
			if marker == 0xDA || marker == 0xD9 {
				break
			}
			if marker == 0x00 || (marker >= 0xD0 && marker <= 0xD7) {
				continue
			}
			if i+2 > len(raw) {
				break
			}
			segLen := int(binary.BigEndian.Uint16(raw[i : i+2]))
			if segLen < 2 || i+segLen > len(raw) {
				break
			}
			if marker == 0xE1 || marker == 0xED {
				return true
			}
			i += segLen
		}
		return false
	}

	// PNG: scan chunks for eXIf
	if bytes.HasPrefix(raw, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) {
		i := 8
		for i+12 <= len(raw) {
			chunkLen := int(binary.BigEndian.Uint32(raw[i : i+4]))
			if chunkLen < 0 || i+12+chunkLen > len(raw) {
				break
			}
			chunkType := string(raw[i+4 : i+8])
			if chunkType == "eXIf" {
				return true
			}
			if chunkType == "IEND" {
				break
			}
			i += 12 + chunkLen
		}
		return false
	}

	// WebP RIFF container: scan chunks for EXIF or XMP
	if len(raw) >= 12 && string(raw[0:4]) == "RIFF" && string(raw[8:12]) == "WEBP" {
		i := 12
		for i+8 <= len(raw) {
			fourCC := string(raw[i : i+4])
			chunkLen := int(binary.LittleEndian.Uint32(raw[i+4 : i+8]))
			if chunkLen < 0 {
				break
			}
			if fourCC == "EXIF" || fourCC == "XMP " {
				return true
			}
			padded := chunkLen
			if padded%2 == 1 {
				padded++
			}
			i += 8 + padded
		}
		return false
	}
	return false
}

// ApplyEXIFOrientation transforms pixel coordinates according to EXIF Orientation (1..8)
// and returns a normalized upright image with (0,0)-anchored bounds.
func ApplyEXIFOrientation(src image.Image, orientation int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 || orientation <= 1 || orientation > 8 {
		if b.Min.X == 0 && b.Min.Y == 0 {
			return src
		}
		dst := image.NewNRGBA(image.Rect(0, 0, w, h))
		draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
		return dst
	}

	srcNRGBA := toNRGBA(src)
	var dst *image.NRGBA
	if orientation >= 5 && orientation <= 8 {
		dst = image.NewNRGBA(image.Rect(0, 0, h, w))
	} else {
		dst = image.NewNRGBA(image.Rect(0, 0, w, h))
	}

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := srcNRGBA.NRGBAAt(x, y)
			switch orientation {
			case 2: // Flip horizontal
				dst.SetNRGBA(w-1-x, y, c)
			case 3: // Rotate 180
				dst.SetNRGBA(w-1-x, h-1-y, c)
			case 4: // Flip vertical
				dst.SetNRGBA(x, h-1-y, c)
			case 5: // Transpose (flip horizontal + rotate 270 CW)
				dst.SetNRGBA(y, x, c)
			case 6: // Rotate 90 CW
				dst.SetNRGBA(h-1-y, x, c)
			case 7: // Transverse (flip horizontal + rotate 90 CW)
				dst.SetNRGBA(h-1-y, w-1-x, c)
			case 8: // Rotate 270 CW
				dst.SetNRGBA(y, w-1-x, c)
			}
		}
	}
	return dst
}

func toNRGBA(src image.Image) *image.NRGBA {
	if n, ok := src.(*image.NRGBA); ok && n.Bounds().Min.X == 0 && n.Bounds().Min.Y == 0 {
		return n
	}
	b := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
	return dst
}

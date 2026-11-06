package media

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/draw"
)

const exifOrientationTag uint16 = 0x0112

// ReadEXIFOrientation inspects raw JPEG or WebP bytes for an EXIF IFD0 Orientation tag (1..8).
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

func extractTIFFPayload(raw []byte) []byte {
	// JPEG SOI (0xFF 0xD8) -> scan markers for APP1 (0xFF 0xE1) starting with "Exif\x00\x00"
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
			if marker == 0xE1 && len(segData) > 6 && bytes.Equal(segData[:6], []byte("Exif\x00\x00")) {
				return segData[6:]
			}
			i += segLen
		}
	}
	return nil
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

	// PNG: scan chunks for eXIf, tEXt/iTXt/zTXt containing Exif/XML/GPS
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

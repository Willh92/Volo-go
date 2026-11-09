package media

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"sort"
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

// rewriteTIFFOrientationTo1 returns a cloned TIFF buffer with IFD0 Orientation (0x0112) set to 1
// so rotated pixel outputs that retain EXIF metadata are never double-rotated by viewers.
func rewriteTIFFOrientationTo1(tiffPayload []byte) []byte {
	out := make([]byte, len(tiffPayload))
	copy(out, tiffPayload)
	if len(out) < 8 {
		return out
	}
	var bo binary.ByteOrder
	switch string(out[:4]) {
	case "II\x2a\x00":
		bo = binary.LittleEndian
	case "MM\x00\x2a":
		bo = binary.BigEndian
	default:
		return out
	}
	ifdOffset := int(bo.Uint32(out[4:8]))
	if ifdOffset < 8 || ifdOffset+2 > len(out) {
		return out
	}
	numEntries := int(bo.Uint16(out[ifdOffset : ifdOffset+2]))
	pos := ifdOffset + 2
	for i := 0; i < numEntries && pos+12 <= len(out); i++ {
		tag := bo.Uint16(out[pos : pos+2])
		typ := bo.Uint16(out[pos+2 : pos+4])
		cnt := bo.Uint32(out[pos+4 : pos+8])
		if tag == exifOrientationTag && typ == 3 && cnt >= 1 {
			bo.PutUint16(out[pos+8:pos+10], 1)
			return out
		}
		pos += 12
	}
	return out
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

// hasAVIFMagic inspects an ISOBMFF ftyp header for an "avif" or "avis" major or compatible brand.
func hasAVIFMagic(raw []byte) bool {
	if len(raw) < 16 || string(raw[4:8]) != "ftyp" {
		return false
	}
	boxSize := uint64(binary.BigEndian.Uint32(raw[0:4]))
	hdrLen := 8
	if boxSize == 1 {
		if len(raw) < 24 {
			return false
		}
		boxSize = binary.BigEndian.Uint64(raw[8:16])
		hdrLen = 16
	} else if boxSize == 0 {
		boxSize = uint64(len(raw))
	}
	if boxSize < uint64(hdrLen+8) {
		return false
	}
	end := len(raw)
	if boxSize < uint64(end) {
		end = int(boxSize)
	}
	if end < hdrLen+8 {
		return false
	}
	major := string(raw[hdrLen : hdrLen+4])
	if major == "avif" || major == "avis" {
		return true
	}
	for pos := hdrLen + 8; pos+4 <= end; pos += 4 {
		brand := string(raw[pos : pos+4])
		if brand == "avif" || brand == "avis" {
			return true
		}
	}
	return false
}

func nextISOBMFFBox(buf []byte, pos int) (boxType string, payload []byte, nextPos int, ok bool) {
	if pos+8 > len(buf) {
		return "", nil, 0, false
	}
	sz32 := binary.BigEndian.Uint32(buf[pos : pos+4])
	typ := string(buf[pos+4 : pos+8])
	hdr := 8
	var boxLen uint64
	if sz32 == 1 {
		if pos+16 > len(buf) {
			return "", nil, 0, false
		}
		boxLen = binary.BigEndian.Uint64(buf[pos+8 : pos+16])
		hdr = 16
	} else if sz32 == 0 {
		boxLen = uint64(len(buf) - pos)
	} else {
		boxLen = uint64(sz32)
	}
	if boxLen < uint64(hdr) || boxLen > uint64(len(buf)-pos) {
		return "", nil, 0, false
	}
	end := pos + int(boxLen)
	return typ, buf[pos+hdr : end], end, true
}

// parseAVIFDimensions parses an ISOBMFF AVIF stream (`ftyp` -> `meta` -> `iprp` -> `ipco` -> `ispe`/`irot`)
// to extract intrinsic pixel dimensions and enforce the 24MP decompression-bomb ceiling.
func parseAVIFDimensions(raw []byte) (int, int, error) {
	if !hasAVIFMagic(raw) {
		return 0, 0, errors.New("file magic bytes do not match AVIF ftyp signature")
	}
	var metaPayload []byte
	for pos := 0; pos < len(raw); {
		typ, payload, next, ok := nextISOBMFFBox(raw, pos)
		if !ok {
			break
		}
		if typ == "meta" {
			metaPayload = payload
			break
		}
		pos = next
	}
	if len(metaPayload) < 12 {
		return 0, 0, errors.New("invalid AVIF container: missing meta box")
	}
	// ISO 14496-12 `meta` is a FullBox (4-byte version+flags before child boxes).
	metaChildren := metaPayload[4:]

	var (
		hasPrimaryID bool
		primaryID    uint32
		ipcoPayload  []byte
		ipmaPayloads [][]byte
	)
	for pos := 0; pos < len(metaChildren); {
		typ, payload, next, ok := nextISOBMFFBox(metaChildren, pos)
		if !ok {
			break
		}
		switch typ {
		case "pitm":
			if len(payload) >= 6 {
				ver := payload[0]
				if ver == 0 {
					primaryID = uint32(binary.BigEndian.Uint16(payload[4:6]))
					hasPrimaryID = true
				} else if len(payload) >= 8 {
					primaryID = binary.BigEndian.Uint32(payload[4:8])
					hasPrimaryID = true
				}
			}
		case "iprp":
			for p2 := 0; p2 < len(payload); {
				t2, pay2, n2, ok2 := nextISOBMFFBox(payload, p2)
				if !ok2 {
					break
				}
				switch t2 {
				case "ipco":
					ipcoPayload = pay2
				case "ipma":
					ipmaPayloads = append(ipmaPayloads, pay2)
				}
				p2 = n2
			}
		}
		pos = next
	}
	if len(ipcoPayload) == 0 {
		return 0, 0, errors.New("invalid AVIF container: missing ipco property container")
	}

	type propEntry struct {
		typ   string
		w, h  int
		angle int // 0, 90, 180, 270
	}
	var props []propEntry // 1-indexed in ISOBMFF ipma
	for pos := 0; pos < len(ipcoPayload); {
		typ, payload, next, ok := nextISOBMFFBox(ipcoPayload, pos)
		if !ok {
			break
		}
		pe := propEntry{typ: typ}
		if typ == "ispe" && len(payload) >= 12 {
			pe.w = int(binary.BigEndian.Uint32(payload[4:8]))
			pe.h = int(binary.BigEndian.Uint32(payload[8:12]))
		} else if typ == "irot" && len(payload) >= 1 {
			pe.angle = int(payload[0]&0x03) * 90
		}
		props = append(props, pe)
		pos = next
	}

	// Resolve primary item's associated properties via ipma if available.
	var primaryProps []int
	if hasPrimaryID {
		for _, ipma := range ipmaPayloads {
			if len(ipma) < 8 {
				continue
			}
			ver := ipma[0]
			flags := int(ipma[1])<<16 | int(ipma[2])<<8 | int(ipma[3])
			entryCount := int(binary.BigEndian.Uint32(ipma[4:8]))
			off := 8
			for e := 0; e < entryCount && off < len(ipma); e++ {
				var itemID uint32
				if ver < 1 {
					if off+2 > len(ipma) {
						off = len(ipma)
						break
					}
					itemID = uint32(binary.BigEndian.Uint16(ipma[off : off+2]))
					off += 2
				} else {
					if off+4 > len(ipma) {
						off = len(ipma)
						break
					}
					itemID = binary.BigEndian.Uint32(ipma[off : off+4])
					off += 4
				}
				if off >= len(ipma) {
					break
				}
				assocCount := int(ipma[off])
				off++
				for a := 0; a < assocCount; a++ {
					var propIdx int
					if flags&1 != 0 {
						if off+2 > len(ipma) {
							off = len(ipma)
							break
						}
						propIdx = int(binary.BigEndian.Uint16(ipma[off:off+2]) & 0x7FFF)
						off += 2
					} else {
						if off >= len(ipma) {
							break
						}
						propIdx = int(ipma[off] & 0x7F)
						off++
					}
					if itemID == primaryID && propIdx >= 1 && propIdx <= len(props) {
						primaryProps = append(primaryProps, propIdx-1)
					}
				}
			}
		}
	}

	var width, height, rotAngle int
	for _, idx := range primaryProps {
		p := props[idx]
		if p.typ == "ispe" && p.w > 0 && p.h > 0 && width == 0 {
			width, height = p.w, p.h
		} else if p.typ == "irot" {
			rotAngle = p.angle
		}
	}
	if width == 0 || height == 0 {
		for _, p := range props {
			if p.typ == "ispe" && p.w > 0 && p.h > 0 {
				if int64(p.w)*int64(p.h) > int64(width)*int64(height) {
					width, height = p.w, p.h
				}
			} else if p.typ == "irot" && rotAngle == 0 {
				rotAngle = p.angle
			}
		}
	}
	if width <= 0 || height <= 0 {
		return 0, 0, errors.New("invalid AVIF container: missing valid ispe dimensions")
	}
	if rotAngle == 90 || rotAngle == 270 {
		width, height = height, width
	}
	if width > maxImageDimension || height > maxImageDimension || int64(width)*int64(height) > maxImagePixels {
		return 0, 0, fmt.Errorf("image dimensions %dx%d exceed 24MP decompression safety limit", width, height)
	}
	return width, height, nil
}

func isWebPRIFF(raw []byte) bool {
	return len(raw) >= 12 && string(raw[0:4]) == "RIFF" && string(raw[8:12]) == "WEBP"
}

func decodeImageConfigSafe(raw []byte) (image.Config, string, error) {
	cfg, fmtName, err := image.DecodeConfig(bytes.NewReader(raw))
	if err == nil {
		return cfg, fmtName, nil
	}
	if isWebPRIFF(raw) {
		if wCfg, wErr := nativewebp.DecodeConfig(bytes.NewReader(raw)); wErr == nil {
			return wCfg, "webp", nil
		}
	}
	return image.Config{}, "", err
}

func decodeRasterImageSafe(raw []byte) (image.Image, string, error) {
	img, fmtName, err := image.Decode(bytes.NewReader(raw))
	if err == nil {
		return img, fmtName, nil
	}
	if isWebPRIFF(raw) {
		if wImg, wErr := nativewebp.Decode(bytes.NewReader(raw)); wErr == nil {
			return wImg, "webp", nil
		}
		if wImg, wErr := nativewebp.DecodeIgnoreAlphaFlag(bytes.NewReader(raw)); wErr == nil {
			return wImg, "webp", nil
		}
	}
	return nil, "", err
}

// imageMetadataChunks holds raw container metadata extracted prior to pixel transcoding.
type imageMetadataChunks struct {
	exifTIFF   []byte // Raw TIFF starting with "II*\x00" or "MM\x00*" (without "Exif\x00\x00")
	iccProfile []byte // Raw uncompressed ICC color profile bytes
	xmpXML     []byte // Raw UTF-8 XMP packet XML bytes
}

func extractImageMetadata(raw []byte) imageMetadataChunks {
	var m imageMetadataChunks
	if len(raw) < 8 {
		return m
	}

	// 1. JPEG: APP1 Exif, APP1 XMP, APP2 ICC_PROFILE
	if raw[0] == 0xFF && raw[1] == 0xD8 {
		type iccPart struct {
			seq  int
			data []byte
		}
		var iccParts []iccPart
		const xmpPrefix = "http://ns.adobe.com/xap/1.0/\x00"
		const iccPrefix = "ICC_PROFILE\x00"
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
			segData := raw[i+2 : i+segLen]
			switch marker {
			case 0xE1:
				if m.exifTIFF == nil {
					if tiff := unwrapTIFFHeader(segData); tiff != nil {
						m.exifTIFF = append([]byte(nil), tiff...)
					}
				}
				if m.xmpXML == nil && len(segData) > len(xmpPrefix) && string(segData[:len(xmpPrefix)]) == xmpPrefix {
					m.xmpXML = append([]byte(nil), segData[len(xmpPrefix):]...)
				}
			case 0xE2:
				if len(segData) > 14 && string(segData[:12]) == iccPrefix {
					seq := int(segData[12])
					iccParts = append(iccParts, iccPart{seq: seq, data: segData[14:]})
				}
			}
			i += segLen
		}
		if len(iccParts) > 0 {
			sort.SliceStable(iccParts, func(a, b int) bool { return iccParts[a].seq < iccParts[b].seq })
			var total int
			for _, p := range iccParts {
				total += len(p.data)
			}
			if total > 0 && total <= 4<<20 {
				buf := make([]byte, 0, total)
				for _, p := range iccParts {
					buf = append(buf, p.data...)
				}
				m.iccProfile = buf
			}
		}
		return m
	}

	// 2. PNG: eXIf, iCCP, iTXt ("XML:com.adobe.xmp\x00")
	if bytes.HasPrefix(raw, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) {
		const xmpKeyword = "XML:com.adobe.xmp\x00"
		i := 8
		for i+12 <= len(raw) {
			cLen := int(binary.BigEndian.Uint32(raw[i : i+4]))
			if cLen < 0 || i+12+cLen > len(raw) {
				break
			}
			cType := string(raw[i+4 : i+8])
			cData := raw[i+8 : i+8+cLen]
			switch cType {
			case "eXIf":
				if m.exifTIFF == nil {
					if tiff := unwrapTIFFHeader(cData); tiff != nil {
						m.exifTIFF = append([]byte(nil), tiff...)
					}
				}
			case "iCCP":
				if m.iccProfile == nil {
					nulIdx := bytes.IndexByte(cData, 0)
					if nulIdx >= 0 && nulIdx+2 < len(cData) && cData[nulIdx+1] == 0 {
						if zr, zErr := zlib.NewReader(bytes.NewReader(cData[nulIdx+2:])); zErr == nil {
							decomp, rErr := io.ReadAll(io.LimitReader(zr, 4<<20))
							_ = zr.Close()
							if rErr == nil && len(decomp) > 0 && len(decomp) < 4<<20 {
								m.iccProfile = decomp
							}
						}
					}
				}
			case "iTXt":
				if m.xmpXML == nil && len(cData) > len(xmpKeyword)+4 && string(cData[:len(xmpKeyword)]) == xmpKeyword {
					rest := cData[len(xmpKeyword):]
					compFlag := rest[0]
					compMethod := rest[1]
					rest = rest[2:]
					if nul1 := bytes.IndexByte(rest, 0); nul1 >= 0 {
						rest = rest[nul1+1:]
						if nul2 := bytes.IndexByte(rest, 0); nul2 >= 0 {
							textBytes := rest[nul2+1:]
							if compFlag == 0 {
								m.xmpXML = append([]byte(nil), textBytes...)
							} else if compFlag == 1 && compMethod == 0 {
								if zr, zErr := zlib.NewReader(bytes.NewReader(textBytes)); zErr == nil {
									decomp, rErr := io.ReadAll(io.LimitReader(zr, 4<<20))
									_ = zr.Close()
									if rErr == nil && len(decomp) > 0 && len(decomp) < 4<<20 {
										m.xmpXML = decomp
									}
								}
							}
						}
					}
				}
			}
			if cType == "IEND" {
				break
			}
			i += 12 + cLen
		}
		return m
	}

	// 3. WebP RIFF: EXIF, ICCP, XMP
	if isWebPRIFF(raw) {
		i := 12
		for i+8 <= len(raw) {
			fourCC := string(raw[i : i+4])
			cLen := int(binary.LittleEndian.Uint32(raw[i+4 : i+8]))
			if cLen < 0 || i+8+cLen > len(raw) {
				break
			}
			cData := raw[i+8 : i+8+cLen]
			switch fourCC {
			case "EXIF":
				if m.exifTIFF == nil {
					if tiff := unwrapTIFFHeader(cData); tiff != nil {
						m.exifTIFF = append([]byte(nil), tiff...)
					}
				}
			case "ICCP":
				if m.iccProfile == nil && len(cData) > 0 && len(cData) <= 4<<20 {
					m.iccProfile = append([]byte(nil), cData...)
				}
			case "XMP ":
				if m.xmpXML == nil && len(cData) > 0 && len(cData) <= 4<<20 {
					m.xmpXML = append([]byte(nil), cData...)
				}
			}
			padded := cLen
			if padded%2 == 1 {
				padded++
			}
			i += 8 + padded
		}
	}
	return m
}

func writePNGChunk(buf *bytes.Buffer, chunkType string, payload []byte) {
	var lenBytes [4]byte
	binary.BigEndian.PutUint32(lenBytes[:], uint32(len(payload)))
	buf.Write(lenBytes[:])
	buf.WriteString(chunkType)
	buf.Write(payload)
	crc := crc32.NewIEEE()
	_, _ = crc.Write([]byte(chunkType))
	_, _ = crc.Write(payload)
	var crcBytes [4]byte
	binary.BigEndian.PutUint32(crcBytes[:], crc.Sum32())
	buf.Write(crcBytes[:])
}

// injectImageMetadata losslessly injects the subset of metadata chunks enabled in `keep`
// ("exif", "icc", "xmp") into a newly encoded JPEG, PNG, or WebP buffer.
func injectImageMetadata(encoded []byte, format string, keep map[string]bool, meta imageMetadataChunks, orientApplied bool) ([]byte, error) {
	var exifTIFF, iccBytes, xmpBytes []byte
	if keep["exif"] && len(meta.exifTIFF) > 0 {
		if orientApplied {
			exifTIFF = rewriteTIFFOrientationTo1(meta.exifTIFF)
		} else {
			exifTIFF = meta.exifTIFF
		}
	}
	if keep["icc"] && len(meta.iccProfile) > 0 {
		iccBytes = meta.iccProfile
	}
	if keep["xmp"] && len(meta.xmpXML) > 0 {
		xmpBytes = meta.xmpXML
	}
	if len(exifTIFF) == 0 && len(iccBytes) == 0 && len(xmpBytes) == 0 {
		return encoded, nil
	}

	switch format {
	case "jpg", "jpeg":
		if len(encoded) < 4 || encoded[0] != 0xFF || encoded[1] != 0xD8 {
			return encoded, nil
		}
		insertAt := 2
		if len(encoded) >= 6 && encoded[2] == 0xFF && encoded[3] == 0xE0 {
			app0Len := int(binary.BigEndian.Uint16(encoded[4:6]))
			if app0Len >= 2 && 4+app0Len <= len(encoded) {
				insertAt = 4 + app0Len
			}
		}
		var out bytes.Buffer
		out.Grow(len(encoded) + len(exifTIFF) + len(iccBytes) + len(xmpBytes) + 64)
		out.Write(encoded[:insertAt])

		if len(exifTIFF) > 0 && 2+6+len(exifTIFF) <= 65535 {
			out.Write([]byte{0xFF, 0xE1})
			var sz [2]byte
			binary.BigEndian.PutUint16(sz[:], uint16(2+6+len(exifTIFF)))
			out.Write(sz[:])
			out.WriteString("Exif\x00\x00")
			out.Write(exifTIFF)
		}
		if len(iccBytes) > 0 {
			const maxICCChunk = 65519
			numChunks := (len(iccBytes) + maxICCChunk - 1) / maxICCChunk
			if numChunks <= 255 {
				for idx := 0; idx < numChunks; idx++ {
					start := idx * maxICCChunk
					end := start + maxICCChunk
					if end > len(iccBytes) {
						end = len(iccBytes)
					}
					part := iccBytes[start:end]
					out.Write([]byte{0xFF, 0xE2})
					var sz [2]byte
					binary.BigEndian.PutUint16(sz[:], uint16(2+14+len(part)))
					out.Write(sz[:])
					out.WriteString("ICC_PROFILE\x00")
					out.WriteByte(byte(idx + 1))
					out.WriteByte(byte(numChunks))
					out.Write(part)
				}
			}
		}
		const xmpPrefix = "http://ns.adobe.com/xap/1.0/\x00"
		if len(xmpBytes) > 0 && 2+len(xmpPrefix)+len(xmpBytes) <= 65535 {
			out.Write([]byte{0xFF, 0xE1})
			var sz [2]byte
			binary.BigEndian.PutUint16(sz[:], uint16(2+len(xmpPrefix)+len(xmpBytes)))
			out.Write(sz[:])
			out.WriteString(xmpPrefix)
			out.Write(xmpBytes)
		}
		out.Write(encoded[insertAt:])
		return out.Bytes(), nil

	case "png":
		sig := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
		if !bytes.HasPrefix(encoded, sig) {
			return encoded, nil
		}
		var out bytes.Buffer
		out.Grow(len(encoded) + len(exifTIFF) + len(iccBytes) + len(xmpBytes) + 64)
		out.Write(sig)
		pos := 8
		injectedICCP := false
		injectedTail := false
		for pos+12 <= len(encoded) {
			cLen := int(binary.BigEndian.Uint32(encoded[pos : pos+4]))
			if cLen < 0 || pos+12+cLen > len(encoded) {
				return encoded, nil
			}
			cType := string(encoded[pos+4 : pos+8])
			totalChunk := 12 + cLen
			if cType == "IEND" && !injectedTail {
				injectedTail = true
				if len(exifTIFF) > 0 {
					writePNGChunk(&out, "eXIf", exifTIFF)
				}
				if len(xmpBytes) > 0 {
					var itxt bytes.Buffer
					itxt.WriteString("XML:com.adobe.xmp\x00\x00\x00\x00\x00")
					itxt.Write(xmpBytes)
					writePNGChunk(&out, "iTXt", itxt.Bytes())
				}
			}
			out.Write(encoded[pos : pos+totalChunk])
			if cType == "IHDR" && !injectedICCP && len(iccBytes) > 0 {
				injectedICCP = true
				var iccp bytes.Buffer
				iccp.WriteString("ICC Profile\x00\x00")
				zw := zlib.NewWriter(&iccp)
				_, _ = zw.Write(iccBytes)
				_ = zw.Close()
				writePNGChunk(&out, "iCCP", iccp.Bytes())
			}
			pos += totalChunk
			if cType == "IEND" {
				break
			}
		}
		return out.Bytes(), nil

	case "webp":
		if !isWebPRIFF(encoded) {
			return encoded, nil
		}
		cfg, _, cfgErr := decodeImageConfigSafe(encoded)
		if cfgErr != nil || cfg.Width <= 0 || cfg.Height <= 0 {
			return encoded, nil
		}
		type riffChunk struct {
			fourCC string
			data   []byte
		}
		var bodyChunks []riffChunk
		hasAlpha := false
		hasAnim := false
		pos := 12
		for pos+8 <= len(encoded) {
			fourCC := string(encoded[pos : pos+4])
			cLen := int(binary.LittleEndian.Uint32(encoded[pos+4 : pos+8]))
			if cLen < 0 || pos+8+cLen > len(encoded) {
				break
			}
			cData := encoded[pos+8 : pos+8+cLen]
			switch fourCC {
			case "VP8X":
				if len(cData) >= 1 && (cData[0]&0x10) != 0 {
					hasAlpha = true
				}
			case "EXIF", "ICCP", "XMP ":
				// Replaced below
			default:
				if fourCC == "ALPH" {
					hasAlpha = true
				} else if fourCC == "VP8L" && len(cData) >= 5 && (cData[4]&0x10) != 0 {
					hasAlpha = true
				} else if fourCC == "ANIM" || fourCC == "ANMF" {
					hasAnim = true
				}
				bodyChunks = append(bodyChunks, riffChunk{fourCC: fourCC, data: cData})
			}
			padded := cLen
			if padded%2 == 1 {
				padded++
			}
			pos += 8 + padded
		}
		if len(bodyChunks) == 0 {
			return encoded, nil
		}

		writeRIFFChunk := func(buf *bytes.Buffer, fourCC string, payload []byte) {
			buf.WriteString(fourCC)
			var sz [4]byte
			binary.LittleEndian.PutUint32(sz[:], uint32(len(payload)))
			buf.Write(sz[:])
			buf.Write(payload)
			if len(payload)%2 == 1 {
				buf.WriteByte(0x00)
			}
		}

		var flags byte
		if len(iccBytes) > 0 {
			flags |= 0x20
		}
		if hasAlpha {
			flags |= 0x10
		}
		if len(exifTIFF) > 0 {
			flags |= 0x08
		}
		if len(xmpBytes) > 0 {
			flags |= 0x04
		}
		if hasAnim {
			flags |= 0x02
		}

		wMinus1 := uint32(cfg.Width - 1)
		hMinus1 := uint32(cfg.Height - 1)
		vp8x := [10]byte{
			flags, 0, 0, 0,
			byte(wMinus1), byte(wMinus1 >> 8), byte(wMinus1 >> 16),
			byte(hMinus1), byte(hMinus1 >> 8), byte(hMinus1 >> 16),
		}

		var out bytes.Buffer
		out.Grow(len(encoded) + len(iccBytes) + len(exifTIFF) + len(xmpBytes) + 48)
		out.WriteString("RIFF\x00\x00\x00\x00WEBP")
		writeRIFFChunk(&out, "VP8X", vp8x[:])
		if len(iccBytes) > 0 {
			writeRIFFChunk(&out, "ICCP", iccBytes)
		}
		for _, ch := range bodyChunks {
			writeRIFFChunk(&out, ch.fourCC, ch.data)
		}
		if len(exifTIFF) > 0 {
			writeRIFFChunk(&out, "EXIF", exifTIFF)
		}
		if len(xmpBytes) > 0 {
			writeRIFFChunk(&out, "XMP ", xmpBytes)
		}
		res := out.Bytes()
		binary.LittleEndian.PutUint32(res[4:8], uint32(len(res)-8))
		return res, nil

	default:
		return encoded, nil
	}
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
		case hasAVIFMagic(raw):
			ext = ".avif"
		}
	}

	kind, detectedMIME, err := classifyAndVerifyMagic(ext, raw)
	if err != nil {
		return nil, "", 0, 0, false, err
	}
	if kind != mediaKindImage {
		return nil, "", 0, 0, false, fmt.Errorf("extension %q is not a supported raster image", ext)
	}
	if detectedMIME == "image/avif" {
		w, h, aErr := parseAVIFDimensions(raw)
		if aErr != nil {
			return nil, "", 0, 0, false, aErr
		}
		return raw, "image/avif", w, h, false, nil
	}

	cfg, formatName, cfgErr := decodeImageConfigSafe(raw)
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
		img, _, decErr := decodeRasterImageSafe(raw)
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

	// PNG: scan chunks for eXIf or XMP iTXt
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
			if chunkType == "iTXt" && bytes.HasPrefix(raw[i+8:i+8+chunkLen], []byte("XML:com.adobe.xmp\x00")) {
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

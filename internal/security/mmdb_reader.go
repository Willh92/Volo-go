package security

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
)

var mmdbMetaMarker = []byte("\xab\xcd\xefMaxMind.com")

const (
	mmdbMaxMetaTailBytes = 128 * 1024
	mmdbMaxDepth         = 32
	mmdbMaxCollectionLen = 4096
	mmdbMaxStringBytes   = 65536
)

type mmdbGeoRecord struct {
	CountryISO      string
	CountryZH       string
	CountryEN       string
	SubdivisionZH   string
	SubdivisionEN   string
	CityZH          string
	CityEN          string
	RegCountryISO   string
	RegCountryZH    string
	RegCountryEN    string
}

// mmdbReader is a zero-mmap, zero-reflection, thread-safe MaxMind DB v2.0 binary reader
// backed by io.ReaderAt (*os.File.ReadAt) so multi-megabyte GeoLite2 files never inflate VmRSS
// or hold Windows file-mapping locks across hot-reloads.
type mmdbReader struct {
	ra               io.ReaderAt
	closer           io.Closer
	fileSize         int64
	nodeCount        uint32
	recordSize       uint16 // 24, 28, or 32
	ipVersion        uint16 // 4 or 6
	databaseType     string
	buildEpoch       uint64
	nodeByteSize     int64
	searchTreeSize   int64
	dataSectionStart int64
	metaStart        int64
	ipv4StartNode    uint32
}

func openMMDBFile(path string) (*mmdbReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	r, err := newMMDBReader(f, st.Size(), f)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return r, nil
}

func newMMDBReader(ra io.ReaderAt, size int64, closer io.Closer) (*mmdbReader, error) {
	if size < int64(len(mmdbMetaMarker))+4 {
		return nil, errors.New("mmdb: file too small")
	}
	tailLen := int64(mmdbMaxMetaTailBytes)
	if tailLen > size {
		tailLen = size
	}
	tailStart := size - tailLen
	tailBuf := make([]byte, tailLen)
	if _, err := readFullAt(ra, tailBuf, tailStart); err != nil {
		return nil, fmt.Errorf("mmdb: read metadata tail: %w", err)
	}
	idx := bytes.LastIndex(tailBuf, mmdbMetaMarker)
	if idx < 0 {
		return nil, errors.New("mmdb: metadata marker not found")
	}
	metaStart := tailStart + int64(idx) + int64(len(mmdbMetaMarker))
	metaBytes := tailBuf[idx+len(mmdbMetaMarker):]

	r := &mmdbReader{
		ra:        ra,
		closer:    closer,
		fileSize:  size,
		metaStart: metaStart,
	}
	if err := r.parseMetadata(metaBytes); err != nil {
		return nil, err
	}
	if r.ipVersion == 6 {
		var node uint32
		for i := 0; i < 96 && node < r.nodeCount; i++ {
			next, err := r.readNodeRecord(node, 0)
			if err != nil {
				return nil, err
			}
			node = next
		}
		r.ipv4StartNode = node
	}
	return r, nil
}

func (r *mmdbReader) Close() error {
	if r == nil || r.closer == nil {
		return nil
	}
	return r.closer.Close()
}

func (r *mmdbReader) parseMetadata(metaBytes []byte) error {
	br := bytes.NewReader(metaBytes)
	cur := mmdbCursor{
		ra:       br,
		limit:    int64(len(metaBytes)),
		baseOff:  0,
	}
	typeNum, mapLen, nextOff, err := cur.readControl(0, 0)
	if err != nil {
		return fmt.Errorf("mmdb: metadata header: %w", err)
	}
	if typeNum != 7 || mapLen > mmdbMaxCollectionLen {
		return errors.New("mmdb: metadata root must be map")
	}

	var majorVer uint64
	off := nextOff
	for i := 0; i < mapLen; i++ {
		key, afterKey, err := cur.readStringValue(off, 0)
		if err != nil {
			return fmt.Errorf("mmdb: metadata key: %w", err)
		}
		switch key {
		case "binary_format_major_version":
			v, afterVal, err := cur.readUintValue(afterKey, 0)
			if err != nil {
				return err
			}
			majorVer = v
			off = afterVal
		case "node_count":
			v, afterVal, err := cur.readUintValue(afterKey, 0)
			if err != nil {
				return err
			}
			if v == 0 || v > 0x7fffffff {
				return errors.New("mmdb: invalid node_count")
			}
			r.nodeCount = uint32(v)
			off = afterVal
		case "record_size":
			v, afterVal, err := cur.readUintValue(afterKey, 0)
			if err != nil {
				return err
			}
			if v != 24 && v != 28 && v != 32 {
				return fmt.Errorf("mmdb: unsupported record_size %d", v)
			}
			r.recordSize = uint16(v)
			off = afterVal
		case "ip_version":
			v, afterVal, err := cur.readUintValue(afterKey, 0)
			if err != nil {
				return err
			}
			if v != 4 && v != 6 {
				return fmt.Errorf("mmdb: unsupported ip_version %d", v)
			}
			r.ipVersion = uint16(v)
			off = afterVal
		case "database_type":
			v, afterVal, err := cur.readStringValue(afterKey, 0)
			if err != nil {
				return err
			}
			r.databaseType = v
			off = afterVal
		case "build_epoch":
			v, afterVal, err := cur.readUintValue(afterKey, 0)
			if err != nil {
				return err
			}
			r.buildEpoch = v
			off = afterVal
		default:
			afterVal, err := cur.skipValue(afterKey, 0)
			if err != nil {
				return err
			}
			off = afterVal
		}
	}

	if majorVer != 2 || r.nodeCount == 0 || r.recordSize == 0 || r.ipVersion == 0 {
		return errors.New("mmdb: incomplete or unsupported v2 metadata")
	}
	r.nodeByteSize = int64(r.recordSize) * 2 / 8
	r.searchTreeSize = int64(r.nodeCount) * r.nodeByteSize
	r.dataSectionStart = r.searchTreeSize + 16
	if r.dataSectionStart > r.metaStart {
		return errors.New("mmdb: corrupt tree bounds exceeding file size")
	}
	return nil
}

func (r *mmdbReader) readNodeRecord(node uint32, bit uint8) (uint32, error) {
	var buf [8]byte
	off := int64(node) * r.nodeByteSize
	slice := buf[:r.nodeByteSize]
	if _, err := readFullAt(r.ra, slice, off); err != nil {
		return 0, err
	}
	switch r.recordSize {
	case 24:
		if bit == 0 {
			return uint32(slice[0])<<16 | uint32(slice[1])<<8 | uint32(slice[2]), nil
		}
		return uint32(slice[3])<<16 | uint32(slice[4])<<8 | uint32(slice[5]), nil
	case 28:
		if bit == 0 {
			return (uint32(slice[3]&0xf0) << 20) | uint32(slice[0])<<16 | uint32(slice[1])<<8 | uint32(slice[2]), nil
		}
		return (uint32(slice[3]&0x0f) << 24) | uint32(slice[4])<<16 | uint32(slice[5])<<8 | uint32(slice[6]), nil
	case 32:
		if bit == 0 {
			return binary.BigEndian.Uint32(slice[0:4]), nil
		}
		return binary.BigEndian.Uint32(slice[4:8]), nil
	default:
		return 0, errors.New("mmdb: invalid record size")
	}
}

func (r *mmdbReader) lookupGeo(addr netip.Addr) (mmdbGeoRecord, bool, error) {
	if r == nil || !addr.IsValid() {
		return mmdbGeoRecord{}, false, nil
	}
	u := addr.Unmap()
	var rawIP []byte
	var node uint32
	if u.Is4() {
		a4 := u.As4()
		rawIP = a4[:]
		if r.ipVersion == 6 {
			node = r.ipv4StartNode
		}
	} else {
		if r.ipVersion == 4 {
			return mmdbGeoRecord{}, false, nil
		}
		a16 := u.As16()
		rawIP = a16[:]
	}

	bitCount := len(rawIP) * 8
	for i := 0; i < bitCount && node < r.nodeCount; i++ {
		b := (rawIP[i>>3] >> uint(7-(i&7))) & 1
		next, err := r.readNodeRecord(node, b)
		if err != nil {
			return mmdbGeoRecord{}, false, err
		}
		node = next
	}

	if node <= r.nodeCount {
		return mmdbGeoRecord{}, false, nil
	}
	relData := int64(node-r.nodeCount) - 16
	if relData < 0 {
		return mmdbGeoRecord{}, false, errors.New("mmdb: invalid data pointer into separator")
	}
	absDataOff := r.dataSectionStart + relData
	if absDataOff >= r.metaStart {
		return mmdbGeoRecord{}, false, errors.New("mmdb: data offset out of range")
	}

	cur := mmdbCursor{
		ra:      r.ra,
		limit:   r.metaStart,
		baseOff: r.dataSectionStart,
	}
	var out mmdbGeoRecord
	if _, err := cur.decodeGeoRootMap(absDataOff, 0, &out); err != nil {
		return mmdbGeoRecord{}, false, err
	}
	return out, true, nil
}

type mmdbCursor struct {
	ra      io.ReaderAt
	limit   int64
	baseOff int64
}

func (c *mmdbCursor) readByte(off int64) (byte, error) {
	if off < 0 || off >= c.limit {
		return 0, io.ErrUnexpectedEOF
	}
	var b [1]byte
	if _, err := readFullAt(c.ra, b[:], off); err != nil {
		return 0, err
	}
	return b[0], nil
}

func (c *mmdbCursor) readBytes(off int64, dst []byte) error {
	if off < 0 || off+int64(len(dst)) > c.limit {
		return io.ErrUnexpectedEOF
	}
	_, err := readFullAt(c.ra, dst, off)
	return err
}

// readControl resolves pointers transparently and returns (typeNum, size, payloadOffset, err).
// Note: when a pointer is followed, payloadOffset is inside the pointed-to target; callers walking
// sequential elements must use readControlWithSpan to advance past the pointer bytes themselves.
func (c *mmdbCursor) readControl(off int64, depth int) (int, int, int64, error) {
	typeNum, size, payloadOff, _, err := c.readControlWithSpan(off, depth)
	return typeNum, size, payloadOff, err
}

// readControlWithSpan returns (typeNum, size, payloadOff, pointerNextOff, err).
// If pointerNextOff >= 0, the value at off was a pointer whose own encoding ended at pointerNextOff,
// while typeNum/size/payloadOff describe the dereferenced target!
func (c *mmdbCursor) readControlWithSpan(off int64, depth int) (int, int, int64, int64, error) {
	if depth > mmdbMaxDepth {
		return 0, 0, 0, -1, errors.New("mmdb: maximum pointer/nesting depth exceeded")
	}
	ctrl, err := c.readByte(off)
	if err != nil {
		return 0, 0, 0, -1, err
	}
	pos := off + 1
	typeNum := int(ctrl >> 5)
	sizeBits := int(ctrl & 0x1f)

	if typeNum == 1 {
		ptrSize := (sizeBits >> 3) & 0x3
		valBits := uint64(sizeBits & 0x7)
		var ptr uint64
		var buf [4]byte
		switch ptrSize {
		case 0:
			if err := c.readBytes(pos, buf[:1]); err != nil {
				return 0, 0, 0, -1, err
			}
			pos++
			ptr = (valBits << 8) | uint64(buf[0])
		case 1:
			if err := c.readBytes(pos, buf[:2]); err != nil {
				return 0, 0, 0, -1, err
			}
			pos += 2
			ptr = ((valBits << 16) | uint64(buf[0])<<8 | uint64(buf[1])) + 2048
		case 2:
			if err := c.readBytes(pos, buf[:3]); err != nil {
				return 0, 0, 0, -1, err
			}
			pos += 3
			ptr = ((valBits << 24) | uint64(buf[0])<<16 | uint64(buf[1])<<8 | uint64(buf[2])) + 526336
		case 3:
			if err := c.readBytes(pos, buf[:4]); err != nil {
				return 0, 0, 0, -1, err
			}
			pos += 4
			ptr = uint64(binary.BigEndian.Uint32(buf[:4]))
		}
		targetOff := c.baseOff + int64(ptr)
		tType, tSize, tPayload, _, err := c.readControlWithSpan(targetOff, depth+1)
		if err != nil {
			return 0, 0, 0, -1, err
		}
		return tType, tSize, tPayload, pos, nil
	}

	if typeNum == 0 {
		ext, err := c.readByte(pos)
		if err != nil {
			return 0, 0, 0, -1, err
		}
		pos++
		typeNum = int(ext) + 7
	}

	if typeNum == 14 {
		// Boolean stores value in sizeBits (0 or 1) with 0 payload bytes.
		return 14, sizeBits, pos, -1, nil
	}

	var size int
	var sbuf [3]byte
	switch {
	case sizeBits < 29:
		size = sizeBits
	case sizeBits == 29:
		if err := c.readBytes(pos, sbuf[:1]); err != nil {
			return 0, 0, 0, -1, err
		}
		pos++
		size = 29 + int(sbuf[0])
	case sizeBits == 30:
		if err := c.readBytes(pos, sbuf[:2]); err != nil {
			return 0, 0, 0, -1, err
		}
		pos += 2
		size = 285 + int(binary.BigEndian.Uint16(sbuf[:2]))
	default:
		if err := c.readBytes(pos, sbuf[:3]); err != nil {
			return 0, 0, 0, -1, err
		}
		pos += 3
		size = 65821 + int(uint32(sbuf[0])<<16|uint32(sbuf[1])<<8|uint32(sbuf[2]))
	}
	return typeNum, size, pos, -1, nil
}

func pickNextOff(computedEnd, ptrNextOff int64) int64 {
	if ptrNextOff >= 0 {
		return ptrNextOff
	}
	return computedEnd
}

func (c *mmdbCursor) readStringValue(off int64, depth int) (string, int64, error) {
	typeNum, size, payloadOff, ptrNext, err := c.readControlWithSpan(off, depth)
	if err != nil {
		return "", 0, err
	}
	if typeNum != 2 {
		next, sErr := c.skipValue(off, depth)
		return "", next, sErr
	}
	if size < 0 || size > mmdbMaxStringBytes {
		return "", 0, errors.New("mmdb: string exceeds safety cap")
	}
	if size == 0 {
		return "", pickNextOff(payloadOff, ptrNext), nil
	}
	buf := make([]byte, size)
	if err := c.readBytes(payloadOff, buf); err != nil {
		return "", 0, err
	}
	return string(buf), pickNextOff(payloadOff+int64(size), ptrNext), nil
}

func (c *mmdbCursor) readUintValue(off int64, depth int) (uint64, int64, error) {
	typeNum, size, payloadOff, ptrNext, err := c.readControlWithSpan(off, depth)
	if err != nil {
		return 0, 0, err
	}
	switch typeNum {
	case 5, 6, 9:
		if size < 0 || size > 8 {
			return 0, 0, errors.New("mmdb: invalid uint byte length")
		}
		var buf [8]byte
		if size > 0 {
			if err := c.readBytes(payloadOff, buf[:size]); err != nil {
				return 0, 0, err
			}
		}
		var v uint64
		for i := 0; i < size; i++ {
			v = (v << 8) | uint64(buf[i])
		}
		return v, pickNextOff(payloadOff+int64(size), ptrNext), nil
	default:
		next, sErr := c.skipValue(off, depth)
		return 0, next, sErr
	}
}

func (c *mmdbCursor) skipValue(off int64, depth int) (int64, error) {
	if depth > mmdbMaxDepth {
		return 0, errors.New("mmdb: nesting depth exceeded")
	}
	ctrl, err := c.readByte(off)
	if err != nil {
		return 0, err
	}
	if (ctrl >> 5) == 1 {
		// Pointer: no need to dereference target at all when skipping!
		ptrSize := int((ctrl & 0x1f) >> 3)
		return off + 1 + int64(ptrSize+1), nil
	}

	typeNum, size, payloadOff, _, err := c.readControlWithSpan(off, depth)
	if err != nil {
		return 0, err
	}
	switch typeNum {
	case 2, 4, 5, 6, 8, 9, 10:
		return payloadOff + int64(size), nil
	case 3:
		return payloadOff + 8, nil
	case 15:
		return payloadOff + 4, nil
	case 13, 14:
		return payloadOff, nil
	case 7:
		if size > mmdbMaxCollectionLen {
			return 0, errors.New("mmdb: map size exceeds cap")
		}
		curOff := payloadOff
		for i := 0; i < size; i++ {
			curOff, err = c.skipValue(curOff, depth+1)
			if err != nil {
				return 0, err
			}
			curOff, err = c.skipValue(curOff, depth+1)
			if err != nil {
				return 0, err
			}
		}
		return curOff, nil
	case 11:
		if size > mmdbMaxCollectionLen {
			return 0, errors.New("mmdb: array size exceeds cap")
		}
		curOff := payloadOff
		for i := 0; i < size; i++ {
			curOff, err = c.skipValue(curOff, depth+1)
			if err != nil {
				return 0, err
			}
		}
		return curOff, nil
	default:
		return 0, fmt.Errorf("mmdb: unsupported data type %d", typeNum)
	}
}

func (c *mmdbCursor) decodeGeoRootMap(off int64, depth int, out *mmdbGeoRecord) (int64, error) {
	typeNum, count, payloadOff, ptrNext, err := c.readControlWithSpan(off, depth)
	if err != nil {
		return 0, err
	}
	if typeNum != 7 || count > mmdbMaxCollectionLen {
		return c.skipValue(off, depth)
	}
	curOff := payloadOff
	for i := 0; i < count; i++ {
		key, afterKey, err := c.readStringValue(curOff, depth+1)
		if err != nil {
			return 0, err
		}
		switch key {
		case "country":
			curOff, err = c.decodePlaceMap(afterKey, depth+1, &out.CountryISO, &out.CountryZH, &out.CountryEN)
		case "registered_country":
			curOff, err = c.decodePlaceMap(afterKey, depth+1, &out.RegCountryISO, &out.RegCountryZH, &out.RegCountryEN)
		case "city":
			curOff, err = c.decodePlaceMap(afterKey, depth+1, nil, &out.CityZH, &out.CityEN)
		case "subdivisions":
			curOff, err = c.decodeSubdivisionsArray(afterKey, depth+1, &out.SubdivisionZH, &out.SubdivisionEN)
		default:
			curOff, err = c.skipValue(afterKey, depth+1)
		}
		if err != nil {
			return 0, err
		}
	}
	return pickNextOff(curOff, ptrNext), nil
}

func (c *mmdbCursor) decodePlaceMap(off int64, depth int, isoOut, zhOut, enOut *string) (int64, error) {
	typeNum, count, payloadOff, ptrNext, err := c.readControlWithSpan(off, depth)
	if err != nil {
		return 0, err
	}
	if typeNum != 7 || count > mmdbMaxCollectionLen {
		return c.skipValue(off, depth)
	}
	curOff := payloadOff
	for i := 0; i < count; i++ {
		key, afterKey, err := c.readStringValue(curOff, depth+1)
		if err != nil {
			return 0, err
		}
		switch key {
		case "iso_code":
			if isoOut != nil {
				v, next, sErr := c.readStringValue(afterKey, depth+1)
				if sErr != nil {
					return 0, sErr
				}
				*isoOut = v
				curOff = next
			} else {
				curOff, err = c.skipValue(afterKey, depth+1)
			}
		case "names":
			curOff, err = c.decodeNamesMap(afterKey, depth+1, zhOut, enOut)
		default:
			curOff, err = c.skipValue(afterKey, depth+1)
		}
		if err != nil {
			return 0, err
		}
	}
	return pickNextOff(curOff, ptrNext), nil
}

func (c *mmdbCursor) decodeNamesMap(off int64, depth int, zhOut, enOut *string) (int64, error) {
	typeNum, count, payloadOff, ptrNext, err := c.readControlWithSpan(off, depth)
	if err != nil {
		return 0, err
	}
	if typeNum != 7 || count > mmdbMaxCollectionLen {
		return c.skipValue(off, depth)
	}
	curOff := payloadOff
	for i := 0; i < count; i++ {
		key, afterKey, err := c.readStringValue(curOff, depth+1)
		if err != nil {
			return 0, err
		}
		switch key {
		case "zh-CN":
			if zhOut != nil {
				v, next, sErr := c.readStringValue(afterKey, depth+1)
				if sErr != nil {
					return 0, sErr
				}
				*zhOut = v
				curOff = next
			} else {
				curOff, err = c.skipValue(afterKey, depth+1)
			}
		case "en":
			if enOut != nil {
				v, next, sErr := c.readStringValue(afterKey, depth+1)
				if sErr != nil {
					return 0, sErr
				}
				*enOut = v
				curOff = next
			} else {
				curOff, err = c.skipValue(afterKey, depth+1)
			}
		default:
			curOff, err = c.skipValue(afterKey, depth+1)
		}
		if err != nil {
			return 0, err
		}
	}
	return pickNextOff(curOff, ptrNext), nil
}

func (c *mmdbCursor) decodeSubdivisionsArray(off int64, depth int, zhOut, enOut *string) (int64, error) {
	typeNum, count, payloadOff, ptrNext, err := c.readControlWithSpan(off, depth)
	if err != nil {
		return 0, err
	}
	if typeNum != 11 || count > mmdbMaxCollectionLen {
		return c.skipValue(off, depth)
	}
	curOff := payloadOff
	for i := 0; i < count; i++ {
		if i == 0 {
			curOff, err = c.decodePlaceMap(curOff, depth+1, nil, zhOut, enOut)
		} else {
			curOff, err = c.skipValue(curOff, depth+1)
		}
		if err != nil {
			return 0, err
		}
	}
	return pickNextOff(curOff, ptrNext), nil
}

func readFullAt(ra io.ReaderAt, dst []byte, off int64) (int, error) {
	if len(dst) == 0 {
		return 0, nil
	}
	n, err := ra.ReadAt(dst, off)
	if n == len(dst) {
		return n, nil
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

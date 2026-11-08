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

const (
	xdbStructure20   = 2
	xdbStructure30   = 3
	xdbHeaderLen     = 256
	xdbVecRows       = 256
	xdbVecCols       = 256
	xdbVecCell       = 8
	xdbVecTotalBytes = xdbVecRows * xdbVecCols * xdbVecCell // 512 KiB
	xdbIPv4Ver       = 4
	xdbIPv6Ver       = 6
	xdbSegSizeV4     = 14 // 4 + 4 + 2 + 4
	xdbSegSizeV6     = 38 // 16 + 16 + 2 + 4
	xdbMaxRegionLen  = 4096
)

// xdbReader is a pure-Go, lock-free, io.ReaderAt-backed ip2region v2.0/v3.0 (IPv4 & IPv6) reader.
// Only the 512 KiB vector index is retained in memory; segment and region reads use stateless pread/ReadAt.
type xdbReader struct {
	ra        io.ReaderAt
	closer    io.Closer
	fileSize  int64
	ipVersion int
	createdAt uint32
	vecIndex  []byte
}

func openXDBFile(path string) (*xdbReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	r, err := newXDBReader(f, st.Size(), f)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return r, nil
}

func newXDBReader(ra io.ReaderAt, size int64, closer io.Closer) (*xdbReader, error) {
	if size < xdbHeaderLen+xdbVecTotalBytes {
		return nil, errors.New("xdb: file too small")
	}
	var hdr [20]byte
	if _, err := readFullAt(ra, hdr[:], 0); err != nil {
		return nil, fmt.Errorf("xdb: read header: %w", err)
	}
	structVer := binary.LittleEndian.Uint16(hdr[0:2])
	createdAt := binary.LittleEndian.Uint32(hdr[4:8])

	var ipVer int
	var ptrBytes int
	switch structVer {
	case xdbStructure20:
		ipVer = xdbIPv4Ver
		ptrBytes = 4
	case xdbStructure30:
		ipVer = int(binary.LittleEndian.Uint16(hdr[16:18]))
		ptrBytes = int(binary.LittleEndian.Uint16(hdr[18:20]))
	default:
		return nil, fmt.Errorf("xdb: unsupported structure version %d", structVer)
	}
	if ipVer != xdbIPv4Ver && ipVer != xdbIPv6Ver {
		return nil, fmt.Errorf("xdb: unsupported ip version %d", ipVer)
	}
	if ptrBytes <= 0 || ptrBytes > 7 {
		return nil, fmt.Errorf("xdb: invalid runtime ptr bytes %d", ptrBytes)
	}
	maxPtr := (int64(1) << (ptrBytes * 8)) - 1
	if size > maxPtr {
		return nil, errors.New("xdb: file exceeds maximum pointer range")
	}

	vec := make([]byte, xdbVecTotalBytes)
	if _, err := readFullAt(ra, vec, xdbHeaderLen); err != nil {
		return nil, fmt.Errorf("xdb: read vector index: %w", err)
	}
	return &xdbReader{
		ra:        ra,
		closer:    closer,
		fileSize:  size,
		ipVersion: ipVer,
		createdAt: createdAt,
		vecIndex:  vec,
	}, nil
}

func (r *xdbReader) Close() error {
	if r == nil || r.closer == nil {
		return nil
	}
	return r.closer.Close()
}

func (r *xdbReader) lookup(addr netip.Addr) (string, error) {
	if r == nil {
		return "", nil
	}
	u := addr.Unmap()
	var il0, il1 int
	var ip4 uint32
	var ip16 [16]byte
	var segSize uint32

	if r.ipVersion == xdbIPv4Ver {
		if !u.Is4() {
			return "", nil
		}
		a4 := u.As4()
		il0, il1 = int(a4[0]), int(a4[1])
		ip4 = binary.BigEndian.Uint32(a4[:])
		segSize = xdbSegSizeV4
	} else {
		if !u.Is6() {
			return "", nil
		}
		ip16 = u.As16()
		il0, il1 = int(ip16[0]), int(ip16[1])
		segSize = xdbSegSizeV6
	}

	idx := il0*xdbVecCols*xdbVecCell + il1*xdbVecCell
	sPtr := binary.LittleEndian.Uint32(r.vecIndex[idx : idx+4])
	ePtr := binary.LittleEndian.Uint32(r.vecIndex[idx+4 : idx+8])
	if sPtr == 0 || ePtr == 0 || ePtr < sPtr {
		return "", nil
	}

	var segBuf [xdbSegSizeV6]byte
	segSlice := segBuf[:segSize]
	var dataLen uint16
	var dataPtr uint32

	l, h := 0, int((ePtr-sPtr)/segSize)
	for l <= h {
		m := (l + h) >> 1
		p := int64(sPtr) + int64(m)*int64(segSize)
		if _, err := readFullAt(r.ra, segSlice, p); err != nil {
			return "", err
		}
		if r.ipVersion == xdbIPv4Ver {
			sIP := binary.LittleEndian.Uint32(segSlice[0:4])
			if ip4 < sIP {
				h = m - 1
				continue
			}
			eIP := binary.LittleEndian.Uint32(segSlice[4:8])
			if ip4 > eIP {
				l = m + 1
				continue
			}
			dataLen = binary.LittleEndian.Uint16(segSlice[8:10])
			dataPtr = binary.LittleEndian.Uint32(segSlice[10:14])
			break
		} else {
			if bytes.Compare(ip16[:], segSlice[0:16]) < 0 {
				h = m - 1
				continue
			}
			if bytes.Compare(ip16[:], segSlice[16:32]) > 0 {
				l = m + 1
				continue
			}
			dataLen = binary.LittleEndian.Uint16(segSlice[32:34])
			dataPtr = binary.LittleEndian.Uint32(segSlice[34:38])
			break
		}
	}

	if dataLen == 0 || dataLen > xdbMaxRegionLen {
		return "", nil
	}
	regBuf := make([]byte, dataLen)
	if _, err := readFullAt(r.ra, regBuf, int64(dataPtr)); err != nil {
		return "", err
	}
	return string(regBuf), nil
}

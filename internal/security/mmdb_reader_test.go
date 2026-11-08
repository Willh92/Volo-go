package security

import (
	"bytes"
	"encoding/binary"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
)

func mmdbEncodeString(s string) []byte {
	b := []byte(s)
	n := len(b)
	var out []byte
	if n < 29 {
		out = append(out, byte((2<<5)|n))
	} else {
		out = append(out, byte((2<<5)|29), byte(n-29))
	}
	return append(out, b...)
}

func mmdbEncodeUint16(v uint16) []byte {
	if v == 0 {
		return []byte{byte(5 << 5)}
	}
	if v <= 0xff {
		return []byte{byte((5 << 5) | 1), byte(v)}
	}
	return []byte{byte((5 << 5) | 2), byte(v >> 8), byte(v)}
}

func mmdbEncodeUint32(v uint32) []byte {
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], v)
	i := 0
	for i < 4 && tmp[i] == 0 {
		i++
	}
	rem := tmp[i:]
	return append([]byte{byte((6 << 5) | len(rem))}, rem...)
}

func mmdbEncodeMapHeader(pairs int) []byte {
	return []byte{byte((7 << 5) | pairs)}
}

func mmdbEncodeArrayHeader(items int) []byte {
	return []byte{byte(0<<5) | byte(items), 4} // extended type 4+7 = 11 (array)
}

func mmdbEncodePointer0(offset int) []byte {
	return []byte{byte((1 << 5) | ((offset >> 8) & 0x7)), byte(offset & 0xff)}
}

func buildSyntheticMMDB(recordSize uint16) []byte {
	// Build a compact IPv6 tree where:
	// - 96 zero bits lead from node 0 -> node 1 (ipv4StartNode = 1)
	//   Actually to keep node count tiny: node 0 left=0? No, cycles in tree aren't allowed;
	//   let's make nodes 0..95 walk 0-bit -> i+1, 1-bit -> nodeCount (empty),
	//   and at node 96 (start of IPv4):
	//   first bit 0 (e.g. 8.8.8.8 starts with 0) -> data record A (US / California / Mountain View)
	//   first bit 1 (e.g. 223.5.5.5 starts with 1) -> data record B (CN / 浙江省 / 杭州市 via shared pointer!)
	//   Also from node 0 bit 1 (e.g. 2001:4860::8888 starts with 0010..., wait 0010 starts with 0,0,1!):
	//   so on node 2 (after 0,0), bit 1 -> data record A!
	const nodeCount = 97

	// Data section:
	var data bytes.Buffer
	// Offset 0: shared string "zh-CN"
	offZH := data.Len()
	data.Write(mmdbEncodeString("zh-CN"))
	// Offset: shared string "en"
	offEN := data.Len()
	data.Write(mmdbEncodeString("en"))
	// Shared map for China country
	offCNCountry := data.Len()
	data.Write(mmdbEncodeMapHeader(2))
	data.Write(mmdbEncodeString("iso_code"))
	data.Write(mmdbEncodeString("CN"))
	data.Write(mmdbEncodeString("names"))
	data.Write(mmdbEncodeMapHeader(2))
	data.Write(mmdbEncodePointer0(offZH))
	data.Write(mmdbEncodeString("中国"))
	data.Write(mmdbEncodePointer0(offEN))
	data.Write(mmdbEncodeString("China"))

	// Record A: US / California / Mountain View (with extra ignored float64 & bool fields)
	offRecA := data.Len()
	data.Write(mmdbEncodeMapHeader(5))
	data.Write(mmdbEncodeString("continent"))
	data.Write(mmdbEncodeMapHeader(1))
	data.Write(mmdbEncodeString("code"))
	data.Write(mmdbEncodeString("NA"))
	data.Write(mmdbEncodeString("is_anycast"))
	data.Write([]byte{0x01, 7}) // extended type 7+7=14 boolean true
	data.Write(mmdbEncodeString("country"))
	data.Write(mmdbEncodeMapHeader(2))
	data.Write(mmdbEncodeString("iso_code"))
	data.Write(mmdbEncodeString("US"))
	data.Write(mmdbEncodeString("names"))
	data.Write(mmdbEncodeMapHeader(1))
	data.Write(mmdbEncodePointer0(offEN))
	data.Write(mmdbEncodeString("United States"))
	data.Write(mmdbEncodeString("subdivisions"))
	data.Write(mmdbEncodeArrayHeader(1))
	data.Write(mmdbEncodeMapHeader(1))
	data.Write(mmdbEncodeString("names"))
	data.Write(mmdbEncodeMapHeader(1))
	data.Write(mmdbEncodePointer0(offEN))
	data.Write(mmdbEncodeString("California"))
	data.Write(mmdbEncodeString("city"))
	data.Write(mmdbEncodeMapHeader(1))
	data.Write(mmdbEncodeString("names"))
	data.Write(mmdbEncodeMapHeader(1))
	data.Write(mmdbEncodePointer0(offEN))
	data.Write(mmdbEncodeString("Mountain View"))

	// Record B: CN (via pointer to offCNCountry) + 浙江省 + 杭州市
	offRecB := data.Len()
	data.Write(mmdbEncodeMapHeader(3))
	data.Write(mmdbEncodeString("country"))
	data.Write(mmdbEncodePointer0(offCNCountry))
	data.Write(mmdbEncodeString("subdivisions"))
	data.Write(mmdbEncodeArrayHeader(1))
	data.Write(mmdbEncodeMapHeader(1))
	data.Write(mmdbEncodeString("names"))
	data.Write(mmdbEncodeMapHeader(2))
	data.Write(mmdbEncodePointer0(offZH))
	data.Write(mmdbEncodeString("浙江省"))
	data.Write(mmdbEncodePointer0(offEN))
	data.Write(mmdbEncodeString("Zhejiang"))
	data.Write(mmdbEncodeString("city"))
	data.Write(mmdbEncodeMapHeader(1))
	data.Write(mmdbEncodeString("names"))
	data.Write(mmdbEncodeMapHeader(1))
	data.Write(mmdbEncodePointer0(offZH))
	data.Write(mmdbEncodeString("杭州市"))

	recAPtr := uint32(nodeCount + 16 + offRecA)
	recBPtr := uint32(nodeCount + 16 + offRecB)

	encodeNode := func(left, right uint32) []byte {
		switch recordSize {
		case 24:
			return []byte{
				byte(left >> 16), byte(left >> 8), byte(left),
				byte(right >> 16), byte(right >> 8), byte(right),
			}
		case 28:
			mid := byte(((left >> 24) & 0x0f) << 4) | byte((right>>24)&0x0f)
			return []byte{
				byte(left >> 16), byte(left >> 8), byte(left),
				mid,
				byte(right >> 16), byte(right >> 8), byte(right),
			}
		default:
			var b [8]byte
			binary.BigEndian.PutUint32(b[0:4], left)
			binary.BigEndian.PutUint32(b[4:8], right)
			return b[:]
		}
	}

	var out bytes.Buffer
	for i := 0; i < 96; i++ {
		right := uint32(nodeCount)
		if i == 2 {
			// 2001:4860:4860::8888 begins with 0x20 = 0010_0000 -> bits 0, 0, 1!
			right = recAPtr
		}
		out.Write(encodeNode(uint32(i+1), right))
	}
	// Node 96: IPv4 root! Bit 0 -> recAPtr, Bit 1 -> recBPtr
	out.Write(encodeNode(recAPtr, recBPtr))

	// 16-byte NUL separator
	out.Write(make([]byte, 16))
	// Data section
	out.Write(data.Bytes())
	// Metadata marker + map
	out.Write(mmdbMetaMarker)
	out.Write(mmdbEncodeMapHeader(4))
	out.Write(mmdbEncodeString("binary_format_major_version"))
	out.Write(mmdbEncodeUint16(2))
	out.Write(mmdbEncodeString("node_count"))
	out.Write(mmdbEncodeUint32(nodeCount))
	out.Write(mmdbEncodeString("record_size"))
	out.Write(mmdbEncodeUint16(recordSize))
	out.Write(mmdbEncodeString("ip_version"))
	out.Write(mmdbEncodeUint16(6))
	return out.Bytes()
}

func TestMMDBReaderRecordSizesPointersAndGeoEngine(t *testing.T) {
	for _, recSize := range []uint16{24, 28, 32} {
		raw := buildSyntheticMMDB(recSize)
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "GeoLite2-City.mmdb")
		if err := os.WriteFile(dbPath, raw, 0o600); err != nil {
			t.Fatal(err)
		}

		eng := NewGeoEngine(dir, nil)
		defer eng.Close()

		// 1. IPv4 8.8.8.8 -> US, California·Mountain View
		locUS, ok := eng.Lookup(netip.MustParseAddr("8.8.8.8"))
		if !ok || locUS.CountryISO != "US" || locUS.Region != "California·Mountain View" {
			t.Fatalf("recSize=%d 8.8.8.8 lookup mismatch: ok=%v loc=%+v", recSize, ok, locUS)
		}

		// 2. IPv4 223.5.5.5 -> CN, 浙江省·杭州市 (via shared country pointer!)
		locCN, ok := eng.Lookup(netip.MustParseAddr("223.5.5.5"))
		if !ok || locCN.CountryISO != "CN" || locCN.Region != "浙江省·杭州市" {
			t.Fatalf("recSize=%d 223.5.5.5 lookup mismatch: ok=%v loc=%+v", recSize, ok, locCN)
		}

		// 3. IPv6 2001:4860:4860::8888 -> US, California·Mountain View
		locV6, ok := eng.Lookup(netip.MustParseAddr("2001:4860:4860::8888"))
		if !ok || locV6.CountryISO != "US" || locV6.Region != "California·Mountain View" {
			t.Fatalf("recSize=%d IPv6 lookup mismatch: ok=%v loc=%+v", recSize, ok, locV6)
		}

		// 4. Unmapped IPv6 starting with bit 1 (e.g. fe00::1 -> bit 0 is 1 -> nodeCount empty)
		if _, ok := eng.Lookup(netip.MustParseAddr("2600::1")); ok {
			// 0x26 = 0010_0110 -> actually starts with 0,0,1 so matches node 2 right! Test 0x8000::1 (bit 0 = 1)
		}
		if _, ok := eng.Lookup(netip.MustParseAddr("8000::1")); ok {
			t.Fatalf("recSize=%d expected 8000::1 to miss tree", recSize)
		}
	}
}

func TestMMDBReaderRejectsCyclicPointerBomb(t *testing.T) {
	// Construct a minimal 1-node IPv4 tree where data offset 0 is a pointer to offset 0 (self-cycle!).
	var out bytes.Buffer
	// 1 node, 24-bit: left -> 1+16+0 = 17, right -> 1 (not found)
	out.Write([]byte{0, 0, 17, 0, 0, 1})
	out.Write(make([]byte, 16))
	// Data offset 0: pointer0(0) -> infinite loop unless depth-guarded!
	out.Write(mmdbEncodePointer0(0))
	out.Write(mmdbMetaMarker)
	out.Write(mmdbEncodeMapHeader(4))
	out.Write(mmdbEncodeString("binary_format_major_version"))
	out.Write(mmdbEncodeUint16(2))
	out.Write(mmdbEncodeString("node_count"))
	out.Write(mmdbEncodeUint32(1))
	out.Write(mmdbEncodeString("record_size"))
	out.Write(mmdbEncodeUint16(24))
	out.Write(mmdbEncodeString("ip_version"))
	out.Write(mmdbEncodeUint16(4))

	r, err := newMMDBReader(bytes.NewReader(out.Bytes()), int64(out.Len()), nil)
	if err != nil {
		t.Fatalf("unexpected open error: %v", err)
	}
	if _, ok, err := r.lookupGeo(netip.MustParseAddr("1.1.1.1")); err == nil || ok {
		t.Fatalf("expected cyclic pointer error, got ok=%v err=%v", ok, err)
	}
}

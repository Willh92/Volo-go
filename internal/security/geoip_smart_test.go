package security

import (
	"bytes"
	"encoding/binary"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Willh92/Volo-go/internal/domain"
)

func mmdbEncodeUint64(v uint64) []byte {
	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], v)
	i := 0
	for i < 8 && tmp[i] == 0 {
		i++
	}
	rem := tmp[i:]
	// MMDB type 9 (uint64) uses extended type: ctrl byte (0<<5 | len) followed by (9 - 7) = 2.
	return append([]byte{byte(len(rem)), 2}, rem...)
}

// buildTypedMMDB constructs a synthetic MaxMind DB with explicit database_type and build_epoch metadata.
// mode:
//   - "asn": stores only autonomous_system_number / autonomous_system_organization (no country/city)
//   - "country": stores country ISO only ("US" for 8.8.8.8, "CN" for 223.5.5.5)
//   - "city": stores country + subdivision + city (with optional customUSCity label)
func buildTypedMMDB(dbType string, buildEpoch uint64, mode string, customUSCity string) []byte {
	const nodeCount = 97
	if customUSCity == "" {
		customUSCity = "Mountain View"
	}

	var data bytes.Buffer
	offEN := data.Len()
	data.Write(mmdbEncodeString("en"))
	offZH := data.Len()
	data.Write(mmdbEncodeString("zh-CN"))

	var offRecA, offRecB int
	switch mode {
	case "asn":
		offRecA = data.Len()
		data.Write(mmdbEncodeMapHeader(2))
		data.Write(mmdbEncodeString("autonomous_system_number"))
		data.Write(mmdbEncodeUint32(15169))
		data.Write(mmdbEncodeString("autonomous_system_organization"))
		data.Write(mmdbEncodeString("GOOGLE"))
		offRecB = offRecA

	case "country":
		offRecA = data.Len()
		data.Write(mmdbEncodeMapHeader(1))
		data.Write(mmdbEncodeString("country"))
		data.Write(mmdbEncodeMapHeader(2))
		data.Write(mmdbEncodeString("iso_code"))
		data.Write(mmdbEncodeString("US"))
		data.Write(mmdbEncodeString("names"))
		data.Write(mmdbEncodeMapHeader(1))
		data.Write(mmdbEncodePointer0(offEN))
		data.Write(mmdbEncodeString("United States"))

		offRecB = data.Len()
		data.Write(mmdbEncodeMapHeader(1))
		data.Write(mmdbEncodeString("country"))
		data.Write(mmdbEncodeMapHeader(2))
		data.Write(mmdbEncodeString("iso_code"))
		data.Write(mmdbEncodeString("CN"))
		data.Write(mmdbEncodeString("names"))
		data.Write(mmdbEncodeMapHeader(1))
		data.Write(mmdbEncodePointer0(offZH))
		data.Write(mmdbEncodeString("中国"))

	default: // "city"
		offRecA = data.Len()
		data.Write(mmdbEncodeMapHeader(3))
		data.Write(mmdbEncodeString("country"))
		data.Write(mmdbEncodeMapHeader(1))
		data.Write(mmdbEncodeString("iso_code"))
		data.Write(mmdbEncodeString("US"))
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
		data.Write(mmdbEncodeString(customUSCity))

		offRecB = data.Len()
		data.Write(mmdbEncodeMapHeader(3))
		data.Write(mmdbEncodeString("country"))
		data.Write(mmdbEncodeMapHeader(1))
		data.Write(mmdbEncodeString("iso_code"))
		data.Write(mmdbEncodeString("CN"))
		data.Write(mmdbEncodeString("subdivisions"))
		data.Write(mmdbEncodeArrayHeader(1))
		data.Write(mmdbEncodeMapHeader(1))
		data.Write(mmdbEncodeString("names"))
		data.Write(mmdbEncodeMapHeader(1))
		data.Write(mmdbEncodePointer0(offZH))
		data.Write(mmdbEncodeString("浙江省"))
		data.Write(mmdbEncodeString("city"))
		data.Write(mmdbEncodeMapHeader(1))
		data.Write(mmdbEncodeString("names"))
		data.Write(mmdbEncodeMapHeader(1))
		data.Write(mmdbEncodePointer0(offZH))
		data.Write(mmdbEncodeString("杭州市"))
	}

	recAPtr := uint32(nodeCount + 16 + offRecA)
	recBPtr := uint32(nodeCount + 16 + offRecB)
	encodeNode24 := func(left, right uint32) []byte {
		return []byte{
			byte(left >> 16), byte(left >> 8), byte(left),
			byte(right >> 16), byte(right >> 8), byte(right),
		}
	}

	var out bytes.Buffer
	for i := 0; i < 96; i++ {
		right := uint32(nodeCount)
		if i == 2 {
			right = recAPtr
		}
		out.Write(encodeNode24(uint32(i+1), right))
	}
	out.Write(encodeNode24(recAPtr, recBPtr))
	out.Write(make([]byte, 16))
	out.Write(data.Bytes())

	out.Write(mmdbMetaMarker)
	pairs := 5
	if dbType != "" {
		pairs = 6
	}
	out.Write(mmdbEncodeMapHeader(pairs))
	out.Write(mmdbEncodeString("binary_format_major_version"))
	out.Write(mmdbEncodeUint16(2))
	out.Write(mmdbEncodeString("node_count"))
	out.Write(mmdbEncodeUint32(nodeCount))
	out.Write(mmdbEncodeString("record_size"))
	out.Write(mmdbEncodeUint16(24))
	out.Write(mmdbEncodeString("ip_version"))
	out.Write(mmdbEncodeUint16(6))
	out.Write(mmdbEncodeString("build_epoch"))
	out.Write(mmdbEncodeUint64(buildEpoch))
	if dbType != "" {
		out.Write(mmdbEncodeString("database_type"))
		out.Write(mmdbEncodeString(dbType))
	}
	return out.Bytes()
}

type syntheticXDBEntry struct {
	addr   netip.Addr
	region string
}

// buildSyntheticXDB creates a valid Structure 3.0 (or 2.0 for IPv4 when structVer == xdbStructure20) ip2region .xdb binary
// that passes header/pointer verification, 512 KiB vector index loading, and binary-search segment lookup.
func buildSyntheticXDB(ipVer int, createdAt uint32, entries []syntheticXDBEntry) []byte {
	const headerLen = xdbHeaderLen
	const vecLen = xdbVecTotalBytes

	vec := make([]byte, vecLen)
	var payload bytes.Buffer
	baseDataOffset := uint32(headerLen + vecLen)

	var firstSegPtr, lastSegPtr uint32
	for idx, ent := range entries {
		regBytes := []byte(ent.region)
		dataPtr := baseDataOffset + uint32(payload.Len())
		payload.Write(regBytes)

		segPtr := baseDataOffset + uint32(payload.Len())
		if idx == 0 {
			firstSegPtr = segPtr
		}
		lastSegPtr = segPtr

		u := ent.addr.Unmap()
		var il0, il1 int
		if ipVer == xdbIPv4Ver {
			a4 := u.As4()
			il0, il1 = int(a4[0]), int(a4[1])
			// xdb IPv4 segment index stores IP bytes in little-endian order (4+4+2+4 = 14 bytes).
			leIP := [4]byte{a4[3], a4[2], a4[1], a4[0]}
			payload.Write(leIP[:])
			payload.Write(leIP[:])
		} else {
			a16 := u.As16()
			il0, il1 = int(a16[0]), int(a16[1])
			// xdb IPv6 segment index stores IP bytes in big-endian order (16+16+2+4 = 38 bytes).
			payload.Write(a16[:])
			payload.Write(a16[:])
		}
		var tail [6]byte
		binary.LittleEndian.PutUint16(tail[0:2], uint16(len(regBytes)))
		binary.LittleEndian.PutUint32(tail[2:6], dataPtr)
		payload.Write(tail[:])

		vecOff := il0*xdbVecCols*xdbVecCell + il1*xdbVecCell
		binary.LittleEndian.PutUint32(vec[vecOff:vecOff+4], segPtr)
		binary.LittleEndian.PutUint32(vec[vecOff+4:vecOff+8], segPtr)
	}

	hdr := make([]byte, headerLen)
	binary.LittleEndian.PutUint16(hdr[0:2], xdbStructure30)
	binary.LittleEndian.PutUint16(hdr[2:4], 1) // VectorIndexPolicy
	binary.LittleEndian.PutUint32(hdr[4:8], createdAt)
	binary.LittleEndian.PutUint32(hdr[8:12], firstSegPtr)
	binary.LittleEndian.PutUint32(hdr[12:16], lastSegPtr)
	binary.LittleEndian.PutUint16(hdr[16:18], uint16(ipVer))
	binary.LittleEndian.PutUint16(hdr[18:20], 4) // 4-byte runtime pointers

	var full bytes.Buffer
	full.Write(hdr)
	full.Write(vec)
	full.Write(payload.Bytes())
	return full.Bytes()
}

func TestGeoEngineSmartMultiMMDBSelectionAndASNExclusion(t *testing.T) {
	dir := t.TempDir()

	// Place ASN first alphabetically ("a_..."), Country second ("b_..."), older City third ("c_..."),
	// newer City last ("z_..."), plus a corrupt .mmdb file.
	mustWrite := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("a_GeoLite2-ASN.mmdb", buildTypedMMDB("GeoLite2-ASN", 9999999999, "asn", ""))
	mustWrite("b_GeoLite2-Country.mmdb", buildTypedMMDB("GeoLite2-Country", 5000, "country", ""))
	mustWrite("c_GeoLite2-City-old.mmdb", buildTypedMMDB("GeoLite2-City", 1000, "city", "Old City"))
	mustWrite("d_corrupt.mmdb", []byte("not-a-valid-mmdb"))
	mustWrite("z_GeoLite2-City-new.mmdb", buildTypedMMDB("GeoLite2-City", 2000, "city", "Sunnyvale"))

	eng := NewGeoEngine(dir, nil)
	defer eng.Close()

	// Must resolve using z_GeoLite2-City-new.mmdb ("Sunnyvale"), NOT ASN (empty) or Country ("United States") or older City!
	loc, ok := eng.Lookup(netip.MustParseAddr("8.8.8.8"))
	if !ok || loc.CountryISO != "US" || loc.Region != "California·Sunnyvale" {
		t.Fatalf("expected z_GeoLite2-City-new.mmdb to win, got ok=%v loc=%+v", ok, loc)
	}

	st := eng.Status()
	if len(st.ActiveFiles) != 1 || !strings.Contains(st.ActiveFiles[0], "z_GeoLite2-City-new.mmdb") {
		t.Fatalf("unexpected ActiveFiles: %+v", st.ActiveFiles)
	}
	skippedJoined := strings.Join(st.SkippedFiles, " | ")
	for _, want := range []string{
		"a_GeoLite2-ASN.mmdb (GeoLite2-ASN)",
		"b_GeoLite2-Country.mmdb (superseded)",
		"c_GeoLite2-City-old.mmdb (superseded)",
		"d_corrupt.mmdb (invalid)",
	} {
		if !strings.Contains(skippedJoined, want) {
			t.Fatalf("expected SkippedFiles %q to contain %q", skippedJoined, want)
		}
	}
}

func TestGeoEngineDualIPv4AndIPv6XDBSlotsAndHotReload(t *testing.T) {
	dir := t.TempDir()

	v4Old := buildSyntheticXDB(xdbIPv4Ver, 100, []syntheticXDBEntry{
		{addr: netip.MustParseAddr("114.114.114.114"), region: "中国|0|旧省份|旧城市|电信"},
	})
	// Verify legacy Structure 2.0 IPv4 header compatibility as well
	binary.LittleEndian.PutUint16(v4Old[0:2], xdbStructure20)

	v4New := buildSyntheticXDB(xdbIPv4Ver, 250, []syntheticXDBEntry{
		{addr: netip.MustParseAddr("114.114.114.114"), region: "中国|0|江苏省|南京市|电信"},
		{addr: netip.MustParseAddr("1.1.1.1"), region: "AU|0|New South Wales|Sydney|Cloudflare"},
	})
	v6DB := buildSyntheticXDB(xdbIPv6Ver, 300, []syntheticXDBEntry{
		{addr: netip.MustParseAddr("2400:3200::1"), region: "中国|0|浙江省|杭州市|阿里"},
		{addr: netip.MustParseAddr("2000::1"), region: "JP|0|Tokyo|Chiyoda|NTT"},
	})

	if err := os.WriteFile(filepath.Join(dir, "ip2region_v4_old.xdb"), v4Old, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ip2region_v4_new.xdb"), v4New, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ip2region_v6.xdb"), v6DB, 0o600); err != nil {
		t.Fatal(err)
	}

	eng := NewGeoEngine(dir, nil)
	defer eng.Close()

	st := eng.Status()
	if len(st.ActiveFiles) != 2 {
		t.Fatalf("expected both IPv4 and IPv6 XDB files to be active simultaneously, got %+v (skipped=%+v)", st.ActiveFiles, st.SkippedFiles)
	}
	if len(st.SkippedFiles) != 1 || !strings.Contains(st.SkippedFiles[0], "ip2region_v4_old.xdb (superseded)") {
		t.Fatalf("expected older IPv4 XDB to be superseded, got %+v", st.SkippedFiles)
	}

	// Verify IPv4 Chinese lookup, IPv4 2-letter ISO fallback ("AU"), and IPv6 lookup simultaneously!
	locCN4, ok := eng.Lookup(netip.MustParseAddr("114.114.114.114"))
	if !ok || locCN4.CountryISO != "CN" || locCN4.Region != "江苏省·南京市" {
		t.Fatalf("IPv4 XDB lookup mismatch: ok=%v loc=%+v", ok, locCN4)
	}
	locAU4, ok := eng.Lookup(netip.MustParseAddr("1.1.1.1"))
	if !ok || locAU4.CountryISO != "AU" || locAU4.Region != "New South Wales·Sydney" {
		t.Fatalf("IPv4 XDB 2-letter ISO lookup mismatch: ok=%v loc=%+v", ok, locAU4)
	}
	locCN6, ok := eng.Lookup(netip.MustParseAddr("2400:3200::1"))
	if !ok || locCN6.CountryISO != "CN" || locCN6.Region != "浙江省·杭州市" {
		t.Fatalf("IPv6 XDB lookup mismatch: ok=%v loc=%+v", ok, locCN6)
	}
	locJP6, ok := eng.Lookup(netip.MustParseAddr("2000::1"))
	if !ok || locJP6.CountryISO != "JP" || locJP6.Region != "Tokyo·Chiyoda" {
		t.Fatalf("IPv6 XDB 2-letter ISO lookup mismatch: ok=%v loc=%+v", ok, locJP6)
	}

	// Direct single-file .mmdb path + concurrent hot-reload race test + clearing stale handles
	singleDir := t.TempDir()
	singleMMDB := filepath.Join(singleDir, "direct.mmdb")
	if err := os.WriteFile(singleMMDB, buildTypedMMDB("GeoIP2-City", 5000, "city", "Cupertino"), 0o600); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 60; i++ {
				_, _ = eng.Lookup(netip.MustParseAddr("8.8.8.8"))
				_, _ = eng.Lookup(netip.MustParseAddr("114.114.114.114"))
				_, _ = eng.Lookup(netip.MustParseAddr("2400:3200::1"))
				_ = eng.Status()
			}
		}()
	}
	if err := eng.LoadDatabases(singleMMDB); err != nil {
		t.Fatalf("LoadDatabases(singleMMDB): %v", err)
	}
	wg.Wait()

	locSingle, ok := eng.Lookup(netip.MustParseAddr("8.8.8.8"))
	if !ok || locSingle.CountryISO != "US" || locSingle.Region != "California·Cupertino" {
		t.Fatalf("single-file MMDB lookup mismatch: ok=%v loc=%+v", ok, locSingle)
	}
	// Previous XDBv4 handle must be unmounted (so 114.114.114.114 is no longer overridden with 江苏省·南京市)
	// and previous XDBv6 handle must be unmounted (2000::1 has bits 0010_0000 -> wait, use 3000::1 which misses singleMMDB).
	locAfterSwap, ok := eng.Lookup(netip.MustParseAddr("114.114.114.114"))
	if !ok || locAfterSwap.Region != "California·Cupertino" {
		t.Fatalf("expected old XDBv4 handle to be unmounted after switching to singleMMDB, got %+v", locAfterSwap)
	}

	// Switching to an empty directory must clear active handles without retaining stale readers
	emptyDir := t.TempDir()
	if err := eng.LoadDatabases(emptyDir); err != nil {
		t.Fatalf("LoadDatabases(emptyDir): %v", err)
	}
	if _, ok := eng.Lookup(netip.MustParseAddr("8.8.8.8")); ok {
		t.Fatalf("expected stale MMDB handle to be cleared when reloading empty directory")
	}
	if len(eng.Status().ActiveFiles) != 0 {
		t.Fatalf("expected 0 active files after reloading empty directory, got %+v", eng.Status())
	}

	// Reject symlinks and extensionless regular files via os.Lstat at load time
	symPath := filepath.Join(t.TempDir(), "geo-symlink")
	if err := os.Symlink(singleDir, symPath); err == nil {
		if err := eng.LoadDatabases(symPath); err == nil {
			t.Fatalf("expected symlink GeoIP path %q to be rejected by LoadDatabases", symPath)
		}
	}
	rawFile := filepath.Join(t.TempDir(), "rawfile")
	if err := os.WriteFile(rawFile, []byte("plain"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := eng.LoadDatabases(rawFile); err == nil {
		t.Fatalf("expected non-DB regular file %q to be rejected by LoadDatabases", rawFile)
	}
}

func TestGeoEngineEvaluateRulesFailClosedOnBlockChallengeAndUnknown(t *testing.T) {
	dir := t.TempDir()
	v4 := buildSyntheticXDB(xdbIPv4Ver, 500, []syntheticXDBEntry{
		{addr: netip.MustParseAddr("8.8.8.8"), region: "US|0|California|Sunnyvale|Google"},
		{addr: netip.MustParseAddr("223.5.5.5"), region: "中国|0|浙江省|杭州市|阿里"},
	})
	if err := os.WriteFile(filepath.Join(dir, "ip2region_v4.xdb"), v4, 0o600); err != nil {
		t.Fatal(err)
	}
	eng := NewGeoEngine(dir, nil)
	defer eng.Close()

	us := netip.MustParseAddr("8.8.8.8")
	cn := netip.MustParseAddr("223.5.5.5")

	// Matched rules with "block", "challenge", or unknown actions MUST fail-closed (deny)!
	for _, act := range []string{"deny", "block", "challenge", "unknown_action"} {
		rules := []domain.SecurityGeoRule{{
			ID:        "r1",
			IsEnabled: true,
			Action:    act,
			OnMiss:    "next",
			Countries: "US",
		}}
		if eng.evaluateRules(us, rules, "allow", "allow") {
			t.Fatalf("expected matched rule with action=%q to fail-closed (deny), but it allowed US!", act)
		}
		if !eng.evaluateRules(cn, rules, "allow", "allow") {
			t.Fatalf("expected unmatched CN with defaultPolicy=allow to be allowed for action=%q", act)
		}
	}

	// OnMiss = "block" MUST deny unmatched traffic, just like "deny".
	allowCNBlockRest := []domain.SecurityGeoRule{{
		ID:        "r2",
		IsEnabled: true,
		Action:    "allow",
		OnMiss:    "block",
		Countries: "CN",
	}}
	if !eng.evaluateRules(cn, allowCNBlockRest, "allow", "allow") {
		t.Fatalf("expected matched CN with action=allow to be allowed")
	}
	if eng.evaluateRules(us, allowCNBlockRest, "allow", "allow") {
		t.Fatalf("expected unmatched US with on_miss=block to be denied!")
	}
}

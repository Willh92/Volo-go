package server_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Willh92/Volo-go/internal/auth"
)

// buildTestServerMMDB builds a minimal 1-node IPv4 MMDB v2 binary where:
//   - first bit 0 (e.g. 8.8.8.8) -> US
//   - first bit 1 (e.g. 223.5.5.5) -> CN
//
// or, when dbType == "GeoLite2-ASN", stores non-geographic ASN metadata only.
func buildTestServerMMDB(dbType string) []byte {
	encStr := func(s string) []byte {
		b := []byte(s)
		if len(b) < 29 {
			return append([]byte{byte((2 << 5) | len(b))}, b...)
		}
		return append([]byte{byte((2 << 5) | 29), byte(len(b) - 29)}, b...)
	}

	var data bytes.Buffer
	var leftPtr, rightPtr uint32
	if strings.Contains(strings.ToLower(dbType), "asn") {
		leftPtr = uint32(1 + 16 + data.Len())
		data.WriteByte(byte((7 << 5) | 1))
		data.Write(encStr("autonomous_system_organization"))
		data.Write(encStr("GOOGLE"))
		rightPtr = leftPtr
	} else {
		leftPtr = uint32(1 + 16 + data.Len())
		data.WriteByte(byte((7 << 5) | 1))
		data.Write(encStr("country"))
		data.WriteByte(byte((7 << 5) | 1))
		data.Write(encStr("iso_code"))
		data.Write(encStr("US"))

		rightPtr = uint32(1 + 16 + data.Len())
		data.WriteByte(byte((7 << 5) | 1))
		data.Write(encStr("country"))
		data.WriteByte(byte((7 << 5) | 1))
		data.Write(encStr("iso_code"))
		data.Write(encStr("CN"))
	}

	var out bytes.Buffer
	out.Write([]byte{
		byte(leftPtr >> 16), byte(leftPtr >> 8), byte(leftPtr),
		byte(rightPtr >> 16), byte(rightPtr >> 8), byte(rightPtr),
	})
	out.Write(make([]byte, 16))
	out.Write(data.Bytes())
	out.WriteString("\xab\xcd\xefMaxMind.com")
	out.WriteByte(byte((7 << 5) | 5))
	out.Write(encStr("binary_format_major_version"))
	out.Write([]byte{byte((5 << 5) | 1), 2})
	out.Write(encStr("node_count"))
	out.Write([]byte{byte((6 << 5) | 1), 1})
	out.Write(encStr("record_size"))
	out.Write([]byte{byte((5 << 5) | 1), 24})
	out.Write(encStr("ip_version"))
	out.Write([]byte{byte((5 << 5) | 1), 4})
	out.Write(encStr("database_type"))
	out.Write(encStr(dbType))
	return out.Bytes()
}

func TestAdminGeoIPDynamicPathHotReloadAndSelfLockoutProtection(t *testing.T) {
	env := setupM5TestEnv(t, "/console")
	// Trust loopback proxy in test so X-Forwarded-For simulates real external visitor/admin IPs.
	env.cfg.TrustedProxies.Prefixes = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}

	setupResp, setupRaw := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/setup/complete", map[string]any{
		"site_title":     "Geo HotReload Studio",
		"site_subtitle":  "Zero Downtime",
		"default_locale": "zh-CN",
		"accent":         "teal",
		"admin_username": "geoadmin",
		"admin_email":    "geo@example.com",
		"admin_password": "Admin!Password#2026",
	}, nil, nil)
	if setupResp.StatusCode != http.StatusOK {
		t.Fatalf("setup complete failed: %d %s", setupResp.StatusCode, string(setupRaw))
	}
	adminCookies := setupResp.Cookies()
	var setupData struct {
		CSRFToken string `json:"csrf_token"`
	}
	_ = json.Unmarshal(setupRaw, &setupData)
	csrfHeaders := map[string]string{auth.CSRFHeaderName: setupData.CSRFToken}

	// 1. Unsafe GeoIP paths must be rejected with 400 Bad Request on both settings PATCH and /geo-rules/reload
	for _, badPath := range []string{
		"../outside",
		"/etc",
		env.cfg.GetUploadDir(),
		filepath.Join(env.cfg.DataDir, "volo.db"),
	} {
		r1, b1 := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/security", map[string]any{
			"geoip_dir": badPath,
		}, adminCookies, csrfHeaders)
		if r1.StatusCode != http.StatusBadRequest {
			t.Fatalf("PATCH settings/security expected 400 for %q, got %d: %s", badPath, r1.StatusCode, string(b1))
		}
		r2, b2 := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/security/geo-rules/reload", map[string]any{
			"geoip_dir": badPath,
		}, adminCookies, csrfHeaders)
		if r2.StatusCode != http.StatusBadRequest {
			t.Fatalf("POST geo-rules/reload expected 400 for %q, got %d: %s", badPath, r2.StatusCode, string(b2))
		}
	}

	// 2. Enable GeoIP policy + add "Deny US" rule while GeoIP directory is still empty
	customGeoDir := filepath.Join(t.TempDir(), "runtime-geoip")
	if err := os.MkdirAll(customGeoDir, 0o750); err != nil {
		t.Fatal(err)
	}

	pSecResp, pSecBody := doJSONReq(t, http.MethodPatch, env.ts.URL+"/api/admin/settings/security", map[string]any{
		"geo_policy_active":  true,
		"geo_default_action": "allow",
		"geoip_dir":          customGeoDir,
	}, adminCookies, csrfHeaders)
	if pSecResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH settings/security valid customGeoDir failed: %d %s", pSecResp.StatusCode, string(pSecBody))
	}

	ruleResp, ruleBody := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/security/geo-rules", map[string]any{
		"name":       "Block US",
		"is_enabled": true,
		"action":     "deny",
		"on_miss":    "next",
		"countries":  "US",
		"sort_order": 10,
	}, adminCookies, csrfHeaders)
	if ruleResp.StatusCode != http.StatusCreated && ruleResp.StatusCode != http.StatusOK {
		t.Fatalf("POST geo-rules failed: %d %s", ruleResp.StatusCode, string(ruleBody))
	}

	// Without .mmdb files on disk yet, 8.8.8.8 falls back to geo_default_action ("allow") -> 200 OK
	preResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/", nil, nil, map[string]string{
		"X-Forwarded-For": "8.8.8.8",
	})
	if preResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 8.8.8.8 to be allowed before DB is added, got %d", preResp.StatusCode)
	}

	// 3. Drop ASN + City MMDBs into customGeoDir on disk and trigger live hot-reload WITHOUT restarting server!
	if err := os.WriteFile(filepath.Join(customGeoDir, "a_GeoLite2-ASN.mmdb"), buildTestServerMMDB("GeoLite2-ASN"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(customGeoDir, "z_GeoLite2-City.mmdb"), buildTestServerMMDB("GeoLite2-City"), 0o600); err != nil {
		t.Fatal(err)
	}

	relResp, relBody := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/security/geo-rules/reload", map[string]any{}, adminCookies, csrfHeaders)
	if relResp.StatusCode != http.StatusOK {
		t.Fatalf("POST geo-rules/reload failed: %d %s", relResp.StatusCode, string(relBody))
	}
	var relData struct {
		OK                bool   `json:"ok"`
		EffectiveGeoIPDir string `json:"effective_geoip_dir"`
		GeoIPStatus       struct {
			ActiveFiles  []string `json:"active_files"`
			SkippedFiles []string `json:"skipped_files"`
		} `json:"geoip_status"`
	}
	if err := json.Unmarshal(relBody, &relData); err != nil {
		t.Fatalf("unmarshal reload response: %v", err)
	}
	if !relData.OK || relData.EffectiveGeoIPDir != customGeoDir {
		t.Fatalf("unexpected reload response: %s", string(relBody))
	}
	if len(relData.GeoIPStatus.ActiveFiles) != 1 || !strings.Contains(relData.GeoIPStatus.ActiveFiles[0], "z_GeoLite2-City.mmdb") {
		t.Fatalf("expected z_GeoLite2-City.mmdb active after hot-reload, got %+v", relData.GeoIPStatus)
	}
	if len(relData.GeoIPStatus.SkippedFiles) != 1 || !strings.Contains(relData.GeoIPStatus.SkippedFiles[0], "a_GeoLite2-ASN.mmdb") {
		t.Fatalf("expected a_GeoLite2-ASN.mmdb skipped after hot-reload, got %+v", relData.GeoIPStatus)
	}

	// Immediately without server restart: 8.8.8.8 (US) is blocked (403) while 223.5.5.5 (CN) is allowed (200)!
	usResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/", nil, nil, map[string]string{
		"X-Forwarded-For": "8.8.8.8",
	})
	if usResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 8.8.8.8 (US) to be 403 Forbidden immediately after hot-reload, got %d", usResp.StatusCode)
	}
	cnResp, _ := doJSONReq(t, http.MethodGet, env.ts.URL+"/", nil, nil, map[string]string{
		"X-Forwarded-For": "223.5.5.5",
	})
	if cnResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 223.5.5.5 (CN) to be 200 OK after hot-reload, got %d", cnResp.StatusCode)
	}

	// /api/admin/doctor must also report geoip_ready_ok
	docResp, docBody := doJSONReq(t, http.MethodGet, env.ts.URL+"/api/admin/doctor", nil, adminCookies, nil)
	if docResp.StatusCode != http.StatusOK || !strings.Contains(string(docBody), `"geoip_ready_ok"`) {
		t.Fatalf("expected /api/admin/doctor to report geoip_ready_ok, got %d %s", docResp.StatusCode, string(docBody))
	}

	// 4. Resetting geoip_dir to "" falls back to DefaultGeoIPDir (empty) -> 8.8.8.8 allowed again;
	// then verify anti-self-lockout refuses switching geoip_dir back to customGeoDir if admin's own IP is 8.8.8.8!
	resetResp, resetBody := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/security/geo-rules/reload", map[string]any{
		"geoip_dir": "",
	}, adminCookies, csrfHeaders)
	if resetResp.StatusCode != http.StatusOK {
		t.Fatalf("reset geoip_dir failed: %d %s", resetResp.StatusCode, string(resetBody))
	}
	if env.cfg.GetGeoIPDir() != env.cfg.GetDefaultGeoIPDir() {
		t.Fatalf("expected active GeoIPDir to reset to DefaultGeoIPDir %q, got %q", env.cfg.GetDefaultGeoIPDir(), env.cfg.GetGeoIPDir())
	}

	lockoutHeaders := map[string]string{
		auth.CSRFHeaderName: setupData.CSRFToken,
		"X-Forwarded-For":   "8.8.8.8",
	}
	lockoutResp, lockoutBody := doJSONReq(t, http.MethodPost, env.ts.URL+"/api/admin/security/geo-rules/reload", map[string]any{
		"geoip_dir": customGeoDir,
	}, adminCookies, lockoutHeaders)
	if lockoutResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected anti-self-lockout 400 when admin from 8.8.8.8 activates DB that blocks US, got %d: %s",
			lockoutResp.StatusCode, string(lockoutBody))
	}
	// Rollback must have restored empty DefaultGeoIPDir so admin is NOT locked out!
	if env.cfg.GetGeoIPDir() != env.cfg.GetDefaultGeoIPDir() {
		t.Fatalf("expected rollback to restore DefaultGeoIPDir after self-lockout refusal, got %q", env.cfg.GetGeoIPDir())
	}
}

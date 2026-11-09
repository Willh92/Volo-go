package adminui

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuildIndexCSPAndMemoryDedup(t *testing.T) {
	inlineScript := `(function(){document.documentElement.dataset.theme="light";})();`
	htmlDoc := `<!doctype html><html lang="zh-CN"><head><script>` + inlineScript + `</script><!--VOLO_BOOT--><script type="module" src="./assets/index.js"></script></head><body><div id="app"></div></body></html>`

	csp := buildIndexCSP(htmlDoc)
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Fatalf("expected script-src not to allow unsafe-inline, got %s", csp)
	}
	sum := sha256.Sum256([]byte(inlineScript))
	wantHash := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	if !strings.Contains(csp, wantHash) {
		t.Fatalf("expected CSP to include %s, got %s", wantHash, csp)
	}
	for _, wantFrame := range []string{"https://player.bilibili.com", "https://www.youtube-nocookie.com"} {
		if !strings.Contains(csp, wantFrame) {
			t.Fatalf("expected CSP frame-src to allow %s, got %s", wantFrame, csp)
		}
	}

	h := Default()
	for name, item := range h.assets {
		if len(item.gz) > 0 && len(item.raw) > 0 {
			t.Fatalf("asset %s retains duplicate raw (%d) and gz (%d) slices in memory", name, len(item.raw), len(item.gz))
		}
	}
	if h.indexHTML == "" {
		t.Fatalf("expected non-empty decompressed indexHTML in Default() handler")
	}

	totalGZ := h.TotalGzipBytes()
	if totalGZ <= 50*1024 || totalGZ >= 300*1024 {
		t.Fatalf("expected TotalGzipBytes in (50 KiB, 300 KiB), got %d bytes (%.1f KiB)", totalGZ, float64(totalGZ)/1024.0)
	}

	// Verify zh-CN boot locale injection
	req := httptest.NewRequest(http.MethodGet, "/console", nil)
	rec := httptest.NewRecorder()
	h.ServeMount(rec, req, "/console", BootConfig{
		Mode:            "console",
		AdminPath:       "/console",
		DefaultLocale:   "zh-CN",
		VendorCDNPrefix: "https://s4.zstatic.net/npm",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status %d", rec.Code)
	}
	bodyZH := rec.Body.String()
	if !strings.Contains(bodyZH, `<script id="volo-boot-locale" type="application/json" data-locale="zh-CN">`) {
		t.Fatalf("expected zh-CN boot locale script in rendered index.html")
	}
	gotCSP := rec.Header().Get("Content-Security-Policy")
	if strings.Contains(gotCSP, "script-src 'self' 'unsafe-inline'") {
		t.Fatalf("unexpected unsafe-inline in ServeMount script-src: %s", gotCSP)
	}
	for _, want := range []string{"https://cdn.jsdelivr.net", "https://s4.zstatic.net", "https://player.bilibili.com"} {
		if !strings.Contains(gotCSP, want) {
			t.Fatalf("expected ServeMount CSP to allow %s, got %s", want, gotCSP)
		}
	}

	// Verify non-Chinese locale (ja-JP) injects en-US boot locale synchronously
	reqJA := httptest.NewRequest(http.MethodGet, "/console", nil)
	recJA := httptest.NewRecorder()
	h.ServeMount(recJA, reqJA, "/console", BootConfig{
		Mode:          "console",
		AdminPath:     "/console",
		DefaultLocale: "ja-JP",
	})
	if !strings.Contains(recJA.Body.String(), `<script id="volo-boot-locale" type="application/json" data-locale="en-US">`) {
		t.Fatalf("expected en-US boot locale script for DefaultLocale=ja-JP")
	}

	// Verify lazy /console/i18n/en-US.json serving (both gzip and plain)
	reqI18nGZ := httptest.NewRequest(http.MethodGet, "/console/i18n/en-US.json", nil)
	reqI18nGZ.Header.Set("Accept-Encoding", "gzip")
	recI18nGZ := httptest.NewRecorder()
	h.ServeMount(recI18nGZ, reqI18nGZ, "/console", BootConfig{})
	if recI18nGZ.Code != http.StatusOK || recI18nGZ.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("expected gzipped 200 for /console/i18n/en-US.json, got %d enc=%q", recI18nGZ.Code, recI18nGZ.Header().Get("Content-Encoding"))
	}
	gr, err := gzip.NewReader(bytes.NewReader(recI18nGZ.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	decBytes, err := io.ReadAll(gr)
	_ = gr.Close()
	if err != nil {
		t.Fatalf("read gunzipped i18n JSON: %v", err)
	}
	var enColumn []string
	if err := json.Unmarshal(decBytes, &enColumn); err != nil {
		t.Fatalf("unmarshal en-US.json column: %v", err)
	}
	foundPublicURL := false
	for _, v := range enColumn {
		if v == "Public Site URL" {
			foundPublicURL = true
			break
		}
	}
	if len(enColumn) < 1500 || !foundPublicURL {
		t.Fatalf("expected >=1500 entries including 'Public Site URL' in embedded en-US.json, got len=%d found=%v", len(enColumn), foundPublicURL)
	}
}

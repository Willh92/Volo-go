package extpkg

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/security"
)

func buildSampleZip(t *testing.T, files map[string]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:])
}

func TestSemVerAndHostCompatibility(t *testing.T) {
	cases := []struct {
		a, b string
		want int
		ok   bool
	}{
		{"v1.0.0", "1.0.0", 0, true},
		{"1.0.0+20261115", "1.0.0+abc", 0, true},
		{"1.2", "1.2.0", 0, true},
		{"1.0.0-alpha.1", "1.0.0-alpha.2", -1, true},
		{"1.0.0-alpha.10", "1.0.0-alpha.2", 1, true},
		{"1.0.0-rc.1", "1.0.0", -1, true},
		{"2.0.4", "2.0.3", 1, true},
		{"not-semver", "1.0.0", 0, false},
	}
	for _, tc := range cases {
		got, ok := CompareSemVer(tc.a, tc.b)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("CompareSemVer(%q, %q) = (%d, %v), want (%d, %v)", tc.a, tc.b, got, ok, tc.want, tc.ok)
		}
	}

	if !IsHostCompatible("dev", "9.9.9") {
		t.Fatal("expected dev host version to be compatible")
	}
	if !IsHostCompatible("v1.0.2", "1.0.2") {
		t.Fatal("expected v1.0.2 >= 1.0.2")
	}
	if IsHostCompatible("v1.0.2", "1.1.0") {
		t.Fatal("expected v1.0.2 < 1.1.0 to be incompatible")
	}
}

func TestMetadataFetcherETagSingleflightAndStrategies(t *testing.T) {
	var hits atomic.Int32
	const etagVal = `"cat-v1"`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/catalog.json":
			hits.Add(1)
			time.Sleep(25 * time.Millisecond)
			if r.Header.Get("If-None-Match") == etagVal {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", etagVal)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{
				"repository": "Willh92/Volo-go",
				"plugins": [
					{
						"id": "sensitive-words",
						"version": "1.2.0",
						"min_host_version": "1.0.2",
						"release_tag": "plugin-sensitive-words-v1.2.0",
						"download_url": "http://%s/dl/sensitive-words.zip",
						"changelog": {"zh-CN": "词库性能优化", "en-US": "Trie optimization"}
					}
				]
			}`, r.Host)
		case "/gh-releases":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `[
				{"tag_name": "v1.0.2", "html_url": "https://example.com/core", "assets": []},
				{
					"tag_name": "plugin-demo-v2.3.0",
					"html_url": "https://example.com/rel/2.3.0",
					"body": "Fix memory leak",
					"assets": [
						{
							"name": "plugin-demo.zip",
							"browser_download_url": "http://%s/dl/plugin-demo.zip",
							"digest": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
						}
					]
				}
			]`, r.Host)
		case "/oversize.json":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(bytes.Repeat([]byte("a"), maxMetadataBodyBytes+10))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dialer := &security.OutboundSafeDialer{AllowLoopbackForTests: true, Timeout: 3 * time.Second}
	fetcher := NewMetadataFetcher()

	// 1. Concurrent singleflight coalescing: 8 concurrent callers -> 1 HTTP GET
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := fetcher.CheckDeclarative(context.Background(), dialer, "sensitive-words", UpdateConfig{
				Strategy: StrategyVoloCatalog,
				URL:      srv.URL + "/catalog.json",
			}, "zh-CN")
			if err != nil {
				t.Errorf("CheckDeclarative failed: %v", err)
				return
			}
			if res.LatestVersion != "1.2.0" || res.Changelog != "词库性能优化" {
				t.Errorf("unexpected result: %+v", res)
			}
		}()
	}
	wg.Wait()
	if got := hits.Load(); got != 1 {
		t.Fatalf("expected singleflight to coalesce into 1 hit, got %d", got)
	}

	// 2. Subsequent check sends If-None-Match and receives 304 Not Modified cleanly
	res2, err := fetcher.CheckDeclarative(context.Background(), dialer, "sensitive-words", UpdateConfig{
		Strategy: StrategyVoloCatalog,
		URL:      srv.URL + "/catalog.json",
	}, "en-US")
	if err != nil {
		t.Fatalf("304 check failed: %v", err)
	}
	if res2.Changelog != "Trie optimization" {
		t.Fatalf("expected en-US changelog from cached 304 body, got %q", res2.Changelog)
	}

	// 3. Capability normalization: check_only strips download_url and marks OnlineUpdatable=false
	stCheckOnly, err := NormalizeUpdateStatus("1.1.0", "v1.0.2", CapabilityCheckOnly, "", true, res2, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !stCheckOnly.HasUpdate || stCheckOnly.OnlineUpdatable || stCheckOnly.DownloadURL != "" {
		t.Fatalf("expected check_only to suppress online update, got %+v", stCheckOnly)
	}

	// 4. GitHub Release list with tag_prefix, asset_pattern, and sha256 digest extraction
	ghRes, err := fetcher.CheckDeclarative(context.Background(), dialer, "demo", UpdateConfig{
		Strategy:     StrategyGitHubRelease,
		URL:          srv.URL + "/gh-releases",
		TagPrefix:    "plugin-demo-v",
		AssetPattern: "plugin-{id}.zip",
	}, "en-US")
	if err != nil {
		t.Fatalf("github-release check failed: %v", err)
	}
	if ghRes.LatestVersion != "2.3.0" || ghRes.SHA256 != "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" {
		t.Fatalf("unexpected ghRes: %+v", ghRes)
	}

	// 5. Oversize metadata body (> 128 KiB) rejected
	if _, err := fetcher.FetchRaw(context.Background(), dialer, srv.URL+"/oversize.json"); err == nil {
		t.Fatal("expected oversize metadata response to fail")
	}
}

func TestStreamVerifyZipSuccessAnomaliesCancellationAndSSRF(t *testing.T) {
	zipBytes, zipSHA := buildSampleZip(t, map[string]string{
		"manifest.json": `{"id":"demo"}`,
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect-zip":
			http.Redirect(w, r, "/direct.zip", http.StatusFound)
		case "/direct.zip":
			w.Header().Set("Content-Type", "application/zip")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(zipBytes)
		case "/webpage":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<!doctype html><html><body>Release page</body></html>"))
		case "/truncated.zip":
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(zipBytes)+500))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(zipBytes[:10])
		case "/stall.zip":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(zipBytes[:4])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done()
		case "/slow-cancel.zip":
			w.WriteHeader(http.StatusOK)
			for i := 0; i < 20; i++ {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(30 * time.Millisecond):
					_, _ = w.Write(bytes.Repeat([]byte("P"), 1024))
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
				}
			}
		case "/ssrf-redirect":
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	tempDir := t.TempDir()
	dialer := &security.OutboundSafeDialer{AllowLoopbackForTests: true, Timeout: 2 * time.Second}

	assertTempClean := func() {
		t.Helper()
		entries, err := os.ReadDir(tempDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("expected tempDir to have 0 leftover files, found %d", len(entries))
		}
	}

	// 1. Redirect + SHA-256 verified streaming download
	dl, err := StreamVerifyZip(context.Background(), dialer, DownloadOptions{
		URL:            srv.URL + "/redirect-zip",
		ExpectedSHA256: "sha256:" + zipSHA,
		TempDir:        tempDir,
		MaxBytes:       1 << 20,
	})
	if err != nil {
		t.Fatalf("StreamVerifyZip valid failed: %v", err)
	}
	if dl.Size != int64(len(zipBytes)) || dl.SHA256 != zipSHA {
		t.Fatalf("unexpected DownloadedZip metadata: %+v", dl)
	}
	dl.Cleanup()
	assertTempClean()

	// 2. Checksum mismatch rejected & cleaned up
	_, err = StreamVerifyZip(context.Background(), dialer, DownloadOptions{
		URL:            srv.URL + "/direct.zip",
		ExpectedSHA256: "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		TempDir:        tempDir,
		MaxBytes:       1 << 20,
	})
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("expected ErrChecksumMismatch, got %v", err)
	}
	assertTempClean()

	// 3. HTML webpage mistaken for .zip rejected with ErrNotZipArchive
	_, err = StreamVerifyZip(context.Background(), dialer, DownloadOptions{
		URL:      srv.URL + "/webpage",
		TempDir:  tempDir,
		MaxBytes: 1 << 20,
	})
	if !errors.Is(err, ErrNotZipArchive) {
		t.Fatalf("expected ErrNotZipArchive, got %v", err)
	}
	assertTempClean()

	// 4. Truncated stream rejected & cleaned up
	if _, err := StreamVerifyZip(context.Background(), dialer, DownloadOptions{
		URL:      srv.URL + "/truncated.zip",
		TempDir:  tempDir,
		MaxBytes: 1 << 20,
	}); err == nil {
		t.Fatal("expected truncated stream to fail")
	}
	assertTempClean()

	// 5. Stall watchdog aborts stalled TCP stream promptly
	_, err = StreamVerifyZip(context.Background(), dialer, DownloadOptions{
		URL:          srv.URL + "/stall.zip",
		TempDir:      tempDir,
		MaxBytes:     1 << 20,
		StallTimeout: 80 * time.Millisecond,
	})
	if !errors.Is(err, ErrDownloadStalled) {
		t.Fatalf("expected ErrDownloadStalled, got %v", err)
	}
	assertTempClean()

	// 6. User/Admin cancellation mid-download aborts immediately and cleans temp file
	cancelCtx, cancel := context.WithTimeout(context.Background(), 70*time.Millisecond)
	defer cancel()
	_, err = StreamVerifyZip(cancelCtx, dialer, DownloadOptions{
		URL:      srv.URL + "/slow-cancel.zip",
		TempDir:  tempDir,
		MaxBytes: 1 << 20,
	})
	if !errors.Is(err, ErrDownloadCanceled) {
		t.Fatalf("expected ErrDownloadCanceled, got %v", err)
	}
	assertTempClean()

	// 7. Redirect to cloud metadata IP blocked by SSRF firewall
	_, err = StreamVerifyZip(context.Background(), dialer, DownloadOptions{
		URL:      srv.URL + "/ssrf-redirect",
		TempDir:  tempDir,
		MaxBytes: 1 << 20,
	})
	if !errors.Is(err, security.ErrSSRFBlocked) {
		t.Fatalf("expected ErrSSRFBlocked on metadata redirect, got %v", err)
	}
	assertTempClean()
}

func TestSanitizeHTTPURLAndSanitizeZipPath(t *testing.T) {
	if got := SanitizeHTTPURL("  https://github.com/Willh92/Volo-go  "); got != "https://github.com/Willh92/Volo-go" {
		t.Fatalf("unexpected valid SanitizeHTTPURL: %q", got)
	}
	for _, bad := range []string{
		"",
		"   ",
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"https://example.com/path\nHeader: evil",
		"https://example.com/\x7f",
		"https:///missing-host",
		"http://user:pass@example.com/secret",
		"http://[fe80::1%25eth0]/",
	} {
		if got := SanitizeHTTPURL(bad); got != "" {
			t.Fatalf("expected SanitizeHTTPURL(%q) == \"\", got %q", bad, got)
		}
	}

	if _, err := ValidatePublicHTTPURL("http://[64:ff9b::a9fe:a9fe%25eth0]/", false); !errors.Is(err, security.ErrSSRFBlocked) {
		t.Fatalf("expected IPv6 zone ID to fail ValidatePublicHTTPURL with ErrSSRFBlocked, got %v", err)
	}
	if _, err := ValidatePublicHTTPURL("http://local/", false); !errors.Is(err, security.ErrSSRFBlocked) {
		t.Fatalf("expected 'local' hostname to fail ValidatePublicHTTPURL, got %v", err)
	}

	goodZipPaths := map[string]string{
		"manifest.json":                  "manifest.json",
		"kanbanniang/assets/widget.js":   "kanbanniang/assets/widget.js",
		"volo-journal/templates/":        "volo-journal/templates",
		"__MACOSX/._theme.json":          "__MACOSX/._theme.json",
		"assets/con-artist.png":          "assets/con-artist.png",
		"assets/com10.png":               "assets/com10.png",
	}
	for in, want := range goodZipPaths {
		got, err := SanitizeZipPath(in)
		if err != nil || got != want {
			t.Fatalf("SanitizeZipPath(%q) = (%q, %v), want (%q, nil)", in, got, err, want)
		}
	}

	badZipPaths := []string{
		"",
		"/",
		"/etc/passwd",
		`kanbanniang\main.js`,
		"../secret.json",
		"a/../b.js",
		"./main.js",
		"a//b.js",
		"a/file.",
		"a/file ",
		"a/dir. /file.js",
		"CON",
		"con.txt",
		"sub/NUL.json",
		"sub/LPT1.html",
		"sub/com9.js",
		"C:evil.js",
		"a<b>c.js",
		"bad\x00name.js",
	}
	for _, bad := range badZipPaths {
		if got, err := SanitizeZipPath(bad); err == nil {
			t.Fatalf("expected SanitizeZipPath(%q) to fail, got %q", bad, got)
		}
	}
}

func TestSlowlorisMaxDurationDownloadRawFileSingleflightAndChecksums(t *testing.T) {
	rawJSON := []byte(`{"app.title":"Volo 繁體"}`)
	sum := sha256.Sum256(rawJSON)
	rawSHA := hex.EncodeToString(sum[:])

	var emitETag atomic.Bool
	emitETag.Store(true)
	var metaHits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/drip.zip":
			w.Header().Set("Content-Type", "application/zip")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("PK\x03\x04"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			ticker := time.NewTicker(25 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-r.Context().Done():
					return
				case <-ticker.C:
					_, _ = w.Write([]byte{0x00})
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
				}
			}
		case "/zh-TW.json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(rawJSON)
		case "/SHA256SUMS.txt":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = fmt.Fprintf(w, "# Official checksums\n%s  *dist/zh-TW.json\n%s  plugin-kanbanniang-v2.0.5.zip\n", strings.ToUpper(rawSHA), rawSHA)
		case "/coalesce-meta.json":
			metaHits.Add(1)
			time.Sleep(80 * time.Millisecond)
			if emitETag.Load() {
				if r.Header.Get("If-None-Match") == `"v1"` {
					w.WriteHeader(http.StatusNotModified)
					return
				}
				w.Header().Set("ETag", `"v1"`)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dialer := &security.OutboundSafeDialer{AllowLoopbackForTests: true}
	tmpDir := t.TempDir()

	// 1. Slowloris drip stream (sends 1 byte every 25ms < StallTimeout 200ms) must fail with ErrDownloadTimeout (NOT ErrDownloadCanceled) when MaxDuration expires!
	_, err := StreamVerifyZip(context.Background(), dialer, DownloadOptions{
		URL:          srv.URL + "/drip.zip",
		TempDir:      tmpDir,
		StallTimeout: 200 * time.Millisecond,
		MaxDuration:  90 * time.Millisecond,
	})
	if !errors.Is(err, ErrDownloadTimeout) || errors.Is(err, ErrDownloadCanceled) {
		t.Fatalf("expected ErrDownloadTimeout (not ErrDownloadCanceled) on Slowloris drip, got %v", err)
	}

	// 2. DownloadRawFile verifies SHA-256 on non-zip JSON payload cleanly.
	rawDL, err := DownloadRawFile(context.Background(), dialer, DownloadOptions{
		URL:            srv.URL + "/zh-TW.json",
		ExpectedSHA256: rawSHA,
		TempDir:        tmpDir,
	})
	if err != nil {
		t.Fatalf("DownloadRawFile failed: %v", err)
	}
	gotBytes, _ := io.ReadAll(rawDL.File)
	rawDL.Cleanup()
	if !bytes.Equal(gotBytes, rawJSON) {
		t.Fatalf("unexpected raw file bytes: %q", string(gotBytes))
	}

	// 3. ResolveCatalogChecksum matches both path and URL basename candidates and normalizes lowercase hex.
	fetcher := NewMetadataFetcher()
	resolvedSum, err := ResolveCatalogChecksum(
		context.Background(),
		fetcher,
		dialer,
		srv.URL+"/SHA256SUMS.txt",
		"https://github.com/Willh92/Volo-go/releases/download/v1/zh-TW.json",
	)
	if err != nil || resolvedSum != rawSHA {
		t.Fatalf("ResolveCatalogChecksum = (%q, %v), want (%q, nil)", resolvedSum, err, rawSHA)
	}

	// 4. MetadataFetcher singleflight leader isolation: impatient caller 1 canceling at 15ms must NOT abort concurrent caller 2!
	impatientCtx, cancel1 := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel1()
	patientCtx, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()

	errCh1 := make(chan error, 1)
	errCh2 := make(chan error, 1)
	go func() {
		_, e := fetcher.FetchRaw(impatientCtx, dialer, srv.URL+"/coalesce-meta.json")
		errCh1 <- e
	}()
	time.Sleep(5 * time.Millisecond)
	go func() {
		_, e := fetcher.FetchRaw(patientCtx, dialer, srv.URL+"/coalesce-meta.json")
		errCh2 <- e
	}()

	if e1 := <-errCh1; !errors.Is(e1, context.DeadlineExceeded) {
		t.Fatalf("expected impatient caller to get DeadlineExceeded, got %v", e1)
	}
	if e2 := <-errCh2; e2 != nil {
		t.Fatalf("expected patient concurrent caller to succeed despite leader caller cancellation, got %v", e2)
	}
	if metaHits.Load() != 1 {
		t.Fatalf("expected coalesced singleflight to hit server once, got %d", metaHits.Load())
	}

	// Subsequent 200 OK without ETag/Last-Modified must evict the stale cache entry!
	emitETag.Store(false)
	if _, err := fetcher.FetchRaw(context.Background(), dialer, srv.URL+"/coalesce-meta.json"); err != nil {
		t.Fatalf("FetchRaw without ETag failed: %v", err)
	}
	fetcher.mu.Lock()
	_, stillCached := fetcher.cache[srv.URL+"/coalesce-meta.json"]
	fetcher.mu.Unlock()
	if stillCached {
		t.Fatal("expected stale cache entry to be evicted when 200 OK omits ETag and Last-Modified")
	}
}

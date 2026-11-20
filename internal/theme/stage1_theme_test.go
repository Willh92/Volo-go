package theme

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/extpkg"
	"github.com/Willh92/Volo-go/internal/i18n"
)

type mockTestSealer struct {
	key string
}

func (m mockTestSealer) SealString(plaintext string, aad string) (string, error) {
	return "enc:v1:" + m.key + ":" + aad + ":" + plaintext, nil
}

func (m mockTestSealer) OpenString(envelope string, aad string) (string, error) {
	prefix := "enc:v1:" + m.key + ":" + aad + ":"
	if !strings.HasPrefix(envelope, prefix) {
		return "", errors.New("cipher: message authentication failed")
	}
	return strings.TrimPrefix(envelope, prefix), nil
}

func TestStage1_ThemeJaJPCoverage_HomepageSanitize_PreviewCache_And_RedactSecret(t *testing.T) {
	cat, err := i18n.NewCatalog("ja-JP", "")
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	repoThemesDir := filepath.Clean(filepath.Join("..", "..", "themes"))
	eng, err := NewEngine(cat, repoThemesDir)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	// 1. Verify ja-JP localization for built-in volo-studio & external volo-journal v1.1.2
	for _, id := range []string{DefaultThemeID, "volo-journal"} {
		lt, ok := eng.GetLocalized(id, "ja-JP")
		if !ok {
			t.Fatalf("expected theme %q to be loaded", id)
		}
		if id == "volo-journal" && lt.Version != "1.1.2" {
			t.Fatalf("expected volo-journal version 1.1.2, got %s", lt.Version)
		}
		if !strings.Contains(lt.PreviewURL, "?v="+lt.Version) {
			t.Fatalf("expected PreviewURL to include ?v=%s, got %s", lt.Version, lt.PreviewURL)
		}
		if lt.Name["ja-JP"] == "" || lt.DisplayName != lt.Name["ja-JP"] {
			t.Fatalf("theme %s missing ja-JP name: %+v", id, lt.Name)
		}
		if lt.Description["ja-JP"] == "" || lt.DisplayDescription != lt.Description["ja-JP"] {
			t.Fatalf("theme %s missing ja-JP description: %+v", id, lt.Description)
		}
		if len(lt.PageTemplates) == 0 {
			t.Fatalf("theme %s missing page_templates", id)
		}
		for _, pt := range lt.PageTemplates {
			if strings.TrimSpace(pt.Label["ja-JP"]) == "" {
				t.Fatalf("theme %s page_template %s missing ja-JP label", id, pt.Value)
			}
		}
		for _, f := range lt.ConfigSchema {
			if strings.TrimSpace(f.Label["ja-JP"]) == "" {
				t.Fatalf("theme %s field %s missing ja-JP label", id, f.Key)
			}
			if len(f.Description) > 0 && strings.TrimSpace(f.Description["ja-JP"]) == "" {
				t.Fatalf("theme %s field %s missing ja-JP description", id, f.Key)
			}
			if len(f.Placeholder) > 0 && strings.TrimSpace(f.Placeholder["ja-JP"]) == "" {
				t.Fatalf("theme %s field %s missing ja-JP placeholder", id, f.Key)
			}
			for _, opt := range f.Options {
				if strings.TrimSpace(opt.Label["ja-JP"]) == "" {
					t.Fatalf("theme %s field %s option %s missing ja-JP label", id, f.Key, opt.Value)
				}
			}
		}
	}

	// 2. Verify Homepage XSS sanitization, Zip path/mode hardening, ServePreview ETag/Cache-Control, and RedactThemeConfig key mismatch
	tmpThemes := t.TempDir()
	eng2, err := NewEngine(cat, tmpThemes)
	if err != nil {
		t.Fatalf("NewEngine tmp: %v", err)
	}

	xssThemeZip, _ := buildThemeZipBytes(t, map[string]string{
		"theme.json": `{
			"id": "xss-sec-skin",
			"version": "1.0.0",
			"homepage": "javascript:alert(1)",
			"name": {"en-US": "XSS Skin"},
			"description": {"en-US": "Desc"},
			"config_schema": [
				{"key": "api_token", "type": "secret", "label": {"en-US": "API Token"}},
				{"key": "title", "type": "text", "label": {"en-US": "Title"}}
			]
		}`,
		"templates/index.html": `{{define "content"}}ok{{end}}`,
	})
	mf, err := eng2.InstallZip(bytes.NewReader(xssThemeZip), int64(len(xssThemeZip)), false)
	if err != nil {
		t.Fatalf("InstallZip xss-sec-skin: %v", err)
	}
	if mf.Homepage != "" {
		t.Fatalf("expected javascript: homepage to be sanitized to empty string, got %q", mf.Homepage)
	}

	// Reject Windows reserved device name & non-regular entry in zip
	var badZipBuf bytes.Buffer
	zw := zip.NewWriter(&badZipBuf)
	hdr := &zip.FileHeader{Name: "CON.json", Method: zip.Deflate}
	hdr.SetMode(0o644)
	fw, _ := zw.CreateHeader(hdr)
	_, _ = fw.Write([]byte("{}"))
	_ = zw.Close()
	if _, err := eng2.InstallZip(bytes.NewReader(badZipBuf.Bytes()), int64(badZipBuf.Len()), true); err == nil {
		t.Fatal("expected reserved Windows device name CON.json in theme zip to be rejected")
	}

	var fifoZipBuf bytes.Buffer
	zw2 := zip.NewWriter(&fifoZipBuf)
	hdr1 := &zip.FileHeader{Name: "theme.json", Method: zip.Deflate}
	hdr1.SetMode(0o644)
	fw1, _ := zw2.CreateHeader(hdr1)
	_, _ = fw1.Write([]byte(`{"id":"fifo-skin","version":"1.0.0","name":{"en-US":"F"}}`))
	hdr2 := &zip.FileHeader{Name: "templates/pipe.html", Method: zip.Deflate}
	hdr2.SetMode(os.ModeNamedPipe | 0o644)
	_, _ = zw2.CreateHeader(hdr2)
	_ = zw2.Close()
	if _, err := eng2.InstallZip(bytes.NewReader(fifoZipBuf.Bytes()), int64(fifoZipBuf.Len()), true); err == nil {
		t.Fatal("expected non-regular FIFO entry in theme zip to be rejected")
	}

	// ServePreview without ?v= vs with ?v= + If-None-Match 304 (generated SVG and embedded custom preview)
	for _, tid := range []string{"xss-sec-skin", DefaultThemeID} {
		reqNoV := httptest.NewRequest(http.MethodGet, "/themes/"+tid+"/preview", nil)
		recNoV := httptest.NewRecorder()
		eng2.ServePreview(recNoV, reqNoV, tid)
		if recNoV.Code != http.StatusOK {
			t.Fatalf("ServePreview(%s) status=%d", tid, recNoV.Code)
		}
		if cc := recNoV.Header().Get("Cache-Control"); cc != "public, max-age=300, must-revalidate" {
			t.Fatalf("ServePreview(%s) no ?v= Cache-Control=%q", tid, cc)
		}
		etag := recNoV.Header().Get("ETag")
		if etag == "" || !strings.Contains(etag, "1.0.0") && !strings.Contains(etag, "1.1.0") {
			t.Fatalf("expected versioned ETag for %s, got %q", tid, etag)
		}

		reqWithV := httptest.NewRequest(http.MethodGet, "/themes/"+tid+"/preview?v=1.0.0", nil)
		recWithV := httptest.NewRecorder()
		eng2.ServePreview(recWithV, reqWithV, tid)
		if cc := recWithV.Header().Get("Cache-Control"); cc != "public, max-age=3600" {
			t.Fatalf("ServePreview(%s) with ?v= Cache-Control=%q", tid, cc)
		}

		req304 := httptest.NewRequest(http.MethodGet, "/themes/"+tid+"/preview?v=1.0.0", nil)
		req304.Header.Set("If-None-Match", etag)
		rec304 := httptest.NewRecorder()
		eng2.ServePreview(rec304, req304, tid)
		if rec304.Code != http.StatusNotModified {
			t.Fatalf("expected 304 Not Modified for %s, got %d", tid, rec304.Code)
		}
	}

	// BKP-3: RedactThemeConfig returns SecretMask when decryptable, and "" when key changed
	sealerA := mockTestSealer{key: "key-A"}
	sealerB := mockTestSealer{key: "key-B"}
	eng2.SetSecretSealer(sealerA)
	merged := eng2.MergeThemeConfigSecrets("xss-sec-skin", map[string]any{
		"api_token": "super-secret-value",
		"title":     "Hello",
	}, nil)
	redactedOK := eng2.RedactThemeConfig("xss-sec-skin", merged)
	if redactedOK["api_token"] != SecretMask {
		t.Fatalf("expected SecretMask under matching key, got %q", redactedOK["api_token"])
	}
	eng2.SetSecretSealer(sealerB)
	redactedMismatch := eng2.RedactThemeConfig("xss-sec-skin", merged)
	if redactedMismatch["api_token"] != "" {
		t.Fatalf("expected empty string under mismatched key (BKP-3), got %q", redactedMismatch["api_token"])
	}

	// H-6: ExportZipContext honors canceled context
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	var expBuf bytes.Buffer
	if err := eng2.ExportZipContext(canceledCtx, DefaultThemeID, &expBuf); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled from ExportZipContext, got %v", err)
	}
}

func TestStage1_ThemeSingleflightCancel_ChecksumURL_And_DowngradeBlock(t *testing.T) {
	ctx := context.Background()
	cat, err := i18n.NewCatalog("en-US", "")
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	eng, err := NewEngine(cat, t.TempDir())
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	eng.SetHostVersion("1.2.0")
	eng.SetAllowLoopbackForTests(true)

	entered := make(chan struct{})
	release := make(chan struct{})
	var serveErr bool
	var mu sync.Mutex
	var v2Zip []byte
	var v2Sum string

	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/catalog.json":
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			mu.Lock()
			fail := serveErr
			mu.Unlock()
			if fail {
				http.Error(w, "upstream 502", http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{
				"themes": [{
					"id": "paper-pro",
					"version": "2.0.0",
					"min_host_version": "1.0.0",
					"download_url": "%s/theme-paper-pro-v2.0.0.zip",
					"checksum_url": "%s/SHA256SUMS.txt"
				}]
			}`, srvURL, srvURL)
		case "/SHA256SUMS.txt":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = fmt.Fprintf(w, "%s  theme-paper-pro-v2.0.0.zip\n", v2Sum)
		case "/theme-paper-pro-v2.0.0.zip":
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(v2Zip)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	makeZip := func(ver string) ([]byte, string) {
		raw, sum := buildThemeZipBytes(t, map[string]string{
			"theme.json": fmt.Sprintf(`{
				"id": "paper-pro",
				"version": %q,
				"min_host_version": "1.0.0",
				"name": {"en-US": "Paper Pro"},
				"description": {"en-US": "Test"},
				"update": {
					"strategy": "volo-catalog",
					"url": "%s/catalog.json"
				}
			}`, ver, srvURL),
			"templates/index.html": fmt.Sprintf(`{{define "content"}}v%s{{end}}`, ver),
		})
		return raw, sum
	}

	v15Zip, _ := makeZip("1.5.0")
	v2Zip, v2Sum = makeZip("2.0.0")
	if _, err := eng.InstallZip(bytes.NewReader(v15Zip), int64(len(v15Zip)), false); err != nil {
		t.Fatalf("InstallZip v1.5.0: %v", err)
	}

	// 1. Singleflight leader survives caller1 cancellation so caller2 succeeds!
	c1Ctx, c1Cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	var st2 extpkg.UpdateStatus
	var err1, err2 error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err1 = eng.CheckThemeUpdate(c1Ctx, "paper-pro", true, "en-US")
	}()
	<-entered
	go func() {
		defer wg.Done()
		st2, err2 = eng.CheckThemeUpdate(ctx, "paper-pro", false, "en-US")
	}()
	time.Sleep(20 * time.Millisecond)
	c1Cancel()
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if !errors.Is(err1, context.Canceled) {
		t.Fatalf("expected caller1 context.Canceled, got %v", err1)
	}
	if err2 != nil || !st2.HasUpdate || st2.ChecksumURL == "" {
		t.Fatalf("expected caller2 to succeed with ChecksumURL, err=%v st=%+v", err2, st2)
	}

	// 2. Canceled ctx must NOT populate StateError cache
	eng.invalidateUpdateStatus("paper-pro")
	release = make(chan struct{})
	mu.Lock()
	serveErr = true
	mu.Unlock()
	c3Ctx, c3Cancel := context.WithCancel(ctx)
	doneCh := make(chan error, 1)
	go func() {
		_, cErr := eng.CheckThemeUpdate(c3Ctx, "paper-pro", true, "en-US")
		doneCh <- cErr
	}()
	<-entered
	c3Cancel()
	close(release)
	if cErr := <-doneCh; !errors.Is(cErr, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", cErr)
	}
	time.Sleep(30 * time.Millisecond)
	if cached := eng.getCachedUpdateStatus("paper-pro", "1.5.0"); cached != nil && cached.State == extpkg.StateError {
		t.Fatalf("expected StateError NOT to be cached when caller ctx was canceled, got %+v", cached)
	}

	// 3. UpgradeTheme auto-resolves ChecksumURL (SHA256SUMS.txt) and upgrades 1.5.0 -> 2.0.0
	mu.Lock()
	serveErr = false
	mu.Unlock()
	release = make(chan struct{})
	close(release)
	if _, err := eng.CheckThemeUpdate(ctx, "paper-pro", true, "en-US"); err != nil {
		t.Fatalf("CheckThemeUpdate before upgrade: %v", err)
	}
	upMf, err := eng.UpgradeTheme(ctx, "paper-pro", "en-US")
	if err != nil {
		t.Fatalf("UpgradeTheme with checksum_url: %v", err)
	}
	if upMf.Version != "2.0.0" {
		t.Fatalf("expected upgraded version 2.0.0, got %s", upMf.Version)
	}

	// 4. Downgrade protection inside installZipInternal rejects 2.0.0 -> 1.5.0 when disallowDowngrade=true
	if _, err := eng.installZipInternal(ctx, bytes.NewReader(v15Zip), int64(len(v15Zip)), installThemeZipOptions{
		overwrite:         true,
		expectedThemeID:   "paper-pro",
		disallowDowngrade: true,
	}); err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("expected downgrade rejection, got %v", err)
	}
}

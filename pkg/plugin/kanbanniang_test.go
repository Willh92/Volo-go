package plugin

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestKanbanniang_CatalogParityAndAll26BundledModels(t *testing.T) {
	ctx := context.Background()
	repoPluginsDir := filepath.Join("..", "..", "plugins")

	// 1. Verify Official Plugin Catalog (plugins/catalog.json) matches every registered plugin manifest.json.
	catRaw, err := os.ReadFile(filepath.Join(repoPluginsDir, "catalog.json"))
	if err != nil {
		t.Fatalf("read plugins/catalog.json: %v", err)
	}
	var catalog struct {
		SchemaVersion int `json:"schema_version"`
		Plugins       []struct {
			ID           string `json:"id"`
			Version      string `json:"version"`
			Distribution string `json:"distribution"`
			ReleaseTag   string `json:"release_tag"`
			ReleaseAsset string `json:"release_asset"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(catRaw, &catalog); err != nil {
		t.Fatalf("parse plugins/catalog.json: %v", err)
	}
	if catalog.SchemaVersion != 1 || len(catalog.Plugins) < 2 {
		t.Fatalf("unexpected catalog header/entries: %+v", catalog)
	}
	seenInCatalog := make(map[string]string)
	for _, entry := range catalog.Plugins {
		mfBytes, rErr := os.ReadFile(filepath.Join(repoPluginsDir, entry.ID, "manifest.json"))
		if rErr != nil {
			t.Fatalf("catalog plugin %q missing manifest.json: %v", entry.ID, rErr)
		}
		mf, pErr := ParseManifest(mfBytes)
		if pErr != nil {
			t.Fatalf("catalog plugin %q invalid manifest: %v", entry.ID, pErr)
		}
		if mf.Version != entry.Version {
			t.Fatalf("catalog version mismatch for %q: catalog=%q vs manifest=%q", entry.ID, entry.Version, mf.Version)
		}
		if entry.Distribution == "standalone-release" {
			wantTag := fmt.Sprintf("plugin-%s-v%s", entry.ID, entry.Version)
			wantAsset := fmt.Sprintf("plugin-%s-v%s.zip", entry.ID, entry.Version)
			if entry.ReleaseTag != wantTag || entry.ReleaseAsset != wantAsset {
				t.Fatalf("standalone release metadata mismatch for %q: tag=%q asset=%q", entry.ID, entry.ReleaseTag, entry.ReleaseAsset)
			}
		}
		seenInCatalog[entry.ID] = entry.Version
	}
	if seenInCatalog["kanbanniang"] != "2.0.0" || seenInCatalog["sensitive-words"] == "" {
		t.Fatalf("expected kanbanniang=2.0.0 and sensitive-words in catalog, got %+v", seenInCatalog)
	}

	// 2. Stage kanbanniang into isolated test environment and verify admin/public endpoints + all 26 models.
	store, kr, i18nCat, tmpDir := openTestPluginEnv(t)
	pluginsDir := filepath.Join(tmpDir, "plugins")
	stagedKbnDir := filepath.Join(pluginsDir, "kanbanniang")
	copyDirRecursive(t, filepath.Join(repoPluginsDir, "kanbanniang"), stagedKbnDir)

	isAdminCaller := false
	mgr := NewManager(ManagerOptions{
		Store:        store,
		Keys:         kr,
		Catalog:      i18nCat,
		PluginsDir:   pluginsDir,
		DataDir:      filepath.Join(tmpDir, "data"),
		HookTimeout:  3 * time.Second,
		RouteTimeout: 3 * time.Second,
	})
	mgr.SetRequestInspector(func(r *http.Request) (bool, string) {
		return isAdminCaller, "127.0.0.0"
	})
	defer func() { _ = mgr.Close(ctx) }()

	if err := mgr.DiscoverAndStart(ctx); err != nil {
		t.Fatalf("DiscoverAndStart: %v", err)
	}
	if err := mgr.Enable(ctx, "kanbanniang"); err != nil {
		t.Fatalf("Enable kanbanniang: %v", err)
	}

	// Anonymous GET /list must return 401; Admin GET /list must return 200 with all 26 models.
	anonList := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(anonList, httptest.NewRequest(http.MethodGet, "/plugins/kanbanniang/api/list", nil))
	if anonList.Code != http.StatusUnauthorized {
		t.Fatalf("expected anonymous GET /api/list to return 401, got %d %s", anonList.Code, anonList.Body.String())
	}

	isAdminCaller = true
	adminList := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(adminList, httptest.NewRequest(http.MethodGet, "/plugins/kanbanniang/api/list", nil))
	isAdminCaller = false
	if adminList.Code != http.StatusOK {
		t.Fatalf("expected admin GET /api/list to return 200, got %d %s", adminList.Code, adminList.Body.String())
	}
	var listResp struct {
		SC     bool     `json:"sc"`
		Msg    string   `json:"msg"`
		Models []string `json:"models"`
	}
	if err := json.Unmarshal(adminList.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode /api/list: %v", err)
	}
	if !listResp.SC || len(listResp.Models) != 26 || strings.Count(listResp.Msg, ";") != 25 {
		t.Fatalf("expected 26 bundled Live2D models, got count=%d msg=%q", len(listResp.Models), listResp.Msg)
	}

	// Verify pure-WebP slimming invariant: zero redundant .png or .gif files remain in bundled assets,
	// and public.footer slot renders zero inline <script> tags (100% script-src 'self' CSP compatible).
	assetsRoot := filepath.Join(stagedKbnDir, "assets")
	if walkErr := filepath.WalkDir(assetsRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if ext == ".png" || ext == ".gif" {
			t.Fatalf("unexpected non-WebP raster asset in bundled kanbanniang: %s", path)
		}
		return nil
	}); walkErr != nil {
		t.Fatalf("walk bundled assets: %v", walkErr)
	}

	footerSlots := mgr.RenderSlots(ctx, SlotPublicFooter, SlotRenderContext{Locale: "zh-CN", Path: "/"})
	if len(footerSlots) != 1 {
		t.Fatalf("expected 1 public.footer slot fragment, got %d", len(footerSlots))
	}
	footerHTML := string(footerSlots[0].HTML)
	if !strings.Contains(footerHTML, `id="volo-kanbanniang-root"`) ||
		!strings.Contains(footerHTML, `data-static-base="/plugins/kanbanniang/assets"`) ||
		!strings.Contains(footerHTML, `data-api-base="/plugins/kanbanniang/api"`) {
		t.Fatalf("expected data-* boot attributes on #volo-kanbanniang-root, got: %s", footerHTML)
	}
	// Only the host-managed external defer <script src="/plugins/kanbanniang/assets/widget.js?v=..."> tag may appear (zero inline JS).
	if strings.Count(footerHTML, "<script") != 1 ||
		!strings.Contains(footerHTML, `<script defer src="/plugins/kanbanniang/assets/widget.js?v=2.0.0"`) ||
		!strings.Contains(footerHTML, `></script>`) {
		t.Fatalf("expected single external defer widget.js script with zero inline JS, got: %s", footerHTML)
	}

	// Verify all 26 bundled models resolve cleanly with pure .webp textures on disk.
	for _, modelName := range listResp.Models {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/plugins/kanbanniang/api/model?model="+modelName, nil)
		mgr.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/model?model=%s returned %d: %s", modelName, rec.Code, rec.Body.String())
		}
		if gotCC := rec.Header().Get("Cache-Control"); gotCC != "no-store" {
			t.Fatalf("expected Cache-Control: no-store on /api/model, got %q", gotCC)
		}
		bodyStr := rec.Body.String()
		if strings.Contains(bodyStr, "http://") || strings.Contains(bodyStr, "https://") {
			t.Fatalf("model %s descriptor leaked external URL: %s", modelName, bodyStr)
		}

		var desc struct {
			Name         string   `json:"name"`
			Model        string   `json:"model"`
			AssetBase    string   `json:"asset_base"`
			Textures     []string `json:"textures"`
			TexturesWebp []string `json:"textures_webp"`
			Physics      string   `json:"physics"`
			Pose         string   `json:"pose"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &desc); err != nil {
			t.Fatalf("unmarshal descriptor for %s: %v", modelName, err)
		}
		if desc.Name != modelName || desc.AssetBase != "/plugins/kanbanniang/assets/" {
			t.Fatalf("unexpected name/asset_base for %s: %+v", modelName, desc)
		}
		if len(desc.Textures) == 0 || len(desc.TexturesWebp) != 0 {
			t.Fatalf("expected non-empty pure-WebP textures (with redundant textures_webp omitted) for %s, got tex=%d webpHints=%d", modelName, len(desc.Textures), len(desc.TexturesWebp))
		}
		if _, err := os.Stat(filepath.Join(assetsRoot, filepath.FromSlash(desc.Model))); err != nil {
			t.Fatalf("missing .moc file for %s (%s): %v", modelName, desc.Model, err)
		}
		for i := range desc.Textures {
			if !strings.HasSuffix(strings.ToLower(desc.Textures[i]), ".webp") {
				t.Fatalf("expected pure .webp texture at %s[%d], got %q", modelName, i, desc.Textures[i])
			}
			if _, err := os.Stat(filepath.Join(assetsRoot, filepath.FromSlash(desc.Textures[i]))); err != nil {
				t.Fatalf("missing WebP texture %s: %v", desc.Textures[i], err)
			}
		}
	}

	// Verify /absoluteRandomModel excludes current model andretired/unknown model falls back cleanly.
	rndRec := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(rndRec, httptest.NewRequest(http.MethodGet, "/plugins/kanbanniang/api/absoluteRandomModel?exclude=Potion-Maker/Pio", nil))
	if rndRec.Code != http.StatusOK || strings.Contains(rndRec.Body.String(), `"name":"Potion-Maker/Pio"`) {
		t.Fatalf("expected /absoluteRandomModel?exclude=Potion-Maker/Pio to pick a different model, got %d %s", rndRec.Code, rndRec.Body.String())
	}

	retiredRec := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(retiredRec, httptest.NewRequest(http.MethodGet, "/plugins/kanbanniang/api/model?model=retired/unknown-model", nil))
	if retiredRec.Code != http.StatusOK {
		t.Fatalf("expected retired/unknown model to fall back to bundled model with 200, got %d", retiredRec.Code)
	}

	// 3. Verify custom PNG-only model compatibility (no companion .webp files -> 200 OK, textures_webp omitted).
	customDir := filepath.Join(assetsRoot, "model", "CustomPack", "PngOnly")
	if err := os.MkdirAll(customDir, 0o750); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(customDir, "avatar.moc"), []byte("moc-placeholder"), 0o640)
	_ = os.WriteFile(filepath.Join(customDir, "sheet.png"), []byte("png-placeholder"), 0o640)
	_ = os.WriteFile(filepath.Join(customDir, "index.json"), []byte(`{
		"model": "model/CustomPack/PngOnly/avatar.moc",
		"textures": ["model/CustomPack/PngOnly/sheet.png"]
	}`), 0o640)
	origManifestList, err := os.ReadFile(filepath.Join(assetsRoot, "model-list.json"))
	if err != nil {
		t.Fatal(err)
	}
	var updatedList []string
	_ = json.Unmarshal(origManifestList, &updatedList)
	updatedList = append(updatedList, "CustomPack/PngOnly")
	updatedListBytes, _ := json.Marshal(updatedList)
	if err := os.WriteFile(filepath.Join(assetsRoot, "model-list.json"), updatedListBytes, 0o640); err != nil {
		t.Fatal(err)
	}

	pngOnlyRec := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(pngOnlyRec, httptest.NewRequest(http.MethodGet, "/plugins/kanbanniang/api/model?model=CustomPack/PngOnly", nil))
	if pngOnlyRec.Code != http.StatusOK {
		t.Fatalf("expected custom PNG-only model to return 200 OK, got %d %s", pngOnlyRec.Code, pngOnlyRec.Body.String())
	}
	if strings.Contains(pngOnlyRec.Body.String(), `"textures_webp"`) {
		t.Fatalf("expected PNG-only custom model to omit textures_webp, got %s", pngOnlyRec.Body.String())
	}

	// 4. Security validation cases ported from KanbanModelsTest (all must fail closed with 503).
	badManifestCases := []string{
		`["../secret"]`,
		`["/etc/passwd"]`,
		`["https://evil.example/model"]`,
		`["Group\\Backslash"]`,
	}
	for _, badList := range badManifestCases {
		_ = os.WriteFile(filepath.Join(assetsRoot, "model-list.json"), []byte(badList), 0o640)
		badRec := httptest.NewRecorder()
		mgr.Handler().ServeHTTP(badRec, httptest.NewRequest(http.MethodGet, "/plugins/kanbanniang/api/model", nil))
		if badRec.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 for unsafe model-list.json %s, got %d %s", badList, badRec.Code, badRec.Body.String())
		}
	}

	// Restore valid model-list with CustomPack/PngOnly and test unsafe resource paths inside index.json.
	_ = os.WriteFile(filepath.Join(assetsRoot, "model-list.json"), []byte(`["CustomPack/PngOnly"]`), 0o640)
	badIndexCases := []string{
		`{"model":"https://evil.example/avatar.moc","textures":["model/CustomPack/PngOnly/sheet.png"]}`,
		`{"model":"model/CustomPack/PngOnly/avatar.moc","textures":["../secret.png"]}`,
		`{"model":"model/CustomPack/PngOnly/avatar.moc","textures":["/etc/passwd"]}`,
		`{"model":"model/CustomPack/PngOnly/missing.moc","textures":["model/CustomPack/PngOnly/sheet.png"]}`,
		`{"model":"model/CustomPack/PngOnly/avatar.moc","textures":[]}`,
	}
	for _, badIdx := range badIndexCases {
		_ = os.WriteFile(filepath.Join(customDir, "index.json"), []byte(badIdx), 0o640)
		badRec := httptest.NewRecorder()
		mgr.Handler().ServeHTTP(badRec, httptest.NewRequest(http.MethodGet, "/plugins/kanbanniang/api/model?model=CustomPack/PngOnly", nil))
		if badRec.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 for unsafe index.json %s, got %d %s", badIdx, badRec.Code, badRec.Body.String())
		}
	}
}

func TestKanbanniang_AssetDeliveryGzipETagRangeAndLargeZipRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, kr, i18nCat, tmpDir := openTestPluginEnv(t)
	pluginsDir := filepath.Join(tmpDir, "plugins")
	copyDirRecursive(t, filepath.Join("..", "..", "plugins", "kanbanniang"), filepath.Join(pluginsDir, "kanbanniang"))

	mgr := NewManager(ManagerOptions{
		Store:      store,
		Keys:       kr,
		Catalog:    i18nCat,
		PluginsDir: pluginsDir,
		DataDir:    filepath.Join(tmpDir, "data"),
	})
	defer func() { _ = mgr.Close(ctx) }()

	if err := mgr.DiscoverAndStart(ctx); err != nil {
		t.Fatalf("DiscoverAndStart: %v", err)
	}
	if err := mgr.Enable(ctx, "kanbanniang"); err != nil {
		t.Fatalf("Enable kanbanniang: %v", err)
	}

	// 1. On-the-fly Gzip + versioned Cache-Control + ETag 304 on .moc binary model file.
	mocRel := "/plugins/kanbanniang/assets/model/Potion-Maker/Pio/model.moc?v=2.0.0"
	rawMocOnDisk, err := os.ReadFile(filepath.Join(pluginsDir, "kanbanniang", "assets", "model", "Potion-Maker", "Pio", "model.moc"))
	if err != nil {
		t.Fatalf("read Pio model.moc: %v", err)
	}

	gzReq := httptest.NewRequest(http.MethodGet, mocRel, nil)
	gzReq.Header.Set("Accept-Encoding", "br, gzip;q=1.0")
	gzRec := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(gzRec, gzReq)
	if gzRec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d", mocRel, gzRec.Code)
	}
	if gzRec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("expected Content-Encoding: gzip on .moc, got %q", gzRec.Header().Get("Content-Encoding"))
	}
	if !strings.Contains(gzRec.Header().Get("Cache-Control"), "max-age=2592000") {
		t.Fatalf("expected 30d Cache-Control with ?v=, got %q", gzRec.Header().Get("Cache-Control"))
	}
	etag := gzRec.Header().Get("ETag")
	if etag == "" || !strings.HasSuffix(etag, `-gz"`) {
		t.Fatalf("expected distinct -gz ETag, got %q", etag)
	}
	gzReader, err := gzip.NewReader(bytes.NewReader(gzRec.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	ungzipped, err := io.ReadAll(gzReader)
	_ = gzReader.Close()
	if err != nil || !bytes.Equal(ungzipped, rawMocOnDisk) {
		t.Fatalf("gunzipped .moc mismatch: got %d bytes, want %d bytes (err=%v)", len(ungzipped), len(rawMocOnDisk), err)
	}

	// Conditional If-None-Match -> 304 Not Modified
	condReq := httptest.NewRequest(http.MethodGet, mocRel, nil)
	condReq.Header.Set("Accept-Encoding", "gzip")
	condReq.Header.Set("If-None-Match", etag)
	condRec := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(condRec, condReq)
	if condRec.Code != http.StatusNotModified {
		t.Fatalf("expected 304 Not Modified for matching ETag, got %d", condRec.Code)
	}

	// HEAD with Accept-Encoding: gzip must return identical -gz ETag + Content-Encoding: gzip + Content-Length (and 304 on If-None-Match).
	headGzReq := httptest.NewRequest(http.MethodHead, mocRel, nil)
	headGzReq.Header.Set("Accept-Encoding", "gzip")
	headGzRec := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(headGzRec, headGzReq)
	if headGzRec.Code != http.StatusOK || headGzRec.Header().Get("ETag") != etag || headGzRec.Header().Get("Content-Encoding") != "gzip" || headGzRec.Body.Len() != 0 {
		t.Fatalf("HEAD gzip parity mismatch: code=%d etag=%q (want %q) ce=%q bodyLen=%d", headGzRec.Code, headGzRec.Header().Get("ETag"), etag, headGzRec.Header().Get("Content-Encoding"), headGzRec.Body.Len())
	}
	if headGzRec.Header().Get("Content-Length") != gzRec.Header().Get("Content-Length") || gzRec.Header().Get("Content-Length") == "" {
		t.Fatalf("expected matching non-empty Content-Length from bounded RAM gzip cache: HEAD=%q GET=%q", headGzRec.Header().Get("Content-Length"), gzRec.Header().Get("Content-Length"))
	}
	head304Req := httptest.NewRequest(http.MethodHead, mocRel, nil)
	head304Req.Header.Set("Accept-Encoding", "gzip")
	head304Req.Header.Set("If-None-Match", etag)
	head304Rec := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(head304Rec, head304Req)
	if head304Rec.Code != http.StatusNotModified {
		t.Fatalf("expected HEAD If-None-Match 304, got %d", head304Rec.Code)
	}

	// Live file modification on disk must invalidate the RAM Gzip cache immediately via (size, modTime) check.
	tipsDiskPath := filepath.Join(pluginsDir, "kanbanniang", "assets", "tips.json")
	origTipsBytes, err := os.ReadFile(tipsDiskPath)
	if err != nil {
		t.Fatalf("read tips.json: %v", err)
	}
	tipsReq1 := httptest.NewRequest(http.MethodGet, "/plugins/kanbanniang/assets/tips.json?v=2.0.0", nil)
	tipsReq1.Header.Set("Accept-Encoding", "gzip")
	tipsRec1 := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(tipsRec1, tipsReq1)
	if tipsRec1.Code != http.StatusOK {
		t.Fatalf("GET tips.json #1 = %d", tipsRec1.Code)
	}
	etagTips1 := tipsRec1.Header().Get("ETag")

	modifiedTips := append(append([]byte(nil), origTipsBytes...), ' ', '\n')
	if err := os.WriteFile(tipsDiskPath, modifiedTips, 0o640); err != nil {
		t.Fatal(err)
	}
	futureTime := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(tipsDiskPath, futureTime, futureTime)

	tipsReq2 := httptest.NewRequest(http.MethodGet, "/plugins/kanbanniang/assets/tips.json?v=2.0.0", nil)
	tipsReq2.Header.Set("Accept-Encoding", "gzip")
	tipsRec2 := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(tipsRec2, tipsReq2)
	if tipsRec2.Code != http.StatusOK || tipsRec2.Header().Get("ETag") == etagTips1 {
		t.Fatalf("expected updated ETag after live asset modification, got code=%d etag1=%q etag2=%q", tipsRec2.Code, etagTips1, tipsRec2.Header().Get("ETag"))
	}
	gzTips2, err := gzip.NewReader(bytes.NewReader(tipsRec2.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	unzippedTips2, _ := io.ReadAll(gzTips2)
	_ = gzTips2.Close()
	if !bytes.Equal(unzippedTips2, modifiedTips) {
		t.Fatalf("expected updated tips.json payload from invalidated gzip cache, got %d bytes want %d bytes", len(unzippedTips2), len(modifiedTips))
	}
	_ = os.WriteFile(tipsDiskPath, origTipsBytes, 0o640)

	// gzip;q=0 must bypass compression
	q0Req := httptest.NewRequest(http.MethodGet, mocRel, nil)
	q0Req.Header.Set("Accept-Encoding", "gzip;q=0")
	q0Rec := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(q0Rec, q0Req)
	if q0Rec.Code != http.StatusOK || q0Rec.Header().Get("Content-Encoding") != "" || !bytes.Equal(q0Rec.Body.Bytes(), rawMocOnDisk) {
		t.Fatalf("expected uncompressed .moc when gzip;q=0, got status=%d ce=%q len=%d", q0Rec.Code, q0Rec.Header().Get("Content-Encoding"), q0Rec.Body.Len())
	}

	// Range request with Accept-Encoding: gzip (and If-Range carrying the -gz ETag) must return 206 Partial Content uncompressed slice.
	rngReq := httptest.NewRequest(http.MethodGet, mocRel, nil)
	rngReq.Header.Set("Accept-Encoding", "gzip")
	rngReq.Header.Set("Range", "bytes=0-63")
	rngReq.Header.Set("If-Range", etag)
	rngRec := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(rngRec, rngReq)
	if rngRec.Code != http.StatusPartialContent || rngRec.Header().Get("Content-Encoding") != "" || !bytes.Equal(rngRec.Body.Bytes(), rawMocOnDisk[:64]) {
		t.Fatalf("expected 206 uncompressed 64-byte slice on Range + If-Range (-gz ETag), got code=%d ce=%q len=%d", rngRec.Code, rngRec.Header().Get("Content-Encoding"), rngRec.Body.Len())
	}

	// Distroless-safe inline MIME types for .webp and .ogg (no forced attachment download)
	webpRec := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(webpRec, httptest.NewRequest(http.MethodGet, "/plugins/kanbanniang/assets/background/sakura0.webp?v=2.0.0", nil))
	if webpRec.Code != http.StatusOK || webpRec.Header().Get("Content-Type") != "image/webp" || webpRec.Header().Get("Content-Disposition") != "" {
		t.Fatalf("unexpected .webp headers: status=%d ct=%q cd=%q", webpRec.Code, webpRec.Header().Get("Content-Type"), webpRec.Header().Get("Content-Disposition"))
	}

	// 2. Streaming ExportZip -> InstallFromZip with > 300 entries (verifying raised MaxZipEntries & Store/Deflate mix).
	bigPluginDir := filepath.Join(pluginsDir, "multi-asset-pack")
	if err := os.MkdirAll(filepath.Join(bigPluginDir, "assets"), 0o750); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(bigPluginDir, "manifest.json"), []byte(`{
		"id": "multi-asset-pack",
		"name": {"en-US": "Multi Asset Pack"},
		"version": "1.0.0",
		"runtime": "goja",
		"entry": "main.js"
	}`), 0o640)
	_ = os.WriteFile(filepath.Join(bigPluginDir, "main.js"), []byte(`module.exports = {};`), 0o640)
	for i := 0; i < 310; i++ {
		ext := ".png"
		if i%2 == 1 {
			ext = ".mtn"
		}
		p := filepath.Join(bigPluginDir, "assets", fmt.Sprintf("item_%03d%s", i, ext))
		if err := os.WriteFile(p, []byte(fmt.Sprintf("payload-%d", i)), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := mgr.DiscoverAndStart(ctx); err != nil {
		t.Fatalf("Discover multi-asset-pack: %v", err)
	}

	zipFile := filepath.Join(tmpDir, "multi-asset-pack.zip")
	zfOut, err := os.Create(zipFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.ExportZip("multi-asset-pack", zfOut); err != nil {
		_ = zfOut.Close()
		t.Fatalf("ExportZip >300 entries failed: %v", err)
	}
	_ = zfOut.Close()

	// Verify ExportZip used zip.Store on .png and zip.Deflate on .mtn
	zr, err := zip.OpenReader(zipFile)
	if err != nil {
		t.Fatal(err)
	}
	sawStoredPNG := false
	sawDeflatedMTN := false
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, ".png") && f.Method == zip.Store {
			sawStoredPNG = true
		}
		if strings.HasSuffix(f.Name, ".mtn") && f.Method == zip.Deflate {
			sawDeflatedMTN = true
		}
	}
	_ = zr.Close()
	if !sawStoredPNG || !sawDeflatedMTN {
		t.Fatalf("expected ExportZip to use Store for .png (%v) and Deflate for .mtn (%v)", sawStoredPNG, sawDeflatedMTN)
	}

	zfIn, err := os.Open(zipFile)
	if err != nil {
		t.Fatal(err)
	}
	defer zfIn.Close()
	zfStat, _ := zfIn.Stat()
	if _, err := mgr.InstallFromZip(ctx, zfIn, zfStat.Size(), true, "en-US"); err != nil {
		t.Fatalf("InstallFromZip >300 entries failed: %v", err)
	}
}

func openBenchKanbanniangManager(b *testing.B) (*Manager, http.Handler) {
	b.Helper()
	ctx := context.Background()
	store, kr, i18nCat, tmpDir := openTestPluginEnv(b)
	mgr := NewManager(ManagerOptions{
		Store:        store,
		Keys:         kr,
		Catalog:      i18nCat,
		PluginsDir:   filepath.Join("..", "..", "plugins"),
		DataDir:      filepath.Join(tmpDir, "data"),
		HookTimeout:  3 * time.Second,
		RouteTimeout: 3 * time.Second,
	})
	mgr.SetRequestInspector(func(r *http.Request) (bool, string) {
		return false, "127.0.0.0"
	})
	b.Cleanup(func() { _ = mgr.Close(ctx) })
	if err := mgr.DiscoverAndStart(ctx); err != nil {
		b.Fatalf("DiscoverAndStart: %v", err)
	}
	if err := mgr.Enable(ctx, "kanbanniang"); err != nil {
		b.Fatalf("Enable kanbanniang: %v", err)
	}
	return mgr, mgr.Handler()
}

func BenchmarkKanbanniang_ModelAPI(b *testing.B) {
	_, handler := openBenchKanbanniangManager(b)
	// Warm up manifest + descriptor cache on initial call.
	warmRec := httptest.NewRecorder()
	handler.ServeHTTP(warmRec, httptest.NewRequest(http.MethodGet, "/plugins/kanbanniang/api/model?model=Potion-Maker/Pio", nil))
	if warmRec.Code != http.StatusOK {
		b.Fatalf("warmup /api/model = %d", warmRec.Code)
	}

	b.Run("ExplicitModel", func(b *testing.B) {
		req := httptest.NewRequest(http.MethodGet, "/plugins/kanbanniang/api/model?model=Potion-Maker/Pio", nil)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				b.Fatalf("unexpected status %d", rec.Code)
			}
		}
	})

	b.Run("RandomSwitch", func(b *testing.B) {
		req := httptest.NewRequest(http.MethodGet, "/plugins/kanbanniang/api/absoluteRandomModel?exclude=Potion-Maker/Pio", nil)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				b.Fatalf("unexpected status %d", rec.Code)
			}
		}
	})
}

func BenchmarkKanbanniang_ServeAssetGzipAndETag(b *testing.B) {
	_, handler := openBenchKanbanniangManager(b)
	targetURL := "/plugins/kanbanniang/assets/live2d.js?v=2.0.0"

	warmReq := httptest.NewRequest(http.MethodGet, targetURL, nil)
	warmReq.Header.Set("Accept-Encoding", "br, gzip;q=1.0")
	warmRec := httptest.NewRecorder()
	handler.ServeHTTP(warmRec, warmReq)
	if warmRec.Code != http.StatusOK {
		b.Fatalf("warmup GET live2d.js = %d", warmRec.Code)
	}
	etag := warmRec.Header().Get("ETag")

	b.Run("Gzip200Cached", func(b *testing.B) {
		req := httptest.NewRequest(http.MethodGet, targetURL, nil)
		req.Header.Set("Accept-Encoding", "br, gzip;q=1.0")
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				b.Fatalf("unexpected status %d", rec.Code)
			}
		}
	})

	b.Run("ETag304Revalidate", func(b *testing.B) {
		req := httptest.NewRequest(http.MethodGet, targetURL, nil)
		req.Header.Set("Accept-Encoding", "br, gzip;q=1.0")
		req.Header.Set("If-None-Match", etag)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotModified {
				b.Fatalf("unexpected status %d", rec.Code)
			}
		}
	})
}

func BenchmarkKanbanniang_RenderFooterSlot(b *testing.B) {
	ctx := context.Background()
	mgr, _ := openBenchKanbanniangManager(b)
	slotCtx := SlotRenderContext{Locale: "zh-CN", Path: "/posts/hello"}
	if out := mgr.RenderSlots(ctx, SlotPublicFooter, slotCtx); len(out) == 0 || !strings.Contains(out[0].HTML, "volo-kanbanniang-root") {
		b.Fatalf("unexpected footer slot output: %+v", out)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := mgr.RenderSlots(ctx, SlotPublicFooter, slotCtx)
		if len(out) == 0 {
			b.Fatal("empty slot output")
		}
	}
}

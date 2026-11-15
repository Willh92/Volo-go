package theme

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/domain"
	"github.com/Willh92/Volo-go/internal/extpkg"
	"github.com/Willh92/Volo-go/internal/i18n"
)

func buildThemeZipBytes(t *testing.T, files map[string]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		hdr := &zip.FileHeader{
			Name:   name,
			Method: zip.Deflate,
		}
		hdr.SetMode(0o644)
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := fw.Write([]byte(body)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	raw := buf.Bytes()
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:])
}

func TestThemeUpdateCapabilitiesURLInstallUpgradeCancellationAndRecovery(t *testing.T) {
	ctx := context.Background()
	cat, err := i18n.NewCatalog("zh-CN", "")
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	themesDir := filepath.Join(t.TempDir(), "themes")

	// Simulate interrupted `.theme-old-recovered-skin` directory and orphaned temp files before startup!
	if err := os.MkdirAll(filepath.Join(themesDir, ".theme-old-recovered-skin", "templates"), 0o755); err != nil {
		t.Fatalf("mkdir backup: %v", err)
	}
	_ = os.WriteFile(filepath.Join(themesDir, ".theme-old-recovered-skin", "theme.json"), []byte(`{
		"id": "recovered-skin",
		"version": "1.0.0",
		"author": "Test",
		"name": {"zh-CN": "恢复主题", "en-US": "Recovered Skin"},
		"description": {"zh-CN": "崩溃恢复测试", "en-US": "Crash recovery test"}
	}`), 0o644)
	_ = os.WriteFile(filepath.Join(themesDir, ".theme-old-recovered-skin", "templates", "index.html"), []byte(`{{define "content"}}<div id="recovered">v1</div>{{end}}`), 0o644)
	_ = os.MkdirAll(filepath.Join(themesDir, ".theme-stage-orphan"), 0o755)
	_ = os.WriteFile(filepath.Join(themesDir, ".volo-theme-dl-orphan.zip"), []byte("partial"), 0o644)

	eng, err := NewEngine(cat, themesDir)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	eng.SetHostVersion("1.0.2")
	eng.SetAllowLoopbackForTests(true)

	if !eng.Has("recovered-skin") {
		t.Fatal("expected crash-recovered theme 'recovered-skin' to be restored on NewEngine startup")
	}
	if _, err := os.Stat(filepath.Join(themesDir, ".theme-stage-orphan")); !os.IsNotExist(err) {
		t.Fatal("expected .theme-stage-orphan to be cleaned up")
	}
	if _, err := os.Stat(filepath.Join(themesDir, ".volo-theme-dl-orphan.zip")); !os.IsNotExist(err) {
		t.Fatal("expected .volo-theme-dl-orphan.zip to be cleaned up")
	}

	// Verify built-in volo-studio capability is "none" and never makes network calls.
	stStudio, err := eng.CheckThemeUpdate(ctx, DefaultThemeID, true, "en-US")
	if err != nil {
		t.Fatalf("CheckThemeUpdate volo-studio: %v", err)
	}
	if stStudio.State != extpkg.StateUnsupported {
		t.Fatalf("expected StateUnsupported for built-in volo-studio, got %+v", stStudio)
	}
	if _, err := eng.UpgradeTheme(ctx, DefaultThemeID, "en-US"); err == nil {
		t.Fatal("expected UpgradeTheme(volo-studio) to be rejected")
	}

	// Verify strategy: "hook" is forbidden in theme.json (themes have no JS VM).
	hookThemeZip, _ := buildThemeZipBytes(t, map[string]string{
		"theme.json": `{
			"id": "bad-hook-theme",
			"version": "1.0.0",
			"name": {"en-US": "Bad Hook Theme"},
			"update": {"strategy": "hook"}
		}`,
		"templates/index.html": `{{define "content"}}ok{{end}}`,
	})
	if _, err := eng.InstallZipContext(ctx, bytes.NewReader(hookThemeZip), int64(len(hookThemeZip)), false); err == nil {
		t.Fatal("expected strategy: 'hook' in theme.json to be rejected")
	}

	var catalogJSON atomic.Value
	zipMap := make(map[string][]byte)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/themes-catalog.json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(catalogJSON.Load().(string)))
		case "/slow-theme.zip":
			w.Header().Set("Content-Type", "application/zip")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("PK\x03\x04"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done()
		default:
			name := strings.TrimPrefix(r.URL.Path, "/")
			if raw, ok := zipMap[name]; ok {
				w.Header().Set("Content-Type", "application/zip")
				_, _ = w.Write(raw)
				return
			}
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	makeThemeZip := func(id, ver, minHost string, allowOnline bool, tplBody string) ([]byte, string) {
		mf := fmt.Sprintf(`{
			"id": %q,
			"version": %q,
			"min_host_version": %q,
			"author": "Volo Team",
			"homepage": "https://example.com/themes/%s",
			"name": {"zh-CN": %q, "en-US": %q},
			"description": {"zh-CN": "测试主题", "en-US": "Test Theme"},
			"update": {
				"strategy": "volo-catalog",
				"url": %q,
				"allow_online_update": %v
			}
		}`, id, ver, minHost, id, id, id, srv.URL+"/themes-catalog.json", allowOnline)
		return buildThemeZipBytes(t, map[string]string{
			id + "/theme.json":           mf,
			id + "/templates/index.html": tplBody,
		})
	}

	v1Zip, v1Sum := makeThemeZip("ink-paper", "1.0.0", "1.0.0", true, `{{define "content"}}<section data-ver="1.0.0">Ink v1</section>{{end}}`)
	v2BadTplZip, v2BadSum := makeThemeZip("ink-paper", "2.0.0", "1.0.0", true, `{{define "content"}}{{ call_nonexistent_func . }}{{end}}`)
	v2SpoofZip, v2SpoofSum := makeThemeZip("wrong-theme-id", "2.0.0", "1.0.0", true, `{{define "content"}}spoof{{end}}`)
	v2GoodZip, v2GoodSum := makeThemeZip("ink-paper", "2.1.0", "1.0.0", true, `{{define "content"}}<section data-ver="2.1.0">Ink v2.1</section>{{end}}`)
	v3FutureHostZip, v3FutureSum := makeThemeZip("ink-paper", "3.0.0", "8.0.0", true, `{{define "content"}}future{{end}}`)
	checkOnlyZip, checkOnlySum := makeThemeZip("manual-only", "1.0.0", "1.0.0", false, `{{define "content"}}manual{{end}}`)

	zipMap["ink-v1.zip"] = v1Zip
	zipMap["ink-v2-bad.zip"] = v2BadTplZip
	zipMap["ink-v2-spoof.zip"] = v2SpoofZip
	zipMap["ink-v2-good.zip"] = v2GoodZip
	zipMap["ink-v3-host.zip"] = v3FutureHostZip
	zipMap["manual-v1.zip"] = checkOnlyZip

	// 1. Install ink-paper v1.0.0 and manual-only v1.0.0 via URL
	if _, err := eng.InstallFromURL(ctx, srv.URL+"/ink-v1.zip", v1Sum, false); err != nil {
		t.Fatalf("InstallFromURL ink-v1: %v", err)
	}
	if _, err := eng.InstallFromURL(ctx, srv.URL+"/manual-v1.zip", checkOnlySum, false); err != nil {
		t.Fatalf("InstallFromURL manual-v1: %v", err)
	}

	assertRenderedMarker := func(wantMarker string) {
		t.Helper()
		rec := httptest.NewRecorder()
		err := eng.Render(rec, http.StatusOK, "ink-paper", "index.html", PageContext{
			Locale: "en-US",
			Site:   domain.SiteSettings{Title: "Volo"},
		})
		if err != nil {
			t.Fatalf("Render ink-paper: %v", err)
		}
		if !strings.Contains(rec.Body.String(), wantMarker) {
			t.Fatalf("expected rendered output to contain %q, got len=%d", wantMarker, rec.Body.Len())
		}
	}
	assertRenderedMarker(`data-ver="1.0.0"`)

	// 2. CheckUpdates batch verifies capabilities:
	// volo-studio + recovered-skin -> 2 unsupported; ink-paper (online) + manual-only (check_only) -> 2 checked & 2 updates!
	catalogJSON.Store(fmt.Sprintf(`{
		"themes": [
			{
				"id": "ink-paper",
				"version": "2.0.0",
				"download_url": %q,
				"sha256": %q,
				"changelog": {"zh-CN": "排版升级", "en-US": "Typography upgrade"}
			},
			{
				"id": "manual-only",
				"version": "1.5.0",
				"download_url": "https://example.com/manual-v1.5.0.zip"
			}
		]
	}`, srv.URL+"/ink-v2-bad.zip", v2BadSum))

	batch, err := eng.CheckUpdates(ctx, nil, true, "zh-CN")
	if err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	if batch.Unsupported != 2 || batch.Checked != 2 || batch.Updates != 2 {
		t.Fatalf("unexpected theme batch summary: %+v", batch)
	}
	if stManual := batch.Statuses["manual-only"]; !stManual.HasUpdate || stManual.OnlineUpdatable || stManual.DownloadURL != "" {
		t.Fatalf("expected check_only status for manual-only, got %+v", stManual)
	}
	if _, err := eng.UpgradeTheme(ctx, "manual-only", "zh-CN"); err == nil {
		t.Fatal("expected UpgradeTheme(manual-only) to be rejected")
	}

	// 3. Broken template in v2.0.0 fails pre-commit dry-run -> v1.0.0 stays live!
	if _, err := eng.UpgradeTheme(ctx, "ink-paper", "zh-CN"); err == nil {
		t.Fatal("expected broken template in ink-v2-bad.zip to fail validation")
	}
	assertRenderedMarker(`data-ver="1.0.0"`)

	// 4. Cancel mid-download -> zero temp files left behind, v1.0.0 untouched!
	cancelCtx, cancel := context.WithTimeout(ctx, 80*time.Millisecond)
	_, cErr := eng.InstallFromURL(cancelCtx, srv.URL+"/slow-theme.zip", "", true)
	cancel()
	if !errors.Is(cErr, context.DeadlineExceeded) && !errors.Is(cErr, context.Canceled) {
		t.Fatalf("expected cancellation error on slow-theme.zip, got %v", cErr)
	}
	assertRenderedMarker(`data-ver="1.0.0"`)

	// 5. Spoofed theme ID in archive -> rejected!
	catalogJSON.Store(fmt.Sprintf(`{
		"themes": [{
			"id": "ink-paper",
			"version": "2.0.0",
			"download_url": %q,
			"sha256": %q
		}]
	}`, srv.URL+"/ink-v2-spoof.zip", v2SpoofSum))
	_, _ = eng.CheckThemeUpdate(ctx, "ink-paper", true, "en-US")
	if _, err := eng.UpgradeTheme(ctx, "ink-paper", "en-US"); err == nil || !strings.Contains(err.Error(), "does not match expected") {
		t.Fatalf("expected spoofed theme ID rejection, got %v", err)
	}

	// 6. Future MinHostVersion (8.0.0 > 1.0.2) -> CompatibleWithHost=false and UpgradeTheme rejected!
	catalogJSON.Store(fmt.Sprintf(`{
		"themes": [{
			"id": "ink-paper",
			"version": "3.0.0",
			"min_host_version": "8.0.0",
			"download_url": %q,
			"sha256": %q
		}]
	}`, srv.URL+"/ink-v3-host.zip", v3FutureSum))
	stIncomp, err := eng.CheckThemeUpdate(ctx, "ink-paper", true, "en-US")
	if err != nil || stIncomp.CompatibleWithHost {
		t.Fatalf("expected CompatibleWithHost=false, got err=%v status=%+v", err, stIncomp)
	}
	if _, err := eng.UpgradeTheme(ctx, "ink-paper", "en-US"); err == nil || !strings.Contains(err.Error(), "8.0.0") {
		t.Fatalf("expected host compatibility error, got %v", err)
	}

	// 7. Healthy v2.1.0 online upgrade -> hot-swaps and renders 2.1.0 immediately!
	catalogJSON.Store(fmt.Sprintf(`{
		"themes": [{
			"id": "ink-paper",
			"version": "2.1.0",
			"min_host_version": "1.0.0",
			"download_url": %q,
			"sha256": %q
		}]
	}`, srv.URL+"/ink-v2-good.zip", v2GoodSum))
	_, _ = eng.CheckThemeUpdate(ctx, "ink-paper", true, "en-US")
	upgradedMf, err := eng.UpgradeTheme(ctx, "ink-paper", "en-US")
	if err != nil {
		t.Fatalf("UpgradeTheme v2.1.0: %v", err)
	}
	if upgradedMf.Version != "2.1.0" {
		t.Fatalf("expected upgraded manifest v2.1.0, got %s", upgradedMf.Version)
	}
	assertRenderedMarker(`data-ver="2.1.0"`)
}

func zipRealThemeDirectory(t *testing.T, srcDir string, topPrefix string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	err := filepath.WalkDir(srcDir, func(cur string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, rErr := filepath.Rel(srcDir, cur)
		if rErr != nil {
			return rErr
		}
		name := filepath.ToSlash(rel)
		if topPrefix != "" {
			name = topPrefix + "/" + name
		}
		raw, fErr := os.ReadFile(cur)
		if fErr != nil {
			return fErr
		}
		hdr := &zip.FileHeader{
			Name:   name,
			Method: zip.Deflate,
		}
		hdr.SetMode(0o644)
		fw, cErr := zw.CreateHeader(hdr)
		if cErr != nil {
			return cErr
		}
		_, wErr := fw.Write(raw)
		return wErr
	})
	if err != nil {
		t.Fatalf("walk real theme dir %s: %v", srcDir, err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close theme zip: %v", err)
	}
	raw := buf.Bytes()
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:])
}

func TestLegacyV102OfficialThemeUpdateBootstrapAndRealCatalogRoundTrip(t *testing.T) {
	ctx := context.Background()
	cat, err := i18n.NewCatalog("zh-CN", "")
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	themesDir := filepath.Join(t.TempDir(), "themes")

	// Seed legacy v1.0.2 volo-journal v1.1.0 WITHOUT an "update" block, plus a 3rd-party theme and an explicit strategy:"none" theme.
	seedLegacyTheme := func(id, ver, author, extraJSON string) {
		tDir := filepath.Join(themesDir, id, "templates")
		if err := os.MkdirAll(tDir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", id, err)
		}
		mf := fmt.Sprintf(`{
			"id": %q,
			"version": %q,
			"author": %q,
			"name": {"zh-CN": %q, "en-US": %q}%s
		}`, id, ver, author, id, id, extraJSON)
		if err := os.WriteFile(filepath.Join(themesDir, id, "theme.json"), []byte(mf), 0o644); err != nil {
			t.Fatalf("write theme.json %s: %v", id, err)
		}
		if err := os.WriteFile(filepath.Join(tDir, "index.html"), []byte(`{{define "content"}}<div data-journal="`+ver+`">ok</div>{{end}}`), 0o644); err != nil {
			t.Fatalf("write index.html %s: %v", id, err)
		}
	}
	seedLegacyTheme("volo-journal", "1.1.0", "Volo-go", "")
	seedLegacyTheme("community-minimal", "1.0.0", "Indie Author", "")
	seedLegacyTheme("explicit-none-theme", "1.0.0", "Volo-go", `, "update": {"strategy": "none"}`)

	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	realJournalZip, realJournalSHA := zipRealThemeDirectory(t, filepath.Join(repoRoot, "themes", "volo-journal"), "volo-journal")

	realCatalogBytes, err := os.ReadFile(filepath.Join(repoRoot, "themes", "catalog.json"))
	if err != nil {
		t.Fatalf("read real themes/catalog.json: %v", err)
	}
	var realCat struct {
		Themes []struct {
			ID      string `json:"id"`
			Version string `json:"version"`
		} `json:"themes"`
	}
	if err := json.Unmarshal(realCatalogBytes, &realCat); err != nil || len(realCat.Themes) == 0 {
		t.Fatalf("parse real themes/catalog.json: %v", err)
	}
	wantLatestVer := realCat.Themes[0].Version

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/themes/catalog.json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{
				"schema_version": 1,
				"themes": [
					{
						"id": "volo-journal",
						"version": %q,
						"min_host_version": "1.0.0",
						"download_url": "%s/releases/theme-volo-journal-v%s.zip",
						"checksum_url": "%s/releases/SHA256SUMS.txt",
						"changelog": {"zh-CN": "刊物主题升级", "en-US": "Journal theme upgrade"}
					}
				]
			}`, wantLatestVer, srv.URL, wantLatestVer, srv.URL)
		case "/releases/theme-volo-journal-v" + wantLatestVer + ".zip":
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(realJournalZip)
		case "/releases/SHA256SUMS.txt":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = fmt.Fprintf(w, "%s  theme-volo-journal.zip\n%s  theme-volo-journal-v%s.zip\n", realJournalSHA, realJournalSHA, wantLatestVer)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	eng, err := NewEngine(cat, themesDir)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	eng.SetHostVersion("1.0.3")
	eng.SetAllowLoopbackForTests(true)
	eng.SetOfficialCatalogURL(srv.URL + "/themes/catalog.json")

	// Verify capabilities: volo-journal (legacy v1.1.0 without update field) -> CapabilityOnline;
	// volo-studio (builtin), community-minimal, explicit-none-theme -> CapabilityNone!
	jLoc, ok := eng.GetLocalized("volo-journal", "zh-CN")
	if !ok || jLoc.UpdateCapability != extpkg.CapabilityOnline {
		t.Fatalf("expected legacy volo-journal to bootstrap CapabilityOnline, got ok=%v cap=%s", ok, jLoc.UpdateCapability)
	}
	sLoc, _ := eng.GetLocalized(DefaultThemeID, "zh-CN")
	cLoc, _ := eng.GetLocalized("community-minimal", "zh-CN")
	nLoc, _ := eng.GetLocalized("explicit-none-theme", "zh-CN")
	if sLoc.UpdateCapability != extpkg.CapabilityNone || cLoc.UpdateCapability != extpkg.CapabilityNone || nLoc.UpdateCapability != extpkg.CapabilityNone {
		t.Fatalf("expected volo-studio, 3rd-party & explicit none to stay CapabilityNone, got studio=%s comm=%s none=%s",
			sLoc.UpdateCapability, cLoc.UpdateCapability, nLoc.UpdateCapability)
	}

	batch, err := eng.CheckUpdates(ctx, nil, true, "zh-CN")
	if err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	if batch.Checked != 1 || batch.Updates != 1 || batch.Unsupported != 3 || batch.Errors != 0 {
		t.Fatalf("unexpected theme CheckUpdates batch: %+v", batch)
	}
	stJournal := batch.Statuses["volo-journal"]
	if !stJournal.HasUpdate || stJournal.LatestVersion != wantLatestVer || !stJournal.OnlineUpdatable {
		t.Fatalf("expected volo-journal %s online update, got %+v", wantLatestVer, stJournal)
	}

	// Upgrade legacy volo-journal v1.1.0 -> real repository v1.1.1 via SHA256SUMS.txt!
	upgradedMf, err := eng.UpgradeTheme(ctx, "volo-journal", "zh-CN")
	if err != nil {
		t.Fatalf("UpgradeTheme volo-journal: %v", err)
	}
	if upgradedMf.Version != wantLatestVer {
		t.Fatalf("expected upgraded volo-journal version %s, got %s", wantLatestVer, upgradedMf.Version)
	}
}

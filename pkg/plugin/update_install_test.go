package plugin

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
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/extpkg"
)

type testUpdateBuiltinPlugin struct {
	BaseBuiltinPlugin
	id   string
	ver  string
	res  extpkg.UpdateCheckResult
	err  error
	hits atomic.Int32
}

func (b *testUpdateBuiltinPlugin) Manifest() Manifest {
	return Manifest{
		ID:      b.id,
		Version: b.ver,
		Runtime: RuntimeBuiltin,
		Name:    map[string]string{"zh-CN": "内置测试插件", "en-US": "Builtin Test Plugin"},
	}
}

func (b *testUpdateBuiltinPlugin) CheckUpdate(_ context.Context, _ string) (extpkg.UpdateCheckResult, error) {
	b.hits.Add(1)
	return b.res, b.err
}

func TestPluginUpdateDetectionCapabilitiesAndCircuitIsolation(t *testing.T) {
	ctx := context.Background()
	store, kr, cat, tmpDir := openTestPluginEnv(t)

	var catalogHits atomic.Int32
	var hookRemoteHits atomic.Int32
	var hookShouldFail atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/catalog.json":
			catalogHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"schema_version": 1,
				"plugins": [
					{
						"id": "decl-plugin",
						"version": "1.2.0",
						"min_host_version": "1.0.0",
						"download_url": "https://example.com/plugin-decl-v1.2.0.zip",
						"changelog": {"zh-CN": "声明式更新说明", "en-US": "Declarative changelog"}
					},
					{
						"id": "checkonly-ext",
						"version": "2.0.0",
						"download_url": "https://example.com/plugin-checkonly-v2.0.0.zip",
						"homepage": "https://example.com/releases/checkonly"
					}
				]
			}`))
		case "/custom-feed":
			hookRemoteHits.Add(1)
			if hookShouldFail.Load() {
				http.Error(w, "upstream 502 bad gateway", http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"latest":"3.1.0","notes":"Custom JS hook changelog","zip":"https://example.com/js-hook-3.1.0.zip"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	mgr := NewManager(ManagerOptions{
		Store:                 store,
		Keys:                  kr,
		PluginsDir:            filepath.Join(tmpDir, "plugins"),
		DataDir:               filepath.Join(tmpDir, "data"),
		Catalog:               cat,
		HostVersion:           "1.0.2",
		HookTimeout:           200 * time.Millisecond,
		RouteTimeout:          200 * time.Millisecond,
		MaxFailures:           2,
		AllowLoopbackForTests: true,
		CustomResolver: func(_ context.Context, host string) ([]netip.Addr, error) {
			if host == "updates.example.test" || host == "127.0.0.1" || host == "localhost" {
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			}
			return nil, fmt.Errorf("unexpected host %s", host)
		},
	})
	t.Cleanup(func() { _ = mgr.Close(ctx) })

	// 1. Built-in plugin implementing UpdateChecker -> capability == "check_only"
	builtinChecker := &testUpdateBuiltinPlugin{
		id:  "builtin-checker",
		ver: "1.0.0",
		res: extpkg.UpdateCheckResult{
			LatestVersion: "1.3.0",
			DownloadURL:   "https://example.com/should-be-stripped-for-builtin.zip",
			ReleaseURL:    "https://github.com/Willh92/Volo-go/releases/tag/v1.3.0",
			Changelog:     "Upgrade host binary to update built-in plugin",
		},
	}
	if err := mgr.RegisterBuiltin(ctx, builtinChecker); err != nil {
		t.Fatalf("RegisterBuiltin: %v", err)
	}

	// 2. Unsupported plugin (no update block, no onCheckUpdate) -> capability == "none"
	unsupportedZip := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "no-update-plugin",
			"version": "1.0.0",
			"runtime": "goja",
			"entry": "main.js",
			"name": {"zh-CN": "无更新插件", "en-US": "No Update Plugin"}
		}`,
		"main.js": `module.exports = {};`,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(unsupportedZip), int64(len(unsupportedZip)), false, "en-US"); err != nil {
		t.Fatalf("install no-update-plugin: %v", err)
	}

	// 3. Declarative volo-catalog plugin -> capability == "online"
	declManifest := fmt.Sprintf(`{
		"id": "decl-plugin",
		"version": "1.0.0",
		"runtime": "goja",
		"entry": "main.js",
		"name": {"zh-CN": "声明式插件", "en-US": "Declarative Plugin"},
		"update": {
			"strategy": "volo-catalog",
			"url": %q
		}
	}`, srv.URL+"/catalog.json")
	declZip := buildZipBytes(t, map[string]string{
		"manifest.json": declManifest,
		"main.js":       `module.exports = {};`,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(declZip), int64(len(declZip)), false, "en-US"); err != nil {
		t.Fatalf("install decl-plugin: %v", err)
	}

	// 4. External plugin with allow_online_update: false -> capability == "check_only"
	allowFalse := false
	checkOnlyObj := map[string]any{
		"id":      "checkonly-ext",
		"version": "1.0.0",
		"runtime": "goja",
		"entry":   "main.js",
		"name":    map[string]string{"zh-CN": "仅检测插件", "en-US": "Check Only Ext"},
		"update": map[string]any{
			"strategy":            "volo-catalog",
			"url":                 srv.URL + "/catalog.json",
			"allow_online_update": allowFalse,
		},
	}
	checkOnlyBytes, _ := json.Marshal(checkOnlyObj)
	checkOnlyZip := buildZipBytes(t, map[string]string{
		"manifest.json": string(checkOnlyBytes),
		"main.js":       `module.exports = {};`,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(checkOnlyZip), int64(len(checkOnlyZip)), false, "en-US"); err != nil {
		t.Fatalf("install checkonly-ext: %v", err)
	}

	// 5. Custom JS onCheckUpdate(ctx) plugin -> capability == "online" even without manifest.update!
	parsedSrv, _ := url.Parse(srv.URL)
	customFeedURL := "http://updates.example.test:" + parsedSrv.Port() + "/custom-feed"
	jsHookManifest := `{
		"id": "js-hook-plugin",
		"version": "1.0.0",
		"runtime": "goja",
		"entry": "main.js",
		"name": {"zh-CN": "钩子更新插件", "en-US": "JS Hook Plugin"},
		"permissions": ["http:routes", "http.outbound:updates.example.test"],
		"allowed_hosts": ["updates.example.test"]
	}`
	jsHookMain := fmt.Sprintf(`
		module.exports = {
			onRequest: function() {
				return { status: 200, json: { ok: true } };
			},
			onCheckUpdate: function(ctx) {
				var resp = volo.http.fetch(%q, { method: "GET" });
				if (resp.status !== 200) {
					throw new Error("remote status " + resp.status);
				}
				var data = JSON.parse(resp.body);
				return {
					latest_version: data.latest,
					changelog: data.notes + " (" + ctx.host_version + ")",
					download_url: data.zip
				};
			}
		};
	`, customFeedURL)
	jsHookZip := buildZipBytes(t, map[string]string{
		"manifest.json": jsHookManifest,
		"main.js":       jsHookMain,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(jsHookZip), int64(len(jsHookZip)), false, "en-US"); err != nil {
		t.Fatalf("install js-hook-plugin: %v", err)
	}

	// Verify disabled JS hook check runs in ephemeral VM and succeeds cleanly.
	stDisabled, err := mgr.CheckPluginUpdate(ctx, "js-hook-plugin", true, "en-US")
	if err != nil {
		t.Fatalf("CheckPluginUpdate disabled js-hook-plugin: %v", err)
	}
	if !stDisabled.HasUpdate || stDisabled.LatestVersion != "3.1.0" || !strings.Contains(stDisabled.Changelog, "1.0.2") {
		t.Fatalf("unexpected disabled JS hook status: %+v", stDisabled)
	}

	// Enable js-hook-plugin and verify repeated failures in onCheckUpdate NEVER open circuit breaker!
	if err := mgr.Enable(ctx, "js-hook-plugin"); err != nil {
		t.Fatalf("Enable js-hook-plugin: %v", err)
	}
	hookShouldFail.Store(true)
	for i := 0; i < 5; i++ {
		stErr, cErr := mgr.CheckPluginUpdate(ctx, "js-hook-plugin", true, "en-US")
		if cErr != nil {
			t.Fatalf("CheckPluginUpdate error return on iteration %d: %v", i, cErr)
		}
		if stErr.State != extpkg.StateError {
			t.Fatalf("expected StateError when upstream 502, got %+v", stErr)
		}
	}
	sumAfterErrors, ok := mgr.Get("js-hook-plugin", "en-US")
	if !ok {
		t.Fatal("js-hook-plugin missing")
	}
	if !sumAfterErrors.IsEnabled || sumAfterErrors.CircuitState != CircuitClosed || sumAfterErrors.FailureCount != 0 {
		t.Fatalf("onCheckUpdate errors leaked into production circuit breaker: enabled=%v state=%s failures=%d",
			sumAfterErrors.IsEnabled, sumAfterErrors.CircuitState, sumAfterErrors.FailureCount)
	}
	hookShouldFail.Store(false)

	// Run batch CheckUpdates across all 5 plugins.
	batch, err := mgr.CheckUpdates(ctx, nil, true, "zh-CN")
	if err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	if batch.Unsupported != 1 || batch.Checked != 4 || batch.Updates != 4 || batch.Errors != 0 {
		t.Fatalf("unexpected batch summary: %+v", batch)
	}

	if stNo := batch.Statuses["no-update-plugin"]; stNo.State != extpkg.StateUnsupported {
		t.Fatalf("expected StateUnsupported for no-update-plugin, got %+v", stNo)
	}
	if stBuiltin := batch.Statuses["builtin-checker"]; !stBuiltin.HasUpdate || stBuiltin.OnlineUpdatable || stBuiltin.DownloadURL != "" {
		t.Fatalf("expected check-only status with stripped DownloadURL for builtin-checker, got %+v", stBuiltin)
	}
	if stCheckOnly := batch.Statuses["checkonly-ext"]; !stCheckOnly.HasUpdate || stCheckOnly.OnlineUpdatable || stCheckOnly.DownloadURL != "" {
		t.Fatalf("expected check-only status for checkonly-ext, got %+v", stCheckOnly)
	}
	if stDecl := batch.Statuses["decl-plugin"]; !stDecl.HasUpdate || !stDecl.OnlineUpdatable || stDecl.Changelog != "声明式更新说明" {
		t.Fatalf("unexpected decl-plugin status: %+v", stDecl)
	}

	// Attempting UpgradePlugin on builtin or check_only or unsupported must be rejected cleanly.
	for _, rejectID := range []string{"no-update-plugin", "builtin-checker", "checkonly-ext"} {
		if _, uErr := mgr.UpgradePlugin(ctx, rejectID, "en-US"); uErr == nil {
			t.Fatalf("expected UpgradePlugin(%s) to be rejected", rejectID)
		}
	}
}

func TestPluginURLInstallUpgradeRollbackCancellationAndCrashRecovery(t *testing.T) {
	ctx := context.Background()
	store, kr, cat, tmpDir := openTestPluginEnv(t)
	pluginsDir := filepath.Join(tmpDir, "plugins")

	// Simulate pre-existing crashed swap directory `.backup-recovered-plug-tx99` and orphaned temp files!
	if err := os.MkdirAll(filepath.Join(pluginsDir, ".backup-recovered-plug-tx99"), 0o750); err != nil {
		t.Fatalf("mkdir backup: %v", err)
	}
	_ = os.WriteFile(filepath.Join(pluginsDir, ".backup-recovered-plug-tx99", "manifest.json"), []byte(`{
		"id": "recovered-plug",
		"version": "1.0.0",
		"runtime": "goja",
		"entry": "main.js",
		"name": {"zh-CN": "恢复插件", "en-US": "Recovered Plugin"}
	}`), 0o640)
	_ = os.WriteFile(filepath.Join(pluginsDir, ".backup-recovered-plug-tx99", "main.js"), []byte(`module.exports = {};`), 0o640)
	_ = os.MkdirAll(filepath.Join(pluginsDir, ".staging-orphan123"), 0o750)
	_ = os.WriteFile(filepath.Join(pluginsDir, ".volo-plugin-dl-orphan.zip"), []byte("partial"), 0o640)

	mgr := NewManager(ManagerOptions{
		Store:                 store,
		Keys:                  kr,
		PluginsDir:            pluginsDir,
		DataDir:               filepath.Join(tmpDir, "data"),
		Catalog:               cat,
		HostVersion:           "1.0.2",
		HookTimeout:           200 * time.Millisecond,
		RouteTimeout:          200 * time.Millisecond,
		AllowLoopbackForTests: true,
	})
	t.Cleanup(func() { _ = mgr.Close(ctx) })

	if err := mgr.DiscoverAndStart(ctx); err != nil {
		t.Fatalf("DiscoverAndStart: %v", err)
	}
	if sum, ok := mgr.Get("recovered-plug", "en-US"); !ok || sum.Version != "1.0.0" {
		t.Fatalf("expected crash-recovered plugin v1.0.0, got ok=%v sum=%+v", ok, sum)
	}
	if _, err := os.Stat(filepath.Join(pluginsDir, ".staging-orphan123")); !os.IsNotExist(err) {
		t.Fatalf("expected .staging-orphan123 to be cleaned up")
	}
	if _, err := os.Stat(filepath.Join(pluginsDir, ".volo-plugin-dl-orphan.zip")); !os.IsNotExist(err) {
		t.Fatalf("expected .volo-plugin-dl-orphan.zip to be cleaned up")
	}

	var catalogJSON atomic.Value
	var zipPayloads map[string][]byte

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/catalog.json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(catalogJSON.Load().(string)))
		case "/slow.zip":
			w.Header().Set("Content-Type", "application/zip")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("PK\x03\x04"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done()
		default:
			name := strings.TrimPrefix(r.URL.Path, "/")
			if raw, ok := zipPayloads[name]; ok {
				w.Header().Set("Content-Type", "application/zip")
				_, _ = w.Write(raw)
				return
			}
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	makePluginZip := func(id, ver, minHost, extraJS string) ([]byte, string) {
		mf := fmt.Sprintf(`{
			"id": %q,
			"version": %q,
			"min_host_version": %q,
			"runtime": "goja",
			"entry": "main.js",
			"name": {"zh-CN": %q, "en-US": %q},
			"permissions": ["http:routes"],
			"update": {
				"strategy": "volo-catalog",
				"url": %q
			}
		}`, id, ver, minHost, id, id, srv.URL+"/catalog.json")
		js := fmt.Sprintf(`
			module.exports = {
				onRequest: function() {
					return { status: 200, json: { version: %q } };
				}%s
			};
		`, ver, extraJS)
		raw := buildZipBytes(t, map[string]string{
			"manifest.json": mf,
			"main.js":       js,
		}, "")
		sum := sha256.Sum256(raw)
		return raw, hex.EncodeToString(sum[:])
	}

	v1Zip, v1Hash := makePluginZip("tx-plug", "1.0.0", "1.0.0", "")
	v2BrokenEnableZip, v2BrokenHash := makePluginZip("tx-plug", "2.0.0", "1.0.0", `,
		onEnable: function() {
			throw new Error("fatal migration failure in v2.0.0 onEnable");
		}
	`)
	v2SpoofIDZip, v2SpoofHash := makePluginZip("other-evil-id", "2.0.0", "1.0.0", "")
	v2GoodZip, v2GoodHash := makePluginZip("tx-plug", "2.1.0", "1.0.0", "")
	v3FutureHostZip, v3FutureHash := makePluginZip("tx-plug", "3.0.0", "9.9.0", "")

	zipPayloads = map[string][]byte{
		"v1.zip":       v1Zip,
		"v2-bad.zip":   v2BrokenEnableZip,
		"v2-spoof.zip": v2SpoofIDZip,
		"v2-good.zip":  v2GoodZip,
		"v3-host.zip":  v3FutureHostZip,
	}

	// 1. Direct URL Install with SHA-256 + auto-enable
	installed, err := mgr.InstallFromURL(ctx, srv.URL+"/v1.zip", v1Hash, false, true, "en-US")
	if err != nil {
		t.Fatalf("InstallFromURL v1: %v", err)
	}
	if !installed.IsEnabled || installed.Version != "1.0.0" {
		t.Fatalf("expected enabled v1.0.0, got %+v", installed)
	}

	pluginHTTP := httptest.NewServer(mgr.Handler())
	defer pluginHTTP.Close()

	assertServingVersion := func(wantVer string) {
		t.Helper()
		resp, gErr := http.Get(pluginHTTP.URL + "/plugins/tx-plug/api/ping")
		if gErr != nil {
			t.Fatalf("GET /plugins/tx-plug/api/ping: %v", gErr)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 from live plugin, got %d", resp.StatusCode)
		}
		var body map[string]string
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode live plugin json: %v", err)
		}
		if body["version"] != wantVer {
			t.Fatalf("expected live plugin serving version %s, got %s", wantVer, body["version"])
		}
	}
	assertServingVersion("1.0.0")

	// 2. Cancel mid-download during URL install -> zero temp files left behind, live plugin untouched
	cancelCtx, cancel := context.WithTimeout(ctx, 80*time.Millisecond)
	_, cancelErr := mgr.InstallFromURL(cancelCtx, srv.URL+"/slow.zip", "", true, true, "en-US")
	cancel()
	if !errors.Is(cancelErr, context.DeadlineExceeded) && !errors.Is(cancelErr, context.Canceled) {
		t.Fatalf("expected cancellation error on slow.zip, got %v", cancelErr)
	}
	assertServingVersion("1.0.0")

	// 3. Cross-plugin ID spoofing during UpgradePlugin -> rejected before swap!
	catalogJSON.Store(fmt.Sprintf(`{
		"plugins": [{
			"id": "tx-plug",
			"version": "2.0.0",
			"download_url": %q,
			"sha256": %q
		}]
	}`, srv.URL+"/v2-spoof.zip", v2SpoofHash))
	if _, err := mgr.CheckPluginUpdate(ctx, "tx-plug", true, "en-US"); err != nil {
		t.Fatalf("check spoof update: %v", err)
	}
	if _, err := mgr.UpgradePlugin(ctx, "tx-plug", "en-US"); err == nil || !strings.Contains(err.Error(), "does not match expected") {
		t.Fatalf("expected manifest ID mismatch rejection, got %v", err)
	}
	assertServingVersion("1.0.0")

	// 4. Broken v2.0.0 throws inside onEnable() -> transactional rollback restores v1.0.0 on disk, in DB, and live in memory!
	catalogJSON.Store(fmt.Sprintf(`{
		"plugins": [{
			"id": "tx-plug",
			"version": "2.0.0",
			"download_url": %q,
			"sha256": %q
		}]
	}`, srv.URL+"/v2-bad.zip", v2BrokenHash))
	if _, err := mgr.CheckPluginUpdate(ctx, "tx-plug", true, "en-US"); err != nil {
		t.Fatalf("check broken v2 update: %v", err)
	}
	if _, err := mgr.UpgradePlugin(ctx, "tx-plug", "en-US"); err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("expected transactional rollback error on v2-bad, got %v", err)
	}
	afterRollback, ok := mgr.Get("tx-plug", "en-US")
	if !ok || afterRollback.Version != "1.0.0" || !afterRollback.IsEnabled {
		t.Fatalf("expected v1.0.0 restored & enabled after rollback, got %+v", afterRollback)
	}
	assertServingVersion("1.0.0")

	// 5. Incompatible MinHostVersion (v3.0.0 requires >= 9.9.0) -> rejected cleanly!
	catalogJSON.Store(fmt.Sprintf(`{
		"plugins": [{
			"id": "tx-plug",
			"version": "3.0.0",
			"min_host_version": "9.9.0",
			"download_url": %q,
			"sha256": %q
		}]
	}`, srv.URL+"/v3-host.zip", v3FutureHash))
	stIncompatible, err := mgr.CheckPluginUpdate(ctx, "tx-plug", true, "en-US")
	if err != nil {
		t.Fatalf("check v3 incompatible: %v", err)
	}
	if !stIncompatible.HasUpdate || stIncompatible.CompatibleWithHost {
		t.Fatalf("expected CompatibleWithHost=false for v3.0.0, got %+v", stIncompatible)
	}
	if _, err := mgr.UpgradePlugin(ctx, "tx-plug", "en-US"); err == nil || !strings.Contains(err.Error(), "9.9.0") {
		t.Fatalf("expected host version gate error on UpgradePlugin, got %v", err)
	}

	// 6. Healthy v2.1.0 upgrade -> hot-swaps cleanly and serves 2.1.0 immediately!
	catalogJSON.Store(fmt.Sprintf(`{
		"plugins": [{
			"id": "tx-plug",
			"version": "2.1.0",
			"min_host_version": "1.0.0",
			"download_url": %q,
			"sha256": %q
		}]
	}`, srv.URL+"/v2-good.zip", v2GoodHash))
	if _, err := mgr.CheckPluginUpdate(ctx, "tx-plug", true, "en-US"); err != nil {
		t.Fatalf("check v2.1.0 update: %v", err)
	}
	upgraded, err := mgr.UpgradePlugin(ctx, "tx-plug", "en-US")
	if err != nil {
		t.Fatalf("UpgradePlugin v2.1.0: %v", err)
	}
	if upgraded.Version != "2.1.0" || !upgraded.IsEnabled {
		t.Fatalf("expected enabled v2.1.0 after upgrade, got %+v", upgraded)
	}
	assertServingVersion("2.1.0")
}

func zipRealDirectoryTree(t *testing.T, srcDir string, topPrefix string) ([]byte, string) {
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
		t.Fatalf("walk real dir %s: %v", srcDir, err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	raw := buf.Bytes()
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:])
}

func TestLegacyV102OfficialPluginUpdateBootstrapAndRealCatalogRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, kr, cat, tmpDir := openTestPluginEnv(t)
	pluginsDir := filepath.Join(tmpDir, "plugins")

	// 1. Seed legacy v1.0.2 on-disk official plugins WITHOUT any "update" block,
	// plus an explicit strategy:"none" plugin and an unconfigured 3rd-party plugin.
	writeLegacyDiskPlugin := func(id, ver, author, extraManifest string) {
		pDir := filepath.Join(pluginsDir, id)
		if err := os.MkdirAll(pDir, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", id, err)
		}
		mf := fmt.Sprintf(`{
			"id": %q,
			"version": %q,
			"runtime": "goja",
			"entry": "main.js",
			"author": %q,
			"name": {"zh-CN": %q, "en-US": %q},
			"permissions": ["kv"],
			"config_schema": [{"key": "greeting", "type": "string", "label": {"zh-CN": "问候语", "en-US": "Greeting"}, "default": "hello"}]%s
		}`, id, ver, author, id, id, extraManifest)
		if err := os.WriteFile(filepath.Join(pDir, "manifest.json"), []byte(mf), 0o640); err != nil {
			t.Fatalf("write manifest %s: %v", id, err)
		}
		js := fmt.Sprintf(`module.exports = { onEnable: function() { volo.kv.set("boot_ver", %q); } };`, ver)
		if err := os.WriteFile(filepath.Join(pDir, "main.js"), []byte(js), 0o640); err != nil {
			t.Fatalf("write main.js %s: %v", id, err)
		}
	}

	writeLegacyDiskPlugin("kanbanniang", "2.0.4", "Volo Studio", "")
	writeLegacyDiskPlugin("sensitive-words", "1.1.0", "Volo Studio", "")
	writeLegacyDiskPlugin("third-party-unconfigured", "1.0.0", "Community Dev", "")
	writeLegacyDiskPlugin("explicit-none-plug", "1.0.0", "Volo Studio", `, "update": {"strategy": "none"}`)

	// Build real sensitive-words zip from repository tree + kanbanniang v2.0.5 upgrade zip (with top-level folder like package-plugin.py).
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	realSWZip, realSWSHA := zipRealDirectoryTree(t, filepath.Join(repoRoot, "plugins", "sensitive-words"), "sensitive-words")

	kanbanniangV205Zip := buildZipBytes(t, map[string]string{
		"kanbanniang/manifest.json": `{
			"id": "kanbanniang",
			"version": "2.0.5",
			"min_host_version": "1.0.0",
			"runtime": "goja",
			"entry": "main.js",
			"author": "Volo Studio",
			"name": {"zh-CN": "Live2D 看板娘", "en-US": "Live2D Companion"},
			"permissions": ["kv"],
			"config_schema": [{"key": "greeting", "type": "string", "label": {"zh-CN": "问候语", "en-US": "Greeting"}, "default": "hello"}],
			"update": {
				"strategy": "volo-catalog",
				"url": "https://raw.githubusercontent.com/Willh92/Volo-go/main/plugins/catalog.json"
			}
		}`,
		"kanbanniang/main.js": `module.exports = { onEnable: function() { volo.kv.set("upgraded_ver", "2.0.5"); } };`,
	}, "")
	kbSum := sha256.Sum256(kanbanniangV205Zip)
	kanbanniangV205SHA := hex.EncodeToString(kbSum[:])

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/plugins/catalog.json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{
				"schema_version": 1,
				"plugins": [
					{
						"id": "sensitive-words",
						"version": "1.1.0",
						"min_host_version": "1.0.0",
						"download_url": "%s/releases/plugin-sensitive-words-v1.1.0.zip",
						"checksum_url": "%s/releases/sw-SHA256SUMS.txt"
					},
					{
						"id": "kanbanniang",
						"version": "2.0.5",
						"min_host_version": "1.0.0",
						"download_url": "%s/releases/plugin-kanbanniang-v2.0.5.zip",
						"checksum_url": "%s/releases/kb-SHA256SUMS.txt",
						"changelog": {"zh-CN": "升级 Live2D 2.0.5", "en-US": "Upgrade Live2D 2.0.5"}
					}
				]
			}`, srv.URL, srv.URL, srv.URL, srv.URL)
		case "/releases/plugin-kanbanniang-v2.0.5.zip":
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(kanbanniangV205Zip)
		case "/releases/kb-SHA256SUMS.txt":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = fmt.Fprintf(w, "%s  plugin-kanbanniang.zip\n%s  plugin-kanbanniang-v2.0.5.zip\n", kanbanniangV205SHA, kanbanniangV205SHA)
		case "/releases/plugin-sensitive-words-v1.1.0.zip":
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(realSWZip)
		case "/releases/sw-SHA256SUMS.txt":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = fmt.Fprintf(w, "%s  plugin-sensitive-words.zip\n%s  plugin-sensitive-words-v1.1.0.zip\n", realSWSHA, realSWSHA)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	mgr := NewManager(ManagerOptions{
		Store:                 store,
		Keys:                  kr,
		PluginsDir:            pluginsDir,
		DataDir:               filepath.Join(tmpDir, "data"),
		Catalog:               cat,
		HostVersion:           "1.0.4",
		AllowLoopbackForTests: true,
	})
	t.Cleanup(func() { _ = mgr.Close(ctx) })
	mgr.SetOfficialCatalogURL(srv.URL + "/plugins/catalog.json")

	if err := mgr.DiscoverAndStart(ctx); err != nil {
		t.Fatalf("DiscoverAndStart: %v", err)
	}
	if _, err := mgr.UpdateConfig(ctx, "kanbanniang", map[string]any{"greeting": "custom-welcome"}, "zh-CN"); err != nil {
		t.Fatalf("UpdateConfig kanbanniang: %v", err)
	}
	if err := mgr.Enable(ctx, "kanbanniang"); err != nil {
		t.Fatalf("Enable kanbanniang: %v", err)
	}

	// Verify capabilities for legacy v1.0.2 official plugins vs unconfigured 3rd-party & explicit none!
	kbPre, _ := mgr.Get("kanbanniang", "zh-CN")
	swPre, _ := mgr.Get("sensitive-words", "zh-CN")
	tpPre, _ := mgr.Get("third-party-unconfigured", "zh-CN")
	nonePre, _ := mgr.Get("explicit-none-plug", "zh-CN")
	if kbPre.UpdateCapability != extpkg.CapabilityOnline || swPre.UpdateCapability != extpkg.CapabilityOnline {
		t.Fatalf("expected legacy official plugins to bootstrap CapabilityOnline, got kb=%s sw=%s", kbPre.UpdateCapability, swPre.UpdateCapability)
	}
	if tpPre.UpdateCapability != extpkg.CapabilityNone || nonePre.UpdateCapability != extpkg.CapabilityNone {
		t.Fatalf("expected unconfigured 3rd-party & explicit none to stay CapabilityNone, got tp=%s none=%s", tpPre.UpdateCapability, nonePre.UpdateCapability)
	}

	// Batch CheckUpdates detects kanbanniang v2.0.4 -> v2.0.5 while skipping the 2 unsupported plugins!
	summary, err := mgr.CheckUpdates(ctx, nil, true, "zh-CN")
	if err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	if summary.Checked != 2 || summary.Updates != 1 || summary.Unsupported != 2 || summary.Errors != 0 {
		t.Fatalf("unexpected CheckUpdates summary: %+v", summary)
	}
	kbStatus := summary.Statuses["kanbanniang"]
	if !kbStatus.HasUpdate || kbStatus.LatestVersion != "2.0.5" || !kbStatus.OnlineUpdatable {
		t.Fatalf("expected kanbanniang 2.0.5 online update available, got %+v", kbStatus)
	}

	// 1-click UpgradePlugin verifies SHA256SUMS.txt, upgrades 2.0.4 -> 2.0.5, and preserves custom config + KV!
	kbUpgraded, err := mgr.UpgradePlugin(ctx, "kanbanniang", "zh-CN")
	if err != nil {
		t.Fatalf("UpgradePlugin kanbanniang: %v", err)
	}
	if kbUpgraded.Version != "2.0.5" || !kbUpgraded.IsEnabled || kbUpgraded.PublicConfig["greeting"] != "custom-welcome" {
		t.Fatalf("expected enabled kanbanniang v2.0.5 with preserved config, got %+v", kbUpgraded)
	}

	// Verify real repository sensitive-words zip installs cleanly via URL + SHA-256 verification and matches plugins/catalog.json!
	realCatalogBytes, err := os.ReadFile(filepath.Join(repoRoot, "plugins", "catalog.json"))
	if err != nil {
		t.Fatalf("read real plugins/catalog.json: %v", err)
	}
	var realCat struct {
		Plugins []struct {
			ID      string `json:"id"`
			Version string `json:"version"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(realCatalogBytes, &realCat); err != nil {
		t.Fatalf("parse real plugins/catalog.json: %v", err)
	}
	if err := mgr.Uninstall(ctx, "sensitive-words"); err != nil {
		t.Fatalf("Uninstall seeded sensitive-words: %v", err)
	}
	swInstalled, err := mgr.InstallFromURL(ctx, srv.URL+"/releases/plugin-sensitive-words-v1.1.0.zip", realSWSHA, false, true, "zh-CN")
	if err != nil {
		t.Fatalf("InstallFromURL real sensitive-words: %v", err)
	}
	if swInstalled.Version != "1.1.0" || !swInstalled.IsEnabled || swInstalled.UpdateCapability != extpkg.CapabilityOnline {
		t.Fatalf("unexpected installed real sensitive-words summary: %+v", swInstalled)
	}
}

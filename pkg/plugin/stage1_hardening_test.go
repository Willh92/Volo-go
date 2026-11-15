package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/extpkg"
)

func TestStage1_DiscardAllKVBuffers_IsolatesRestoreAndReloadsBase(t *testing.T) {
	ctx := context.Background()
	store, _, _, _ := openTestPluginEnv(t)

	kv := NewKVStore(store, "kv-discard-plug")
	if err := kv.Set(ctx, "hits", "10", 0); err != nil {
		t.Fatalf("Set initial base: %v", err)
	}

	got1, err := kv.IncrBuffered(ctx, "hits", 5)
	if err != nil || got1 != 15 {
		t.Fatalf("IncrBuffered(5) = %d, %v (want 15)", got1, err)
	}
	if _, err := kv.IncrBuffered(ctx, "transient_only", 3); err != nil {
		t.Fatalf("IncrBuffered transient_only: %v", err)
	}

	// Simulate DB restore replacing underlying SQLite state while dirty KV buffer entries exist.
	if _, err := store.ExecContext(ctx, `UPDATE plugin_kv SET value_text = '100', value_int = 100 WHERE plugin_id = ? AND k = ?`, "kv-discard-plug", "hits"); err != nil {
		t.Fatalf("simulate restored DB row: %v", err)
	}

	DiscardAllKVBuffers(store)

	// FlushAllKV after DiscardAllKVBuffers must write zero rows (neither 15 nor transient_only).
	if err := FlushAllKV(ctx, store); err != nil {
		t.Fatalf("FlushAllKV after discard: %v", err)
	}
	dbVal, ok, err := kv.Get(ctx, "hits")
	if err != nil || !ok {
		t.Fatalf("read hits from DB: ok=%v err=%v", ok, err)
	}
	if dbVal != "100" {
		t.Fatalf("expected restored DB value 100 untouched after FlushAllKV, got %q", dbVal)
	}
	if _, exists, err := kv.Get(ctx, "transient_only"); err != nil || exists {
		t.Fatalf("expected transient_only not written to DB, exists=%v err=%v", exists, err)
	}

	// Subsequent IncrBuffered must reload fresh base (100) from DB!
	gotAfter, err := kv.IncrBuffered(ctx, "hits", 2)
	if err != nil || gotAfter != 102 {
		t.Fatalf("expected IncrBuffered after discard to reload DB base 100 -> 102, got %d (err=%v)", gotAfter, err)
	}
}

func TestStage1_ReloadAll_RediscoversDiskManifest_SkipsBackupResurrection_And_ClearsAssetCache(t *testing.T) {
	ctx := context.Background()
	store, kr, cat, tmpDir := openTestPluginEnv(t)
	pluginsDir := filepath.Join(tmpDir, "plugins")
	mgr := NewManager(ManagerOptions{
		Store:                 store,
		Keys:                  kr,
		Catalog:               cat,
		PluginsDir:            pluginsDir,
		DataDir:               filepath.Join(tmpDir, "data"),
		AllowLoopbackForTests: true,
	})
	defer func() { _ = mgr.Close(ctx) }()

	largeCSSV1 := strings.Repeat("/* v1-style */ body { color: #111; }\n", 20)
	zipV1 := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "reload-ext",
			"version": "1.0.0",
			"runtime": "goja",
			"entry": "main.js",
			"name": {"en-US": "Reload Ext v1"}
		}`,
		"main.js":          `module.exports = {};`,
		"assets/style.css": largeCSSV1,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zipV1), int64(len(zipV1)), false, "en-US"); err != nil {
		t.Fatalf("InstallFromZip v1: %v", err)
	}
	if err := mgr.Enable(ctx, "reload-ext"); err != nil {
		t.Fatalf("Enable v1: %v", err)
	}

	// Prime gzip asset cache and verify versioned vs unversioned Cache-Control + version-bearing ETag.
	reqUnver := httptest.NewRequest(http.MethodGet, "/plugins/reload-ext/assets/style.css", nil)
	reqUnver.Header.Set("Accept-Encoding", "gzip")
	recUnver := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(recUnver, reqUnver)
	if recUnver.Code != http.StatusOK {
		t.Fatalf("expected 200 on asset, got %d", recUnver.Code)
	}
	if cc := recUnver.Header().Get("Cache-Control"); cc != "public, max-age=300, must-revalidate" {
		t.Fatalf("expected must-revalidate Cache-Control without ?v=, got %q", cc)
	}
	etagV1 := recUnver.Header().Get("ETag")
	if !strings.Contains(etagV1, "1.0.0") {
		t.Fatalf("expected ETag to include manifest version 1.0.0, got %q", etagV1)
	}

	// Simulate full-site restore updating manifest.json & asset on disk AND leaving an orphan .backup-ghost-tx1 dir.
	if err := os.WriteFile(filepath.Join(pluginsDir, "reload-ext", "manifest.json"), []byte(`{
		"id": "reload-ext",
		"version": "1.2.0",
		"runtime": "goja",
		"entry": "main.js",
		"name": {"en-US": "Reload Ext v1.2"}
	}`), 0o644); err != nil {
		t.Fatalf("write updated manifest: %v", err)
	}
	ghostBackup := filepath.Join(pluginsDir, ".backup-ghostplug-tx99")
	if err := os.MkdirAll(ghostBackup, 0o755); err != nil {
		t.Fatalf("mkdir ghost backup: %v", err)
	}
	_ = os.WriteFile(filepath.Join(ghostBackup, "manifest.json"), []byte(`{"id":"ghostplug","version":"0.1.0","runtime":"goja","name":{"en-US":"Ghost"}}`), 0o644)
	_ = os.WriteFile(filepath.Join(ghostBackup, "main.js"), []byte(`module.exports = {};`), 0o644)

	if err := mgr.ReloadAll(ctx); err != nil {
		t.Fatalf("ReloadAll: %v", err)
	}
	sumAfter, ok := mgr.Get("reload-ext", "en-US")
	if !ok || sumAfter.Version != "1.2.0" || sumAfter.Name != "Reload Ext v1.2" {
		t.Fatalf("expected ReloadAll to re-parse manifest v1.2.0 from disk, got %+v", sumAfter)
	}
	if _, ghostLoaded := mgr.Get("ghostplug", "en-US"); ghostLoaded {
		t.Fatalf("ReloadAll MUST NOT call recoverInterruptedSwaps or resurrect .backup-* directories")
	}

	// Removing external plugin directory on disk + ReloadAll cleanly detaches it from memory.
	_ = os.RemoveAll(filepath.Join(pluginsDir, "reload-ext"))
	if err := mgr.ReloadAll(ctx); err != nil {
		t.Fatalf("ReloadAll after disk removal: %v", err)
	}
	if _, stillThere := mgr.Get("reload-ext", "en-US"); stillThere {
		t.Fatalf("expected removed external plugin to be detached on ReloadAll")
	}
}

func TestStage1_FreshInstallRollback_DisabledCronIsolation_ChecksumUpgrade_And_SingleflightCancel(t *testing.T) {
	ctx := context.Background()
	store, kr, cat, tmpDir := openTestPluginEnv(t)
	pluginsDir := filepath.Join(tmpDir, "plugins")
	mgr := NewManager(ManagerOptions{
		Store:                 store,
		Keys:                  kr,
		Catalog:               cat,
		PluginsDir:            pluginsDir,
		DataDir:               filepath.Join(tmpDir, "data"),
		HostVersion:           "1.0.0",
		HookTimeout:           2 * time.Second,
		AllowLoopbackForTests: true,
	})
	defer func() { _ = mgr.Close(ctx) }()

	// 1. Fresh install throwing in onInstall MUST roll back directory, DB row, KV, and memory!
	badFreshZip := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "fresh-fail",
			"version": "1.0.0",
			"runtime": "goja",
			"entry": "main.js",
			"name": {"en-US": "Fresh Fail"},
			"permissions": ["kv"]
		}`,
		"main.js": `module.exports = {
			onInstall: function() {
				volo.kv.set("dirty", "1", 0);
				throw new Error("boom in fresh onInstall");
			}
		};`,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(badFreshZip), int64(len(badFreshZip)), false, "en-US"); err == nil {
		t.Fatalf("expected fresh install with failing onInstall to fail and roll back")
	}
	if _, exists := mgr.Get("fresh-fail", "en-US"); exists {
		t.Fatalf("expected fresh-fail removed from manager after rollback")
	}
	if _, statErr := os.Stat(filepath.Join(pluginsDir, "fresh-fail")); !os.IsNotExist(statErr) {
		t.Fatalf("expected fresh-fail directory removed on rollback, statErr=%v", statErr)
	}
	type cntRow struct {
		Cnt int `db:"cnt"`
	}
	var cnt cntRow
	_ = sqlx.GetInto(ctx, store, &cnt, `SELECT COUNT(*) AS cnt FROM plugins WHERE id = ?`, "fresh-fail")
	if cnt.Cnt != 0 {
		t.Fatalf("expected 0 DB rows for fresh-fail after rollback, got %d", cnt.Cnt)
	}

	// 2. Disabled install calling volo.cron.register inside onInstall MUST NOT register live cron jobs!
	disabledCronZip := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "cron-iso",
			"version": "1.0.0",
			"runtime": "goja",
			"entry": "main.js",
			"name": {"en-US": "Cron Isolation"},
			"permissions": ["cron"]
		}`,
		"main.js": `module.exports = {
			onInstall: function() {
				if (volo.cron) {
					volo.cron.register("sneaky", { handler: "tick", interval_seconds: 10 });
				}
			},
			tick: function() {}
		};`,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(disabledCronZip), int64(len(disabledCronZip)), false, "en-US"); err != nil {
		t.Fatalf("InstallFromZip cron-iso: %v", err)
	}
	if jobs := mgr.PluginCronJobs("cron-iso"); len(jobs) != 0 {
		t.Fatalf("expected 0 live cron jobs for disabled plugin after onInstall, got %+v", jobs)
	}

	// 3. ChecksumURL auto-verification + downgrade protection + singleflight cancel isolation!
	var checkSlow atomic.Bool
	var servedLatestVer atomic.Value
	servedLatestVer.Store("1.1.0")
	var v2Zip []byte
	var v2SHA string
	var downgradeZip []byte
	var downgradeSHA string

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/catalog.json":
			if checkSlow.Load() {
				time.Sleep(120 * time.Millisecond)
			}
			ver := servedLatestVer.Load().(string)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"schema_version": 1,
				"items": [{
					"id": "auto-sha",
					"version": "` + ver + `",
					"download_url": "` + srv.URL + `/plugin-auto-sha-v` + ver + `.zip",
					"checksum_url": "` + srv.URL + `/SHA256SUMS.txt"
				}]
			}`))
		case "/SHA256SUMS.txt":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(
				v2SHA + "  plugin-auto-sha-v1.1.0.zip\n" +
					downgradeSHA + "  plugin-auto-sha-v2.0.0.zip\n",
			))
		case "/plugin-auto-sha-v1.1.0.zip":
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(v2Zip)
		case "/plugin-auto-sha-v2.0.0.zip":
			// Malicious/misconfigured asset claiming 2.0.0 in catalog but shipping v1.0.0 inside zip!
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(downgradeZip)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	mkAutoShaZip := func(ver string) ([]byte, string) {
		zb := buildZipBytes(t, map[string]string{
			"manifest.json": `{
				"id": "auto-sha",
				"version": "` + ver + `",
				"runtime": "goja",
				"entry": "main.js",
				"name": {"en-US": "Auto SHA"},
				"update": {"strategy": "` + extpkg.StrategyVoloCatalog + `", "url": "` + srv.URL + `/catalog.json"}
			}`,
			"main.js": `module.exports = {};`,
		}, "")
		sum := sha256.Sum256(zb)
		return zb, hex.EncodeToString(sum[:])
	}

	v1Zip, v1SHA := mkAutoShaZip("1.0.0")
	v2Zip, v2SHA = mkAutoShaZip("1.1.0")
	downgradeZip, downgradeSHA = mkAutoShaZip("1.0.0")

	if _, err := mgr.InstallFromURL(ctx, srv.URL+"/plugin-auto-sha-v1.1.0.zip", v2SHA, false, false, "en-US"); err != nil {
		// Install v1 first via zip so we test upgrade from 1.0.0 -> 1.1.0.
		t.Fatalf("unexpected setup error: %v", err)
	}
	_ = mgr.Uninstall(ctx, "auto-sha")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(v1Zip), int64(len(v1Zip)), false, "en-US"); err != nil {
		t.Fatalf("install v1: %v (sha=%s)", err, v1SHA)
	}

	// Singleflight cancellation isolation: impatient caller 1 cancels after 15ms while caller 2 awaits completion.
	checkSlow.Store(true)
	c1Ctx, c1Cancel := context.WithTimeout(ctx, 15*time.Millisecond)
	defer c1Cancel()

	var wg sync.WaitGroup
	var st2 extpkg.UpdateStatus
	var err2 error
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(5 * time.Millisecond)
		st2, err2 = mgr.CheckPluginUpdate(ctx, "auto-sha", true, "en-US")
	}()
	_, err1 := mgr.CheckPluginUpdate(c1Ctx, "auto-sha", true, "en-US")
	if err1 == nil {
		t.Fatalf("expected caller 1 context deadline error")
	}
	wg.Wait()
	checkSlow.Store(false)
	if err2 != nil || !st2.HasUpdate || st2.ChecksumURL == "" {
		t.Fatalf("expected caller 2 to succeed with ChecksumURL despite caller 1 cancel: st=%+v err=%v", st2, err2)
	}

	// UpgradePlugin must auto-fetch SHA256SUMS.txt via ChecksumURL and verify v1.1.0!
	upSum, err := mgr.UpgradePlugin(ctx, "auto-sha", "en-US")
	if err != nil || upSum.Version != "1.1.0" {
		t.Fatalf("expected UpgradePlugin with auto ChecksumURL to reach 1.1.0, got sum=%+v err=%v", upSum, err)
	}

	// Catalog advertises 2.0.0, but downloaded zip actually contains manifest version 1.0.0 -> disallowDowngrade MUST reject!
	servedLatestVer.Store("2.0.0")
	if _, err := mgr.CheckPluginUpdate(ctx, "auto-sha", true, "en-US"); err != nil {
		t.Fatalf("check 2.0.0: %v", err)
	}
	if _, err := mgr.UpgradePlugin(ctx, "auto-sha", "en-US"); err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("expected disallowDowngrade rejection when zip contains 1.0.0 <= 1.1.0, got %v", err)
	}
}

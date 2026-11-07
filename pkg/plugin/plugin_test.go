package plugin

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/backup"
	"github.com/Willh92/Volo-go/internal/config"
	"github.com/Willh92/Volo-go/internal/db/migrate"
	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/i18n"
)

func openTestPluginEnv(t *testing.T) (*sqlx.Store, *config.KeyRing, *i18n.Catalog, string) {
	t.Helper()
	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "volo-plugin-test.db")
	store, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    dbPath,
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := migrate.Up(ctx, store); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	kr, _, err := config.LoadOrInitKeyRing("unit-test-master-secret-key-2026", tmpDir)
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	cat, err := i18n.NewCatalog("zh-CN", "")
	if err != nil {
		t.Fatalf("i18n catalog: %v", err)
	}
	return store, kr, cat, tmpDir
}

func buildZipBytes(t *testing.T, files map[string]string, symlinkName string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		hdr := &zip.FileHeader{
			Name:   name,
			Method: zip.Deflate,
		}
		hdr.SetMode(0o640)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if symlinkName != "" {
		hdr := &zip.FileHeader{
			Name:   symlinkName,
			Method: zip.Store,
		}
		hdr.SetMode(os.ModeSymlink | 0o777)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatalf("zip symlink: %v", err)
		}
		_, _ = w.Write([]byte("/etc/passwd"))
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// 1. Malicious / Faulty Plugin Chaos Suite:
// - Infinite `while(true){}` loop interrupted within millisecond timeout without goroutine leaks
// - Stack overflow recursion caught cleanly
// - Thrown JS Error isolated
// - Consecutive failures trip CIRCUIT_OPEN, auto-disable plugin in memory & DB, and write JSONL logs
func TestChaosSandboxInfiniteLoopStackOverflowAndCircuitBreaker(t *testing.T) {
	ctx := context.Background()
	store, kr, cat, tmpDir := openTestPluginEnv(t)

	mgr := NewManager(ManagerOptions{
		Store:        store,
		Keys:         kr,
		PluginsDir:   filepath.Join(tmpDir, "plugins"),
		DataDir:      filepath.Join(tmpDir, "data"),
		Catalog:      cat,
		HookTimeout:  45 * time.Millisecond,
		RouteTimeout: 60 * time.Millisecond,
		MaxFailures:  3,
	})
	t.Cleanup(func() { _ = mgr.Close(ctx) })

	manifestJSON := `{
		"id": "chaos-monkey",
		"version": "1.0.0",
		"runtime": "goja",
		"entry": "main.js",
		"name": {"zh-CN": "混沌测试插件", "en-US": "Chaos Monkey"},
		"permissions": ["http:routes", "hooks:*"]
	}`
	mainJS := `
		function recurseForever(n) { return recurseForever(n + 1) + 1; }
		module.exports = {
			onRequest: function(req) {
				var mode = req.query.mode || "ok";
				if (mode === "loop") {
					while (true) {}
				}
				if (mode === "stack") {
					return { status: 200, body: String(recurseForever(0)) };
				}
				if (mode === "throw") {
					throw new Error("deliberate plugin crash");
				}
				return { status: 200, json: { alive: true } };
			}
		};
	`
	zipData := buildZipBytes(t, map[string]string{
		"chaos-monkey/manifest.json": manifestJSON,
		"chaos-monkey/main.js":       mainJS,
	}, "")

	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zipData), int64(len(zipData)), false, "en-US"); err != nil {
		t.Fatalf("InstallFromZip failed: %v", err)
	}
	if err := mgr.Enable(ctx, "chaos-monkey"); err != nil {
		t.Fatalf("Enable failed: %v", err)
	}

	srv := httptest.NewServer(mgr.Handler())
	defer srv.Close()

	baseGoroutines := runtime.NumGoroutine()

	// Healthy call succeeds and resets failure count.
	resp, err := http.Get(srv.URL + "/plugins/chaos-monkey/api/ping?mode=ok")
	if err != nil {
		t.Fatalf("GET ok: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	// Failure #1: Infinite while(true){} loop -> interrupted cleanly in ~60ms!
	start := time.Now()
	resp1, err := http.Get(srv.URL + "/plugins/chaos-monkey/api/ping?mode=loop")
	if err != nil {
		t.Fatalf("GET loop: %v", err)
	}
	resp1.Body.Close()
	elapsed := time.Since(start)
	if resp1.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on infinite loop timeout, got %d", resp1.StatusCode)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("infinite loop was not interrupted promptly (took %v)", elapsed)
	}

	// Failure #2: Infinite recursion -> caught by SetMaxCallStackSize(512).
	resp2, err := http.Get(srv.URL + "/plugins/chaos-monkey/api/ping?mode=stack")
	if err != nil {
		t.Fatalf("GET stack: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on stack overflow, got %d", resp2.StatusCode)
	}

	// Failure #3: Explicit throw -> reaches MaxFailures=3 -> trips CIRCUIT_OPEN and auto-disables!
	resp3, err := http.Get(srv.URL + "/plugins/chaos-monkey/api/ping?mode=throw")
	if err != nil {
		t.Fatalf("GET throw: %v", err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on thrown error, got %d", resp3.StatusCode)
	}

	summary, ok := mgr.Get("chaos-monkey", "en-US")
	if !ok {
		t.Fatal("expected chaos-monkey summary")
	}
	if summary.CircuitState != CircuitOpen || summary.IsEnabled {
		t.Fatalf("expected CIRCUIT_OPEN and IsEnabled=false, got state=%q enabled=%v count=%d",
			summary.CircuitState, summary.IsEnabled, summary.FailureCount)
	}

	// Subsequent requests return 404 immediately while circuit is open.
	resp4, err := http.Get(srv.URL + "/plugins/chaos-monkey/api/ping?mode=ok")
	if err != nil {
		t.Fatalf("GET after circuit open: %v", err)
	}
	resp4.Body.Close()
	if resp4.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 when plugin is circuit-open, got %d", resp4.StatusCode)
	}

	// Enabling without ResetCircuit must be rejected.
	if err := mgr.Enable(ctx, "chaos-monkey"); err == nil {
		t.Fatal("expected Enable to fail while circuit is OPEN")
	}

	// ResetCircuit + Enable restores service.
	if err := mgr.ResetCircuit(ctx, "chaos-monkey"); err != nil {
		t.Fatalf("ResetCircuit: %v", err)
	}
	if err := mgr.Enable(ctx, "chaos-monkey"); err != nil {
		t.Fatalf("Enable after ResetCircuit: %v", err)
	}
	resp5, err := http.Get(srv.URL + "/plugins/chaos-monkey/api/ping?mode=ok")
	if err != nil {
		t.Fatalf("GET after recovery: %v", err)
	}
	resp5.Body.Close()
	if resp5.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after circuit reset, got %d", resp5.StatusCode)
	}

	// Verify isolated JSONL plugin logs captured errors and circuit open event.
	logs, err := mgr.Logger().Tail("chaos-monkey", 50)
	if err != nil {
		t.Fatalf("Tail logs: %v", err)
	}
	foundCircuitLog := false
	for _, l := range logs {
		if l.Event == "circuit" && strings.Contains(l.Message, "CIRCUIT_OPEN") {
			foundCircuitLog = true
		}
	}
	if !foundCircuitLog {
		t.Fatalf("expected CIRCUIT_OPEN entry in plugin JSONL log, got %+v", logs)
	}

	// Allow brief goroutine settle and verify zero goroutine leak.
	time.Sleep(30 * time.Millisecond)
	afterGoroutines := runtime.NumGoroutine()
	if afterGoroutines > baseGoroutines+6 {
		t.Fatalf("unexpected goroutine growth: before=%d after=%d", baseGoroutines, afterGoroutines)
	}
}

// 2. Capability Sandbox & Outbound SSRF Firewall Tests:
// - Undeclared permissions (`kv`) are NOT injected into `volo` object (`undefined`)
// - Outbound fetch to cloud metadata `169.254.169.254`, loopback `127.0.0.1`, DNS-rebound internal IP,
//   and non-allowlisted host `evil.example.com` are 100% blocked!
func TestCapabilitySandboxAndSSRFFirewall(t *testing.T) {
	ctx := context.Background()
	store, kr, cat, tmpDir := openTestPluginEnv(t)

	mgr := NewManager(ManagerOptions{
		Store:      store,
		Keys:       kr,
		PluginsDir: filepath.Join(tmpDir, "plugins"),
		DataDir:    filepath.Join(tmpDir, "data"),
		Catalog:    cat,
		CustomResolver: func(_ context.Context, host string) ([]netip.Addr, error) {
			if host == "rebind.allowed.example" {
				// Simulate DNS rebinding to AWS/cloud metadata IP 169.254.169.254!
				return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
			}
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		},
	})
	t.Cleanup(func() { _ = mgr.Close(ctx) })

	manifestJSON := `{
		"id": "net-probe",
		"version": "1.0.0",
		"runtime": "goja",
		"entry": "main.js",
		"name": {"en-US": "Net Probe"},
		"permissions": ["http:routes", "http.outbound:*.allowed.example"]
	}`
	mainJS := `
		module.exports = {
			onRequest: function(req) {
				if (req.path === "/check-caps") {
					return {
						status: 200,
						json: {
							hasKV: typeof volo.kv !== "undefined",
							hasHTTP: typeof volo.http !== "undefined"
						}
					};
				}
				var target = req.query.url;
				try {
					var res = volo.http.fetch(target, { method: "GET" });
					return { status: 200, json: { blocked: false, status: res.status } };
				} catch (e) {
					return { status: 200, json: { blocked: true, error: String(e) } };
				}
			}
		};
	`
	zipData := buildZipBytes(t, map[string]string{
		"manifest.json": manifestJSON,
		"main.js":       mainJS,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zipData), int64(len(zipData)), false, "en-US"); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := mgr.Enable(ctx, "net-probe"); err != nil {
		t.Fatalf("enable: %v", err)
	}

	srv := httptest.NewServer(mgr.Handler())
	defer srv.Close()

	// Verify undeclared `kv` is undefined while declared `http` is present.
	capResp, err := http.Get(srv.URL + "/plugins/net-probe/api/check-caps")
	if err != nil {
		t.Fatalf("check-caps: %v", err)
	}
	var caps struct {
		HasKV   bool `json:"hasKV"`
		HasHTTP bool `json:"hasHTTP"`
	}
	_ = json.NewDecoder(capResp.Body).Decode(&caps)
	capResp.Body.Close()
	if caps.HasKV {
		t.Fatal("expected volo.kv to be undefined when 'kv' permission is not declared")
	}
	if !caps.HasHTTP {
		t.Fatal("expected volo.http to be injected when http.outbound:<host> is declared")
	}

	// Verify SSRF & host allowlist blocks all forbidden targets.
	ssrfTargets := []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://127.0.0.1:8080/healthz",
		"https://evil.unlisted.example/steal",
		"http://rebind.allowed.example/internal",
		"file:///etc/passwd",
	}
	for _, target := range ssrfTargets {
		r, gErr := http.Get(srv.URL + "/plugins/net-probe/api/fetch?url=" + target)
		if gErr != nil {
			t.Fatalf("fetch %s: %v", target, gErr)
		}
		var out struct {
			Blocked bool   `json:"blocked"`
			Error   string `json:"error"`
		}
		_ = json.NewDecoder(r.Body).Decode(&out)
		r.Body.Close()
		if !out.Blocked {
			t.Fatalf("SSRF breach: target %q was NOT blocked!", target)
		}
	}
}

// 3. Zip Slip, Symlink Escape, Asset Traversal, Persistent PJAX Slot & Encrypted Secret Config Tests
func TestZipSlipAssetTraversalPersistentSlotAndEncryptedConfig(t *testing.T) {
	ctx := context.Background()
	store, kr, cat, tmpDir := openTestPluginEnv(t)

	mgr := NewManager(ManagerOptions{
		Store:      store,
		Keys:       kr,
		PluginsDir: filepath.Join(tmpDir, "plugins"),
		DataDir:    filepath.Join(tmpDir, "data"),
		Catalog:    cat,
	})
	t.Cleanup(func() { _ = mgr.Close(ctx) })

	// 3a. Reject Zip Slip (`../../.env`)
	evilZip := buildZipBytes(t, map[string]string{
		"../../.env":    "PWNED=1",
		"manifest.json": `{"id":"evil","version":"1.0.0","runtime":"goja","name":{"en-US":"Evil"}}`,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(evilZip), int64(len(evilZip)), false, "en-US"); err == nil {
		t.Fatal("expected Zip Slip archive to be rejected")
	}

	// 3b. Reject symlink inside ZIP
	symZip := buildZipBytes(t, map[string]string{
		"manifest.json": `{"id":"sym","version":"1.0.0","runtime":"goja","name":{"en-US":"Sym"}}`,
		"main.js":       `module.exports = {};`,
	}, "assets/leak.txt")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(symZip), int64(len(symZip)), false, "en-US"); err == nil {
		t.Fatal("expected ZIP with symlink entry to be rejected")
	}

	// 3c. Install valid plugin with persistent footer slot, static assets, and secret config field
	manifestJSON := `{
		"id": "mascot-demo",
		"version": "1.2.0",
		"runtime": "goja",
		"entry": "main.js",
		"name": {"zh-CN": "吉祥物挂件", "en-US": "Mascot Widget"},
		"permissions": ["slots:public.footer", "http:routes"],
		"ui_slots": [
			{
				"slot": "public.footer",
				"persist": true,
				"style_asset": "widget.css",
				"script_asset": "widget.js",
				"dynamic": true
			}
		],
		"config_schema": [
			{
				"key": "model_name",
				"type": "select",
				"default": "pio",
				"options": [
					{"value": "pio", "label": {"zh-CN": "Pio", "en-US": "Pio"}},
					{"value": "tia", "label": {"zh-CN": "Tia", "en-US": "Tia"}}
				],
				"label": {"zh-CN": "默认模型", "en-US": "Default Model"}
			},
			{
				"key": "api_token",
				"type": "string",
				"secret": true,
				"label": {"zh-CN": "私有密钥", "en-US": "API Secret"}
			}
		]
	}`
	mainJS := `
		module.exports = {
			renderSlot: function(slot, sctx) {
				return '<div class="mascot-root" data-model="' + volo.config.get("model_name") + '"></div>';
			},
			onRequest: function(req) {
				return {
					status: 200,
					json: {
						model: volo.config.get("model_name"),
						hasSecretInRuntime: Boolean(volo.config.get("api_token"))
					}
				};
			}
		};
	`
	validZip := buildZipBytes(t, map[string]string{
		"manifest.json":     manifestJSON,
		"main.js":           mainJS,
		"assets/widget.css": ".mascot-root { position: fixed; }",
		"assets/widget.js":  "console.log('mascot ready');",
	}, "")

	sum, err := mgr.InstallFromZip(ctx, bytes.NewReader(validZip), int64(len(validZip)), false, "zh-CN")
	if err != nil {
		t.Fatalf("install valid plugin: %v", err)
	}
	if sum.Name != "吉祥物挂件" {
		t.Fatalf("expected localized zh-CN name, got %q", sum.Name)
	}
	if err := mgr.Enable(ctx, "mascot-demo"); err != nil {
		t.Fatalf("enable mascot-demo: %v", err)
	}

	// Update secret + public config, then verify partial PATCH preserves secret without exposing plaintext in summary!
	sum1, err := mgr.UpdateConfig(ctx, "mascot-demo", map[string]any{
		"model_name": "tia",
		"api_token":  "super-secret-mascot-token",
	}, "en-US")
	if err != nil {
		t.Fatalf("UpdateConfig 1: %v", err)
	}
	if !sum1.SecretKeysSet["api_token"] {
		t.Fatal("expected SecretKeysSet[api_token]=true")
	}
	if _, leaked := sum1.PublicConfig["api_token"]; leaked {
		t.Fatal("secret field leaked into PublicConfig!")
	}

	// Partial PATCH of model_name with omitted api_token must NOT wipe the saved encrypted secret.
	sum2, err := mgr.UpdateConfig(ctx, "mascot-demo", map[string]any{
		"model_name": "pio",
	}, "en-US")
	if err != nil {
		t.Fatalf("UpdateConfig 2: %v", err)
	}
	if !sum2.SecretKeysSet["api_token"] || sum2.PublicConfig["model_name"] != "pio" {
		t.Fatalf("partial PATCH corrupted state: %+v", sum2)
	}

	// Verify DB row stores AES-GCM envelope ("enc:v1:..."), never raw secret!
	var rawCipher string
	if err := store.QueryRowContext(ctx, `SELECT secret_ciphertext FROM plugins WHERE id = ?`, "mascot-demo").Scan(&rawCipher); err != nil {
		t.Fatalf("query secret_ciphertext: %v", err)
	}
	if !strings.HasPrefix(rawCipher, "enc:v1:") || strings.Contains(rawCipher, "super-secret-mascot-token") {
		t.Fatalf("expected AES-256-GCM enc:v1: envelope in DB, got %q", rawCipher)
	}

	// Verify persistent slot rendering (`data-volo-persist="mascot-demo"`).
	slots := mgr.RenderSlots(ctx, SlotPublicFooter, SlotRenderContext{Locale: "zh-CN", Path: "/"})
	if len(slots) != 1 || !slots[0].Persist || !strings.Contains(slots[0].HTML, `data-volo-persist="mascot-demo"`) || !strings.Contains(slots[0].HTML, `data-model="pio"`) {
		t.Fatalf("unexpected slot output: %+v", slots)
	}

	// Verify asset serving + symlink & path traversal defense!
	secretOutsideFile := filepath.Join(tmpDir, "outside-secret.txt")
	_ = os.WriteFile(secretOutsideFile, []byte("TOP_SECRET_ENV"), 0o600)
	_ = os.Symlink(secretOutsideFile, filepath.Join(tmpDir, "plugins", "mascot-demo", "assets", "escape.txt"))

	srv := httptest.NewServer(mgr.Handler())
	defer srv.Close()

	cssResp, err := http.Get(srv.URL + "/plugins/mascot-demo/assets/widget.css")
	if err != nil || cssResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on valid asset widget.css, got err=%v status=%d", err, cssResp.StatusCode)
	}
	cssResp.Body.Close()
	if cssResp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing X-Content-Type-Options: nosniff on plugin asset")
	}

	for _, attackPath := range []string{
		"/plugins/mascot-demo/assets/../../.env",
		"/plugins/mascot-demo/assets/%2e%2e/%2e%2e/outside-secret.txt",
		"/plugins/mascot-demo/assets/escape.txt",
		"/plugins/mascot-demo/assets/../main.js",
	} {
		r, aErr := http.Get(srv.URL + attackPath)
		if aErr != nil {
			t.Fatalf("GET %s: %v", attackPath, aErr)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusNotFound {
			t.Fatalf("path traversal / symlink escape %q was NOT blocked (status=%d)!", attackPath, r.StatusCode)
		}
	}

	// Uninstall cleans up everything.
	if err := mgr.Uninstall(ctx, "mascot-demo"); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, exists := mgr.Get("mascot-demo", "en-US"); exists {
		t.Fatal("expected mascot-demo to be removed after Uninstall")
	}
}

// 4. plugin_kv Concurrency, TTL Expiry, and Native Backup/Restore Consistency
func TestPluginKVAtomicConcurrencyTTLAndBackupRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, _, _, tmpDir := openTestPluginEnv(t)

	kv := NewKVStore(store, "counter-plugin")
	now := time.Unix(1770000000, 0)
	kv.nowFn = func() time.Time { return now }

	// Concurrent atomic IncrBy: 20 goroutines * 5 increments = exact 100.
	var wg sync.WaitGroup
	for g := 0; g < 20; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				if _, err := kv.IncrBy(ctx, "hits:total", 1, 0); err != nil {
					t.Errorf("IncrBy error: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	val, ok, err := kv.Get(ctx, "hits:total")
	if err != nil || !ok || val != "100" {
		t.Fatalf("expected atomic counter 100, got val=%q ok=%v err=%v", val, ok, err)
	}

	// TTL test: set key with 10s TTL, advance clock +15s -> invisible on Get, and IncrBy resets from 0!
	if err := kv.Set(ctx, "ephemeral:token", "abc", 10); err != nil {
		t.Fatalf("Set TTL: %v", err)
	}
	if _, err := kv.IncrBy(ctx, "ephemeral:counter", 50, 10); err != nil {
		t.Fatalf("IncrBy TTL: %v", err)
	}

	now = now.Add(15 * time.Second)
	if _, ok, _ := kv.Get(ctx, "ephemeral:token"); ok {
		t.Fatal("expected expired key to be invisible on Get")
	}
	resetVal, err := kv.IncrBy(ctx, "ephemeral:counter", 3, 60)
	if err != nil || resetVal != 3 {
		t.Fatalf("expected expired counter to reset to 3, got %d (err=%v)", resetVal, err)
	}

	// Export native backup ZIP and restore into a brand-new SQLite database!
	zipPath := filepath.Join(tmpDir, "plugin-kv-backup.zip")
	if _, err := backup.CreateToFile(ctx, store, zipPath, backup.CreateOptions{}); err != nil {
		t.Fatalf("backup.CreateToFile: %v", err)
	}

	store2, err := sqlx.Open(ctx, sqlx.Options{
		Driver: "sqlite",
		DSN:    filepath.Join(tmpDir, "restored.db"),
	})
	if err != nil {
		t.Fatalf("open restored sqlite: %v", err)
	}
	defer store2.Close()
	if err := migrate.Up(ctx, store2); err != nil {
		t.Fatalf("migrate restored sqlite: %v", err)
	}
	if _, err := backup.RestoreFromFile(ctx, store2, zipPath, backup.RestoreOptions{}); err != nil {
		t.Fatalf("backup.RestoreFromFile: %v", err)
	}

	kv2 := NewKVStore(store2, "counter-plugin")
	kv2.nowFn = func() time.Time { return now }
	restoredHits, ok, err := kv2.Get(ctx, "hits:total")
	if err != nil || !ok || restoredHits != "100" {
		t.Fatalf("expected hits:total=100 after backup restore, got %q ok=%v err=%v", restoredHits, ok, err)
	}
}

// 5. 10th Privacy Surface Redaction & Provider Hot-Swap Fallback Tests
func TestPrivacySurfaceRedactionAndProviderHotSwap(t *testing.T) {
	ctx := context.Background()
	store, kr, cat, tmpDir := openTestPluginEnv(t)

	defaultSpam := &stubAntiSpam{verdict: SpamVerdict{Action: SpamActionModerate, Reason: "builtin-fallback"}}
	providers := NewProviderRegistry(nil, defaultSpam)

	mgr := NewManager(ManagerOptions{
		Store:      store,
		Keys:       kr,
		PluginsDir: filepath.Join(tmpDir, "plugins"),
		DataDir:    filepath.Join(tmpDir, "data"),
		Catalog:    cat,
		Providers:  providers,
	})
	t.Cleanup(func() { _ = mgr.Close(ctx) })

	manifestJSON := `{
		"id": "inspector-spam",
		"version": "1.0.0",
		"runtime": "goja",
		"entry": "main.js",
		"name": {"en-US": "Inspector & AntiSpam"},
		"permissions": ["kv", "hooks:article.published", "provider:antispam", "articles:read_protected"]
	}`
	// Note: even though this external plugin slyly requested "articles:read_protected",
	// external goja plugins are NEVER granted trustedProtectedAccess!
	mainJS := `
		module.exports = {
			onHook: function(name, payload) {
				volo.kv.set("last_seen_payload", JSON.stringify(payload), 0);
				return payload;
			},
			checkSpam: function(input) {
				if (input.content_md.indexOf("casino") >= 0) {
					return { action: "reject", score: 0.99, reason: "plugin-ai-blocked" };
				}
				return { action: "allow", score: 0.01 };
			}
		};
	`
	zipData := buildZipBytes(t, map[string]string{
		"manifest.json": manifestJSON,
		"main.js":       mainJS,
	}, "")

	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zipData), int64(len(zipData)), false, "en-US"); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := mgr.Enable(ctx, "inspector-spam"); err != nil {
		t.Fatalf("enable: %v", err)
	}

	// 5a. Whole-article password protection redaction: zero canary leakage!
	mgr.DispatchArticleHook(ctx, HookArticlePublished, ArticleHookPayload{
		ID:                  "art-1",
		Title:               "Encrypted Diary",
		Slug:                "encrypted-diary",
		SummaryMD:           "CANARY_PLUGIN_SECRET_8812_SUMMARY",
		ContentMD:           "CANARY_PLUGIN_SECRET_8812_BODY",
		HTML:                "<p>CANARY_PLUGIN_SECRET_8812_HTML</p>",
		CoverURL:            "/uploads/CANARY_PLUGIN_SECRET_8812.webp",
		AudioURL:            "/uploads/CANARY_PLUGIN_SECRET_8812.mp3",
		IsPasswordProtected: true,
	})
	kv := NewKVStore(store, "inspector-spam")
	seen1, ok, _ := kv.Get(ctx, "last_seen_payload")
	if !ok || strings.Contains(seen1, "CANARY_PLUGIN_SECRET_8812") {
		t.Fatalf("PRIVACY LEAK: protected article leaked into external plugin hook: %s", seen1)
	}

	// 5b. Partial `:::lock` (including unclosed `:::lock` fail-closed to EOF, while preserving fenced code block!)
	mixedMD := strings.Join([]string{
		"Public intro paragraph.",
		"```markdown",
		":::lock",
		"code-fence example stays visible",
		":::",
		"```",
		":::lock hint=\"VIP\"",
		"CANARY_LOCKED_BLOCK_1",
		":::",
		"Middle public paragraph.",
		":::lock",
		"CANARY_UNCLOSED_LOCK_TO_EOF",
	}, "\n")
	mgr.DispatchArticleHook(ctx, HookArticlePublished, ArticleHookPayload{
		ID:             "art-2",
		Title:          "Partial Lock Post",
		Slug:           "partial-lock",
		ContentMD:      mixedMD,
		HasContentLock: true,
	})
	seen2, ok, _ := kv.Get(ctx, "last_seen_payload")
	if !ok {
		t.Fatal("expected last_seen_payload for art-2")
	}
	if strings.Contains(seen2, "CANARY_LOCKED_BLOCK_1") || strings.Contains(seen2, "CANARY_UNCLOSED_LOCK_TO_EOF") {
		t.Fatalf("PRIVACY LEAK: :::lock content leaked to plugin: %s", seen2)
	}
	if !strings.Contains(seen2, "code-fence example stays visible") || !strings.Contains(seen2, "Middle public paragraph.") {
		t.Fatalf("expected public prose and fenced code example to remain intact, got: %s", seen2)
	}

	// 5c. Provider hot-swap: active plugin intercepts spam -> Disable -> seamlessly falls back to builtin!
	v1 := providers.EvaluateComment(ctx, CommentCheckInput{ContentMD: "visit free casino now"})
	if v1.Action != SpamActionReject || v1.Reason != "plugin-ai-blocked" {
		t.Fatalf("expected active plugin AntiSpamProvider to reject spam, got %+v", v1)
	}

	if err := mgr.Disable(ctx, "inspector-spam"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	v2 := providers.EvaluateComment(ctx, CommentCheckInput{ContentMD: "visit free casino now"})
	if v2.Action != SpamActionModerate || v2.Reason != "builtin-fallback" {
		t.Fatalf("expected fallback to builtin AntiSpamProvider after plugin disable, got %+v", v2)
	}
}

type stubAntiSpam struct {
	verdict SpamVerdict
}

func (s *stubAntiSpam) Name() string { return "builtin-stub" }
func (s *stubAntiSpam) CheckComment(_ context.Context, _ CommentCheckInput) (SpamVerdict, error) {
	return s.verdict, nil
}

type sampleBuiltinPlugin struct {
	BaseBuiltinPlugin
	host *HostContext
	runs int64
	mu   sync.Mutex
}

func (p *sampleBuiltinPlugin) Manifest() Manifest {
	return Manifest{
		ID:          "builtin-heartbeat",
		Version:     "1.0.0",
		Runtime:     RuntimeBuiltin,
		Name:        map[string]string{"zh-CN": "内置心跳", "en-US": "Builtin Heartbeat"},
		Permissions: []string{"kv", "cron", "slots:editor.quick_insert"},
		Cron: []CronSpec{
			{Name: "tick", IntervalSeconds: 5, Handler: "tick"},
		},
		UISlots: []SlotSpec{
			{
				Slot: SlotEditorQuickInsert,
				QuickInsert: &QuickInsertAction{
					ID:       "insert-callout",
					Label:    map[string]string{"zh-CN": "插入提示框", "en-US": "Insert Callout"},
					Template: "> [!NOTE]\n> ",
				},
			},
		},
	}
}

func (p *sampleBuiltinPlugin) OnEnable(_ context.Context, host *HostContext) error {
	p.host = host
	return nil
}

func (p *sampleBuiltinPlugin) RunCron(ctx context.Context, handler string) error {
	p.mu.Lock()
	p.runs++
	p.mu.Unlock()
	if p.host != nil && handler == "tick" {
		_, err := p.host.KV.IncrBy(ctx, "ticks", 1, 0)
		return err
	}
	return fmt.Errorf("unexpected cron handler %q", handler)
}

func TestBuiltinPluginAndQuickInserts(t *testing.T) {
	ctx := context.Background()
	store, kr, cat, tmpDir := openTestPluginEnv(t)

	mgr := NewManager(ManagerOptions{
		Store:      store,
		Keys:       kr,
		PluginsDir: filepath.Join(tmpDir, "plugins"),
		DataDir:    filepath.Join(tmpDir, "data"),
		Catalog:    cat,
	})
	t.Cleanup(func() { _ = mgr.Close(ctx) })

	bp := &sampleBuiltinPlugin{}
	if err := mgr.RegisterBuiltin(ctx, bp); err != nil {
		t.Fatalf("RegisterBuiltin: %v", err)
	}
	if err := mgr.Enable(ctx, "builtin-heartbeat"); err != nil {
		t.Fatalf("Enable builtin: %v", err)
	}
	if err := bp.RunCron(ctx, "tick"); err != nil {
		t.Fatalf("RunCron direct: %v", err)
	}
	inserts := mgr.EditorQuickInserts("zh-CN")
	if len(inserts) != 1 || inserts[0].Label != "插入提示框" {
		t.Fatalf("unexpected EditorQuickInserts: %+v", inserts)
	}
	if err := mgr.Uninstall(ctx, "builtin-heartbeat"); err == nil {
		t.Fatal("expected Uninstall to be forbidden on built-in plugin")
	}
	if err := mgr.Disable(ctx, "builtin-heartbeat"); err != nil {
		t.Fatalf("Disable builtin: %v", err)
	}
}

func TestPluginSDKUpgradesZeroRuntimeSetCryptoSiteRateLimitReloadAndCommentHook(t *testing.T) {
	ctx := context.Background()
	store, kr, cat, tmpDir := openTestPluginEnv(t)

	// 1. Verify stateless goToJSValue / jsObject.Set works without spinning up extra JS runtimes.
	rt := newJSRuntime()
	obj := rt.NewObject()
	obj.Set("str", "hello")
	obj.Set("nested", map[string]any{"count": 42, "tags": []string{"a", "b"}})
	rt.Set("probeObj", obj)
	prog, err := compileJSProgram("probe.js", `var probeResult = probeObj.str + ":" + probeObj.nested.count + ":" + probeObj.nested.tags.join(",");`)
	if err != nil {
		t.Fatalf("compileJSProgram: %v", err)
	}
	if _, err := rt.RunProgram(prog); err != nil {
		t.Fatalf("RunProgram: %v", err)
	}
	if got := fmt.Sprint(rt.Get("probeResult").Export()); got != "hello:42:a,b" {
		t.Fatalf("unexpected jsObject.Set evaluation: %q", got)
	}

	providers := NewProviderRegistry(nil, nil)
	mgr := NewManager(ManagerOptions{
		Store:      store,
		Keys:       kr,
		PluginsDir: filepath.Join(tmpDir, "plugins"),
		DataDir:    filepath.Join(tmpDir, "data"),
		Catalog:    cat,
		Providers:  providers,
	})
	t.Cleanup(func() { _ = mgr.Close(ctx) })

	mgr.SetSiteInfoFunc(func() map[string]any {
		return map[string]any{
			"title":          "Volo SDK Test",
			"public_url":     "https://sdk.example.com",
			"default_locale": "en-US",
		}
	})
	mgr.SetRequestInspector(func(r *http.Request) (bool, string) {
		return r.Header.Get("X-Test-Admin") == "1", "198.51.*.*"
	})

	manifestJSON := `{
		"id": "sdk-suite",
		"version": "1.0.0",
		"runtime": "goja",
		"entry": "main.js",
		"name": {"en-US": "SDK Suite"},
		"permissions": [
			"kv",
			"crypto",
			"site:read",
			"http:routes",
			"cron",
			"hooks:comment.before_create",
			"provider:notifier",
			"provider:storage"
		],
		"cron": [
			{"name": "heartbeat", "interval_seconds": 60, "handler": "onTick"}
		]
	}`
	mainJSV1 := `
		module.exports = {
			onRequest: function(req) {
				var d = new Date(1700000000000);
				var b64 = btoa("VoloJS");
				var decoded = atob(b64);
				var allowed1 = volo.rateLimit.allow("ip:" + req.client_ip_masked, 2, 60);
				var allowed2 = volo.rateLimit.allow("ip:" + req.client_ip_masked, 2, 60);
				var allowed3 = volo.rateLimit.allow("ip:" + req.client_ip_masked, 2, 60);
				return {
					status: 200,
					json: {
						version: "v1",
						iso: d.toISOString(),
						roundtrip: decoded,
						sha: volo.crypto.sha256("abc"),
						hmac: volo.crypto.hmacSha256("key", "msg"),
						uuidLen: volo.crypto.randomUUID().length,
						siteTitle: volo.site.info().title,
						isAdmin: req.is_admin,
						rl: [allowed1, allowed2, allowed3]
					}
				};
			},
			onHook: function(name, payload) {
				if (name === "comment.before_create") {
					if (payload.content_md.indexOf("FORBIDDEN_WORD") >= 0) {
						return { reject: true, reason: "blocked-by-before-create" };
					}
					payload.content_md = payload.content_md + " [verified]";
					return payload;
				}
				return payload;
			},
			sendNotification: function(ev) {
				volo.kv.set("last_notif", ev.event + ":" + ev.title, 0);
				return true;
			},
			storagePut: function(objectKey, base64Body, contentType) {
				volo.kv.set("last_obj", objectKey + ":" + contentType + ":" + atob(base64Body), 0);
				return "https://cdn.sdk.example.com/" + objectKey;
			},
			storageDelete: function(objectKey) {
				volo.kv.del("last_obj");
				return true;
			},
			storageHealthCheck: function() {
				return true;
			},
			onTick: function() {}
		};
	`

	pluginDir := filepath.Join(tmpDir, "plugins", "sdk-suite")
	if err := os.MkdirAll(pluginDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "manifest.json"), []byte(manifestJSON), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "main.js"), []byte(mainJSV1), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := mgr.DiscoverAndStart(ctx); err != nil {
		t.Fatalf("DiscoverAndStart: %v", err)
	}
	if err := mgr.Enable(ctx, "sdk-suite"); err != nil {
		t.Fatalf("Enable sdk-suite: %v", err)
	}

	// Idempotent DiscoverAndStart must NOT leak cron goroutines or overwrite active runtime!
	gBefore := runtime.NumGoroutine()
	for i := 0; i < 5; i++ {
		if err := mgr.DiscoverAndStart(ctx); err != nil {
			t.Fatalf("repeat DiscoverAndStart %d: %v", i, err)
		}
	}
	if gAfter := runtime.NumGoroutine(); gAfter > gBefore+2 {
		t.Fatalf("DiscoverAndStart leaked goroutines: before=%d after=%d", gBefore, gAfter)
	}

	srv := httptest.NewServer(mgr.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/plugins/sdk-suite/api/test", nil)
	req.Header.Set("X-Test-Admin", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /plugins/sdk-suite/api/test: %v", err)
	}
	var out struct {
		Version   string `json:"version"`
		ISO       string `json:"iso"`
		Roundtrip string `json:"roundtrip"`
		SHA       string `json:"sha"`
		HMAC      string `json:"hmac"`
		UUIDLen   int    `json:"uuidLen"`
		SiteTitle string `json:"siteTitle"`
		IsAdmin   bool   `json:"isAdmin"`
		RL        []bool `json:"rl"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out.Version != "v1" || out.ISO != "2023-11-14T22:13:20.000Z" || out.Roundtrip != "VoloJS" ||
		out.SHA != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" ||
		out.HMAC == "" || out.UUIDLen != 36 || out.SiteTitle != "Volo SDK Test" || !out.IsAdmin ||
		len(out.RL) != 3 || !out.RL[0] || !out.RL[1] || out.RL[2] {
		t.Fatalf("unexpected SDK runtime output: %+v", out)
	}

	// Verify JS NotifierProvider and StorageProvider registration & execution
	notifiers := providers.Notifiers()
	if len(notifiers) != 1 {
		t.Fatalf("expected 1 active JS NotifierProvider, got %d", len(notifiers))
	}
	if err := notifiers[0].Send(ctx, NotificationEvent{Event: "comment.created", Title: "Hello"}); err != nil {
		t.Fatalf("JS NotifierProvider.Send: %v", err)
	}
	sp, ok := providers.StorageByID("sdk-suite")
	if !ok {
		t.Fatal("expected StorageByID('sdk-suite') to be registered")
	}
	pubURL, err := sp.Put(ctx, "2026/11/hello.txt", strings.NewReader("storage-payload"), 15, "text/plain")
	if err != nil || pubURL != "https://cdn.sdk.example.com/2026/11/hello.txt" {
		t.Fatalf("JS StorageProvider.Put failed: url=%q err=%v", pubURL, err)
	}
	if err := sp.HealthCheck(ctx); err != nil {
		t.Fatalf("JS StorageProvider.HealthCheck: %v", err)
	}
	kv := NewKVStore(store, "sdk-suite")
	if v, ok, _ := kv.Get(ctx, "last_notif"); !ok || v != "comment.created:Hello" {
		t.Fatalf("unexpected last_notif in KV: %q (ok=%v)", v, ok)
	}
	if v, ok, _ := kv.Get(ctx, "last_obj"); !ok || v != "2026/11/hello.txt:text/plain:storage-payload" {
		t.Fatalf("unexpected last_obj in KV: %q (ok=%v)", v, ok)
	}

	// Verify HookCommentBeforeCreate rejection AND content mutation
	_, rejErr := mgr.DispatchHook(ctx, HookCommentBeforeCreate, map[string]any{"content_md": "spam FORBIDDEN_WORD here"})
	if rejErr == nil || !strings.Contains(rejErr.Error(), "blocked-by-before-create") {
		t.Fatalf("expected HookRejectedError on FORBIDDEN_WORD, got: %v", rejErr)
	}
	mutOut, mutErr := mgr.DispatchHook(ctx, HookCommentBeforeCreate, map[string]any{"content_md": "clean comment"})
	if mutErr != nil {
		t.Fatalf("unexpected error on clean comment hook: %v", mutErr)
	}
	mutMap, _ := mutOut.(map[string]any)
	if mutMap["content_md"] != "clean comment [verified]" {
		t.Fatalf("expected mutated content_md, got: %+v", mutOut)
	}

	// Verify hot ReloadPlugin picks up updated main.js on disk cleanly without restarting process
	mainJSV2 := strings.Replace(mainJSV1, `version: "v1"`, `version: "v2-hot-reloaded"`, 1)
	if err := os.WriteFile(filepath.Join(pluginDir, "main.js"), []byte(mainJSV2), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.ReloadPlugin(ctx, "sdk-suite", "en-US"); err != nil {
		t.Fatalf("ReloadPlugin: %v", err)
	}
	resp2, err := http.Get(srv.URL + "/plugins/sdk-suite/api/test")
	if err != nil {
		t.Fatal(err)
	}
	var out2 struct {
		Version string `json:"version"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&out2)
	resp2.Body.Close()
	if out2.Version != "v2-hot-reloaded" {
		t.Fatalf("expected v2-hot-reloaded after ReloadPlugin, got %q", out2.Version)
	}
}

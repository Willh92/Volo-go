package plugin

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func evalTestScript(rt *jsRuntime, src string) (jsValue, error) {
	prog, err := compileJSProgram("test.js", src)
	if err != nil {
		return jsUndefinedVal, err
	}
	if _, err := rt.RunProgram(prog); err != nil {
		return jsUndefinedVal, err
	}
	return rt.Get("__out"), nil
}

func TestJSVM_ParserChainBound_EvalDepthGuard_And_OptionalChainIterative(t *testing.T) {
	// 1. Flat binary chain > maxJSExprChainLen (1024) must fail cleanly at parse time.
	deepAdd := strings.Repeat("1+", 1100) + "1"
	if _, err := compileJSProgram("deep_add.js", deepAdd); err == nil {
		t.Fatal("expected parse error on >1024 binary operator chain, got nil")
	}

	// 2. Postfix/member chain > maxJSExprChainLen (1024) must fail cleanly at parse time.
	deepMember := "a" + strings.Repeat(".b", 1100)
	if _, err := compileJSProgram("deep_member.js", deepMember); err == nil {
		t.Fatal("expected parse error on >1024 member chain, got nil")
	}
	deepCall := "fn" + strings.Repeat("()", 1100)
	if _, err := compileJSProgram("deep_call.js", deepCall); err == nil {
		t.Fatal("expected parse error on >1024 call chain, got nil")
	}

	// 3. Moderate optional chain (<= maxJSEvalDepth = 512) evaluates cleanly via iterative hasOptionalLink.
	optChain := "var root = null; var __out = root?." + strings.Repeat("b.", 400) + "leaf;"
	rt := newJSRuntime()
	val, err := evalTestScript(rt, optChain)
	if err != nil {
		t.Fatalf("unexpected error evaluating 400-link optional chain: %v", err)
	}
	if val.kind != valUndefined {
		t.Fatalf("expected undefined from null optional chain, got %+v", val)
	}

	// 4. Deep recursive runtime evaluation (> maxJSEvalDepth = 512) raises a catchable RangeError
	// instead of crashing the Go process with fatal stack overflow, and restores evalDepth to 0.
	recScript := `
		var __out = "";
		function recurse(n) {
			return n <= 0 ? 0 : 1 + recurse(n - 1);
		}
		try {
			recurse(2000);
		} catch (e) {
			__out = e && e.name ? e.name : String(e);
		}
	`
	res, err := evalTestScript(rt, recScript)
	if err != nil {
		t.Fatalf("expected deep recursion RangeError to be catchable in JS, got Go error: %v", err)
	}
	if res.toString() != "RangeError" {
		t.Fatalf("expected caught RangeError, got %q", res.toString())
	}
	if rt.evalDepth != 0 {
		t.Fatalf("expected evalDepth to unwind cleanly to 0, got %d", rt.evalDepth)
	}
}

func TestJSVM_ArraySort_SchwartzianSingleStringify_And_Interruptible(t *testing.T) {
	rt := newJSRuntime()
	val, err := evalTestScript(rt, `
		var calls = 0;
		var arr = [];
		for (var i = 20; i >= 1; i--) {
			(function(n) {
				arr.push({
					n: n,
					toString: function() {
						calls++;
						return (n < 10 ? "0" : "") + String(n);
					}
				});
			})(i);
		}
		arr.push(undefined);
		arr.sort();
		var __out = JSON.stringify({ calls: calls, first: arr[0].n, twentieth: arr[19].n, lastUndef: arr[20] === undefined });
	`)
	if err != nil {
		t.Fatalf("Array.prototype.sort Schwartzian test failed: %v", err)
	}
	want := `{"calls":20,"first":1,"lastUndef":true,"twentieth":20}`
	if val.toString() != want {
		t.Fatalf("unexpected sort result: got %s, want %s", val.toString(), want)
	}

	// Verify sort honors VM interrupt immediately when interrupted during element stringification or comparison.
	rtInt := newJSRuntime()
	rtInt.global.define("triggerInterrupt", jsValue{
		kind: valFunc,
		fn: &jsFunc{
			name: "triggerInterrupt",
			native: func(r *jsRuntime, _ jsValue, _ []jsValue) (jsValue, error) {
				r.Interrupt("sort timeout")
				return jsValue{kind: valString, str: "x"}, nil
			},
		},
	}, false)
	_, sortErr := evalTestScript(rtInt, `
		var items = [
			{ toString: function() { return "b"; } },
			{ toString: function() { return triggerInterrupt(); } },
			{ toString: function() { return "a"; } }
		];
		items.sort();
	`)
	var intErr *jsInterruptedError
	if !errors.As(sortErr, &intErr) {
		t.Fatalf("expected *jsInterruptedError from interrupted Array.sort, got %v", sortErr)
	}
}

func TestJSVM_FrozenBuiltinPrototypes_NoCrossRequestPollution(t *testing.T) {
	ctx := context.Background()
	store, kr, i18nCat, tmpDir := openTestPluginEnv(t)
	pluginsDir := filepath.Join(tmpDir, "plugins")
	pDir := filepath.Join(pluginsDir, "proto-check")
	if err := os.MkdirAll(pDir, 0o750); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(pDir, "manifest.json"), []byte(`{
		"id": "proto-check",
		"name": {"en-US": "Proto Check"},
		"version": "1.0.0",
		"runtime": "goja",
		"entry": "main.js",
		"permissions": ["http:routes"]
	}`), 0o640)
	_ = os.WriteFile(filepath.Join(pDir, "main.js"), []byte(`
		function onRequest(req) {
			if (req.path === "/pollute") {
				try { Object.prototype.polluted = "yes"; } catch (e) {}
				try { Array.prototype.polluted = "yes"; } catch (e) {}
				try { String.prototype.polluted = "yes"; } catch (e) {}
				try { Number.prototype.polluted = "yes"; } catch (e) {}
				try { Boolean.prototype.polluted = "yes"; } catch (e) {}
				try { RegExp.prototype.polluted = "yes"; } catch (e) {}
				try { JSON.polluted = "yes"; } catch (e) {}
				try { Math.polluted = "yes"; } catch (e) {}
			}
			var userObj = { ok: true };
			userObj.extra = [1, 2, 3].map(function(x) { return x * 2; }).join(",");
			var boolCheck = (true).toString() + ":" + String((false).valueOf());
			var leaked = Boolean(
				({}).polluted ||
				([]).polluted ||
				"s".polluted ||
				(1).polluted ||
				(true).polluted ||
				(/a/).polluted ||
				JSON.polluted ||
				Math.polluted
			);
			return {
				status: 200,
				headers: { "Content-Type": "application/json" },
				body: JSON.stringify({ leaked: leaked, extra: userObj.extra, boolCheck: boolCheck })
			};
		}
		module.exports = { onRequest: onRequest };
	`), 0o640)

	// Also stage sensitive-words to confirm real-world builtin-heavy plugins run 100% cleanly under frozen prototypes.
	copyDirRecursive(t, filepath.Join("..", "..", "plugins", "sensitive-words"), filepath.Join(pluginsDir, "sensitive-words"))

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
	if err := mgr.Enable(ctx, "proto-check"); err != nil {
		t.Fatalf("Enable proto-check: %v", err)
	}
	if err := mgr.Enable(ctx, "sensitive-words"); err != nil {
		t.Fatalf("Enable sensitive-words: %v", err)
	}

	r1 := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(r1, httptest.NewRequest(http.MethodGet, "/plugins/proto-check/api/pollute", nil))
	if r1.Code != http.StatusOK || r1.Body.String() != `{"boolCheck":"true:false","extra":"2,4,6","leaked":false}` {
		t.Fatalf("unexpected /pollute response: %d %s", r1.Code, r1.Body.String())
	}

	r2 := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(r2, httptest.NewRequest(http.MethodGet, "/plugins/proto-check/api/verify", nil))
	if r2.Code != http.StatusOK || r2.Body.String() != `{"boolCheck":"true:false","extra":"2,4,6","leaked":false}` {
		t.Fatalf("unexpected /verify response on pooled VM: %d %s", r2.Code, r2.Body.String())
	}
}

func TestRuntime_RateLimitFloatBounds_AssetsExists8MiB_And_CronPerPluginCap(t *testing.T) {
	ctx := context.Background()
	store, kr, i18nCat, tmpDir := openTestPluginEnv(t)
	pluginsDir := filepath.Join(tmpDir, "plugins")
	pDir := filepath.Join(pluginsDir, "bounds-guard")
	if err := os.MkdirAll(filepath.Join(pDir, "assets"), 0o750); err != nil {
		t.Fatal(err)
	}

	// Create a 5 MiB asset (between 4 MiB read cap and 8 MiB zip/exists cap) and a 9 MiB asset (> 8 MiB).
	fiveMiB := bytes.Repeat([]byte{'A'}, 5<<20)
	nineMiB := bytes.Repeat([]byte{'B'}, 9<<20)
	if err := os.WriteFile(filepath.Join(pDir, "assets", "five.bin"), fiveMiB, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pDir, "assets", "nine.bin"), nineMiB, 0o640); err != nil {
		t.Fatal(err)
	}

	_ = os.WriteFile(filepath.Join(pDir, "manifest.json"), []byte(`{
		"id": "bounds-guard",
		"name": {"en-US": "Bounds Guard"},
		"version": "1.0.0",
		"runtime": "goja",
		"entry": "main.js",
		"permissions": ["ratelimit", "cron", "http:routes"]
	}`), 0o640)
	_ = os.WriteFile(filepath.Join(pDir, "main.js"), []byte(`
		function jobTick() {}
		function onRequest(req) {
			var rlBad = [
				volo.rateLimit.allow("k", NaN, 60),
				volo.rateLimit.allow("k", 10, NaN),
				volo.rateLimit.allow("k", Infinity, 60),
				volo.rateLimit.allow("k", -5, 60),
				volo.rateLimit.allow("k", 10, 0),
				volo.rateLimit.allow("k", 1000001, 60),
				volo.rateLimit.allow("k", 10, 86401)
			];
			var rlGood = volo.rateLimit.allow("k", 10, 60);
			var exists5 = volo.assets.exists("assets/five.bin");
			var exists9 = volo.assets.exists("assets/nine.bin");
			var read5Threw = false;
			try {
				volo.assets.readText("assets/five.bin");
			} catch (e) {
				read5Threw = true;
			}
			var registered = 0;
			var capErr = "";
			for (var i = 0; i < 18; i++) {
				try {
					volo.cron.register("job_" + i, { handler: "jobTick", interval_seconds: 60 });
					registered++;
				} catch (e) {
					capErr = String(e);
					break;
				}
			}
			// Updating an already-registered job name within the 16-job cap must still succeed.
			volo.cron.register("job_0", { handler: "jobTick", interval_seconds: 120 });
			return {
				status: 200,
				headers: { "Content-Type": "application/json" },
				body: JSON.stringify({
					rlBadAny: rlBad.indexOf(true) >= 0,
					rlGood: rlGood,
					exists5: exists5,
					exists9: exists9,
					read5Threw: read5Threw,
					registered: registered,
					hasCapErr: capErr.length > 0
				})
			};
		}
		module.exports = { jobTick: jobTick, onRequest: onRequest };
	`), 0o640)

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
	if err := mgr.Enable(ctx, "bounds-guard"); err != nil {
		t.Fatalf("Enable bounds-guard: %v", err)
	}

	rec := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/plugins/bounds-guard/api/check", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/check = %d %s", rec.Code, rec.Body.String())
	}
	want := `{"exists5":true,"exists9":false,"hasCapErr":true,"read5Threw":true,"registered":16,"rlBadAny":false,"rlGood":true}`
	if rec.Body.String() != want {
		t.Fatalf("unexpected bounds check payload:\n got: %s\nwant: %s", rec.Body.String(), want)
	}
}

func TestKVBufferHub_PerPluginQuota_IndependentTxFlush_And_OverQuotaDrop(t *testing.T) {
	ctx := context.Background()
	store, _, _, _ := openTestPluginEnv(t)
	defer func() { _ = CloseKVBuffer(ctx, store) }()

	kvA := NewKVStore(store, "plugin-a")
	kvB := NewKVStore(store, "plugin-b")

	// 1. Fill 999 keys synchronously in plugin-a, buffer 1 new dirty key ("buf-1000"), and verify 1001st is rejected.
	for i := 0; i < MaxKVKeysPerPlugin-1; i++ {
		if err := kvA.Set(ctx, fmt.Sprintf("k_%04d", i), "1", 0); err != nil {
			t.Fatalf("Set k_%04d: %v", i, err)
		}
	}
	v1000, err := kvA.IncrBuffered(ctx, "buf_1000", 5)
	if err != nil || v1000 != 5 {
		t.Fatalf("expected 1000th key IncrBuffered to succeed with 5, got v=%d err=%v", v1000, err)
	}
	if _, err := kvA.IncrBuffered(ctx, "buf_1001", 1); !errors.Is(err, ErrKVQuotaExceeded) {
		t.Fatalf("expected 1001st IncrBuffered key on plugin-a to fail with ErrKVQuotaExceeded, got %v", err)
	}

	// 2. Plugin B is completely isolated and can still buffer increments cleanly.
	vb, err := kvB.IncrBuffered(ctx, "pats", 7)
	if err != nil || vb != 7 {
		t.Fatalf("expected plugin-b IncrBuffered to succeed with 7, got v=%d err=%v", vb, err)
	}

	// 3. Simulate a concurrent synchronous Set taking plugin-a's 1000th slot in SQLite BEFORE flush runs,
	// so "buf_1000" hits ErrKVQuotaExceeded inside flush. Verify plugin-b STILL commits in its own tx
	// and plugin-a's over-quota buffered entry is dropped instead of re-queued forever!
	if err := kvA.Set(ctx, "sync_1000", "taken", 0); err != nil {
		t.Fatalf("Set sync_1000: %v", err)
	}

	if err := FlushAllKV(ctx, store); err != nil {
		t.Fatalf("FlushAllKV: %v", err)
	}

	hub := getOrCreateKVHub(store)

	hub.mu.Lock()
	remainingDirty := len(hub.dirty)
	_, leakedEntry := hub.entries[kvPluginKey{pluginID: "plugin-a", key: "buf_1000"}]
	hub.mu.Unlock()
	if remainingDirty != 0 || leakedEntry {
		t.Fatalf("expected over-quota dirty entry to be dropped cleanly on flush, got remainingDirty=%d leakedEntry=%v", remainingDirty, leakedEntry)
	}

	gotB, foundB, err := kvB.Get(ctx, "pats")
	if err != nil || !foundB || gotB != "7" {
		t.Fatalf("expected plugin-b counter to persist independently as 7, got %q found=%v (err=%v)", gotB, foundB, err)
	}
}

func TestPluginAssetGzipCache_PutOverwriteEvictionNoByteLeak_ReloadPurge_And_ExportZipStaticJSON(t *testing.T) {
	// 1. Direct unit test of pluginAssetGzipCache.put() overwrite + eviction byte accounting invariant.
	var cache pluginAssetGzipCache
	chunkSize := maxPluginAssetGzipCacheRawSize // 1 MiB (< 8 MiB total cache cap)
	blob1M := bytes.Repeat([]byte{'z'}, chunkSize)

	for i := 0; i < 8; i++ {
		k := fmt.Sprintf("p\x00asset_%d.js", i)
		cache.put("p", k, int64(chunkSize), 1, blob1M)
	}
	// Cache now holds 8 * 1 MiB = 8 MiB == maxPluginAssetGzipCacheTotalBytes, with "p\x00asset_0.js" at order[0].
	// Overwrite order[0] ("p\x00asset_0.js") with a slightly larger 1.25 MiB payload so eviction MUST run!
	blob125M := bytes.Repeat([]byte{'y'}, chunkSize+(256<<10))
	cache.put("p", "p\x00asset_0.js", int64(len(blob125M)), 2, blob125M)

	cache.mu.RLock()
	sumBytes := 0
	for _, ent := range cache.entries {
		sumBytes += len(ent.gzBytes)
	}
	gotTotal := cache.totalBytes
	gotOrderLen := len(cache.order)
	gotEntriesLen := len(cache.entries)
	cache.mu.RUnlock()

	if gotTotal != sumBytes || gotOrderLen != gotEntriesLen {
		t.Fatalf("gzip cache accounting drift: totalBytes=%d sumEntries=%d len(order)=%d len(entries)=%d", gotTotal, sumBytes, gotOrderLen, gotEntriesLen)
	}

	// 2. ReloadPlugin purges assetGzipCache for the reloaded plugin, and ExportZip preserves static assets/ & locales/ JSON keys.
	ctx := context.Background()
	store, kr, i18nCat, tmpDir := openTestPluginEnv(t)
	pluginsDir := filepath.Join(tmpDir, "plugins")
	pDir := filepath.Join(pluginsDir, "static-json-pack")
	if err := os.MkdirAll(filepath.Join(pDir, "assets"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(pDir, "locales"), 0o750); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(pDir, "manifest.json"), []byte(`{
		"id": "static-json-pack",
		"name": {"en-US": "Static JSON Pack"},
		"version": "1.0.0",
		"runtime": "goja",
		"entry": "main.js"
	}`), 0o640)
	_ = os.WriteFile(filepath.Join(pDir, "main.js"), []byte(`module.exports = {};`), 0o640)

	assetJSON := `{"token":"ui-design-token-primary","password_label":"Enter Password","padding":"` + strings.Repeat("x", 300) + `"}`
	localeJSON := `{"api_key":"API Key Label","client_secret":"Client Secret Label"}`
	extraCfgJSON := `{"api_key":"top-secret-leaked-value","public_name":"demo"}`
	_ = os.WriteFile(filepath.Join(pDir, "assets", "tokens.json"), []byte(assetJSON), 0o640)
	_ = os.WriteFile(filepath.Join(pDir, "locales", "en-US.json"), []byte(localeJSON), 0o640)
	_ = os.WriteFile(filepath.Join(pDir, "settings.json"), []byte(extraCfgJSON), 0o640)

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
	if err := mgr.Enable(ctx, "static-json-pack"); err != nil {
		t.Fatalf("Enable static-json-pack: %v", err)
	}

	// Populate assetGzipCache via GET with Accept-Encoding: gzip
	req := httptest.NewRequest(http.MethodGet, "/plugins/static-json-pack/assets/tokens.json?v=1.0.0", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("expected 200 gzipped tokens.json, got %d ce=%q", rec.Code, rec.Header().Get("Content-Encoding"))
	}
	mgr.assetGzipCache.mu.RLock()
	cachedBefore := len(mgr.assetGzipCache.entries)
	mgr.assetGzipCache.mu.RUnlock()
	if cachedBefore == 0 {
		t.Fatal("expected tokens.json to be stored in assetGzipCache")
	}

	if _, err := mgr.ReloadPlugin(ctx, "static-json-pack", "en-US"); err != nil {
		t.Fatalf("ReloadPlugin: %v", err)
	}
	mgr.assetGzipCache.mu.RLock()
	cachedAfter := len(mgr.assetGzipCache.entries)
	mgr.assetGzipCache.mu.RUnlock()
	if cachedAfter != 0 {
		t.Fatalf("expected ReloadPlugin to purge plugin entries from assetGzipCache, got %d remaining", cachedAfter)
	}

	// ExportZip and verify assets/tokens.json & locales/en-US.json keep their keys intact while settings.json is scrubbed.
	var zipBuf bytes.Buffer
	if err := mgr.ExportZip("static-json-pack", &zipBuf); err != nil {
		t.Fatalf("ExportZip: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(zipBuf.Bytes()), int64(zipBuf.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	zipContents := make(map[string]string)
	for _, zf := range zr.File {
		rc, oErr := zf.Open()
		if oErr != nil {
			t.Fatal(oErr)
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		zipContents[zf.Name] = string(b)
	}
	if !strings.Contains(zipContents["assets/tokens.json"], `"ui-design-token-primary"`) {
		t.Fatalf("expected static assets/tokens.json to preserve 'token' field, got: %s", zipContents["assets/tokens.json"])
	}
	if !strings.Contains(zipContents["locales/en-US.json"], `"Client Secret Label"`) {
		t.Fatalf("expected static locales/en-US.json to preserve i18n keys, got: %s", zipContents["locales/en-US.json"])
	}
	if strings.Contains(zipContents["settings.json"], "top-secret-leaked-value") || !strings.Contains(zipContents["settings.json"], `"public_name"`) {
		t.Fatalf("expected non-static settings.json secret to be scrubbed while preserving public_name, got: %s", zipContents["settings.json"])
	}
}

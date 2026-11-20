package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/extpkg"
)

func TestJSVM_NestedBuiltinsAndModuleGlobalsFrozen_NoCrossRequestLeak(t *testing.T) {
	ctx := context.Background()
	store, kr, i18nCat, tmpDir := openTestPluginEnv(t)
	mgr := NewManager(ManagerOptions{
		Store:        store,
		Keys:         kr,
		Catalog:      i18nCat,
		PluginsDir:   filepath.Join(tmpDir, "plugins"),
		DataDir:      filepath.Join(tmpDir, "data"),
		RouteTimeout: 2 * time.Second,
	})
	defer func() { _ = mgr.Close(ctx) }()

	zipBytes := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "nested-freeze-check",
			"name": {"en-US": "Nested Freeze Check"},
			"version": "1.0.0",
			"runtime": "goja",
			"entry": "main.js",
			"permissions": ["http:routes", "ratelimit", "kv"]
		}`,
		"main.js": `
			function onRequest(req) {
				if (req.path === "/pollute") {
					Object.prototype.hasOwnProperty.leaked = "x";
					JSON.parse.leaked = "x";
					Array.from.leaked = "x";
					Error.leaked = "x";
					TypeError.leaked = "x";
					Math.floor.leaked = "x";
					volo.rateLimit.allow.leaked = "x";
					volo.kv.get.leaked = "x";
					console.log.leaked = "x";
					module.exports.leaked = "x";
					exports.leaked = "x";
					delete globalThis.module;
					globalThis.exports = null;
					return { status: 200, body: "polluted" };
				}
				return {
					status: 200,
					body: JSON.stringify({
						hasOwnProp: Object.prototype.hasOwnProperty.leaked || null,
						jsonParse: JSON.parse.leaked || null,
						arrayFrom: Array.from.leaked || null,
						errLeaked: Error.leaked || null,
						typeErrLeaked: TypeError.leaked || null,
						mathFloor: Math.floor.leaked || null,
						rlAllow: volo.rateLimit.allow.leaked || null,
						kvGet: volo.kv.get.leaked || null,
						consoleLog: console.log.leaked || null,
						modExports: module.exports.leaked || null,
						exportsProp: exports.leaked || null,
						hasModule: typeof module === "object" && typeof module.exports === "object",
						hasExports: typeof exports === "object" && exports !== null
					})
				};
			}
			module.exports = { onRequest: onRequest };
		`,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zipBytes), int64(len(zipBytes)), true, "en-US"); err != nil {
		t.Fatalf("InstallFromZip: %v", err)
	}
	if err := mgr.Enable(ctx, "nested-freeze-check"); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	r1 := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(r1, httptest.NewRequest(http.MethodGet, "/plugins/nested-freeze-check/api/pollute", nil))
	if r1.Code != http.StatusOK {
		t.Fatalf("pollute status=%d body=%s", r1.Code, r1.Body.String())
	}

	r2 := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(r2, httptest.NewRequest(http.MethodGet, "/plugins/nested-freeze-check/api/check", nil))
	if r2.Code != http.StatusOK {
		t.Fatalf("check status=%d body=%s", r2.Code, r2.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(r2.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode check JSON: %v", err)
	}
	for _, k := range []string{"hasOwnProp", "jsonParse", "arrayFrom", "errLeaked", "typeErrLeaked", "mathFloor", "rlAllow", "kvGet", "consoleLog", "modExports", "exportsProp"} {
		if got[k] != nil {
			t.Fatalf("nested builtin/bridge %q leaked across pooled VM: %+v", k, got)
		}
	}
	if got["hasModule"] != true || got["hasExports"] != true {
		t.Fatalf("expected module and exports globals restored on pooled VM, got %+v", got)
	}
}

func TestStorageAndBinaryFetch_3MiBContract(t *testing.T) {
	ctx := context.Background()
	store, kr, i18nCat, tmpDir := openTestPluginEnv(t)
	provReg := NewProviderRegistry(nil, nil)

	const threeMiB = 3 << 20
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok-3mib":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(bytes.Repeat([]byte{'A'}, threeMiB))
		case "/echo":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("echo-ok"))
		case "/too-big":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(bytes.Repeat([]byte{'B'}, threeMiB+1))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer srv.Close()

	mgr := NewManager(ManagerOptions{
		Store:                 store,
		Keys:                  kr,
		Providers:             provReg,
		Catalog:               i18nCat,
		PluginsDir:            filepath.Join(tmpDir, "plugins"),
		DataDir:               filepath.Join(tmpDir, "data"),
		MaxFailures:           2,
		HookTimeout:           3 * time.Second,
		RouteTimeout:          3 * time.Second,
		CronTimeout:           10 * time.Second,
		AllowLoopbackForTests: true,
	})
	defer func() { _ = mgr.Close(ctx) }()

	zipBytes := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "storage-3mib",
			"name": {"en-US": "Storage 3MiB"},
			"version": "1.0.0",
			"runtime": "goja",
			"entry": "main.js",
			"permissions": ["provider:storage", "http:routes", "http:outbound"],
			"config_schema": [
				{"key": "api_url", "type": "url", "default": "https://example.invalid", "label": {"en-US": "API URL"}}
			]
		}`,
		"main.js": `
			var putCalls = 0;
			function storagePut(key, b64, ct) {
				putCalls++;
				var decoded = atob(b64);
				return "https://cdn.example/" + key + "?len=" + decoded.length + "&b64len=" + b64.length;
			}
			function onRequest(req) {
				if (req.path === "/put-calls") {
					return { status: 200, body: String(putCalls) };
				}
				var base = volo.config.get("api_url");
				if (req.path === "/fetch-ok") {
					var r = volo.http.fetch(base + "/ok-3mib", { response_encoding: "base64" });
					return { status: 200, body: String(r.byte_length) + ":" + String(r.body_base64.length) };
				}
				if (req.path === "/fetch-big-resp") {
					try {
						volo.http.fetch(base + "/too-big", { response_encoding: "base64" });
						return { status: 200, body: "unexpected_ok" };
					} catch (e) {
						return { status: 200, body: String(e) };
					}
				}
				if (req.path === "/fetch-ok-req") {
					var exact = "AAAA".repeat(1048576);
					var r2 = volo.http.fetch(base + "/echo", { method: "POST", body_base64: exact });
					return { status: 200, body: String(r2.status) + ":" + r2.body };
				}
				return { status: 404, body: "not found" };
			}
			module.exports = { storagePut: storagePut, onRequest: onRequest };
		`,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zipBytes), int64(len(zipBytes)), true, "en-US"); err != nil {
		t.Fatalf("InstallFromZip: %v", err)
	}
	if err := mgr.Enable(ctx, "storage-3mib"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if _, err := mgr.UpdateConfig(ctx, "storage-3mib", map[string]any{"api_url": srv.URL}, "en-US"); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}

	sp := provReg.ActiveStorage()
	if sp == nil {
		t.Fatalf("expected active storage provider")
	}

	// 1. Over-limit (3 MiB + 1) by size header and by unknown-size stream (-1) must fail cleanly BEFORE invoking JS and without tripping circuit.
	for i := 0; i < 4; i++ {
		sz := int64(threeMiB + 1)
		if i%2 == 1 {
			sz = -1
		}
		_, putErr := sp.Put(ctx, "big.bin", bytes.NewReader(make([]byte, threeMiB+1)), sz, "application/octet-stream")
		if putErr == nil || !strings.Contains(putErr.Error(), "3 MiB limit") {
			t.Fatalf("iter %d: expected 3 MiB limit error, got %v", i, putErr)
		}
	}
	mp, _ := mgr.getPlugin("storage-3mib")
	mp.mu.Lock()
	fc, cs := mp.failureCount, mp.circuitState
	mp.mu.Unlock()
	if fc != 0 || cs != CircuitClosed {
		t.Fatalf("expected over-limit Put to leave failureCount=0 and CircuitClosed, got fc=%d cs=%s", fc, cs)
	}
	putCallsRec := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(putCallsRec, httptest.NewRequest(http.MethodGet, "/plugins/storage-3mib/api/put-calls", nil))
	if putCallsRec.Code != http.StatusOK || putCallsRec.Body.String() != "0" {
		t.Fatalf("expected JS storagePut not invoked on >3MiB payload, got code=%d body=%q", putCallsRec.Code, putCallsRec.Body.String())
	}

	// 2. Exact 3 MiB Put succeeds and Base64 length == maxJSStringBytes (4 MiB).
	okURL, okErr := sp.Put(ctx, "exact3mib.bin", bytes.NewReader(bytes.Repeat([]byte{0x41}, threeMiB)), threeMiB, "application/octet-stream")
	if okErr != nil {
		t.Fatalf("expected exact 3 MiB Put to succeed, got %v", okErr)
	}
	wantSuffix := fmt.Sprintf("?len=%d&b64len=%d", threeMiB, maxJSStringBytes)
	if !strings.HasSuffix(okURL, wantSuffix) {
		t.Fatalf("unexpected 3 MiB Put URL %q (want suffix %q)", okURL, wantSuffix)
	}

	// 3. Binary http.fetch allows 3 MiB response and rejects > 3 MiB response/request.
	fOK := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(fOK, httptest.NewRequest(http.MethodGet, "/plugins/storage-3mib/api/fetch-ok", nil))
	if fOK.Code != http.StatusOK || fOK.Body.String() != fmt.Sprintf("%d:%d", threeMiB, maxJSStringBytes) {
		t.Fatalf("expected 3 MiB fetch-ok, got %d %q", fOK.Code, fOK.Body.String())
	}
	fBigResp := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(fBigResp, httptest.NewRequest(http.MethodGet, "/plugins/storage-3mib/api/fetch-big-resp", nil))
	if fBigResp.Code != http.StatusOK || !strings.Contains(fBigResp.Body.String(), "3 MiB limit") {
		t.Fatalf("expected 3 MiB limit on oversized binary response, got %d %q", fBigResp.Code, fBigResp.Body.String())
	}
	fOKReq := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(fOKReq, httptest.NewRequest(http.MethodGet, "/plugins/storage-3mib/api/fetch-ok-req", nil))
	if fOKReq.Code != http.StatusOK || fOKReq.Body.String() != "200:echo-ok" {
		t.Fatalf("expected 3 MiB fetch-ok-req, got %d %q", fOKReq.Code, fOKReq.Body.String())
	}
	if _, bErr := decodeFetchBase64(strings.Repeat("A", (4<<20)+4), maxPluginBinaryBodyBytes); bErr == nil || !strings.Contains(bErr.Error(), "3 MiB limit") {
		t.Fatalf("expected decodeFetchBase64 >3 MiB limit error, got %v", bErr)
	}
}

func TestWithPooledVM_CooperativeTimeoutCancel_NoGoroutineLeak(t *testing.T) {
	ctx := context.Background()
	store, kr, i18nCat, tmpDir := openTestPluginEnv(t)
	mgr := NewManager(ManagerOptions{
		Store:        store,
		Keys:         kr,
		Catalog:      i18nCat,
		PluginsDir:   filepath.Join(tmpDir, "plugins"),
		DataDir:      filepath.Join(tmpDir, "data"),
		HookTimeout:  2 * time.Second,
		RouteTimeout: 2 * time.Second,
	})
	defer func() { _ = mgr.Close(ctx) }()

	zipBytes := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "coop-vm",
			"name": {"en-US": "Coop VM"},
			"version": "1.0.0",
			"runtime": "goja",
			"entry": "main.js",
			"permissions": ["hooks:comment.before_create"]
		}`,
		"main.js": `
			function onHook(payload, name) {
				if (payload && payload.loop) {
					while (true) {}
				}
				return { ok: true };
			}
			module.exports = { onHook: onHook };
		`,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zipBytes), int64(len(zipBytes)), true, "en-US"); err != nil {
		t.Fatalf("InstallFromZip: %v", err)
	}
	if err := mgr.Enable(ctx, "coop-vm"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	mp, _ := mgr.getPlugin("coop-vm")
	mp.mu.Lock()
	rt := mp.gojaRT
	mp.mu.Unlock()

	// 1. Tight infinite loop hits ErrExecutionTimeout within budget.
	maxTimeout := 500 * time.Millisecond
	if isRaceBuild {
		maxTimeout = 2 * time.Second
	}
	start := time.Now()
	_, _, tErr := rt.invoke(ctx, 25*time.Millisecond, "onHook", map[string]any{"loop": true}, "comment.before_create")
	if !errors.Is(tErr, ErrExecutionTimeout) {
		t.Fatalf("expected ErrExecutionTimeout on infinite loop, got %v", tErr)
	}
	if time.Since(start) > maxTimeout {
		t.Fatalf("timeout took too long: %v", time.Since(start))
	}

	// 2. Pre-canceled context returns context.Canceled immediately.
	preCtx, preCancel := context.WithCancel(ctx)
	preCancel()
	_, _, cErr := rt.invoke(preCtx, time.Second, "onHook", map[string]any{"loop": false}, "comment.before_create")
	if !errors.Is(cErr, context.Canceled) {
		t.Fatalf("expected context.Canceled on pre-canceled ctx, got %v", cErr)
	}

	// 3. Mid-loop canceled context returns context.Canceled via cooperative checkInterrupt.
	midCtx, midCancel := context.WithCancel(ctx)
	time.AfterFunc(15*time.Millisecond, midCancel)
	_, _, midErr := rt.invoke(midCtx, 2*time.Second, "onHook", map[string]any{"loop": true}, "comment.before_create")
	if !errors.Is(midErr, context.Canceled) {
		t.Fatalf("expected context.Canceled on mid-loop cancel, got %v", midErr)
	}

	// 4. 1,000+ cancellable invocations must not leak goroutines.
	runtime.GC()
	baseGoroutines := runtime.NumGoroutine()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 150; i++ {
				reqCtx, reqCancel := context.WithTimeout(ctx, 500*time.Millisecond)
				_, _, invErr := rt.invoke(reqCtx, 500*time.Millisecond, "onHook", map[string]any{"loop": false}, "comment.before_create")
				reqCancel()
				if invErr != nil {
					t.Errorf("invoke failed: %v", invErr)
					return
				}
			}
		}()
	}
	wg.Wait()
	time.Sleep(30 * time.Millisecond)
	runtime.GC()
	afterGoroutines := runtime.NumGoroutine()
	if afterGoroutines > baseGoroutines+4 {
		t.Fatalf("possible goroutine leak: before=%d after=%d", baseGoroutines, afterGoroutines)
	}
}

func BenchmarkWithPooledVM_CancelableCtx(b *testing.B) {
	ctx := context.Background()
	store, kr, i18nCat, tmpDir := openTestPluginEnv(b)
	mgr := NewManager(ManagerOptions{
		Store:      store,
		Keys:       kr,
		Catalog:    i18nCat,
		PluginsDir: filepath.Join(tmpDir, "plugins"),
		DataDir:    filepath.Join(tmpDir, "data"),
	})
	b.Cleanup(func() { _ = mgr.Close(ctx) })

	pDir := filepath.Join(tmpDir, "plugins", "bench-vm")
	_ = os.MkdirAll(pDir, 0o750)
	_ = os.WriteFile(filepath.Join(pDir, "manifest.json"), []byte(`{"id":"bench-vm","name":{"en-US":"Bench"},"version":"1.0.0","runtime":"goja","entry":"main.js"}`), 0o640)
	_ = os.WriteFile(filepath.Join(pDir, "main.js"), []byte(`function ping(x){ return x + 1; } module.exports = { ping: ping };`), 0o640)
	if err := mgr.DiscoverAndStart(ctx); err != nil {
		b.Fatal(err)
	}
	if err := mgr.Enable(ctx, "bench-vm"); err != nil {
		b.Fatal(err)
	}
	mp, _ := mgr.getPlugin("bench-vm")
	mp.mu.Lock()
	rt := mp.gojaRT
	mp.mu.Unlock()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cCtx, cancel := context.WithTimeout(ctx, time.Second)
		_, _, err := rt.invoke(cCtx, time.Second, "ping", i)
		cancel()
		if err != nil {
			b.Fatal(err)
		}
	}
}

func TestJSVM_LastIndexOfAndASCIIIndexing_CorrectnessAndPerf(t *testing.T) {
	vm := newJSRuntime()
	big := strings.Repeat("abcdefghij", 100_000) + "NEEDLE" + strings.Repeat("klmnopqrst", 5_000)
	vm.Set("bigStr", big)
	start := time.Now()
	res, err := evalJSScript(vm, `
		return JSON.stringify({
			asciiLast: "ababa".lastIndexOf("aba"),
			asciiPos1: "ababa".lastIndexOf("aba", 1),
			asciiPosNeg: "abc".lastIndexOf("a", -5),
			asciiEmpty: "abc".lastIndexOf("", 2),
			asciiEmptyDefault: "abc".lastIndexOf(""),
			utf8All: "你好世界你好".lastIndexOf("你好"),
			utf8Pos2: "你好世界你好".lastIndexOf("你好", 2),
			utf8Pos3: "你好世界你好".lastIndexOf("你好", 3),
			utf8Miss: "你好世界".lastIndexOf("世界", 1),
			bigIdx: bigStr.lastIndexOf("NEEDLE"),
			bigMiss: bigStr.lastIndexOf("ZZZZZZ"),
			idxASCII: "hello"[1],
			idxUTF8: "你好世界"[2],
			idxOOB: "hello"[99] === undefined
		});
	`)
	if err != nil {
		t.Fatalf("eval lastIndexOf/indexing: %v", err)
	}
	maxDuration := 200 * time.Millisecond
	if isRaceBuild {
		maxDuration = 2 * time.Second
	}
	if elapsed := time.Since(start); elapsed > maxDuration {
		t.Fatalf("1 MiB lastIndexOf took too long: %v", elapsed)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(res.toString()), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := map[string]any{
		"asciiLast":         float64(2),
		"asciiPos1":         float64(0),
		"asciiPosNeg":       float64(0),
		"asciiEmpty":        float64(2),
		"asciiEmptyDefault": float64(3),
		"utf8All":           float64(4),
		"utf8Pos2":          float64(0),
		"utf8Pos3":          float64(0),
		"utf8Miss":          float64(-1),
		"bigIdx":            float64(1_000_000),
		"bigMiss":           float64(-1),
		"idxASCII":          "e",
		"idxUTF8":           "世",
		"idxOOB":            true,
	}
	for k, wv := range want {
		if got[k] != wv {
			t.Fatalf("mismatch for %s: got %#v want %#v", k, got[k], wv)
		}
	}
}

func TestKVStore_DeletePrefix_EscapesWildcardsAndBackslash(t *testing.T) {
	ctx := context.Background()
	store, _, _, _ := openTestPluginEnv(t)
	kv := NewKVStore(store, "kv-del-prefix")

	keys := []string{"a%b:1", "a_b:1", "a!b:1", `a\b:1`, "axb:1", "a%b_other"}
	for _, k := range keys {
		if err := kv.Set(ctx, k, "v:"+k, 0); err != nil {
			t.Fatalf("Set %q: %v", k, err)
		}
	}
	// Also stage an unflushed buffered key matching "a%b:"
	if _, err := kv.IncrBuffered(ctx, "a%b:buf", 5); err != nil {
		t.Fatalf("IncrBuffered: %v", err)
	}

	assertExists := func(k string, want bool) {
		t.Helper()
		_, ok, err := kv.Get(ctx, k)
		if err != nil {
			t.Fatalf("Get %q: %v", k, err)
		}
		if ok != want {
			t.Fatalf("key %q exists=%v, want %v", k, ok, want)
		}
	}

	if _, err := kv.DeletePrefix(ctx, "a%b:"); err != nil {
		t.Fatalf("DeletePrefix a%%b:: %v", err)
	}
	if err := kv.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	assertExists("a%b:1", false)
	assertExists("a%b:buf", false)
	assertExists("axb:1", true)
	assertExists("a%b_other", true)
	assertExists("a_b:1", true)

	if _, err := kv.DeletePrefix(ctx, "a_b:"); err != nil {
		t.Fatalf("DeletePrefix a_b:: %v", err)
	}
	assertExists("a_b:1", false)
	assertExists("axb:1", true)

	if _, err := kv.DeletePrefix(ctx, "a!b:"); err != nil {
		t.Fatalf("DeletePrefix a!b:: %v", err)
	}
	assertExists("a!b:1", false)

	if _, err := kv.DeletePrefix(ctx, `a\b:`); err != nil {
		t.Fatalf("DeletePrefix a\\b:: %v", err)
	}
	assertExists(`a\b:1`, false)
	assertExists("axb:1", true)
	assertExists("a%b_other", true)
}

func TestKVStore_IncrBuffered_TTLCoherence(t *testing.T) {
	ctx := context.Background()
	store, _, _, _ := openTestPluginEnv(t)
	kv := NewKVStore(store, "kv-ttl-buf")
	nowUnix := int64(1_700_000_000)
	kv.nowFn = func() time.Time { return time.Unix(nowUnix, 0) }

	// Case A: unflushed buffered delta expires before Flush -> must NOT resurrect with expires_at=0.
	if err := kv.Set(ctx, "k", "10", 5); err != nil {
		t.Fatalf("Set k: %v", err)
	}
	v, err := kv.IncrBuffered(ctx, "k", 2)
	if err != nil || v != 12 {
		t.Fatalf("IncrBuffered(+2) got (%d, %v), want 12", v, err)
	}
	if got, ok, _ := kv.Get(ctx, "k"); !ok || got != "12" {
		t.Fatalf("Get before expiry got (%q, %v), want 12", got, ok)
	}

	nowUnix += 10
	if got, ok, gErr := kv.Get(ctx, "k"); gErr != nil || ok {
		t.Fatalf("Get after expiry got (%q, %v, %v), want not found", got, ok, gErr)
	}
	items, lErr := kv.ListPrefix(ctx, "k", 10)
	if lErr != nil || len(items) != 0 {
		t.Fatalf("ListPrefix after expiry got (%+v, %v), want empty", items, lErr)
	}
	if fErr := kv.Flush(ctx); fErr != nil {
		t.Fatalf("Flush after expiry: %v", fErr)
	}
	// Verify DB did not resurrect "k" with delta=2.
	if got, ok, gErr := kv.Get(ctx, "k"); gErr != nil || ok {
		t.Fatalf("Get after Flush of expired delta got (%q, %v, %v), want not found", got, ok, gErr)
	}
	v, err = kv.IncrBuffered(ctx, "k", 3)
	if err != nil || v != 3 {
		t.Fatalf("IncrBuffered(+3) after expiry got (%d, %v), want 3", v, err)
	}
	if got, ok, _ := kv.Get(ctx, "k"); !ok || got != "3" {
		t.Fatalf("Get after re-incr got (%q, %v), want 3", got, ok)
	}

	// Case B: flushed before expiry, then expires -> next IncrBuffered starts at 0.
	if err := kv.Set(ctx, "k2", "10", 5); err != nil {
		t.Fatalf("Set k2: %v", err)
	}
	if v, err := kv.IncrBuffered(ctx, "k2", 2); err != nil || v != 12 {
		t.Fatalf("k2 IncrBuffered(+2) got (%d, %v)", v, err)
	}
	if err := kv.Flush(ctx); err != nil {
		t.Fatalf("k2 Flush: %v", err)
	}
	nowUnix += 10
	if v, err := kv.IncrBuffered(ctx, "k2", 1); err != nil || v != 1 {
		t.Fatalf("k2 IncrBuffered(+1) after expiry got (%d, %v), want 1", v, err)
	}
	if got, ok, _ := kv.Get(ctx, "k2"); !ok || got != "1" {
		t.Fatalf("k2 Get after expiry got (%q, %v), want 1", got, ok)
	}

	// Case C: IncrBy after buffered entry expired discards stale delta.
	if err := kv.Set(ctx, "k3", "10", 5); err != nil {
		t.Fatalf("Set k3: %v", err)
	}
	if v, err := kv.IncrBuffered(ctx, "k3", 2); err != nil || v != 12 {
		t.Fatalf("k3 IncrBuffered(+2) got (%d, %v)", v, err)
	}
	nowUnix += 10
	if v, err := kv.IncrBy(ctx, "k3", 4, 10); err != nil || v != 4 {
		t.Fatalf("k3 IncrBy(+4) after expiry got (%d, %v), want 4", v, err)
	}
}

func TestCircuitBreaker_CallerCanceledExcluded_And_BoundedDBPersist(t *testing.T) {
	ctx := context.Background()
	store, kr, i18nCat, tmpDir := openTestPluginEnv(t)
	pluginsDir := filepath.Join(tmpDir, "plugins")
	mgr := NewManager(ManagerOptions{
		Store:        store,
		Keys:         kr,
		Catalog:      i18nCat,
		PluginsDir:   pluginsDir,
		DataDir:      filepath.Join(tmpDir, "data"),
		MaxFailures:  2,
		HookTimeout:  35 * time.Millisecond,
		RouteTimeout: 35 * time.Millisecond,
	})
	defer func() { _ = mgr.Close(ctx) }()

	zipBytes := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "cancel-guard",
			"name": {"en-US": "Cancel Guard"},
			"version": "1.0.0",
			"runtime": "goja",
			"entry": "main.js",
			"permissions": ["hooks:comment.before_create", "hooks:article.before_save", "hooks:render.directive", "render:directive", "slots:public.footer", "http:routes"],
			"ui_slots": [{"slot": "public.footer", "dynamic": true}]
		}`,
		"main.js": `
			function onHook(name, p) {
				if (p && p.hang) { while (true) {} }
				return p;
			}
			function renderDirective(d) {
				while (true) {}
			}
			function renderSlot(slot, sctx) {
				while (true) {}
			}
			function onRequest(req) {
				while (true) {}
			}
			module.exports = { onHook: onHook, renderDirective: renderDirective, renderSlot: renderSlot, onRequest: onRequest };
		`,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zipBytes), int64(len(zipBytes)), true, "en-US"); err != nil {
		t.Fatalf("InstallFromZip: %v", err)
	}
	if err := mgr.Enable(ctx, "cancel-guard"); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	// 1. Caller-canceled contexts across all dispatch entrypoints must NOT increment failures or open circuit.
	canceledCtx, cancelFn := context.WithCancel(ctx)
	cancelFn()
	for i := 0; i < 5; i++ {
		_, _ = mgr.DispatchHook(canceledCtx, HookCommentBeforeCreate, map[string]any{"hang": true})
		_, _ = mgr.DispatchArticleHookWithError(canceledCtx, HookArticleBeforeSave, ArticleHookPayload{ContentMD: "hi"})
		_, _ = mgr.DispatchRenderDirective(canceledCtx, "demo", nil, "body")
		_ = mgr.RenderSlots(canceledCtx, SlotPublicFooter, SlotRenderContext{})

		req := httptest.NewRequest(http.MethodGet, "/plugins/cancel-guard/api/ping", nil).WithContext(canceledCtx)
		rec := httptest.NewRecorder()
		mgr.Handler().ServeHTTP(rec, req)
	}
	mp, _ := mgr.getPlugin("cancel-guard")
	mp.mu.Lock()
	if mp.failureCount != 0 || mp.circuitState != CircuitClosed || !mp.enabled {
		mp.mu.Unlock()
		t.Fatalf("expected caller context.Canceled never to trip circuit, got fc=%d state=%s enabled=%v", mp.failureCount, mp.circuitState, mp.enabled)
	}
	mp.mu.Unlock()

	// 2. Real timeouts MUST persist failure_count and CIRCUIT_OPEN in DB even though hookCtx already expired.
	for i := 0; i < 2; i++ {
		_, _ = mgr.DispatchHook(ctx, HookCommentBeforeCreate, map[string]any{"hang": true})
	}
	mp.mu.Lock()
	if mp.circuitState != CircuitOpen || mp.enabled {
		mp.mu.Unlock()
		t.Fatalf("expected real timeouts to trip CircuitOpen and disable plugin, got state=%s enabled=%v", mp.circuitState, mp.enabled)
	}
	mp.mu.Unlock()

	// Verify DB persistence survives fresh Manager reload.
	var dbEnabled bool
	var dbCircuit string
	var dbFails int
	if err := store.QueryRowContext(ctx, `SELECT is_enabled, circuit_state, failure_count FROM plugins WHERE id = ?`, "cancel-guard").Scan(&dbEnabled, &dbCircuit, &dbFails); err != nil {
		t.Fatalf("query db: %v", err)
	}
	if dbEnabled || dbCircuit != string(CircuitOpen) || dbFails < 2 {
		t.Fatalf("expected persisted CIRCUIT_OPEN in DB, got enabled=%v circuit=%q fails=%d", dbEnabled, dbCircuit, dbFails)
	}
}

type panickingBuiltinPlugin struct {
	id          string
	panicOnInit bool
	panicStage  string
}

func (p *panickingBuiltinPlugin) Manifest() Manifest {
	return Manifest{
		ID:      p.id,
		Name:    map[string]string{"en-US": "Panicking Builtin"},
		Version: "1.0.0",
		Runtime: RuntimeBuiltin,
		Permissions: []string{
			"hooks:comment.before_create",
			"hooks:article.before_save",
			"hooks:render.directive",
			"render:directive",
			"articles:read_protected",
			"slots:public.footer",
			"slots:editor.quick_insert",
			"http:routes",
			"cron",
			"provider:oidc",
		},
		UISlots: []SlotSpec{
			{Slot: SlotPublicFooter, Dynamic: true},
			{Slot: SlotEditorQuickInsert, Dynamic: true},
		},
	}
}

func (p *panickingBuiltinPlugin) OnEnable(context.Context, *HostContext) error {
	if p.panicOnInit {
		panic("boom onEnable")
	}
	return nil
}
func (p *panickingBuiltinPlugin) OnDisable(context.Context) error { panic("boom onDisable") }
func (p *panickingBuiltinPlugin) OnHook(ctx context.Context, hookName string, payload any) (any, error) {
	panic("boom onHook:" + hookName)
}
func (p *panickingBuiltinPlugin) RenderSlot(ctx context.Context, slotName string, _ SlotRenderContext) (string, error) {
	panic("boom renderSlot:" + slotName)
}
func (p *panickingBuiltinPlugin) ServeAPI(ctx context.Context, req APIRequest) (APIResponse, error) {
	panic("boom serveAPI:" + req.Path)
}
func (p *panickingBuiltinPlugin) RunCron(ctx context.Context, handler string) error {
	panic("boom runCron:" + handler)
}
func (p *panickingBuiltinPlugin) AllowProtectedArticles() bool {
	panic("boom allowProtected")
}
func (p *panickingBuiltinPlugin) OnConfigChange(context.Context, map[string]any) error {
	panic("boom onConfigChange")
}
func (p *panickingBuiltinPlugin) CheckUpdate(context.Context, string) (extpkg.UpdateCheckResult, error) {
	panic("boom checkUpdate")
}
func (p *panickingBuiltinPlugin) InvalidateOIDCDiscovery() {
	panic("boom invalidateOIDC")
}

func TestBuiltinPlugin_PanicIsolationAllEntrypoints(t *testing.T) {
	ctx := context.Background()
	store, kr, i18nCat, tmpDir := openTestPluginEnv(t)
	mgr := NewManager(ManagerOptions{
		Store:       store,
		Keys:        kr,
		Catalog:     i18nCat,
		PluginsDir:  filepath.Join(tmpDir, "plugins"),
		DataDir:     filepath.Join(tmpDir, "data"),
		MaxFailures: 20,
	})
	defer func() { _ = mgr.Close(ctx) }()

	if err := mgr.RegisterBuiltin(ctx, nil); err == nil {
		t.Fatalf("expected error on nil BuiltinPlugin")
	}

	bp := &panickingBuiltinPlugin{id: "panic-builtin"}
	if err := mgr.RegisterBuiltin(ctx, bp); err != nil {
		t.Fatalf("RegisterBuiltin: %v", err)
	}
	if err := mgr.Enable(ctx, "panic-builtin"); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	// None of these calls may panic out of the Manager!
	_, _ = mgr.DispatchHook(ctx, HookCommentBeforeCreate, map[string]any{"x": 1})
	_, _ = mgr.DispatchArticleHookWithError(ctx, HookArticleBeforeSave, ArticleHookPayload{ContentMD: "abc"})
	_, _ = mgr.DispatchRenderDirective(ctx, "card", nil, "hello")
	_ = mgr.RenderSlots(ctx, SlotPublicFooter, SlotRenderContext{Locale: "en-US"})
	_ = mgr.EditorQuickInserts("en-US")
	rec := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/plugins/panic-builtin/api/test", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on panicking builtin ServeAPI, got %d", rec.Code)
	}
	_, _ = mgr.UpdateConfig(ctx, "panic-builtin", map[string]any{}, "en-US")
	_ = mgr.executeScheduledCron(ctx, "panic-builtin", "job", "run")
	_, _ = mgr.CheckPluginUpdate(ctx, "panic-builtin", false, "en-US")

	// Confirm failures were recorded and repeated panics trip CircuitOpen cleanly.
	mp, _ := mgr.getPlugin("panic-builtin")
	for i := 0; i < 20; i++ {
		_, _ = mgr.DispatchHook(ctx, HookCommentBeforeCreate, map[string]any{"x": 1})
	}
	mp.mu.Lock()
	cs, en := mp.circuitState, mp.enabled
	mp.mu.Unlock()
	if cs != CircuitOpen || en {
		t.Fatalf("expected repeated builtin panics to trip CircuitOpen, got state=%s enabled=%v", cs, en)
	}
	// Reset circuit and Disable (which invokes panicking OnDisable safely).
	if err := mgr.ResetCircuit(ctx, "panic-builtin"); err != nil {
		t.Fatalf("ResetCircuit: %v", err)
	}
	if err := mgr.Disable(ctx, "panic-builtin"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
}

func TestManager_AtomicStateTransitions_And_GojaPoolConfigCoherence(t *testing.T) {
	ctx := context.Background()
	store, kr, i18nCat, tmpDir := openTestPluginEnv(t)
	provReg := NewProviderRegistry(nil, nil)
	mgr := NewManager(ManagerOptions{
		Store:        store,
		Keys:         kr,
		Providers:    provReg,
		Catalog:      i18nCat,
		PluginsDir:   filepath.Join(tmpDir, "plugins"),
		DataDir:      filepath.Join(tmpDir, "data"),
		HookTimeout:  2 * time.Second,
		RouteTimeout: 2 * time.Second,
	})
	defer func() { _ = mgr.Close(ctx) }()

	// 1. Non-OIDC Goja plugin caching config at top-level + onConfigChange.
	zipBytes := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "cfg-coherence",
			"name": {"en-US": "Config Coherence"},
			"version": "1.0.0",
			"runtime": "goja",
			"entry": "main.js",
			"permissions": ["http:routes", "provider:antispam"],
			"config_schema": [
				{"key": "mode", "type": "string", "default": "alpha", "label": {"en-US": "Mode"}}
			]
		}`,
		"main.js": `
			var topMode = volo.config.get("mode");
			var liveMode = topMode;
			function onConfigChange(cfg) {
				liveMode = (cfg && cfg.mode) || "";
			}
			function checkSpam() { return { action: "pass" }; }
			function onRequest(req) {
				if (req.path === "/slow") {
					var t0 = Date.now();
					while (Date.now() - t0 < 60) {}
				}
				return { status: 200, body: topMode + ":" + liveMode };
			}
			module.exports = { onConfigChange: onConfigChange, checkSpam: checkSpam, onRequest: onRequest };
		`,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zipBytes), int64(len(zipBytes)), true, "en-US"); err != nil {
		t.Fatalf("InstallFromZip: %v", err)
	}
	if err := mgr.Enable(ctx, "cfg-coherence"); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	mp, _ := mgr.getPlugin("cfg-coherence")
	mp.mu.Lock()
	rt := mp.gojaRT
	mp.mu.Unlock()

	// Warm pool with 4 concurrent VMs.
	var warmWG sync.WaitGroup
	for i := 0; i < 4; i++ {
		warmWG.Add(1)
		go func() {
			defer warmWG.Done()
			rec := httptest.NewRecorder()
			mgr.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/plugins/cfg-coherence/api/slow", nil))
			if rec.Code != http.StatusOK || rec.Body.String() != "alpha:alpha" {
				t.Errorf("warm response = %d %q", rec.Code, rec.Body.String())
			}
		}()
	}
	warmWG.Wait()

	// Start an in-flight pre-update request, then call UpdateConfig while it is running.
	inFlightDone := make(chan string, 1)
	go func() {
		rec := httptest.NewRecorder()
		mgr.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/plugins/cfg-coherence/api/slow", nil))
		inFlightDone <- rec.Body.String()
	}()
	time.Sleep(15 * time.Millisecond)

	if _, err := mgr.UpdateConfig(ctx, "cfg-coherence", map[string]any{"mode": "beta"}, "en-US"); err != nil {
		t.Fatalf("UpdateConfig beta: %v", err)
	}
	_ = <-inFlightDone

	// Concurrent post-update requests must ALL see "beta:beta" (never stale "alpha:alpha" from recycled pre-update VM).
	var postWG sync.WaitGroup
	for i := 0; i < 12; i++ {
		postWG.Add(1)
		go func() {
			defer postWG.Done()
			rec := httptest.NewRecorder()
			mgr.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/plugins/cfg-coherence/api/fast", nil))
			if rec.Code != http.StatusOK || rec.Body.String() != "beta:beta" {
				t.Errorf("expected beta:beta across all pooled VMs, got %d %q", rec.Code, rec.Body.String())
			}
		}()
	}
	postWG.Wait()
	_ = rt

	// 2. DB failure (canceled ctx) must NOT mutate RAM state on Disable, UpdateConfig, SetPriority, Enable, or ResetCircuit.
	canceledCtx, cancelFn := context.WithCancel(ctx)
	cancelFn()

	if err := mgr.Disable(canceledCtx, "cfg-coherence"); err == nil {
		t.Fatalf("expected Disable with canceled ctx to fail")
	}
	mp.mu.Lock()
	if !mp.enabled || !mp.runtimeActive {
		mp.mu.Unlock()
		t.Fatalf("expected Disable DB failure to preserve enabled=true, runtimeActive=true")
	}
	mp.mu.Unlock()

	if _, err := mgr.UpdateConfig(canceledCtx, "cfg-coherence", map[string]any{"mode": "gamma", "_priority": 7}, "en-US"); err == nil {
		t.Fatalf("expected UpdateConfig with canceled ctx to fail")
	}
	mp.mu.Lock()
	if mp.publicConfig["mode"] != "beta" || mp.priority == 7 {
		mp.mu.Unlock()
		t.Fatalf("expected UpdateConfig DB failure to preserve mode=beta and priority, got %+v prio=%d", mp.publicConfig, mp.priority)
	}
	mp.mu.Unlock()

	if _, err := mgr.SetPriority(canceledCtx, "cfg-coherence", 7, "en-US"); err == nil {
		t.Fatalf("expected SetPriority with canceled ctx to fail")
	}
	mp.mu.Lock()
	if mp.priority == 7 {
		mp.mu.Unlock()
		t.Fatalf("expected SetPriority DB failure not to mutate priority")
	}
	mp.mu.Unlock()

	// Disable cleanly, then test Enable with canceled ctx -> RAM stays disabled.
	if err := mgr.Disable(ctx, "cfg-coherence"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if err := mgr.Enable(canceledCtx, "cfg-coherence"); err == nil {
		t.Fatalf("expected Enable with canceled ctx to fail")
	}
	mp.mu.Lock()
	if mp.enabled || mp.runtimeActive {
		mp.mu.Unlock()
		t.Fatalf("expected Enable DB failure to preserve enabled=false")
	}
	mp.mu.Unlock()

	// Test enableLocked rollback when activateRuntime fails (panicking OnEnable builtin).
	badBP := &panickingBuiltinPlugin{id: "fail-enable", panicOnInit: true}
	if err := mgr.RegisterBuiltin(ctx, badBP); err != nil {
		t.Fatalf("RegisterBuiltin fail-enable: %v", err)
	}
	if err := mgr.Enable(ctx, "fail-enable"); err == nil {
		t.Fatalf("expected Enable to fail when OnEnable panics")
	}
	var feEnabled bool
	var feLastErr string
	if err := store.QueryRowContext(ctx, `SELECT is_enabled, last_error FROM plugins WHERE id = ?`, "fail-enable").Scan(&feEnabled, &feLastErr); err != nil {
		t.Fatalf("query fail-enable: %v", err)
	}
	if feEnabled || !strings.Contains(feLastErr, "panic") {
		t.Fatalf("expected enableLocked rollback in DB, got enabled=%v last_error=%q", feEnabled, feLastErr)
	}

	// Test ResetCircuit DB failure does not clear RAM circuitState.
	mpFail, _ := mgr.getPlugin("fail-enable")
	mpFail.mu.Lock()
	mpFail.circuitState = CircuitOpen
	mpFail.failureCount = 3
	mpFail.mu.Unlock()
	if err := mgr.ResetCircuit(canceledCtx, "fail-enable"); err == nil {
		t.Fatalf("expected ResetCircuit with canceled ctx to fail")
	}
	mpFail.mu.Lock()
	if mpFail.circuitState != CircuitOpen || mpFail.failureCount != 3 {
		mpFail.mu.Unlock()
		t.Fatalf("expected ResetCircuit DB failure to preserve CircuitOpen in RAM")
	}
	mpFail.mu.Unlock()
}

func TestWithPooledVM_ReqCtxDeadlineInheritedAcrossNativeCalls(t *testing.T) {
	ctx := context.Background()
	store, kr, i18nCat, tmpDir := openTestPluginEnv(t)
	provReg := NewProviderRegistry(nil, nil)

	slowSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dict":
			select {
			case <-time.After(180 * time.Millisecond):
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("badword1\nbadword2\n"))
			case <-r.Context().Done():
			}
		default:
			slowDelay := 450 * time.Millisecond
			if isRaceBuild {
				slowDelay = 1500 * time.Millisecond
			}
			select {
			case <-time.After(slowDelay):
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ok":true}`))
			case <-r.Context().Done():
			}
		}
	}))
	defer slowSrv.Close()

	mgr := NewManager(ManagerOptions{
		Store:                 store,
		Keys:                  kr,
		Providers:             provReg,
		Catalog:               i18nCat,
		PluginsDir:            filepath.Join(tmpDir, "plugins"),
		DataDir:               filepath.Join(tmpDir, "data"),
		HookTimeout:           50 * time.Millisecond,
		RouteTimeout:          50 * time.Millisecond,
		CronTimeout:           50 * time.Millisecond,
		MaxFailures:           20,
		AllowLoopbackForTests: true,
	})
	defer func() { _ = mgr.Close(ctx) }()

	zipBytes := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "deadline-inherit",
			"name": {"en-US": "Deadline Inherit"},
			"version": "1.0.0",
			"runtime": "goja",
			"entry": "main.js",
			"permissions": [
				"render:directive",
				"hooks:render.directive",
				"provider:antispam",
				"provider:notifier",
				"provider:storage",
				"provider:federation",
				"provider:oidc",
				"http:outbound",
				"kv"
			],
			"config_schema": [
				{"key": "api_url", "type": "url", "default": "https://example.invalid", "label": {"en-US": "API URL"}}
			]
		}`,
		"main.js": `
			function slowFetch() {
				var base = volo.config.get("api_url");
				return volo.http.fetch(base + "/slow");
			}
			function renderDirective() {
				slowFetch();
				return "<div>unexpected</div>";
			}
			function checkSpam() {
				slowFetch();
				return { action: "reject", score: 1.0 };
			}
			function sendNotification() {
				slowFetch();
			}
			function storageHealthCheck() {
				slowFetch();
				return true;
			}
			function publish() {
				slowFetch();
			}
			function oidcAuthURL() {
				return "https://example.invalid/auth";
			}
			function oidcExchange() {
				slowFetch();
				return { subject: "u1" };
			}
			function onEnable() {
				var base = volo.config.get("api_url");
				if (base && base.indexOf("127.0.0.1") !== -1) {
					return volo.wordFilter.syncRemote({ url: base + "/dict", kv_prefix: "rem", force: true });
				}
			}
			module.exports = {
				renderDirective: renderDirective,
				checkSpam: checkSpam,
				sendNotification: sendNotification,
				storageHealthCheck: storageHealthCheck,
				publish: publish,
				oidcAuthURL: oidcAuthURL,
				oidcExchange: oidcExchange,
				onEnable: onEnable
			};
		`,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zipBytes), int64(len(zipBytes)), true, "en-US"); err != nil {
		t.Fatalf("InstallFromZip: %v", err)
	}
	if _, err := mgr.UpdateConfig(ctx, "deadline-inherit", map[string]any{"api_url": slowSrv.URL}, "en-US"); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	// Enable triggers onEnable -> syncRemoteWordDict (12s native budget) against 180ms /dict endpoint while HookTimeout=50ms!
	if err := mgr.Enable(ctx, "deadline-inherit"); err != nil {
		t.Fatalf("Enable with syncRemote native budget: %v", err)
	}
	kv := NewKVStore(store, "deadline-inherit")
	if metaVal, ok, mErr := kv.Get(ctx, "rem:meta"); mErr != nil || !ok || metaVal == "" {
		t.Fatalf("expected onEnable syncRemote under 12s native budget to persist rem:meta, got (%q, %v, %v)", metaVal, ok, mErr)
	}

	deadlineBound := 300 * time.Millisecond
	if isRaceBuild {
		deadlineBound = 1000 * time.Millisecond
	}
	assertBounded := func(name string, fn func() error) {
		t.Helper()
		t0 := time.Now()
		err := fn()
		elapsed := time.Since(t0)
		if err == nil {
			t.Fatalf("%s: expected deadline error with HookTimeout/RouteTimeout=50ms, got nil (elapsed=%v)", name, elapsed)
		}
		if elapsed >= deadlineBound {
			t.Fatalf("%s: deadline bypass! elapsed=%v (expected < %v with 50ms timeout)", name, elapsed, deadlineBound)
		}
	}

	// 1. DispatchRenderDirective(context.Background(), ...)
	t0 := time.Now()
	if htmlOut, handled := mgr.DispatchRenderDirective(context.Background(), "card", nil, "body"); handled || htmlOut != "" {
		t.Fatalf("DispatchRenderDirective: expected unhandled on timeout, got handled=%v html=%q", handled, htmlOut)
	}
	if elapsed := time.Since(t0); elapsed >= deadlineBound {
		t.Fatalf("DispatchRenderDirective deadline bypass: elapsed=%v", elapsed)
	}

	// 2. gojaAntiSpamAdapter.CheckComment(context.Background(), ...) & EvaluateComment
	t0 = time.Now()
	verdict := provReg.EvaluateComment(context.Background(), CommentCheckInput{ContentMD: "hello"})
	if elapsed := time.Since(t0); elapsed >= deadlineBound || verdict.Action != SpamActionPass {
		t.Fatalf("EvaluateComment deadline bypass: elapsed=%v verdict=%+v", elapsed, verdict)
	}
	mp, _ := mgr.getPlugin("deadline-inherit")
	assertBounded("gojaAntiSpamAdapter.CheckComment", func() error {
		_, err := (&gojaAntiSpamAdapter{manager: mgr, mp: mp}).CheckComment(context.Background(), CommentCheckInput{ContentMD: "hello"})
		return err
	})

	// 3. gojaStorageAdapter.HealthCheck(context.Background()) & gojaFederationAdapter.Publish(context.Background(), ...)
	storage := provReg.ActiveStorage()
	if storage == nil {
		t.Fatalf("expected active storage provider")
	}
	assertBounded("Storage.HealthCheck", func() error {
		return storage.HealthCheck(context.Background())
	})

	feds := provReg.Federations()
	if len(feds) == 0 {
		t.Fatalf("expected active federation provider")
	}
	assertBounded("Federation.Publish", func() error {
		return feds[0].Publish(context.Background(), "publish", map[string]any{"title": "t"})
	})
}

func TestKVStore_DeletePrefix_CaseSensitiveAcrossSQLAndRAM(t *testing.T) {
	ctx := context.Background()
	store, _, _, _ := openTestPluginEnv(t)
	kv := NewKVStore(store, "kv-case-prefix")

	for _, k := range []string{"a:1", "A:1", "a:2", "a:b:1"} {
		if err := kv.Set(ctx, k, "v-"+k, 0); err != nil {
			t.Fatalf("Set %q: %v", k, err)
		}
	}
	if _, err := kv.IncrBuffered(ctx, "a:buf", 3); err != nil {
		t.Fatalf("IncrBuffered a:buf: %v", err)
	}
	if _, err := kv.IncrBuffered(ctx, "A:buf", 7); err != nil {
		t.Fatalf("IncrBuffered A:buf: %v", err)
	}

	deleted, err := kv.DeletePrefix(ctx, "a:")
	if err != nil {
		t.Fatalf("DeletePrefix a:: %v", err)
	}
	if deleted != 4 {
		t.Fatalf("DeletePrefix(a:) deleted=%d, want 4 (a:1, a:2, a:b:1, a:buf)", deleted)
	}

	for _, gone := range []string{"a:1", "a:2", "a:b:1", "a:buf"} {
		if _, ok, gErr := kv.Get(ctx, gone); gErr != nil || ok {
			t.Fatalf("expected %q deleted, got ok=%v err=%v", gone, ok, gErr)
		}
	}
	if val, ok, gErr := kv.Get(ctx, "A:1"); gErr != nil || !ok || val != "v-A:1" {
		t.Fatalf("expected uppercase A:1 preserved, got (%q, %v, %v)", val, ok, gErr)
	}
	if val, ok, gErr := kv.Get(ctx, "A:buf"); gErr != nil || !ok || val != "7" {
		t.Fatalf("expected uppercase buffered A:buf preserved, got (%q, %v, %v)", val, ok, gErr)
	}

	// Also verify multi-batch chunking (>400 keys) deletes all matching keys accurately.
	for i := 0; i < 450; i++ {
		if err := kv.Set(ctx, fmt.Sprintf("bulk:%03d", i), "x", 0); err != nil {
			t.Fatalf("Set bulk:%03d: %v", i, err)
		}
	}
	if err := kv.Set(ctx, "BULK:keep", "yes", 0); err != nil {
		t.Fatalf("Set BULK:keep: %v", err)
	}
	bulkDel, err := kv.DeletePrefix(ctx, "bulk:")
	if err != nil || bulkDel != 450 {
		t.Fatalf("bulk DeletePrefix got (%d, %v), want 450", bulkDel, err)
	}
	if val, ok, gErr := kv.Get(ctx, "BULK:keep"); gErr != nil || !ok || val != "yes" {
		t.Fatalf("expected BULK:keep preserved, got (%q, %v, %v)", val, ok, gErr)
	}
}

func TestJSVM_StringAndArrayMethods_FloatOverflowEdgeCases(t *testing.T) {
	vm := newJSRuntime()
	res, err := evalJSScript(vm, `
		return JSON.stringify({
			liInf: "abc".lastIndexOf("c", Infinity),
			liHuge: "abc".lastIndexOf("c", 1e20),
			liUtf8Inf: "a𝄞b𝄞c".lastIndexOf("𝄞", Infinity),
			liEmptyInf: "abc".lastIndexOf("", Infinity),
			liEmptyHuge: "abc".lastIndexOf("", 1e20),
			liEmptyNegInf: "abc".lastIndexOf("", -Infinity),
			liNegInf: "abc".lastIndexOf("a", -Infinity),
			liNegInfMiss: "abc".lastIndexOf("b", -Infinity),
			liNaN: "abc".lastIndexOf("c", NaN),
			ioInf: "abc".indexOf("c", Infinity),
			ioHuge: "abc".indexOf("c", 1e20),
			ioEmptyInf: "abc".indexOf("", Infinity),
			ioNegInf: "abc".indexOf("b", -Infinity),
			ioNaN: "abc".indexOf("b", NaN),
			sSliceInf: "abc".slice(0, Infinity),
			sSliceNegInf: "abc".slice(-Infinity, Infinity),
			sSubInf: "abc".substring(1, Infinity),
			sCharInf: "abc".charAt(Infinity),
			aSliceInf: [1, 2, 3].slice(0, Infinity),
			aIdxInf: [1, 2, 3].indexOf(2, Infinity),
			aIncNegInf: [1, 2, 3].includes(1, -Infinity),
			aSpliceInf: (function() { var x = [1, 2, 3]; var r = x.splice(Infinity, 1); return x.length + ":" + r.length; })(),
			repEmptyHuge: "".repeat(1e20),
			repNaN: "a".repeat(NaN),
			repUndef: "a".repeat(undefined),
			repNegFrac: "a".repeat(-0.5),
			repHugeErr: (function() { try { return "a".repeat(1e20); } catch (e) { return e.name; } })(),
			repInfErr: (function() { try { return "a".repeat(Infinity); } catch (e) { return e.name; } })(),
			repNegErr: (function() { try { return "a".repeat(-1); } catch (e) { return e.name; } })(),
			splitHuge: "a,b".split(",", 1e20).join("|"),
			splitInf: "a,b".split(",", Infinity).length,
			splitNeg: "a,b".split(",", -1).join("|")
		});
	`)
	if err != nil {
		t.Fatalf("eval float overflow edge cases: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(res.toString()), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := map[string]any{
		"liInf":         float64(2),
		"liHuge":        float64(2),
		"liUtf8Inf":     float64(3),
		"liEmptyInf":    float64(3),
		"liEmptyHuge":   float64(3),
		"liEmptyNegInf": float64(0),
		"liNegInf":      float64(0),
		"liNegInfMiss":  float64(-1),
		"liNaN":         float64(2),
		"ioInf":         float64(-1),
		"ioHuge":        float64(-1),
		"ioEmptyInf":    float64(3),
		"ioNegInf":      float64(1),
		"ioNaN":         float64(1),
		"sSliceInf":     "abc",
		"sSliceNegInf":  "abc",
		"sSubInf":       "bc",
		"sCharInf":      "",
		"aIdxInf":       float64(-1),
		"aIncNegInf":    true,
		"aSpliceInf":    "3:0",
		"repEmptyHuge":  "",
		"repNaN":        "",
		"repUndef":      "",
		"repNegFrac":    "",
		"repHugeErr":    "RangeError",
		"repInfErr":     "RangeError",
		"repNegErr":     "RangeError",
		"splitHuge":     "a|b",
		"splitInf":      float64(0),
		"splitNeg":      "a|b",
	}
	for k, wv := range want {
		if got[k] != wv {
			t.Fatalf("mismatch for %s: got %#v want %#v", k, got[k], wv)
		}
	}
	aSlice, ok := got["aSliceInf"].([]any)
	if !ok || len(aSlice) != 3 {
		t.Fatalf("aSliceInf got %#v, want [1,2,3]", got["aSliceInf"])
	}
}

func TestJSVM_FrozenArrays_ModuleExportsAndObjectFreeze(t *testing.T) {
	ctx := context.Background()
	store, kr, i18nCat, tmpDir := openTestPluginEnv(t)
	mgr := NewManager(ManagerOptions{
		Store:        store,
		Keys:         kr,
		Catalog:      i18nCat,
		PluginsDir:   filepath.Join(tmpDir, "plugins"),
		DataDir:      filepath.Join(tmpDir, "data"),
		RouteTimeout: 2 * time.Second,
	})
	defer func() { _ = mgr.Close(ctx) }()

	zipBytes := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "frozen-array-check",
			"name": {"en-US": "Frozen Array Check"},
			"version": "1.0.0",
			"runtime": "goja",
			"entry": "main.js",
			"permissions": ["http:routes"]
		}`,
		"main.js": `
			function onRequest(req) {
				if (req.path === "/attack") {
					var errs = {};
					module.exports.arr[0] = 999;
					module.exports.arr.length = 0;
					var delOk = delete module.exports.arr[1];
					try { module.exports.arr.push(777); } catch (e) { errs.push = String(e); }
					try { module.exports.nested.arr.pop(); } catch (e) { errs.pop = String(e); }
					try { module.exports.nested.arr.shift(); } catch (e) { errs.shift = String(e); }
					try { module.exports.nested.arr.unshift("evil"); } catch (e) { errs.unshift = String(e); }
					try { module.exports.nested.arr.splice(0, 1, "evil"); } catch (e) { errs.splice = String(e); }
					try { module.exports.arr.reverse(); } catch (e) { errs.reverse = String(e); }
					try { module.exports.arr.sort(); } catch (e) { errs.sort = String(e); }

					var fa = Object.freeze([10, 20]);
					fa[0] = 99;
					var faDel = delete fa[1];
					var faPushErr = "";
					try { fa.push(30); } catch (e) { faPushErr = String(e); }
					return {
						status: 200,
						body: JSON.stringify({
							delOk: delOk,
							faDel: faDel,
							faFrozen: Object.isFrozen(fa),
							expFrozen: Object.isFrozen(module.exports.arr),
							nestedFrozen: Object.isFrozen(module.exports.nested.arr),
							fa: fa,
							faPushErr: faPushErr,
							errs: errs
						})
					};
				}
				return {
					status: 200,
					body: JSON.stringify({
						arr: module.exports.arr,
						nestedArr: module.exports.nested.arr
					})
				};
			}
			module.exports = {
				onRequest: onRequest,
				arr: [2, 1],
				nested: { arr: ["a", "b"] }
			};
		`,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zipBytes), int64(len(zipBytes)), true, "en-US"); err != nil {
		t.Fatalf("InstallFromZip: %v", err)
	}
	if err := mgr.Enable(ctx, "frozen-array-check"); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	r1 := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(r1, httptest.NewRequest(http.MethodGet, "/plugins/frozen-array-check/api/attack", nil))
	if r1.Code != http.StatusOK {
		t.Fatalf("attack status=%d body=%s", r1.Code, r1.Body.String())
	}
	var attack map[string]any
	if err := json.Unmarshal(r1.Body.Bytes(), &attack); err != nil {
		t.Fatalf("decode attack JSON: %v", err)
	}
	if attack["delOk"] != false || attack["faDel"] != false || attack["faFrozen"] != true || attack["expFrozen"] != true || attack["nestedFrozen"] != true {
		t.Fatalf("unexpected frozen flags: %+v", attack)
	}
	if faSlice, _ := attack["fa"].([]any); len(faSlice) != 2 || faSlice[0] != float64(10) || faSlice[1] != float64(20) {
		t.Fatalf("Object.freeze([10,20]) mutated: %+v", attack["fa"])
	}
	if pushErr, _ := attack["faPushErr"].(string); !strings.Contains(pushErr, "TypeError") {
		t.Fatalf("expected TypeError on fa.push, got %q", pushErr)
	}
	errsMap, _ := attack["errs"].(map[string]any)
	for _, op := range []string{"push", "pop", "shift", "unshift", "splice", "reverse", "sort"} {
		if msg, _ := errsMap[op].(string); !strings.Contains(msg, "TypeError") {
			t.Fatalf("expected TypeError on frozen array.%s, got %#v", op, errsMap[op])
		}
	}

	r2 := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(r2, httptest.NewRequest(http.MethodGet, "/plugins/frozen-array-check/api/check", nil))
	if r2.Code != http.StatusOK {
		t.Fatalf("check status=%d body=%s", r2.Code, r2.Body.String())
	}
	var check map[string]any
	if err := json.Unmarshal(r2.Body.Bytes(), &check); err != nil {
		t.Fatalf("decode check JSON: %v", err)
	}
	arrGot, _ := check["arr"].([]any)
	nestedGot, _ := check["nestedArr"].([]any)
	if len(arrGot) != 2 || arrGot[0] != float64(2) || arrGot[1] != float64(1) {
		t.Fatalf("module.exports.arr mutated across pooled VM calls: %+v", arrGot)
	}
	if len(nestedGot) != 2 || nestedGot[0] != "a" || nestedGot[1] != "b" {
		t.Fatalf("module.exports.nested.arr mutated across pooled VM calls: %+v", nestedGot)
	}
}

func TestJSVM_RepeatFloatOverflow_RouteTryCatchAndNoCircuitTrip(t *testing.T) {
	ctx := context.Background()
	store, kr, i18nCat, tmpDir := openTestPluginEnv(t)
	mgr := NewManager(ManagerOptions{
		Store:        store,
		Keys:         kr,
		Catalog:      i18nCat,
		PluginsDir:   filepath.Join(tmpDir, "plugins"),
		DataDir:      filepath.Join(tmpDir, "data"),
		MaxFailures:  2,
		RouteTimeout: 2 * time.Second,
	})
	defer func() { _ = mgr.Close(ctx) }()

	zipBytes := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "repeat-overflow",
			"name": {"en-US": "Repeat Overflow"},
			"version": "1.0.0",
			"runtime": "goja",
			"entry": "main.js",
			"permissions": ["http:routes"]
		}`,
		"main.js": `
			function onRequest(req) {
				var emptyHuge = "".repeat(1e20);
				var errName = "";
				try {
					"a".repeat(1e20);
				} catch (e) {
					errName = e && e.name ? e.name : String(e);
				}
				return {
					status: 200,
					body: JSON.stringify({
						emptyHuge: emptyHuge,
						errName: errName
					})
				};
			}
			module.exports = { onRequest: onRequest };
		`,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zipBytes), int64(len(zipBytes)), true, "en-US"); err != nil {
		t.Fatalf("InstallFromZip: %v", err)
	}
	if err := mgr.Enable(ctx, "repeat-overflow"); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	for i := 0; i < 4; i++ {
		rec := httptest.NewRecorder()
		mgr.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/plugins/repeat-overflow/api/test", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("req %d: expected HTTP 200 caught RangeError, got %d %q", i, rec.Code, rec.Body.String())
		}
		var got map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("req %d: unmarshal: %v", i, err)
		}
		if got["emptyHuge"] != "" || got["errName"] != "RangeError" {
			t.Fatalf("req %d: unexpected result %+v", i, got)
		}
	}

	mp, _ := mgr.getPlugin("repeat-overflow")
	mp.mu.Lock()
	fc, cs, en := mp.failureCount, mp.circuitState, mp.enabled
	mp.mu.Unlock()
	if fc != 0 || cs != CircuitClosed || !en {
		t.Fatalf("expected caught repeat(1e20) not to trip circuit, got fc=%d state=%s enabled=%v", fc, cs, en)
	}
}

func TestWithPooledVM_SingleBoundedTail_PreventsCompileAndSyncLoopBypass(t *testing.T) {
	ctx := context.Background()
	store, kr, i18nCat, tmpDir := openTestPluginEnv(t)
	provReg := NewProviderRegistry(nil, nil)

	var slowSyncCalls atomic.Int32
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/slow-dict":
			slowSyncCalls.Add(1)
			select {
			case <-time.After(70 * time.Millisecond):
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("bad1\nbad2\n"))
			case <-r.Context().Done():
			}
		case "/fast-dict":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("evilword1\nevilword2\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockSrv.Close()

	mgr := NewManager(ManagerOptions{
		Store:                 store,
		Keys:                  kr,
		Providers:             provReg,
		Catalog:               i18nCat,
		PluginsDir:            filepath.Join(tmpDir, "plugins"),
		DataDir:               filepath.Join(tmpDir, "data"),
		HookTimeout:           50 * time.Millisecond,
		RouteTimeout:          50 * time.Millisecond,
		CronTimeout:           50 * time.Millisecond,
		MaxFailures:           20,
		AllowLoopbackForTests: true,
	})
	defer func() { _ = mgr.Close(ctx) }()

	// 1. Zero-permission plugin attempting 12-iteration compile loop (20,000 deny words each) under HookTimeout=50ms.
	var denyWordsBuf strings.Builder
	for w := 0; w < 20000; w++ {
		fmt.Fprintf(&denyWordsBuf, "denyword_%d_token\n", w)
	}
	compileLoopZip := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "zero-perm-compile-loop",
			"name": {"en-US": "Zero Perm Compile Loop"},
			"version": "1.0.0",
			"runtime": "goja",
			"entry": "main.js",
			"permissions": ["render:directive", "hooks:render.directive"]
		}`,
		"main.js": fmt.Sprintf(`
			var denyWords = %q;
			function renderDirective(d) {
				for (var i = 0; i < 12; i++) {
					try {
						volo.wordFilter.compile("sig:" + i, { deny: denyWords });
					} catch (_e) {}
				}
				return "<div>bypassed-hook-timeout</div>";
			}
			module.exports = { renderDirective: renderDirective };
		`, denyWordsBuf.String()),
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(compileLoopZip), int64(len(compileLoopZip)), true, "en-US"); err != nil {
		t.Fatalf("InstallFromZip compile-loop: %v", err)
	}
	if err := mgr.Enable(ctx, "zero-perm-compile-loop"); err != nil {
		t.Fatalf("Enable compile-loop: %v", err)
	}

	t0 := time.Now()
	htmlOut, handled := mgr.DispatchRenderDirective(context.Background(), "bypass", nil, "body")
	compileElapsed := time.Since(t0)
	if handled || htmlOut != "" {
		t.Fatalf("zero-perm compile loop bypassed HookTimeout! elapsed=%v handled=%v html=%q", compileElapsed, handled, htmlOut)
	}
	mpCompile, _ := mgr.getPlugin("zero-perm-compile-loop")
	mpCompile.mu.Lock()
	rtCompile := mpCompile.gojaRT
	mpCompile.mu.Unlock()
	if rtCompile.hasWordMatcher("sig:11") {
		t.Fatalf("expected compile loop to abort before completing all 12 iterations (elapsed=%v)", compileElapsed)
	}

	// 2. syncRemote loop (10 calls x 70ms delay under HookTimeout=50ms) must abort after the single 50ms tail.
	syncLoopZip := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "sync-loop-guard",
			"name": {"en-US": "Sync Loop Guard"},
			"version": "1.0.0",
			"runtime": "goja",
			"entry": "main.js",
			"permissions": ["render:directive", "hooks:render.directive", "http:outbound", "kv"],
			"config_schema": [
				{"key": "api_url", "type": "url", "default": "https://example.invalid", "label": {"en-US": "API URL"}}
			]
		}`,
		"main.js": `
			function renderDirective(d) {
				var base = volo.config.get("api_url");
				for (var i = 0; i < 10; i++) {
					try {
						volo.wordFilter.syncRemote({ url: base + "/slow-dict", kv_prefix: "loop" + i, force: true });
					} catch (_e) {}
				}
				return "<div>sync-loop-bypassed</div>";
			}
			module.exports = { renderDirective: renderDirective };
		`,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(syncLoopZip), int64(len(syncLoopZip)), true, "en-US"); err != nil {
		t.Fatalf("InstallFromZip sync-loop: %v", err)
	}
	if _, err := mgr.UpdateConfig(ctx, "sync-loop-guard", map[string]any{"api_url": mockSrv.URL}, "en-US"); err != nil {
		t.Fatalf("UpdateConfig sync-loop: %v", err)
	}
	if err := mgr.Enable(ctx, "sync-loop-guard"); err != nil {
		t.Fatalf("Enable sync-loop: %v", err)
	}

	t1 := time.Now()
	syncHTML, syncHandled := mgr.DispatchRenderDirective(context.Background(), "syncloop", nil, "body")
	syncElapsed := time.Since(t1)
	if syncHandled || syncHTML != "" {
		t.Fatalf("syncRemote loop bypassed HookTimeout! elapsed=%v handled=%v html=%q", syncElapsed, syncHandled, syncHTML)
	}
	if calls := slowSyncCalls.Load(); calls > 2 {
		t.Fatalf("expected syncRemote loop to abort within at most 2 HTTP attempts (initial + single tail), got %d calls (elapsed=%v)", calls, syncElapsed)
	}

	// 3. Legitimate sensitive-words 2-compile ensureCompiled (64k+ bundled words + reserved nicknames) under HookTimeout=50ms,
	//    plus POST /sync (RouteTimeout=2s) and onCronSync (CronTimeout=5s) must continue working cleanly.
	swStore, swKR, swI18n, swTmpDir := openTestPluginEnv(t)
	swProvReg := NewProviderRegistry(nil, nil)
	swPluginsDir := filepath.Join(swTmpDir, "plugins")
	copyDirRecursive(t, filepath.Join("..", "..", "plugins", "sensitive-words"), filepath.Join(swPluginsDir, "sensitive-words"))
	swMgr := NewManager(ManagerOptions{
		Store:                 swStore,
		Keys:                  swKR,
		Providers:             swProvReg,
		Catalog:               swI18n,
		PluginsDir:            swPluginsDir,
		DataDir:               filepath.Join(swTmpDir, "data"),
		HookTimeout:           50 * time.Millisecond,
		RouteTimeout:          2 * time.Second,
		CronTimeout:           5 * time.Second,
		MaxFailures:           5,
		AllowLoopbackForTests: true,
	})
	swMgr.SetRequestInspector(func(r *http.Request) (bool, string) {
		return r.Header.Get("X-Test-Admin") == "1", "127.0.0.1"
	})
	defer func() { _ = swMgr.Close(ctx) }()

	if err := swMgr.DiscoverAndStart(ctx); err != nil {
		t.Fatalf("DiscoverAndStart sensitive-words: %v", err)
	}
	if err := swMgr.Enable(ctx, "sensitive-words"); err != nil {
		t.Fatalf("Enable sensitive-words: %v", err)
	}
	if _, err := swMgr.UpdateConfig(ctx, "sensitive-words", map[string]any{
		"auto_update": false,
		"update_url":  mockSrv.URL + "/fast-dict",
	}, "en-US"); err != nil {
		t.Fatalf("UpdateConfig sensitive-words: %v", err)
	}

	syncReq := httptest.NewRequest(http.MethodPost, "/plugins/sensitive-words/api/sync", nil)
	syncReq.Header.Set("X-Test-Admin", "1")
	syncRec := httptest.NewRecorder()
	swMgr.Handler().ServeHTTP(syncRec, syncReq)
	if syncRec.Code != http.StatusOK || !strings.Contains(syncRec.Body.String(), `"ok":true`) {
		t.Fatalf("sensitive-words POST /sync failed: code=%d body=%s", syncRec.Code, syncRec.Body.String())
	}

	verdict := swProvReg.EvaluateComment(context.Background(), CommentCheckInput{
		AuthorName: "guest",
		ContentMD:  "contains evilword1 here",
		IsGuest:    true,
		Locale:     "en-US",
	})
	if verdict.Action != SpamActionReject {
		t.Fatalf("expected sensitive-words EvaluateComment under 50ms HookTimeout to reject evilword1, got %+v", verdict)
	}

	if err := swMgr.executeScheduledCron(ctx, "sensitive-words", "sync_dict", "onCronSync"); err != nil {
		t.Fatalf("sensitive-words onCronSync failed: %v", err)
	}
}

func TestColdVMBurst_DefaultHookTimeout_KanbanniangAndCancellation(t *testing.T) {
	ctx := context.Background()
	store, kr, i18nCat, tmpDir := openTestPluginEnv(t)
	pluginsDir := filepath.Join(tmpDir, "plugins")
	copyDirRecursive(t, filepath.Join("..", "..", "plugins", "kanbanniang"), filepath.Join(pluginsDir, "kanbanniang"))

	// Omit HookTimeout so Manager uses DefaultHookTimeout (500ms), matching cmd/volo/main.go.
	mgr := NewManager(ManagerOptions{
		Store:      store,
		Keys:       kr,
		Catalog:    i18nCat,
		PluginsDir: pluginsDir,
		DataDir:    filepath.Join(tmpDir, "data"),
	})
	defer func() { _ = mgr.Close(ctx) }()

	if mgr.hookTimeout != DefaultHookTimeout || DefaultHookTimeout != 500*time.Millisecond {
		t.Fatalf("expected DefaultHookTimeout=500ms, got %v", mgr.hookTimeout)
	}
	if err := mgr.DiscoverAndStart(ctx); err != nil {
		t.Fatalf("DiscoverAndStart: %v", err)
	}
	if err := mgr.Enable(ctx, "kanbanniang"); err != nil {
		t.Fatalf("Enable kanbanniang: %v", err)
	}

	assertParallelFooterBurst := func(label string, n int) {
		t.Helper()
		var wg sync.WaitGroup
		errs := make(chan string, n)
		startGate := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				<-startGate
				slots := mgr.RenderSlots(ctx, SlotPublicFooter, SlotRenderContext{
					Locale: "zh-CN",
					Path:   fmt.Sprintf("/post/%d", idx),
				})
				var found bool
				for _, s := range slots {
					if s.PluginID == "kanbanniang" && strings.Contains(s.HTML, `id="volo-kanbanniang-root"`) {
						found = true
						break
					}
				}
				if !found {
					var diag string
					if mp, ok := mgr.getPlugin("kanbanniang"); ok {
						mp.mu.Lock()
						diag = fmt.Sprintf(" fc=%d state=%s enabled=%v lastError=%q", mp.failureCount, mp.circuitState, mp.enabled, mp.lastError)
						mp.mu.Unlock()
					}
					errs <- fmt.Sprintf("%s worker %d: kanbanniang public.footer slot dropped (got %d slots)%s", label, idx, len(slots), diag)
				}
			}(i)
		}
		close(startGate)
		wg.Wait()
		close(errs)
		for msg := range errs {
			t.Fatal(msg)
		}

		mp, ok := mgr.getPlugin("kanbanniang")
		if !ok {
			t.Fatalf("%s: kanbanniang missing", label)
		}
		mp.mu.Lock()
		fc, st, en, le := mp.failureCount, mp.circuitState, mp.enabled, mp.lastError
		mp.mu.Unlock()
		if fc != 0 || st != CircuitClosed || !en {
			t.Fatalf("%s: expected zero failures and CircuitClosed, got fc=%d state=%s enabled=%v lastError=%q", label, fc, st, en, le)
		}
	}

	// 1. 8 concurrent cold-pool RenderSlots immediately after Enable("kanbanniang").
	assertParallelFooterBurst("post-enable-burst", 8)

	// 2. UpdateConfig drains VM pool via resetPool() and forces cold selectModelDescriptorJson for Potion-Maker/Pio.
	if _, err := mgr.UpdateConfig(ctx, "kanbanniang", map[string]any{
		"model": "Potion-Maker/Pio",
	}, "en-US"); err != nil {
		t.Fatalf("UpdateConfig kanbanniang: %v", err)
	}
	assertParallelFooterBurst("post-update-config-burst", 8)

	// 3. Caller context.Canceled must still be respected and never increment failureCount.
	canceledCtx, cancelFn := context.WithCancel(ctx)
	cancelFn()
	if slots := mgr.RenderSlots(canceledCtx, SlotPublicFooter, SlotRenderContext{Locale: "en-US"}); len(slots) != 0 {
		t.Fatalf("expected canceled RenderSlots to return empty, got %d", len(slots))
	}
	mpKbn, _ := mgr.getPlugin("kanbanniang")
	mpKbn.mu.Lock()
	if mpKbn.failureCount != 0 || mpKbn.circuitState != CircuitClosed || !mpKbn.enabled {
		mpKbn.mu.Unlock()
		t.Fatalf("expected canceled RenderSlots not to record failure, got fc=%d state=%s enabled=%v", mpKbn.failureCount, mpKbn.circuitState, mpKbn.enabled)
	}
	mpKbn.mu.Unlock()

	// 4. Synthetic cold-init isolation: slow top-level init (~60ms) must not consume HookTimeout (90ms),
	//    while slow/looping hook execution still trips CircuitOpen and shorter caller deadline is inherited.
	st2, kr2, cat2, tmp2 := openTestPluginEnv(t)
	mgr2 := NewManager(ManagerOptions{
		Store:       st2,
		Keys:        kr2,
		Catalog:     cat2,
		PluginsDir:  filepath.Join(tmp2, "plugins"),
		DataDir:     filepath.Join(tmp2, "data"),
		HookTimeout: 90 * time.Millisecond,
		MaxFailures: 2,
	})
	defer func() { _ = mgr2.Close(ctx) }()

	// Each JS spin iteration takes a fraction of a millisecond; calibrate in Go via JSON/loop or Date.now().
	coldZip := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "cold-budget-check",
			"name": {"en-US": "Cold Budget Check"},
			"version": "1.0.0",
			"runtime": "goja",
			"entry": "main.js",
			"permissions": ["slots:public.footer", "hooks:comment.before_create"],
			"ui_slots": [{"slot": "public.footer", "dynamic": true}]
		}`,
		"main.js": `
			var tInit = Date.now();
			while (Date.now() - tInit < 65) {}
			function renderSlot(slot, sctx) {
				if (sctx && sctx.path === "/loop") {
					while (true) {}
				}
				var tHook = Date.now();
				while (Date.now() - tHook < 20) {}
				return "<div>cold-ok</div>";
			}
			function onHook(name, payload) {
				if (payload && payload.hang) {
					while (true) {}
				}
				var tHook = Date.now();
				while (Date.now() - tHook < 20) {}
				return payload;
			}
			module.exports = { renderSlot: renderSlot, onHook: onHook };
		`,
	}, "")
	if _, err := mgr2.InstallFromZip(ctx, bytes.NewReader(coldZip), int64(len(coldZip)), true, "en-US"); err != nil {
		t.Fatalf("InstallFromZip cold-budget-check: %v", err)
	}
	if err := mgr2.Enable(ctx, "cold-budget-check"); err != nil {
		t.Fatalf("Enable cold-budget-check: %v", err)
	}
	mpCold, _ := mgr2.getPlugin("cold-budget-check")
	mpCold.mu.Lock()
	coldRT := mpCold.gojaRT
	mpCold.mu.Unlock()
	coldRT.resetPool()

	// 4 parallel cold invocations (65ms init + 20ms exec = 85ms; under -race init may be ~120ms, exec ~30ms < 90ms).
	// Wait: resetPool emptied pool, so all 4 call createVM() (~65ms init) + execute (~20ms < 90ms HookTimeout).
	var wg2 sync.WaitGroup
	errs2 := make(chan string, 4)
	gate2 := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg2.Add(1)
		go func(idx int) {
			defer wg2.Done()
			<-gate2
			slots := mgr2.RenderSlots(ctx, SlotPublicFooter, SlotRenderContext{Path: "/ok"})
			if len(slots) != 1 || !strings.Contains(slots[0].HTML, "cold-ok") {
				errs2 <- fmt.Sprintf("cold burst worker %d dropped slot: %+v", idx, slots)
			}
		}(i)
	}
	close(gate2)
	wg2.Wait()
	close(errs2)
	for msg := range errs2 {
		t.Fatal(msg)
	}

	// Shorter caller deadline (15ms < 90ms HookTimeout) on invokeSlot must be inherited and cancel fast.
	shortCtx, shortCancel := context.WithTimeout(ctx, 15*time.Millisecond)
	defer shortCancel()
	tShort := time.Now()
	_, _, sErr := mgr2.invokeSlot(shortCtx, mpCold, SlotPublicFooter, SlotRenderContext{Path: "/loop"})
	shortElapsed := time.Since(tShort)
	if isRaceBuild {
		if !errors.Is(sErr, context.DeadlineExceeded) {
			t.Fatalf("expected context.DeadlineExceeded on shorter caller deadline (15ms < 90ms), got %v", sErr)
		}
		if shortElapsed > 500*time.Millisecond {
			t.Fatalf("expected shorter caller deadline (15ms) under -race to cancel promptly, took %v", shortElapsed)
		}
	} else {
		if !errors.Is(sErr, context.DeadlineExceeded) && !errors.Is(sErr, ErrExecutionTimeout) {
			t.Fatalf("expected shorter caller deadline error on /loop, got %v", sErr)
		} else if shortElapsed >= 75*time.Millisecond {
			t.Fatalf("expected shorter caller deadline (15ms) to cancel before HookTimeout (90ms), took %v", shortElapsed)
		}
	}

	// Runaway JS execution in renderSlot & onHook still times out in HookTimeout and trips CircuitOpen.
	_ = mgr2.RenderSlots(ctx, SlotPublicFooter, SlotRenderContext{Path: "/loop"})
	_, _ = mgr2.DispatchHook(ctx, HookCommentBeforeCreate, map[string]any{"hang": true})
	mpCold.mu.Lock()
	if mpCold.circuitState != CircuitOpen || mpCold.enabled {
		mpCold.mu.Unlock()
		t.Fatalf("expected runaway hook timeouts to trip CircuitOpen, got state=%s enabled=%v fc=%d", mpCold.circuitState, mpCold.enabled, mpCold.failureCount)
	}
	mpCold.mu.Unlock()
}

func TestKVDelete_vs_IncrBuffered_NoResurrection(t *testing.T) {
	ctx := context.Background()
	store, _, _, _ := openTestPluginEnv(t)
	kv := NewKVStore(store, "kv-del-race")
	hub := getOrCreateKVHub(store)
	if hub == nil {
		t.Fatal("expected kvHub registered for store")
	}

	const rounds = 60
	for i := 0; i < rounds; i++ {
		key := fmt.Sprintf("race-key-%d", i)
		if err := kv.Set(ctx, key, "100", 0); err != nil {
			t.Fatalf("round %d Set: %v", i, err)
		}
		// Evict cached entry from hub so IncrBuffered performs a cold-miss ReadDB lookup
		pk := kvPluginKey{pluginID: kv.pluginID, key: key}
		hub.mu.Lock()
		delete(hub.dirty, pk)
		delete(hub.entries, pk)
		hub.mu.Unlock()

		var wg sync.WaitGroup
		start := make(chan struct{})
		var incrErr error
		var delErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, incrErr = kv.IncrBuffered(ctx, key, 1)
		}()
		go func() {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				delErr = kv.Delete(ctx, key)
			} else {
				delErr = ClearPluginKV(ctx, store, kv.pluginID)
			}
		}()
		close(start)
		wg.Wait()
		if incrErr != nil {
			t.Fatalf("round %d IncrBuffered: %v", i, incrErr)
		}
		if delErr != nil {
			t.Fatalf("round %d Delete/Clear: %v", i, delErr)
		}
		if err := kv.Flush(ctx); err != nil {
			t.Fatalf("round %d Flush: %v", i, err)
		}
		val, ok, gErr := kv.Get(ctx, key)
		if gErr != nil {
			t.Fatalf("round %d Get: %v", i, gErr)
		}
		if ok && val != "1" {
			t.Fatalf("round %d: deleted base=100 resurrected via IncrBuffered! got ok=%v val=%q", i, ok, val)
		}
	}
}

func TestJSVM_GoBuiltinInterruptNotSwallowedByTryCatch(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{name: "context.Canceled", err: context.Canceled},
		{name: "context.DeadlineExceeded", err: context.DeadlineExceeded},
		{name: "ErrExecutionTimeout", err: ErrExecutionTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := newJSRuntime()
			hostObj := rt.NewObject()
			hostObj.Set("fail", func() (string, error) {
				return "", tc.err
			})
			rt.global.define("host", jsValue{kind: valObject, obj: hostObj}, false)

			prog, pErr := compileJSProgram("try_catch_interrupt.js", `
				var caught = false;
				try {
					host.fail();
				} catch (e) {
					caught = true;
				}
				caught;
			`)
			if pErr != nil {
				t.Fatalf("compileJSProgram: %v", pErr)
			}
			_, err := rt.RunProgram(prog)
			if err == nil {
				t.Fatalf("expected %v to interrupt JS execution and bypass try/catch, got nil error", tc.err)
			}
			var inter *jsInterruptedError
			if !errors.As(err, &inter) {
				t.Fatalf("expected *jsInterruptedError, got %T: %v", err, err)
			}
			if !errors.Is(err, tc.err) {
				t.Fatalf("expected unwrapped error %v, got %v", tc.err, err)
			}
			caughtVal := rt.Get("caught")
			if b, ok := caughtVal.Export().(bool); ok && b {
				t.Fatalf("catch block executed for %v; interrupt was swallowed", tc.err)
			}
		})
	}
}

func TestRuntimeMemoAndBatchExistsAll(t *testing.T) {
	ctx := context.Background()
	store, kr, cat, tmpDir := openTestPluginEnv(t)
	mgr := NewManager(ManagerOptions{
		Store:      store,
		Keys:       kr,
		Catalog:    cat,
		PluginsDir: filepath.Join(tmpDir, "plugins"),
		DataDir:    filepath.Join(tmpDir, "data"),
	})
	defer func() { _ = mgr.Close(ctx) }()

	zipBytes := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "memo-batch-test",
			"name": {"en-US": "Memo Batch Test"},
			"version": "1.0.0",
			"runtime": "goja",
			"entry": "main.js",
			"permissions": ["slots:public.footer"],
			"config_schema": [{"key": "label", "type": "string", "default": "a", "label": {"en-US": "Label"}}],
			"ui_slots": [{"slot": "public.footer", "dynamic": true}]
		}`,
		"assets/a.txt": "alpha",
		"assets/b.txt": "beta",
		"main.js": `
			function renderSlot(slot, sctx) {
				var nonStr = volo.memo.set("bad", { obj: true });
				var tooBig = volo.memo.set("huge", "x".repeat(600000));
				var okSet = volo.memo.set("k1", "v1");
				var got = volo.memo.get("k1");
				var existsOk = volo.assets.existsAll(["assets/a.txt", "assets/a.txt", "assets/b.txt"]);
				var existsTraversal = volo.assets.existsAll(["assets/a.txt", "../manifest.json"]);
				var existsMissing = volo.assets.existsAll(["assets/a.txt", "assets/nope.txt"]);
				var existsEmpty = volo.assets.existsAll([]);
				return JSON.stringify({
					cfg: volo.config.get("label"),
					nonStr: nonStr,
					tooBig: tooBig,
					okSet: okSet,
					got: got,
					existsOk: existsOk,
					existsTraversal: existsTraversal,
					existsMissing: existsMissing,
					existsEmpty: existsEmpty
				});
			}
			function onHook(name, payload) {
				if (name === "checkMemo") {
					return { k1: volo.memo.get("k1") || null, cfg: volo.config.get("label") };
				}
				if (name === "evictTest") {
					for (var i = 0; i < 70; i++) {
						volo.memo.set("ev:" + i, "val-" + i);
					}
					volo.memo.delete("ev:69");
					return {
						oldEvicted: volo.memo.get("ev:0") === undefined,
						deletedGone: volo.memo.get("ev:69") === undefined,
						keptTail: volo.memo.get("ev:68") === "val-68"
					};
				}
			}
			module.exports = { renderSlot: renderSlot, onHook: onHook };
		`,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zipBytes), int64(len(zipBytes)), true, "en-US"); err != nil {
		t.Fatalf("InstallFromZip: %v", err)
	}
	if err := mgr.Enable(ctx, "memo-batch-test"); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	slots := mgr.RenderSlots(ctx, SlotPublicFooter, SlotRenderContext{Path: "/"})
	if len(slots) != 1 {
		t.Fatalf("expected 1 slot, got %d", len(slots))
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(slots[0].HTML), &res); err != nil {
		t.Fatalf("unmarshal slot output %q: %v", slots[0].HTML, err)
	}
	if res["cfg"] != "a" || res["nonStr"] != false || res["tooBig"] != false || res["okSet"] != true || res["got"] != "v1" ||
		res["existsOk"] != true || res["existsTraversal"] != false || res["existsMissing"] != false || res["existsEmpty"] != true {
		t.Fatalf("unexpected memo/existsAll result: %+v", res)
	}

	// UpdateConfig resets VM pool (and warms defaultVMPoolCap VMs), but preserves volo.memo across pool reset.
	if _, err := mgr.UpdateConfig(ctx, "memo-batch-test", map[string]any{"label": "b"}, "en-US"); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	mp, _ := mgr.getPlugin("memo-batch-test")
	rt := mp.gojaRT
	out, _, err := rt.invoke(ctx, time.Second, "onHook", "checkMemo", nil)
	if err != nil {
		t.Fatalf("invoke checkMemo: %v", err)
	}
	outMap, _ := out.(map[string]any)
	if outMap["k1"] != "v1" || outMap["cfg"] != "b" {
		t.Fatalf("expected memo preserved and config updated after UpdateConfig, got %+v", outMap)
	}

	// Verify bounded FIFO eviction (64 entries) + delete.
	evOut, _, err := rt.invoke(ctx, time.Second, "onHook", "evictTest", nil)
	if err != nil {
		t.Fatalf("invoke evictTest: %v", err)
	}
	evMap, _ := evOut.(map[string]any)
	if evMap["oldEvicted"] != true || evMap["deletedGone"] != true || evMap["keptTail"] != true {
		t.Fatalf("unexpected eviction result: %+v", evMap)
	}

	// Verify closeRuntime clears memo.
	rt.closeRuntime()
	if _, ok := rt.getMemo("ev:68"); ok {
		t.Fatalf("expected closeRuntime to clear memo")
	}
}

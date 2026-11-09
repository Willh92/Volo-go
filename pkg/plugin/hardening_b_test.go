package plugin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Willh92/Volo-go/internal/db/sqlx"
	"github.com/Willh92/Volo-go/internal/domain"
)

func TestJSVM_JSONParse_CatchableRangeError_DepthArrayAndNodeCaps(t *testing.T) {
	vm := newJSRuntime()
	vm.SetMaxCallStackSize(defaultVMStackLimit)

	// 1. Deeply nested JSON (> 64 levels) must throw a JS-catchable RangeError inside try/catch.
	deepJSON := strings.Repeat("[", 70) + "1" + strings.Repeat("]", 70)
	vm.Set("deepInput", deepJSON)
	res, err := evalJSScript(vm, `
		try {
			JSON.parse(deepInput);
			return "unexpected_ok";
		} catch (e) {
			return e.name + ":" + e.message;
		}
	`)
	if err != nil {
		t.Fatalf("expected JS try/catch to catch deep JSON.parse error cleanly, got Go error: %v", err)
	}
	if got := res.toString(); !strings.HasPrefix(got, "RangeError:") {
		t.Fatalf("expected RangeError from deep JSON.parse, got %q", got)
	}

	// 2. Oversized JSON array (> 65,536 elements) must throw a JS-catchable RangeError.
	var arrBuf strings.Builder
	arrBuf.Grow(70000 * 2)
	arrBuf.WriteByte('[')
	for i := 0; i < 66000; i++ {
		if i > 0 {
			arrBuf.WriteByte(',')
		}
		arrBuf.WriteByte('0')
	}
	arrBuf.WriteByte(']')
	vm.Set("wideArrayInput", arrBuf.String())
	res, err = evalJSScript(vm, `
		try {
			JSON.parse(wideArrayInput);
			return "unexpected_ok";
		} catch (e) {
			return e.name;
		}
	`)
	if err != nil {
		t.Fatalf("expected JS try/catch to catch oversized array JSON.parse, got Go error: %v", err)
	}
	if got := res.toString(); got != "RangeError" {
		t.Fatalf("expected RangeError from >65536 JSON array, got %q", got)
	}

	// 3. Total converted node budget (> 100,000 nodes even when each array length <= 40,000) must throw catchable RangeError.
	var nodeBomb strings.Builder
	nodeBomb.Grow(40000 * 6)
	nodeBomb.WriteByte('[')
	for i := 0; i < 36000; i++ {
		if i > 0 {
			nodeBomb.WriteByte(',')
		}
		nodeBomb.WriteString("[1,2]")
	}
	nodeBomb.WriteByte(']')
	vm.Set("nodeBombInput", nodeBomb.String())
	res, err = evalJSScript(vm, `
		try {
			JSON.parse(nodeBombInput);
			return "unexpected_ok";
		} catch (e) {
			return e.name;
		}
	`)
	if err != nil {
		t.Fatalf("expected JS try/catch to catch node-budget JSON.parse, got Go error: %v", err)
	}
	if got := res.toString(); got != "RangeError" {
		t.Fatalf("expected RangeError from >100k JSON nodes, got %q", got)
	}

	// 4. goToJSValue via native Go function return (> 65,536 items) must also be catchable in JS try/catch!
	vm.Set("makeHugeSlice", func() []int {
		return make([]int, 70000)
	})
	res, err = evalJSScript(vm, `
		try {
			makeHugeSlice();
			return "unexpected_ok";
		} catch (e) {
			return e.name;
		}
	`)
	if err != nil {
		t.Fatalf("expected JS try/catch to catch oversized goToJSValue conversion, got Go error: %v", err)
	}
	if got := res.toString(); got != "RangeError" {
		t.Fatalf("expected RangeError from oversized native return slice, got %q", got)
	}
}

func TestJSVM_ParserAndLexer_MaxRecursionDepth128(t *testing.T) {
	// 1. Deeply nested parenthesized expressions (> 128 levels) -> clean parse error.
	deepParens := "return " + strings.Repeat("(", 140) + "1" + strings.Repeat(")", 140) + ";"
	if _, err := compileJSProgram("deep_parens.js", deepParens); err == nil || !strings.Contains(err.Error(), "nesting depth") {
		t.Fatalf("expected clean SyntaxError on >128 nested parens, got %v", err)
	}

	// 2. Deeply nested block statements (> 128 levels) -> clean parse error.
	deepBlocks := strings.Repeat("{ ", 140) + "let x = 1;" + strings.Repeat(" }", 140)
	if _, err := compileJSProgram("deep_blocks.js", deepBlocks); err == nil || !strings.Contains(err.Error(), "nesting depth") {
		t.Fatalf("expected clean SyntaxError on >128 nested blocks, got %v", err)
	}

	// 3. Deeply nested template literal ${...} interpolations (> 128 levels) -> clean lexer error.
	var tpl strings.Builder
	tpl.WriteString("return ")
	for i := 0; i < 135; i++ {
		tpl.WriteString("`${")
	}
	tpl.WriteString("1")
	for i := 0; i < 135; i++ {
		tpl.WriteString("}`")
	}
	tpl.WriteByte(';')
	if _, err := compileJSProgram("deep_tpl.js", tpl.String()); err == nil || !strings.Contains(err.Error(), "nesting depth") {
		t.Fatalf("expected clean SyntaxError on >128 nested template literals, got %v", err)
	}

	// 4. Reasonable nesting (32 levels) continues to compile and evaluate cleanly.
	vm := newJSRuntime()
	okSrc := "return " + strings.Repeat("(", 32) + "42" + strings.Repeat(")", 32) + ";"
	val, err := evalJSScript(vm, okSrc)
	if err != nil || val.toNumber() != 42 {
		t.Fatalf("expected 32-level expression to evaluate to 42, got val=%v err=%v", val, err)
	}
}

func TestJSVM_JSONStringify_PreservesErrExecutionTimeout(t *testing.T) {
	ctx := context.Background()
	store, kr, cat, tmpDir := openTestPluginEnv(t)
	mgr := NewManager(ManagerOptions{
		Store:        store,
		Keys:         kr,
		Catalog:      cat,
		PluginsDir:   filepath.Join(tmpDir, "plugins"),
		DataDir:      filepath.Join(tmpDir, "data"),
		HookTimeout:  50 * time.Millisecond,
		RouteTimeout: 50 * time.Millisecond,
	})
	defer func() { _ = mgr.Close(ctx) }()

	// Directly verify VM interrupt sentinel preservation inside JSON.stringify.
	vm := newJSRuntime()
	if _, err := evalJSScript(vm, `
		var big = [];
		for (var i = 0; i < 2000; i++) {
			big.push({ idx: i, text: "abcdefghij" });
		}
	`); err != nil {
		t.Fatalf("setup big array: %v", err)
	}
	// Pre-trigger interrupt with ErrExecutionTimeout right before JSON.stringify runs.
	vm.Interrupt(ErrExecutionTimeout)
	_, err := evalJSScript(vm, `return JSON.stringify(big);`)
	if err == nil {
		t.Fatal("expected interrupted JSON.stringify to fail")
	}
	var iErr *jsInterruptedError
	if !errors.As(err, &iErr) {
		t.Fatalf("expected *jsInterruptedError from JSON.stringify, got %T: %v", err, err)
	}
	cause, ok := iErr.Value().(error)
	if !ok || !errors.Is(cause, ErrExecutionTimeout) {
		t.Fatalf("expected JSON.stringify interrupt cause to preserve ErrExecutionTimeout, got %#v", iErr.Value())
	}

	// Also verify end-to-end through gojaPluginRuntime.invoke timeout mapping!
	zipBytes := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "stringify-timeout",
			"name": {"en-US": "Stringify Timeout"},
			"version": "1.0.0",
			"runtime": "goja",
			"entry": "main.js",
			"permissions": ["hooks:article.before_save"]
		}`,
		"main.js": `
			var rows = [];
			for (var i = 0; i < 1500; i++) {
				rows.push({ a: i, b: "payload_value_" + i, c: [1, 2, 3, 4] });
			}
			function onHook() {
				while (true) {
					JSON.stringify(rows);
				}
			}
		`,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zipBytes), int64(len(zipBytes)), true, "en-US"); err != nil {
		t.Fatalf("InstallFromZip: %v", err)
	}
	if err := mgr.Enable(ctx, "stringify-timeout"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	mp, _ := mgr.getPlugin("stringify-timeout")
	mp.mu.Lock()
	rt := mp.gojaRT
	mp.mu.Unlock()
	_, _, invErr := rt.invoke(ctx, 15*time.Millisecond, "onHook")
	if !errors.Is(invErr, ErrExecutionTimeout) {
		t.Fatalf("expected rt.invoke during JSON.stringify loop to return ErrExecutionTimeout, got %v", invErr)
	}
}

func TestJSVM_TryCatchFinally_FunctionHoisting(t *testing.T) {
	vm := newJSRuntime()
	res, err := evalJSScript(vm, `
		let trace = [];
		let val = 0;
		try {
			val = f();
			function f() { return 42; }
			throw new Error("boom");
		} catch (e) {
			trace.push(g());
			function g() { return "caught:" + val; }
		} finally {
			h();
			function h() { trace.push("finally_ok"); }
		}
		return val + "|" + trace.join(",");
	`)
	if err != nil {
		t.Fatalf("eval try/catch/finally hoisted functions: %v", err)
	}
	if got := res.toString(); got != "42|caught:42,finally_ok" {
		t.Fatalf("unexpected try/catch/finally hoisting result: %q", got)
	}
}

func TestJSVM_RegExpGlobalMatchCap(t *testing.T) {
	vm := newJSRuntime()
	res, err := evalJSScript(vm, `
		const big = "a".repeat(70000);
		const results = [];
		const cases = [
			function() { return big.match(/()/g); },
			function() { return Array.from(big.matchAll(/()/g)); },
			function() { return big.replace(/()/g, ""); },
			function() { return big.replaceAll("", "x"); },
			function() { return big.split(/()/); }
		];
		for (let i = 0; i < cases.length; i++) {
			try {
				cases[i]();
				results.push("ok");
			} catch (e) {
				results.push(e.name);
			}
		}
		return results.join(",");
	`)
	if err != nil {
		t.Fatalf("eval RegExp global match cap: %v", err)
	}
	if got := res.toString(); got != "RangeError,RangeError,RangeError,RangeError,RangeError" {
		t.Fatalf("expected all 5 global empty-regex match operations to throw RangeError, got %q", got)
	}
}

func TestJSVM_PooledVM_BuiltinPrototypeAndGlobalIsolation(t *testing.T) {
	ctx := context.Background()
	store, kr, cat, tmpDir := openTestPluginEnv(t)
	mgr := NewManager(ManagerOptions{
		Store:        store,
		Keys:         kr,
		Catalog:      cat,
		PluginsDir:   filepath.Join(tmpDir, "plugins"),
		DataDir:      filepath.Join(tmpDir, "data"),
		RouteTimeout: 2 * time.Second,
	})
	defer func() { _ = mgr.Close(ctx) }()

	zipBytes := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "pool-isolation",
			"name": {"en-US": "Pool Isolation"},
			"version": "1.0.0",
			"runtime": "goja",
			"entry": "main.js",
			"permissions": ["http:routes"]
		}`,
		"main.js": `
			var moduleInitialized = "boot_v1";
			function onRequest(req) {
				if (req.path === "/pollute") {
					var protoBlocked = 0;
					try { Object.prototype.polluted = "bad"; } catch (e) { protoBlocked++; }
					try { Array.prototype.polluted = "bad"; } catch (e) { protoBlocked++; }
					try { String.prototype.polluted = "bad"; } catch (e) { protoBlocked++; }
					globalThis.leakedSecret = "top_secret_token";
					undeclaredLeak = "leaked_global";
					globalThis.btoa = "hijacked";
					delete globalThis.moduleInitialized;
					return {
						status: 200,
						body: JSON.stringify({ status: "polluted", proto_blocked: protoBlocked })
					};
				}
				return {
					status: 200,
					body: JSON.stringify({
						obj_polluted: ({}).polluted || null,
						arr_polluted: ([]).polluted || null,
						str_polluted: ("").polluted || null,
						global_leaked: typeof globalThis.leakedSecret === "undefined" ? null : globalThis.leakedSecret,
						undeclared_leaked: typeof undeclaredLeak === "undefined" ? null : undeclaredLeak,
						btoa_restored: typeof btoa === "function",
						module_init: moduleInitialized
					})
				};
			}
		`,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zipBytes), int64(len(zipBytes)), true, "en-US"); err != nil {
		t.Fatalf("InstallFromZip: %v", err)
	}
	if err := mgr.Enable(ctx, "pool-isolation"); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	req1 := httptest.NewRequest(http.MethodGet, "/plugins/pool-isolation/api/pollute", nil)
	rec1 := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("pollute status=%d body=%s", rec1.Code, rec1.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodGet, "/plugins/pool-isolation/api/check", nil)
	rec2 := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("check status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	var check map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &check); err != nil {
		t.Fatalf("decode check JSON: %v", err)
	}
	if check["obj_polluted"] != nil || check["arr_polluted"] != nil || check["str_polluted"] != nil {
		t.Fatalf("builtin prototype mutation leaked across pooled VM invocations: %+v", check)
	}
	if check["global_leaked"] != nil || check["undeclared_leaked"] != nil {
		t.Fatalf("globalThis/undeclared property leaked across pooled VM invocations: %+v", check)
	}
	if check["btoa_restored"] != true {
		t.Fatalf("expected overwritten builtin global btoa to be restored on return to pool, got %+v", check)
	}
	if check["module_init"] != "boot_v1" {
		t.Fatalf("expected module-level top-level binding initialized at module load to remain intact, got %+v", check)
	}
}

func TestJSVM_BinarySafeAtobBtoa_And_FetchBodyBase64(t *testing.T) {
	// 1. Direct VM test: all 256 raw byte values 0x00..0xFF map 1:1 in btoa/atob, and > 0xFF throws InvalidCharacterError.
	vm := newJSRuntime()
	res, err := evalJSScript(vm, `
		let raw = "";
		for (let i = 0; i < 256; i++) {
			raw += String.fromCharCode(i);
		}
		const b64 = btoa(raw);
		const decoded = atob(b64);
		if (decoded.length !== 256) {
			return "bad_len:" + decoded.length;
		}
		for (let i = 0; i < 256; i++) {
			if (decoded.charCodeAt(i) !== i) {
				return "mismatch_at:" + i + ":" + decoded.charCodeAt(i);
			}
		}
		let nonLatin1Rejected = false;
		try {
			btoa("\u0100");
		} catch (e) {
			nonLatin1Rejected = String(e.message || e).indexOf("InvalidCharacterError") !== -1 || e.name === "InvalidCharacterError";
		}
		// Verify \xHH escape in JS string literal maps to single code unit 0xFF (not UTF-8 2 chars).
		const hexLit = "\x00\x80\xff";
		const hexOK = hexLit.length === 3 && hexLit.charCodeAt(1) === 128 && hexLit.charCodeAt(2) === 255;
		return b64.length + "|" + nonLatin1Rejected + "|" + hexOK;
	`)
	if err != nil {
		t.Fatalf("btoa/atob eval failed: %v", err)
	}
	// 256 raw bytes encoded in Base64 = ceil(256 / 3) * 4 = 86 * 4 = 344 chars (UTF-8 bug would have been 384 bytes -> 512 chars!).
	if got := res.toString(); got != "344|true|true" {
		t.Fatalf("expected 344|true|true for 0x00..0xFF btoa/atob & \\xHH escapes, got %q", got)
	}

	// 2. End-to-end volo.http.fetch binary upload (body_base64) and binary download (response_encoding: "base64").
	wantUpload := make([]byte, 256)
	for i := range wantUpload {
		wantUpload[i] = byte(255 - i)
	}
	wantDownload := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0xFF, 0xFE, 0x80, 0xC3, 0x28}
	var gotUpload []byte

	binSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotUpload = body
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(wantDownload)
	}))
	defer binSrv.Close()

	ctx := context.Background()
	store, kr, cat, tmpDir := openTestPluginEnv(t)
	mgr := NewManager(ManagerOptions{
		Store:                 store,
		Keys:                  kr,
		Catalog:               cat,
		PluginsDir:            filepath.Join(tmpDir, "plugins"),
		DataDir:               filepath.Join(tmpDir, "data"),
		RouteTimeout:          3 * time.Second,
		AllowLoopbackForTests: true,
	})
	defer func() { _ = mgr.Close(ctx) }()

	zipBytes := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "binary-bridge",
			"name": {"en-US": "Binary Bridge"},
			"version": "1.0.0",
			"runtime": "goja",
			"entry": "main.js",
			"permissions": ["http:outbound", "http:routes"],
			"config_schema": [
				{"key": "target_url", "type": "url", "label": {"en-US": "Target URL"}}
			]
		}`,
		"main.js": `
			function onRequest(req) {
				let raw = "";
				for (let i = 0; i < 256; i++) {
					raw += String.fromCharCode(255 - i);
				}
				const resp = volo.http.fetch(volo.config.get("target_url"), {
					method: "PUT",
					headers: { "Content-Type": "application/octet-stream" },
					body_base64: btoa(raw),
					response_encoding: "base64"
				});
				const dlBin = atob(resp.body_base64);
				const bytes = [];
				for (let i = 0; i < dlBin.length; i++) {
					bytes.push(dlBin.charCodeAt(i));
				}
				return {
					status: 200,
					body: JSON.stringify({
						byte_length: resp.byte_length,
						body_b64: resp.body_base64,
						bytes: bytes
					})
				};
			}
		`,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zipBytes), int64(len(zipBytes)), true, "en-US"); err != nil {
		t.Fatalf("InstallFromZip binary-bridge: %v", err)
	}
	if err := mgr.Enable(ctx, "binary-bridge"); err != nil {
		t.Fatalf("Enable binary-bridge: %v", err)
	}
	if _, err := mgr.UpdateConfig(ctx, "binary-bridge", map[string]any{"target_url": binSrv.URL + "/upload"}, "en-US"); err != nil {
		t.Fatalf("UpdateConfig binary-bridge: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/plugins/binary-bridge/api/run", nil)
	rec := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("binary-bridge status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(gotUpload, wantUpload) {
		t.Fatalf("uploaded binary payload corrupted: got %d bytes, want %d bytes", len(gotUpload), len(wantUpload))
	}
	var out struct {
		ByteLength int    `json:"byte_length"`
		BodyB64    string `json:"body_b64"`
		Bytes      []int  `json:"bytes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.ByteLength != len(wantDownload) || out.BodyB64 != base64.StdEncoding.EncodeToString(wantDownload) {
		t.Fatalf("downloaded binary payload mismatch: %+v", out)
	}
}

func TestBufferedKV_ZeroSyncWriteStorm_ImmediateVisibility_And_LifecycleFlush(t *testing.T) {
	ctx := context.Background()
	store, kr, cat, tmpDir := openTestPluginEnv(t)
	mgr := NewManager(ManagerOptions{
		Store:        store,
		Keys:         kr,
		Catalog:      cat,
		PluginsDir:   filepath.Join(tmpDir, "plugins"),
		DataDir:      filepath.Join(tmpDir, "data"),
		RouteTimeout: 2 * time.Second,
	})

	zipBytes := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "buffered-kv-demo",
			"name": {"en-US": "Buffered KV Demo"},
			"version": "1.0.0",
			"runtime": "goja",
			"entry": "main.js",
			"permissions": ["kv", "http:routes"]
		}`,
		"main.js": `
			function onRequest(req) {
				if (req.method === "POST" && req.path === "/burst") {
					let latest = 0;
					for (let i = 0; i < 250; i++) {
						latest = volo.kv.incrAsync("clicks", 1);
					}
					return {
						status: 200,
						body: JSON.stringify({
							latest: latest,
							get_int: volo.kv.getInt("clicks"),
							get_str: volo.kv.get("clicks")
						})
					};
				}
				return { status: 200, body: String(volo.kv.getInt("clicks")) };
			}
		`,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zipBytes), int64(len(zipBytes)), true, "en-US"); err != nil {
		t.Fatalf("InstallFromZip: %v", err)
	}
	if err := mgr.Enable(ctx, "buffered-kv-demo"); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	// 1. Burst 250 increments in memory -> ZERO synchronous SQLite rows written yet, while KV.Get sees 250 immediately!
	req := httptest.NewRequest(http.MethodPost, "/plugins/buffered-kv-demo/api/burst", nil)
	rec := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("burst failed: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"latest":250`) || !strings.Contains(rec.Body.String(), `"get_int":250`) || !strings.Contains(rec.Body.String(), `"get_str":"250"`) {
		t.Fatalf("expected immediate O(1) visibility of 250, got %s", rec.Body.String())
	}

	var rawRow domain.PluginKV
	rawErr := sqlx.GetInto(ctx, store, &rawRow, `SELECT * FROM plugin_kv WHERE plugin_id = ? AND k = ?`, "buffered-kv-demo", "clicks")
	if !errors.Is(rawErr, sql.ErrNoRows) {
		t.Fatalf("expected zero synchronous SQLite writes before flush, got row=%+v err=%v", rawRow, rawErr)
	}

	// 2. Disable plugin -> deterministically flushes 250 to SQLite!
	if err := mgr.Disable(ctx, "buffered-kv-demo"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if err := sqlx.GetInto(ctx, store, &rawRow, `SELECT * FROM plugin_kv WHERE plugin_id = ? AND k = ?`, "buffered-kv-demo", "clicks"); err != nil {
		t.Fatalf("expected SQLite row after Disable flush, got %v", err)
	}
	if rawRow.ValueInt != 250 || rawRow.ValueText != "250" {
		t.Fatalf("expected persisted count=250 after Disable flush, got %+v", rawRow)
	}

	// 3. Re-enable, burst another 250 (-> 500), and verify Manager.Close flushes deterministically!
	if err := mgr.Enable(ctx, "buffered-kv-demo"); err != nil {
		t.Fatalf("re-Enable: %v", err)
	}
	rec2 := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/plugins/buffered-kv-demo/api/burst", nil))
	if !strings.Contains(rec2.Body.String(), `"latest":500`) {
		t.Fatalf("expected combined base+delta=500, got %s", rec2.Body.String())
	}

	// Also verify Delete discards pending buffered deltas so deleted keys never resurrect on flush.
	kvDirect := NewKVStore(store, "buffered-kv-demo")
	if v, err := kvDirect.IncrBuffered(ctx, "ephemeral_counter", 99); err != nil || v != 99 {
		t.Fatalf("IncrBuffered ephemeral: v=%d err=%v", v, err)
	}
	if err := kvDirect.Delete(ctx, "ephemeral_counter"); err != nil {
		t.Fatalf("Delete ephemeral_counter: %v", err)
	}

	if err := mgr.Close(ctx); err != nil {
		t.Fatalf("Manager.Close: %v", err)
	}
	if err := sqlx.GetInto(ctx, store, &rawRow, `SELECT * FROM plugin_kv WHERE plugin_id = ? AND k = ?`, "buffered-kv-demo", "clicks"); err != nil || rawRow.ValueInt != 500 {
		t.Fatalf("expected persisted count=500 after Manager.Close flush, got row=%+v err=%v", rawRow, err)
	}
	var deletedRow domain.PluginKV
	if err := sqlx.GetInto(ctx, store, &deletedRow, `SELECT * FROM plugin_kv WHERE plugin_id = ? AND k = ?`, "buffered-kv-demo", "ephemeral_counter"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected deleted buffered key not to resurrect on flush, got %+v err=%v", deletedRow, err)
	}
}

func TestInstallFromZip_ActiveUpgradePreservesEnabled_And_OIDCDiscoveryInvalidation(t *testing.T) {
	ctx := context.Background()
	store, kr, cat, tmpDir := openTestPluginEnv(t)
	mgr := NewManager(ManagerOptions{
		Store:                 store,
		Keys:                  kr,
		Catalog:               cat,
		PluginsDir:            filepath.Join(tmpDir, "plugins"),
		DataDir:               filepath.Join(tmpDir, "data"),
		HookTimeout:           2 * time.Second,
		RouteTimeout:          2 * time.Second,
		AllowLoopbackForTests: true,
	})
	defer func() { _ = mgr.Close(ctx) }()

	// Part A: InstallFromZip(..., overwrite=true) on an ENABLED plugin must auto-re-enable the upgraded version!
	makeZip := func(ver string) []byte {
		return buildZipBytes(t, map[string]string{
			"manifest.json": `{
				"id": "hot-upgrade",
				"name": {"en-US": "Hot Upgrade"},
				"version": "` + ver + `",
				"runtime": "goja",
				"entry": "main.js",
				"permissions": ["http:routes"]
			}`,
			"main.js": `
				function onRequest() {
					return { status: 200, body: "v:` + ver + `" };
				}
			`,
		}, "")
	}

	zipV1 := makeZip("1.0.0")
	sum1, err := mgr.InstallFromZip(ctx, bytes.NewReader(zipV1), int64(len(zipV1)), false, "en-US")
	if err != nil || sum1.IsEnabled {
		t.Fatalf("fresh InstallFromZip should start disabled: sum=%+v err=%v", sum1, err)
	}
	if err := mgr.Enable(ctx, "hot-upgrade"); err != nil {
		t.Fatalf("Enable v1.0.0: %v", err)
	}

	zipV2 := makeZip("2.0.0")
	sum2, err := mgr.InstallFromZip(ctx, bytes.NewReader(zipV2), int64(len(zipV2)), true, "en-US")
	if err != nil {
		t.Fatalf("InstallFromZip overwrite v2.0.0: %v", err)
	}
	if !sum2.IsEnabled || sum2.Version != "2.0.0" {
		t.Fatalf("expected active plugin to remain IsEnabled=true at v2.0.0 after overwrite upgrade, got %+v", sum2)
	}
	rec := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/plugins/hot-upgrade/api/version", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "v:2.0.0" {
		t.Fatalf("expected upgraded plugin route to serve v:2.0.0 immediately without manual Enable, got %d %q", rec.Code, rec.Body.String())
	}

	// Part B: Updating config on an auth:oidc plugin invalidates cached OIDC discovery metadata immediately!
	var idp1Hits, idp2Hits atomic.Int64
	var idp1Srv, idp2Srv *httptest.Server
	idp1Srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idp1Hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"authorization_endpoint":"` + idp1Srv.URL + `/oauth2/v1/authorize"}`))
	}))
	defer idp1Srv.Close()

	idp2Srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idp2Hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"authorization_endpoint":"` + idp2Srv.URL + `/oauth2/v2/authorize"}`))
	}))
	defer idp2Srv.Close()

	oidcZip := buildZipBytes(t, map[string]string{
		"manifest.json": `{
			"id": "sso-discovery",
			"name": {"en-US": "SSO Discovery"},
			"version": "1.0.0",
			"runtime": "goja",
			"entry": "main.js",
			"permissions": ["auth:oidc", "kv", "http:outbound"],
			"config_schema": [
				{"key": "issuer", "type": "url", "label": {"en-US": "Issuer URL"}},
				{"key": "display_label", "type": "string", "default": "SSO One", "label": {"en-US": "Label"}}
			]
		}`,
		"main.js": `
			const memCache = { doc: null };
			function loadDiscovery() {
				if (memCache.doc) return memCache.doc;
				const kvRaw = volo.kv.get("oidc:discovery");
				if (kvRaw) {
					memCache.doc = JSON.parse(kvRaw);
					return memCache.doc;
				}
				const issuer = volo.config.get("issuer");
				const resp = volo.http.fetch(issuer + "/.well-known/openid-configuration");
				memCache.doc = JSON.parse(resp.body);
				volo.kv.set("oidc:discovery", resp.body, 3600);
				return memCache.doc;
			}
			function oidcProviderInfo() {
				return { id: "corp-oidc", display_name: volo.config.get("display_label") || "SSO One" };
			}
			function oidcAuthURL(state, redirectURI) {
				const doc = loadDiscovery();
				return doc.authorization_endpoint + "?state=" + encodeURIComponent(state) + "&redirect_uri=" + encodeURIComponent(redirectURI);
			}
			function oidcExchange(code) {
				return { subject: "u_" + code, email: "u@example.com", email_verified: true };
			}
		`,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(oidcZip), int64(len(oidcZip)), true, "en-US"); err != nil {
		t.Fatalf("InstallFromZip sso-discovery: %v", err)
	}
	if _, err := mgr.UpdateConfig(ctx, "sso-discovery", map[string]any{
		"issuer":        idp1Srv.URL,
		"display_label": "IdP One",
	}, "en-US"); err != nil {
		t.Fatalf("UpdateConfig initial issuer: %v", err)
	}
	if err := mgr.Enable(ctx, "sso-discovery"); err != nil {
		t.Fatalf("Enable sso-discovery: %v", err)
	}

	prov1, ok := mgr.Providers().OIDC("corp-oidc")
	if !ok || prov1.DisplayName("en-US") != "IdP One" {
		t.Fatalf("expected registered corp-oidc provider with label 'IdP One', got ok=%v prov=%v", ok, prov1)
	}
	u1, err := prov1.AuthCodeURL("st1", "https://blog.example.com/cb")
	if err != nil || !strings.HasPrefix(u1, idp1Srv.URL+"/oauth2/v1/authorize") {
		t.Fatalf("expected idp1 authorize URL, got %q err=%v", u1, err)
	}
	// Second call hits cache (idp1Hits remains 1).
	_, _ = prov1.AuthCodeURL("st2", "https://blog.example.com/cb")
	if idp1Hits.Load() != 1 {
		t.Fatalf("expected discovery metadata to be cached before UpdateConfig, got %d hits", idp1Hits.Load())
	}

	// UpdateConfig switches issuer to idp2Srv -> must invalidate both VM pool cache, KV discovery cache, and adapter metadata!
	if _, err := mgr.UpdateConfig(ctx, "sso-discovery", map[string]any{
		"issuer":        idp2Srv.URL,
		"display_label": "IdP Two",
	}, "en-US"); err != nil {
		t.Fatalf("UpdateConfig idp2: %v", err)
	}

	prov2, ok := mgr.Providers().OIDC("corp-oidc")
	if !ok || prov2.DisplayName("en-US") != "IdP Two" {
		t.Fatalf("expected refreshed OIDC DisplayName 'IdP Two' immediately after UpdateConfig, got %v", prov2)
	}
	u2, err := prov2.AuthCodeURL("st3", "https://blog.example.com/cb")
	if err != nil || !strings.HasPrefix(u2, idp2Srv.URL+"/oauth2/v2/authorize") || idp2Hits.Load() != 1 {
		t.Fatalf("expected immediate switch to idp2 discovery endpoint without restart, got url=%q hits=%d err=%v", u2, idp2Hits.Load(), err)
	}
}

type stubNamedStorage struct {
	name string
}

func (s stubNamedStorage) Name() string { return s.name }
func (s stubNamedStorage) Put(context.Context, string, io.Reader, int64, string) (string, error) {
	return "https://cdn.example.com/" + s.name, nil
}
func (s stubNamedStorage) Delete(context.Context, string) error { return nil }
func (s stubNamedStorage) HealthCheck(context.Context) error    { return nil }

func TestManifest_PublicHeadPersist_StorageTieBreaker_And_BuiltinPlugins(t *testing.T) {
	ctx := context.Background()

	// 1. public.head with persist: true must pass Manifest validation and render without <div> wrapper!
	rawManifest := `{
		"id": "head-theme-vars",
		"name": {"en-US": "Head Theme Vars"},
		"version": "1.0.0",
		"runtime": "goja",
		"entry": "main.js",
		"permissions": ["slots:public.head"],
		"ui_slots": [
			{
				"slot": "public.head",
				"persist": true,
				"style_asset": "head.css",
				"html": "<style>:root { --demo: 1; }</style>"
			}
		]
	}`
	parsed, err := ParseManifest([]byte(rawManifest))
	if err != nil {
		t.Fatalf("expected public.head with persist=true to validate cleanly, got %v", err)
	}
	if len(parsed.UISlots) != 1 || !parsed.UISlots[0].Persist {
		t.Fatalf("unexpected parsed UISlots: %+v", parsed.UISlots)
	}

	// 2. Deterministic storage provider tie-breaker (a.pluginID < b.pluginID) regardless of registration order!
	reg1 := NewProviderRegistry(nil, nil)
	reg1.RegisterStorage("zeta-storage", stubNamedStorage{name: "zeta"})
	reg1.RegisterStorage("alpha-storage", stubNamedStorage{name: "alpha"})
	if got := reg1.ActiveStorage().Name(); got != "alpha" {
		t.Fatalf("expected alpha-storage to win equal-priority tie when registered second, got %q", got)
	}

	reg2 := NewProviderRegistry(nil, nil)
	reg2.RegisterStorage("alpha-storage", stubNamedStorage{name: "alpha"})
	reg2.RegisterStorage("zeta-storage", stubNamedStorage{name: "zeta"})
	if got := reg2.ActiveStorage().Name(); got != "alpha" {
		t.Fatalf("expected alpha-storage to win equal-priority tie when registered first, got %q", got)
	}
	// Higher precedence (smaller priority number) still overrides alphabetical tie-breaker.
	reg2.SetPluginPriority("zeta-storage", 10)
	if got := reg2.ActiveStorage().Name(); got != "zeta" {
		t.Fatalf("expected priority=10 zeta-storage to beat priority=100 alpha-storage, got %q", got)
	}

	// 3. Stage head-theme-vars + builtin kanbanniang (ja-JP + buffered pats) + sensitive-words (buffered block stats).
	store, kr, cat, tmpDir := openTestPluginEnv(t)
	pluginsDir := filepath.Join(tmpDir, "plugins")
	copyDirRecursive(t, filepath.Join("..", "..", "plugins", "kanbanniang"), filepath.Join(pluginsDir, "kanbanniang"))
	copyDirRecursive(t, filepath.Join("..", "..", "plugins", "sensitive-words"), filepath.Join(pluginsDir, "sensitive-words"))

	mgr := NewManager(ManagerOptions{
		Store:        store,
		Keys:         kr,
		Catalog:      cat,
		PluginsDir:   pluginsDir,
		DataDir:      filepath.Join(tmpDir, "data"),
		HookTimeout:  2 * time.Second,
		RouteTimeout: 2 * time.Second,
	})
	defer func() { _ = mgr.Close(ctx) }()

	if err := mgr.DiscoverAndStart(ctx); err != nil {
		t.Fatalf("DiscoverAndStart: %v", err)
	}

	headZip := buildZipBytes(t, map[string]string{
		"manifest.json":   rawManifest,
		"main.js":         "",
		"assets/head.css": ":root{}",
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(headZip), int64(len(headZip)), true, "en-US"); err != nil {
		t.Fatalf("InstallFromZip head-theme-vars: %v", err)
	}
	if err := mgr.Enable(ctx, "head-theme-vars"); err != nil {
		t.Fatalf("Enable head-theme-vars: %v", err)
	}
	headSlots := mgr.RenderSlots(ctx, SlotPublicHead, SlotRenderContext{Locale: "en-US", Path: "/"})
	if len(headSlots) != 1 || !headSlots[0].Persist {
		t.Fatalf("expected 1 persistent public.head slot, got %+v", headSlots)
	}
	if strings.Contains(headSlots[0].HTML, "<div") || !strings.Contains(headSlots[0].HTML, `data-volo-persist="head-theme-vars"`) {
		t.Fatalf("expected public.head persistent slot to annotate head tags without <div> wrapper, got %s", headSlots[0].HTML)
	}

	// Verify kanbanniang ja-JP locale + zero-write-tx buffered pat counter!
	if err := mgr.Enable(ctx, "kanbanniang"); err != nil {
		t.Fatalf("Enable kanbanniang: %v", err)
	}
	patRec := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(patRec, httptest.NewRequest(http.MethodPost, "/plugins/kanbanniang/api/pat", nil))
	if patRec.Code != http.StatusOK || !strings.Contains(patRec.Body.String(), `"total_pats":1`) {
		t.Fatalf("kanbanniang POST /pat failed: %d %s", patRec.Code, patRec.Body.String())
	}
	jaFooter := mgr.RenderSlots(ctx, SlotPublicFooter, SlotRenderContext{Locale: "ja-JP", Path: "/"})
	if len(jaFooter) != 1 ||
		!strings.Contains(jaFooter[0].HTML, "スタジオ看板娘") ||
		!strings.Contains(jaFooter[0].HTML, "個人ブログへようこそ") ||
		!strings.Contains(jaFooter[0].HTML, `data-pats="1"`) {
		t.Fatalf("expected kanbanniang ja-JP footer with buffered data-pats=1, got %+v", jaFooter)
	}
	jaCard := mgr.RenderDashboardCards(ctx, SlotRenderContext{Locale: "ja-JP"})
	foundJaCard := false
	for _, c := range jaCard {
		if c.PluginID == "kanbanniang" && strings.Contains(c.HTML, "看板娘なでなで統計") {
			foundJaCard = true
		}
	}
	if !foundJaCard {
		t.Fatalf("expected kanbanniang ja-JP dashboard card, got %+v", jaCard)
	}

	// Verify sensitive-words buffered block stats!
	if err := mgr.Enable(ctx, "sensitive-words"); err != nil {
		t.Fatalf("Enable sensitive-words: %v", err)
	}
	if _, err := mgr.UpdateConfig(ctx, "sensitive-words", map[string]any{
		"custom_deny": []string{"testspamtoken"},
	}, "en-US"); err != nil {
		t.Fatalf("UpdateConfig sensitive-words: %v", err)
	}
	verdict := mgr.Providers().EvaluateComment(ctx, CommentCheckInput{
		TargetType: "article",
		TargetID:   "a1",
		AuthorName: "Reader",
		ContentMD:  "hello testspamtoken world",
		Locale:     "en-US",
		IsGuest:    true,
	})
	if verdict.Action != SpamActionReject {
		t.Fatalf("expected sensitive-words to reject custom_deny match, got %+v", verdict)
	}
	// Immediate buffered visibility before SQLite flush:
	var rawSW domain.PluginKV
	if err := sqlx.GetInto(ctx, store, &rawSW, `SELECT * FROM plugin_kv WHERE plugin_id = ? AND k = ?`, "sensitive-words", "stats:blocked_total"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected sensitive-words stats:blocked_total to be buffered in RAM before flush, got %+v", rawSW)
	}
	swKV := NewKVStore(store, "sensitive-words")
	if v, ok, err := swKV.Get(ctx, "stats:blocked_total"); err != nil || !ok || v != "1" {
		t.Fatalf("expected immediate KV.Get visibility of stats:blocked_total=\"1\", got v=%q ok=%v err=%v", v, ok, err)
	}
	// Flush deterministically on Disable:
	if err := mgr.Disable(ctx, "sensitive-words"); err != nil {
		t.Fatalf("Disable sensitive-words: %v", err)
	}
	if err := sqlx.GetInto(ctx, store, &rawSW, `SELECT * FROM plugin_kv WHERE plugin_id = ? AND k = ?`, "sensitive-words", "stats:blocked_total"); err != nil || rawSW.ValueInt != 1 {
		t.Fatalf("expected stats:blocked_total=1 flushed to SQLite after Disable, got row=%+v err=%v", rawSW, err)
	}
}

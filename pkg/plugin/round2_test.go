package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func evalJSScript(vm *jsRuntime, src string) (jsValue, error) {
	prog, err := compileJSProgram("test.js", src)
	if err != nil {
		return jsUndefinedVal, err
	}
	return vm.RunProgram(prog)
}

func TestJSVM_CyclicObjectExportToStringStringifyAndStringAmplificationBounds(t *testing.T) {
	vm := newJSRuntime()
	vm.SetMaxCallStackSize(defaultVMStackLimit)

	// 1. Cyclic object and cyclic array Export() must NEVER stack-overflow Go!
	valObj, err := evalJSScript(vm, `let a = {name: "cycle"}; a.self = a; let arr = [1]; arr.push(arr); a.arr = arr; return a;`)
	if err != nil {
		t.Fatalf("run cyclic script: %v", err)
	}
	exported := valObj.Export()
	m, ok := exported.(map[string]any)
	if !ok || m["name"] != "cycle" {
		t.Fatalf("unexpected exported cyclic map: %#v", exported)
	}

	// 2. Cyclic array String(arr) must terminate cleanly without stack overflow.
	strVal, err := evalJSScript(vm, `let arr = ["x"]; arr.push(arr); return String(arr);`)
	if err != nil {
		t.Fatalf("String(cyclicArray) failed: %v", err)
	}
	sOut, _ := strVal.Export().(string)
	if !strings.HasPrefix(sOut, "x") {
		t.Fatalf("unexpected String(cyclicArray): %q", sOut)
	}

	// 3. Cyclic JSON.stringify(a) must return a controlled JS error (not panic / stack-overflow).
	if _, err := evalJSScript(vm, `let c = {}; c.self = c; return JSON.stringify(c);`); err == nil {
		t.Fatal("expected cyclic JSON.stringify to fail cleanly with circular structure error")
	}

	// 4. Exponential String.prototype.replace("$&$&$&$&") amplification must abort at maxJSStringBytes (4 MiB)!
	_, err = evalJSScript(vm, `
		let s = "x".repeat(65536);
		for (let i = 0; i < 12; i++) {
			s = s.replace(/x/g, "$&$&$&$&");
		}
		return s.length;
	`)
	if err == nil || !strings.Contains(err.Error(), "Invalid string length") {
		t.Fatalf("expected $-group replace amplification to hit 4 MiB string cap, got %v", err)
	}

	// 5. JSON.stringify multi-MiB amplification must abort at maxJSStringBytes!
	_, err = evalJSScript(vm, `
		let chunk = "a".repeat(65536);
		let list = [];
		for (let i = 0; i < 100; i++) { list.push(chunk); }
		return JSON.stringify(list);
	`)
	if err == nil || !strings.Contains(err.Error(), "Invalid string length") {
		t.Fatalf("expected oversized JSON.stringify to hit 4 MiB string cap, got %v", err)
	}
}

type builtinDynamicInsertPlugin struct {
	BaseBuiltinPlugin
}

func (builtinDynamicInsertPlugin) Manifest() Manifest {
	return Manifest{
		ID:          "builtin-dyn-qi",
		Name:        map[string]string{"en-US": "Builtin Dynamic QI"},
		Version:     "1.0.0",
		Runtime:     "builtin",
		Permissions: []string{"ui:slots"},
		UISlots: []SlotSpec{
			{Slot: SlotEditorQuickInsert, Dynamic: true},
		},
	}
}

func (builtinDynamicInsertPlugin) RenderSlot(_ context.Context, slotName string, _ SlotRenderContext) (string, error) {
	if slotName == SlotEditorQuickInsert {
		return `[{"id":"callout","label":"Callout Box","template":":::info\n${selection}\n:::\n"}]`, nil
	}
	return "", nil
}

func TestSDK_VoloOnEscapeHTMLPreMarkdownOIDCDashboardXSSRawHeadersAndKVCursor(t *testing.T) {
	ctx := context.Background()
	store, kr, cat, tmpDir := openTestPluginEnv(t)

	mgr := NewManager(ManagerOptions{
		Store:      store,
		Keys:       kr,
		PluginsDir: filepath.Join(tmpDir, "plugins"),
		DataDir:    filepath.Join(tmpDir, "data"),
		Catalog:    cat,
	})
	defer mgr.Close(ctx)

	if err := mgr.RegisterBuiltin(ctx, builtinDynamicInsertPlugin{}); err != nil {
		t.Fatalf("RegisterBuiltin: %v", err)
	}
	if err := mgr.DiscoverAndStart(ctx); err != nil {
		t.Fatalf("DiscoverAndStart: %v", err)
	}
	if err := mgr.Enable(ctx, "builtin-dyn-qi"); err != nil {
		t.Fatalf("Enable builtin-dyn-qi: %v", err)
	}

	manifestJSON := `{
		"id": "r2-suite",
		"name": {"en-US": "Round 2 Suite"},
		"version": "1.0.0",
		"runtime": "goja",
		"entry": "main.js",
		"permissions": ["kv:rw", "hooks:render.pre_markdown", "ui:slots", "provider:oidc", "http:routes", "http:raw_headers"],
		"allowed_hosts": ["idp.example.com", "*.cdn.example.com"],
		"hooks": ["render.pre_markdown"],
		"ui_slots": [{"slot": "admin.dashboard_card", "dynamic": true}],
		"config_schema": [
			{"key": "endpoint", "type": "url", "label": {"en-US": "Endpoint"}, "default": "https://idp.example.com"},
			{"key": "accent", "type": "color", "label": {"en-US": "Accent"}, "default": "#2563eb"},
			{"key": "notes", "type": "textarea", "label": {"en-US": "Notes"}, "default": "hello"},
			{"key": "ratio", "type": "number", "label": {"en-US": "Ratio"}, "min": 0, "max": 10, "step": 0.5, "default": 2.5}
		]
	}`
	mainJS := `
		volo.on("render.pre_markdown", function(payload) {
			return {
				content_md: payload.content_md + "\n\nSafeTag: " + volo.escapeHTML("<img src=x onerror=1>")
			};
		});

		function oidcProviderInfo() {
			return { id: "corp-sso", name: "Corp SSO" };
		}
		function oidcAuthURL(state, redirectURI) {
			return "https://idp.example.com/authorize?state=" + encodeURIComponent(state) + "&redirect_uri=" + encodeURIComponent(redirectURI);
		}
		function oidcExchange(code, redirectURI) {
			return {
				subject: "sub_" + code,
				email: "alice@corp.example.com",
				display_name: "Alice"
			};
		}

		function renderSlot(slot, ctx) {
			if (slot === "admin.dashboard_card") {
				return '<div class="r2-card"><h4>Stats</h4><script>evil()</script><a href="javascript:alert(1)" onclick="steal()">Bad</a><p>OK</p></div>';
			}
			return "";
		}

		function onRequest(req) {
			volo.kv.set("item:a", "1", 0);
			volo.kv.set("item:b", "2", 0);
			volo.kv.set("item:c", "3", 0);
			let page2 = volo.kv.list("item:", 2, "item:a");
			return {
				status: 200,
				content_type: "application/json",
				body: JSON.stringify({
					sig: req.headers["x-hub-signature-256"] || "",
					keys: page2.map(function(x){ return x.key; })
				})
			};
		}
	`
	zipBytes := buildZipBytes(t, map[string]string{
		"manifest.json": manifestJSON,
		"main.js":       mainJS,
	}, "")
	if _, err := mgr.InstallFromZip(ctx, bytes.NewReader(zipBytes), int64(len(zipBytes)), true, "en-US"); err != nil {
		t.Fatalf("InstallFromZip r2-suite: %v", err)
	}
	if err := mgr.Enable(ctx, "r2-suite"); err != nil {
		t.Fatalf("Enable r2-suite: %v", err)
	}

	// 1. Verify OIDCProvider registration & execution!
	oidcList := mgr.Providers().OIDCProviders()
	if len(oidcList) != 1 || oidcList[0].DisplayName("en-US") != "Corp SSO" {
		t.Fatalf("expected registered goja OIDCProvider 'Corp SSO', got %+v", oidcList)
	}
	authURL, err := oidcList[0].AuthCodeURL("st_123", "https://blog.example.com/cb")
	if err != nil || !strings.Contains(authURL, "state=st_123") {
		t.Fatalf("unexpected OIDC AuthCodeURL: url=%q err=%v", authURL, err)
	}
	ident, err := oidcList[0].Exchange(ctx, "code_xyz", "https://blog.example.com/cb")
	if err != nil || ident.Subject != "sub_code_xyz" || ident.Email != "alice@corp.example.com" {
		t.Fatalf("unexpected OIDC Exchange identity: %+v err=%v", ident, err)
	}

	// 2. Verify HookRenderPreMarkdown via volo.on + volo.escapeHTML AND privacy protection on locked articles!
	unlockedOut := mgr.DispatchArticleHook(ctx, HookRenderPreMarkdown, ArticleHookPayload{
		ID:        "a1",
		ContentMD: "# Title",
	})
	if !strings.Contains(unlockedOut.ContentMD, "&lt;img src=x onerror=1&gt;") {
		t.Fatalf("expected render.pre_markdown to append escaped HTML, got %q", unlockedOut.ContentMD)
	}
	lockedIn := ArticleHookPayload{
		ID:                  "a2",
		ContentMD:           "TOP_SECRET_MARKDOWN",
		IsPasswordProtected: true,
	}
	lockedOut := mgr.DispatchArticleHook(ctx, HookRenderPreMarkdown, lockedIn)
	if lockedOut.ContentMD != "TOP_SECRET_MARKDOWN" {
		t.Fatalf("protected article ContentMD must NEVER be overwritten by untrusted pre_markdown hook: got %q", lockedOut.ContentMD)
	}

	// 3. Verify RenderDashboardCards strips <script>, onclick, and javascript: href!
	cards := mgr.RenderDashboardCards(ctx, SlotRenderContext{Locale: "en-US"})
	if len(cards) != 1 {
		t.Fatalf("expected 1 dashboard card, got %d", len(cards))
	}
	if strings.Contains(cards[0].HTML, "<script") || strings.Contains(cards[0].HTML, "onclick") || strings.Contains(cards[0].HTML, "javascript:") {
		t.Fatalf("UNSANITIZED XSS in dashboard card HTML: %s", cards[0].HTML)
	}
	if !strings.Contains(cards[0].HTML, "<h4>Stats</h4>") {
		t.Fatalf("expected safe markup preserved in dashboard card, got: %s", cards[0].HTML)
	}

	// 4. Verify http:raw_headers passes x-hub-signature-256, cursor kv.list works, AND KV/telemetry writes do NOT bump DataRevision()!
	revBefore := store.DataRevision()
	req := httptest.NewRequest(http.MethodPost, "/plugins/r2-suite/api/webhook", strings.NewReader(`{}`))
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	rec := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from plugin webhook route, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Sig  string   `json:"sig"`
		Keys []string `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Sig != "sha256=deadbeef" {
		t.Fatalf("expected x-hub-signature-256 passed via http:raw_headers, got %q", resp.Sig)
	}
	if len(resp.Keys) != 2 || resp.Keys[0] != "item:b" || resp.Keys[1] != "item:c" {
		t.Fatalf("expected cursor volo.kv.list after 'item:a' to return [item:b item:c], got %v", resp.Keys)
	}
	if revAfter := store.DataRevision(); revAfter != revBefore {
		t.Fatalf("plugin KV writes & telemetry bumped global content dataRev (%d -> %d)!", revBefore, revAfter)
	}

	// 5. Verify builtin dynamic editor.quick_insert slot works!
	qis := mgr.EditorQuickInserts("en-US")
	foundBuiltinQI := false
	for _, item := range qis {
		if item.PluginID == "builtin-dyn-qi" && item.ID == "callout" {
			foundBuiltinQI = true
		}
	}
	if !foundBuiltinQI {
		t.Fatalf("expected builtin dynamic quick insert 'callout', got %+v", qis)
	}

	// 6. Verify Manifest.Validate() rejects invalid Hooks, loopback/scheme AllowedHosts, and bad ConfigSchema url!
	badHost := Manifest{
		ID:           "bad-host",
		Name:         map[string]string{"en-US": "Bad"},
		Version:      "1.0.0",
		Runtime:      "goja",
		Entry:        "main.js",
		AllowedHosts: []string{"https://evil.com/path"},
	}
	if err := badHost.Validate(); err == nil {
		t.Fatal("expected AllowedHosts with scheme/path to fail Manifest.Validate()")
	}
	if _, err := mgr.UpdateConfig(ctx, "r2-suite", map[string]any{"endpoint": "javascript:alert(1)"}, "en-US"); err == nil {
		t.Fatal("expected UpdateConfig to reject javascript: URL for type=url config field")
	}
}
